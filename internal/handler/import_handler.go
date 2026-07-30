package handler

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"go_warehouse_service/internal/apperr"
	"go_warehouse_service/internal/helper"
	"go_warehouse_service/internal/middleware"
	"go_warehouse_service/internal/parser"
	"go_warehouse_service/internal/service"
)

// maxUploadSize — выгрузки владельца укладываются в сотни килобайт;
// 32 МБ с большим запасом и защищают от загрузки чего-то постороннего.
const maxUploadSize = 32 << 20

type ImportHandler struct {
	imports *service.ImportService
}

func NewImportHandler(i *service.ImportService) *ImportHandler {
	return &ImportHandler{imports: i}
}

// Import принимает файл и ЯВНО указанный формат.
//
// ⚠ Формат не угадывается по содержимому намеренно. Три из четырёх файлов
// внешне неотличимы (все — «оборотно-сальдовая ведомость»), а цена ошибки
// высока: счёт 10, прочитанный как МЦ.04, положил бы складские остатки
// в строки «за людьми».
func (h *ImportHandler) Import() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// ⚠ Импорт — АДМИНСКОЕ действие, а не просто «право записи».
		//
		// Раньше здесь стояло CanWrite, то есть загружать выгрузки мог и МОЛ.
		// Одна перезаливка переписывает остатки ВСЕГО предприятия: именно так
		// вскрылись две утечки этой сессии — повтор возвращал агрегатам
		// отщеплённое (+407 единиц) и выдумывал факт на 407 строках. Создание
		// склада при этом уже требовало администратора, то есть импорт был
		// одновременно опаснее и доступнее. МОЛ проводит пересчёт (PATCH
		// факта), а загрузка выгрузок 1С — отдельное действие.
		// Решение владельца 29.07.2026.
		user, ok := middleware.GetUser(r.Context())
		if !ok || !user.IsInventoryAdmin() {
			helper.WriteError(w, apperr.New(apperr.CodeInsufficientRights, "insufficient rights"))
			return
		}

		if err := r.ParseMultipartForm(maxUploadSize); err != nil {
			helper.WriteError(w, apperr.New(apperr.CodeFileTooLarge, "file too large"))
			return
		}

		format := r.FormValue("format")
		if !parser.ValidFormat(format) {
			helper.WriteError(w, apperr.New(apperr.CodeUnknownFormat,
				"format must be one of: mc04, account10, manual, registry"))
			return
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			helper.WriteError(w, apperr.New(apperr.CodeFileNotProvided, "file not provided"))
			return
		}
		defer func() { _ = file.Close() }()

		if ext := filepath.Ext(header.Filename); ext != ".xlsx" {
			helper.WriteError(w, apperr.New(apperr.CodeUnknownFormat, "only .xlsx is supported"))
			return
		}

		// excelize читает с диска, поэтому загрузку сначала кладём во временный файл.
		tmp, err := os.CreateTemp("", "warehouse-import-*.xlsx")
		if err != nil {
			helper.WriteError(w, fmt.Errorf("временный файл: %w", err))
			return
		}
		tmpName := tmp.Name()
		defer func() {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}()

		if _, err := io.Copy(tmp, io.LimitReader(file, maxUploadSize)); err != nil {
			helper.WriteError(w, fmt.Errorf("сохранение загрузки: %w", err))
			return
		}
		if err := tmp.Close(); err != nil {
			helper.WriteError(w, fmt.Errorf("сохранение загрузки: %w", err))
			return
		}

		report, err := h.imports.Import(r.Context(), tmpName, parser.Format(format))
		if err != nil {
			helper.WriteError(w, apperr.Wrap(apperr.CodeImportFailed, "import failed", err))
			return
		}
		// Имя временного файла наружу не показываем — подставляем настоящее.
		report.File = header.Filename

		helper.WriteJSON(w, http.StatusOK, map[string]any{"report": report})
	}
}
