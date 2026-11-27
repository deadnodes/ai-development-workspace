package application

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
)

type reviewWork struct {
	t                           *testing.T
	service                     *Service
	store                       *memoryStore
	owner, agent, bob, bobAgent context.Context
}

func ownerWorkReviewFixture(t *testing.T) reviewWork {
	store := &memoryStore{domain.EmptyState()}
	work := ownerWorkReviewOnStore(t, store)
	work.store = store
	return work
}

func ownerWorkReviewOnStore(t *testing.T, store Store) reviewWork {
	work := reviewWork{t, New(store), nil, userCtx("alice", "human", "", "member"), userCtx("agent/alice", "agent", "alice", "member"), userCtx("bob", "human", "", "member"), userCtx("agent/bob", "agent", "bob", "member")}
	work.run(work.owner, "create_product", "", "", "", "p", map[string]any{"name": "Product"})
	work.run(work.owner, "create_feature", "p", "", "", "f", map[string]any{"title": "Feature", "goal": "Goal"})
	work.run(work.owner, "create_integration", "p", "f", "", "i", map[string]any{"title": "Slice", "objective": "Ship"})
	return work
}

func (work reviewWork) apply(ctx context.Context, action, productID, featureID, integrationID, id string, data map[string]any) (any, error) {
	return work.service.Execute(ctx, domain.Command{Action: action, ProductID: productID, FeatureID: featureID, IntegrationID: integrationID, ID: id, Data: data})
}

func (work reviewWork) run(ctx context.Context, action, productID, featureID, integrationID, id string, data map[string]any) any {
	work.t.Helper()
	result, err := work.apply(ctx, action, productID, featureID, integrationID, id, data)
	if err != nil {
		work.t.Fatalf("%s: %v", action, err)
	}
	return result
}

func (work reviewWork) query(ctx context.Context, name, input string) map[string]any {
	work.t.Helper()
	value, err := work.service.Query(ctx, name, input)
	if err != nil {
		work.t.Fatal(err)
	}
	return value.(map[string]any)
}

func expectWorkError(t *testing.T, err error, text string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), text) {
		t.Fatalf("want %q, got %v", text, err)
	}
}

func (work reviewWork) idea(id string) {
	work.run(work.owner, "create_feature", "p", "", "", id, map[string]any{"title": "Idea", "goal": "Goal", "status": "idea"})
}

func reviewAnswer(questionID, option string) map[string]any {
	return map[string]any{"answers": []map[string]any{{"question_id": questionID, "text": "Reviewed", "option_id": option, "validation": map[string]any{"verdict": "consistent"}}}}
}

func (work reviewWork) claimData(target string) map[string]any {
	data := map[string]any{"orchestrator_id": "orchestrator", "agent_id": "executor"}
	if target != "" {
		data["harness"] = map[string]any{"kind": "codex", "tmux_target": target}
	}
	return data
}

func reviewQuestion(blocking bool) map[string]any {
	return map[string]any{"title": "Question", "body": "What should we do?", "kind": "question", "blocking": blocking}
}

func TestReviewPromotionAuthorityIgnoresFeatureOwner(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	work.idea("idea")
	work.run(work.bobAgent, "update_feature", "p", "idea", "", "", map[string]any{"owner": "bob"})
	question := work.run(work.bobAgent, "request_idea_promotion", "p", "idea", "", "", map[string]any{"rationale": "Proceed"}).(domain.Memory)
	if question.Question.AddressedTo != "alice" {
		t.Fatal(question.Question.AddressedTo)
	}
	// alice's own agent may relay alice's chat answer (TestDecisionAnswerRelayedByOwnersOtherAgent); others may not.
	for _, caller := range []context.Context{work.bobAgent, work.bob} {
		_, err := work.apply(caller, "answer_questions", "p", "", "", "", reviewAnswer(question.ID, "approve"))
		expectWorkError(t, err, "forbidden")
	}
	_, err := work.apply(work.bob, "update_feature", "p", "idea", "", "", map[string]any{"status": "ready_for_execution"})
	expectWorkError(t, err, "OWNER_APPROVAL_REQUIRED")
	edge := work.run(work.bobAgent, "propose_dependency", "p", "", "", "", map[string]any{"from_feature_id": "idea", "to_feature_id": "f", "type": "relates", "rationale": "Related"}).(domain.FeatureDependency)
	decision := work.run(work.bobAgent, "request_dependency_decision", "p", "", "", "", map[string]any{"dependency_id": edge.ID}).(domain.Memory)
	if decision.Question.AddressedTo != "alice" {
		t.Fatal(decision.Question.AddressedTo)
	}
}

func TestReviewArchivedIdeaCannotRestoreAsExecutable(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	work.idea("idea")
	work.run(work.agent, "update_feature", "p", "idea", "", "", map[string]any{"status": "archived"})
	for _, status := range []string{"active", "ready_for_execution"} {
		_, err := work.apply(work.agent, "update_feature", "p", "idea", "", "", map[string]any{"status": status})
		expectWorkError(t, err, "restored to idea")
	}
	work.run(work.agent, "update_feature", "p", "idea", "", "", map[string]any{"status": "idea"})
	_, err := work.apply(work.agent, "update_feature", "p", "idea", "", "", map[string]any{"status": "ready_for_execution"})
	expectWorkError(t, err, "OWNER_APPROVAL_REQUIRED")
	work.run(work.agent, "update_feature", "p", "f", "", "", map[string]any{"status": "archived"})
	work.run(work.agent, "update_feature", "p", "f", "", "", map[string]any{"status": "active"})
}

