package application

import (
	"context"
	"encoding/json"
	"errors"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
	"strings"
	"testing"
	"time"
)

type fakeChecks struct {
	*fakeDelivery
	current                                delivery.CheckRunObservation
	creates, updates, reads, finds         int
	lostCreate, lostUpdate, missing, wrong bool
	during                                 func()
}

func TestWorkerChecksLongRunningWaitDoesNotSpendObservationBudget(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			s, store, _, p := checksFixture(t, kind)
			defer store.Close()
			j := checksQueue(t, s, time.Now().Add(40*time.Minute))
			checkTick(t, s, store)
			l := checksClaim(t, s)
			if _, e := s.StartWorker(context.Background(), workerIdentity(t, s), workerRequest(l)); e != nil {
				t.Fatal(e)
			}
			checkTick(t, s, store)
			st, _ := store.Read(context.Background())
			before := operationByID(&st, j.CheckOperationID).CheckPublication.Observations
			writes := p.creates + p.updates
			for range 70 {
				if _, e := s.HeartbeatWorker(context.Background(), workerIdentity(t, s), workerRequest(l)); e != nil {
					t.Fatal(e)
				}
				checkTick(t, s, store)
			}
			st, _ = store.Read(context.Background())
			after := operationByID(&st, j.CheckOperationID)
			if after.Status != "RUNNING" || after.CheckPublication.Observations != before || p.creates+p.updates != writes || p.reads != 0 || p.finds != 0 {
				t.Fatal("ordinary execution wait exhausted provider observation budget")
			}
		})
	}
}

func TestWorkerChecksCancellationUnknownAndLateTimeoutGrace(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		for _, mode := range []string{"cancelled", "unknown", "late-timeout", "budget"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				s, store, reopen, p := checksFixture(t, kind)
				defer func() { store.Close() }()
				deadline := time.Time{}
				if mode == "late-timeout" {
					deadline = time.Now().Add(60 * time.Millisecond)
				}
				j := checksQueue(t, s, deadline)
				if mode == "late-timeout" {
					time.Sleep(80 * time.Millisecond)
					store.Close()
					store = reopen()
					s = NewWithProvider(store, p)
					s.ConfigureWorkers(checksAuthority(t))
					checkTick(t, s, store)
					checkTick(t, s, store)
					if p.current.Conclusion != "timed_out" {
						t.Fatal("timeout first discovered after deadline not finalized during grace", p.current)
					}
					if l, e := s.ClaimWorker(context.Background(), workerIdentity(t, s), domain.WorkerClaim{ProductID: "p", RepositoryID: "r", Capability: "local_oci_build"}); e != nil || l != nil {
						t.Fatal("execution deadline extended by reporting grace")
					}
					return
				}
				checkTick(t, s, store)
				if mode == "budget" {
					p.lostUpdate = true
					l := checksClaim(t, s)
					if _, e := s.StartWorker(context.Background(), workerIdentity(t, s), workerRequest(l)); e != nil {
						t.Fatal(e)
					}
					checkTick(t, s, store)
					p.missing = true
					if e := store.Update(context.Background(), func(st *domain.State) error {
						operationByID(st, j.CheckOperationID).CheckPublication.Observations = 64
						return nil
					}); e != nil {
						t.Fatal(e)
					}
					writes := p.creates + p.updates
					checkTick(t, s, store)
					st, _ := store.Read(context.Background())
					if operationByID(&st, j.CheckOperationID).Status != "FAILED" || writes != p.creates+p.updates {
						t.Fatal("exhausted observation budget wrote again")
					}
					return
				}
				l := checksClaim(t, s)
				if _, e := s.StartWorker(context.Background(), workerIdentity(t, s), workerRequest(l)); e != nil {
					t.Fatal(e)
				}
				checkTick(t, s, store)
				if mode == "unknown" {
					expireWorker(t, store, l.Attempt.ID)
					checkTick(t, s, store)
					if p.current.Status != "in_progress" || p.current.Conclusion != "" {
						t.Fatal("unknown execution became green", p.current)
					}
					st, _ := store.Read(context.Background())
					if workerJob(&st, j.ID).Status != "RECONCILIATION_REQUIRED" {
						t.Fatal("unknown execution replayable")
					}
					return
				}
				if _, e := s.Execute(context.Background(), domain.Command{Action: "cancel_worker_job", Actor: "fixture", ProductID: "p", ID: j.ID}); e != nil {
					t.Fatal(e)
				}
				checkTick(t, s, store)
				if p.current.Conclusion != "cancelled" {
					t.Fatal("cancelled execution not reported truthfully", p.current)
				}
			})
		}
	}
}

