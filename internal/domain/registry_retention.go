package domain

import (
	"reflect"
	"sort"
	"strings"
	"time"
)

type RegistryCoverageSummary struct {
	RecipeRevision          string     `json:"recipe_revision"`
	ApplicationID           string     `json:"application_id"`
	ImageRepository         string     `json:"image_repository"`
	RecipeID                string     `json:"recipe_id"`
	RecipeHash              string     `json:"recipe_hash"`
	ConfigRevision          string     `json:"config_revision"`
	InventoryID             string     `json:"inventory_id"`
	Status                  string     `json:"status"`
	Freshness               string     `json:"freshness"`
	Attestation             string     `json:"attestation"`
	RootSetHash             string     `json:"root_set_hash"`
	GraphHash               string     `json:"graph_hash"`
	StartedAt               *time.Time `json:"started_at,omitempty"`
	FinishedAt              *time.Time `json:"finished_at,omitempty"`
	ReceivedAt              *time.Time `json:"received_at,omitempty"`
	RequiredRoots           int        `json:"required_roots"`
	CoveredRoots            int        `json:"covered_roots"`
	Nodes                   int        `json:"nodes"`
	Edges                   int        `json:"edges"`
	Blockers                []string   `json:"blockers"`
	GlobalInventoryComplete bool       `json:"global_inventory_complete"`
}
type RegistryProtectedObject struct {
	ImageRepository string   `json:"image_repository"`
	Digest          string   `json:"digest"`
	Kind            string   `json:"kind"`
	MediaType       string   `json:"media_type"`
	Availability    string   `json:"availability"`
	Size            int64    `json:"size"`
	Protected       bool     `json:"protected"`
	RootDigests     []string `json:"root_digests"`
	Reasons         []string `json:"reasons"`
	Blockers        []string `json:"blockers"`
}
type RegistryProtectedEdge struct {
	ImageRepository string `json:"image_repository"`
	RegistryEdge
}

func RegistryKnownRoots(report RetentionReport, image string) []string {
	roots := []string{}
	for _, v := range report.Images {
		if v.ImageRepository == image {
			roots = append(roots, v.Digest)
		}
	}
	return sortedUnique(roots)
}
func RegistryInventoryFreshness(v RegistryInventory, now time.Time) string {
	state := "FRESH"
	for _, at := range []time.Time{v.Report.StartedAt, v.Report.FinishedAt, v.ReceivedAt} {
		f := evidenceFreshness(at, now)
		if f == "UNKNOWN" {
			return f
		}
		if f == "STALE" {
			state = f
		}
	}
	return state
}
func inventoryMatches(v RegistryInventory, a RegistryObservationAuthority) bool {
	return v.ProductID == a.Scope.ProductID && reflect.DeepEqual(v.Report.Scope, a.Scope) && v.Inputs.Registry != nil && reflect.DeepEqual(v.Inputs.Registry.Scope, a.Scope) && v.Inputs.RecipeID == a.RecipeID && v.Inputs.RecipeRevision == a.RecipeRevision && v.Inputs.RecipeHash == a.RecipeHash && v.Inputs.ConfigRevision == a.ConfigRevision && reflect.DeepEqual(v.Inputs.Registry.Limits, a.Limits)
}
func registryReachable(graph RegistryGraph, root string) map[string]bool {
	edges := map[string][]string{}
	for _, e := range graph.Edges {
		edges[e.From] = append(edges[e.From], e.To)
	}
	seen := map[string]bool{}
	queue := []string{root}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		if seen[d] {
			continue
		}
		seen[d] = true
		queue = append(queue, edges[d]...)
	}
	return seen
}
func registryCovered(g RegistryGraph, digest string) bool {
	for _, root := range g.Roots {
		if root.Closure == "COMPLETE" && registryReachable(g, root.Digest)[digest] {
			for _, node := range g.Nodes {
				if node.Digest == digest && node.Availability == "PRESENT" && node.Kind != "BLOB" && node.Kind != "UNKNOWN" {
					return true
				}
			}
		}
	}
	return false
}
func registryLatest(inventories []RegistryInventory, a RegistryObservationAuthority) *RegistryInventory {
	var latest *RegistryInventory
	for i := range inventories {
		v := &inventories[i]
		if inventoryMatches(*v, a) && (latest == nil || v.ReceivedAt.After(latest.ReceivedAt) || v.ReceivedAt.Equal(latest.ReceivedAt) && v.ID < latest.ID) {
			latest = v
		}
	}
	return latest
}

