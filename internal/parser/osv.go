package parser

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/shopspring/decimal"
	"github.com/xuri/excelize/v2"

	"go_warehouse_service/internal/model"
)

// Разбор «оборотно-сальдовой ведомости» 1С — форматы МЦ.04, счёт 10, ручная.
//
// СТРУКТУРА ЧИТАЕТСЯ ИЗ ШАПКИ, А НЕ ЗАШИТА КОНСТАНТАМИ. Это не украшательство:
//   - в выгрузке по складу ИТ уровни идут «Подразделение → Склады → Номенклатура»,
//     а в ручной ведомости — «Склады → Подразделение → Номенклатура»;
//   - в выгрузке по ЛМЗ добавлен четвёртый уровень «Партии», который повторяет
//     количество номенклатуры и при наивном суммировании удваивает остаток;
//   - количество лежит в колонке F у МЦ.04 и в колонке G у счёта 10.
//
// Всё это объявлено в шапке файла, и парсер читает именно её.

// уровни группировки, объявленные в шапке (первая колонка под строкой «Счет»)
const (
	levelPerson     = "person"
	levelDepartment = "department"
	levelWarehouse  = "warehouse"
	levelItem       = "item"
	levelBatch      = "batch"
	levelUnknown    = "unknown"
)

// classifyLevel сопоставляет подпись уровня из шапки с ролью в модели.
func classifyLevel(header string) string {
	switch {
	case strings.HasPrefix(header, "работник"):
		return levelPerson
	case strings.HasPrefix(header, "номенклатура"):
		return levelItem
	case strings.HasPrefix(header, "склад"):
		return levelWarehouse
	case strings.HasPrefix(header, "подразделение"):
		return levelDepartment
	case strings.HasPrefix(header, "парти"):
		// ⚠ «Партии» повторяют количество номенклатуры. Их обязательно пропустить,
		// иначе выгрузка по ЛМЗ даёт 1356 единиц вместо 678 — ровно вдвое.
		return levelBatch
	default:
		return levelUnknown
	}
}

type osvLayout struct {
	headerRow    int            // строка с «Счет» в колонке A
	firstDataRow int            // первая строка с числом в колонке количества
	qtyCol       int            // «Сальдо на конец периода» / «Дебет»
	indicatorCol int            // «Показатели» (БУ / Кол.); 0 — однострочный формат
	factCol      int            // «Фактическое наличие» (ручная ведомость)
	noteCol      int            // «Примечание» (ручная ведомость)
	levels       map[int]string // отступ → роль уровня
	levelIndent  map[string]int // роль уровня → отступ
}

// sheetData — разобранный лист: значения (сырые, без форматирования числа)
// и отступ первой колонки для каждой строки.
type sheetData struct {
	rows    [][]string
	indents []int
	// contRow — строки, которые являются продолжением вертикально объединённой
	// ячейки. excelize отдаёт значение объединённой ячейки НА КАЖДОЙ строке
	// диапазона, поэтому без этого множества каждая позиция считалась бы дважды.
	contRow map[int]bool
}

func (s sheetData) cell(row, col int) string {
	if row < 1 || row > len(s.rows) || col < 1 {
		return ""
	}
	r := s.rows[row-1]
	if col > len(r) {
		return ""
	}
	return strings.TrimSpace(r[col-1])
}

func (s sheetData) indent(row int) int {
	if row < 1 || row > len(s.indents) {
		return 0
	}
	return s.indents[row-1]
}

func (s sheetData) maxRow() int { return len(s.rows) }

func (s sheetData) maxCol() int {
	m := 0
	for _, r := range s.rows {
		if len(r) > m {
			m = len(r)
		}
	}
	return m
}