func TestReviewNonAssigneeCannotStoreBlocker(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	work.run(work.owner, "assign_integration", "p", "", "i", "", map[string]any{"assignee": "alice"})
	_, err := work.apply(work.bob, "ask_question", "p", "f", "i", "", reviewQuestion(true))
	expectWorkError(t, err, "NOT_ASSIGNEE")
	work.run(work.bob, "ask_question", "p", "f", "i", "", reviewQuestion(false))
	work.run(work.agent, "claim_execution", "p", "", "i", "", work.claimData(""))
}

func TestReviewHeartbeatAndReconcilerRespectStartGuard(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	execution := work.run(work.agent, "claim_execution", "p", "", "i", "", work.claimData("")).(domain.IntegrationExecution)
	work.run(work.agent, "ask_question", "p", "f", "i", "", reviewQuestion(true))
	for _, state := range []string{"lost", "stalled"} {
		executionFor(&work.store.state, "i").State = state
		_, err := work.apply(work.agent, "heartbeat_execution", "p", "", "i", "", map[string]any{"fence": execution.Fence})
		expectWorkError(t, err, "OPEN_BLOCKING_QUESTION")
		reconcileExecutions(&work.store.state, time.Now(), 0)
		if current := executionFor(&work.store.state, "i"); current.State != state {
			t.Fatalf("reconciler resumed %s: %+v", state, current)
		}
	}
}

func TestReviewPrerequisiteBlocksEvenOutsideTokenScope(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	work.run(work.owner, "create_product", "", "", "", "other", map[string]any{"name": "Other"})
	work.run(work.owner, "create_feature", "other", "", "", "secret-target", map[string]any{"title": "Target", "goal": "Goal"})
	edge := work.run(work.owner, "propose_dependency", "p", "", "", "", map[string]any{"from_feature_id": "f", "to_feature_id": "secret-target", "type": "requires", "rationale": "Secret rationale", "accept": true}).(domain.FeatureDependency)
	narrow := auth.WithPrincipal(context.Background(), auth.Principal{ID: "agent/alice/narrow", Kind: "agent", Owner: "alice", Role: "member", Products: []string{"p"}, Permissions: []string{"read", "write"}})
	for _, caller := range []context.Context{work.agent, narrow} {
		queue := work.query(caller, "next_work", "p")
		if len(queue["items"].([]workItem)) != 0 || len(queue["skipped"].([]workItem)) != 1 {
			t.Fatal(queue)
		}
		_, err := work.apply(caller, "claim_execution", "p", "", "i", "", work.claimData(""))
		expectWorkError(t, err, "FEATURE_DEPENDENCY_NOT_COMPLETED")
		if caller == narrow {
			if strings.Contains(err.Error(), "secret-target") || queue["skipped"].([]workItem)[0].DependencyID != "" {
				t.Fatalf("hidden target leaked: %v / %v", err, queue)
			}
			graph := work.query(caller, "get_dependency_graph", "p")
			if text := fmt.Sprint(graph); strings.Contains(text, edge.ID) || strings.Contains(text, "secret-target") || !strings.Contains(text, "unblocked:false") {
				t.Fatal(graph)
			}
		}
	}
}

func TestReviewPromotionApprovalExpiresOnReturnToIdea(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	work.idea("idea")
	request := func() domain.Memory {
		return work.run(work.agent, "request_idea_promotion", "p", "idea", "", "", map[string]any{"rationale": "Proceed"}).(domain.Memory)
	}
	approved := request()
	work.run(work.owner, "answer_questions", "p", "", "", "", reviewAnswer(approved.ID, "approve"))
	work.run(work.owner, "update_feature", "p", "idea", "", "", map[string]any{"status": "idea"})
	_, err := work.apply(work.agent, "update_feature", "p", "idea", "", "", map[string]any{"status": "ready_for_execution", "approval_question_id": approved.ID})
	expectWorkError(t, err, "OWNER_APPROVAL_REQUIRED")
	stale := request()
	work.run(work.owner, "update_feature", "p", "idea", "", "", map[string]any{"goal": "New scope"})
	_, err = work.apply(work.owner, "answer_questions", "p", "", "", "", reviewAnswer(stale.ID, "approve"))
	expectWorkError(t, err, "STALE_DECISION")
	work.run(work.owner, "answer_questions", "p", "", "", "", reviewAnswer(request().ID, "approve"))
}

func TestReviewDependencyApprovalBindsExactActionAndAttempt(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	work.idea("idea")
	edge := work.run(work.agent, "propose_dependency", "p", "", "", "", map[string]any{"from_feature_id": "idea", "to_feature_id": "f", "type": "requires", "rationale": "First"}).(domain.FeatureDependency)
	request := func() domain.Memory {
		question := work.run(work.agent, "request_dependency_decision", "p", "", "", "", map[string]any{"dependency_id": edge.ID, "decision": "accept"}).(domain.Memory)
		work.run(work.owner, "answer_questions", "p", "", "", "", reviewAnswer(question.ID, "approve"))
		return question
	}
	first, second := request(), request()
	for _, data := range []map[string]any{{"dependency_id": edge.ID, "decision": "reject", "approval_question_id": second.ID}, {"dependency_id": edge.ID, "decision": "accept", "approval_question_id": first.ID}} {
		_, err := work.apply(work.agent, "decide_dependency", "p", "", "", "", data)
		expectWorkError(t, err, "OWNER_APPROVAL_REQUIRED")
	}
	work.run(work.agent, "decide_dependency", "p", "", "", "", map[string]any{"dependency_id": edge.ID, "decision": "accept", "approval_question_id": second.ID})
}

