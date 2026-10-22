package domain

import (
	"encoding/json"
	"fmt"
	"reflect"
	"releasecontrol/internal/delivery"
	"strings"
	"testing"
	"time"
)

var retentionNow = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func retentionSHA(n int) string { return fmt.Sprintf("sha256:%064x", n+1) }
func retentionBase() State {
	return State{Products: []Product{{Meta: Meta{ID: "p"}}}, Applications: []Application{{Meta: Meta{ID: "app", ProductID: "p"}, RepositoryID: "repo"}}}
}
func retentionGHA(st *State, n int, profile string, at time.Time) DeliveryArtifact {
	id := fmt.Sprintf("gha-%d-%s", n, profile)
	source := fmt.Sprintf("%040x", n+1)
	req := delivery.BuildRequest{Repository: "owner/repo", Workflow: "build.yml", Ref: "feature", SourceSHA: source, ImageTag: id, OperationID: id}
	snap := DeliverySnapshot{Application: st.Applications[0], Build: ComponentBuild{Meta: Meta{ProductID: "p"}, ApplicationID: "app", ImageRepository: "ghcr.io/owner/app", Profile: profile}, Revision: IntegrationRevision{Meta: Meta{ProductID: "p"}, RepositoryID: "repo", HeadCommit: source}, Source: RepositoryBinding{Meta: Meta{ProductID: "p"}, RepositoryID: "repo", FullName: "owner/repo"}, BuildRequest: req, SourceConnection: delivery.Connection{AppID: 7}}
	st.Operations = append(st.Operations, ExternalOperation{Meta: Meta{ID: id, ProductID: "p"}, Kind: "DEPLOY", ApplicationID: "app", Status: "SUCCEEDED", Snapshot: &snap})
	fields := map[string]string{"operation_id": id, "source_sha": source, "image_repository": snap.Build.ImageRepository, "image_tag": id, "digest": retentionSHA(n), "available": "true", "observed_at": at.Format(time.RFC3339Nano)}
	st.DeliveryBuildRuns = append(st.DeliveryBuildRuns, DeliveryBuildRun{Meta: Meta{ID: "run-" + id, ProductID: "p"}, OperationID: id, ApplicationID: "app", Request: req, Result: delivery.RunResult{Found: true, ID: int64(n + 1), Status: "completed", Conclusion: "success", HeadSHA: source, Artifacts: fields}})
	a := DeliveryArtifact{Meta: Meta{ID: "artifact-" + id, ProductID: "p", CreatedAt: retentionNow.Add(time.Duration(n) * time.Hour)}, OperationID: id, ApplicationID: "app", RepositoryID: "repo", SourceCommit: source, ImageRepository: snap.Build.ImageRepository, Digest: retentionSHA(n), Availability: "PRESENT", ObservedAt: retentionNow, BuildRequest: req, BuildRunID: int64(n + 1)}
	st.DeliveryArtifacts = append(st.DeliveryArtifacts, a)
	return a
}
func retentionFind(t *testing.T, r RetentionReport, digest string) RetentionImage {
	t.Helper()
	for _, v := range r.Images {
		if v.Digest == digest {
			return v
		}
	}
	t.Fatal("missing root", digest, r)
	return RetentionImage{}
}
func retentionHas(v []string, s string) bool {
	for _, x := range v {
		if x == s {
			return true
		}
	}
	return false
}
func TestRetentionFloorChronologyDedupProfiles(t *testing.T) {
	st := retentionBase()
	st.Applications[0].Retention = &ArtifactRetention{KeepLast: 1, KeepCurrent: false}
	for n := 0; n < 14; n++ {
		retentionGHA(&st, n, "default", retentionNow.Add(-time.Duration(n+1)*time.Minute))
	}
	// New local insertion/observation of an old image must not reorder publication.
	dup := st.DeliveryArtifacts[13]
	dup.ID = "duplicate"
	dup.CreatedAt = retentionNow.Add(time.Hour)
	dup.ObservedAt = retentionNow
	st.DeliveryArtifacts = append(st.DeliveryArtifacts, dup)
	r := EvaluateRetention(st, "p", retentionNow)
	if len(r.Images) != 14 || r.Policies[0].EffectiveKeepLast != 10 || !r.Policies[0].KeepCurrent || r.Policies[0].KeepPrevious != 1 {
		t.Fatal(r)
	}
	for n := 0; n < 14; n++ {
		v := retentionFind(t, r, retentionSHA(n))
		if v.Protected != (n < 10) {
			t.Fatal("local timestamp displaced publication chronology", n, v)
		}
	}
	// An independent build profile receives its own quota; dedup image identities globally.
	for n := 14; n < 26; n++ {
		retentionGHA(&st, n, "release", retentionNow.Add(-time.Duration(n)*time.Minute))
	}
	r = EvaluateRetention(st, "p", retentionNow)
	for n := 14; n < 26; n++ {
		if retentionFind(t, r, retentionSHA(n)).Protected != (n < 24) {
			t.Fatal("profile quota mixed", n)
		}
	}
	if r.DeletionAllowed || !retentionHas(r.Blockers, "oci_closure_unavailable") || !retentionHas(r.Blockers, "registry_completeness_unavailable") || !retentionHas(r.Blockers, "workload_coverage_unavailable") {
		t.Fatal("advisory report permits deletion", r)
	}
}
func TestRetentionUnknownChronologyOutsideQuota(t *testing.T) {
	for _, mode := range []string{"missing", "zero", "future", "wrong-source", "wrong-run", "wrong-operation", "bad-profile"} {
		t.Run(mode, func(t *testing.T) {
			st := retentionBase()
			for n := 0; n < 12; n++ {
				retentionGHA(&st, n, "default", retentionNow.Add(-time.Duration(n+1)*time.Minute))
			}
			a := retentionGHA(&st, 99, "default", retentionNow.Add(-time.Hour))
			run := &st.DeliveryBuildRuns[len(st.DeliveryBuildRuns)-1]
			switch mode {
			case "missing":
				st.DeliveryBuildRuns = st.DeliveryBuildRuns[:len(st.DeliveryBuildRuns)-1]
			case "zero":
				run.Result.Artifacts["observed_at"] = time.Time{}.Format(time.RFC3339Nano)
			case "future":
				run.Result.Artifacts["observed_at"] = retentionNow.Add(time.Nanosecond).Format(time.RFC3339Nano)
			case "wrong-source":
				run.Result.HeadSHA = "other"
			case "wrong-run":
				run.Result.ID++
			case "wrong-operation":
				run.OperationID = "other"
			case "bad-profile":
				st.Operations[len(st.Operations)-1].Snapshot.Build.Profile = "bad profile"
			}
			r := EvaluateRetention(st, "p", retentionNow)
			v := retentionFind(t, r, a.Digest)
			if !v.Protected || !retentionHas(v.Reasons, "unknown_publication_chronology") {
				t.Fatal(v)
			}
			for n := 0; n < 12; n++ {
				if retentionFind(t, r, retentionSHA(n)).Protected != (n < 10) {
					t.Fatal("unknown consumed trusted quota", n)
				}
			}
		})
	}
}
func TestRetentionEarliestTrustedPublicationBoundary(t *testing.T) {
	st := retentionBase()
	for n := 0; n < 11; n++ {
		retentionGHA(&st, n, "default", retentionNow.Add(-time.Duration(n+1)*time.Minute))
	}
	a := retentionGHA(&st, 0, "default", retentionNow.Add(-time.Hour))
	a.ID = "same-digest-old-publication"
	st.DeliveryArtifacts[len(st.DeliveryArtifacts)-1] = a
	r := EvaluateRetention(st, "p", retentionNow)
	if retentionFind(t, r, retentionSHA(0)).Protected || !retentionFind(t, r, retentionSHA(10)).Protected {
		t.Fatal("latest replay publication inflated quota", r)
	}
}
func retentionNative(st *State, n int, at time.Time) DeliveryArtifact {
	source := fmt.Sprintf("%040x", n+1)
	bid := fmt.Sprintf("build-%d", n)
	aid := "attempt-" + bid
	pubid := fmt.Sprintf("publish-%d", n)
	paid := "attempt-" + pubid
	execution := &WorkerExecution{ToolchainImage: "tool@" + retentionSHA(80), Platform: "linux/arm64", OutputBytes: 4096}
	inputs := WorkerInputs{RepositoryID: "repo", SourceSHA: source, RecipeHash: retentionSHA(81), ConfigRevision: "cfg", Execution: execution}
	h := checkHash(inputs)
	receipt := WorkerLocalOCI{JobID: bid, AttemptID: aid, InputHash: h, SourceSHA: source, RecipeHash: inputs.RecipeHash, ConfigRevision: "cfg", ToolchainImage: execution.ToolchainImage, Platform: execution.Platform, ArchiveBytes: 1024, ArchiveHash: retentionSHA(82), LayoutHash: retentionSHA(83), IndexDigest: retentionSHA(n), ManifestDigest: retentionSHA(n)}
	build := WorkerJob{Meta: Meta{ID: bid, ProductID: "p"}, Capability: "local_oci_build", Inputs: inputs, InputHash: h, Status: "SUCCEEDED", Fence: 1, Deadline: retentionNow.Add(time.Hour)}
	ba := WorkerAttempt{Meta: Meta{ID: aid, ProductID: "p", UpdatedAt: at.Add(-time.Second)}, JobID: bid, Status: "SUCCEEDED", Fence: 1, InputHash: h, Report: &WorkerReport{WorkerAttemptRequest: WorkerAttemptRequest{JobID: bid, AttemptID: aid, Fence: 1, InputHash: h}, Result: "PASSED", LocalOCI: &receipt}}
	pi := WorkerInputs{RepositoryID: "repo", SourceSHA: source, Publication: &WorkerPublication{ComponentID: "app", Profile: "release", Origin: "https://ghcr.io", Repository: "owner/app", Receipt: receipt}}
	ph := checkHash(pi)
	pj := WorkerJob{Meta: Meta{ID: pubid, ProductID: "p"}, OperationID: "op-" + pubid, Capability: "registry_publish", Inputs: pi, InputHash: ph, Status: "SUCCEEDED", Fence: 1, Deadline: retentionNow.Add(time.Hour)}
	pa := WorkerAttempt{Meta: Meta{ID: paid, ProductID: "p", UpdatedAt: at}, JobID: pubid, Status: "SUCCEEDED", Fence: 1, InputHash: ph, Report: &WorkerReport{WorkerAttemptRequest: WorkerAttemptRequest{JobID: pubid, AttemptID: paid, Fence: 1, InputHash: ph}, Result: "PASSED", Publication: &WorkerPublicationReport{ManifestDigest: receipt.ManifestDigest}}}
	st.WorkerJobs = append(st.WorkerJobs, build, pj)
	st.WorkerAttempts = append(st.WorkerAttempts, ba, pa)
	a := DeliveryArtifact{Meta: Meta{ID: fmt.Sprintf("native-%d", n), ProductID: "p", CreatedAt: retentionNow.Add(time.Hour)}, ApplicationID: "app", RepositoryID: "repo", SourceCommit: source, OperationID: pj.OperationID, ImageRepository: "ghcr.io/owner/app", Digest: receipt.ManifestDigest, Availability: "PRESENT", ObservedAt: retentionNow, Provenance: "NATIVE_WORKER", Native: &NativeArtifact{PublicationJobID: pubid, PublicationAttemptID: paid, PublicationInputHash: ph, Profile: "release", Receipt: receipt, Observation: NativeObservation{ManifestDigest: receipt.ManifestDigest}}}
	st.DeliveryArtifacts = append(st.DeliveryArtifacts, a)
	return a
}
func TestRetentionNativeAcceptedPublicationNotBuildTime(t *testing.T) {
	st := retentionBase()
	for n := 0; n < 12; n++ {
		retentionNative(&st, n, retentionNow.Add(-time.Duration(n+1)*time.Minute))
	}
	r := EvaluateRetention(st, "p", retentionNow)
	for n := 0; n < 12; n++ {
		v := retentionFind(t, r, retentionSHA(n))
		if v.Protected != (n < 10) || v.PublishedAt == nil || !v.PublishedAt.Equal(st.WorkerAttempts[n*2+1].UpdatedAt) {
			t.Fatal(n, v)
		}
	}
	for _, mode := range []string{"reconciled", "nilreport", "wrongreceipt", "hash", "future"} {
		t.Run(mode, func(t *testing.T) {
			b, _ := json.Marshal(st)
			var bad State
			json.Unmarshal(b, &bad)
			n := bad.DeliveryArtifacts[11].Native
			switch mode {
			case "reconciled":
				n.Reconciliation = &NativeReconciliation{}
			case "nilreport":
				bad.WorkerAttempts[23].Report = nil
			case "wrongreceipt":
				n.Receipt.ManifestDigest = retentionSHA(999)
			case "hash":
				bad.WorkerJobs[23].InputHash = "wrong"
			case "future":
				bad.WorkerAttempts[23].UpdatedAt = retentionNow.Add(time.Second)
			}
			v := retentionFind(t, EvaluateRetention(bad, "p", retentionNow), retentionSHA(11))
			if !v.Protected || !retentionHas(v.Blockers, "unknown_publication_chronology") {
				t.Fatal(v)
			}
		})
	}
}
func TestRetentionRuntimeRootsWithoutArtifactsAndFreshness(t *testing.T) {
	st := retentionBase()
	st.RuntimeSnapshots = []RuntimeSnapshot{{Meta: Meta{ID: "runtime", ProductID: "p"}, ApplicationID: "app", EnvironmentID: "env", Expected: RuntimeExpected{ImageRepository: "ghcr.io/owner/app", Value: retentionSHA(50)}, Runtime: delivery.RuntimeSnapshot{ObservedAt: retentionNow, WorkloadImage: "ghcr.io/owner/app@" + retentionSHA(51), Health: "UNHEALTHY", Pods: []delivery.RuntimePod{{Image: "ghcr.io/owner/app@" + retentionSHA(52), ImageID: "docker-pullable://ghcr.io/owner/app@" + retentionSHA(53), Ready: false}}, Flux: &delivery.RuntimeFluxSnapshot{Revision: "old", Status: "NotReady"}}}}
	r := EvaluateRetention(st, "p", retentionNow)
	if len(r.Images) != 4 {
		t.Fatal(r)
	}
	for n := 50; n <= 53; n++ {
		if !retentionFind(t, r, retentionSHA(n)).Protected {
			t.Fatal("old/unhealthy pod not rooted")
		}
	}
	st.RuntimeSnapshots[0].Runtime.Pods[0].Image = "ghcr.io/owner/app:main"
	r = EvaluateRetention(st, "p", retentionNow.Add(5*time.Minute))
	if retentionHas(r.Blockers, "runtime_evidence_stale") {
		t.Fatal("5min inclusive boundary")
	}
	r = EvaluateRetention(st, "p", retentionNow.Add(5*time.Minute+time.Nanosecond))
	if !retentionHas(r.Blockers, "runtime_evidence_stale") || !retentionHas(r.Blockers, "mutable_or_unresolved_image_reference") {
		t.Fatal(r)
	}
	r = EvaluateRetention(st, "p", retentionNow.Add(-time.Nanosecond))
	if !retentionHas(r.Blockers, "runtime_evidence_unknown") {
		t.Fatal("future treated fresh", r)
	}
}
func TestRetentionSharedPhysicalPrivacyPinsAndUncertainCancellation(t *testing.T) {
	st := retentionBase()
	for n := 0; n < 12; n++ {
		retentionGHA(&st, n, "default", retentionNow.Add(-time.Duration(n+1)*time.Minute))
	}
	st.Products = append(st.Products, Product{Meta: Meta{ID: "foreign-secret-product"}})
	st.ArtifactPins = append(st.ArtifactPins, ArtifactPin{Meta: Meta{ID: "secret-job", ProductID: "foreign-secret-product"}, ImageRepository: "GHCR.IO:443/owner/app", Digest: retentionSHA(11), Purpose: "rollback"})
	r := EvaluateRetention(st, "p", retentionNow)
	v := retentionFind(t, r, retentionSHA(11))
	if !v.Protected || !retentionHas(v.Reasons, "shared_product_root") {
		t.Fatal(v)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "foreign-secret") || strings.Contains(string(b), "secret-job") {
		t.Fatal("foreign metadata leaked", string(b))
	}
	snap := DeliverySnapshot{Application: st.Applications[0], Build: ComponentBuild{ImageRepository: "ghcr.io/owner/app"}}
	st.Operations = append(st.Operations, ExternalOperation{Meta: Meta{ID: "cancelled", ProductID: "p"}, Kind: "DEPLOY", Status: "CANCELLED", Attempts: 1, Snapshot: &snap, Artifact: &delivery.ArtifactResult{Digest: retentionSHA(10)}})
	st.NativeQuarantines = []NativeQuarantine{{Meta: Meta{ProductID: "p"}}}
	r = EvaluateRetention(st, "p", retentionNow)
	if !retentionFind(t, r, retentionSHA(10)).Protected || !retentionHas(r.Blockers, "uncertain_external_intent") || !retentionHas(r.Blockers, "restore_quarantine") {
		t.Fatal(r)
	}
}
func retentionRelease(st *State, n int, scope string) {
	parent := fmt.Sprintf("release-op-%d-%s", n, scope)
	child := parent + "-child"
	digest := retentionSHA(100 + n)
	at := retentionNow.Add(-time.Duration(n+1) * time.Hour)
	snap := DeliverySnapshot{Application: st.Applications[0], Build: ComponentBuild{ImageRepository: "ghcr.io/owner/app"}, Target: EnvironmentBinding{Purpose: "PROD", RepositoryID: "flux", Ref: "main", Path: scope, EnvironmentID: "env"}, Environment: Environment{Meta: Meta{ID: "env"}, Cluster: "cluster", Namespace: "prod"}, Revision: IntegrationRevision{HeadCommit: "source"}}
	st.Operations = append(st.Operations, ExternalOperation{Meta: Meta{ID: parent, ProductID: "p"}, Kind: "RELEASE_PROMOTION", Status: "SUCCEEDED", ChildIDs: []string{child}}, ExternalOperation{Meta: Meta{ID: child, ProductID: "p"}, ParentID: parent, Kind: "DEPLOY", Status: "SUCCEEDED", EnvironmentID: "env", ApplicationID: "app", Snapshot: &snap, Artifact: &delivery.ArtifactResult{Digest: digest}, GitOpsResult: &delivery.GitOpsResult{CommitSHA: child + "commit"}})
	st.RuntimeObservations = append(st.RuntimeObservations, RuntimeObservation{Meta: Meta{ID: child + "-runtime", ProductID: "p", CreatedAt: at.Add(-time.Second)}, OperationID: child, EnvironmentID: "env", ArtifactDigest: digest, GitOpsCommit: child + "commit", Healthy: true})
	st.Releases = append(st.Releases, Release{Meta: Meta{ID: parent + "-record", ProductID: "p", CreatedAt: at}, ExecutionOperationID: parent, Status: "released", SourceCommits: map[string]string{"app": "source"}, ArtifactDigests: map[string]string{"app": digest}, GitOpsCommits: map[string]string{"app": child + "commit"}})
}
func TestRetentionFrozenWholeProductionTargetAndIncompleteRelease(t *testing.T) {
	st := retentionBase()
	for n := 0; n < 3; n++ {
		retentionRelease(&st, n, "old/path")
	}
	retentionRelease(&st, 4, "second/path")
	// Live mapping no longer says PROD and points elsewhere; frozen targets remain.
	st.ComponentBuilds = []ComponentBuild{{ApplicationID: "app", ImageRepository: "different.io/new/image"}}
	st.EnvironmentBindings = []EnvironmentBinding{{ApplicationID: "app", Purpose: "DEV", Path: "new/path"}}
	// Historical runtime roots are independently protected, so check production reasons.
	r := EvaluateRetention(st, "p", retentionNow)
	if !retentionHas(retentionFind(t, r, retentionSHA(100)).Reasons, "production_current") || !retentionHas(retentionFind(t, r, retentionSHA(101)).Reasons, "production_previous") || retentionHas(retentionFind(t, r, retentionSHA(102)).Reasons, "production_previous") || !retentionHas(retentionFind(t, r, retentionSHA(104)).Reasons, "production_current") {
		t.Fatal(r)
	}
	st.Releases[2].RecordedExternally = true
	r = EvaluateRetention(st, "p", retentionNow)
	if !retentionHas(retentionFind(t, r, retentionSHA(102)).Reasons, "release_unproven") || !retentionHas(r.Blockers, "release_lineage_incomplete_or_external") {
		t.Fatal(r)
	}
}
func TestRetentionHashClockPermutationAndEvidence(t *testing.T) {
	st := retentionBase()
	for n := 0; n < 12; n++ {
		retentionGHA(&st, n, "default", retentionNow.Add(-time.Duration(n+1)*time.Minute))
	}
	r := EvaluateRetention(st, "p", retentionNow)
	later := EvaluateRetention(st, "p", retentionNow.Add(time.Minute))
	if r.InputHash != later.InputHash || r.EvaluatedAt == later.EvaluatedAt {
		t.Fatal("raw clock in input fingerprint")
	}
	reverse := func(v any) {
		x := reflect.ValueOf(v)
		for i, j := 0, x.Len()-1; i < j; i, j = i+1, j-1 {
			a, b := x.Index(i), x.Index(j)
			tmp := reflect.New(a.Type()).Elem()
			tmp.Set(a)
			a.Set(b)
			b.Set(tmp)
		}
	}
	reverse(st.DeliveryArtifacts)
	reverse(st.Operations)
	reverse(st.DeliveryBuildRuns)
	if EvaluateRetention(st, "p", retentionNow).InputHash != r.InputHash {
		t.Fatal("array ordering changed canonical hash")
	}
	if EvaluateRetention(st, "p", retentionNow.Add(5*time.Minute)).InputHash != r.InputHash || EvaluateRetention(st, "p", retentionNow.Add(5*time.Minute+time.Nanosecond)).InputHash == r.InputHash {
		t.Fatal("freshness hash boundary incorrect")
	}
	st.DeliveryBuildRuns[0].Result.Artifacts["available"] = "false"
	if EvaluateRetention(st, "p", retentionNow).InputHash == r.InputHash {
		t.Fatal("relevant evidence not hashed")
	}
}
func TestRetentionDefaultRegistryRequiresFrozenGHAProof(t *testing.T) {
	st := retentionBase()
	retentionGHA(&st, 1, "", retentionNow.Add(-time.Minute))
	a := &st.DeliveryArtifacts[0]
	a.ImageRepository = "owner/app"
	st.Operations[0].Snapshot.Build.ImageRepository = a.ImageRepository
	st.DeliveryBuildRuns[0].Result.Artifacts["image_repository"] = a.ImageRepository
	r := EvaluateRetention(st, "p", retentionNow)
	v := retentionFind(t, r, a.Digest)
	if v.ImageRepository != "ghcr.io/owner/app" || !retentionHas(v.Profiles, "default") {
		t.Fatal(v)
	}
	st.Operations[0].Snapshot = nil
	r = EvaluateRetention(st, "p", retentionNow)
	if len(r.Images) != 0 || !retentionHas(r.Blockers, "unresolved_image_reference") {
		t.Fatal("ambiguous registry guessed", r)
	}
}

