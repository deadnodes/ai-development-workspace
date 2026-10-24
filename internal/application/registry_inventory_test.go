package application

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
)

const registryTestToken = "registry-observer-fixture-token-00000000000000000001"

func registryDigest(n int) string { return fmt.Sprintf("sha256:%064x", n+1) }
func registryRecipe() workerconfig.RegistryObserveRecipe {
	return workerconfig.RegistryObserveRecipe{ID: "observe", Revision: "v1", ProductID: "p", RepositoryID: "r", ComponentID: "app", Origin: "https://registry.example", Repository: "team/app", Limits: domain.RegistryObserveLimits{MaxResponseBytes: 2 << 20, MaxTotalBytes: 8 << 20, MaxNodes: 64, MaxEdges: 128, MaxDepth: 16, MaxPages: 16, MaxRequests: 512, TimeoutSeconds: 60}}
}
func registryAuthority(t *testing.T, r workerconfig.RegistryObserveRecipe, revision string, expires time.Time, revoked bool) *workerconfig.Authority {
	t.Helper()
	r.Hash = workerconfig.ObservationHash(r)
	a, e := workerconfig.New(workerconfig.Config{Revision: revision, LeaseSeconds: 10, Observations: []workerconfig.RegistryObserveRecipe{r}, Credentials: []workerconfig.Credential{{ID: "observer", Principal: "worker/observer", TokenEnv: "REGISTRY_FIXTURE_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"r"}, Capabilities: []string{"registry_observe"}, ExpiresAt: expires, Revoked: revoked}}}, "fixture-control-token", func(string) string { return registryTestToken })
	if e != nil {
		t.Fatal(e)
	}
	return a
}
func registryFixture(t *testing.T, kind string) (*Service, workerTestStore, func() workerTestStore) {
	t.Helper()
	store, reopen := openWorkerTestStore(t, kind)
	s := New(store)
	s.ConfigureWorkers(registryAuthority(t, registryRecipe(), "observe-cfg", time.Now().Add(time.Hour), false))
	workerSeed(t, s)
	if _, e := s.Execute(context.Background(), domain.Command{Action: "create_application", Actor: "operator", ID: "app", ProductID: "p", Data: map[string]any{"name": "app", "repository_id": "r"}}); e != nil {
		t.Fatal(e)
	}
	registryPin(t, s, registryDigest(1), "initial-observation-root")
	return s, store, reopen
}
func registryPin(t *testing.T, s *Service, digest, key string) domain.ArtifactPin {
	t.Helper()
	v, e := s.Execute(context.Background(), domain.Command{Action: "pin_artifact", Actor: "operator", ProductID: "p", Data: map[string]any{"application_id": "app", "image_repository": "registry.example/team/app", "digest": digest, "purpose": "manual", "request_key": key}})
	if e != nil {
		t.Fatal(e)
	}
	return v.(domain.ArtifactPin)
}
func registryQueueCommand(s *Service, key string) domain.Command {
	r, _ := s.externalWorkers.Observation("observe")
	return domain.Command{Action: "refresh_registry_inventory", Actor: "operator", ProductID: "p", FeatureID: "f", IntegrationID: "i", Data: map[string]any{"application_id": "app", "recipe_id": r.ID, "recipe_revision": r.Revision, "recipe_hash": r.Hash, "config_revision": s.externalWorkers.Revision(), "request_key": key, "deadline": time.Now().Add(time.Minute)}}
}
func registryQueue(t *testing.T, s *Service) *domain.WorkerLease {
	t.Helper()
	v, e := s.Execute(context.Background(), registryQueueCommand(s, id()))
	if e != nil {
		t.Fatal(e)
	}
	job := v.(domain.WorkerJob)
	if job.Capability != "registry_observe" || job.Inputs.SourceSHA != "" || job.Inputs.RevisionID != "" || job.Inputs.Registry == nil || job.Inputs.Execution != nil {
		t.Fatal("observer overloaded source/build identity", job)
	}
	p := registryPrincipal(t, s)
	l, e := s.ClaimWorker(context.Background(), p, domain.WorkerClaim{ProductID: "p", RepositoryID: "r", Capability: "registry_observe", RecipeID: job.Inputs.RecipeID, RecipeHash: job.Inputs.RecipeHash})
	if e != nil || l == nil {
		t.Fatal("observer claim", e)
	}
	started, e := s.StartWorker(context.Background(), p, workerRequest(l))
	if e != nil {
		t.Fatal(e)
	}
	return &started
}
func registryPrincipal(t *testing.T, s *Service) workerconfig.Principal {
	t.Helper()
	p, e := s.externalWorkers.Authenticate(registryTestToken, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func registryReport(t *testing.T, l *domain.WorkerLease, complete bool) domain.WorkerReport {
	t.Helper()
	in := l.Job.Inputs.Registry
	g := domain.RegistryGraph{Nodes: []domain.RegistryNode{}, Edges: []domain.RegistryEdge{}, Roots: []domain.RegistryRoot{}, Discovery: "COMPLETE", TagSetHash: registryDigest(99), Blockers: []string{"registry_global_inventory_unproven"}}
	bytes := int64(0)
	cfg := registryDigest(200)
	if complete {
		g.Nodes = append(g.Nodes, domain.RegistryNode{Digest: cfg, Kind: "BLOB", MediaType: "application/vnd.oci.image.config.v1+json", Size: 32, Availability: "PRESENT"})
		bytes += 32
	}
	for _, root := range in.Roots {
		if complete {
			g.Nodes = append(g.Nodes, domain.RegistryNode{Digest: root, Kind: "MANIFEST", MediaType: "application/vnd.oci.image.manifest.v1+json", Size: 64, Availability: "PRESENT"})
			g.Edges = append(g.Edges, domain.RegistryEdge{From: root, To: cfg, Kind: "CONFIG"})
			g.Roots = append(g.Roots, domain.RegistryRoot{Digest: root, Closure: "COMPLETE", Blockers: []string{}})
			bytes += 64
		} else {
			g.Nodes = append(g.Nodes, domain.RegistryNode{Digest: root, Kind: "UNKNOWN", Availability: "UNKNOWN"})
			g.Roots = append(g.Roots, domain.RegistryRoot{Digest: root, Closure: "UNKNOWN", Blockers: []string{"registry_request_failed"}})
		}
	}
	if !complete {
		g.Discovery = "PARTIAL"
		g.Blockers = append(g.Blockers, "registry_request_failed")
	}
	var e error
	g, e = domain.NormalizeRegistryGraph(g)
	if e != nil {
		t.Fatal(e)
	}
	g.GraphHash = domain.RegistryGraphHash(g)
	now := time.Now().UTC()
	return domain.WorkerReport{WorkerAttemptRequest: workerRequest(l), Result: "PASSED", Registry: &domain.WorkerRegistryReport{Kind: domain.RegistryObservationKind, Scope: in.Scope, RecipeID: l.Job.Inputs.RecipeID, RecipeRevision: l.Job.Inputs.RecipeRevision, RecipeHash: l.Job.Inputs.RecipeHash, ConfigRevision: l.Job.Inputs.ConfigRevision, RootSetHash: in.RootSetHash, StartedAt: now, FinishedAt: now, Requests: len(g.Nodes) + 1, Bytes: bytes, Graph: g}}
}
func registryQuery(t *testing.T, s *Service) domain.RetentionReport {
	t.Helper()
	v, e := s.Query(context.Background(), "get_artifact_retention", "p")
	if e != nil {
		t.Fatal(e)
	}
	return v.(domain.RetentionReport)
}
func TestRegistryObservationAdmissionReplayCurrentCoverageLocalPostgres(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			s, store, reopen := registryFixture(t, kind)
			defer func() { store.Close() }()
			command := registryQueueCommand(s, "registry-idempotent-request-0001")
			v, e := s.Execute(context.Background(), command)
			if e != nil {
				t.Fatal(e)
			}
			original := v.(domain.WorkerJob)
			v, e = s.Execute(context.Background(), command)
			if e != nil || v.(domain.WorkerJob).ID != original.ID {
				t.Fatal("queue replay", e)
			}
			p := registryPrincipal(t, s)
			lease, e := s.ClaimWorker(context.Background(), p, domain.WorkerClaim{ProductID: "p", RepositoryID: "r", Capability: "registry_observe"})
			if e != nil || lease == nil {
				t.Fatal(e)
			}
			if _, e = s.StartWorker(context.Background(), p, workerRequest(lease)); e != nil {
				t.Fatal(e)
			}
			report := registryReport(t, lease, true)
			if _, e = s.ReportWorker(context.Background(), p, report); e != nil {
				t.Fatal(e)
			}
			before, e := store.Read(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			if len(before.RegistryInventories) != 1 {
				t.Fatal("inventory missing")
			}
			inventory := before.RegistryInventories[0]
			query := registryQuery(t, s)
			if len(query.Registry) != 1 || query.Registry[0].Status != "CURRENT" || query.Registry[0].CoveredRoots != 1 || query.DeletionAllowed || query.GlobalInventoryComplete {
				t.Fatal("first observation self-invalidated", query.Registry)
			}
			if query.InputHash == lease.Job.Inputs.Registry.RequestRetentionInputHash {
				t.Fatal("fixture did not change retention evidence on admission")
			}
			if _, e = s.ReportWorker(context.Background(), p, report); e != nil {
				t.Fatal("accepted report replay", e)
			}
			after, _ := store.Read(context.Background())
			if len(after.Events) != len(before.Events) || len(after.RegistryInventories) != 1 || !reflect.DeepEqual(after.RegistryInventories[0], inventory) {
				t.Fatal("replay renewed observation/admission")
			}
			registryPin(t, s, registryDigest(1), "second-pin-already-covered")
			if registryQuery(t, s).Registry[0].Status != "CURRENT" {
				t.Fatal("same covered root unnecessarily refetched")
			}
			registryPin(t, s, registryDigest(2), "new-uncovered-root")
			if registryQuery(t, s).Registry[0].Status != "PARTIAL" {
				t.Fatal("uncovered root falsely current")
			}
			store.Close()
			store = reopen()
			s = New(store)
			s.ConfigureWorkers(registryAuthority(t, registryRecipe(), "observe-cfg", time.Now().Add(time.Hour), false))
			if registryQuery(t, s).Registry[0].Status != "PARTIAL" {
				t.Fatal("reopen changed factual coverage")
			}
			// A newer partial scan degrades coverage but cannot remove known reachable bytes.
			lease = registryQueue(t, s)
			partial := registryReport(t, lease, false)
			if _, e = s.ReportWorker(context.Background(), registryPrincipal(t, s), partial); e != nil {
				t.Fatal(e)
			}
			query = registryQuery(t, s)
			found := false
			for _, o := range query.ProtectedObjects {
				found = found || o.Digest == registryDigest(200) && o.Protected
			}
			if !found || query.Registry[0].Status != "PARTIAL" {
				t.Fatal("partial scan unprotected historical config blob", query)
			}
			s.ConfigureWorkers(registryAuthority(t, registryRecipe(), "rotated-cfg", time.Now().Add(time.Hour), false))
			if registryQuery(t, s).Registry[0].Status == "CURRENT" {
				t.Fatal("config drift retained current coverage")
			}
		})
	}
}
func TestRegistryObservationRejectsReportAndAuthorityDrift(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		for _, mode := range []string{"graph-hash", "scope", "foreign-ref", "future", "before-attempt", "cancel", "lease", "config", "revoked", "expired", "repository-drift", "wrong-kind", "source-receipt"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				s, store, _ := registryFixture(t, kind)
				defer store.Close()
				lease := registryQueue(t, s)
				p := registryPrincipal(t, s)
				report := registryReport(t, lease, true)
				switch mode {
				case "graph-hash":
					report.Registry.Graph.GraphHash = registryDigest(777)
				case "scope":
					report.Registry.Scope.ProductID = "other"
				case "foreign-ref":
					report.Registry.Graph.Edges[0].To = registryDigest(999)
					report.Registry.Graph.GraphHash = domain.RegistryGraphHash(report.Registry.Graph)
				case "future":
					report.Registry.FinishedAt = time.Now().Add(time.Hour)
				case "before-attempt":
					report.Registry.StartedAt = lease.Attempt.CreatedAt.Add(-time.Second)
				case "cancel":
					if _, e := s.Execute(context.Background(), domain.Command{Action: "cancel_worker_job", Actor: "operator", ProductID: "p", ID: lease.Job.ID, Data: map[string]any{}}); e != nil {
						t.Fatal(e)
					}
				case "lease":
					expireWorker(t, store, lease.Attempt.ID)
				case "config":
					s.ConfigureWorkers(registryAuthority(t, registryRecipe(), "new-config", time.Now().Add(time.Hour), false))
				case "revoked":
					s.ConfigureWorkers(registryAuthority(t, registryRecipe(), "observe-cfg", time.Now().Add(time.Hour), true))
				case "expired":
					s.ConfigureWorkers(registryAuthority(t, registryRecipe(), "observe-cfg", time.Now().Add(-time.Second), false))
				case "repository-drift":
					if e := store.Update(context.Background(), func(st *domain.State) error { st.Repositories[0].URL = "https://github.com/test/changed"; return nil }); e != nil {
						t.Fatal(e)
					}
				case "wrong-kind":
					report.Registry.Kind = "SERVER_VERIFIED"
				case "source-receipt":
					report.LocalOCI = &domain.WorkerLocalOCI{}
				}
				if _, e := s.ReportWorker(context.Background(), p, report); e == nil {
					t.Fatal("invalid/stale registry observation admitted", mode)
				}
				st, e := store.Read(context.Background())
				if e != nil {
					t.Fatal(e)
				}
				if len(st.RegistryInventories) != 0 {
					t.Fatal("rejected report persisted inventory")
				}
			})
		}
	}
}
func TestRegistryObservationDurableReferenceCorruption(t *testing.T) {
	s, store, _ := registryFixture(t, "local")
	defer store.Close()
	lease := registryQueue(t, s)
	report := registryReport(t, lease, true)
	if _, e := s.ReportWorker(context.Background(), registryPrincipal(t, s), report); e != nil {
		t.Fatal(e)
	}
	original, e := store.Read(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	for _, mode := range []string{"hash", "attempt", "received", "scope", "graph", "operation"} {
		t.Run(mode, func(t *testing.T) {
			raw, _ := json.Marshal(original)
			var bad domain.State
			json.Unmarshal(raw, &bad)
			v := &bad.RegistryInventories[0]
			switch mode {
			case "hash":
				v.InputHash = registryDigest(77)
			case "attempt":
				v.AttemptID = "other"
			case "received":
				v.ReceivedAt = v.ReceivedAt.Add(time.Nanosecond)
			case "scope":
				v.Report.Scope.ProductID = "other"
			case "graph":
				v.Report.Graph.Nodes[0].Size++
			case "operation":
				bad.Operations[len(bad.Operations)-1].RegistryObservation = nil
			}
			if domain.ValidateRegistryInventories(bad) == nil || validateBackupState(bad) == nil {
				t.Fatal("corrupt inventory survived store/backup validation")
			}
		})
	}
}

func TestRegistryObservationRestoreQuarantinesHistoryAndActiveLease(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			s, store, _ := registryFixture(t, kind)
			defer store.Close()
			completed := registryQueue(t, s)
			if _, e := s.ReportWorker(context.Background(), registryPrincipal(t, s), registryReport(t, completed, true)); e != nil {
				t.Fatal(e)
			}
			active := registryQueue(t, s)
			backup, e := s.CreateBackup(context.Background(), "fixture/operator")
			if e != nil {
				t.Fatal(e)
			}
			destination, _ := openWorkerTestStore(t, kind)
			defer destination.Close()
			restored := New(destination)
			restored.ConfigureWorkers(registryAuthority(t, registryRecipe(), "observe-cfg", time.Now().Add(time.Hour), false))
			if _, e = restored.RestoreBackup(context.Background(), "fixture/operator", backup.ArchiveBase64, backup.SHA256); e != nil {
				t.Fatal(e)
			}
			state, e := destination.Read(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			if len(state.RegistryInventories) != 1 || len(state.NativeQuarantines) != 1 {
				t.Fatal("restore lost inventory or quarantine")
			}
			for _, j := range state.WorkerJobs {
				if j.ID == active.Job.ID && j.Status != "RECONCILIATION_REQUIRED" {
					t.Fatal("restored active observer executable", j.Status)
				}
			}
			report := registryQuery(t, restored)
			if len(report.Registry) != 1 || report.Registry[0].Status != "QUARANTINED" || len(report.ProtectedObjects) == 0 {
				t.Fatal("restore resurrected coverage or lost protected graph", report.Registry)
			}
			if _, e = restored.ReportWorker(context.Background(), registryPrincipal(t, restored), registryReport(t, active, true)); e == nil {
				t.Fatal("restored lease admitted report")
			}
			if l, e := restored.ClaimWorker(context.Background(), registryPrincipal(t, restored), domain.WorkerClaim{ProductID: "p", RepositoryID: "r", Capability: "registry_observe"}); e == nil && l != nil {
				t.Fatal("restored observer claimed")
			}
		})
	}
}
