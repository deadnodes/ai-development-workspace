package domain

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"time"
)

const RegistryObservationKind = "WORKER_ATTESTED_REGISTRY_READ"
const RegistryObserveOperationKind = "REFRESH_REGISTRY_INVENTORY"
const RegistryReportMaxBytes = 256 << 10

type RegistryObserveLimits struct {
	MaxResponseBytes int64 `json:"max_response_bytes"`
	MaxTotalBytes    int64 `json:"max_total_bytes"`
	MaxNodes         int   `json:"max_nodes"`
	MaxEdges         int   `json:"max_edges"`
	MaxDepth         int   `json:"max_depth"`
	MaxPages         int   `json:"max_pages"`
	MaxRequests      int   `json:"max_requests"`
	TimeoutSeconds   int   `json:"timeout_seconds"`
}
type RegistryScope struct {
	ProductID       string `json:"product_id"`
	ComponentID     string `json:"component_id"`
	RepositoryID    string `json:"repository_id"`
	Origin          string `json:"origin"`
	Repository      string `json:"repository"`
	ImageRepository string `json:"image_repository"`
}
type RegistryObservationAuthority struct {
	Scope          RegistryScope         `json:"scope"`
	RecipeID       string                `json:"recipe_id"`
	RecipeRevision string                `json:"recipe_revision"`
	RecipeHash     string                `json:"recipe_hash"`
	ConfigRevision string                `json:"config_revision"`
	Limits         RegistryObserveLimits `json:"limits"`
	Current        bool                  `json:"current"`
}
type WorkerRegistryInputs struct {
	Scope                     RegistryScope         `json:"scope"`
	Roots                     []string              `json:"roots"`
	RootSetHash               string                `json:"root_set_hash"`
	RequestRetentionInputHash string                `json:"request_retention_input_hash"`
	Limits                    RegistryObserveLimits `json:"limits"`
}
type RegistryNode struct {
	Digest       string `json:"digest"`
	Kind         string `json:"kind"`
	MediaType    string `json:"media_type"`
	Availability string `json:"availability"`
	Size         int64  `json:"size"`
}
type RegistryPlatform struct {
	OS           string `json:"os,omitempty"`
	Architecture string `json:"architecture,omitempty"`
	Variant      string `json:"variant,omitempty"`
}
type RegistryEdge struct {
	From     string            `json:"from"`
	To       string            `json:"to"`
	Kind     string            `json:"kind"`
	Platform *RegistryPlatform `json:"platform,omitempty"`
}
type RegistryRoot struct {
	Digest   string   `json:"digest"`
	Closure  string   `json:"closure"`
	Blockers []string `json:"blockers"`
}
type RegistryGraph struct {
	ImageContentRoots       []RegistryImageContentRoot `json:"image_content_roots,omitempty"`
	Nodes                   []RegistryNode             `json:"nodes"`
	Edges                   []RegistryEdge             `json:"edges"`
	Roots                   []RegistryRoot             `json:"roots"`
	TagCount                int                        `json:"tag_count"`
	TagSetHash              string                     `json:"tag_set_hash"`
	Discovery               string                     `json:"discovery"`
	GlobalInventoryComplete bool                       `json:"global_inventory_complete"`
	Blockers                []string                   `json:"blockers"`
	GraphHash               string                     `json:"graph_hash"`
}
type WorkerRegistryReport struct {
	Kind           string        `json:"kind"`
	Scope          RegistryScope `json:"scope"`
	RecipeID       string        `json:"recipe_id"`
	RecipeRevision string        `json:"recipe_revision"`
	RecipeHash     string        `json:"recipe_hash"`
	ConfigRevision string        `json:"config_revision"`
	RootSetHash    string        `json:"root_set_hash"`
	StartedAt      time.Time     `json:"started_at"`
	FinishedAt     time.Time     `json:"finished_at"`
	Requests       int           `json:"requests"`
	Bytes          int64         `json:"bytes"`
	Graph          RegistryGraph `json:"graph"`
}
type RegistryInventory struct {
	Meta
	JobID      string               `json:"job_id"`
	AttemptID  string               `json:"attempt_id"`
	Fence      uint64               `json:"fence"`
	InputHash  string               `json:"input_hash"`
	Inputs     WorkerInputs         `json:"inputs"`
	Report     WorkerRegistryReport `json:"report"`
	ReceivedAt time.Time            `json:"received_at"`
}

var registryCodes = map[string]bool{"registry_global_inventory_unproven": true, "registry_request_failed": true, "registry_unauthorized": true, "registry_missing": true, "registry_malformed": true, "registry_digest_mismatch": true, "registry_descriptor_conflict": true, "registry_unsupported": true, "registry_limit": true, "registry_pagination": true, "registry_discovery_changed": true, "registry_referrers_unknown": true, "registry_filtered_referrers": true, "registry_cancelled": true}