func TestWorkerChecksRegisteredAliasOwnershipAndAtomicAdmission(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			s, store, _, p := checksFixture(t, kind)
			defer store.Close()
			r1 := checkRecipe()
			r2 := checkRecipe()
			r2.ID = "other-check"
			r2.ProductID = "p2"
			r2.RepositoryID = "r2"
			r2.RepositoryURL = "https://github.com/test/alias"
			s.ConfigureWorkers(checksAuthority(t, r1, r2))
			if e := store.Update(context.Background(), func(st *domain.State) error {
				st.Products = append(st.Products, domain.Product{Meta: domain.Meta{ID: "p2"}, Name: "other"})
				st.Repositories = append(st.Repositories, domain.Repository{Meta: domain.Meta{ID: "r2", ProductID: "p2"}, URL: r2.RepositoryURL, Role: "APPLICATION", RegisteredRepositoryID: "registered"})
				st.RepositoryBindings = append(st.RepositoryBindings, domain.RepositoryBinding{Meta: domain.Meta{ID: "alias-binding", ProductID: "p2"}, RepositoryID: "r2", ConnectionID: "source-conn", Role: "APPLICATION", FullName: "test/alias"})
				return nil
			}); e != nil {
				t.Fatal(e)
			}
			cmd := queueWorkerCommand(s)
			cmd.Data["parameters"] = map[string]any{}
			if _, e := s.Execute(context.Background(), cmd); e == nil {
				t.Fatal("cross-Product numeric physical repository alias stole check name")
			}
			st, _ := store.Read(context.Background())
			if len(st.WorkerJobs) != 0 || len(st.Operations) != 0 || p.creates != 0 {
				t.Fatal("failed check admission left an orphan")
			}
			s.ConfigureWorkers(checksAuthority(t))
			checksQueue(t, s, time.Time{})
			checkTick(t, s, store)
			if p.creates != 1 {
				t.Fatal("disabled historical/configured owner locked check name forever")
			}
		})
	}
}

func TestWorkerChecksTypedReferenceAndPayloadValidation(t *testing.T) {
	s, store, _, _ := checksFixture(t, "local")
	defer store.Close()
	j := checksQueue(t, s, time.Time{})
	checkTick(t, s, store)
	original, _ := store.Read(context.Background())
	for _, mutate := range []func(*domain.State){func(st *domain.State) { workerJob(st, j.ID).CheckOperationID = "foreign" }, func(st *domain.State) { operationByID(st, j.CheckOperationID).ParentID = "foreign" }, func(st *domain.State) { operationByID(st, j.CheckOperationID).CheckPublication.JobID = "foreign" }, func(st *domain.State) { operationByID(st, j.CheckOperationID).CheckPublication.Identity.RepositoryID++ }, func(st *domain.State) { operationByID(st, j.CheckOperationID).CheckPublication.Identity.AppID++ }, func(st *domain.State) {
		operationByID(st, j.CheckOperationID).CheckPublication.Identity.ExternalID = "other"
	}, func(st *domain.State) {
		operationByID(st, j.CheckOperationID).CheckPublication.Identity.HeadSHA = strings.Repeat("c", 40)
	}, func(st *domain.State) {
		operationByID(st, j.CheckOperationID).CheckPublication.FinalizeDeadline = time.Now()
	}, func(st *domain.State) { operationByID(st, j.CheckOperationID).CheckPublication.IntentHash = "wrong" }, func(st *domain.State) {
		p := operationByID(st, j.CheckOperationID).CheckPublication
		p.Intent.Output.Summary = "untrusted raw output"
		p.IntentHash = workerconfig.Fingerprint(*p.Intent)
	}, func(st *domain.State) {
		operationByID(st, j.CheckOperationID).CheckPublication.Observed.Identity.Name = "foreign"
	}} {
		b, _ := json.Marshal(original)
		var bad domain.State
		json.Unmarshal(b, &bad)
		mutate(&bad)
		if domain.ValidateStateStatuses(bad) == nil || validateBackupState(bad) == nil {
			t.Fatal("corrupt check reference/intent survived store or backup validation")
		}
	}
}

func TestWorkerChecksPostIOAuthorityLeaseAndPhysicalFences(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		for _, mode := range []string{"config", "physical", "quarantine", "lease", "equivalent-api"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				s, store, _, p := checksFixture(t, kind)
				defer store.Close()
				j := checksQueue(t, s, time.Time{})
				p.during = func() {
					if mode == "config" {
						r := checkRecipe()
						r.Check.FinalizeSeconds = 301
						s.ConfigureWorkers(checksAuthority(t, r))
						return
					}
					if e := store.Update(context.Background(), func(st *domain.State) error {
						switch mode {
						case "physical":
							st.RegisteredRepositories[0].ProviderRepositoryID++
						case "equivalent-api":
							st.GitHubConnections[0].Config.APIURL = ""
						case "quarantine":
							st.NativeQuarantines = append(st.NativeQuarantines, domain.NativeQuarantine{Meta: domain.Meta{ID: "quarantine", ProductID: "p"}, RestoreID: "fixture"})
						case "lease":
							operationByID(st, j.CheckOperationID).LeaseUntil = time.Now().Add(-time.Second)
						}
						return nil
					}); e != nil {
						t.Fatal(e)
					}
				}
				_, e := s.Tick(context.Background())
				if mode == "lease" && e == nil {
					t.Fatal("expired check operation lease admitted response")
				}
				if mode != "lease" && e != nil {
					t.Fatal(e)
				}
				st, _ := store.Read(context.Background())
				op := operationByID(&st, j.CheckOperationID)
				if op.CheckPublication.Observed != nil || checkInitialReady(&st, workerJob(&st, j.ID)) {
					t.Fatal("post-I/O stale authority enabled execution")
				}
				if mode != "lease" && op.Status != "FAILED" {
					t.Fatal("authority loss hidden from attention")
				}
			})
		}
	}
}

