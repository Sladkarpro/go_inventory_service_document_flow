package auth

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestAdminGetsInventoryAdmin — САМЫЙ ВАЖНЫЙ ТЕСТ ФАЙЛА.
//
// В JWT у администратора портала лежит ровно ["ROLE_ADMIN"]: Symfony кладёт
// в токен сырой User::getRoles() и иерархию не разворачивает. Если бы сервис
// сравнивал строки напрямую, администратор получил бы 403 на собственном модуле.
func TestAdminGetsInventoryAdmin(t *testing.T) {
	jwtRoles := []string{RoleAdmin} // ровно то, что приезжает в токене

	if !IsInventoryAdmin(jwtRoles) {
		t.Fatal("ROLE_ADMIN обязан получать ROLE_INVENTORY_ADMIN через иерархию")
	}
	if !CanWrite(jwtRoles) {
		t.Error("ROLE_ADMIN обязан иметь право записи")
	}
	if !CanRead(jwtRoles) {
		t.Error("ROLE_ADMIN обязан иметь право чтения")
	}

	// Транзитивность: ROLE_ADMIN → ROLE_INVENTORY_ADMIN → {MOL, VIEWER, DEPT_VIEWER}.
	// Разворот на один уровень эту цепочку не прошёл бы.
	for _, want := range []string{RoleInventoryMol, RoleInventoryViewer, RoleInventoryDeptViewer, RoleUser} {
		if !Has(jwtRoles, want) {
			t.Errorf("ROLE_ADMIN обязан транзитивно получать %s", want)
		}
	}
}

// TestAnalyticGetsNoInventoryRoles фиксирует осознанное решение из security.yaml:
// аналитик получает все прикладные роли портала и НИ ОДНОЙ инвентарной —
// в модуле пароли устройств и матценности, доступ выдаётся явно через админку.
func TestAnalyticGetsNoInventoryRoles(t *testing.T) {
	roles := []string{RoleAnalytic}

	for _, forbidden := range []string{
		RoleInventoryAdmin, RoleInventoryMol, RoleInventoryViewer, RoleInventoryDeptViewer,
	} {
		if Has(roles, forbidden) {
			t.Errorf("ROLE_ANALYTIC не должен получать %s", forbidden)
		}
	}
	if CanRead(roles) || CanWrite(roles) {
		t.Error("ROLE_ANALYTIC не должен иметь доступа к инвентаризации")
	}
	// При этом прикладные роли портала он получает.
	if !Has(roles, "ROLE_MANAGER") || !Has(roles, RoleUser) {
		t.Error("ROLE_ANALYTIC обязан получать прикладные роли портала")
	}
}

// TestInventoryBranchesAreParallel фиксирует ловушку, на которой уже обожглись
// в монолите: МОЛ, наблюдатель и начальник отдела — ПАРАЛЛЕЛЬНЫЕ ветки.
// Проверка одной ролью на весь контроллер отрезала бы МОЛа.
func TestInventoryBranchesAreParallel(t *testing.T) {
	cases := []struct{ have, mustNotHave string }{
		{RoleInventoryMol, RoleInventoryViewer},
		{RoleInventoryMol, RoleInventoryDeptViewer},
		{RoleInventoryViewer, RoleInventoryMol},
		{RoleInventoryViewer, RoleInventoryDeptViewer},
		{RoleInventoryDeptViewer, RoleInventoryMol},
		{RoleInventoryDeptViewer, RoleInventoryViewer},
	}
	for _, c := range cases {
		if Has([]string{c.have}, c.mustNotHave) {
			t.Errorf("%s не должен наследовать %s — это параллельные ветки", c.have, c.mustNotHave)
		}
	}

	// Каждая из трёх ветвей обязана давать ROLE_USER: иерархия в монолите
	// это задаёт, и расхождение поймает TestHierarchyMatchesSecurityYaml.
	for _, r := range []string{RoleInventoryMol, RoleInventoryViewer, RoleInventoryDeptViewer} {
		if !Has([]string{r}, RoleUser) {
			t.Errorf("%s обязан давать ROLE_USER", r)
		}
	}

	// Право ЧТЕНИЯ дают только две из трёх.
	for _, r := range []string{RoleInventoryMol, RoleInventoryViewer} {
		if !CanRead([]string{r}) {
			t.Errorf("%s обязан давать право чтения", r)
		}
	}
}