func TestReviewHiddenMutationIDsMatchMissing(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	work.idea("idea")
	question := work.run(work.owner, "ask_question", "p", "idea", "", "", reviewQuestion(false)).(domain.Memory)
	edge := work.run(work.agent, "propose_dependency", "p", "", "", "", map[string]any{"from_feature_id": "idea", "to_feature_id": "f", "type": "relates", "rationale": "Related"}).(domain.FeatureDependency)
	productByID(work.store.state, "p").Visibility = "private"
	for _, test := range []struct{ action, id, field string }{{"withdraw_question", question.ID, "question_id"}, {"decide_dependency", edge.ID, "dependency_id"}, {"request_dependency_decision", edge.ID, "dependency_id"}, {"answer_questions", question.ID, ""}} {
		data := func(id string) map[string]any {
			if test.field == "" {
				return reviewAnswer(id, "")
			}
			return map[string]any{test.field: id, "reason": "Withdraw", "decision": "accept"}
		}
		_, hidden := work.apply(work.bob, test.action, "p", "", "", "", data(test.id))
		_, absent := work.apply(work.bob, test.action, "p", "", "", "", data("absent-id"))
		if hidden == nil || absent == nil || strings.ReplaceAll(hidden.Error(), test.id, "absent-id") != absent.Error() {
			t.Fatalf("%s: hidden=%v absent=%v", test.action, hidden, absent)
		}
	}
}

func (work reviewWork) register(ctx context.Context, target string) {
	work.run(ctx, "register_user_orchestrator", "p", "", "", "", map[string]any{"harness": map[string]any{"kind": "codex", "tmux_target": target}})
}

func TestReviewFallbackWakeSurfacesFencedAssigneeResume(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	work.register(work.owner, "alice-orchestrator")
	execution := work.run(work.agent, "claim_execution", "p", "", "i", "", work.claimData("")).(domain.IntegrationExecution)
	question := work.run(work.agent, "ask_question", "p", "f", "i", "", reviewQuestion(true)).(domain.Memory)
	work.run(work.owner, "answer_questions", "p", "", "", "", reviewAnswer(question.ID, ""))
	if !slices.ContainsFunc(work.store.state.ExecutionEvents, func(event domain.ExecutionEvent) bool {
		return event.Cause == domain.ExecutionCauseQuestionContinuation && event.IntegrationID == "i" && event.Owner == "alice" && event.Fence == execution.Fence && event.WakeTarget.TmuxTarget == "alice-orchestrator"
	}) {
		t.Fatal("no fenced fallback wake")
	}
	items := work.query(work.agent, "next_work", "p")["items"].([]workItem)
	if len(items) != 1 || items[0].Kind != "resume_integration" || items[0].ExecutionFence == nil || *items[0].ExecutionFence != execution.Fence || !strings.Contains(items[0].NextAction, "report_execution") {
		t.Fatal(items)
	}
	if items := work.query(work.bobAgent, "next_work", "p")["items"].([]workItem); len(items) != 0 {
		t.Fatal(items)
	}
	work.run(work.agent, "report_execution", "p", "", "i", "", map[string]any{"fence": execution.Fence, "state": "running"})
}

func TestReviewTransferInvalidatesExecutorAndGroupWakeTargets(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	work.register(work.bob, "bob-orchestrator")
	work.run(work.agent, "create_execution_group", "p", "", "", "", map[string]any{"name": "Old group", "orchestrator_id": "orchestrator", "orchestrator_harness": map[string]any{"kind": "codex", "tmux_target": "old-group"}, "integration_ids": []string{"i"}})
	execution := work.run(work.agent, "claim_execution", "p", "", "i", "", work.claimData("old-executor")).(domain.IntegrationExecution)
	question := work.run(work.agent, "ask_question", "p", "f", "i", "", reviewQuestion(true)).(domain.Memory)
	expired := time.Now().Add(-time.Minute)
	executionFor(&work.store.state, "i").LeaseUntil = &expired
	work.run(work.owner, "assign_integration", "p", "", "i", "", map[string]any{"assignee": "bob"})
	work.run(work.bob, "answer_questions", "p", "", "", "", reviewAnswer(question.ID, ""))
	current := executionFor(&work.store.state, "i")
	if current.Fence <= execution.Fence || current.Harness.Kind != "" || slices.Contains(work.store.state.ExecutionGroups[0].MemberIntegrationIDs, "i") {
		t.Fatalf("bindings survived transfer: %+v", current)
	}
	_, err := work.apply(work.bobAgent, "heartbeat_execution", "p", "", "i", "", map[string]any{"fence": execution.Fence})
	expectWorkError(t, err, "STALE_FENCE")
	for _, event := range work.store.state.ExecutionEvents {
		if event.Resolution.ResolvedAt == nil && strings.HasPrefix(event.WakeTarget.TmuxTarget, "old-") {
			t.Fatalf("stale wake left pending: %+v", event)
		}
	}
	if len(work.query(work.bobAgent, "next_work", "p")["items"].([]workItem)) != 1 {
		t.Fatal("replacement assignee did not receive the requeued integration")
	}
	if !slices.ContainsFunc(work.store.state.ExecutionEvents, func(event domain.ExecutionEvent) bool {
		return event.Owner == "bob" && event.Cause == domain.ExecutionCauseQueueAvailable && event.Resolution.ResolvedAt == nil && event.WakeTarget.TmuxTarget == "bob-orchestrator"
	}) {
		t.Fatal("replacement assignee did not receive a wake")
	}
}

