package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go_warehouse_service/internal/apperr"
	"go_warehouse_service/internal/config"
	"go_warehouse_service/internal/crypto"
	"go_warehouse_service/internal/helper"
)

// Сквозная проверка сервиса против НАСТОЯЩЕЙ базы.
//
// Проверяет ровно то, что перечислено в критериях приёмки:
//   1. /healthz отвечает; запрос без токена — 401; ROLE_ADMIN получает права.
//   2. Сценарий владельца: числится 22, вбили 2 → строка попала в ведомость с −20.
//   3. Пароль не виден в списке; reveal пишет журнал; to-rotate не пуст.
//   4. Импорт реального файла владельца кладёт в базу верные суммы.
//
// Требует поднятой БД с применённой миграцией. Адрес — в WAREHOUSE_TEST_DSN;
// без неё тест ПРОПУСКАЕТСЯ, а не падает: на машине без Docker это была бы
// ложная тревога.
//
//	docker run -d --name wh_test_pg -e POSTGRES_USER=root -e POSTGRES_PASSWORD=password \
//	  -e POSTGRES_DB=warehouse_test -p 55434:5432 postgres:16.4
//	GOOSE_DRIVER=postgres GOOSE_DBSTRING=... goose up
//	WAREHOUSE_TEST_DSN="postgresql://root:password@localhost:55434/warehouse_test?sslmode=disable" go test ./internal/app/...

type testEnv struct {
	t       *testing.T
	srv     *httptest.Server
	db      *pgxpool.Pool
	signKey *rsa.PrivateKey
}

func setup(t *testing.T) *testEnv {
	t.Helper()

	dsn := os.Getenv("WAREHOUSE_TEST_DSN")
	if dsn == "" {
		t.Skip("WAREHOUSE_TEST_DSN не задан — сквозной тест пропущен")
	}

	db, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("подключение к тестовой БД: %v", err)
	}
	if err := db.Ping(context.Background()); err != nil {
		t.Skipf("тестовая БД недоступна (%v) — тест пропущен", err)
	}

	// Symfony подписывает токены приватным ключом, мы проверяем публичным.
	// Настоящего ключа на машине разработчика может не быть, поэтому пара
	// генерируется здесь: проверяется механика RS256, а не конкретный ключ.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("генерация ключа: %v", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("сериализация публичного ключа: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "public.pem")
	if err := os.WriteFile(keyPath,
		pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o600); err != nil {
		t.Fatalf("запись публичного ключа: %v", err)
	}

	credKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("генерация ключа шифрования: %v", err)
	}

	cfg := &config.Config{
		Env:              "test",
		Port:             "0",
		JWTPublicKeyPath: keyPath,
		CredentialsKey:   credKey,
		Clock:            helper.NewClock(),
		// RabbitMQ и Symfony намеренно не заданы: сервис обязан работать
		// и без них, просто без живой синхронизации.
	}

	app, err := NewApp(cfg, db)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}

	env := &testEnv{t: t, srv: httptest.NewServer(app.router), db: db, signKey: key}
	t.Cleanup(func() {
		env.srv.Close()
		db.Close()
	})
	env.truncate()
	return env
}

func (e *testEnv) truncate() {
	e.t.Helper()
	_, err := e.db.Exec(context.Background(), `
		TRUNCATE wh_credential_view_log, wh_credential, wh_holding, wh_item,
		         wh_warehouse, users, departments RESTART IDENTITY CASCADE`)
	if err != nil {
		e.t.Fatalf("очистка БД: %v", err)
	}
}

func (e *testEnv) token(id int64, roles ...string) string {
	e.t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"id":       id,
		"username": fmt.Sprintf("user%d", id),
		"roles":    roles,
		"exp":      time.Now().Add(time.Hour).Unix(),
		"iat":      time.Now().Unix(),
	})
	s, err := tok.SignedString(e.signKey)
	if err != nil {
		e.t.Fatalf("подпись токена: %v", err)
	}
	return s
}

func (e *testEnv) expiredToken(id int64, roles ...string) string {
	e.t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"id": id, "username": "expired", "roles": roles,
		"exp": time.Now().Add(-time.Hour).Unix(),
	})
	s, _ := tok.SignedString(e.signKey)
	return s
}

func (e *testEnv) do(method, path, token string, body any) (int, map[string]any) {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rdr)
	if err != nil {
		e.t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	out := map[string]any{}
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out
}

const (
	roleAdmin  = "ROLE_ADMIN"
	roleUser   = "ROLE_USER"
	roleViewer = "ROLE_INVENTORY_VIEWER"
	roleMol    = "ROLE_INVENTORY_MOL"
)

// inventDir — выгрузки владельца лежат ВНЕ репозитория сервиса: это рабочие
// файлы бухгалтерии, а не тестовые данные, и коммитить их сюда нельзя.
const inventDir = `E:\projects\kanban\invent`

// inventFile ищет выгрузку владельца по фрагменту имени.
//
// Каталога может не быть (чужая машина, CI) — тогда тест ПРОПУСКАЕТСЯ.
// Файл не найден при доступном каталоге — возвращается "", и решение принимает
// вызывающий: одному тесту нужен один файл, другому три.
func inventFile(t *testing.T, sub string) string {
	t.Helper()
	entries, err := os.ReadDir(inventDir)
	if err != nil {
		t.Skipf("каталог с выгрузками недоступен (%v)", err)
	}
	for _, en := range entries {
		if !en.IsDir() && filepath.Ext(en.Name()) == ".xlsx" &&
			bytes.Contains([]byte(en.Name()), []byte(sub)) {
			return filepath.Join(inventDir, en.Name())
		}
	}
	return ""
}

// upload прогоняет файл через настоящую ручку импорта и возвращает отчёт.
//
// Именно через HTTP, а не вызовом сервиса напрямую: проверяется весь путь,
// включая multipart, спуливание во временный файл и проверку прав.
func (e *testEnv) upload(token, path, format string) map[string]any {
	e.t.Helper()

	f, err := os.Open(path)
	if err != nil {
		e.t.Fatalf("открытие %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("format", format)
	part, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		e.t.Fatalf("multipart: %v", err)
	}
	if _, err := io.Copy(part, f); err != nil {
		e.t.Fatalf("copy: %v", err)
	}
	_ = mw.Close()

	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/spa/api/warehouse/import", &buf)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("import: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("импорт %s вернул %d: %s", format, resp.StatusCode, string(raw))
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	report, ok := out["report"].(map[string]any)
	if !ok {
		e.t.Fatalf("в ответе импорта нет отчёта: %s", string(raw))
	}
	return report
}

// scalar — короткая обёртка над одиночным значением из БД: в этих тестах
// проверяется в основном «что в итоге лежит в базе», и запросов таких много.
func (e *testEnv) scalar(query string, args ...any) string {
	e.t.Helper()
	var v *string
	if err := e.db.QueryRow(context.Background(), query, args...).Scan(&v); err != nil {
		e.t.Fatalf("запрос %q: %v", query, err)
	}
	if v == nil {
		return ""
	}
	return *v
}

// count — то же для целых.
func (e *testEnv) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		e.t.Fatalf("запрос %q: %v", query, err)
	}
	return n
}

