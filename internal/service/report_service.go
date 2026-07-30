package service

import (
	"context"
	"fmt"
	"io"

	"github.com/shopspring/decimal"
	"github.com/xuri/excelize/v2"

	"go_warehouse_service/internal/model"
	"go_warehouse_service/internal/repository"
)

type ReportService struct {
	reports *repository.ReportRepository
}

func NewReportService(r *repository.ReportRepository) *ReportService {
	return &ReportService{reports: r}
}

func (s *ReportService) Losses(ctx context.Context, f repository.LossFilters) ([]model.LossRow, int64, error) {
	if _, err := requireRead(ctx); err != nil {
		return nil, 0, err
	}
	return s.reports.Losses(ctx, f)
}

func (s *ReportService) Summary(ctx context.Context, f repository.SummaryFilters) ([]model.ItemTotalRow, int64, error) {
	if _, err := requireRead(ctx); err != nil {
		return nil, 0, err
	}
	return s.reports.Summary(ctx, f)
}

// LossesXLSX — сличительная ведомость файлом, то, что уносят в бухгалтерию.
//
// Колонка «Вид расхождения» приходит из представления, а не считается здесь:
// иначе экран и выгрузка могли бы разойтись в трактовке одной и той же строки.
func (s *ReportService) LossesXLSX(ctx context.Context, f repository.LossFilters, w io.Writer) error {
	if _, err := requireRead(ctx); err != nil {
		return err
	}

	// Выгрузка идёт целиком: акт списания с постраничной разбивкой бессмыслен.
	f.Limit, f.Offset = 0, 0
	rows, _, err := s.reports.Losses(ctx, f)
	if err != nil {
		return err
	}

	xl := excelize.NewFile()
	defer func() { _ = xl.Close() }()

	const sheet = "Ведомость"
	idx, err := xl.NewSheet(sheet)
	if err != nil {
		return err
	}
	xl.SetActiveSheet(idx)
	_ = xl.DeleteSheet("Sheet1")

	headers := []string{
		"Вид расхождения", "Наименование", "Область", "Тип держателя",
		"Сотрудник", "Подразделение", "Склад", "МОЛ склада", "Кабинет",
		"Инв. номер", "Серийный номер",
		"Числится", "Найдено", "Разница", "Итог", "Решение", "Документ", "Примечание",
	}
	for i, h := range headers {
		cell, err := excelize.CoordinatesToCellName(i+1, 1)
		if err != nil {
			return err
		}
		if err := xl.SetCellValue(sheet, cell, h); err != nil {
			return err
		}
	}
	bold, err := xl.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	if err == nil {
		last, _ := excelize.CoordinatesToCellName(len(headers), 1)
		_ = xl.SetCellStyle(sheet, "A1", last, bold)
	}

	for r, row := range rows {
		values := []any{
			row.DiscrepancyKind,
			row.Name,
			deref(row.Area),
			holderTypeLabel(row.HolderType),
			deref(row.HolderPerson),
			deref(row.HolderDepartment),
			deref(row.Warehouse),
			row.ResponsiblePerson,
			deref(row.Room),
			deref(row.InvNumber),
			deref(row.SerialNumber),
			// Количества строками: NUMERIC(14,3) не должен превращаться
			// во float по дороге в отчёт.
			row.QtyExpected.String(),
			decStr(row.QtyActual),
			decStr(row.QtyDiff),
			outcomeLabel(row.Outcome),
			resolutionLabel(row.Resolution),
			deref(row.ResolutionRef),
			deref(row.Note),
		}
		for c, v := range values {
			cell, err := excelize.CoordinatesToCellName(c+1, r+2)
			if err != nil {
				return err
			}
			if err := xl.SetCellValue(sheet, cell, v); err != nil {
				return err
			}
		}
	}

	widths := []float64{16, 46, 12, 14, 26, 28, 20, 24, 14, 14, 20, 11, 11, 11, 14, 14, 18, 40}
	for i, wd := range widths {
		col, err := excelize.ColumnNumberToName(i + 1)
		if err != nil {
			continue
		}
		_ = xl.SetColWidth(sheet, col, col, wd)
	}
	if len(rows) > 0 {
		last, _ := excelize.CoordinatesToCellName(len(headers), len(rows)+1)
		_ = xl.AutoFilter(sheet, fmt.Sprintf("A1:%s", last), nil)
	}
	_ = xl.SetPanes(sheet, &excelize.Panes{
		Freeze: true, Split: false, XSplit: 0, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft",
	})

	return xl.Write(w)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func decStr(d *decimal.Decimal) string {
	if d == nil {
		return ""
	}
	return d.String()
}

func holderTypeLabel(t string) string {
	switch model.HolderType(t) {
	case model.HolderPerson:
		return "за человеком"
	case model.HolderDepartment:
		return "за отделом"
	case model.HolderWarehouse:
		return "на складе"
	case model.HolderUnknown:
		return "не определён"
	}
	return t
}

func outcomeLabel(o *string) string {
	if o == nil {
		return ""
	}
	switch model.Outcome(*o) {
	case model.OutcomeFound:
		return "найдено"
	case model.OutcomeMissing:
		return "не найдено"
	case model.OutcomeBroken:
		return "сломано"
	case model.OutcomeInRepair:
		return "в ремонте"
	case model.OutcomeSurplus:
		return "излишек"
	}
	return *o
}

func resolutionLabel(r *string) string {
	if r == nil {
		return ""
	}
	switch model.Resolution(*r) {
	case model.ResolutionWrittenOff:
		return "списано"
	case model.ResolutionFoundLater:
		return "нашлось позже"
	case model.ResolutionExplained:
		return "объяснено"
	case model.ResolutionPending:
		return "в работе"
	}
	return *r
}