func TestRetentionDesiredPreparedChildrenAndFluxRoots(t *testing.T) {
	st := retentionBase()
	for n := 0; n < 14; n++ {
		retentionGHA(&st, n, "default", retentionNow.Add(-time.Duration(n+1)*time.Minute))
	}
	a := st.DeliveryArtifacts[11]
	st.Compositions = []Composition{{Meta: Meta{ID: "desired", ProductID: "p"}, Components: []CompositionComponent{{ApplicationID: "app", BaseCommit: a.SourceCommit}}}}
	snap := DeliverySnapshot{Application: st.Applications[0], Build: ComponentBuild{ImageRepository: "ghcr.io/owner/app"}, Revision: IntegrationRevision{HeadCommit: st.DeliveryArtifacts[12].SourceCommit}}
	parent := ExternalOperation{Meta: Meta{ID: "parent", ProductID: "p"}, Status: "SUCCEEDED", ChildIDs: []string{"child"}, PreparedDeployments: []DeliverySnapshot{snap}}
	child := ExternalOperation{Meta: Meta{ID: "child", ProductID: "p"}, ParentID: parent.ID, Status: "SUCCEEDED", ApplicationID: "app", EnvironmentID: "env", Snapshot: &snap, Artifact: &delivery.ArtifactResult{Digest: retentionSHA(13)}, GitOpsResult: &delivery.GitOpsResult{CommitSHA: "old-flux"}}
	st.Operations = append(st.Operations, parent, child)
	st.Environments = []Environment{{Meta: Meta{ID: "env", ProductID: "p"}, DesiredCompositionID: "desired", DesiredOperationID: "parent"}}
	// Flux proves only a known recorded image at this revision, not healthy pods.
	st.RuntimeSnapshots = []RuntimeSnapshot{{Meta: Meta{ID: "runtime", ProductID: "p"}, ApplicationID: "app", EnvironmentID: "env", Runtime: delivery.RuntimeSnapshot{ObservedAt: retentionNow, Health: "UNKNOWN", Flux: &delivery.RuntimeFluxSnapshot{Revision: "main@sha1:old-flux", Status: "NotReady"}}}}
	r := EvaluateRetention(st, "p", retentionNow)
	if !retentionHas(retentionFind(t, r, retentionSHA(11)).Reasons, "desired_composition") || !retentionHas(retentionFind(t, r, retentionSHA(12)).Reasons, "desired_operation") || !retentionHas(retentionFind(t, r, retentionSHA(13)).Reasons, "flux_reconciled") {
		t.Fatal(r)
	}
	// A new in-flight exact digest remains a root without an artifact registry row.
	st.Operations = append(st.Operations, ExternalOperation{Meta: Meta{ID: "inflight", ProductID: "p"}, Status: "RUNNING", ApplicationID: "app", Snapshot: &snap, ExpectedDigest: retentionSHA(70)})
	if !retentionFind(t, EvaluateRetention(st, "p", retentionNow), retentionSHA(70)).Protected {
		t.Fatal("inflight missing artifact root dropped")
	}
}
func TestRetentionWholeReleaseRejectsPartialGreen(t *testing.T) {
	st := retentionBase()
	retentionRelease(&st, 0, "prod/path")
	app := st.Applications[0]
	app.ID = "app-b"
	st.Applications = append(st.Applications, app)
	// An incomplete B entry prevents treating A as a proven whole release.
	st.Releases[0].ArtifactDigests["app-b"] = retentionSHA(201)
	b, _ := json.Marshal(st.Operations[1])
	var child ExternalOperation
	json.Unmarshal(b, &child)
	child.ID = "b-child"
	child.ApplicationID = "app-b"
	child.Snapshot.Application = app
	child.Artifact.Digest = retentionSHA(201)
	child.Snapshot.Target.Purpose = "DEV"
	st.Operations[0].ChildIDs = append(st.Operations[0].ChildIDs, child.ID)
	st.Operations = append(st.Operations, child)
	st.Releases[0].GitOpsCommits["app-b"] = child.GitOpsResult.CommitSHA
	st.Releases[0].SourceCommits["app-b"] = "source"
	st.RuntimeObservations = append(st.RuntimeObservations, RuntimeObservation{Meta: Meta{ProductID: "p", CreatedAt: st.Releases[0].CreatedAt.Add(-time.Second)}, OperationID: child.ID, EnvironmentID: "env", ArtifactDigest: child.Artifact.Digest, GitOpsCommit: child.GitOpsResult.CommitSHA, Healthy: true})
	r := EvaluateRetention(st, "p", retentionNow)
	for _, d := range []string{retentionSHA(100), retentionSHA(201)} {
		v := retentionFind(t, r, d)
		if !v.Protected || !retentionHas(v.Reasons, "release_unproven") || retentionHas(v.Reasons, "production_current") {
			t.Fatal("partial release declared production", v)
		}
	}
}
func TestRetentionHashCanonicalEffectivePolicyAndRuntimePodPermutation(t *testing.T) {
	st := retentionBase()
	retentionGHA(&st, 0, "default", retentionNow.Add(-time.Minute))
	st.Applications[0].Retention = &ArtifactRetention{KeepLast: 3}
	r := EvaluateRetention(st, "p", retentionNow)
	st.Applications[0].Retention.KeepLast = 4
	if r.InputHash != EvaluateRetention(st, "p", retentionNow).InputHash {
		t.Fatal("ineffective below-floor count changed effective-policy hash")
	}
	st.Applications[0].Retention.KeepLast = 11
	if r.InputHash == EvaluateRetention(st, "p", retentionNow).InputHash {
		t.Fatal("effective policy absent from hash")
	}
	st.RuntimeSnapshots = []RuntimeSnapshot{{Meta: Meta{ID: "runtime", ProductID: "p"}, ApplicationID: "app", Runtime: delivery.RuntimeSnapshot{ObservedAt: retentionNow, WorkloadImage: "ghcr.io/owner/app@" + retentionSHA(40), Pods: []delivery.RuntimePod{{UID: "one", Image: "ghcr.io/owner/app@" + retentionSHA(41)}, {UID: "two", Image: "ghcr.io/owner/app@" + retentionSHA(42)}}}}}
	r = EvaluateRetention(st, "p", retentionNow)
	pods := st.RuntimeSnapshots[0].Runtime.Pods
	pods[0], pods[1] = pods[1], pods[0]
	if r.InputHash != EvaluateRetention(st, "p", retentionNow).InputHash {
		t.Fatal("pod order changed canonical hash")
	}
	pods[0].Ready = true
	if r.InputHash == EvaluateRetention(st, "p", retentionNow).InputHash {
		t.Fatal("runtime evidence mutation not hashed")
	}
}

