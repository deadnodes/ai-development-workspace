package persistence

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
	"time"
)

func historyFixture(t *testing.T) domain.State {
	t.Helper()
	st := domain.EmptyState()
	raw := `{"products":[{"id":"p"},{"id":"private"}],"features":[{"id":"a","product_id":"p","status":"planned"},{"id":"b","product_id":"p","status":"planned"},{"id":"foreign","product_id":"private","status":"planned"}],"integration_revisions":[{"id":"r-direct","product_id":"p","feature_id":"a","integration_id":"ia","repository_id":"repo","head_commit":"old"}],"worker_jobs":[{"id":"j1","product_id":"p","integration_id":"ia","operation_id":"w1","capability":"local_oci_build","status":"FAILED","inputs":{"repository_id":"repo","revision_id":"r-direct","source_sha":"old"}},{"id":"j2","product_id":"p","operation_id":"w2","capability":"local_oci_build","status":"CANCELLED","inputs":{"repository_id":"repo","revision_id":"generated","source_sha":"composed","source_proof":{"delivery_id":"d","parent_id":"parent","snapshot_hash":"recorded","parent_hash":"recorded-parent"}}},{"id":"foreign-job","product_id":"private","capability":"local_oci_build","status":"FAILED","inputs":{}}],"worker_attempts":[{"id":"at1","product_id":"p","job_id":"j2","status":"FAILED"},{"id":"at2","product_id":"p","job_id":"j2","status":"EXPIRED"},{"id":"at3","product_id":"p","job_id":"j2","status":"CANCELLED"},{"id":"private-at","product_id":"private","job_id":"j2","status":"FAILED"}],"operations":[{"id":"d","product_id":"p","kind":"DEPLOY","status":"FAILED","parent_id":"parent","application_id":"app","native":{"build_job_id":"j2","parent_hash":"recorded-parent"},"snapshot":{"revision":{"id":"generated","product_id":"p","repository_id":"repo","head_commit":"composed"},"application":{"id":"app","product_id":"p"}}},{"id":"parent","product_id":"p","kind":"COMPOSE","status":"FAILED","child_ids":["d"],"sources":[{"repository_id":"repo","plan":{"head_shas":["old-a","old-b"]},"result":{"sha":"composed","conflict":false}}],"composition_snapshot":{"id":"comp","product_id":"p","status":"planned","components":[{"application_id":"app","revision_ids":["ra","rb"]}],"revision_snapshots":[{"id":"ra","product_id":"p","feature_id":"a","integration_id":"ia","repository_id":"repo","head_commit":"old-a"},{"id":"rb","product_id":"p","feature_id":"b","integration_id":"ib","repository_id":"repo","head_commit":"old-b"}]}}]}`
	if e := json.Unmarshal([]byte(raw), &st); e != nil {
		t.Fatal(e)
	}
	// Large unrelated payloads and reports must never enter the history projection.
	st.Products[0].Description = strings.Repeat("UNRELATED_SECRET", 1<<17)
	st.WorkerJobs[1].Inputs.Parameters = map[string]any{"credential": strings.Repeat("RECIPE_SECRET", 1<<17)}
	return st
}

type historyReader interface {
	ReadWorkerHistory(context.Context, domain.WorkerHistoryMetadataRequest) (domain.WorkerHistoryMetadata, error)
}

