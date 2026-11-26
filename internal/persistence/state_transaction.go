package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

type stateRow interface{ Scan(...any) error }

type stateRows interface {
	stateRow
	Next() bool
	Err() error
	Close()
}

type stateBatch interface {
	exec(string, ...any) error
	flush() error
}

type stateTransaction interface {
	Query(context.Context, string, ...any) (stateRows, error)
	QueryRow(context.Context, string, ...any) stateRow
	Exec(context.Context, string, ...any) error
	Commit(context.Context) error
	sql(string, string) string
	batch(context.Context) stateBatch
}

type postgresTransaction struct{ pgx.Tx }

func (tx postgresTransaction) Query(ctx context.Context, query string, args ...any) (stateRows, error) {
	return tx.Tx.Query(ctx, query, args...)
}

func (tx postgresTransaction) QueryRow(ctx context.Context, query string, args ...any) stateRow {
	return tx.Tx.QueryRow(ctx, query, args...)
}

func (tx postgresTransaction) Exec(ctx context.Context, query string, args ...any) error {
	_, err := tx.Tx.Exec(ctx, query, args...)
	return err
}

func (postgresTransaction) sql(postgres, _ string) string { return postgres }

func (tx postgresTransaction) batch(ctx context.Context) stateBatch {
	return &postgresBatch{tx: tx.Tx, ctx: ctx}
}

type postgresBatch struct {
	tx     pgx.Tx
	ctx    context.Context
	queued pgx.Batch
}

func (batch *postgresBatch) exec(query string, args ...any) error {
	batch.queued.Queue(query, args...)
	if batch.queued.Len() >= 256 {
		return batch.flush()
	}
	return nil
}

func (batch *postgresBatch) flush() error {
	if batch.queued.Len() == 0 {
		return nil
	}
	err := batch.tx.SendBatch(batch.ctx, &batch.queued).Close()
	batch.queued = pgx.Batch{}
	return err
}

type sqliteTransaction struct{ *sql.Tx }
type sqliteRows struct{ *sql.Rows }

func (rows sqliteRows) Close() { _ = rows.Rows.Close() }

func sqliteArgs(args []any) []any {
	for index, arg := range args {
		switch value := arg.(type) {
		case []string:
			if value == nil {
				value = []string{}
			}
			encoded, _ := json.Marshal(value)
			args[index] = string(encoded)
		case time.Time:
			args[index] = value.UTC().Format("2006-01-02 15:04:05.000000000Z07:00")
		case json.RawMessage:
			args[index] = string(value)
		}
	}
	return args
}

func (tx sqliteTransaction) Query(ctx context.Context, query string, args ...any) (stateRows, error) {
	rows, err := tx.QueryContext(ctx, query, sqliteArgs(args)...)
	if err != nil {
		return nil, err
	}
	return sqliteRows{rows}, nil
}

func (tx sqliteTransaction) QueryRow(ctx context.Context, query string, args ...any) stateRow {
	return tx.QueryRowContext(ctx, query, sqliteArgs(args)...)
}

func (tx sqliteTransaction) Exec(ctx context.Context, query string, args ...any) error {
	_, err := tx.ExecContext(ctx, query, sqliteArgs(args)...)
	return err
}

func (tx sqliteTransaction) Commit(context.Context) error   { return tx.Tx.Commit() }
func (tx sqliteTransaction) Rollback(context.Context) error { return tx.Tx.Rollback() }
func (sqliteTransaction) sql(_, sqlite string) string       { return sqlite }

func (tx sqliteTransaction) batch(ctx context.Context) stateBatch {
	return sqliteBatch{tx: tx, ctx: ctx}
}

type sqliteBatch struct {
	tx  sqliteTransaction
	ctx context.Context
}

func (batch sqliteBatch) exec(query string, args ...any) error {
	return batch.tx.Exec(batch.ctx, query, args...)
}

func (sqliteBatch) flush() error { return nil }