func TestWorkerChecksAcceptedSuccessFinalizesDuringGraceAndOldSHA(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			s, store, reopen, p := checksFixture(t, kind)
			defer func() { store.Close() }()
			j := checksQueue(t, s, time.Now().Add(2*time.Second))
			checkTick(t, s, store)
			l := checksClaim(t, s)
			if _, e := s.StartWorker(context.Background(), workerIdentity(t, s), workerRequest(l)); e != nil {
				t.Fatal(e)
			}
			if _, e := s.ReportWorker(context.Background(), workerIdentity(t, s), checksReport(l, "PASSED")); e != nil {
				t.Fatal(e)
			}
			if _, e := s.Execute(context.Background(), domain.Command{Action: "record_integration_revision", ID: "new-revision", Actor: "fixture", IntegrationID: "i", Data: map[string]any{"repository_id": "r", "branch": "feature", "base_commit": strings.Repeat("b", 40), "head_commit": strings.Repeat("c", 40), "commits": []string{strings.Repeat("c", 40)}}}); e != nil {
				t.Fatal(e)
			}
			time.Sleep(time.Until(j.Deadline) + 20*time.Millisecond)
			store.Close()
			store = reopen()
			s = NewWithProvider(store, p)
			s.ConfigureWorkers(checksAuthority(t))
			checkTick(t, s, store)
			if p.current.Conclusion != "success" || p.current.Identity.HeadSHA != j.Inputs.SourceSHA {
				t.Fatal("accepted old SHA not finalized truthfully during grace", p.current)
			}
		})
	}
}

func TestWorkerChecksRestoreActiveAndCompletedMetadataCannotWrite(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		for _, complete := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/active", true: "/completed"}[complete], func(t *testing.T) {
				s, store, _, p := checksFixture(t, kind)
				defer store.Close()
				checksQueue(t, s, time.Time{})
				checkTick(t, s, store)
				if complete {
					l := checksClaim(t, s)
					if _, e := s.StartWorker(context.Background(), workerIdentity(t, s), workerRequest(l)); e != nil {
						t.Fatal(e)
					}
					if _, e := s.ReportWorker(context.Background(), workerIdentity(t, s), checksReport(l, "PASSED")); e != nil {
						t.Fatal(e)
					}
					checkTick(t, s, store)
				}
				backup, e := s.CreateBackup(context.Background(), "fixture")
				if e != nil {
					t.Fatal(e)
				}
				dest, m := fixture(t)
				m.state = domain.EmptyState()
				dest.provider = p
				dest.ConfigureWorkers(checksAuthority(t))
				if _, e = dest.RestoreBackup(context.Background(), "fixture", backup.ArchiveBase64, backup.SHA256); e != nil {
					t.Fatal(e)
				}
				writes := p.creates + p.updates
				for range 3 {
					if _, e = dest.Tick(context.Background()); e != nil {
						t.Fatal(e)
					}
				}
				if p.creates+p.updates != writes {
					t.Fatal("restore replayed provider writes")
				}
				st, _ := dest.store.Read(context.Background())
				if !nativeQuarantined(&st, "p") {
					t.Fatal("restore did not quarantine check owner")
				}
			})
		}
	}
}

func TestWorkerChecksManagedTerminalFailureUsesImmutableOrigin(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		for _, outcome := range []string{"failure", "timed_out"} {
			t.Run(kind+"/"+outcome, func(t *testing.T) {
				s, store, _, p := checksFixture(t, kind)
				defer store.Close()
				j := checksQueue(t, s, time.Time{})
				checkTick(t, s, store)
				l := checksClaim(t, s)
				if _, e := s.StartWorker(context.Background(), workerIdentity(t, s), workerRequest(l)); e != nil {
					t.Fatal(e)
				}
				if _, e := s.ReportWorker(context.Background(), workerIdentity(t, s), checksReport(l, "CHECK_FAILED")); e != nil {
					t.Fatal(e)
				}
				if e := store.Update(context.Background(), func(st *domain.State) error {
					job := workerJob(st, j.ID)
					child := operationByID(st, j.CheckOperationID)
					cp := child.CheckPublication
					origin := domain.ExternalOperation{Meta: domain.Meta{ID: "managed-origin", ProductID: job.ProductID, FeatureID: job.FeatureID}, IntegrationID: job.IntegrationID, Kind: "DEPLOY", Status: "FAILED", Phase: "NATIVE_BUILD", Native: &domain.NativeDelivery{BuildJobID: job.ID}, Snapshot: &domain.DeliverySnapshot{Revision: st.IntegrationRevisions[0], Source: cp.Source, SourceConnection: cp.Connection}}
					job.Inputs.SourceProof = nativeSourceProof(&origin)
					job.InputHash = workerconfig.Fingerprint(job.Inputs)
					cp.Inputs = job.Inputs
					cp.InputHash = job.InputHash
					// Reset only the synthetic fixture's initial publication snapshot to
					// model the accepted managed origin before provider registration.
					cp.Observed = nil
					cp.Intent = nil
					cp.IntentHash = ""
					cp.Observations = 0
					child.Phase = "CHECK_CREATE_READY"
					a := currentWorkerAttempt(st, job)
					a.InputHash = job.InputHash
					a.Report.InputHash = job.InputHash
					if outcome == "timed_out" {
						job.Status = "DEADLINE_EXCEEDED"
						job.Deadline = time.Now().UTC().Add(-time.Second)
						job.CreatedAt = job.Deadline.Add(-time.Minute)
						cp.FinalizeDeadline = job.Deadline.Add(time.Duration(job.Inputs.Check.FinalizeSeconds) * time.Second)
						a.Status = "EXPIRED"
						a.Report = nil
					}
					operationByID(st, job.OperationID).ParentID = origin.ID
					st.Operations = append(st.Operations, origin)
					return nil
				}); e != nil {
					t.Fatal(e)
				}
				checkTick(t, s, store)
				checkTick(t, s, store)
				if p.current.Conclusion != outcome {
					t.Fatal("natural failed managed parent erased accepted CHECK_FAILED outcome", p.current)
				}
			})
		}
	}

}