func checkHistoryReader(t *testing.T, r historyReader) {
	t.Helper()
	ctx := context.Background()
	read := func(q domain.WorkerHistoryMetadataRequest) domain.WorkerHistoryMetadata {
		t.Helper()
		v, e := r.ReadWorkerHistory(ctx, q)
		if e != nil {
			t.Fatal(e)
		}
		b, _ := json.Marshal(v)
		if len(b) > 32<<10 || strings.Contains(string(b), "SECRET") {
			t.Fatal("unbounded or sensitive projection")
		}
		return v
	}
	q := domain.WorkerHistoryMetadataRequest{ProductID: "p", FeatureID: "b", Collection: "jobs", Limit: 2}
	v := read(q)
	if len(v.Items) != 1 || v.Items[0].ID != "j2" || v.Items[0].Attempts != 3 || v.Items[0].Links != 2 {
		t.Fatalf("multi Feature history missing %+v", v)
	}
	q.JobID = "j2"
	q.Collection = "links"
	q.Limit = 1
	v = read(q)
	if len(v.Items) != 1 || v.Items[0].FeatureID != "a" || v.Items[0].SourceSHA != "old-a" || v.Items[0].Provenance != "recorded_snapshot_unverified" {
		t.Fatalf("frozen link %+v", v)
	}
	q.AfterID = v.Items[0].ID
	v = read(q)
	if len(v.Items) != 1 || v.Items[0].FeatureID != "b" {
		t.Fatalf("second link %+v", v)
	}
	q.Collection = "attempts"
	q.AfterID = ""
	q.Limit = 2
	v = read(q)
	if len(v.Items) != 2 || v.Items[1].RecordedStatus != "EXPIRED" {
		t.Fatalf("attempts %+v", v)
	}
	q.AfterID = "at2"
	v = read(q)
	if len(v.Items) != 1 || v.Items[0].RecordedStatus != "CANCELLED" {
		t.Fatalf("last attempt %+v", v)
	}
	q = domain.WorkerHistoryMetadataRequest{ProductID: "p", FeatureID: "a", Collection: "jobs", Limit: 3}
	v = read(q)
	if len(v.Items) != 2 {
		t.Fatalf("direct plus managed %+v", v)
	}
	q.FeatureID = "foreign"
	v = read(q)
	if v.FeatureFound || len(v.Items) != 0 {
		t.Fatal("cross product leak")
	}
	q.FeatureID = ""
	q.JobID = "foreign-job"
	v = read(q)
	if v.JobFound || len(v.Items) != 0 {
		t.Fatal("foreign job leak")
	}
}
func TestWorkerHistoryStores(t *testing.T) {
	ctx := context.Background()
	st := historyFixture(t)
	path := filepath.Join(t.TempDir(), "state.db")
	local, e := OpenLocal(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	if e = local.Update(ctx, func(s *domain.State) error { *s = st; return nil }); e != nil {
		t.Fatal(e)
	}
	checkHistoryReader(t, local)
	local.Close()
	local, e = OpenLocal(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	defer local.Close()
	checkHistoryReader(t, local)
	// Current revision changes cannot rewrite managed frozen composition membership.
	if e = local.Update(ctx, func(s *domain.State) error { s.IntegrationRevisions[0].HeadCommit = "new"; return nil }); e != nil {
		t.Fatal(e)
	}
	q := domain.WorkerHistoryMetadataRequest{ProductID: "p", FeatureID: "b", JobID: "j2", Collection: "links", Limit: 10}
	got, e := local.ReadWorkerHistory(ctx, q)
	if e != nil || len(got.Items) != 2 {
		t.Fatal(got, e)
	}
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("TEST_DATABASE_URL")
		if dsn == "" {
			t.Skip("actual PostgreSQL requires TEST_DATABASE_URL")
		}
		conn, e := pgx.Connect(ctx, dsn)
		if e != nil {
			t.Fatal(e)
		}
		defer conn.Close(ctx)
		schema := fmt.Sprintf("worker_history_%d_tests", time.Now().UnixNano())
		if _, e = conn.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); e != nil {
			t.Fatal(e)
		}
		defer conn.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		u, e := url.Parse(dsn)
		if e != nil {
			t.Fatal(e)
		}
		args := u.Query()
		args.Set("search_path", schema)
		u.RawQuery = args.Encode()
		pg, e := Open(ctx, u.String())
		if e != nil {
			t.Fatal(e)
		}
		defer pg.Close()
		if e = pg.Update(ctx, func(s *domain.State) error { *s = st; return nil }); e != nil {
			t.Fatal(e)
		}
		if _, e = pg.pool.Exec(ctx, `DROP TABLE events`); e != nil {
			t.Fatal(e)
		}
		checkHistoryReader(t, pg)
		pgot, e := pg.ReadWorkerHistory(ctx, q)
		if e != nil || !reflect.DeepEqual(got, pgot) {
			t.Fatalf("store parity local=%+v pg=%+v err=%v", got, pgot, e)
		}
		if _, e = pg.Read(ctx); e == nil {
			t.Fatal("full read unexpectedly succeeded")
		}
	})
}
func TestWorkerHistoryBrokenFrozenChain(t *testing.T) {
	ctx := context.Background()
	local, e := OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer local.Close()
	for _, mutate := range []func(*domain.State){
		func(s *domain.State) { s.Operations[0].Snapshot.Revision.HeadCommit = "moved" },
		func(s *domain.State) { s.Operations[1].ProductID = "private" },
		func(s *domain.State) { s.Operations[1].Sources[0].Result.SHA = "moved" },
		func(s *domain.State) { s.Operations[1].ChildIDs = nil },
		func(s *domain.State) { s.Operations[1].CompositionSnapshot.Components[0].RevisionIDs = nil },
	} {
		st := historyFixture(t)
		mutate(&st)
		if e = local.Update(ctx, func(s *domain.State) error { *s = st; return nil }); e != nil {
			t.Fatal(e)
		}
		v, e := local.ReadWorkerHistory(ctx, domain.WorkerHistoryMetadataRequest{ProductID: "p", FeatureID: "b", Collection: "jobs", Limit: 10})
		if e != nil || len(v.Items) != 0 {
			t.Fatalf("bad chain attributed %+v %v", v, e)
		}
		v, e = local.ReadWorkerHistory(ctx, domain.WorkerHistoryMetadataRequest{ProductID: "p", JobID: "j2", Collection: "jobs", Limit: 10})
		if e != nil || len(v.Items) != 1 || v.Items[0].Provenance != "unknown" {
			t.Fatalf("unknown history omitted %+v %v", v, e)
		}
	}
}