func TestRetentionForeignOnlyDigestUnionWithoutPayloadLeak(t *testing.T) {
	st := retentionBase()
	retentionGHA(&st, 0, "default", retentionNow.Add(-time.Minute))
	st.Products = append(st.Products, Product{Meta: Meta{ID: "foreign-product"}})
	st.ArtifactPins = []ArtifactPin{{Meta: Meta{ID: "foreign-pin", ProductID: "foreign-product"}, ApplicationID: "private-app", ImageRepository: "ghcr.io/owner/app", Digest: retentionSHA(999), Purpose: "manual"}}
	st.RuntimeSnapshots = []RuntimeSnapshot{{Meta: Meta{ID: "foreign-runtime", ProductID: "foreign-product"}, ApplicationID: "private-app", Runtime: delivery.RuntimeSnapshot{ObservedAt: retentionNow, WorkloadImage: "ghcr.io/owner/app@" + retentionSHA(998), Pods: []delivery.RuntimePod{{ImageID: "docker-pullable://different.example/private/app@" + retentionSHA(997)}}}}}
	r := EvaluateRetention(st, "p", retentionNow)
	if len(r.Images) != 3 {
		t.Fatal("foreign-only root union incomplete", r)
	}
	for _, n := range []int{998, 999} {
		v := retentionFind(t, r, retentionSHA(n))
		if !v.Protected || len(v.Reasons) != 1 || v.Reasons[0] != "shared_product_root" || len(v.ApplicationIDs)+len(v.ArtifactIDs)+len(v.Profiles) != 0 || v.PublishedAt != nil || v.Availability != "UNKNOWN" {
			t.Fatal("foreign scope leaked", v)
		}
	}
	b, _ := json.Marshal(r)
	for _, secret := range []string{"foreign-product", "foreign-pin", "foreign-runtime", "private-app", "different.example"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("foreign metadata leaked", secret)
		}
	}
}
func TestRetentionIncompleteReleaseKnownFrozenRepositoryNoArtifacts(t *testing.T) {
	for _, mode := range []string{"no-gitops", "failed-child", "wrong-commit", "missing-listed-child", "conflicting-digest"} {
		t.Run(mode, func(t *testing.T) {
			st := retentionBase()
			retentionRelease(&st, 0, "prod/path")
			st.RuntimeObservations = nil
			child := &st.Operations[1]
			switch mode {
			case "no-gitops":
				child.GitOpsResult = nil
			case "failed-child":
				child.Status = "FAILED"
			case "wrong-commit":
				st.Releases[0].GitOpsCommits["app"] = "other"
			case "missing-listed-child":
				st.Operations[0].ChildIDs = nil
			case "conflicting-digest":
				child.Artifact.Digest = retentionSHA(102)
			}
			r := EvaluateRetention(st, "p", retentionNow)
			v := retentionFind(t, r, retentionSHA(100))
			if !v.Protected || !retentionHas(v.Reasons, "release_unproven") || retentionHas(v.Reasons, "production_current") || !retentionHas(r.Blockers, "release_lineage_incomplete_or_external") {
				t.Fatal("incomplete release lost frozen image", v, r.Blockers)
			}
			if mode == "conflicting-digest" && !retentionHas(retentionFind(t, r, retentionSHA(102)).Reasons, "release_unproven") {
				t.Fatal("conflicting child image lost")
			}
		})
	}
}
func TestRetentionSuccessfulPublicationWithoutValidAdmission(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt", "normal-history"} {
		t.Run(mode, func(t *testing.T) {
			st := retentionBase()
			for n := 0; n < 12; n++ {
				retentionNative(&st, n, retentionNow.Add(-time.Duration(n+1)*time.Minute))
			}
			switch mode {
			case "missing":
				st.DeliveryArtifacts = st.DeliveryArtifacts[:11]
			case "corrupt":
				st.DeliveryArtifacts[11].Native.PublicationInputHash = "wrong"
			}
			r := EvaluateRetention(st, "p", retentionNow)
			v := retentionFind(t, r, retentionSHA(11))
			if mode == "normal-history" {
				if v.Protected || retentionHas(r.Blockers, "publication_admission_missing_or_invalid") {
					t.Fatal("completed history permanently pinned", v)
				}
			} else if !v.Protected || !retentionHas(v.Reasons, "worker_publication_receipt") || !retentionHas(r.Blockers, "publication_admission_missing_or_invalid") {
				t.Fatal("unadmitted published bytes unrooted", v, r.Blockers)
			}
		})
	}
}
func TestRetentionManagedAcceptedBuildReceiptBeforePublication(t *testing.T) {
	st := retentionBase()
	a := retentionNative(&st, 11, retentionNow.Add(-time.Minute))
	st.DeliveryArtifacts = nil
	st.WorkerJobs = st.WorkerJobs[:1]
	st.WorkerAttempts = st.WorkerAttempts[:1]
	j := &st.WorkerJobs[0]
	j.Inputs.SourceProof = &WorkerSourceProof{DeliveryID: "delivery"}
	j.InputHash = checkHash(j.Inputs)
	attempt := &st.WorkerAttempts[0]
	attempt.InputHash = j.InputHash
	attempt.Report.InputHash = j.InputHash
	attempt.Report.LocalOCI.InputHash = j.InputHash
	st.Operations = []ExternalOperation{{Meta: Meta{ID: "delivery", ProductID: "p"}, Status: "RUNNING", Snapshot: &DeliverySnapshot{Application: st.Applications[0], Build: ComponentBuild{Mode: "NATIVE", ImageRepository: "ghcr.io/owner/app"}}}}
	v := retentionFind(t, EvaluateRetention(st, "p", retentionNow), a.Digest)
	if !retentionHas(v.Reasons, "accepted_build_receipt") || v.Availability != "UNKNOWN" {
		t.Fatal(v)
	}
}
func TestRetentionUnqualifiedRegistryRejectsCopiedOrCorruptScope(t *testing.T) {
	for _, mode := range []string{"artifact-product", "artifact-app", "snapshot-product", "build-product", "build-app", "source-repository", "source-product", "source-sha", "request-repository", "request-operation", "unsupported-mode", "foreign-original"} {
		t.Run(mode, func(t *testing.T) {
			st := retentionBase()
			retentionGHA(&st, 0, "default", retentionNow.Add(-time.Minute))
			a := &st.DeliveryArtifacts[0]
			a.ImageRepository = "owner/app"
			o := &st.Operations[0]
			o.Snapshot.Build.ImageRepository = a.ImageRepository
			st.DeliveryBuildRuns[0].Result.Artifacts["image_repository"] = a.ImageRepository
			switch mode {
			case "artifact-product":
				a.ProductID = "other"
			case "artifact-app":
				a.ApplicationID = "other"
			case "snapshot-product":
				o.Snapshot.Application.ProductID = "other"
			case "build-product":
				o.Snapshot.Build.ProductID = "other"
			case "build-app":
				o.Snapshot.Build.ApplicationID = "other"
			case "source-repository":
				o.Snapshot.Source.RepositoryID = "other"
			case "source-product":
				o.Snapshot.Source.ProductID = "other"
			case "source-sha":
				a.SourceCommit = "other"
			case "request-repository":
				a.BuildRequest.Repository = "other/repo"
			case "request-operation":
				a.BuildRequest.OperationID = "other"
			case "unsupported-mode":
				o.Snapshot.Build.Mode = "OTHER"
			case "foreign-original":
				o.ProductID = "other"
			}
			r := EvaluateRetention(st, "p", retentionNow)
			if len(r.Images) != 0 {
				t.Fatal("ambiguous default-registry alias resolved", mode, r)
			}
			if mode != "artifact-product" && !retentionHas(r.Blockers, "unresolved_image_reference") {
				t.Fatal("ambiguous alias lacked blocker")
			}
		})
	}
}
func TestRetentionSemanticMergeOrderBoundInHash(t *testing.T) {
	st := retentionBase()
	st.Compositions = []Composition{{Meta: Meta{ID: "comp", ProductID: "p"}, Components: []CompositionComponent{{ApplicationID: "app", RevisionIDs: []string{"a", "b"}}}}}
	r := EvaluateRetention(st, "p", retentionNow)
	st.Compositions[0].Components[0].RevisionIDs = []string{"b", "a"}
	if r.InputHash == EvaluateRetention(st, "p", retentionNow).InputHash {
		t.Fatal("semantic merge order normalized away")
	}
}

