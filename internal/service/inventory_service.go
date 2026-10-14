package service

import (
	"context"

	"github.com/shopspring/decimal"

	"go_warehouse_service/internal/apperr"
	"go_warehouse_service/internal/helper"
	"go_warehouse_service/internal/middleware"
	"go_warehouse_service/internal/model"
	"go_warehouse_service/internal/repository"
)

// requireRead / requireWrite / requireAdmin — единственные места проверки прав.
//
// ⚠ Проверка идёт ПОМЕТОДНО, а не одной ролью на весь ресурс. МОЛ, наблюдатель
// и начальник отдела — параллельные ветки иерархии, и «повесить ROLE_..._VIEWER
// на весь контроллер» отрезало бы МОЛа, который как раз и проводит пересчёт.
// В монолите на этом уже обожглись.
func requireRead(ctx context.Context) (middleware.UserClaims, error) {
	user, ok := middleware.GetUser(ctx)
	if !ok {
		return user, apperr.ErrUnauthorized
	}
	if !user.CanRead() {
		return user, apperr.New(apperr.CodeInsufficientRights, "insufficient rights")
	}
	return user, nil
}

func requireWrite(ctx context.Context) (middleware.UserClaims, error) {
	user, ok := middleware.GetUser(ctx)
	if !ok {
		return user, apperr.ErrUnauthorized
	}
	if !user.CanWrite() {
		return user, apperr.New(apperr.CodeInsufficientRights, "insufficient rights")
	}
	return user, nil
}

func requireAdmin(ctx context.Context) (middleware.UserClaims, error) {
	user, ok := middleware.GetUser(ctx)
	if !ok {
		return user, apperr.ErrUnauthorized
	}
	if !user.IsInventoryAdmin() {
		return user, apperr.New(apperr.CodeInsufficientRights, "insufficient rights")
	}
	return user, nil
}

type ItemService struct {
	items    *repository.ItemRepository
	holdings *repository.HoldingRepository
}

func NewItemService(items *repository.ItemRepository, holdings *repository.HoldingRepository) *ItemService {
	return &ItemService{items: items, holdings: holdings}
}

func (s *ItemService) List(ctx context.Context, f repository.ItemFilters) ([]model.Item, int64, error) {
	if _, err := requireRead(ctx); err != nil {
		return nil, 0, err
	}
	if f.Area != "" && !model.ValidArea(f.Area) {
		return nil, 0, apperr.New(apperr.CodeInvalidArea, "invalid area")
	}
	return s.items.List(ctx, f)
}

