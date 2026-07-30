package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ExecTx выполняет fn в транзакции: Rollback гарантирован при ошибке,
// панике или если Commit не был вызван.
//
// Импорту транзакция обязательна: файл на 1774 строки должен либо примениться
// целиком, либо не примениться вовсе — иначе повторная загрузка после сбоя
// доложит остатки поверх уже вставленных.
func ExecTx(ctx context.Context, db *pgxpool.Pool, fn func(pgx.Tx) error) (err error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	committed := false
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p) // повторная паника уже после отката
		}
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	if err = fn(tx); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	committed = true
	return nil
}
