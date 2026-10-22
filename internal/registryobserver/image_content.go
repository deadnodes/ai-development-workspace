package registryobserver

import (
	"encoding/json"
	"sort"

	"releasecontrol/internal/domain"
)

// Record the FULL parsed descriptor list before any graph insertion can drop a
// node or edge. Surviving graph subsets never establish expected completeness.
func (s *scanner) recordImageContent(d string, m manifest, size int64) {
	kind := manifestKind(m.MediaType)
	record := domain.RegistryImageContentManifest{Digest: d, Kind: kind, MediaType: m.MediaType, Size: size, Descriptors: []domain.RegistryImageContentDescriptor{}}
	add := func(desc descriptor, edge string) bool {
		if !s.validDesc(desc) {
			return false
		}
		if edge == "CHILD" && manifestKind(desc.MediaType) == "UNKNOWN" {
			s.block("registry_unsupported")
			return false
		}
		record.Descriptors = append(record.Descriptors, domain.RegistryImageContentDescriptor{Digest: desc.Digest, Kind: edge, MediaType: desc.MediaType, Size: desc.Size, Platform: desc.Platform})
		return true
	}
	if kind == "INDEX" {
		if m.Config != nil || m.Layers != nil || len(m.Manifests) == 0 {
			return
		}
		if len(m.Manifests) > s.recipe.Limits.MaxEdges {
			s.block("registry_limit")
			return
		}
		for _, child := range m.Manifests {
			if !add(child, "CHILD") {
				return
			}
		}
	} else if kind == "MANIFEST" {
		if m.Config == nil || m.Layers == nil || m.Manifests != nil {
			return
		}
		if len(m.Layers)+1 > s.recipe.Limits.MaxEdges {
			s.block("registry_limit")
			return
		}
		if !add(*m.Config, "CONFIG") {
			return
		}
		for _, layer := range m.Layers {
			if !add(layer, "LAYER") {
				return
			}
		}
	} else {
		return
	}
	record.DescriptorCount = len(record.Descriptors)
	record.DescriptorHash = domain.RegistryImageContentDescriptorHash(record.Descriptors)
	b, _ := json.Marshal(record)
	if s.contentBytes+len(b) > domain.RegistryReportMaxBytes {
		s.block("registry_limit")
		return
	}
	s.contentBytes += len(b)
	s.content[d] = record
}

func (s *scanner) imageContentRoots() []domain.RegistryImageContentRoot {
	roots := make([]domain.RegistryImageContentRoot, 0, len(s.in.Roots))
	// Metadata-only incoming-referrer uncertainty cannot invalidate verified pull
	// bytes. Every other scan failure remains conservative, including limits and
	// changed mutable tag identities. Explicit descriptor proofs are still required.
	clean := s.ctx.Err() == nil
	for code := range s.blockers {
		// A missing fallback/referrer and a missing pull blob share the legacy
		// metadata code. Exact expected-descriptor traversal below independently
		// rejects every missing pull byte; it never relies on this code's absence.
		if code != "registry_referrers_unknown" && code != "registry_missing" {
			clean = false
		}
	}
	for _, digest := range s.in.Roots {
		proof := domain.RegistryImageContentRoot{Digest: digest, Closure: "PARTIAL", Manifests: []domain.RegistryImageContentManifest{}}
		if clean {
			seen, active := map[string]bool{}, map[string]bool{}
			var collect func(string, int) bool
			collect = func(d string, depth int) bool {
				if depth > s.recipe.Limits.MaxDepth || active[d] {
					return false
				}
				if seen[d] {
					return true
				}
				record, ok := s.content[d]
				if !ok {
					return false
				}
				node, ok := s.nodes[d]
				if !ok || node.Availability != "PRESENT" {
					return false
				}
				active[d] = true
				for _, desc := range record.Descriptors {
					target, ok := s.nodes[desc.Digest]
					if !ok || target.Availability != "PRESENT" || target.Size != desc.Size || target.MediaType != desc.MediaType {
						return false
					}
					if desc.Kind == "CHILD" && !collect(desc.Digest, depth+1) {
						return false
					}
				}
				delete(active, d)
				seen[d] = true
				proof.Manifests = append(proof.Manifests, record)
				return true
			}
			if collect(digest, 0) {
				proof.Closure = "COMPLETE"
				sort.Slice(proof.Manifests, func(i, j int) bool { return proof.Manifests[i].Digest < proof.Manifests[j].Digest })
			} else {
				proof.Manifests = []domain.RegistryImageContentManifest{}
			}
		}
		proof.Hash = domain.RegistryImageContentRootHash(proof)
		roots = append(roots, proof)
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].Digest < roots[j].Digest })
	return roots
}
