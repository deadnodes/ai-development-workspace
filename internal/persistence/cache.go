package persistence

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"sync"

	"github.com/jackc/pgx/v5"
	"releasecontrol/internal/domain"
)

// The committed cache is borrowed only by read-only View callbacks. A revision read under the
// database writer lock is the authority even when several processes share a DB.
type stateCache struct {
	revision  int64
	state     domain.State
	positions map[string]int
}

// cloneValue isolates nested maps, pointers and slices, including opaque provider
// payloads. Struct values (notably time.Time) retain their unexported fields.
func cloneValue(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		if !mutableType(v.Type().Elem()) {
			reflect.Copy(out, v)
			return out
		}
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(cloneValue(v.Index(i)))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		it := v.MapRange()
		for it.Next() {
			out.SetMapIndex(it.Key(), cloneValue(it.Value()))
		}
		return out
	case reflect.Pointer:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(cloneValue(v.Elem()))
		return out
	case reflect.Interface:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(cloneValue(v.Elem()))
		return out

	case reflect.Array:
		if !mutableType(v.Type().Elem()) {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(cloneValue(v.Index(i)))
		}
		return out
	case reflect.Struct:
		fields := cloneFields(v.Type())
		if len(fields) == 0 {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for _, i := range fields {
			out.Field(i).Set(cloneValue(v.Field(i)))
		}
		return out

	default:
		return v
	}
}

// Update supplies committed records and a narrow write-provenance event index.
// Callbacks append new events; full history is available through Read outside
// the write lock. A record reset must preserve the supplied Events slice.
func (s *Store) Update(ctx context.Context, fn func(*domain.State) error) error {
	return s.updateCollections(ctx, nil, fn)
}

// UpdateScoped is an internal application contract: only the named
// collections may be mutated. All other collections are borrowed read-only.
// Unknown callbacks must use Update, which isolates the complete state.
func (s *Store) UpdateScoped(ctx context.Context, scope domain.StateMutationScope, fn func(*domain.State) error) error {
	return s.updateCollections(ctx, &scope, fn)
}