// TestAuth — критерий приёмки №1.
func TestAuth(t *testing.T) {
	e := setup(t)

	t.Run("healthz публичен", func(t *testing.T) {
		code, body := e.do(http.MethodGet, "/healthz", "", nil)
		if code != http.StatusOK {
			t.Fatalf("/healthz вернул %d", code)
		}
		if body["service"] != "warehouse" {
			t.Errorf("/healthz вернул %v", body)
		}
		// Возраст синхронизации обязателен: без него молчаливая деградация
		// (очередь встала, Symfony недоступна) ничем себя не выдаёт.
		if _, ok := body["lastSyncAgeSeconds"]; !ok {
			if body["status"] != "degraded" {
				t.Error("/healthz обязан сообщать либо возраст синхронизации, либо degraded")
			}
		}
	})

	t.Run("без токена 401", func(t *testing.T) {
		if code, _ := e.do(http.MethodGet, "/spa/api/warehouse/items", "", nil); code != http.StatusUnauthorized {
			t.Fatalf("без токена получили %d, ожидался 401", code)
		}
	})

	t.Run("мусорный токен 401", func(t *testing.T) {
		if code, _ := e.do(http.MethodGet, "/spa/api/warehouse/items", "не-токен", nil); code != http.StatusUnauthorized {
			t.Fatalf("получили %d, ожидался 401", code)
		}
	})

	t.Run("просроченный токен 401", func(t *testing.T) {
		code, _ := e.do(http.MethodGet, "/spa/api/warehouse/items", e.expiredToken(1, roleAdmin), nil)
		if code != http.StatusUnauthorized {
			t.Fatalf("просроченный токен дал %d, ожидался 401 "+
				"(WithExpirationRequired не работает?)", code)
		}
	})

	// ⚠ ГЛАВНОЕ. В токене у администратора портала лежит только ROLE_ADMIN;
	// ROLE_INVENTORY_ADMIN он получает лишь через иерархию, развёрнутую в Go.
	t.Run("ROLE_ADMIN получает права администратора инвентаризации", func(t *testing.T) {
		code, _ := e.do(http.MethodGet, "/spa/api/warehouse/items", e.token(1, roleAdmin), nil)
		if code != http.StatusOK {
			t.Fatalf("ROLE_ADMIN получил %d, ожидался 200 — иерархия ролей не развёрнута", code)
		}
		// Права админа: доступ к отчёту о ротации паролей.
		code, _ = e.do(http.MethodGet, "/spa/api/warehouse/credentials/to-rotate", e.token(1, roleAdmin), nil)
		if code != http.StatusOK {
			t.Errorf("ROLE_ADMIN не получил доступ к /credentials/to-rotate: %d", code)
		}
	})

	t.Run("ROLE_USER без прав инвентаризации 403", func(t *testing.T) {
		code, body := e.do(http.MethodGet, "/spa/api/warehouse/items", e.token(2, roleUser), nil)
		if code != http.StatusForbidden {
			t.Fatalf("ROLE_USER получил %d, ожидался 403", code)
		}
		if body["code"] != "insufficient_rights" {
			t.Errorf("код ошибки %v", body["code"])
		}
	})

	// ⚠ Три ручки закрыты до администратора решением владельца 29.07.2026.
	// Все три были найдены состязательным ревью и подтверждены замером.
	t.Run("список доступов закрыт до администратора", func(t *testing.T) {
		// Пароль в списке и раньше не отдавался, но login и url админ-панелей
		// получала любая инвентарная роль — а это половина доступа.
		for name, tok := range map[string]string{
			"наблюдатель":      e.token(2, roleViewer),
			"МОЛ":              e.token(3, roleMol),
			"начальник отдела": e.token(4, "ROLE_INVENTORY_DEPT_VIEWER"),
		} {
			code, body := e.do(http.MethodGet,
				"/spa/api/warehouse/credentials?holding_id=1", tok, nil)
			if code != http.StatusForbidden {
				t.Errorf("%s получил список доступов: HTTP %d %v", name, code, body)
			}
		}
	})

	t.Run("импорт закрыт до администратора", func(t *testing.T) {
		// Одна перезаливка переписывает остатки всего предприятия — этим
		// вскрылись обе утечки повторного импорта. Раньше хватало права записи,
		// то есть загрузить мог и МОЛ.
		code, body := e.do(http.MethodPost, "/spa/api/warehouse/import", e.token(3, roleMol), nil)
		if code != http.StatusForbidden {
			t.Errorf("МОЛ допущен к импорту: HTTP %d %v", code, body)
		}
	})

	t.Run("начальник отдела не читает ничего, пока нет скоупа", func(t *testing.T) {
		// Роль звучит ограниченной, но фильтра по подразделению в сервисе нет:
		// она давала данные всего предприятия. До реализации скоупа не даёт
		// ничего, чтобы ошибочная выдача была безвредной, а не тихой.
		code, _ := e.do(http.MethodGet, "/spa/api/warehouse/items",
			e.token(4, "ROLE_INVENTORY_DEPT_VIEWER"), nil)
		if code != http.StatusForbidden {
			t.Errorf("начальник отдела читает инвентаризацию: HTTP %d "+
				"(скоупа по подразделению в сервисе нет — значит видит всё)", code)
		}
	})

	t.Run("наблюдатель читает, но не пишет", func(t *testing.T) {
		if code, _ := e.do(http.MethodGet, "/spa/api/warehouse/items", e.token(3, roleViewer), nil); code != http.StatusOK {
			t.Errorf("наблюдатель не смог прочитать каталог: %d", code)
		}
		code, _ := e.do(http.MethodPost, "/spa/api/warehouse/holdings", e.token(3, roleViewer),
			map[string]any{"itemId": 1, "holderType": "unknown", "qtyExpected": "1"})
		if code != http.StatusForbidden {
			t.Errorf("наблюдатель смог создать строку: %d", code)
		}
	})
}

