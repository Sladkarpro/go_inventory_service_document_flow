package service

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"go_warehouse_service/internal/helper"
	"go_warehouse_service/internal/model"
	"go_warehouse_service/internal/parser"
	"go_warehouse_service/internal/repository"
)

// ImportService переносит разобранный файл в базу.
//
// ⚠ МАТРИЦА ИСТОЧНИКОВ — главный риск проекта. Шесть файлов владельца
// ПЕРЕСЕКАЮТСЯ, и без явного правила слияния сервис либо отвергнет корректные
// строки, либо ТИХО УДВОИТ остатки. Вторая ошибка хуже: она не видна.
//
//	Счёт 10 (склады)   — СОЗДАЁТ складские строки. Единственный источник остатка склада.
//	МЦ.04              — СОЗДАЁТ строки за людьми.
//	«Материалы» (27.07)— НЕ ГРУЗИТСЯ: конфликтует с МЦ.04, это другая версия того же.
//	Ручная ведомость   — ОБОГАЩАЕТ существующие строки (qty_actual, note). Не создаёт остаток.
//	Реестр (40 листов) — РАСЩЕПЛЯЕТ агрегат на именованные экземпляры.
//	                     НЕ добавляет количество.
//
// Если написать импорт «в лоб», 1105 номерных предметов лягут ПОВЕРХ 3912 единиц,
// и сводка сложит их вместе.
type ImportService struct {
	db          *pgxpool.Pool
	items       *repository.ItemRepository
	holdings    *repository.HoldingRepository
	users       *repository.UserRepository
	departments *repository.DepartmentRepository
	clock       helper.Clock
}

func NewImportService(
	db *pgxpool.Pool,
	items *repository.ItemRepository,
	holdings *repository.HoldingRepository,
	users *repository.UserRepository,
	departments *repository.DepartmentRepository,
	clk helper.Clock,
) *ImportService {
	return &ImportService{db: db, items: items, holdings: holdings,
		users: users, departments: departments, clock: clk}
}

// ImportReport — что именно сделал импорт. Владелец обязан это видеть:
// молчаливый импорт, который «вроде прошёл», — способ потерять предметы.
type ImportReport struct {
	Format          string   `json:"format"`
	File            string   `json:"file"`
	RowsParsed      int      `json:"rowsParsed"`
	ItemsCreated    int      `json:"itemsCreated"`
	HoldingsCreated int      `json:"holdingsCreated"`
	HoldingsUpdated int      `json:"holdingsUpdated"`
	HoldingsSplit   int      `json:"holdingsSplit"`
	MatchedUsers    int      `json:"matchedUsers"`
	UnmatchedUsers  int      `json:"unmatchedUsers"`
	TotalQty        string   `json:"totalQty"`
	Warnings        []string `json:"warnings"`
	SkippedNumbers  []string `json:"skippedNumbers"`
	NamelessNumbers []string `json:"namelessNumbers"`
}

