package application

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
)

type retentionFlowProvider struct {
	*fakeFlow
	ensureCalls, verifyCalls int
	onEnsure, onVerify       func()
	lostResponse             bool
	boundedContext           bool
	waitForCancellation      bool
}

func (p *retentionFlowProvider) EnsureRef(ctx context.Context, c delivery.Connection, repo, branch, sha string) (string, error) {
	p.ensureCalls++
	deadline, ok := ctx.Deadline()
	p.boundedContext = ok && time.Until(deadline) <= 25*time.Second
	if p.waitForCancellation {
		<-ctx.Done()
		return "", ctx.Err()
	}
	result, err := p.fakeFlow.EnsureRef(ctx, c, repo, branch, sha)
	if p.onEnsure != nil {
		hook := p.onEnsure
		p.onEnsure = nil
		hook()
	}
	if p.lostResponse {
		p.lostResponse = false
		return "", errors.New("lost create response")
	}
	return result, err
}
func (p *retentionFlowProvider) VerifyComposedSource(ctx context.Context, c delivery.Connection, plan delivery.SourcePlan, result delivery.SourceResult) error {
	p.verifyCalls++
	if p.onVerify != nil {
		hook := p.onVerify
		p.onVerify = nil
		hook()
	}
	return p.fakeFlow.VerifyComposedSource(ctx, c, plan, result)
}
func retentionFlowFixture(t *testing.T) (*Service, *memoryStore, *retentionFlowProvider, string) {
	t.Helper()
	s, m, f := featurePatchFixture(t)
	p := &retentionFlowProvider{fakeFlow: f}
	s.provider = p
	parent := exec(t, s, domain.Command{Action: "reconcile_composition", ID: "baseline"}).(domain.ExternalOperation)
	return s, m, p, parent.ID
}
func retentionSourceReady(op domain.ExternalOperation) bool {
	return len(op.Sources) == 1 && op.Sources[0].Result != nil
}
func retentionClaim(t *testing.T, s *Service, parent string) (domain.ExternalOperation, string) {
	t.Helper()
	token := id()
	var op domain.ExternalOperation
	if err := s.store.Update(context.Background(), func(st *domain.State) error {
		current := operationByID(st, parent)
		current.LeaseOwner = token
		current.LeaseUntil = time.Now().Add(time.Minute)
		op = cloneFlowOperation(*current)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return op, token
}
func TestSourceRetentionBoundedPinsLostResponseAndSavedResultDrift(t *testing.T) {
	s, m, p, parent := retentionFlowFixture(t)
	p.lostResponse = true
	op, token := retentionClaim(t, s, parent)
	if err := s.tickFlow(context.Background(), op, token); err != nil {
		t.Fatal(err)
	}
	current := operationByID(&m.state, parent)
	if len(current.Sources[0].Retained) != 1 || current.Sources[0].Result != nil || p.ensureCalls != 1 {
		t.Fatal("pin progress is not one durable readback per tick")
	}
	if p.branches["rcp/revisions/rev"] != shaHead || len(m.state.IntegrationRevisions) != 4 {
		t.Fatal("retention mutated revision records or did not pin exact SHA")
	}
	flowTickUntil(t, s, m, parent, retentionSourceReady)
	current = operationByID(&m.state, parent)
	if len(current.Sources[0].Retained) != 2 || p.ensureCalls != 2 {
		t.Fatal("lost response repeated ref writes or missing bounded proof")
	}
	oldProof := append([]domain.SourceRetentionObservation(nil), current.Sources[0].Retained...)
	delete(p.branches, "rcp/revisions/rev")
	calls := p.ensureCalls
	op, token = retentionClaim(t, s, parent)
	if err := s.tickFlow(context.Background(), op, token); err != nil {
		t.Fatal(err)
	}
	current = operationByID(&m.state, parent)
	if current.Status != "FAILED" || len(current.ChildIDs) != 0 || p.ensureCalls != calls || !reflect.DeepEqual(oldProof, current.Sources[0].Retained) {
		t.Fatal("missing admitted pin healed, erased proof or queued build")
	}
}
func TestSourceRetentionInitialWrongPinAndGeneratedDrift(t *testing.T) {
	for _, mode := range []string{"initial wrong pin", "saved final drift", "queue final drift", "queue missing pin", "queue moved pin", "proof hash", "intent hash", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			s, m, p, parent := retentionFlowFixture(t)
			switch mode {
			case "initial wrong pin":
				p.branches["rcp/revisions/rev"] = strings.Repeat("f", 40)
			case "legacy":
				operationByID(&m.state, parent).Sources[0].Retention = nil
			case "intent hash":
				operationByID(&m.state, parent).Sources[0].Retention.InputHash = "forged"
			default:
				flowTickUntil(t, s, m, parent, retentionSourceReady)
				if strings.HasPrefix(mode, "queue ") {
					flowTickUntil(t, s, m, parent, func(op domain.ExternalOperation) bool { return op.Phase == "QUEUE_BUILDS" })
				}
				current := operationByID(&m.state, parent)
				if mode == "queue missing pin" {
					delete(p.branches, "rcp/revisions/rev")
				} else if mode == "queue moved pin" {
					p.branches["rcp/revisions/rev"] = strings.Repeat("f", 40)
				} else if mode == "proof hash" {
					current.Sources[0].Retained[0].IntentHash = "forged"
				} else {
					p.branches[current.Sources[0].Plan.TargetBranch] = strings.Repeat("f", 40)
				}
			}
			op, token := retentionClaim(t, s, parent)
			if err := s.tickFlow(context.Background(), op, token); err != nil {
				t.Fatal(err)
			}
			current := operationByID(&m.state, parent)
			if current.Status != "FAILED" || len(current.ChildIDs) != 0 || len(p.builds) != 0 {
				t.Fatalf("invalid source admitted mode=%s current=%+v", mode, current)
			}
		})
	}
}
func TestSourceRetentionAdmissionFences(t *testing.T) {
	for _, boundary := range []string{"pin", "result", "queue"} {
		for _, drift := range []string{"expired lease", "lease owner", "superseded", "connection", "grant", "config", "cancelled"} {
			t.Run(boundary+"/"+drift, func(t *testing.T) {
				s, m, p, parent := retentionFlowFixture(t)
				if boundary != "pin" {
					flowTickUntil(t, s, m, parent, retentionSourceReady)
				}
				if boundary == "queue" {
					flowTickUntil(t, s, m, parent, func(op domain.ExternalOperation) bool { return op.Phase == "QUEUE_BUILDS" })
				}
				before := cloneFlowOperation(*operationByID(&m.state, parent))
				proofs := len(before.Sources[0].Retained)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				hook := func() {
					current := operationByID(&m.state, parent)
					switch drift {
					case "expired lease":
						current.LeaseUntil = time.Now().Add(-time.Second)
					case "lease owner":
						current.LeaseOwner = "another"
					case "superseded":
						environmentByID(&m.state, "dev").DesiredOperationID = "new-intent"
					case "connection":
						connection(&m.state, "conn").Config.Owner = "changed"
					case "grant":
						m.state.ConnectionGrants = nil
					case "config":
						componentBuild(&m.state, "component").Workflow = "different.yml"
					case "cancelled":
						cancel()
					}
				}
				if boundary == "pin" {
					p.onEnsure = hook
				} else {
					p.onVerify = hook
				}
				op, token := retentionClaim(t, s, parent)
				_ = s.tickFlow(ctx, op, token)
				current := operationByID(&m.state, parent)
				if len(current.ChildIDs) != 0 || len(p.builds) != 0 || len(current.Sources[0].Retained) != proofs || (boundary == "pin" && current.Sources[0].Result != nil) || (boundary == "result" && current.Phase != "COMPOSE_SOURCE") {
					t.Fatalf("drift admitted boundary=%s drift=%s %+v", boundary, drift, current)
				}
			})
		}
	}
}
func TestSourceRetentionSchedulerLegacySkipAndExplicitLineage(t *testing.T) {
	s, m, p := featurePatchFixture(t)
	// Legacy refreshable selection cannot mint retention authority or audit.
	compositionByID(&m.state, "baseline").AutoRefresh = true
	before := featurePatchFingerprint(m.state)
	if err := s.ScheduleCompositionRefresh(context.Background(), time.Now(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if string(before) != string(featurePatchFingerprint(m.state)) {
		t.Fatal("legacy scheduler created an operation or audit")
	}
	// An explicit new parent establishes retention intent; after completion, the
	// scheduler may inherit only its selected feature scope and captured token.
	parent := exec(t, s, domain.Command{Action: "reconcile_composition", ID: "baseline"}).(domain.ExternalOperation)
	operationByID(&m.state, parent.ID).Status = "SUCCEEDED"
	if err := s.ScheduleCompositionRefresh(context.Background(), time.Now(), time.Minute); err != nil {
		t.Fatal(err)
	}
	latest := m.state.Operations[len(m.state.Operations)-1]
	if latest.ID == parent.ID || latest.SourceRetentionOriginID != parent.ID || latest.SourceRetentionParentID != parent.ID || validSourceRetention(latest) != nil || len(p.plans) != 0 {
		t.Fatalf("refresh lacks server-derived lineage %+v", latest)
	}
	// A stale refresh snapshot is fenced even when the previous parent had intent.
	operationByID(&m.state, latest.ID).Status = "SUCCEEDED"
	exec(t, s, domain.Command{Action: "record_integration_revision", ID: "scheduler-new-a", IntegrationID: "i", Data: map[string]any{"repository_id": "source", "branch": "feature/a", "base_commit": shaBase, "head_commit": strings.Repeat("9", 40), "commits": []string{strings.Repeat("9", 40)}}})
	c := featurePatchCommand(m.state, "f", "rev", "pin-earlier")
	wrapper := &staleFeaturePatchStore{Store: m, mutate: func() { exec(t, s, c) }}
	staleScheduler := NewWithProvider(wrapper, p)
	if err := staleScheduler.ScheduleCompositionRefresh(context.Background(), time.Now(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if len(m.state.Operations) != 3 {
		t.Fatal("stale scheduler added work after patch")
	}
}
func TestSourceRetentionRequestBoundaries(t *testing.T) {
	for _, mode := range []string{"direct", "duplicate head", "malformed ref", "oversized ref", "public origin"} {
		t.Run(mode, func(t *testing.T) {
			s, m, _ := featurePatchFixture(t)
			c := domain.Command{Action: "reconcile_composition", Actor: "agent", ID: "baseline", Data: map[string]any{}}
			switch mode {
			case "direct":
				environmentBinding(&m.state, "dev", "component").GitOpsMode = "DIRECT"
			case "duplicate head":
				comp := compositionByID(&m.state, "baseline")
				comp.RevisionSnapshots[1].HeadCommit = comp.RevisionSnapshots[0].HeadCommit
			case "malformed ref":
				comp := compositionByID(&m.state, "baseline")
				comp.Components[0].RevisionIDs[0] = "bad..id"
				comp.RevisionSnapshots[0].ID = "bad..id"
			case "oversized ref":
				comp := compositionByID(&m.state, "baseline")
				rid := strings.Repeat("x", 200)
				comp.Components[0].RevisionIDs[0] = rid
				comp.RevisionSnapshots[0].ID = rid
			case "public origin":
				c.Data["source_retention_origin_id"] = "approved"
			}
			before := featurePatchFingerprint(m.state)
			reject(t, s, c)
			if string(before) != string(featurePatchFingerprint(m.state)) {
				t.Fatal("request refusal mutated canonical state")
			}
		})
	}
}

func TestSourceRetentionConcurrentTicksAndRestoreSchedulerFence(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			_, m, p, parent := retentionFlowFixture(t)
			store, _ := openWorkerTestStore(t, kind)
			defer store.Close()
			if err := store.Update(ctx, func(st *domain.State) error { *st = m.state; return nil }); err != nil {
				t.Fatal(err)
			}
			s := NewWithProvider(store, p)
			var wg sync.WaitGroup
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, err := s.Tick(ctx); err != nil {
						t.Error(err)
					}
				}()
			}
			wg.Wait()
			st, _ := store.Read(ctx)
			op := operationByID(&st, parent)
			if p.ensureCalls != 1 || len(op.Sources[0].Retained) != 1 || len(st.OperationSteps) != 1 || len(op.ChildIDs) != 0 {
				t.Fatal("concurrent ticks duplicated pin admission")
			}
			// Completed pre-restore intent is historical evidence, not permission to
			// restart source writes from a newly restored installation's scheduler.
			_ = store.Update(ctx, func(st *domain.State) error {
				op := operationByID(st, parent)
				op.Status = "SUCCEEDED"
				compositionByID(st, "baseline").AutoRefresh = true
				st.Events = append(st.Events, domain.Event{ID: id(), Action: "restore_backup", At: time.Now().UTC()})
				return nil
			})
			st, _ = store.Read(ctx)
			before := featurePatchFingerprint(st)
			if err := s.ScheduleCompositionRefresh(ctx, time.Now(), time.Minute); err != nil {
				t.Fatal(err)
			}
			st, _ = store.Read(ctx)
			if string(before) != string(featurePatchFingerprint(st)) {
				t.Fatal("restored completed intent restarted automatic work")
			}
		})
	}
}
func TestSourceRetentionFullHistoryRejectsContaminatedCaptureDespitePins(t *testing.T) {
	s, m, p, parent := retentionFlowFixture(t)
	foreign := strings.Repeat("8", 40)
	p.parents[foreign] = []string{shaBase}
	p.parents[shaHead] = []string{foreign}
	// Both exact revision refs are admitted before the independent full history
	// check rejects the undeclared commit. Pins alone are never source ownership.
	for range 3 {
		op, token := retentionClaim(t, s, parent)
		if err := s.tickFlow(context.Background(), op, token); err != nil {
			t.Fatal(err)
		}
	}
	op := operationByID(&m.state, parent)
	if len(op.Sources[0].Retained) != 2 || op.Status != "FAILED" || len(op.ChildIDs) != 0 || len(p.plans) != 0 || !strings.Contains(op.Detail, "complete branch history") {
		t.Fatalf("contaminated capture accepted despite pins %+v", op)
	}
}