func TestReviewPendingQuestionWakeRetiredOnTransferWithoutExecution(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	work.register(work.owner, "alice-orchestrator")
	work.run(work.owner, "assign_integration", "p", "", "i", "", map[string]any{"assignee": "alice"})
	question := work.run(work.owner, "ask_question", "p", "f", "i", "", reviewQuestion(false)).(domain.Memory)
	work.run(work.owner, "answer_questions", "p", "", "", "", reviewAnswer(question.ID, ""))
	wakeIndex := slices.IndexFunc(work.store.state.ExecutionEvents, func(event domain.ExecutionEvent) bool {
		return event.Cause == domain.ExecutionCauseQuestionContinuation && event.IntegrationID == "i" && event.Owner == "alice" && event.Fence == 0 && event.Resolution.ResolvedAt == nil
	})
	if wakeIndex < 0 || executionFor(&work.store.state, "i") != nil {
		t.Fatal("expected a pending assignee wake without an execution")
	}
	work.run(work.owner, "assign_integration", "p", "", "i", "", map[string]any{"assignee": "bob"})
	if work.store.state.ExecutionEvents[wakeIndex].Resolution.ResolvedAt == nil {
		t.Fatal("pending question wake survived assignment transfer")
	}
}

func TestReviewCallerReasonCannotSetTypedWakeCause(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	execution := work.run(work.agent, "claim_execution", "p", "", "i", "", work.claimData("executor")).(domain.IntegrationExecution)
	work.run(work.agent, "report_execution", "p", "", "i", "", map[string]any{"fence": execution.Fence, "state": "exited_incomplete", "reason": "blocker resolved"})
	for _, event := range work.store.state.ExecutionEvents {
		if event.IntegrationID == "i" && event.Type == "execution_continuation_required" {
			if event.Reason != "blocker resolved" || event.Cause != "" || event.WakeTarget.Kind != "" {
				t.Fatalf("display reason changed wake admission: %+v", event)
			}
			return
		}
	}
	t.Fatal("missing execution continuation event")
}

func TestReviewQueueDrainRetiresOnlyQueueWakesAcrossGenerations(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	work.register(work.owner, "alice-orchestrator")
	work.register(work.bob, "bob-orchestrator")
	question := work.run(work.owner, "ask_question", "p", "f", "", "", reviewQuestion(false)).(domain.Memory)
	work.run(work.owner, "answer_questions", "p", "", "", "", reviewAnswer(question.ID, ""))
	for index := 0; index < 3; index++ {
		work.run(work.agent, "claim_execution", "p", "", "i", "", work.claimData(""))
		for _, event := range work.store.state.ExecutionEvents {
			if event.Cause == domain.ExecutionCauseQueueAvailable && event.Resolution.ResolvedAt == nil {
				t.Fatalf("stale queue wake: %+v", event)
			}
		}
		if !slices.ContainsFunc(work.store.state.ExecutionEvents, func(event domain.ExecutionEvent) bool {
			return event.Cause == domain.ExecutionCauseQuestionContinuation && event.Resolution.ResolvedAt == nil
		}) {
			t.Fatal("queue drain retired a question continuation")
		}
		work.run(work.owner, "unassign_integration", "p", "", "i", "", nil)
	}
}

func TestRereviewFencedProgressRespectsStartGuard(t *testing.T) {
	for _, blocker := range []string{"question", "prerequisite"} {
		t.Run(blocker, func(t *testing.T) {
			work := ownerWorkReviewFixture(t)
			execution := work.run(work.agent, "claim_execution", "p", "", "i", "", work.claimData("")).(domain.IntegrationExecution)
			current := executionFor(&work.store.state, "i")
			expired := time.Now().Add(-time.Minute)
			current.State, current.LeaseUntil = "lost", &expired
			var expected string
			var clear func()
			if blocker == "question" {
				question := work.run(work.agent, "ask_question", "p", "f", "i", "", reviewQuestion(true)).(domain.Memory)
				expected = "OPEN_BLOCKING_QUESTION"
				clear = func() { work.run(work.owner, "answer_questions", "p", "", "", "", reviewAnswer(question.ID, "")) }
			} else {
				work.run(work.owner, "create_feature", "p", "", "", "prereq", map[string]any{"title": "Prerequisite", "goal": "Needed"})
				edge := work.run(work.owner, "propose_dependency", "p", "", "", "", map[string]any{"from_feature_id": "f", "to_feature_id": "prereq", "type": "requires", "rationale": "Needed", "accept": true}).(domain.FeatureDependency)
				expected = "FEATURE_DEPENDENCY_NOT_COMPLETED"
				clear = func() { dependencyByID(&work.store.state, edge.ID).Status = "rejected" }
			}
			count := len(work.store.state.Memories)
			progress := map[string]any{"body": "Progress", "execution_fence": execution.Fence}
			_, err := work.apply(work.agent, "record_progress", "p", "", "i", "", progress)
			expectWorkError(t, err, expected)
			if current = executionFor(&work.store.state, "i"); current.State != "lost" || !current.LeaseUntil.Equal(expired) || len(work.store.state.Memories) != count {
				t.Fatalf("denied progress changed execution or memory: %+v", current)
			}
			clear()
			work.run(work.agent, "record_progress", "p", "", "i", "", progress)
			if current = executionFor(&work.store.state, "i"); current.State != "running" || !current.LeaseUntil.After(expired) {
				t.Fatalf("eligible progress did not resume: %+v", current)
			}
		})
	}
}

