package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/ocipublisher"
	"releasecontrol/internal/workerconfig"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This is a deterministic application-side peer, not a GitHub/Kube integration.
// The owning workerrunner suite separately exercises the production HTTP adapter.
type restorationEffectPeer struct {
	*devActivationProvider
	original, current map[string]delivery.GitOpsSnapshot
	commits           map[string]map[string]delivery.GitOpsSnapshot
	requests          map[string]delivery.GitOpsBatchApply
	prs               map[string]delivery.GitOpsPullRequest
	head              string
	hook              func(int)
	sends             int
	unknown           bool
	lost              bool
	lostAt            int
	now               func() time.Time
	afterObserve      func()
	afterRead         func(delivery.GitOpsRequest)
	afterRevision     func(delivery.GitOpsRevisionRequest)
}

func restorationSelector(r delivery.GitOpsRequest) string {
	return r.Repository + "|" + r.Ref + "|" + r.Path + "|" + r.ImageField + "|" + r.DigestField
}
func restorationPath(r delivery.GitOpsRequest) string {
	return r.Repository + "|" + r.Path + "|" + r.ImageField + "|" + r.DigestField + "|" + r.ImageRepository
}
func newRestorationEffectPeer(t *testing.T, f *nativeFixture, p *devActivationProvider, hold domain.ExternalOperation) *restorationEffectPeer {
	t.Helper()
	st, _ := f.store.Read(context.Background())
	capture, _ := latestBaseline(&st, hold.ID)
	out := &restorationEffectPeer{devActivationProvider: p, original: map[string]delivery.GitOpsSnapshot{}, current: map[string]delivery.GitOpsSnapshot{}, commits: map[string]map[string]delivery.GitOpsSnapshot{}, requests: map[string]delivery.GitOpsBatchApply{}, prs: map[string]delivery.GitOpsPullRequest{}, head: strings.Repeat("f", 40)}
	for _, c := range capture.DevBaseline.Components {
		key := restorationPath(c.GitOps)
		out.original[key] = delivery.GitOpsSnapshot{ImageRepository: c.ImageRepository, Digest: c.Digest, HeadSHA: c.ObservedGitOps.HeadSHA, BlobSHA: c.ObservedGitOps.BlobSHA, Content: "fixture baseline complete shared Helm bytes"}
		request := c.GitOps
		selected := false
		for _, activation := range st.Operations {
			if activation.DevActivation == nil || activation.DevActivation.ReservationOperationID != hold.ID {
				continue
			}
			for _, member := range activation.DevActivation.Components {
				if member.Artifact.ApplicationID == c.ApplicationID {
					request.ImageRepository = member.Artifact.ImageRepository
					selected = true
				}
			}
		}
		v := out.original[key]
		if selected {
			var e error
			v, e = p.ReadGitOps(context.Background(), delivery.Connection{}, request)
			if e != nil {
				t.Fatal(e)
			}
		}
		v.HeadSHA = out.head
		v.Content = "fixture exact admitted candidate document"
		currentSelector := c.GitOps
		currentSelector.ImageRepository = v.ImageRepository
		out.current[restorationPath(currentSelector)] = v
	}
	for _, a := range st.Operations {
		if a.DevActivation == nil || a.DevActivation.ReservationOperationID != hold.ID {
			continue
		}
		for _, g := range a.DevActivation.Groups {
			leader := operationByID(&st, g.LeaderID)
			if leader.DevActivationBatch == nil || leader.GitOpsPR == nil {
				t.Fatal("peer requires exact original activation effect")
			}
			out.requests[leader.ID] = *leader.DevActivationBatch
			pr := *leader.GitOpsPR
			pr.Merged = true
			pr.State = "closed"
			out.prs[leader.ID] = pr
			out.commits[pr.HeadSHA] = cloneRestorationFiles(out.current)
		}
	}
	return out
}
func cloneRestorationFiles(in map[string]delivery.GitOpsSnapshot) map[string]delivery.GitOpsSnapshot {
	out := map[string]delivery.GitOpsSnapshot{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
func (p *restorationEffectPeer) ReadGitOps(_ context.Context, _ delivery.Connection, r delivery.GitOpsRequest) (delivery.GitOpsSnapshot, error) {
	v, ok := p.current[restorationPath(r)]
	if !ok {
		return v, invalid("fixture selector absent")
	}
	if r.ImageRepository == "" || v.ImageRepository != r.ImageRepository {
		return v, invalid("fixture exact image repository selector absent")
	}
	v.HeadSHA = p.head
	if r.ExpectedHeadSHA != "" && r.ExpectedHeadSHA != v.HeadSHA {
		return v, invalid("fixture head changed")
	}
	if p.afterRead != nil {
		p.afterRead(r)
	}
	return v, nil
}
func (p *restorationEffectPeer) ReadGitOpsAtCommit(ctx context.Context, _ delivery.Connection, r delivery.GitOpsRequest, commit string) (delivery.GitOpsSnapshot, error) {
	if !delivery.CachedAuthenticationOnly(ctx) {
		return delivery.GitOpsSnapshot{}, invalid("immutable lookup may not refresh")
	}
	v, ok := p.commits[commit][restorationPath(r)]
	if !ok {
		return v, invalid("immutable proposal absent")
	}
	if r.ImageRepository == "" || v.ImageRepository != r.ImageRepository {
		return v, invalid("fixture exact immutable image selector absent")
	}
	v.HeadSHA = commit
	return v, nil
}
func (p *restorationEffectPeer) VerifyGitOpsRevision(ctx context.Context, _ delivery.Connection, r delivery.GitOpsRevisionRequest) (delivery.GitOpsRevisionProof, error) {
	if !delivery.CachedAuthenticationOnly(ctx) {
		return delivery.GitOpsRevisionProof{}, invalid("recovery ancestry may not refresh")
	}
	if r.HeadSHA != "" && r.HeadSHA != p.head {
		return delivery.GitOpsRevisionProof{}, invalid("fixture head changed")
	}
	proof := delivery.GitOpsRevisionProof{Repository: r.Repository, Ref: r.Ref, HeadSHA: p.head, AncestorSHAs: r.AncestorSHAs}
	if p.afterRevision != nil {
		p.afterRevision(r)
	}
	return proof, nil
}
func (p *restorationEffectPeer) ProposeGitOpsBatchGuarded(ctx context.Context, _ delivery.Connection, b delivery.GitOpsBatchApply, guard func(context.Context) error) (delivery.GitOpsPullRequest, error) {
	if !delivery.CachedAuthenticationOnly(ctx) {
		return delivery.GitOpsPullRequest{}, invalid("cold mutation may not refresh")
	}
	// tree/commit/ref/PR plus one redirected send; guard is latched every time.
	for stage := 1; stage <= 5; stage++ {
		if e := guard(ctx); e != nil {
			return delivery.GitOpsPullRequest{}, e
		}
		p.sends++
		if p.hook != nil {
			p.hook(stage)
		}
		if p.lostAt == stage {
			return delivery.GitOpsPullRequest{}, invalid("lost physical proposal stage response")
		}
	}
	pr := delivery.GitOpsPullRequest{Number: 100 + len(p.prs), URL: fmt.Sprintf("https://fixture.invalid/pulls/%d", 100+len(p.prs)), HeadSHA: fmt.Sprintf("%040x", 1000+len(p.prs)), State: "open"}
	files := cloneRestorationFiles(p.current)
	for _, change := range b.Changes {
		originalSelector := change.Selector
		originalSelector.ImageRepository = change.ImageRepository
		key := restorationPath(originalSelector)
		v := p.original[key]
		if v.ImageRepository != change.ImageRepository || v.Digest != change.Digest {
			return pr, invalid("peer accepts original-image restore only")
		}
		delete(files, restorationPath(change.Selector))
		files[key] = v
	}
	p.commits[pr.HeadSHA] = files
	p.requests[b.OperationID] = b
	p.prs[b.OperationID] = pr
	if p.lost {
		return delivery.GitOpsPullRequest{}, invalid("lost physical proposal response")
	}
	return pr, nil
}
func (p *restorationEffectPeer) LookupGitOpsBatchProposal(ctx context.Context, _ delivery.Connection, b delivery.GitOpsBatchApply) (delivery.GitOpsPullRequest, error) {
	if !delivery.CachedAuthenticationOnly(ctx) {
		return delivery.GitOpsPullRequest{}, invalid("lookup may not refresh")
	}
	pr, ok := p.prs[b.OperationID]
	if p.unknown && (!ok || !pr.Merged) {
		return delivery.GitOpsPullRequest{}, invalid("unknown cached lookup")
	}
	if !ok {
		return pr, invalid("unknown absent proposal")
	}
	if workerconfig.Fingerprint(p.requests[b.OperationID]) != workerconfig.Fingerprint(b) {
		return pr, invalid("batch changed")
	}
	return pr, nil
}
func (p *restorationEffectPeer) ObserveGitOpsBatchPR(ctx context.Context, c delivery.Connection, b delivery.GitOpsBatchApply, expected delivery.GitOpsPullRequest) (delivery.GitOpsPullRequest, error) {
	pr, e := p.LookupGitOpsBatchProposal(ctx, c, b)
	if e != nil {
		return pr, e
	}
	if activationPRHash(pr) != activationPRHash(expected) {
		return pr, invalid("proposal identity changed")
	}
	if p.afterObserve != nil {
		p.afterObserve()
	}
	return pr, nil
}
func (p *restorationEffectPeer) merge(operation string) {
	pr := p.prs[operation]
	pr.Merged = true
	pr.State = "closed"
	at := time.Now().UTC()
	if p.now != nil {
		at = p.now()
	}
	pr.MergedAt = &at
	p.prs[operation] = pr
	p.current = cloneRestorationFiles(p.commits[pr.HeadSHA])
	p.head = pr.HeadSHA
}
func restorationChangedFixture(t *testing.T, kind string, setup ...func(*nativeFixture)) (*nativeFixture, *restorationEffectPeer, domain.ExternalOperation, domain.ExternalOperation) {
	t.Helper()
	ctx := context.Background()
	f, p, hold, command := devActivationMultiFixture(t, kind, setup...)
	v, e := f.s.Execute(ctx, command)
	if e != nil {
		t.Fatal(e)
	}
	activation := v.(domain.ExternalOperation)
	activationTicks(t, f, 20)
	p.merged = true
	activationTicks(t, f, 20)
	st, _ := f.store.Read(ctx)
	if operationByID(&st, activation.ID).Status != "SUCCEEDED" {
		t.Fatal("activation not merged")
	}
	peer := newRestorationEffectPeer(t, f, p, hold)
	f.s.provider = peer
	request := restorationCommand(t, f, "request_candidate_dev_restoration", hold.ID, "restore-changed-group-v1")
	v, e = f.s.Execute(ctx, request)
	if e != nil {
		t.Fatal(e)
	}
	restore := v.(domain.ExternalOperation)
	completeRestorationRegistry(t, f)
	forceActivationTick(t, f, restore.ID)
	st, _ = f.store.Read(ctx)
	restore = *operationByID(&st, restore.ID)
	if restore.Phase != "PREPARE_PROPOSALS" || restore.DevRestorationPlan == nil || len(restore.DevRestorationPlan.Groups) < 1 {
		_ = f.store.Update(ctx, func(st *domain.State) error {
			op := operationByID(st, restore.ID)
			op.LeaseOwner = "debug"
			op.LeaseUntil = time.Now().Add(time.Minute)
			return nil
		})
		debug, _ := f.store.Read(ctx)
		restore = *operationByID(&debug, restore.ID)
		settlement, se := f.s.reconcileRestorationEffects(delivery.WithCachedAuthentication(ctx), &debug, restore, false)
		_, pe := f.s.prepareRestorationPlan(delivery.WithCachedAuthentication(ctx), restore, "debug", settlement)
		t.Fatal("changed grouped restoration not prepared", restore.Phase, restore.Detail, se, pe)
	}
	return f, peer, hold, restore
}
func TestDevRestorationEveryPhysicalSendAndRedirectFenced(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			f, peer, _, parent := restorationChangedFixture(t, kind)
			ctx := context.Background()
			initial, _ := f.store.Read(ctx)
			for name, mutate := range map[string]func(*domain.State){"grant": func(st *domain.State) {
				for i := range st.ConnectionGrants {
					st.ConnectionGrants[i].ProductID = "foreign"
				}
			}, "lease": func(st *domain.State) { operationByID(st, parent.ID).LeaseUntil = time.Now().Add(-time.Second) }, "lease-owner": func(st *domain.State) { operationByID(st, parent.ID).LeaseOwner = "other" }, "config": func(st *domain.State) { environmentBinding(st, "dev", "component").Path = "other.yaml" }, "epoch": func(st *domain.State) { environmentByID(st, "dev").ReservationEpoch++ }, "selection-generation": func(st *domain.State) { environmentByID(st, "dev").SelectionGeneration++ }} {
				for stage := 1; stage <= 5; stage++ {
					t.Run(fmt.Sprintf("%s/send%d", name, stage), func(t *testing.T) {
						store, _ := openWorkerTestStore(t, kind)
						defer store.Close()
						copy := cloneStateForActivation(t, initial)
						if e := store.Update(ctx, func(st *domain.State) error {
							*st = copy
							op := operationByID(st, parent.ID)
							op.LeaseOwner = "recovery-test"
							op.LeaseUntil = time.Now().Add(time.Minute)
							return nil
						}); e != nil {
							t.Fatal(e)
						}
						service := NewWithProvider(store, nil)
						service.ConfigureWorkers(f.s.externalWorkers)
						service.SetRuntimeObservers(f.s.runtimeObservers)
						p := *peer
						p.current = cloneRestorationFiles(peer.current)
						p.commits = map[string]map[string]delivery.GitOpsSnapshot{}
						for k, v := range peer.commits {
							p.commits[k] = cloneRestorationFiles(v)
						}
						p.requests = map[string]delivery.GitOpsBatchApply{}
						p.prs = map[string]delivery.GitOpsPullRequest{}
						for k, v := range peer.requests {
							p.requests[k] = v
						}
						for k, v := range peer.prs {
							p.prs[k] = v
						}
						p.sends = 0
						p.hook = func(at int) {
							if at == stage {
								if e := store.Update(ctx, func(st *domain.State) error { mutate(st); return nil }); e != nil {
									t.Fatal(e)
								}
							}
						}
						service.provider = &p
						state, e := store.Read(ctx)
						if e != nil {
							t.Fatal(e)
						}
						leased := *operationByID(&state, parent.ID)
						_ = service.tickDevRestoration(ctx, leased, "recovery-test")
						if p.sends != stage {
							t.Fatalf("guard allowed extra physical sends: got%d want%d", p.sends, stage)
						}
						state, e = store.Read(ctx)
						if e != nil {
							t.Fatal(e)
						}
						saved := operationByID(&state, parent.ID)
						if saved.DevRestorationPlan == nil || !saved.DevRestorationPlan.Groups[0].SendStarted {
							t.Fatal("durable send latch lost")
						}
						if saved.DevRestorationPlan.Groups[0].PullRequest != nil {
							t.Fatal("revoked final save persisted a proposal receipt")
						}
						if saved.Status == "SUCCEEDED" || activeDevReservation(&state, "dev") == nil {
							t.Fatal("revoked final save succeeded/unlocked")
						}
					})
				}
			}
		})
	}
}

