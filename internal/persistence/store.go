package persistence

import (
	"bytes"
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
	st, e = decodeStoredDocuments(groups)
	if e != nil {
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
	if e = rows.Err(); e != nil {
		return st, e
	}
	if e = domain.ValidateRegistryInventories(st); e != nil {
		return st, e
	}
	if e = domain.ValidateArtifactPins(st); e != nil {
		return st, e
	}
	return st, nil
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
	if e = domain.ValidateStateStatuses(st); e != nil {
		return e
	}
	after, e := documents(st)
	if e != nil {
		return e
	}
	beforeIndex, e := indexDocuments(before)
	if e != nil {
		return e
	}
	afterIndex, e := indexDocuments(after)
	if e != nil {
		return e
	}
	for kind, records := range before {
		var deletedIDs []string
		for _, raw := range records {
			if beforeIndex.productRecordRemoved(afterIndex, kind, raw) {
				var meta struct {
					ID string `json:"id"`
				}
				if e = json.Unmarshal(raw, &meta); e != nil {
					return e
				}
				deletedIDs = append(deletedIDs, meta.ID)
			}
		}
		// Preserve the exact product-deletion predicate while avoiding one SQL
		// round trip per retained historical record. The transaction still owns
		// the revision lock and rolls back every deletion on any later error.
		if len(deletedIDs) > 0 {
			if _, e = tx.Exec(ctx, "DELETE FROM records WHERE id=ANY($1::text[]) AND kind=$2", deletedIDs, kind); e != nil {
				return e
			}
		}
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
			if beforeIndex.unchanged(kind, obj.ID, r) {
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
func documents(st domain.State) (map[string][]json.RawMessage, error) { return codecDocuments(st) }

// Only a deleted product permits deletion of its owned records.
func productRecordRemoved(before, after map[string][]json.RawMessage, kind string, raw json.RawMessage) bool {
	var m struct {
		ID        string `json:"id"`
		ProductID string `json:"product_id"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	owner := m.ProductID
	if kind == "products" {
		owner = m.ID
	}
	if owner == "" {
		return false
	}
	exists := func(rows []json.RawMessage, id string) bool {
		for _, r := range rows {
			var v struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(r, &v)
			if v.ID == id {
				return true
			}
		}
		return false
	}
	for _, rows := range after {
		for _, row := range rows {
			var v struct {
				ProductID string `json:"product_id"`
			}
			_ = json.Unmarshal(row, &v)
			if v.ProductID == owner {
				return false
			}
		}
	}
	return exists(before["products"], owner) && !exists(after["products"], owner) && !exists(after[kind], m.ID)
}

// Index once per transaction rather than scanning the complete historical state
// for every row. Documents remain byte-exact: reordered JSON or reordered rows
// retain the previous update/position behavior. Raw messages are borrowed, not
// copied into strings, to keep large retained histories within the memory budget.
type documentIndex struct {
	byKind map[string]map[string][]json.RawMessage
	owners map[string]bool
}

func indexDocuments(groups map[string][]json.RawMessage) (documentIndex, error) {
	index := documentIndex{byKind: make(map[string]map[string][]json.RawMessage, len(groups)), owners: map[string]bool{}}
	for kind, rows := range groups {
		indexed := make(map[string][]json.RawMessage, len(rows))
		for _, raw := range rows {
			var meta struct {
				ID        string `json:"id"`
				ProductID string `json:"product_id"`
			}
			if err := json.Unmarshal(raw, &meta); err != nil {
				return documentIndex{}, err
			}
			indexed[meta.ID] = append(indexed[meta.ID], raw)
			index.owners[meta.ProductID] = true
		}
		index.byKind[kind] = indexed
	}
	return index, nil
}
func (index documentIndex) unchanged(kind, id string, raw json.RawMessage) bool {
	// A list also preserves exact legacy behavior for repeated IDs in callback
	// output; SQL constraints and application validation still own ID policy.
	for _, old := range index.byKind[kind][id] {
		if bytes.Equal(old, raw) {
			return true
		}
	}
	return false
}
func (index documentIndex) productRecordRemoved(after documentIndex, kind string, raw json.RawMessage) bool {
	var meta struct {
		ID        string `json:"id"`
		ProductID string `json:"product_id"`
	}
	if json.Unmarshal(raw, &meta) != nil {
		return false
	}
	owner := meta.ProductID
	if kind == "products" {
		owner = meta.ID
	}
	return owner != "" && !after.owners[owner] && len(index.byKind["products"][owner]) > 0 && len(after.byKind["products"][owner]) == 0 && len(after.byKind[kind][meta.ID]) == 0
}
