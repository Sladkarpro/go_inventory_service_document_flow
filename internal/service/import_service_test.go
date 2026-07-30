package service

import (
	"testing"

	"github.com/shopspring/decimal"

	"go_warehouse_service/internal/model"
	"go_warehouse_service/internal/parser"
)

func d(s string) decimal.Decimal {
	v, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return v
}

func total(rows []parser.Row) string {
	sum := decimal.Zero
	for _, r := range rows {
		sum = sum.Add(r.Qty)
	}
	return sum.String()
}

// TestAggregateRowsSumsRepeatedHolderItemPairs — регрессия на ошибку, которая
// молча теряла 14 единиц из 3912.
//
// В МЦ.04 одна и та же номенклатура перечислена у человека несколькими строками
// (у директора «Стул на раме ISO BLACK» — шестью). В базе такие строки
// схлопывает частичный индекс uq_wh_holding_natural, и upsert, ЗАПИСЫВАЮЩИЙ
// количество вместо сложения, оставлял в базе только последнюю строку.
// Ни в логе, ни на экране это не видно.
func TestAggregateRowsSumsRepeatedHolderItemPairs(t *testing.T) {
	rows := []parser.Row{
		{ItemName: "Стул на раме ISO BLACK", HolderType: model.HolderPerson,
			UserFIO: "Ширяев Игорь Вадимович", Qty: d("1")},
		{ItemName: "Стул на раме ISO BLACK", HolderType: model.HolderPerson,
			UserFIO: "Ширяев Игорь Вадимович", Qty: d("1")},
		{ItemName: "Стул на раме ISO BLACK", HolderType: model.HolderPerson,
			UserFIO: "Ширяев Игорь Вадимович", Qty: d("4")},
	}

	got := aggregateRows(rows)
	if len(got) != 1 {
		t.Fatalf("строк после агрегации %d, ожидалась 1", len(got))
	}
	if got[0].Qty.String() != "6" {
		t.Errorf("количество = %s, ожидалось 6", got[0].Qty)
	}
	if total(rows) != total(got) {
		t.Errorf("агрегация изменила общую сумму: было %s, стало %s", total(rows), total(got))
	}
}

// TestAggregateRowsNormalizesKeys — разное написание одного держателя
// и одного наименования обязано схлопнуться, иначе строки разъедутся
// по разным записям и одна из них перезапишет другую в базе.
func TestAggregateRowsNormalizesKeys(t *testing.T) {
	rows := []parser.Row{
		{ItemName: "Монитор RDW 2401К", HolderType: model.HolderPerson,
			UserFIO: "Иванов Иван Иванович", Qty: d("2")},
		{ItemName: "монитор  rdw 2401К", HolderType: model.HolderPerson,
			UserFIO: "иванов  иван иванович", Qty: d("3")},
	}
	got := aggregateRows(rows)
	if len(got) != 1 || got[0].Qty.String() != "5" {
		t.Fatalf("получено %d строк с количеством %v, ожидалась 1 строка с 5",
			len(got), func() string {
				if len(got) > 0 {
					return got[0].Qty.String()
				}
				return "-"
			}())
	}
}

// TestAggregateRowsKeepsDifferentHoldersApart — складывать можно только
// одинаковых держателей. Иначе имущество одного человека уехало бы к другому.
func TestAggregateRowsKeepsDifferentHoldersApart(t *testing.T) {
	rows := []parser.Row{
		{ItemName: "Кресло", HolderType: model.HolderPerson, UserFIO: "Иванов И.И.", Qty: d("1")},
		{ItemName: "Кресло", HolderType: model.HolderPerson, UserFIO: "Петров П.П.", Qty: d("1")},
		{ItemName: "Кресло", HolderType: model.HolderWarehouse, WarehouseName: "Склад ИТ", Qty: d("1")},
		{ItemName: "Кресло", HolderType: model.HolderDepartment, DepartmentName: "Бухгалтерия", Qty: d("1")},
		// Один склад, но разные кабинеты — тоже разные строки: кабинет входит
		// в естественный ключ, потому что при обходе считают по кабинетам.
		{ItemName: "Кресло", HolderType: model.HolderWarehouse, WarehouseName: "Склад ИТ",
			Room: "Каб. 5", Qty: d("1")},
	}
	got := aggregateRows(rows)
	if len(got) != 5 {
		t.Errorf("строк после агрегации %d, ожидалось 5 — разные держатели слились", len(got))
	}
	if total(rows) != total(got) {
		t.Errorf("сумма изменилась: было %s, стало %s", total(rows), total(got))
	}
}

