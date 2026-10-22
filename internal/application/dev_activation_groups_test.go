package application

import (
	"context"
	"encoding/json"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
	"strings"
	"testing"
)

func TestDevActivationGroupFrozenMembershipAtomicResultsAndGETOnlyRecovery(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, p, hold, command := devActivationMultiFixture(t, kind, func(f *nativeFixture) {
				if e := f.store.Update(ctx, func(st *domain.State) error {
					environmentBinding(st, "dev", "component").Path = "shared.yaml"
					environmentBinding(st, "dev", "component-b").Path = "shared.yaml"
					return nil
				}); e != nil {
					t.Fatal(e)
				}
			})
			p.sourceReads.Store(0)
			p.uncachedSourceReads.Store(0)
			v, e := f.s.Execute(ctx, command)
			if e != nil {
				t.Fatal(e)
			}
			parent := v.(domain.ExternalOperation)
			if len(parent.DevActivation.Groups) != 1 {
				t.Fatal("shared physical destination split")
			}
			g := parent.DevActivation.Groups[0]
			follower := g.MemberIDs[1]
			forceActivationTick(t, f, follower)
			forceActivationTick(t, f, follower)
			if p.proposals != 0 {
				t.Fatal("follower created group proposal")
			}
			p.lost, p.absent = true, true
			activationTicks(t, f, 20)
			st, _ := f.store.Read(ctx)
			leader := operationByID(&st, g.LeaderID)
			if p.proposals != 1 || leader.DevActivationBatch == nil || leader.Phase != "APPLY_UNCERTAIN" || operationByID(&st, follower).DevActivationBatch != nil {
				t.Fatal("group intent incomplete or duplicated")
			}
			for _, id := range parent.ChildIDs {
				child := operationByID(&st, id)
				if child.GitOpsBase == nil || child.GitOpsBase.Content != "" {
					t.Fatal("raw provider YAML persisted")
				}
			}
			if p.uncachedSourceReads.Load() != 0 {
				t.Fatal("group source preflight could refresh authentication")
			}
			p.sourceReads.Store(0)
			forceActivationTick(t, f, parent.ID)
			forceActivationTick(t, f, g.LeaderID)
			if p.sourceReads.Load() != 0 || p.proposals != 1 {
				t.Fatal("uncertain recovery performed source preflight or duplicated proposal")
			}
			for name, mutate := range map[string]func(*domain.State){
				"leader": func(st *domain.State) { operationByID(st, parent.ID).DevActivation.Groups[0].LeaderID = follower },
				"duplicate": func(st *domain.State) {
					p := operationByID(st, parent.ID).DevActivation
					p.Groups[0].MemberIDs[1] = p.Groups[0].MemberIDs[0]
				},
				"selector": func(st *domain.State) {
					p := operationByID(st, parent.ID).DevActivation
					p.Components[0].Selector.ImageRepository = "registry.invalid/foreign"
					p.SnapshotHash = activationProofHash(*p)
				},
				"batch-member": func(st *domain.State) {
					v := operationByID(st, g.LeaderID)
					v.DevActivationBatch.Changes = v.DevActivationBatch.Changes[:1]
					v.DevActivationIntentHash = workerconfig.Fingerprint(v.DevActivationBatch)
				},
				"follower-authority": func(st *domain.State) {
					operationByID(st, follower).DevActivationBatch = operationByID(st, g.LeaderID).DevActivationBatch
				},
				"oversized": func(st *domain.State) {
					v := operationByID(st, g.LeaderID)
					v.DevActivationBatch.Message = strings.Repeat("x", 65537)
					v.DevActivationIntentHash = workerconfig.Fingerprint(v.DevActivationBatch)
				},
			} {
				t.Run(name, func(t *testing.T) {
					copy := cloneStateForActivation(t, st)
					mutate(&copy)
					if f.s.devActivationAllowed(&copy, *operationByID(&copy, parent.ID)) == nil {
						t.Fatal("changed grouped authority accepted")
					}
				})
			}
			p.absent = false
			forceActivationTick(t, f, g.LeaderID)
			p.merged = true
			forceActivationTick(t, f, g.LeaderID)
			st, _ = f.store.Read(ctx)
			first, second := operationByID(&st, g.MemberIDs[0]), operationByID(&st, g.MemberIDs[1])
			if first.Status != "SUCCEEDED" || second.Status != "SUCCEEDED" || first.GitOpsResult.CommitSHA != second.GitOpsResult.CommitSHA || first.GitOpsPR.Number != second.GitOpsPR.Number || p.proposals != 1 {
				t.Fatal("group result was partial or duplicated")
			}
			copy := cloneStateForActivation(t, st)
			child := operationByID(&copy, follower)
			child.GitOpsResult = nil
			child.DevActivationResultHash = ""
			if f.s.devActivationAllowed(&copy, *operationByID(&copy, parent.ID)) == nil {
				t.Fatal("partial group completion accepted")
			}
			portable, e := f.s.ExportWorkspace(ctx, "p")
			if e != nil {
				t.Fatal(e)
			}
			raw, _ := json.Marshal(portable)
			if strings.Contains(string(raw), "dev_activation_batch") || strings.Contains(string(raw), g.ID) {
				t.Fatal("portable export retained grouped authority")
			}
			if activeDevReservation(&st, hold.EnvironmentID) == nil {
				t.Fatal("group desired state released hold")
			}
		})
	}
}

