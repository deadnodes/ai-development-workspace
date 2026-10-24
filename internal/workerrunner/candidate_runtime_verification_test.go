package workerrunner

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"releasecontrol/internal/domain"
)

func assertRealCandidateRuntimeVerification(t *testing.T, endpoint, token, activation string, peer *candidateRuntimeHTTPFixture, ticks func(), inventoryTick func(), read func() domain.State, post func(domain.Command, any, bool)) {
	t.Helper()
	operation := func(id string) domain.ExternalOperation {
		for _, o := range read().Operations {
			if o.ID == id {
				return o
			}
		}
		t.Fatal("runtime operation absent", id)
		return domain.ExternalOperation{}
	}
	state := func() map[string]any {
		req, _ := http.NewRequest(http.MethodGet, endpoint+"/api/environments/dev/state", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var result map[string]any
		if response.StatusCode != 200 || json.NewDecoder(response.Body).Decode(&result) != nil {
			t.Fatal("runtime current-state query failed", response.StatusCode)
		}
		return result
	}
	var selection domain.EnvironmentSelection
	for _, env := range read().Environments {
		if env.ID == "dev" {
			selection = domain.EnvironmentSelection{CompositionID: env.DesiredCompositionID, OperationID: env.DesiredOperationID, Generation: env.SelectionGeneration}
		}
	}
	verify := func(key string) domain.ExternalOperation {
		payload := map[string]any{"actor": "fixture/operator", "activation_operation_id": activation, "request_key": key, "expected_selection": selection}
		b, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, endpoint+"/api/products/p/candidate-dev/verify-runtime", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		var result domain.ExternalOperation
		if response.StatusCode != 200 || json.Unmarshal(raw, &result) != nil {
			t.Fatalf("named runtime verification HTTP%d: %s", response.StatusCode, raw)
		}
		return result
	}
	before := read()
	positive := verify("real-runtime-verification-pass")
	ticks()
	passed := operation(positive.ID)
	current := state()
	if passed.Status != "SUCCEEDED" || passed.Phase != "RUNTIME_VERIFIED" || current["candidate_runtime_verified"] != true || current["candidate_runtime_verification_id"] != positive.ID {
		t.Fatalf("real HTTPS configured runtime not verified op=%+v current=%+v", passed, current)
	}
	encoded, _ := json.Marshal(passed)
	for _, secret := range []string{"must-not-persist-runtime-payload", "fixture-runtime-only", "fixture-token", "fixture-ca.pem"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("runtime proof leaked nonmetadata", secret)
		}
	}
	original, _ := json.Marshal(passed)
	peer.setWrongDigest(true)
	negative := verify("real-runtime-verification-wrong-digest")
	if state()["candidate_runtime_verified"] == true {
		t.Fatal("new pending request reused older runtime PASS")
	}
	ticks()
	if operation(negative.ID).Status == "SUCCEEDED" || state()["candidate_runtime_verified"] == true {
		t.Fatal("wrong immutable runtime imageID accepted")
	}
	// The ordinary inventory path can succeed as a read, but cannot certify the
	// requested candidate or supersede a failed server verification generation.
	var informational domain.ExternalOperation
	post(domain.Command{Action: "refresh_environment_runtime", ProductID: "p", Data: map[string]any{"environment_id": "dev"}}, &informational, true)
	inventoryTick()
	if operation(informational.ID).Status != "SUCCEEDED" || state()["candidate_runtime_verified"] == true {
		t.Fatalf("informational inventory status=%s phase=%s query=%v", operation(informational.ID).Status, operation(informational.ID).Phase, state()["candidate_runtime_verified"])
	}
	snapshot := read()
	replay := verify("real-runtime-verification-pass")
	if replay.ID != positive.ID || len(read().Events) != len(snapshot.Events) || len(read().Operations) != len(snapshot.Operations) || state()["candidate_runtime_verified"] == true {
		t.Fatal("historical runtime replay granted new rights or superseded later failure")
	}
	now, _ := json.Marshal(operation(positive.ID))
	if !bytes.Equal(original, now) {
		t.Fatal("historical verification proof changed")
	}
	peer.assertReadOnly(t)
	after := read()
	if !reflect.DeepEqual(before.Environments, after.Environments) || !reflect.DeepEqual(before.ComponentBuilds, after.ComponentBuilds) || !reflect.DeepEqual(before.DeliveryArtifacts, after.DeliveryArtifacts) || len(before.WorkerJobs) != len(after.WorkerJobs) || len(before.Releases) != len(after.Releases) {
		t.Fatal("runtime verification mutated baseline/config/artifacts or queued execution")
	}
	t.Log("Actual OCI artifact verified through production HTTPS Kubernetes observer for api+worker at exact common Flux head; wrong imageID/later pending/failure cannot reuse old PASS, informational inventory/historical replay cannot forge current proof; no runtime or GitOps writes")
}
