package usersync

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestIsPermanent фиксирует правило, которое уже роняло сервис-образец:
// отравленное сообщение нельзя возвращать в очередь. Битый JSON и нарушение
// констрейнта повтором не лечатся — requeue зациклит consumer.
func TestIsPermanent(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"битый JSON", fmt.Errorf("%w: decode json", errInvalidMessage), true},
		{"пустой id", fmt.Errorf("%w: user id is empty", errInvalidMessage), true},
		{"22001 слишком длинное значение", &pgconn.PgError{Code: "22001"}, true},
		{"22P02 некорректный текст", &pgconn.PgError{Code: "22P02"}, true},
		{"23505 нарушение уникальности", &pgconn.PgError{Code: "23505"}, true},
		{"23502 not-null", &pgconn.PgError{Code: "23502"}, true},
		{"обёрнутая PG-ошибка", fmt.Errorf("upsert: %w", &pgconn.PgError{Code: "23514"}), true},

		// Временные — повтор осмыслен.
		{"08006 обрыв соединения", &pgconn.PgError{Code: "08006"}, false},
		{"40001 сериализация", &pgconn.PgError{Code: "40001"}, false},
		{"53300 нет слотов", &pgconn.PgError{Code: "53300"}, false},
		{"обычная ошибка", errors.New("connection refused"), false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isPermanent(c.err); got != c.want {
				t.Errorf("isPermanent(%v) = %v, ожидалось %v", c.err, got, c.want)
			}
		})
	}
}

func TestNormalizeEvent(t *testing.T) {
	cases := []struct {
		event, routingKey, want string
		wantErr                 bool
	}{
		{"user.upserted", "", "upserted", false},
		{"upserted", "", "upserted", false},
		{"", "user.upserted", "upserted", false},
		{"", "user.deleted", "deleted", false},
		{"user.deleted", "user.upserted", "deleted", false}, // тело важнее ключа
		{"", "", "", true},
		{"user.exploded", "", "", true},
	}
	for _, c := range cases {
		got, err := normalizeEvent(c.event, c.routingKey)
		if (err != nil) != c.wantErr {
			t.Errorf("normalizeEvent(%q,%q) err=%v, ожидалась ошибка=%v",
				c.event, c.routingKey, err, c.wantErr)
			continue
		}
		if got != c.want {
			t.Errorf("normalizeEvent(%q,%q) = %q, ожидалось %q", c.event, c.routingKey, got, c.want)
		}
	}
	// Неизвестное событие обязано считаться постоянной ошибкой,
	// иначе оно будет крутиться в очереди вечно.
	_, err := normalizeEvent("user.exploded", "")
	if !isPermanent(err) {
		t.Error("неизвестное событие должно быть постоянной ошибкой")
	}
}

func TestPayloadAcceptsBothNamingStyles(t *testing.T) {
	p, err := decodePayload([]byte(`{"event":"user.upserted","userId":42,"login":"ivanov",
		"lastname":"Иванов","firstname":"Иван","patronymic":"Иванович"}`))
	if err != nil {
		t.Fatalf("decodePayload: %v", err)
	}
	if p.userID() != 42 {
		t.Errorf("userId camelCase = %d", p.userID())
	}

	p2, err := decodePayload([]byte(`{"event":"user.deleted","user_id":7,
		"login":"petrov","deleted_at":"2026-07-29T10:00:00+00:00"}`))
	if err != nil {
		t.Fatalf("decodePayload snake_case: %v", err)
	}
	if p2.userID() != 7 {
		t.Errorf("user_id snake_case = %d", p2.userID())
	}
	if p2.deletedAt() == nil {
		t.Error("deleted_at не разобрана")
	}
}

func TestUserRequiresLogin(t *testing.T) {
	p, _ := decodePayload([]byte(`{"event":"user.upserted","userId":1,"login":"  "}`))
	if _, err := p.user(); !errors.Is(err, errInvalidMessage) {
		t.Errorf("пустой login должен давать errInvalidMessage, получено %v", err)
	}
	// А такое сообщение обязано уйти из очереди, а не повторяться вечно.
	_, err := p.user()
	if !isPermanent(err) {
		t.Error("сообщение без login должно быть постоянной ошибкой")
	}
}