// TestAggregateRowsNumberedItemsNeverSumQuantity — количество номерной строки
// НИКОГДА не складывается: инвентарный номер именует РОВНО ОДИН предмет,
// и «инв. ДСК0001 в количестве 3» — бессмыслица (это же запрещает
// констрейнт chk_wh_holding_inv_qty).
//
// Но сами строки с одинаковым номером СХЛОПЫВАЮТСЯ, и это не то же самое:
// в реестре один предмет описан дважды — в общем листе и в листе своего
// отдела (2174 строки на 1085 номеров). Это устройство файла, а не ошибка
// данных. Если не схлопнуть, расщепление агрегата выполнится по разу
// на каждое упоминание, и остаток уедет вниз: проверено, 3912 → 3505.
func TestAggregateRowsNumberedItemsNeverSumQuantity(t *testing.T) {
	rows := []parser.Row{
		{ItemName: "Кресло", HolderType: model.HolderPerson, UserFIO: "Иванов И.И.",
			InvNumber: "ДСК 0001", Qty: d("1")},
		{ItemName: "Кресло", HolderType: model.HolderPerson, UserFIO: "Иванов И.И.",
			InvNumber: "ДСК 0002", Qty: d("1")},
		// Тот же предмет со второго листа — разное написание номера,
		// плюс серийник, которого не было в первой строке.
		{ItemName: "Кресло", HolderType: model.HolderPerson, UserFIO: "Иванов И.И.",
			InvNumber: "ДСК0001", SerialNumber: "SN-42", Qty: d("1")},
	}
	got := aggregateRows(rows)

	if len(got) != 2 {
		t.Fatalf("номерных строк после агрегации %d, ожидалось 2 "+
			"(ДСК 0001 и ДСК0001 — один предмет)", len(got))
	}
	for _, r := range got {
		if r.Qty.String() != "1" {
			t.Errorf("у номерной строки %q количество %s, ожидалась 1", r.InvNumber, r.Qty)
		}
	}
	// Данные из дубликата обязаны подхватиться, а не потеряться.
	if got[0].SerialNumber != "SN-42" {
		t.Errorf("серийный номер из дубликата потерян: %q", got[0].SerialNumber)
	}
}

// TestFillEmptyDoesNotOverwrite — уточняем только пустое. Заполненное значение
// из первой строки важнее: перезапись данными более позднего листа молча
// меняла бы держателя предмета.
func TestFillEmptyDoesNotOverwrite(t *testing.T) {
	dst := parser.Row{
		HolderType: model.HolderPerson, UserFIO: "Иванов И.И.",
		SerialNumber: "SN-1", Room: "Каб. 5",
	}
	fillEmpty(&dst, parser.Row{
		HolderType: model.HolderDepartment, DepartmentName: "Бухгалтерия",
		SerialNumber: "SN-2", Room: "Каб. 9", Note: "из второго листа",
	})
	if dst.SerialNumber != "SN-1" || dst.Room != "Каб. 5" {
		t.Error("заполненные поля были перезаписаны")
	}
	if dst.HolderType != model.HolderPerson || dst.UserFIO != "Иванов И.И." {
		t.Error("держатель подменён данными дубликата")
	}
	if dst.Note != "из второго листа" {
		t.Error("пустое поле не заполнилось из дубликата")
	}
}

func TestAggregateRowsEmptyInput(t *testing.T) {
	if got := aggregateRows(nil); len(got) != 0 {
		t.Errorf("пустой вход дал %d строк", len(got))
	}
}

// TestAggregateRowsSumsFactToo — если факт уже проставлен в файле (ручная
// ведомость), он обязан складываться вместе с учётным количеством,
// иначе строка получит расхождение на ровном месте.
func TestAggregateRowsSumsFactToo(t *testing.T) {
	two, three := d("2"), d("3")
	rows := []parser.Row{
		{ItemName: "Картридж", HolderType: model.HolderWarehouse, WarehouseName: "Склад ИТ",
			Qty: d("4"), QtyActual: &two},
		{ItemName: "Картридж", HolderType: model.HolderWarehouse, WarehouseName: "Склад ИТ",
			Qty: d("6"), QtyActual: &three},
	}
	got := aggregateRows(rows)
	if len(got) != 1 {
		t.Fatalf("строк %d, ожидалась 1", len(got))
	}
	if got[0].Qty.String() != "10" {
		t.Errorf("учёт = %s, ожидалось 10", got[0].Qty)
	}
	if got[0].QtyActual == nil || got[0].QtyActual.String() != "5" {
		t.Errorf("факт = %v, ожидалось 5", got[0].QtyActual)
	}
}
