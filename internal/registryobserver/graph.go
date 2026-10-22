// Package registryobserver performs bounded, GET-only observations. Its output
// is a scoped worker attestation, never a complete registry inventory.
package registryobserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/url"
	"regexp"
	"sort"
	"time"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/ocipublisher"
	"releasecontrol/internal/workerconfig"
)

const ociIndex = "application/vnd.oci.image.index.v1+json"
const ociManifest = "application/vnd.oci.image.manifest.v1+json"
const dockerIndex = "application/vnd.docker.distribution.manifest.list.v2+json"
const dockerManifest = "application/vnd.docker.distribution.manifest.v2+json"

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var tagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
var errScan = errors.New("registry_observation_failed")

type descriptor struct {
	MediaType string                   `json:"mediaType"`
	Digest    string                   `json:"digest"`
	Size      int64                    `json:"size"`
	URLs      []string                 `json:"urls,omitempty"`
	Platform  *domain.RegistryPlatform `json:"platform,omitempty"`
}
type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []descriptor `json:"manifests"`
	Config        *descriptor  `json:"config"`
	Layers        []descriptor `json:"layers"`
	Subject       *descriptor  `json:"subject"`
	ArtifactType  string       `json:"artifactType"`
}
type scanner struct {
	ctx             context.Context
	c               *ocipublisher.ReadClient
	recipe          workerconfig.RegistryObserveRecipe
	in              domain.WorkerRegistryInputs
	nodes           map[string]domain.RegistryNode
	edges           map[string]domain.RegistryEdge
	roots           map[string]bool
	visited         map[string]bool
	depths          map[string]int
	subjects        map[string]string
	media           map[string]string
	refsets         map[string]string
	blockers        map[string]bool
	requests, pages int
	size            int64
	tagCount        int
	tagHash         string
	discovery       string
	graphBytes      int
	content         map[string]domain.RegistryImageContentManifest
	contentBytes    int
}

