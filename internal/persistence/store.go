package persistence

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"reflect"
	"releasecontrol/internal/domain"
	"sort"
	"strconv"
	"strings"
	"sync"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct {
	pool          *pgxpool.Pool
	mu            sync.Mutex
	cached        *stateCache
	journal       *journalCache
	viewRevision  int64
	viewState     *domain.State
	decodedEvents uint64
	writeJournal  *journalCache
}

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
	return &Store{pool: pool}, nil
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

// View borrows a validated snapshot only for the duration of fn. The callback
// must not mutate or retain any part of it, call this Store, or perform I/O.
// Read remains the detached, mutable snapshot API.
func (s *Store) View(ctx context.Context, fn func(*domain.State) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	s.mu.Lock()
	defer s.mu.Unlock()
	var revision int64
	if err = tx.QueryRow(ctx, "SELECT revision FROM control_revision WHERE id=1").Scan(&revision); err != nil {
		return err
	}
	if s.journal != nil && revision < s.journal.revision {
		s.journal = nil
		s.viewState = nil
	}
	_, err = s.refreshJournal(ctx, tx, revision)
	if err != nil {
		return err
	}
	if s.viewState == nil || s.viewRevision != revision || len(s.viewState.Events) != len(s.journal.events) {
		var st domain.State
		if s.cached != nil && s.cached.revision == revision {
			st = s.cached.state
		} else {
			positions := map[string]int{}
			st, err = loadRecords(ctx, tx, positions)
			if err != nil {
				return err
			}
			// Warm the write cache without copying record payloads. It owns immutable
			// decoded records; write callbacks detach all mutable collections.
			base := st
			base.Events = nil
			for _, event := range s.journal.events {
				if domain.IsWriteProvenanceEvent(event) {
					base.Events = append(base.Events, event)
				}
			}
			if domain.ValidateStateStatuses(base) == nil {
				s.cached = &stateCache{revision: revision, state: base, positions: positions}
			}
		}
		st.Events = s.journal.events
		// Records may change references to old events: retain complete validations
		// whenever the revision changes. Unchanged Views reuse their validated result.
		if err = domain.ValidateRegistryInventories(st); err != nil {
			return err
		}
		if err = domain.ValidateArtifactPins(st); err != nil {
			return err
		}
		s.viewState = &st
		s.viewRevision = revision
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	borrowed := *s.viewState
	return fn(&borrowed)
}
func (s *Store) Read(ctx context.Context) (domain.State, error) {
	var st domain.State
	err := s.View(ctx, func(snapshot *domain.State) error {
		st = cloneValue(reflect.ValueOf(*snapshot)).Interface().(domain.State)
		return nil
	})
	return st, err
}

// Events have no rewrite/delete path, including restore (empty journal only).
// Sequence gaps can arise from rolled-back inserts; rebuild on discovering one
// rather than assuming contiguous IDs. A revision regression also rebuilds.
type journalCache struct {
	revision, max int64
	events        []domain.Event
}

func (s *Store) refreshJournal(ctx context.Context, tx pgx.Tx, revision int64) (int64, error) {
	var max int64
	if err := tx.QueryRow(ctx, "SELECT COALESCE(max(sequence),0) FROM events").Scan(&max); err != nil {
		return 0, err
	}
	if s.journal == nil || max < s.journal.max {
		s.journal = &journalCache{events: []domain.Event{}}
		s.viewState = nil
	}
	old := s.journal
	if max == old.max {
		old.revision = revision
		return max, nil
	}
	fetch := func(after int64) ([]domain.Event, bool, error) {
		rows, err := tx.Query(ctx, "SELECT sequence, document FROM events WHERE sequence > $1 ORDER BY sequence", after)
		if err != nil {
			return nil, false, err
		}
		defer rows.Close()
		events := []domain.Event{}
		gap := false
		for rows.Next() {
			var seq int64
			var raw []byte
			if err = rows.Scan(&seq, &raw); err != nil {
				return nil, false, err
			}
			gap = gap || seq != after+1
			after = seq
			var event domain.Event
			if err = json.Unmarshal(raw, &event); err != nil {
				return nil, false, err
			}
			s.decodedEvents++
			events = append(events, event)
		}
		return events, gap, rows.Err()
	}
	added, gap, err := fetch(old.max)
	if err != nil {
		return 0, err
	}
	if gap && old.max != 0 {
		added, _, err = fetch(0)
		if err != nil {
			return 0, err
		}
		old = &journalCache{events: []domain.Event{}}
		s.viewState = nil
	}
	s.journal = &journalCache{revision: revision, max: max, events: append(old.events, added...)}
	return max, nil
}
func readRecords(ctx context.Context, tx pgx.Tx) (domain.State, error) {
	return loadRecords(ctx, tx, nil)
}
func loadRecords(ctx context.Context, tx pgx.Tx, positions map[string]int) (domain.State, error) {
	st := domain.EmptyState()
	groups := map[string][]json.RawMessage{}
	rows, e := tx.Query(ctx, "SELECT kind, document, id, position FROM records ORDER BY kind,position,id")
	if e != nil {
		return st, e
	}
	for rows.Next() {
		var kind string
		var doc []byte
		var id string
		var position int
		if e = rows.Scan(&kind, &doc, &id, &position); e != nil {
			rows.Close()
			return st, e
		}
		if positions != nil {
			positions[id] = position
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
	return st, nil
}
func read(ctx context.Context, tx pgx.Tx) (domain.State, error) {
	st, e := readRecords(ctx, tx)
	if e != nil {
		return st, e
	}
	if e = readJournal(ctx, tx, &st); e != nil {
		return st, e
	}
	return st, nil
}
func readJournal(ctx context.Context, tx pgx.Tx, st *domain.State) error {
	rows, e := tx.Query(ctx, "SELECT document FROM events ORDER BY sequence")
	if e != nil {
		return e
	}
	defer rows.Close()
	for rows.Next() {
		var b []byte
		if e = rows.Scan(&b); e != nil {
			return e
		}
		var event domain.Event
		if e = json.Unmarshal(b, &event); e != nil {
			return e
		}
		st.Events = append(st.Events, event)
	}
	if e = rows.Err(); e != nil {
		return e
	}
	if e = domain.ValidateRegistryInventories(*st); e != nil {
		return e
	}
	if e = domain.ValidateArtifactPins(*st); e != nil {
		return e
	}
	return nil
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
