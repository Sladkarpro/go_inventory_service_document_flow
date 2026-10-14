package repository

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go_warehouse_service/internal/helper"
	"go_warehouse_service/internal/model"
)

// UserRepository — реплика справочника пользователей монолита.
//
// ⚠ Наполняется ДВУМЯ путями, и второй обязателен.
// События RabbitMQ (user.upserted / user.deleted) описывают только ИЗМЕНЕНИЯ.
// Topic exchange выбрасывает сообщение, если под него нет очереди, поэтому всё,
// что монолит опубликовал до первого запуска нашего consumer'а, потеряно —
// реплика стартует ПУСТОЙ. Канбану это безразлично, он знает user_id из своих
// таблиц; мы же матчим сотрудников по ФИО из выгрузки, и на пустой реплике
// первый импорт сматчил бы НОЛЬ человек из 163.
// Поэтому при старте выполняется массовая загрузка всех активных пользователей.
type UserRepository struct {
	db    *pgxpool.Pool
	clock helper.Clock
}

func NewUserRepository(db *pgxpool.Pool, clk helper.Clock) *UserRepository {
	return &UserRepository{db: db, clock: clk}
}

const userColumns = `id, login, lastname, firstname, patronymic, deleted_at, synced_at`

func scanUser(row pgx.Row) (model.User, error) {
	var u model.User
	err := row.Scan(&u.ID, &u.Login, &u.Lastname, &u.Firstname, &u.Patronymic, &u.DeletedAt, &u.SyncedAt)
	return u, err
}