func hash(b []byte) string           { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }
func (s *scanner) block(code string) { s.blockers[code] = true }
func (s *scanner) node(d, kind, media, availability string, size int64) bool {
	if !digestPattern.MatchString(d) {
		s.block("registry_unsupported")
		return false
	}
	if old, ok := s.nodes[d]; ok {
		if old.Kind != "UNKNOWN" && kind != "UNKNOWN" && old.Kind != kind || old.Kind != "UNKNOWN" && kind != "UNKNOWN" && old.Size != size || old.MediaType != "" && media != "" && old.MediaType != media {
			s.block("registry_descriptor_conflict")
			return false
		}
		if kind == "UNKNOWN" {
			kind = old.Kind
		}
		if media == "" {
			media = old.MediaType
		}
		if size == 0 {
			size = old.Size
		}
		if old.Availability == "PRESENT" && availability == "UNKNOWN" {
			availability = old.Availability
		}
	} else if len(s.nodes) >= s.recipe.Limits.MaxNodes {
		s.block("registry_limit")
		return false
	}
	v := domain.RegistryNode{Digest: d, Kind: kind, MediaType: media, Size: size, Availability: availability}
	b, _ := json.Marshal(v)
	oldBytes := 0
	if old, ok := s.nodes[d]; ok {
		ob, _ := json.Marshal(old)
		oldBytes = len(ob) + 1
	}
	delta := len(b) + 1 - oldBytes
	if s.graphBytes+delta > domain.RegistryReportMaxBytes-8192 {
		s.block("registry_limit")
		return false
	}
	s.graphBytes += delta
	s.nodes[d] = v
	return true
}
func (s *scanner) edge(from, to, kind string, p *domain.RegistryPlatform) {
	if _, ok := s.nodes[from]; !ok {
		return
	}
	if _, ok := s.nodes[to]; !ok {
		return
	}
	f, t := s.nodes[from], s.nodes[to]
	if kind == "CHILD" && (f.Kind != "INDEX" || t.Kind == "BLOB") || (kind == "CONFIG" || kind == "LAYER") && (f.Kind != "MANIFEST" || (t.Kind != "BLOB" && t.Kind != "UNKNOWN")) || (kind == "SUBJECT" || kind == "REFERRER" || kind == "REFERRERS_INDEX") && (f.Kind == "BLOB" || t.Kind == "BLOB") {
		s.block("registry_descriptor_conflict")
		return
	}
	v := domain.RegistryEdge{From: from, To: to, Kind: kind, Platform: p}
	b, _ := json.Marshal(v)
	k := string(b)
	if _, ok := s.edges[k]; ok {
		return
	}
	if len(s.edges) >= s.recipe.Limits.MaxEdges {
		s.block("registry_limit")
		return
	}
	if s.graphBytes+len(b)+1 > domain.RegistryReportMaxBytes-8192 {
		s.block("registry_limit")
		return
	}
	s.graphBytes += len(b) + 1
	s.edges[k] = v
}
func (s *scanner) get(kind, ref string, q url.Values) (ocipublisher.ReadResponse, bool) {
	if s.ctx.Err() != nil {
		s.block("registry_cancelled")
		return ocipublisher.ReadResponse{}, false
	}
	if s.requests >= s.recipe.Limits.MaxRequests || s.size >= s.recipe.Limits.MaxTotalBytes {
		s.block("registry_limit")
		return ocipublisher.ReadResponse{}, false
	}
	s.requests++
	r, e := s.c.Get(s.ctx, kind, ref, q)
	if e != nil {
		s.block("registry_request_failed")
		return r, false
	}
	return r, true
}
func (s *scanner) read(r ocipublisher.ReadResponse) ([]byte, bool) {
	defer r.Body.Close()
	remaining := s.recipe.Limits.MaxTotalBytes - s.size
	if remaining <= 0 {
		s.block("registry_limit")
		return nil, false
	}
	max := s.recipe.Limits.MaxResponseBytes
	if max > 16<<20 {
		max = 16 << 20
	}
	if max > remaining {
		max = remaining
	}
	b, e := io.ReadAll(io.LimitReader(r.Body, max+1))
	n := int64(len(b))
	if n > max {
		s.size += max
		s.block("registry_limit")
		return nil, false
	}
	s.size += n
	if e != nil {
		s.block("registry_limit")
		return nil, false
	}
	return b, true
}
func (s *scanner) status(status int) bool {
	switch status {
	case 200:
		return true
	case 401, 403:
		s.block("registry_unauthorized")
	case 404:
		s.block("registry_missing")
	default:
		s.block("registry_request_failed")
	}
	return false
}
func (s *scanner) fetch(ref string) ([]byte, string, bool) {
	r, ok := s.get("manifests", ref, nil)
	if !ok {
		return nil, "", false
	}
	if !s.status(r.Status) {
		r.Body.Close()
		return nil, "", false
	}
	b, ok := s.read(r)
	if !ok {
		return nil, "", false
	}
	d := hash(b)
	if digestPattern.MatchString(ref) && ref != d || r.Digest != "" && r.Digest != d {
		s.block("registry_digest_mismatch")
		return nil, "", false
	}
	var m manifest
	if !safeJSON(b) || json.Unmarshal(b, &m) != nil || m.SchemaVersion != 2 {
		s.block("registry_malformed")
		return nil, "", false
	}
	if r.ContentType != "" {
		ct, _, e := mime.ParseMediaType(r.ContentType)
		if e != nil || m.MediaType != "" && ct != m.MediaType || manifestKind(ct) == "UNKNOWN" {
			s.block("registry_descriptor_conflict")
			return nil, "", false
		}
	}
	if m.MediaType != "" {
		s.media[d] = m.MediaType
	} else if r.ContentType != "" {
		ct, _, _ := mime.ParseMediaType(r.ContentType)
		s.media[d] = ct
	}
	return b, d, true
}
func manifestKind(media string) string {
	switch media {
	case ociIndex, dockerIndex:
		return "INDEX"
	case ociManifest, dockerManifest:
		return "MANIFEST"
	}
	return "UNKNOWN"
}
func (s *scanner) validDesc(d descriptor) bool {
	if d.Size > s.recipe.Limits.MaxResponseBytes {
		s.block("registry_limit")
		return false
	}
	if d.Platform != nil {
		for _, v := range []string{d.Platform.OS, d.Platform.Architecture, d.Platform.Variant} {
			if v != "" && !regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`).MatchString(v) {
				s.block("registry_malformed")
				return false
			}
		}
	}
	if !digestPattern.MatchString(d.Digest) || d.Size < 0 || d.MediaType == "" || len(d.MediaType) > 128 || !regexp.MustCompile(`^[a-z0-9!#$&^_.+-]+/[a-z0-9!#$&^_.+-]+$`).MatchString(d.MediaType) || len(d.URLs) > 0 {
		s.block("registry_unsupported")
		return false
	}
	return true
}
func (s *scanner) blob(d descriptor) {
	if !s.validDesc(d) {
		return
	}
	if !s.node(d.Digest, "BLOB", d.MediaType, "UNKNOWN", d.Size) {
		return
	}
	if s.nodes[d.Digest].Availability == "PRESENT" {
		return
	}
	if d.Size > s.recipe.Limits.MaxResponseBytes || d.Size > s.recipe.Limits.MaxTotalBytes-s.size {
		s.block("registry_limit")
		return
	}
	r, ok := s.get("blobs", d.Digest, nil)
	if !ok {
		return
	}
	defer r.Body.Close()
	if !s.status(r.Status) {
		if r.Status == 404 {
			s.node(d.Digest, "BLOB", d.MediaType, "MISSING", d.Size)
		}
		return
	}
	h := sha256.New()
	n, e := io.Copy(h, io.LimitReader(r.Body, d.Size+1))
	s.size += n
	if s.size > s.recipe.Limits.MaxTotalBytes {
		s.size = s.recipe.Limits.MaxTotalBytes
		s.block("registry_limit")
		return
	}
	if e != nil || n != d.Size || "sha256:"+hex.EncodeToString(h.Sum(nil)) != d.Digest || r.Digest != "" && r.Digest != d.Digest {
		s.block("registry_digest_mismatch")
		return
	}
	s.node(d.Digest, "BLOB", d.MediaType, "PRESENT", d.Size)
}
func (s *scanner) visit(d string, expected *descriptor, depth int, raw []byte) {
	if depth > s.recipe.Limits.MaxDepth {
		s.block("registry_limit")
		return
	}
	if !s.node(d, "UNKNOWN", "", "UNKNOWN", 0) {
		return
	}
	if expected != nil {
		if !s.validDesc(*expected) || !s.node(d, manifestKind(expected.MediaType), expected.MediaType, "UNKNOWN", expected.Size) {
			return
		}
	}
	if s.visited[d] {
		return
	}
	s.visited[d] = true
	s.depths[d] = depth
	if raw == nil {
		var ok bool
		raw, _, ok = s.fetch(d)
		if !ok {
			return
		}
	}
	if expected != nil && (int64(len(raw)) != expected.Size || hash(raw) != expected.Digest) {
		s.block("registry_descriptor_conflict")
		return
	}
	var m manifest
	if !safeJSON(raw) || json.Unmarshal(raw, &m) != nil || m.SchemaVersion != 2 {
		s.block("registry_malformed")
		return
	}
	if m.MediaType == "" {
		m.MediaType = s.media[d]
		if m.MediaType == "" && expected != nil {
			m.MediaType = expected.MediaType
		}
	}
	kind := manifestKind(m.MediaType)
	if kind == "UNKNOWN" {
		s.block("registry_unsupported")
		return
	}
	s.recordImageContent(d, m, int64(len(raw)))
	if !s.node(d, kind, m.MediaType, "PRESENT", int64(len(raw))) {
		return
	}
	if depth >= s.recipe.Limits.MaxDepth {
		s.block("registry_limit")
		return
	}
	if kind == "INDEX" {
		if m.Config != nil || m.Layers != nil || m.Manifests == nil {
			s.block("registry_malformed")
			return
		}
		for _, child := range m.Manifests {
			if !s.validDesc(child) {
				continue
			}
			s.visit(child.Digest, &child, depth+1, nil)
			s.edge(d, child.Digest, "CHILD", child.Platform)
		}
	} else {
		if m.Config == nil || m.Layers == nil || m.Manifests != nil {
			s.block("registry_malformed")
			return
		}
		s.blob(*m.Config)
		s.edge(d, m.Config.Digest, "CONFIG", nil)
		for _, layer := range m.Layers {
			s.blob(layer)
			s.edge(d, layer.Digest, "LAYER", nil)
		}
	}
	if m.Subject != nil && s.validDesc(*m.Subject) {
		s.subjects[d] = m.Subject.Digest
		s.visit(m.Subject.Digest, m.Subject, depth+1, nil)
		s.edge(d, m.Subject.Digest, "SUBJECT", nil)
	}
	refs, fp := s.referrers(d, depth)
	s.refsets[d] = fp
	for _, ref := range refs {
		s.visit(ref.Digest, &ref, depth+1, nil)
		if s.subjects[ref.Digest] != d {
			s.block("registry_descriptor_conflict")
			continue
		}
		s.edge(d, ref.Digest, "REFERRER", nil)
	}
}

// Observe completes a bounded scan, including partial/unknown facts. It never
// renews the supplied earliest observation time on recovery.
func Observe(ctx context.Context, r workerconfig.RegistryObserveRecipe, in domain.WorkerRegistryInputs, started time.Time) (*domain.WorkerRegistryReport, error) {
	if workerconfig.ValidateObservation(r) != nil || domain.ValidateRegistryScope(in.Scope) != nil || !workerconfig.ValidFingerprint(in.RootSetHash) || !workerconfig.ValidFingerprint(in.RequestRetentionInputHash) || in.Scope.ProductID != r.ProductID || in.Scope.ComponentID != r.ComponentID || in.Scope.RepositoryID != r.RepositoryID || in.Scope.Origin != r.Origin || in.Scope.Repository != r.Repository || in.Limits != r.Limits || started.IsZero() || started.After(time.Now().UTC()) {
		return nil, errScan
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.Limits.TimeoutSeconds)*time.Second)
	defer cancel()
	c, e := ocipublisher.NewReadClient(ocipublisher.ReadConfig{Origin: r.Origin, Repository: r.Repository, CAFile: r.Pull.CAFile, UsernameEnv: r.Pull.UsernameEnv, PasswordEnv: r.Pull.PasswordEnv, BearerEnv: r.Pull.BearerEnv, MaxBytes: r.Limits.MaxResponseBytes, TimeoutSeconds: r.Limits.TimeoutSeconds, FixtureLoopbackHTTP: r.Pull.FixtureLoopbackHTTP})
	if e != nil {
		return nil, errScan
	}
	defer c.Close()
	if len(in.Roots) > 256 {
		return nil, errScan
	}
	s := &scanner{content: map[string]domain.RegistryImageContentManifest{}, graphBytes: len(in.Roots) * 200, ctx: ctx, c: c, recipe: r, in: in, nodes: map[string]domain.RegistryNode{}, edges: map[string]domain.RegistryEdge{}, roots: map[string]bool{}, visited: map[string]bool{}, depths: map[string]int{}, subjects: map[string]string{}, media: map[string]string{}, refsets: map[string]string{}, blockers: map[string]bool{}, discovery: "COMPLETE"}
	for _, d := range in.Roots {
		if !digestPattern.MatchString(d) || !s.node(d, "UNKNOWN", "", "UNKNOWN", 0) {
			return nil, errScan
		}
		s.roots[d] = true
	}
	for _, d := range in.Roots {
		s.visit(d, nil, 0, nil)
	}
	aliases := s.tags()
	s.tagCount = len(aliases)
	s.tagHash = s.resolveTags(aliases, true)
	final := s.resolveTags(s.tags(), true)
	if final != s.tagHash {
		s.block("registry_discovery_changed")
	}
	keys := make([]string, 0, len(s.refsets))
	for d := range s.refsets {
		keys = append(keys, d)
	}
	sort.Strings(keys)
	for _, d := range keys {
		refs, fp := s.referrers(d, s.depths[d])
		if fp != s.refsets[d] {
			s.block("registry_discovery_changed")
			for _, ref := range refs {
				s.visit(ref.Digest, &ref, s.depths[d]+1, nil)
				if s.subjects[ref.Digest] == d {
					s.edge(d, ref.Digest, "REFERRER", nil)
				}
			}
		}
	}
	if ctx.Err() != nil {
		return nil, errScan
	}
	g := domain.RegistryGraph{Nodes: []domain.RegistryNode{}, Edges: []domain.RegistryEdge{}, Roots: []domain.RegistryRoot{}, TagCount: s.tagCount, TagSetHash: s.tagHash, Discovery: s.discovery, GlobalInventoryComplete: false, Blockers: []string{"registry_global_inventory_unproven"}}
	codes := []string{}
	for code := range s.blockers {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	g.Blockers = append(g.Blockers, codes...)
	closure := "COMPLETE"
	if len(codes) > 0 {
		closure = "PARTIAL"
		g.Discovery = "PARTIAL"
	}
	for _, n := range s.nodes {
		g.Nodes = append(g.Nodes, n)
	}
	for _, v := range s.edges {
		g.Edges = append(g.Edges, v)
	}
	for d := range s.roots {
		g.Roots = append(g.Roots, domain.RegistryRoot{Digest: d, Closure: closure, Blockers: representative(codes)})
	}
	g.ImageContentRoots = s.imageContentRoots()
	// Optional content proofs may not make an otherwise bounded historical graph
	// oversized. Omitting them grants no image-content capability to its consumer.
	encoded, _ := json.Marshal(g)
	if len(encoded) > domain.RegistryReportMaxBytes-2048 {
		g.ImageContentRoots = nil
	}
	g, e = domain.NormalizeRegistryGraph(g)
	if e != nil {
		return nil, errScan
	}
	g.GraphHash = domain.RegistryGraphHash(g)
	if domain.ValidateRegistryGraph(g, r.Limits, in.Roots) != nil {
		return nil, errScan
	}
	return &domain.WorkerRegistryReport{Kind: "WORKER_ATTESTED_REGISTRY_READ", Scope: in.Scope, RecipeID: r.ID, RecipeRevision: r.Revision, RecipeHash: r.Hash, RootSetHash: in.RootSetHash, StartedAt: started, FinishedAt: time.Now().UTC(), Requests: s.requests, Bytes: s.size, Graph: g}, nil
}
func (s *scanner) resolveTags(tags []string, include bool) string {
	values := make([]string, 0, len(tags))
	for _, tag := range tags {
		raw, d, ok := s.fetch(tag)
		if !ok {
			values = append(values, tag+"=UNKNOWN")
			continue
		}
		values = append(values, tag+"="+d)
		if include {
			s.visit(d, nil, 0, raw)
			if _, ok := s.nodes[d]; ok && !s.roots[d] {
				if s.graphBytes+200 > domain.RegistryReportMaxBytes-8192 {
					s.block("registry_limit")
				} else {
					s.graphBytes += 200
					s.roots[d] = true
				}
			}
		}
	}
	sort.Strings(values)
	b, _ := json.Marshal(values)
	return hash(b)
}

func representative(codes []string) []string {
	if len(codes) == 0 {
		return []string{}
	}
	return []string{codes[0]}
}
