package domain

import (
	"encoding/json"
	"sort"
	"testing"
)

func imageContentDomainFixture(t *testing.T) (RegistryGraph, RegistryObserveLimits, []string) {
	t.Helper()
	st, a := registryDomainFixture(t)
	g := st.RegistryInventories[0].Report.Graph
	root := retentionSHA(300)
	desc := func(n int, kind, media string, p *RegistryPlatform) RegistryImageContentDescriptor {
		return RegistryImageContentDescriptor{Digest: retentionSHA(n), Kind: kind, MediaType: media, Size: 32, Platform: p}
	}
	manifest := func(n int, kind, media string, descs ...RegistryImageContentDescriptor) RegistryImageContentManifest {
		v := RegistryImageContentManifest{Digest: retentionSHA(n), Kind: kind, MediaType: media, Size: 32, Descriptors: descs, DescriptorCount: len(descs)}
		v.DescriptorHash = RegistryImageContentDescriptorHash(descs)
		return v
	}
	image, index, blob := "application/vnd.oci.image.manifest.v1+json", "application/vnd.oci.image.index.v1+json", "application/octet-stream"
	m := []RegistryImageContentManifest{
		manifest(300, "INDEX", index, desc(301, "CHILD", image, &RegistryPlatform{OS: "linux", Architecture: "arm64"}), desc(306, "CHILD", index, &RegistryPlatform{OS: "linux", Architecture: "amd64"})),
		manifest(301, "MANIFEST", image, desc(303, "CONFIG", blob, nil), desc(304, "LAYER", blob, nil)),
		manifest(302, "MANIFEST", image, desc(303, "CONFIG", blob, nil), desc(304, "LAYER", blob, nil)),
		manifest(306, "INDEX", index, desc(302, "CHILD", image, nil)),
	}
	p := RegistryImageContentRoot{Digest: root, Closure: "COMPLETE", Manifests: m}
	p.Hash = RegistryImageContentRootHash(p)
	g.ImageContentRoots = []RegistryImageContentRoot{p}
	g.Roots[0].Closure = "PARTIAL"
	g.Roots[0].Blockers = []string{"registry_referrers_unknown"}
	g.Discovery = "PARTIAL"
	g.Blockers = append(g.Blockers, "registry_referrers_unknown")
	g, e := NormalizeRegistryGraph(g)
	if e != nil {
		t.Fatal(e)
	}
	g.GraphHash = RegistryGraphHash(g)
	return g, a.Limits, []string{root}
}

func rehashContentTest(g *RegistryGraph) {
	for i := range g.ImageContentRoots {
		root := &g.ImageContentRoots[i]
		for j := range root.Manifests {
			m := &root.Manifests[j]
			m.DescriptorCount = len(m.Descriptors)
			m.DescriptorHash = RegistryImageContentDescriptorHash(m.Descriptors)
		}
		sort.Slice(root.Manifests, func(i, j int) bool { return root.Manifests[i].Digest < root.Manifests[j].Digest })
		root.Hash = RegistryImageContentRootHash(*root)
	}
	normalized, _ := NormalizeRegistryGraph(*g)
	*g = normalized
	g.GraphHash = RegistryGraphHash(*g)
}

func TestRegistryImageContentIndependentFromMetadataAndHistoricalInventory(t *testing.T) {
	g, l, roots := imageContentDomainFixture(t)
	if ValidateRegistryGraph(g, l, roots) != nil {
		t.Fatal("valid typed content rejected")
	}
	proof, e := RegistryImageContentProofHash(g, l, roots, roots[0])
	if e != nil || proof != g.ImageContentRoots[0].Hash {
		t.Fatal("complete pull content lost", e)
	}
	if g.Discovery != "PARTIAL" || g.Roots[0].Closure != "PARTIAL" || g.GlobalInventoryComplete {
		t.Fatal("content upgraded metadata completeness")
	}
	historical := g
	historical.ImageContentRoots = nil
	historical.GraphHash = RegistryGraphHash(historical)
	if ValidateRegistryGraph(historical, l, roots) != nil {
		t.Fatal("old inventory no longer valid")
	}
	if _, e = RegistryImageContentProofHash(historical, l, roots, roots[0]); e == nil {
		t.Fatal("generic historical inventory became pull proof")
	}
	if historical.GraphHash == g.GraphHash {
		t.Fatal("new attestation absent from graph hash")
	}
	// omitempty must preserve the old hash shape for records lacking this field.
	raw, _ := json.Marshal(historical)
	var fields map[string]json.RawMessage
	json.Unmarshal(raw, &fields)
	if _, ok := fields["image_content_roots"]; ok {
		t.Fatal("historical hash shape changed")
	}
}

