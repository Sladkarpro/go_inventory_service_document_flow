package handler

import (
	"encoding/json"
	"net/http"

	"go_warehouse_service/internal/apperr"
	"go_warehouse_service/internal/dto"
	"go_warehouse_service/internal/helper"
	"go_warehouse_service/internal/repository"
	"go_warehouse_service/internal/service"
)

const (
	defaultLimit = 50
	maxLimit     = 500
)

func decode(r *http.Request, dst any) error {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		return apperr.New(apperr.CodeInvalidJSON, "invalid json")
	}
	return nil
}

type ItemHandler struct {
	items *service.ItemService
}

func NewItemHandler(items *service.ItemService) *ItemHandler {
	return &ItemHandler{items: items}
}

func (h *ItemHandler) List() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, offset := helper.Pagination(r, defaultLimit, maxLimit)
		f := repository.ItemFilters{
			Search:          r.URL.Query().Get("search"),
			Area:            r.URL.Query().Get("area"),
			OnlyUnallocated: helper.QueryBool(r, "only_unallocated"),
			OnlyLosses:      helper.QueryBool(r, "only_losses"),
			Limit:           limit, Offset: offset,
		}
		items, total, err := h.items.List(r.Context(), f)
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		helper.WriteJSON(w, http.StatusOK, map[string]any{
			"items":      dto.MapItems(items),
			"pagination": dto.NewPagination(total, limit, offset),
		})
	}
}

func (h *ItemHandler) Get() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := helper.IDParam(r, "id")
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		item, holdings, err := h.items.Get(r.Context(), id)
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		helper.WriteJSON(w, http.StatusOK, map[string]any{
			"item":     dto.MapItem(item),
			"holdings": dto.MapHoldings(holdings),
		})
	}
}

func (h *ItemHandler) Update() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := helper.IDParam(r, "id")
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		var req dto.UpdateItemRequest
		if err := decode(r, &req); err != nil {
			helper.WriteError(w, err)
			return
		}
		item, err := h.items.Update(r.Context(), id, repository.ItemUpdate{
			Area: req.Area, Unit: req.Unit, Attributes: req.Attributes, Note: req.Note,
		})
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		helper.WriteJSON(w, http.StatusOK, map[string]any{"item": dto.MapItem(item)})
	}
}

type HoldingHandler struct {
	holdings *service.HoldingService
}

func NewHoldingHandler(h *service.HoldingService) *HoldingHandler {
	return &HoldingHandler{holdings: h}
}

func (h *HoldingHandler) List() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, offset := helper.Pagination(r, defaultLimit, maxLimit)

		userID, err := helper.QueryInt64(r, "user_id")
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		deptID, err := helper.QueryInt64(r, "department_id")
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		whID, err := helper.QueryInt64(r, "warehouse_id")
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		itemID, err := helper.QueryInt64(r, "item_id")
		if err != nil {
			helper.WriteError(w, err)
			return
		}

		f := repository.HoldingFilters{
			HolderType:   r.URL.Query().Get("holder_type"),
			UserID:       userID,
			DepartmentID: deptID,
			WarehouseID:  whID,
			ItemID:       itemID,
			Room:         r.URL.Query().Get("room"),
			Outcome:      r.URL.Query().Get("outcome"),
			InvNumber:    r.URL.Query().Get("inv_number"),
			Search:       r.URL.Query().Get("search"),
			// Главный рабочий фильтр обхода: «что ещё не считали».
			OnlyUncounted: helper.QueryBool(r, "only_uncounted"),
			OnlyDiff:      helper.QueryBool(r, "only_diff"),
			Limit:         limit, Offset: offset,
		}
		holdings, total, err := h.holdings.List(r.Context(), f)
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		helper.WriteJSON(w, http.StatusOK, map[string]any{
			"holdings":   dto.MapHoldings(holdings),
			"pagination": dto.NewPagination(total, limit, offset),
		})
	}
}