func ValidateRegistryObserveLimits(l RegistryObserveLimits) error {
	if l.MaxResponseBytes < 1024 || l.MaxResponseBytes > 1<<30 || l.MaxTotalBytes < l.MaxResponseBytes || l.MaxTotalBytes > 8<<30 || l.MaxNodes < 1 || l.MaxNodes > 1024 || l.MaxEdges < 1 || l.MaxEdges > 4096 || l.MaxDepth < 1 || l.MaxDepth > 32 || l.MaxPages < 1 || l.MaxPages > 128 || l.MaxRequests < 1 || l.MaxRequests > 8192 || l.TimeoutSeconds < 1 || l.TimeoutSeconds > 900 {
		return fmt.Errorf("invalid registry observation limits")
	}
	return nil
}
func ValidateRegistryScope(s RegistryScope) error {
	if s.ProductID == "" || s.ComponentID == "" || s.RepositoryID == "" {
		return fmt.Errorf("missing registry scope")
	}
	u, e := url.Parse(s.Origin)
	if e != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.String() != s.Origin {
		return fmt.Errorf("invalid registry origin")
	}
	if u.Scheme != "https" && (u.Scheme != "http" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback()) {
		return fmt.Errorf("registry TLS required")
	}
	image, e := CanonicalImageRepository(u.Host + "/" + s.Repository)
	if e != nil || image != s.ImageRepository {
		return fmt.Errorf("noncanonical registry repository")
	}
	return nil
}
func RegistryRootSetHash(scope RegistryScope, recipeID, recipeRevision, recipeHash, configRevision string, roots []string) string {
	return checkHash(struct {
		Scope                                                RegistryScope
		RecipeID, RecipeRevision, RecipeHash, ConfigRevision string
		Roots                                                []string
	}{scope, recipeID, recipeRevision, recipeHash, configRevision, sortedUnique(append([]string{}, roots...))})
}
func registryStatus(s string) bool { return s == "COMPLETE" || s == "PARTIAL" || s == "UNKNOWN" }
func registryBlockers(v []string) bool {
	if len(v) > len(registryCodes) {
		return false
	}
	for _, code := range v {
		if !registryCodes[code] {
			return false
		}
	}
	return true
}