func TestRereviewOwnershipAdoptionIsSeparateAndHumanOnly(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		service := New(store)
		ctx := context.Background()
		privateReviewCreate(t, service, ctx, "unowned", "shared")
		privateReviewCreate(t, service, ctx, "local-product", "shared")
		if err := store.Update(ctx, func(st *domain.State) error { productByID(*st, "local-product").Owner = "local"; return nil }); err != nil {
			t.Fatal(err)
		}
		alice := userCtx("alice", "human", "", "member")
		admin := userCtx("admin", "human", "", "admin")
		bootstrap := userCtx("service/bootstrap", "service", "", "admin")
		for productID, initialOwner := range map[string]string{"unowned": "", "local-product": "local"} {
			agent := auth.WithPrincipal(ctx, auth.Principal{ID: "agent/alice", Kind: "agent", Owner: "alice", Products: []string{productID}, Permissions: []string{"read", "write"}})
			visibility := domain.Command{Action: "set_product_visibility", ProductID: productID, Data: map[string]any{"visibility": "shared"}}
			adopt := domain.Command{Action: "adopt_product_ownership", ProductID: productID}
			for _, caller := range []context.Context{agent, ctx} {
				if _, err := service.Execute(caller, adopt); !errors.Is(err, auth.ErrForbidden) {
					t.Fatalf("unauthorized adoption: %v", err)
				}
			}
			if _, err := service.Execute(agent, visibility); !errors.Is(err, auth.ErrForbidden) {
				t.Fatalf("scoped agent changed visibility: %v", err)
			}
			if value, err := service.Execute(bootstrap, visibility); err != nil || value.(domain.Product).Owner != initialOwner {
				t.Fatalf("visibility changed ownership: %+v (%v)", value, err)
			}
			caller := alice
			if initialOwner == "local" {
				caller, adopt.Data = admin, map[string]any{"user_id": "alice"}
			}
			if value, err := service.Execute(caller, adopt); err != nil || value.(domain.Product).Owner != "alice" || value.(domain.Product).Visibility != "shared" {
				t.Fatalf("explicit adoption failed: %+v (%v)", value, err)
			}
		}
	})
}

func TestRereviewIntegrationStatusAliasesEnforceAssignee(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	work.run(work.owner, "assign_integration", "p", "", "i", "", map[string]any{"assignee": "alice"})
	for action, data := range map[string]map[string]any{"start_integration": nil, "complete_integration": nil, "transition_integration": {"status": "working"}} {
		_, err := work.apply(work.bob, action, "p", "", "i", "", data)
		expectWorkError(t, err, "NOT_ASSIGNEE")
	}
	started := work.run(work.owner, "start_integration", "p", "", "i", "", nil).(domain.Integration)
	if started.Owner != "alice" || started.Status != "working" {
		t.Fatal(started)
	}
	integration(&work.store.state, "i").Status = "ready"
	_, err := work.apply(work.bobAgent, "update_integration", "p", "", "i", "", map[string]any{"title": "Foreign edit"})
	expectWorkError(t, err, "NOT_ASSIGNEE")
	if current := integration(&work.store.state, "i"); current.Status != "ready" || current.Owner != "alice" || current.Title != "Slice" {
		t.Fatal(current)
	}
	work.run(work.agent, "update_integration", "p", "", "i", "", map[string]any{"title": "Authorized edit"})
}

