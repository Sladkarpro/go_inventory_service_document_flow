package repository

import (
	"context"
	"encoding/json"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"go_warehouse_service/internal/apperr"
	"go_warehouse_service/internal/model"
)

type HoldingFilters struct {
	HolderType    string
	UserID        *int64
	DepartmentID  *int64
	WarehouseID   *int64
	ItemID        *int64
	Room          string
	Outcome       string
	InvNumber     string
	Search        string
	OnlyUncounted bool
	OnlyDiff      bool
	Limit         int
	Offset        int
}

type HoldingRepository struct {
	db *pgxpool.Pool
}

func NewHoldingRepository(db *pgxpool.Pool) *HoldingRepository {
	return &HoldingRepository{db: db}
}

const holdingColumns = `h.id, h.item_id, h.holder_type, h.user_id, h.user_fio,
	h.department_id, h.department_name, h.warehouse_id, h.room,
	h.inv_number, h.inv_number_norm, h.serial_number,
	h.hostname, h.ip_address, h.mac_address, h.specs,
	h.qty_expected, h.qty_actual, h.outcome, h.counted_at, h.counted_by_id, h.note,
	h.resolution, h.resolution_ref, h.resolved_at, h.resolved_by_id,
	h.source_file, h.source_sheet, h.source_row, h.created_at, h.updated_at`

// scanHolding читает строку закрепления вместе с денормализованными полями
// связей (наименование позиции, склад) — экран пересчёта показывает их всегда.
func scanHolding(row pgx.Row, withJoins bool) (model.Holding, error) {
	var h model.Holding
	var specs []byte
	var ip *string

	dest := []any{
		&h.ID, &h.ItemID, &h.HolderType, &h.UserID, &h.UserFIO,
		&h.DepartmentID, &h.DepartmentName, &h.WarehouseID, &h.Room,
		&h.InvNumber, &h.InvNumberNorm, &h.SerialNumber,
		&h.Hostname, &ip, &h.MACAddress, &specs,
		&h.QtyExpected, &h.QtyActual, &h.Outcome, &h.CountedAt, &h.CountedByID, &h.Note,
		&h.Resolution, &h.ResolutionRef, &h.ResolvedAt, &h.ResolvedByID,
		&h.SourceFile, &h.SourceSheet, &h.SourceRow, &h.CreatedAt, &h.UpdatedAt,
	}

	var itemName, warehouseName *string
	var credCount int
	if withJoins {
		dest = append(dest, &itemName, &warehouseName, &credCount)
	}

	if err := row.Scan(dest...); err != nil {
		return h, err
	}

	h.IPAddress = ip
	if len(specs) > 0 {
		_ = json.Unmarshal(specs, &h.Specs)
	}
	if h.Specs == nil {
		h.Specs = map[string]any{}
	}
	if withJoins {
		if itemName != nil {
			h.Item = &model.Item{ID: h.ItemID, Name: *itemName}
		}
		if warehouseName != nil && h.WarehouseID != nil {
			h.Warehouse = &model.Warehouse{ID: *h.WarehouseID, Name: *warehouseName}
		}
		h.CredentialCnt = credCount
	}
	return h, nil
}

