package dto

// Запросы. Указатель означает «поле прислано»; отсутствие поля — «не трогать».
// Это важно для PATCH: разница между «не прислали room» и «прислали пустой room»
// (то есть очистить) в JSON выражается только так.

type UpdateItemRequest struct {
	Area       *string        `json:"area"`
	Unit       *string        `json:"unit"`
	Attributes map[string]any `json:"attributes"`
	Note       *string        `json:"note"`
}

type CreateHoldingRequest struct {
	ItemID     int64  `json:"itemId"`
	ItemName   string `json:"itemName"`
	HolderType string `json:"holderType" validate:"required"`

	UserID         *int64  `json:"userId"`
	UserFIO        *string `json:"userFio"`
	DepartmentID   *int64  `json:"departmentId"`
	DepartmentName *string `json:"departmentName"`
	WarehouseID    *int64  `json:"warehouseId"`
	Room           *string `json:"room"`

	InvNumber    *string `json:"invNumber"`
	SerialNumber *string `json:"serialNumber"`

	// Количества строками — см. комментарий к пакету.
	QtyExpected string  `json:"qtyExpected"`
	QtyActual   *string `json:"qtyActual"`
	Outcome     *string `json:"outcome"`
	Note        *string `json:"note"`
}

type UpdateHoldingRequest struct {
	// QtyActual: строка «12.5» — проставить факт; null или отсутствие поля — не менять.
	// ClearQtyActual сбрасывает факт в «ещё не считали» и имеет приоритет над QtyActual.
	QtyActual      *string `json:"qtyActual"`
	ClearQtyActual bool    `json:"clearQtyActual"`

	Outcome *string `json:"outcome"`
	Room    *string `json:"room"`
	Note    *string `json:"note"`

	HolderType     *string `json:"holderType"`
	UserID         *int64  `json:"userId"`
	UserFIO        *string `json:"userFio"`
	DepartmentID   *int64  `json:"departmentId"`
	DepartmentName *string `json:"departmentName"`
	WarehouseID    *int64  `json:"warehouseId"`

	InvNumber    *string `json:"invNumber"`
	SerialNumber *string `json:"serialNumber"`

	Hostname   *string        `json:"hostname"`
	IPAddress  *string        `json:"ipAddress"`
	MACAddress *string        `json:"macAddress"`
	Specs      map[string]any `json:"specs"`

	Resolution    *string `json:"resolution"`
	ResolutionRef *string `json:"resolutionRef"`
}

type WarehouseRequest struct {
	Name              string  `json:"name"`
	DepartmentID      *int64  `json:"departmentId"`
	DepartmentName    *string `json:"departmentName"`
	ResponsibleUserID *int64  `json:"responsibleUserId"`
	Location          *string `json:"location"`
	IsActive          *bool   `json:"isActive"`
	Note              *string `json:"note"`
}

type CreateCredentialRequest struct {
	HoldingID int64   `json:"holdingId" validate:"required"`
	Kind      string  `json:"kind" validate:"required"`
	Title     string  `json:"title" validate:"required"`
	URL       *string `json:"url"`
	Login     *string `json:"login"`
	// Secret приходит открытым текстом по HTTPS и немедленно шифруется.
	// В логи не попадает никогда.
	Secret string  `json:"secret" validate:"required"`
	Note   *string `json:"note"`
}

type UpdateCredentialRequest struct {
	Kind  *string `json:"kind"`
	Title *string `json:"title"`
	URL   *string `json:"url"`
	Login *string `json:"login"`
	Note  *string `json:"note"`
	// Непустой Secret = смена пароля, она же ротация.
	Secret *string `json:"secret"`
}