func (s *Store) updateCollections(ctx context.Context, scope *domain.StateMutationScope, fn func(*domain.State) error) error {
	full := scope == nil || scope.AllCollections
	// Always acquire a pool connection before the cache mutex, matching Read.
	// Holding the mutex while waiting for the pool can deadlock with a reader.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	s.mu.Lock()
	defer s.mu.Unlock()
	var revision int64
	if err = tx.QueryRow(ctx, "SELECT revision FROM control_revision WHERE id=1 FOR UPDATE").Scan(&revision); err != nil {
		return err
	}
	if scope != nil && scope.RequireEmptyJournal {
		var exists bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM events)").Scan(&exists); err != nil {
			return err
		}
		if exists {
			return domain.ErrJournalNotEmpty
		}
	}
	if s.cached == nil || s.cached.revision != revision {
		positions := map[string]int{}
		st, e := loadRecords(ctx, tx, positions)
		if e != nil {
			return e
		}
		// Only provenance consulted by domain/application write logic belongs in the
		// write projection. UI/history/export use the separate full journal reader.
		if e = s.readCachedWriteEvents(ctx, tx, &st, revision); e != nil {
			return e
		}
		if e = domain.ValidateStateStatuses(st); e != nil {
			return e
		}
		s.cached = &stateCache{revision: revision, state: st, positions: positions}
	}
	before := s.cached.state
	st := before
	if scope != nil && len(scope.EventReferences) > 0 {
		st.Events, err = referenceEvents(ctx, tx, scope.EventReferences, before.Events)
		if err != nil {
			return err
		}
	}
	writeEvents := st.Events
	selected := map[string]bool{"events": true}
	appendOnly := map[string]bool{}
	if scope != nil {
		for _, name := range scope.MutableCollections {
			selected[name] = true
		}
		for _, name := range scope.AppendCollections {
			selected[name] = true
			appendOnly[name] = true
		}
	}
	dst := reflect.ValueOf(&st).Elem()
	src := reflect.ValueOf(before)
	typ := src.Type()
	for i := 0; i < src.NumField(); i++ {
		if full || selected[typ.Field(i).Tag.Get("json")] {
			if appendOnly[typ.Field(i).Tag.Get("json")] {
				shallow := reflect.MakeSlice(src.Field(i).Type(), src.Field(i).Len(), src.Field(i).Len())
				reflect.Copy(shallow, src.Field(i))
				dst.Field(i).Set(shallow)
			} else {
				if typ.Field(i).Name == "Events" {
					dst.Field(i).Set(cloneValue(reflect.ValueOf(writeEvents)))
				} else {
					dst.Field(i).Set(cloneValue(src.Field(i)))
				}
			}
		}
	}
	oldEvents := len(st.Events)
	if err = fn(&st); err != nil {
		return err
	}
	if len(st.Events) < oldEvents || oldEvents > 0 && !reflect.DeepEqual(st.Events[:oldEvents], writeEvents) {
		return fmt.Errorf("events are append-only")
	}

	if scope != nil && len(scope.RequireAbsentIDs) > 0 {
		var exists bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM records WHERE id=ANY($1::text[]) AND NOT $2 UNION ALL SELECT 1 FROM events e WHERE id=ANY($1::text[]) AND (NOT $2 OR NOT EXISTS(SELECT 1 FROM records r WHERE r.id=e.id)))", scope.RequireAbsentIDs, scope.AllowExistingRecords).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return domain.ErrIdentityExists
		}
	}

	reserved := []string{}
	for _, p := range st.ArtifactPins {
		reserved = append(reserved, p.ID)
	}
	for _, v := range st.RegistryInventories {
		reserved = append(reserved, v.ID)
	}
	if len(reserved) > 0 {
		var collision bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM events WHERE id=ANY($1::text[]))", reserved).Scan(&collision); err != nil {
			return err
		}
		if collision {
			return fmt.Errorf("invalid reserved record/event identity collision")
		}
	}
	if full {
		if err = domain.ValidateStateStatuses(st); err != nil {
			return err
		}
	}
	// Detect changes on typed values; serialize only changed/new documents.
	// Retained omissions and unchanged positions follow the original SQL contract.
	type change struct {
		kind, id, product, feature string
		raw                        []byte
		position                   int
	}
	changes := []change{}
	delta := domain.EmptyState()
	dv := reflect.ValueOf(&delta).Elem()
	next := st
	nv := reflect.ValueOf(&next).Elem()
	afterOwners := map[string]bool{}
	afterIDs := map[string]map[string]bool{}
	productsBefore := map[string]bool{}
	productsAfter := map[string]bool{}
	for _, p := range before.Products {
		productsBefore[p.ID] = true
	}
	for _, p := range st.Products {
		productsAfter[p.ID] = true
	}
	deleting := false
	for id := range productsBefore {
		deleting = deleting || !productsAfter[id]
	}
	if deleting {
		for i := 0; i < dst.NumField(); i++ {
			kind := typ.Field(i).Tag.Get("json")
			if kind == "events" {
				continue
			}
			afterIDs[kind] = map[string]bool{}
			for j := 0; j < dst.Field(i).Len(); j++ {
				v := dst.Field(i).Index(j)
				afterIDs[kind][recordString(v, "ID")] = true
				afterOwners[recordString(v, "ProductID")] = true
			}
		}
	}
	for i := 0; i < src.NumField(); i++ {
		kind := typ.Field(i).Tag.Get("json")
		if kind == "events" || !full && !selected[kind] {
			continue
		}
		old, cur := src.Field(i), dst.Field(i)
		oldByID := map[string]reflect.Value{}
		positions := map[string]int{}
		for j := 0; j < old.Len(); j++ {
			id := recordString(old.Index(j), "ID")
			oldByID[id] = old.Index(j)
			positions[id] = s.cached.positions[id]
		}
		values := map[string]reflect.Value{}
		deleted := []string{}
		for j := 0; j < old.Len(); j++ {
			v := old.Index(j)
			id := recordString(v, "ID")
			owner := recordString(v, "ProductID")
			if kind == "products" {
				owner = id
			}
			if deleting && owner != "" && productsBefore[owner] && !productsAfter[owner] && !afterOwners[owner] && !afterIDs[kind][id] {
				deleted = append(deleted, id)
			} else {
				values[id] = v
			}
		}
		if len(deleted) > 0 {
			if _, err = tx.Exec(ctx, "DELETE FROM records WHERE id=ANY($1::text[]) AND kind=$2", deleted, kind); err != nil {
				return err
			}
		}
		for j := 0; j < cur.Len(); j++ {
			v := cur.Index(j)
			id := recordString(v, "ID")
			prev, ok := oldByID[id]
			if ok && (appendOnly[kind] || reflect.DeepEqual(prev.Addr().Interface(), v.Addr().Interface())) {
				continue
			}
			raw, e := json.Marshal(v.Addr().Interface())
			if e != nil {
				return e
			}
			if ok {
				oldRaw, e := json.Marshal(prev.Addr().Interface())
				if e != nil {
					return e
				}
				if bytes.Equal(oldRaw, raw) {
					continue
				}
			}
			// Cache the persisted representation, including JSON normalization of
			// opaque payload numbers. Never expose callback-owned maps/slices.
			persisted := reflect.New(v.Type())
			if e = json.Unmarshal(raw, persisted.Interface()); e != nil {
				return e
			}
			v = persisted.Elem()
			dv.Field(i).Set(reflect.Append(dv.Field(i), v))
			changes = append(changes, change{kind, id, recordString(v, "ProductID"), recordString(v, "FeatureID"), raw, j})
			values[id] = v
			positions[id] = j
		}
		ids := make([]string, 0, len(values))
		for id := range values {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(a, b int) bool {
			if positions[ids[a]] == positions[ids[b]] {
				return ids[a] < ids[b]
			}
			return positions[ids[a]] < positions[ids[b]]
		})
		merged := reflect.ValueOf(domain.EmptyState()).Field(i)
		if len(ids) > 0 {
			merged = reflect.MakeSlice(cur.Type(), len(ids), len(ids))
		}
		for j, id := range ids {
			merged.Index(j).Set(values[id])
		}
		nv.Field(i).Set(merged)
	}

	if !full {
		safe := true
		for name := range selected {
			if name != "events" && name != "features" && name != "integrations" && name != "memories" && name != "gates" {
				safe = false
			}
		}
		if safe {
			err = validateOrdinaryDelta(before, st, delta, st.Events[oldEvents:])
		} else {
			err = domain.ValidateStateStatuses(st)
		}
		if err != nil {
			return err
		}
	}

	batch := &pgx.Batch{}
	flush := func() error {
		if batch.Len() == 0 {
			return nil
		}
		results := tx.SendBatch(ctx, batch)
		for range batch.Len() {
			if _, e := results.Exec(); e != nil {
				_ = results.Close()
				return e
			}
		}
		e := results.Close()
		batch = &pgx.Batch{}
		return e
	}
	for _, c := range changes {
		batch.Queue("INSERT INTO records(id,kind,product_id,feature_id,document,position) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(id) DO UPDATE SET document=EXCLUDED.document,product_id=EXCLUDED.product_id,feature_id=EXCLUDED.feature_id,position=EXCLUDED.position", c.id, c.kind, c.product, c.feature, c.raw, c.position)
		if batch.Len() >= 256 {
			if err = flush(); err != nil {
				return err
			}
		}
	}
	for _, event := range st.Events[oldEvents:] {
		raw, e := json.Marshal(event)
		if e != nil {
			return e
		}
		batch.Queue("INSERT INTO events(id,product_id,feature_id,document) VALUES($1,$2,$3,$4)", event.ID, event.ProductID, event.FeatureID, raw)
		if batch.Len() >= 256 {
			if err = flush(); err != nil {
				return err
			}
		}
	}
	if err = flush(); err != nil {
		return err
	}

	// Merged records consist only of immutable committed values and freshly
	// decoded changed documents. No callback-owned value escapes into the cache.
	// Retain the old immutable event index and detach just new provenance events.
	next.Events = append([]domain.Event(nil), before.Events...)
	for _, event := range st.Events[oldEvents:] {
		if domain.IsWriteProvenanceEvent(event) {
			raw, e := json.Marshal(event)
			if e != nil {
				return e
			}
			var stored domain.Event
			if e = json.Unmarshal(raw, &stored); e != nil {
				return e
			}
			next.Events = append(next.Events, stored)
		}
	}

	if _, err = tx.Exec(ctx, "UPDATE control_revision SET revision=revision+1 WHERE id=1"); err != nil {
		return err
	}
	var journalMax int64
	if err = tx.QueryRow(ctx, "SELECT COALESCE(max(sequence),0) FROM events").Scan(&journalMax); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	s.writeJournal = &journalCache{revision: revision + 1, max: journalMax, events: next.Events}
	// Restore imports a new journal into an empty instance. Drop any previously
	// borrowed full snapshot even if the imported sequence happens to match it.
	for _, event := range st.Events[oldEvents:] {
		if event.Action == "restore_backup" {
			s.journal = nil
			s.viewState = nil
			break
		}
	}

	positions := s.cached.positions
	for _, c := range changes {
		positions[c.id] = c.position
	}
	s.cached = &stateCache{revision: revision + 1, state: next, positions: positions}
	return nil
}

