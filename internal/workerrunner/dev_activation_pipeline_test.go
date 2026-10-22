package workerrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
)

// Git/source/build/registry use production code. Only the external PR server
// and its separately initiated merge are simulated in this fixture.
type mainCandidateActivationProvider struct {
	*composedPipelineProvider
	lostProposal bool
	lookups      int
	merged       bool
}

func (p *mainCandidateActivationProvider) ProposeGitOpsGuarded(ctx context.Context, c delivery.Connection, a delivery.GitOpsApply, guard func(context.Context) error) (delivery.GitOpsPullRequest, error) {
	if guard == nil {
		return delivery.GitOpsPullRequest{}, fmt.Errorf("fixture missing mutation guard")
	}
	// This fixture models one physical PR mutation. The production adapter's
	// per-HTTP ref/commit/PR boundaries are verified by its own HTTP tests.
	if err := guard(ctx); err != nil {
		return delivery.GitOpsPullRequest{}, err
	}
	return p.ProposeGitOps(ctx, c, a)
}

func (p *mainCandidateActivationProvider) ProposeGitOps(ctx context.Context, c delivery.Connection, a delivery.GitOpsApply) (delivery.GitOpsPullRequest, error) {
	r, e := p.composedPipelineProvider.ProposeGitOps(ctx, c, a)
	if e == nil && p.lostProposal {
		p.lostProposal = false
		return delivery.GitOpsPullRequest{}, fmt.Errorf("fixture lost response after creating exact PR")
	}
	return r, e
}
func (p *mainCandidateActivationProvider) LookupGitOpsProposal(_ context.Context, _ delivery.Connection, a delivery.GitOpsApply) (delivery.GitOpsPullRequest, error) {
	p.lookups++
	for n, old := range p.proposals {
		if reflect.DeepEqual(old, a) {
			return delivery.GitOpsPullRequest{Number: n + 1, URL: fmt.Sprintf("https://github.com/fixture/gitops/pull/%d", n+1), HeadSHA: strings.Repeat("d", 40), State: "open"}, nil
		}
	}
	return delivery.GitOpsPullRequest{}, fmt.Errorf("fixture no exact proposal; no write authorized")
}
func (p *mainCandidateActivationProvider) ObserveGitOpsPR(ctx context.Context, c delivery.Connection, a delivery.GitOpsApply, r delivery.GitOpsPullRequest) (delivery.GitOpsPullRequest, error) {
	r, e := p.composedPipelineProvider.ObserveGitOpsPR(ctx, c, a, r)
	if e == nil && p.merged {
		r.Merged = true
		r.State = "closed"
	}
	return r, e
}
func (p *mainCandidateActivationProvider) ReadGitOps(ctx context.Context, c delivery.Connection, r delivery.GitOpsRequest) (delivery.GitOpsSnapshot, error) {
	if p.merged {
		for _, a := range p.proposals {
			if a.Request.Repository == r.Repository && a.Request.Ref == r.Ref && a.Request.Path == r.Path {
				return delivery.GitOpsSnapshot{HeadSHA: strings.Repeat("f", 40), BlobSHA: strings.Repeat("b", 40), ImageRepository: a.Request.ImageRepository, Digest: a.Digest}, nil
			}
		}
	}
	return p.composedPipelineProvider.ReadGitOps(ctx, c, r)
}
func assertRealCandidateDevActivation(t *testing.T, endpoint, token, candidate string, provider *mainCandidateActivationProvider, ticks func(), read func() domain.State, post func(domain.Command, any, bool)) []byte {
	t.Helper()
	environment := func(s domain.State) domain.Environment {
		for _, e := range s.Environments {
			if e.ID == "dev" {
				return e
			}
		}
		t.Fatal("missing Dev")
		return domain.Environment{}
	}
	selection := func(e domain.Environment) domain.EnvironmentSelection {
		return domain.EnvironmentSelection{CompositionID: e.DesiredCompositionID, OperationID: e.DesiredOperationID, Generation: e.SelectionGeneration}
	}
	operation := func(s domain.State, id string) domain.ExternalOperation {
		for _, o := range s.Operations {
			if o.ID == id {
				return o
			}
		}
		t.Fatal("operation absent", id)
		return domain.ExternalOperation{}
	}
	request := func(path string, data map[string]any, out any, want bool) {
		t.Helper()
		body := map[string]any{"actor": "fixture/operator"}
		for k, v := range data {
			body[k] = v
		}
		b, _ := json.Marshal(body)
		r, _ := http.NewRequest(http.MethodPost, endpoint+"/api/products/p/candidate-dev/"+path, bytes.NewReader(b))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		response, e := http.DefaultClient.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		b, _ = io.ReadAll(response.Body)
		if want && response.StatusCode != 200 || !want && (response.StatusCode < 400 || response.StatusCode >= 500) {
			t.Fatalf("candidate Dev %s HTTP%d: %s", path, response.StatusCode, b)
		}
		if want && out != nil {
			if e = json.Unmarshal(b, out); e != nil {
				t.Fatal(e)
			}
		}
	}
	before := read()
	baseline := environment(before)
	oldCandidate := operation(before, candidate)
	original, _ := json.Marshal(oldCandidate)
	var reservation domain.ExternalOperation
	request("reserve", map[string]any{"candidate_operation_id": candidate, "environment_id": "dev", "request_key": "real-candidate-activation-reserve", "expected_selection": selection(baseline)}, &reservation, true)
	held := environment(read())
	input := map[string]any{"reservation_operation_id": reservation.ID, "request_key": "real-candidate-activation-deploy", "expected_selection": selection(held)}
	var activation domain.ExternalOperation
	provider.lostProposal = true
	request("deploy", input, &activation, true)
	if activation.Kind != "DEV_CANDIDATE_DEPLOY" || activation.EnvironmentID != "dev" || activation.ID == candidate {
		t.Fatal("not a separate Dev activation")
	}
	admitted := read()
	var replay domain.ExternalOperation
	request("deploy", input, &replay, true)
	if replay.ID != activation.ID || len(read().Operations) != len(admitted.Operations) || len(read().Events) != len(admitted.Events) {
		t.Fatal("activation replay minted work")
	}
	ticks()
	pending := operation(read(), activation.ID)
	if len(pending.ChildIDs) != 1 {
		t.Fatal("missing exact candidate child", pending)
	}
	child := operation(read(), pending.ChildIDs[0])
	built := operation(before, oldCandidate.ChildIDs[0])
	if child.Phase != "WAIT_GITOPS_PR" || child.ExistingArtifact == nil || child.Artifact == nil || child.Native != nil || child.BuildOnly || child.EnvironmentID != "dev" || child.Artifact.Digest != built.Artifact.Digest || child.Snapshot.Build.Profile != "prod" || child.Snapshot.Revision.HeadCommit != built.Snapshot.Revision.HeadCommit || child.GitOpsPR == nil {
		t.Fatalf("exact existing main artifact did not reach Dev PR: %+v", child)
	}
	if len(provider.proposals) != 1 || provider.lookups < 1 || provider.proposals[0].Digest != built.Artifact.Digest || provider.proposals[0].Request.Path != child.Snapshot.Target.Path || provider.forbidden != 0 {
		t.Fatal("lost response caused repeat write or unbound PR")
	}
	request("release", map[string]any{"reservation_operation_id": reservation.ID, "request_key": "unsafe-release-after-activation", "expected_selection": selection(held)}, nil, false)
	provider.merged = true // External merge fixture; RCP never merges the PR.
	ticks()
	after := read()
	done := operation(after, activation.ID)
	if done.Status != "SUCCEEDED" || done.DeploymentState != "GITOPS_APPLIED" {
		t.Fatal("exact merged GitOps result not aggregated", done)
	}
	request("release", map[string]any{"reservation_operation_id": reservation.ID, "request_key": "unsafe-release-after-merge", "expected_selection": selection(held)}, nil, false)
	post(domain.Command{Action: "promote_release_candidate", ID: candidate, ProductID: "p", Data: map[string]any{"approve": true}}, nil, false)
	if !reflect.DeepEqual(environment(after), held) || len(after.WorkerJobs) != len(before.WorkerJobs) || len(after.DeliveryArtifacts) != len(before.DeliveryArtifacts) || len(after.RuntimeObservations) != len(before.RuntimeObservations) || len(after.Releases) != len(before.Releases) || !reflect.DeepEqual(after.ComponentBuilds, before.ComponentBuilds) {
		t.Fatal("activation changed baseline/maps/config/runtime or rebuilt/published")
	}
	currentCandidate, _ := json.Marshal(operation(after, candidate))
	if !bytes.Equal(original, currentCandidate) {
		t.Fatal("activation rewrote original candidate/provenance")
	}
	request("deploy", input, &replay, true)
	if replay.ID != activation.ID || len(provider.proposals) != 1 {
		t.Fatal("historical merged replay repeated effect")
	}
	b, _ := json.Marshal(read().Environments)
	t.Log("Actual main Git→OCI→Distribution artifact delivered through authenticated named Dev HTTP; lost proposal recovered read-only; external merge observed; baseline unchanged, no extra build/publication/source write, no runtime acceptance/promotion/unlock")
	return b
}
