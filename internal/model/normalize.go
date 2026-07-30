package model

import (
	"strings"
	"unicode"
)

// ⚠ ЭТОТ ФАЙЛ — ЕДИНСТВЕННОЕ МЕСТО НОРМАЛИЗАЦИИ.
//
// И API, и все четыре импортёра обязаны звать эти функции. Если появится второй
// путь записи со своей нормализацией, он сохранит «ДСК 0001» с чужим norm,
// уникальный индекс uq_wh_holding_inv_number не сработает, и один предмет
// окажется в базе дважды. Ровно то, ради чего колонки *_norm заведены.

// invPlaceholders — заглушки инвентарного номера. Сравниваются С НОРМАЛИЗОВАННОЙ
// формой (пробелы убраны, верхний регистр).
//
// ⚠ Заглушки обязаны становиться NULL, а не нормализоваться. В реестре владельца
// 23 строки с буквальным «б/н» (Штамп, Штамп согласовано, Рутокен СБИС…).
// Если превратить их в 'Б/Н', частичный уникальный индекс пропустит ОДНУ из 23,
// а остальные 22 отобьёт — и законные строки не попадут в инвентаризацию.
var invPlaceholders = map[string]struct{}{
	"":            {},
	"Б/Н":         {},
	"Б\\Н":        {},
	"БН":          {},
	"НЕТ":         {},
	"НЕПРИСВОЕН":  {},
	"НЕПРИСВОЕНО": {},
	"-":           {},
	"—":           {},
	"–":           {},
	"0":           {},
	"ПУСТО":       {},
}

// Пробельные символы, которые приезжают из 1С и Excel и глазом не отличаются
// от обычного пробела. Заданы КОДАМИ намеренно: в исходнике они невидимы, и
// безобидная «починка отступов» превратила бы NBSP в обычный пробел, тихо
// сломав нормализацию — а вместе с ней уникальность инвентарных номеров.
const (
	nbsp       = rune(0x00A0) // NO-BREAK SPACE — самый частый гость из 1С
	narrowNbsp = rune(0x202F) // NARROW NO-BREAK SPACE
	figureSp   = rune(0x2007) // FIGURE SPACE
	thinSp     = rune(0x2009) // THIN SPACE
	bom        = rune(0xFEFF) // ZERO WIDTH NO-BREAK SPACE / BOM
)

// replaceSpaces приводит неразрывные и прочие «широкие» пробелы к обычному,
// а BOM выбрасывает совсем: он невидим и ширины не имеет.
func replaceSpaces(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case bom:
			return -1
		case nbsp, narrowNbsp, figureSp, thinSp:
			return ' '
		}
		if unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, s)
}

// NormalizeName приводит наименование к сравнимой форме:
// NBSP→пробел, схлопывание пробелов, обрезка, нижний регистр.
// Под UNIQUE в wh_item идёт именно она.
func NormalizeName(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(replaceSpaces(s)), " "))
}

// NormalizeInvNumber возвращает нормализованный инвентарный номер и признак,
// что номер осмысленный. Для заглушек ok=false — вызывающий обязан записать
// в inv_number и inv_number_norm оба NULL (констрейнт chk_wh_holding_inv_norm
// требует, чтобы они существовали вместе).
//
// Нормализация: убраны ВСЕ пробелы, верхний регистр. В реестре владельца
// «ДСК 0001», «ДСК0001» и «ДСК  0004» соседствуют — без этого один и тот же
// предмет проходит как несколько разных.
func NormalizeInvNumber(s string) (string, bool) {
	cleaned := strings.Map(func(r rune) rune {
		if r == ' ' {
			return -1
		}
		return r
	}, replaceSpaces(s))
	cleaned = strings.ToUpper(cleaned)

	if _, isPlaceholder := invPlaceholders[cleaned]; isPlaceholder {
		return "", false
	}
	return cleaned, true
}

// NormalizeFIO приводит ФИО к форме, по которой ищется реплика users.
// Должна совпадать с выражением индекса ix_users_fio:
//
//	LOWER(TRIM(lastname || ' ' || firstname || ' ' || COALESCE(patronymic, '')))
//
// ⚠ Расхождение здесь означает 1774 полных просмотра таблицы на импорте
// и ноль сматченных сотрудников.
func NormalizeFIO(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(replaceSpaces(s)), " "))
}

