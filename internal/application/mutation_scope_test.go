package application

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"releasecontrol/internal/domain"
)

// Verify the declared write sets against actual commands, including parent
// reactivation and derived gates. This fails if command logic grows a new write
// without updating its trusted persistence scope.
type checkingScopeStore struct {
	*memoryStore
	t     *testing.T
	calls int
}

func (s *checkingScopeStore) UpdateScoped(ctx context.Context, scope domain.StateMutationScope, fn func(*domain.State) error) error {
	s.calls++
	return s.memoryStore.Update(ctx, func(st *domain.State) error {
		raw, _ := json.Marshal(st)
		var before domain.State
		_ = json.Unmarshal(raw, &before)
		if err := fn(st); err != nil {
			return err
		}
		mutable := map[string]bool{"events": true}
		appends := map[string]bool{}
		for _, key := range scope.MutableCollections {
			mutable[key] = true
		}
		for _, key := range scope.AppendCollections {
			appends[key] = true
		}
		a, b := reflect.ValueOf(before), reflect.ValueOf(*st)
		for i := 0; i < a.NumField(); i++ {
			key := a.Type().Field(i).Tag.Get("json")
			if scope.AllCollections || mutable[key] {
				continue
			}
			if appends[key] {
				if b.Field(i).Len() < a.Field(i).Len() || !reflect.DeepEqual(a.Field(i).Interface(), b.Field(i).Slice(0, a.Field(i).Len()).Interface()) {
					s.t.Fatalf("append-only collection mutated: %s", key)
				}
				continue
			}
			if !reflect.DeepEqual(a.Field(i).Interface(), b.Field(i).Interface()) {
				s.t.Fatalf("undeclared mutation: %s", key)
			}
		}
		return nil
	})
}
func TestCommonCommandMutationScopes(t *testing.T) {
	s, m := fixture(t)
	checked := &checkingScopeStore{memoryStore: m, t: t}
	s.store = checked
	exec(t, s, domain.Command{Action: "create_gate", ID: "g", FeatureID: "f", Data: map[string]any{"title": "checks", "reason": "evidence", "integration_ids": []string{"i"}, "blocking": false}})
	exec(t, s, domain.Command{Action: "record_progress", FeatureID: "f", IntegrationID: "i", Data: map[string]any{"title": "progress", "completed": []string{"part"}, "remaining": []string{"rest"}}})
	exec(t, s, domain.Command{Action: "update_feature", FeatureID: "f", Data: map[string]any{"title": "updated"}})
	exec(t, s, domain.Command{Action: "start_integration", IntegrationID: "i"})
	exec(t, s, domain.Command{Action: "complete_integration", IntegrationID: "i"})
	exec(t, s, domain.Command{Action: "update_feature", FeatureID: "f", Data: map[string]any{"status": "completed"}})
	exec(t, s, domain.Command{Action: "create_integration", FeatureID: "f", ID: "child", Data: map[string]any{"title": "new child", "objective": "reactivate parent"}})
	if m.state.Features[0].Status != "active" {
		t.Fatal("parent was not reactivated")
	}
	exec(t, s, domain.Command{Action: "update_integration", IntegrationID: "i", Data: map[string]any{"objective": "more work"}})
	for _, action := range []string{"record_decision", "record_discovery", "add_blocker", "handoff"} {
		exec(t, s, domain.Command{Action: action, FeatureID: "f", Data: map[string]any{"title": "memory", "reason": "because"}})
	}
	if checked.calls != 12 {
		t.Fatalf("scope not used: %d", checked.calls)
	}
}
