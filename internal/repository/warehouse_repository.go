package repository

import (
	"context"

	sq "github.com/Masterminds/squirrel"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go_warehouse_service/internal/apperr"
	"go_warehouse_service/internal/model"
)

type WarehouseRepository struct {
	db *pgxpool.Pool
}

func NewWarehouseRepository(db *pgxpool.Pool) *WarehouseRepository {
	return &WarehouseRepository{db: db}
}

const warehouseColumns = `w.id, w.name, w.department_id, w.department_name, w.responsible_user_id,
	w.location, w.is_active, w.note, w.created_at, w.updated_at`

func scanWarehouse(row pgx.Row) (model.Warehouse, error) {
	var w model.Warehouse
	err := row.Scan(&w.ID, &w.Name, &w.DepartmentID, &w.DepartmentName, &w.ResponsibleUserID,
		&w.Location, &w.IsActive, &w.Note, &w.CreatedAt, &w.UpdatedAt)
	return w, err
}

func (r *WarehouseRepository) List(ctx context.Context, onlyActive bool) ([]model.Warehouse, error) {
	q := `SELECT ` + warehouseColumns + `,
	       u.id, u.login, u.lastname, u.firstname, u.patronymic
	      FROM wh_warehouse w
	      LEFT JOIN users u ON u.id = w.responsible_user_id`
	if onlyActive {
		q += ` WHERE w.is_active`
	}
	q += ` ORDER BY w.name`

	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]model.Warehouse, 0, 8)
	for rows.Next() {
		var w model.Warehouse
		var uid *int64
		var login, lastname, firstname *string
		var patronymic *string
		if err := rows.Scan(&w.ID, &w.Name, &w.DepartmentID, &w.DepartmentName, &w.ResponsibleUserID,
			&w.Location, &w.IsActive, &w.Note, &w.CreatedAt, &w.UpdatedAt,
			&uid, &login, &lastname, &firstname, &patronymic); err != nil {
			return nil, err
		}
		if uid != nil {
			w.ResponsibleUser = &model.User{
				ID: *uid, Login: derefStr(login), Lastname: derefStr(lastname),
				Firstname: derefStr(firstname), Patronymic: patronymic,
			}
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (r *WarehouseRepository) GetByID(ctx context.Context, id int64) (*model.Warehouse, error) {
	w, err := scanWarehouse(r.db.QueryRow(ctx,
		`SELECT `+warehouseColumns+` FROM wh_warehouse w WHERE w.id = $1`, id))
	if err != nil {
		return nil, NotFoundAs(err, apperr.CodeWarehouseNotFound)
	}
	return &w, nil
}

type WarehouseInput struct {
	Name              string
	DepartmentID      *int64
	DepartmentName    *string
	ResponsibleUserID *int64
	Location          *string
	IsActive          *bool
	Note              *string
}

func (r *WarehouseRepository) Create(ctx context.Context, in WarehouseInput) (*model.Warehouse, error) {
	active := true
	if in.IsActive != nil {
		active = *in.IsActive
	}
	var id int64
	err := r.db.QueryRow(ctx, `
		INSERT INTO wh_warehouse (name, department_id, department_name, responsible_user_id,
		                          location, is_active, note)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id
	`, model.CleanText(in.Name), in.DepartmentID, in.DepartmentName, in.ResponsibleUserID,
		in.Location, active, in.Note).Scan(&id)
	if err != nil {
		if IsUniqueViolation(err, "uq_wh_warehouse_name") {
			return nil, apperr.New(apperr.CodeWarehouseNameTaken, "warehouse name taken")
		}
		return nil, err
	}
	return r.GetByID(ctx, id)
}

func (r *WarehouseRepository) Update(ctx context.Context, id int64, in WarehouseInput) (*model.Warehouse, error) {
	q := sq.Update("wh_warehouse").PlaceholderFormat(sq.Dollar).Set("updated_at", sq.Expr("NOW()"))
	changed := false
	if in.Name != "" {
		q, changed = q.Set("name", model.CleanText(in.Name)), true
	}
	if in.DepartmentID != nil {
		q, changed = q.Set("department_id", *in.DepartmentID), true
	}
	if in.DepartmentName != nil {
		q, changed = q.Set("department_name", nullIfEmpty(*in.DepartmentName)), true
	}
	if in.ResponsibleUserID != nil {
		// МОЛ склада: ключевое поле, без него ведомость потерь по складу безадресна.
		q, changed = q.Set("responsible_user_id", *in.ResponsibleUserID), true
	}
	if in.Location != nil {
		q, changed = q.Set("location", nullIfEmpty(*in.Location)), true
	}
	if in.IsActive != nil {
		q, changed = q.Set("is_active", *in.IsActive), true
	}
	if in.Note != nil {
		q, changed = q.Set("note", nullIfEmpty(*in.Note)), true
	}
	if !changed {
		return nil, apperr.New(apperr.CodeUpdateFieldsRequired, "update fields required")
	}

	sqlStr, args, err := q.Where(sq.Eq{"id": id}).Suffix("RETURNING id").ToSql()
	if err != nil {
		return nil, err
	}
	var updated int64
	if err := r.db.QueryRow(ctx, sqlStr, args...).Scan(&updated); err != nil {
		if IsUniqueViolation(err, "uq_wh_warehouse_name") {
			return nil, apperr.New(apperr.CodeWarehouseNameTaken, "warehouse name taken")
		}
		return nil, NotFoundAs(err, apperr.CodeWarehouseNotFound)
	}
	return r.GetByID(ctx, id)
}

// EnsureWarehouse находит склад по имени или создаёт его.
// Счёт 10 несёт иерархию «подразделение → склад», поэтому склад заводится
// при первом упоминании в выгрузке, а не руками заранее.
func EnsureWarehouse(ctx context.Context, tx pgx.Tx, name, departmentName string) (int64, error) {
	clean := model.CleanText(name)
	if clean == "" {
		return 0, apperr.New(apperr.CodeWarehouseNameRequired, "warehouse name required")
	}
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO wh_warehouse (name, department_name)
		VALUES ($1, $2)
		ON CONFLICT (name) DO UPDATE SET
			-- название подразделения обновляем только если его ещё не знали:
			-- руками введённое значение важнее пришедшего из выгрузки
			department_name = COALESCE(wh_warehouse.department_name, EXCLUDED.department_name)
		RETURNING id
	`, clean, model.NullableText(departmentName)).Scan(&id)
	return id, err
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