func (r *UserRepository) GetByID(ctx context.Context, id int64) (*model.User, error) {
	u, err := scanUser(r.db.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = $1`, id))
	if err != nil {
		return nil, NormalizeError(err)
	}
	return &u, nil
}

func (r *UserRepository) GetByIDs(ctx context.Context, ids []int64) ([]model.User, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := r.db.Query(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// FIOIndex строит карту «нормализованное ФИО → id» для матчинга импорта.
//
// ⚠ Выражение LOWER(TRIM(...)) обязано совпадать с индексом ix_users_fio
// и с model.NormalizeFIO. Иначе либо промах по индексу и 1774 полных
// просмотра таблицы на один импорт, либо ноль совпадений.
//
// Уволенные (deleted_at IS NOT NULL) в карту НЕ попадают: закреплять за ними
// имущество при новом пересчёте смысла нет, а вот их старые строки остаются
// и попадают в ведомость потерь — это и нужно.
func (r *UserRepository) FIOIndex(ctx context.Context) (map[string]int64, error) {
	rows, err := r.db.Query(ctx, `
		SELECT LOWER(TRIM(lastname || ' ' || firstname || ' ' || COALESCE(patronymic, ''))), id
		FROM users
		WHERE deleted_at IS NULL
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	index := make(map[string]int64, 512)
	for rows.Next() {
		var fio string
		var id int64
		if err := rows.Scan(&fio, &id); err != nil {
			return nil, err
		}
		// Схлопываем внутренние двойные пробелы так же, как NormalizeFIO:
		// у сотрудников без отчества конкатенация даёт хвостовой пробел.
		index[model.NormalizeFIO(fio)] = id
	}
	return index, rows.Err()
}

// UpsertUsers — массовая вставка через временную таблицу и COPY.
// На bootstrap приезжает вся организация разом, построчный INSERT был бы
// на порядок медленнее.
func (r *UserRepository) UpsertUsers(ctx context.Context, users []model.User) error {
	if len(users) == 0 {
		return nil
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	if _, err = tx.Exec(ctx, `
		CREATE TEMP TABLE users_tmp (LIKE users INCLUDING DEFAULTS) ON COMMIT DROP
	`); err != nil {
		return err
	}

	now := r.clock.Now()
	if _, err = tx.CopyFrom(ctx,
		pgx.Identifier{"users_tmp"},
		[]string{"id", "login", "lastname", "firstname", "patronymic", "deleted_at", "synced_at"},
		pgx.CopyFromSlice(len(users), func(i int) ([]any, error) {
			u := users[i]
			return []any{u.ID, u.Login, u.Lastname, u.Firstname, u.Patronymic, u.DeletedAt, now}, nil
		}),
	); err != nil {
		return err
	}

	// DISTINCT ON: в одной пачке может приехать два события об одном человеке,
	// и ON CONFLICT упал бы с «cannot affect row a second time».
	if _, err = tx.Exec(ctx, `
		INSERT INTO users (id, login, lastname, firstname, patronymic, deleted_at, synced_at)
		SELECT DISTINCT ON (id) id, login, lastname, firstname, patronymic, deleted_at, synced_at
		FROM users_tmp
		ORDER BY id, synced_at DESC
		ON CONFLICT (id) DO UPDATE SET
			login      = EXCLUDED.login,
			lastname   = EXCLUDED.lastname,
			firstname  = EXCLUDED.firstname,
			patronymic = EXCLUDED.patronymic,
			deleted_at = EXCLUDED.deleted_at,
			synced_at  = EXCLUDED.synced_at
	`); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

func (r *UserRepository) MarkUserDeleted(ctx context.Context, userID int64, deletedAt time.Time) error {
	_, err := r.db.Exec(ctx, `
		UPDATE users SET deleted_at = $2, synced_at = $3 WHERE id = $1
	`, userID, deletedAt, r.clock.Now())
	return err
}

// LastSyncAge — возраст последней успешной синхронизации.
//
// Отдаётся в /healthz намеренно: из трёх сценариев отказа (очередь встала,
// Symfony недоступна, сервис лежал) ни один не ломает экран — он просто
// показывает устаревшие данные. Без этой величины деградация молчит.
func (r *UserRepository) LastSyncAge(ctx context.Context) (*time.Duration, error) {
	var last *time.Time
	if err := r.db.QueryRow(ctx, `SELECT MAX(synced_at) FROM users`).Scan(&last); err != nil {
		return nil, err
	}
	if last == nil {
		return nil, nil
	}
	age := r.clock.Now().Sub(*last)
	return &age, nil
}

func (r *UserRepository) Count(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

type DepartmentRepository struct {
	db    *pgxpool.Pool
	clock helper.Clock
}

func NewDepartmentRepository(db *pgxpool.Pool, clk helper.Clock) *DepartmentRepository {
	return &DepartmentRepository{db: db, clock: clk}
}

func (r *DepartmentRepository) List(ctx context.Context) ([]model.Department, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, name, parent_id, deleted_at, synced_at
		FROM departments
		WHERE deleted_at IS NULL
		ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.Department
	for rows.Next() {
		var d model.Department
		if err := rows.Scan(&d.ID, &d.Name, &d.ParentID, &d.DeletedAt, &d.SyncedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// NameIndex — карта «нормализованное название → id» для привязки строк ОСВ,
// в которых подразделение приходит только текстом.
func (r *DepartmentRepository) NameIndex(ctx context.Context) (map[string]int64, error) {
	deps, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	index := make(map[string]int64, len(deps))
	for _, d := range deps {
		index[model.NormalizeName(d.Name)] = d.ID
	}
	return index, nil
}

func (r *DepartmentRepository) Upsert(ctx context.Context, deps []model.Department) error {
	if len(deps) == 0 {
		return nil
	}
	now := r.clock.Now()
	batch := &pgx.Batch{}
	for _, d := range deps {
		batch.Queue(`
			INSERT INTO departments (id, name, parent_id, deleted_at, synced_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (id) DO UPDATE SET
				name       = EXCLUDED.name,
				parent_id  = EXCLUDED.parent_id,
				deleted_at = EXCLUDED.deleted_at,
				synced_at  = EXCLUDED.synced_at
		`, d.ID, d.Name, d.ParentID, d.DeletedAt, now)
	}
	res := r.db.SendBatch(ctx, batch)
	defer func() { _ = res.Close() }()
	for range deps {
		if _, err := res.Exec(); err != nil {
			return err
		}
	}
	return nil
}
