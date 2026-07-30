// Package helper содержит утилиты HTTP-слоя: единообразную запись JSON-ответов
// и маппинг доменных ошибок (apperr) в HTTP-статусы.
package helper

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"go_warehouse_service/internal/apperr"
)

// WriteJSON пишет data как JSON с указанным статусом.
func WriteJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if data == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(data); err != nil {
		// Заголовки уже отправлены — ответ ушёл частично, писать ещё раз нельзя.
		return
	}
}

func WriteError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "internal server error"
	code := "internal_error"

	var appErr *apperr.Error
	if errors.As(err, &appErr) {
		code = string(appErr.Code)
		message = code
		status = statusForErrorCode(appErr.Code)
	} else {
		switch {
		case errors.Is(err, apperr.ErrNotFound):
			status = http.StatusNotFound
			code = string(apperr.CodeNotFound)
			message = code
		case errors.Is(err, apperr.ErrForbidden):
			status = http.StatusForbidden
			code = string(apperr.CodeForbidden)
			message = code
		case errors.Is(err, apperr.ErrUnauthorized):
			status = http.StatusUnauthorized
			code = string(apperr.CodeUnauthorized)
			message = code
		case errors.Is(err, apperr.ErrConflict):
			status = http.StatusConflict
			code = string(apperr.CodeConflict)
			message = code
		case errors.Is(err, apperr.ErrValidation):
			status = http.StatusBadRequest
			code = string(apperr.CodeValidation)
			message = code
		}
	}

	// 5xx маскируются generic-телом, поэтому исходную ошибку логируем здесь
	// централизованно — иначе она пропадает бесследно.
	if status >= http.StatusInternalServerError {
		slog.Error("request failed", "code", code, "error", err)
	}

	WriteJSON(w, status, map[string]string{
		"error": message,
		"code":  code,
	})
}

func statusForErrorCode(code apperr.ErrorCode) int {
	switch category(code) {
	case apperr.CodeNotFound:
		return http.StatusNotFound
	case apperr.CodeForbidden:
		return http.StatusForbidden
	case apperr.CodeUnauthorized:
		return http.StatusUnauthorized
	case apperr.CodeConflict:
		return http.StatusConflict
	case apperr.CodeValidation:
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// category приводит частный код к категории, используя тот же маппинг, что и apperr.Is.
func category(code apperr.ErrorCode) apperr.ErrorCode {
	for _, c := range []apperr.ErrorCode{
		apperr.CodeNotFound,
		apperr.CodeForbidden,
		apperr.CodeUnauthorized,
		apperr.CodeConflict,
		apperr.CodeValidation,
	} {
		if apperr.New(code, "").Is(apperr.New(c, "")) {
			return c
		}
	}
	return code
}
