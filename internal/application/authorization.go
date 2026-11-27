package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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

// principalUser identifies the human whose work an authenticated agent may take.
// Missing principals are trusted local mode; services and admins are instance-wide.
func principalUser(ctx context.Context) (user string, enforced bool) {
	p, ok := auth.FromContext(ctx)
	if !ok || p.Kind == "service" || p.Role == "admin" && slices.Contains(p.Permissions, "admin") {
		return "", false
	}
	if p.Kind == "agent" {
		if p.Owner == "" {
			return "", true
		}
		return p.Owner, true
	}
	if p.Kind == "human" {
		return p.ID, true
	}
	return "", false
}
func allowedProduct(p auth.Principal, product string) bool {
	return product != "" && (slices.Contains(p.Products, "*") || slices.Contains(p.Products, product))
}
func bootstrapPrincipal(ctx context.Context) bool {
	p, ok := auth.FromContext(ctx)
	return ok && p.Kind == "service" && p.ID == "service/bootstrap"
}
func productVisibleTo(ctx context.Context, product *domain.Product) bool {
	return product == nil || auth.ProductVisible(ctx, product.Visibility, product.Owner, product.Members)
}
func productByID(st domain.State, id string) *domain.Product {
	for i := range st.Products {
		if st.Products[i].ID == id {
			return &st.Products[i]
		}
	}
	return nil
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
	if !ok || bootstrapPrincipal(ctx) && instanceAdmin(p) {
		return st, nil
	}
	return filterScopedStateForContext(ctx, st, p)
}

// filterScopedState shallow-copies each collection before filtering so its
// returned slices never alias Store's cached state. State currently consists
// only of collections. The former JSON decode into map[string][] rejected any
// encoded non-null non-array field; omitted/null fields decoded as zero. Keep
// that behavior for future fields rather than silently exposing them.
func filterScopedState(st domain.State, p auth.Principal) (domain.State, error) {
	return filterScopedStateForContext(auth.WithPrincipal(context.Background(), p), st, p)
}
func filterScopedStateForContext(ctx context.Context, st domain.State, p auth.Principal) (domain.State, error) {
	out := st
	sv := reflect.ValueOf(st)
	ov := reflect.ValueOf(&out).Elem()
	visibleProducts := map[string]bool{}
	for i := range st.Products {
		pr := &st.Products[i]
		if (allowedProduct(p, pr.ID) || instanceAdmin(p)) && productVisibleTo(ctx, pr) {
			visibleProducts[pr.ID] = true
		}
	}
	featureProducts := map[string]string{}
	for _, feature := range st.Features {
		featureProducts[feature.ID] = feature.ProductID
	}
	visibleEdges := map[string]bool{}
	for _, edge := range st.FeatureDependencies {
		visibleEdges[edge.ID] = visibleProducts[featureProducts[edge.FromFeatureID]] && visibleProducts[featureProducts[edge.ToFeatureID]]
	}
	workEventOwners := map[string]string{}
	for _, event := range st.ExecutionEvents {
		if event.Type == "work_available" {
			workEventOwners[event.ID] = event.Owner
		}
	}
	for i := 0; i < sv.NumField(); i++ {
		field := sv.Field(i)
		dst := ov.Field(i)
		if field.Kind() != reflect.Slice {
			meta := sv.Type().Field(i)
			jsonTag := strings.Split(meta.Tag.Get("json"), ",")
			if jsonTag[0] == "-" || (slices.Contains(jsonTag[1:], "omitempty") && field.IsZero()) {
				continue
			}
			b, err := json.Marshal(field.Interface())
			if err != nil {
				return domain.State{}, err
			}
			if string(b) != "null" {
				return domain.State{}, fmt.Errorf("json: cannot unmarshal value into Go struct field State.%s of type []map[string]interface {}", meta.Name)
			}
			continue
		}
		// Two passes: size the result by matching rows only, so a reader of a small
		// Product never holds an array sized for the whole instance journal.
		isProducts := sv.Type().Field(i).Name == "Products"
		isEvents := sv.Type().Field(i).Name == "Events"
		keep := func(row reflect.Value) bool {
			switch item := row.Interface().(type) {
			case domain.FeatureDependency:
				return visibleEdges[item.ID]
			case domain.UserOrchestrator:
				return false
			case domain.ExecutionEvent:
				if item.Type == "work_available" && item.Owner != workUser(ctx) {
					return false
				}
			case domain.Event:
				if item.Action == "propose_dependency" || item.Action == "decide_dependency" {
					return visibleEdges[item.EntityID]
				}
				if owner := workEventOwners[item.EntityID]; owner != "" && owner != workUser(ctx) {
					return false
				}
			}
			product, found := stateRowProduct(row, isProducts)
			if !found {
				return false
			}
			if product == "" && instanceAdmin(p) {
				return !isEvents || !row.Interface().(domain.Event).HasUnscopedProductEvidence()
			}
			return visibleProducts[product]
		}
		matches := 0
		for j := 0; j < field.Len(); j++ {
			if keep(field.Index(j)) {
				matches++
			}
		}
		filtered := reflect.MakeSlice(field.Type(), 0, matches)
		for j := 0; j < field.Len() && filtered.Len() < matches; j++ {
			if row := field.Index(j); keep(row) {
				filtered = reflect.Append(filtered, row)
			}
		}
		dst.Set(filtered)
	}
	return out, nil
}

