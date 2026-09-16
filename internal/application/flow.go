package application

import (
	"fmt"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"slices"
	"strings"
	"time"
)

func compositionByID(st *domain.State, id string) *domain.Composition {
	for i := range st.Compositions {
		if st.Compositions[i].ID == id {
			return &st.Compositions[i]
		}
	}
	return nil
}
func selectedIntegrations(comp domain.Composition) []string {
	out := []string{}
	for _, r := range comp.RevisionSnapshots {
		if !slices.Contains(out, r.IntegrationID) {
			out = append(out, r.IntegrationID)
		}
	}
	return out
}
func releaseWorkReady(st *domain.State, comp domain.Composition) error {
	selected := selectedIntegrations(comp)
	if len(selected) == 0 {
		return invalid("release requires selected integrations")
	}
	for _, iid := range selected {
		in := integration(st, iid)
		if in == nil || in.ProductID != comp.ProductID || (in.Status != "ready" && in.Status != "released") {
			return invalid("selected integration must be ready: %s", iid)
		}
		if err := readiness(st, *in); err != nil {
			return err
		}
		for _, dep := range in.Dependencies {
			d := integration(st, dep)
			if d == nil || (d.Status != "released" && !slices.Contains(selected, dep)) {
				return invalid("unreleased dependency %s", dep)
			}
		}
	}
	// A release cannot silently leave selected integration repositories out.
	for _, iid := range selected {
		in := integration(st, iid)
		for _, repo := range in.Repositories {
			found := false
			for _, r := range comp.RevisionSnapshots {
				if r.IntegrationID == iid && r.RepositoryID == repo {
					found = true
				}
			}
			if !found {
				return invalid("release missing integration repository %s", repo)
			}
		}
	}
	return nil
}
func flowSources(st *domain.State, comp domain.Composition, opID string, production bool) ([]domain.FlowSource, error) {
	out := []domain.FlowSource{}
	seen := map[string]bool{}
	for _, component := range comp.Components {
		app := applicationByID(st, component.ApplicationID)
		if app == nil {
			return nil, invalid("component missing")
		}
		source := repositoryBinding(st, app.RepositoryID)
		build := componentBuild(st, app.ID)
		target := environmentBinding(st, comp.EnvironmentID, app.ID)
		if source == nil || source.Role != "SOURCE" || build == nil || target == nil || !target.AllowDeploy {
			return nil, invalid("source/build and enabled target required for %s", app.ID)
		}
		if production && target.Purpose != "PROD" {
			return nil, invalid("release requires explicit PROD mapping")
		}
		if !production && !slices.Contains([]string{"DEV", "TEST"}, target.Purpose) {
			return nil, invalid("compositions require DEV or TEST mapping")
		}
		if component.BaseRef != configuredBase(*source) {
			return nil, invalid("base_ref must match configured main/base branch")
		}
		if strings.Contains(repositoryLocator(st, *source), "/") {
			return nil, invalid("discover numeric repository identity before execution")
		}
		if seen[app.RepositoryID] {
			continue
		}
		seen[app.RepositoryID] = true
		conn, err := scopedConnection(st, source.ConnectionID, comp.ProductID)
		if err != nil {
			return nil, err
		}
		heads := []string{}
		for _, rid := range component.RevisionIDs {
			r := revisionByID(st, rid)
			if r == nil {
				return nil, invalid("revision missing")
			}
			heads = append(heads, r.HeadCommit)
		}
		branch := fmt.Sprintf("generated/%s/%s/%s", comp.EnvironmentID, opID, app.RepositoryID)
		out = append(out, domain.FlowSource{RepositoryID: app.RepositoryID, Connection: conn.Config, Plan: delivery.SourcePlan{Repository: repositoryLocator(st, *source), BaseRef: component.BaseRef, BaseSHA: component.BaseCommit, TargetBranch: branch, OperationID: opID, HeadSHAs: heads}})
	}
	return out, nil
}
func applyFlow(st *domain.State, c domain.Command, m domain.Meta) (any, domain.Meta, error) {
	switch c.Action {
	case "create_hotfix":
		var in struct {
			Title        string   `json:"title"`
			Objective    string   `json:"objective"`
			FindingID    string   `json:"finding_id"`
			Repositories []string `json:"repositories"`
			Remaining    []string `json:"remaining"`
		}
		if err := decode(c.Data, &in); err != nil {
			return nil, m, err
		}
		if in.FindingID != "" {
			found := false
			for _, f := range st.Findings {
				if f.ID == in.FindingID && f.FeatureID == c.FeatureID {
					found = true
				}
			}
			if !found {
				return nil, m, invalid("finding must belong to same feature")
			}
		}
		_, err := apply(st, domain.Command{Action: "create_integration", Actor: c.Actor, ID: m.ID, FeatureID: c.FeatureID, Data: map[string]any{"title": in.Title, "objective": in.Objective, "repositories": in.Repositories, "remaining": in.Remaining}})
		if err != nil {
			return nil, m, err
		}
		created := integration(st, m.ID)
		created.Kind = "hotfix"
		created.FixesFindingID = in.FindingID
		return *created, created.Meta, nil
	case "record_runtime_observation":
		var in struct {
			EnvironmentID  string `json:"environment_id"`
			GitOpsCommit   string `json:"gitops_commit"`
			ArtifactDigest string `json:"artifact_digest"`
			Healthy        bool   `json:"healthy"`
			Details        string `json:"details"`
		}
		if err := decode(c.Data, &in); err != nil {
			return nil, m, err
		}
		op := operationByID(st, c.ID)
		if op == nil || op.Status != "SUCCEEDED" || op.GitOpsResult == nil || op.Artifact == nil || op.EnvironmentID != in.EnvironmentID || op.GitOpsResult.CommitSHA != in.GitOpsCommit || op.Artifact.Digest != in.ArtifactDigest || strings.TrimSpace(in.Details) == "" {
			return nil, m, invalid("runtime evidence must match successful deployment commit/digest/environment")
		}
		env := environmentByID(st, op.EnvironmentID)
		if op.ParentID != "" && (env == nil || env.DesiredOperationID != op.ParentID) {
			return nil, m, invalid("deployment superseded")
		}
		m.ID = id()
		m.ProductID = op.ProductID
		m.FeatureID = op.FeatureID
		v := domain.RuntimeObservation{Meta: m, OperationID: op.ID, EnvironmentID: in.EnvironmentID, GitOpsCommit: in.GitOpsCommit, ArtifactDigest: in.ArtifactDigest, Healthy: in.Healthy, Details: in.Details}
		st.RuntimeObservations = append(st.RuntimeObservations, v)
		return v, m, nil
	case "reconcile_composition", "prepare_release_candidate":
		var comp *domain.Composition
		production := c.Action == "prepare_release_candidate"
		if production {
			var input struct {
				Name          string                        `json:"name"`
				EnvironmentID string                        `json:"environment_id"`
				Components    []domain.CompositionComponent `json:"components"`
				Approve       bool                          `json:"approve_main_update"`
			}
			if err := decode(c.Data, &input); err != nil {
				return nil, m, err
			}
			if !input.Approve {
				return nil, m, invalid("approve_main_update required for selected source merge into main")
			}
			cm := m
			cm.ID = id()
			result, _, err := applyComposition(st, domain.Command{Action: "plan_composition", Actor: c.Actor, ProductID: c.ProductID, Data: map[string]any{"name": input.Name, "environment_id": input.EnvironmentID, "components": input.Components}}, cm)
			if err != nil {
				return nil, m, err
			}
			value := result.(domain.Composition)
			comp = &value
			if err = releaseWorkReady(st, *comp); err != nil {
				return nil, m, err
			}
		} else {
			var input struct{}
			if err := decode(c.Data, &input); err != nil {
				return nil, m, err
			}
			comp = compositionByID(st, c.ID)
			if comp == nil {
				return nil, m, missing("composition", c.ID)
			}
			if c.ProductID != "" && c.ProductID != comp.ProductID {
				return nil, m, invalid("composition outside product")
			}
			m.ID = id()
		}
		env := environmentByID(st, comp.EnvironmentID)
		if env == nil || env.Cluster != comp.EnvironmentSnapshot.Cluster || env.Namespace != comp.EnvironmentSnapshot.Namespace {
			return nil, m, invalid("target changed; replan")
		}
		if !production {
			if err := selectionAllowed(st, env.ID); err != nil {
				return nil, m, err
			}
		}
		m.ProductID = comp.ProductID
		m.FeatureID = ""
		sources, err := flowSources(st, *comp, m.ID, production)
		if err != nil {
			return nil, m, err
		}
		prepared, err := prepareFlowDeployments(st, *comp, m)
		if err != nil {
			return nil, m, err
		}
		for _, op := range st.Operations {
			if production && op.Kind == "RELEASE_CANDIDATE" && !terminalOperation(op.Status) && op.ProductID == m.ProductID {
				return nil, m, invalid("another candidate is advancing main")
			}
		}
		kind := "COMPOSE"
		if production {
			kind = "RELEASE_CANDIDATE"
		} else {
			env.DesiredCompositionID = comp.ID
			env.DesiredOperationID = m.ID
			env.UpdatedAt = m.UpdatedAt
		}
		op := domain.ExternalOperation{Meta: m, Kind: kind, EnvironmentID: comp.EnvironmentID, Status: "PENDING", Phase: "COMPOSE_SOURCE", RequestedBy: c.Actor, CompositionSnapshot: comp, Sources: sources, PreparedDeployments: prepared, IntegrationIDs: selectedIntegrations(*comp), ChildIDs: []string{}, NextAttemptAt: time.Now().UTC()}
		st.Operations = append(st.Operations, op)
		return op, m, nil
	case "promote_release_candidate":
		var input struct {
			Approve bool `json:"approve"`
		}
		if err := decode(c.Data, &input); err != nil {
			return nil, m, err
		}
		if !input.Approve {
			return nil, m, invalid("explicit approve required")
		}
		candidate := operationByID(st, c.ID)
		if candidate == nil || candidate.Kind != "RELEASE_CANDIDATE" || candidate.Status != "SUCCEEDED" || candidate.DeploymentState != "READY_FOR_VERIFICATION" || candidate.CompositionSnapshot == nil {
			return nil, m, invalid("completed main candidate required")
		}
		if c.ProductID != "" && candidate.ProductID != c.ProductID {
			return nil, m, invalid("candidate outside product")
		}
		if err := releaseWorkReady(st, *candidate.CompositionSnapshot); err != nil {
			return nil, m, err
		}
		if err := scenarioCandidateReady(st, candidate.ID); err != nil {
			return nil, m, err
		}
		for _, op := range st.Operations {
			if op.Kind == "RELEASE_PROMOTION" && op.ReleaseCandidateID == candidate.ID {
				return nil, m, invalid("candidate promotion already requested; inspect existing operation")
			}
		}
		m.ID = id()
		m.ProductID = candidate.ProductID
		m.FeatureID = ""
		op := domain.ExternalOperation{Meta: m, Kind: "RELEASE_PROMOTION", EnvironmentID: candidate.EnvironmentID, Status: "PENDING", Phase: "QUEUE_DEPLOYMENTS", RequestedBy: c.Actor, CompositionSnapshot: candidate.CompositionSnapshot, Sources: candidate.Sources, PreparedDeployments: candidate.PreparedDeployments, IntegrationIDs: candidate.IntegrationIDs, ReleaseCandidateID: candidate.ID, ChildIDs: []string{}, NextAttemptAt: time.Now().UTC()}
		env := environmentByID(st, op.EnvironmentID)
		if env == nil {
			return nil, m, invalid("environment missing")
		}
		if err := selectionAllowed(st, env.ID); err != nil {
			return nil, m, err
		}
		env.DesiredCompositionID = op.CompositionSnapshot.ID
		env.DesiredOperationID = op.ID
		env.UpdatedAt = m.UpdatedAt
		st.Operations = append(st.Operations, op)
		return op, m, nil
	}
	return nil, m, invalid("unknown flow command")
}

