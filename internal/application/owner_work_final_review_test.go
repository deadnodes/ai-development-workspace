package application

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
)

func TestFinalReviewGenericWritesRequireCanonicalWritableFields(t *testing.T) {
	work := ownerWorkReviewFixture(t)
	admin := userCtx("admin", "human", "", "admin")
	work.run(work.owner, "assign_integration", "p", "", "i", "", map[string]any{"assignee": "alice"})
	work.run(work.owner, "update_feature", "p", "f", "", "", map[string]any{"status": "archived"})
	work.run(admin, "create_environment", "p", "", "", "env", map[string]any{"name": "Dev"})
	before := work.store.state
	tests := []struct {
		command domain.Command
		fields  map[string]any
	}{
		{domain.Command{Action: "update_integration", IntegrationID: "i"}, map[string]any{
			"Assignee": "bob", "ASSIGNEE": "bob", "assignee": "bob", "Status": "ready", "STATUS": "ready", "status": "ready",
			"aſſignee": "bob", "ſtatus": "ready", "fixes_finding_id": "forged", "kind": "hotfix", "Title": "noncanonical", "typo": "unknown",
		}},
		{domain.Command{Action: "create_integration", FeatureID: "f", Data: map[string]any{"title": "New", "objective": "Goal"}}, map[string]any{
			"Assignee": "bob", "assignee": "bob", "Status": "planned", "status": "planned",
		}},
		{domain.Command{Action: "update_feature", FeatureID: "f"}, map[string]any{
			"Archived_From": "idea", "ARCHIVED_FROM": "idea", "archived_from": "idea", "Completed_At": nil, "completed_at": nil,
			"Status": "active", "STATUS": "active", "ſtatus": "active", "Approval_Question_ID": "forged", "Title": "noncanonical",
		}},
		{domain.Command{Action: "record_progress", IntegrationID: "i", Data: map[string]any{"body": "Progress"}}, map[string]any{
			"Question": nil, "QUESTION": map[string]any{"kind": "decision"}, "queſtion": nil, "question": nil,
		}},
		{domain.Command{Action: "record_decision", FeatureID: "f", Data: map[string]any{"title": "Decision", "reason": "Reason"}}, map[string]any{
			"Question": nil, "question": nil, "kind": "question",
		}},
		{domain.Command{Action: "update_environment", ID: "env"}, map[string]any{
			"Namespace": "other", "Selection_Generation": 7, "reservation_epoch": 7, "Runtime": nil,
		}},
		{domain.Command{Action: "create_product", Data: map[string]any{"name": "New"}}, map[string]any{
			"Owner": "bob", "owner": "bob", "stalled_timeout_seconds": 7, "idea_promotion": "agent_allowed",
		}},
	}
	for _, test := range tests {
		for field, value := range test.fields {
			t.Run(test.command.Action+"/"+field, func(t *testing.T) {
				command := test.command
				command.Data = map[string]any{field: value}
				maps.Copy(command.Data, test.command.Data)
				caller := work.bob
				if command.Action == "update_environment" {
					caller = admin
				}
				_, err := work.service.Execute(caller, command)
				if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), field) {
					t.Fatalf("want validation error for %q, got %v", field, err)
				}
				if !reflect.DeepEqual(work.store.state, before) {
					t.Fatal("rejected command changed state")
				}
			})
		}
	}
	work.run(work.bob, "update_integration", "p", "", "i", "", map[string]any{"title": "Canonical integration edit"})
	updated := work.run(work.bob, "update_feature", "p", "f", "", "", map[string]any{"title": "Canonical feature edit"}).(domain.Feature)
	if updated.ArchivedFrom != "active" || integration(&work.store.state, "i").Assignee != "alice" {
		t.Fatal("canonical edit did not preserve server-managed fields")
	}
}

func TestFinalReviewFeatureIneligibilityChecksAllUnfinishedAssignments(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		work := ownerWorkReviewOnStore(t, store)
		for _, transition := range []struct{ from, to string }{{"active", "blocked"}, {"planned", "idea"}, {"active", "archived"}} {
			t.Run(transition.to, func(t *testing.T) {
				ownerAgent := userCtx("agent/owner", "agent", "alice", "member")
				for _, actor := range []struct {
					name    string
					caller  context.Context
					foreign bool
					allow   bool
				}{
					{"mixed-assignments", work.bob, true, false},
					{"foreign-agent", work.bobAgent, true, false},
					{"ownerless-agent", userCtx("agent/no-owner", "agent", "", "member"), true, false},
					{"unrelated-admin", userCtx("admin", "human", "", "admin"), true, false},
					{"product-owner", work.owner, true, true},
					{"owner-agent", ownerAgent, true, true},
					{"sole-assignee", work.bob, false, true},
					{"assignee-agent", work.bobAgent, false, true},
				} {
					t.Run(actor.name, func(t *testing.T) {
						featureID := transition.to + "-" + actor.name
						work.run(work.owner, "create_feature", "p", "", "", featureID, map[string]any{"title": featureID, "goal": "Goal", "status": transition.from, "owner": "bob"})
						for _, suffix := range []string{"bob", "other", "ready", "released"} {
							work.run(work.owner, "create_integration", "p", featureID, "", featureID+"-"+suffix, map[string]any{"title": suffix, "objective": "Deliver"})
						}
						if err := store.Update(context.Background(), func(state *domain.State) error {
							integration(state, featureID+"-bob").Assignee = "bob"
							if actor.foreign {
								integration(state, featureID+"-other").Assignee = "carol"
							}
							for _, status := range []string{"ready", "released"} {
								child := integration(state, featureID+"-"+status)
								child.Assignee, child.Status = "carol", status
							}
							return nil
						}); err != nil {
							t.Fatal(err)
						}
						out, err := work.apply(actor.caller, "update_feature", "p", featureID, "", "", map[string]any{"status": transition.to})
						if actor.allow {
							if err != nil || out.(domain.Feature).Status != transition.to {
								t.Fatalf("authorized transition failed: %v", err)
							}
							return
						}
						if !errors.Is(err, auth.ErrForbidden) {
							t.Fatalf("want forbidden, got %v", err)
						}
						expectWorkError(t, err, "NOT_ASSIGNEE")
						state, err := store.Read(context.Background())
						if err != nil || feature(&state, featureID).Status != transition.from || feature(&state, featureID).ArchivedFrom != "" {
							t.Fatalf("rejected transition changed feature: %v", err)
						}
					})
				}
			})
		}
	})
}
