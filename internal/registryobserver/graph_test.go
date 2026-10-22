package registryobserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/registryobserver/fixture"
	"releasecontrol/internal/workerconfig"
)

func limits() domain.RegistryObserveLimits {
	return domain.RegistryObserveLimits{MaxResponseBytes: 1 << 20, MaxTotalBytes: 16 << 20, MaxNodes: 128, MaxEdges: 256, MaxDepth: 16, MaxPages: 64, MaxRequests: 512, TimeoutSeconds: 30}
}
func recipeFor(origin string) workerconfig.RegistryObserveRecipe {
	r := workerconfig.RegistryObserveRecipe{ID: "observe", Revision: "v1", ProductID: "p", RepositoryID: "r", ComponentID: "a", Origin: origin, Repository: "test/component", Limits: limits(), Pull: workerconfig.RegistryRead{FixtureLoopbackHTTP: true}}
	r.Hash = workerconfig.ObservationHash(r)
	return r
}
func inputsFor(r workerconfig.RegistryObserveRecipe, roots []string) domain.WorkerRegistryInputs {
	image, _ := domain.CanonicalImageRepository(strings.TrimPrefix(strings.TrimPrefix(r.Origin, "http://"), "https://") + "/" + r.Repository)
	scope := domain.RegistryScope{ProductID: r.ProductID, RepositoryID: r.RepositoryID, ComponentID: r.ComponentID, Origin: r.Origin, Repository: r.Repository, ImageRepository: image}
	return domain.WorkerRegistryInputs{Scope: scope, Roots: roots, Limits: r.Limits, RootSetHash: domain.RegistryRootSetHash(scope, r.ID, r.Revision, r.Hash, "cfg1", roots), RequestRetentionInputHash: "sha256:" + strings.Repeat("a", 64)}
}
func TestObserveAllPlatformsNestedSharedLayersSubjectReferrersAndKnownUntagged(t *testing.T) {
	f := fixture.New()
	var writes atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes.Add(1)
		}
		f.Handler().ServeHTTP(w, r)
	}))
	defer s.Close()
	r := recipeFor(s.URL)
	in := inputsFor(r, f.Roots)
	start := time.Now().UTC()
	got, e := Observe(context.Background(), r, in, start)
	if e != nil {
		t.Fatal(e)
	}
	if domain.ValidateRegistryGraph(got.Graph, r.Limits, in.Roots) != nil {
		b, _ := json.Marshal(got.Graph)
		t.Fatalf("invalid graph %s", b)
	}
	if got.Graph.Discovery != "COMPLETE" || got.Graph.GlobalInventoryComplete || len(got.Graph.Blockers) != 1 || !got.StartedAt.Equal(start) || writes.Load() != 0 {
		t.Fatalf("bad scoped completion: %+v", got)
	}
	for _, d := range []string{f.Index, f.Nested, f.ARM, f.AMD, f.Referrer, f.SharedBlob, f.UntTagged} {
		found := false
		for _, n := range got.Graph.Nodes {
			if n.Digest == d && n.Availability == "PRESENT" {
				found = true
			}
		}
		if !found {
			t.Fatal("missing graph object", d)
		}
	}
	platforms := map[string]bool{}
	kinds := map[string]bool{}
	for _, edge := range got.Graph.Edges {
		kinds[edge.Kind] = true
		if edge.Platform != nil {
			platforms[edge.Platform.Architecture] = true
		}
	}
	for _, kind := range []string{"CHILD", "CONFIG", "LAYER", "SUBJECT", "REFERRER"} {
		if !kinds[kind] {
			t.Fatal(kind)
		}
	}
	if !platforms["arm64"] || !platforms["amd64"] || got.Graph.TagCount != 1 || len(got.Graph.Roots) != 2 {
		t.Fatal("platform/tag/untagged discovery lost")
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "latest") || strings.Contains(string(raw), "synthetic-") {
		t.Fatal("raw registry metadata persisted")
	}
}
func TestObserveConservativeFailuresAndNoForeignRequests(t *testing.T) {
	for _, mode := range []string{"401", "403", "500", "missing_blob", "corrupt_blob", "corrupt_manifest", "foreign_descriptor", "unknown_manifest", "foreign_next", "repeat_next", "filtered_referrers", "fallback", "tag_drift", "referrer_drift", "duplicate_json", "request_limit", "byte_limit", "node_limit", "depth_limit", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := fixture.New()
			var foreign atomic.Int64
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreign.Add(1); w.WriteHeader(200) }))
			defer other.Close()
			var tagCalls, refCalls atomic.Int64
			base := f.Handler()
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				p := r.URL.Path
				switch mode {
				case "401":
					w.WriteHeader(401)
					return
				case "403":
					w.WriteHeader(403)
					return
				case "500":
					w.WriteHeader(500)
					return
				case "missing_blob":
					if strings.Contains(p, "/blobs/") {
						w.WriteHeader(404)
						return
					}
				case "corrupt_blob":
					if strings.Contains(p, "/blobs/") {
						w.Write([]byte("corrupt"))
						return
					}
				case "corrupt_manifest":
					if strings.Contains(p, "/manifests/sha256:") {
						w.Write([]byte("corrupt"))
						return
					}
				case "foreign_next":
					if strings.HasSuffix(p, "/tags/list") {
						w.Header().Set("Link", "<"+other.URL+"/steal?last=x>; rel=\"next\"")
					}
				case "repeat_next":
					if strings.HasSuffix(p, "/tags/list") {
						w.Header().Set("Link", "</v2/test/component/tags/list?n=100&last=latest>; rel=\"next\"")
					}
				case "filtered_referrers":
					if strings.Contains(p, "/referrers/") {
						w.Header().Set("OCI-Filters-Applied", "artifactType")
					}
				case "fallback":
					if strings.Contains(p, "/referrers/") {
						w.WriteHeader(404)
						return
					}
				case "tag_drift":
					if strings.HasSuffix(p, "/tags/list") && tagCalls.Add(1) > 1 {
						json.NewEncoder(w).Encode(map[string]any{"name": f.Repository, "tags": []string{}})
						return
					}
				case "referrer_drift":
					if strings.HasSuffix(p, "/referrers/"+f.Index) && refCalls.Add(1) > 1 {
						json.NewEncoder(w).Encode(map[string]any{"schemaVersion": 2, "mediaType": ociIndex, "manifests": []descriptor{}})
						return
					}
				case "duplicate_json":
					if strings.HasSuffix(p, "/tags/list") {
						w.Write([]byte(`{"name":"test/component","tags":[],"tags":["latest"]}`))
						return
					}
				}
				base.ServeHTTP(w, r)
			}))
			defer s.Close()
			r := recipeFor(s.URL)
			switch mode {
			case "foreign_descriptor", "unknown_manifest":
				var m map[string]any
				json.Unmarshal(f.Manifests[f.UntTagged], &m)
				if mode == "foreign_descriptor" {
					m["layers"].([]any)[0].(map[string]any)["urls"] = []string{other.URL + "/steal"}
				} else {
					m["mediaType"] = "application/vnd.unknown.manifest"
				}
				b, _ := json.Marshal(m)
				d := fixture.Digest(b)
				f.Manifests[d] = b
				f.Roots = []string{d}
			case "request_limit":
				r.Limits.MaxRequests = 1
			case "byte_limit":
				r.Limits.MaxTotalBytes = 1024
				r.Limits.MaxResponseBytes = 1024
			case "node_limit":
				r.Limits.MaxNodes = 3
			case "depth_limit":
				r.Limits.MaxDepth = 1
			}
			r.Hash = workerconfig.ObservationHash(r)
			in := inputsFor(r, f.Roots)
			ctx := context.Background()
			if mode == "cancel" {
				var stop context.CancelFunc
				ctx, stop = context.WithCancel(ctx)
				stop()
			}
			got, e := Observe(ctx, r, in, time.Now().UTC())
			if e != nil {
				if mode != "cancel" && mode != "node_limit" && mode != "depth_limit" {
					t.Fatal(e)
				}
				return
			}
			if e := domain.ValidateRegistryGraph(got.Graph, r.Limits, in.Roots); e != nil {
				t.Fatalf("partial graph unreportable: %v %+v", e, got.Graph)
			}
			if got.Graph.Discovery == "COMPLETE" || len(got.Graph.Blockers) < 2 || got.Graph.GlobalInventoryComplete {
				t.Fatalf("failure became complete %+v", got.Graph)
			}
			if foreign.Load() != 0 {
				t.Fatal("foreign descriptor/next was followed")
			}
			for _, root := range got.Graph.Roots {
				if root.Closure == "COMPLETE" {
					t.Fatal("failed scan falsely complete")
				}
			}
		})
	}
}
func TestObserveFallbackPreservesVerifiedReferrer(t *testing.T) {
	f := fixture.New()
	b, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": ociIndex, "manifests": f.Referrers[f.Index]})
	d := fixture.Digest(b)
	f.Manifests[d] = b
	f.Tags["sha256-"+strings.TrimPrefix(f.Index, "sha256:")] = d
	base := f.Handler()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/referrers/") {
			w.WriteHeader(404)
			return
		}
		base.ServeHTTP(w, r)
	}))
	defer s.Close()
	r := recipeFor(s.URL)
	got, e := Observe(context.Background(), r, inputsFor(r, f.Roots), time.Now().UTC())
	if e != nil {
		t.Fatal(e)
	}
	found := false
	fallbackFound := false
	for _, e := range got.Graph.Edges {
		if e.Kind == "REFERRERS_INDEX" && e.From == f.Index && e.To == d {
			fallbackFound = true
		}
		if e.Kind == "REFERRER" && e.From == f.Index && e.To == f.Referrer {
			found = true
		}
	}
	if domain.ValidateRegistryGraph(got.Graph, r.Limits, f.Roots) != nil {
		t.Fatal("fallback graph not admissible")
	}
	if !found || !fallbackFound || got.Graph.Discovery == "COMPLETE" {
		t.Fatal("fallback lost verified referrer or claimed complete")
	}
}