func (s *ItemService) Get(ctx context.Context, id int64) (*model.Item, []model.Holding, error) {
	if _, err := requireRead(ctx); err != nil {
		return nil, nil, err
	}
	item, err := s.items.GetByID(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	holdings, err := s.holdings.ListByItem(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return item, holdings, nil
}

func (s *ItemService) Update(ctx context.Context, id int64, upd repository.ItemUpdate) (*model.Item, error) {
	if _, err := requireWrite(ctx); err != nil {
		return nil, err
	}
	if upd.Area != nil && *upd.Area != "" && !model.ValidArea(*upd.Area) {
		return nil, apperr.New(apperr.CodeInvalidArea, "invalid area")
	}
	return s.items.Update(ctx, id, upd)
}

type HoldingService struct {
	holdings *repository.HoldingRepository
	clock    helper.Clock
}

func NewHoldingService(holdings *repository.HoldingRepository, clk helper.Clock) *HoldingService {
	return &HoldingService{holdings: holdings, clock: clk}
}

func (s *HoldingService) List(ctx context.Context, f repository.HoldingFilters) ([]model.Holding, int64, error) {
	if _, err := requireRead(ctx); err != nil {
		return nil, 0, err
	}
	if f.HolderType != "" && !model.ValidHolderType(f.HolderType) {
		return nil, 0, apperr.New(apperr.CodeInvalidHolderType, "invalid holder type")
	}
	if f.Outcome != "" && !model.ValidOutcome(f.Outcome) {
		return nil, 0, apperr.New(apperr.CodeInvalidOutcome, "invalid outcome")
	}
	return s.holdings.List(ctx, f)
}

func (s *HoldingService) Get(ctx context.Context, id int64) (*model.Holding, error) {
	if _, err := requireRead(ctx); err != nil {
		return nil, err
	}
	return s.holdings.GetByID(ctx, id)
}

func (s *HoldingService) Create(ctx context.Context, in repository.HoldingInput) (*model.Holding, error) {
	user, err := requireWrite(ctx)
	if err != nil {
		return nil, err
	}
	if !model.ValidHolderType(in.HolderType) {
		return nil, apperr.New(apperr.CodeInvalidHolderType, "invalid holder type")
	}
	if in.Outcome != nil && *in.Outcome != "" && !model.ValidOutcome(*in.Outcome) {
		return nil, apperr.New(apperr.CodeInvalidOutcome, "invalid outcome")
	}
	if in.QtyExpected.IsNegative() ||
		(in.QtyActual != nil && in.QtyActual.IsNegative()) {
		return nil, apperr.New(apperr.CodeInvalidQuantity, "quantity must not be negative")
	}
	in.CountedByID = &user.ID
	return s.holdings.Create(ctx, in, s.clock.Now())
}

func (s *HoldingService) Update(ctx context.Context, id int64, upd repository.HoldingUpdate) (*model.Holding, error) {
	user, err := requireWrite(ctx)
	if err != nil {
		return nil, err
	}
	if upd.HolderType != nil && !model.ValidHolderType(*upd.HolderType) {
		return nil, apperr.New(apperr.CodeInvalidHolderType, "invalid holder type")
	}
	if upd.Outcome != nil && *upd.Outcome != "" && !model.ValidOutcome(*upd.Outcome) {
		return nil, apperr.New(apperr.CodeInvalidOutcome, "invalid outcome")
	}
	if upd.Resolution != nil && *upd.Resolution != "" && !model.ValidResolution(*upd.Resolution) {
		return nil, apperr.New(apperr.CodeInvalidResolution, "invalid resolution")
	}
	if upd.QtyActual != nil && upd.QtyActual.IsNegative() {
		return nil, apperr.New(apperr.CodeInvalidQuantity, "quantity must not be negative")
	}
	return s.holdings.Update(ctx, id, upd, user.ID, s.clock.Now())
}

func (s *HoldingService) Delete(ctx context.Context, id int64) error {
	if _, err := requireWrite(ctx); err != nil {
		return err
	}
	return s.holdings.Delete(ctx, id)
}

func (s *HoldingService) Rooms(ctx context.Context) ([]string, error) {
	if _, err := requireRead(ctx); err != nil {
		return nil, err
	}
	return s.holdings.Rooms(ctx)
}

type WarehouseService struct {
	warehouses *repository.WarehouseRepository
}

func NewWarehouseService(w *repository.WarehouseRepository) *WarehouseService {
	return &WarehouseService{warehouses: w}
}

func (s *WarehouseService) List(ctx context.Context, onlyActive bool) ([]model.Warehouse, error) {
	if _, err := requireRead(ctx); err != nil {
		return nil, err
	}
	return s.warehouses.List(ctx, onlyActive)
}

func (s *WarehouseService) Create(ctx context.Context, in repository.WarehouseInput) (*model.Warehouse, error) {
	if _, err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if model.CleanText(in.Name) == "" {
		return nil, apperr.New(apperr.CodeWarehouseNameRequired, "warehouse name required")
	}
	return s.warehouses.Create(ctx, in)
}

func (s *WarehouseService) Update(ctx context.Context, id int64, in repository.WarehouseInput) (*model.Warehouse, error) {
	if _, err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	return s.warehouses.Update(ctx, id, in)
}

// DecimalPtr — маленький помощник для DTO-слоя: количество приходит строкой,
// а не числом, чтобы «1 234,56» не потеряло точность на float.
func DecimalPtr(s string) (*decimal.Decimal, error) {
	if s == "" {
		return nil, nil
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return nil, apperr.New(apperr.CodeInvalidQuantity, "invalid quantity")
	}
	return &d, nil
}