func TestDevRestorationLostProposalCachedOnlyRenewalAndEarlyMerge(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			f, p, hold, parent := restorationChangedFixture(t, kind)
			ctx := context.Background()
			p.lost = true
			p.unknown = true
			forceActivationTick(t, f, parent.ID)
			before := p.sends
			if before != 5 {
				t.Fatal("fixture did not send full group once")
			}
			forceActivationTick(t, f, parent.ID)
			if p.sends != before {
				t.Fatal("unknown proposal repeated writes")
			}
			renew := restorationCommand(t, f, "request_candidate_dev_restoration", hold.ID, "restore-unknown-own-renew-v1")
			if _, e := f.s.Execute(ctx, renew); e == nil {
				t.Fatal("unknown own effect renewed")
			}
			p.unknown = false
			forceActivationTick(t, f, parent.ID)
			forceActivationTick(t, f, parent.ID)
			st, _ := f.store.Read(ctx)
			parent = *operationByID(&st, parent.ID)
			if parent.Phase != "WAIT_EXTERNAL_MERGE" || p.sends != before {
				t.Fatal("cached recovery did not freeze exact complete plan")
			}
			p.merge(parent.DevRestorationPlan.Groups[0].LeaderID)
			confirm := restorationCommand(t, f, "confirm_candidate_dev_restoration", parent.ID, "restore-early-merge-confirm-v1")
			confirm.Data["expected_plan_hash"] = parent.DevRestorationPlan.Hash
			if _, e := f.s.Execute(ctx, confirm); e == nil {
				t.Fatal("early merged effect backdated confirmation")
			}
			st, _ = f.store.Read(ctx)
			if !operationByID(&st, parent.ID).DevRestorationUnapproved || activeDevReservation(&st, "dev") == nil {
				t.Fatal("unapproved external effect not recorded/retained")
			}
		})
	}
}
func TestDevRestorationMergeChronologyAndGETSaveRace(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			f, p, hold, parent := restorationChangedFixture(t, kind)
			_ = hold
			ctx := context.Background()
			forceActivationTick(t, f, parent.ID)
			forceActivationTick(t, f, parent.ID)
			st, _ := f.store.Read(ctx)
			parent = *operationByID(&st, parent.ID)
			if parent.Phase != "WAIT_EXTERNAL_MERGE" {
				t.Fatal("plan pending")
			}
			confirm := restorationCommand(t, f, "confirm_candidate_dev_restoration", parent.ID, "restore-chronology-confirm-v1")
			confirm.Data["expected_plan_hash"] = parent.DevRestorationPlan.Hash
			v, e := f.s.Execute(ctx, confirm)
			if e != nil {
				t.Fatal(e)
			}
			confirmation := v.(domain.ExternalOperation)
			at := confirmation.DevRestorationConfirmation.ConfirmedAt
			operation := parent.DevRestorationPlan.Groups[0].LeaderID
			for name, stamp := range map[string]*time.Time{"missing": nil, "equal": &at, "before": func() *time.Time { v := at.Add(-time.Second); return &v }(), "future": func() *time.Time { v := time.Now().Add(time.Hour); return &v }()} {
				t.Run(name, func(t *testing.T) {
					pr := p.prs[operation]
					pr.Merged = true
					pr.State = "closed"
					pr.MergedAt = stamp
					p.prs[operation] = pr
					if f.s.observeRestorationPlan(ctx, parent, true) == nil {
						t.Fatal("ambiguous/early merge chronology accepted")
					}
				})
			}
			// Model a merge after the open GET but before the confirmation transaction.
			// A new fixture-generation cloned state avoids modifying immutable receipts.
			state, _ := f.store.Read(ctx)
			copy := cloneStateForActivation(t, state)
			filtered := []domain.ExternalOperation{}
			for _, o := range copy.Operations {
				if o.ID != confirmation.ID {
					filtered = append(filtered, o)
				}
			}
			copy.Operations = filtered
			store := &memoryStore{state: copy}
			service := NewWithProvider(store, p)
			service.ConfigureWorkers(f.s.externalWorkers)
			service.SetRuntimeObservers(f.s.runtimeObservers)
			pr := p.prs[operation]
			pr.Merged = false
			pr.State = "open"
			pr.MergedAt = nil
			p.prs[operation] = pr
			reads := 0
			p.afterRead = func(delivery.GitOpsRequest) {
				reads++
				if reads == len(parent.DevRestorationPlan.Components) {
					p.merge(operation)
				}
			}
			raced, e := service.Execute(ctx, confirm)
			if e != nil {
				t.Fatal("open read/save race fixture", e)
			}
			raceReceipt := raced.(domain.ExternalOperation)
			if p.prs[operation].MergedAt == nil || !p.prs[operation].MergedAt.Before(raceReceipt.DevRestorationConfirmation.ConfirmedAt) {
				t.Fatal("merge did not precede durable confirmation")
			}
			p.afterRead = nil
			op := operationByID(&store.state, parent.ID)
			op.LeaseOwner = "chronology"
			op.LeaseUntil = time.Now().Add(time.Minute)
			if e = service.tickDevRestoration(ctx, *op, "chronology"); e != nil {
				t.Fatal(e)
			}
			if !operationByID(&store.state, parent.ID).DevRestorationUnapproved || activeDevReservation(&store.state, "dev") == nil {
				t.Fatal("GET/save early merge backdated into approval")
			}
		})
	}
}
func TestDevRestorationBoundsAndReadReconciliationPastGenericTTL(t *testing.T) {
	f, _, hold, parent := restorationChangedFixture(t, "local")
	ctx := context.Background()
	state, _ := f.store.Read(ctx)
	t.Run("attempt-history", func(t *testing.T) {
		copy := cloneStateForActivation(t, state)
		for i := 2; i <= 256; i++ {
			op := parent
			op.ID = fmt.Sprintf("old-attempt-%d", i)
			proof := *parent.DevRestoration
			proof.Generation = uint64(i)
			op.DevRestoration = &proof
			op.DevRestorationPlan = nil
			copy.Operations = append(copy.Operations, op)
		}
		if _, e := restorationEffects(&copy, operationByID(&copy, hold.ID)); e == nil {
			t.Fatal("256attempt history accepted")
		}
	})
	for name, mutate := range map[string]func(*domain.DevRestorationProof){"effect257": func(p *domain.DevRestorationProof) {
		for len(p.Effects) < 257 {
			p.Effects = append(p.Effects, domain.DevRestorationEffect{OperationID: fmt.Sprintf("effect-%d", len(p.Effects)), NoIntent: true})
		}
	}, "duplicate-effect": func(p *domain.DevRestorationProof) { p.Effects = append(p.Effects, p.Effects[0]) }} {
		t.Run(name, func(t *testing.T) {
			copy := cloneStateForActivation(t, state)
			op := operationByID(&copy, parent.ID)
			mutate(op.DevRestoration)
			op.DevRestoration.SnapshotHash = restorationProofHash(*op.DevRestoration)
			op.DevRestorationPlan.ExpectedHash = op.DevRestoration.SnapshotHash
			if f.s.restorationAllowed(&copy, *op, false) == nil {
				t.Fatal("oversized/duplicate recovery effects admitted")
			}
		})
	}
	t.Run("expired-proposal-read-only-after45minutes", func(t *testing.T) {
		copy := cloneStateForActivation(t, state)
		op := operationByID(&copy, parent.ID)
		op.CreatedAt = time.Now().Add(-2 * time.Hour)
		op.DevRestoration.Deadline = op.CreatedAt.Add(time.Hour)
		op.DevRestoration.SnapshotHash = restorationProofHash(*op.DevRestoration)
		op.DevRestorationPlan.ExpectedHash = op.DevRestoration.SnapshotHash
		op.LeaseOwner = "read-only"
		op.LeaseUntil = time.Now().Add(time.Minute)
		store := &memoryStore{state: copy}
		service := NewWithProvider(store, f.s.provider)
		service.ConfigureWorkers(f.s.externalWorkers)
		service.SetRuntimeObservers(f.s.runtimeObservers)
		leased := *operationByID(&store.state, parent.ID)
		if service.restorationAllowed(&store.state, leased, true) == nil {
			t.Fatal("expired restoration write renewed")
		}
		if e := service.retryRestoration(ctx, leased, "read-only", "GET-only recovery remains pending"); e != nil {
			t.Fatal(e)
		}
		saved := operationByID(&store.state, parent.ID)
		if saved.Status != "RUNNING" || saved.Phase != "PREPARE_PROPOSALS" || saved.LeaseOwner != "" {
			t.Fatal("generic45minute terminal cap or implicit unlock applied")
		}
		t.Log("synthetic recovery authority clone; original candidate/baseline untouched; tests read-only retry policy, not elapsed production clock")
	})
}
func TestDevRestorationClosedUnmergedRequiresExplicitNewAttempt(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			f, p, hold, parent := restorationChangedFixture(t, kind)
			ctx := context.Background()
			forceActivationTick(t, f, parent.ID)
			forceActivationTick(t, f, parent.ID)
			st, _ := f.store.Read(ctx)
			parent = *operationByID(&st, parent.ID)
			operation := parent.DevRestorationPlan.Groups[0].LeaderID
			pr := p.prs[operation]
			pr.State = "closed"
			p.prs[operation] = pr
			beforeSends := p.sends
			renew := restorationCommand(t, f, "request_candidate_dev_restoration", hold.ID, "restore-explicit-closed-renew-v1")
			v, e := f.s.Execute(ctx, renew)
			if e != nil {
				t.Fatal(e)
			}
			next := v.(domain.ExternalOperation)
			if next.ID == parent.ID || next.DevRestoration.Generation != parent.DevRestoration.Generation+1 || next.ReservationEpoch != parent.ReservationEpoch+1 {
				t.Fatal("explicit renewal did not advance recovery generation/fence")
			}
			completeRestorationRegistry(t, f)
			forceActivationTick(t, f, next.ID)
			forceActivationTick(t, f, next.ID)
			if p.sends != beforeSends+5 {
				t.Fatal("new explicit attempt did not issue exactly one new group")
			}
			st, _ = f.store.Read(ctx)
			if f.s.restorationAllowed(&st, *operationByID(&st, parent.ID), true) == nil {
				t.Fatal("old generation resumed after explicit renewal")
			}
			replayed, e := f.s.Execute(ctx, renew)
			if e != nil || replayed.(domain.ExternalOperation).ID != next.ID || p.sends != beforeSends+5 {
				t.Fatal("renewal replay minted authority/effect", e)
			}
			pr.State = "open"
			p.prs[operation] = pr
			block := restorationCommand(t, f, "request_candidate_dev_restoration", hold.ID, "restore-reopened-old-renew-v1")
			if _, e = f.s.Execute(ctx, block); e == nil {
				t.Fatal("reopened historical effect omitted from settlement")
			}
		})
	}
}
func TestDevRestorationLateCandidateMergeAfterExpiryIgnoresCurrentSourceChecks(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, p, hold, activation := devActivationMultiFixture(t, kind)
			v, e := f.s.Execute(ctx, activation)
			if e != nil {
				t.Fatal(e)
			}
			old := v.(domain.ExternalOperation)
			activationTicks(t, f, 20)
			st, _ := f.store.Read(ctx)
			if operationByID(&st, old.DevActivation.Groups[0].LeaderID).GitOpsPR == nil {
				t.Fatal("candidate proposal absent")
			}
			candidate := operationByID(&st, hold.DevReservation.CandidateOperationID)
			immutable := workerconfig.Fingerprint(candidate.MainCandidate)
			baseline, _ := latestBaseline(&st, hold.ID)
			baselineHash := baselineCaptureHash(*baseline)
			f.s.nativeAuthorityNow = func() time.Time { return candidate.CreatedAt.Add(2 * time.Hour) }
			p.merged = true
			if e = f.store.Update(ctx, func(st *domain.State) error {
				st.Results = append(st.Results, domain.CheckResult{Meta: domain.Meta{ID: "current-main-check-failed", ProductID: "p", CreatedAt: time.Now().UTC()}, CheckID: "source-check", Commit: candidate.PreparedDeployments[0].Revision.HeadCommit, Result: "failed"})
				return nil
			}); e != nil {
				t.Fatal(e)
			}
			p.sourceReads.Store(0)
			p.mainCandidateProvider.onObserve = func() { t.Fatal("recovery consulted deleted author/source PR state") }
			peer := newRestorationEffectPeer(t, f, p, hold)
			f.s.provider = peer
			request := restorationCommand(t, f, "request_candidate_dev_restoration", hold.ID, "restore-late-expired-merge-v1")
			v, e = f.s.Execute(ctx, request)
			if e != nil {
				t.Fatal(e)
			}
			parent := v.(domain.ExternalOperation)
			completeRestorationRegistry(t, f)
			forceActivationTick(t, f, parent.ID)
			forceActivationTick(t, f, parent.ID)
			forceActivationTick(t, f, parent.ID)
			st, _ = f.store.Read(ctx)
			parent = *operationByID(&st, parent.ID)
			if parent.Phase != "WAIT_EXTERNAL_MERGE" || peer.sends != 5 || p.sourceReads.Load() != 0 {
				t.Fatal("late merged expired candidate could not form current restore-only proposal", parent.Phase)
			}
			if workerconfig.Fingerprint(operationByID(&st, candidate.ID).MainCandidate) != immutable {
				t.Fatal("candidate proof rewritten")
			}
			after, _ := latestBaseline(&st, hold.ID)
			if baselineCaptureHash(*after) != baselineHash {
				t.Fatal("original physical baseline rewritten")
			}
			if f.s.mainCandidateAllowed(&st, operationByID(&st, candidate.ID)) == nil {
				t.Fatal("expired candidate/Prod authority renewed")
			}
		})
	}
}
func completeChangedRestoration(t *testing.T, f *nativeFixture, p *restorationEffectPeer, parent domain.ExternalOperation) (domain.ExternalOperation, domain.ExternalOperation) {
	t.Helper()
	ctx := context.Background()
	forceActivationTick(t, f, parent.ID)
	forceActivationTick(t, f, parent.ID)
	st, _ := f.store.Read(ctx)
	parent = *operationByID(&st, parent.ID)
	confirm := restorationCommand(t, f, "confirm_candidate_dev_restoration", parent.ID, "restore-complete-confirm-v1")
	confirm.Data["expected_plan_hash"] = parent.DevRestorationPlan.Hash
	if _, e := f.s.Execute(ctx, confirm); e != nil {
		t.Fatal(e)
	}
	p.merge(parent.DevRestorationPlan.Groups[0].LeaderID)
	forceActivationTick(t, f, parent.ID)
	for _, observer := range f.s.runtimeObservers {
		o := observer.(*devRuntimeObserver)
		o.value.Flux.Revision = "main@sha1:" + p.head
	}
	verify := restorationCommand(t, f, "verify_candidate_dev_restoration", parent.ID, "restore-complete-proof-v1")
	v, e := f.s.Execute(ctx, verify)
	if e != nil {
		t.Fatal(e)
	}
	proof := v.(domain.ExternalOperation)
	completeRestorationRegistry(t, f)
	forceActivationTick(t, f, proof.ID)
	st, _ = f.store.Read(ctx)
	proof = *operationByID(&st, proof.ID)
	parent = *operationByID(&st, parent.ID)
	if proof.Status != "SUCCEEDED" || proof.Phase != "RESTORATION_VERIFIED" {
		t.Fatal("changed restoration complete proof failed", proof.Status, proof.Detail)
	}
	return parent, proof
}
func TestDevRestorationFinalUnlockReadSaveRacesRetainHold(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			f, p, hold, parent := restorationChangedFixture(t, kind)
			parent, proof := completeChangedRestoration(t, f, p, parent)
			ctx := context.Background()
			initial, _ := f.store.Read(ctx)
			for _, mode := range []string{"epoch", "new-proof"} {
				t.Run(mode, func(t *testing.T) {
					store, _ := openWorkerTestStore(t, kind)
					defer store.Close()
					copy := cloneStateForActivation(t, initial)
					if e := store.Update(ctx, func(st *domain.State) error { *st = copy; return nil }); e != nil {
						t.Fatal(e)
					}
					peer := *p
					service := NewWithProvider(store, &peer)
					service.ConfigureWorkers(f.s.externalWorkers)
					service.SetRuntimeObservers(f.s.runtimeObservers)
					fired := false
					peer.afterRevision = func(r delivery.GitOpsRevisionRequest) {
						if fired || r.HeadSHA == "" {
							return
						}
						fired = true
						if mode == "epoch" {
							if e := store.Update(ctx, func(st *domain.State) error { environmentByID(st, "dev").ReservationEpoch++; return nil }); e != nil {
								t.Fatal(e)
							}
						} else {
							data := map[string]any{"restoration_operation_id": parent.ID, "request_key": "restore-concurrent-proof-v1", "expected_selection": parent.DevRestoration.Selection}
							if _, e := service.Execute(ctx, domain.Command{Action: "verify_candidate_dev_restoration", ProductID: "p", Actor: "operator", Data: data}); e != nil {
								t.Fatal(e)
							}
						}
					}
					release := domain.Command{Action: "release_candidate_dev_after_restoration", ProductID: "p", Actor: "operator", Data: map[string]any{"restoration_operation_id": parent.ID, "restoration_verification_operation_id": proof.ID, "request_key": "restore-unlock-race-" + mode, "expected_selection": parent.DevRestoration.Selection}}
					if _, e := service.Execute(ctx, release); e == nil {
						t.Fatal("read/save race released hold")
					}
					after, _ := store.Read(ctx)
					if !fired || activeDevReservation(&after, "dev") == nil || activeDevReservation(&after, "dev").ID != hold.ID {
						t.Fatal("final raced authority did not retain exact hold")
					}
				})
			}
		})
	}
}
func TestDevRestorationDistinctPhysicalGroupsRemainHonestPartial(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, p, hold, parent := restorationChangedFixture(t, kind, func(f *nativeFixture) {
				if e := f.store.Update(ctx, func(st *domain.State) error {
					original := repositoryBinding(st, "gitops")
					binding := *original
					binding.ID = "gitops-b-binding"
					binding.RepositoryID = "gitops-b"
					repo := *restorationTestRepository(st, "gitops")
					repo.ID = "gitops-b"
					repo.RegisteredRepositoryID = "gitops-b-registered"
					repo.Name = "owner/gitops-b"
					repo.URL = "https://github.com/owner/gitops-b"
					var registered domain.RegisteredRepository
					for _, r := range st.RegisteredRepositories {
						if r.ID == restorationTestRepository(st, "gitops").RegisteredRepositoryID {
							registered = r
						}
					}
					registered.ID = repo.RegisteredRepositoryID
					registered.ProviderRepositoryID = 44
					registered.FullName = "owner/gitops-b"
					st.Repositories = append(st.Repositories, repo)
					st.RegisteredRepositories = append(st.RegisteredRepositories, registered)
					st.RepositoryBindings = append(st.RepositoryBindings, binding)
					environmentBinding(st, "dev", "component-b").RepositoryID = repo.ID
					return nil
				}); e != nil {
					t.Fatal(e)
				}
			})
			if len(parent.DevRestorationPlan.Groups) != 2 {
				t.Fatal("different numeric repositories coalesced")
			}
			for range 3 {
				forceActivationTick(t, f, parent.ID)
			}
			st, _ := f.store.Read(ctx)
			parent = *operationByID(&st, parent.ID)
			if parent.Phase != "WAIT_EXTERNAL_MERGE" || p.sends != 10 {
				t.Fatal("two honest group proposals not prepared", parent.Phase, p.sends)
			}
			confirm := restorationCommand(t, f, "confirm_candidate_dev_restoration", parent.ID, "restore-partial-confirm-v1")
			confirm.Data["expected_plan_hash"] = parent.DevRestorationPlan.Hash
			if _, e := f.s.Execute(ctx, confirm); e != nil {
				t.Fatal(e)
			}
			p.merge(parent.DevRestorationPlan.Groups[0].LeaderID)
			forceActivationTick(t, f, parent.ID)
			st, _ = f.store.Read(ctx)
			if operationByID(&st, parent.ID).Status == "SUCCEEDED" || activeDevReservation(&st, "dev") == nil {
				t.Fatal("one physical group made complete success/unlock")
			}
			verify := restorationCommand(t, f, "verify_candidate_dev_restoration", parent.ID, "restore-partial-verify-v1")
			if _, e := f.s.Execute(ctx, verify); e == nil {
				t.Fatal("partial groups authorized runtime verification")
			}
			renew := restorationCommand(t, f, "request_candidate_dev_restoration", hold.ID, "restore-partial-renew-v1")
			if _, e := f.s.Execute(ctx, renew); e == nil {
				t.Fatal("open second physical group omitted from renewal")
			}
			if p.sends != 10 {
				t.Fatal("partial reconciliation recreated proposal")
			}
		})
	}
}