// TestOwnerCountScenario — критерий приёмки №3 и сценарий из §11 плана:
// склад числится 22, вбили факт 2 → строка попала в ведомость потерь с −20.
func TestOwnerCountScenario(t *testing.T) {
	e := setup(t)
	admin := e.token(1, roleAdmin)

	code, body := e.do(http.MethodPost, "/spa/api/warehouse/warehouses", admin,
		map[string]any{"name": "Склад ИТ", "responsibleUserId": nil})
	if code != http.StatusCreated {
		t.Fatalf("создание склада: %d %v", code, body)
	}
	whID := int64(body["warehouse"].(map[string]any)["id"].(float64))

	// Позиция каталога заводится напрямую: обычно её создаёт импорт.
	var itemID int64
	err := e.db.QueryRow(context.Background(),
		`INSERT INTO wh_item (name, name_norm) VALUES ($1,$2) RETURNING id`,
		"Монитор RDW 2401К", "монитор rdw 2401к").Scan(&itemID)
	if err != nil {
		t.Fatalf("создание позиции: %v", err)
	}

	code, body = e.do(http.MethodPost, "/spa/api/warehouse/holdings", admin, map[string]any{
		"itemId": itemID, "holderType": "warehouse",
		"warehouseId": whID, "qtyExpected": "22",
	})
	if code != http.StatusCreated {
		t.Fatalf("создание строки: %d %v", code, body)
	}
	holding := body["holding"].(map[string]any)
	holdingID := int64(holding["id"].(float64))

	// ⚠ Пока не считали — qtyActual обязан быть null, а НЕ 0.
	// Ноль означал бы «искали и не нашли», и ведомость соврала бы на 22 единицы.
	if holding["qtyActual"] != nil {
		t.Fatalf("до пересчёта qtyActual = %v, ожидался null", holding["qtyActual"])
	}

	// Строка должна попадать в фильтр «что ещё не считали».
	code, body = e.do(http.MethodGet, "/spa/api/warehouse/holdings?only_uncounted=1", admin, nil)
	if code != http.StatusOK || len(body["holdings"].([]any)) != 1 {
		t.Fatalf("фильтр «не считали» вернул %d, %v", code, body["holdings"])
	}

	// Владелец вбивает факт.
	code, body = e.do(http.MethodPatch, fmt.Sprintf("/spa/api/warehouse/holdings/%d", holdingID),
		admin, map[string]any{"qtyActual": "2", "outcome": "missing"})
	if code != http.StatusOK {
		t.Fatalf("проставление факта: %d %v", code, body)
	}
	if got := body["holding"].(map[string]any)["qtyActual"]; got != "2" {
		t.Errorf("qtyActual = %v, ожидалось \"2\"", got)
	}

	// И строка появляется в ведомости с недостачей −20.
	code, body = e.do(http.MethodGet, "/spa/api/warehouse/reports/losses", admin, nil)
	if code != http.StatusOK {
		t.Fatalf("ведомость: %d %v", code, body)
	}
	losses := body["losses"].([]any)
	if len(losses) != 1 {
		t.Fatalf("в ведомости %d строк, ожидалась 1", len(losses))
	}
	row := losses[0].(map[string]any)
	if row["qtyDiff"] != "-20" {
		t.Errorf("qtyDiff = %v, ожидалось \"-20\"", row["qtyDiff"])
	}
	if row["discrepancyKind"] != "недостача" {
		t.Errorf("вид расхождения = %v, ожидалась «недостача»", row["discrepancyKind"])
	}

	// Выгрузка ведомости отдаёт настоящий xlsx.
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/spa/api/warehouse/reports/losses.xlsx", nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("выгрузка: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("выгрузка вернула %d", resp.StatusCode)
	}
	// xlsx — это zip, он начинается с «PK».
	if len(raw) < 4 || raw[0] != 'P' || raw[1] != 'K' {
		t.Errorf("выгрузка не похожа на xlsx: первые байты %q", raw[:min(8, len(raw))])
	}

	// Естественный ключ обязан отбить попытку завести ВТОРУЮ безномерную строку
	// с тем же держателем и той же позицией: иначе повторный импорт удвоил бы остаток.
	_, err = e.db.Exec(context.Background(), `
		INSERT INTO wh_holding (item_id, holder_type, warehouse_id, qty_expected, source_file)
		VALUES ($1,'warehouse',$2, 5, 'тест')`, itemID, whID)
	if err == nil {
		t.Error("естественный ключ пропустил дубль строки закрепления")
	}

	// Поломка и излишек тоже обязаны попадать в ведомость: у сломанной вещи
	// qty_actual == qty_expected, и первая редакция представления её теряла.
	var brokenItemID, brokenID int64
	if err := e.db.QueryRow(context.Background(),
		`INSERT INTO wh_item (name, name_norm) VALUES ('Принтер Pantum','принтер pantum') RETURNING id`).
		Scan(&brokenItemID); err != nil {
		t.Fatalf("создание позиции для поломки: %v", err)
	}
	err = e.db.QueryRow(context.Background(), `
		INSERT INTO wh_holding (item_id, holder_type, warehouse_id, qty_expected, qty_actual, outcome, source_file)
		VALUES ($1,'warehouse',$2, 3, 3, 'broken', 'тест') RETURNING id`, brokenItemID, whID).Scan(&brokenID)
	if err != nil {
		t.Fatalf("создание сломанной строки: %v", err)
	}
	_, body = e.do(http.MethodGet, "/spa/api/warehouse/reports/losses", admin, nil)
	if n := len(body["losses"].([]any)); n != 2 {
		t.Errorf("после добавления поломки в ведомости %d строк, ожидалось 2 "+
			"(поломка при qty_actual == qty_expected теряется?)", n)
	}
}

