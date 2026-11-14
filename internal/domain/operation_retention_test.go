package domain

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func operationRetentionFixture() (State, time.Time) {
	cutoff := time.Now().UTC().Add(-720 * time.Hour)
	st := EmptyState()
	for i, status := range []string{"FAILED", "SUCCEEDED", "CANCELLED", "BLOCKED", "GITOPS_APPLIED", "RUNNING", "PENDING", "FAILED", "FAILED"} {
		at := cutoff.Add(-time.Hour)
		if i == 7 {
			at = cutoff
		}
		st.Operations = append(st.Operations, ExternalOperation{Meta: Meta{ID: fmt.Sprint("op-", i), CreatedAt: at, UpdatedAt: at}, Kind: "REFRESH_GIT", Status: status})
	}
	st.Operations[8].CreatedAt = cutoff.Add(time.Hour)
	return st, cutoff
}
func TestOperationRetentionEligibility(t *testing.T) {
	st, cutoff := operationRetentionFixture()
	got := PrunableOperations(&st, cutoff, cutoff, 5000)
	if len(got) != 5 {
		t.Fatalf("candidates: %v", got)
	}
	for i := 0; i < 5; i++ {
		if !got[fmt.Sprint("op-", i)] {
			t.Fatal(got)
		}
	}
	if len(PrunableOperations(&st, cutoff, cutoff, 2)) != 2 {
		t.Fatal("batch cap")
	}
	st.Operations[0].FinishedAt = new(time.Time)
	*st.Operations[0].FinishedAt = cutoff
	if PrunableOperations(&st, cutoff, cutoff, 5000)["op-0"] {
		t.Fatal("recent finish")
	}
}

// Exercise every State collection and nested reference family. Exact ID values
// in unknown/opaque fields are conservative roots too, avoiding a fragile list
// of operation_id aliases in the implementation.
func TestOperationRetentionReferences(t *testing.T) {
	fixtures := []string{
		`{"features":[{"id":"feature","context":"op-0"}]}`,
		`{"integrations":[{"id":"integration","objective":"op-0"}]}`,
		`{"environments":[{"desired_operation_id":"op-0"}]}`,
		`{"releases":[{"execution_operation_id":"op-0"}]}`,
		`{"runtime_observations":[{"operation_id":"op-0"}]}`,
		`{"delivery_artifacts":[{"operation_id":"op-0"}]}`,
		`{"delivery_artifacts":[{"build_request":{"operation_id":"op-0"}}]}`,
		`{"delivery_build_runs":[{"operation_id":"op-0"}]}`,
		`{"worker_jobs":[{"operation_id":"op-0"}]}`,

		`{"operation_steps":[{"operation_id":"other","evidence":{"delivery":"op-0"}}]}`,
	}
	// All collections are scanned, regardless of their evolving reference schema.
	typ := reflect.TypeOf(State{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Tag.Get("json")
		if name == "events" || name == "operations" || name == "operation_steps" {
			continue
		}
		fixtures = append(fixtures, fmt.Sprintf(`{"%s":[{"id":"op-0"}]}`, name))
	}
	// Some explicit fields above may evolve: assert actual deserialized references
	// exist so a misspelled fixture can never silently claim coverage.
	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			st, cutoff := operationRetentionFixture()
			if err := json.Unmarshal([]byte(fixture), &st); err != nil {
				t.Fatal(err)
			}
			if PrunableOperations(&st, cutoff, cutoff, 5000)["op-0"] {
				t.Fatal("reference unprotected")
			}
		})
	}
}
func TestOperationRetentionLinksAndRevalidation(t *testing.T) {
	for _, change := range []string{"reference", "running", "recent", "missing", "latest", "child", "parent", "source"} {
		t.Run(change, func(t *testing.T) {
			st, cutoff := operationRetentionFixture()
			batch := map[string]bool{"op-0": true}
			switch change {
			case "reference":
				st.RuntimeObservations = []RuntimeObservation{{OperationID: "op-0"}}
			case "running":
				st.Operations[0].Status = "RUNNING"
			case "recent":
				st.Operations[0].UpdatedAt = cutoff
			case "missing":
				st.Operations = st.Operations[1:]
			case "latest":
				st.Operations = st.Operations[:1]
			case "child":
				st.Operations[8].ChildIDs = []string{"op-0"}
			case "parent":
				st.Operations[8].ParentID = "op-0"
			case "source":
				st.Operations[8].SourceRetentionParentID = "op-0"
			}
			if len(RevalidateOperationCandidates(&st, batch, cutoff, cutoff)) != 0 {
				t.Fatal("candidate not revalidated")
			}
		})
	}
	st, cutoff := operationRetentionFixture()
	st.OperationSteps = []OperationStep{{OperationID: "op-0"}}
	st.Events = []Event{{EntityID: "op-0"}}
	if !PrunableOperations(&st, cutoff, cutoff, 5000)["op-0"] {
		t.Fatal("child or journal pinned owner")
	}
	if len(RevalidateOperationCandidates(&st, map[string]bool{"op-0": true}, cutoff, cutoff)) != 1 {
		t.Fatal("batch expanded")
	}
}

