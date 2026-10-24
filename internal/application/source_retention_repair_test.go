package application

import (
	"context"
	"reflect"
	"testing"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
)

type retentionConflictAdmissionProvider struct {
	*retentionFlowProvider
	afterCompose func()
}

func (p *retentionConflictAdmissionProvider) ComposeSource(ctx context.Context, c delivery.Connection, plan delivery.SourcePlan) (delivery.SourceResult, error) {
	result, err := p.fakeFlow.ComposeSource(ctx, c, plan)
	if p.afterCompose != nil {
		p.afterCompose()
	}
	return result, err
}
func TestSourceRetentionConflictAdmissionAtomicAuthority(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		for _, drift := range []string{"legitimate", "expired same owner", "superseded", "grant revoked", "config changed"} {
			t.Run(kind+"/"+drift, func(t *testing.T) {
				ctx := context.Background()
				_, memory, p, parent := retentionFlowFixture(t)
				store, _ := openWorkerTestStore(t, kind)
				defer store.Close()
				if err := store.Update(ctx, func(st *domain.State) error { *st = memory.state; return nil }); err != nil {
					t.Fatal(err)
				}
				wrapped := &retentionConflictAdmissionProvider{retentionFlowProvider: p}
				s := NewWithProvider(store, wrapped)
				for range 2 {
					durableRetentionTick(t, s, store)
				}
				before, _ := store.Read(ctx)
				prior := cloneFlowOperation(*operationByID(&before, parent))
				p.conflict = true
				wrapped.afterCompose = func() {
					if err := store.Update(ctx, func(st *domain.State) error {
						switch drift {
						case "expired same owner":
							operationByID(st, parent).LeaseUntil = time.Now().Add(-time.Second)
						case "superseded":
							environmentByID(st, "dev").DesiredOperationID = "replacement"
						case "grant revoked":
							for i := range st.ConnectionGrants {
								if st.ConnectionGrants[i].ConnectionID == "conn" {
									st.ConnectionGrants[i].ConnectionID = "revoked"
								}
							}
						case "config changed":
							componentBuild(st, "component").Workflow = "changed.yml"
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				checked, token := retentionClaim(t, s, parent)
				err := s.tickFlow(ctx, checked, token)
				after, _ := store.Read(ctx)
				current := operationByID(&after, parent)
				if drift != "legitimate" {
					if err == nil || len(after.CompositionConflicts) != len(before.CompositionConflicts) || len(after.Events) != len(before.Events) || len(after.OperationSteps) != len(before.OperationSteps) || !reflect.DeepEqual(current.Sources, prior.Sources) || len(current.ChildIDs) != 0 {
						t.Fatalf("stale conflict/source outcome admitted: %s error=%v", drift, err)
					}
					return
				}
				if err != nil || len(after.CompositionConflicts) != 1 || len(after.Events) != len(before.Events)+1 || len(after.OperationSteps) != len(before.OperationSteps)+1 || current.Status != "FAILED" || current.DeploymentState != "BLOCKED" || current.FinishedAt == nil || current.LeaseOwner != "" || current.Sources[0].Result == nil || !current.Sources[0].Result.Conflict {
					t.Fatalf("legitimate conflict/outcome not atomic: %v %+v", err, current)
				}
				conflict := after.CompositionConflicts[0]
				if conflict.OperationID != parent || conflict.CompositionID != prior.CompositionSnapshot.ID || len(conflict.Integrations) != 2 || len(conflict.Features) != 2 || conflict.Source.Result == nil || !conflict.Source.Result.Conflict {
					t.Fatal("conflict lost exact semantic context")
				}
				if err := s.tickFlow(ctx, checked, token); err == nil {
					t.Fatal("released conflict lease admitted another outcome")
				}
				replayed, _ := store.Read(ctx)
				if len(replayed.Events) != len(after.Events) || len(replayed.OperationSteps) != len(after.OperationSteps) || len(replayed.CompositionConflicts) != 1 {
					t.Fatal("conflict duplicate mutated audit")
				}
			})
		}
	}
}

func TestSourceRetentionRestoreUsesCausalLineage(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			donor, m, p, parent := retentionFlowFixture(t)
			flowTickUntil(t, donor, m, parent, retentionSourceReady)
			operation := operationByID(&m.state, parent)
			compositionByID(&m.state, "baseline").AutoRefresh = true
			ahead := time.Now().UTC().Add(750 * time.Millisecond)
			operation.CreatedAt = ahead
			operation.UpdatedAt = ahead
			operation.Status = "SUCCEEDED"
			for i := range operation.Sources[0].Retained {
				operation.Sources[0].Retained[i].ObservedAt = ahead
			}
			for i := range m.state.Events {
				if m.state.Events[i].EntityID == parent {
					m.state.Events[i].At = ahead
				}
			}
			if err := validSourceRetention(*operation); err != nil {
				t.Fatal(err)
			}
			backup, err := donor.CreateBackup(ctx, "tester")
			if err != nil {
				t.Fatal(err)
			}
			store, _ := openWorkerTestStore(t, kind)
			defer store.Close()
			s := NewWithProvider(store, p)
			if _, err = s.RestoreBackup(ctx, "tester", backup.ArchiveBase64, backup.SHA256); err != nil {
				t.Fatal(err)
			}
			restored, _ := store.Read(ctx)
			restoredParent := operationByID(&restored, parent)
			restoreIndex := len(restored.Events) - 1
			restoreEvent := restored.Events[restoreIndex]
			if restoreEvent.Action != "restore_backup" || !restoreEvent.At.Before(restoredParent.CreatedAt) || restoredParent.Status != "SUCCEEDED" {
				t.Fatal("fixture lacks cross-host clock skew across an actual restore")
			}
			before := featurePatchFingerprint(restored)
			pins := p.ensureCalls
			if err = s.ScheduleCompositionRefresh(ctx, time.Now(), time.Minute); err != nil {
				t.Fatal(err)
			}
			state, _ := store.Read(ctx)
			if string(before) != string(featurePatchFingerprint(state)) || p.ensureCalls != pins {
				t.Fatal("pre-restore intent restarted automatic source writes")
			}
			// Fresh explicit GHA intent remains available after restore. Its creation
			// event, rather than its clock, causally establishes a new authority root.
			fresh := exec(t, s, domain.Command{Action: "reconcile_composition", ID: "baseline"}).(domain.ExternalOperation)
			state, _ = store.Read(ctx)
			if err := sourceRetentionProvenance(&state, fresh); err != nil {
				t.Fatal("fresh post-restore intent rejected", err)
			}
			// Even a selected descendant with a post-restore creation event may not
			// revive a pre-restore origin. Rebind canonical hashes to isolate provenance.
			if err = store.Update(ctx, func(st *domain.State) error {
				op := operationByID(st, fresh.ID)
				op.SourceRetentionOriginID = parent
				op.SourceRetentionParentID = parent
				op.Status = "SUCCEEDED"
				for i := range op.Sources {
					intent, e := expectedSourceRetention(*op, op.Sources[i])
					if e != nil {
						return e
					}
					op.Sources[i].Retention = intent
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			state, _ = store.Read(ctx)
			before = featurePatchFingerprint(state)
			if err = s.ScheduleCompositionRefresh(ctx, time.Now(), time.Minute); err != nil {
				t.Fatal(err)
			}
			state, _ = store.Read(ctx)
			if string(before) != string(featurePatchFingerprint(state)) {
				t.Fatal("post-restore descendant revived old origin")
			}
			// A second truly explicit root can pin and later authorize a bounded refresh.
			explicit := exec(t, s, domain.Command{Action: "reconcile_composition", ID: "baseline"}).(domain.ExternalOperation)
			durableRetentionTick(t, s, store)
			state, _ = store.Read(ctx)
			if len(operationByID(&state, explicit.ID).Sources[0].Retained) != 1 {
				t.Fatal("fresh explicit post-restore root could not admit exact ref")
			}
			if err = store.Update(ctx, func(st *domain.State) error { operationByID(st, explicit.ID).Status = "SUCCEEDED"; return nil }); err != nil {
				t.Fatal(err)
			}
			if err = s.ScheduleCompositionRefresh(ctx, time.Now(), time.Minute); err != nil {
				t.Fatal(err)
			}
			state, _ = store.Read(ctx)
			latest := state.Operations[len(state.Operations)-1]
			if latest.ID == explicit.ID || latest.SourceRetentionOriginID != explicit.ID || latest.SourceRetentionParentID != explicit.ID || sourceRetentionProvenance(&state, latest) != nil {
				t.Fatal("fresh explicit lineage could not authorize later refresh")
			}
		})
	}
}

func TestSourceRetentionLineageRequiresUnambiguousScopedCreation(t *testing.T) {
	for _, mode := range []string{"missing creation", "duplicate creation", "foreign creation", "cancelled root", "missing origin", "foreign origin", "cycle", "duplicate operation"} {
		t.Run(mode, func(t *testing.T) {
			_, m, _, parent := retentionFlowFixture(t)
			op := operationByID(&m.state, parent)
			switch mode {
			case "missing creation":
				for i := range m.state.Events {
					if m.state.Events[i].EntityID == parent {
						m.state.Events[i].Action = "record_progress"
					}
				}
			case "duplicate creation":
				for _, event := range m.state.Events {
					if event.EntityID == parent {
						event.ID = id()
						m.state.Events = append(m.state.Events, event)
						break
					}
				}
			case "foreign creation":
				for i := range m.state.Events {
					if m.state.Events[i].EntityID == parent {
						m.state.Events[i].ProductID = "foreign"
					}
				}
			case "cancelled root":
				op.Status = "CANCELLED"
			case "missing origin":
				op.SourceRetentionOriginID = "missing"
			case "foreign origin":
				foreign := cloneFlowOperation(*op)
				foreign.ID = "foreign-root"
				foreign.ProductID = "foreign"
				m.state.Operations = append(m.state.Operations, foreign)
				op = operationByID(&m.state, parent)
				op.SourceRetentionOriginID = foreign.ID
				op.SourceRetentionParentID = foreign.ID
			case "cycle":
				op.SourceRetentionParentID = parent
			case "duplicate operation":
				m.state.Operations = append(m.state.Operations, cloneFlowOperation(*op))
				op = operationByID(&m.state, parent)
			}
			if mode == "missing origin" || mode == "foreign origin" || mode == "cycle" {
				for i := range op.Sources {
					intent, err := expectedSourceRetention(*op, op.Sources[i])
					if err != nil {
						t.Fatal(err)
					}
					op.Sources[i].Retention = intent
				}
			}
			if sourceRetentionProvenance(&m.state, *op) == nil || sourceRetentionRefreshAllowed(&m.state, *op) {
				t.Fatal("ambiguous or foreign source authority accepted")
			}
		})
	}
}