func TestWorkerChecksPersistedSuccessRequiresAcceptedTypedReport(t *testing.T) {
	s, store, _, _ := checksFixture(t, "local")
	defer store.Close()
	j := checksQueue(t, s, time.Time{})
	checkTick(t, s, store)
	st, _ := store.Read(context.Background())
	op := operationByID(&st, j.CheckOperationID)
	cp := op.CheckPublication
	w := *cp.Intent
	w.Status = "completed"
	w.Conclusion = "success"
	w.ID = cp.Observed.ID
	w.StartedAt = j.CreatedAt.UTC().Truncate(time.Second)
	w.CompletedAt = w.StartedAt
	w.Output.Title = "RCP checks/build: success"
	cp.Intent = &w
	cp.IntentHash = workerconfig.Fingerprint(w)
	cp.Observed = &delivery.CheckRunObservation{Found: true, ID: w.ID, Identity: w.Identity, Status: w.Status, Conclusion: w.Conclusion, StartedAt: w.StartedAt, CompletedAt: w.CompletedAt, Output: w.Output}
	op.Status = "SUCCEEDED"
	if domain.ValidateStateStatuses(st) == nil || validateBackupState(st) == nil {
		t.Fatal("persisted provider success manufactured without an accepted build receipt")
	}
}

func TestWorkerChecksClaimedDeadlineAndAcceptedFailureClassification(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		for _, boundary := range []string{"after-deadline", "lease-only-before-deadline"} {
			t.Run(kind+"/"+boundary, func(t *testing.T) {
				s, store, reopen, p := checksFixture(t, kind)
				defer func() { store.Close() }()
				cmd := queueWorkerCommand(s)
				cmd.Data["parameters"] = map[string]any{}
				cmd.Data["max_attempts"] = 1
				v, e := s.Execute(context.Background(), cmd)
				if e != nil {
					t.Fatal(e)
				}
				j := v.(domain.WorkerJob)
				checkTick(t, s, store)
				l := checksClaim(t, s)
				if e = store.Update(context.Background(), func(st *domain.State) error {
					job := workerJob(st, j.ID)
					a := workerAttempt(st, l.Attempt.ID)
					a.LeaseUntil = time.Now().UTC().Add(-time.Second)
					if boundary == "after-deadline" {
						job.Deadline = time.Now().UTC().Add(-time.Second)
						job.CreatedAt = job.Deadline.Add(-time.Minute)
						operationByID(st, j.CheckOperationID).CheckPublication.FinalizeDeadline = job.Deadline.Add(time.Duration(job.Inputs.Check.FinalizeSeconds) * time.Second)
					}
					return nil
				}); e != nil {
					t.Fatal(e)
				}
				store.Close()
				store = reopen()
				s = NewWithProvider(store, p)
				s.ConfigureWorkers(checksAuthority(t))
				checkTick(t, s, store)
				expected := "failure"
				if boundary == "after-deadline" {
					expected = "timed_out"
				}
				if p.current.Conclusion != expected {
					t.Fatalf("CLAIMED expiry classified %q, want %q", p.current.Conclusion, expected)
				}
				st, _ := store.Read(context.Background())
				if workerJob(&st, j.ID).Status != "FAILED" || workerAttempt(&st, l.Attempt.ID).Status != "EXPIRED" || workerAttempt(&st, l.Attempt.ID).Report != nil {
					t.Fatal("classification changed execution/reconciliation provenance")
				}
				if boundary == "lease-only-before-deadline" {
					w, e := s.desiredCheck(&st, operationByID(&st, j.CheckOperationID), workerJob(&st, j.ID).Deadline.Add(time.Second))
					if e != nil || w.Conclusion != "failure" {
						t.Fatal("earlier lease-only failure was later relabeled as execution timeout", w, e)
					}
				}
			})
		}
		for _, result := range []string{"CHECK_FAILED", "EXECUTOR_FAILED"} {
			t.Run(kind+"/accepted-"+result, func(t *testing.T) {
				s, store, _, _ := checksFixture(t, kind)
				defer store.Close()
				j := checksQueue(t, s, time.Time{})
				checkTick(t, s, store)
				l := checksClaim(t, s)
				if _, e := s.StartWorker(context.Background(), workerIdentity(t, s), workerRequest(l)); e != nil {
					t.Fatal(e)
				}
				if _, e := s.ReportWorker(context.Background(), workerIdentity(t, s), checksReport(l, result)); e != nil {
					t.Fatal(e)
				}
				st, _ := store.Read(context.Background())
				w, e := s.desiredCheck(&st, operationByID(&st, j.CheckOperationID), j.Deadline.Add(time.Second))
				if e != nil || w.Conclusion != "failure" {
					t.Fatal("accepted failure relabeled as later timeout", w, e)
				}
			})
		}
	}
}

