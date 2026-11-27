package application

import (
	"context"
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
	work := reviewWork{t, New(store), store, userCtx("alice", "human", "", "member"), userCtx("agent/alice", "agent", "alice", "member"), userCtx("bob", "human", "", "member"), userCtx("agent/bob", "agent", "bob", "member")}
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
	for _, caller := range []context.Context{work.bobAgent, work.bob, work.agent} {
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
	work.run(work.owner, "answer_questions", "p", "", "", "", reviewAnswer(question.ID, ""))
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
