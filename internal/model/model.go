// Package model содержит доменные структуры сервиса складского учёта.
//
// Соответствие таблицам — migrations/00001_init_warehouse_schema.sql.
// Количества везде decimal.Decimal (колонки NUMERIC(14,3)): float здесь
// недопустим, «1 234,56» на нём тихо теряет точность.
package model

import (
	"time"

	"github.com/shopspring/decimal"
)

// Реплики монолита. FK на них НЕ ставим: строка может приехать позже события,
// которое на неё ссылается.

type User struct {
	ID         int64      `json:"id"`
	Login      string     `json:"login"`
	Lastname   string     `json:"lastname"`
	Firstname  string     `json:"firstname"`
	Patronymic *string    `json:"patronymic"`
	DeletedAt  *time.Time `json:"deleted_at"`
	SyncedAt   time.Time  `json:"-"`
}

// FIO возвращает «Фамилия Имя Отчество» — форма, по которой идёт матчинг выгрузок.
func (u User) FIO() string {
	s := u.Lastname + " " + u.Firstname
	if u.Patronymic != nil && *u.Patronymic != "" {
		s += " " + *u.Patronymic
	}
	return s
}

type Department struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	ParentID  *int64     `json:"parent_id"`
	DeletedAt *time.Time `json:"deleted_at"`
	SyncedAt  time.Time  `json:"-"`
}

// Собственный домен

type Warehouse struct {
	ID                int64
	Name              string
	DepartmentID      *int64
	DepartmentName    *string
	ResponsibleUserID *int64
	Location          *string
	IsActive          bool
	Note              *string
	CreatedAt         time.Time
	UpdatedAt         time.Time

	// Заполняются только в выборках со связями.
	ResponsibleUser *User
}

// Area — область ответственности позиции каталога.
type Area string

const (
	AreaIT          Area = "it"
	AreaProcurement Area = "procurement"
	AreaFacility    Area = "facility"
)

func ValidArea(s string) bool {
	switch Area(s) {
	case AreaIT, AreaProcurement, AreaFacility:
		return true
	}
	return false
}