// TestCredentials — критерий приёмки №4.
func TestCredentials(t *testing.T) {
	e := setup(t)
	admin := e.token(1, roleAdmin)
	mol := e.token(2, roleMol)

	var itemID, holdingID int64
	ctx := context.Background()
	if err := e.db.QueryRow(ctx,
		`INSERT INTO wh_item (name, name_norm) VALUES ('Роутер Mikrotik','роутер mikrotik') RETURNING id`).
		Scan(&itemID); err != nil {
		t.Fatalf("позиция: %v", err)
	}
	if err := e.db.QueryRow(ctx, `
		INSERT INTO wh_holding (item_id, holder_type, user_fio, qty_expected, inv_number, inv_number_norm, source_file)
		VALUES ($1,'person','Иванов И.И.',1,'ДСК 0487','ДСК0487','тест') RETURNING id`, itemID).
		Scan(&holdingID); err != nil {
		t.Fatalf("строка закрепления: %v", err)
	}

	const secret = "СуперСекретныйПароль123"
	code, body := e.do(http.MethodPost, "/spa/api/warehouse/credentials", admin, map[string]any{
		"holdingId": holdingID, "kind": "admin",
		"title": "Веб-панель роутера", "login": "admin", "secret": secret,
	})
	if code != http.StatusCreated {
		t.Fatalf("создание доступа: %d %v", code, body)
	}
	credID := int64(body["credential"].(map[string]any)["id"].(float64))

	// ⚠ Секрета в ответе на создание быть не должно.
	if raw, _ := json.Marshal(body); bytes.Contains(raw, []byte(secret)) {
		t.Fatal("секрет утёк в ответе на создание доступа")
	}

	t.Run("секрет не виден в списке", func(t *testing.T) {
		code, body := e.do(http.MethodGet,
			fmt.Sprintf("/spa/api/warehouse/credentials?holding_id=%d", holdingID), admin, nil)
		if code != http.StatusOK {
			t.Fatalf("список доступов: %d", code)
		}
		raw, _ := json.Marshal(body)
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("СЕКРЕТ УТЁК В СПИСКЕ ДОСТУПОВ")
		}
		cred := body["credentials"].([]any)[0].(map[string]any)
		if cred["hasSecret"] != true {
			t.Error("hasSecret должен быть true")
		}
		if _, exists := cred["secret"]; exists {
			t.Error("в ответе есть поле secret")
		}
	})

	t.Run("в базе секрет зашифрован", func(t *testing.T) {
		var cipherText string
		if err := e.db.QueryRow(ctx,
			`SELECT secret_cipher FROM wh_credential WHERE id=$1`, credID).Scan(&cipherText); err != nil {
			t.Fatalf("чтение шифротекста: %v", err)
		}
		if bytes.Contains([]byte(cipherText), []byte(secret)) {
			t.Fatal("СЕКРЕТ ЛЕЖИТ В БАЗЕ ОТКРЫТЫМ ТЕКСТОМ")
		}
		if len(cipherText) < 4 || cipherText[:3] != "v1:" {
			t.Errorf("конверт имеет неожиданный вид: %q", cipherText[:min(10, len(cipherText))])
		}
	})

	t.Run("МОЛ не может смотреть пароль", func(t *testing.T) {
		code, _ := e.do(http.MethodPost,
			fmt.Sprintf("/spa/api/warehouse/credentials/%d/reveal", credID), mol, nil)
		if code != http.StatusForbidden {
			t.Errorf("МОЛ получил %d при попытке reveal, ожидался 403", code)
		}
	})

	t.Run("reveal отдаёт секрет и пишет журнал", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost,
			fmt.Sprintf("%s/spa/api/warehouse/credentials/%d/reveal", e.srv.URL, credID), nil)
		req.Header.Set("Authorization", "Bearer "+admin)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("reveal: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("reveal вернул %d", resp.StatusCode)
		}
		// Без no-store секрет осядет в кэше браузера и промежуточных прокси.
		if cc := resp.Header.Get("Cache-Control"); cc == "" || !bytes.Contains([]byte(cc), []byte("no-store")) {
			t.Errorf("Cache-Control = %q, ожидался no-store", cc)
		}

		var out map[string]any
		raw, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(raw, &out)
		if out["secret"] != secret {
			t.Fatalf("reveal вернул %q, ожидался исходный секрет", out["secret"])
		}

		var views int
		if err := e.db.QueryRow(ctx,
			`SELECT COUNT(*) FROM wh_credential_view_log WHERE credential_id=$1 AND user_id=1`,
			credID).Scan(&views); err != nil {
			t.Fatalf("журнал: %v", err)
		}
		if views != 1 {
			t.Errorf("в журнале %d записей, ожидалась 1", views)
		}
	})

	t.Run("to-rotate непуст после просмотра", func(t *testing.T) {
		code, body := e.do(http.MethodGet,
			"/spa/api/warehouse/credentials/to-rotate?user_id=1", admin, nil)
		if code != http.StatusOK {
			t.Fatalf("to-rotate: %d", code)
		}
		rows := body["credentials"].([]any)
		if len(rows) != 1 {
			t.Fatalf("to-rotate вернул %d строк, ожидалась 1", len(rows))
		}
	})

	t.Run("после смены пароля доступ уходит из to-rotate", func(t *testing.T) {
		code, _ := e.do(http.MethodPatch,
			fmt.Sprintf("/spa/api/warehouse/credentials/%d", credID), admin,
			map[string]any{"secret": "НовыйПароль456"})
		if code != http.StatusOK {
			t.Fatalf("смена пароля: %d", code)
		}
		_, body := e.do(http.MethodGet,
			"/spa/api/warehouse/credentials/to-rotate?user_id=1", admin, nil)
		if n := len(body["credentials"].([]any)); n != 0 {
			t.Errorf("после ротации to-rotate вернул %d строк, ожидалось 0", n)
		}
	})

	t.Run("удаление просмотренного доступа затирает, но не удаляет", func(t *testing.T) {
		code, body := e.do(http.MethodDelete,
			fmt.Sprintf("/spa/api/warehouse/credentials/%d", credID), admin, nil)
		if code != http.StatusOK || body["wiped"] != true {
			t.Fatalf("удаление вернуло %d %v, ожидалось затирание", code, body)
		}
		var cipherText *string
		var wipedAt *time.Time
		if err := e.db.QueryRow(ctx,
			`SELECT secret_cipher, wiped_at FROM wh_credential WHERE id=$1`, credID).
			Scan(&cipherText, &wipedAt); err != nil {
			t.Fatalf("строка доступа исчезла вместе с журналом: %v", err)
		}
		if cipherText != nil {
			t.Error("секрет не затёрт")
		}
		if wipedAt == nil {
			t.Error("wiped_at не проставлен")
		}
		var views int
		_ = e.db.QueryRow(ctx,
			`SELECT COUNT(*) FROM wh_credential_view_log WHERE credential_id=$1`, credID).Scan(&views)
		if views == 0 {
			t.Error("журнал просмотров стёрся вместе с доступом")
		}
	})

	// ⚠ Надгробие НЕ оживляется сменой пароля.
	//
	// Затёртая запись хранится только ради журнала просмотров: он относится
	// к ПРЕЖНЕМУ паролю. Раньше PATCH с новым секретом снимал wiped_at, запись
	// возвращалась к жизни — и чужой журнал просмотров оказывался приписан
	// новому паролю. Замерено: wiped_at снимался, а строка в
	// wh_credential_view_log оставалась на месте. После этого отчёт «кто видел
	// этот пароль» врёт, а именно за него сервис и отвечает.
	t.Run("смена пароля не оживляет затёртый доступ", func(t *testing.T) {
		code, body := e.do(http.MethodPatch,
			fmt.Sprintf("/spa/api/warehouse/credentials/%d", credID), admin,
			map[string]any{"secret": "НовыйПароль456"})
		if code == http.StatusOK {
			t.Fatalf("затёртый доступ ожил: PATCH вернул 200 %v", body)
		}
		if body["code"] != string(apperr.CodeCredentialWiped) {
			t.Errorf("код ошибки %v, ожидался %s", body["code"], apperr.CodeCredentialWiped)
		}

		var wipedAt *time.Time
		var cipherText *string
		_ = e.db.QueryRow(ctx,
			`SELECT wiped_at, secret_cipher FROM wh_credential WHERE id=$1`, credID).
			Scan(&wipedAt, &cipherText)
		if wipedAt == nil {
			t.Error("wiped_at снят — надгробие ожило")
		}
		if cipherText != nil {
			t.Error("в затёртую запись записан новый секрет")
		}

		// Журнал прежнего пароля обязан остаться при надгробии, а не при новом.
		var views int
		_ = e.db.QueryRow(ctx,
			`SELECT COUNT(*) FROM wh_credential_view_log WHERE credential_id=$1`, credID).Scan(&views)
		if views == 0 {
			t.Error("журнал просмотров прежнего пароля потерян")
		}
	})
}

