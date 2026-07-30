# Warehouse microservice (складской учёт и инвентаризация)

Go-микросервис складского учёта и инвентаризации. Устроен по той же схеме, что и соседний
`go_kanban_service_document_flow`: собственная база PostgreSQL, авторизация по JWT монолита
Symfony (`symfony_documents_flow`), синхронизация справочника пользователей через RabbitMQ,
наружу порт не публикуется — запросы проксирует nginx фронтенда.

Отличия от канбана по портам (всё остальное совпадает):

| Что | Канбан | Warehouse |
|---|---|---|
| HTTP-порт сервиса | 8085 | **8087** |
| Порт Postgres на хосте | 5433 | **5434** |
| База | `kanban_db` | `warehouse_db` |
| Docker-сеть сервиса | `kanban-net` | `warehouse-net` |
| Каталог данных Postgres | `./../postgres_data_kanban` | `./../postgres_data_warehouse` |

Порт 8086 занят `notification_service` — не использовать.

---

## Инструкция по развертыванию

### 1. Подготовка сети и окружения

Убедитесь, что внешняя сеть `project-net` существует (она используется в `docker-compose.yml`).
Если нет, создайте её:

```bash
docker network create project-net
```

Скопируйте пример файла конфигурации и настройте переменные:

```bash
cp .env.example .env
```

Обязательно задайте `WAREHOUSE_CREDENTIALS_KEY` (см. раздел «Ключ шифрования» ниже) и
`SYMFONY_INTERNAL_API_KEY`.

Публичный ключ JWT в контейнер не копируется, а монтируется read-only из монолита:

```
./../symfony_documents_flow/config/jwt/public.pem:/config/jwt/public.pem:ro
```

То есть каталог `symfony_documents_flow` должен лежать рядом с этим репозиторием, и ключ
`config/jwt/public.pem` в нём должен существовать — иначе сервис не поднимется.

### 2. Сборка бинарника на хосте

⚠ **Dockerfile однослойный: внутри образа нет Go-тулчейна и нет `go build`.** Контейнер только
запускает уже готовый бинарник `./main`, который лежит в примонтированной папке проекта
(`./:/app`). Поэтому собирать нужно на хосте, ДО `docker compose up`.

Для Linux / macOS:

```bash
GOOS=linux GOARCH=amd64 go build -o main cmd/main.go
```

Для Windows (PowerShell / CMD / терминал IDE) — сборка под Linux через контейнер с тулчейном:

```bash
docker run --rm -v "./:/app" -v go-mod-cache:/go/pkg/mod -v go-build-cache:/root/.cache/go-build -w /app golang:1.25 go build -o main cmd/main.go
```

Бинарник `main` намеренно в `.gitignore` — он артефакт сборки, а не часть репозитория.

### 3. Запуск и миграции

Запустите контейнеры:

```bash
docker compose build
docker compose up -d
```

Сервис **не накатывает миграции сам**. После старта БД выполните миграции вручную с помощью утилиты `goose` (доступы она возьмет из вашего `.env`):

```bash
~/go/bin/goose up
```

### 4. Настройка nginx

Наружу порт 8087 не публикуется. Доступ идёт через nginx фронтенда (`analytics_platform`), куда нужно добавить маршрут:
`^/spa/api/warehouse(/.*)?$   ->   warehouse_service:8087`

### 5. Обновление кода (workflow разработки)

1. Пересобрать бинарник (командой из шага 2 — он собирается в текущую директорию, которая
   прокинута в контейнер как volume).
2. Перезапустить контейнер, он подхватит обновлённый `main`:

```bash
docker compose restart warehouse_service
```

Просмотр логов:

```bash
docker compose logs -f warehouse_service
```

---

## ⚠ Миграции применяет владелец, не сервис