type Item struct {
	ID   int64
	Name string
	// NameNorm — служебное: lower + схлопнутые пробелы + NBSP→пробел.
	// Под UNIQUE идёт оно: 874 позиции из 881 встречаются в двух и более
	// выгрузках и без нормализации задвоились бы.
	NameNorm   string
	Area       *string
	Unit       *string
	Attributes map[string]any
	Note       *string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// HolderType — «чьё». Отвечает за форму строки закрепления.
type HolderType string

const (
	HolderPerson     HolderType = "person"
	HolderDepartment HolderType = "department"
	HolderWarehouse  HolderType = "warehouse"
	// HolderUnknown — предмет найден, держатель не выяснен. Не выдумка:
	// в реестре есть строки с номером и наименованием, но пустыми
	// «Отдел» и «Ответственное лицо».
	HolderUnknown HolderType = "unknown"
)

func ValidHolderType(s string) bool {
	switch HolderType(s) {
	case HolderPerson, HolderDepartment, HolderWarehouse, HolderUnknown:
		return true
	}
	return false
}

// Outcome — что увидели при пересчёте.
type Outcome string

const (
	OutcomeFound    Outcome = "found"
	OutcomeMissing  Outcome = "missing"
	OutcomeBroken   Outcome = "broken"
	OutcomeInRepair Outcome = "in_repair"
	OutcomeSurplus  Outcome = "surplus"
)

func ValidOutcome(s string) bool {
	switch Outcome(s) {
	case OutcomeFound, OutcomeMissing, OutcomeBroken, OutcomeInRepair, OutcomeSurplus:
		return true
	}
	return false
}

// Resolution — что с этим сделала бухгалтерия.
type Resolution string

const (
	ResolutionWrittenOff Resolution = "written_off"
	ResolutionFoundLater Resolution = "found_later"
	ResolutionExplained  Resolution = "explained"
	ResolutionPending    Resolution = "pending"
)

func ValidResolution(s string) bool {
	switch Resolution(s) {
	case ResolutionWrittenOff, ResolutionFoundLater, ResolutionExplained, ResolutionPending:
		return true
	}
	return false
}

// Holding — закрепление: у держателя числится qty_expected, найдено qty_actual.
// Строка с заполненным InvNumber — карточка конкретной единицы техники:
// qty строго 0 или 1, есть сетевые поля, к ней цепляются доступы.
type Holding struct {
	ID     int64
	ItemID int64

	HolderType     HolderType
	UserID         *int64
	UserFIO        *string
	DepartmentID   *int64
	DepartmentName *string
	WarehouseID    *int64
	Room           *string

	InvNumber *string
	// InvNumberNorm — все пробелы убраны, верхний регистр.
	// ⚠ Заглушки («б/н», «нет», «-») обязаны становиться NULL, а не нормализоваться:
	// иначе уникальный индекс пропустит одну из них и отобьёт остальные.
	InvNumberNorm *string
	SerialNumber  *string

	Hostname   *string
	IPAddress  *string
	MACAddress *string
	Specs      map[string]any

	QtyExpected decimal.Decimal
	// QtyActual: NULL = ЕЩЁ НЕ СЧИТАЛИ. Это не то же самое, что 0 («искали, не нашли»).
	QtyActual   *decimal.Decimal
	Outcome     *string
	CountedAt   *time.Time
	CountedByID *int64
	Note        *string

	Resolution    *string
	ResolutionRef *string
	ResolvedAt    *time.Time
	ResolvedByID  *int64

	SourceFile string
	// SourceSheet обязателен: в реестре на 40 листах строка 1 есть почти везде,
	// и пары файл+строка недостаточно для прослеживаемости.
	SourceSheet *string
	SourceRow   *int32
	CreatedAt   time.Time
	UpdatedAt   time.Time

	// Заполняются только в выборках со связями.
	Item          *Item
	User          *User
	Department    *Department
	Warehouse     *Warehouse
	CredentialCnt int
}

// CredentialKind — вид доступа к технике.
type CredentialKind string

const (
	CredAdmin CredentialKind = "admin"
	CredSSH   CredentialKind = "ssh"
	CredWiFi  CredentialKind = "wifi"
	CredRDP   CredentialKind = "rdp"
	CredOther CredentialKind = "other"
)

func ValidCredentialKind(s string) bool {
	switch CredentialKind(s) {
	case CredAdmin, CredSSH, CredWiFi, CredRDP, CredOther:
		return true
	}
	return false
}

// Credential — доступ к железке. Привязан к оборудованию, а не к человеку:
// пароль от роутера — свойство роутера, перевод сотрудника его не трогает.
type Credential struct {
	ID        int64
	HoldingID int64
	Kind      string
	Title     string
	URL       *string
	Login     *string
	// SecretCipher — конверт 'v{key_version}:' || base64(nonce ‖ tag ‖ cipher).
	// NULL означает «секрет затёрт» (см. WipedAt): строка живёт ради журнала.
	// ⚠ Наружу это поле не отдаётся НИКОГДА, кроме ответа на /reveal.
	SecretCipher *string
	KeyVersion   int16
	RotatedAt    *time.Time
	WipedAt      *time.Time
	Note         *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type CredentialViewLog struct {
	ID           int64
	CredentialID int64
	UserID       int64
	ViewedAt     time.Time
	IPAddress    *string
}

// Строки представлений (только чтение)

// LossRow — строка сличительной ведомости (wh_v_losses).
type LossRow struct {
	ItemID            int64
	Name              string
	Area              *string
	HoldingID         int64
	HolderType        string
	HolderPerson      *string
	HolderDepartment  *string
	Warehouse         *string
	ResponsiblePerson string
	ResponsibleUserID *int64
	Room              *string
	InvNumber         *string
	SerialNumber      *string
	QtyExpected       decimal.Decimal
	QtyActual         *decimal.Decimal
	QtyDiff           *decimal.Decimal
	DiscrepancyKind   string
	Outcome           *string
	Resolution        *string
	ResolutionRef     *string
	Note              *string
}

// ItemTotalRow — сводка по позиции (wh_v_item_totals).
type ItemTotalRow struct {
	ItemID              int64
	Name                string
	Area                *string
	Unit                *string
	PersonExpected      *decimal.Decimal
	DepartmentExpected  *decimal.Decimal
	UnallocatedExpected *decimal.Decimal
	TotalExpected       *decimal.Decimal
	TotalActual         *decimal.Decimal
	NotCountedRows      int64
}

// CredentialToRotateRow — что менять после увольнения (wh_v_credentials_to_rotate).
type CredentialToRotateRow struct {
	UserID       int64
	CredentialID int64
	Kind         string
	Title        string
	DeviceName   string
	InvNumber    *string
	Warehouse    *string
	LastSeenAt   time.Time
	RotatedAt    *time.Time
}
