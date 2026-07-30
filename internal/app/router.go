package app

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"

	"go_warehouse_service/internal/middleware"
	"go_warehouse_service/internal/service"
)

const apiPrefix = "/spa/api/warehouse"

func setupRouter(h Handlers, authMw *middleware.AuthMiddleware, sync *service.SyncService) *chi.Mux {
	r := chi.NewRouter()

	r.Use(middleware.RequestLogger())
	r.Use(chiMiddleware.Recoverer)
	// Импорт разбирает файл на 2600 строк в одной транзакции — 15 секунд ему мало.
	r.Use(chiMiddleware.Timeout(120 * time.Second))

	// Публичная ручка.
	r.Get("/healthz", healthz(sync))

	r.Group(func(r chi.Router) {
		r.Use(authMw.Handler)

		r.Route(apiPrefix, func(r chi.Router) {
			r.Route("/items", func(r chi.Router) {
				r.Get("/", h.Item.List())
				r.Get("/{id}", h.Item.Get())
				r.Patch("/{id}", h.Item.Update())
			})

			r.Route("/holdings", func(r chi.Router) {
				r.Get("/", h.Holding.List())
				r.Post("/", h.Holding.Create())
				r.Patch("/{id}", h.Holding.Update())
				r.Delete("/{id}", h.Holding.Delete())
			})

			r.Get("/rooms", h.Holding.Rooms())

			r.Route("/warehouses", func(r chi.Router) {
				r.Get("/", h.Warehouse.List())
				r.Post("/", h.Warehouse.Create())
				r.Patch("/{id}", h.Warehouse.Update())
			})

			r.Post("/import", h.Import.Import())

			r.Route("/credentials", func(r chi.Router) {
				r.Get("/", h.Credential.List())
				r.Post("/", h.Credential.Create())
				// to-rotate объявлен ДО /{id}, иначе chi примет "to-rotate"
				// за идентификатор и вернёт validation_failed.
				r.Get("/to-rotate", h.Credential.ToRotate())
				r.Patch("/{id}", h.Credential.Update())
				r.Delete("/{id}", h.Credential.Delete())
				r.Post("/{id}/reveal", h.Credential.Reveal())
			})

			r.Route("/reports", func(r chi.Router) {
				r.Get("/losses", h.Report.Losses())
				r.Get("/losses.xlsx", h.Report.LossesXLSX())
				r.Get("/summary", h.Report.Summary())
			})
		})
	})

	return r
}

// healthz отдаёт возраст последней успешной синхронизации справочников.
//
// ⚠ Это не украшение. Три сценария отказа деградируют МОЛЧА: очередь встала —
// реплика отстаёт; Symfony недоступна — показываем старые названия; сервис
// лежал — очередь копит. Экран при этом не ломается и ничего не сообщает.
// Возраст синхронизации — единственный внешний признак, что что-то не так.
func healthz(sync *service.SyncService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		body := map[string]any{"status": "ok", "service": "warehouse"}

		if sync != nil {
			if age := sync.LastSyncAge(ctx); age != nil {
				body["lastSyncAgeSeconds"] = int64(age.Seconds())
				// Сутки без синхронизации — уже повод посмотреть на очередь.
				if *age > 24*time.Hour {
					body["status"] = "degraded"
					body["warning"] = "справочник пользователей не обновлялся более суток"
				}
			} else {
				// Реплика пуста: импорт не сможет связать сотрудников по ФИО.
				body["status"] = "degraded"
				body["warning"] = "реплика пользователей пуста — импорт не сматчит сотрудников"
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if body["status"] != "ok" {
			// 200 сохраняем намеренно: сервис РАБОТАЕТ, просто данные устарели.
			// 503 заставил бы оркестратор перезапускать здоровый контейнер.
			w.WriteHeader(http.StatusOK)
		}
		_ = json.NewEncoder(w).Encode(body)
	}
}