func restorationTestRepository(st *domain.State, id string) *domain.Repository {
	for i := range st.Repositories {
		if st.Repositories[i].ID == id {
			return &st.Repositories[i]
		}
	}
	return nil
}
func TestDevRestorationExactMappedDevProdSelectorsSharedFile(t *testing.T) {
	ctx := delivery.WithCachedAuthentication(context.Background())
	connection := delivery.Connection{APIURL: "https://api.github.com"}
	base := delivery.GitOpsRequest{Repository: "43", Ref: "main", Path: "shared.yaml", ImageField: "spec.values.image.repository", DigestField: "spec.values.image.tag"}
	original := map[string]delivery.GitOpsSnapshot{}
	current := map[string]delivery.GitOpsSnapshot{}
	components := []domain.DevBaselineComponent{}
	batch := delivery.GitOpsBatchApply{Repository: "43", Ref: "main", OperationID: "leader", ExpectedHeadSHA: strings.Repeat("b", 40)}
	state := domain.EmptyState()
	for _, app := range []string{"a", "b", "c"} {
		selector := base
		selector.ImageRepository = "registry.example/dev/" + app
		observed := domain.DevRuntimeGitOpsObservation{ImageRepository: selector.ImageRepository, Digest: registryDigest(900), HeadSHA: strings.Repeat("b", 40), BlobSHA: strings.Repeat("d", 40), ContentHash: workerconfig.Fingerprint("exact complete original shared file")}
		authority, _ := domain.GitOpsAuthorityFingerprint(connection)
		c := domain.DevBaselineComponent{AuthorityHash: authority, ApplicationID: app, ImageRepository: selector.ImageRepository, Digest: observed.Digest, GitOps: selector, ObservedGitOps: &observed}
		components = append(components, c)
		v := delivery.GitOpsSnapshot{ImageRepository: selector.ImageRepository, Digest: observed.Digest, HeadSHA: strings.Repeat("f", 40), BlobSHA: strings.Repeat("e", 40), Content: "exact complete admitted candidate shared file"}
		original[restorationPath(selector)] = delivery.GitOpsSnapshot{ImageRepository: selector.ImageRepository, Digest: observed.Digest, HeadSHA: observed.HeadSHA, BlobSHA: observed.BlobSHA, Content: "exact complete original shared file"}
		if app != "c" {
			id := "child-" + app
			state.Operations = append(state.Operations, domain.ExternalOperation{Meta: domain.Meta{ID: id, ProductID: "p"}, ApplicationID: app})
			batch.Changes = append(batch.Changes, delivery.GitOpsImageChange{ID: id, Selector: selector, ImageRepository: "registry.example/prod/" + app, Digest: registryDigest(901)})
			v.ImageRepository = "registry.example/prod/" + app
			v.Digest = registryDigest(901)
			selector.ImageRepository = v.ImageRepository
		}
		current[restorationPath(selector)] = v
	}
	pr := delivery.GitOpsPullRequest{Number: 81, URL: "https://fixture.invalid/pulls/81", HeadSHA: strings.Repeat("a", 40), State: "closed", Merged: true}
	peer := &restorationEffectPeer{original: original, current: current, commits: map[string]map[string]delivery.GitOpsSnapshot{pr.HeadSHA: cloneRestorationFiles(current)}, requests: map[string]delivery.GitOpsBatchApply{"leader": batch}, prs: map[string]delivery.GitOpsPullRequest{"leader": pr}, head: strings.Repeat("f", 40)}
	service := NewWithProvider(&memoryStore{state: state}, peer)
	op := domain.ExternalOperation{Meta: domain.Meta{ID: "restore", ProductID: "p"}, DevRestoration: &domain.DevRestorationProof{Components: components, Effects: []domain.DevRestorationEffect{{OperationID: "leader", Connection: connection, Batch: &batch, PullRequest: &pr}}}}
	for _, c := range components {
		v, e := service.readCurrentRestoration(ctx, &state, op, c, connection, peer.head)
		if e != nil {
			t.Fatal(c.ApplicationID, e)
		}
		expected := "registry.example/prod/" + c.ApplicationID
		if c.ApplicationID == "c" {
			expected = c.ImageRepository
		}
		if v.ImageRepository != expected {
			t.Fatal("component adopted another selector", c.ApplicationID, v.ImageRepository)
		}
	}
	copy := op
	proof := *op.DevRestoration
	proof.Effects = append([]domain.DevRestorationEffect(nil), proof.Effects...)
	wrong := batch
	wrong.Changes = append([]delivery.GitOpsImageChange(nil), batch.Changes...)
	wrong.Changes[0].ID = "child-b"
	proof.Effects[0].Batch = &wrong
	copy.DevRestoration = &proof
	if _, e := service.readCurrentRestoration(ctx, &state, copy, components[0], connection, peer.head); e == nil {
		t.Fatal("A adopted B admitted mapping by shared path alone")
	}

	// All original images, including untouched C, remain explicit when a sibling
	// edits the shared file. The restored immutable file may have normalized YAML
	// bytes; C requires the exact admitted restore blob rather than its old blob.
	op.RequestedBy = "operator"
	op.EnvironmentID = "dev"
	op.ReservationEpoch = 2
	op.CreatedAt = time.Now().Add(-time.Minute)
	groupKey := activationPhysicalKey(connection, base.Repository, base.Ref)
	groupID := workerconfig.Fingerprint(groupKey)
	leader := op.ID + "-" + workerconfig.Fingerprint(groupKey)[7:23]
	plan := domain.DevRestorationPlan{ExpectedHash: "frozen-recovery", SettlementHash: "settlement"}
	restoreBatch := delivery.GitOpsBatchApply{Repository: base.Repository, Ref: base.Ref, OperationID: leader, Message: "release-control: " + leader, ExpectedHeadSHA: peer.head}
	for _, c := range components {
		actual, e := service.readCurrentRestoration(ctx, &state, op, c, connection, peer.head)
		if e != nil {
			t.Fatal(e)
		}
		selector := c.GitOps
		selector.ImageRepository = actual.ImageRepository
		noWrite := actual.ImageRepository == c.ImageRepository && actual.Digest == c.Digest
		plan.Components = append(plan.Components, domain.DevRestorationMember{ApplicationID: c.ApplicationID, GroupID: groupID, Original: *c.ObservedGitOps, Current: runtimeGitOpsObservation(actual), Selector: selector, NoWrite: noWrite})
		if !noWrite {
			restoreBatch.Changes = append(restoreBatch.Changes, delivery.GitOpsImageChange{ID: c.ApplicationID, Selector: selector, ExpectedBlobSHA: actual.BlobSHA, ImageRepository: c.ImageRepository, Digest: c.Digest})
		}
	}
	mergedAt := time.Now().Add(-time.Second)
	restorePR := delivery.GitOpsPullRequest{Number: 82, URL: "https://fixture.invalid/pulls/82", HeadSHA: strings.Repeat("9", 40), State: "open"}
	plan.Groups = []domain.DevRestorationGroup{{ID: groupID, LeaderID: leader, Connection: connection, Repository: base.Repository, Ref: base.Ref, Batch: &restoreBatch, IntentHash: workerconfig.Fingerprint(restoreBatch), SendStarted: true, PullRequest: &restorePR}}
	op.DevRestoration.SnapshotHash = plan.ExpectedHash
	op.DevRestorationPlan = &plan
	plan.Hash = restorationPlanHash(plan)
	if !plan.Components[2].NoWrite || len(restoreBatch.Changes) != 2 || validateRestorationPlan(op) != nil {
		t.Fatal("untouched C fabricated a write or vanished from complete plan")
	}
	confirmationTime := time.Now().Add(-2 * time.Second)
	selection := op.DevRestoration.Selection
	request := domain.DevRestorationRequest{RestorationOperationID: op.ID, ExpectedPlanHash: plan.Hash, ExpectedSelection: &selection, RequestKey: "shared-file-confirmation-v1"}
	receipt := domain.ExternalOperation{Meta: domain.Meta{ID: "confirmation", ProductID: "p", CreatedAt: confirmationTime}, Kind: devRestorationConfirmationKind, ParentID: op.ID, RequestedBy: op.RequestedBy, EnvironmentID: op.EnvironmentID, ReservationEpoch: op.ReservationEpoch, RequestKey: request.RequestKey, Status: "SUCCEEDED", Phase: "ATTRIBUTED_CONTROL_CONFIRMATION"}
	receipt.RequestHash = workerconfig.Fingerprint(struct {
		Action, Product, Actor string
		Input                  domain.DevRestorationRequest
	}{"confirm_candidate_dev_restoration", receipt.ProductID, receipt.RequestedBy, request})
	receipt.DevRestorationConfirmation = &domain.DevRestorationConfirmation{Request: request, RestorationHash: plan.ExpectedHash, Plan: plan, TrustGrade: "ATTRIBUTED_CONTROL_CONFIRMATION", ConfirmedAt: confirmationTime}
	receipt.DevRestorationConfirmation.Hash = restorationConfirmationHash(*receipt.DevRestorationConfirmation)
	state.Operations = append(state.Operations, op, receipt)
	service.store = &memoryStore{state: state}
	restored := cloneRestorationFiles(original)
	for k, v := range restored {
		v.Content = "exact normalized admitted restored shared file"
		v.BlobSHA = strings.Repeat("8", 40)
		v.HeadSHA = restorePR.HeadSHA
		restored[k] = v
	}
	peer.current = restored
	peer.commits[restorePR.HeadSHA] = cloneRestorationFiles(restored)
	peer.head = restorePR.HeadSHA
	peer.requests[leader] = restoreBatch
	mergedPR := restorePR
	mergedPR.State = "closed"
	mergedPR.Merged = true
	mergedPR.MergedAt = &mergedAt
	peer.prs[leader] = mergedPR
	if e := service.collectRestorationResults(ctx, &op); e != nil {
		t.Fatal("complete shared-file originals including no-write C", e)
	}
	if len(op.DevRestorationPlan.Groups[0].Result) != 3 {
		t.Fatal("untouched C omitted from exact restore group results")
	}
}
func TestDevRestorationLostPhysicalStagesPersistAcrossReopenWithoutPOST(t *testing.T) {
	f, peer, _, parent := restorationChangedFixture(t, "local")
	ctx := context.Background()
	initial, _ := f.store.Read(ctx)
	for _, kind := range []string{"local", "postgres"} {
		for stage := 1; stage <= 5; stage++ {
			t.Run(fmt.Sprintf("%s/stage%d", kind, stage), func(t *testing.T) {
				store, reopen := openWorkerTestStore(t, kind)
				copy := cloneStateForActivation(t, initial)
				if e := store.Update(ctx, func(st *domain.State) error {
					*st = copy
					cur := operationByID(st, parent.ID)
					cur.LeaseOwner = "first-send"
					cur.LeaseUntil = time.Now().Add(time.Minute)
					return nil
				}); e != nil {
					t.Fatal(e)
				}
				p := *peer
				p.sends = 0
				p.lostAt = stage
				p.hook = nil
				p.prs = map[string]delivery.GitOpsPullRequest{}
				p.requests = map[string]delivery.GitOpsBatchApply{}
				for k, v := range peer.prs {
					p.prs[k] = v
				}
				for k, v := range peer.requests {
					p.requests[k] = v
				}
				service := NewWithProvider(store, &p)
				service.ConfigureWorkers(f.s.externalWorkers)
				service.SetRuntimeObservers(f.s.runtimeObservers)
				state, _ := store.Read(ctx)
				leased := *operationByID(&state, parent.ID)
				if e := service.tickDevRestoration(ctx, leased, "first-send"); e != nil {
					t.Fatal(e)
				}
				if p.sends != stage {
					t.Fatal("lost-stage peer sends", p.sends, stage)
				}
				store.Close()
				store = reopen()
				defer store.Close()
				service.store = store
				state, _ = store.Read(ctx)
				persisted := operationByID(&state, parent.ID)
				if persisted.DevRestorationPlan == nil || !persisted.DevRestorationPlan.Groups[0].SendStarted {
					t.Fatal("reopen lost durable send intent")
				}
				if e := store.Update(ctx, func(st *domain.State) error {
					for i := range st.Operations {
						st.Operations[i].NextAttemptAt = time.Now().Add(time.Hour)
					}
					operationByID(st, parent.ID).NextAttemptAt = time.Time{}
					return nil
				}); e != nil {
					t.Fatal(e)
				}
				if _, e := service.Tick(ctx); e != nil {
					t.Fatal(e)
				}
				state, _ = store.Read(ctx)
				if p.sends != stage || activeDevReservation(&state, "dev") == nil || operationByID(&state, parent.ID).Status == "SUCCEEDED" {
					t.Fatal("reopened UNKNOWN lost-stage repeatedPOST or unlocked")
				}
			})
		}
	}
}

