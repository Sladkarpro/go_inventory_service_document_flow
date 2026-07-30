package handler

import (
	"fmt"
	"net/http"

	"go_warehouse_service/internal/dto"
	"go_warehouse_service/internal/helper"
	"go_warehouse_service/internal/repository"
	"go_warehouse_service/internal/service"
)

type ReportHandler struct {
	reports *service.ReportService
	clock   helper.Clock
}

func NewReportHandler(r *service.ReportService, clk helper.Clock) *ReportHandler {
	return &ReportHandler{reports: r, clock: clk}
}

func (h *ReportHandler) lossFilters(r *http.Request, limit, offset int) (repository.LossFilters, error) {
	warehouseID, err := helper.QueryInt64(r, "warehouse_id")
	if err != nil {
		return repository.LossFilters{}, err
	}
	userID, err := helper.QueryInt64(r, "user_id")
	if err != nil {
		return repository.LossFilters{}, err
	}
	responsibleID, err := helper.QueryInt64(r, "responsible_user_id")
	if err != nil {
		return repository.LossFilters{}, err
	}
	return repository.LossFilters{
		HolderType:        r.URL.Query().Get("holder_type"),
		WarehouseID:       warehouseID,
		UserID:            userID,
		ResponsibleUserID: responsibleID,
		Area:              r.URL.Query().Get("area"),
		Kind:              r.URL.Query().Get("kind"),
		Unresolved:        helper.QueryBool(r, "unresolved"),
		Search:            r.URL.Query().Get("search"),
		Room:              r.URL.Query().Get("room"),
		Limit:             limit, Offset: offset,
	}, nil
}

// Losses — сличительная ведомость: пропажи, поломки и неучтёнка.
func (h *ReportHandler) Losses() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, offset := helper.Pagination(r, defaultLimit, maxLimit)
		f, err := h.lossFilters(r, limit, offset)
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		rows, total, err := h.reports.Losses(r.Context(), f)
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		helper.WriteJSON(w, http.StatusOK, map[string]any{
			"losses":     dto.MapLosses(rows),
			"pagination": dto.NewPagination(total, limit, offset),
		})
	}
}

func (h *ReportHandler) Summary() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, offset := helper.Pagination(r, defaultLimit, maxLimit)
		rows, total, err := h.reports.Summary(r.Context(), repository.SummaryFilters{
			Search: r.URL.Query().Get("search"),
			Area:   r.URL.Query().Get("area"),
			Limit:  limit, Offset: offset,
		})
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		helper.WriteJSON(w, http.StatusOK, map[string]any{
			"summary":    dto.MapSummary(rows),
			"pagination": dto.NewPagination(total, limit, offset),
		})
	}
}

// LossesXLSX — выгрузка ведомости файлом.
func (h *ReportHandler) LossesXLSX() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, err := h.lossFilters(r, 0, 0)
		if err != nil {
			helper.WriteError(w, err)
			return
		}

		// Заголовки ставим ДО записи тела: после первого байта их уже не изменить,
		// и ошибка ушла бы клиенту как битый xlsx со статусом 200.
		filename := fmt.Sprintf("Сличительная ведомость %s.xlsx",
			h.clock.Now().Format("2006-01-02"))
		w.Header().Set("Content-Type",
			"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
		w.Header().Set("Content-Disposition",
			fmt.Sprintf("attachment; filename*=UTF-8''%s", urlEncode(filename)))

		if err := h.reports.LossesXLSX(r.Context(), f, w); err != nil {
			// Тело могло уже начать уходить — пишем в лог, менять статус поздно.
			helper.WriteError(w, err)
			return
		}
	}
}

// urlEncode кодирует имя файла по RFC 5987 (filename* с UTF-8):
// имя кириллическое, без кодирования браузер сохранит мусор.
func urlEncode(s string) string {
	const hex = "0123456789ABCDEF"
	out := make([]byte, 0, len(s)*3)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			out = append(out, c)
			continue
		}
		out = append(out, '%', hex[c>>4], hex[c&0x0F])
	}
	return string(out)
}