func TestDevActivationGroupRejectsPhysicalConnectionAliasesBeforeIO(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			f, p, _, command := devActivationMultiFixture(t, kind, func(f *nativeFixture) {
				if e := f.store.Update(context.Background(), func(st *domain.State) error {
					conn := *connection(st, "conn")
					conn.ID = "alias"
					conn.Config.ID = conn.ID
					st.GitHubConnections = append(st.GitHubConnections, conn)
					grant := st.ConnectionGrants[0]
					grant.ID = "alias-grant"
					grant.ConnectionID = conn.ID
					st.ConnectionGrants = append(st.ConnectionGrants, grant)
					binding := *repositoryBinding(st, "gitops")
					binding.ID = "alias-binding"
					binding.RepositoryID = "alias-repo"
					binding.ConnectionID = conn.ID
					var repo domain.Repository
					for _, v := range st.Repositories {
						if v.ID == "gitops" {
							repo = v
						}
					}
					repo.ID = binding.RepositoryID
					st.Repositories = append(st.Repositories, repo)
					st.RepositoryBindings = append(st.RepositoryBindings, binding)
					target := environmentBinding(st, "dev", "component-b")
					target.ConnectionID = conn.ID
					target.RepositoryID = repo.ID
					return nil
				}); e != nil {
					t.Fatal(e)
				}
			})
			p.sourceReads.Store(0)
			if _, e := f.s.Execute(context.Background(), command); e == nil {
				t.Fatal("physical alias admitted")
			}
			if p.sourceReads.Load() != 0 || p.proposals != 0 {
				t.Fatal("alias failed after provider preflight")
			}
		})
	}
}

func TestDevActivationOrphanAuthorityCannotFallback(t *testing.T) {
	if activationGroupBounds(nil, nil) == nil {
		t.Fatal("malformed absent group proof accepted")
	}
	st := domain.State{}
	for _, op := range []domain.ExternalOperation{{DevActivationGroupID: "group"}, {DevActivationBatch: &delivery.GitOpsBatchApply{}}, {DevActivationIntentHash: "intent"}, {DevActivationPRHash: "proof"}, {DevActivationResultHash: "result"}} {
		if reservationOperationAllowed(&st, op) == nil {
			t.Fatal("orphan authority admitted to legacy path")
		}
	}
}

func TestDevActivationDuplicateOriginalChildRejectedBeforeSourceIO(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			for _, mode := range []string{"child", "application", "foreign"} {
				t.Run(mode, func(t *testing.T) {
					ctx := context.Background()
					f, p, hold, command := devActivationFixture(t, kind)
					if e := f.store.Update(ctx, func(st *domain.State) error {
						candidate := operationByID(st, hold.DevReservation.CandidateOperationID)
						switch mode {
						case "child":
							candidate.ChildIDs = append(candidate.ChildIDs, candidate.ChildIDs[0])
						case "application":
							child := *operationByID(st, candidate.ChildIDs[0])
							child.ID = "duplicate-application-child"
							st.Operations = append(st.Operations, child)
							candidate = operationByID(st, hold.DevReservation.CandidateOperationID)
							candidate.ChildIDs = append(candidate.ChildIDs, child.ID)
						case "foreign":
							operationByID(st, candidate.ChildIDs[0]).ParentID = "foreign-candidate"
						}
						return nil
					}); e != nil {
						t.Fatal(e)
					}
					p.sourceReads.Store(0)
					_, err := f.s.Execute(ctx, command)
					if err == nil || p.sourceReads.Load() != 0 || p.proposals != 0 {
						t.Fatalf("invalid original membership reached preflight/admission: err=%v sourceReads=%d proposals=%d", err, p.sourceReads.Load(), p.proposals)
					}
				})
			}
		})
	}
}