// The combined coordinator fixture is admitted once with all three baseline
// components. Its selected A/B main images use separate Prod repositories;
// untouched C has no candidate build, deployment child or mutation authority.
func restorationThreeSelectedABFixture(t *testing.T, kind string) (*nativeFixture, *restorationEffectPeer, domain.ExternalOperation, domain.ExternalOperation) {
	t.Helper()
	ctx := context.Background()
	f, source, command := mainCandidateFixture(t, kind)
	build, _ := f.s.externalWorkers.Recipe("build")
	devB := f.recipe
	devB.ID = "dev-b"
	devB.ComponentID = "component-b"
	devB.Repository += "-dev-b"
	devB.Hash = workerconfig.PublicationHash(devB)
	devC := f.recipe
	devC.ID = "dev-c"
	devC.ComponentID = "component-c"
	devC.Repository += "-dev-c"
	devC.Hash = workerconfig.PublicationHash(devC)
	prodA := f.recipe
	prodA.ID = "prod-a"
	prodA.Repository += "-prod-a"
	prodA.Hash = workerconfig.PublicationHash(prodA)
	prodB := devB
	prodB.ID = "prod-b"
	prodB.Repository += "-prod"
	prodB.Hash = workerconfig.PublicationHash(prodB)
	publications := []workerconfig.PublicationRecipe{f.recipe, devB, devC, prodA, prodB}
	cfg := workerconfig.Config{Revision: f.s.externalWorkers.Revision(), LeaseSeconds: 5, Recipes: []workerconfig.Recipe{build}, Publications: publications, Credentials: []workerconfig.Credential{{ID: "build", Principal: "worker/build", TokenEnv: "NATIVE_BUILD_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"local_oci_build"}, ExpiresAt: time.Now().Add(time.Hour)}, {ID: "publish", Principal: "worker/publish", TokenEnv: "NATIVE_PUBLISH_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"registry_publish"}, ExpiresAt: time.Now().Add(time.Hour)}}}
	authority, e := workerconfig.New(cfg, "native-control-fixture", os.Getenv)
	if e != nil {
		t.Fatal(e)
	}
	f.s.ConfigureWorkers(authority)
	f.p, _ = authority.Authenticate(nativeToken, time.Now())
	for _, r := range []workerconfig.PublicationRecipe{devB, devC} {
		exec(t, f.s, domain.Command{Action: "create_application", ID: r.ComponentID, ProductID: "p", Data: map[string]any{"name": r.ComponentID, "repository_id": "source"}})
		exec(t, f.s, domain.Command{Action: "configure_component", ProductID: "p", Data: map[string]any{"application_id": r.ComponentID, "connection_id": "conn", "mode": "NATIVE", "profile": r.Profile, "image_repository": nativeImage(r), "native": domain.NativeBinding{BuildRecipeID: build.ID, PublicationRecipeID: r.ID, Parameters: map[string]any{}}}})
	}
	for _, app := range []string{"component", "component-b", "component-c"} {
		exec(t, f.s, domain.Command{Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": "dev", "application_id": app, "purpose": "DEV", "connection_id": "conn", "repository_id": "gitops", "ref": "main", "path": "shared.yaml", "image_field": "spec.values.image.repository", "digest_field": "spec.values.image.tag", "allow_deploy": true, "gitops_mode": "PR"}})
	}
	exec(t, f.s, domain.Command{Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": "prod", "application_id": "component-b", "purpose": "PROD", "connection_id": "conn", "repository_id": "gitops", "ref": "main", "path": "shared.yaml", "image_field": "spec.values.image.repository", "digest_field": "spec.values.image.tag", "allow_deploy": true, "gitops_mode": "PR"}})
	var request domain.MainCandidateRequest
	raw, _ := json.Marshal(command.Data)
	if e = json.Unmarshal(raw, &request); e != nil {
		t.Fatal(e)
	}
	request.MainSources[0].Native.PublicationRecipeID = prodA.ID
	selected := request.MainSources[0]
	selected.ApplicationID = "component-b"
	selected.Native.PublicationRecipeID = prodB.ID
	request.MainSources = append(request.MainSources, selected)
	raw, _ = json.Marshal(request)
	if e = json.Unmarshal(raw, &command.Data); e != nil {
		t.Fatal(e)
	}
	v, e := f.s.Execute(ctx, command)
	if e != nil {
		t.Fatal(e)
	}
	candidate := v.(domain.ExternalOperation)
	activationTicks(t, f, 8)
	automaticBuild(t, f)
	automaticBuild(t, f)
	activationTicks(t, f, 10)
	for range 2 {
		lease, e := f.s.ClaimWorker(ctx, f.p, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "registry_publish"})
		if e != nil || lease == nil {
			t.Fatal("combined candidate publisher claim", e)
		}
		if _, e = f.s.StartWorker(ctx, f.p, workerRequest(lease)); e != nil {
			t.Fatal(e)
		}
		pub := lease.Job.Inputs.Publication
		f.publisher.AllowedOrigin = pub.Origin
		f.publisher.AllowedRepository = pub.Repository
		f.publisher.Plan = ocipublisher.Plan{ID: lease.Job.ID, Approved: true, ProductID: "p", ComponentID: pub.ComponentID, RepositoryID: "source", Origin: pub.Origin, Repository: pub.Repository, Receipt: pub.Receipt}
		if _, e = ocipublisher.Publish(ctx, f.publisher); e != nil {
			t.Fatal(e)
		}
		if _, e = f.s.ReportWorker(ctx, f.p, domain.WorkerReport{WorkerAttemptRequest: workerRequest(lease), Result: "PASSED", Publication: &domain.WorkerPublicationReport{ManifestDigest: pub.Receipt.ManifestDigest}}); e != nil {
			t.Fatal(e)
		}
	}
	activationTicks(t, f, 16)
	state, _ := f.store.Read(ctx)
	candidate = *operationByID(&state, candidate.ID)
	if candidate.Status != "SUCCEEDED" || len(candidate.ChildIDs) != 2 {
		t.Fatal("combined selected candidate incomplete")
	}
	exec(t, f.s, domain.Command{Action: "plan_composition", ID: "combined-three-originals", ProductID: "p", Data: map[string]any{"name": "A+B selected, C untouched full shared baseline", "environment_id": "dev", "components": []domain.CompositionComponent{component("component", "rev"), component("component-b", "rev"), component("component-c", "rev")}}})
	exec(t, f.s, domain.Command{Action: "select_composition", ID: "combined-three-originals"})
	state, _ = f.store.Read(ctx)
	v, e = f.s.Execute(ctx, domain.Command{Action: "reserve_candidate_dev", ProductID: "p", Actor: "operator", Data: map[string]any{"candidate_operation_id": candidate.ID, "environment_id": "dev", "request_key": "combined-three-reservation-v1", "expected_selection": selectionToken(*environmentByID(&state, "dev"))}})
	if e != nil {
		t.Fatal(e)
	}
	hold := v.(domain.ExternalOperation)
	p := &devActivationProvider{mainCandidateProvider: source}
	f.s.provider = p
	prepareActivationFixtureBaseline(t, f, p, hold)
	activation := restorationCommand(t, f, "request_candidate_dev_restoration", hold.ID, "unused")
	activation.Action = "deploy_candidate_dev"
	activation.Data["request_key"] = "combined-selected-activation-v1"
	v, e = f.s.Execute(ctx, activation)
	if e != nil {
		t.Fatal(e)
	}
	activated := v.(domain.ExternalOperation)
	activationTicks(t, f, 24)
	p.merged = true
	activationTicks(t, f, 24)
	state, _ = f.store.Read(ctx)
	if operationByID(&state, activated.ID).Status != "SUCCEEDED" {
		t.Fatal("combined selected activation incomplete")
	}
	peer := newRestorationEffectPeer(t, f, p, hold)
	f.s.provider = peer
	return f, peer, hold, activated
}
func TestDevRestorationCombinedSelectedABUntouchedCSharedFile(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, p, hold, activation := restorationThreeSelectedABFixture(t, kind)
			initial, _ := f.store.Read(ctx)
			capture, _ := latestBaseline(&initial, hold.ID)
			baselineHash := baselineCaptureHash(*capture)
			candidate := operationByID(&initial, hold.DevReservation.CandidateOperationID)
			candidateHash := workerconfig.Fingerprint(candidate.MainCandidate)
			artifactHash := workerconfig.Fingerprint(initial.DeliveryArtifacts)
			builds, publishes := 0, 0
			for _, j := range initial.WorkerJobs {
				if j.Capability == "local_oci_build" {
					builds++
				}
				if j.Capability == "registry_publish" {
					publishes++
				}
			}
			if len(capture.DevBaseline.Components) != 3 || len(activation.ChildIDs) != 2 || len(activation.DevActivation.Groups) != 1 {
				t.Fatal("combined original/selected memberships wrong")
			}
			for _, m := range activation.DevActivation.Components {
				for _, original := range capture.DevBaseline.Components {
					if original.ApplicationID == m.Artifact.ApplicationID && original.ImageRepository == m.Artifact.ImageRepository {
						t.Fatal("selected candidate did not use independent Prod repository")
					}
				}
			}
			command := restorationCommand(t, f, "request_candidate_dev_restoration", hold.ID, "combined-restore-request-v1")
			v, e := f.s.Execute(ctx, command)
			if e != nil {
				t.Fatal(e)
			}
			parent := v.(domain.ExternalOperation)
			completeRestorationRegistry(t, f)
			forceActivationTick(t, f, parent.ID)
			state, _ := f.store.Read(ctx)
			parent = *operationByID(&state, parent.ID)
			if parent.Phase != "PREPARE_PROPOSALS" || len(parent.DevRestorationPlan.Groups) != 1 || len(parent.DevRestorationPlan.Groups[0].Batch.Changes) != 2 || len(parent.DevRestorationPlan.Components) != 3 {
				t.Fatal("combined conservative restoration plan incomplete", parent.Phase, parent.Detail)
			}
			untouched := false
			for _, m := range parent.DevRestorationPlan.Components {
				if m.ApplicationID == "component-c" {
					untouched = m.NoWrite && m.Current.ImageRepository == m.Original.ImageRepository && m.Current.Digest == m.Original.Digest && m.Current.ContentHash != m.Original.ContentHash
				}
			}
			if !untouched {
				t.Fatal("C same-file exact known candidate blob/no-write proof missing")
			}
			parent, proof := completeChangedRestoration(t, f, p, parent)
			if len(proof.DevRuntimeResult.Components) != 3 {
				t.Fatal("full original runtime membership missing")
			}
			roles := 0
			for _, r := range proof.DevRuntimeResult.Components {
				roles += len(r.Roles)
			}
			if roles != 6 {
				t.Fatal("API+worker complete six-role coverage missing")
			}
			release := restorationCommand(t, f, "release_candidate_dev_after_restoration", parent.ID, "combined-restore-release-v1")
			release.Data["restoration_verification_operation_id"] = proof.ID
			if _, e = f.s.Execute(ctx, release); e != nil {
				t.Fatal(e)
			}
			state, _ = f.store.Read(ctx)
			if activeDevReservation(&state, "dev") != nil || p.sends != 5 {
				t.Fatal("combined scoped unlock/one grouped proposal failed")
			}
			afterCapture, _ := latestBaseline(&state, hold.ID)
			if baselineCaptureHash(*afterCapture) != baselineHash || workerconfig.Fingerprint(operationByID(&state, candidate.ID).MainCandidate) != candidateHash || workerconfig.Fingerprint(state.DeliveryArtifacts) != artifactHash {
				t.Fatal("combined original baseline/candidate/artifact provenance rewritten")
			}
			afterBuilds, afterPublishes := 0, 0
			for _, j := range state.WorkerJobs {
				if j.Capability == "local_oci_build" {
					afterBuilds++
				}
				if j.Capability == "registry_publish" {
					afterPublishes++
				}
			}
			if afterBuilds != builds || afterPublishes != publishes {
				t.Fatal("restoration rebuilt/republished original images")
			}
			retention := f.s.evaluateRetention(state, "p", time.Now().UTC())
			if retention.DeletionAllowed || retention.GlobalInventoryComplete {
				t.Fatal("combined release granted globalGC")
			}
			t.Log("full coordinator local/isolatedPG selectedA+B distinctProd versusoriginalDev, untouchedC samefile, one restoregroup2changes, all3/6roles and scopedunlock; GitHub/Kube/registryattestation peer simulated")
		})
	}
}