func TestObserveEncodedReportBudgetPreservesFrozenRoots(t *testing.T) {
	f := fixture.New()
	f.Tags = map[string]string{}
	var source map[string]any
	json.Unmarshal(f.Manifests[f.ARM], &source)
	children := []descriptor{}
	for i := 0; i < 1020; i++ {
		source["annotations"] = map[string]string{"fixture": fmt.Sprint(i)}
		b, _ := json.Marshal(source)
		d := fixture.Digest(b)
		f.Manifests[d] = b
		children = append(children, descriptor{Digest: d, MediaType: ociManifest, Size: int64(len(b))})
	}
	b, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": ociIndex, "manifests": children})
	d := fixture.Digest(b)
	f.Manifests[d] = b
	f.Roots = []string{d}
	s := httptest.NewServer(f.Handler())
	defer s.Close()
	r := recipeFor(s.URL)
	r.Limits.MaxNodes = 1024
	r.Limits.MaxEdges = 4096
	r.Limits.MaxRequests = 8192
	r.Limits.MaxPages = 128
	r.Hash = workerconfig.ObservationHash(r)
	in := inputsFor(r, f.Roots)
	got, e := Observe(context.Background(), r, in, time.Now().UTC())
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(got)
	if len(raw) > domain.RegistryReportMaxBytes-4096 || domain.ValidateRegistryGraph(got.Graph, r.Limits, in.Roots) != nil {
		t.Fatalf("unreportable bounded partial graph: bytes=%d", len(raw))
	}
	if got.Graph.Discovery == "COMPLETE" || len(got.Graph.Roots) != 1 || got.Graph.Roots[0].Digest != d {
		t.Fatal("budget removed frozen root or reported complete")
	}
	found := false
	for _, code := range got.Graph.Blockers {
		found = found || code == "registry_limit"
	}
	if !found {
		t.Fatal("missing explicit budget limit")
	}
}
func TestObserveValidPaginationAndWrongSubject(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		t.Run(fmt.Sprint(wrong), func(t *testing.T) {
			f := fixture.New()
			base := f.Handler()
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/tags/list") {
					if r.URL.Query().Get("last") == "" {
						w.Header().Set("Link", "</v2/test/component/tags/list?n=1&last=first>; rel=\"next\"")
						json.NewEncoder(w).Encode(map[string]any{"name": f.Repository, "tags": []string{"first"}})
					} else {
						json.NewEncoder(w).Encode(map[string]any{"name": f.Repository, "tags": []string{"second"}})
					}
					return
				}
				base.ServeHTTP(w, r)
			}))
			defer s.Close()
			f.Tags = map[string]string{"first": f.Index, "second": f.Index}
			if wrong {
				f.Referrers[f.UntTagged] = f.Referrers[f.Index]
			}
			r := recipeFor(s.URL)
			got, e := Observe(context.Background(), r, inputsFor(r, f.Roots), time.Now().UTC())
			if e != nil || domain.ValidateRegistryGraph(got.Graph, r.Limits, f.Roots) != nil {
				t.Fatal("invalid pagination observation", e)
			}
			if !wrong && got.Graph.Discovery != "COMPLETE" {
				t.Fatal("valid sameorigin pagination incomplete")
			}
			if wrong {
				if got.Graph.Discovery == "COMPLETE" {
					t.Fatal("wrong subject claimed complete")
				}
				for _, edge := range got.Graph.Edges {
					if edge.Kind == "REFERRER" && edge.From == f.UntTagged && edge.To == f.Referrer {
						t.Fatal("wrong subject adopted")
					}
				}
			}
		})
	}
}

