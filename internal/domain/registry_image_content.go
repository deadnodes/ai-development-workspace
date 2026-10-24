package domain

import (
	"fmt"
	"reflect"
	"sort"
)

const RegistryImageContentKind = "IMAGE_CONTENT"

// RegistryImageContentRoot attests pull-required bytes only. Incoming referrers,
// subjects and mutable discovery indexes are deliberately outside this proof.
type RegistryImageContentRoot struct {
	Digest    string                         `json:"digest"`
	Closure   string                         `json:"closure"`
	Manifests []RegistryImageContentManifest `json:"manifests"`
	Hash      string                         `json:"hash"`
}
type RegistryImageContentManifest struct {
	Digest          string                           `json:"digest"`
	Kind            string                           `json:"kind"`
	MediaType       string                           `json:"media_type"`
	Size            int64                            `json:"size"`
	DescriptorCount int                              `json:"descriptor_count"`
	DescriptorHash  string                           `json:"descriptor_hash"`
	Descriptors     []RegistryImageContentDescriptor `json:"descriptors"`
}
type RegistryImageContentDescriptor struct {
	Digest    string            `json:"digest"`
	Kind      string            `json:"kind"`
	MediaType string            `json:"media_type"`
	Size      int64             `json:"size"`
	Platform  *RegistryPlatform `json:"platform,omitempty"`
}

func RegistryImageContentDescriptorHash(descriptors []RegistryImageContentDescriptor) string {
	return checkHash(descriptors)
}
func RegistryImageContentRootHash(root RegistryImageContentRoot) string {
	root.Hash = ""
	return checkHash(root)
}

func boundedImageContent(roots []RegistryImageContentRoot, l RegistryObserveLimits) bool {
	if len(roots) > 256 {
		return false
	}
	manifests, descriptors := 0, 0
	for _, root := range roots {
		if len(root.Digest) > 71 || len(root.Closure) > 8 || len(root.Hash) > 71 || len(root.Manifests) > l.MaxNodes {
			return false
		}
		manifests += len(root.Manifests)
		if manifests > RegistryReportMaxBytes/64 {
			return false
		}
		for _, m := range root.Manifests {
			if len(m.Digest) > 71 || len(m.Kind) > 8 || len(m.MediaType) > 128 || len(m.DescriptorHash) > 71 || len(m.Descriptors) > l.MaxEdges {
				return false
			}
			descriptors += len(m.Descriptors)
			if descriptors > RegistryReportMaxBytes/64 {
				return false
			}
			for _, d := range m.Descriptors {
				if len(d.Digest) > 71 || len(d.Kind) > 8 || len(d.MediaType) > 128 {
					return false
				}
				if d.Platform != nil && (len(d.Platform.OS) > 32 || len(d.Platform.Architecture) > 32 || len(d.Platform.Variant) > 32) {
					return false
				}
			}
		}
	}
	return true
}

func normalizeImageContent(roots []RegistryImageContentRoot) []RegistryImageContentRoot {
	if roots == nil {
		return nil
	}
	out := append([]RegistryImageContentRoot{}, roots...)
	for i := range out {
		out[i].Manifests = append([]RegistryImageContentManifest{}, out[i].Manifests...)
		for j := range out[i].Manifests {
			m := &out[i].Manifests[j]
			m.Descriptors = append([]RegistryImageContentDescriptor{}, m.Descriptors...)
			for k := range m.Descriptors {
				if m.Descriptors[k].Platform != nil {
					v := *m.Descriptors[k].Platform
					m.Descriptors[k].Platform = &v
				}
			}
		}
		sort.Slice(out[i].Manifests, func(a, b int) bool { return out[i].Manifests[a].Digest < out[i].Manifests[b].Digest })
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Digest < out[j].Digest })
	return out
}

