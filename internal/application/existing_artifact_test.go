package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
)

type existingArtifactProvider struct {
	*fakeDelivery
	request delivery.ArtifactRequest
	missing bool
	stale   bool
}

func (f *existingArtifactProvider) InspectArtifact(ctx context.Context, c delivery.Connection, r delivery.ArtifactRequest) (delivery.ArtifactResult, error) {
	f.request = r
	if f.stale {
		return delivery.ArtifactResult{}, errors.New("workflow registry evidence is stale or mismatched")
	}
	if f.missing {
		return delivery.ArtifactResult{ObservedAt: time.Now().UTC()}, nil
	}
	return f.fakeDelivery.InspectArtifact(ctx, c, r)
}
func existingArtifactFixture(t *testing.T) (*Service, *memoryStore, *existingArtifactProvider, domain.Command) {
	t.Helper()
	s, m, p := externalFixture(t)
	exec(t, s, deployCommand())
	runDelivery(t, s, m)
	if p.dispatches != 1 || p.applies != 1 {
		t.Fatal("initial real orchestration did not build and apply")
	}
	var artifact domain.DeliveryArtifact
	for _, a := range m.state.DeliveryArtifacts {
		if a.Availability == "PRESENT" {
			artifact = a
		}
	}
	if artifact.ID == "" {
		t.Fatal("no artifact recorded")
	}
	p.currentDigest = ""
	f := &existingArtifactProvider{fakeDelivery: p}
	s = NewWithProvider(m, f)
	cmd := domain.Command{Action: "deploy_existing_artifact", ID: "existing", IntegrationID: "i", Data: map[string]any{"environment_id": "dev", "application_id": "component", "revision_id": "rev", "artifact_id": artifact.ID}}
	return s, m, f, cmd
}
func finishExisting(t *testing.T, s *Service, m *memoryStore) domain.ExternalOperation {
	t.Helper()
	for range 8 {
		op := operationByID(&m.state, "existing")
		if terminalOperation(op.Status) {
			return *op
		}
		tickNow(t, s, m)
	}
	t.Fatal("operation did not terminate")
	return domain.ExternalOperation{}
}
func TestDeployExistingArtifactRechecksWithoutCI(t *testing.T) {
	s, m, f, cmd := existingArtifactFixture(t)
	exec(t, s, cmd)
	// Environment/component serialization applies across normal and existing-image operations.
	duplicate := cmd
	duplicate.ID = "second"
	reject(t, s, duplicate)
	op := finishExisting(t, s, m)
	if op.Status != "SUCCEEDED" || op.DeploymentState != "GITOPS_APPLIED" || f.dispatches != 1 || f.applies != 2 {
		t.Fatalf("bad completion: %+v dispatches=%d applies=%d", op, f.dispatches, f.applies)
	}
	if f.request.EvidenceOperationID != "deploy" || f.request.EvidenceRunID != 12 || f.request.SourceSHA != shaHead {
		t.Fatalf("lost original build provenance: %+v", f.request)
	}
	last := m.state.DeliveryArtifacts[len(m.state.DeliveryArtifacts)-1]
	if last.BuildRequest.OperationID != "deploy" || last.BuildRunID != 12 {
		t.Fatalf("invented build provenance: %+v", last)
	}
}
func TestDeployExistingArtifactRejectsScopeAndUnprovenance(t *testing.T) {
	for _, field := range []string{"product", "component", "repository", "source", "digest", "run", "availability"} {
		t.Run(field, func(t *testing.T) {
			s, m, _, cmd := existingArtifactFixture(t)
			for n := range m.state.DeliveryArtifacts {
				a := &m.state.DeliveryArtifacts[n]
				if a.ID != cmd.Data["artifact_id"] {
					continue
				}
				switch field {
				case "product":
					a.ProductID = "other"
				case "component":
					a.ApplicationID = "other"
				case "repository":
					a.RepositoryID = "other"
				case "source":
					a.SourceCommit = shaBase
				case "digest":
					a.Digest = "sha256:" + strings.Repeat("e", 64)
				case "run":
					a.BuildRunID = 99
				case "availability":
					a.Availability = "MISSING"
				}
			}
			reject(t, s, cmd)
		})
	}
}
func TestDeployExistingArtifactCannotBuildWhenMissingOrStale(t *testing.T) {
	for _, mode := range []string{"missing", "stale"} {
		t.Run(mode, func(t *testing.T) {
			s, m, f, cmd := existingArtifactFixture(t)
			f.missing = mode == "missing"
			f.stale = mode == "stale"
			exec(t, s, cmd)
			op := finishExisting(t, s, m)
			if op.Status != "FAILED" || f.dispatches != 1 || f.applies != 1 {
				t.Fatalf("unsafe outcome: %+v", op)
			}
		})
	}
}
func TestDeployExistingArtifactRespectsGitOpsCAS(t *testing.T) {
	s, m, f, cmd := existingArtifactFixture(t)
	exec(t, s, cmd)
	tickNow(t, s, m) // pin branch and GitOps
	tickNow(t, s, m) // verify registry
	f.currentDigest = "sha256:" + strings.Repeat("e", 64)
	op := finishExisting(t, s, m)
	if op.Status != "FAILED" || f.applies != 1 || f.dispatches != 1 {
		t.Fatalf("overwrote concurrent change: %+v", op)
	}
}