// Normalization copies all collections, rejects conflicting object identities,
// and never removes a reported fact to make a malformed graph appear complete.
func NormalizeRegistryGraph(g RegistryGraph) (RegistryGraph, error) {
	g.ImageContentRoots = normalizeImageContent(g.ImageContentRoots)
	g.Nodes = append([]RegistryNode{}, g.Nodes...)
	g.Edges = append([]RegistryEdge{}, g.Edges...)
	g.Roots = append([]RegistryRoot{}, g.Roots...)
	g.Blockers = sortedUnique(append([]string{}, g.Blockers...))
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].Digest < g.Nodes[j].Digest })
	for i := 1; i < len(g.Nodes); i++ {
		if g.Nodes[i-1].Digest == g.Nodes[i].Digest {
			return g, fmt.Errorf("duplicate registry object identity")
		}
	}
	for i := range g.Edges {
		if g.Edges[i].Platform != nil {
			v := *g.Edges[i].Platform
			g.Edges[i].Platform = &v
		}
	}
	sort.Slice(g.Edges, func(i, j int) bool { return registryEdgeKey(g.Edges[i]) < registryEdgeKey(g.Edges[j]) })
	for i := 1; i < len(g.Edges); i++ {
		if registryEdgeKey(g.Edges[i-1]) == registryEdgeKey(g.Edges[i]) {
			return g, fmt.Errorf("duplicate registry edge")
		}
	}
	sort.Slice(g.Roots, func(i, j int) bool { return g.Roots[i].Digest < g.Roots[j].Digest })
	for i := range g.Roots {
		g.Roots[i].Blockers = sortedUnique(append([]string{}, g.Roots[i].Blockers...))
		if i > 0 && g.Roots[i-1].Digest == g.Roots[i].Digest {
			return g, fmt.Errorf("duplicate registry root")
		}
	}
	return g, nil
}
func registryEdgeKey(e RegistryEdge) string {
	return e.From + "\x00" + e.To + "\x00" + e.Kind + "\x00" + checkHash(e.Platform)
}
func RegistryGraphHash(g RegistryGraph) string {
	g, e := NormalizeRegistryGraph(g)
	if e != nil {
		return ""
	}
	g.GraphHash = ""
	return checkHash(g)
}
func ValidateRegistryGraph(g RegistryGraph, l RegistryObserveLimits, knownRoots []string) error {
	fail := func() error { return fmt.Errorf("invalid bounded registry graph") }
	if ValidateRegistryObserveLimits(l) != nil || len(g.Nodes) > l.MaxNodes || len(g.Edges) > l.MaxEdges || len(g.Roots) > l.MaxNodes || g.TagCount < 0 || g.TagCount > l.MaxNodes*l.MaxPages || g.GlobalInventoryComplete || !registryStatus(g.Discovery) || !registryBlockers(g.Blockers) || !retentionDigest.MatchString(g.TagSetHash) {
		return fail()
	}
	if !boundedImageContent(g.ImageContentRoots, l) {
		return fail()
	}
	normalized, e := NormalizeRegistryGraph(g)
	if e != nil || !reflect.DeepEqual(g, normalized) || g.GraphHash != RegistryGraphHash(g) {
		return fail()
	}
	nodes := map[string]RegistryNode{}
	media := regexp.MustCompile(`^[a-z0-9!#$&^_.+-]+/[a-z0-9!#$&^_.+-]+$`)
	platform := regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)
	for _, n := range g.Nodes {
		if !retentionDigest.MatchString(n.Digest) || (n.Kind != "INDEX" && n.Kind != "MANIFEST" && n.Kind != "BLOB" && n.Kind != "UNKNOWN") || (n.Availability != "PRESENT" && n.Availability != "MISSING" && n.Availability != "UNKNOWN") || n.Size < 0 || n.Size > l.MaxResponseBytes || len(n.MediaType) > 128 || n.MediaType != "" && !media.MatchString(n.MediaType) || n.Availability == "PRESENT" && (n.Size == 0 && n.Kind != "BLOB" || n.Kind == "UNKNOWN" || n.MediaType == "") {
			return fail()
		}
		nodes[n.Digest] = n
	}
	adjacency := map[string][]string{}
	for _, edge := range g.Edges {
		from, ok := nodes[edge.From]
		to, ok2 := nodes[edge.To]
		if !ok || !ok2 || (edge.Kind != "CHILD" && edge.Kind != "CONFIG" && edge.Kind != "LAYER" && edge.Kind != "SUBJECT" && edge.Kind != "REFERRER" && edge.Kind != "REFERRERS_INDEX") {
			return fail()
		}
		if edge.Platform != nil {
			if edge.Kind != "CHILD" || edge.Platform.OS != "" && !platform.MatchString(edge.Platform.OS) || edge.Platform.Architecture != "" && !platform.MatchString(edge.Platform.Architecture) || edge.Platform.Variant != "" && !platform.MatchString(edge.Platform.Variant) {
				return fail()
			}
		}
		if edge.Kind == "CHILD" && (from.Kind != "INDEX" || to.Kind == "BLOB") || (edge.Kind == "CONFIG" || edge.Kind == "LAYER") && (from.Kind != "MANIFEST" || to.Kind != "BLOB" && to.Kind != "UNKNOWN") || edge.Kind == "SUBJECT" && (from.Kind == "BLOB" || to.Kind == "BLOB") || (edge.Kind == "REFERRER" || edge.Kind == "REFERRERS_INDEX") && (from.Kind == "BLOB" || to.Kind == "BLOB") {
			return fail()
		}
		adjacency[edge.From] = append(adjacency[edge.From], edge.To)
	}
	if g.Discovery == "COMPLETE" {
		for _, code := range g.Blockers {
			if code != "registry_global_inventory_unproven" {
				return fail()
			}
		}
	}
	configs := map[string]int{}
	for _, e := range g.Edges {
		if e.Kind == "CONFIG" {
			configs[e.From]++
		}
	}
	roots := map[string]bool{}
	for _, r := range g.Roots {
		n, ok := nodes[r.Digest]
		if !ok || n.Kind == "BLOB" || !registryStatus(r.Closure) || !registryBlockers(r.Blockers) || r.Closure == "COMPLETE" && (len(r.Blockers) > 0 || n.Availability != "PRESENT") {
			return fail()
		}
		roots[r.Digest] = true
		type item struct {
			digest string
			depth  int
		}
		queue := []item{{r.Digest, 0}}
		seen := map[string]bool{}
		for len(queue) > 0 {
			x := queue[0]
			queue = queue[1:]
			if seen[x.digest] {
				continue
			}
			seen[x.digest] = true
			if x.depth > l.MaxDepth {
				return fail()
			}
			if r.Closure == "COMPLETE" && nodes[x.digest].Availability != "PRESENT" {
				return fail()
			}
			if r.Closure == "COMPLETE" {
				node := nodes[x.digest]
				if node.Kind == "MANIFEST" && (node.MediaType == "application/vnd.oci.image.manifest.v1+json" || node.MediaType == "application/vnd.docker.distribution.manifest.v2+json") && configs[x.digest] != 1 {
					return fail()
				}
			}

			for _, next := range adjacency[x.digest] {
				queue = append(queue, item{next, x.depth + 1})
			}
		}
	}
	for _, digest := range knownRoots {
		if !retentionDigest.MatchString(digest) || !roots[digest] {
			return fail()
		}
	}
	if !retentionHasCode(g.Blockers, "registry_global_inventory_unproven") {
		return fail()
	}
	if validateImageContent(g, l, knownRoots) != nil {
		return fail()
	}
	b, _ := json.Marshal(g)
	if len(b) > RegistryReportMaxBytes {
		return fail()
	}
	return nil
}
func retentionHasCode(codes []string, want string) bool {
	for _, code := range codes {
		if code == want {
			return true
		}
	}
	return false
}
func ValidateRegistryWorkerReport(j WorkerJob, a WorkerAttempt, r WorkerReport, received time.Time) error {
	fail := func() error { return fmt.Errorf("invalid scoped registry observation report") }
	p := j.Inputs.Registry
	v := r.Registry
	if j.Capability != "registry_observe" || p == nil || r.LocalOCI != nil || r.Publication != nil || r.JobID != j.ID || r.AttemptID != a.ID || r.Fence != a.Fence || r.InputHash != j.InputHash {
		return fail()
	}
	if r.Result != "PASSED" && r.Result != "EXECUTOR_FAILED" {
		return fail()
	}
	if r.Result != "PASSED" {
		if v != nil {
			return fail()
		}
		return nil
	}
	if v == nil || v.Kind != RegistryObservationKind || !reflect.DeepEqual(v.Scope, p.Scope) || v.RecipeID != j.Inputs.RecipeID || v.RecipeRevision != j.Inputs.RecipeRevision || v.RecipeHash != j.Inputs.RecipeHash || v.ConfigRevision != j.Inputs.ConfigRevision || v.RootSetHash != p.RootSetHash || v.Requests < 0 || v.Requests > p.Limits.MaxRequests || v.Bytes < 0 || v.Bytes > p.Limits.MaxTotalBytes || v.StartedAt.IsZero() || v.FinishedAt.IsZero() || v.StartedAt.Before(a.CreatedAt) || v.FinishedAt.Before(v.StartedAt) || v.FinishedAt.After(received) || received.Before(a.CreatedAt) || v.FinishedAt.Sub(v.StartedAt) > time.Duration(p.Limits.TimeoutSeconds)*time.Second || ValidateRegistryGraph(v.Graph, p.Limits, p.Roots) != nil {
		return fail()
	}
	verifiedBytes := int64(0)
	for _, node := range v.Graph.Nodes {
		if node.Availability == "PRESENT" {
			verifiedBytes += node.Size
		}
	}
	if verifiedBytes > v.Bytes {
		return fail()
	}
	b, _ := json.Marshal(r)
	if len(b) > RegistryReportMaxBytes {
		return fail()
	}
	return nil
}
func ValidateRegistryInventories(st State) error {
	fail := func() error { return fmt.Errorf("invalid registry inventory references or immutable receipt") }
	jobs := map[string]WorkerJob{}
	attempts := map[string]WorkerAttempt{}
	ops := map[string]ExternalOperation{}
	for _, j := range st.WorkerJobs {
		jobs[j.ID] = j
	}
	for _, a := range st.WorkerAttempts {
		attempts[a.ID] = a
	}
	for _, o := range st.Operations {
		ops[o.ID] = o
	}
	for _, j := range st.WorkerJobs {
		if j.Capability != "registry_observe" {
			if j.Inputs.Registry != nil {
				return fail()
			}
			continue
		}
		p := j.Inputs.Registry
		o, ok := ops[j.OperationID]
		if p == nil || !ok || o.Kind != RegistryObserveOperationKind || o.ProductID != j.ProductID || o.ApplicationID != p.Scope.ComponentID || o.RepositoryID != p.Scope.RepositoryID || o.RegistryObservation == nil || !reflect.DeepEqual(*o.RegistryObservation, j.Inputs) || p.Scope.ProductID != j.ProductID || p.Scope.RepositoryID != j.Inputs.RepositoryID || ValidateRegistryScope(p.Scope) != nil || ValidateRegistryObserveLimits(p.Limits) != nil || j.InputHash != checkHash(j.Inputs) || j.Inputs.SourceSHA != "" || j.Inputs.RevisionID != "" || j.Inputs.Execution != nil || j.Inputs.SourceProof != nil || j.Inputs.Publication != nil || j.Inputs.Check != nil || j.Inputs.Trust != "registry_observation" || !retentionDigest.MatchString(j.Inputs.RecipeHash) || j.Inputs.RecipeID == "" || j.Inputs.RecipeRevision == "" || j.Inputs.ConfigRevision == "" || len(p.Roots) > 256 || len(j.Inputs.Parameters) != 0 || p.RootSetHash != RegistryRootSetHash(p.Scope, j.Inputs.RecipeID, j.Inputs.RecipeRevision, j.Inputs.RecipeHash, j.Inputs.ConfigRevision, p.Roots) || !retentionDigest.MatchString(p.RequestRetentionInputHash) || !reflect.DeepEqual(p.Roots, sortedUnique(append([]string{}, p.Roots...))) {
			return fail()
		}
		for _, root := range p.Roots {
			if !retentionDigest.MatchString(root) {
				return fail()
			}
		}
		validApp, validRepo := false, false
		for _, app := range st.Applications {
			validApp = validApp || app.ID == p.Scope.ComponentID && app.ProductID == j.ProductID
		}
		for _, repo := range st.Repositories {
			validRepo = validRepo || repo.ID == p.Scope.RepositoryID && repo.ProductID == j.ProductID
		}
		if !validApp || !validRepo {
			return fail()
		}
	}
	inventoryIDs := map[string]int{}
	if len(st.RegistryInventories) > 0 {
		state := reflect.ValueOf(st)
		for i := 0; i < state.NumField(); i++ {
			field := state.Field(i)
			if field.Kind() != reflect.Slice {
				continue
			}
			for n := 0; n < field.Len(); n++ {
				element := field.Index(n)
				if element.Kind() != reflect.Struct {
					continue
				}
				id := element.FieldByName("ID")
				if id.IsValid() && id.Kind() == reflect.String && id.String() != "" {
					inventoryIDs[id.String()]++
				}
			}
		}
	}
	seen := map[string]bool{}
	for _, v := range st.RegistryInventories {
		j, ok := jobs[v.JobID]
		a, ok2 := attempts[v.AttemptID]
		if !ok || !ok2 || v.ID == "" || inventoryIDs[v.ID] != 1 || v.FeatureID != j.FeatureID || a.Report != nil && a.Report.Result == "PASSED" && (j.Status != "SUCCEEDED" || a.Status != "SUCCEEDED") || v.ProductID != j.ProductID || a.ProductID != j.ProductID || a.JobID != j.ID || a.Fence != v.Fence || j.InputHash != v.InputHash || a.InputHash != v.InputHash || j.Capability != "registry_observe" || !reflect.DeepEqual(v.Inputs, j.Inputs) || a.Report == nil || a.Report.Result != "PASSED" || a.Report.Registry == nil || !reflect.DeepEqual(v.Report, *a.Report.Registry) || !a.UpdatedAt.Equal(v.ReceivedAt) || !v.CreatedAt.Equal(v.ReceivedAt) || !v.UpdatedAt.Equal(v.ReceivedAt) || ValidateRegistryWorkerReport(j, a, *a.Report, v.ReceivedAt) != nil || seen[v.AttemptID] {
			return fail()
		}
		seen[v.AttemptID] = true
	}
	for _, a := range st.WorkerAttempts {
		if j, ok := jobs[a.JobID]; ok && j.Capability == "registry_observe" && a.Report != nil {
			if ValidateRegistryWorkerReport(j, a, *a.Report, a.UpdatedAt) != nil || a.Report.Result == "PASSED" && !seen[a.ID] {
				return fail()
			}
		} else if a.Report != nil && a.Report.Registry != nil {
			return fail()
		}
	}
	return nil
}

func retentionWorkerReportHash(r *WorkerReport) string {
	if r == nil || r.Registry == nil {
		return checkHash(r)
	}
	copy := *r
	registry := *r.Registry
	normalized, e := NormalizeRegistryGraph(registry.Graph)
	if e == nil {
		registry.Graph = normalized
	}
	copy.Registry = &registry
	return checkHash(copy)
}
func registryInventoryEvidence(v RegistryInventory, now time.Time) string {
	normalized, e := NormalizeRegistryGraph(v.Report.Graph)
	if e == nil {
		v.Report.Graph = normalized
	}
	return checkHash(struct {
		Inventory RegistryInventory
		Freshness string
	}{v, RegistryInventoryFreshness(v, now)})
}