func TestRetentionFluxAndDesiredFrozenChildExpectedDigestWithoutArtifact(t *testing.T) {
	st := retentionBase()
	snap := DeliverySnapshot{Application: st.Applications[0], Build: ComponentBuild{ImageRepository: "ghcr.io/owner/app"}}
	st.Operations = []ExternalOperation{{Meta: Meta{ID: "parent", ProductID: "p"}, Status: "SUCCEEDED", ChildIDs: []string{"child"}}, {Meta: Meta{ID: "child", ProductID: "p"}, ParentID: "parent", Status: "SUCCEEDED", EnvironmentID: "env", ApplicationID: "app", Snapshot: &snap, ExpectedDigest: retentionSHA(77), GitOpsResult: &delivery.GitOpsResult{CommitSHA: "flux-commit"}}}
	st.Environments = []Environment{{Meta: Meta{ProductID: "p"}, DesiredOperationID: "parent"}}
	st.RuntimeSnapshots = []RuntimeSnapshot{{Meta: Meta{ProductID: "p"}, EnvironmentID: "env", ApplicationID: "app", Runtime: delivery.RuntimeSnapshot{ObservedAt: retentionNow, Flux: &delivery.RuntimeFluxSnapshot{Revision: "main@sha1:flux-commit"}}}}
	v := retentionFind(t, EvaluateRetention(st, "p", retentionNow), retentionSHA(77))
	if !retentionHas(v.Reasons, "desired_operation") || !retentionHas(v.Reasons, "flux_reconciled") {
		t.Fatal(v)
	}
}