func stateRowProduct(row reflect.Value, products bool) (string, bool) {
	for row.Kind() == reflect.Pointer || row.Kind() == reflect.Interface {
		if row.IsNil() {
			return "", false
		}
		row = row.Elem()
	}
	if row.Kind() != reflect.Struct {
		return "", false
	}
	var find func(reflect.Value) (string, bool)
	find = func(v reflect.Value) (string, bool) {
		for n := 0; n < v.NumField(); n++ {
			f := v.Field(n)
			meta := v.Type().Field(n)
			tag := strings.Split(meta.Tag.Get("json"), ",")[0]
			if (products && tag == "id") || (!products && tag == "product_id") {
				if f.Kind() == reflect.String {
					return f.String(), true
				}
				return "", false
			}
			if meta.Anonymous {
				for f.Kind() == reflect.Pointer {
					if f.IsNil() {
						break
					}
					f = f.Elem()
				}
				if f.Kind() == reflect.Struct {
					if value, ok := find(f); ok {
						return value, true
					}
				}
			}
		}
		return "", false
	}
	return find(row)
}

// This allowlist deliberately fails closed for newly introduced commands.
var memberWriteActions = []string{
	"create_product", "adopt_product_ownership", "set_product_visibility", "add_product_member", "remove_product_member",
	"claim_execution", "heartbeat_execution", "report_execution", "release_execution", "takeover_execution", "set_desired_state", "create_execution_group", "add_execution_group_member", "remove_execution_group_member", "set_execution_group_harness", "set_product_stalled_timeout", "report_turn_end", "claim_execution_event_delivery", "ack_execution_event", "resolve_execution_event", "assign_integration", "unassign_integration",
	"queue_intelligence_task", "cancel_intelligence_task",
	"create_integration_branch", "protect_integration_revision", "claim_composition_conflict", "record_conflict_resolution", "verify_conflict_resolution",
	"record_package_artifact", "create_hotfix", "create_test_scenario", "revise_test_scenario", "record_scenario_run",
	"refresh_integration_git", "refresh_repository_git", "refresh_environment_runtime", "record_runtime_observation",
	"record_integration_revision", "plan_composition", "select_composition", "create_feature", "update_feature", "create_integration", "update_integration", "start_integration", "transition_integration", "complete_integration", "record_progress", "record_decision", "record_discovery", "add_blocker", "resolve_blocker", "handoff", "create_gate", "add_check", "record_check_result", "record_finding", "resolve_finding",
}
var memberDeployActions = []string{"deploy_integration", "deploy_existing_artifact", "assemble_environment", "reconcile_composition"}

