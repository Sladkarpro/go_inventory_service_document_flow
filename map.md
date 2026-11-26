# Карта: Pulse — go_inventory_service_document_flow

Канонический путь: `D:\projects\pulse\go_inventory_service_document_flow`. Навигация проверена 24.10.2026 по локальному README, manifest и структуре; сервис и БД не запускались.

Один из девяти репозиториев [Pulse](<../map.md>): складской учёт и инвентаризация на Go с отдельной PostgreSQL. README описывает JWT-контракт с Symfony, синхронизацию пользователей через RabbitMQ и публикацию API через Nginx.

## Начало работы

Прочитать [правила Pulse](<../CLAUDE.md>), проверить Git status и актуальный план задачи, затем [README.md](<README.md>) и [go.mod](<go.mod>).

## Точки входа

| Путь | Назначение |
|---|---|
| [cmd/](<cmd>) | Точка запуска сервиса |
| [internal/](<internal>) | HTTP/API, бизнес-логика и доступ к данным |
| [migrations/](<migrations>) | Миграции схемы; наличие файлов не разрешает их запуск |
| [docker-compose.yml](<docker-compose.yml>) | Описание локального окружения |

Контракты и порядок миграций описаны в README. Соседний [Symfony](<../symfony_documents_flow/map.md>), очередь, рабочая БД и её резервные копии не проверялись. Данные, ключи и секреты сохранять отдельно от навигационных материалов.
