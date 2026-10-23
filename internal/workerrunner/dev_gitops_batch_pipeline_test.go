package workerrunner

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
)

func (f *candidateGitOpsHTTPFixture) configureBatchBaseline(artifacts []domain.DeliveryArtifact) {
	f.configureBaseline(artifacts[0].ImageRepository, artifacts[0].Digest)
	f.mu.Lock()
	defer f.mu.Unlock()
	selected, untouched, _ := strings.Cut(f.initial, "---\napiVersion: v1")
	second := strings.NewReplacer(artifacts[0].ImageRepository, artifacts[1].ImageRepository, artifacts[0].Digest, artifacts[1].Digest, "name: api\n", "name: b-api\n", "name: worker\n", "name: b-worker\n").Replace(selected)
	f.initial = selected + "---\n" + second + "---\napiVersion: v1" + untouched
	blob := candidateGitHash("blob", f.initial)
	f.blobs[blob] = f.initial
	sibling := candidateGitHash("blob", "untouched sibling\n")
	tree := candidateGitHash("tree", "batch-base"+blob)
	f.trees[tree] = []map[string]string{{"path": "dev.yaml", "mode": "100644", "type": "blob", "sha": blob}, {"path": "prod.yaml", "mode": "100644", "type": "blob", "sha": blob}, {"path": "README.md", "mode": "100644", "type": "blob", "sha": sibling}}
	base := candidateGitHash("commit", "batch-base"+tree)
	f.commits[base] = map[string]any{"sha": base, "message": "Original actual native Dev pair", "tree": map[string]string{"sha": tree}, "parents": []any{}}
	f.refs["main"] = base
}

