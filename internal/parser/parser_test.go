package parser

import (
	"os"
	"path/filepath"
	"testing"

	"go_warehouse_service/internal/model"
)

// ---------------------------------------------------------------------------
// КОНТРОЛЬНЫЕ СУММЫ (§11 плана).
//
// Это не отчёт, а тест. Двухстрочный формат счёта 10 при небрежном чтении даёт
// нули вместо количеств и не выглядит поломкой; неверная раскладка листа реестра
// тихо выбрасывает сотни предметов. Единственная защита — считать суммы
// и сверять их с тем, что бухгалтерия видит в 1С.
//
// Файлы владельца лежат вне репозитория сервиса. Если их нет — тесты
// пропускаются, а не падают: на CI без данных падение было бы ложным.
// ---------------------------------------------------------------------------

const inventDir = `E:\projects\kanban\invent`

func file(t *testing.T, substr string) string {
	t.Helper()
	entries, err := os.ReadDir(inventDir)
	if err != nil {
		t.Skipf("каталог с выгрузками владельца недоступен (%v) — тест пропущен", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".xlsx" {
			continue
		}
		if containsFold(e.Name(), substr) {
			return filepath.Join(inventDir, e.Name())
		}
	}
	t.Skipf("файл с фрагментом %q не найден в %s — тест пропущен", substr, inventDir)
	return ""
}

func containsFold(s, sub string) bool {
	return len(sub) == 0 || len(s) >= len(sub) &&
		model.NormalizeName(s) != "" &&
		containsRunes(model.NormalizeName(s), model.NormalizeName(sub))
}

func containsRunes(s, sub string) bool {
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func mustParse(t *testing.T, path string, f Format) *Result {
	t.Helper()
	res, err := Parse(path, f)
	if err != nil {
		t.Fatalf("Parse(%s, %s): %v", filepath.Base(path), f, err)
	}
	return res
}

func eqInt(t *testing.T, label string, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %d, ожидалось %d", label, got, want)
	}
}

// TestMC04 — счёт МЦ.04 «за людьми»: 3029 единиц у 163 человек, 857 наименований.
func TestMC04(t *testing.T) {
	res := mustParse(t, file(t, "МЦ.04"), FormatMC04)

	if got := res.TotalQty().String(); got != "3029" {
		t.Errorf("сумма количеств = %s, ожидалось 3029", got)
	}
	eqInt(t, "человек", res.DistinctHolders(model.HolderPerson), 163)
	eqInt(t, "наименований", res.DistinctItemNames(), 857)
	eqInt(t, "строк", len(res.Rows), 1774)

	for _, r := range res.Rows {
		if r.HolderType != model.HolderPerson {
			t.Fatalf("в МЦ.04 все строки должны быть за людьми, встретилась %q (%s)",
				r.HolderType, r.ItemName)
		}
		if r.UserFIO == "" {
			t.Fatalf("строка %d без ФИО держателя", r.SourceRow)
		}
	}
	if len(res.Warnings) != 0 {
		t.Errorf("неожиданные предупреждения: %v", res.Warnings)
	}
}

// TestAccount10IT — счёт 10, склад ИТ: 205 единиц, 36 наименований.
//
// Здесь проверяется двухстрочный формат: количество лежит НЕ на строке позиции,
// а на следующей строке с пометкой «Кол.», и не в колонке суммы.
func TestAccount10IT(t *testing.T) {
	res := mustParse(t, file(t, "склад ИТ"), FormatAccount10)

	if got := res.TotalQty().String(); got != "205" {
		t.Errorf("сумма количеств = %s, ожидалось 205 "+
			"(нули означают, что двухстрочный формат прочитан как однострочный)", got)
	}
	eqInt(t, "наименований", res.DistinctItemNames(), 36)

	for _, r := range res.Rows {
		if r.HolderType != model.HolderWarehouse {
			t.Fatalf("в счёте 10 все строки складские, встретилась %q", r.HolderType)
		}
		if r.WarehouseName == "" {
			t.Fatalf("строка %d без склада", r.SourceRow)
		}
		// Подразделение у складской строки заполнено — иерархия счёта 10.
		if r.DepartmentName == "" {
			t.Fatalf("строка %d без подразделения", r.SourceRow)
		}
	}
}

// TestAccount10LMZ — склад ЛМЗ: 678 единиц, 3 наименования.
//
// В этом файле есть ЧЕТВЁРТЫЙ уровень «Партии», который повторяет количество
// номенклатуры. Если его не пропустить, сумма удвоится до 1356.
func TestAccount10LMZ(t *testing.T) {
	res := mustParse(t, file(t, "ЛМЗ"), FormatAccount10)

	if got := res.TotalQty().String(); got != "678" {
		t.Errorf("сумма количеств = %s, ожидалось 678 "+
			"(1356 означает, что уровень «Партии» посчитан как номенклатура)", got)
	}
	eqInt(t, "наименований", res.DistinctItemNames(), 3)
	eqInt(t, "строк", len(res.Rows), 3)
}

// TestThreeFilesTogether — три файла вместе: 3912 единиц, каталог 881 позиция.
//
// ⚠ Каталог именно 881, а не 917: 917 включало наименования из «Материалов
// выданных» (27.07), количества которых в 3912 не входят. Проверка на 917
// не сошлась бы никогда, и ошибку искали бы в нормализации имён, где её нет.
func TestThreeFilesTogether(t *testing.T) {
	mc04 := mustParse(t, file(t, "МЦ.04"), FormatMC04)
	it := mustParse(t, file(t, "склад ИТ"), FormatAccount10)
	lmz := mustParse(t, file(t, "ЛМЗ"), FormatAccount10)

	total := mc04.TotalQty().Add(it.TotalQty()).Add(lmz.TotalQty())
	if got := total.String(); got != "3912" {
		t.Errorf("сумма по трём файлам = %s, ожидалось 3912", got)
	}

	catalog := map[string]struct{}{}
	for _, res := range []*Result{mc04, it, lmz} {
		for _, r := range res.Rows {
			catalog[model.NormalizeName(r.ItemName)] = struct{}{}
		}
	}
	eqInt(t, "каталог позиций", len(catalog), 881)
}

// TestManualSheet — ручная ведомость владельца.
//
// Уровни здесь идут в ДРУГОМ порядке, чем в остальных ОСВ
// («Склады → Подразделение → Номенклатура»). Парсер, читающий шапку,
// это переживает; парсер с зашитыми константами — нет.
func TestManualSheet(t *testing.T) {
	res := mustParse(t, file(t, "сверка остатков"), FormatManual)

	eqInt(t, "позиций номенклатуры", len(res.Rows), 36)

	withFact := 0
	for _, r := range res.Rows {
		if r.QtyActual != nil {
			withFact++
			if r.Outcome == "" {
				t.Errorf("строка %d: есть факт, но не проставлен outcome", r.SourceRow)
			}
		}
	}
	eqInt(t, "позиций с фактом", withFact, 16)

	// Ради этого формат и разбирается: у владельца уже проставлен факт руками,
	// и он не должен пропасть.
	found := false
	for _, r := range res.Rows {
		if r.QtyActual != nil && r.Note != "" {
			found = true
			break
		}
	}
	if !found {
		t.Error("ни одна строка с фактом не принесла примечание владельца")
	}
}

// TestRegistry — реестр «инвентаризация 2025-2026», 40 листов.
func TestRegistry(t *testing.T) {
	res := mustParse(t, file(t, "инвентаризация"), FormatRegistry)

	// 23 заглушки «б/н» обязаны стать строками БЕЗ номера, а не схлопнуться
	// в один «Б/Н»: иначе уникальный индекс пропустил бы одну из них.
	placeholders := 0
	uniq := map[string]struct{}{}
	noHolder := 0
	for _, r := range res.Rows {
		if r.InvNumber == "" {
			placeholders++
			continue
		}
		n, ok := model.NormalizeInvNumber(r.InvNumber)
		if !ok {
			t.Errorf("строка %s:%d — номер %q сохранён, но не нормализуется",
				r.SourceSheet, r.SourceRow, r.InvNumber)
			continue
		}
		uniq[n] = struct{}{}
		if r.HolderType == model.HolderUnknown {
			noHolder++
		}
	}

	eqInt(t, "заглушек «б/н» → без номера", placeholders, 23)

	// Число уникальных номеров, пригодных к импорту (есть наименование).
	// ⚠ В §11 плана стоит 1113 — цифра получена наивным чтением колонки B
	// на ВСЕХ листах, включая листы-сетки, то есть ровно тем способом, который
	// §7 того же плана запрещает. Подробности и способ воспроизведения —
	// dev_docks/ADR-001-parser-checksums.md.
	eqInt(t, "уникальных импортируемых номеров", len(uniq), 1085)

	// Номера без наименования импортировать нельзя (item_id NOT NULL), но и
	// терять молча недопустимо: они возвращаются отдельным списком.
	namelessOnly := map[string]struct{}{}
	for _, raw := range res.NamelessNumbers {
		if n, ok := model.NormalizeInvNumber(raw); ok {
			if _, alsoNamed := uniq[n]; !alsoNamed {
				namelessOnly[n] = struct{}{}
			}
		}
	}
	eqInt(t, "номеров только без наименования", len(namelessOnly), 20)

	// 1085 импортируемых + 20 безымянных = 1105 уникальных номеров на листах
	// с данными. Это и есть настоящее число реестра при пораздельной раскладке.
	eqInt(t, "всего уникальных номеров на листах с данными", len(uniq)+len(namelessOnly), 1105)

	// Листы-сетки не импортируются, но и не теряются.
	if len(res.SkippedNumbers) == 0 {
		t.Error("номера с листов-сеток должны возвращаться наружу, а не исчезать")
	}

	gridSheets := 0
	for _, sh := range res.Sheets {
		if sh.Layout == "сетка номеров (не импортируется)" {
			gridSheets++
		}
	}
	eqInt(t, "листов-сеток", gridSheets, 4)

	// Каждая импортированная строка обязана иметь наименование:
	// item_id NOT NULL, строка без наименования в базу не пройдёт.
	for _, r := range res.Rows {
		if r.ItemName == "" {
			t.Fatalf("строка %s:%d без наименования прошла в импорт", r.SourceSheet, r.SourceRow)
		}
		if r.Qty.String() != "1" {
			t.Fatalf("строка %s:%d имеет количество %s — реестр всегда даёт по одному предмету",
				r.SourceSheet, r.SourceRow, r.Qty)
		}
	}
}

// TestRegistryLayoutsDetected — три раскладки распознаются каждая своей.
//
// ⚠ Если A/C/F-листы прочитать как B/D/G, они дадут строки с пустым
// наименованием и ТИХО выпадут из инвентаризации целиком.
func TestRegistryLayoutsDetected(t *testing.T) {
	res := mustParse(t, file(t, "инвентаризация"), FormatRegistry)

	byName := map[string]SheetReport{}
	for _, sh := range res.Sheets {
		byName[sh.Sheet] = sh
	}

	cases := []struct{ sheet, wantLayout string }{
		{"инв.ном.общ", "hdr:B/D/G"},
		{"инв.ном.меб.б.у.СД 2", "hdr:B/D/G"},
		{"абонотдел(типография)", "auto:B/D/G"},
		{"малоценка(инструменты) СМУ", "auto:A/C/F"},
		{"отдел труда и зп", "auto:A/C/F"},
		{"Лист1", "сетка номеров (не импортируется)"},
		{"Лист2", "сетка номеров (не импортируется)"},
		{"Лист7", "сетка номеров (не импортируется)"},
		{"Лист8", "сетка номеров (не импортируется)"},
	}
	for _, c := range cases {
		sh, ok := byName[c.sheet]
		if !ok {
			t.Errorf("лист %q не разобран вовсе", c.sheet)
			continue
		}
		if sh.Layout != c.wantLayout {
			t.Errorf("лист %q: раскладка %q, ожидалась %q", c.sheet, sh.Layout, c.wantLayout)
		}
	}

	// «малоценка СМУ» — 27 строк. В плане стоит 225: это max_row из служебной
	// записи Excel, реальных непустых строк там 27.
	if sh := byName["малоценка(инструменты) СМУ"]; sh.Rows != 27 {
		t.Errorf("«малоценка СМУ»: строк %d, ожидалось 27", sh.Rows)
	}
}

// TestRegistrySplitsDoNotAddQuantity фиксирует главное свойство реестра:
// он расщепляет уже учтённый агрегат, а не добавляет остаток.
func TestRegistrySplitsDoNotAddQuantity(t *testing.T) {
	res := mustParse(t, file(t, "инвентаризация"), FormatRegistry)
	for _, r := range res.Rows {
		if !r.Qty.Equal(r.Qty.Truncate(0)) || r.Qty.String() != "1" {
			t.Fatalf("реестр вернул количество %s — он обязан давать ровно 1 на предмет", r.Qty)
		}
	}
}

// TestParseRejectsUnknownFormat — формат приходит из формы загрузки и должен
// проверяться: счёт 10, прочитанный как МЦ.04, тихо задвоил бы остатки.
func TestParseRejectsUnknownFormat(t *testing.T) {
	if _, err := Parse("любой.xlsx", Format("нет такого")); err == nil {
		t.Error("Parse обязан отвергать неизвестный формат")
	}
	for _, f := range []Format{FormatMC04, FormatAccount10, FormatManual, FormatRegistry} {
		if !ValidFormat(string(f)) {
			t.Errorf("ValidFormat(%q) = false", f)
		}
	}
	if ValidFormat("materials") {
		t.Error("ValidFormat пропустил несуществующий формат")
	}
}
