package domain

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// Build a second independently linked accepted job/attempt, rather than an
// unreferenced inventory row, so the equal-time case is valid persisted state.
func appendRegistryAcceptedObservation(t *testing.T, st *State, inventoryID string, graph RegistryGraph, received time.Time) {
	t.Helper()
	for i := range st.Repositories {
		st.Repositories[i].Role = "APPLICATION"
	}
	for i := range st.Applications {
		st.Applications[i].Kind = "APPLICATION"
	}
	raw, e := json.Marshal(st)
	if e != nil {
		t.Fatal(e)
	}
	var copy State
	if e = json.Unmarshal(raw, &copy); e != nil {
		t.Fatal(e)
	}
	v, j, a, op := copy.RegistryInventories[0], copy.WorkerJobs[0], copy.WorkerAttempts[0], copy.Operations[0]
	v.ID = inventoryID
	v.JobID = "job-" + inventoryID
	v.AttemptID = "attempt-" + inventoryID
	v.ReceivedAt = received
	v.CreatedAt = received
	v.UpdatedAt = received
	graph, e = NormalizeRegistryGraph(graph)
	if e != nil {
		t.Fatal(e)
	}
	graph.GraphHash = RegistryGraphHash(graph)
	v.Report.Graph = graph
	j.ID = v.JobID
	j.OperationID = "operation-" + inventoryID
	j.Inputs = v.Inputs
	j.InputHash = checkHash(j.Inputs)
	a.ID = v.AttemptID
	a.JobID = j.ID
	a.InputHash = j.InputHash
	a.UpdatedAt = received
	report := *a.Report
	report.JobID = j.ID
	report.AttemptID = a.ID
	report.Registry = &v.Report
	a.Report = &report
	op.ID = j.OperationID
	op.RegistryObservation = &j.Inputs
	st.WorkerJobs = append(st.WorkerJobs, j)
	st.WorkerAttempts = append(st.WorkerAttempts, a)
	st.Operations = append(st.Operations, op)
	st.RegistryInventories = append(st.RegistryInventories, v)
	if e = ValidateStateStatuses(*st); e != nil {
		t.Fatal("second admission must be valid persisted state", e)
	}
}

func TestRegistryInventoryEqualAcceptanceTimeDoesNotOrderContradictoryObservations(t *testing.T) {
	for _, mode := range []string{"single", "equivalent", "complete-id-first", "partial-id-first", "later-partial", "later-complete"} {
		t.Run(mode, func(t *testing.T) {
			st, a := registryDomainFixture(t)
			old := st.RegistryInventories[0]
			if mode != "single" {
				id := "z-second"
				st.RegistryInventories[0].ID = "a-first"
				if mode == "partial-id-first" {
					id = "a-second"
					st.RegistryInventories[0].ID = "z-first"
				}
				graph := old.Report.Graph
				received := old.ReceivedAt
				if mode != "equivalent" && mode != "later-complete" {
					root := st.ArtifactPins[0].Digest
					graph = RegistryGraph{Nodes: []RegistryNode{{Digest: root, Kind: "UNKNOWN", Availability: "UNKNOWN"}}, Edges: []RegistryEdge{}, Roots: []RegistryRoot{{Digest: root, Closure: "UNKNOWN", Blockers: []string{"registry_missing"}}}, Discovery: "PARTIAL", TagSetHash: retentionSHA(702), Blockers: []string{"registry_global_inventory_unproven", "registry_missing"}}
				}
				if mode == "later-partial" || mode == "later-complete" {
					received = received.Add(time.Second)
				}
				appendRegistryAcceptedObservation(t, &st, id, graph, received)
			}
			first := EvaluateRetention(st, "p", retentionNow, []RegistryObservationAuthority{a})
			summary := registrySummaryForTest(t, first)
			want := "UNKNOWN"
			if mode == "single" || mode == "equivalent" || mode == "later-complete" {
				want = "CURRENT"
			}
			if mode == "later-partial" {
				want = "PARTIAL"
			}
			if summary.Status != want {
				t.Fatalf("%s: status %s want %s", mode, summary.Status, want)
			}
			if want == "UNKNOWN" && !retentionHasCode(summary.Blockers, "registry_observation_order_ambiguous") {
				t.Fatal("missing explicit order ambiguity")
			}
			if len(first.ProtectedObjects) != 7 || len(first.ProtectedEdges) != 10 || first.DeletionAllowed || first.GlobalInventoryComplete {
				t.Fatal("coverage ambiguity changed protected history or authority")
			}
			if len(st.RegistryInventories) > 1 {
				// Freeze the value before rearranging the backing slice: summary
				// timestamps are optional pointers into the evaluated snapshot.
				snapshot, err := json.Marshal(first)
				if err != nil {
					t.Fatal(err)
				}
				var frozen RetentionReport
				if err = json.Unmarshal(snapshot, &frozen); err != nil {
					t.Fatal(err)
				}
				first = frozen
				st.RegistryInventories[0], st.RegistryInventories[1] = st.RegistryInventories[1], st.RegistryInventories[0]
				reordered := EvaluateRetention(st, "p", retentionNow, []RegistryObservationAuthority{a})
				if !reflect.DeepEqual(first, reordered) {
					t.Fatal("inventory permutation changed canonical report")
				}
			}
		})
	}
}

