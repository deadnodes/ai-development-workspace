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

func TestOwnerWorkAcrossStores(t *testing.T) {
	for name, open := range map[string]func(*testing.T) Store{
		"memory":   func(*testing.T) Store { return &memoryStore{domain.EmptyState()} },
		"sqlite":   func(t *testing.T) Store { return privateLocalFixture(t) },
		"postgres": func(t *testing.T) Store { return privatePostgresFixture(t) },
	} {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			service := New(store)
			owner := userCtx("alice", "human", "", "member")
			agent := userCtx("agent/alice", "agent", "alice", "member")
			other := userCtx("bob", "human", "", "member")
			apply := func(ctx context.Context, action, product, parent, child, id string, data map[string]any) (any, error) {
				return service.Execute(ctx, domain.Command{Action: action, ProductID: product, FeatureID: parent, IntegrationID: child, ID: id, Data: data})
			}
			run := func(ctx context.Context, action, product, parent, child, id string, data map[string]any) any {
				t.Helper()
				result, err := apply(ctx, action, product, parent, child, id, data)
				if err != nil {
					t.Fatalf("%s: %v", action, err)
				}
				return result
			}
			query := func(ctx context.Context, name, input string) any {
				t.Helper()
				result, err := service.Query(ctx, name, input)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				return result
			}
			run(owner, "create_product", "", "", "", "p", map[string]any{"name": "Product"})
			run(owner, "register_user_orchestrator", "p", "", "", "", map[string]any{"harness": map[string]any{"kind": "codex", "tmux_target": "owner:1.0", "session_ref": "owner-session"}})
			for _, parent := range []string{"idea", "prerequisite"} {
				run(owner, "create_feature", "p", "", "", parent, map[string]any{"title": parent, "goal": "Goal", "status": "idea", "priority": 10})
			}
			if _, err := apply(agent, "update_feature", "", "idea", "", "", map[string]any{"status": "ready_for_execution"}); !errors.Is(err, auth.ErrForbidden) {
				t.Fatalf("agent bypassed approval: %v", err)
			}
			request := run(agent, "request_idea_promotion", "", "idea", "", "", map[string]any{"rationale": "Sequenced execution"}).(domain.Memory)
			if request.Question.AddressedTo != "alice" || request.Question.AskedBy != "agent/alice" {
				t.Fatalf("decision attribution: %+v", request.Question)
			}
			if _, err := apply(agent, "ask_question", "", "idea", "", "", map[string]any{"title": "Conflict", "body": "No preference", "kind": "conflict"}); err == nil {
				t.Fatal("accepted conflict without recommendation")
			}
			second := run(agent, "ask_question", "", "idea", "", "", map[string]any{"title": "Timing", "body": "What is the timing?", "kind": "question", "context_refs": []string{"idea"}}).(domain.Memory)
			if view := query(owner, "get_question", second.ID).(questionView); len(view.Context) != 1 || view.Context[0].Title != "idea" {
				t.Fatalf("missing resolved context: %+v", view)
			}
			if err := store.Update(context.Background(), func(state *domain.State) error {
				for position := range 22 {
					state.Memories = append(state.Memories, domain.Memory{Meta: domain.Meta{ID: fmt.Sprintf("recent-%02d", position), ProductID: "p", FeatureID: "idea"}, Kind: "decision", Title: "Recent history"})
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			summary, err := service.ResumeSummary(owner, "idea")
			if err != nil {
				t.Fatal(err)
			}
			openQuestions := summary.(map[string]any)["open_questions"].([]map[string]any)
			if len(openQuestions) != 2 || !slices.ContainsFunc(openQuestions, func(question map[string]any) bool { return question["id"] == second.ID }) {
				t.Fatalf("older open questions missing from resume: %+v", openQuestions)
			}
			entries := []map[string]any{
				{"question_id": request.ID, "text": "Proceed", "option_id": "approve", "validation": map[string]any{"verdict": "consistent", "note": "Reviewed"}},
				{"question_id": second.ID, "text": "Tomorrow", "validation": map[string]any{"verdict": "unclear", "note": "Local date unclear"}},
			}
			if _, err := apply(owner, "answer_questions", "", "", "", "", map[string]any{"answers": entries}); err == nil || !strings.Contains(err.Error(), "ANSWER_NEEDS_CLARIFICATION") {
				t.Fatalf("contradiction should reject the batch: %v", err)
			}
			if got := query(owner, "get_question", request.ID).(questionView); got.Status != "open" || got.Answer != nil {
				t.Fatal("partial answer committed")
			}
			entries[1]["override"] = true
			if _, err := apply(other, "answer_questions", "", "", "", "", map[string]any{"answers": entries}); !errors.Is(err, auth.ErrForbidden) {
				t.Fatalf("non-owner answered: %v", err)
			}
			run(owner, "answer_questions", "", "", "", "", map[string]any{"answers": entries})
			work := query(owner, "next_work", "p").(map[string]any)["items"].([]workItem)
			if len(work) != 1 || work[0].Kind != "plan_feature" || work[0].FeatureID != "idea" {
				t.Fatalf("wrong plan work: %+v", work)
			}
			checkState := func() domain.State {
				t.Helper()
				state, err := store.Read(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				return state
			}
			countWork := func(state domain.State) int {
				count := 0
				for _, event := range state.ExecutionEvents {
					if event.Cause == domain.ExecutionCauseQueueAvailable {
						count++
						if event.Owner != "alice" || event.WakeTarget.TmuxTarget != "owner:1.0" || event.DedupeKey == "" {
							t.Fatalf("wrong work wake: %+v", event)
						}
					}
				}
				return count
			}
			state := checkState()
			if countWork(state) != 1 || !slices.ContainsFunc(state.Memories, func(memory domain.Memory) bool {
				return memory.Kind == "decision" && strings.Contains(memory.Body, "What is the timing?") && strings.Contains(memory.Body, "Tomorrow")
			}) {
				t.Fatal("missing deduplicated wake or durable decision")
			}
			if err := service.ReconcileExecutionsOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if countWork(checkState()) != 1 {
				t.Fatal("unchanged queue emitted a new wake")
			}
			followup := run(agent, "ask_question", "", "idea", "", "", map[string]any{"title": "Follow-up", "body": "Is this still needed?", "kind": "question"}).(domain.Memory)
			run(owner, "withdraw_question", "", "", "", "", map[string]any{"question_id": followup.ID, "reason": "No longer needed"})
			if got := query(owner, "get_question", followup.ID).(questionView); got.Status != "withdrawn" {
				t.Fatal(got)
			}
			edge := run(agent, "propose_dependency", "", "", "", "", map[string]any{"from_feature_id": "idea", "to_feature_id": "prerequisite", "type": "requires", "rationale": "Needed first"}).(domain.FeatureDependency)
			approval := run(agent, "request_dependency_decision", "", "", "", "", map[string]any{"dependency_id": edge.ID}).(domain.Memory)
			decision := []map[string]any{{"question_id": approval.ID, "text": "Approved", "option_id": "approve", "validation": map[string]any{"verdict": "consistent", "note": "Reviewed"}}}
			run(owner, "answer_questions", "", "", "", "", map[string]any{"answers": decision})
			run(agent, "decide_dependency", "", "", "", "", map[string]any{"dependency_id": edge.ID, "decision": "accept", "approval_question_id": approval.ID})
			if _, err := apply(owner, "propose_dependency", "", "", "", "", map[string]any{"from_feature_id": "prerequisite", "to_feature_id": "idea", "type": "requires", "rationale": "Loop", "accept": true}); err == nil || !strings.Contains(err.Error(), "DEPENDENCY_CYCLE: prerequisite -> idea -> prerequisite") {
				t.Fatalf("cycle not identified: %v", err)
			}
			run(owner, "create_product", "", "", "", "other-product", map[string]any{"name": "Private target", "visibility": "private"})
			run(owner, "create_feature", "other-product", "", "", "private-prerequisite", map[string]any{"title": "Private milestone", "goal": "Goal", "status": "idea"})
			crossEdge := run(agent, "propose_dependency", "", "", "", "", map[string]any{"from_feature_id": "idea", "to_feature_id": "private-prerequisite", "type": "relates", "rationale": "Private milestone"}).(domain.FeatureDependency)
			crossApproval := run(agent, "request_dependency_decision", "", "", "", "", map[string]any{"dependency_id": crossEdge.ID}).(domain.Memory)
			if strings.Contains(crossApproval.Title+crossApproval.Body, "Private milestone") || len(crossApproval.Question.ContextRefs) != 1 {
				t.Fatalf("cross-product approval exposed the target: %+v", crossApproval)
			}
			decision[0]["question_id"] = crossApproval.ID
			run(owner, "answer_questions", "", "", "", "", map[string]any{"answers": decision})
			run(agent, "decide_dependency", "", "", "", "", map[string]any{"dependency_id": crossEdge.ID, "decision": "accept", "approval_question_id": crossApproval.ID})
			run(owner, "unregister_user_orchestrator", "p", "", "", "", nil)
			state = checkState()
			if len(state.UserOrchestrators) != 1 || state.UserOrchestrators[0].UnregisteredAt == nil {
				t.Fatal("registration still active")
			}
		})
	}
}

func TestNextWorkOrderingAndExclusions(t *testing.T) {
	priority := 5
	state := domain.EmptyState()
	state.Features = []domain.Feature{
		{Meta: domain.Meta{ID: "default", ProductID: "p"}, Status: "ready_for_execution"},
		{Meta: domain.Meta{ID: "later", ProductID: "p", CreatedAt: time.Unix(2, 0)}, Status: "ready_for_execution", Priority: &priority},
		{Meta: domain.Meta{ID: "first", ProductID: "p", CreatedAt: time.Unix(1, 0)}, Status: "active", Priority: &priority},
		{Meta: domain.Meta{ID: "waiting", ProductID: "p"}, Status: "ready_for_execution"},
		{Meta: domain.Meta{ID: "draft", ProductID: "p"}, Status: "idea"},
	}
	child := func(id, status, assignee string, position int) domain.Integration {
		return domain.Integration{Meta: domain.Meta{ID: id, ProductID: "p", FeatureID: "first"}, Status: status, Assignee: assignee, Position: position}
	}
	state.Integrations = []domain.Integration{
		child("second", "planned", "alice", 2), child("first", "planned", "", 1), child("theirs", "planned", "bob", 0),
		child("active", "working", "alice", 3), child("ready", "ready", "alice", 4), child("ask", "working", "alice", 5),
		child("dependent", "planned", "alice", 6),
	}
	state.Integrations[6].Dependencies = []string{"ask"}
	state.IntegrationExecutions = []domain.IntegrationExecution{{IntegrationID: "active", State: "running"}, {IntegrationID: "ask", State: "blocked"}, {IntegrationID: "second", State: "lost"}}
	state.Memories = []domain.Memory{{Meta: domain.Meta{ID: "question", ProductID: "p", FeatureID: "first"}, IntegrationID: "ask", Kind: "question", Status: "open", Question: &domain.QuestionDetails{Blocking: true}}}
	state.FeatureDependencies = []domain.FeatureDependency{
		{Meta: domain.Meta{ID: "edge"}, FromFeatureID: "waiting", ToFeatureID: "draft", Type: "requires", Status: "accepted"},
		{FromFeatureID: "later", ToFeatureID: "draft", Type: "requires", Status: "proposed"},
	}
	items, skipped := nextWork(&state, "alice", "p", func(string) bool { return true }, 25)
	if len(items) != 4 || items[0].IntegrationID != "first" || items[1].IntegrationID != "second" || items[2].FeatureID != "later" || items[3].FeatureID != "default" {
		t.Fatalf("wrong work order: %+v", items)
	}
	if len(skipped) != 3 || skipped[0].QuestionID != "question" || skipped[1].DependencyID != "ask" || skipped[2].DependencyID != "edge" {
		t.Fatalf("wrong skipped work: %+v", skipped)
	}
}

func TestBlockingQuestionReconcilesExecution(t *testing.T) {
	_, store := fixture(t)
	service := New(store)
	store.state.Products[0].Owner = "alice"
	agent := userCtx("agent/alice", "agent", "alice", "member")
	owner := userCtx("alice", "human", "", "member")
	apply := func(ctx context.Context, command domain.Command) any {
		t.Helper()
		result, err := service.Execute(ctx, command)
		if err != nil {
			t.Fatalf("%s: %v", command.Action, err)
		}
		return result
	}
	apply(agent, domain.Command{Action: "claim_execution", ProductID: "p", IntegrationID: "i", Data: map[string]any{"orchestrator_id": "orchestrator", "agent_id": "executor", "harness": map[string]any{"kind": "codex", "tmux_target": "executor:0.0"}}})
	question := apply(agent, domain.Command{Action: "ask_question", FeatureID: "f", IntegrationID: "i", Data: map[string]any{"title": "Blocked", "body": "Choose a path", "kind": "question", "blocking": true}}).(domain.Memory)
	if execution := executionFor(&store.state, "i"); execution.State != "blocked" || execution.BlockerRef != question.ID {
		t.Fatalf("execution not blocked: %+v", execution)
	}
	apply(owner, domain.Command{Action: "answer_questions", Data: map[string]any{"answers": []map[string]any{{"question_id": question.ID, "text": "Proceed", "validation": map[string]any{"verdict": "consistent", "note": "Reviewed"}}}}})
	if execution := executionFor(&store.state, "i"); !execution.Reconciliation.ActionRequired {
		t.Fatalf("execution not actionable: %+v", execution)
	}
	if !slices.ContainsFunc(store.state.ExecutionEvents, func(event domain.ExecutionEvent) bool {
		return event.IntegrationID == "i" && event.Cause == domain.ExecutionCauseBlockerResolved && event.WakeTarget.TmuxTarget == "executor:0.0"
	}) {
		t.Fatalf("missing targeted blocker wake: %+v", store.state.ExecutionEvents)
	}
}