// Concurrent stale observation regression: a valid current winner remains immutable.
func TestDevRestorationUnapprovedClassificationFencesWorker(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, peer, _, parent := restorationChangedFixture(t, kind)
			forceActivationTick(t, f, parent.ID)
			forceActivationTick(t, f, parent.ID)
			initial, _ := f.store.Read(ctx)
			parent = *operationByID(&initial, parent.ID)
			confirm := restorationCommand(t, f, "confirm_candidate_dev_restoration", parent.ID, "classification-fence-confirm")
			confirm.Data["expected_plan_hash"] = parent.DevRestorationPlan.Hash
			raw, err := f.s.Execute(ctx, confirm)
			if err != nil {
				t.Fatal(err)
			}
			confirmation := raw.(domain.ExternalOperation)
			leader := parent.DevRestorationPlan.Groups[0].LeaderID
			pr := peer.prs[leader]
			pr.Merged, pr.State = true, "closed"
			at := confirmation.DevRestorationConfirmation.ConfirmedAt
			pr.MergedAt = &at // Conservatively invalid chronology still requires live worker authority to mark.
			peer.prs[leader] = pr
			initial, _ = f.store.Read(ctx)
			for name, mutate := range map[string]func(*domain.State){
				"lease-owner":   func(st *domain.State) { operationByID(st, parent.ID).LeaseOwner = "replacement" },
				"lease-expired": func(st *domain.State) { operationByID(st, parent.ID).LeaseUntil = time.Now().Add(-time.Second) },
				"epoch":         func(st *domain.State) { environmentByID(st, "dev").ReservationEpoch++ },
				"selection":     func(st *domain.State) { environmentByID(st, "dev").SelectionGeneration++ },
				"config":        func(st *domain.State) { environmentBinding(st, "dev", "component").Path = "foreign.yaml" },
				"grant": func(st *domain.State) {
					for i := range st.ConnectionGrants {
						st.ConnectionGrants[i].ProductID = "foreign"
					}
				},
				"final-save-owner":   func(st *domain.State) { operationByID(st, parent.ID).LeaseOwner = "replacement" },
				"final-save-expired": func(st *domain.State) { operationByID(st, parent.ID).LeaseUntil = time.Now().Add(-time.Second) },
			} {
				t.Run(name, func(t *testing.T) {
					store, _ := openWorkerTestStore(t, kind)
					defer store.Close()
					copy := cloneStateForActivation(t, initial)
					op := operationByID(&copy, parent.ID)
					op.LeaseOwner, op.LeaseUntil = "classification-worker", time.Now().Add(time.Minute)
					leased := *op
					late := strings.HasPrefix(name, "final-save-")
					if !late {
						mutate(&copy)
					}
					if e := store.Update(ctx, func(st *domain.State) error { *st = copy; return nil }); e != nil {
						t.Fatal(e)
					}
					p := *peer
					p.afterObserve = nil
					if late {
						p.afterObserve = func() {
							if e := store.Update(ctx, func(st *domain.State) error { mutate(st); return nil }); e != nil {
								t.Fatal(e)
							}
						}
					}
					service := NewWithProvider(store, &p)
					service.ConfigureWorkers(f.s.externalWorkers)
					service.SetRuntimeObservers(f.s.runtimeObservers)
					if e := service.markRestorationUnapproved(ctx, leased, "classification-worker"); !errors.Is(e, ErrValidation) {
						t.Fatal("stale worker classification accepted", e)
					}
					saved, e := store.Read(ctx)
					if e != nil {
						t.Fatal(e)
					}
					current := operationByID(&saved, parent.ID)
					if current.DevRestorationUnapproved || current.Status != parent.Status || current.Phase != parent.Phase || current.LeaseOwner == "" {
						t.Fatal("stale classification mutated parent or cleared replacement lease")
					}
				})
			}
		})
	}
}