func TestObserveDockerSchema2OmittedMediaAndEmptyBlob(t *testing.T) {
	f := fixture.New()
	f.Tags = map[string]string{}
	empty := []byte{}
	emptyDigest := fixture.Digest(empty)
	f.Blobs[emptyDigest] = empty
	config := []byte(`{"os":"linux","architecture":"amd64"}`)
	configDigest := fixture.Digest(config)
	f.Blobs[configDigest] = config
	raw, _ := json.Marshal(map[string]any{"schemaVersion": 2, "config": descriptor{MediaType: "application/vnd.docker.container.image.v1+json", Digest: configDigest, Size: int64(len(config))}, "layers": []descriptor{{MediaType: "application/vnd.docker.image.rootfs.diff.tar.gzip", Digest: emptyDigest, Size: 0}}})
	d := fixture.Digest(raw)
	f.Manifests[d] = raw
	index, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": dockerIndex, "manifests": []descriptor{{MediaType: dockerManifest, Digest: d, Size: int64(len(raw)), Platform: &domain.RegistryPlatform{OS: "linux", Architecture: "amd64"}}}})
	root := fixture.Digest(index)
	f.Manifests[root] = index
	f.Roots = []string{root}
	base := f.Handler()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/manifests/"+d) {
			w.Header().Set("Content-Type", dockerManifest)
			w.Header().Set("Docker-Content-Digest", d)
			w.Write(raw)
			return
		}
		base.ServeHTTP(w, r)
	}))
	defer s.Close()
	r := recipeFor(s.URL)
	got, e := Observe(context.Background(), r, inputsFor(r, f.Roots), time.Now().UTC())
	if e != nil {
		t.Fatal(e)
	}
	if got.Graph.Discovery != "COMPLETE" || domain.ValidateRegistryGraph(got.Graph, r.Limits, f.Roots) != nil {
		t.Fatalf("valid Docker graph/emptyblob rejected %+v", got.Graph)
	}
	found := false
	for _, n := range got.Graph.Nodes {
		if n.Digest == emptyDigest && n.Size == 0 && n.Availability == "PRESENT" {
			found = true
		}
	}
	if !found {
		t.Fatal("zero byte blob not hash verified")
	}
}

