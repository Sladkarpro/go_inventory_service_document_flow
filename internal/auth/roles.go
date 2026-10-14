// Package auth разворачивает иерархию ролей Symfony на стороне Go.
//
// ⚠ ЗАЧЕМ ЭТОТ ФАЙЛ ВООБЩЕ СУЩЕСТВУЕТ.
// Symfony кладёт в JWT то, что вернул User::getRoles() — только НАПРЯМУЮ назначенные
// роли из pivot-таблицы. Иерархия из config/packages/security.yaml применяется
// в security-слое монолита на каждом запросе и в токен НЕ разворачивается.
//
// Практическое следствие: у администратора портала в токене лежит ровно
// roles: ["ROLE_ADMIN"], и наивная проверка hasRole("ROLE_INVENTORY_ADMIN")
// ОТКАЖЕТ администратору. Поэтому иерархию разворачиваем здесь.
//
// ⚠ ЭТО ВТОРОЙ ИСТОЧНИК ИСТИНЫ. Он разъедется с security.yaml при первой же правке
// ролей в монолите. Смягчение — таблица ниже скопирована дословно, тест
// roles_test.go фиксирует каждое ребро, а здесь стоит ссылка на оригинал:
//
//	symfony_documents_flow/config/packages/security.yaml  (блок role_hierarchy)
//	symfony_documents_flow/src/Enum/User/UserRole.php     (канонический список)
//
// После правки ролей в монолите — править и здесь, тест подскажет, что именно.
package auth

const (
	RoleAdmin    = "ROLE_ADMIN"
	RoleAnalytic = "ROLE_ANALYTIC"
	RoleUser     = "ROLE_USER"

	// Прежние прямые роли сохраняются для совместимости, вне иерархии Symfony.
	RoleInventoryAdmin      = "ROLE_INVENTORY_ADMIN"
	RoleInventoryManager    = "ROLE_INVENTORY_MANAGER"
	RoleInventoryMol        = "ROLE_INVENTORY_MOL"
	RoleInventoryViewer     = "ROLE_INVENTORY_VIEWER"
	RoleInventoryDeptViewer = "ROLE_INVENTORY_DEPT_VIEWER"
)

// hierarchy — дословная копия role_hierarchy из security.yaml.
// Слева роль, справа то, что она получает В ПРИДАЧУ (один уровень;
// транзитивность считает Expand).
var hierarchy = map[string][]string{
	// Администратор портала получает всё, включая администратора инвентаризации.
	RoleAdmin: {
		RoleAnalytic, "ROLE_MANAGER", "ROLE_CITIZEN_APPEAL", "ROLE_HR",
		"ROLE_CONTRACT_APPLICATION", "ROLE_TKO", "ROLE_FINANCE",
		"ROLE_CLIENTS_DEPARTMENT", "ROLE_MECHANIC", RoleInventoryAdmin, "ROLE_DOC_OFFICE", RoleUser,
	},
	// ⚠ Аналитик — все прикладные роли портала, но НИ ОДНОЙ инвентарной.
	// Так задумано: в модуле пароли устройств и матценности, доступ выдаётся явно.
	RoleAnalytic: {
		"ROLE_MANAGER", "ROLE_CITIZEN_APPEAL", "ROLE_HR", "ROLE_CONTRACT_APPLICATION",
		"ROLE_TKO", "ROLE_FINANCE", "ROLE_CLIENTS_DEPARTMENT", "ROLE_MECHANIC", RoleUser,
	},

	RoleInventoryAdmin:   {RoleInventoryManager, RoleUser},
	RoleInventoryManager: {RoleUser},
	"ROLE_DOC_OFFICE":    {RoleUser},

	"ROLE_MANAGER":                  {RoleUser},
	"ROLE_CITIZEN_APPEAL":           {"ROLE_CITIZEN_APPEAL_VOICEMAIL", RoleUser},
	"ROLE_CITIZEN_APPEAL_VOICEMAIL": {RoleUser},
	"ROLE_HR":                       {RoleUser},
	"ROLE_CONTRACT_APPLICATION":     {RoleUser},
	"ROLE_TKO":                      {RoleUser},
	"ROLE_FINANCE":                  {RoleUser},
	"ROLE_CLIENTS_DEPARTMENT":       {RoleUser},
	"ROLE_MECHANIC":                 {RoleUser},
}

// Expand возвращает транзитивное замыкание набора ролей.
// Один уровень был бы недостаточен: ROLE_ADMIN → ROLE_INVENTORY_ADMIN →
// ROLE_INVENTORY_MANAGER. Цикл в таблице обход не подвешивает:
// посещённые роли не повторяются.
func Expand(roles []string) map[string]bool {
	out := make(map[string]bool, len(roles)*2)
	stack := make([]string, 0, len(roles))
	stack = append(stack, roles...)

	for len(stack) > 0 {
		role := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if role == "" || out[role] {
			continue
		}
		out[role] = true
		stack = append(stack, hierarchy[role]...)
	}
	return out
}

// Has сообщает, есть ли у пользователя роль с учётом иерархии.
func Has(roles []string, want string) bool {
	return Expand(roles)[want]
}

// HasAny сообщает, есть ли хотя бы одна из перечисленных ролей.
func HasAny(roles []string, want ...string) bool {
	granted := Expand(roles)
	for _, w := range want {
		if granted[w] {
			return true
		}
	}
	return false
}

// IsInventoryAdmin — право администрировать инвентаризацию.
// Только этой ролью разрешён просмотр паролей (reveal).
func IsInventoryAdmin(roles []string) bool {
	return Has(roles, RoleInventoryAdmin)
}

// CanWrite — право менять данные инвентаризации: администратор или МОЛ.
// Наблюдатели (VIEWER, DEPT_VIEWER) сюда не попадают — они смотрят, но не правят.
// ROLE_INVENTORY_MANAGER требует ограничений по организациям и категориям,
// как в Symfony; без них эта роль не даёт ни чтения, ни записи.
func CanWrite(roles []string) bool {
	return HasAny(roles, RoleInventoryAdmin, RoleInventoryMol)
}

// CanRead — право видеть данные инвентаризации.
//
// ⚠ RoleInventoryDeptViewer СЮДА НЕ ВХОДИТ, и это не забывчивость.
//
// По прежнему комментарию в security.yaml эта роль означает «только своё
// подразделение и потомки». Такого ограничения в сервисе НЕТ НИ В ОДНОЙ РУЧКЕ:
// фильтра по подразделению не существует, user.ID из токена нигде не
// используется как скоуп. Замерено: с этой ролью читается инвентаризация
// всего предприятия.
//
// То есть роль, которая ЗВУЧИТ ограниченной, давала полный доступ — и выдали бы
// её именно потому, что она звучит ограниченной. Пока настоящий скоуп не
// реализован, роль не даёт ничего: ошибка «выдали начальнику отдела, думая, что
// он увидит только свой отдел» становится безвредной вместо тихой.
// Решение владельца 29.07.2026: не пускать в оборот до реализации скоупа.
//
// Вернуть роль сюда можно только вместе с фильтром по подразделению во всех
// читающих ручках и тестами на него.
func CanRead(roles []string) bool {
	return HasAny(roles, RoleInventoryAdmin, RoleInventoryMol, RoleInventoryViewer)
}