type restorationStaleConfirmationProvider struct {
	*restorationEffectPeer
	target  string
	arrived chan struct{}
	resume  chan struct{}
	calls   atomic.Int32
}

func (p *restorationStaleConfirmationProvider) ObserveGitOpsBatchPR(ctx context.Context, c delivery.Connection, b delivery.GitOpsBatchApply, expected delivery.GitOpsPullRequest) (delivery.GitOpsPullRequest, error) {
	if b.OperationID == p.target && p.calls.Add(1) == 1 {
		close(p.arrived)
		<-p.resume
	}
	return p.restorationEffectPeer.ObserveGitOpsBatchPR(ctx, c, b, expected)
}
func TestDevRestorationStaleConfirmationObserverPreservesWinner(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, peer, hold, parent := restorationChangedFixture(t, kind)
			forceActivationTick(t, f, parent.ID)
			forceActivationTick(t, f, parent.ID)
			st, _ := f.store.Read(ctx)
			parent = *operationByID(&st, parent.ID)
			if parent.Phase != "WAIT_EXTERNAL_MERGE" {
				t.Fatal("prepared plan unavailable", parent.Phase)
			}
			p := &restorationStaleConfirmationProvider{restorationEffectPeer: peer, target: parent.DevRestorationPlan.Groups[0].LeaderID, arrived: make(chan struct{}), resume: make(chan struct{})}
			f.s.provider = p
			stale := restorationCommand(t, f, "confirm_candidate_dev_restoration", parent.ID, "independent-stale-confirm-observer")
			stale.Data["expected_plan_hash"] = parent.DevRestorationPlan.Hash
			type result struct {
				value any
				err   error
			}
			losing := make(chan result, 1)
			go func() { v, e := f.s.Execute(ctx, stale); losing <- result{v, e} }()
			select {
			case <-p.arrived:
			case <-time.After(30 * time.Second):
				close(p.resume)
				t.Fatal("stale request did not reach pre-observation barrier")
			}
			winning := restorationCommand(t, f, "confirm_candidate_dev_restoration", parent.ID, "independent-current-confirm-winner")
			winning.Data["expected_plan_hash"] = parent.DevRestorationPlan.Hash
			raw, e := f.s.Execute(ctx, winning)
			if e != nil {
				close(p.resume)
				t.Fatal("winning current confirmation", e)
			}
			confirmation := raw.(domain.ExternalOperation)
			confirmationHash := workerconfig.Fingerprint(confirmation)
			peer.merge(p.target)
			pr := peer.prs[p.target]
			if pr.MergedAt == nil || !pr.MergedAt.After(confirmation.DevRestorationConfirmation.ConfirmedAt) {
				close(p.resume)
				t.Fatal("fixture merge chronology not after current winner")
			}
			close(p.resume)
			loser := <-losing
			if loser.err == nil || !errors.Is(loser.err, ErrValidation) {
				t.Errorf("stale losing confirmation did not return validation: %v", loser.err)
			}
			st, _ = f.store.Read(ctx)
			current := *operationByID(&st, parent.ID)
			count := 0
			for _, op := range st.Operations {
				if op.Kind == devRestorationConfirmationKind && op.ParentID == parent.ID {
					count++
				}
			}
			if current.DevRestorationUnapproved || current.Status == "FAILED" || current.Phase == "UNAPPROVED_EFFECT" || count != 1 || restorationConfirmation(&st, current) == nil {
				t.Errorf("stale unconfirmed observation poisoned valid confirmed merge: unapproved=%v status=%s phase=%s confirmations=%d", current.DevRestorationUnapproved, current.Status, current.Phase, count)
				return
			}
			if workerconfig.Fingerprint(operationByID(&st, confirmation.ID)) != confirmationHash {
				t.Fatal("winner immutable receipt changed")
			}
			forceActivationTick(t, f, parent.ID)
			for _, o := range f.s.runtimeObservers {
				o.(*devRuntimeObserver).value.Flux.Revision = "main@sha1:" + peer.head
			}
			raw, e = f.s.Execute(ctx, restorationCommand(t, f, "verify_candidate_dev_restoration", parent.ID, "independent-stale-confirm-verify"))
			if e != nil {
				t.Fatal("winner could not verify", e)
			}
			proof := raw.(domain.ExternalOperation)
			completeRestorationRegistry(t, f)
			forceActivationTick(t, f, proof.ID)
			st, _ = f.store.Read(ctx)
			proof = *operationByID(&st, proof.ID)
			if proof.Status != "SUCCEEDED" {
				t.Fatal("winner runtime unverified", proof.Status, proof.Phase)
			}
			command := restorationCommand(t, f, "release_candidate_dev_after_restoration", parent.ID, "independent-stale-confirm-unlock")
			command.Data["restoration_verification_operation_id"] = proof.ID
			if _, e = f.s.Execute(ctx, command); e != nil {
				t.Fatal("winner could not unlock", e)
			}
			st, _ = f.store.Read(ctx)
			if activeDevReservation(&st, "dev") != nil {
				t.Fatal("stale loser retained winner hold", hold.ID)
			}
		})
	}
}