func (f *fakeChecks) echo(w delivery.CheckRunWrite) delivery.CheckRunObservation {
	return delivery.CheckRunObservation{Found: true, ID: 91, Identity: w.Identity, Status: w.Status, Conclusion: w.Conclusion, StartedAt: w.StartedAt, CompletedAt: w.CompletedAt, Output: w.Output}
}
func (f *fakeChecks) CreateCheckRun(_ context.Context, _ delivery.Connection, w delivery.CheckRunWrite) (delivery.CheckRunObservation, error) {
	f.creates++
	f.current = f.echo(w)
	if f.during != nil {
		f.during()
	}
	if f.lostCreate {
		return delivery.CheckRunObservation{}, errors.New("fixture lost response")
	}
	return f.current, nil
}
func (f *fakeChecks) UpdateCheckRun(_ context.Context, _ delivery.Connection, w delivery.CheckRunWrite) (delivery.CheckRunObservation, error) {
	f.updates++
	f.current = f.echo(w)
	if f.during != nil {
		f.during()
	}
	if f.lostUpdate {
		return delivery.CheckRunObservation{}, errors.New("fixture lost response")
	}
	return f.current, nil
}
func (f *fakeChecks) FindCheckRun(context.Context, delivery.Connection, delivery.CheckRunIdentity) (delivery.CheckRunObservation, error) {
	f.finds++
	if f.missing {
		return delivery.CheckRunObservation{}, nil
	}
	v := f.current
	if f.wrong {
		v.Identity.AppID++
	}
	return v, nil
}
func (f *fakeChecks) ReadCheckRun(context.Context, delivery.Connection, delivery.CheckRunIdentity, int64) (delivery.CheckRunObservation, error) {
	f.reads++
	if f.missing {
		return delivery.CheckRunObservation{}, nil
	}
	return f.current, nil
}
func checkRecipe() workerconfig.Recipe {
	return workerconfig.Recipe{ID: "check", Revision: "v1", ProductID: "p", RepositoryID: "r", RepositoryURL: "https://github.com/test/repo", Capability: "local_oci_build", Trust: "trusted_source", Check: &domain.WorkerCheckBinding{Name: "RCP / checks and build", ExpectedAppID: 7, FinalizeSeconds: 300}, Parameters: map[string]workerconfig.Parameter{}, Execution: &domain.WorkerExecution{Kind: "local_oci_build", ToolchainImage: "golang@sha256:" + strings.Repeat("a", 64), BuildKitImage: "moby/buildkit@sha256:" + strings.Repeat("b", 64), Platform: "linux/arm64", CheckArgv: []string{"make", "check"}, BuildArgv: []string{"make", "build"}, OutputFile: "app", ImagePath: "app", Entrypoint: []string{"/app"}, Network: "none", TimeoutSeconds: 60, MemoryMB: 256, StorageMB: 64, CPUs: 1, PIDs: 64, SourceBytes: 1 << 20, OutputBytes: 4 << 20, LogBytes: 4096}}
}
func checksAuthority(t *testing.T, recipes ...workerconfig.Recipe) *workerconfig.Authority {
	t.Helper()
	if len(recipes) == 0 {
		recipes = []workerconfig.Recipe{checkRecipe()}
	}
	for i := range recipes {
		recipes[i].Hash = workerconfig.RecipeHash(recipes[i])
	}
	a, e := workerconfig.New(workerconfig.Config{Revision: "cfg1", LeaseSeconds: 5, Recipes: recipes, Credentials: []workerconfig.Credential{{ID: "runner", Principal: "worker/one", TokenEnv: "CHECK_FIXTURE_WORKER_TOKEN", ProductIDs: []string{"p", "p2"}, RepositoryIDs: []string{"r", "r2"}, Capabilities: []string{"local_oci_build"}, ExpiresAt: time.Now().Add(time.Hour)}}}, "control", func(string) string { return workerTestToken })
	if e != nil {
		t.Fatal(e)
	}
	return a
}
func checksFixture(t *testing.T, kind string) (*Service, workerTestStore, func() workerTestStore, *fakeChecks) {
	t.Helper()
	store, reopen := openWorkerTestStore(t, kind)
	p := &fakeChecks{fakeDelivery: &fakeDelivery{}}
	s := NewWithProvider(store, p)
	s.ConfigureWorkers(checksAuthority(t))
	workerSeed(t, s)
	if e := store.Update(context.Background(), func(st *domain.State) error {
		st.GitHubConnections = append(st.GitHubConnections, domain.GitHubConnection{Meta: domain.Meta{ID: "source-conn", ProductID: "p"}, Config: delivery.Connection{ID: "source-conn", AppID: 7, InstallationID: 8, Owner: "test", APIURL: "https://api.github.com", PrivateKeyRef: "env:FAKE_APP_KEY"}})
		st.RepositoryBindings = append(st.RepositoryBindings, domain.RepositoryBinding{Meta: domain.Meta{ID: "source-binding", ProductID: "p"}, RepositoryID: "r", ConnectionID: "source-conn", FullName: "test/repo", Role: "APPLICATION", DefaultBranch: "main"})
		st.RegisteredRepositories = append(st.RegisteredRepositories, domain.RegisteredRepository{Meta: domain.Meta{ID: "registered"}, ProviderRepositoryID: 42, FullName: "test/repo", ProviderAPIURL: "https://api.github.com", ConnectionID: "source-conn", ConnectionIDs: []string{"source-conn"}})
		st.Repositories[0].RegisteredRepositoryID = "registered"
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	return s, store, reopen, p
}
func checksQueue(t *testing.T, s *Service, deadline time.Time) domain.WorkerJob {
	t.Helper()
	cmd := queueWorkerCommand(s)
	cmd.Data["parameters"] = map[string]any{}
	if !deadline.IsZero() {
		cmd.Data["deadline"] = deadline
	}
	v, e := s.Execute(context.Background(), cmd)
	if e != nil {
		t.Fatal(e)
	}
	return v.(domain.WorkerJob)
}
func checkTick(t *testing.T, s *Service, store Store) {
	t.Helper()
	if e := store.Update(context.Background(), func(st *domain.State) error {
		for i := range st.Operations {
			if st.Operations[i].Kind == "CHECK_PUBLICATION" {
				st.Operations[i].NextAttemptAt = time.Time{}
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func checksClaim(t *testing.T, s *Service) *domain.WorkerLease {
	t.Helper()
	l, e := s.ClaimWorker(context.Background(), workerIdentity(t, s), domain.WorkerClaim{ProductID: "p", RepositoryID: "r", Capability: "local_oci_build"})
	if e != nil || l == nil {
		t.Fatal("claim", e)
	}
	return l
}
func checksReport(l *domain.WorkerLease, result string) domain.WorkerReport {
	r := domain.WorkerReport{WorkerAttemptRequest: workerRequest(l), Result: result}
	if result == "PASSED" {
		r.LocalOCI = &domain.WorkerLocalOCI{JobID: l.Job.ID, AttemptID: l.Attempt.ID, InputHash: l.Job.InputHash, SourceSHA: l.Job.Inputs.SourceSHA, RecipeHash: l.Job.Inputs.RecipeHash, ConfigRevision: l.Job.Inputs.ConfigRevision, ToolchainImage: l.Job.Inputs.Execution.ToolchainImage, Platform: l.Job.Inputs.Execution.Platform, ArchiveHash: "sha256:" + strings.Repeat("c", 64), LayoutHash: "sha256:" + strings.Repeat("d", 64), IndexDigest: "sha256:" + strings.Repeat("e", 64), ManifestDigest: "sha256:" + strings.Repeat("f", 64), ArchiveBytes: 1024}
	}
	return r
}
func TestWorkerChecksLifecycleReplayReopen(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		for _, result := range []string{"PASSED", "CHECK_FAILED", "EXECUTOR_FAILED"} {
			t.Run(kind+"/"+result, func(t *testing.T) {
				s, store, reopen, p := checksFixture(t, kind)
				defer func() { store.Close() }()
				j := checksQueue(t, s, time.Time{})
				if j.CheckOperationID == "" {
					t.Fatal("check child missing")
				}
				l, e := s.ClaimWorker(context.Background(), workerIdentity(t, s), domain.WorkerClaim{ProductID: "p", RepositoryID: "r", Capability: "local_oci_build"})
				if e != nil || l != nil {
					t.Fatal("execution claimed before observed queued check")
				}
				checkTick(t, s, store)
				l = checksClaim(t, s)
				if _, e = s.StartWorker(context.Background(), workerIdentity(t, s), workerRequest(l)); e != nil {
					t.Fatal(e)
				}
				checkTick(t, s, store)
				if p.current.Status != "in_progress" {
					t.Fatal("running execution not reflected")
				}
				report := checksReport(l, result)
				if _, e = s.ReportWorker(context.Background(), workerIdentity(t, s), report); e != nil {
					t.Fatal(e)
				}
				if _, e = s.ReportWorker(context.Background(), workerIdentity(t, s), report); e != nil {
					t.Fatal(e)
				}
				store.Close()
				store = reopen()
				s = NewWithProvider(store, p)
				s.ConfigureWorkers(checksAuthority(t))
				checkTick(t, s, store)
				expected := "failure"
				if result == "PASSED" {
					expected = "success"
				}
				if p.current.Conclusion != expected || p.creates != 1 || p.updates != 2 {
					t.Fatal("wrong provider lifecycle", p.current, p.creates, p.updates)
				}
				st, _ := store.Read(context.Background())
				if operationByID(&st, j.OperationID).Status != map[string]string{"PASSED": "SUCCEEDED", "CHECK_FAILED": "FAILED", "EXECUTOR_FAILED": "FAILED"}[result] || operationByID(&st, j.CheckOperationID).Status != "SUCCEEDED" {
					t.Fatal("passive projection confused with provider publication")
				}
				if _, e = s.ReportWorker(context.Background(), workerIdentity(t, s), report); e != nil {
					t.Fatal(e)
				}
				if len(st.WorkerJobs) != 1 || len(st.Operations) != 2 {
					t.Fatal("replay created extra work")
				}
			})
		}
	}
}
func TestWorkerChecksUnknownCreatePatchNeverRepeatsWrites(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		for _, phase := range []string{"create", "update"} {
			for _, missing := range []bool{false, true} {
				t.Run(kind+"/"+phase+map[bool]string{false: "/found", true: "/absent"}[missing], func(t *testing.T) {
					s, store, reopen, p := checksFixture(t, kind)
					defer func() { store.Close() }()
					checksQueue(t, s, time.Time{})
					p.lostCreate = phase == "create"
					checkTick(t, s, store)
					if phase == "update" {
						l := checksClaim(t, s)
						if _, e := s.StartWorker(context.Background(), workerIdentity(t, s), workerRequest(l)); e != nil {
							t.Fatal(e)
						}
						p.lostUpdate = true
						checkTick(t, s, store)
					}
					writes := p.creates + p.updates
					p.missing = missing
					store.Close()
					store = reopen()
					s = NewWithProvider(store, p)
					s.ConfigureWorkers(checksAuthority(t))
					checkTick(t, s, store)
					if writes != p.creates+p.updates {
						t.Fatal("uncertain provider write blindly replayed")
					}
					if phase == "create" && p.finds != 1 || phase == "update" && p.reads != 1 {
						t.Fatal("recovery did not observe exact existing identity")
					}
					if missing {
						for range 3 {
							checkTick(t, s, store)
						}
						if writes != p.creates+p.updates {
							t.Fatal("absent uncertain check recreated")
						}
					}
				})
			}
		}
	}
}
func TestWorkerChecksPostIOGrantLossAndRestoreNoWrites(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			s, store, _, p := checksFixture(t, kind)
			defer store.Close()
			j := checksQueue(t, s, time.Time{})
			p.during = func() {
				if e := store.Update(context.Background(), func(st *domain.State) error { st.GitHubConnections[0].Config.InstallationID++; return nil }); e != nil {
					t.Fatal("provider I/O held DB lock", e)
				}
			}
			checkTick(t, s, store)
			st, _ := store.Read(context.Background())
			if operationByID(&st, j.CheckOperationID).Status != "FAILED" || checkInitialReady(&st, workerJob(&st, j.ID)) {
				t.Fatal("post-I/O authority change admitted")
			}
			backup, e := s.CreateBackup(context.Background(), "fixture")
			if e != nil {
				t.Fatal(e)
			}
			dest, destStore := fixture(t)
			destStore.state = domain.EmptyState()
			dest.provider = p
			dest.ConfigureWorkers(checksAuthority(t))
			if _, e = dest.RestoreBackup(context.Background(), "fixture", backup.ArchiveBase64, backup.SHA256); e != nil {
				t.Fatal(e)
			}
			writes := p.creates + p.updates
			for range 2 {
				if _, e = dest.Tick(context.Background()); e != nil {
					t.Fatal(e)
				}
			}
			if p.creates+p.updates != writes {
				t.Fatal("restored check state resurrected external writes")
			}
		})
	}

}

func TestWorkerChecksPhysicalConnectionAssociation(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		for _, mode := range []string{"missing", "primary", "list"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				s, store, _, p := checksFixture(t, kind)
				defer store.Close()
				if e := store.Update(context.Background(), func(st *domain.State) error {
					r := &st.RegisteredRepositories[0]
					r.ConnectionID = "other"
					r.ConnectionIDs = nil
					if mode == "primary" {
						r.ConnectionID = "source-conn"
					}
					if mode == "list" {
						r.ConnectionIDs = []string{"other", "source-conn"}
					}
					return nil
				}); e != nil {
					t.Fatal(e)
				}
				cmd := queueWorkerCommand(s)
				cmd.Data["parameters"] = map[string]any{}
				_, e := s.Execute(context.Background(), cmd)
				if mode == "missing" {
					if e == nil {
						t.Fatal("unassociated registry mapping admitted")
					}
					st, _ := store.Read(context.Background())
					if len(st.WorkerJobs) != 0 || len(st.Operations) != 0 || p.creates+p.updates+p.reads+p.finds != 0 {
						t.Fatal("rejected admission mutated state or called provider")
					}
				} else {
					if e != nil {
						t.Fatal(e)
					}
					checkTick(t, s, store)
					if p.creates != 1 {
						t.Fatal("valid association rejected")
					}
				}
			})
		}
	}
}