func TestRereviewTransferReaddressesOnlyOpenIntegrationBlockers(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		work := ownerWorkReviewOnStore(t, store)
		former := userCtx("former", "human", "", "member")
		formerAgent := userCtx("agent/former", "agent", "former", "member")
		admin := userCtx("service/bootstrap", "service", "", "admin")
		work.run(work.owner, "set_product_visibility", "p", "", "", "", map[string]any{"visibility": "private"})
		for _, member := range []string{"former", "bob"} {
			work.run(work.owner, "add_product_member", "p", "", "", "", map[string]any{"user_id": member})
		}
		work.run(former, "assign_integration", "p", "", "i", "", map[string]any{"assignee": "former"})
		blocking := work.run(formerAgent, "ask_question", "p", "f", "i", "", reviewQuestion(true)).(domain.Memory)
		nonblocking := work.run(formerAgent, "ask_question", "p", "f", "i", "", reviewQuestion(false)).(domain.Memory)
		work.run(work.owner, "remove_product_member", "p", "", "", "", map[string]any{"user_id": "former"})
		work.run(admin, "assign_integration", "p", "", "i", "", map[string]any{"assignee": "bob"})
		_, err := work.apply(former, "answer_questions", "p", "", "", "", reviewAnswer(blocking.ID, ""))
		expectWorkError(t, err, "not found")
		_, err = work.apply(work.bobAgent, "claim_execution", "p", "", "i", "", work.claimData(""))
		expectWorkError(t, err, "OPEN_BLOCKING_QUESTION")
		work.run(work.bob, "answer_questions", "p", "", "", "", reviewAnswer(blocking.ID, ""))
		work.run(work.bobAgent, "claim_execution", "p", "", "i", "", work.claimData(""))
		st, err := store.Read(context.Background())
		if err != nil || questionByID(&st, nonblocking.ID).Question.AddressedTo != "former" || !slices.ContainsFunc(st.Events, func(event domain.Event) bool {
			return event.Action == "question_readdressed" && event.EntityID == blocking.ID && event.Data.Data["previous_addressed_to"] == "former" && event.Data.Data["addressed_to"] == "bob"
		}) {
			t.Fatalf("question/audit mismatch: %v", err)
		}
	})
}

func TestRereviewResumeUsesActualDesiredStateDespiteQueueFilters(t *testing.T) {
	for _, parentStatus := range []string{"active", "planned", "blocked"} {
		t.Run(parentStatus, func(t *testing.T) {
			work := ownerWorkReviewFixture(t)
			data := work.claimData("executor")
			data["desired_state"] = "released"
			execution := work.run(work.agent, "claim_execution", "p", "", "i", "", data).(domain.IntegrationExecution)
			question := work.run(work.agent, "ask_question", "p", "f", "i", "", reviewQuestion(true)).(domain.Memory)
			feature(&work.store.state, "f").Status = parentStatus
			integration(&work.store.state, "i").Status = "ready"
			work.run(work.owner, "answer_questions", "p", "", "", "", reviewAnswer(question.ID, ""))
			items := work.query(work.agent, "next_work", "p")["items"].([]workItem)
			if len(items) != 1 || items[0].Kind != "resume_integration" || items[0].ExecutionFence == nil || *items[0].ExecutionFence != execution.Fence || len(work.query(work.bobAgent, "next_work", "p")["items"].([]workItem)) != 0 {
				t.Fatalf("incorrect assignee resume: %+v", items)
			}
			event := reviewPendingWake(t, work.store.state, domain.ExecutionCauseBlockerResolved)
			executionFor(&work.store.state, "i").DesiredState = "ready"
			if items := work.query(work.agent, "next_work", "p")["items"].([]workItem); len(items) != 0 {
				t.Fatalf("already-satisfied work should not resume: %+v", items)
			}
			_, err := work.apply(work.agent, "claim_execution_event_delivery", "p", "", "", "", reviewDelivery(event.ID))
			expectWorkError(t, err, "INVALID_STATE")
		})
	}
}

func reviewPendingWake(t *testing.T, st domain.State, cause domain.ExecutionEventCause) domain.ExecutionEvent {
	t.Helper()
	var matching []domain.ExecutionEvent
	for _, event := range st.ExecutionEvents {
		if event.Cause == cause && event.Resolution.ResolvedAt == nil {
			matching = append(matching, event)
		}
	}
	if len(matching) != 1 {
		t.Fatalf("want one pending %s wake, got %+v", cause, matching)
	}
	return matching[0]
}

func reviewDelivery(eventID string) map[string]any {
	return map[string]any{"event_id": eventID, "supervisor_id": "review"}
}

func TestRereviewBlockerBindingRequiresOpenExactScopeAndFence(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	work.run(work.owner, "create_integration", "p", "f", "", "other", map[string]any{"title": "Other", "objective": "Ship"})
	execution := work.run(work.agent, "claim_execution", "p", "", "i", "", work.claimData("executor")).(domain.IntegrationExecution)
	foreign := work.run(work.owner, "add_blocker", "p", "", "other", "", map[string]any{"title": "Other blocker"}).(domain.Memory)
	local := work.run(work.owner, "add_blocker", "p", "", "i", "", map[string]any{"title": "Local blocker"}).(domain.Memory)
	resolve := func(blocker domain.Memory) {
		work.run(work.owner, "resolve_blocker", "p", "", "", blocker.ID, map[string]any{"body": "Resolved"})
	}
	bind := func(blocker domain.Memory) error {
		_, err := work.apply(work.agent, "report_execution", "p", "", "i", "", map[string]any{"state": "blocked", "blocker_ref": blocker.ID, "fence": execution.Fence})
		return err
	}
	expectWorkError(t, bind(foreign), "SCOPE_MISMATCH")
	resolve(foreign)
	expectWorkError(t, bind(foreign), "SCOPE_MISMATCH")
	if err := bind(local); err != nil {
		t.Fatal(err)
	}
	current := executionFor(&work.store.state, "i")
	if current.BlockerFence == nil || *current.BlockerFence != current.Fence {
		t.Fatal(current)
	}
	resolve(local)
	expectWorkError(t, bind(local), "SCOPE_MISMATCH")
	current = executionFor(&work.store.state, "i")
	current.Fence++
	reconcileExecutions(&work.store.state, time.Now(), 0)
	if slices.ContainsFunc(work.store.state.ExecutionEvents, func(event domain.ExecutionEvent) bool { return event.Cause == domain.ExecutionCauseBlockerResolved }) {
		t.Fatal("resolution woke a different fence")
	}
	current.Fence--
	reconcileExecutions(&work.store.state, time.Now(), 0)
	if event := reviewPendingWake(t, work.store.state, domain.ExecutionCauseBlockerResolved); event.Fence != execution.Fence {
		t.Fatal(event)
	}
}

