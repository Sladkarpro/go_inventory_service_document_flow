package repository

import (
	"context"

	sq "github.com/Masterminds/squirrel"
	"github.com/jackc/pgx/v5/pgxpool"

	"go_warehouse_service/internal/model"
)

// ReportRepository читает три представления схемы. Логики здесь нет
// намеренно: правила расхождений живут в SQL представления wh_v_losses,
// чтобы выгрузка в xlsx и экран не могли разойтись в трактовке.
type ReportRepository struct {
	db *pgxpool.Pool
}

func NewReportRepository(db *pgxpool.Pool) *ReportRepository {
	return &ReportRepository{db: db}
}

type LossFilters struct {
	HolderType string
	// WarehouseID / UserID — «чья строка». Представление отдаёт держателя
	// текстом, поэтому фильтруем по исходной таблице через holding_id.
	WarehouseID *int64
	UserID      *int64
	// ResponsibleUserID — МОЛ склада, то есть тот, КОМУ адресована недостача.
	// Это не то же самое, что UserID: за человеком может числиться предмет,
	// а отвечать за склад будет другой.
	ResponsibleUserID *int64
	Area              string
	Kind              string // discrepancy_kind: недостача | излишек | поломка | в ремонте
	Unresolved        bool
	// Search и Room повторяют фильтры экрана закреплений: на вкладке «Потери»
	// владелец ищет так же, как на остальных, и поиск обязан работать там тоже.
	Search string
	Room   string
	Limit  int
	Offset int
}

func (r *ReportRepository) buildLosses(f LossFilters) sq.SelectBuilder {
	q := sq.Select().PlaceholderFormat(sq.Dollar).From("wh_v_losses v")
	if f.HolderType != "" {
		q = q.Where(sq.Eq{"v.holder_type": f.HolderType})
	}
	if f.WarehouseID != nil {
		q = q.Where(sq.Expr(
			"v.holding_id IN (SELECT id FROM wh_holding WHERE warehouse_id = ?)", *f.WarehouseID))
	}
	if f.UserID != nil {
		q = q.Where(sq.Expr(
			"v.holding_id IN (SELECT id FROM wh_holding WHERE user_id = ?)", *f.UserID))
	}
	if f.ResponsibleUserID != nil {
		q = q.Where(sq.Eq{"v.responsible_user_id": *f.ResponsibleUserID})
	}
	if f.Area != "" {
		q = q.Where(sq.Eq{"v.area": f.Area})
	}
	if f.Kind != "" {
		q = q.Where(sq.Eq{"v.discrepancy_kind": f.Kind})
	}
	if f.Unresolved {
		// Что ещё не обработано бухгалтерией — рабочий список для акта списания.
		q = q.Where("v.resolution IS NULL")
	}
	if room := model.CleanText(f.Room); room != "" {
		q = q.Where(sq.Eq{"v.room": room})
	}
	if s := model.NormalizeName(f.Search); s != "" {
		q = q.Where(sq.Or{
			sq.Expr("LOWER(v.name) LIKE ?", "%"+s+"%"),
			sq.Expr("LOWER(COALESCE(v.holder_person, '')) LIKE ?", "%"+s+"%"),
			sq.Expr("LOWER(COALESCE(v.holder_department, '')) LIKE ?", "%"+s+"%"),
			sq.Expr("LOWER(COALESCE(v.inv_number, '')) LIKE ?", "%"+s+"%"),
		})
	}
	return q
}

const lossColumns = `v.item_id, v.name, v.area, v.holding_id, v.holder_type,
	v.holder_person, v.holder_department, v.warehouse, v.responsible_person, v.responsible_user_id,
	v.room, v.inv_number, v.serial_number,
	v.qty_expected, v.qty_actual, v.qty_diff, v.discrepancy_kind,
	v.outcome, v.resolution, v.resolution_ref, v.note`

func (r *ReportRepository) Losses(ctx context.Context, f LossFilters) ([]model.LossRow, int64, error) {
	countSQL, countArgs, err := r.buildLosses(f).Columns("COUNT(*)").ToSql()
	if err != nil {
		return nil, 0, err
	}
	var total int64
	if err := r.db.QueryRow(ctx, countSQL, countArgs...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := r.buildLosses(f).Columns(lossColumns).
		OrderBy("v.discrepancy_kind", "v.name", "v.holding_id")
	if f.Limit > 0 {
		q = q.Limit(uint64(f.Limit)).Offset(uint64(f.Offset))
	}
	listSQL, listArgs, err := q.ToSql()
	if err != nil {
		return nil, 0, err
	}

	rows, err := r.db.Query(ctx, listSQL, listArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]model.LossRow, 0, 64)
	for rows.Next() {
		var l model.LossRow
		if err := rows.Scan(&l.ItemID, &l.Name, &l.Area, &l.HoldingID, &l.HolderType,
			&l.HolderPerson, &l.HolderDepartment, &l.Warehouse, &l.ResponsiblePerson,
			&l.ResponsibleUserID, &l.Room, &l.InvNumber, &l.SerialNumber,
			&l.QtyExpected, &l.QtyActual, &l.QtyDiff, &l.DiscrepancyKind,
			&l.Outcome, &l.Resolution, &l.ResolutionRef, &l.Note); err != nil {
			return nil, 0, err
		}
		out = append(out, l)
	}
	return out, total, rows.Err()
}

type SummaryFilters struct {
	Search string
	Area   string
	Limit  int
	Offset int
}

func (r *ReportRepository) Summary(ctx context.Context, f SummaryFilters) ([]model.ItemTotalRow, int64, error) {
	build := func() sq.SelectBuilder {
		q := sq.Select().PlaceholderFormat(sq.Dollar).From("wh_v_item_totals v")
		if s := model.NormalizeName(f.Search); s != "" {
			q = q.Where(sq.Expr("LOWER(v.name) LIKE ?", "%"+s+"%"))
		}
		if f.Area != "" {
			q = q.Where(sq.Eq{"v.area": f.Area})
		}
		return q
	}

	countSQL, countArgs, err := build().Columns("COUNT(*)").ToSql()
	if err != nil {
		return nil, 0, err
	}
	var total int64
	if err := r.db.QueryRow(ctx, countSQL, countArgs...).Scan(&total); err != nil {
		return nil, 0, err
	}

	listSQL, listArgs, err := build().
		Columns(`v.item_id, v.name, v.area, v.unit, v.person_expected, v.department_expected,
		         v.unallocated_expected, v.total_expected, v.total_actual, v.not_counted_rows`).
		OrderBy("v.name").
		Limit(uint64(f.Limit)).Offset(uint64(f.Offset)).
		ToSql()
	if err != nil {
		return nil, 0, err
	}

	rows, err := r.db.Query(ctx, listSQL, listArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]model.ItemTotalRow, 0, f.Limit)
	for rows.Next() {
		var s model.ItemTotalRow
		if err := rows.Scan(&s.ItemID, &s.Name, &s.Area, &s.Unit,
			&s.PersonExpected, &s.DepartmentExpected, &s.UnallocatedExpected,
			&s.TotalExpected, &s.TotalActual, &s.NotCountedRows); err != nil {
			return nil, 0, err
		}
		out = append(out, s)
	}
	return out, total, rows.Err()
}
