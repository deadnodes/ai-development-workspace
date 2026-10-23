package workerrunner

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/domain"
)

// The images below were built and published by the enclosing native pipeline.
// Only GitHub/Kubernetes responses and the separately performed merge are peers.
func assertRealGroupedDevRestoration(t *testing.T, hold, capture, candidate domain.ExternalOperation, baselines []domain.DeliveryArtifact, peer *candidateRuntimeHTTPFixture, gitops *candidateGitOpsHTTPFixture, ticks func(), read func() domain.State, operation func(string) domain.ExternalOperation, selection func() domain.EnvironmentSelection, request func(string, map[string]any, any, bool), registryFault func(bool)) {
	t.Helper()
	before := read()
	baselineBytes, _ := json.Marshal(capture)
	candidateBytes, _ := json.Marshal(candidate)
	gitops.mu.Lock()
	candidateHead := gitops.head
	candidatePR := gitops.prs[0]
	candidatePRBytes, _ := json.Marshal(candidatePR)
	gitops.mu.Unlock()
	priorSelection := selection()
	var restoration domain.ExternalOperation
	request("request-restoration", map[string]any{"reservation_operation_id": hold.ID, "request_key": "real-batch-restore-request", "expected_selection": priorSelection}, &restoration, true)
	recoverySelection := selection()
	if recoverySelection.Generation <= priorSelection.Generation || recoverySelection.CompositionID != priorSelection.CompositionID || recoverySelection.OperationID != priorSelection.OperationID {
		t.Fatal("restoration did not fence selection while preserving desired composition")
	}
	ticks()
	restoration = operation(restoration.ID)
	if restoration.DevRestoration == nil || restoration.DevRestorationPlan == nil || restoration.DevRestorationPlan.Hash == "" || restoration.Phase != "WAIT_EXTERNAL_MERGE" || len(restoration.DevRestorationPlan.Groups) != 1 || len(restoration.DevRestorationPlan.Components) != 2 {
		t.Fatalf("complete restoration proposal absent %+v", restoration)
	}
	plan := restoration.DevRestorationPlan
	if plan.Groups[0].Batch == nil || len(plan.Groups[0].Batch.Changes) != 2 || plan.Groups[0].PullRequest == nil {
		t.Fatal("restoration omitted grouped original members")
	}
	request("release-after-restoration", map[string]any{"restoration_operation_id": restoration.ID, "restoration_verification_operation_id": restoration.ID, "request_key": "real-restore-premature-unlock", "expected_selection": recoverySelection}, nil, false)
	request("confirm-restoration", map[string]any{"restoration_operation_id": restoration.ID, "expected_plan_hash": strings.Repeat("0", 64), "request_key": "real-restore-stale-confirmation", "expected_selection": recoverySelection}, nil, false)
	var confirmation domain.ExternalOperation
	request("confirm-restoration", map[string]any{"restoration_operation_id": restoration.ID, "expected_plan_hash": plan.Hash, "request_key": "real-restore-attributed-confirmation", "expected_selection": recoverySelection}, &confirmation, true)
	if confirmation.Status != "SUCCEEDED" || confirmation.DevRestorationConfirmation == nil || confirmation.DevRestorationConfirmation.TrustGrade != "ATTRIBUTED_CONTROL_CONFIRMATION" {
		t.Fatalf("separate attributed confirmation missing %+v", confirmation)
	}
	gitops.mu.Lock()
	restoreHead := gitops.head
	restorePR := gitops.pr
	historicalBytes, _ := json.Marshal(gitops.prs[0])
	actual := gitops.blobs[gitops.fileBlob(restoreHead, "dev.yaml")]
	expected := gitops.initial
	for _, original := range baselines {
		expected = strings.ReplaceAll(expected, "'"+original.ImageRepository+"'", strconv.Quote(original.ImageRepository))
		expected = strings.ReplaceAll(expected, "'old@"+original.Digest+"'", strconv.Quote("rcp@"+original.Digest))
	}
	preserved := len(gitops.prs) == 2 && gitops.prs[0]["head"].(map[string]any)["sha"] == candidateHead && bytes.Equal(candidatePRBytes, historicalBytes) && restorePR["number"] != candidatePR["number"] && gitops.blobs[gitops.fileBlob(restoreHead, "prod.yaml")] == gitops.initial && gitops.blobs[gitops.fileBlob(restoreHead, "README.md")] == "untouched sibling\n"
	writes := append([]string(nil), gitops.mutations...)
	gitops.mu.Unlock()
	if actual != expected || !preserved || len(writes) != 8 {
		t.Fatalf("restoration changed original bytes/history or repeated physical writes: preserved=%v writes=%v\nactual=%s\nexpected=%s", preserved, writes, actual, expected)
	}
	// This external merge follows the attributed control receipt. It is not a
	// real human text/voice decision or an agent merge capability.
	time.Sleep(2 * time.Second) // Real wait: GitHub merge timestamps have second precision.
	gitops.externalMerge(t)
	for _, original := range baselines {
		if original.ApplicationID == "component" {
			peer.setImage(original.ImageRepository, original.Digest)
		} else {
			for _, role := range []string{"b-api", "b-worker"} {
				peer.setRoleImage(role, original.ImageRepository, original.Digest)
			}
		}
	}
	ticks()
	restoration = operation(restoration.ID)
	if restoration.Status != "SUCCEEDED" || restoration.DeploymentState != "GITOPS_APPLIED" {
		t.Fatalf("restoration originals not applied %+v", restoration)
	}
	verify := func(key string) domain.ExternalOperation {
		var result domain.ExternalOperation
		request("verify-restoration", map[string]any{"restoration_operation_id": restoration.ID, "request_key": key, "expected_selection": recoverySelection}, &result, true)
		ticks()
		return operation(result.ID)
	}
	release := func(v domain.ExternalOperation, key string, want bool) domain.ExternalOperation {
		var result domain.ExternalOperation
		request("release-after-restoration", map[string]any{"restoration_operation_id": restoration.ID, "restoration_verification_operation_id": v.ID, "request_key": key, "expected_selection": recoverySelection}, &result, want)
		return result
	}
	good := verify("real-restore-all-original-roles")
	assertRestoredRuntime := func(v domain.ExternalOperation) {
		t.Helper()
		if v.Status != "SUCCEEDED" || v.Phase != "RESTORATION_VERIFIED" || v.DevRestorationVerification == nil || v.DevRuntimeResult == nil || len(v.DevRuntimeResult.Components) != 2 {
			t.Fatalf("restoration missing complete fresh runtime %+v", v)
		}
		for _, component := range v.DevRuntimeResult.Components {
			found := false
			for _, original := range baselines {
				if original.ApplicationID == component.ApplicationID {
					found = len(component.Roles) == 2 && component.GitOps.HeadSHA == restoreHead && component.GitOps.ImageRepository == original.ImageRepository && component.GitOps.Digest == original.Digest
				}
			}
			if !found {
				t.Fatal("restoration omitted original image/api+worker", component.ApplicationID)
			}
		}
	}
	assertRestoredRuntime(good)
	peer.setWrongDigest(true)
	bad := verify("real-restore-newer-wrong-image-proof")
	peer.setWrongDigest(false)
	if bad.Status == "SUCCEEDED" {
		t.Fatal("restoration accepted wrong restored imageID")
	}
	release(good, "real-restore-reject-older-pass", false)
	registryFault(true)
	missing := verify("real-restore-original-bytes-missing")
	registryFault(false)
	if missing.Status == "SUCCEEDED" {
		t.Fatal("restoration accepted missing original bytes")
	}
	release(good, "real-restore-reject-missing-bytes", false)
	final := verify("real-restore-fresh-complete-proof")
	assertRestoredRuntime(final)
	released := release(final, "real-restore-explicit-scoped-unlock", true)
	if released.Status != "SUCCEEDED" || released.Phase != "RELEASED" || released.DevRestorationRelease == nil {
		t.Fatalf("explicit restoration successor unlock absent %+v", released)
	}
	after := read()
	gitops.mu.Lock()
	finalWrites := len(gitops.mutations)
	finalPRs := len(gitops.prs)
	gitops.mu.Unlock()
	if finalWrites != 8 || finalPRs != 2 {
		t.Fatal("verification/unlock repeated restoration proposal writes", finalWrites, finalPRs)
	}
	afterBaseline, _ := json.Marshal(operation(capture.ID))
	afterCandidate, _ := json.Marshal(operation(candidate.ID))
	if !bytes.Equal(baselineBytes, afterBaseline) || !bytes.Equal(candidateBytes, afterCandidate) || !reflect.DeepEqual(before.DeliveryArtifacts, after.DeliveryArtifacts) || !reflect.DeepEqual(before.ComponentBuilds, after.ComponentBuilds) || len(before.WorkerJobs) != len(after.WorkerJobs) || !reflect.DeepEqual(before.Applications, after.Applications) || !reflect.DeepEqual(before.RepositoryBindings, after.RepositoryBindings) || !reflect.DeepEqual(before.EnvironmentBindings, after.EnvironmentBindings) || len(before.Releases) != len(after.Releases) {
		t.Fatal("restoration rebuilt/published or rewrote original native provenance/configuration")
	}
	if selection().Generation <= recoverySelection.Generation {
		t.Fatal("scoped unlock did not fence recovery selection")
	}
	peer.assertReadOnly(t)
	t.Log("ACTUAL temporary Git/native BuildKit/spool/Distribution images and local/isolated PostgreSQL persistence reused without extra builds/publications; production grouped GitHub adapter and read-only HTTPS runtime observer: restoration proposal, separate ATTRIBUTED_CONTROL_CONFIRMATION, simulated external merge, all four original API/worker roles, failed-newer-proof and missing-byte rejection, explicit scoped unlock. GitHub/Kubernetes/merge peers SIMULATED; no actual human text/voice identity, agent protected merge, live Dev/Prod or physical GC claim")
}