// TestDeptViewerGrantsNothingUntilScopeExists фиксирует осознанное решение
// владельца от 29.07.2026, а не текущее положение дел.
//
// Роль ЗВУЧИТ как ограниченная («только своё подразделение» — комментарий
// в security.yaml монолита), но скоупа в сервисе нет ни в одной ручке: фильтра
// по подразделению не существует, user.ID из токена нигде не используется
// как ограничение. Замерено: с этой ролью читалась инвентаризация всего
// предприятия. Выдали бы её как раз потому, что она звучит ограниченной.
//
// Пока скоуп не реализован, роль не даёт НИЧЕГО — тогда ошибочная выдача
// безвредна, а не тиха. Тест обязан упасть, если кто-то вернёт роль в CanRead,
// не сделав фильтр: сообщение объясняет, что именно нужно сделать сначала.
func TestDeptViewerGrantsNothingUntilScopeExists(t *testing.T) {
	roles := []string{RoleInventoryDeptViewer}

	if CanRead(roles) {
		t.Error("ROLE_INVENTORY_DEPT_VIEWER снова даёт чтение. Если скоуп " +
			"по подразделению действительно реализован во всех читающих ручках " +
			"и покрыт тестами — обновите этот тест вместе с CanRead. Если нет, " +
			"роль отдаёт данные всего предприятия под видом ограниченной")
	}
	if CanWrite(roles) {
		t.Error("ROLE_INVENTORY_DEPT_VIEWER не должен давать право записи")
	}
	if IsInventoryAdmin(roles) {
		t.Error("ROLE_INVENTORY_DEPT_VIEWER не должен давать права администратора")
	}

	// При этом администратор инвентаризации по-прежнему получает её транзитивно:
	// иерархия монолита не меняется, меняется только смысл самой роли.
	if !Has([]string{RoleInventoryAdmin}, RoleInventoryDeptViewer) {
		t.Error("иерархия поехала: администратор обязан транзитивно получать роль")
	}
}

// TestViewersCannotWrite — наблюдатели видят всё, но не правят ничего.
func TestViewersCannotWrite(t *testing.T) {
	for _, r := range []string{RoleInventoryViewer, RoleInventoryDeptViewer} {
		if CanWrite([]string{r}) {
			t.Errorf("%s не должен иметь права записи", r)
		}
	}
	if !CanWrite([]string{RoleInventoryMol}) {
		t.Error("МОЛ обязан иметь право записи — он и проводит пересчёт")
	}
	if !CanWrite([]string{RoleInventoryAdmin}) {
		t.Error("администратор инвентаризации обязан иметь право записи")
	}
}

// TestPlainUserHasNothing — базовая роль не даёт доступа к инвентаризации.
func TestPlainUserHasNothing(t *testing.T) {
	roles := []string{RoleUser}
	if CanRead(roles) || CanWrite(roles) || IsInventoryAdmin(roles) {
		t.Error("ROLE_USER не должен иметь доступа к инвентаризации")
	}
}

// TestExpandTerminatesOnUnknownAndEmpty — обход не должен падать или зависать
// на ролях, которых нет в таблице (монолит завёл новую, сервис ещё не знает).
func TestExpandTerminatesOnUnknownAndEmpty(t *testing.T) {
	got := Expand([]string{"ROLE_SOMETHING_NEW", "", RoleUser})
	if !got["ROLE_SOMETHING_NEW"] {
		t.Error("неизвестная роль должна оставаться в наборе как есть")
	}
	if got[""] {
		t.Error("пустая строка не должна попадать в набор ролей")
	}
	if len(Expand(nil)) != 0 {
		t.Error("пустой вход должен давать пустой набор")
	}
}

// securityYamlRel — путь к security.yaml монолита от каталога ЭТОГО пакета.
// Монолит лежит рядом со сервисом: .../kanban/{go_warehouse_service,symfony_documents_flow}.
const securityYamlRel = "../../../symfony_documents_flow/config/packages/security.yaml"