func assertRealGroupedDevGitOpsPipeline(t *testing.T, endpoint, token string, candidate domain.ExternalOperation, baselines []domain.DeliveryArtifact, provider *mainCandidateActivationProvider, service *application.Service, ticks func(), read func() domain.State, registryFault func(bool)) {
	t.Helper()
	operation := func(id string) domain.ExternalOperation {
		for _, o := range read().Operations {
			if o.ID == id {
				return o
			}
		}
		t.Fatal("batch operation missing", id)
		return domain.ExternalOperation{}
	}
	selection := func() domain.EnvironmentSelection {
		for _, e := range read().Environments {
			if e.ID == "dev" {
				return domain.EnvironmentSelection{CompositionID: e.DesiredCompositionID, OperationID: e.DesiredOperationID, Generation: e.SelectionGeneration}
			}
		}
		t.Fatal("Dev missing")
		return domain.EnvironmentSelection{}
	}
	request := func(path string, data map[string]any, out any, want bool) {
		t.Helper()
		data["actor"] = "fixture/operator"
		body, _ := json.Marshal(data)
		r, _ := http.NewRequest(http.MethodPost, endpoint+"/api/products/p/candidate-dev/"+path, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		if want && response.StatusCode != 200 || !want && (response.StatusCode < 400 || response.StatusCode >= 500) {
			t.Fatalf("batch named %s HTTP%d %s", path, response.StatusCode, raw)
		}
		if want && out != nil {
			if err := json.Unmarshal(raw, out); err != nil {
				t.Fatal(err)
			}
		}
	}
	peer, observers := newCandidateRuntimeHTTPFixture(t, baselines[0].ImageRepository, baselines[0].Digest, provider.gitops)
	for _, role := range []string{"b-api", "b-worker"} {
		peer.setRoleImage(role, baselines[1].ImageRepository, baselines[1].Digest)
	}
	observers = append(observers, peer.observers(t, "component-b", []string{"b-api", "b-worker"})...)
	service.SetRuntimeObservers(observers)
	before := read()
	candidateBefore, _ := json.Marshal(candidate)
	var hold, capture, activation domain.ExternalOperation
	request("reserve", map[string]any{"candidate_operation_id": candidate.ID, "environment_id": "dev", "request_key": "real-batch-reserve", "expected_selection": selection()}, &hold, true)
	heldSelection := selection()
	heldEnvironments := read().Environments
	// Both negative captures exercise the same real registry and TLS observer
	// admission as the successful complete api+worker capture.
	peer.setWrongDigest(true)
	request("capture-baseline", map[string]any{"reservation_operation_id": hold.ID, "request_key": "real-batch-wrong-pod-image", "expected_selection": heldSelection}, &capture, true)
	ticks()
	if operation(capture.ID).Status == "SUCCEEDED" {
		t.Fatal("batch accepted wrong original pod imageID")
	}
	peer.setWrongDigest(false)
	registryFault(true)
	request("capture-baseline", map[string]any{"reservation_operation_id": hold.ID, "request_key": "real-batch-missing-original-bytes", "expected_selection": heldSelection}, &capture, true)
	ticks()
	registryFault(false)
	if operation(capture.ID).Status == "SUCCEEDED" {
		t.Fatal("batch capture accepted missing original registry bytes")
	}
	request("deploy", map[string]any{"reservation_operation_id": hold.ID, "request_key": "real-batch-reject-incomplete-baseline", "expected_selection": heldSelection}, nil, false)
	request("capture-baseline", map[string]any{"reservation_operation_id": hold.ID, "request_key": "real-batch-original-dev", "expected_selection": heldSelection}, &capture, true)
	ticks()
	capture = operation(capture.ID)
	if capture.Status != "SUCCEEDED" || capture.DevBaseline == nil || capture.DevBaselineResult == nil || len(capture.DevBaseline.Components) != 2 || len(capture.DevBaselineResult.Components) != 2 {
		t.Fatalf("batch original capture incomplete %+v", capture)
	}
	for _, c := range capture.DevBaseline.Components {
		if len(c.Scopes) != 2 {
			t.Fatal("batch capture omitted component api/worker", c.ApplicationID)
		}
	}
	for _, base := range baselines {
		found := false
		for _, c := range capture.DevBaselineResult.Components {
			if c.ApplicationID == base.ApplicationID {
				found = c.ArtifactID == base.ID && c.ImageRepository == base.ImageRepository && c.Digest == base.Digest
			}
		}
		if !found {
			t.Fatal("batch baseline native artifact identity changed", base.ApplicationID)
		}
	}
	baselineProof, _ := json.Marshal(capture)
	assertRealBaselineRetained(t, endpoint, token, capture)
	provider.gitops.loseNextPRReply()
	request("deploy", map[string]any{"reservation_operation_id": hold.ID, "request_key": "real-batch-deploy", "expected_selection": heldSelection}, &activation, true)
	ticks()
	activation = operation(activation.ID)
	if len(activation.ChildIDs) != 2 || activation.DevActivation == nil || len(activation.DevActivation.Groups) != 1 || len(activation.DevActivation.Groups[0].MemberIDs) != 2 {
		t.Fatalf("batch activation children missing %+v", activation)
	}
	desired := map[string]domain.ExternalOperation{}
	for _, id := range activation.ChildIDs {
		c := operation(id)
		group := activation.DevActivation.Groups[0]
		if c.DevActivationGroupID != group.ID {
			t.Fatal("batch child lost frozen group membership", c.ID)
		}
		if id == group.LeaderID {
			if c.Phase != "WAIT_GITOPS_PR" || c.DevActivationBatch == nil || c.GitOpsPR == nil || c.DevActivationIntentHash == "" || c.DevActivationPRHash == "" {
				t.Fatalf("leader lacks exact durable batch and verified common PR %+v", c)
			}
		} else if c.Phase != "APPLY" || c.DevActivationBatch != nil || c.GitOpsPR != nil || c.DevActivationIntentHash != "" || c.DevActivationPRHash != "" {
			t.Fatalf("follower gained proposal authority or left store-only APPLY %+v", c)
		}
		if c.Artifact == nil || c.ExistingArtifact == nil || c.Native != nil {
			t.Fatal("batch member rebuilt artifact")
		}
		desired[c.Snapshot.Application.ID] = c
	}
	f := provider.gitops
	f.mu.Lock()
	documents := strings.Split(f.initial, "---\n")
	for _, base := range baselines {
		c := desired[base.ApplicationID]
		t.Logf("component=%s original_dev source=%s image=%s@%s artifact=%s; independently_built_main_prod source=%s image=%s@%s artifact=%s", base.ApplicationID, base.SourceCommit, base.ImageRepository, base.Digest, base.ID, c.Snapshot.Revision.HeadCommit, c.Snapshot.Build.ImageRepository, c.Artifact.Digest, c.ExistingArtifact.ID)
		if c.Snapshot.Build.ImageRepository == base.ImageRepository {
			t.Fatal("batch did not exercise distinct Dev/Prod repositories")
		}
		for i, document := range documents {
			if strings.Contains(document, "repository: '"+base.ImageRepository+"'") {
				document = strings.ReplaceAll(document, "'"+base.ImageRepository+"'", strconv.Quote(c.Snapshot.Build.ImageRepository))
				documents[i] = strings.ReplaceAll(document, "'old@"+base.Digest+"'", strconv.Quote("rcp@"+c.Artifact.Digest))
			}
		}
	}
	expected := strings.Join(documents, "---\n")
	actual := f.blobs[f.fileBlob(f.head, "dev.yaml")]
	mutations := append([]string(nil), f.mutations...)
	recoveryWrites := f.recoveryWrites
	unchanged := f.blobs[f.fileBlob(f.head, "prod.yaml")] == f.initial && f.blobs[f.fileBlob(f.head, "README.md")] == "untouched sibling\n"
	f.mu.Unlock()
	if actual != expected || !unchanged {
		t.Fatalf("shared Helm batch changed unrelated exact bytes\nactual=%s\nexpected=%s", actual, expected)
	}
	want := []string{"POST /repos/fixture/gitops/git/trees", "POST /repos/fixture/gitops/git/commits", "POST /repos/fixture/gitops/git/refs", "POST /repos/fixture/gitops/pulls"}
	if !reflect.DeepEqual(mutations, want) || recoveryWrites != 0 || len(provider.proposals) != 1 || provider.lookups < 1 {
		t.Fatalf("batch not one commit/ref/PR with GET-only recovery: %v recovery=%d proposals=%d lookups=%d", mutations, recoveryWrites, len(provider.proposals), provider.lookups)
	}
	t.Logf("frozen group=%s leader=%s exact_members=%v one proposal=%s commit=%s; four exact guarded physical writes=%v", activation.DevActivation.Groups[0].ID, activation.DevActivation.Groups[0].LeaderID, activation.DevActivation.Groups[0].MemberIDs, f.branch, f.head, mutations)
	f.externalMerge(t)
	for id, c := range desired {
		if id == "component" {
			peer.setImage(c.Snapshot.Build.ImageRepository, c.Artifact.Digest)
		} else {
			for _, role := range []string{"b-api", "b-worker"} {
				peer.setRoleImage(role, c.Snapshot.Build.ImageRepository, c.Artifact.Digest)
			}
		}
	}
	ticks()
	activation = operation(activation.ID)
	if activation.Status != "SUCCEEDED" || activation.DeploymentState != "GITOPS_APPLIED" {
		t.Fatalf("group desired state not applied %+v", activation)
	}
	for _, id := range activation.ChildIDs {
		c := operation(id)
		if c.Status != "SUCCEEDED" || c.GitOpsPR == nil || c.GitOpsResult == nil || c.GitOpsResult.CommitSHA != f.head {
			t.Fatalf("member missing common exact group result %+v", c)
		}
	}
	var verification domain.ExternalOperation
	request("verify-runtime", map[string]any{"activation_operation_id": activation.ID, "request_key": "real-batch-all-four-roles", "expected_selection": heldSelection}, &verification, true)
	ticks()
	verification = operation(verification.ID)
	if verification.Status != "SUCCEEDED" || verification.Phase != "RUNTIME_VERIFIED" || verification.DevRuntimeResult == nil || len(verification.DevRuntimeResult.Components) != 2 {
		t.Fatalf("both components/api+worker did not verify %+v", verification)
	}
	for _, component := range verification.DevRuntimeResult.Components {
		want := desired[component.ApplicationID]
		if len(component.Roles) != 2 || component.GitOps.HeadSHA != f.head || component.GitOps.ImageRepository != want.Snapshot.Build.ImageRepository || component.GitOps.Digest != want.Artifact.Digest {
			t.Fatal("runtime proof omitted exact common group revision or component roles", component.ApplicationID)
		}
	}
	request("release", map[string]any{"reservation_operation_id": hold.ID, "request_key": "unsafe-batch-release", "expected_selection": heldSelection}, nil, false)
	after := read()
	unchangedCandidate, _ := json.Marshal(operation(candidate.ID))
	unchangedBaseline, _ := json.Marshal(operation(capture.ID))
	if !bytes.Equal(candidateBefore, unchangedCandidate) || !bytes.Equal(baselineProof, unchangedBaseline) || !reflect.DeepEqual(before.DeliveryArtifacts, after.DeliveryArtifacts) || len(before.WorkerJobs) != len(after.WorkerJobs) || !reflect.DeepEqual(before.ComponentBuilds, after.ComponentBuilds) || !reflect.DeepEqual(heldEnvironments, after.Environments) || !reflect.DeepEqual(before.EnvironmentBindings, after.EnvironmentBindings) || !reflect.DeepEqual(before.Applications, after.Applications) || !reflect.DeepEqual(before.RepositoryBindings, after.RepositoryBindings) || len(after.Releases) != len(before.Releases) {
		t.Fatal("batch capture/activation changed native provenance or built/published/released")
	}
	assertRealBaselineRetained(t, endpoint, token, capture)
	peer.assertReadOnly(t)
	assertRealGroupedDevRestoration(t, hold, capture, candidate, baselines, peer, f, ticks, read, operation, selection, request, registryFault)
	t.Log("ACTUAL temporary Git -> independent Dev/Prod native BuildKit/spool/publications -> Distribution; local+PostgreSQL metastore. SIMULATED GitHub HTTP and Kubernetes HTTPS peers/external merge: two components, four roles, distinct Dev/Prod repositories, one grouped commit/branch/PR, GET-only lost-response recovery, exact untouched YAML, original artifacts/baseline/roots preserved, zero capture/activation builds/publications; no live Dev/Prod or unlock claim")
}