// CleanText схлопывает пробелы и обрезает края, сохраняя регистр.
// Для значений, которые едут в базу как есть (наименование, ФИО из выгрузки).
func CleanText(s string) string {
	return strings.Join(strings.Fields(replaceSpaces(s)), " ")
}

// NullableText возвращает nil для пустых строк — в базе такие поля NULL,
// а не пустая строка: частичные индексы и CHECK-и рассчитаны на NULL.
func NullableText(s string) *string {
	c := CleanText(s)
	if c == "" {
		return nil
	}
	return &c
}

// PersonKey извлекает из строки держателя «фамилия + инициалы» — ключ, по
// которому один и тот же человек узнаётся в РАЗНЫХ источниках.
//
// ⚠ БЕЗ ЭТОГО РАСЩЕПЛЕНИЕ РЕЕСТРА НЕ РАБОТАЕТ ВООБЩЕ. Проверено на данных
// владельца: из 132 держателей реестра с ФИО из 1С дословно совпадают СЕМЬ.
// Реестр пишет «Бражкина Н.А. бухгалтерия», МЦ.04 — «Бражкина Наталья
// Анатольевна». Сравнение строк целиком даёт 0 совпадений, и все 1085 номерных
// предметов уходят в излишки вместо того, чтобы уменьшить учтённый агрегат.
//
// Опознаём человека двумя способами, в порядке надёжности:
//  1. Явные инициалы («Н.А.», «Д. Р.») — фамилия стоит непосредственно перед ними.
//     Так разбирается и «Подразделение эксплуатации полигонов Коноплев И.А.»,
//     где человек спрятан в хвосте строки.
//  2. Классические три слова с заглавной буквы («Иванов Иван Иванович»).
//
// ⚠ Если ни то, ни другое не подошло — возвращается ПУСТАЯ строка, и она
// НИКОГДА не считается совпадением. «Абонентский отдел» человеком не является,
// и принять его за «Абонентский О.» означало бы перенести чужое имущество.
// Лучше не расщепить, чем расщепить неверно.
func PersonKey(s string) string {
	fields := strings.Fields(replaceSpaces(s))
	if len(fields) < 2 {
		return ""
	}

	upperLetters := func(w string) []rune {
		out := make([]rune, 0, len(w))
		for _, r := range w {
			if unicode.IsLetter(r) {
				out = append(out, unicode.ToUpper(r))
			}
		}
		return out
	}
	// «Н.А.», «Д.», «М.» — не больше двух букв и обязательная точка.
	isInitials := func(w string) bool {
		if !strings.Contains(w, ".") {
			return false
		}
		n := len(upperLetters(w))
		return n >= 1 && n <= 2
	}
	isCapitalizedName := func(w string) bool {
		r := []rune(w)
		if len(r) < 3 || !unicode.IsUpper(r[0]) {
			return false
		}
		for _, c := range r {
			if !unicode.IsLetter(c) && c != '-' {
				return false
			}
		}
		return true
	}

	build := func(surname string, initials []rune) string {
		if surname == "" || len(initials) == 0 {
			return ""
		}
		key := surname
		for _, r := range initials {
			key += "|" + string(r)
		}
		return key
	}

	// Способ 1: ищем явные инициалы, фамилия — слово перед ними.
	for i := 1; i < len(fields); i++ {
		if !isInitials(fields[i]) {
			continue
		}
		surname := string(upperLetters(fields[i-1]))
		if len(surname) < 2 || isInitials(fields[i-1]) {
			continue
		}
		initials := upperLetters(fields[i])
		// Инициалы могут быть разнесены: «М. А.»
		if len(initials) == 1 && i+1 < len(fields) && isInitials(fields[i+1]) {
			initials = append(initials, upperLetters(fields[i+1])...)
		}
		if len(initials) > 2 {
			initials = initials[:2]
		}
		return build(surname, initials)
	}

	// Способ 2: «Фамилия Имя Отчество» — все три слова с заглавной буквы.
	if len(fields) >= 3 &&
		isCapitalizedName(fields[0]) && isCapitalizedName(fields[1]) && isCapitalizedName(fields[2]) {
		return build(string(upperLetters(fields[0])),
			[]rune{upperLetters(fields[1])[0], upperLetters(fields[2])[0]})
	}
	return ""
}