func (r *HoldingRepository) buildQuery(f HoldingFilters) sq.SelectBuilder {
	q := sq.Select().PlaceholderFormat(sq.Dollar).
		From("wh_holding h").
		Join("wh_item i ON i.id = h.item_id").
		LeftJoin("wh_warehouse w ON w.id = h.warehouse_id")

	if f.HolderType != "" {
		q = q.Where(sq.Eq{"h.holder_type": f.HolderType})
	}
	if f.UserID != nil {
		q = q.Where(sq.Eq{"h.user_id": *f.UserID})
	}
	if f.DepartmentID != nil {
		q = q.Where(sq.Eq{"h.department_id": *f.DepartmentID})
	}
	if f.WarehouseID != nil {
		q = q.Where(sq.Eq{"h.warehouse_id": *f.WarehouseID})
	}
	if f.ItemID != nil {
		q = q.Where(sq.Eq{"h.item_id": *f.ItemID})
	}
	if room := model.CleanText(f.Room); room != "" {
		q = q.Where(sq.Eq{"h.room": room})
	}
	if f.Outcome != "" {
		q = q.Where(sq.Eq{"h.outcome": f.Outcome})
	}
	if inv := model.CleanText(f.InvNumber); inv != "" {
		// Ищем по нормализованному номеру: владелец наберёт «дск 487»,
		// а записано «ДСК0487».
		if norm, ok := model.NormalizeInvNumber(inv); ok {
			q = q.Where(sq.Like{"h.inv_number_norm": "%" + norm + "%"})
		}
	}
	if s := model.CleanText(f.Search); s != "" {
		norm := model.NormalizeName(s)
		q = q.Where(sq.Or{
			sq.Like{"i.name_norm": "%" + norm + "%"},
			sq.Expr("LOWER(COALESCE(h.user_fio, '')) LIKE ?", "%"+norm+"%"),
			sq.Expr("LOWER(COALESCE(h.department_name, '')) LIKE ?", "%"+norm+"%"),
			sq.Expr("LOWER(COALESCE(h.room, '')) LIKE ?", "%"+norm+"%"),
		})
	}
	if f.OnlyUncounted {
		// Главный рабочий фильтр во время обхода: «что ещё не считали».
		// Именно NULL, а не 0 — ноль означает «искали и не нашли».
		q = q.Where("h.qty_actual IS NULL")
	}
	if f.OnlyDiff {
		q = q.Where(`h.qty_actual IS NOT NULL
			AND (h.qty_actual <> h.qty_expected OR h.outcome IN ('broken','in_repair'))`)
	}
	return q
}

func (r *HoldingRepository) List(ctx context.Context, f HoldingFilters) ([]model.Holding, int64, error) {
	countSQL, countArgs, err := r.buildQuery(f).Columns("COUNT(h.id)").ToSql()
	if err != nil {
		return nil, 0, err
	}
	var total int64
	if err := r.db.QueryRow(ctx, countSQL, countArgs...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := r.buildQuery(f).
		Columns(holdingColumns, "i.name", "w.name",
			"(SELECT COUNT(*) FROM wh_credential c WHERE c.holding_id = h.id)").
		OrderBy("i.name", "h.id")
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

	out := make([]model.Holding, 0, f.Limit)
	for rows.Next() {
		h, err := scanHolding(rows, true)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, h)
	}
	return out, total, rows.Err()
}

func (r *HoldingRepository) GetByID(ctx context.Context, id int64) (*model.Holding, error) {
	sqlStr := `SELECT ` + holdingColumns + `, i.name, w.name,
		       (SELECT COUNT(*) FROM wh_credential c WHERE c.holding_id = h.id)
		FROM wh_holding h
		JOIN wh_item i ON i.id = h.item_id
		LEFT JOIN wh_warehouse w ON w.id = h.warehouse_id
		WHERE h.id = $1`
	h, err := scanHolding(r.db.QueryRow(ctx, sqlStr, id), true)
	if err != nil {
		return nil, NotFoundAs(err, apperr.CodeHoldingNotFound)
	}
	return &h, nil
}

// HoldingInput — данные для создания строки.
type HoldingInput struct {
	ItemID         int64
	HolderType     string
	UserID         *int64
	UserFIO        *string
	DepartmentID   *int64
	DepartmentName *string
	WarehouseID    *int64
	Room           *string
	InvNumber      *string
	SerialNumber   *string
	QtyExpected    decimal.Decimal
	QtyActual      *decimal.Decimal
	Outcome        *string
	Note           *string
	CountedByID    *int64
	SourceFile     string
	SourceSheet    *string
	SourceRow      *int32
}