func TestSourceRetentionTickContextBoundedAndCancellation(t *testing.T) {
	s, m, p, parent := retentionFlowFixture(t)
	if _, err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !p.boundedContext {
		t.Fatal("direct Tick background context lacks25second provider deadline")
	}
	p.waitForCancellation = true
	operationByID(&m.state, parent).NextAttemptAt = time.Time{}
	before := cloneFlowOperation(*operationByID(&m.state, parent))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, _ = s.Tick(ctx)
	current := operationByID(&m.state, parent)
	if len(current.Sources[0].Retained) != len(before.Sources[0].Retained) || current.Sources[0].Result != nil || len(current.ChildIDs) != 0 {
		t.Fatal("slow cancelled provider admitted source progress")
	}
}
func TestSourceRetentionNativeAuthorityChangesDuringIO(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		for _, boundary := range []string{"pin", "queue"} {
			t.Run(kind+"/"+boundary, func(t *testing.T) {
				f, parent, fake := compositionAuthorityFixture(t, kind)
				p := &retentionFlowProvider{fakeFlow: fake}
				f.s.provider = p
				if boundary == "queue" {
					for range 5 {
						nativeTick(t, f)
						st, _ := f.store.Read(context.Background())
						if operationByID(&st, parent.ID).Phase == "QUEUE_BUILDS" {
							break
						}
					}
				}
				hook := func() { f.s.ConfigureWorkers(replacementCompositionAuthority(t, f, "config")) }
				if boundary == "pin" {
					p.onEnsure = hook
				} else {
					p.onVerify = hook
				}
				_ = f.store.Update(context.Background(), func(st *domain.State) error { operationByID(st, parent.ID).NextAttemptAt = time.Time{}; return nil })
				before, _ := f.store.Read(context.Background())
				prior := operationByID(&before, parent.ID)
				_, _ = f.s.Tick(context.Background())
				after, _ := f.store.Read(context.Background())
				current := operationByID(&after, parent.ID)
				if len(current.Sources[0].Retained) != len(prior.Sources[0].Retained) || len(current.ChildIDs) != 0 || len(after.WorkerJobs) != 0 {
					t.Fatal("native authority drift during source I/O admitted proof or child")
				}
			})
		}
	}
}
