package application

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"

	"releasecontrol/internal/domain"
)

func commandReferenceIDs(c domain.Command) ([]string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	var value any
	if err = json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	var visit func(any)
	visit = func(v any) {
		switch v := v.(type) {
		case string:
			if v != "" {
				ids[v] = true
			}
		case []any:
			for _, item := range v {
				visit(item)
			}
		case map[string]any:
			for key, item := range v {
				if key != "actor" {
					visit(item)
				}
			}
		}
	}
	visit(value)
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
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

func authorizationProjection(st domain.State, c domain.Command) (domain.State, error) {
	ids, err := commandReferenceIDs(c)
	if err != nil {
		return domain.State{}, err
	}
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
				if selected[key] {
					continue
				}
				row := field.Index(j)
				id := row.FieldByIndex(path).String()
				if !bindings && !needed[id] {
					continue
				}
				selected[key] = true
				dst.Field(i).Set(reflect.Append(dst.Field(i), row))
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
	return out, nil
}

func (s commandAuthorizationStore) authorizePreflight(ctx context.Context) error {
	var st domain.State
	var err error
	if reader, ok := s.Store.(interface {
		ReadReferences(context.Context, []string) (domain.State, error)
	}); ok {
		var ids []string
		ids, err = commandReferenceIDs(s.command)
		if err == nil {
			st, err = reader.ReadReferences(ctx, ids)
		}
	} else {
		st, err = s.Store.Read(ctx)
	}
	if err != nil {
		return err
	}
	return authorizeCommand(ctx, st, s.command)
}