func (r *HoldingRepository) Create(ctx context.Context, in HoldingInput, now time.Time) (*model.Holding, error) {
	invNumber, invNorm := normalizeInv(in.InvNumber)

	var countedAt *time.Time
	if in.QtyActual != nil {
		countedAt = &now
	}

	var id int64
	err := r.db.QueryRow(ctx, `
		INSERT INTO wh_holding (item_id, holder_type, user_id, user_fio,
			department_id, department_name, warehouse_id, room,
			inv_number, inv_number_norm, serial_number,
			qty_expected, qty_actual, outcome, counted_at, counted_by_id, note,
			source_file, source_sheet, source_row)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		RETURNING id
	`, in.ItemID, in.HolderType, in.UserID, in.UserFIO,
		in.DepartmentID, in.DepartmentName, in.WarehouseID, in.Room,
		invNumber, invNorm, in.SerialNumber,
		in.QtyExpected, in.QtyActual, in.Outcome, countedAt, in.CountedByID, in.Note,
		in.SourceFile, in.SourceSheet, in.SourceRow).Scan(&id)
	if err != nil {
		return nil, translateHoldingErr(err)
	}
	return r.GetByID(ctx, id)
}

// HoldingUpdate — частичное обновление. Каждое поле-указатель означает
// «поле прислано»; nil — «не трогать».
type HoldingUpdate struct {
	QtyActual      *decimal.Decimal
	ClearQtyActual bool
	Outcome        *string
	Room           *string
	Note           *string
	HolderType     *string
	UserID         *int64
	UserFIO        *string
	DepartmentID   *int64
	DepartmentName *string
	WarehouseID    *int64
	InvNumber      *string
	SerialNumber   *string
	Hostname       *string
	IPAddress      *string
	MACAddress     *string
	Specs          map[string]any
	Resolution     *string
	ResolutionRef  *string
}

func (r *HoldingRepository) Update(
	ctx context.Context, id int64, upd HoldingUpdate, actorID int64, now time.Time,
) (*model.Holding, error) {
	q := sq.Update("wh_holding").PlaceholderFormat(sq.Dollar).Set("updated_at", sq.Expr("NOW()"))
	changed := false

	switch {
	case upd.ClearQtyActual:
		// Вернуть строку в состояние «ещё не считали». Итог пересчёта при этом
		// обязан уйти вместе с фактом: chk_wh_holding_counted не разрешает
		// outcome без qty_actual.
		q = q.Set("qty_actual", nil).Set("counted_at", nil).Set("counted_by_id", nil).
			Set("outcome", nil)
		changed = true
	case upd.QtyActual != nil:
		q = q.Set("qty_actual", *upd.QtyActual).
			Set("counted_at", now).
			Set("counted_by_id", actorID)
		changed = true
	}

	if upd.Outcome != nil {
		q, changed = q.Set("outcome", nullIfEmpty(*upd.Outcome)), true
	}
	if upd.Room != nil {
		q, changed = q.Set("room", nullIfEmpty(*upd.Room)), true
	}
	if upd.Note != nil {
		q, changed = q.Set("note", nullIfEmpty(*upd.Note)), true
	}
	if upd.HolderType != nil {
		q, changed = q.Set("holder_type", *upd.HolderType), true
	}
	if upd.UserID != nil {
		q, changed = q.Set("user_id", zeroToNull(*upd.UserID)), true
	}
	if upd.UserFIO != nil {
		q, changed = q.Set("user_fio", nullIfEmpty(*upd.UserFIO)), true
	}
	if upd.DepartmentID != nil {
		q, changed = q.Set("department_id", zeroToNull(*upd.DepartmentID)), true
	}
	if upd.DepartmentName != nil {
		q, changed = q.Set("department_name", nullIfEmpty(*upd.DepartmentName)), true
	}
	if upd.WarehouseID != nil {
		q, changed = q.Set("warehouse_id", zeroToNull(*upd.WarehouseID)), true
	}
	if upd.InvNumber != nil {
		inv, norm := normalizeInv(upd.InvNumber)
		q, changed = q.Set("inv_number", inv).Set("inv_number_norm", norm), true
	}
	if upd.SerialNumber != nil {
		q, changed = q.Set("serial_number", nullIfEmpty(*upd.SerialNumber)), true
	}
	if upd.Hostname != nil {
		q, changed = q.Set("hostname", nullIfEmpty(*upd.Hostname)), true
	}
	if upd.IPAddress != nil {
		q, changed = q.Set("ip_address", nullIfEmpty(*upd.IPAddress)), true
	}
	if upd.MACAddress != nil {
		q, changed = q.Set("mac_address", nullIfEmpty(*upd.MACAddress)), true
	}
	if upd.Specs != nil {
		raw, err := json.Marshal(upd.Specs)
		if err != nil {
			return nil, err
		}
		q, changed = q.Set("specs", raw), true
	}
	if upd.Resolution != nil {
		// Списание фиксируется вместе с тем, кто и когда его провёл:
		// без этого «нашли недостачу» есть, а «списали пропажу» — нет.
		q = q.Set("resolution", nullIfEmpty(*upd.Resolution)).
			Set("resolved_at", now).
			Set("resolved_by_id", actorID)
		changed = true
	}
	if upd.ResolutionRef != nil {
		q, changed = q.Set("resolution_ref", nullIfEmpty(*upd.ResolutionRef)), true
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
		if e := translateHoldingErr(err); e != err {
			return nil, e
		}
		return nil, NotFoundAs(err, apperr.CodeHoldingNotFound)
	}
	return r.GetByID(ctx, id)
}

