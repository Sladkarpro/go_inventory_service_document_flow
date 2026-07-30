// Package dto описывает форму HTTP-ответов.
//
// ⚠ КОЛИЧЕСТВА ОТДАЮТСЯ СТРОКАМИ. Колонки NUMERIC(14,3); если пустить их
// через float64, «1234.567» вернётся клиенту как 1234.5670000000001, а сложение
// таких значений в браузере даст расхождение в ведомости, которое никто
// не сможет объяснить.
//
// ⚠ qtyActual = null означает «ЕЩЁ НЕ СЧИТАЛИ» и это НЕ то же самое, что 0
// («искали, не нашли»). Поэтому у поля нет omitempty: null обязан доехать
// до клиента как null.
package dto

import (
	"time"

	"github.com/shopspring/decimal"

	"go_warehouse_service/internal/model"
	"go_warehouse_service/internal/repository"
)

// Pagination — единая обёртка списков: {<ресурс>: [...], pagination: {...}}.
type Pagination struct {
	Total  int64 `json:"total"`
	Limit  int   `json:"limit"`
	Offset int   `json:"offset"`
}

func NewPagination(total int64, limit, offset int) Pagination {
	return Pagination{Total: total, Limit: limit, Offset: offset}
}

// ---------------------------------------------------------------------------

type ItemResponse struct {
	ID         int64          `json:"id"`
	Name       string         `json:"name"`
	Area       *string        `json:"area"`
	Unit       *string        `json:"unit"`
	Attributes map[string]any `json:"attributes"`
	Note       *string        `json:"note"`
}

func MapItem(i *model.Item) ItemResponse {
	attrs := i.Attributes
	if attrs == nil {
		attrs = map[string]any{}
	}
	return ItemResponse{
		ID: i.ID, Name: i.Name, Area: i.Area, Unit: i.Unit,
		Attributes: attrs, Note: i.Note,
	}
}

func MapItems(items []model.Item) []ItemResponse {
	out := make([]ItemResponse, 0, len(items))
	for i := range items {
		out = append(out, MapItem(&items[i]))
	}
	return out
}

// ---------------------------------------------------------------------------

type HoldingResponse struct {
	ID       int64  `json:"id"`
	ItemID   int64  `json:"itemId"`
	ItemName string `json:"itemName"`

	HolderType     string  `json:"holderType"`
	UserID         *int64  `json:"userId"`
	UserFIO        *string `json:"userFio"`
	DepartmentID   *int64  `json:"departmentId"`
	DepartmentName *string `json:"departmentName"`
	WarehouseID    *int64  `json:"warehouseId"`
	WarehouseName  *string `json:"warehouseName"`
	Room           *string `json:"room"`

	InvNumber    *string `json:"invNumber"`
	SerialNumber *string `json:"serialNumber"`

	Hostname   *string        `json:"hostname"`
	IPAddress  *string        `json:"ipAddress"`
	MACAddress *string        `json:"macAddress"`
	Specs      map[string]any `json:"specs"`

	QtyExpected string  `json:"qtyExpected"`
	QtyActual   *string `json:"qtyActual"`
	Outcome     *string `json:"outcome"`
	CountedAt   *string `json:"countedAt"`
	Note        *string `json:"note"`

	Resolution    *string `json:"resolution"`
	ResolutionRef *string `json:"resolutionRef"`

	CredentialCount int `json:"credentialCount"`

	SourceFile  string  `json:"sourceFile"`
	SourceSheet *string `json:"sourceSheet"`
}

func MapHolding(h *model.Holding) HoldingResponse {
	specs := h.Specs
	if specs == nil {
		specs = map[string]any{}
	}
	r := HoldingResponse{
		ID: h.ID, ItemID: h.ItemID,
		HolderType:     string(h.HolderType),
		UserID:         h.UserID,
		UserFIO:        h.UserFIO,
		DepartmentID:   h.DepartmentID,
		DepartmentName: h.DepartmentName,
		WarehouseID:    h.WarehouseID,
		Room:           h.Room,
		InvNumber:      h.InvNumber,
		SerialNumber:   h.SerialNumber,
		Hostname:       h.Hostname,
		IPAddress:      h.IPAddress,
		MACAddress:     h.MACAddress,
		Specs:          specs,
		QtyExpected:    h.QtyExpected.String(),
		QtyActual:      decStr(h.QtyActual),
		Outcome:        h.Outcome,
		CountedAt:      timeStr(h.CountedAt),
		Note:           h.Note,
		Resolution:     h.Resolution,
		ResolutionRef:  h.ResolutionRef,

		CredentialCount: h.CredentialCnt,
		SourceFile:      h.SourceFile,
		SourceSheet:     h.SourceSheet,
	}
	if h.Item != nil {
		r.ItemName = h.Item.Name
	}
	if h.Warehouse != nil {
		name := h.Warehouse.Name
		r.WarehouseName = &name
	}
	return r
}

