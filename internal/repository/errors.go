package repository

import (
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"go_warehouse_service/internal/apperr"
)

// NormalizeError переводит ошибки драйвера в доменные.
func NormalizeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.ErrNotFound
	}
	return err
}

// NotFoundAs переводит «нет строк» в конкретный код сущности.
func NotFoundAs(err error, code apperr.ErrorCode) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.New(code, string(code))
	}
	return err
}

// IsUniqueViolation сообщает о нарушении уникальности (код 23505).
// Нужен, чтобы двойной ввод инвентарного номера при обходе вдвоём
// превращался в понятное 409, а не в 500.
func IsUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return false
	}
	return constraint == "" || pgErr.ConstraintName == constraint
}

// IsCheckViolation сообщает о нарушении CHECK-констрейнта (23514).
func IsCheckViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		return false
	}
	return constraint == "" || pgErr.ConstraintName == constraint
}

// IsForeignKeyViolation сообщает о нарушении внешнего ключа (23503).
// Например попытка удалить склад, на котором ещё числятся остатки.
func IsForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
