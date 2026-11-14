package persistence

import (
	"context"
	"path/filepath"
	"reflect"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
	"time"
)

func TestRuntimeFreshnessStores(t *testing.T) {
	for _, backend := range []string{"local", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			var store interface {
				TouchRuntimeFreshness(context.Context, string, string, time.Time, bool) error
				RuntimeFreshness(context.Context, []string) (map[string]domain.RuntimeFreshness, error)
				Read(context.Context) (domain.State, error)
			}
			if backend == "postgres" {
				store = retrievalPostgres(t)
			} else {
				s, e := OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
				if e != nil {
					t.Fatal(e)
				}
				defer s.Close()
				store = s
			}
			before, e := store.Read(ctx)
			if e != nil {
				t.Fatal(e)
			}
			var revision int64
			if s, ok := store.(*Store); ok {
				if e = s.pool.QueryRow(ctx, "SELECT revision FROM control_revision WHERE id=1").Scan(&revision); e != nil {
					t.Fatal(e)
				}
			}
			at := time.Now().UTC().Truncate(time.Microsecond)
			for _, v := range []struct {
				at      time.Time
				healthy bool
			}{{at, true}, {at.Add(time.Minute), false}, {at, false}} {
				if e = store.TouchRuntimeFreshness(ctx, "op", "observation", v.at, v.healthy); e != nil {
					t.Fatal(e)
				}
			}
			got, e := store.RuntimeFreshness(ctx, []string{"op", "missing"})
			if e != nil || len(got) != 1 || got["op"].ObservationID != "observation" || got["op"].Healthy || !got["op"].ObservedAt.Equal(at.Add(time.Minute)) {
				t.Fatalf("freshness %v %v", got, e)
			}
			after, e := store.Read(ctx)
			if e != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("freshness changed state: %v", e)
			}
			if s, ok := store.(*Store); ok {
				var next int64
				var healthy bool
				if e = s.pool.QueryRow(ctx, "SELECT revision FROM control_revision WHERE id=1").Scan(&next); e != nil || next != revision {
					t.Fatalf("revision changed %v", e)
				}
				if e = s.pool.QueryRow(ctx, "SELECT healthy FROM runtime_observation_freshness WHERE operation_id='op'").Scan(&healthy); e != nil || healthy {
					t.Fatalf("health upsert %v", e)
				}
				var applied bool
				if e = s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=8)").Scan(&applied); e != nil || !applied {
					t.Fatalf("migration missing %v", e)
				}
			}
		})
	}
}
func TestRuntimeFreshnessMigration(t *testing.T) {
	sql, e := migrations.ReadFile("migrations/008_runtime_observation_freshness.sql")
	if e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{"CREATE TABLE runtime_observation_freshness", "operation_id text PRIMARY KEY", "observed_at timestamptz NOT NULL", "healthy boolean NOT NULL", "observation_id text NOT NULL"} {
		if !strings.Contains(string(sql), want) {
			t.Fatal(want)
		}
	}
}

func TestRuntimeObservationScopedDeletionStores(t *testing.T) {
	for _, backend := range []string{"local", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			var store interface {
				Update(context.Context, func(*domain.State) error) error
				UpdateScoped(context.Context, domain.StateMutationScope, func(*domain.State) error) error
				Read(context.Context) (domain.State, error)
			}
			if backend == "postgres" {
				store = retrievalPostgres(t)
			} else {
				s, e := OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
				if e != nil {
					t.Fatal(e)
				}
				defer s.Close()
				store = s
			}
			if e := store.Update(ctx, func(st *domain.State) error {
				st.RuntimeObservations = []domain.RuntimeObservation{{Meta: domain.Meta{ID: "old"}}, {Meta: domain.Meta{ID: "latest"}}}
				return nil
			}); e != nil {
				t.Fatal(e)
			}
			if e := store.UpdateScoped(ctx, domain.StateMutationScope{MutableCollections: []string{"runtime_observations"}}, func(st *domain.State) error {
				st.RuntimeObservations = st.RuntimeObservations[1:]
				st.Events = append(st.Events, domain.Event{ID: "prune", Action: "prune_runtime_observations"})
				return nil
			}); e != nil {
				t.Fatal(e)
			}
			got, e := store.Read(ctx)
			if e != nil || len(got.RuntimeObservations) != 1 || got.RuntimeObservations[0].ID != "latest" || len(got.Events) != 1 {
				t.Fatalf("deletion not persisted: %+v %v", got.RuntimeObservations, e)
			}
		})
	}
}
