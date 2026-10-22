package workerrunner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"releasecontrol/internal/domain"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Independent synthetic pre-existing OCI bytes, uploaded with ordinary registry
// HTTP. No RCP build job, worker receipt, source proof or publication is invented.
// The HTTPS Kubernetes peer is simulated; this image makes no executable claim.
func publishIndependentBaselineImage(t *testing.T, origin, repository string) string {
	t.Helper()
	hash := func(b []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(b)) }
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	payload := []byte("independent pre-existing baseline fixture\n")
	if err := tw.WriteHeader(&tar.Header{Name: "fixture.txt", Mode: 0600, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(layer.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal(map[string]any{"architecture": "arm64", "os": "linux", "rootfs": map[string]any{"type": "layers", "diff_ids": []string{hash(layer.Bytes())}}})
	upload := func(blob []byte) string {
		t.Helper()
		response, err := http.Post(origin+"/v2/"+repository+"/blobs/uploads/", "application/octet-stream", nil)
		if err != nil {
			t.Fatal(err)
		}
		location := response.Header.Get("Location")
		response.Body.Close()
		if response.StatusCode != 202 || location == "" {
			t.Fatal("independent OCI upload not accepted", response.StatusCode)
		}
		base, _ := url.Parse(origin)
		reference, err := url.Parse(location)
		if err != nil {
			t.Fatal(err)
		}
		target := base.ResolveReference(reference)
		query := target.Query()
		digest := hash(blob)
		query.Set("digest", digest)
		target.RawQuery = query.Encode()
		request, _ := http.NewRequest(http.MethodPut, target.String(), bytes.NewReader(blob))
		request.Header.Set("Content-Type", "application/octet-stream")
		response, err = http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != 201 {
			t.Fatal("independent OCI blob rejected", response.StatusCode)
		}
		return digest
	}
	configDigest, layerDigest := upload(config), upload(compressed.Bytes())
	descriptor := func(kind, digest string, size int) map[string]any {
		return map[string]any{"mediaType": kind, "digest": digest, "size": size}
	}
	manifest, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json", "config": descriptor("application/vnd.oci.image.config.v1+json", configDigest, len(config)), "layers": []any{descriptor("application/vnd.oci.image.layer.v1.tar+gzip", layerDigest, compressed.Len())}})
	digest := hash(manifest)
	putManifest := func(reference, kind string, content []byte) {
		t.Helper()
		request, _ := http.NewRequest(http.MethodPut, origin+"/v2/"+repository+"/manifests/"+reference, bytes.NewReader(content))
		request.Header.Set("Content-Type", kind)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != 201 || response.Header.Get("Docker-Content-Digest") != hash(content) {
			t.Fatal("independent OCI manifest rejected", response.StatusCode)
		}
	}
	manifestKind := "application/vnd.oci.image.manifest.v1+json"
	putManifest("independent-baseline", manifestKind, manifest)
	// A real untagged incoming referrer is intentionally absent from the stable
	// empty client-maintained fallback index. The index cannot prove that the
	// registry has no other referrers, even though the image pull bytes exist.
	hidden, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": manifestKind, "artifactType": "application/vnd.fixture.hidden", "config": descriptor("application/vnd.oci.image.config.v1+json", configDigest, len(config)), "layers": []any{descriptor("application/vnd.oci.image.layer.v1.tar+gzip", layerDigest, compressed.Len())}, "subject": descriptor(manifestKind, digest, len(manifest))})
	hiddenDigest := hash(hidden)
	putManifest(hiddenDigest, manifestKind, hidden)
	emptyIndex := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	fallbackTag := "sha256-" + strings.TrimPrefix(digest, "sha256:")
	putManifest(fallbackTag, "application/vnd.oci.image.index.v1+json", emptyIndex)
	for _, expected := range []struct {
		reference string
		content   []byte
	}{{hiddenDigest, hidden}, {fallbackTag, emptyIndex}, {fallbackTag, emptyIndex}} {
		request, _ := http.NewRequest(http.MethodGet, origin+"/v2/"+repository+"/manifests/"+expected.reference, nil)
		request.Header.Set("Accept", manifestKind+", application/vnd.oci.image.index.v1+json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		response.Body.Close()
		if err != nil || response.StatusCode != 200 || !bytes.Equal(actual, expected.content) || response.Header.Get("Docker-Content-Digest") != hash(expected.content) {
			t.Fatal("actual independent referrer/index bytes changed")
		}
	}
	response, err := http.Get(origin + "/v2/" + repository + "/tags/list?n=100")
	if err != nil {
		t.Fatal(err)
	}
	var tags struct {
		Tags []string `json:"tags"`
	}
	if response.StatusCode != 200 || json.NewDecoder(response.Body).Decode(&tags) != nil {
		response.Body.Close()
		t.Fatal("independent OCI tag inventory unavailable")
	}
	response.Body.Close()
	if !slices.Contains(tags.Tags, fallbackTag) || slices.Contains(tags.Tags, hiddenDigest) {
		t.Fatal("hidden subject referrer unexpectedly tagged or fallback absent")
	}
	t.Logf("Actual independent OCI image %s has stable empty fallback tag plus actual untagged subject referrer %s; no RCP provenance", digest, hiddenDigest)
	return digest
}

// Explicit scoped GET fault injection; actual Distribution bytes remain intact.
// It never deletes registry data or grants a cleanup/GC capability.
func baselineRegistryReadFault(t *testing.T, upstream string) (string, func(string, bool)) {
	t.Helper()
	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	var mu sync.Mutex
	var denied string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fail := denied != "" && r.Method == http.MethodGet && r.URL.Path == denied
		mu.Unlock()
		if fail {
			http.Error(w, "explicit baseline fixture GET fault", http.StatusNotFound)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server.URL, func(path string, enabled bool) {
		mu.Lock()
		defer mu.Unlock()
		denied = ""
		if enabled {
			denied = path
		}
	}
}

func assertRealDevBaselineCapture(t *testing.T, reservation domain.ExternalOperation, selection domain.EnvironmentSelection, peer *candidateRuntimeHTTPFixture, provider *mainCandidateActivationProvider, ticks func(), read func() domain.State, request func(string, map[string]any, any, bool), registryFault func(bool)) domain.ExternalOperation {
	t.Helper()
	operation := func(id string) domain.ExternalOperation {
		for _, o := range read().Operations {
			if o.ID == id {
				return o
			}
		}
		t.Fatal("baseline capture missing", id)
		return domain.ExternalOperation{}
	}
	capture := func(key string) domain.ExternalOperation {
		var out domain.ExternalOperation
		request("capture-baseline", map[string]any{"reservation_operation_id": reservation.ID, "request_key": key, "expected_selection": selection}, &out, true)
		return out
	}
	deploy := map[string]any{"reservation_operation_id": reservation.ID, "request_key": "baseline-rejected-deploy", "expected_selection": selection}
	request("deploy", deploy, nil, false)
	before := read()
	peer.setWrongDigest(true)
	wrong := capture("actual-baseline-wrong-image-id")
	ticks()
	if operation(wrong.ID).Status == "SUCCEEDED" {
		t.Fatal("baseline accepted wrong immutable pod imageID")
	}
	request("deploy", deploy, nil, false)
	peer.setWrongDigest(false)
	registryFault(true)
	missing := capture("actual-baseline-missing-registry-bytes")
	ticks()
	registryFault(false)
	if operation(missing.ID).Status == "SUCCEEDED" {
		t.Fatal("baseline accepted unavailable original registry bytes")
	}
	request("deploy", deploy, nil, false)
	positive := capture("actual-baseline-complete-original-dev")
	ticks()
	positive = operation(positive.ID)
	if positive.Status != "SUCCEEDED" || positive.DevBaseline == nil || positive.DevBaselineResult == nil || positive.DevBaselineResult.Outcome != "CAPTURED" {
		t.Fatalf("full original Dev baseline capture failed: %+v", positive)
	}
	if len(positive.DevBaseline.Components) != 1 || len(positive.DevBaseline.Components[0].Scopes) != 2 || len(positive.DevBaselineResult.Components) != 1 {
		t.Fatal("baseline omitted A+B component or api/worker role")
	}
	c := positive.DevBaselineResult.Components[0]
	peer.mu.Lock()
	image, digest := peer.image, peer.digest
	peer.mu.Unlock()
	if c.ImageRepository != image || c.Digest != digest || c.ArtifactID == "" {
		t.Fatal("baseline did not preserve original admitted Dev artifact")
	}
	after := read()
	if len(provider.proposals) != 0 || len(before.DeliveryArtifacts) != len(after.DeliveryArtifacts) || len(before.WorkerJobs) != len(after.WorkerJobs) {
		t.Fatal("capture built/published or sent GitOps mutation")
	}
	a, _ := json.Marshal(before.Environments)
	b, _ := json.Marshal(after.Environments)
	if string(a) != string(b) {
		t.Fatal("capture mutated desired/environment maps")
	}
	peer.assertReadOnly(t)
	t.Log("Named HTTP baseline capture reads actual original Dev-profile A1+B1 Distribution artifact and simulated GitHub/HTTPS Kubernetes api+worker; wrong imageID and explicit registry GET404 block capture and activation with zero mutating sends")
	return positive
}

// This control points an ordinary reserved baseline at real externally published
// bytes for which this server has never admitted a DeliveryArtifact.
func assertRealPreexistingBaseline(t *testing.T, endpoint, token, candidate, image, digest string, peer *candidateRuntimeHTTPFixture, provider *mainCandidateActivationProvider, ticks func(), read func() domain.State, post func(domain.Command, any, bool)) {
	t.Helper()
	provider.gitops.configureBaseline(image, digest)
	peer.setImage(image, digest)
	selection := func() domain.EnvironmentSelection {
		for _, e := range read().Environments {
			if e.ID == "dev" {
				return domain.EnvironmentSelection{CompositionID: e.DesiredCompositionID, OperationID: e.DesiredOperationID, Generation: e.SelectionGeneration}
			}
		}
		t.Fatal("Dev missing")
		return domain.EnvironmentSelection{}
	}
	request := func(path string, data map[string]any, out any) {
		t.Helper()
		data["actor"] = "fixture/operator"
		b, _ := json.Marshal(data)
		r, _ := http.NewRequest(http.MethodPost, endpoint+"/api/products/p/candidate-dev/"+path, bytes.NewReader(b))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		if response.StatusCode != 200 {
			t.Fatalf("preexisting baseline %s HTTP%d: %s", path, response.StatusCode, raw)
		}
		if out != nil {
			if err := json.Unmarshal(raw, out); err != nil {
				t.Fatal(err)
			}
		}
	}
	before := read()
	for _, a := range before.DeliveryArtifacts {
		if a.ImageRepository == image && a.Digest == digest {
			t.Fatal("preexisting control already has admitted artifact")
		}
	}
	for _, attempt := range before.WorkerAttempts {
		if attempt.Report != nil && attempt.Report.LocalOCI != nil && attempt.Report.LocalOCI.ManifestDigest == digest {
			t.Fatal("independent pre-existing image already has RCP build provenance")
		}
	}
	for _, job := range before.WorkerJobs {
		if job.Inputs.Publication != nil && job.Inputs.Publication.Receipt.ManifestDigest == digest {
			t.Fatal("independent pre-existing image already has RCP publication provenance")
		}
	}
	var hold, capture domain.ExternalOperation
	request("reserve", map[string]any{"candidate_operation_id": candidate, "environment_id": "dev", "request_key": "external-baseline-reservation", "expected_selection": selection()}, &hold)
	defer request("release", map[string]any{"reservation_operation_id": hold.ID, "request_key": "external-baseline-release", "expected_selection": selection()}, nil)
	request("capture-baseline", map[string]any{"reservation_operation_id": hold.ID, "request_key": "external-baseline-read-receipt", "expected_selection": selection()}, &capture)
	ticks()
	for _, op := range read().Operations {
		if op.ID == capture.ID {
			capture = op
		}
	}
	if capture.Status != "SUCCEEDED" || capture.DevBaselineResult == nil || len(capture.DevBaselineResult.Components) != 1 {
		for _, inventory := range read().RegistryInventories {
			if capture.DevBaseline == nil {
				continue
			}
			for _, part := range capture.DevBaseline.Components {
				if inventory.JobID == part.RegistryJobID {
					t.Logf("actual registry closure job=%s roots=%+v blockers=%v", inventory.JobID, inventory.Report.Graph.Roots, inventory.Report.Graph.Blockers)
				}
			}
		}
		detail, _ := json.Marshal(capture)
		t.Fatalf("preexisting Distribution capture not complete: %s", detail)
	}
	c := capture.DevBaselineResult.Components[0]
	if c.Registry == nil || c.Registry.JobID == "" || c.ArtifactID != "" || c.Availability != nil || c.ImageRepository != image || c.Digest != digest {
		t.Fatalf("preexisting bytes forged native artifact instead of causal read-only receipt: %+v", c)
	}
	receipt := c.Registry
	state := read()
	var job domain.WorkerJob
	for _, j := range state.WorkerJobs {
		if j.ID == receipt.JobID {
			job = j
		}
	}
	if job.ID == "" || job.OperationID != receipt.OperationID || job.Capability != "registry_observe" || job.InputHash != receipt.InputHash || job.Fence != receipt.Fence || job.Inputs.Registry == nil || job.Inputs.Registry.RootSetHash != receipt.RootSetHash {
		t.Fatal("baseline registry receipt not bound to exact configured worker job")
	}
	causal := false
	for _, op := range state.Operations {
		if op.ID == job.OperationID && op.ParentID == capture.ID && op.Kind == domain.RegistryObserveOperationKind {
			causal = true
		}
	}
	if !causal {
		t.Fatal("registry observation not causal child of exact capture")
	}
	attempt := false
	for _, a := range state.WorkerAttempts {
		if a.ID == receipt.AttemptID && a.JobID == job.ID && a.Fence == receipt.Fence && a.InputHash == receipt.InputHash && a.Status == "SUCCEEDED" && a.Report != nil && a.Report.Registry != nil {
			attempt = true
		}
	}
	if !attempt {
		t.Fatal("baseline registry receipt missing successful exact attempt/fence")
	}
	complete := false
	for _, inventory := range state.RegistryInventories {
		if inventory.ID != receipt.InventoryID {
			continue
		}
		if inventory.JobID != job.ID || inventory.AttemptID != receipt.AttemptID || inventory.Fence != receipt.Fence || inventory.InputHash != receipt.InputHash || inventory.Report.RootSetHash != receipt.RootSetHash || inventory.Report.Graph.GraphHash != receipt.GraphHash || !inventory.Report.FinishedAt.Equal(receipt.ObservedAt) {
			t.Fatal("baseline receipt inventory provenance changed")
		}
		graph := inventory.Report.Graph
		if graph.Discovery != "PARTIAL" || graph.GlobalInventoryComplete || !slices.Contains(graph.Blockers, "registry_referrers_unknown") {
			t.Fatal("empty fallback incorrectly promoted incoming-referrer inventory completeness")
		}
		if receipt.ClosureKind != "IMAGE_CONTENT" || receipt.ContentProofHash == "" || receipt.MetadataRootClosure != "PARTIAL" || receipt.MetadataDiscovery != "PARTIAL" || receipt.GlobalInventoryComplete || !slices.Contains(receipt.MetadataBlockers, "registry_referrers_unknown") {
			t.Fatal("restoration-only receipt omitted distinct content scope or honest unknown metadata")
		}
		hash, err := domain.RegistryImageContentProofHash(graph, inventory.Inputs.Registry.Limits, inventory.Inputs.Registry.Roots, digest)
		if err != nil || hash != receipt.ContentProofHash {
			t.Fatalf("baseline typed image-content proof hash mismatch: %v", err)
		}
		for _, root := range graph.Roots {
			if root.Digest == digest && root.Closure == "PARTIAL" && len(root.Blockers) > 0 {
				complete = true
			}
		}
	}
	if !complete {
		t.Fatal("typed image-content availability incorrectly replaced original partial metadata root")
	}
	if len(read().DeliveryArtifacts) != len(before.DeliveryArtifacts) || len(provider.proposals) != 0 {
		t.Fatal("preexisting registry capture minted candidate/build/publication rights")
	}
	oldJobs := map[string]bool{}
	for _, old := range before.WorkerJobs {
		oldJobs[old.ID] = true
	}
	for _, added := range read().WorkerJobs {
		if !oldJobs[added.ID] && (added.ID != receipt.JobID || added.Capability != "registry_observe" || added.Inputs.Execution != nil || added.Inputs.Publication != nil || added.Inputs.SourceProof != nil) {
			t.Fatal("preexisting capture queued authority beyond its exact read-only registry observation")
		}
	}
	assertRealBaselineRetained(t, endpoint, token, capture)
	assertRealBaselineQuery(t, endpoint, token, capture, true)
	t.Log("Actual Distribution pre-existing image has zero RCP build/publication records; configured registry observer proves distinct COMPLETE IMAGE_CONTENT closure despite stable empty fallback plus untagged referrer, while original metadata remains PARTIAL/referrers unknown; restoration-only receipt, no native/Prod/GC rights")
}

func assertRealBaselineRetained(t *testing.T, endpoint, token string, capture domain.ExternalOperation) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, endpoint+"/api/products/p/retention", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var report domain.RetentionReport
	if response.StatusCode != 200 || json.NewDecoder(response.Body).Decode(&report) != nil {
		t.Fatal("baseline retention query failed")
	}
	if report.DeletionAllowed || report.GlobalInventoryComplete {
		t.Fatal("baseline proof granted physical registry GC/global inventory authority")
	}
	for _, c := range capture.DevBaselineResult.Components {
		found := false
		for _, image := range report.Images {
			if image.ImageRepository == c.ImageRepository && image.Digest == c.Digest && image.Protected {
				for _, reason := range image.Reasons {
					if reason == "dev_baseline:"+capture.ID {
						found = true
					}
				}
			}
		}
		if !found {
			t.Fatalf("held original A+B baseline image lost owned retention root %s@%s", c.ImageRepository, c.Digest)
		}
	}
}

func assertRealBaselineQuery(t *testing.T, endpoint, token string, capture domain.ExternalOperation, current bool) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, endpoint+"/api/environments/dev/state", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var state map[string]any
	if response.StatusCode != 200 || json.NewDecoder(response.Body).Decode(&state) != nil {
		t.Fatal("baseline current-state query failed")
	}
	if state["baseline_capture_id"] != capture.ID || state["baseline_capture_status"] != "SUCCEEDED" || state["baseline_capture_observed_at"] == nil || state["baseline_capture_current_valid"] != current {
		t.Fatalf("baseline query confused immutable original proof with current candidate authority: %+v", state)
	}
}
