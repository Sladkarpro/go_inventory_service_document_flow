package helper

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"go_warehouse_service/internal/apperr"
)

// IDParam читает числовой параметр пути (например {id}) и возвращает его как int64.
func IDParam(r *http.Request, name string) (int64, error) {
	raw := chi.URLParam(r, name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%w: invalid %s", apperr.ErrValidation, name)
	}
	return id, nil
}

// QueryInt64 читает необязательный числовой query-параметр.
func QueryInt64(r *http.Request, name string) (*int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return nil, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid %s", apperr.ErrValidation, name)
	}
	return &v, nil
}

// QueryBool читает флаг вида ?only_losses=1 / true / yes.
func QueryBool(r *http.Request, name string) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// QueryString читает необязательный строковый параметр, пустой → nil.
func QueryString(r *http.Request, name string) *string {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return nil
	}
	return &raw
}

// Pagination читает limit/offset с безопасными границами.
func Pagination(r *http.Request, defLimit, maxLimit int) (limit, offset int) {
	limit, offset = defLimit, 0
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v > 0 {
		offset = v
	}
	return limit, offset
}

// ClientIP достаёт адрес клиента для журнала просмотров паролей.
//
// ⚠ ЭТОТ АДРЕС — ЧАСТЬ АУДИТА, а не удобство. Журнал просмотров паролей
// (wh_credential_view_log) — единственный след того, кто видел пароль от техники,
// и он сохраняется даже при удалении доступа. Подставной адрес в нём означает,
// что аудит указывает не на того человека.
//
// ⚠ ПОЧЕМУ БЕРЁТСЯ ПОСЛЕДНИЙ ЭЛЕМЕНТ X-Forwarded-For, А НЕ ПЕРВЫЙ.
// Прежняя версия брала ПЕРВЫЙ — и его подставлял сам клиент. nginx фронта
// настроен так:
//
//	proxy_set_header X-Real-IP        $remote_addr;
//	proxy_set_header X-Forwarded-For  $proxy_add_x_forwarded_for;
//
// `$proxy_add_x_forwarded_for` ДОПИСЫВАЕТ настоящий адрес в КОНЕЦ к тому, что
// прислал клиент, и присланное не перезаписывает. Значит администратор, смотрящий
// пароль, мог послать `X-Forwarded-For: 10.0.0.99` — и в журнал попадал бы
// именно этот адрес, а настоящий оставался бы вторым элементом.
// Последний элемент подставить нельзя: его пишет nginx.
//
// ⚠ ЭТО ВЕРНО РОВНО ДЛЯ ОДНОГО доверенного прокси. Появится второй (внешний
// балансировщик перед nginx) — последним станет адрес nginx, и здесь понадобится
// брать предпоследний либо считать доверенные хопы явно. Другого способа нет:
// сколько звеньев доверять — знание об окружении, из запроса оно не выводится.
//
// X-Real-IP проверяется ПЕРВЫМ, потому что nginx перезаписывает его безусловно:
// клиентское значение до сервиса не доезжает.
func ClientIP(r *http.Request) *string {
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(real) != nil {
		return &real
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		last := strings.TrimSpace(parts[len(parts)-1])
		if net.ParseIP(last) != nil {
			return &last
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return nil
	}
	if net.ParseIP(host) == nil {
		return nil
	}
	return &host
}