// loadSheet читает лист целиком.
//
// ⚠ RawCellValue обязателен. Без него excelize применяет числовой формат ячейки
// и отдаёт «2,328,405.23» вместо «2328405.23» — разделители разрядов превращают
// количество в мусор, и это НЕ выглядит поломкой.
func loadSheet(f *excelize.File, sheet string, needIndent bool) (sheetData, error) {
	rows, err := f.GetRows(sheet, excelize.Options{RawCellValue: true})
	if err != nil {
		return sheetData{}, fmt.Errorf("чтение листа %q: %w", sheet, err)
	}

	data := sheetData{rows: rows, indents: make([]int, len(rows)), contRow: map[int]bool{}}

	if needIndent {
		// Отступ несёт уровень иерархии. Стилей в файле немного, поэтому кэшируем.
		indentByStyle := make(map[int]int)
		for i := range rows {
			axis, err := excelize.CoordinatesToCellName(1, i+1)
			if err != nil {
				continue
			}
			styleID, err := f.GetCellStyle(sheet, axis)
			if err != nil {
				continue
			}
			ind, ok := indentByStyle[styleID]
			if !ok {
				if st, err := f.GetStyle(styleID); err == nil && st.Alignment != nil {
					ind = st.Alignment.Indent
				}
				indentByStyle[styleID] = ind
			}
			data.indents[i] = ind
		}
	}

	merged, err := f.GetMergeCells(sheet)
	if err == nil {
		for _, m := range merged {
			c1, r1, err1 := excelize.CellNameToCoordinates(m.GetStartAxis())
			c2, r2, err2 := excelize.CellNameToCoordinates(m.GetEndAxis())
			if err1 != nil || err2 != nil {
				continue
			}
			// интересуют только объединения, накрывающие первую колонку
			if c1 > 1 || c2 < 1 || r2 <= r1 {
				continue
			}
			for r := r1 + 1; r <= r2; r++ {
				data.contRow[r] = true
			}
		}
	}
	return data, nil
}

// detectOSVLayout восстанавливает структуру отчёта по его шапке.
func detectOSVLayout(s sheetData) (osvLayout, error) {
	lay := osvLayout{levels: map[int]string{}, levelIndent: map[string]int{}}
	maxRow, maxCol := s.maxRow(), s.maxCol()

	// 1. Строка шапки — та, где в колонке A стоит «Счет».
	for r := 1; r <= min(maxRow, 40); r++ {
		if normHeader(s.cell(r, 1)) == "счет" {
			lay.headerRow = r
			break
		}
	}
	if lay.headerRow == 0 {
		return lay, fmt.Errorf("не найдена строка шапки со «Счет» в первой колонке")
	}
	hr := lay.headerRow

	// 2. Группы колонок стоят в строке шапки, подписи «Дебет»/«Кредит» — строкой ниже.
	//    Количество лежит в дебете САЛЬДО НА КОНЕЦ ПЕРИОДА: это остаток на дату,
	//    а не оборот. Такой поиск переживает и МЦ.04 (колонка F), и счёт 10 (G).
	groups := make(map[int]string)
	for c := 1; c <= maxCol; c++ {
		if v := normHeader(s.cell(hr, c)); v != "" {
			groups[c] = v
		}
	}
	groupOf := func(col int) string {
		name, best := "", 0
		for gc, g := range groups {
			if gc <= col && gc > best {
				name, best = g, gc
			}
		}
		return name
	}
	for c := 2; c <= maxCol; c++ {
		if normHeader(s.cell(hr+1, c)) == "дебет" && groupOf(c) == "сальдо на конец периода" {
			lay.qtyCol = c
			break
		}
	}
	if lay.qtyCol == 0 {
		return lay, fmt.Errorf("не найдена колонка количества («Сальдо на конец периода» / «Дебет»)")
	}

	// 3. Колонка «Показатели» означает двухстрочный формат: строка позиции несёт
	//    сумму (БУ), а количество лежит на СЛЕДУЮЩЕЙ строке с пометкой «Кол.».
	for c, name := range groups {
		switch {
		case strings.HasPrefix(name, "показа"):
			lay.indicatorCol = c
		case strings.HasPrefix(name, "фактическое"):
			lay.factCol = c
		case strings.HasPrefix(name, "примечание"):
			lay.noteCol = c
		}
	}

	// 4. Данные начинаются с первой строки, где в колонке количества стоит число.
	for r := hr + 1; r <= maxRow; r++ {
		if _, ok := parseQty(s.cell(r, lay.qtyCol)); ok {
			lay.firstDataRow = r
			break
		}
	}
	if lay.firstDataRow == 0 {
		return lay, fmt.Errorf("в отчёте нет строк с количеством")
	}

	// 5. Уровни группировки перечислены в колонке A между шапкой и первой строкой данных.
	//    Их порядок и состав меняются от файла к файлу — читаем, а не предполагаем.
	indent := 1
	for r := hr + 1; r < lay.firstDataRow; r++ {
		v := normHeader(s.cell(r, 1))
		if v == "" {
			continue
		}
		kind := classifyLevel(v)
		lay.levels[indent] = kind
		if _, seen := lay.levelIndent[kind]; !seen {
			lay.levelIndent[kind] = indent
		}
		indent++
	}
	if _, ok := lay.levelIndent[levelItem]; !ok {
		return lay, fmt.Errorf("в шапке нет уровня «Номенклатура»")
	}
	return lay, nil
}

