package workerrunner

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"releasecontrol/internal/domain"
	"testing"
)

// This is called only after the real Git -> native worker -> Distribution chain
// admitted the main candidate. It never fabricates candidate or artifact proof.
func assertRealCandidateDevReservation(t *testing.T, url, token, candidate, mainB string, read func() domain.State, post func(domain.Command, any, bool)) []byte {
	t.Helper()
	var baseline domain.Composition
	post(domain.Command{Action: "plan_composition", ProductID: "p", Data: map[string]any{"environment_id": "dev", "name": "actual main A+B baseline", "components": []domain.CompositionComponent{{ApplicationID: "component", BaseRef: "main", BaseCommit: mainB, TargetBranch: "generated/reservation-baseline", RevisionIDs: []string{}}}}}, &baseline, true)
	post(domain.Command{Action: "select_composition", ID: baseline.ID}, nil, true)
	env := func(st domain.State) domain.Environment {
		for _, e := range st.Environments {
			if e.ID == "dev" {
				return e
			}
		}
		t.Fatal("Dev missing")
		return domain.Environment{}
	}
	selection := func(e domain.Environment) domain.EnvironmentSelection {
		return domain.EnvironmentSelection{CompositionID: e.DesiredCompositionID, OperationID: e.DesiredOperationID, Generation: e.SelectionGeneration}
	}
	before := read()
	previous := env(before)
	maps := func(e domain.Environment) []byte {
		b, _ := json.Marshal([]any{e.Desired, e.Reconciled, e.Runtime, e.Composition, e.Parameters, e.Cluster, e.Namespace})
		return b
	}
	request := func(path string, data map[string]any, out any, ok bool) {
		t.Helper()
		data["actor"] = "fixture/operator"
		b, _ := json.Marshal(data)
		req, _ := http.NewRequest("POST", url+"/api/products/p/candidate-dev/"+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ = io.ReadAll(r.Body)
		if ok && r.StatusCode != 200 || !ok && (r.StatusCode < 400 || r.StatusCode >= 500) {
			t.Fatalf("reservation %s HTTP%d %s", path, r.StatusCode, b)
		}
		if ok && out != nil {
			if err = json.Unmarshal(b, out); err != nil {
				t.Fatal(err)
			}
		}
	}
	queryHold := func() (bool, string, uint64) {
		t.Helper()
		req, _ := http.NewRequest("GET", url+"/api/environments/dev/state", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var state struct {
			Reservation struct {
				Active                 bool   `json:"active"`
				OperationID            string `json:"operation_id"`
				Epoch                  uint64 `json:"epoch"`
				CandidateDeployed      bool   `json:"candidate_deployed"`
				ReconciliationRequired bool   `json:"reconciliation_required"`
			} `json:"dev_reservation"`
		}
		if r.StatusCode != 200 || json.NewDecoder(r.Body).Decode(&state) != nil || state.Reservation.CandidateDeployed || state.Reservation.ReconciliationRequired {
			t.Fatal("reservation query confused preparation with deployment/recovery")
		}
		return state.Reservation.Active, state.Reservation.OperationID, state.Reservation.Epoch
	}
	reserve := map[string]any{"candidate_operation_id": candidate, "environment_id": "dev", "request_key": "real-main-dev-reserve-1", "expected_selection": selection(previous)}
	var receipt domain.ExternalOperation
	request("reserve", reserve, &receipt, true)
	heldState := read()
	held := env(heldState)
	if receipt.Kind != "DEV_RESERVATION" || receipt.Status != "SUCCEEDED" || receipt.DevReservation == nil || held.ReservationEpoch != previous.ReservationEpoch+1 || held.SelectionGeneration != previous.SelectionGeneration+1 || held.DesiredCompositionID != previous.DesiredCompositionID || held.DesiredOperationID != previous.DesiredOperationID || !bytes.Equal(maps(held), maps(previous)) {
		t.Fatal("reservation did not preserve baseline intent/maps with a new execution epoch")
	}
	if active, id, epoch := queryHold(); !active || id != receipt.ID || epoch != held.ReservationEpoch {
		t.Fatal("active reservation missing from environment query")
	}
	post(domain.Command{Action: "select_composition", ID: baseline.ID}, nil, false)
	post(domain.Command{Action: "update_environment", ID: "dev", Data: map[string]any{"name": "displaced"}}, nil, false)
	post(domain.Command{Action: "deploy_integration", IntegrationID: "ia", Data: map[string]any{"environment_id": "dev", "application_id": "component", "revision_id": "a1"}}, nil, false)
	post(domain.Command{Action: "promote_release_candidate", ID: candidate, ProductID: "p", Data: map[string]any{"approve": true}}, nil, false)
	request("release", map[string]any{"reservation_operation_id": receipt.ID, "request_key": "real-main-dev-bad-release", "expected_selection": selection(previous)}, nil, false)
	release := map[string]any{"reservation_operation_id": receipt.ID, "request_key": "real-main-dev-release-1", "expected_selection": selection(held)}
	var released domain.ExternalOperation
	request("release", release, &released, true)
	after := read()
	current := env(after)
	if released.Kind != "DEV_RESERVATION_RELEASE" || current.SelectionGeneration != held.SelectionGeneration+1 || current.ReservationEpoch != held.ReservationEpoch || current.DesiredCompositionID != previous.DesiredCompositionID || current.DesiredOperationID != previous.DesiredOperationID || !bytes.Equal(maps(current), maps(previous)) || len(after.DeliveryArtifacts) != len(before.DeliveryArtifacts) || len(after.RuntimeObservations) != len(before.RuntimeObservations) || len(after.Releases) != len(before.Releases) {
		t.Fatal("release altered baseline/runtime/artifacts or lost monotonic epoch")
	}
	var replay domain.ExternalOperation
	request("reserve", reserve, &replay, true)
	if replay.ID != receipt.ID {
		t.Fatal("historical reservation replay minted a new receipt")
	}
	request("release", release, &replay, true)
	if replay.ID != released.ID {
		t.Fatal("historical release replay minted a new receipt")
	}
	afterReplay := read()
	if selection(env(afterReplay)) != selection(current) || len(afterReplay.Events) != len(after.Events) || len(afterReplay.Operations) != len(after.Operations) {
		t.Fatal("historical replay changed desired selection, authority or audit")
	}
	if active, _, _ := queryHold(); active {
		t.Fatal("historical replay renewed the released reservation")
	}
	post(domain.Command{Action: "select_composition", ID: baseline.ID}, nil, true)
	post(domain.Command{Action: "promote_release_candidate", ID: candidate, ProductID: "p", Data: map[string]any{"approve": true}}, nil, false)
	result := read()
	if !bytes.Equal(maps(env(result)), maps(previous)) {
		t.Fatal("fresh post-release selection changed observed state")
	}
	b, _ := json.Marshal(result.Environments)
	t.Log("Actual built main candidate reserved and released A+B baseline through authenticated named HTTP; CAS/replay/epoch held; no candidate deployment or Dev acceptance")
	return b
}