Сервис **не накатывает миграции сам**, и мы их тоже не накатываем. Готовый файл схемы лежит в
`migrations/00001_init_warehouse_schema.sql`. Тот, кто разворачивает сервис, применяет его
вручную утилитой [goose](https://github.com/pressly/goose):

```bash
~/go/bin/goose up
```

`goose` берёт параметры из окружения — переменные уже описаны в `.env.example`:

```
GOOSE_DRIVER=postgres
GOOSE_DBSTRING=postgresql://${DB_USER}:${DB_PASSWORD}@${DB_HOST}:${DB_PORT}/${DB_NAME}?sslmode=disable
GOOSE_MIGRATION_DIR=migrations
```

⚠ **Накатывать эту миграцию можно только на `warehouse_db`.** Перед запуском проверьте, что
`GOOSE_DBSTRING` указывает именно на базу этого сервиса (порт `5434` на хосте, `DB_NAME=warehouse_db`).
Запуск против чужой базы — канбана (`kanban_db`, порт 5433), монолита Symfony (порт 5432) или
любой другой — недопустим.

---

## ⚠ Ключ шифрования `WAREHOUSE_CREDENTIALS_KEY`

Учётные данные оборудования (пароли устройств) хранятся в базе в зашифрованном виде. Ключ
шифрования — **32 байта в base64**, живёт **только в переменной окружения**.

Сгенерировать:

```bash
openssl rand -base64 32
```

Правила, без исключений:

- ключ **не хранится в базе** — в БД лежат только шифротексты;
- ключ **не коммитится в git** — `.env` в `.gitignore`, в репозиторий попадает только `.env.example`
  с плейсхолдером;
- ключ **не пишется в лог** и не выводится в отладочные сообщения.

⚠ **Потеря ключа необратима.** Если ключ утрачен, расшифровать сохранённые пароли устройств
невозможно — механизма восстановления нет, их придётся вводить заново. Храните ключ отдельно от
сервера (в менеджере секретов или парольном хранилище) и держите вторую копию в другом месте.

При смене ключа старые записи новым ключом не расшифруются — это отдельная процедура
перешифрования, а не просто правка `.env`.

---

## ⚠ Маршрут nginx на фронтенде

Наружу порт 8087 не публикуется (в `docker-compose.yml` секция `ports` у сервиса намеренно
закомментирована). Доступ идёт через nginx фронтенда, поэтому там должен быть маршрут:

```
^/spa/api/warehouse(/.*)?$   ->   warehouse_service:8087
```

Файл: `analytics_platform/docker_env/nginx/config/default.conf`.

⚠ Это **nginx фронтенда**, а не nginx монолита. В конфиг `symfony_documents_flow` этот маршрут
добавлять не нужно.

Связность обеспечивает общая внешняя сеть `project-net`: контейнер nginx и `warehouse_service`
должны быть в ней оба, тогда имя `warehouse_service` резолвится по внутреннему DNS Docker.

---

## RabbitMQ: dead-letter для очереди `warehouse.user_sync`

Политика dead-letter `dlx-go` живёт в монолите —
`symfony_documents_flow/docker_env/rabbitmq/definitions.json`. Её паттерн расширен
до трёх сервисов (правка согласована с владельцем 2026-07-29):

```
^(kanban|notification|warehouse)\.
```

Очередь под него попадает, и отравленные сообщения после `Nack(false, false)` уезжают
в `events.dlx` → `dead.events` (там свой лимит: 100 000 сообщений, TTL 14 суток).

⚠ **Точка в имени очереди — часть контракта, а не стиль.** Паттерн привязан к разделителю:
`warehouse_user_sync` с подчёркиванием под него не попадёт, dead-lettering молча выключится,
и потерю сообщений будет видно только по расхождению реплики справочников. Имя задаётся
`RABBITMQ_USER_SYNC_QUEUE`; и значение по умолчанию в `internal/config/config.go`,
и строка в `.env.example` — с точкой.

Саму очередь `definitions.json` не объявляет: её создаёт консьюмер при старте
(`internal/messaging/usersync/consumer.go`, `QueueDeclare`). Это не мешает политике —
политики RabbitMQ применяются по имени, независимо от того, кто объявил очередь.
Так же сделано и у соседей.

---

## Проверка после развёртывания

1. **Health-check отвечает.** Порт наружу не проброшен, а в образе сервиса нет `curl`
   (в Dockerfile ставятся только `ca-certificates` и `tzdata`), поэтому проверяем из соседнего
   контейнера в сети `project-net` — например, из nginx фронтенда:

   ```bash
   docker compose exec <контейнер-nginx> curl -i http://warehouse_service:8087/healthz
   ```

   Ожидаем `200 OK`.

2. **Запрос с настоящим JWT от Symfony проходит.** Возьмите валидный access-токен, выданный
   монолитом, и обратитесь через фронтовый nginx:

   ```bash
   curl -i -H "Authorization: Bearer <JWT>" https://<хост-фронтенда>/spa/api/warehouse/...
   ```

   Ожидаем ответ приложения (не 401 и не 502). 502 обычно означает, что маршрут nginx или сеть
   `project-net` настроены неверно.

3. **Запрос без токена отклоняется.**

   ```bash
   curl -i https://<хост-фронтенда>/spa/api/warehouse/...
   ```

   Ожидаем `401 Unauthorized`. Если вместо этого приходит `200`, проверьте, что
   `JWT_PUBLIC_KEY_PATH` указывает на реально примонтированный `/config/jwt/public.pem`.

Дополнительно стоит убедиться, что в логах (`docker compose logs -f warehouse_service`) нет
ошибок подключения к Postgres и к RabbitMQ.
