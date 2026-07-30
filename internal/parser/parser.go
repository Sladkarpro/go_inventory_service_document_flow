// Package parser разбирает выгрузки 1С и рабочий реестр владельца.
//
// ⚠ ЗДЕСЬ ОШИБКА ПРОХОДИТ МОЛЧА. Двухстрочный формат счёта 10 при небрежном
// чтении даёт нули вместо количеств и не выглядит поломкой; неверно определённая
// раскладка листа реестра тихо выбрасывает сотни предметов из инвентаризации.
// Поэтому контрольные суммы в parser_test.go — не отчёт, а тест: он обязан
// падать, если разбор поехал.
//
// Четыре формата:
//
//	FormatMC04      счёт МЦ.04  — «за людьми»,  сотрудник → номенклатура
//	FormatAccount10 счёт 10     — «на складе»,  подразделение → склад → номенклатура (двухстрочный)
//	FormatManual    ручная ведомость — тот же ОСВ плюс колонки факта и примечаний
//	FormatRegistry  реестр «инвентаризация 2025-2026» — 40 листов, три раскладки
//
// Первые три — это одна и та же «оборотно-сальдовая ведомость» 1С, и разбирает
// их ОДИН парсер (osv.go), который читает структуру из шапки, а не из констант.
// Именно поэтому он переживает то, что в ручной ведомости уровни идут в другом
// порядке (склад → подразделение вместо подразделение → склад), а в выгрузке
// по ЛМЗ добавлен лишний уровень «Партии».
package parser

import (
	"fmt"
	"strings"

	"github.com/shopspring/decimal"

	"go_warehouse_service/internal/model"
)

// Format — формат входного файла.
type Format string

const (
	FormatMC04      Format = "mc04"
	FormatAccount10 Format = "account10"
	FormatManual    Format = "manual"
	FormatRegistry  Format = "registry"
)

func ValidFormat(s string) bool {
	switch Format(s) {
	case FormatMC04, FormatAccount10, FormatManual, FormatRegistry:
		return true
	}
	return false
}

// Row — одна разобранная строка, ещё не привязанная к базе.
// Поля-строки хранятся как есть (уже очищенные от лишних пробелов);
// нормализацию под UNIQUE делает model.Normalize* на слое импорта.
type Row struct {
	ItemName string
	Qty      decimal.Decimal

	HolderType     model.HolderType
	UserFIO        string
	DepartmentName string
	WarehouseName  string
	Room           string

	InvNumber    string
	SerialNumber string
	Note         string

	// QtyActual заполняет только ручная ведомость: работа владельца,
	// уже проделанная руками, не должна пропасть при импорте.
	QtyActual *decimal.Decimal
	Outcome   string

	SourceFile  string
	SourceSheet string
	SourceRow   int
}

// Warning — то, что разобрать не удалось. Импорт из-за этого не падает,
// но владелец обязан увидеть список: молчаливая потеря строк здесь недопустима.
type Warning struct {
	Sheet   string
	Row     int
	Message string
}

func (w Warning) String() string {
	if w.Row > 0 {
		return fmt.Sprintf("%s, строка %d: %s", w.Sheet, w.Row, w.Message)
	}
	if w.Sheet != "" {
		return fmt.Sprintf("%s: %s", w.Sheet, w.Message)
	}
	return w.Message
}

// SheetReport — как был разобран лист. Нужен для отчёта об импорте:
// по нему видно, что раскладка определилась именно так, как ожидал владелец.
type SheetReport struct {
	Sheet  string
	Layout string
	Rows   int
}

// Result — итог разбора файла.
type Result struct {
	Format   Format
	File     string
	Rows     []Row
	Warnings []Warning
	Sheets   []SheetReport

	// SkippedNumbers — инвентарные номера с листов-сеток («Лист1», «Лист2»,
	// «Лист7», «Лист8»). Что это за номера и нужны ли они — открытый вопрос
	// владельцу, поэтому они не импортируются, но и не теряются: список
	// возвращается наружу.
	SkippedNumbers []string

	// NamelessNumbers — номера, записанные в реестре БЕЗ наименования.
	// Строкой закрепления такой номер стать не может: item_id NOT NULL.
	// ⚠ Молча их терять нельзя — это ровно тот класс ошибки, ради которого
	// заводились контрольные суммы. Владелец обязан увидеть список и дописать
	// наименования в реестре либо подтвердить, что номера отменены.
	NamelessNumbers []string
}

// TotalQty — сумма количеств. Ради неё существуют контрольные суммы §11 плана.
func (r Result) TotalQty() decimal.Decimal {
	sum := decimal.Zero
	for _, row := range r.Rows {
		sum = sum.Add(row.Qty)
	}
	return sum
}

// DistinctItemNames — размер каталога по нормализованному наименованию.
func (r Result) DistinctItemNames() int {
	seen := make(map[string]struct{}, len(r.Rows))
	for _, row := range r.Rows {
		if n := model.NormalizeName(row.ItemName); n != "" {
			seen[n] = struct{}{}
		}
	}
	return len(seen)
}

// DistinctHolders — число различных держателей заданного типа.
func (r Result) DistinctHolders(t model.HolderType) int {
	seen := make(map[string]struct{}, len(r.Rows))
	for _, row := range r.Rows {
		if row.HolderType != t {
			continue
		}
		var key string
		switch t {
		case model.HolderPerson:
			key = model.NormalizeFIO(row.UserFIO)
		case model.HolderDepartment:
			key = model.NormalizeName(row.DepartmentName)
		case model.HolderWarehouse:
			key = model.NormalizeName(row.WarehouseName)
		}
		if key != "" {
			seen[key] = struct{}{}
		}
	}
	return len(seen)
}

// Parse разбирает файл по явно указанному формату.
// Формат передаёт владелец в форме загрузки: угадывать его по содержимому
// нельзя — три из четырёх файлов внешне неотличимы, а цена ошибки
// (счёт 10 вместо МЦ.04) — задвоенные остатки.
func Parse(path string, format Format) (*Result, error) {
	switch format {
	case FormatMC04, FormatAccount10, FormatManual:
		return parseOSV(path, format)
	case FormatRegistry:
		return parseRegistry(path)
	default:
		return nil, fmt.Errorf("неизвестный формат %q", format)
	}
}

// normHeader приводит текст шапки к сравнимому виду.
// 1С переносит заголовки внутри ячейки («Показа-\nтели»), поэтому дефис
// с переводом строки склеивается обратно, а не превращается в пробел.
func normHeader(s string) string {
	s = strings.ReplaceAll(s, "-\n", "")
	s = strings.ReplaceAll(s, "\n", " ")
	return model.NormalizeName(s)
}