// readRoleHierarchy достаёт блок role_hierarchy из security.yaml монолита.
//
// ⚠ Файла может не быть: сервис — отдельный репозиторий, и на CI монолит рядом
// не лежит. Тогда тест ПРОПУСКАЕТСЯ, а не падает: падение означало бы «сломан
// сервис», хотя сломано окружение.
//
// Значение справа бывает двух видов — список (`ROLE_ADMIN: [A, B]`) и скаляр
// (`ROLE_MANAGER: ROLE_USER`). Оба встречаются в этом самом файле, поэтому
// разбираем через any и различаем типом, а не предполагаем список.
func readRoleHierarchy(t *testing.T) map[string][]string {
	t.Helper()

	raw, err := os.ReadFile(filepath.FromSlash(securityYamlRel))
	if err != nil {
		t.Skipf("security.yaml монолита недоступен (%v) — сверка иерархии пропущена", err)
	}

	// Верхний уровень файла — ключи security: и when@test:. Нас интересует первый;
	// неизвестные ключи yaml.v3 молча игнорирует, поэтому when@test не мешает.
	var doc struct {
		Security struct {
			RoleHierarchy map[string]any `yaml:"role_hierarchy"`
		} `yaml:"security"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("security.yaml не разбирается как YAML: %v", err)
	}
	if len(doc.Security.RoleHierarchy) == 0 {
		t.Fatalf("в %s нет блока security.role_hierarchy — структура файла изменилась",
			securityYamlRel)
	}

	out := make(map[string][]string, len(doc.Security.RoleHierarchy))
	for role, v := range doc.Security.RoleHierarchy {
		switch x := v.(type) {
		case string:
			out[role] = []string{x}
		case []any:
			children := make([]string, 0, len(x))
			for _, c := range x {
				s, ok := c.(string)
				if !ok {
					t.Fatalf("%s: наследуемая роль не строка, а %T", role, c)
				}
				children = append(children, s)
			}
			out[role] = children
		default:
			t.Fatalf("%s: неожидаемый тип значения %T в role_hierarchy", role, v)
		}
	}
	return out
}

// TestHierarchyMatchesSecurityYaml сверяет карту hierarchy с НАСТОЯЩИМ файлом
// монолита, а не с копией, набранной рядом руками.
//
// ⚠ Зачем это переписано. Прошлая версия теста держала второй экземпляр той же
// карты в переменной expected и сравнивала Go с Go: файл монолита она не
// открывала (в импортах был только "testing"). Такой тест не мог упасть ни от
// одной правки в security.yaml, хотя его комментарий обещал ровно обратное —
// то есть он не детектировал расхождение, а маскировал его.
//
// Источник истины: symfony_documents_flow/config/packages/security.yaml,
// блок security.role_hierarchy. Иерархия в JWT НЕ приезжает (Symfony кладёт
// сырой User::getRoles()), поэтому сервис разворачивает её сам — и обязан
// разворачивать так же, как монолит.
func TestHierarchyMatchesSecurityYaml(t *testing.T) {
	want := readRoleHierarchy(t)

	// Ветка монолита без модуля инвентаризации — отдельный, гораздо более частый
	// случай, чем настоящее расхождение: у проекта несколько долгоживущих ветвей,
	// и инвентарные роли есть не во всех. Сообщение должно называть причину,
	// иначе диагноз уйдёт в «роли пропали».
	if _, ok := want[RoleInventoryAdmin]; !ok {
		t.Fatalf("в %s нет %s.\n"+
			"Скорее всего монолит выкачан на ветке без модуля инвентаризации — "+
			"проверьте `git -C ../symfony_documents_flow branch --show-current`.\n"+
			"Сервису нужна ветка, где инвентарные роли объявлены: без них "+
			"ROLE_INVENTORY_MOL не попадёт ни в один токен.",
			securityYamlRel, RoleInventoryAdmin)
	}

	if len(hierarchy) != len(want) {
		t.Errorf("число ролей разъехалось: в Go %d, в security.yaml %d",
			len(hierarchy), len(want))
	}

	for role, wantChildren := range want {
		gotChildren, ok := hierarchy[role]
		if !ok {
			t.Errorf("роль %s есть в security.yaml, но отсутствует в Go", role)
			continue
		}
		gotSet := make(map[string]bool, len(gotChildren))
		for _, c := range gotChildren {
			gotSet[c] = true
		}
		for _, w := range wantChildren {
			if !gotSet[w] {
				t.Errorf("%s: в security.yaml наследует %s, в Go — нет", role, w)
			}
		}
		wantSet := make(map[string]bool, len(wantChildren))
		for _, w := range wantChildren {
			wantSet[w] = true
		}
		for _, g := range gotChildren {
			if !wantSet[g] {
				t.Errorf("%s: в Go наследует %s, в security.yaml — нет", role, g)
			}
		}
	}

	for role := range hierarchy {
		if _, ok := want[role]; !ok {
			t.Errorf("роль %s есть в Go, но отсутствует в security.yaml", role)
		}
	}
}
