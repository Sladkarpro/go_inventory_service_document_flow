package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"go_warehouse_service/internal/helper"
)

// RequestLogger возвращает middleware, который логирует каждый HTTP-запрос через slog.
//
// ⚠ В ЛОГЕ АДРЕС КЛИЕНТА, А НЕ АДРЕС NGINX. Раньше писался r.RemoteAddr —
// за обратным прокси это TCP-пир, то есть контейнер nginx, у всех запросов
// одинаковый и с портом. Единственный след разрушающих запросов (DELETE строк,
// PATCH факта) не позволял понять, откуда они пришли. helper.ClientIP берёт
// адрес так, чтобы его нельзя было подменить заголовком (разбор — там же).
//
// ⚠ ПОЛЬЗОВАТЕЛЯ ЗДЕСЬ НЕТ, и это ограничение слоя, а не недоделка.
// Логгер навешен глобально (нужен и для публичного /healthz), то есть ОБОРАЧИВАЕТ
// проверку токена. Прочитать пользователя «после next» тоже нельзя: auth кладёт
// его в производный запрос — `next.ServeHTTP(w, r.WithContext(ctx))`
// (internal/middleware/auth.go:140) — а здесь остаётся исходный r, у которого
// в контексте пользователя нет и не появится.
//
// Кто именно менял данные, восстанавливается не по этому логу, а по базе:
// counted_by_id и resolved_by_id на строке, wh_credential_view_log на просмотрах
// паролей. Если понадобится пользователь и в логе — писать его должен слой auth,
// у которого он на руках, а не этот.
func RequestLogger() func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(ww, r)

			ip := "неизвестен"
			if addr := helper.ClientIP(r); addr != nil {
				ip = *addr
			}

			slog.Info("http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Duration("duration", time.Since(start)),
				slog.String("ip", ip),
			)
		})
	}
}