func TestRereviewGroupWakeReroutesOnReplacementAndRemoval(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		work := ownerWorkReviewOnStore(t, store)
		groupHarness := func(target string) map[string]any { return map[string]any{"kind": "codex", "tmux_target": target} }
		group := work.run(work.agent, "create_execution_group", "p", "", "", "", map[string]any{"name": "Group", "orchestrator_id": "orchestrator", "orchestrator_harness": groupHarness("old-group"), "integration_ids": []string{"i"}}).(domain.ExecutionGroup)
		work.run(work.agent, "claim_execution", "p", "", "i", "", work.claimData("executor"))
		question := work.run(work.agent, "ask_question", "p", "f", "i", "", reviewQuestion(true)).(domain.Memory)
		work.run(work.owner, "answer_questions", "p", "", "", "", reviewAnswer(question.ID, ""))
		pending := func() domain.ExecutionEvent {
			st, err := store.Read(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			return reviewPendingWake(t, st, domain.ExecutionCauseBlockerResolved)
		}
		old := pending()
		work.run(work.agent, "set_execution_group_harness", "p", "", "", "", map[string]any{"group_id": group.ID, "harness": groupHarness("new-group")})
		replacement := pending()
		if old.WakeTarget.GroupID != group.ID || replacement.ID == old.ID || replacement.WakeTarget.TmuxTarget != "new-group" || replacement.WakeTarget.HarnessRevision <= old.WakeTarget.HarnessRevision {
			t.Fatalf("wake not rebound: %+v -> %+v", old.WakeTarget, replacement.WakeTarget)
		}
		work.run(work.agent, "remove_execution_group_member", "p", "", "", "", map[string]any{"group_id": group.ID, "integration_id": "i"})
		fallback := pending()
		if fallback.WakeTarget.Source != "executor" || fallback.WakeTarget.TmuxTarget != "executor" || fallback.Fence != old.Fence {
			t.Fatal(fallback)
		}
		for _, stale := range []domain.ExecutionEvent{old, replacement} {
			_, err := work.apply(work.agent, "claim_execution_event_delivery", "p", "", "", "", reviewDelivery(stale.ID))
			expectWorkError(t, err, "INVALID_STATE")
		}
		work.run(work.agent, "claim_execution_event_delivery", "p", "", "", "", reviewDelivery(fallback.ID))
	})
}

func TestRereviewGlobalQueueReplacesRevokedProductWake(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	work.run(work.owner, "set_product_visibility", "p", "", "", "", map[string]any{"visibility": "private"})
	work.run(work.owner, "add_product_member", "p", "", "", "", map[string]any{"user_id": "bob"})
	work.run(work.owner, "create_product", "", "", "", "other-product", map[string]any{"name": "Other product"})
	work.run(work.owner, "create_feature", "other-product", "", "", "other-feature", map[string]any{"title": "Other feature", "goal": "Other goal"})
	work.run(work.owner, "create_integration", "other-product", "other-feature", "", "other-integration", map[string]any{"title": "Other work", "objective": "Ship"})
	work.run(work.bob, "register_user_orchestrator", "", "", "", "", map[string]any{"harness": map[string]any{"kind": "codex", "tmux_target": "bob-queue"}})
	old := reviewPendingWake(t, work.store.state, domain.ExecutionCauseQueueAvailable)
	if old.ProductID != "p" {
		t.Fatal(old)
	}
	work.run(work.owner, "remove_product_member", "p", "", "", "", map[string]any{"user_id": "bob"})
	replacement := reviewPendingWake(t, work.store.state, domain.ExecutionCauseQueueAvailable)
	if replacement.ProductID != "other-product" || replacement.RegistrationID != old.RegistrationID || replacement.ID == old.ID || !work.store.state.UserOrchestrators[0].Available {
		t.Fatal(replacement)
	}
	work.run(work.bobAgent, "claim_execution_event_delivery", "other-product", "", "", "", reviewDelivery(replacement.ID))
}