func TestRegistryInventoryRemovedAuthorityVersionHistoryIsCanonical(t *testing.T) {
	for _, sameTime := range []bool{false, true} {
		t.Run(map[bool]string{false: "later-version", true: "co-timed-versions"}[sameTime], func(t *testing.T) {
			st, authority := registryDomainFixture(t)
			first := st.RegistryInventories[0]
			received := first.ReceivedAt
			if !sameTime {
				received = received.Add(time.Second)
			}
			appendRegistryAcceptedObservation(t, &st, "second-version", first.Report.Graph, received)
			// Repin the second complete admission to a different immutable operator
			// recipe/config, preserving exact job/attempt/operation/report references.
			v := &st.RegistryInventories[1]
			j := &st.WorkerJobs[1]
			a := &st.WorkerAttempts[1]
			op := &st.Operations[1]
			v.Inputs.RecipeRevision = "v2"
			v.Inputs.RecipeHash = retentionSHA(888)
			v.Inputs.ConfigRevision = "cfg2"
			in := *v.Inputs.Registry
			in.RootSetHash = RegistryRootSetHash(in.Scope, v.Inputs.RecipeID, v.Inputs.RecipeRevision, v.Inputs.RecipeHash, v.Inputs.ConfigRevision, in.Roots)
			v.Inputs.Registry = &in
			v.Report.RecipeRevision = v.Inputs.RecipeRevision
			v.Report.RecipeHash = v.Inputs.RecipeHash
			v.Report.ConfigRevision = v.Inputs.ConfigRevision
			v.Report.RootSetHash = in.RootSetHash
			v.Report.Graph.TagSetHash = retentionSHA(889)
			v.Report.Graph.GraphHash = RegistryGraphHash(v.Report.Graph)
			j.Inputs = v.Inputs
			j.InputHash = checkHash(j.Inputs)
			v.InputHash = j.InputHash
			a.InputHash = j.InputHash
			report := *a.Report
			report.InputHash = j.InputHash
			registryReport := v.Report
			report.Registry = &registryReport
			a.Report = &report
			op.RegistryObservation = &j.Inputs
			if e := ValidateStateStatuses(st); e != nil {
				t.Fatal("two version admissions must both be valid", e)
			}
			before := EvaluateRetention(st, "p", retentionNow, nil)
			summary := registrySummaryForTest(t, before)
			if summary.Status != "AUTHORITY_UNKNOWN" || len(before.ProtectedObjects) != 7 || len(before.ProtectedEdges) != 10 || before.DeletionAllowed || before.GlobalInventoryComplete {
				t.Fatal("removed authority changed protection or gained confidence", summary)
			}
			// Freeze through JSON rather than retain optional timestamp pointers into
			// the backing snapshot that will be permuted.
			frozen, e := json.Marshal(before)
			if e != nil {
				t.Fatal(e)
			}
			st.RegistryInventories[0], st.RegistryInventories[1] = st.RegistryInventories[1], st.RegistryInventories[0]
			after := EvaluateRetention(st, "p", retentionNow, nil)
			actual, e := json.Marshal(after)
			if e != nil {
				t.Fatal(e)
			}
			if string(frozen) != string(actual) {
				t.Fatal("removed authority selected recipe/version from inventory array order")
			}
			// An explicit current authority still selects only its exact immutable pins.
			current := registrySummaryForTest(t, EvaluateRetention(st, "p", retentionNow, []RegistryObservationAuthority{authority}))
			if current.Status != "CURRENT" || current.ConfigRevision != authority.ConfigRevision {
				t.Fatal("historical fallback displaced current authority", current)
			}
		})
	}
}