func MapHoldings(hs []model.Holding) []HoldingResponse {
	out := make([]HoldingResponse, 0, len(hs))
	for i := range hs {
		out = append(out, MapHolding(&hs[i]))
	}
	return out
}

// ---------------------------------------------------------------------------

type WarehouseResponse struct {
	ID                int64   `json:"id"`
	Name              string  `json:"name"`
	DepartmentID      *int64  `json:"departmentId"`
	DepartmentName    *string `json:"departmentName"`
	ResponsibleUserID *int64  `json:"responsibleUserId"`
	// МОЛ склада: недостача адресуется ему, без имени ведомость безадресна.
	ResponsibleUserFio *string `json:"responsibleUserFio"`
	Location           *string `json:"location"`
	IsActive           bool    `json:"isActive"`
	Note               *string `json:"note"`
}

func MapWarehouse(w *model.Warehouse) WarehouseResponse {
	r := WarehouseResponse{
		ID: w.ID, Name: w.Name,
		DepartmentID: w.DepartmentID, DepartmentName: w.DepartmentName,
		ResponsibleUserID: w.ResponsibleUserID,
		Location:          w.Location, IsActive: w.IsActive, Note: w.Note,
	}
	if w.ResponsibleUser != nil {
		fio := w.ResponsibleUser.FIO()
		r.ResponsibleUserFio = &fio
	}
	return r
}

func MapWarehouses(ws []model.Warehouse) []WarehouseResponse {
	out := make([]WarehouseResponse, 0, len(ws))
	for i := range ws {
		out = append(out, MapWarehouse(&ws[i]))
	}
	return out
}

// ---------------------------------------------------------------------------

// CredentialResponse — ВСЁ, кроме секрета.
// ⚠ Поля для шифротекста здесь нет и быть не должно: секрет уходит клиенту
// только из ответа на POST /credentials/{id}/reveal.
type CredentialResponse struct {
	ID        int64   `json:"id"`
	HoldingID int64   `json:"holdingId"`
	Kind      string  `json:"kind"`
	Title     string  `json:"title"`
	URL       *string `json:"url"`
	Login     *string `json:"login"`
	// HasSecret говорит, есть ли что показывать, не показывая этого.
	HasSecret bool    `json:"hasSecret"`
	RotatedAt *string `json:"rotatedAt"`
	WipedAt   *string `json:"wipedAt"`
	Note      *string `json:"note"`
}

func MapCredential(c *repository.CredentialView) CredentialResponse {
	return CredentialResponse{
		ID: c.ID, HoldingID: c.HoldingID, Kind: c.Kind, Title: c.Title,
		URL: c.URL, Login: c.Login, HasSecret: c.HasSecret,
		RotatedAt: timeStr(c.RotatedAt), WipedAt: timeStr(c.WipedAt), Note: c.Note,
	}
}

func MapCredentials(cs []repository.CredentialView) []CredentialResponse {
	out := make([]CredentialResponse, 0, len(cs))
	for i := range cs {
		out = append(out, MapCredential(&cs[i]))
	}
	return out
}

type ToRotateResponse struct {
	UserID       int64   `json:"userId"`
	CredentialID int64   `json:"credentialId"`
	Kind         string  `json:"kind"`
	Title        string  `json:"title"`
	DeviceName   string  `json:"deviceName"`
	InvNumber    *string `json:"invNumber"`
	Warehouse    *string `json:"warehouse"`
	LastSeenAt   string  `json:"lastSeenAt"`
	RotatedAt    *string `json:"rotatedAt"`
}

