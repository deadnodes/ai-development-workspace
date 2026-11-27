package application

import (
	"context"
	"slices"
	"testing"
	"time"

	"releasecontrol/internal/domain"
)

func thirdReviewState(t *testing.T, store Store) domain.State {
	t.Helper()
	state, err := store.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func thirdReviewGroup(work reviewWork) domain.ExecutionGroup {
	return work.run(work.agent, "create_execution_group", "p", "", "", "", map[string]any{
		"name": "Group", "orchestrator_id": "orchestrator", "integration_ids": []string{"i"},
		"orchestrator_harness": map[string]any{"kind": "codex", "tmux_target": "alice-group"},
	}).(domain.ExecutionGroup)
}

func thirdReviewTurnEnd(work reviewWork) {
	work.run(work.agent, "report_turn_end", "p", "", "", "", map[string]any{"harness_kind": "codex", "tmux_target": "alice-group", "turn_id": "alice-turn"})
}

func TestThirdReviewGroupAssignmentFencesFanoutAndWakeRouting(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		work := ownerWorkReviewOnStore(t, store)
		group := thirdReviewGroup(work)
		for _, integrationID := range []string{"added", "foreign", "legacy-with-execution", "legacy-without-execution"} {
			work.run(work.owner, "create_integration", "p", "f", "", integrationID, map[string]any{"title": integrationID, "objective": "Ship"})
		}
		member := func(integrationID string) map[string]any {
			return map[string]any{"group_id": group.ID, "integration_id": integrationID}
		}
		work.run(work.agent, "add_execution_group_member", "p", "", "", "", member("added"))
		state := thirdReviewState(t, store)
		if group.Owner != "alice" || integration(&state, "i").Assignee != "alice" || integration(&state, "added").Assignee != "alice" {
			t.Fatal("group creation or addition failed to assign work")
		}
		_, err := work.apply(work.bobAgent, "claim_execution", "p", "", "added", "", work.claimData("bob-executor"))
		expectWorkError(t, err, "NOT_ASSIGNEE")
		foreign := work.run(work.bobAgent, "claim_execution", "p", "", "foreign", "", work.claimData("bob-executor")).(domain.IntegrationExecution)
		_, err = work.apply(work.agent, "add_execution_group_member", "p", "", "", "", member("foreign"))
		expectWorkError(t, err, "NOT_ASSIGNEE")
		work.run(work.agent, "claim_execution", "p", "", "i", "", work.claimData("alice-executor"))
		if err := store.Update(context.Background(), func(state *domain.State) error {
			storedGroup := &state.ExecutionGroups[0]
			storedGroup.MemberIntegrationIDs = append(storedGroup.MemberIntegrationIDs, "foreign", "legacy-with-execution", "legacy-without-execution")
			storedGroup.Revisions = append(storedGroup.Revisions, domain.ExecutionGroupRevision{Revision: uint64(len(storedGroup.Revisions) + 1), MemberIntegrationIDs: storedGroup.MemberIntegrationIDs, CreatedAt: time.Now().UTC()})
			state.IntegrationExecutions = append(state.IntegrationExecutions, domain.IntegrationExecution{ID: "legacy-execution", ProductID: "p", IntegrationID: "legacy-with-execution", State: "unassigned", DesiredState: "ready"})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, integrationID := range []string{"legacy-with-execution", "legacy-without-execution"} {
			work.run(work.bobAgent, "claim_execution", "p", "", integrationID, "", work.claimData("bob-legacy"))
			state = thirdReviewState(t, store)
			if slices.Contains(state.ExecutionGroups[0].MemberIntegrationIDs, integrationID) || integration(&state, integrationID).Assignee != "bob" {
				t.Fatalf("claim did not detach %s", integrationID)
			}
		}
		thirdReviewTurnEnd(work)
		if err := store.Update(context.Background(), func(state *domain.State) error {
			reconcileExecutions(state, time.Now().UTC().Add(30*time.Second), 900)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		work.run(work.bobAgent, "heartbeat_execution", "p", "", "foreign", "", map[string]any{"fence": foreign.Fence})
		question := work.run(work.bobAgent, "ask_question", "p", "f", "foreign", "", reviewQuestion(true)).(domain.Memory)
		work.run(work.bob, "answer_questions", "p", "", "", "", reviewAnswer(question.ID, ""))
		wake := reviewPendingWake(t, thirdReviewState(t, store), domain.ExecutionCauseBlockerResolved)
		if wake.WakeTarget.Source != "executor" || wake.WakeTarget.TmuxTarget != "bob-executor" {
			t.Fatal(wake.WakeTarget)
		}
	})
}

func TestThirdReviewExpiredLeaseCannotBeRenewedBeforeReconciliation(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	work.run(work.agent, "start_integration", "p", "", "i", "", nil)
	execution := work.run(work.agent, "claim_execution", "p", "", "i", "", work.claimData("")).(domain.IntegrationExecution)
	work.run(work.owner, "ask_question", "p", "f", "", "", reviewQuestion(true))
	expired := time.Now().UTC().Add(-time.Minute)
	current := executionFor(&work.store.state, "i")
	current.LeaseUntil = &expired
	commands := []domain.Command{
		{Action: "heartbeat_execution", Data: map[string]any{"fence": execution.Fence}},
		{Action: "record_progress", Data: map[string]any{"execution_fence": execution.Fence, "body": "Progress"}},
		{Action: "transition_integration", Data: map[string]any{"execution_fence": execution.Fence, "status": "working"}},
		{Action: "report_execution", Data: map[string]any{"fence": execution.Fence, "state": "running"}},
	}
	for _, condition := range []struct{ state, errorCode string }{{"running", "LEASE_EXPIRED"}, {"lost", "OPEN_BLOCKING_QUESTION"}} {
		executionFor(&work.store.state, "i").State = condition.state
		for _, command := range commands {
			_, err := work.apply(work.agent, command.Action, "p", "", "i", "", command.Data)
			expectWorkError(t, err, condition.errorCode)
			if current = executionFor(&work.store.state, "i"); current.State != condition.state || !current.LeaseUntil.Equal(expired) {
				t.Fatalf("%s changed execution: %+v", command.Action, current)
			}
		}
	}
	manual := ownerWorkReviewFixture(t)
	execution = manual.run(manual.agent, "claim_execution", "p", "", "i", "", manual.claimData("")).(domain.IntegrationExecution)
	blocker := manual.run(manual.owner, "add_blocker", "p", "", "i", "", map[string]any{"title": "Manual blocker"}).(domain.Memory)
	manual.run(manual.agent, "report_execution", "p", "", "i", "", map[string]any{"fence": execution.Fence, "state": "blocked", "blocker_ref": blocker.ID})
	_, err := manual.apply(manual.agent, "report_execution", "p", "", "i", "", map[string]any{"fence": execution.Fence, "state": "running"})
	expectWorkError(t, err, "OPEN_BLOCKER")
}

func TestThirdReviewForeignReadinessEditsRequireAssignee(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	for _, integrationID := range []string{"first", "second"} {
		work.run(work.owner, "create_integration", "p", "f", "", integrationID, map[string]any{"title": integrationID, "objective": "Ship"})
	}
	work.run(work.owner, "assign_integration", "p", "", "i", "", map[string]any{"assignee": "alice"})
	for _, status := range []string{"planned", "working"} {
		if status == "working" {
			work.run(work.agent, "start_integration", "p", "", "i", "", nil)
		}
		_, err := work.apply(work.bob, "update_integration", "p", "", "i", "", map[string]any{"dependencies": []string{"first"}})
		expectWorkError(t, err, "NOT_ASSIGNEE")
	}
	work.run(work.agent, "update_integration", "p", "", "i", "", map[string]any{"dependencies": []string{"first"}, "acceptance_criteria": []string{"Original"}})
	for key, replacement := range map[string]any{"dependencies": []string{"second"}, "acceptance_criteria": []string{"Changed"}, "commits": []domain.Commit{{}}, "repositories": []string{"new"}} {
		_, err := work.apply(work.bob, "update_integration", "p", "", "i", "", map[string]any{key: replacement})
		expectWorkError(t, err, "NOT_ASSIGNEE")
	}
	if child := integration(&work.store.state, "i"); !slices.Equal(child.Dependencies, []string{"first"}) || !slices.Equal(child.AcceptanceCriteria, []string{"Original"}) {
		t.Fatal(child)
	}
}

func TestThirdReviewDebouncedGroupExitPreservesBlockerResume(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	work.run(work.agent, "start_integration", "p", "", "i", "", nil)
	thirdReviewGroup(work)
	claim := work.claimData("alice-executor")
	claim["desired_state"] = "released"
	execution := work.run(work.agent, "claim_execution", "p", "", "i", "", claim).(domain.IntegrationExecution)
	work.run(work.agent, "complete_integration", "p", "", "i", "", nil)
	thirdReviewTurnEnd(work)
	question := work.run(work.agent, "ask_question", "p", "f", "i", "", reviewQuestion(true)).(domain.Memory)
	work.run(work.owner, "answer_questions", "p", "", "", "", reviewAnswer(question.ID, ""))
	initial := reviewPendingWake(t, work.store.state, domain.ExecutionCauseBlockerResolved)
	reconcileExecutions(&work.store.state, time.Now().UTC().Add(30*time.Second), 900)
	current := executionFor(&work.store.state, "i")
	wake := reviewPendingWake(t, work.store.state, domain.ExecutionCauseBlockerResolved)
	items := work.query(work.agent, "next_work", "p")["items"].([]workItem)
	if current.State != "blocked" || current.BlockerFence == nil || *current.BlockerFence != execution.Fence || wake.ID != initial.ID || len(items) != 1 || items[0].Kind != "resume_integration" {
		t.Fatalf("lost actionable blocker resume: %+v, %+v, %+v", current, wake, items)
	}
}

func TestThirdReviewBlockerTransitionPreservesOrdinaryAnswerContinuation(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	work.register(work.owner, "alice-orchestrator")
	work.run(work.agent, "claim_execution", "p", "", "i", "", work.claimData(""))
	ordinary := work.run(work.agent, "ask_question", "p", "f", "i", "", reviewQuestion(false)).(domain.Memory)
	work.run(work.owner, "answer_questions", "p", "", "", "", reviewAnswer(ordinary.ID, ""))
	initial := reviewPendingWake(t, work.store.state, domain.ExecutionCauseQuestionContinuation)
	blocker := work.run(work.agent, "ask_question", "p", "f", "i", "", reviewQuestion(true)).(domain.Memory)
	work.run(work.owner, "answer_questions", "p", "", "", "", reviewAnswer(blocker.ID, ""))
	current := reviewPendingWake(t, work.store.state, domain.ExecutionCauseQuestionContinuation)
	if current.ID != initial.ID || !slices.Equal(current.QuestionIDs, []string{ordinary.ID, blocker.ID}) {
		t.Fatal(current)
	}
	work.run(work.agent, "claim_execution_event_delivery", "p", "", "", "", reviewDelivery(current.ID))
	fence := executionFor(&work.store.state, "i").Fence
	work.run(work.agent, "report_execution", "p", "", "i", "", map[string]any{"fence": fence, "state": "paused"})
	work.run(work.agent, "claim_execution", "p", "", "i", "", work.claimData(""))
	if slices.ContainsFunc(work.store.state.ExecutionEvents, func(event domain.ExecutionEvent) bool {
		return event.ID == current.ID && event.Resolution.ResolvedAt == nil
	}) {
		t.Fatal("continuation survived an invalidated fence")
	}
}

func TestThirdReviewDeletingWakeProductReroutesGlobalQueue(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		work := ownerWorkReviewOnStore(t, store)
		work.run(work.owner, "create_product", "", "", "", "remaining-product", map[string]any{"name": "Remaining"})
		work.run(work.owner, "create_feature", "remaining-product", "", "", "remaining-feature", map[string]any{"title": "Remaining", "goal": "Ship"})
		work.run(work.owner, "create_integration", "remaining-product", "remaining-feature", "", "remaining-integration", map[string]any{"title": "Remaining", "objective": "Ship"})
		work.run(work.owner, "register_user_orchestrator", "", "", "", "", map[string]any{"harness": map[string]any{"kind": "codex", "tmux_target": "alice-queue"}})
		old := reviewPendingWake(t, thirdReviewState(t, store), domain.ExecutionCauseQueueAvailable)
		if old.ProductID != "p" {
			t.Fatal(old)
		}
		work.run(userCtx("admin", "human", "", "admin"), "delete_product", "p", "", "", "", map[string]any{"name": "Product"})
		state := thirdReviewState(t, store)
		wake := reviewPendingWake(t, state, domain.ExecutionCauseQueueAvailable)
		if wake.ProductID != "remaining-product" || wake.RegistrationID != old.RegistrationID || wake.ID == old.ID || !slices.ContainsFunc(state.Events, func(event domain.Event) bool {
			return event.EntityID == old.ID && event.Action == "execution_event_resolved"
		}) {
			t.Fatal(wake)
		}
		work.run(work.agent, "claim_execution_event_delivery", "remaining-product", "", "", "", reviewDelivery(wake.ID))
	})
}

func TestThirdReviewDirectDependencyDecisionWithdrawsOpenApproval(t *testing.T) {
	for _, decision := range []string{"accept", "reject"} {
		t.Run(decision, func(t *testing.T) {
			work := ownerWorkReviewFixture(t)
			work.idea("other")
			dependency := work.run(work.bobAgent, "propose_dependency", "p", "", "", "", map[string]any{"from_feature_id": "f", "to_feature_id": "other", "type": "relates", "rationale": "Related"}).(domain.FeatureDependency)
			approval := work.run(work.bobAgent, "request_dependency_decision", "p", "", "", "", map[string]any{"dependency_id": dependency.ID}).(domain.Memory)
			first := work.run(work.owner, "ask_question", "p", "f", "", "", reviewQuestion(false)).(domain.Memory)
			second := work.run(work.owner, "ask_question", "p", "f", "", "", reviewQuestion(false)).(domain.Memory)
			work.run(work.owner, "decide_dependency", "p", "", "", "", map[string]any{"dependency_id": dependency.ID, "decision": decision})
			inbox := work.query(work.owner, "list_questions", "p")["items"].([]questionView)
			if len(inbox) != 2 || slices.ContainsFunc(inbox, func(question questionView) bool { return question.ID == approval.ID }) {
				t.Fatalf("stale approval in inbox: %+v", inbox)
			}
			answers := append(reviewAnswer(first.ID, "")["answers"].([]map[string]any), reviewAnswer(second.ID, "")["answers"].([]map[string]any)...)
			work.run(work.owner, "answer_questions", "p", "", "", "", map[string]any{"answers": answers})
			state := work.store.state
			if old := questionByID(&state, approval.ID); old.Status != "withdrawn" || old.Question.WithdrawnReason == "" || dependencyByID(&state, dependency.ID).ApprovalQuestionID != old.ID || !hasAuditAction(state.Events, "supersede_dependency_decision") {
				t.Fatal(old)
			}
		})
	}
}

func TestThirdReviewLegacyRestoreRecoversImplicitReopenAndWarnsOnFallback(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		work := ownerWorkReviewOnStore(t, store)
		work.run(work.agent, "start_integration", "p", "", "i", "", nil)
		work.run(work.agent, "complete_integration", "p", "", "i", "", nil)
		work.run(work.owner, "update_feature", "p", "f", "", "", map[string]any{"status": "completed"})
		work.run(work.owner, "create_integration", "p", "f", "", "reopens", map[string]any{"title": "Reopens", "objective": "Ship"})
		work.run(work.owner, "update_feature", "p", "f", "", "", map[string]any{"status": "archived"})
		work.run(work.owner, "create_integration", "p", "f", "", "while-archived", map[string]any{"title": "Archived addition", "objective": "Ship"})
		work.run(work.owner, "update_feature", "p", "f", "", "", map[string]any{"status": "archived"})
		work.idea("approved")
		approval := work.run(work.agent, "request_idea_promotion", "p", "approved", "", "", map[string]any{"rationale": "Proceed"}).(domain.Memory)
		work.run(work.owner, "answer_questions", "p", "", "", "", reviewAnswer(approval.ID, "approve"))
		work.run(work.owner, "update_feature", "p", "approved", "", "", map[string]any{"status": "archived"})
		if err := store.Update(context.Background(), func(state *domain.State) error {
			feature(state, "f").ArchivedFrom = ""
			feature(state, "approved").ArchivedFrom = ""
			for _, featureID := range []string{"unknown", "invalid-completed"} {
				state.Features = append(state.Features, domain.Feature{Meta: domain.Meta{ID: featureID, ProductID: "p"}, Status: "archived", Title: featureID, Goal: "Restore work"})
			}
			feature(state, "invalid-completed").ArchivedFrom = "completed"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if restored := work.run(work.owner, "update_feature", "p", "f", "", "", map[string]any{"status": "active"}).(domain.Feature); restored.Status != "active" || restored.ArchivedFrom != "" {
			t.Fatal(restored)
		}
		work.run(work.owner, "update_feature", "p", "approved", "", "", map[string]any{"status": "ready_for_execution"})
		for _, featureID := range []string{"unknown", "invalid-completed"} {
			_, err := work.apply(work.agent, "update_feature", "p", featureID, "", "", map[string]any{"status": "planned"})
			expectWorkError(t, err, "OWNER_APPROVAL_REQUIRED")
			restored := work.run(work.owner, "update_feature", "p", featureID, "", "", map[string]any{"status": "planned"}).(domain.Feature)
			state := thirdReviewState(t, store)
			if restored.Status != "planned" || restored.ArchivedFrom != "" || !slices.ContainsFunc(state.Events, func(event domain.Event) bool {
				return event.EntityID == featureID && event.Action == "feature_restore_warning" && event.Data.Data["status"] == "planned" && event.Data.Data["warning"] != ""
			}) {
				t.Fatal(restored)
			}
		}
	})
}