func TestRetentionSharedPinLifecycleOpaqueHashAndScope(t *testing.T) {
	st := retentionBase()
	retentionGHA(&st, 0, "default", retentionNow.Add(-time.Minute))
	st.Products = append(st.Products, Product{Meta: Meta{ID: "foreign-private-product"}})
	st.Applications = append(st.Applications, Application{Meta: Meta{ID: "foreign-private-app", ProductID: "foreign-private-product"}})
	for _, key := range []string{"one", "two"} {
		pin := ArtifactPin{Meta: Meta{ID: "foreign-private-pin-" + key, ProductID: "foreign-private-product", Actor: "foreign-private-actor", CreatedAt: retentionNow, UpdatedAt: retentionNow}, ApplicationID: "foreign-private-app", ImageRepository: "ghcr.io/owner/app", Digest: retentionSHA(0), Purpose: "rollback", RequestKey: key}
		pin.RequestHash = ArtifactPinRequestHash(pin.ProductID, pin.ApplicationID, pin.ImageRepository, pin.Digest, pin.Purpose, pin.RequestKey)
		st.ArtifactPins = append(st.ArtifactPins, pin)
	}
	if e := ValidateArtifactPins(st); e != nil {
		t.Fatal(e)
	}
	before := EvaluateRetention(st, "p", retentionNow.Add(2*time.Second))
	artifactPinReleaseProof(&st)
	if e := ValidateArtifactPins(st); e != nil {
		t.Fatal(e)
	}
	after := EvaluateRetention(st, "p", retentionNow.Add(2*time.Second))
	a, b := before, after
	a.InputHash = ""
	b.InputHash = ""
	if !reflect.DeepEqual(a, b) {
		t.Fatal("unchanged shared root projection changed", a, b)
	}
	if before.InputHash == after.InputHash {
		t.Fatal("foreign release evidence omitted while another pin retains same shared root")
	}
	for _, r := range []RetentionReport{before, after} {
		v := retentionFind(t, r, retentionSHA(0))
		if !v.Protected || !retentionHas(v.Reasons, "shared_product_root") {
			t.Fatal(v)
		}
		encoded, _ := json.Marshal(r)
		if strings.Contains(string(encoded), "foreign-private") {
			t.Fatal("foreign identifiers escaped opaque hash")
		}
	}
	// Pin/event collection order and clock movement within the same freshness
	// classes have no semantic effect on the scoped evidence fingerprint.
	st.ArtifactPins[0], st.ArtifactPins[1] = st.ArtifactPins[1], st.ArtifactPins[0]
	if got := EvaluateRetention(st, "p", retentionNow.Add(3*time.Second)); got.InputHash != after.InputHash {
		t.Fatal("collection order or raw clock changed opaque hash")
	}
	// Even the same foreign application can own another private registry image.
	// Its independent protection and runtime facts are outside the shared scope.
	pin := st.ArtifactPins[0]
	pin.ID = "foreign-private-unshared-pin"
	pin.RequestKey = "unshared"
	pin.ImageRepository = "private.example/secret/app"
	pin.Digest = retentionSHA(900)
	pin.ReleasedAt = nil
	pin.UpdatedAt = pin.CreatedAt
	pin.RequestHash = ArtifactPinRequestHash(pin.ProductID, pin.ApplicationID, pin.ImageRepository, pin.Digest, pin.Purpose, pin.RequestKey)
	st.ArtifactPins = append(st.ArtifactPins, pin)
	st.RuntimeSnapshots = append(st.RuntimeSnapshots, RuntimeSnapshot{Meta: Meta{ID: "foreign-private-runtime", ProductID: pin.ProductID}, ApplicationID: pin.ApplicationID, Runtime: delivery.RuntimeSnapshot{ObservedAt: retentionNow, WorkloadImage: pin.ImageRepository + "@" + pin.Digest, Pods: []delivery.RuntimePod{{ImageID: "docker-pullable://" + pin.ImageRepository + "@" + pin.Digest}}}})
	if e := ValidateArtifactPins(st); e != nil {
		t.Fatal(e)
	}
	if got := EvaluateRetention(st, "p", retentionNow.Add(2*time.Second)); got.InputHash != after.InputHash {
		t.Fatal("unshared foreign repository churned local fingerprint")
	}
	st.RuntimeSnapshots[0].Runtime.Pods[0].Ready = true
	if got := EvaluateRetention(st, "p", retentionNow.Add(2*time.Second)); got.InputHash != after.InputHash {
		t.Fatal("unshared foreign runtime mutation churned local fingerprint")
	}
}