func MapToRotate(rows []model.CredentialToRotateRow) []ToRotateResponse {
	out := make([]ToRotateResponse, 0, len(rows))
	for _, r := range rows {
		out = append(out, ToRotateResponse{
			UserID: r.UserID, CredentialID: r.CredentialID, Kind: r.Kind, Title: r.Title,
			DeviceName: r.DeviceName, InvNumber: r.InvNumber, Warehouse: r.Warehouse,
			LastSeenAt: r.LastSeenAt.Format(time.RFC3339), RotatedAt: timeStr(r.RotatedAt),
		})
	}
	return out
}

// ---------------------------------------------------------------------------

type LossResponse struct {
	ItemID           int64   `json:"itemId"`
	Name             string  `json:"name"`
	Area             *string `json:"area"`
	HoldingID        int64   `json:"holdingId"`
	HolderType       string  `json:"holderType"`
	HolderPerson     *string `json:"holderPerson"`
	HolderDepartment *string `json:"holderDepartment"`
	Warehouse        *string `json:"warehouse"`
	// Кому адресуется недостача по этой строке.
	ResponsiblePerson string  `json:"responsiblePerson"`
	Room              *string `json:"room"`
	InvNumber         *string `json:"invNumber"`
	SerialNumber      *string `json:"serialNumber"`
	QtyExpected       string  `json:"qtyExpected"`
	QtyActual         *string `json:"qtyActual"`
	QtyDiff           *string `json:"qtyDiff"`
	// DiscrepancyKind приходит из представления: недостача | излишек |
	// поломка | в ремонте. Это разделы акта списания.
	DiscrepancyKind string  `json:"discrepancyKind"`
	Outcome         *string `json:"outcome"`
	Resolution      *string `json:"resolution"`
	ResolutionRef   *string `json:"resolutionRef"`
	Note            *string `json:"note"`
}

func MapLosses(rows []model.LossRow) []LossResponse {
	out := make([]LossResponse, 0, len(rows))
	for _, r := range rows {
		out = append(out, LossResponse{
			ItemID: r.ItemID, Name: r.Name, Area: r.Area,
			HoldingID: r.HoldingID, HolderType: r.HolderType,
			HolderPerson: r.HolderPerson, HolderDepartment: r.HolderDepartment,
			Warehouse: r.Warehouse, ResponsiblePerson: r.ResponsiblePerson,
			Room: r.Room, InvNumber: r.InvNumber, SerialNumber: r.SerialNumber,
			QtyExpected:     r.QtyExpected.String(),
			QtyActual:       decStr(r.QtyActual),
			QtyDiff:         decStr(r.QtyDiff),
			DiscrepancyKind: r.DiscrepancyKind, Outcome: r.Outcome,
			Resolution: r.Resolution, ResolutionRef: r.ResolutionRef, Note: r.Note,
		})
	}
	return out
}

type SummaryResponse struct {
	ItemID              int64   `json:"itemId"`
	Name                string  `json:"name"`
	Area                *string `json:"area"`
	Unit                *string `json:"unit"`
	PersonExpected      *string `json:"personExpected"`
	DepartmentExpected  *string `json:"departmentExpected"`
	UnallocatedExpected *string `json:"unallocatedExpected"`
	TotalExpected       *string `json:"totalExpected"`
	TotalActual         *string `json:"totalActual"`
	NotCountedRows      int64   `json:"notCountedRows"`
}

func MapSummary(rows []model.ItemTotalRow) []SummaryResponse {
	out := make([]SummaryResponse, 0, len(rows))
	for _, r := range rows {
		out = append(out, SummaryResponse{
			ItemID: r.ItemID, Name: r.Name, Area: r.Area, Unit: r.Unit,
			PersonExpected:      decStr(r.PersonExpected),
			DepartmentExpected:  decStr(r.DepartmentExpected),
			UnallocatedExpected: decStr(r.UnallocatedExpected),
			TotalExpected:       decStr(r.TotalExpected),
			TotalActual:         decStr(r.TotalActual),
			NotCountedRows:      r.NotCountedRows,
		})
	}
	return out
}

// ---------------------------------------------------------------------------

func decStr(d *decimal.Decimal) *string {
	if d == nil {
		return nil
	}
	s := d.String()
	return &s
}

func timeStr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.RFC3339)
	return &s
}