func (h *HoldingHandler) Create() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req dto.CreateHoldingRequest
		if err := decode(r, &req); err != nil {
			helper.WriteError(w, err)
			return
		}
		if req.ItemID == 0 {
			helper.WriteError(w, apperr.New(apperr.CodeItemNameRequired, "item id required"))
			return
		}

		qtyExpected, err := service.DecimalPtr(req.QtyExpected)
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		in := repository.HoldingInput{
			ItemID:         req.ItemID,
			HolderType:     req.HolderType,
			UserID:         req.UserID,
			UserFIO:        req.UserFIO,
			DepartmentID:   req.DepartmentID,
			DepartmentName: req.DepartmentName,
			WarehouseID:    req.WarehouseID,
			Room:           req.Room,
			InvNumber:      req.InvNumber,
			SerialNumber:   req.SerialNumber,
			Outcome:        req.Outcome,
			Note:           req.Note,
			SourceFile:     "ручной ввод",
		}
		if qtyExpected != nil {
			in.QtyExpected = *qtyExpected
		}
		if req.QtyActual != nil {
			qa, err := service.DecimalPtr(*req.QtyActual)
			if err != nil {
				helper.WriteError(w, err)
				return
			}
			in.QtyActual = qa
		}

		holding, err := h.holdings.Create(r.Context(), in)
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		helper.WriteJSON(w, http.StatusCreated, map[string]any{"holding": dto.MapHolding(holding)})
	}
}

func (h *HoldingHandler) Update() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := helper.IDParam(r, "id")
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		var req dto.UpdateHoldingRequest
		if err := decode(r, &req); err != nil {
			helper.WriteError(w, err)
			return
		}

		upd := repository.HoldingUpdate{
			ClearQtyActual: req.ClearQtyActual,
			Outcome:        req.Outcome,
			Room:           req.Room,
			Note:           req.Note,
			HolderType:     req.HolderType,
			UserID:         req.UserID,
			UserFIO:        req.UserFIO,
			DepartmentID:   req.DepartmentID,
			DepartmentName: req.DepartmentName,
			WarehouseID:    req.WarehouseID,
			InvNumber:      req.InvNumber,
			SerialNumber:   req.SerialNumber,
			Hostname:       req.Hostname,
			IPAddress:      req.IPAddress,
			MACAddress:     req.MACAddress,
			Specs:          req.Specs,
			Resolution:     req.Resolution,
			ResolutionRef:  req.ResolutionRef,
		}
		if req.QtyActual != nil {
			qa, err := service.DecimalPtr(*req.QtyActual)
			if err != nil {
				helper.WriteError(w, err)
				return
			}
			upd.QtyActual = qa
		}

		holding, err := h.holdings.Update(r.Context(), id, upd)
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		helper.WriteJSON(w, http.StatusOK, map[string]any{"holding": dto.MapHolding(holding)})
	}
}

func (h *HoldingHandler) Delete() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := helper.IDParam(r, "id")
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		if err := h.holdings.Delete(r.Context(), id); err != nil {
			helper.WriteError(w, err)
			return
		}
		helper.WriteJSON(w, http.StatusNoContent, nil)
	}
}

func (h *HoldingHandler) Rooms() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rooms, err := h.holdings.Rooms(r.Context())
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		helper.WriteJSON(w, http.StatusOK, map[string]any{"rooms": rooms})
	}
}

type WarehouseHandler struct {
	warehouses *service.WarehouseService
}

func NewWarehouseHandler(s *service.WarehouseService) *WarehouseHandler {
	return &WarehouseHandler{warehouses: s}
}

func (h *WarehouseHandler) List() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := h.warehouses.List(r.Context(), helper.QueryBool(r, "only_active"))
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		helper.WriteJSON(w, http.StatusOK, map[string]any{
			"warehouses": dto.MapWarehouses(items),
		})
	}
}

func (h *WarehouseHandler) Create() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req dto.WarehouseRequest
		if err := decode(r, &req); err != nil {
			helper.WriteError(w, err)
			return
		}
		wh, err := h.warehouses.Create(r.Context(), toWarehouseInput(req))
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		helper.WriteJSON(w, http.StatusCreated, map[string]any{"warehouse": dto.MapWarehouse(wh)})
	}
}

func (h *WarehouseHandler) Update() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := helper.IDParam(r, "id")
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		var req dto.WarehouseRequest
		if err := decode(r, &req); err != nil {
			helper.WriteError(w, err)
			return
		}
		wh, err := h.warehouses.Update(r.Context(), id, toWarehouseInput(req))
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		helper.WriteJSON(w, http.StatusOK, map[string]any{"warehouse": dto.MapWarehouse(wh)})
	}
}

func toWarehouseInput(req dto.WarehouseRequest) repository.WarehouseInput {
	return repository.WarehouseInput{
		Name:              req.Name,
		DepartmentID:      req.DepartmentID,
		DepartmentName:    req.DepartmentName,
		ResponsibleUserID: req.ResponsibleUserID,
		Location:          req.Location,
		IsActive:          req.IsActive,
		Note:              req.Note,
	}
}