// Freeze provider configuration before asynchronous source or deployment changes.
func prepareFlowDeployments(st *domain.State, comp domain.Composition, m domain.Meta) ([]domain.DeliverySnapshot, error) {
	out := []domain.DeliverySnapshot{}
	for _, component := range comp.Components {
		app := applicationByID(st, component.ApplicationID)
		if app == nil {
			return nil, invalid("component missing")
		}
		source := repositoryBinding(st, app.RepositoryID)
		build := componentBuild(st, app.ID)
		target := environmentBinding(st, comp.EnvironmentID, app.ID)
		env := environmentByID(st, comp.EnvironmentID)
		if source == nil || build == nil || target == nil || env == nil {
			return nil, invalid("deployment configuration missing")
		}
		gitops := repositoryBinding(st, target.RepositoryID)
		if gitops == nil || gitops.Role != "GITOPS" || gitops.ProductID != comp.ProductID || target.ConnectionID != gitops.ConnectionID || build.ConnectionID != source.ConnectionID {
			return nil, invalid("GitOps/source connection mismatch")
		}
		sc, err := scopedConnection(st, source.ConnectionID, comp.ProductID)
		if err != nil {
			return nil, err
		}
		gc, err := scopedConnection(st, gitops.ConnectionID, comp.ProductID)
		if err != nil {
			return nil, err
		}
		inputs := map[string]string{}
		for k, v := range build.Inputs {
			inputs[k] = v
		}
		inputs["image_repository"] = build.ImageRepository
		ref := build.WorkflowRef
		if ref == "" {
			ref = source.DefaultBranch
		}
		request := delivery.BuildRequest{Repository: repositoryLocator(st, *source), Workflow: build.Workflow, Ref: ref, Inputs: inputs, RequestedAt: m.CreatedAt}
		out = append(out, domain.DeliverySnapshot{Application: *app, Build: *build, Target: *target, Environment: *env, Source: *source, GitOps: *gitops, SourceConnection: sc.Config, GitOpsConnection: gc.Config, BuildRequest: request, GitOpsRequest: delivery.GitOpsRequest{Repository: repositoryLocator(st, *gitops), Ref: target.Ref, Path: target.Path, ImageRepository: build.ImageRepository, ImageField: target.ImageField, DigestField: target.DigestField}})
	}
	return out, nil
}
