package model

import "testing"

func TestNormalizeInvNumber(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		want  string
		wantK bool
	}{
		// Ради этого колонка *_norm и заведена: в реестре владельца три написания
		// одного номера дают три «уникальных» значения из-за пробелов.
		{"пробел внутри", "ДСК 0001", "ДСК0001", true},
		{"без пробела", "ДСК0001", "ДСК0001", true},
		{"двойной пробел", "ДСК  0004", "ДСК0004", true},
		{"неразрывный пробел", "ДСК 0004", "ДСК0004", true},
		{"края", "  ДСК 0004  ", "ДСК0004", true},
		{"нижний регистр", "дск 0001", "ДСК0001", true},
		{"БП с дефисом", "БП-000336", "БП-000336", true},

		// Заглушки обязаны стать NULL. Если бы они нормализовались в 'Б/Н',
		// частичный уникальный индекс пропустил бы одну из 23 строк реестра.
		{"заглушка б/н", "б/н", "", false},
		{"заглушка б/н пробелы", " Б / Н ", "", false},
		{"заглушка обратный слеш", "б\\н", "", false},
		{"заглушка нет", "нет", "", false},
		{"заглушка не присвоен", "не присвоен", "", false},
		{"заглушка тире", "-", "", false},
		{"заглушка длинное тире", "—", "", false},
		{"пустая строка", "", "", false},
		{"только пробелы", "   ", "", false},
		{"ноль", "0", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := NormalizeInvNumber(tt.in)
			if got != tt.want || ok != tt.wantK {
				t.Errorf("NormalizeInvNumber(%q) = (%q, %v), want (%q, %v)",
					tt.in, got, ok, tt.want, tt.wantK)
			}
		})
	}
}

func TestNormalizeInvNumberCollapsesRegistryVariants(t *testing.T) {
	// Все три написания обязаны схлопнуться в один ключ, иначе один стул
	// попадёт в инвентаризацию трижды.
	variants := []string{"ДСК 0004", "ДСК0004", "ДСК  0004", "дск0004", "ДСК 0004"}
	first, ok := NormalizeInvNumber(variants[0])
	if !ok {
		t.Fatalf("первый вариант неожиданно признан заглушкой")
	}
	for _, v := range variants[1:] {
		got, ok := NormalizeInvNumber(v)
		if !ok || got != first {
			t.Errorf("NormalizeInvNumber(%q) = (%q, %v), want (%q, true)", v, got, ok, first)
		}
	}
}

func TestNormalizeName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Кресло офисное CHOOSE LITE", "кресло офисное choose lite"},
		{"  Кресло   офисное  ", "кресло офисное"},
		{"Кресло офисное", "кресло офисное"},
		{"Шкаф для документов средний  с  глухими дверками",
			"шкаф для документов средний с глухими дверками"},
		{"Стул каркас черный\t\t", "стул каркас черный"},
	}
	for _, tt := range tests {
		if got := NormalizeName(tt.in); got != tt.want {
			t.Errorf("NormalizeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNormalizeNameCollapsesDuplicatesAcrossFiles(t *testing.T) {
	// 874 позиции из 881 встречаются в двух и более выгрузках; без схлопывания
	// каталог задвоился бы, а wh_v_item_totals сложил бы остатки одного предмета
	// как двух разных.
	a := NormalizeName("Монитор RDW 2401К, 23.8 Расп. 67-Р1")
	b := NormalizeName("монитор  rdw 2401К,  23.8 расп. 67-р1")
	if a != b {
		t.Errorf("варианты одного наименования не схлопнулись: %q != %q", a, b)
	}
}

func TestNullableText(t *testing.T) {
	if NullableText("   ") != nil {
		t.Error("пустая строка должна давать nil, иначе в базу уедет '' вместо NULL")
	}
	if got := NullableText("  Склад ИТ  "); got == nil || *got != "Склад ИТ" {
		t.Errorf("NullableText вернул %v", got)
	}
}

// TestPersonKeyMatchesAcrossSources — ради этого функция и написана.
// Реестр и 1С пишут одного человека по-разному; ключ обязан их свести.
func TestPersonKeyMatchesAcrossSources(t *testing.T) {
	pairs := []struct{ registry, osv string }{
		{"Бражкина Н.А. бухгалтерия", "Бражкина Наталья Анатольевна"},
		{"Глоговская М.Д. гл.бухг. бухгалтерия", "Глоговская Марина Дмитриевна"},
		{"Фаустов А.А. ", "Фаустов Артём Андреевич"},
		{"Ширяев И.В. Генеральный директор", "Ширяев Игорь Вадимович"},
		{"Изотова М. А.\tэкономист по труду", "Изотова Мария Андреевна"},
		{"Колесниченко С.В. Зам.ген.дир по производству", "Колесниченко Сергей Викторович"},
	}
	for _, p := range pairs {
		a, b := PersonKey(p.registry), PersonKey(p.osv)
		if a == "" {
			t.Errorf("PersonKey(%q) пуст", p.registry)
			continue
		}
		if a != b {
			t.Errorf("не свелись: PersonKey(%q)=%q, PersonKey(%q)=%q",
				p.registry, a, p.osv, b)
		}
	}
}

// TestPersonKeyDoesNotConflateDifferentPeople — однофамильцы с разными
// инициалами обязаны остаться разными: иначе имущество уедет к другому человеку.
func TestPersonKeyDoesNotConflateDifferentPeople(t *testing.T) {
	if PersonKey("Иванов И.И.") == PersonKey("Иванов П.С.") {
		t.Error("однофамильцы с разными инициалами слились")
	}
	if PersonKey("Иванов И.И.") == PersonKey("Петров И.И.") {
		t.Error("разные фамилии с одинаковыми инициалами слились")
	}
}

// TestPersonKeyRefusesDepartments — подразделение человеком не является,
// и ключа у него быть не должно: пустой ключ никогда не считается совпадением.
func TestPersonKeyRefusesDepartments(t *testing.T) {
	for _, s := range []string{
		"", "   ", "Склад", "Каб.№5", "Абонентский отдел",
	} {
		if got := PersonKey(s); got != "" {
			t.Errorf("PersonKey(%q) = %q, ожидалась пустая строка", s, got)
		}
	}
}