// parseQty читает количество. Принимает и «1 234,56», и «1234.56»:
// разделитель разрядов в 1С — неразрывный пробел, дробный — запятая или точка.
func parseQty(s string) (decimal.Decimal, bool) {
	s = strings.TrimSpace(model.CleanText(s))
	if s == "" {
		return decimal.Zero, false
	}
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, ",", ".")
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero, false
	}
	return d, true
}

func parseOSV(path string, format Format) (*Result, error) {
	f, err := excelize.OpenFile(path)
	if err != nil {
		return nil, fmt.Errorf("открытие файла: %w", err)
	}
	defer func() { _ = f.Close() }()

	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, fmt.Errorf("в файле нет листов")
	}
	sheet := sheets[0]

	s, err := loadSheet(f, sheet, true)
	if err != nil {
		return nil, err
	}
	lay, err := detectOSVLayout(s)
	if err != nil {
		return nil, fmt.Errorf("разбор шапки: %w", err)
	}

	res := &Result{Format: format, File: filepath.Base(path)}
	ctx := map[string]string{}

	for r := lay.firstDataRow; r <= s.maxRow(); r++ {
		// продолжение вертикально объединённой ячейки — значение уже учтено
		if s.contRow[r] {
			continue
		}
		name := model.CleanText(s.cell(r, 1))
		if name == "" {
			continue
		}
		if normHeader(name) == "итого" {
			break
		}

		ind := s.indent(r)
		if ind == 0 {
			// строка самого счёта («10», «МЦ.04») — итог по файлу, не данные
			ctx = map[string]string{}
			continue
		}
		kind, known := lay.levels[ind]
		if !known {
			res.Warnings = append(res.Warnings, Warning{
				Sheet: sheet, Row: r,
				Message: fmt.Sprintf("неизвестный уровень вложенности %d у «%s»", ind, name),
			})
			continue
		}
		if kind == levelBatch {
			continue // партии повторяют количество номенклатуры
		}

		if kind != levelItem {
			// уровень группировки: запоминаем и сбрасываем всё, что глубже
			for k, v := range ctx {
				_ = v
				if lay.levelIndent[k] >= ind {
					delete(ctx, k)
				}
			}
			ctx[kind] = name
			continue
		}

		// --- строка номенклатуры ---
		qtyRow := r
		if lay.indicatorCol != 0 {
			cur := normHeader(s.cell(r, lay.indicatorCol))
			switch {
			case strings.HasPrefix(cur, "бу"):
				if strings.HasPrefix(normHeader(s.cell(r+1, lay.indicatorCol)), "кол") {
					qtyRow = r + 1
				} else {
					// у позиции есть сумма, но нет количества — импортировать нечего
					res.Warnings = append(res.Warnings, Warning{
						Sheet: sheet, Row: r,
						Message: fmt.Sprintf("у позиции «%s» нет строки «Кол.» — пропущена", name),
					})
					continue
				}

			case strings.HasPrefix(cur, "кол"):
				// Сама строка количества — читаем её же (qtyRow уже равен r).
				// Ветка нужна ровно для того, чтобы законную строку не съел
				// default ниже, если объединение шапки не накрыло колонку.

			default:
				// ⚠ Колонка «Показатели» в шапке ЕСТЬ, а подпись незнакомая
				// (другая версия 1С, другой язык выгрузки, пустая ячейка).
				// Считать, что количество лежит в этой же строке, НЕЛЬЗЯ:
				// в двухстрочном формате там стоит СУММА В РУБЛЯХ, она
				// разбирается без ошибки и уехала бы молча.
				//
				// Замерено на файлах владельца: склад ИТ дал бы 2328405.23
				// вместо 205, ЛМЗ — 28315944.96 вместо 678. (Удвоение до 1356 —
				// это другой механизм, уровень «Партии», см. ADR-002 §4.)
				//
				// Отказ, а не «разберём как-нибудь»: то же решение, что
				// в соседней ветке выше, и та же доктрина ADR-002 §3.
				// Пропущенная строка видна в отчёте и в контрольной сумме,
				// а 2,3 млн в количестве отравят агрегат незаметно.
				res.Warnings = append(res.Warnings, Warning{
					Sheet: sheet, Row: r,
					Message: fmt.Sprintf(
						"у позиции «%s» неизвестная подпись в колонке «Показатели» (%q) — "+
							"строка пропущена, чтобы в количество не попала сумма", name, cur),
				})
				continue
			}
		}
		qty, ok := parseQty(s.cell(qtyRow, lay.qtyCol))
		if !ok {
			res.Warnings = append(res.Warnings, Warning{
				Sheet: sheet, Row: r,
				Message: fmt.Sprintf("не разобрано количество у «%s»", name),
			})
			continue
		}

		row := Row{
			ItemName:    name,
			Qty:         qty,
			SourceFile:  res.File,
			SourceSheet: sheet,
			SourceRow:   r,
		}

		// Держатель. Порядок проверок задаёт приоритет: человек важнее склада,
		// склад важнее подразделения — «чьё» перевешивает «где лежит».
		switch {
		case ctx[levelPerson] != "":
			row.HolderType = model.HolderPerson
			row.UserFIO = ctx[levelPerson]
		case ctx[levelWarehouse] != "":
			row.HolderType = model.HolderWarehouse
			row.WarehouseName = ctx[levelWarehouse]
			// Подразделение у складской строки заполнено и это не ошибка:
			// складской остаток изначально приписан подразделению (иерархия счёта 10).
			row.DepartmentName = ctx[levelDepartment]
		case ctx[levelDepartment] != "":
			row.HolderType = model.HolderDepartment
			row.DepartmentName = ctx[levelDepartment]
		default:
			row.HolderType = model.HolderUnknown
		}

		// Ручная ведомость: факт и примечание владельца.
		if lay.factCol != 0 {
			if fact, ok := parseQty(s.cell(qtyRow, lay.factCol)); ok {
				row.QtyActual = &fact
				switch {
				case fact.LessThan(qty):
					row.Outcome = string(model.OutcomeMissing)
				case fact.GreaterThan(qty):
					row.Outcome = string(model.OutcomeSurplus)
				default:
					row.Outcome = string(model.OutcomeFound)
				}
			}
		}
		if lay.noteCol != 0 {
			row.Note = model.CleanText(s.cell(qtyRow, lay.noteCol))
		}

		res.Rows = append(res.Rows, row)
	}

	res.Sheets = []SheetReport{{Sheet: sheet, Layout: describeLayout(lay), Rows: len(res.Rows)}}
	return res, nil
}

func describeLayout(lay osvLayout) string {
	col := func(c int) string {
		if c == 0 {
			return "-"
		}
		n, _ := excelize.ColumnNumberToName(c)
		return n
	}
	order := make([]string, 0, len(lay.levels))
	for i := 1; i <= len(lay.levels); i++ {
		if k, ok := lay.levels[i]; ok {
			order = append(order, k)
		}
	}
	return fmt.Sprintf("qty=%s indicator=%s fact=%s levels=%s",
		col(lay.qtyCol), col(lay.indicatorCol), col(lay.factCol), strings.Join(order, ">"))
}
