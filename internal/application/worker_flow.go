package application

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
)

func flowGuard(st *domain.State, op domain.ExternalOperation) error {
	if op.CompositionSnapshot == nil {
		return invalid("missing composition snapshot")
	}
	if op.Kind != "RELEASE_CANDIDATE" {
		env := environmentByID(st, op.EnvironmentID)
		if env == nil || env.DesiredOperationID != op.ID || env.DesiredCompositionID != op.CompositionSnapshot.ID {
			return invalid("environment operation superseded")
		}
	}
	selected := selectedIntegrations(*op.CompositionSnapshot)
	for _, iid := range selected {
		in := integration(st, iid)
		if in == nil || in.ProductID != op.ProductID {
			return invalid("integration unavailable")
		}
		for _, dep := range in.Dependencies {
			d := integration(st, dep)
			if d == nil || (d.Status != "released" && !slices.Contains(selected, dep)) {
				return invalid("composition dependency changed; replan")
			}
		}
	}
	for _, frozen := range op.PreparedDeployments {
		target := environmentBinding(st, op.EnvironmentID, frozen.Application.ID)
		env := environmentByID(st, op.EnvironmentID)
		source := repositoryBinding(st, frozen.Source.RepositoryID)
		gitops := repositoryBinding(st, frozen.GitOps.RepositoryID)
		build := componentBuild(st, frozen.Application.ID)
		if target == nil || !target.AllowDeploy || !reflect.DeepEqual(*target, frozen.Target) || env == nil || env.Cluster != frozen.Environment.Cluster || env.Namespace != frozen.Environment.Namespace || source == nil || !reflect.DeepEqual(*source, frozen.Source) || gitops == nil || !reflect.DeepEqual(*gitops, frozen.GitOps) || build == nil || !reflect.DeepEqual(*build, frozen.Build) {
			return invalid("frozen deployment configuration changed; create a new operation")
		}
		sourceConnection, err := scopedConnection(st, source.ConnectionID, op.ProductID)
		if err != nil {
			return err
		}
		gitopsConnection, err := scopedConnection(st, gitops.ConnectionID, op.ProductID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(sourceConnection.Config, frozen.SourceConnection) || !reflect.DeepEqual(gitopsConnection.Config, frozen.GitOpsConnection) {
			return invalid("provider connection changed; create a new operation")
		}
	}
	if op.Kind == "RELEASE_CANDIDATE" || op.Kind == "RELEASE_PROMOTION" {
		if err := releaseWorkReady(st, *op.CompositionSnapshot); err != nil {
			return err
		}
	}
	if op.Kind == "RELEASE_PROMOTION" {
		return scenarioCandidateReady(st, op.ReleaseCandidateID)
	}
	return nil
}

func (s *Service) tickFlow(ctx context.Context, op domain.ExternalOperation, token string) error {
	st, err := s.State(ctx)
	if err != nil {
		return err
	}
	if err = flowGuard(&st, op); err != nil {
		return s.finish(ctx, op, token, "BLOCKED", err.Error(), nil)
	}
	switch op.Phase {
	case "COMPOSE_SOURCE":
		provider, ok := s.provider.(delivery.SourceProvider)
		if !ok {
			return s.finish(ctx, op, token, "BLOCKED", "source provider does not support composition", nil)
		}
		for i := range op.Sources {
			source := &op.Sources[i]
			if source.Result != nil {
				continue
			}
			if e := validateFlowSource(ctx, s.provider, op, *source); e != nil {
				return s.finish(ctx, op, token, "BLOCKED", e.Error(), nil)
			}
			result, e := provider.ComposeSource(ctx, source.Connection, source.Plan)
			if e != nil {
				return s.finish(ctx, op, token, "FAILED", "source composition failed: "+e.Error(), nil)
			}
			source.Result = &result
			evidence := map[string]string{"repository_id": source.RepositoryID, "branch": result.Branch, "source_sha": result.SHA}
			if result.Conflict {
				return s.finish(ctx, op, token, "BLOCKED", "source conflict: "+result.Detail, evidence)
			}
			if !fullSHA.MatchString(result.SHA) || result.Branch != source.Plan.TargetBranch {
				return s.finish(ctx, op, token, "BLOCKED", "provider returned invalid composition identity", evidence)
			}
			return s.advance(ctx, op, token, "COMPOSE_SOURCE", "COMPOSING", "source composition recorded", evidence)
		}
		phase := "QUEUE_BUILDS"
		if op.Kind == "RELEASE_CANDIDATE" {
			phase = "ADVANCE_MAIN"
		}
		return s.advance(ctx, op, token, phase, "SOURCE_READY", "all component source compositions ready", nil)
	case "ADVANCE_MAIN":
		provider, ok := s.provider.(delivery.SourceProvider)
		if !ok {
			return s.finish(ctx, op, token, "BLOCKED", "source provider cannot advance main", nil)
		}
		for i := range op.Sources {
			source := &op.Sources[i]
			if source.MainAdvanced {
				continue
			}
			if source.Result == nil {
				return s.finish(ctx, op, token, "FAILED", "missing selected source result", nil)
			}
			result, e := provider.AdvanceMain(ctx, source.Connection, source.Plan.Repository, source.Plan.BaseRef, source.Plan.BaseSHA, source.Result.SHA)
			if e != nil {
				return s.finish(ctx, op, token, "FAILED", "main advance failed; inspect partial repository progress: "+e.Error(), nil)
			}
			if result.Conflict || result.SHA != source.Result.SHA || result.Branch != source.Plan.BaseRef {
				return s.finish(ctx, op, token, "BLOCKED", "main advance did not preserve selected commit", nil)
			}
			source.MainAdvanced = true
			return s.advance(ctx, op, token, "ADVANCE_MAIN", "UPDATING_MAIN", "selected source advanced main", map[string]string{"repository_id": source.RepositoryID, "source_sha": result.SHA})
		}
		return s.advance(ctx, op, token, "QUEUE_BUILDS", "SOURCE_READY", "selected main commits pinned", nil)
	case "QUEUE_BUILDS", "QUEUE_DEPLOYMENTS":
		return s.queueFlowChildren(ctx, op, token)
	case "WAIT_CHILDREN":
		if len(op.ChildIDs) == 0 {
			return s.finish(ctx, op, token, "FAILED", "no component operations", nil)
		}
		for _, childID := range op.ChildIDs {
			child := operationByID(&st, childID)
			if child == nil {
				return s.finish(ctx, op, token, "FAILED", "component operation missing", nil)
			}
			if child.Status == "FAILED" || child.Status == "CANCELLED" {
				return s.finish(ctx, op, token, "FAILED", "component operation "+childID+": "+child.Detail, nil)
			}
			if child.Status != "SUCCEEDED" {
				return s.retry(ctx, op, token, "waiting for component operations")
			}
		}
		if op.Kind == "RELEASE_CANDIDATE" {
			return s.finish(ctx, op, token, "READY_FOR_VERIFICATION", "main-derived artifacts await isolated candidate verification", nil)
		}
		return s.advance(ctx, op, token, "WAIT_RUNTIME", "PENDING_RECONCILIATION", "GitOps applied; waiting for exact runtime evidence", nil)
	case "WAIT_RUNTIME":
		for _, childID := range op.ChildIDs {
			child := operationByID(&st, childID)
			if child == nil || child.GitOpsResult == nil || child.Artifact == nil {
				return s.finish(ctx, op, token, "FAILED", "deployment output missing", nil)
			}
			var latest *domain.RuntimeObservation
			for i := range st.RuntimeObservations {
				r := &st.RuntimeObservations[i]
				if r.OperationID == childID && (latest == nil || !r.CreatedAt.Before(latest.CreatedAt)) {
					latest = r
				}
			}
			if latest != nil && !latest.Healthy && latest.EnvironmentID == op.EnvironmentID && latest.GitOpsCommit == child.GitOpsResult.CommitSHA && latest.ArtifactDigest == child.Artifact.Digest {
				return s.finish(ctx, op, token, "FAILED", "runtime unhealthy: "+latest.Details, map[string]string{"operation_id": childID})
			}
			if latest == nil || latest.EnvironmentID != op.EnvironmentID || latest.GitOpsCommit != child.GitOpsResult.CommitSHA || latest.ArtifactDigest != child.Artifact.Digest {
				return s.retry(ctx, op, token, "waiting for healthy runtime evidence matching each GitOps commit and image digest")
			}
		}
		if op.Kind == "RELEASE_PROMOTION" {
			return s.finishFlowRelease(ctx, op, token)
		}
		return s.finish(ctx, op, token, "DEPLOYED", "exact composition runtime confirmed by recorded executor evidence", nil)
	}
	return s.finish(ctx, op, token, "FAILED", "unknown flow phase", nil)
}

func cloneDeliverySnapshot(v domain.DeliverySnapshot) domain.DeliverySnapshot {
	b, _ := json.Marshal(v)
	var out domain.DeliverySnapshot
	_ = json.Unmarshal(b, &out)
	return out
}
func (s *Service) queueFlowChildren(ctx context.Context, op domain.ExternalOperation, token string) error {
	return s.store.Update(ctx, func(st *domain.State) error {
		parent := operationByID(st, op.ID)
		if parent == nil || parent.LeaseOwner != token {
			return invalid("operation lease lost")
		}
		if err := flowGuard(st, *parent); err != nil {
			return err
		}
		if len(parent.ChildIDs) > 0 {
			return invalid("component operations already queued")
		}
		now := time.Now().UTC()
		children := []domain.ExternalOperation{}
		if op.Kind == "RELEASE_PROMOTION" {
			candidate := operationByID(st, op.ReleaseCandidateID)
			if candidate == nil {
				return invalid("candidate missing")
			}
			for _, childID := range candidate.ChildIDs {
				built := operationByID(st, childID)
				if built == nil || built.Status != "SUCCEEDED" || built.Snapshot == nil || built.Artifact == nil {
					return invalid("candidate component evidence missing")
				}
				snap := cloneDeliverySnapshot(*built.Snapshot)
				cm := domain.Meta{ID: id(), ProductID: op.ProductID, Actor: op.RequestedBy, CreatedAt: now, UpdatedAt: now}
				artifact := *built.Artifact
				var run *delivery.RunResult
				if built.Run != nil {
					copy := *built.Run
					run = &copy
				}
				children = append(children, domain.ExternalOperation{Meta: cm, ParentID: op.ID, ReleaseCandidateID: candidate.ID, Kind: "DEPLOY", Status: "PENDING", Phase: "PROMOTION_PREFLIGHT", CurrentStep: "PROMOTION_PREFLIGHT", EnvironmentID: op.EnvironmentID, ApplicationID: snap.Application.ID, Snapshot: &snap, Artifact: &artifact, ExpectedDigest: artifact.Digest, Run: run, RequestedBy: op.RequestedBy, NextAttemptAt: now})
			}
		} else {
			if len(op.PreparedDeployments) == 0 {
				return invalid("frozen component configuration missing")
			}
			for _, prepared := range op.PreparedDeployments {
				snap := cloneDeliverySnapshot(prepared)
				var source *domain.FlowSource
				for i := range op.Sources {
					if op.Sources[i].RepositoryID == snap.Application.RepositoryID {
						source = &op.Sources[i]
					}
				}
				if source == nil || source.Result == nil || !fullSHA.MatchString(source.Result.SHA) {
					return invalid("component composed source missing")
				}
				cm := domain.Meta{ID: id(), ProductID: op.ProductID, Actor: op.RequestedBy, CreatedAt: now, UpdatedAt: now}
				branch := source.Result.Branch
				if op.Kind == "RELEASE_CANDIDATE" {
					if !source.MainAdvanced {
						return invalid("main not advanced")
					}
					branch = source.Plan.BaseRef
				}
				snap.Revision = domain.IntegrationRevision{Meta: domain.Meta{ID: id(), ProductID: op.ProductID, Actor: op.RequestedBy, CreatedAt: now, UpdatedAt: now}, RepositoryID: source.RepositoryID, Branch: branch, BaseCommit: source.Plan.BaseSHA, HeadCommit: source.Result.SHA, Commits: []string{source.Result.SHA}}
				snap.BuildRequest.SourceSHA = source.Result.SHA
				snap.BuildRequest.ImageTag = "rcp-" + source.Result.SHA
				snap.BuildRequest.OperationID = cm.ID
				snap.BuildRequest.RequestedAt = now
				if snap.BuildRequest.Inputs == nil {
					snap.BuildRequest.Inputs = map[string]string{}
				}
				snap.BuildRequest.Inputs["source_sha"] = source.Result.SHA
				snap.BuildRequest.Inputs["image_tag"] = snap.BuildRequest.ImageTag
				snap.BuildRequest.Inputs["operation_id"] = cm.ID
				snap.BuildRequest.Inputs["image_repository"] = snap.Build.ImageRepository
				children = append(children, domain.ExternalOperation{Meta: cm, ParentID: op.ID, Kind: "DEPLOY", Status: "PENDING", Phase: "PREFLIGHT", CurrentStep: "PREFLIGHT", EnvironmentID: op.EnvironmentID, ApplicationID: snap.Application.ID, Snapshot: &snap, BuildOnly: op.Kind == "RELEASE_CANDIDATE", RequestedBy: op.RequestedBy, NextAttemptAt: now})
			}
		}
		if len(children) == 0 {
			return invalid("no child operations prepared")
		}
		parent.ChildIDs = []string{}
		for _, child := range children {
			parent.ChildIDs = append(parent.ChildIDs, child.ID)
		}
		parent.Phase = "WAIT_CHILDREN"
		parent.CurrentStep = parent.Phase
		parent.DeploymentState = "WAITING_FOR_COMPONENTS"
		parent.Status = "RUNNING"
		parent.UpdatedAt = now
		parent.NextAttemptAt = now.Add(2 * time.Second)
		parent.LeaseOwner = ""
		parent.LeaseUntil = time.Time{}
		st.OperationSteps = append(st.OperationSteps, domain.OperationStep{Meta: domain.Meta{ID: id(), ProductID: op.ProductID, Actor: "worker", CreatedAt: now, UpdatedAt: now}, OperationID: op.ID, Phase: "QUEUE_COMPONENTS", Status: "SUCCEEDED", Detail: fmt.Sprintf("queued %d component operations", len(children))})
		st.Operations = append(st.Operations, children...)
		return nil
	})
}

func (s *Service) finishFlowRelease(ctx context.Context, op domain.ExternalOperation, token string) error {
	return s.store.Update(ctx, func(st *domain.State) error {
		parent := operationByID(st, op.ID)
		if parent == nil || parent.LeaseOwner != token {
			return invalid("operation lease lost")
		}
		if err := flowGuard(st, *parent); err != nil {
			return err
		}
		now := time.Now().UTC()
		record := domain.Release{Meta: domain.Meta{ID: id(), ProductID: op.ProductID, Actor: op.RequestedBy, CreatedAt: now, UpdatedAt: now}, Name: op.CompositionSnapshot.Name, Status: "released", ExecutionOperationID: op.ID, CandidateOperationID: op.ReleaseCandidateID, IntegrationIDs: append([]string{}, op.IntegrationIDs...), SourceCommits: map[string]string{}, ArtifactDigests: map[string]string{}, GitOpsCommits: map[string]string{}, Snapshots: []domain.Integration{}}
		for _, childID := range parent.ChildIDs {
			child := operationByID(st, childID)
			if child == nil || child.Snapshot == nil || child.Artifact == nil || child.GitOpsResult == nil {
				return invalid("release component outputs missing")
			}
			var latest *domain.RuntimeObservation
			for i := range st.RuntimeObservations {
				r := &st.RuntimeObservations[i]
				if r.OperationID == childID && (latest == nil || !r.CreatedAt.Before(latest.CreatedAt)) {
					latest = r
				}
			}
			if latest == nil || !latest.Healthy || latest.GitOpsCommit != child.GitOpsResult.CommitSHA || latest.ArtifactDigest != child.Artifact.Digest {
				return invalid("runtime evidence changed")
			}
			app := child.ApplicationID
			record.SourceCommits[app] = child.Snapshot.Revision.HeadCommit
			record.ArtifactDigests[app] = child.Artifact.Digest
			record.GitOpsCommits[app] = child.GitOpsResult.CommitSHA
		}
		for _, iid := range op.IntegrationIDs {
			in := integration(st, iid)
			if in == nil {
				return invalid("released integration missing")
			}
			in.Status = "released"
			in.UpdatedAt = now
			b, _ := json.Marshal(in)
			var snapshot domain.Integration
			_ = json.Unmarshal(b, &snapshot)
			record.Snapshots = append(record.Snapshots, snapshot)
		}
		exists := false
		for _, r := range st.Releases {
			if r.ExecutionOperationID == op.ID {
				exists = true
			}
		}
		if !exists {
			st.Releases = append(st.Releases, record)
		}
		parent.Status = "SUCCEEDED"
		parent.DeploymentState = "DEPLOYED"
		parent.Detail = "verified main-derived release runtime confirmed"
		parent.UpdatedAt = now
		parent.FinishedAt = &now
		parent.LeaseOwner = ""
		parent.LeaseUntil = time.Time{}
		st.OperationSteps = append(st.OperationSteps, domain.OperationStep{Meta: domain.Meta{ID: id(), ProductID: op.ProductID, Actor: "worker", CreatedAt: now, UpdatedAt: now}, OperationID: op.ID, Phase: "RELEASE_RECORDED", Status: "SUCCEEDED", Detail: parent.Detail, Evidence: map[string]string{"release_id": record.ID}})
		return nil
	})
}
