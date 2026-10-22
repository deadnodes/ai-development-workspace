package domain

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func registryDomainFixture(t *testing.T) (State, RegistryObservationAuthority) {
	t.Helper()
	st := retentionBase()
	st.Repositories = []Repository{{Meta: Meta{ID: "repo", ProductID: "p"}, URL: "https://github.com/owner/repo"}}
	scope := RegistryScope{ProductID: "p", ComponentID: "app", RepositoryID: "repo", Origin: "https://registry.example", Repository: "team/app", ImageRepository: "registry.example/team/app"}
	limits := RegistryObserveLimits{MaxResponseBytes: 1 << 20, MaxTotalBytes: 8 << 20, MaxNodes: 64, MaxEdges: 128, MaxDepth: 16, MaxPages: 8, MaxRequests: 256, TimeoutSeconds: 900}
	a := RegistryObservationAuthority{Scope: scope, RecipeID: "observe", RecipeRevision: "v1", RecipeHash: retentionSHA(700), ConfigRevision: "cfg", Limits: limits, Current: true}
	root := retentionSHA(300)
	pin := ArtifactPin{Meta: Meta{ID: "pin", ProductID: "p", Actor: "operator", CreatedAt: retentionNow.Add(-time.Hour), UpdatedAt: retentionNow.Add(-time.Hour)}, ApplicationID: "app", ImageRepository: scope.ImageRepository, Digest: root, Purpose: "manual", RequestKey: "registry-root-pin"}
	pin.RequestHash = ArtifactPinRequestHash(pin.ProductID, pin.ApplicationID, pin.ImageRepository, pin.Digest, pin.Purpose, pin.RequestKey)
	st.ArtifactPins = []ArtifactPin{pin}
	in := WorkerInputs{RecipeID: a.RecipeID, RecipeRevision: a.RecipeRevision, RecipeHash: a.RecipeHash, ConfigRevision: a.ConfigRevision, RepositoryID: "repo", RepositoryURL: st.Repositories[0].URL, Trust: "registry_observation", Parameters: map[string]any{}, Registry: &WorkerRegistryInputs{Scope: scope, Roots: []string{root}, RootSetHash: RegistryRootSetHash(scope, a.RecipeID, a.RecipeRevision, a.RecipeHash, a.ConfigRevision, []string{root}), RequestRetentionInputHash: retentionSHA(701), Limits: limits}}
	graph := RegistryGraph{Nodes: []RegistryNode{}, Edges: []RegistryEdge{{From: root, To: retentionSHA(301), Kind: "CHILD", Platform: &RegistryPlatform{OS: "linux", Architecture: "arm64"}}, {From: root, To: retentionSHA(306), Kind: "CHILD", Platform: &RegistryPlatform{OS: "linux", Architecture: "amd64"}}, {From: retentionSHA(306), To: retentionSHA(302), Kind: "CHILD"}, {From: retentionSHA(301), To: retentionSHA(303), Kind: "CONFIG"}, {From: retentionSHA(302), To: retentionSHA(303), Kind: "CONFIG"}, {From: retentionSHA(301), To: retentionSHA(304), Kind: "LAYER"}, {From: retentionSHA(302), To: retentionSHA(304), Kind: "LAYER"}, {From: root, To: retentionSHA(305), Kind: "REFERRER"}, {From: retentionSHA(305), To: root, Kind: "SUBJECT"}, {From: retentionSHA(305), To: retentionSHA(303), Kind: "CONFIG"}}, Roots: []RegistryRoot{{Digest: root, Closure: "COMPLETE", Blockers: []string{}}}, TagSetHash: retentionSHA(702), Discovery: "COMPLETE", Blockers: []string{"registry_global_inventory_unproven"}}
	for n := 300; n <= 306; n++ {
		kind, media := "MANIFEST", "application/vnd.oci.image.manifest.v1+json"
		if n == 300 || n == 306 {
			kind = "INDEX"
			media = "application/vnd.oci.image.index.v1+json"
		}
		if n == 303 || n == 304 {
			kind = "BLOB"
			media = "application/octet-stream"
		}
		graph.Nodes = append(graph.Nodes, RegistryNode{Digest: retentionSHA(n), Kind: kind, MediaType: media, Size: 32, Availability: "PRESENT"})
	}
	var e error
	graph, e = NormalizeRegistryGraph(graph)
	if e != nil {
		t.Fatal(e)
	}
	graph.GraphHash = RegistryGraphHash(graph)
	received := retentionNow.Add(-5 * time.Second)
	j := WorkerJob{Meta: Meta{ID: "observe-job", ProductID: "p"}, OperationID: "observe-operation", Capability: "registry_observe", Inputs: in, InputHash: checkHash(in), Status: "SUCCEEDED", Fence: 1, Deadline: retentionNow.Add(time.Hour)}
	r := WorkerReport{WorkerAttemptRequest: WorkerAttemptRequest{JobID: j.ID, AttemptID: "observe-attempt", Fence: 1, InputHash: j.InputHash}, Result: "PASSED", Registry: &WorkerRegistryReport{Kind: RegistryObservationKind, Scope: scope, RecipeID: a.RecipeID, RecipeRevision: a.RecipeRevision, RecipeHash: a.RecipeHash, ConfigRevision: a.ConfigRevision, RootSetHash: in.Registry.RootSetHash, StartedAt: retentionNow.Add(-30 * time.Second), FinishedAt: retentionNow.Add(-10 * time.Second), Requests: 16, Bytes: 512, Graph: graph}}
	attempt := WorkerAttempt{Meta: Meta{ID: r.AttemptID, ProductID: "p", CreatedAt: retentionNow.Add(-time.Minute), UpdatedAt: received}, JobID: j.ID, Fence: 1, InputHash: j.InputHash, Status: "SUCCEEDED", Report: &r}
	st.WorkerJobs = []WorkerJob{j}
	st.WorkerAttempts = []WorkerAttempt{attempt}
	st.Operations = []ExternalOperation{{Meta: Meta{ID: j.OperationID, ProductID: "p"}, Kind: RegistryObserveOperationKind, Status: "SUCCEEDED", ApplicationID: "app", RepositoryID: "repo", RegistryObservation: &in}}
	st.RegistryInventories = []RegistryInventory{{Meta: Meta{ID: "inventory", ProductID: "p", CreatedAt: received, UpdatedAt: received}, JobID: j.ID, AttemptID: attempt.ID, Fence: 1, InputHash: j.InputHash, Inputs: in, Report: *r.Registry, ReceivedAt: received}}
	if e := ValidateRegistryInventories(st); e != nil {
		t.Fatal("fixture", e)
	}
	return st, a
}
func registrySummaryForTest(t *testing.T, r RetentionReport) RegistryCoverageSummary {
	t.Helper()
	if len(r.Registry) != 1 {
		t.Fatal(r.Registry)
	}
	return r.Registry[0]
}
func TestRegistryInventoryCurrentRootCoverageWithoutEvidenceHashEquality(t *testing.T) {
	st, a := registryDomainFixture(t)
	r := EvaluateRetention(st, "p", retentionNow, []RegistryObservationAuthority{a})
	summary := registrySummaryForTest(t, r)
	if summary.Status != "CURRENT" || summary.CoveredRoots != 1 || summary.RequiredRoots != 1 || r.GlobalInventoryComplete || r.DeletionAllowed || len(r.ProtectedObjects) != 7 || len(r.ProtectedEdges) != 10 {
		t.Fatal(r)
	}
	if r.InputHash == st.RegistryInventories[0].Inputs.Registry.RequestRetentionInputHash {
		t.Fatal("fixture requires distinct historical provenance")
	}
	// Child manifest already observed under the complete index requires no network fetch.
	pin := st.ArtifactPins[0]
	pin.ID = "child-pin"
	pin.RequestKey = "child-pin"
	pin.Digest = retentionSHA(302)
	pin.RequestHash = ArtifactPinRequestHash(pin.ProductID, pin.ApplicationID, pin.ImageRepository, pin.Digest, pin.Purpose, pin.RequestKey)
	st.ArtifactPins = append(st.ArtifactPins, pin)
	if s := registrySummaryForTest(t, EvaluateRetention(st, "p", retentionNow, []RegistryObservationAuthority{a})); s.Status != "CURRENT" || s.CoveredRoots != 2 {
		t.Fatal(s)
	}
	pin.ID = "unknown-pin"
	pin.RequestKey = "unknown-pin"
	pin.Digest = retentionSHA(399)
	pin.RequestHash = ArtifactPinRequestHash(pin.ProductID, pin.ApplicationID, pin.ImageRepository, pin.Digest, pin.Purpose, pin.RequestKey)
	st.ArtifactPins = append(st.ArtifactPins, pin)
	if registrySummaryForTest(t, EvaluateRetention(st, "p", retentionNow, []RegistryObservationAuthority{a})).Status != "PARTIAL" {
		t.Fatal("new root falsely covered")
	}
	a.ConfigRevision = "rotated"
	if registrySummaryForTest(t, EvaluateRetention(st, "p", retentionNow, []RegistryObservationAuthority{a})).Status == "CURRENT" {
		t.Fatal("new authority matched historical scope")
	}
	if registrySummaryForTest(t, EvaluateRetention(st, "p", retentionNow, nil)).Status != "AUTHORITY_UNKNOWN" {
		t.Fatal("nil authority claims current")
	}
}
func TestRegistryInventoryWindowFreshnessConservativeStartBoundary(t *testing.T) {
	st, a := registryDomainFixture(t)
	v := st.RegistryInventories[0]
	if RegistryInventoryFreshness(v, v.Report.StartedAt.Add(5*time.Minute)) != "FRESH" || RegistryInventoryFreshness(v, v.Report.StartedAt.Add(5*time.Minute+time.Nanosecond)) != "STALE" {
		t.Fatal("5min start boundary")
	}
	for _, field := range []string{"start", "finish", "received"} {
		for _, mode := range []string{"zero", "future"} {
			x := v
			at := time.Time{}
			if mode == "future" {
				at = retentionNow.Add(time.Nanosecond)
			}
			switch field {
			case "start":
				x.Report.StartedAt = at
			case "finish":
				x.Report.FinishedAt = at
			case "received":
				x.ReceivedAt = at
			}
			if RegistryInventoryFreshness(x, retentionNow) != "UNKNOWN" {
				t.Fatal(field, mode)
			}
		}
	}
	// Recent finish/receipt cannot freshen an old scan. Query/read/retry clocks are excluded.
	st.RegistryInventories[0].Report.StartedAt = retentionNow.Add(-6 * time.Minute)
	r := EvaluateRetention(st, "p", retentionNow, []RegistryObservationAuthority{a})
	if registrySummaryForTest(t, r).Status != "STALE" {
		t.Fatal(r.Registry)
	}
	st, a = registryDomainFixture(t)
	before := EvaluateRetention(st, "p", retentionNow, []RegistryObservationAuthority{a})
	after := EvaluateRetention(st, "p", retentionNow.Add(time.Second), []RegistryObservationAuthority{a})
	if before.InputHash != after.InputHash {
		t.Fatal("raw clock changed hash within freshness class")
	}
	st.NativeQuarantines = []NativeQuarantine{{Meta: Meta{ProductID: "p"}}}
	if registrySummaryForTest(t, EvaluateRetention(st, "p", retentionNow, []RegistryObservationAuthority{a})).Status != "QUARANTINED" {
		t.Fatal("restore quarantine claims coverage")
	}
}
func TestRegistryInventoryPartialHistoryUnionOnlyCurrentProtectedRoots(t *testing.T) {
	st, a := registryDomainFixture(t)
	partial := st.RegistryInventories[0]
	partial.ID = "new-partial"
	partial.ReceivedAt = retentionNow.Add(-time.Second)
	partial.Report.Graph = RegistryGraph{Nodes: []RegistryNode{{Digest: retentionSHA(300), Kind: "UNKNOWN", Availability: "UNKNOWN"}}, Roots: []RegistryRoot{{Digest: retentionSHA(300), Closure: "UNKNOWN", Blockers: []string{"registry_missing"}}}, Edges: []RegistryEdge{}, TagSetHash: retentionSHA(702), Discovery: "PARTIAL", Blockers: []string{"registry_global_inventory_unproven", "registry_missing"}}
	partial.Report.Graph, _ = NormalizeRegistryGraph(partial.Report.Graph)
	partial.Report.Graph.GraphHash = RegistryGraphHash(partial.Report.Graph)
	st.RegistryInventories = append(st.RegistryInventories, partial)
	r := EvaluateRetention(st, "p", retentionNow, []RegistryObservationAuthority{a})
	if registrySummaryForTest(t, r).Status != "PARTIAL" || len(r.ProtectedObjects) != 7 {
		t.Fatal("partial graph unprotected old reachable objects", r)
	}
	for _, o := range r.ProtectedObjects {
		if !o.Protected {
			t.Fatal(o)
		}
	}
	// Released roots do not turn historical graph into permanent protection.
	artifactPinReleaseProof(&st)
	r = EvaluateRetention(st, "p", retentionNow.Add(2*time.Second), []RegistryObservationAuthority{a})
	if len(r.ProtectedObjects) != 0 || len(r.ProtectedEdges) != 0 {
		t.Fatal("historical graph permanently pinned old root", r.ProtectedObjects)
	}
}
func TestRegistryInventoryGraphValidationAndPermutation(t *testing.T) {
	st, a := registryDomainFixture(t)
	v := st.RegistryInventories[0]
	for _, mode := range []string{"hash", "unknown-code", "raw-media", "duplicate-node", "missing-node", "bad-platform", "global-complete", "bounds"} {
		t.Run(mode, func(t *testing.T) {
			raw, _ := json.Marshal(v.Report.Graph)
			var g RegistryGraph
			json.Unmarshal(raw, &g)
			limits := a.Limits
			switch mode {
			case "hash":
				g.GraphHash = retentionSHA(999)
			case "unknown-code":
				g.Blockers = append(g.Blockers, "raw-provider-error")
			case "raw-media":
				g.Nodes[0].MediaType = "https://credential.example/token"
			case "duplicate-node":
				g.Nodes = append(g.Nodes, g.Nodes[0])
			case "missing-node":
				g.Nodes = g.Nodes[1:]
			case "bad-platform":
				for i := range g.Edges {
					if g.Edges[i].Kind == "CONFIG" {
						g.Edges[i].Platform = &RegistryPlatform{OS: "linux"}
						break
					}
				}
			case "global-complete":
				g.GlobalInventoryComplete = true
			case "bounds":
				limits.MaxNodes = 1
			}
			if mode != "hash" {
				g, _ = NormalizeRegistryGraph(g)
				g.GraphHash = RegistryGraphHash(g)
			}
			if ValidateRegistryGraph(g, limits, v.Inputs.Registry.Roots) == nil {
				t.Fatal("invalid graph accepted", mode)
			}
		})
	}
	r := EvaluateRetention(st, "p", retentionNow, []RegistryObservationAuthority{a})
	g := st.RegistryInventories[0].Report.Graph
	for i, j := 0, len(g.Nodes)-1; i < j; i, j = i+1, j-1 {
		g.Nodes[i], g.Nodes[j] = g.Nodes[j], g.Nodes[i]
	}
	for i, j := 0, len(g.Edges)-1; i < j; i, j = i+1, j-1 {
		g.Edges[i], g.Edges[j] = g.Edges[j], g.Edges[i]
	}
	normalized, e := NormalizeRegistryGraph(g)
	if e != nil || RegistryGraphHash(g) != v.Report.Graph.GraphHash {
		t.Fatal("graph canonicalization")
	}
	st.RegistryInventories[0].Report.Graph = normalized
	if r.InputHash != EvaluateRetention(st, "p", retentionNow, []RegistryObservationAuthority{a}).InputHash {
		t.Fatal("graph ordering changed normalized evidence hash")
	}
}
func TestRegistryInventoryReferencesAndForeignGraphPrivacy(t *testing.T) {
	st, a := registryDomainFixture(t)
	original, _ := json.Marshal(st)
	for _, mode := range []string{"input", "scope", "time", "job", "receipt"} {
		t.Run(mode, func(t *testing.T) {
			var bad State
			json.Unmarshal(original, &bad)
			v := &bad.RegistryInventories[0]
			switch mode {
			case "input":
				v.InputHash = retentionSHA(99)
			case "scope":
				v.Report.Scope.ProductID = "foreign"
			case "time":
				v.ReceivedAt = v.ReceivedAt.Add(time.Second)
			case "job":
				v.JobID = "other"
			case "receipt":
				bad.WorkerAttempts[0].Report.Registry = nil
			}
			if ValidateRegistryInventories(bad) == nil {
				t.Fatal("corrupt typed reference accepted")
			}
		})
	}
	foreign := st.RegistryInventories[0]
	foreign.ID = "foreign-secret-inventory"
	foreign.ProductID = "foreign-secret-product"
	foreign.Report.Scope.ProductID = foreign.ProductID
	foreign.Report.Graph.GraphHash = RegistryGraphHash(foreign.Report.Graph)
	st.RegistryInventories = append(st.RegistryInventories, foreign)
	r := EvaluateRetention(st, "p", retentionNow, []RegistryObservationAuthority{a})
	raw, _ := json.Marshal(r)
	if stringsContainsRegistryTest(string(raw), "foreign-secret") {
		t.Fatal("foreign inventory metadata leaked")
	}
	if len(r.ProtectedObjects) != 7 {
		t.Fatal(r)
	}
	copy := r
	copy.InputHash = ""
	st.RegistryInventories[1].Report.Requests++
	next := EvaluateRetention(st, "p", retentionNow, []RegistryObservationAuthority{a})
	other := next
	other.InputHash = ""
	if !reflect.DeepEqual(copy, other) || r.InputHash == next.InputHash {
		t.Fatal("foreign consumed inventory facts not opaque/hash-bound")
	}
}
func stringsContainsRegistryTest(s, v string) bool {
	for i := 0; i+len(v) <= len(s); i++ {
		if s[i:i+len(v)] == v {
			return true
		}
	}
	return false
}

func TestRegistryInventoryZeroByteBlobIsValidButEmptyManifestIsNot(t *testing.T) {
	st, a := registryDomainFixture(t)
	g := st.RegistryInventories[0].Report.Graph
	changed := false
	for i := range g.Nodes {
		if g.Nodes[i].Kind == "BLOB" {
			g.Nodes[i].Size = 0
			changed = true
			break
		}
	}
	if !changed {
		t.Fatal("fixture needs blob")
	}
	g, _ = NormalizeRegistryGraph(g)
	g.GraphHash = RegistryGraphHash(g)
	if e := ValidateRegistryGraph(g, a.Limits, st.RegistryInventories[0].Inputs.Registry.Roots); e != nil {
		t.Fatal("verified zero-byte blob rejected", e)
	}
	for i := range g.Nodes {
		if g.Nodes[i].Kind == "MANIFEST" {
			g.Nodes[i].Size = 0
			break
		}
	}
	g.GraphHash = RegistryGraphHash(g)
	if ValidateRegistryGraph(g, a.Limits, st.RegistryInventories[0].Inputs.Registry.Roots) == nil {
		t.Fatal("empty manifest accepted")
	}
}