func TestObserveReferrerPaginationCompleteness(t *testing.T) {
	for _, mode := range []string{"valid", "duplicate", "conflicting_size", "foreign_next"} {
		t.Run(mode, func(t *testing.T) {
			f := fixture.New()
			var m map[string]any
			json.Unmarshal(f.Manifests[f.Referrer], &m)
			m["annotations"] = map[string]string{"fixture": "second-referrer"}
			b, _ := json.Marshal(m)
			second := descriptor{MediaType: ociManifest, Digest: fixture.Digest(b), Size: int64(len(b))}
			f.Manifests[second.Digest] = b
			first := descriptor{MediaType: ociManifest, Digest: f.Referrer, Size: int64(len(f.Manifests[f.Referrer]))}
			base := f.Handler()
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/referrers/"+f.Index) {
					desc := first
					if r.URL.Query().Get("last") == "" {
						link := "</v2/test/component/referrers/" + f.Index + "?n=1&last=" + first.Digest + ">; rel=\"next\""
						if mode == "foreign_next" {
							link = "<https://other.invalid/steal?last=x>; rel=\"next\""
						}
						w.Header().Set("Link", link)
					} else {
						desc = second
						if mode == "duplicate" {
							desc = first
						}
						if mode == "conflicting_size" {
							desc = first
							desc.Size++
						}
					}
					json.NewEncoder(w).Encode(map[string]any{"schemaVersion": 2, "mediaType": ociIndex, "manifests": []descriptor{desc}})
					return
				}
				base.ServeHTTP(w, r)
			}))
			defer s.Close()
			r := recipeFor(s.URL)
			got, e := Observe(context.Background(), r, inputsFor(r, f.Roots), time.Now().UTC())
			if e != nil || domain.ValidateRegistryGraph(got.Graph, r.Limits, f.Roots) != nil {
				t.Fatal("invalid graph", e)
			}
			if (mode == "valid") != (got.Graph.Discovery == "COMPLETE") {
				t.Fatal("pagination completeness wrong", mode, got.Graph.Blockers)
			}
			if mode == "valid" {
				found := false
				for _, edge := range got.Graph.Edges {
					if edge.Kind == "REFERRER" && edge.To == second.Digest {
						found = true
					}
				}
				if !found {
					t.Fatal("second referrer page ignored")
				}
			}
		})
	}
}