func validateImageContent(g RegistryGraph, l RegistryObserveLimits, requested []string) error {
	fail := func() error { return fmt.Errorf("invalid image content attestation") }
	if len(g.ImageContentRoots) == 0 {
		return nil
	} // Historical inventory grants no content capability.
	expected := sortedUnique(append([]string{}, requested...))
	if len(g.ImageContentRoots) != len(expected) || len(expected) > 256 || !reflect.DeepEqual(g.ImageContentRoots, normalizeImageContent(g.ImageContentRoots)) {
		return fail()
	}
	nodes := map[string]RegistryNode{}
	for _, n := range g.Nodes {
		nodes[n.Digest] = n
	}
	edges := map[string]bool{}
	outgoing := map[string]map[string]bool{}
	for _, e := range g.Edges {
		if e.Kind == "CHILD" || e.Kind == "CONFIG" || e.Kind == "LAYER" {
			key := registryEdgeKey(e)
			edges[key] = true
			if outgoing[e.From] == nil {
				outgoing[e.From] = map[string]bool{}
			}
			outgoing[e.From][key] = true
		}
	}
	for i, root := range g.ImageContentRoots {
		if root.Digest != expected[i] || root.Hash != RegistryImageContentRootHash(root) || (root.Closure != "COMPLETE" && root.Closure != "PARTIAL") {
			return fail()
		}
		if root.Closure != "COMPLETE" {
			if len(root.Manifests) != 0 {
				return fail()
			}
			continue
		}
		allowedCode := func(code string) bool {
			return code == "registry_global_inventory_unproven" || code == "registry_referrers_unknown" || code == "registry_missing"
		}
		for _, code := range g.Blockers {
			if !allowedCode(code) {
				return fail()
			}
		}
		for _, metadata := range g.Roots {
			for _, code := range metadata.Blockers {
				if !allowedCode(code) {
					return fail()
				}
			}
		}
		if len(root.Manifests) == 0 || len(root.Manifests) > l.MaxNodes {
			return fail()
		}
		manifests := map[string]RegistryImageContentManifest{}
		for _, m := range root.Manifests {
			if _, duplicate := manifests[m.Digest]; duplicate {
				return fail()
			}
			n, ok := nodes[m.Digest]
			if !ok || n.Availability != "PRESENT" || n.Digest != m.Digest || n.Kind != m.Kind || n.MediaType != m.MediaType || n.Size != m.Size || m.Size < 1 || m.DescriptorCount != len(m.Descriptors) || m.DescriptorCount > l.MaxEdges || m.DescriptorHash != RegistryImageContentDescriptorHash(m.Descriptors) {
				return fail()
			}
			if (m.Kind == "INDEX" && (m.MediaType != "application/vnd.oci.image.index.v1+json" && m.MediaType != "application/vnd.docker.distribution.manifest.list.v2+json")) || (m.Kind == "MANIFEST" && (m.MediaType != "application/vnd.oci.image.manifest.v1+json" && m.MediaType != "application/vnd.docker.distribution.manifest.v2+json")) || (m.Kind != "INDEX" && m.Kind != "MANIFEST") {
				return fail()
			}
			configs := 0
			wanted := map[string]bool{}
			for _, d := range m.Descriptors {
				target, ok := nodes[d.Digest]
				if !ok || target.Availability != "PRESENT" || target.MediaType != d.MediaType || target.Size != d.Size || !retentionDigest.MatchString(d.Digest) || d.Size < 0 {
					return fail()
				}
				if m.Kind == "INDEX" {
					if d.Kind != "CHILD" || (target.Kind != "MANIFEST" && target.Kind != "INDEX") {
						return fail()
					}
				} else {
					if (d.Kind != "CONFIG" && d.Kind != "LAYER") || target.Kind != "BLOB" || d.Platform != nil {
						return fail()
					}
					if d.Kind == "CONFIG" {
						configs++
					}
				}
				key := registryEdgeKey(RegistryEdge{From: m.Digest, To: d.Digest, Kind: d.Kind, Platform: d.Platform})
				if !edges[key] {
					return fail()
				}
				wanted[key] = true
			}
			if (m.Kind == "INDEX" && len(m.Descriptors) == 0) || (m.Kind == "MANIFEST" && configs != 1) || !reflect.DeepEqual(wanted, outgoing[m.Digest]) {
				return fail()
			}
			manifests[m.Digest] = m
		}
		seen := map[string]bool{}
		active := map[string]bool{}
		var walk func(string, int) bool
		walk = func(d string, depth int) bool {
			if depth > l.MaxDepth || active[d] {
				return false
			}
			if seen[d] {
				return true
			}
			m, ok := manifests[d]
			if !ok {
				return false
			}
			active[d] = true
			for _, desc := range m.Descriptors {
				if desc.Kind == "CHILD" && !walk(desc.Digest, depth+1) {
					return false
				}
			}
			delete(active, d)
			seen[d] = true
			return true
		}
		if !walk(root.Digest, 0) || len(seen) != len(manifests) {
			return fail()
		}
	}
	return nil
}

// RegistryImageContentProofHash requires an explicit, validated requested-root
// attestation. Metadata COMPLETE alone and historical generic PASS are insufficient.
func RegistryImageContentProofHash(g RegistryGraph, l RegistryObserveLimits, requested []string, digest string) (string, error) {
	if ValidateRegistryGraph(g, l, requested) != nil {
		return "", fmt.Errorf("invalid image content graph")
	}
	for _, root := range g.ImageContentRoots {
		if root.Digest == digest && root.Closure == "COMPLETE" {
			return root.Hash, nil
		}
	}
	return "", fmt.Errorf("complete requested image content proof unavailable")
}
