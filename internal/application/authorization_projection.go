package application

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"

	"releasecontrol/internal/domain"
)

func commandReferenceIDs(c domain.Command) ([]string, error) {
	ids, _, err := commandAuthorizationInputs(c)
	return ids, err
}

func commandAuthorizationInputs(c domain.Command) ([]string, bool, error) {
	if c.Action == "upsert_agent_skill" {
		c.Data = nil
	}
	if c.Action == "update_feature" {
		c.Data = maps.Clone(c.Data)
		delete(c.Data, "approval_question_id")
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, false, err
	}
	var value any
	if err = json.Unmarshal(raw, &value); err != nil {
		return nil, false, err
	}
	ids := map[string]bool{}
	operatorOnly := false
	var visit func(string, any)
	visit = func(key string, v any) {
		if key == "approve_main_update" && v == true || key == "status" && v == "released" {
			operatorOnly = true
		}
		switch v := v.(type) {
		case string:
			if v != "" && (key == "id" || strings.HasSuffix(key, "_id") || strings.HasSuffix(key, "_ids") ||
				key == "depends_on" || key == "dependencies" || key == "repositories" || key == "waiting_ref" || key == "blocker_ref" || key == "deployment") {
				ids[v] = true
			}
		case []any:
			for _, item := range v {
				visit(key, item)
			}
		case map[string]any:
			for key, item := range v {
				if key != "actor" {
					visit(key, item)
				}
			}
		}
	}
	visit("", value)
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, operatorOnly, nil
}

// The authorization policy follows only these persisted edges. Preserve the
// same policy while serializing the referenced closure rather than all history.
var authorizationPaths = func() [][][]int {
	typ := reflect.TypeOf(domain.State{})
	out := make([][][]int, typ.NumField())
	for i := range out {
		for _, name := range []string{"EnvironmentID", "CompositionID", "CandidateOperationID", "OperationID", "ParentID"} {
			if field, ok := typ.Field(i).Type.Elem().FieldByName(name); ok && field.Type.Kind() == reflect.String {
				out[i] = append(out[i], field.Index)
			}
		}
	}
	return out
}()

func projectReferences(st domain.State, ids []string) (domain.State, map[string]bool) {
	needed := map[string]bool{}
	for _, id := range ids {
		needed[id] = true
	}
	out := domain.EmptyState()
	src, dst := reflect.ValueOf(st), reflect.ValueOf(&out).Elem()
	selected := map[[2]int]bool{}
	for more := true; more; {
		more = false
		for i, path := range stateIDPaths {
			field := src.Field(i)
			bindings := src.Type().Field(i).Name == "EnvironmentBindings"
			for j := 0; j < field.Len(); j++ {
				key := [2]int{i, j}
				row := field.Index(j)
				id := row.FieldByIndex(path).String()
				if !bindings && !needed[id] {
					continue
				}
				if !selected[key] {
					selected[key] = true
					dst.Field(i).Set(reflect.Append(dst.Field(i), row))
				}
				// Binding rows provide production metadata, not additional graph roots.
				if !needed[id] {
					continue
				}
				for _, edge := range authorizationPaths[i] {
					target := row.FieldByIndex(edge).String()
					if target != "" && !needed[target] {
						needed[target] = true
						more = true
					}
				}
			}
		}
	}
	return out, needed
}

type referenceReader interface {
	ReadReferences(context.Context, []string) (domain.State, error)
}

func (s commandAuthorizationStore) authorizePreflight(ctx context.Context) error {
	if slices.Contains(domain.WorkActions, s.command.Action) {
		return nil
	}
	var st domain.State
	var err error
	reader, projected := s.Store.(referenceReader)
	if projected {
		var ids []string
		ids, err = commandReferenceIDs(s.command)
		if err == nil {
			st, err = reader.ReadReferences(ctx, ids)
		}
	} else {
		st, err = s.Store.Read(ctx)
	}
	if errors.Is(err, domain.ErrReadScopeNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return authorizeCommand(ctx, st, s.command, !projected)
}