func retentionAssertEvidenceChange(t *testing.T, before, after RetentionReport) {
	t.Helper()
	x, y := before, after
	x.InputHash = ""
	y.InputHash = ""
	if !reflect.DeepEqual(x, y) {
		t.Fatal("fixture changed public retention projection", x, y)
	}
	if before.InputHash == after.InputHash {
		t.Fatal("consumed evidence change missing from InputHash")
	}
}
func TestRetentionLocalRunEvidenceBeyondEarliestPublication(t *testing.T) {
	for _, mode := range []string{"later-qualified-observation", "unknown-run-conclusion"} {
		t.Run(mode, func(t *testing.T) {
			st := retentionBase()
			retentionGHA(&st, 0, "default", retentionNow.Add(-2*time.Minute))
			if mode == "later-qualified-observation" {
				later := st.DeliveryBuildRuns[0]
				later.ID = "later-qualifying-report"
				later.Result.Artifacts = map[string]string{}
				for k, v := range st.DeliveryBuildRuns[0].Result.Artifacts {
					later.Result.Artifacts[k] = v
				}
				later.Result.Artifacts["observed_at"] = retentionNow.Add(-time.Minute).Format(time.RFC3339Nano)
				st.DeliveryBuildRuns = append(st.DeliveryBuildRuns, later)
			} else {
				st.DeliveryBuildRuns[0].Result.Conclusion = "failure"
			}
			before := EvaluateRetention(st, "p", retentionNow)
			if mode == "later-qualified-observation" {
				st.DeliveryBuildRuns[1].Result.Artifacts["observed_at"] = retentionNow.Add(-45 * time.Second).Format(time.RFC3339Nano)
			} else {
				st.DeliveryBuildRuns[0].Result.Conclusion = "cancelled"
			}
			after := EvaluateRetention(st, "p", retentionNow)
			retentionAssertEvidenceChange(t, before, after)
			if mode == "later-qualified-observation" {
				v := retentionFind(t, after, retentionSHA(0))
				if v.PublishedAt == nil || !v.PublishedAt.Equal(retentionNow.Add(-2*time.Minute)) {
					t.Fatal("earliest publication boundary changed")
				}
				st.DeliveryBuildRuns[0], st.DeliveryBuildRuns[1] = st.DeliveryBuildRuns[1], st.DeliveryBuildRuns[0]
				if EvaluateRetention(st, "p", retentionNow).InputHash != after.InputHash {
					t.Fatal("run record permutation changed normalized hash")
				}
			}
			st.DeliveryBuildRuns[0].Result.URL = "https://example.invalid/unconsumed-run-log"
			if EvaluateRetention(st, "p", retentionNow).InputHash != after.InputHash {
				t.Fatal("unconsumed provider URL included in hash")
			}
		})
	}
}
func TestRetentionRuntimeErrorPresenceAndReleaseSourceEvidenceHash(t *testing.T) {
	t.Run("per-snapshot-error-presence", func(t *testing.T) {
		st := retentionBase()
		for _, id := range []string{"existing-error", "new-error"} {
			st.RuntimeSnapshots = append(st.RuntimeSnapshots, RuntimeSnapshot{Meta: Meta{ID: id, ProductID: "p"}, ApplicationID: "app", Runtime: delivery.RuntimeSnapshot{ObservedAt: retentionNow, WorkloadImage: "ghcr.io/owner/app@" + retentionSHA(80)}})
		}
		st.RuntimeSnapshots[0].Runtime.Errors = []string{"existing fixture error"}
		before := EvaluateRetention(st, "p", retentionNow)
		st.RuntimeSnapshots[1].Runtime.Errors = []string{"new fixture error"}
		after := EvaluateRetention(st, "p", retentionNow)
		retentionAssertEvidenceChange(t, before, after)
		st.RuntimeSnapshots[1].Runtime.Errors = []string{"different raw error details"}
		if EvaluateRetention(st, "p", retentionNow).InputHash != after.InputHash {
			t.Fatal("raw error payload hashed instead of normalized presence")
		}
		st.RuntimeSnapshots[0], st.RuntimeSnapshots[1] = st.RuntimeSnapshots[1], st.RuntimeSnapshots[0]
		if EvaluateRetention(st, "p", retentionNow).InputHash != after.InputHash {
			t.Fatal("snapshot order changed hash")
		}
	})
	t.Run("incomplete-release-source-map", func(t *testing.T) {
		st := retentionBase()
		retentionRelease(&st, 0, "prod/path")
		st.Operations[1].Snapshot.Revision.HeadCommit = strings.Repeat("a", 40)
		st.Releases[0].SourceCommits["app"] = strings.Repeat("b", 40)
		before := EvaluateRetention(st, "p", retentionNow)
		st.Releases[0].SourceCommits["app"] = strings.Repeat("c", 40)
		after := EvaluateRetention(st, "p", retentionNow)
		retentionAssertEvidenceChange(t, before, after)
		if !retentionHas(retentionFind(t, after, retentionSHA(100)).Reasons, "release_unproven") {
			t.Fatal("fixture not incomplete")
		}
	})
}