func TestRereviewPendingContinuationsCoalesceAndExposeAnsweredQuestions(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	work.register(work.owner, "orchestrator")
	work.run(work.owner, "assign_integration", "p", "", "i", "", map[string]any{"assignee": "alice"})
	answer := func(integrationID string) domain.Memory {
		question := work.run(work.agent, "ask_question", "p", "f", integrationID, "", reviewQuestion(false)).(domain.Memory)
		work.run(work.owner, "answer_questions", "p", "", "", "", reviewAnswer(question.ID, ""))
		return question
	}
	first := answer("i")
	initial := reviewPendingWake(t, work.store.state, domain.ExecutionCauseQuestionContinuation)
	delivery := work.run(work.agent, "claim_execution_event_delivery", "p", "", "", "", reviewDelivery(initial.ID)).(string)
	second := answer("i")
	merged := reviewPendingWake(t, work.store.state, domain.ExecutionCauseQuestionContinuation)
	if merged.ID != initial.ID || !slices.Equal(merged.QuestionIDs, []string{first.ID, second.ID}) {
		t.Fatal(merged)
	}
	_, err := work.apply(work.agent, "ack_execution_event", "p", "", "", "", map[string]any{"event_id": initial.ID, "delivery_id": delivery, "supervisor_id": "review", "outcome": "delivered"})
	expectWorkError(t, err, "INVALID_STATE")
	for _, questionID := range merged.QuestionIDs {
		if question := questionByID(&work.store.state, questionID); question == nil || question.Status != "answered" || question.Question.Answer == nil {
			t.Fatal(question)
		}
	}
	separate := answer("")
	if !slices.ContainsFunc(work.store.state.ExecutionEvents, func(event domain.ExecutionEvent) bool {
		return event.ID != merged.ID && event.Resolution.ResolvedAt == nil && slices.Equal(event.QuestionIDs, []string{separate.ID})
	}) {
		t.Fatal("feature-level answer was merged into an integration continuation")
	}
}

func TestRereviewPromotionPreflightCannotDistinguishHiddenApproval(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		work := ownerWorkReviewOnStore(t, store)
		work.idea("visible")
		privateReviewCreate(t, work.service, work.owner, "hidden-product", "private")
		work.run(work.owner, "create_feature", "hidden-product", "", "", "hidden-idea", map[string]any{"title": "Hidden", "goal": "Hidden", "status": "idea"})
		hidden := work.run(work.agent, "request_idea_promotion", "hidden-product", "hidden-idea", "", "", map[string]any{"rationale": "Proceed"}).(domain.Memory)
		for _, approvalID := range []string{hidden.ID, "not-a-question"} {
			_, err := work.apply(work.bobAgent, "update_feature", "p", "visible", "", "", map[string]any{"status": "ready_for_execution", "approval_question_id": approvalID})
			if !errors.Is(err, ErrNotFound) || err.Error() != ErrNotFound.Error() {
				t.Fatalf("not a resource-level 404: %v", err)
			}
		}
	})
}

func TestRereviewEditedIdeaSupersedesInboxRequestWithoutBlockingBatch(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		work := ownerWorkReviewOnStore(t, store)
		work.idea("idea")
		request := func() domain.Memory {
			return work.run(work.agent, "request_idea_promotion", "p", "idea", "", "", map[string]any{"rationale": "Proceed"}).(domain.Memory)
		}
		old := request()
		work.run(work.agent, "update_feature", "p", "idea", "", "", map[string]any{"goal": "Changed scope"})
		current := request()
		ordinary := work.run(work.owner, "ask_question", "p", "f", "", "", reviewQuestion(false)).(domain.Memory)
		inbox := work.query(work.owner, "list_questions", "p")["items"].([]questionView)
		if len(inbox) != 2 || slices.ContainsFunc(inbox, func(question questionView) bool { return question.ID == old.ID }) {
			t.Fatalf("stale question in inbox: %+v", inbox)
		}
		batch := append(reviewAnswer(current.ID, "approve")["answers"].([]map[string]any), reviewAnswer(ordinary.ID, "")["answers"].([]map[string]any)...)
		work.run(work.owner, "answer_questions", "p", "", "", "", map[string]any{"answers": batch})
		st, err := store.Read(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if stale := questionByID(&st, old.ID); stale.Status != "withdrawn" || stale.Question.WithdrawnReason == "" || stale.Question.DecisionRevision == 0 || !slices.ContainsFunc(st.Events, func(event domain.Event) bool {
			return event.EntityID == old.ID && event.Action == "supersede_idea_promotion"
		}) {
			t.Fatal(stale)
		}
	})
}

func TestRereviewLegacyArchiveRecoversJournalStatusOrDefaultsPlanned(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		work := ownerWorkReviewOnStore(t, store)
		work.run(work.owner, "update_feature", "p", "f", "", "", map[string]any{"status": "blocked"})
		work.run(work.owner, "update_feature", "p", "f", "", "", map[string]any{"status": "archived"})
		work.run(work.owner, "update_feature", "p", "f", "", "", map[string]any{"title": "Edited while archived"})
		work.run(work.owner, "update_feature", "p", "f", "", "", map[string]any{"status": "archived"})
		if err := store.Update(context.Background(), func(st *domain.State) error {
			feature(st, "f").ArchivedFrom = ""
			st.Features = append(st.Features, domain.Feature{Meta: domain.Meta{ID: "no-history", ProductID: "p"}, Status: "archived", Title: "No history", Goal: "Keep working"})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for featureID, target := range map[string]string{"f": "blocked", "no-history": "planned"} {
			_, err := work.apply(work.agent, "update_feature", "p", featureID, "", "", map[string]any{"status": "idea"})
			expectWorkError(t, err, "restored to "+target)
			if restored := work.run(work.owner, "update_feature", "p", featureID, "", "", map[string]any{"status": target}).(domain.Feature); restored.Status != target || restored.ArchivedFrom != "" {
				t.Fatal(restored)
			}
		}
	})
}