// Equal server acceptance timestamps do not establish which distinct observation
// came last. IDs select a stable display record, never observation authority.
func registryLatestAmbiguous(inventories []RegistryInventory, a RegistryObservationAuthority, latest *RegistryInventory) bool {
	if latest == nil {
		return false
	}
	for _, v := range inventories {
		if inventoryMatches(v, a) && v.ReceivedAt.Equal(latest.ReceivedAt) && !reflect.DeepEqual(v.Report, latest.Report) {
			return true
		}
	}
	return false
}
func registrySummary(st State, report RetentionReport, a RegistryObservationAuthority, now time.Time) RegistryCoverageSummary {
	roots := RegistryKnownRoots(report, a.Scope.ImageRepository)
	s := RegistryCoverageSummary{ApplicationID: a.Scope.ComponentID, ImageRepository: a.Scope.ImageRepository, RecipeID: a.RecipeID, RecipeRevision: a.RecipeRevision, RecipeHash: a.RecipeHash, ConfigRevision: a.ConfigRevision, Status: "MISSING", Freshness: "UNKNOWN", RequiredRoots: len(roots), RootSetHash: RegistryRootSetHash(a.Scope, a.RecipeID, a.RecipeRevision, a.RecipeHash, a.ConfigRevision, roots), Blockers: []string{"registry_global_inventory_unproven"}}
	latest := registryLatest(st.RegistryInventories, a)
	if latest != nil {
		s.InventoryID = latest.ID
		s.Attestation = latest.Report.Kind
		s.GraphHash = latest.Report.Graph.GraphHash
		s.StartedAt = &latest.Report.StartedAt
		s.FinishedAt = &latest.Report.FinishedAt
		s.ReceivedAt = &latest.ReceivedAt
		s.Nodes = len(latest.Report.Graph.Nodes)
		s.Edges = len(latest.Report.Graph.Edges)
		s.Freshness = RegistryInventoryFreshness(*latest, now)
		for _, root := range roots {
			if registryCovered(latest.Report.Graph, root) {
				s.CoveredRoots++
			}
		}
		s.Blockers = append(s.Blockers, latest.Report.Graph.Blockers...)
		s.Status = "CURRENT"
		if s.CoveredRoots != s.RequiredRoots || latest.Report.Graph.Discovery != "COMPLETE" {
			s.Status = "PARTIAL"
			s.Blockers = append(s.Blockers, "registry_current_roots_uncovered")
		}
		if s.Freshness == "STALE" {
			s.Status = "STALE"
			s.Blockers = append(s.Blockers, "registry_observation_stale")
		}
		if s.Freshness == "UNKNOWN" {
			s.Status = "UNKNOWN"
			s.Blockers = append(s.Blockers, "registry_observation_window_unknown")
		}
	} else {
		s.Blockers = append(s.Blockers, "registry_observation_missing")
	}
	if registryLatestAmbiguous(st.RegistryInventories, a, latest) {
		s.Status = "UNKNOWN"
		s.Blockers = append(s.Blockers, "registry_observation_order_ambiguous")
	}
	for _, j := range st.WorkerJobs {
		if j.ProductID != report.ProductID || j.Inputs.Registry == nil || !reflect.DeepEqual(j.Inputs.Registry.Scope, a.Scope) || j.Inputs.RecipeHash != a.RecipeHash || j.Inputs.ConfigRevision != a.ConfigRevision {
			continue
		}
		if (j.Status == "FAILED" || j.Status == "RECONCILIATION_REQUIRED" || j.Status == "DEADLINE_EXCEEDED") && (latest == nil || j.UpdatedAt.After(latest.ReceivedAt)) {
			s.Status = "UNKNOWN"
			s.Blockers = append(s.Blockers, "registry_observer_execution_unknown")
		}
	}
	if !a.Current {
		s.Status = "AUTHORITY_UNKNOWN"
		s.Blockers = append(s.Blockers, "registry_authority_unavailable")
	}
	for _, q := range st.NativeQuarantines {
		if q.ProductID == report.ProductID {
			s.Status = "QUARANTINED"
			s.Blockers = append(s.Blockers, "restore_quarantine")
		}
	}
	s.Blockers = sortedUnique(s.Blockers)
	return s
}
func registryRetention(st State, report *RetentionReport, now time.Time, authorities []RegistryObservationAuthority) []string {
	report.Registry = []RegistryCoverageSummary{}
	report.ProtectedObjects = []RegistryProtectedObject{}
	report.ProtectedEdges = []RegistryProtectedEdge{}
	evidence := []string{}
	scopeRepos := map[string]bool{}
	for _, image := range report.Images {
		scopeRepos[image.ImageRepository] = true
	}
	for _, a := range authorities {
		if a.Scope.ProductID == report.ProductID {
			scopeRepos[a.Scope.ImageRepository] = true
		}
	}
	seenAuthorities := map[string]bool{}
	for _, a := range authorities {
		if a.Scope.ProductID != report.ProductID {
			continue
		}
		key := a.Scope.ComponentID + "\x00" + a.Scope.ImageRepository + "\x00" + a.RecipeID
		seenAuthorities[key] = true
		report.Registry = append(report.Registry, registrySummary(st, *report, a, now))
		evidence = append(evidence, checkHash(a))
	}
	// Historical pins describe unavailable authority; choose their display
	// representative canonically rather than from the State slice order. This
	// ordering supplies no current authority or execution chronology.
	history := append([]RegistryInventory(nil), st.RegistryInventories...)
	sort.Slice(history, func(i, j int) bool {
		if !history[i].ReceivedAt.Equal(history[j].ReceivedAt) {
			return history[i].ReceivedAt.After(history[j].ReceivedAt)
		}
		return history[i].ID < history[j].ID
	})
	for _, v := range history {
		if !scopeRepos[v.Report.Scope.ImageRepository] {
			continue
		}
		evidence = append(evidence, registryInventoryEvidence(v, now))
		if v.ProductID == report.ProductID {
			key := v.Report.Scope.ComponentID + "\x00" + v.Report.Scope.ImageRepository + "\x00" + v.Report.RecipeID
			if !seenAuthorities[key] {
				a := RegistryObservationAuthority{Scope: v.Report.Scope, RecipeID: v.Report.RecipeID, RecipeRevision: v.Report.RecipeRevision, RecipeHash: v.Report.RecipeHash, ConfigRevision: v.Report.ConfigRevision}
				if v.Inputs.Registry != nil {
					a.Limits = v.Inputs.Registry.Limits
				}
				report.Registry = append(report.Registry, registrySummary(st, *report, a, now))
				seenAuthorities[key] = true
			}
		}
	}
	sort.Slice(report.Registry, func(i, j int) bool {
		a, b := report.Registry[i], report.Registry[j]
		return a.ApplicationID+"\x00"+a.ImageRepository+"\x00"+a.RecipeID < b.ApplicationID+"\x00"+b.ImageRepository+"\x00"+b.RecipeID
	})
	// Edges are conservatively unioned across admitted history, irrespective of
	// latest/config/freshness. Traversal starts only at CURRENT protected roots.
	// Thus a newer partial graph cannot unprotect bytes; an old inventory is not a pin.
	type historicalNode struct {
		node     RegistryNode
		at       time.Time
		id       string
		conflict bool
	}
	nodes := map[string]historicalNode{}
	edges := map[string]RegistryProtectedEdge{}
	partial := map[string]bool{}
	for _, v := range st.RegistryInventories {
		repo := v.Report.Scope.ImageRepository
		if !scopeRepos[repo] {
			continue
		}
		if v.Report.Graph.Discovery != "COMPLETE" || RegistryInventoryFreshness(v, now) != "FRESH" {
			partial[repo] = true
		}
		for _, n := range v.Report.Graph.Nodes {
			key := repo + "@" + n.Digest
			old, ok := nodes[key]
			conflict := ok && (old.node.Kind != n.Kind || old.node.MediaType != n.MediaType || old.node.Size != n.Size)
			if !ok || v.ReceivedAt.After(old.at) || v.ReceivedAt.Equal(old.at) && v.ID < old.id {
				nodes[key] = historicalNode{n, v.ReceivedAt, v.ID, old.conflict || conflict}
			} else if conflict {
				old.conflict = true
				nodes[key] = old
			}
		}
		for _, e := range v.Report.Graph.Edges {
			key := repo + "\x00" + registryEdgeKey(e)
			edges[key] = RegistryProtectedEdge{repo, e}
		}
	}
	protected := map[string]*RegistryProtectedObject{}
	usedEdges := map[string]RegistryProtectedEdge{}
	for _, root := range report.Images {
		if !root.Protected {
			continue
		}
		repo := root.ImageRepository
		queue := []string{root.Digest}
		seen := map[string]bool{}
		reason := "registry_historical_closure"
		if retentionHasCode(root.Reasons, "shared_product_root") {
			reason = "shared_product_root"
		}
		for len(queue) > 0 {
			digest := queue[0]
			queue = queue[1:]
			if seen[digest] {
				continue
			}
			seen[digest] = true
			key := repo + "@" + digest
			object := protected[key]
			if object == nil {
				object = &RegistryProtectedObject{ImageRepository: repo, Digest: digest, Kind: "UNKNOWN", Availability: "UNKNOWN", Protected: true, RootDigests: []string{}, Reasons: []string{}, Blockers: []string{}}
				if n, ok := nodes[key]; ok {
					object.Kind = n.node.Kind
					object.MediaType = n.node.MediaType
					object.Size = n.node.Size
					object.Availability = n.node.Availability
					if n.conflict {
						object.Blockers = append(object.Blockers, "registry_historical_descriptor_conflict")
					}
				} else {
					object.Blockers = append(object.Blockers, "registry_object_unknown")
				}
				if partial[repo] {
					object.Blockers = append(object.Blockers, "registry_historical_coverage_degraded")
				}
				protected[key] = object
			}
			object.RootDigests = append(object.RootDigests, root.Digest)
			object.Reasons = append(object.Reasons, reason)
			for edgeKey, e := range edges {
				if e.ImageRepository == repo && e.From == digest {
					queue = append(queue, e.To)
					usedEdges[edgeKey] = e
				}
			}
		}
	}
	for _, v := range protected {
		v.RootDigests = sortedUnique(v.RootDigests)
		v.Reasons = sortedUnique(v.Reasons)
		v.Blockers = sortedUnique(v.Blockers)
		report.ProtectedObjects = append(report.ProtectedObjects, *v)
	}
	sort.Slice(report.ProtectedObjects, func(i, j int) bool {
		return report.ProtectedObjects[i].ImageRepository+"@"+report.ProtectedObjects[i].Digest < report.ProtectedObjects[j].ImageRepository+"@"+report.ProtectedObjects[j].Digest
	})
	for _, e := range usedEdges {
		report.ProtectedEdges = append(report.ProtectedEdges, e)
	}
	sort.Slice(report.ProtectedEdges, func(i, j int) bool {
		return report.ProtectedEdges[i].ImageRepository+registryEdgeKey(report.ProtectedEdges[i].RegistryEdge) < report.ProtectedEdges[j].ImageRepository+registryEdgeKey(report.ProtectedEdges[j].RegistryEdge)
	})
	for _, summary := range report.Registry {
		for _, b := range summary.Blockers {
			if strings.HasPrefix(b, "registry_") && b != "registry_global_inventory_unproven" {
				report.Blockers = append(report.Blockers, b)
			}
		}
	}
	report.Blockers = append(report.Blockers, "registry_global_inventory_unproven", "deletion_fence_unavailable")
	report.Blockers = sortedUnique(report.Blockers)
	return sortedUnique(evidence)
}
