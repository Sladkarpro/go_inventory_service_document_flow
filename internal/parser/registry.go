package parser

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/shopspring/decimal"
	"github.com/xuri/excelize/v2"

	"go_warehouse_service/internal/model"
)

// ---------------------------------------------------------------------------
// Разбор реестра «инвентаризация 2025-2026» — 40 листов, три раскладки.
//
// ⚠ РАСКЛАДКУ ОПРЕДЕЛЯЕМ ПО СОДЕРЖИМОМУ, А НЕ ПО ПОЗИЦИИ КОЛОНКИ.
// Шапка есть только на 2 листах из 40, остальные 38 начинаются сразу с данных,
// и колонки на них «плывут»:
//
//	B/D/G — большинство листов («абонотдел», «ИТО отдел», «Закупка»…)
//	A/C/F — «малоценка (инструменты) СМУ», «Оргтехника в ремонте», «отдел труда и зп»
//	сетка — «Лист1», «Лист2», «Лист7», «Лист8»: одни номера, наименований нет
//
// Если применить B/D/G ко всем листам, листы с раскладкой A/C/F дадут строки
// с пустым наименованием, item_id NOT NULL их не пропустит, и предметы ТИХО
// выпадут из инвентаризации.
// ---------------------------------------------------------------------------

// invNumberRe — что считается инвентарным номером у этого владельца.
// Дефис обязателен в наборе: на «Лист1» номера записаны как «БП-000336».
var invNumberRe = regexp.MustCompile(`^(?i)(ДСК|БП|БУ)[\s\-–—]*\d+`)

// gridSheetLimit — сколько строк листа хватает, чтобы понять раскладку.
const probeRows = 25

func looksLikeInv(v string) bool {
	return invNumberRe.MatchString(strings.TrimSpace(model.CleanText(v)))
}

// isPlaceholderInv — «б/н» и родня. Такие ячейки означают «номер не присвоен»,
// строка при этом законная и импортируется с inv_number = NULL.
func isPlaceholderInv(v string) bool {
	if strings.TrimSpace(v) == "" {
		return false
	}
	_, ok := model.NormalizeInvNumber(v)
	return !ok
}

// isDescriptive — похоже ли значение на наименование оборудования.
//
// ⚠ Требование «не меньше трёх букв» — не придирка. На «Лист1» колонка G
// заполнена значениями вида «00-000095»: девять символов, но ни одной буквы.
// Без проверки на буквы такая колонка проходит как наименование, лист-сетка
// принимается за обычный, и в инвентаризацию уезжают 35 мусорных строк.
func isDescriptive(v string) bool {
	s := strings.TrimSpace(model.CleanText(v))
	if s == "" || looksLikeInv(s) || isPlaceholderInv(s) {
		return false
	}
	letters := 0
	for _, r := range s {
		if (r >= 'а' && r <= 'я') || (r >= 'А' && r <= 'Я') ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == 'ё' || r == 'Ё' {
			letters++
		}
	}
	return letters >= 3 && len([]rune(s)) >= 6
}

// isTextish — годится ли колонка на роль держателя. Требования мягче, чем
// к наименованию: держатель бывает коротким («Каб.№5», «Склад»).
func isTextish(v string) bool {
	s := strings.TrimSpace(model.CleanText(v))
	if s == "" || looksLikeInv(s) || isPlaceholderInv(s) {
		return false
	}
	if _, isNum := parseQty(s); isNum {
		return false
	}
	return true
}

type regLayout struct {
	kind      string // "rows" | "grid" | "empty"
	source    string // "hdr" | "auto"
	headerRow int
	invCol    int
	nameCol   int
	holderCol int
	personCol int
	noteCol   int
	serialCol int
	gridCols  []int
}