// Discover typed operation references across all State collections rather than
// allowing newly added nested reference types to escape the test inventory.
func TestOperationRetentionTypedReferenceInventory(t *testing.T) {
	typ := reflect.TypeOf(State{})
	tested := 0
	for i := 0; i < typ.NumField(); i++ {
		collection := typ.Field(i).Name
		if collection == "Events" || collection == "OperationSteps" {
			continue
		}
		st, cutoff := operationRetentionFixture()
		rows := reflect.ValueOf(&st).Elem().Field(i)
		var row reflect.Value
		if collection == "Operations" {
			row = rows.Index(rows.Len() - 1)
		} else {
			rows.Set(reflect.MakeSlice(rows.Type(), 1, 1))
			row = rows.Index(0)
		}
		var walk func(reflect.Value, string, int)
		walk = func(v reflect.Value, path string, depth int) {
			if depth > 16 {
				return
			}
			switch v.Kind() {
			case reflect.Pointer:
				if v.Type().Elem().Kind() != reflect.Struct {
					return
				}
				v.Set(reflect.New(v.Type().Elem()))
				walk(v.Elem(), path, depth+1)
				v.SetZero()
			case reflect.Struct:
				for j := 0; j < v.NumField(); j++ {
					f := v.Type().Field(j)
					if f.PkgPath == "" {
						walk(v.Field(j), path+"."+f.Name, depth+1)
					}
				}
			case reflect.Slice:
				v.Set(reflect.MakeSlice(v.Type(), 1, 1))
				walk(v.Index(0), path+"[]", depth+1)
				v.SetZero()
			case reflect.String:
				if !(strings.Contains(path, "OperationID") || strings.HasSuffix(path, "DeliveryID") || strings.HasSuffix(path, "ParentID") || strings.HasSuffix(path, "ChildIDs[]") || strings.HasSuffix(path, "SourceRetentionOriginID")) {
					return
				}
				original := v.String()
				v.SetString("op-0")
				tested++
				if PrunableOperations(&st, cutoff, cutoff, 5000)["op-0"] {
					t.Errorf("unprotected typed reference %s", path)
				}
				v.SetString(original)
			}
		}
		walk(row, collection, 0)
	}
	if tested < 40 {
		t.Fatalf("unexpectedly small reference inventory: %d", tested)
	}
	t.Logf("protected %d typed operation reference paths", tested)
}

func TestOperationRetentionLatestTargets(t *testing.T) {
	st, cutoff := operationRetentionFixture()
	for _, change := range []func(*ExternalOperation){
		func(o *ExternalOperation) { o.Kind = "OTHER" },
		func(o *ExternalOperation) { o.ProductID = "other" },
		func(o *ExternalOperation) { o.IntegrationID = "integration" },
		func(o *ExternalOperation) { o.RepositoryID = "repository" },
		func(o *ExternalOperation) { o.EnvironmentID = "environment" },
		func(o *ExternalOperation) { o.ApplicationID = "application" },
	} {
		op := st.Operations[0]
		op.ID = fmt.Sprint("target-", len(st.Operations))
		change(&op)
		st.Operations = append(st.Operations, op)
	}
	if got := PrunableOperations(&st, cutoff, cutoff, 5000); len(got) != 5 {
		t.Fatalf("latest target removed: %v", got)
	}
}

func TestOperationRetentionReplayReceipts(t *testing.T) {
	for _, fixture := range []string{
		`{"request_key":"replay"}`,
		`{"request_key":" "}`,
		`{"feature_update":{"idempotency_key":"replay"}}`,
		`{"main_candidate":{"request":{"idempotency_key":"replay"}}}`,
		`{"dev_runtime":{"request":{"request_key":"replay"}}}`,
		`{"snapshot":{"build_request":{"operation_id":"op-0"}}}`,
		`{"snapshot":{"build_request":{"operation_id":"dispatch-request"}}}`,
		`{"prepared_deployments":[{"build_request":{"operation_id":"dispatch-request"}}]}`,
	} {
		t.Run(fixture, func(t *testing.T) {
			st, cutoff := operationRetentionFixture()
			if !PrunableOperations(&st, cutoff, cutoff, 5000)["op-0"] {
				t.Fatal("fixture must start prunable")
			}
			batch := map[string]bool{"op-0": true}
			if err := json.Unmarshal([]byte(fixture), &st.Operations[0]); err != nil {
				t.Fatal(err)
			}
			if PrunableOperations(&st, cutoff, cutoff, 5000)["op-0"] {
				t.Fatal("replay receipt selected for pruning")
			}
			if len(RevalidateOperationCandidates(&st, batch, cutoff, cutoff)) != 0 {
				t.Fatal("replay receipt not protected during revalidation")
			}
			if !PrunableOperations(&st, cutoff, cutoff, 5000)["op-1"] {
				t.Fatal("unkeyed operation unexpectedly protected")
			}
		})
	}
}

func TestOperationRetentionRefreshRevalidation(t *testing.T) {
	st, cutoff := operationRetentionFixture()
	// A refresh aged between the two windows is eligible only as a refresh.
	st.Operations[0].UpdatedAt = cutoff.Add(100 * time.Hour)
	refreshCutoff := cutoff.Add(552 * time.Hour)
	batch := PrunableOperations(&st, cutoff, refreshCutoff, 5000)
	if !batch["op-0"] {
		t.Fatal("refresh not selected")
	}
	for _, kind := range []string{"REFRESH_GIT", "REFRESH_REPOSITORY_GIT", "OTHER"} {
		st.Operations[0].Kind = kind
		// Keep a newer operation for the same kind so latest-target protection
		// does not mask which cutoff revalidation uses.
		st.Operations[8].Kind = kind
		got := RevalidateOperationCandidates(&st, batch, cutoff, refreshCutoff)
		if got["op-0"] != (kind != "OTHER") {
			t.Fatalf("kind=%s candidates=%v", kind, got)
		}
	}
}
