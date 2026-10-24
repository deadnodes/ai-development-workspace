package application

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
)

// A context without a principal is reserved for the trusted local application and
// internally constructed worker contexts. Authenticated transports fail closed
// before calling this layer when no principal is available.
func attributedActor(ctx context.Context, actor string) string {
	if p, ok := auth.FromContext(ctx); ok {
		return p.ID
	}
	return actor
}
func instanceAdmin(p auth.Principal) bool {
	return (p.Kind == "human" || (p.Kind == "service" && p.ID == "service/bootstrap")) && p.Role == "admin" && slices.Contains(p.Permissions, "admin") && slices.Contains(p.Products, "*")
}
func allowedProduct(p auth.Principal, product string) bool {
	return product != "" && (slices.Contains(p.Products, "*") || slices.Contains(p.Products, product))
}
func authorize(ctx context.Context, permission, product string) error {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return nil
	}
	if instanceAdmin(p) {
		return nil
	}
	if permission == "admin" || !slices.Contains(p.Permissions, permission) || (product != "" && !allowedProduct(p, product)) {
		return auth.ErrForbidden
	}
	return nil
}
func stateRows(st domain.State) (map[string][]map[string]any, error) {
	b, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	var rows map[string][]map[string]any
	err = json.Unmarshal(b, &rows)
	return rows, err
}
func rowProduct(kind string, row map[string]any) string {
	if kind == "products" {
		id, _ := row["id"].(string)
		return id
	}
	p, _ := row["product_id"].(string)
	return p
}

// Every state collection is projected using its persisted ownership. Global
// provider/registry records are visible only to an explicit instance admin.
func scopedState(ctx context.Context, st domain.State) (domain.State, error) {
	if err := authorize(ctx, "read", ""); err != nil {
		return domain.State{}, err
	}
	p, ok := auth.FromContext(ctx)
	if !ok || instanceAdmin(p) {
		return st, nil
	}
	rows, err := stateRows(st)
	if err != nil {
		return domain.State{}, err
	}
	for kind, group := range rows {
		filtered := []map[string]any{}
		for _, row := range group {
			if allowedProduct(p, rowProduct(kind, row)) {
				filtered = append(filtered, row)
			}
		}
		rows[kind] = filtered
	}
	b, err := json.Marshal(rows)
	if err != nil {
		return domain.State{}, err
	}
	var out domain.State
	err = json.Unmarshal(b, &out)
	return out, err
}

// This allowlist deliberately fails closed for newly introduced commands.
var memberWriteActions = []string{
	"create_integration_branch", "protect_integration_revision", "claim_composition_conflict", "record_conflict_resolution", "verify_conflict_resolution",
	"record_package_artifact", "create_hotfix", "create_test_scenario", "revise_test_scenario", "record_scenario_run",
	"refresh_integration_git", "refresh_repository_git", "refresh_environment_runtime", "record_runtime_observation",
	"record_integration_revision", "plan_composition", "select_composition", "create_feature", "update_feature", "create_integration", "update_integration", "start_integration", "transition_integration", "complete_integration", "record_progress", "record_decision", "record_discovery", "add_blocker", "resolve_blocker", "handoff", "create_gate", "add_check", "record_check_result", "record_finding", "resolve_finding",
}
var memberDeployActions = []string{"deploy_integration", "deploy_existing_artifact", "assemble_environment", "reconcile_composition"}

func authorizeCommand(ctx context.Context, st domain.State, c domain.Command) error {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return nil
	}
	if p.Kind != "human" && slices.Contains([]string{"prepare_release_candidate", "promote_release_candidate", "plan_release", "record_release"}, c.Action) {
		return auth.ErrForbidden
	}
	if instanceAdmin(p) {
		return nil
	}
	permission := "admin"
	if slices.Contains(memberWriteActions, c.Action) {
		permission = "write"
	}
	if slices.Contains(memberDeployActions, c.Action) {
		permission = "deploy"
	}
	if err := authorize(ctx, permission, c.ProductID); err != nil {
		return err
	}
	rows, err := stateRows(st)
	if err != nil {
		return err
	}
	owners := map[string]string{}
	entities := map[string]map[string]any{}
	production := map[string]bool{}
	for kind, group := range rows {
		for _, row := range group {
			key, _ := row["id"].(string)
			if key == "" {
				continue
			}
			owners[key] = rowProduct(kind, row)
			entities[key] = row
			if kind == "environments" {
				purpose, _ := row["kind"].(string)
				production[key] = production[key] || strings.EqualFold(purpose, "PROD") || strings.EqualFold(purpose, "production")
			}
			if kind == "environment_bindings" && row["purpose"] == "PROD" {
				env, _ := row["environment_id"].(string)
				production[env] = true
			}
		}
	}
	// Resolve every persisted reference, including nested arrays/objects. Checking
	// only Command.ProductID permits IDs in data to bypass ownership checks.
	matched := c.ProductID != "" && allowedProduct(p, c.ProductID)
	for _, ref := range []string{c.ID, c.FeatureID, c.IntegrationID, c.GateID, c.CheckID, c.ResultID} {
		if owner, exists := owners[ref]; exists && allowedProduct(p, owner) {
			matched = true
		}
	}
	visited := map[string]bool{}
	var inspect func(any) error
	inspect = func(v any) error {
		switch v := v.(type) {
		case string:
			if owner, found := owners[v]; found {
				if !allowedProduct(p, owner) || production[v] {
					return auth.ErrForbidden
				}
				if !visited[v] {
					visited[v] = true
					for _, key := range []string{"environment_id", "composition_id", "candidate_operation_id", "operation_id", "parent_id"} {
						if target, ok := entities[v][key].(string); ok && target != "" {
							if err := inspect(target); err != nil {
								return err
							}
						}
					}
				}
			}
		case []any:
			for _, item := range v {
				if err := inspect(item); err != nil {
					return err
				}
			}
		case map[string]any:
			for key, item := range v {
				if key == "actor" {
					continue
				}
				if key == "approve_main_update" && item == true {
					return auth.ErrForbidden
				}
				if key == "status" && item == "released" {
					return auth.ErrForbidden
				}
				if err := inspect(item); err != nil {
					return err
				}
			}
		}
		return nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	var data any
	if err = json.Unmarshal(b, &data); err != nil {
		return err
	}
	if err = inspect(data); err != nil {
		return err
	}
	if !matched {
		return auth.ErrForbidden
	}
	return nil
}

func (s *Service) authorizeReferences(ctx context.Context, permission string, ids ...string) error {
	if err := authorize(ctx, permission, ""); err != nil {
		return err
	}
	p, ok := auth.FromContext(ctx)
	if !ok || instanceAdmin(p) {
		return nil
	}
	st, err := s.store.Read(ctx)
	if err != nil {
		return err
	}
	rows, err := stateRows(st)
	if err != nil {
		return err
	}
	for _, id := range ids {
		found := false
		for kind, group := range rows {
			for _, row := range group {
				if row["id"] == id {
					found = true
					if !allowedProduct(p, rowProduct(kind, row)) {
						return auth.ErrForbidden
					}
				}
			}
		}
		if !found {
			return auth.ErrForbidden
		}
	}
	return nil
}