// detectByHeader — шапка есть лишь на двух листах, но где есть, там она точнее
// любой эвристики: на «инв.ном.общ» отдельно подписаны «Отдел», «Ответственное
// лицо», «Примечание» и «серийный номер», и различить их по содержимому нельзя.
func detectByHeader(s sheetData) (regLayout, bool) {
	lay := regLayout{source: "hdr"}
	maxRow, maxCol := min(s.maxRow(), probeRows), s.maxCol()

	for r := 1; r <= maxRow; r++ {
		var inv, name, holder, person, note, serial int
		for c := 1; c <= maxCol; c++ {
			h := normHeader(s.cell(r, c))
			switch {
			case h == "":
			case inv == 0 && (strings.HasPrefix(h, "инвентарный номер") || strings.HasPrefix(h, "инв.номер") || strings.HasPrefix(h, "инв. номер")):
				inv = c
			case name == 0 && strings.HasPrefix(h, "наименование"):
				name = c
			case holder == 0 && (h == "отдел" || strings.HasPrefix(h, "подразделение")):
				holder = c
			case person == 0 && strings.HasPrefix(h, "ответственное лицо"):
				person = c
			case note == 0 && strings.HasPrefix(h, "примечание"):
				note = c
			case serial == 0 && (strings.HasPrefix(h, "серийный") || strings.HasPrefix(h, "заводской")):
				serial = c
			}
		}
		if inv != 0 && name != 0 {
			lay.kind = "rows"
			lay.headerRow = r
			lay.invCol, lay.nameCol = inv, name
			lay.holderCol, lay.personCol = holder, person
			lay.noteCol, lay.serialCol = note, serial
			return lay, true
		}
	}
	return lay, false
}

