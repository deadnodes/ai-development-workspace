package persistence

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"reflect"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/sqlitefile"
)

//go:embed sqlite_migrations/*.sql
var sqliteMigrations embed.FS

type SQLiteStore struct {
	db *sql.DB
	stateSnapshots
}

func OpenSQLite(ctx context.Context, path string) (*SQLiteStore, error) {
	db, err := sqlitefile.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	store := &SQLiteStore{db: db}
	if err := store.migrate(ctx); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

func (s *SQLiteStore) Close() { _ = s.db.Close() }

func OpenSQLiteReadOnly(ctx context.Context, path string) (*SQLiteStore, error) {
	db, err := sqlitefile.OpenReadOnly(ctx, path)
	if err != nil {
		return nil, err
	}
	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) begin(ctx context.Context, readOnly bool) (sqliteTransaction, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: readOnly})
	return sqliteTransaction{tx}, err
}

func (s *SQLiteStore) migrate(ctx context.Context) error {
	tx, err := s.begin(ctx, false)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var version int
	if err := tx.QueryRow(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	names, err := fs.Glob(sqliteMigrations, "sqlite_migrations/*.sql")
	if err != nil {
		return err
	}
	if version > len(names) {
		return fmt.Errorf("SQLite schema version %d is newer than supported version %d", version, len(names))
	}
	for _, name := range names[version:] {
		body, err := sqliteMigrations.ReadFile(name)
		if err != nil {
			return err
		}
		if err := tx.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
		version++
		if err := tx.Exec(ctx, fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *SQLiteStore) View(ctx context.Context, fn func(*domain.State) error) error {
	tx, err := s.begin(ctx, true)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	return s.stateSnapshots.view(ctx, tx, fn)
}

func (s *SQLiteStore) Read(ctx context.Context) (domain.State, error) {
	var out domain.State
	err := s.View(ctx, func(st *domain.State) error {
		out = cloneValue(reflect.ValueOf(*st)).Interface().(domain.State)
		return nil
	})
	return out, err
}

func (s *SQLiteStore) Update(ctx context.Context, fn func(*domain.State) error) error {
	return s.update(ctx, nil, fn)
}

func (s *SQLiteStore) UpdateScoped(ctx context.Context, scope domain.StateMutationScope, fn func(*domain.State) error) error {
	return s.update(ctx, &scope, fn)
}

func (s *SQLiteStore) update(ctx context.Context, scope *domain.StateMutationScope, fn func(*domain.State) error) error {
	tx, err := s.begin(ctx, false)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	return s.stateSnapshots.updateCollections(ctx, tx, scope, fn)
}