func TestRegistryImageContentRejectsIncompleteOrMalformedAttestation(t *testing.T) {
	for _, mode := range []string{"hash", "descriptor-hash", "count", "descriptor-omitted", "edge-omitted", "node-missing", "config-missing", "layer-missing", "wrong-size", "wrong-media", "wrong-kind", "config-count", "partial", "unrequested", "duplicate-root", "duplicate-manifest", "unreachable", "depth-limit", "node-limit", "noncontent-edge", "extra-graph-edge"} {
		t.Run(mode, func(t *testing.T) {
			g, l, roots := imageContentDomainFixture(t)
			p := &g.ImageContentRoots[0]
			rehash := true
			switch mode {
			case "hash":
				p.Hash = "bad"
				rehash = false
			case "descriptor-hash":
				p.Manifests[1].DescriptorHash = "bad"
				rehash = false
			case "count":
				p.Manifests[1].DescriptorCount++
				rehash = false
			case "descriptor-omitted":
				p.Manifests[1].Descriptors = p.Manifests[1].Descriptors[:1]
			case "edge-omitted":
				for i, e := range g.Edges {
					if e.Kind == "LAYER" {
						g.Edges = append(g.Edges[:i], g.Edges[i+1:]...)
						break
					}
				}
			case "node-missing":
				g.Nodes = g.Nodes[1:]
			case "config-missing":
				for i := range g.Nodes {
					if g.Nodes[i].Digest == retentionSHA(303) {
						g.Nodes[i].Availability = "MISSING"
					}
				}
			case "layer-missing":
				for i := range g.Nodes {
					if g.Nodes[i].Digest == retentionSHA(304) {
						g.Nodes[i].Availability = "UNKNOWN"
					}
				}
			case "wrong-size":
				p.Manifests[1].Descriptors[0].Size++
			case "wrong-media":
				p.Manifests[1].MediaType = "application/unsupported"
			case "wrong-kind":
				p.Manifests[1].Kind = "INDEX"
			case "config-count":
				p.Manifests[1].Descriptors[0].Kind = "LAYER"
			case "partial":
				p.Closure = "PARTIAL"
			case "unrequested":
				p.Digest = retentionSHA(301)
			case "duplicate-root":
				g.ImageContentRoots = append(g.ImageContentRoots, *p)
			case "duplicate-manifest":
				p.Manifests = append(p.Manifests, p.Manifests[0])
			case "unreachable":
				extra := p.Manifests[1]
				extra.Digest = retentionSHA(305)
				p.Manifests = append(p.Manifests, extra)
			case "depth-limit":
				l.MaxDepth = 1
			case "node-limit":
				l.MaxNodes = 3
			case "noncontent-edge":
				p.Manifests[1].Descriptors[0].Kind = "SUBJECT"
			case "extra-graph-edge":
				g.Edges = append(g.Edges, RegistryEdge{From: retentionSHA(301), To: retentionSHA(303), Kind: "LAYER"})
			}
			if rehash {
				rehashContentTest(&g)
			} else {
				g.GraphHash = RegistryGraphHash(g)
			}
			if _, e := RegistryImageContentProofHash(g, l, roots, roots[0]); e == nil {
				t.Fatal("malformed/missing bytes became COMPLETE")
			}
		})
	}
}

func TestRegistryImageContentPreservesRepeatedLayerDescriptors(t *testing.T) {
	g, l, roots := imageContentDomainFixture(t)
	m := &g.ImageContentRoots[0].Manifests[1]
	m.Descriptors = append(m.Descriptors, m.Descriptors[1])
	rehashContentTest(&g)
	if _, e := RegistryImageContentProofHash(g, l, roots, roots[0]); e != nil {
		t.Fatal("legitimate repeated layer rejected", e)
	}
}

func TestRegistryImageContentCannotOverrideReadFailureBlockers(t *testing.T) {
	for _, code := range []string{"registry_unauthorized", "registry_request_failed", "registry_digest_mismatch", "registry_descriptor_conflict", "registry_unsupported", "registry_malformed", "registry_limit", "registry_cancelled", "registry_discovery_changed", "registry_pagination", "registry_filtered_referrers"} {
		t.Run(code, func(t *testing.T) {
			g, l, roots := imageContentDomainFixture(t)
			g.Blockers = append(g.Blockers, code)
			g, _ = NormalizeRegistryGraph(g)
			g.GraphHash = RegistryGraphHash(g)
			if _, e := RegistryImageContentProofHash(g, l, roots, roots[0]); e == nil {
				t.Fatal("complete-looking typed proof overrode failed read", code)
			}
		})
	}
}