func (r *HoldingRepository) Delete(ctx context.Context, id int64) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM wh_holding WHERE id = $1`, id)
	if err != nil {
		if IsForeignKeyViolation(err) {
			// Доступы с просмотрами удалить нельзя: журнал стоит на RESTRICT,
			// и след того, кто видел пароль, не должен стираться вместе с техникой.
			return apperr.New(apperr.CodeCredentialViewed,
				"holding has viewed credentials")
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return apperr.New(apperr.CodeHoldingNotFound, "holding not found")
	}
	return nil
}

// Rooms — подсказка кабинетов для формы обхода.
func (r *HoldingRepository) Rooms(ctx context.Context) ([]string, error) {
	rows, err := r.db.Query(ctx, `
		SELECT DISTINCT room FROM wh_holding WHERE room IS NOT NULL AND room <> '' ORDER BY room
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]string, 0, 32)
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *HoldingRepository) ListByItem(ctx context.Context, itemID int64) ([]model.Holding, error) {
	// Карточка позиции не имеет пагинации: возвращаем все её закрепления.
	items, _, err := r.List(ctx, HoldingFilters{ItemID: &itemID})
	return items, err
}

// normalizeInv возвращает пару (inv_number, inv_number_norm) для базы.
//
// ⚠ Заглушки («б/н») дают ОБА NULL. Констрейнт chk_wh_holding_inv_norm требует,
// чтобы номер и его нормализованная форма существовали вместе, а частичный
// уникальный индекс пропустил бы только одну строку из 23 с «б/н».
func normalizeInv(raw *string) (any, any) {
	if raw == nil {
		return nil, nil
	}
	clean := model.CleanText(*raw)
	if clean == "" {
		return nil, nil
	}
	norm, ok := model.NormalizeInvNumber(clean)
	if !ok {
		return nil, nil
	}
	return clean, norm
}

func zeroToNull(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// translateHoldingErr превращает нарушения констрейнтов в понятные коды.
// Без этого двойной ввод инвентарного номера при обходе вдвоём выглядел бы
// как 500, и обходчик не понял бы, что предмет уже посчитан коллегой.
func translateHoldingErr(err error) error {
	switch {
	case IsUniqueViolation(err, "uq_wh_holding_inv_number"):
		return apperr.New(apperr.CodeInvNumberDuplicate, "inventory number already used")
	case IsUniqueViolation(err, "uq_wh_holding_natural"):
		return apperr.New(apperr.CodeConflict, "holding already exists")
	case IsCheckViolation(err, "chk_wh_holding_shape"):
		return apperr.New(apperr.CodeHolderShapeInvalid, "holder shape invalid")
	case IsCheckViolation(err, "chk_wh_holding_inv_qty"):
		return apperr.New(apperr.CodeInvNumberQtyInvalid, "inventory number requires qty 0 or 1")
	case IsCheckViolation(err, "chk_wh_holding_counted"):
		return apperr.New(apperr.CodeInvalidOutcome, "outcome requires actual quantity")
	case IsCheckViolation(err, ""):
		return apperr.New(apperr.CodeValidation, "constraint violated")
	}
	return err
}