type recordFieldPaths struct{ id, product, feature []int }

var recordPlans sync.Map

func recordString(v reflect.Value, name string) string {
	typ := v.Type()
	cached, ok := recordPlans.Load(typ)
	if !ok {
		paths := recordFieldPaths{}
		if f, found := typ.FieldByName("ID"); found {
			paths.id = f.Index
		}
		if f, found := typ.FieldByName("ProductID"); found {
			paths.product = f.Index
		}
		if f, found := typ.FieldByName("FeatureID"); found {
			paths.feature = f.Index
		}
		cached, _ = recordPlans.LoadOrStore(typ, paths)
	}
	paths := cached.(recordFieldPaths)
	var path []int
	switch name {
	case "ID":
		path = paths.id
	case "ProductID":
		path = paths.product
	case "FeatureID":
		path = paths.feature
	}
	if path == nil {
		return ""
	}
	return v.FieldByIndex(path).String()
}

const writeEventPredicate = `document->>'action' IN ('close_deployment','close_build','restore_backup','assemble_environment','reconcile_composition','update_environment_feature','worker_report','pin_artifact','release_artifact_pin') OR document->'data'->>'action'='release_artifact_pin' OR id IN (SELECT id FROM records WHERE kind IN ('artifact_pins','registry_inventories'))`

