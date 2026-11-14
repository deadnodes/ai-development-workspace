package domain

import (
	"reflect"
	"releasecontrol/internal/delivery"
	"time"
)

// TerminalOperation is shared by execution and retention.
func TerminalOperation(status string) bool {
	switch status {
	case "GITOPS_APPLIED", "SUCCEEDED", "FAILED", "BLOCKED", "CANCELLED":
		return true
	}
	return false
}

func operationRetentionTime(o ExternalOperation) time.Time {
	at := o.UpdatedAt
	if o.FinishedAt != nil && o.FinishedAt.After(at) {
		at = *o.FinishedAt
	}
	return at
}

// operationRetentionRoots deliberately recognizes exact ID values in every
// retained collection, including opaque maps and nested frozen snapshots. This
// conservative inventory also covers references whose names are not operation_id
// (delivery IDs, parent/child links, selections and restoration lineage). Events
// are history, not live roots. Steps are owned children: their owner link alone
// does not protect an operation. No other provenance collection is disposable.
func operationRetentionRoots(st *State) map[string]bool {
	ids, keep := map[string]bool{}, map[string]bool{}
	type target struct{ product, kind, integration, repository, environment, application string }
	latest := map[target]ExternalOperation{}
	for _, o := range st.Operations {
		ids[o.ID] = true
		k := target{o.ProductID, o.Kind, o.IntegrationID, o.RepositoryID, o.EnvironmentID, o.ApplicationID}
		old, ok := latest[k]
		if !ok || o.CreatedAt.After(old.CreatedAt) || o.CreatedAt.Equal(old.CreatedAt) && o.ID > old.ID {
			latest[k] = o
		}
		// Request keys are durable replay receipts, even without an incoming edge.
		if o.RequestKey != "" {
			keep[o.ID] = true
		}
		// These records are durable recovery proofs, even without an incoming edge.
		if IsRestorationOperation(o) || o.DevBaseline != nil || o.DevBaselineResult != nil {
			keep[o.ID] = true
		}
	}
	for _, o := range latest {
		keep[o.ID] = true
	}
	var visit func(reflect.Value, string)
	visit = func(v reflect.Value, owner string) {
		switch v.Kind() {
		case reflect.Interface, reflect.Pointer:
			if !v.IsNil() {
				visit(v.Elem(), owner)
			}
		case reflect.String:
			s := v.String()
			if s != owner && ids[s] {
				keep[s] = true
			}
		case reflect.Struct:
			// Frozen request keys and provider dispatch correlations are receipts
			// too. Preserve them even if no separate record refers to the owner.
			if request, ok := v.Interface().(delivery.BuildRequest); ok && request.OperationID != "" && owner != "" {
				keep[owner] = true
			}
			for i := 0; i < v.NumField(); i++ {
				if v.Type().Field(i).PkgPath == "" {
					field := v.Type().Field(i)
					value := v.Field(i)
					if owner != "" && (field.Name == "RequestKey" || field.Name == "IdempotencyKey") && value.Kind() == reflect.String && value.String() != "" {
						keep[owner] = true
					}
					visit(v.Field(i), owner)
				}
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < v.Len(); i++ {
				visit(v.Index(i), owner)
			}
		case reflect.Map:
			it := v.MapRange()
			for it.Next() {
				visit(it.Key(), owner)
				visit(it.Value(), owner)
			}
		}
	}
	v := reflect.ValueOf(st).Elem()
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		if name == "Events" {
			continue
		}
		rows := v.Field(i)
		for j := 0; j < rows.Len(); j++ {
			owner := ""
			if name == "Operations" {
				owner = st.Operations[j].ID
			}
			if name == "OperationSteps" {
				owner = st.OperationSteps[j].OperationID
			}
			visit(rows.Index(j), owner)
		}
	}
	return keep
}

// PrunableOperations plans a bounded batch in stored order in O(state + refs).
func PrunableOperations(st *State, cutoff, refreshCutoff time.Time, limit int) map[string]bool {
	return operationCandidates(st, nil, cutoff, refreshCutoff, limit)
}

// RevalidateOperationCandidates cannot expand the borrowed-view batch.
func RevalidateOperationCandidates(st *State, candidates map[string]bool, cutoff, refreshCutoff time.Time) map[string]bool {
	return operationCandidates(st, candidates, cutoff, refreshCutoff, len(candidates))
}
func operationCandidates(st *State, candidates map[string]bool, cutoff, refreshCutoff time.Time, limit int) map[string]bool {
	keep := operationRetentionRoots(st)
	out := map[string]bool{}
	for _, o := range st.Operations {
		if len(out) >= limit {
			break
		}
		at := operationRetentionTime(o)
		operationCutoff := cutoff
		if o.Kind == "REFRESH_GIT" || o.Kind == "REFRESH_REPOSITORY_GIT" {
			operationCutoff = refreshCutoff
		}
		if o.ID != "" && (candidates == nil || candidates[o.ID]) && !keep[o.ID] && TerminalOperation(o.Status) && !at.IsZero() && at.Before(operationCutoff) {
			out[o.ID] = true
		}
	}
	return out
}