func TestWorkerChecksPhysicalAPIAliasOwnership(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		for _, api := range []string{"https://api.github.com", "https://API.GITHUB.COM:443/"} {
			t.Run(kind+"/"+api, func(t *testing.T) {
				s, store, _, p := checksFixture(t, kind)
				defer store.Close()
				r1, r2 := checkRecipe(), checkRecipe()
				r2.ID = "alias"
				r2.ProductID = "p2"
				r2.RepositoryID = "r2"
				r2.RepositoryURL = "https://github.com/test/renamed"
				s.ConfigureWorkers(checksAuthority(t, r1, r2))
				if e := store.Update(context.Background(), func(st *domain.State) error {
					st.GitHubConnections[0].Config.APIURL = ""
					st.RegisteredRepositories[0].ProviderAPIURL = ""
					st.Products = append(st.Products, domain.Product{Meta: domain.Meta{ID: "p2"}, Name: "other"})
					c := st.GitHubConnections[0]
					c.ID = "alias-conn"
					c.ProductID = "p2"
					c.Config.ID = c.ID
					c.Config.APIURL = api
					st.GitHubConnections = append(st.GitHubConnections, c)
					st.Repositories = append(st.Repositories, domain.Repository{Meta: domain.Meta{ID: "r2", ProductID: "p2"}, URL: r2.RepositoryURL, Role: "APPLICATION", RegisteredRepositoryID: "alias-registered"})
					st.RepositoryBindings = append(st.RepositoryBindings, domain.RepositoryBinding{Meta: domain.Meta{ID: "alias-binding", ProductID: "p2"}, RepositoryID: "r2", ConnectionID: c.ID, Role: "APPLICATION", FullName: "test/renamed"})
					st.RegisteredRepositories = append(st.RegisteredRepositories, domain.RegisteredRepository{Meta: domain.Meta{ID: "alias-registered"}, ProviderRepositoryID: 42, FullName: "test/renamed", ProviderAPIURL: api, ConnectionID: c.ID})
					return nil
				}); e != nil {
					t.Fatal(e)
				}
				cmd := queueWorkerCommand(s)
				cmd.Data["parameters"] = map[string]any{}
				if _, e := s.Execute(context.Background(), cmd); e == nil {
					t.Fatal("equivalent API numeric alias stole check ownership")
				}
				st, _ := store.Read(context.Background())
				if len(st.WorkerJobs) != 0 || len(st.Operations) != 0 || p.creates+p.updates+p.reads+p.finds != 0 {
					t.Fatal("alias rejection not atomic/zero HTTP")
				}
			})
		}
	}
}

