package repository

import (
	"context"
	"encoding/json"

	sq "github.com/Masterminds/squirrel"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go_warehouse_service/internal/apperr"
	"go_warehouse_service/internal/model"
)

type ItemFilters struct {
	Search          string
	Area            string
	OnlyUnallocated bool
	OnlyLosses      bool
	Limit           int
	Offset          int
}

type ItemRepository struct {
	db *pgxpool.Pool
}

func NewItemRepository(db *pgxpool.Pool) *ItemRepository {
	return &ItemRepository{db: db}
}

const itemColumns = `i.id, i.name, i.name_norm, i.area, i.unit, i.attributes, i.note, i.created_at, i.updated_at`

func scanItem(row pgx.Row) (model.Item, error) {
	var it model.Item
	var attrs []byte
	err := row.Scan(&it.ID, &it.Name, &it.NameNorm, &it.Area, &it.Unit, &attrs, &it.Note,
		&it.CreatedAt, &it.UpdatedAt)
	if err != nil {
		return it, err
	}
	if len(attrs) > 0 {
		_ = json.Unmarshal(attrs, &it.Attributes)
	}
	if it.Attributes == nil {
		it.Attributes = map[string]any{}
	}
	return it, nil
}

func (r *ItemRepository) buildQuery(f ItemFilters) sq.SelectBuilder {
	q := sq.Select().PlaceholderFormat(sq.Dollar).From("wh_item i")

	if s := model.CleanText(f.Search); s != "" {
		// Поиск по нормализованному имени: пользователь ищет «монитор rdw»,
		// а в 1С записано «Монитор  RDW 2401К».
		q = q.Where(sq.Like{"i.name_norm": "%" + model.NormalizeName(s) + "%"})
	}
	if f.Area != "" {
		q = q.Where(sq.Eq{"i.area": f.Area})
	}
	if f.OnlyUnallocated {
		q = q.Where(`EXISTS (SELECT 1 FROM wh_holding h
			WHERE h.item_id = i.id AND h.holder_type = 'warehouse')`)
	}
	if f.OnlyLosses {
		// Расхождение — это не только недостача: поломка и излишек тоже
		// попадают в акт списания.
		q = q.Where(`EXISTS (SELECT 1 FROM wh_holding h
			WHERE h.item_id = i.id
			  AND h.qty_actual IS NOT NULL
			  AND (h.qty_actual <> h.qty_expected OR h.outcome IN ('broken','in_repair')))`)
	}
	return q
}

func (r *ItemRepository) List(ctx context.Context, f ItemFilters) ([]model.Item, int64, error) {
	base := r.buildQuery(f)

	countSQL, countArgs, err := base.Columns("COUNT(i.id)").ToSql()
	if err != nil {
		return nil, 0, err
	}
	var total int64
	if err := r.db.QueryRow(ctx, countSQL, countArgs...).Scan(&total); err != nil {
		return nil, 0, err
	}

	listSQL, listArgs, err := r.buildQuery(f).
		Columns(itemColumns).
		OrderBy("i.name").
		Limit(uint64(f.Limit)).
		Offset(uint64(f.Offset)).
		ToSql()
	if err != nil {
		return nil, 0, err
	}
	rows, err := r.db.Query(ctx, listSQL, listArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]model.Item, 0, f.Limit)
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, it)
	}
	return items, total, rows.Err()
}

func (r *ItemRepository) GetByID(ctx context.Context, id int64) (*model.Item, error) {
	it, err := scanItem(r.db.QueryRow(ctx,
		`SELECT `+itemColumns+` FROM wh_item i WHERE i.id = $1`, id))
	if err != nil {
		return nil, NotFoundAs(err, apperr.CodeItemNotFound)
	}
	return &it, nil
}

type ItemUpdate struct {
	Area       *string
	Unit       *string
	Attributes map[string]any
	Note       *string
}

