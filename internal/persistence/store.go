package persistence

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"releasecontrol/internal/domain"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		pool.Close()
		return nil, err
	}
	committed := false
	defer func() {
		// Roll back before closing the pool; Close waits for acquired connections.
		_ = tx.Rollback(context.Background())
		if !committed {
			pool.Close()
		}
	}()
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(793814023)"); err != nil {
		return nil, err
	}
	if err = migrate(ctx, tx); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	committed = true
	return &Store{pool}, nil
}

func migrate(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations(version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())"); err != nil {
		return err
	}
	files, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	type migration struct {
		version int
		name    string
	}
	pending := make([]migration, 0, len(files))
	seen := map[int]bool{}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(f.Name(), "_")
		version, err := strconv.Atoi(prefix)
		if !ok || err != nil || version <= 0 || seen[version] {
			return fmt.Errorf("invalid or duplicate migration version: %s", f.Name())
		}
		seen[version] = true
		pending = append(pending, migration{version, f.Name()})
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].version < pending[j].version })
	for _, m := range pending {
		var applied bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)", m.version).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + m.name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", m.name, err)
		}
		if _, err = tx.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES($1)", m.version); err != nil {
			return fmt.Errorf("record migration %s: %w", m.name, err)
		}
	}
	return nil
}
func (s *Store) Close() { s.pool.Close() }
func (s *Store) Read(ctx context.Context) (domain.State, error) {
	tx, e := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return domain.State{}, e
	}
	defer tx.Rollback(ctx)
	st, e := read(ctx, tx)
	if e != nil {
		return st, e
	}
	return st, tx.Commit(ctx)
}
func read(ctx context.Context, tx pgx.Tx) (domain.State, error) {
	st := domain.EmptyState()
	groups := map[string][]json.RawMessage{}
	rows, e := tx.Query(ctx, "SELECT kind, document FROM records ORDER BY kind,position,id")
	if e != nil {
		return st, e
	}
	for rows.Next() {
		var kind string
		var doc []byte
		if e = rows.Scan(&kind, &doc); e != nil {
			rows.Close()
			return st, e
		}
		groups[kind] = append(groups[kind], json.RawMessage(doc))
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return st, e
	}
	b, e := json.Marshal(groups)
	if e != nil {
		return st, e
	}
	if e = json.Unmarshal(b, &st); e != nil {
		return st, e
	}
	rows, e = tx.Query(ctx, "SELECT document FROM events ORDER BY sequence")
	if e != nil {
		return st, e
	}
	defer rows.Close()
	for rows.Next() {
		var b []byte
		if e = rows.Scan(&b); e != nil {
			return st, e
		}
		var event domain.Event
		if e = json.Unmarshal(b, &event); e != nil {
			return st, e
		}
		st.Events = append(st.Events, event)
	}
	return st, rows.Err()
}
func (s *Store) Update(ctx context.Context, fn func(*domain.State) error) error {
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT revision FROM control_revision WHERE id=1 FOR UPDATE"); e != nil {
		return e
	}
	st, e := read(ctx, tx)
	if e != nil {
		return e
	}
	oldEvents := len(st.Events)
	before, e := documents(st)
	if e != nil {
		return e
	}
	if e = fn(&st); e != nil {
		return e
	}
	after, e := documents(st)
	if e != nil {
		return e
	}
	for kind, records := range after {
		for position, r := range records {
			var obj struct {
				ID        string `json:"id"`
				ProductID string `json:"product_id"`
				FeatureID string `json:"feature_id"`
			}
			if e = json.Unmarshal(r, &obj); e != nil {
				return e
			}
			unchanged := false
			for _, old := range before[kind] {
				if string(old) == string(r) {
					unchanged = true
					break
				}
			}
			if unchanged {
				continue
			}
			if _, e = tx.Exec(ctx, "INSERT INTO records(id,kind,product_id,feature_id,document,position) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(id) DO UPDATE SET document=EXCLUDED.document,product_id=EXCLUDED.product_id,feature_id=EXCLUDED.feature_id,position=EXCLUDED.position", obj.ID, kind, obj.ProductID, obj.FeatureID, []byte(r), position); e != nil {
				return e
			}
		}
	}
	for _, v := range st.Events[oldEvents:] {
		b, _ := json.Marshal(v)
		if _, e = tx.Exec(ctx, "INSERT INTO events(id,product_id,feature_id,document) VALUES($1,$2,$3,$4)", v.ID, v.ProductID, v.FeatureID, b); e != nil {
			return e
		}
	}
	if _, e = tx.Exec(ctx, "UPDATE control_revision SET revision=revision+1 WHERE id=1"); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func documents(st domain.State) (map[string][]json.RawMessage, error) {
	b, e := json.Marshal(st)
	if e != nil {
		return nil, e
	}
	var m map[string][]json.RawMessage
	e = json.Unmarshal(b, &m)
	delete(m, "events")
	return m, e
}