func readWriteEvents(ctx context.Context, tx pgx.Tx, st *domain.State) error {
	rows, err := tx.Query(ctx, "SELECT document FROM events WHERE "+writeEventPredicate+" ORDER BY sequence")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return err
		}
		var e domain.Event
		if err = json.Unmarshal(raw, &e); err != nil {
			return err
		}
		st.Events = append(st.Events, e)
	}
	return rows.Err()
}

// The ordinary scope cannot alter pin/inventory reference collections. Their
// previously validated invariants therefore change only through reserved ID
// collisions or new release audit evidence; check exactly those edges.
func validateOrdinaryDelta(before, after, delta domain.State, events []domain.Event) error {
	if err := domain.ValidateStateStatuses(delta); err != nil {
		return err
	}
	pins := map[string]bool{}
	reserved := map[string]bool{}
	for _, p := range before.ArtifactPins {
		pins[p.ID] = true
		reserved[p.ID] = true
	}
	for _, v := range before.RegistryInventories {
		reserved[v.ID] = true
	}
	for _, e := range before.Events {
		if (e.Action == "release_artifact_pin" || e.Data.Action == "release_artifact_pin") && (pins[e.EntityID] || pins[e.Data.ID]) {
			reserved[e.ID] = true
		}
	}
	for _, e := range events {
		if reserved[e.ID] {
			return fmt.Errorf("invalid reserved record/event identity collision")
		}
		if (e.Action == "release_artifact_pin" || e.Data.Action == "release_artifact_pin") && (pins[e.EntityID] || pins[e.Data.ID]) {
			if err := domain.ValidateArtifactPins(after); err != nil {
				return err
			}
		}
	}
	value := reflect.ValueOf(delta)
	for i := 0; i < value.NumField(); i++ {
		field := value.Field(i)
		for j := 0; j < field.Len(); j++ {
			if reserved[recordString(field.Index(j), "ID")] {
				return fmt.Errorf("invalid reserved record identity collision")
			}
		}
	}
	return nil
}

var clonePlans sync.Map // reflect.Type -> []int; immutable after publication
func cloneFields(typ reflect.Type) []int {
	if cached, ok := clonePlans.Load(typ); ok {
		return cached.([]int)
	}
	fields := []int{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.PkgPath == "" && mutableType(f.Type) {
			fields = append(fields, i)
		}
	}
	actual, _ := clonePlans.LoadOrStore(typ, fields)
	return actual.([]int)
}
func mutableType(typ reflect.Type) bool {
	switch typ.Kind() {
	case reflect.Slice, reflect.Map, reflect.Pointer, reflect.Interface:
		return true
	case reflect.Struct:
		return len(cloneFields(typ)) > 0
	case reflect.Array:
		return mutableType(typ.Elem())
	}
	return false
}

// Writer transactions hold control_revision's row lock: all journal and record
// observations are stable against other Store writers until commit. Keep this
// narrow projection separate so ordinary opaque history need not be decoded.
func (s *Store) readCachedWriteEvents(ctx context.Context, tx pgx.Tx, st *domain.State, revision int64) error {
	var max int64
	if err := tx.QueryRow(ctx, "SELECT COALESCE(max(sequence),0) FROM events").Scan(&max); err != nil {
		return err
	}
	cache := s.writeJournal
	if cache == nil || revision < cache.revision || max < cache.max {
		cache = &journalCache{}
	}
	if cache.max > 0 && max > cache.max {
		var added int64
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM events WHERE sequence > $1", cache.max).Scan(&added); err != nil {
			return err
		}
		if added != max-cache.max {
			cache = &journalCache{}
		}
	}
	rows, err := tx.Query(ctx, "SELECT document FROM events WHERE sequence > $1 AND ("+writeEventPredicate+") ORDER BY sequence", cache.max)
	if err != nil {
		return err
	}
	defer rows.Close()
	events := append([]domain.Event(nil), cache.events...)
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return err
		}
		var event domain.Event
		if err = json.Unmarshal(raw, &event); err != nil {
			return err
		}
		events = append(events, event)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	st.Events = events
	s.writeJournal = &journalCache{revision: revision, max: max, events: events}
	return nil
}