func (r *ItemRepository) Update(ctx context.Context, id int64, upd ItemUpdate) (*model.Item, error) {
	q := sq.Update("wh_item").PlaceholderFormat(sq.Dollar).Set("updated_at", sq.Expr("NOW()"))
	changed := false
	if upd.Area != nil {
		q, changed = q.Set("area", nullIfEmpty(*upd.Area)), true
	}
	if upd.Unit != nil {
		q, changed = q.Set("unit", nullIfEmpty(*upd.Unit)), true
	}
	if upd.Note != nil {
		q, changed = q.Set("note", nullIfEmpty(*upd.Note)), true
	}
	if upd.Attributes != nil {
		raw, err := json.Marshal(upd.Attributes)
		if err != nil {
			return nil, err
		}
		q, changed = q.Set("attributes", raw), true
	}
	if !changed {
		return nil, apperr.New(apperr.CodeUpdateFieldsRequired, "update fields required")
	}

	sqlStr, args, err := q.Where(sq.Eq{"id": id}).Suffix(`RETURNING id`).ToSql()
	if err != nil {
		return nil, err
	}
	var updated int64
	if err := r.db.QueryRow(ctx, sqlStr, args...).Scan(&updated); err != nil {
		return nil, NotFoundAs(err, apperr.CodeItemNotFound)
	}
	return r.GetByID(ctx, id)
}

// EnsureItems создаёт недостающие позиции каталога и возвращает карту
// «нормализованное имя → id» для всего переданного набора, а также число
// ДЕЙСТВИТЕЛЬНО созданных позиций.
//
// ON CONFLICT по name_norm — то, ради чего колонка заведена: 874 наименования
// из 881 встречаются в нескольких выгрузках, и без схлопывания каталог задвоился бы,
// а сводка сложила бы остатки одного предмета как двух разных.
//
// ⚠ Второе возвращаемое значение — именно СОЗДАННЫЕ, а не размер карты.
// Раньше отчёт об импорте показывал len(index), то есть число упомянутых
// в файле позиций, под меткой «Позиций создано»: повторная загрузка того же
// файла честно сообщала «создано 881», не создав ни одной. Владелец сверяет
// отчёт глазами на каждом файле — цифра, которая всегда равна размеру файла,
// не даёт ему ничего и маскирует настоящее пополнение каталога.
//
// Различить вставку от конфликта позволяет xmax: у строки, вставленной этим
// же запросом, xmax = 0, у обновлённой — идентификатор транзакции.
// RETURNING сам по себе одинаков в обоих случаях, поэтому без xmax отличить
// нельзя.
func EnsureItems(ctx context.Context, tx pgx.Tx, names []string) (map[string]int64, int, error) {
	index := make(map[string]int64, len(names))
	if len(names) == 0 {
		return index, 0, nil
	}

	// уникализируем вход, сохраняя первое (человеческое) написание
	seen := make(map[string]string, len(names))
	order := make([]string, 0, len(names))
	for _, n := range names {
		norm := model.NormalizeName(n)
		if norm == "" {
			continue
		}
		if _, ok := seen[norm]; !ok {
			seen[norm] = model.CleanText(n)
			order = append(order, norm)
		}
	}

	batch := &pgx.Batch{}
	for _, norm := range order {
		batch.Queue(`
			INSERT INTO wh_item (name, name_norm)
			VALUES ($1, $2)
			ON CONFLICT (name_norm) DO UPDATE SET name_norm = EXCLUDED.name_norm
			RETURNING id, (xmax = 0) AS inserted
		`, seen[norm], norm)
	}
	res := tx.SendBatch(ctx, batch)
	defer func() { _ = res.Close() }()

	created := 0
	for _, norm := range order {
		var id int64
		var inserted bool
		if err := res.QueryRow().Scan(&id, &inserted); err != nil {
			return nil, 0, err
		}
		index[norm] = id
		if inserted {
			created++
		}
	}
	return index, created, nil
}

func nullIfEmpty(s string) any {
	if v := model.NullableText(s); v != nil {
		return *v
	}
	return nil
}
