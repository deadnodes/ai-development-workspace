package domain

import (
	"strings"
	"testing"

	"releasecontrol/internal/delivery"
)

func statusStateConstructors() map[string]func(string) State {
	m := Meta{ID: "entity"}
	return map[string]func(string) State{
		"feature":        func(v string) State { return State{Features: []Feature{{Meta: m, Status: v}}} },
		"integration":    func(v string) State { return State{Integrations: []Integration{{Meta: m, Status: v}}} },
		"blocker":        func(v string) State { return State{Memories: []Memory{{Meta: m, Kind: "blocker", Status: v}}} },
		"finding":        func(v string) State { return State{Findings: []Finding{{Meta: m, Status: v}}} },
		"check_result":   func(v string) State { return State{Results: []CheckResult{{Meta: m, Result: v}}} },
		"gate_result":    func(v string) State { return State{Gates: []Gate{{Meta: m, Result: v}}} },
		"composition":    func(v string) State { return State{Compositions: []Composition{{Meta: m, Status: v}}} },
		"release":        func(v string) State { return State{Releases: []Release{{Meta: m, Status: v}}} },
		"operation":      func(v string) State { return State{Operations: []ExternalOperation{{Meta: m, Status: v}}} },
		"operation_step": func(v string) State { return State{OperationSteps: []OperationStep{{Meta: m, Status: v}}} },
		"deployment_state": func(v string) State {
			return State{Operations: []ExternalOperation{{Meta: m, Status: "PENDING", DeploymentState: v}}}
		},
		"review_sync":     func(v string) State { return State{ReviewSyncs: []ReviewSync{{Meta: m, Status: v}}} },
		"git_observation": func(v string) State { return State{GitObservations: []GitObservation{{Meta: m, Status: v}}} },
		"pull_request": func(v string) State {
			return State{Integrations: []Integration{{Meta: m, Status: "planned", PullRequests: []PullRequest{{ID: "pr", Status: v}}}}}
		},
		"artifact_availability": func(v string) State { return State{DeliveryArtifacts: []DeliveryArtifact{{Meta: m, Availability: v}}} },
		"scenario_result":       func(v string) State { return State{ScenarioRuns: []ScenarioRun{{Meta: m, Result: v}}} },
	}
}
func TestStateStatusesRejectUnknownAndRequiredEmpty(t *testing.T) {
	for kind, build := range statusStateConstructors() {
		t.Run(kind, func(t *testing.T) {
			for _, unknown := range []string{"invented", " PASSED ", ""} {
				if unknown == "" && kind == "deployment_state" {
					continue
				}
				err := ValidateStateStatuses(build(unknown))
				if err == nil || !strings.Contains(err.Error(), kind) || !strings.Contains(err.Error(), "entity") {
					t.Fatalf("missing contextual rejection for %q: %v", unknown, err)
				}
			}
		})
	}
}
func TestStateStatusesAcceptEntirePublishedCatalog(t *testing.T) {
	catalog := StatusCatalog()
	for kind, build := range statusStateConstructors() {
		t.Run(kind, func(t *testing.T) {
			if len(catalog[kind]) == 0 {
				t.Fatal("missing status catalog")
			}
			for _, value := range catalog[kind] {
				if err := ValidateStateStatuses(build(value)); err != nil {
					t.Fatalf("published %q rejected: %v", value, err)
				}
			}
		})
	}
	if err := ValidateStateStatuses(State{Operations: []ExternalOperation{{Meta: Meta{ID: "pending"}, Status: "PENDING"}}}); err != nil {
		t.Fatal(err)
	}
}
func TestStateStatusesValidateTypedSnapshots(t *testing.T) {
	tests := []State{
		{Releases: []Release{{Meta: Meta{ID: "release"}, Status: "planned", Snapshots: []Integration{{Meta: Meta{ID: "snapshot"}, Status: "invented"}}}}},
		{Releases: []Release{{Meta: Meta{ID: "release"}, Status: "planned", Snapshots: []Integration{{Meta: Meta{ID: "snapshot"}, Status: "ready", PullRequests: []PullRequest{{ID: "pr", Status: "invented"}}}}}}},
		{Operations: []ExternalOperation{{Meta: Meta{ID: "operation"}, Status: "PENDING", CompositionSnapshot: &Composition{Meta: Meta{ID: "snapshot"}, Status: "invented"}}}},
	}
	for _, state := range tests {
		if err := ValidateStateStatuses(state); err == nil || !strings.Contains(err.Error(), "snapshot") {
			t.Fatalf("snapshot status accepted: %v", err)
		}
	}
}
func TestStateStatusesPreserveRawProviderAndHistoricalVocabulary(t *testing.T) {
	state := State{
		Operations:        []ExternalOperation{{Status: "PENDING", Run: &delivery.RunResult{Status: "future_vendor_status"}}},
		DeliveryBuildRuns: []DeliveryBuildRun{{Result: delivery.RunResult{Status: "vendor_waiting"}}},
		Events:            []Event{{Action: "historic_action", Data: Command{Data: map[string]any{"status": "legacy_status"}}}},
		Environments:      []Environment{{Desired: map[string]any{"status": "provider_owned"}}},
	}
	if err := ValidateStateStatuses(state); err != nil {
		t.Fatal(err)
	}
	state.Memories = []Memory{{Meta: Meta{ID: "decision"}, Kind: "decision", Status: "open"}}
	if err := ValidateStateStatuses(state); err == nil {
		t.Fatal("status allowed on non-blocker memory")
	}
}
