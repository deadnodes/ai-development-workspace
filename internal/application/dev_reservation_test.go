package application

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/ocipublisher"
)

func devReservationFixture(t *testing.T, kind string) (*nativeFixture, domain.ExternalOperation, domain.Command) {
	t.Helper()
	ctx := context.Background()
	f, _, c := mainCandidateFixture(t, kind)
	v, err := f.s.Execute(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	candidate := v.(domain.ExternalOperation)
	for range 3 {
		nativeTick(t, f)
	}
	automaticBuild(t, f)
	for range 4 {
		nativeTick(t, f)
	}
	lease, err := f.s.ClaimWorker(ctx, f.p, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "registry_publish"})
	if err != nil || lease == nil {
		t.Fatal("publisher claim", err)
	}
	if _, err = f.s.StartWorker(ctx, f.p, workerRequest(lease)); err != nil {
		t.Fatal(err)
	}
	pub := lease.Job.Inputs.Publication
	f.publisher.Plan = ocipublisher.Plan{ID: lease.Job.ID, Approved: true, ProductID: "p", ComponentID: pub.ComponentID, RepositoryID: "source", Origin: pub.Origin, Repository: pub.Repository, Receipt: pub.Receipt}
	if _, err = ocipublisher.Publish(ctx, f.publisher); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.ReportWorker(ctx, f.p, domain.WorkerReport{WorkerAttemptRequest: workerRequest(lease), Result: "PASSED", Publication: &domain.WorkerPublicationReport{ManifestDigest: pub.Receipt.ManifestDigest}}); err != nil {
		t.Fatal(err)
	}
	for range 15 {
		nativeTick(t, f)
		st, _ := f.store.Read(ctx)
		if operationByID(&st, candidate.ID).Status == "SUCCEEDED" {
			break
		}
	}
	st, _ := f.store.Read(ctx)
	candidate = *operationByID(&st, candidate.ID)
	if candidate.Status != "SUCCEEDED" || candidate.DeploymentState != "READY_FOR_VERIFICATION" {
		t.Fatalf("candidate completion %s %s %s", candidate.Status, candidate.DeploymentState, candidate.Detail)
	}
	// Build a selected baseline A+B. This composition is planned/selected intent,
	// not a fabricated deployment or runtime claim. B is a separate integration.
	exec(t, f.s, domain.Command{Action: "create_feature", ID: "fb", ProductID: "p", Data: map[string]any{"title": "B", "goal": "B"}})
	exec(t, f.s, domain.Command{Action: "create_integration", ID: "ib", FeatureID: "fb", Data: map[string]any{"title": "B", "objective": "B", "repositories": []string{"source"}}})
	exec(t, f.s, domain.Command{Action: "record_integration_revision", ID: "b1", IntegrationID: "ib", Data: map[string]any{"repository_id": "source", "branch": "feature/b", "base_commit": shaBase, "head_commit": strings.Repeat("e", 40), "commits": []string{strings.Repeat("e", 40)}}})
	flowPlan(t, f.s, "dev-baseline", "dev", "rev", "b1")
	exec(t, f.s, domain.Command{Action: "select_composition", ID: "dev-baseline"})
	st, _ = f.store.Read(ctx)
	return f, candidate, domain.Command{Action: "reserve_candidate_dev", Actor: "operator", ProductID: "p", Data: map[string]any{"candidate_operation_id": candidate.ID, "environment_id": "dev", "request_key": "fixture-reserve-main-A", "expected_selection": selectionToken(*environmentByID(&st, "dev"))}}
}
func releaseReservationCommand(st domain.State, hold domain.ExternalOperation) domain.Command {
	return domain.Command{Action: "release_candidate_dev", Actor: hold.RequestedBy, ProductID: hold.ProductID, Data: map[string]any{"reservation_operation_id": hold.ID, "request_key": "fixture-release-main-A", "expected_selection": selectionToken(*environmentByID(&st, hold.EnvironmentID))}}
}
func assertReservationPreserved(t *testing.T, before, after domain.Environment, generationDelta, epochDelta uint64) {
	t.Helper()
	after.SelectionGeneration -= generationDelta
	after.ReservationEpoch -= epochDelta
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("reservation changed baseline/environment: before=%+v after=%+v", before, after)
	}
}
func TestDevReservationPreservesBaselineReplayReleaseAndEpoch(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, candidate, c := devReservationFixture(t, kind)
			before, _ := f.store.Read(ctx)
			envBefore := *environmentByID(&before, "dev")
			value, err := f.s.Execute(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			hold := value.(domain.ExternalOperation)
			st, _ := f.store.Read(ctx)
			if hold.Kind != "DEV_RESERVATION" || hold.Status != "SUCCEEDED" || hold.Phase != "RESERVED" || hold.DevReservation.CandidateOperationID != candidate.ID {
				t.Fatal("invalid reservation receipt")
			}
			assertReservationPreserved(t, envBefore, *environmentByID(&st, "dev"), 1, 1)
			if !reflect.DeepEqual(before.Compositions, st.Compositions) || !reflect.DeepEqual(before.ComponentBuilds, st.ComponentBuilds) || !reflect.DeepEqual(before.IntegrationRevisions, st.IntegrationRevisions) {
				t.Fatal("baseline/source/configuration mutated")
			}
			old := domain.ExternalOperation{Meta: domain.Meta{ID: "old", ProductID: "p"}, Kind: "DEPLOY", EnvironmentID: "dev", ReservationEpoch: envBefore.ReservationEpoch}
			if reservationOperationAllowed(&st, old) == nil {
				t.Fatal("old operation retained rights")
			}
			query, err := f.s.Query(ctx, "get_environment_state", "dev")
			if err != nil || query.(map[string]any)["dev_reservation"].(map[string]any)["active"] != true {
				t.Fatal("reservation query missing", err)
			}
			for _, command := range []domain.Command{
				{Action: "select_composition", ID: "dev-baseline"},
				{Action: "reconcile_composition", ID: "dev-baseline"},
				{Action: "assemble_environment", ProductID: "p", Data: map[string]any{"environment_id": "dev"}},
				{Action: "update_environment", ID: "dev", Data: map[string]any{"namespace": "other"}},
				{Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": "dev"}},
				automaticCommand(),
				{Action: "delete_product", ProductID: "p", Data: map[string]any{"name": before.Products[0].Name}},
			} {
				command.Actor = "operator"
				if _, err = f.s.Execute(ctx, command); err == nil {
					t.Fatal("active hold admitted", command.Action)
				}
			}
			f.store.Close()
			f.store = f.reopen()
			f.s.store = f.store
			beforeReplay, _ := f.store.Read(ctx)
			value, err = f.s.Execute(ctx, c)
			if err != nil || value.(domain.ExternalOperation).ID != hold.ID {
				t.Fatal("durable reserve replay", err)
			}
			afterReplay, _ := f.store.Read(ctx)
			if !reflect.DeepEqual(beforeReplay, afterReplay) {
				t.Fatal("replay changed state")
			}
			conflict := c
			conflict.Actor = "other"
			if _, err = f.s.Execute(ctx, conflict); err == nil {
				t.Fatal("actor conflict admitted")
			}
			release := releaseReservationCommand(afterReplay, hold)
			conflict = release
			conflict.Actor = "other"
			if _, err = f.s.Execute(ctx, conflict); err == nil {
				t.Fatal("foreign release admitted")
			}
			value, err = f.s.Execute(ctx, release)
			if err != nil {
				t.Fatal(err)
			}
			released := value.(domain.ExternalOperation)
			st, _ = f.store.Read(ctx)
			assertReservationPreserved(t, envBefore, *environmentByID(&st, "dev"), 2, 1)
			if activeDevReservation(&st, "dev") != nil || reservationOperationAllowed(&st, old) == nil {
				t.Fatal("release reactivated old rights")
			}
			state, _ := json.Marshal(st)
			for _, replay := range []domain.Command{c, release} {
				value, err = f.s.Execute(ctx, replay)
				if err != nil {
					t.Fatal(err)
				}
				if replay.Action == "release_candidate_dev" && value.(domain.ExternalOperation).ID != released.ID {
					t.Fatal("release replay identity")
				}
			}
			after, _ := f.store.Read(ctx)
			a, _ := json.Marshal(after)
			if string(a) != string(state) {
				t.Fatal("historical replay changed released state")
			}
			// A fresh explicit standalone deployment captures the post-release epoch.
			fresh := automaticCommand()
			fresh.Data["request_key"] = "fresh-after-reservation"
			value, err = f.s.Execute(ctx, fresh)
			if err != nil {
				t.Fatal("fresh explicit deployment", err)
			}
			newOp := value.(domain.ExternalOperation)
			if newOp.ReservationEpoch != 1 || reservationOperationAllowed(&after, newOp) != nil {
				t.Fatal("new operation epoch missing")
			}
		})
	}
}
func TestDevReservationConcurrentCASAndUncertainEffects(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, _, c := devReservationFixture(t, kind)
			var wg sync.WaitGroup
			results := make(chan error, 2)
			for _, actor := range []string{"owner-a", "owner-b"} {
				wg.Add(1)
				go func(actor string) {
					defer wg.Done()
					copy := c
					copy.Actor = actor
					copy.Data = map[string]any{}
					for k, v := range c.Data {
						copy.Data[k] = v
					}
					copy.Data["request_key"] = "concurrent-key-" + actor
					_, err := f.s.Execute(ctx, copy)
					results <- err
				}(actor)
			}
			wg.Wait()
			close(results)
			passed := 0
			for err := range results {
				if err == nil {
					passed++
				}
			}
			if passed != 1 {
				t.Fatalf("concurrent reservations passed=%d", passed)
			}
			st, _ := f.store.Read(ctx)
			hold := *activeDevReservation(&st, "dev")
			release := releaseReservationCommand(st, hold)
			if _, err := f.s.Execute(ctx, release); err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name, phase, status string
				pr                  *delivery.GitOpsPullRequest
			}{
				{"expired_uncertain", "APPLY_UNCERTAIN", "RUNNING", nil}, {"cancelled_uncertain", "APPLY_UNCERTAIN", "CANCELLED", nil}, {"cancelled_PR", "WAIT_GITOPS_PR", "CANCELLED", &delivery.GitOpsPullRequest{Number: 1, State: "open"}}, {"terminal_PR", "WAIT_GITOPS_PR", "SUCCEEDED", &delivery.GitOpsPullRequest{Number: 1, State: "open"}},
			} {
				t.Run(test.name, func(t *testing.T) {
					if err := f.store.Update(ctx, func(st *domain.State) error {
						st.Operations = append(st.Operations, domain.ExternalOperation{Meta: domain.Meta{ID: "uncertain-" + test.name, ProductID: "p"}, Kind: "DEPLOY", EnvironmentID: "dev", Status: test.status, Phase: test.phase, GitOpsPR: test.pr, LeaseUntil: time.Now().Add(-time.Hour)})
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					st, _ := f.store.Read(ctx)
					copy := c
					copy.Data = map[string]any{"candidate_operation_id": c.Data["candidate_operation_id"], "environment_id": "dev", "request_key": "uncertain-" + test.name, "expected_selection": selectionToken(*environmentByID(&st, "dev"))}
					if _, err := f.s.Execute(ctx, copy); err == nil {
						t.Fatal("uncertain external effect accepted")
					}
					if err := f.store.Update(ctx, func(st *domain.State) error {
						op := &st.Operations[len(st.Operations)-1]
						op.Phase = "CONFIRMED_CLOSED"
						op.GitOpsPR = nil
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}
func TestDevReservationOverflowRestoreAndPortableExport(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, _, c := devReservationFixture(t, kind)
			for _, field := range []string{"epoch", "generation"} {
				original, _ := f.store.Read(ctx)
				if err := f.store.Update(ctx, func(st *domain.State) error {
					e := environmentByID(st, "dev")
					if field == "epoch" {
						e.ReservationEpoch = math.MaxUint64
					} else {
						e.SelectionGeneration = math.MaxUint64
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				st, _ := f.store.Read(ctx)
				copy := c
				copy.Data = map[string]any{}
				for k, v := range c.Data {
					copy.Data[k] = v
				}
				copy.Data["expected_selection"] = selectionToken(*environmentByID(&st, "dev"))
				before, _ := json.Marshal(st)
				if _, err := f.s.Execute(ctx, copy); err == nil {
					t.Fatal("overflow admitted", field)
				}
				after, _ := f.store.Read(ctx)
				b, _ := json.Marshal(after)
				if string(before) != string(b) {
					t.Fatal("overflow mutated state")
				}
				if err := f.store.Update(ctx, func(st *domain.State) error { *st = original; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			value, err := f.s.Execute(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			hold := value.(domain.ExternalOperation)
			st, _ := f.store.Read(ctx)
			archive, err := f.s.CreateBackup(ctx, "operator")
			if err != nil {
				t.Fatal(err)
			}
			destStore, reopen := openWorkerTestStore(t, kind)
			defer destStore.Close()
			dest := NewWithProvider(destStore, f.s.provider)
			dest.ConfigureWorkers(f.s.externalWorkers)
			if _, err = dest.RestoreBackup(ctx, "restore-owner", archive.ArchiveBase64, archive.SHA256); err != nil {
				t.Fatal(err)
			}
			restored, _ := destStore.Read(ctx)
			if activeDevReservation(&restored, "dev") == nil || !operationByID(&restored, hold.ID).ReservationQuarantined {
				t.Fatal("restore lost quarantine/exclusion")
			}
			value, err = dest.Execute(ctx, c)
			if err != nil || value.(domain.ExternalOperation).ID != hold.ID {
				t.Fatal("restored historical receipt replay", err)
			}
			if _, err = dest.Execute(ctx, releaseReservationCommand(restored, hold)); err == nil {
				t.Fatal("restored hold blindly released")
			}
			destStore.Close()
			destStore = reopen()
			dest.store = destStore
			restored, _ = destStore.Read(ctx)
			if reservationFree(&restored, "dev") == nil {
				t.Fatal("restored reopened hold lost")
			}
			portable, err := f.s.ExportWorkspace(ctx, "p")
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range portable.Environments {
				if e.ReservationEpoch != 0 || e.SelectionGeneration != 0 || e.DesiredCompositionID != "" {
					t.Fatal("portable authority leaked")
				}
			}
			assertReservationPreserved(t, hold.DevReservation.Environment, *environmentByID(&st, "dev"), 1, 1)
		})
	}
}

type reservationDelayedProvider struct {
	*mainCandidateProvider
	posts          int
	beforeProposal func()
}

func (p *reservationDelayedProvider) ProposeGitOps(_ context.Context, _ delivery.Connection, _ delivery.GitOpsApply) (delivery.GitOpsPullRequest, error) {
	p.posts++
	if p.beforeProposal != nil {
		p.beforeProposal()
	}
	return delivery.GitOpsPullRequest{Number: 77, URL: "https://example.test/pr/77", HeadSHA: shaHead, State: "open"}, nil
}
func (p *reservationDelayedProvider) ObserveGitOpsPR(_ context.Context, _ delivery.Connection, _ delivery.GitOpsApply, pr delivery.GitOpsPullRequest) (delivery.GitOpsPullRequest, error) {
	return pr, nil
}
func TestDevReservationDelayedGitOpsIntentOrdering(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		for _, order := range []string{"reserve-before-intent", "intent-before-reserve"} {
			t.Run(kind+"/"+order, func(t *testing.T) {
				ctx := context.Background()
				f, _, reserve := devReservationFixture(t, kind)
				original := f.s.provider.(*mainCandidateProvider)
				original.branches = map[string]string{"feature/work": shaHead}
				p := &reservationDelayedProvider{mainCandidateProvider: original}
				f.s.provider = p
				st, _ := f.store.Read(ctx)
				var artifactID string
				for _, a := range st.DeliveryArtifacts {
					if a.Native != nil {
						artifactID = a.ID
					}
				}
				exec(t, f.s, domain.Command{Action: "record_integration_revision", ID: "rev-main", IntegrationID: "i", Data: map[string]any{"repository_id": "source", "branch": "feature/work", "base_commit": shaHead, "head_commit": shaHead, "commits": []string{shaHead}}})
				deploy := automaticCommand()
				deploy.Data["revision_id"] = "rev-main"
				deploy.Action = "deploy_existing_artifact"
				deploy.Data["artifact_id"] = artifactID
				deploy.Data["request_key"] = "delayed-existing-artifact"
				v, err := f.s.Execute(ctx, deploy)
				if err != nil {
					t.Fatal(err)
				}
				old := v.(domain.ExternalOperation)
				for range 8 {
					st, _ = f.store.Read(ctx)
					if operationByID(&st, old.ID).Phase == "APPLY" {
						break
					}
					nativeTick(t, f)
				}
				st, _ = f.store.Read(ctx)
				if op := operationByID(&st, old.ID); op.Phase != "APPLY" {
					t.Fatalf("existing native deploy not ready for GitOps %s %s %s", op.Phase, op.Status, op.Detail)
				}
				var reserveErr error
				hook := func() { _, reserveErr = f.s.Execute(ctx, reserve) }
				if order == "reserve-before-intent" {
					p.onRead = func() { p.onRead = nil; hook() }
				} else {
					p.beforeProposal = hook
				}
				nativeTick(t, f)
				st, _ = f.store.Read(ctx)
				if order == "reserve-before-intent" {
					if reserveErr != nil || activeDevReservation(&st, "dev") == nil || p.posts != 0 {
						t.Fatalf("reserve before intent: err=%v posts=%d op=%+v", reserveErr, p.posts, *operationByID(&st, old.ID))
					}
				} else {
					if reserveErr == nil || activeDevReservation(&st, "dev") != nil || p.posts != 1 {
						t.Fatalf("intent before reserve: err=%v posts=%d op=%+v", reserveErr, p.posts, *operationByID(&st, old.ID))
					}
				}
			})
		}
	}
}

func TestDevReservationSourceIntentOrderingAndHistoricalReleaseScope(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, _, reserve := devReservationFixture(t, kind)
			value, err := f.s.Execute(ctx, domain.Command{Action: "reconcile_composition", Actor: "operator", ProductID: "p", ID: "dev-baseline", Data: map[string]any{}})
			if err != nil {
				t.Fatal(err)
			}
			parent := value.(domain.ExternalOperation)
			st, _ := f.store.Read(ctx)
			reserve.Data["expected_selection"] = selectionToken(*environmentByID(&st, "dev"))
			token := "source-intent-owner"
			if err = f.store.Update(ctx, func(st *domain.State) error {
				op := operationByID(st, parent.ID)
				op.LeaseOwner = token
				op.LeaseUntil = time.Now().Add(time.Minute)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err = f.s.beginReservationMutation(ctx, parent, token, "SOURCE"); err != nil {
				t.Fatal(err)
			}
			// Expiry and cancellation do not erase the committed source ref mutation intent.
			if err = f.store.Update(ctx, func(st *domain.State) error {
				op := operationByID(st, parent.ID)
				op.LeaseUntil = time.Now().Add(-time.Hour)
				op.Status = "CANCELLED"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err = f.s.Execute(ctx, reserve); err == nil {
				t.Fatal("uncertain source write admitted reservation")
			}
			// Confirm a known source outcome in this fixture, then verify the opposite order.
			if err = f.store.Update(ctx, func(st *domain.State) error {
				op := operationByID(st, parent.ID)
				op.ReservationMutationIntent = ""
				op.LeaseOwner = token
				op.LeaseUntil = time.Now().Add(time.Minute)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			value, err = f.s.Execute(ctx, reserve)
			if err != nil {
				t.Fatal(err)
			}
			hold := value.(domain.ExternalOperation)
			if err = f.s.beginReservationMutation(ctx, parent, token, "SOURCE"); err == nil {
				t.Fatal("source ref write intent accepted after reservation")
			}
			st, _ = f.store.Read(ctx)
			release := releaseReservationCommand(st, hold)
			value, err = f.s.Execute(ctx, release)
			if err != nil {
				t.Fatal(err)
			}
			receipt := value.(domain.ExternalOperation)
			// Invalid/foreign historical release records cannot remove a later active hold.
			st, _ = f.store.Read(ctx)
			next := reserve
			next.Data = map[string]any{}
			for k, v := range reserve.Data {
				next.Data[k] = v
			}
			next.Data["request_key"] = "second-reservation-key"
			next.Data["expected_selection"] = selectionToken(*environmentByID(&st, "dev"))
			value, err = f.s.Execute(ctx, next)
			if err != nil {
				t.Fatal(err)
			}
			newHold := value.(domain.ExternalOperation)
			if err = f.store.Update(ctx, func(st *domain.State) error {
				bad := cloneFlowOperation(receipt)
				bad.ID = "foreign-release"
				bad.ProductID = "foreign-product"
				bad.DevReservation.ReservationOperationID = newHold.ID
				st.Operations = append(st.Operations, bad)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			st, _ = f.store.Read(ctx)
			if activeDevReservation(&st, "dev") == nil || activeDevReservation(&st, "dev").ID != newHold.ID {
				t.Fatal("foreign release reference removed active hold")
			}
		})
	}
}

func TestDevReservationAdmissionAuthorityNegatives(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, _, reserve := devReservationFixture(t, kind)
			cases := []struct {
				name    string
				command func(domain.Command) domain.Command
				mutate  func(*domain.State)
			}{
				{name: "stale CAS", command: func(c domain.Command) domain.Command {
					token := c.Data["expected_selection"].(domain.EnvironmentSelection)
					token.Generation++
					c.Data["expected_selection"] = token
					return c
				}},
				{name: "foreign product", command: func(c domain.Command) domain.Command { c.ProductID = "foreign"; return c }},
				{name: "non-candidate", command: func(c domain.Command) domain.Command { c.Data["candidate_operation_id"] = "missing"; return c }},
				{name: "non Dev target", mutate: func(st *domain.State) { environmentBinding(st, "dev", "component").Purpose = "PROD" }},
				{name: "direct target", mutate: func(st *domain.State) { environmentBinding(st, "dev", "component").GitOpsMode = "DIRECT" }},
				{name: "revoked grant", mutate: func(st *domain.State) {
					for i := range st.ConnectionGrants {
						st.ConnectionGrants[i].ProductID = "foreign"
					}
				}},
				{name: "changed configuration", mutate: func(st *domain.State) { componentBuild(st, "component").Profile = "changed" }},
				{name: "missing baseline component", mutate: func(st *domain.State) { compositionByID(st, "dev-baseline").ApplicationSnapshots = nil }},
			}
			for _, test := range cases {
				t.Run(test.name, func(t *testing.T) {
					original, _ := f.store.Read(ctx)
					if test.mutate != nil {
						if err := f.store.Update(ctx, func(st *domain.State) error { test.mutate(st); return nil }); err != nil {
							t.Fatal(err)
						}
					}
					copy := reserve
					copy.Data = map[string]any{}
					for k, v := range reserve.Data {
						copy.Data[k] = v
					}
					if test.command != nil {
						copy = test.command(copy)
					}
					before, _ := f.store.Read(ctx)
					if _, err := f.s.Execute(ctx, copy); err == nil {
						t.Fatal("invalid reservation admitted")
					}
					after, _ := f.store.Read(ctx)
					if !reflect.DeepEqual(before, after) {
						t.Fatal("negative command mutated state")
					}
					if err := f.store.Update(ctx, func(st *domain.State) error { *st = original; return nil }); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}
