package handler

import (
	"net/http"

	"go_warehouse_service/internal/apperr"
	"go_warehouse_service/internal/dto"
	"go_warehouse_service/internal/helper"
	"go_warehouse_service/internal/service"
)

type CredentialHandler struct {
	creds *service.CredentialService
}

func NewCredentialHandler(c *service.CredentialService) *CredentialHandler {
	return &CredentialHandler{creds: c}
}

// List отдаёт перечень доступов БЕЗ секретов.
func (h *CredentialHandler) List() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		holdingID, err := helper.QueryInt64(r, "holding_id")
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		if holdingID == nil {
			helper.WriteError(w, apperr.New(apperr.CodeValidation, "holding_id required"))
			return
		}
		creds, err := h.creds.ListByHolding(r.Context(), *holdingID)
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		helper.WriteJSON(w, http.StatusOK, map[string]any{
			"credentials": dto.MapCredentials(creds),
		})
	}
}

func (h *CredentialHandler) Create() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req dto.CreateCredentialRequest
		if err := decode(r, &req); err != nil {
			helper.WriteError(w, err)
			return
		}
		cred, err := h.creds.Create(r.Context(), service.CredentialCreate{
			HoldingID: req.HoldingID, Kind: req.Kind, Title: req.Title,
			URL: req.URL, Login: req.Login, Secret: req.Secret, Note: req.Note,
		})
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		helper.WriteJSON(w, http.StatusCreated, map[string]any{
			"credential": dto.MapCredential(cred),
		})
	}
}

func (h *CredentialHandler) Update() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := helper.IDParam(r, "id")
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		var req dto.UpdateCredentialRequest
		if err := decode(r, &req); err != nil {
			helper.WriteError(w, err)
			return
		}
		cred, err := h.creds.Update(r.Context(), id, service.CredentialPatch{
			Kind: req.Kind, Title: req.Title, URL: req.URL,
			Login: req.Login, Note: req.Note, Secret: req.Secret,
		})
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		helper.WriteJSON(w, http.StatusOK, map[string]any{
			"credential": dto.MapCredential(cred),
		})
	}
}

func (h *CredentialHandler) Delete() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := helper.IDParam(r, "id")
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		wiped, err := h.creds.Delete(r.Context(), id)
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		if wiped {
			// Доступ смотрели — строка осталась ради журнала, секрет затёрт.
			// Отвечаем 200 и говорим об этом явно: 204 соврал бы, что запись исчезла.
			helper.WriteJSON(w, http.StatusOK, map[string]any{
				"wiped":   true,
				"message": "доступ просматривали — секрет затёрт, запись сохранена ради журнала",
			})
			return
		}
		helper.WriteJSON(w, http.StatusNoContent, nil)
	}
}

// Reveal — единственная ручка, отдающая пароль.
//
// Только администратор инвентаризации, с записью в журнал.
// Cache-Control: no-store обязателен: без него секрет осядет в кэше браузера
// и в кэше любого прокси по дороге.
func (h *CredentialHandler) Reveal() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := helper.IDParam(r, "id")
		if err != nil {
			helper.WriteError(w, err)
			return
		}

		secret, err := h.creds.Reveal(r.Context(), id, helper.ClientIP(r))
		if err != nil {
			helper.WriteError(w, err)
			return
		}

		w.Header().Set("Cache-Control", "no-store, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		// Ответ не должен попасть в реферер и в историю посредников.
		w.Header().Set("Referrer-Policy", "no-referrer")
		helper.WriteJSON(w, http.StatusOK, map[string]any{"secret": secret})
	}
}

// ToRotate — что менять после увольнения.
func (h *CredentialHandler) ToRotate() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, err := helper.QueryInt64(r, "user_id")
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		rows, err := h.creds.ToRotate(r.Context(), userID)
		if err != nil {
			helper.WriteError(w, err)
			return
		}
		helper.WriteJSON(w, http.StatusOK, map[string]any{
			"credentials": dto.MapToRotate(rows),
		})
	}
}