// Import разбирает файл и применяет его одной транзакцией.
//
// Транзакция обязательна: файл на 1774 строки должен примениться целиком либо
// не примениться вовсе. Частично применённый импорт при повторной загрузке
// доложил бы остатки поверх уже вставленных.
func (s *ImportService) Import(ctx context.Context, path string, format parser.Format) (*ImportReport, error) {
	res, err := parser.Parse(path, format)
	if err != nil {
		return nil, err
	}

	report := &ImportReport{
		Format:          string(res.Format),
		File:            res.File,
		RowsParsed:      len(res.Rows),
		TotalQty:        res.TotalQty().String(),
		SkippedNumbers:  res.SkippedNumbers,
		NamelessNumbers: res.NamelessNumbers,
	}
	for _, w := range res.Warnings {
		report.Warnings = append(report.Warnings, w.String())
	}

	// ⚠ Строки со ОДИНАКОВЫМ держателем и позицией обязаны СЛОЖИТЬСЯ до записи.
	// В МЦ.04 одна и та же номенклатура перечислена у человека несколькими
	// строками (у директора «Стул на раме ISO BLACK» — шестью). В базе они
	// схлопываются частичным уникальным индексом uq_wh_holding_natural, и
	// upsert, который ЗАПИСЫВАЕТ количество вместо сложения, тихо теряет разницу:
	// 3912 единиц превращались в 3898. Ошибка не видна ни в логе, ни на экране —
	// поймал её только сквозной тест по контрольной сумме.
	res.Rows = aggregateRows(res.Rows)

	fioIndex, err := s.users.FIOIndex(ctx)
	if err != nil {
		return nil, fmt.Errorf("индекс ФИО: %w", err)
	}
	deptIndex, err := s.departments.NameIndex(ctx)
	if err != nil {
		return nil, fmt.Errorf("индекс подразделений: %w", err)
	}

	err = repository.ExecTx(ctx, s.db, func(tx pgx.Tx) error {
		if err := lockImportNumbers(ctx, tx, res.Rows); err != nil {
			return err
		}
		names := make([]string, 0, len(res.Rows))
		for _, r := range res.Rows {
			names = append(names, r.ItemName)
		}
		// EnsureItems блокирует строки каталога: порядок должен совпадать у
		// разных файлов. Stable сохраняет первое написание одинаковых имён.
		sort.SliceStable(names, func(i, j int) bool {
			return model.NormalizeName(names[i]) < model.NormalizeName(names[j])
		})
		itemIndex, itemsCreated, err := repository.EnsureItems(ctx, tx, names)
		if err != nil {
			return fmt.Errorf("каталог позиций: %w", err)
		}
		// Именно созданные, а не упомянутые: под меткой «Позиций создано»
		// повторная загрузка показывала размер файла и не создавала ничего.
		report.ItemsCreated = itemsCreated

		warehouseIndex := map[string]int64{}

		// Строки ручной ведомости, в которых владелец ещё не написал факт.
		// Считаются, но не превращаются в предупреждения по одной: это норма
		// хода пересчёта, а не проблема (см. ветку FormatManual ниже).
		factNotFilled := 0

		for _, row := range res.Rows {
			itemID, ok := itemIndex[model.NormalizeName(row.ItemName)]
			if !ok {
				report.Warnings = append(report.Warnings,
					fmt.Sprintf("%s:%d — позиция «%s» не попала в каталог",
						row.SourceSheet, row.SourceRow, row.ItemName))
				continue
			}

			in, err := s.buildHolding(ctx, tx, row, itemID, fioIndex, deptIndex, warehouseIndex, report)
			if err != nil {
				return err
			}

			switch res.Format {
			case parser.FormatManual:
				// Ручная ведомость ОБОГАЩАЕТ: она не создаёт 205 новых единиц,
				// а проставляет факт на уже существующих строках склада.
				//
				// ⚠ Пустой факт и ненайденная строка учёта — РАЗНЫЕ вещи,
				// и раньше обе давали одно предупреждение «строка учёта
				// не найдена». На файле владельца из 36 строк факт заполнен
				// в 16, поэтому отчёт выдавал 20 предупреждений о ненайденных
				// строках, хотя не найдено НИ ОДНОЙ: просто 20 позиций ещё
				// не пересчитаны. При повторной загрузке предупреждений
				// становилось 36 из 36. Настоящая потеря факта в этом шуме
				// невидима — а это ровно та ошибка, ради которой отчёт и
				// заведён.
				if in.QtyActual == nil {
					factNotFilled++
					continue
				}
				n, err := s.enrich(ctx, tx, in, row)
				if err != nil {
					return err
				}
				report.HoldingsUpdated += n
				if n == 0 {
					// Здесь факт ЕСТЬ, а положить его некуда. Либо позиции нет
					// в остатках склада (её не создал счёт 10), либо факт по ней
					// уже проставлен раньше — enrich не перезаписывает.
					report.Warnings = append(report.Warnings, fmt.Sprintf(
						"%s:%d — «%s»: факт %s не проставлен, подходящей строки склада нет "+
							"(не загружен счёт 10 по этому складу или факт уже стоит)",
						row.SourceSheet, row.SourceRow, row.ItemName, in.QtyActual.String()))
				}

			case parser.FormatRegistry:
				if in.InvNumber == nil {
					// «б/н» не подтверждает пересчёт и не заменяет уже учтённый агрегат.
					created, _, err := s.upsertHolding(ctx, tx, in, true)
					if err != nil {
						return err
					}
					if created {
						report.HoldingsCreated++
					}
					continue
				}
				// Реестр РАСЩЕПЛЯЕТ: уменьшает агрегат на 1 и заводит экземпляр.
				// Сумма при этом не меняется.
				//
				// ⚠ РАСЩЕПЛЯТЬ МОЖНО ТОЛЬКО ОДИН РАЗ НА ПРЕДМЕТ. Один и тот же
				// инвентарный номер встречается в реестре несколько раз: предмет
				// попадает и в общий реестр, и в лист своего отдела (2174 строки
				// на 1085 номеров). Если расщеплять на каждой строке, агрегат
				// уменьшается многократно, а экземплярная строка остаётся одна —
				// сумма утекает. Проверено: 3912 превращались в 3505.
				known, err := holdingExistsByInv(ctx, tx, in.InvNumber)
				if err != nil {
					return err
				}
				split := false
				if !known {
					split, err = s.splitAggregate(ctx, tx, in, itemID)
					if err != nil {
						return err
					}
				}
				if split {
					report.HoldingsSplit++
				} else if !known {
					// ⚠ Условие `!known` обязательно, иначе ПОВТОРНЫЙ импорт
					// реестра объявляет излишком уже расщеплённые строки.
					// При known=true splitAggregate не зовётся, split остаётся
					// false — и без этой проверки ветка ниже проставляла факт
					// предметам, которых никто не пересчитывал.
					// Измерено на данных владельца: строк с фактом становилось
					// 1106 вместо 699, то есть 407 корректно расщеплённых
					// предметов объявлялись найденным излишком.
					//
					// Агрегата не нашлось: номерной предмет есть в реестре,
					// но не в 1С. Решение владельца от 29.07.2026 — заводить
					// излишком, а не отбрасывать: qty_expected=0, qty_actual=1,
					// outcome='surplus'. Тогда ведомость покажет его как
					// неучтёнку, и владелец сможет его оприходовать.
					in.QtyExpected = decimal.Zero
					one := decimal.NewFromInt(1)
					in.QtyActual = &one
					surplus := string(model.OutcomeSurplus)
					in.Outcome = &surplus
				}
				created, _, err := s.upsertHolding(ctx, tx, in, known)
				if err != nil {
					return err
				}
				if created {
					report.HoldingsCreated++
				} else {
					report.HoldingsUpdated++
				}

			default:
				created, qtySplit, err := s.upsertHolding(ctx, tx, in, false)
				if err != nil {
					return err
				}
				// Количество в выгрузке стало МЕНЬШЕ уже отщеплённого: GREATEST
				// обрезал разницу до нуля, иначе файл не загрузился бы целиком.
				// Молчать об этом нельзя — тихое обрезание остатка и есть тот
				// класс ошибки, ради которого заведены контрольные суммы.
				if over := qtySplit.Sub(in.QtyExpected); over.IsPositive() {
					report.Warnings = append(report.Warnings, fmt.Sprintf(
						"%s:%d — «%s»: в выгрузке %s ед., а по реестру уже отщеплено %s; "+
							"остаток агрегата обрезан до 0, разница %s не учтена — сверьте с 1С",
						row.SourceSheet, row.SourceRow, row.ItemName,
						in.QtyExpected.String(), qtySplit.String(), over.String()))
				}
				if created {
					report.HoldingsCreated++
				} else {
					report.HoldingsUpdated++
				}
			}
		}

		// Одна сводная строка вместо десятков одинаковых. Молчать нельзя:
		// владелец должен видеть, сколько позиций ведомости ещё не пересчитано,
		// иначе «обновлено 16» из 36 строк выглядит как потеря 20.
		if factNotFilled > 0 {
			report.Warnings = append(report.Warnings, fmt.Sprintf(
				"в ведомости не заполнен факт у %d строк из %d — они пропущены, это не ошибка",
				factNotFilled, len(res.Rows)))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	slog.Info("импорт выполнен",
		"file", report.File, "format", report.Format,
		"rows", report.RowsParsed, "created", report.HoldingsCreated,
		"updated", report.HoldingsUpdated, "split", report.HoldingsSplit,
		"warnings", len(report.Warnings))

	return report, nil
}

// lockImportNumbers резервирует и ещё отсутствующие номера до EnsureItems и
// любых изменений остатков. Иначе два импорта разных позиций расщепят два
// агрегата, а UNIQUE inv_number_norm сохранит только один экземпляр.
func lockImportNumbers(ctx context.Context, tx pgx.Tx, rows []parser.Row) error {
	const namespace int32 = 0x57485249 // WHRI: warehouse import.
	var buckets [256]bool
	for _, row := range rows {
		if norm, ok := model.NormalizeInvNumber(row.InvNumber); ok {
			h := fnv.New64a()
			_, _ = h.Write([]byte("warehouse-import-inv:" + norm))
			buckets[h.Sum64()%uint64(len(buckets))] = true
		}
	}
	// Не более 256 общих ключей независимо от размера файла. Коллизия лишь
	// сериализует номера; возрастающий порядок исключает инверсию захвата.
	for bucket, needed := range buckets {
		if !needed {
			continue
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, namespace, int32(bucket)); err != nil {
			return fmt.Errorf("блокировка инвентарного номера: %w", err)
		}
	}
	return nil
}

func (s *ImportService) buildHolding(
	ctx context.Context, tx pgx.Tx, row parser.Row, itemID int64,
	fioIndex, deptIndex map[string]int64, warehouseIndex map[string]int64,
	report *ImportReport,
) (repository.HoldingInput, error) {
	in := repository.HoldingInput{
		ItemID:      itemID,
		HolderType:  string(row.HolderType),
		QtyExpected: row.Qty,
		SourceFile:  row.SourceFile,
		SourceRow:   int32Ptr(row.SourceRow),
	}
	if row.SourceSheet != "" {
		sheet := row.SourceSheet
		in.SourceSheet = &sheet
	}
	in.Room = model.NullableText(row.Room)
	in.SerialNumber = model.NullableText(row.SerialNumber)
	in.Note = model.NullableText(row.Note)
	if row.InvNumber != "" {
		inv := row.InvNumber
		in.InvNumber = &inv
	}
	if row.QtyActual != nil {
		in.QtyActual = row.QtyActual
	}
	if row.Outcome != "" {
		o := row.Outcome
		in.Outcome = &o
	}

	switch row.HolderType {
	case model.HolderPerson:
		in.UserFIO = model.NullableText(row.UserFIO)
		// Матчинг по точному совпадению ФИО. Не сматчилось — не падаем:
		// имя остаётся строкой, импорт продолжается, владелец свяжет вручную.
		if id, ok := fioIndex[model.NormalizeFIO(row.UserFIO)]; ok {
			in.UserID = &id
			report.MatchedUsers++
		} else {
			report.UnmatchedUsers++
		}

	case model.HolderDepartment:
		in.DepartmentName = model.NullableText(row.DepartmentName)
		if id, ok := deptIndex[model.NormalizeName(row.DepartmentName)]; ok {
			in.DepartmentID = &id
		}

	case model.HolderWarehouse:
		key := model.NormalizeName(row.WarehouseName)
		id, ok := warehouseIndex[key]
		if !ok {
			var err error
			id, err = repository.EnsureWarehouse(ctx, tx, row.WarehouseName, row.DepartmentName)
			if err != nil {
				return in, fmt.Errorf("склад «%s»: %w", row.WarehouseName, err)
			}
			warehouseIndex[key] = id
		}
		in.WarehouseID = &id
		// Подразделение у складской строки заполнено и это не ошибка:
		// иерархия счёта 10 приписывает остаток подразделению.
		in.DepartmentName = model.NullableText(row.DepartmentName)
		if did, ok := deptIndex[model.NormalizeName(row.DepartmentName)]; ok {
			in.DepartmentID = &did
		}
	}

	return in, nil
}

// upsertHolding вставляет строку либо обновляет её по естественному ключу.
// keepQty сохраняет количество номерной строки, а безномерную не меняет целиком.
//
// ⚠ УЖЕ ПРОСТАВЛЕННЫЙ qty_actual НЕ ЗАТИРАЕТСЯ. Повторная загрузка того же
// файла — обычное дело (бухгалтерия прислала свежую выгрузку в середине
// пересчёта), и она не должна стереть результаты обхода.
func (s *ImportService) upsertHolding(
	ctx context.Context, tx pgx.Tx, in repository.HoldingInput, keepQty bool,
) (created bool, qtySplit decimal.Decimal, err error) {
	invNumber, invNorm := normalizeInvPair(in.InvNumber)

	var inserted bool
	q := `
		INSERT INTO wh_holding (item_id, holder_type, user_id, user_fio,
			department_id, department_name, warehouse_id, room,
			inv_number, inv_number_norm, serial_number,
			qty_expected, qty_actual, outcome, note,
			source_file, source_sheet, source_row)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		ON CONFLICT %s DO UPDATE SET
			qty_expected  = %s,
			-- COALESCE в таком порядке: старое значение важнее нового.
			-- Владелец уже посчитал — импорт не имеет права это стереть.
			qty_actual    = COALESCE(wh_holding.qty_actual, EXCLUDED.qty_actual),
			outcome       = COALESCE(wh_holding.outcome, EXCLUDED.outcome),
			serial_number = COALESCE(EXCLUDED.serial_number, wh_holding.serial_number),
			note          = COALESCE(wh_holding.note, EXCLUDED.note),
			source_file   = EXCLUDED.source_file,
			source_sheet  = EXCLUDED.source_sheet,
			source_row    = EXCLUDED.source_row,
			updated_at    = NOW()
		WHERE NOT $19
		RETURNING (xmax = 0) AS inserted, qty_split
	`
	// Конфликт ловим по тому ключу, который применим к этой строке:
	// у номерной — по инвентарному номеру, у безномерной — по держателю.
	conflict := `(item_id, holder_type, COALESCE(user_id, 0), COALESCE(user_fio, ''),
	              COALESCE(department_name, ''), COALESCE(warehouse_id, 0), COALESCE(room, ''))
	              WHERE inv_number_norm IS NULL`
	if invNorm != nil {
		conflict = `(inv_number_norm) WHERE inv_number_norm IS NOT NULL`
	}

	// ⚠ У номерной строки количество решается ОДИН раз при заведении: 1, если
	// агрегат расщеплён, и 0, если предмета нет в 1С (излишек). Пересчитывать
	// его на повторном импорте нельзя — расщепление уже уменьшило агрегат,
	// и запись нового значения увела бы сумму.
	//
	// ⚠ АГРЕГАТУ ОТЩЕПЛЁННОЕ НЕ ВОЗВРАЩАЕТСЯ. Выгрузка 1С про расщепление
	// не знает и приносит ПОЛНОЕ количество. Записать его как есть значит
	// посчитать каждую расщеплённую единицу дважды — и в агрегате, и в
	// экземпляре. Замерено: повтор МЦ.04 после реестра давал 4319 вместо 3912,
	// ровно +407 = число расщеплений. Сколько отнято, знает только сам агрегат
	// (qty_split): связь «экземпляр -> агрегат» из состояния базы не выводится,
	// её выбирал порядок обработки строк реестра. Разбор — ADR-003 §9.
	//
	// GREATEST — на случай выгрузки, где количество СТАЛО МЕНЬШЕ уже
	// отщеплённого (бухгалтерия списала часть): без него chk_wh_holding_qty
	// отбил бы весь файл целиком. Обрезание НЕ молчаливое — вызывающий получает
	// qty_split и кладёт предупреждение в отчёт (решение владельца 29.07.2026).
	//
	// У номерных строк qty_split всегда 0 (splitAggregate трогает только строки
	// с inv_number_norm IS NULL), поэтому для них выражение тождественно
	// прежнему EXCLUDED.qty_expected.
	qtyExpr := "GREATEST(EXCLUDED.qty_expected - wh_holding.qty_split, 0)"
	if keepQty {
		qtyExpr = "wh_holding.qty_expected"
	}

	skipExisting := keepQty && invNorm == nil
	err = tx.QueryRow(ctx, fmt.Sprintf(q, conflict, qtyExpr),
		in.ItemID, in.HolderType, in.UserID, in.UserFIO,
		in.DepartmentID, in.DepartmentName, in.WarehouseID, in.Room,
		invNumber, invNorm, in.SerialNumber,
		in.QtyExpected, in.QtyActual, in.Outcome, in.Note,
		in.SourceFile, in.SourceSheet, in.SourceRow, skipExisting).Scan(&inserted, &qtySplit)
	if err == pgx.ErrNoRows && skipExisting {
		return false, decimal.Zero, nil
	}
	if err != nil {
		return false, decimal.Zero, fmt.Errorf("строка %s:%v: %w", in.SourceFile, in.SourceRow, err)
	}
	return inserted, qtySplit, nil
}

// enrich проставляет факт на СУЩЕСТВУЮЩИХ строках — работа ручной ведомости.
// Новых остатков она не создаёт: 205 единиц уже пришли из счёта 10.
func (s *ImportService) enrich(
	ctx context.Context, tx pgx.Tx, in repository.HoldingInput, row parser.Row,
) (int, error) {
	if in.QtyActual == nil {
		return 0, nil
	}
	tag, err := tx.Exec(ctx, `
		UPDATE wh_holding
		SET qty_actual = $3,
		    outcome    = COALESCE($4, outcome),
		    note       = COALESCE(NULLIF($5, ''), note),
		    counted_at = NOW(),
		    updated_at = NOW()
		WHERE item_id = $1
		  AND holder_type = 'warehouse'
		  AND ($2::bigint IS NULL OR warehouse_id = $2)
		  AND qty_actual IS NULL
	`, in.ItemID, in.WarehouseID, in.QtyActual, in.Outcome, row.Note)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// splitAggregate — механика расщепления.
//
// Строка реестра с инвентарным номером находит агрегат (та же позиция, тот же
// держатель, номера нет) и уменьшает его qty_expected на 1; экземплярная строка
// добавляет 1. ОБЩАЯ СУММА НЕ МЕНЯЕТСЯ.
//
// Без этого правила реестр просто добавит 1105 строк поверх 3912 единиц,
// и wh_v_item_totals сложит их вместе.
//
// Агрегат не нашёлся — предмет заводится излишком (решение владельца от
// 29.07.2026): qty_expected = 0, и ведомость покажет его как неучтёнку,
// а не потеряет.
func (s *ImportService) splitAggregate(
	ctx context.Context, tx pgx.Tx, in repository.HoldingInput, itemID int64,
) (bool, error) {
	if in.InvNumber == nil {
		return false, nil
	}

	// Шаг 1: дословное совпадение держателя. Самый надёжный случай.
	//
	// qty_split растёт вместе с уменьшением: это ЕДИНСТВЕННЫЙ след того, что
	// у агрегата отняли единицу. Без него повторная выгрузка 1С принесёт полное
	// количество и вернёт отнятое — предмет посчитается дважды (замерено:
	// 3912 -> 4319). Из состояния базы этот факт не выводится: какой именно
	// агрегат получил −1, решал порядок обработки строк реестра.
	tag, err := tx.Exec(ctx, `
		UPDATE wh_holding
		SET qty_expected = qty_expected - 1,
		    qty_split    = qty_split + 1,
		    updated_at   = NOW()
		WHERE id = (
			SELECT id FROM wh_holding
			WHERE item_id = $1
			  AND inv_number_norm IS NULL
			  AND qty_expected >= 1
			  AND holder_type = $2
			  AND COALESCE(user_fio, '')        = COALESCE($3::text, '')
			  AND COALESCE(department_name, '') = COALESCE($4::text, '')
			ORDER BY qty_expected DESC
			LIMIT 1
		)
	`, itemID, in.HolderType, in.UserFIO, in.DepartmentName)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() > 0 {
		return true, nil
	}

	// Шаг 2: тот же человек, записанный иначе.
	//
	// ⚠ Без этого шага расщепление не срабатывает ПОЧТИ НИКОГДА: реестр пишет
	// «Бражкина Н.А. бухгалтерия», МЦ.04 — «Бражкина Наталья Анатольевна»,
	// и из 132 держателей реестра дословно совпадают семь. Сверяем по
	// «фамилия + инициалы»; пустой ключ совпадением не считается.
	if in.HolderType != string(model.HolderPerson) || in.UserFIO == nil {
		return false, nil
	}
	wantKey := model.PersonKey(*in.UserFIO)
	if wantKey == "" {
		return false, nil
	}

	rows, err := tx.Query(ctx, `
		SELECT id, user_fio FROM wh_holding
		WHERE item_id = $1
		  AND holder_type = 'person'
		  AND inv_number_norm IS NULL
		  AND qty_expected >= 1
		  AND user_fio IS NOT NULL
		ORDER BY qty_expected DESC
	`, itemID)
	if err != nil {
		return false, err
	}
	var matchID int64
	for rows.Next() {
		var id int64
		var fio string
		if err := rows.Scan(&id, &fio); err != nil {
			rows.Close()
			return false, err
		}
		if matchID == 0 && model.PersonKey(fio) == wantKey {
			matchID = id
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	if matchID == 0 {
		// Агрегат не нашёлся — вызывающий заведёт предмет излишком.
		return false, nil
	}

	if _, err := tx.Exec(ctx, `
		UPDATE wh_holding
		SET qty_expected = qty_expected - 1,
		    qty_split    = qty_split + 1,
		    updated_at   = NOW()
		WHERE id = $1
	`, matchID); err != nil {
		return false, err
	}
	return true, nil
}

func normalizeInvPair(raw *string) (any, any) {
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

// aggregateRows складывает строки с одинаковым естественным ключом.
//
// Ключ повторяет частичный индекс uq_wh_holding_natural: позиция + тип
// держателя + сам держатель + кабинет. Строки С инвентарным номером НЕ
// агрегируются: у них свой уникальный индекс, и каждый номер — отдельный предмет
// («инв. 00123 в количестве 5» — бессмыслица).
//
// Складываем ДО записи, а не через `qty_expected = qty_expected + EXCLUDED`,
// потому что тот же upsert обслуживает ПОВТОРНУЮ загрузку того же файла:
// накопление удвоило бы остатки при второй загрузке. Сложить в памяти и записать
// итог — единственный способ получить верную сумму в обоих случаях.
func aggregateRows(rows []parser.Row) []parser.Row {
	type key struct {
		item, holder, user, dept, warehouse, room string
	}

	out := make([]parser.Row, 0, len(rows))
	index := make(map[key]int, len(rows))

	// Номерные строки дедуплицируются по нормализованному номеру: один предмет
	// встречается в реестре многократно (общий лист + лист отдела), а номер
	// именует РОВНО ОДИН предмет. Поля добираем из более поздних строк — там,
	// где в первой было пусто.
	byInv := make(map[string]int, len(rows))

	for _, r := range rows {
		if r.InvNumber != "" {
			norm, ok := model.NormalizeInvNumber(r.InvNumber)
			if ok {
				if pos, seen := byInv[norm]; seen {
					fillEmpty(&out[pos], r)
					continue
				}
				byInv[norm] = len(out)
			}
			out = append(out, r)
			continue
		}
		k := key{
			item:      model.NormalizeName(r.ItemName),
			holder:    string(r.HolderType),
			user:      model.NormalizeFIO(r.UserFIO),
			dept:      model.NormalizeName(r.DepartmentName),
			warehouse: model.NormalizeName(r.WarehouseName),
			room:      model.NormalizeName(r.Room),
		}
		if pos, seen := index[k]; seen {
			out[pos].Qty = out[pos].Qty.Add(r.Qty)
			if out[pos].QtyActual != nil && r.QtyActual != nil {
				sum := out[pos].QtyActual.Add(*r.QtyActual)
				out[pos].QtyActual = &sum
			} else if r.QtyActual != nil {
				out[pos].QtyActual = r.QtyActual
			}
			continue
		}
		index[k] = len(out)
		out = append(out, r)
	}
	return out
}

func int32Ptr(v int) *int32 {
	if v == 0 {
		return nil
	}
	x := int32(v)
	return &x
}

// holdingExistsByInv сообщает, есть ли уже строка с таким инвентарным номером.
//
// Нужна, чтобы расщепление агрегата случилось РОВНО ОДИН раз на предмет:
// один и тот же номер встречается в реестре многократно (общий лист плюс лист
// отдела), и повторное расщепление молча уводит остатки вниз.
func holdingExistsByInv(ctx context.Context, tx pgx.Tx, raw *string) (bool, error) {
	if raw == nil {
		return false, nil
	}
	norm, ok := model.NormalizeInvNumber(model.CleanText(*raw))
	if !ok {
		return false, nil
	}
	var exists bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM wh_holding WHERE inv_number_norm = $1)`, norm).Scan(&exists)
	return exists, err
}

// fillEmpty добирает пустые поля строки из её дубликата: один и тот же предмет
// на разных листах реестра описан с разной полнотой (где-то есть серийник,
// где-то держатель). Заполненное НЕ перезаписывается.
func fillEmpty(dst *parser.Row, src parser.Row) {
	if dst.SerialNumber == "" {
		dst.SerialNumber = src.SerialNumber
	}
	if dst.Note == "" {
		dst.Note = src.Note
	}
	if dst.Room == "" {
		dst.Room = src.Room
	}
	// Держатель уточняется, только если в первой строке его не выяснили.
	if dst.HolderType == model.HolderUnknown && src.HolderType != model.HolderUnknown {
		dst.HolderType = src.HolderType
		dst.UserFIO = src.UserFIO
		dst.DepartmentName = src.DepartmentName
		dst.WarehouseName = src.WarehouseName
	}
}