func TestDevRestorationRenewedRetentionSettlement(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		for _, mode := range []string{"closed_confirmed", "prepared_unsent", "admitted_no_plan", "merged_verified"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				var f *nativeFixture
				var peer *restorationEffectPeer
				var hold, prior domain.ExternalOperation
				if mode == "admitted_no_plan" {
					f, _, hold, _ = devActivationMultiFixture(t, kind)
					raw, e := f.s.Execute(ctx, restorationCommand(t, f, "request_candidate_dev_restoration", hold.ID, "independent-no-plan-prior"))
					if e != nil {
						t.Fatal(e)
					}
					prior = raw.(domain.ExternalOperation)
					if prior.DevRestorationPlan != nil {
						t.Fatal("admitted no-plan fixture unexpectedly prepared")
					}
				} else {
					f, peer, hold, prior = restorationChangedFixture(t, kind)
					if mode == "merged_verified" {
						forceActivationTick(t, f, prior.ID)
						forceActivationTick(t, f, prior.ID)
						state, _ := f.store.Read(ctx)
						prior = *operationByID(&state, prior.ID)
						confirm := restorationCommand(t, f, "confirm_candidate_dev_restoration", prior.ID, "prior-merged-confirm-v1")
						confirm.Data["expected_plan_hash"] = prior.DevRestorationPlan.Hash
						if _, err := f.s.Execute(ctx, confirm); err != nil {
							t.Fatal(err)
						}
						peer.merge(prior.DevRestorationPlan.Groups[0].LeaderID)
						forceActivationTick(t, f, prior.ID)
						for _, observer := range f.s.runtimeObservers {
							observer.(*devRuntimeObserver).value.Flux.Revision = "main@sha1:" + peer.head
						}
						value, err := f.s.Execute(ctx, restorationCommand(t, f, "verify_candidate_dev_restoration", prior.ID, "prior-merged-verify-v1"))
						if err != nil {
							t.Fatal(err)
						}
						oldProof := value.(domain.ExternalOperation)
						completeRestorationRegistry(t, f)
						forceActivationTick(t, f, oldProof.ID)
						state, _ = f.store.Read(ctx)
						if operationByID(&state, oldProof.ID).Status != "SUCCEEDED" {
							t.Fatal("prior merged verification incomplete")
						}
						prior = *operationByID(&state, prior.ID)
					} else if mode == "closed_confirmed" {
						forceActivationTick(t, f, prior.ID)
						forceActivationTick(t, f, prior.ID)
						s, _ := f.store.Read(ctx)
						prior = *operationByID(&s, prior.ID)
						c := restorationCommand(t, f, "confirm_candidate_dev_restoration", prior.ID, "independent-prior-confirmation")
						c.Data["expected_plan_hash"] = prior.DevRestorationPlan.Hash
						if _, e := f.s.Execute(ctx, c); e != nil {
							t.Fatal(e)
						}
						for _, g := range prior.DevRestorationPlan.Groups {
							pr := peer.prs[g.LeaderID]
							pr.State = "closed"
							peer.prs[g.LeaderID] = pr
						}
					} else {
						for _, g := range prior.DevRestorationPlan.Groups {
							if g.SendStarted {
								t.Fatal("prepared-unsent fixture already sent")
							}
						}
					}
				}
				before, _ := f.store.Read(ctx)
				candidateHash := workerconfig.Fingerprint(operationByID(&before, hold.DevReservation.CandidateOperationID).MainCandidate)
				capture, _ := latestBaseline(&before, hold.ID)
				captureID := capture.ID
				captureHash := baselineCaptureHash(*capture)
				raw, e := f.s.Execute(ctx, restorationCommand(t, f, "request_candidate_dev_restoration", hold.ID, "independent-renewed-"+mode))
				if e != nil {
					t.Fatal(e)
				}
				next := raw.(domain.ExternalOperation)
				if next.DevRestoration.Generation != prior.DevRestoration.Generation+1 {
					t.Fatal("ordinary explicit renewal did not advance")
				}
				completeRestorationRegistry(t, f)
				var proof domain.ExternalOperation
				if peer != nil && mode != "merged_verified" {
					forceActivationTick(t, f, next.ID)
					next, proof = completeChangedRestoration(t, f, peer, next)
				} else {
					for range 3 {
						forceActivationTick(t, f, next.ID)
					}
					s, _ := f.store.Read(ctx)
					next = *operationByID(&s, next.ID)
					c := restorationCommand(t, f, "confirm_candidate_dev_restoration", next.ID, "independent-renewed-confirm")
					c.Data["expected_plan_hash"] = next.DevRestorationPlan.Hash
					if _, e = f.s.Execute(ctx, c); e != nil {
						t.Fatal(e)
					}
					forceActivationTick(t, f, next.ID)
					raw, e = f.s.Execute(ctx, restorationCommand(t, f, "verify_candidate_dev_restoration", next.ID, "independent-renewed-verify"))
					if e != nil {
						t.Fatal(e)
					}
					proof = raw.(domain.ExternalOperation)
					completeRestorationRegistry(t, f)
					forceActivationTick(t, f, proof.ID)
				}
				c := restorationCommand(t, f, "release_candidate_dev_after_restoration", next.ID, "independent-renewed-unlock")
				c.Data["restoration_verification_operation_id"] = proof.ID
				raw, e = f.s.Execute(ctx, c)
				if e != nil {
					t.Fatal(e)
				}
				receipt := raw.(domain.ExternalOperation)
				s, _ := f.store.Read(ctx)
				if !domain.ValidRestorationRelease(&s, receipt, *operationByID(&s, hold.ID)) {
					t.Fatal("ordinary final typed release invalid")
				}
				if workerconfig.Fingerprint(operationByID(&s, hold.DevReservation.CandidateOperationID).MainCandidate) != candidateHash || baselineCaptureHash(*operationByID(&s, captureID)) != captureHash {
					t.Fatal("original candidate/capture changed")
				}
				r := domain.EvaluateRetention(s, "p", time.Now().UTC(), nil)
				for _, im := range r.Images {
					for _, reason := range im.Reasons {
						if strings.HasPrefix(reason, "dev_restoration:") {
							t.Error("normal earlier/current attempt root permanently pinned after exact successor", reason, im.Identity)
						}
					}
				}
				extraRepo := "registry.example/independent/renewed-extra"
				extraDigest := fmt.Sprintf("sha256:%064x", 1234)
				orphan := domain.ExternalOperation{Meta: domain.Meta{ID: "renewed-orphan", ProductID: "p"}, ParentID: prior.ID, Kind: "DEV_RESTORATION_CONFIRMATION", DevRestorationConfirmation: &domain.DevRestorationConfirmation{Plan: domain.DevRestorationPlan{Components: []domain.DevRestorationMember{{Original: domain.DevRuntimeGitOpsObservation{ImageRepository: extraRepo, Digest: extraDigest}}}}}}
				s.Operations = append(s.Operations, orphan)
				r = domain.EvaluateRetention(s, "p", time.Now().UTC(), nil)
				found := false
				for _, im := range r.Images {
					if im.ImageRepository == extraRepo && im.Digest == extraDigest && im.Protected {
						for _, reason := range im.Reasons {
							if reason == "dev_restoration:renewed-orphan" {
								found = true
							}
						}
					}
				}
				if !found {
					t.Fatal("unknown orphan inherited prior-attempt settlement")
				}
				if r.DeletionAllowed {
					t.Fatal("retention projection enabled physical deletion")
				}
			})
		}
	}
}