func TestWorkerChecksCurrentAuthorityAtWorkerAdmission(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		for _, mode := range []string{"binding", "physical", "grant"} {
			for _, action := range []string{"claim", "start", "heartbeat", "report", "persisted-report"} {
				t.Run(kind+"/"+mode+"/"+action, func(t *testing.T) {
					s, store, _, p := checksFixture(t, kind)
					defer store.Close()
					if mode == "grant" {
						if e := store.Update(context.Background(), func(st *domain.State) error {
							st.GitHubConnections[0].ProductID = "instance"
							st.ConnectionGrants = append(st.ConnectionGrants, domain.ConnectionGrant{Meta: domain.Meta{ID: "grant", ProductID: "p"}, ConnectionID: "source-conn"})
							return nil
						}); e != nil {
							t.Fatal(e)
						}
					}
					j := checksQueue(t, s, time.Time{})
					checkTick(t, s, store)
					var l *domain.WorkerLease
					var report domain.WorkerReport
					if action != "claim" {
						l = checksClaim(t, s)
						if action != "start" {
							if _, e := s.StartWorker(context.Background(), workerIdentity(t, s), workerRequest(l)); e != nil {
								t.Fatal(e)
							}
						}
						report = checksReport(l, "PASSED")
						if action == "persisted-report" {
							if _, e := s.ReportWorker(context.Background(), workerIdentity(t, s), report); e != nil {
								t.Fatal(e)
							}
						}
					}
					if e := store.Update(context.Background(), func(st *domain.State) error {
						switch mode {
						case "binding":
							st.RepositoryBindings[0].FullName = "test/changed"
						case "physical":
							st.RegisteredRepositories[0].ProviderRepositoryID++
						case "equivalent-api":
							st.GitHubConnections[0].Config.APIURL = ""
						case "grant":
							st.ConnectionGrants[0].ProductID = "other"
						}
						return nil
					}); e != nil {
						t.Fatal(e)
					}
					var e error
					switch action {
					case "claim":
						var got *domain.WorkerLease
						got, e = s.ClaimWorker(context.Background(), workerIdentity(t, s), domain.WorkerClaim{ProductID: "p", RepositoryID: "r", Capability: "local_oci_build"})
						if got != nil {
							t.Fatal("stale authority claimed execution")
						}
					case "start":
						_, e = s.StartWorker(context.Background(), workerIdentity(t, s), workerRequest(l))
					case "heartbeat":
						_, e = s.HeartbeatWorker(context.Background(), workerIdentity(t, s), workerRequest(l))
					case "report", "persisted-report":
						_, e = s.ReportWorker(context.Background(), workerIdentity(t, s), report)
					}
					if action == "persisted-report" {
						if e != nil {
							t.Fatal("identical persisted report replay rejected", e)
						}
					} else if action != "claim" && e == nil {
						t.Fatal("new worker mutation admitted stale authority")
					}
					st, _ := store.Read(context.Background())
					if action != "persisted-report" {
						jj := workerJob(&st, j.ID)
						if jj.Status == "SUCCEEDED" {
							t.Fatal("stale new PASSED admitted")
						}
						if action == "claim" && len(st.WorkerAttempts) != 0 {
							t.Fatal("stale claim created attempt")
						}
					}
					if p.creates != 1 || p.updates+p.finds+p.reads != 0 {
						t.Fatal("worker authority guard performed provider I/O")
					}
				})
			}
		}
	}
}

func TestWorkerChecksProviderAPIIdentity(t *testing.T) {
	for _, api := range []string{"", "https://api.github.com", "https://API.GITHUB.COM:443/", "https://api.github.com///"} {
		if !checkAPISame(api, "https://api.github.com") {
			t.Fatal("default API alias not canonical", api)
		}
	}
	for _, api := range []string{"http://api.github.com", "https://api.github.com:444", "https://other.example", "https://api.github.com/api/v3", "https://user@api.github.com", "https://api.github.com?q=1", "://bad"} {
		if checkAPISame(api, "https://api.github.com") {
			t.Fatal("different or malformed API identity collapsed", api)
		}
	}
	if !checkAPISame("https://GHE.example:443/api/v3/", "https://ghe.example/api/v3") || checkAPISame("https://ghe.example/api/v3", "https://ghe.example/api/v4") {
		t.Fatal("enterprise API path identity lost")
	}
}