// detectByContent — раскладка по содержимому для 38 листов без шапки.
func detectByContent(s sheetData) regLayout {
	lay := regLayout{source: "auto"}
	maxCol := s.maxCol()
	if s.maxRow() == 0 || maxCol == 0 {
		lay.kind = "empty"
		return lay
	}

	// пробуем первые непустые строки
	var probe [][]string
	for r := 1; r <= s.maxRow() && len(probe) < probeRows; r++ {
		row := make([]string, maxCol)
		nonEmpty := false
		for c := 1; c <= maxCol; c++ {
			row[c-1] = s.cell(r, c)
			if row[c-1] != "" {
				nonEmpty = true
			}
		}
		if nonEmpty {
			probe = append(probe, row)
		}
	}
	if len(probe) == 0 {
		lay.kind = "empty"
		return lay
	}

	values := func(c int) []string {
		var out []string
		for _, row := range probe {
			if c-1 < len(row) && row[c-1] != "" {
				out = append(out, row[c-1])
			}
		}
		return out
	}
	share := func(c int, pred func(string) bool) float64 {
		v := values(c)
		if len(v) == 0 {
			return 0
		}
		n := 0
		for _, x := range v {
			if pred(x) {
				n++
			}
		}
		return float64(n) / float64(len(v))
	}

	var invCols []int
	for c := 1; c <= maxCol; c++ {
		if len(values(c)) > 0 && share(c, func(v string) bool {
			return looksLikeInv(v) || isPlaceholderInv(v)
		}) >= 0.7 {
			invCols = append(invCols, c)
		}
	}
	if len(invCols) == 0 {
		lay.kind = "empty"
		return lay
	}

	minFill := max(1, len(probe)/3)
	nameCol := 0
	for c := 1; c <= maxCol; c++ {
		if containsInt(invCols, c) || len(values(c)) < minFill {
			continue
		}
		if share(c, isDescriptive) >= 0.6 {
			nameCol = c
			break
		}
	}

	// Ни одной описательной колонки — это голая сетка номеров, чем бы ни
	// оказались прочие колонки. Именно так распознаются «Лист1»/«Лист2»/«Лист7»/«Лист8».
	if nameCol == 0 {
		lay.kind = "grid"
		lay.gridCols = invCols
		return lay
	}

	invCol := invCols[0]
	for _, c := range invCols {
		if c < nameCol {
			invCol = c
		}
	}

	holderCol := 0
	for c := nameCol + 1; c <= maxCol; c++ {
		if containsInt(invCols, c) || len(values(c)) < max(1, len(probe)/4) {
			continue
		}
		if share(c, isTextish) >= 0.6 {
			holderCol = c
			break
		}
	}

	lay.kind = "rows"
	lay.invCol, lay.nameCol, lay.holderCol = invCol, nameCol, holderCol
	return lay
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// firstDataRow — сколько строк сверху занимает шапка при автоопределении.
func firstDataRow(s sheetData, invCol int) int {
	for r := 1; r <= min(s.maxRow(), 30); r++ {
		v := s.cell(r, invCol)
		if looksLikeInv(v) || isPlaceholderInv(v) {
			return r
		}
	}
	return 1
}

func parseRegistry(path string) (*Result, error) {
	f, err := excelize.OpenFile(path)
	if err != nil {
		return nil, fmt.Errorf("открытие файла: %w", err)
	}
	defer func() { _ = f.Close() }()

	res := &Result{Format: FormatRegistry, File: filepath.Base(path)}

	for _, sheet := range f.GetSheetList() {
		s, err := loadSheet(f, sheet, false)
		if err != nil {
			res.Warnings = append(res.Warnings, Warning{Sheet: sheet, Message: err.Error()})
			continue
		}

		lay, ok := detectByHeader(s)
		start := 0
		if ok {
			start = lay.headerRow
			// строка «1 2 3 4 5 6» под шапкой — нумерация колонок, не данные
			for r := start + 1; r <= min(s.maxRow(), start+3); r++ {
				if v := s.cell(r, lay.invCol); v != "" && !looksLikeInv(v) && !isPlaceholderInv(v) {
					start = r
					continue
				}
				break
			}
		} else {
			lay = detectByContent(s)
			if lay.kind == "rows" {
				start = firstDataRow(s, lay.invCol) - 1
			}
		}

		switch lay.kind {
		case "empty":
			res.Sheets = append(res.Sheets, SheetReport{Sheet: sheet, Layout: "пустой", Rows: 0})
			continue

		case "grid":
			// Листы-сетки: номера без наименований. Что это за номера и нужны ли
			// они — открытый вопрос владельцу (§13.3 плана), поэтому строки не
			// создаём, но и не выбрасываем молча: номера возвращаются наружу.
			n := 0
			for r := 1; r <= s.maxRow(); r++ {
				for _, c := range lay.gridCols {
					if v := s.cell(r, c); looksLikeInv(v) {
						res.SkippedNumbers = append(res.SkippedNumbers, model.CleanText(v))
						n++
					}
				}
			}
			res.Sheets = append(res.Sheets, SheetReport{
				Sheet: sheet, Layout: "сетка номеров (не импортируется)", Rows: n,
			})
			res.Warnings = append(res.Warnings, Warning{
				Sheet: sheet,
				Message: fmt.Sprintf(
					"лист-сетка: %d инвентарных номеров без наименований — требуется решение владельца, не импортированы", n),
			})
			continue
		}

		n := 0
		for r := start + 1; r <= s.maxRow(); r++ {
			raw := s.cell(r, lay.invCol)
			name := model.CleanText(s.cell(r, lay.nameCol))
			if raw == "" || (!looksLikeInv(raw) && !isPlaceholderInv(raw)) {
				// ⚠ Наименование есть, а номера нет или он не опознан: пустая
				// ячейка, «выяснить», «ЛИЧНЫЙ», «ОС 00-000018». Импортировать
				// такую строку нельзя (номер — ключ экземпляра), но и МОЛЧА
				// выбросить нельзя.
				//
				// Непоследовательность, из-за которой это и нашлось: заглушка
				// «б/н» в той же ячейке фильтр ПРОХОДИТ и становится NULL,
				// а пустая ячейка исчезала без следа — ни в Warnings,
				// ни в SkippedNumbers, ни в NamelessNumbers.
				//
				// Замерено на файле владельца: таких строк ровно 10, у всех
				// осмысленное наименование, у девяти есть держатель. Мусора
				// среди отброшенного нет — isDescriptive отсекает заметки
				// в чужих колонках, поэтому предупреждений тоже ровно 10.
				if isDescriptive(name) {
					res.Warnings = append(res.Warnings, Warning{
						Sheet: sheet, Row: r,
						Message: fmt.Sprintf(
							"позиция «%s»: инвентарный номер не заполнен или не распознан (%q) — "+
								"строка не импортирована", name, model.CleanText(raw)),
					})
				}
				continue
			}

			holder := model.CleanText(s.cell(r, lay.holderCol))
			if holder == "" && lay.personCol != 0 {
				holder = model.CleanText(s.cell(r, lay.personCol))
			}

			row := Row{
				ItemName: name,
				// Реестр НЕ добавляет количество: он расщепляет уже учтённый
				// агрегат на именованные экземпляры. Единица здесь — это «один
				// предмет с этим номером», а не «плюс один к остатку».
				Qty:          decimal.NewFromInt(1),
				InvNumber:    model.CleanText(raw),
				SerialNumber: model.CleanText(s.cell(r, lay.serialCol)),
				Note:         model.CleanText(s.cell(r, lay.noteCol)),
				SourceFile:   res.File,
				SourceSheet:  sheet,
				SourceRow:    r,
			}
			if _, ok := model.NormalizeInvNumber(row.InvNumber); !ok {
				// заглушка «б/н» — предмет есть, номера у него нет
				row.InvNumber = ""
			} else if why := suspiciousInvNumber(row.InvNumber); why != "" {
				// Строка импортируется как есть — но владелец обязан её увидеть.
				res.Warnings = append(res.Warnings, Warning{
					Sheet: sheet, Row: r,
					Message: fmt.Sprintf("инвентарный номер «%s»: %s — проверьте вручную",
						row.InvNumber, why),
				})
			}

			// Держатель. Правило из §7 плана: «чьё» решает содержимое ячейки,
			// а не то, как называется лист. На листе «Склад ИТ» встречаются
			// строки, закреплённые за человеком, — предмет физически на складе,
			// но числится за сотрудником.
			switch {
			case holder == "":
				row.HolderType = model.HolderUnknown
			case looksLikePerson(holder):
				row.HolderType = model.HolderPerson
				row.UserFIO = holder
			default:
				row.HolderType = model.HolderDepartment
				row.DepartmentName = holder
			}

			if name == "" {
				// Номер записан, наименования нет. Строкой закрепления стать
				// не может (item_id NOT NULL), но и потеряться не должен.
				res.NamelessNumbers = append(res.NamelessNumbers, row.InvNumber)
				res.Warnings = append(res.Warnings, Warning{
					Sheet: sheet, Row: r,
					Message: fmt.Sprintf("номер «%s» без наименования — не импортирован", row.InvNumber),
				})
				continue
			}

			res.Rows = append(res.Rows, row)
			n++
		}

		res.Sheets = append(res.Sheets, SheetReport{
			Sheet:  sheet,
			Layout: fmt.Sprintf("%s:%s/%s/%s", lay.source, colName(lay.invCol), colName(lay.nameCol), colName(lay.holderCol)),
			Rows:   n,
		})
	}

	return res, nil
}

func colName(c int) string {
	if c == 0 {
		return "-"
	}
	n, _ := excelize.ColumnNumberToName(c)
	return n
}

// fioRe ловит «Фамилия И.О.» и «Фамилия И. О.» — самую частую форму в реестре
// («Бражкина Н.А.», «Фаустов А.А.»).
var fioRe = regexp.MustCompile(`\p{Cyrillic}{2,}\s+\p{Cyrillic}\s*\.\s*\p{Cyrillic}?\s*\.?`)

// compoundInvRe ловит ячейку, в которой записан НЕ ОДИН номер:
//
//	«ДСК0454;ДСК0454/1;ДСК0454/2»  — перечисление через ; или /
//	«ДСК0263-0267»                 — диапазон номеров
//
// ⚠ Такую ячейку нельзя разбирать на свой страх: диапазон «0263-0267» — это
// пять физических предметов, записанных одной строкой, и автоматическое
// раскрытие изменило бы количество в инвентаризации. Но и промолчать нельзя:
// пять предметов, посчитанных как один, — это недостача, которой никто
// не заметит. Поэтому парсер их ПОМЕЧАЕТ, а решение принимает владелец.
//
// Суффикс «/1», «/2» (часть комплекта) составным номером НЕ считается:
// это законная форма записи одного предмета.
var (
	compoundInvRe = regexp.MustCompile(`[;,]|/\s*[А-Яа-яA-Za-z]`)
	rangeInvRe    = regexp.MustCompile(`^\s*[А-Яа-яA-Za-z]+\s*\d+\s*-\s*\d{3,}\s*$`)
)

// suspiciousInvNumber сообщает, что в ячейке, похоже, не один номер.
func suspiciousInvNumber(s string) string {
	switch {
	case rangeInvRe.MatchString(s):
		return "похоже на ДИАПАЗОН номеров — это несколько предметов, записанных одной строкой"
	case compoundInvRe.MatchString(s):
		return "похоже на НЕСКОЛЬКО номеров в одной ячейке"
	}
	return ""
}

// looksLikePerson отличает человека от подразделения в ячейке держателя.
//
// В реестре они перемешаны в одной колонке: «Ширяев И.В. Генеральный директор»
// соседствует с «Подразделение эксплуатации полигонов» и «Каб.№5». Различить
// их надёжно нельзя — берём инициалы как признак, остальное считаем
// подразделением. Спорные случаи правятся вручную на экране пересчёта.
func looksLikePerson(s string) bool {
	return fioRe.MatchString(s)
}
