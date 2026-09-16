package application

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"releasecontrol/internal/domain"
)

func TestResumeCombinesLatestHandoffsAndRemainingInPlanOrder(t *testing.T) {
	s, _ := fixture(t)
	exec(t, s, domain.Command{Action: "update_integration", IntegrationID: "i", Data: map[string]any{"position": 2}})
	exec(t, s, domain.Command{Action: "create_integration", ID: "j", FeatureID: "f", Data: map[string]any{"title": "First", "objective": "First", "position": 1}})
	exec(t, s, domain.Command{Action: "create_integration", ID: "k", FeatureID: "f", Data: map[string]any{"title": "Third", "objective": "Third", "position": 3, "remaining": []string{"remaining task", "shared"}}})
	exec(t, s, domain.Command{Action: "handoff", FeatureID: "f", IntegrationID: "i", Data: map[string]any{"current": "old", "next": []string{"obsolete"}}})
	exec(t, s, domain.Command{Action: "handoff", FeatureID: "f", IntegrationID: "i", Data: map[string]any{"current": "second", "next": []string{"second task", "shared"}}})
	exec(t, s, domain.Command{Action: "handoff", FeatureID: "f", IntegrationID: "j", Data: map[string]any{"current": "first", "next": []string{"first task", "shared"}}})
	exec(t, s, domain.Command{Action: "handoff", FeatureID: "f", Data: map[string]any{"current": "feature", "next": []string{"feature task"}}})
	exec(t, s, domain.Command{Action: "create_gate", ID: "late", FeatureID: "f", Data: map[string]any{"title": "Later", "reason": "risk", "position": 2, "integration_ids": []string{"i"}}})
	exec(t, s, domain.Command{Action: "create_gate", ID: "early", FeatureID: "f", Data: map[string]any{"title": "Earlier", "reason": "risk", "position": 1, "integration_ids": []string{"j"}}})
	v, e := s.Resume(context.Background(), "f")
	if e != nil {
		t.Fatal(e)
	}
	context := v.(map[string]any)
	want := []string{"feature task", "first task", "shared", "second task", "remaining task"}
	if !reflect.DeepEqual(context["next_actions"], want) {
		t.Fatalf("next actions: %#v", context["next_actions"])
	}
	if context["gates"].([]domain.Gate)[0].ID != "early" {
		t.Fatal("gates not in plan order")
	}
	if len(context["memories"].([]domain.Memory)) != 4 {
		t.Fatal("historical handoffs missing")
	}
}

func TestResumeBoundsAuditWithoutCommandPayloads(t *testing.T) {
	s, _ := fixture(t)
	for range 25 {
		exec(t, s, domain.Command{Action: "record_discovery", FeatureID: "f", Data: map[string]any{"body": "discovery"}})
	}
	v, e := s.Resume(context.Background(), "f")
	if e != nil {
		t.Fatal(e)
	}
	context := v.(map[string]any)
	events := context["events"].([]EventSummary)
	if len(events) != 20 || context["events_total"] != 27 || context["events_truncated"] != true {
		t.Fatal("incorrect audit bounds", context["events_total"])
	}
	b, e := json.Marshal(events)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(b), `"data"`) {
		t.Fatal("command payload leaked into summary")
	}
	if len(context["memories"].([]domain.Memory)) != 25 {
		t.Fatal("structured memory truncated")
	}
}