func authorizeCommand(ctx context.Context, st domain.State, c domain.Command, fullSnapshot bool) error {
	if slices.Contains(domain.WorkActions, c.Action) {
		return nil
	}
	p, ok := auth.FromContext(ctx)
	if !ok {
		return nil
	}
	// Resolve the command's product scope before permission checks so an
	// invisible private Product is indistinguishable from a missing one.
	productID := c.ProductID
	if productID == "" {
		for i := range st.Features {
			if st.Features[i].ID == c.FeatureID {
				productID = st.Features[i].ProductID
				break
			}
		}
		for i := range st.Integrations {
			if st.Integrations[i].ID == c.IntegrationID {
				productID = st.Integrations[i].ProductID
				break
			}
		}
	}
	if pr := productByID(st, productID); pr != nil && !productVisibleTo(ctx, pr) {
		return ErrNotFound
	}
	if c.Action == "create_product" {
		if pr := productByID(st, c.ID); pr != nil && !productVisibleTo(ctx, pr) {
			return ErrNotFound
		}
		if !slices.Contains(p.Products, "*") && !allowedProduct(p, c.ID) {
			return auth.ErrForbidden
		}
	}
	operatorOnly := slices.Contains([]string{"prepare_release_candidate", "promote_release_candidate", "plan_release", "record_release"}, c.Action)
	if bootstrapPrincipal(ctx) && instanceAdmin(p) {
		if operatorOnly {
			return auth.ErrForbidden
		}
		return nil
	}
	refs, restrictedFields, err := commandAuthorizationInputs(c)
	if err != nil {
		return err
	}
	projected, needed := projectReferences(st, refs)
	if fullSnapshot {
		if err := requireVisibleReferences(ctx, st, projected, needed); err != nil {
			return err
		}
	}
	if p.Kind != "human" && operatorOnly {
		return auth.ErrForbidden
	}
	if instanceAdmin(p) {
		return nil
	}
	if restrictedFields {
		return auth.ErrForbidden
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
	rows, err := stateRows(projected)
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
	matched := c.Action == "create_product" || c.ProductID != "" && allowedProduct(p, c.ProductID)
	for _, ref := range []string{c.ID, c.FeatureID, c.IntegrationID, c.GateID, c.CheckID, c.ResultID} {
		if owner, exists := owners[ref]; exists && allowedProduct(p, owner) {
			matched = true
		}
	}
	visited := map[string]bool{}
	var inspect func(string) error
	inspect = func(ref string) error {
		if owner, found := owners[ref]; found && !visited[ref] {
			if !allowedProduct(p, owner) || production[ref] {
				return auth.ErrForbidden
			}
			visited[ref] = true
			for _, key := range []string{"environment_id", "composition_id", "candidate_operation_id", "operation_id", "parent_id"} {
				if target, ok := entities[ref][key].(string); ok && target != "" {
					if err := inspect(target); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	for _, ref := range refs {
		if err = inspect(ref); err != nil {
			return err
		}
	}
	if !matched {
		return auth.ErrForbidden
	}
	return nil
}

func (s *Service) authorizeReferences(ctx context.Context, permission string, ids ...string) error {
	p, ok := auth.FromContext(ctx)
	if !ok || bootstrapPrincipal(ctx) {
		return authorize(ctx, permission, "")
	}
	var st domain.State
	var err error
	if reader, ok := s.store.(referenceReader); ok {
		st, err = reader.ReadReferences(ctx, ids)
	} else {
		err = s.view(ctx, func(raw *domain.State) error {
			var needed map[string]bool
			st, needed = projectReferences(*raw, ids)
			return requireVisibleReferences(ctx, *raw, st, needed)
		})
	}
	if errors.Is(err, domain.ErrReadScopeNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := authorize(ctx, permission, ""); err != nil {
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
					if !instanceAdmin(p) && !allowedProduct(p, rowProduct(kind, row)) {
						return auth.ErrForbidden
					}
				}
			}
		}
		if !found {
			return ErrNotFound
		}
	}
	return nil
}

func requireVisibleReferences(ctx context.Context, raw, projected domain.State, needed map[string]bool) error {
	value := reflect.ValueOf(projected)
	for index, path := range stateIDPaths {
		rows := value.Field(index)
		name := value.Type().Field(index).Name
		for rowIndex := 0; rowIndex < rows.Len(); rowIndex++ {
			row := rows.Index(rowIndex)
			if !needed[row.FieldByIndex(path).String()] {
				continue
			}
			if name == "Events" && row.Interface().(domain.Event).HasUnscopedProductEvidence() {
				return ErrNotFound
			}
			if product, _ := stateRowProduct(row, name == "Products"); product != "" {
				if err := requireProductRead(ctx, &raw, product); err != nil {
					return ErrNotFound
				}
			}
		}
	}
	return nil
}