func TestWorkerHistoryMainCandidateAndBaseOnly(t *testing.T) {
	ctx := context.Background()
	local, e := OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer local.Close()
	st := historyFixture(t)
	parent := &st.Operations[1]
	parent.Kind = "RELEASE_CANDIDATE"
	parent.Sources = nil
	parent.MainCandidate = &domain.MainCandidateProof{EvidenceKind: "ATTRIBUTED_RCP_VERIFICATION", Request: domain.MainCandidateRequest{MainSources: []domain.MainCandidateSource{{ApplicationID: "app", MainSHA: "composed", Revisions: []domain.MainCandidateRevision{{RevisionID: "ra"}, {RevisionID: "rb"}}}}}}
	if e = local.Update(ctx, func(s *domain.State) error { *s = st; return nil }); e != nil {
		t.Fatal(e)
	}
	checkHistoryReader(t, local)
	// A base-only composition has no Feature linkage, even if an Integration ID
	// is present on a synthetic delivery snapshot. Product history still retains it.
	st.Operations[1].CompositionSnapshot.Components[0].RevisionIDs = nil
	st.Operations[0].IntegrationID = "ia"
	st.WorkerJobs[1].IntegrationID = "ia"
	st.Operations[0].Snapshot.Revision.IntegrationID = "ia"
	if e = local.Update(ctx, func(s *domain.State) error { *s = st; return nil }); e != nil {
		t.Fatal(e)
	}
	q := domain.WorkerHistoryMetadataRequest{ProductID: "p", JobID: "j2", Collection: "jobs", Limit: 10}
	v, e := local.ReadWorkerHistory(ctx, q)
	if e != nil || len(v.Items) != 1 || v.Items[0].Links != 0 || v.Items[0].Provenance != "unknown" {
		t.Fatal("base-only invented linkage", v, e)
	}
	q.FeatureID = "a"
	v, e = local.ReadWorkerHistory(ctx, q)
	if e != nil || len(v.Items) != 0 || v.JobFound {
		t.Fatal("base-only linked Feature", v, e)
	}
}