// TestImportRealFiles — критерий приёмки №2: загружаются реальные файлы
// владельца, и контрольные суммы сходятся уже В БАЗЕ, а не только в парсере.
func TestImportRealFiles(t *testing.T) {
	e := setup(t)
	admin := e.token(1, roleAdmin)

	mc04 := inventFile(t, "МЦ.04")
	it := inventFile(t, "склад ИТ")
	lmz := inventFile(t, "ЛМЗ")
	if mc04 == "" || it == "" || lmz == "" {
		t.Skip("не найдены файлы выгрузок владельца")
	}

	upload := func(path, format string) map[string]any {
		t.Helper()
		return e.upload(admin, path, format)
	}

	r1 := upload(mc04, "mc04")
	if r1["totalQty"] != "3029" {
		t.Errorf("МЦ.04: totalQty = %v, ожидалось 3029", r1["totalQty"])
	}
	upload(it, "account10")
	upload(lmz, "account10")

	ctx := context.Background()

	// Суммы в БАЗЕ — то, что увидит владелец на экране.
	var totalQty string
	if err := e.db.QueryRow(ctx, `SELECT SUM(qty_expected)::text FROM wh_holding`).Scan(&totalQty); err != nil {
		t.Fatalf("сумма: %v", err)
	}
	if totalQty != "3912.000" && totalQty != "3912" {
		t.Errorf("сумма в базе = %s, ожидалось 3912", totalQty)
	}

	var items int
	if err := e.db.QueryRow(ctx, `SELECT COUNT(*) FROM wh_item`).Scan(&items); err != nil {
		t.Fatalf("каталог: %v", err)
	}
	if items != 881 {
		t.Errorf("каталог = %d позиций, ожидалась 881", items)
	}

	var people int
	_ = e.db.QueryRow(ctx,
		`SELECT COUNT(DISTINCT user_fio) FROM wh_holding WHERE holder_type='person'`).Scan(&people)
	if people != 163 {
		t.Errorf("сотрудников = %d, ожидалось 163", people)
	}

	// ⚠ Повторная загрузка того же файла НЕ должна удваивать остатки.
	upload(it, "account10")
	var afterRepeat string
	_ = e.db.QueryRow(ctx, `SELECT SUM(qty_expected)::text FROM wh_holding`).Scan(&afterRepeat)
	if afterRepeat != totalQty {
		t.Errorf("после повторной загрузки сумма стала %s (было %s) — импорт удваивает остатки",
			afterRepeat, totalQty)
	}

	// ⚠ ГЛАВНЫЙ ИНВАРИАНТ РЕЕСТРА: он РАСЩЕПЛЯЕТ агрегат на именованные
	// экземпляры, а НЕ добавляет количество. Строка с инвентарным номером
	// уменьшает агрегат на 1 и создаёт экземпляр — общая сумма не меняется.
	// Без этого правила 1105 номерных предметов легли бы ПОВЕРХ 3912 единиц,
	// и сводка сложила бы их вместе.
	if reg := inventFile(t, "инвентаризация"); reg != "" {
		before := totalQty
		lastID := e.count(`SELECT COALESCE(MAX(id), 0) FROM wh_holding`)
		rep := upload(reg, "registry")

		var after string
		_ = e.db.QueryRow(ctx, `SELECT SUM(qty_expected)::text FROM wh_holding`).Scan(&after)

		split := int(rep["holdingsSplit"].(float64))
		created := int(rep["holdingsCreated"].(float64))
		t.Logf("реестр: расщеплено %d, создано %d строк", split, created)

		// Номерные строки суммы не меняют. Новые «б/н» добавляют ожидаемое
		// количество; исключаем его вместе с отщеплёнными от них экземплярами.
		afterWithoutNewAggregates := e.scalar(`SELECT (SUM(qty_expected) - COALESCE(
			SUM(qty_expected + qty_split) FILTER (WHERE id > $1 AND inv_number_norm IS NULL), 0))::text
			FROM wh_holding`, lastID)
		if afterWithoutNewAggregates != before {
			t.Errorf("после импорта реестра сумма стала %s (была %s) — "+
				"реестр изменил количество помимо новых безномерных строк", afterWithoutNewAggregates, before)
		}

		// Номерные строки обязаны появиться и иметь ровно 0 или 1 в учёте.
		var numbered, badQty int
		_ = e.db.QueryRow(ctx,
			`SELECT COUNT(*) FROM wh_holding WHERE inv_number_norm IS NOT NULL`).Scan(&numbered)
		if numbered == 0 {
			t.Error("реестр не создал ни одной номерной строки")
		}
		_ = e.db.QueryRow(ctx,
			`SELECT COUNT(*) FROM wh_holding
			 WHERE inv_number_norm IS NOT NULL AND qty_expected NOT IN (0,1)`).Scan(&badQty)
		if badQty != 0 {
			t.Errorf("%d номерных строк имеют количество не 0 и не 1", badQty)
		}

		// Повторная загрузка реестра не должна расщеплять во второй раз:
		// иначе агрегаты уйдут в минус.
		upload(reg, "registry")
		var afterTwice string
		_ = e.db.QueryRow(ctx, `SELECT SUM(qty_expected)::text FROM wh_holding`).Scan(&afterTwice)
		if afterTwice != after {
			t.Errorf("повторный импорт реестра изменил сумму: %s против %s "+
				"(расщепление выполнено дважды)", afterTwice, after)
		}
		var negative int
		_ = e.db.QueryRow(ctx,
			`SELECT COUNT(*) FROM wh_holding WHERE qty_expected < 0`).Scan(&negative)
		if negative != 0 {
			t.Errorf("%d строк ушли в отрицательное количество", negative)
		}
	}

	// ⚠ Повторная загрузка не должна затирать уже проставленный факт.
	var someID int64
	_ = e.db.QueryRow(ctx,
		`SELECT id FROM wh_holding WHERE holder_type='warehouse' LIMIT 1`).Scan(&someID)
	if someID != 0 {
		code, _ := e.do(http.MethodPatch, fmt.Sprintf("/spa/api/warehouse/holdings/%d", someID),
			admin, map[string]any{"qtyActual": "7"})
		if code != http.StatusOK {
			t.Fatalf("проставление факта: %d", code)
		}
		upload(it, "account10")
		var qtyActual *string
		_ = e.db.QueryRow(ctx, `SELECT qty_actual::text FROM wh_holding WHERE id=$1`, someID).Scan(&qtyActual)
		if qtyActual == nil {
			t.Error("повторный импорт СТЁР проставленный владельцем факт")
		}
	}
}

