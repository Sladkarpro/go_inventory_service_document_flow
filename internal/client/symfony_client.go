package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go_warehouse_service/internal/config"
	"go_warehouse_service/internal/model"
)

// SymfonyClient читает справочники монолита через внутренний API.
//
// ⚠ Требует НОВОГО контроллера в Symfony:
//
//	#[Route('/api/internal/warehouse')]
//	GET /users        — все активные, для bootstrap и матчинга ФИО
//	GET /departments  — дерево подразделений
//
// Существующий KanbanUserController умеет только ?ids= и для нашей задачи
// не годится: мы стартуем с ФИО строкой и никаких id ещё не знаем.
// Защита — X-API-Key со СВОИМ ключом (warehouse_internal_api_key), отдельный
// контроллер, чтобы не зависеть от контракта канбана.
type SymfonyClient struct {
	client  *http.Client
	baseURL string
	apiKey  string
}

func NewSymfonyClient(cfg *config.Config) *SymfonyClient {
	return &SymfonyClient{
		// Bootstrap тянет всю организацию разом — 5 секунд канбана здесь мало.
		client:  &http.Client{Timeout: 30 * time.Second},
		baseURL: strings.TrimRight(cfg.SymfonyInternalApiUrl, "/"),
		apiKey:  cfg.SymfonyInternalApiKey,
	}
}

func (c *SymfonyClient) configured() bool {
	return c.baseURL != "" && c.apiKey != ""
}

func (c *SymfonyClient) get(ctx context.Context, path string, out any) error {
	if !c.configured() {
		return fmt.Errorf("внутренний API Symfony не настроен")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("запрос %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s вернул %d: %s", path, resp.StatusCode, string(body))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// userDTO повторяет форму ответа монолита: snake_case, как в KanbanUserController.
type userDTO struct {
	ID         int64   `json:"id"`
	Login      string  `json:"login"`
	Lastname   string  `json:"lastname"`
	Firstname  string  `json:"firstname"`
	Patronymic *string `json:"patronymic"`
	DeletedAt  *string `json:"deleted_at"`
}

// FetchAllUsers — массовая загрузка при старте.
//
// ⚠ БЕЗ НЕЁ ПЕРВЫЙ ИМПОРТ ДАЁТ НОЛЬ СМАТЧЕННЫХ СОТРУДНИКОВ.
// Topic exchange выбрасывает сообщение, если под него нет очереди: всё, что
// монолит опубликовал до первого запуска нашего consumer'а, потеряно, и реплика
// стартует пустой. Канбану это безразлично — он знает user_id из своих таблиц;
// мы же матчим людей по ФИО из выгрузки.
func (c *SymfonyClient) FetchAllUsers(ctx context.Context) ([]model.User, error) {
	var raw []userDTO
	if err := c.get(ctx, "/api/internal/warehouse/users", &raw); err != nil {
		return nil, err
	}
	out := make([]model.User, 0, len(raw))
	for _, u := range raw {
		out = append(out, model.User{
			ID: u.ID, Login: u.Login, Lastname: u.Lastname,
			Firstname: u.Firstname, Patronymic: u.Patronymic,
			DeletedAt: parseTimePtr(u.DeletedAt),
		})
	}
	return out, nil
}

type departmentDTO struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	ParentID *int64 `json:"parent_id"`
}

// FetchDepartments — справочник подразделений целиком.
// Событий по организациям монолит не публикует, а подразделений около десятка
// и меняются они при реорганизациях: событийная модель здесь избыточна.
func (c *SymfonyClient) FetchDepartments(ctx context.Context) ([]model.Department, error) {
	var raw []departmentDTO
	if err := c.get(ctx, "/api/internal/warehouse/departments", &raw); err != nil {
		return nil, err
	}
	out := make([]model.Department, 0, len(raw))
	for _, d := range raw {
		out = append(out, model.Department{ID: d.ID, Name: d.Name, ParentID: d.ParentID})
	}
	return out, nil
}

func parseTimePtr(s *string) *time.Time {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, *s); err == nil {
			u := t.UTC().Truncate(time.Second)
			return &u
		}
	}
	return nil
}