func TestWorkerHistoryFrozenSourceParity(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("actual PostgreSQL requires TEST_DATABASE_URL")
	}
	cases := []struct {
		name      string
		mutate    func(*domain.State)
		wantLinks int
	}{
		{"main_candidate", func(s *domain.State) { historyMainFixture(s) }, 2},
		{"compose_reverse_subset", func(s *domain.State) { s.Operations[1].CompositionSnapshot.Components[0].RevisionIDs = []string{"ra"} }, 0},
		{"main_reverse_subset", func(s *domain.State) {
			historyMainFixture(s)
			s.Operations[1].CompositionSnapshot.Components[0].RevisionIDs = []string{"ra"}
		}, 0},
		{"compose_extra_unknown_head", func(s *domain.State) {
			s.Operations[1].Sources[0].Plan.HeadSHAs = append(s.Operations[1].Sources[0].Plan.HeadSHAs, "unknown-head")
		}, 0},
		{"main_extra_unknown_revision", func(s *domain.State) {
			historyMainFixture(s)
			src := &s.Operations[1].MainCandidate.Request.MainSources[0]
			src.Revisions = append(src.Revisions, domain.MainCandidateRevision{RevisionID: "unknown-revision"})
		}, 0},
		{"compose_duplicate_head", func(s *domain.State) {
			s.Operations[1].Sources[0].Plan.HeadSHAs = append(s.Operations[1].Sources[0].Plan.HeadSHAs, "old-a")
		}, 0},
		{"compose_unselected_frozen_snapshot", func(s *domain.State) {
			s.Operations[1].CompositionSnapshot.Components[0].RevisionIDs = []string{"ra"}
			s.Operations[1].Sources[0].Plan.HeadSHAs = []string{"old-a"}
		}, 0},
		{"compose_singleton", func(s *domain.State) {
			historySingleton(s)
			s.Operations[1].Sources[0].Plan.HeadSHAs = []string{"old-a"}
		}, 1},
		{"main_singleton", func(s *domain.State) {
			historyMainFixture(s)
			historySingleton(s)
			s.Operations[1].MainCandidate.Request.MainSources[0].Revisions = s.Operations[1].MainCandidate.Request.MainSources[0].Revisions[:1]
		}, 1},
		{"compose_valid_base_only", func(s *domain.State) {
			s.Operations[1].CompositionSnapshot.Components[0].RevisionIDs = nil
			s.Operations[1].CompositionSnapshot.RevisionSnapshots = nil
			s.Operations[1].Sources[0].Plan.HeadSHAs = nil
		}, 0},
		{"main_valid_base_only", func(s *domain.State) {
			historyMainFixture(s)
			s.Operations[1].CompositionSnapshot.Components[0].RevisionIDs = nil
			s.Operations[1].CompositionSnapshot.RevisionSnapshots = nil
			s.Operations[1].MainCandidate.Request.MainSources[0].Revisions = nil
		}, 0},
		{"compose_shared_repository", func(s *domain.State) { historySharedRepository(s) }, 2},
		{"main_shared_repository", func(s *domain.State) { historyMainFixture(s); historySharedRepository(s) }, 2},
		{"base_only", func(s *domain.State) {
			historyMainFixture(s)
			s.Operations[1].CompositionSnapshot.Components[0].RevisionIDs = nil
		}, 0},
		{"compose_missing_sibling_source", func(s *domain.State) { s.Operations[1].Sources[0].Plan.HeadSHAs = []string{"old-a"} }, 0},
		{"main_missing_sibling_evidence", func(s *domain.State) {
			historyMainFixture(s)
			s.Operations[1].MainCandidate.Request.MainSources[0].Revisions = s.Operations[1].MainCandidate.Request.MainSources[0].Revisions[:1]
		}, 0},
		{"duplicate_conflicting_frozen_revision", func(s *domain.State) {
			r := s.Operations[1].CompositionSnapshot.RevisionSnapshots[0]
			r.HeadCommit = "unrecognized-head"
			s.Operations[1].CompositionSnapshot.RevisionSnapshots = append(s.Operations[1].CompositionSnapshot.RevisionSnapshots, r)
		}, 0},
		{"duplicate_wrong_owner_frozen_revision", func(s *domain.State) {
			r := s.Operations[1].CompositionSnapshot.RevisionSnapshots[0]
			r.ProductID = "private"
			s.Operations[1].CompositionSnapshot.RevisionSnapshots = append(s.Operations[1].CompositionSnapshot.RevisionSnapshots, r)
		}, 0},
		{"duplicate_identical_frozen_revision", func(s *domain.State) {
			r := s.Operations[1].CompositionSnapshot.RevisionSnapshots[0]
			s.Operations[1].CompositionSnapshot.RevisionSnapshots = append(s.Operations[1].CompositionSnapshot.RevisionSnapshots, r)
		}, 0},
		{"duplicate_component", func(s *domain.State) {
			c := s.Operations[1].CompositionSnapshot.Components[0]
			c.RevisionIDs = nil
			s.Operations[1].CompositionSnapshot.Components = append(s.Operations[1].CompositionSnapshot.Components, c)
		}, 0},
		{"duplicate_selected_revision", func(s *domain.State) {
			s.Operations[1].CompositionSnapshot.Components[0].RevisionIDs = append(s.Operations[1].CompositionSnapshot.Components[0].RevisionIDs, "ra")
		}, 0},
		{"duplicate_compose_source", func(s *domain.State) {
			src := s.Operations[1].Sources[0]
			src.Result = nil
			s.Operations[1].Sources = append(s.Operations[1].Sources, src)
		}, 0},
		{"duplicate_main_source", func(s *domain.State) {
			historyMainFixture(s)
			src := s.Operations[1].MainCandidate.Request.MainSources[0]
			src.MainSHA = "unrecognized-head"
			s.Operations[1].MainCandidate.Request.MainSources = append(s.Operations[1].MainCandidate.Request.MainSources, src)
		}, 0},
		{"duplicate_main_revision", func(s *domain.State) {
			historyMainFixture(s)
			src := &s.Operations[1].MainCandidate.Request.MainSources[0]
			src.Revisions = append(src.Revisions, src.Revisions[0])
		}, 0},
		{"null_compose_sources", func(s *domain.State) { s.Operations[1].Sources = nil }, 0},
		{"null_main_sources", func(s *domain.State) { historyMainFixture(s); s.Operations[1].MainCandidate.Request.MainSources = nil }, 0},
		{"null_main_revisions", func(s *domain.State) {
			historyMainFixture(s)
			s.Operations[1].MainCandidate.Request.MainSources[0].Revisions = nil
		}, 0},
		{"missing_sibling_snapshot", func(s *domain.State) {
			s.Operations[1].CompositionSnapshot.RevisionSnapshots = s.Operations[1].CompositionSnapshot.RevisionSnapshots[:1]
		}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			st := historyFixture(t)
			st.Products[0].Description = ""
			st.WorkerJobs[1].Inputs.Parameters = nil
			tc.mutate(&st)
			local, e := OpenLocal(ctx, filepath.Join(t.TempDir(), "independent.db"))
			if e != nil {
				t.Fatal(e)
			}
			defer local.Close()
			if e = local.Update(ctx, func(s *domain.State) error { *s = st; return nil }); e != nil {
				t.Fatal(e)
			}
			dsn := os.Getenv("TEST_DATABASE_URL")
			u, e := url.Parse(dsn)
			if e != nil {
				t.Fatal(e)
			}
			conn, e := pgx.Connect(ctx, dsn)
			if e != nil {
				t.Fatal(e)
			}
			defer conn.Close(context.Background())
			schema := fmt.Sprintf("frozen_worker_history_%d_tests", time.Now().UnixNano())
			quoted := pgx.Identifier{schema}.Sanitize()
			if _, e = conn.Exec(ctx, "CREATE SCHEMA "+quoted); e != nil {
				t.Fatal(e)
			}
			defer func() {
				cleanup, done := context.WithTimeout(context.Background(), 15*time.Second)
				defer done()
				if _, e := conn.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); e != nil {
					t.Error(e)
				}
			}()
			args := u.Query()
			args.Set("search_path", schema)
			u.RawQuery = args.Encode()
			pg, e := Open(ctx, u.String())
			if e != nil {
				t.Fatal(e)
			}
			defer pg.Close()
			if e = pg.Update(ctx, func(s *domain.State) error { *s = st; return nil }); e != nil {
				t.Fatal(e)
			}
			for _, collection := range []string{"jobs", "links", "attempts"} {
				q := domain.WorkerHistoryMetadataRequest{ProductID: "p", JobID: "j2", Collection: collection, Limit: 10}
				l, le := local.ReadWorkerHistory(ctx, q)
				p, pe := pg.ReadWorkerHistory(ctx, q)
				if le != nil || pe != nil || !reflect.DeepEqual(l, p) {
					t.Errorf("parity collection=%s local=%+v pg=%+v errors=%v/%v", collection, l, p, le, pe)
				}
				for name, v := range map[string]domain.WorkerHistoryMetadata{"local": l, "postgres": p} {
					t.Logf("%s %s items=%+v", name, collection, v.Items)
					if collection == "links" && len(v.Items) != tc.wantLinks {
						t.Errorf("%s partial attribution: links=%d want=%d", name, len(v.Items), tc.wantLinks)
					}
					if collection == "jobs" && (len(v.Items) != 1 || v.Items[0].Links != tc.wantLinks || (tc.wantLinks == 0 && v.Items[0].Provenance != "unknown")) {
						t.Errorf("%s job attribution should be unknown/zero on malformed/base-only chains: %+v", name, v.Items)
					}
				}
			}
		})
	}
}
func historyMainFixture(st *domain.State) {
	p := &st.Operations[1]
	p.Kind = "RELEASE_CANDIDATE"
	p.Sources = nil
	p.MainCandidate = &domain.MainCandidateProof{EvidenceKind: "ATTRIBUTED_RCP_VERIFICATION", Request: domain.MainCandidateRequest{MainSources: []domain.MainCandidateSource{{ApplicationID: "app", MainSHA: "composed", Revisions: []domain.MainCandidateRevision{{RevisionID: "ra"}, {RevisionID: "rb"}}}}}}
}

func historySingleton(s *domain.State) {
	s.Operations[1].CompositionSnapshot.Components[0].RevisionIDs = []string{"ra"}
	s.Operations[1].CompositionSnapshot.RevisionSnapshots = s.Operations[1].CompositionSnapshot.RevisionSnapshots[:1]
}
func historySharedRepository(s *domain.State) {
	comp := s.Operations[1].CompositionSnapshot
	second := comp.Components[0]
	second.ApplicationID = "second-app"
	comp.Components = append(comp.Components, second)
	comp.ApplicationSnapshots = []domain.Application{{Meta: domain.Meta{ID: "app", ProductID: "p"}, RepositoryID: "repo"}, {Meta: domain.Meta{ID: "second-app", ProductID: "p"}, RepositoryID: "repo"}}
}