// TestImportRepeatAfterSplit — повторная загрузка ПОСЛЕ расщепления реестра.
//
// ⚠ Почему это отдельный тест, а не строчка в TestImportRealFiles: там проверка
// повтора стоит ДО загрузки реестра (сначала `upload(it)` второй раз, потом
// реестр), поэтому расщеплённого состояния она не видит вовсе. А проверка
// повтора реестра, которая идёт после, смотрит только SUM(qty_expected)
// и отсутствие минусов — в qty_actual не заглядывает. Оба дефекта ниже жили
// ровно в этой слепой зоне и были найдены состязательным ревью.
//
// Для владельца повторная загрузка — штатное действие: перепутал формат,
// докрутил файл, перезалил. Она обязана быть безвредной в ЛЮБОМ порядке,
// а не только до реестра.
func TestImportRepeatAfterSplit(t *testing.T) {
	e := setup(t)
	admin := e.token(1, roleAdmin)

	mc04 := inventFile(t, "МЦ.04")
	it := inventFile(t, "склад ИТ")
	lmz := inventFile(t, "ЛМЗ")
	reg := inventFile(t, "инвентаризация")
	if mc04 == "" || it == "" || lmz == "" || reg == "" {
		t.Skip("не найдены выгрузки владельца")
	}

	e.upload(admin, mc04, "mc04")
	e.upload(admin, it, "account10")
	e.upload(admin, lmz, "account10")

	rep := e.upload(admin, reg, "registry")
	split := int(rep["holdingsSplit"].(float64))
	if split == 0 {
		t.Fatal("реестр не расщепил ни одного агрегата — проверять нечего")
	}

	afterSplit := e.scalar(`SELECT SUM(qty_expected)::text FROM wh_holding`)
	factsAfterSplit := e.count(`SELECT COUNT(*) FROM wh_holding WHERE qty_actual IS NOT NULL`)
	surplusAfterSplit := e.count(`SELECT COUNT(*) FROM wh_holding WHERE outcome = 'surplus'`)
	t.Logf("после реестра: сумма=%s, расщеплено=%d, с фактом=%d, излишков=%d",
		afterSplit, split, factsAfterSplit, surplusAfterSplit)

	// --- ДЕФЕКТ B ---------------------------------------------------------
	// Повторный реестр не должен объявлять расщеплённые предметы найденными.
	// Строка, отщеплённая от агрегата, НАШЛАСЬ в 1С: у неё qty_expected=1,
	// а qty_actual обязан остаться NULL — «ещё не считали». Излишком
	// (qty_expected=0, qty_actual=1, outcome=surplus) заводятся только те,
	// под кого агрегата не нашлось; это решение владельца, и его трогать нельзя.
	// Измерено до починки: с фактом 699 -> 1106, то есть 407 расщеплённых
	// строк объявлены найденным излишком без пересчёта.
	e.upload(admin, reg, "registry")

	if got := e.count(`SELECT COUNT(*) FROM wh_holding WHERE qty_actual IS NOT NULL`); got != factsAfterSplit {
		t.Errorf("повтор реестра выдумал факт: строк с фактом было %d, стало %d "+
			"(+%d предметов объявлены пересчитанными, хотя их никто не видел)",
			factsAfterSplit, got, got-factsAfterSplit)
	}
	if got := e.count(`SELECT COUNT(*) FROM wh_holding WHERE outcome = 'surplus'`); got != surplusAfterSplit {
		t.Errorf("повтор реестра создал излишки из воздуха: было %d, стало %d",
			surplusAfterSplit, got)
	}
	if n := e.count(`SELECT COUNT(*) FROM wh_holding
	                 WHERE inv_number_norm IS NOT NULL AND qty_expected = 1
	                   AND qty_actual IS NOT NULL`); n != 0 {
		t.Errorf("у %d расщеплённых строк стоит факт, хотя их не пересчитывали", n)
	}

	// Прежние инварианты повтора реестра обязаны сохраниться.
	if got := e.scalar(`SELECT SUM(qty_expected)::text FROM wh_holding`); got != afterSplit {
		t.Errorf("повтор реестра изменил сумму: %s -> %s", afterSplit, got)
	}
	if n := e.count(`SELECT COUNT(*) FROM wh_holding WHERE qty_expected < 0`); n != 0 {
		t.Errorf("%d строк ушли в отрицательное количество", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM wh_holding
	                 WHERE inv_number_norm IS NOT NULL AND qty_expected NOT IN (0,1)`); n != 0 {
		t.Errorf("%d номерных строк имеют количество не 0 и не 1", n)
	}
}

// TestImportRepeatRestoresSplitAggregates — сторож починки, для которой владелец
// согласовал колонку qty_split (миграция 00002).
//
// Измерено ДО починки против настоящей БД:
//
//	МЦ.04 + два счёта 10                 -> SUM(qty_expected) = 3912.000
//	реестр (расщеплено 407)              -> 3912.000
//	ПОВТОРНО МЦ.04                       -> 4319.000   ровно +407
//
// splitAggregate уменьшает агрегат на 1 за каждый найденный номер, а выгрузка
// 1С про расщепление не знает и приносит ПОЛНОЕ количество: upsertHolding
// с keepQty=false записывает его как есть, и каждая расщеплённая единица
// начинает считаться дважды — в агрегате и в экземпляре.
//
// ⚠ Почему нельзя восстановить отнятое, не храня его. Три причины, каждая
// проверена по коду:
//   - выбор уменьшаемого агрегата зависел от ПОРЯДКА обработки 2174 строк
//     реестра (ORDER BY qty_expected DESC по полю, которое тут же меняется,
//     import_service.go, splitAggregate, шаги 1 и 2) — из финального состояния он не выводится;
//   - предикат «был расщеплён» отсутствует: POST /holdings проверяет только
//     неотрицательность (inventory_service.go), поэтому номерная строка,
//     заведённая владельцем руками, от расщеплённой неотличима;
//   - шаг 1 сопоставления не сравнивает warehouse_id и room, а позиции, общие
//     для складов ИТ и ЛМЗ, в данных есть.
func TestImportRepeatRestoresSplitAggregates(t *testing.T) {
	e := setup(t)
	admin := e.token(1, roleAdmin)

	mc04 := inventFile(t, "МЦ.04")
	it := inventFile(t, "склад ИТ")
	lmz := inventFile(t, "ЛМЗ")
	reg := inventFile(t, "инвентаризация")
	if mc04 == "" || it == "" || lmz == "" || reg == "" {
		t.Skip("не найдены выгрузки владельца")
	}

	e.upload(admin, mc04, "mc04")
	e.upload(admin, it, "account10")
	e.upload(admin, lmz, "account10")
	e.upload(admin, reg, "registry")
	afterSplit := e.scalar(`SELECT SUM(qty_expected)::text FROM wh_holding`)

	e.upload(admin, mc04, "mc04")
	if got := e.scalar(`SELECT SUM(qty_expected)::text FROM wh_holding`); got != afterSplit {
		t.Errorf("повтор МЦ.04 после расщепления изменил сумму: %s -> %s "+
			"(расщепление отменено, единицы считаются дважды)", afterSplit, got)
	}
	e.upload(admin, it, "account10")
	e.upload(admin, lmz, "account10")
	if got := e.scalar(`SELECT SUM(qty_expected)::text FROM wh_holding`); got != afterSplit {
		t.Errorf("повтор всех трёх выгрузок изменил сумму: %s -> %s", afterSplit, got)
	}
}

// TestResolutionPatchContract фиксирует контракт списания, на который опирается
// экран «Решение по расхождению» во фронте.
//
// ⚠ Тонкость, из-за которой тест и написан: снять решение можно только ПУСТОЙ
// СТРОКОЙ, а не JSON null. Поле в DTO — *string, и ветка обновления входит по
// условию upd.Resolution != nil: null даёт нулевой указатель, ветка не
// срабатывает, и запрос молча НИЧЕГО не меняет. Фронт, отправивший null в
// расчёте «очистить», получил бы 200 и прежнее значение в базе — то есть
// молчаливый отказ, который на экране выглядит как успех.
//
// Так ведут себя ВСЕ *string-поля этого DTO, поэтому поведение не правится
// точечно, а закрепляется тестом и комментарием в типе фронта.
func TestResolutionPatchContract(t *testing.T) {
	e := setup(t)
	admin := e.token(1, roleAdmin)

	code, body := e.do(http.MethodPost, "/spa/api/warehouse/warehouses", admin,
		map[string]any{"name": "Склад проверки списания", "responsibleUserId": nil})
	if code != http.StatusCreated {
		t.Fatalf("создание склада: %d %v", code, body)
	}
	whID := int64(body["warehouse"].(map[string]any)["id"].(float64))

	// Позицию каталога обычно создаёт импорт — здесь напрямую, как в
	// TestOwnerCountScenario.
	var itemID int64
	if err := e.db.QueryRow(context.Background(),
		`INSERT INTO wh_item (name, name_norm) VALUES ($1,$2) RETURNING id`,
		"Стул для проверки списания", "стул для проверки списания").Scan(&itemID); err != nil {
		t.Fatalf("создание позиции: %v", err)
	}

	// Строка с расхождением: числится 5, найдено 2.
	code, body = e.do(http.MethodPost, "/spa/api/warehouse/holdings", admin, map[string]any{
		"itemId": itemID, "holderType": "warehouse",
		"warehouseId": whID, "qtyExpected": "5",
	})
	if code != http.StatusCreated {
		t.Fatalf("создание строки: %d %v", code, body)
	}
	id := int64(body["holding"].(map[string]any)["id"].(float64))
	path := fmt.Sprintf("/spa/api/warehouse/holdings/%d", id)

	if code, _ := e.do(http.MethodPatch, path, admin,
		map[string]any{"qtyActual": "2", "outcome": "missing"}); code != http.StatusOK {
		t.Fatalf("проставление факта: %d", code)
	}

	// 1. Списание проставляется вместе с автором и временем — без этого
	//    «нашли недостачу» есть, а «кто списал» нет.
	code, _ = e.do(http.MethodPatch, path, admin, map[string]any{
		"resolution": "written_off", "resolutionRef": "акт № 14 от 29.07.2026",
	})
	if code != http.StatusOK {
		t.Fatalf("списание: %d", code)
	}
	var resolution, ref *string
	var resolvedBy *int64
	var resolvedAt *time.Time
	if err := e.db.QueryRow(context.Background(),
		`SELECT resolution, resolution_ref, resolved_by_id, resolved_at
		   FROM wh_holding WHERE id = $1`, id).
		Scan(&resolution, &ref, &resolvedBy, &resolvedAt); err != nil {
		t.Fatalf("чтение строки: %v", err)
	}
	if resolution == nil || *resolution != "written_off" {
		t.Errorf("resolution = %v, ожидалось written_off", resolution)
	}
	if ref == nil || *ref != "акт № 14 от 29.07.2026" {
		t.Errorf("resolution_ref = %v — номер документа не сохранился", ref)
	}
	if resolvedBy == nil || *resolvedBy != 1 {
		t.Errorf("resolved_by_id = %v, ожидался 1: списание без автора неотслеживаемо", resolvedBy)
	}
	if resolvedAt == nil {
		t.Error("resolved_at пуст — непонятно, когда списали")
	}

	// 2. Неизвестный код решения отклоняется, а не пишется в базу как есть.
	if code, _ := e.do(http.MethodPatch, path, admin,
		map[string]any{"resolution": "выкинули"}); code == http.StatusOK {
		t.Error("неизвестный код решения принят — ValidResolution не сработал")
	}

	// 3. JSON null НЕ снимает решение. Это и есть та самая ловушка.
	if code, _ := e.do(http.MethodPatch, path, admin,
		map[string]any{"resolution": nil, "note": "трогаем что-то ещё"}); code != http.StatusOK {
		t.Fatalf("патч с null: %d", code)
	}
	var afterNull *string
	_ = e.db.QueryRow(context.Background(),
		`SELECT resolution FROM wh_holding WHERE id = $1`, id).Scan(&afterNull)
	if afterNull == nil {
		t.Error("JSON null СНЯЛ решение: контракт изменился — во фронте " +
			"(types/features/warehouse.ts, WarehouseHoldingPatch.resolution) " +
			"написано обратное, и комментарий надо править вместе с этим тестом")
	}

	// 4. Пустая строка снимает — этим и пользуется фронт.
	if code, _ := e.do(http.MethodPatch, path, admin,
		map[string]any{"resolution": ""}); code != http.StatusOK {
		t.Fatalf("патч пустой строкой: %d", code)
	}
	var afterEmpty *string
	_ = e.db.QueryRow(context.Background(),
		`SELECT resolution FROM wh_holding WHERE id = $1`, id).Scan(&afterEmpty)
	if afterEmpty != nil {
		t.Errorf("пустая строка не сняла решение (осталось %v) — кнопка «Решения нет» "+
			"во фронте перестанет работать", *afterEmpty)
	}
}

// TestImportManualSheet — ЕДИНСТВЕННЫЙ формат, чей путь до базы не был покрыт
// ничем (§1 п.3 плана мегасессии 5: `grep manual integration_test.go` давал 0).
// Парсер ручной ведомости покрыт (parser_test.go: 36 строк, 16 с фактом), а вот
// что из этого в итоге ЛОЖИТСЯ В БАЗУ — не проверял никто.
//
// Почему это важнее, чем выглядит: ручная ведомость — единственный файл, куда
// владелец вписывает результат своей работы РУКАМИ. Остальные пять приезжают
// из 1С, и при ошибке импорта их можно перезалить. Потерянный здесь факт
// восстанавливать неоткуда — придётся пересчитывать заново по кабинетам.
//
// Ключевое отличие формата: он ОБОГАЩАЕТ, а не создаёт. 205 единиц склада ИТ
// уже пришли счётом 10; ведомость обязана только проставить на них факт.
func TestImportManualSheet(t *testing.T) {
	e := setup(t)
	admin := e.token(1, roleAdmin)

	it := inventFile(t, "склад ИТ")
	lmz := inventFile(t, "ЛМЗ")
	manual := inventFile(t, "сверка остатков")
	if it == "" || lmz == "" || manual == "" {
		t.Skip("не найдены выгрузки владельца")
	}

	// Оба склада намеренно: ЛМЗ здесь не декорация, а КОНТРОЛЬ. Ведомость
	// описывает только склад ИТ, и часть позиций числится на обоих складах —
	// значит UPDATE, потерявший привязку к складу, будет виден как факт,
	// проставленный на ЛМЗ. Проверять надо именно это, а не только
	// «16 строк обновилось».
	e.upload(admin, it, "account10")
	e.upload(admin, lmz, "account10")

	const itWarehouse = "Склад ИТ"

	shared := e.count(`SELECT COUNT(*) FROM (
	                     SELECT item_id FROM wh_holding WHERE holder_type='warehouse'
	                     GROUP BY item_id HAVING COUNT(DISTINCT warehouse_id) > 1) x`)
	if shared == 0 {
		t.Log("⚠ позиций, общих для двух складов, нет — проверка на утечку между " +
			"складами в этом прогоне ничего не доказывает")
	}

	sumBefore := e.scalar(`SELECT SUM(qty_expected)::text FROM wh_holding`)
	rowsBefore := e.count(`SELECT COUNT(*) FROM wh_holding`)
	if n := e.count(`SELECT COUNT(*) FROM wh_holding WHERE qty_actual IS NOT NULL`); n != 0 {
		t.Fatalf("до ведомости факт не должен стоять нигде, а стоит у %d строк", n)
	}

	rep := e.upload(admin, manual, "manual")

	// --- 1. Ведомость НЕ создаёт остатков ----------------------------------
	// Если этот блок падает — значит факт владельца лёг новой строкой рядом
	// с учётной, и в сводке одна и та же вещь посчитана дважды.
	if created := int(rep["holdingsCreated"].(float64)); created != 0 {
		t.Errorf("ручная ведомость создала %d строк — она обязана только обогащать", created)
	}
	if got := e.scalar(`SELECT SUM(qty_expected)::text FROM wh_holding`); got != sumBefore {
		t.Errorf("сумма числящегося изменилась: было %s, стало %s — ведомость не должна "+
			"менять qty_expected, она про ФАКТ", sumBefore, got)
	}
	if got := e.count(`SELECT COUNT(*) FROM wh_holding`); got != rowsBefore {
		t.Errorf("число строк изменилось: было %d, стало %d", rowsBefore, got)
	}

	// --- 2. Факт проставлен ровно там, где он есть в ведомости -------------
	updated := int(rep["holdingsUpdated"].(float64))
	if updated != 16 {
		t.Errorf("обновлено %d строк, ожидалось 16 (столько строк с фактом в файле "+
			"владельца — зафиксировано parser_test.go, TestManualSheet)", updated)
	}
	withFact := e.count(`SELECT COUNT(*) FROM wh_holding WHERE qty_actual IS NOT NULL`)
	if withFact != updated {
		t.Errorf("в базе с фактом %d строк, а отчёт сообщил об %d — отчёт врёт", withFact, updated)
	}

	// Позиции каталога уже созданы счётом 10, значит ведомость не создаёт ни одной.
	// Раньше здесь стоял бы размер файла: отчёт показывал len(index) под меткой
	// «Позиций создано» и на повторной загрузке честно сообщал «создано 36».
	if n := int(rep["itemsCreated"].(float64)); n != 0 {
		t.Errorf("«Позиций создано» = %d, ожидалось 0: все позиции уже в каталоге "+
			"после счёта 10", n)
	}

	// --- 3. КОНТРОЛЬ: чужой склад не задет --------------------------------
	// UPDATE в enrich ограничен складом только когда warehouse_id разрешился.
	// На данных владельца он разрешается всегда, и этот тест фиксирует, что так
	// и остаётся: если ограничение потеряется, факт расползётся по складам.
	if n := e.count(`
		SELECT COUNT(*) FROM wh_holding h
		JOIN wh_warehouse w ON w.id = h.warehouse_id
		WHERE h.qty_actual IS NOT NULL AND w.name <> $1`, itWarehouse); n != 0 {
		t.Errorf("факт проставлен на %d строках ЧУЖОГО склада — ведомость описывает "+
			"только «%s»; UPDATE в enrich потерял привязку к складу", n, itWarehouse)
	}

	// --- 4. Что именно записано в обогащённых строках ---------------------
	if n := e.count(`SELECT COUNT(*) FROM wh_holding
	                 WHERE qty_actual IS NOT NULL AND outcome IS NULL`); n != 0 {
		t.Errorf("%d строк с фактом без outcome: ограничение chk_wh_holding_counted "+
			"их пропустит, но в ведомость потерь они не попадут", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM wh_holding
	                 WHERE qty_actual IS NOT NULL AND counted_at IS NULL`); n != 0 {
		t.Errorf("%d строк с фактом без counted_at — непонятно, когда считали", n)
	}
	// ⚠ Не баг, а зафиксированное отличие от ручного ввода: у импорта нет
	// действующего лица, поэтому counted_by_id остаётся пустым, тогда как PATCH
	// его заполняет (holding_repository.go, ветка upd.QtyActual != nil).
	// Если однажды сюда начнёт попадать id — значит импорт приписал пересчёт
	// человеку, который его не делал.
	if n := e.count(`SELECT COUNT(*) FROM wh_holding
	                 WHERE qty_actual IS NOT NULL AND counted_by_id IS NOT NULL`); n != 0 {
		t.Errorf("%d строк, где импорт приписал пересчёт пользователю: у импорта "+
			"нет действующего лица, counted_by_id обязан остаться пустым", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM wh_holding
	                 WHERE note IS NOT NULL AND note <> ''`); n == 0 {
		t.Error("ни одного примечания: в ведомости владельца они есть, " +
			"колонка «Примечание» потерялась")
	}

	// --- 5. Предупреждения говорят правду ---------------------------------
	// Раньше обе причины «факт не проставлен» давали один текст «строка учёта
	// не найдена». На файле владельца это 20 предупреждений о ненайденных
	// строках, хотя не найдено ни одной: просто 20 позиций ещё не пересчитаны.
	// Настоящая потеря факта в таком шуме неразличима.
	warns, _ := rep["warnings"].([]any)
	notFound, summary := 0, 0
	for _, w := range warns {
		s, _ := w.(string)
		switch {
		case strings.Contains(s, "подходящей строки склада нет"):
			notFound++
		case strings.Contains(s, "не заполнен факт"):
			summary++
		}
	}
	if notFound != 0 {
		t.Errorf("%d предупреждений «строки склада нет», хотя счёт 10 загружен "+
			"и все 16 фактов проставлены: %v", notFound, warns)
	}
	if summary != 1 {
		t.Errorf("ожидалась ровно одна сводная строка о незаполненном факте, "+
			"получено %d: %v", summary, warns)
	}

	// --- 6. Повторная загрузка ведомости ничего не портит -----------------
	// Владелец грузит ведомость по мере пересчёта, тем же файлом. Повтор обязан
	// быть безвредным, и уже проставленный факт перезаписан быть не должен.
	rep2 := e.upload(admin, manual, "manual")
	if n := int(rep2["holdingsUpdated"].(float64)); n != 0 {
		t.Errorf("повторная загрузка обновила %d строк — факт перезаписывается", n)
	}
	if got := e.count(`SELECT COUNT(*) FROM wh_holding WHERE qty_actual IS NOT NULL`); got != withFact {
		t.Errorf("после повтора строк с фактом %d, было %d", got, withFact)
	}
	if got := e.scalar(`SELECT SUM(qty_expected)::text FROM wh_holding`); got != sumBefore {
		t.Errorf("после повтора сумма стала %s, было %s", got, sumBefore)
	}

	// --- 7. Факт доехал до ведомости потерь -------------------------------
	// Это то, за чем владелец пришёл: недостача из его ведомости обязана
	// оказаться в отчёте, по которому потом составляется акт списания.
	//
	// В ведомость попадают не все посчитанные строки, а только расхождения:
	// wh_v_losses фильтрует qty_actual <> qty_expected OR outcome IN
	// ('broken','in_repair'). На файле владельца из 16 проставленных фактов
	// 9 сошлись (outcome=found) и 7 дали недостачу от −1 до −3.
	code, body := e.do(http.MethodGet,
		"/spa/api/warehouse/reports/losses?limit=100&offset=0", admin, nil)
	if code != http.StatusOK {
		t.Fatalf("ведомость потерь вернула %d", code)
	}
	losses, _ := body["losses"].([]any)
	if len(losses) == 0 {
		t.Fatal("ведомость потерь пуста, хотя в ведомости владельца есть недостача")
	}
	if len(losses) != 7 {
		t.Errorf("в ведомости потерь %d строк, ожидалось 7 (столько расхождений даёт "+
			"файл владельца; 9 из 16 фактов сходятся и в потери не идут)", len(losses))
	}

	shortage := 0
	for _, r := range losses {
		row, _ := r.(map[string]any)
		kind, _ := row["discrepancyKind"].(string)
		diff, _ := row["qtyDiff"].(string)
		if kind == "недостача" {
			shortage++
			// Знак — это разница между «не хватает» и «лишнее». Ошибка в нём
			// превратила бы недостачу в излишек в акте списания.
			if !strings.HasPrefix(diff, "-") {
				t.Errorf("строка «%v»: вид «недостача», а qtyDiff = %s (ожидался минус)",
					row["name"], diff)
			}
		}
	}
	if shortage != 7 {
		t.Errorf("строк с видом «недостача» %d, ожидалось 7", shortage)
	}
	t.Logf("ведомость потерь: %d строк, из них недостача %d", len(losses), shortage)
}
