package application

import (
	"context"
	"releasecontrol/internal/domain"
	"sort"
	"time"
)

func (s *Service) Query(ctx context.Context, name, id string) (any, error) {
	st, e := s.State(ctx)
	if e != nil {
		return nil, e
	}
	switch name {
	case "get_product_configuration":
		return productConfiguration(st, id)
	case "test_connection", "discover_repositories":
		c := connection(&st, id)
		if c == nil {
			return nil, missing("connection", id)
		}
		if s.provider == nil {
			return nil, invalid("external provider not configured")
		}
		if name == "test_connection" {
			err := s.provider.TestConnection(ctx, c.Config)
			now := time.Now().UTC()
			saveErr := s.store.Update(ctx, func(current *domain.State) error {
				v := connection(current, id)
				if v == nil {
					return missing("connection", id)
				}
				v.Connected = err == nil
				v.LastTestedAt = &now
				v.LastError = ""
				if err != nil {
					v.LastError = err.Error()
				}
				return nil
			})
			if saveErr != nil {
				return nil, saveErr
			}
			if err != nil {
				return nil, invalid("connection test: %s", err)
			}
			return map[string]any{"ok": true, "connection_id": id}, nil
		}
		repos, err := s.provider.DiscoverRepositories(ctx, c.Config)
		if err != nil {
			return nil, invalid("repository discovery: %s", err)
		}
		now := time.Now().UTC()
		err = s.store.Update(ctx, func(current *domain.State) error {
			v := connection(current, id)
			if v == nil {
				return missing("connection", id)
			}
			v.RepositoryCount = len(repos)
			v.Connected = true
			v.LastTestedAt = &now
			v.LastError = ""
			for _, info := range repos {
				registered := registeredRepositoryIdentity(current, id, info.FullName, info.ID)
				if registered == nil {
					current.RegisteredRepositories = append(current.RegisteredRepositories, domain.RegisteredRepository{Meta: domain.Meta{ID: generateRegistryID(), Actor: "discovery", CreatedAt: now, UpdatedAt: now}, ConnectionID: id})
					registered = &current.RegisteredRepositories[len(current.RegisteredRepositories)-1]
				}
				associateRegistryConnection(registered, v)
				registered.ProviderRepositoryID = info.ID
				registered.FullName = info.FullName
				registered.DefaultBranch = info.DefaultBranch
				registered.URL = info.URL
				registered.Private = info.Private
				registered.ObservedAt = now
				registered.UpdatedAt = now
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return repos, nil

	case "list_branches":
		b := repositoryBinding(&st, id)
		if b == nil {
			return nil, missing("repository binding", id)
		}
		c := connection(&st, b.ConnectionID)
		if c == nil || s.provider == nil {
			return nil, invalid("external provider/connection unavailable")
		}
		branches, err := s.provider.Branches(ctx, c.Config, repositoryLocator(&st, *b))
		if err != nil {
			return nil, invalid("branch discovery: %s", err)
		}
		return branches, nil
	case "get_integration_context":
		in := integration(&st, id)
		if in == nil {
			return nil, missing("integration", id)
		}
		featureContext, e := s.Resume(ctx, in.FeatureID)
		if e != nil {
			return nil, e
		}
		out := map[string]any{"integration": in, "feature_context": featureContext}
		addDeliveryContext(out, st, in.FeatureID, id)
		out["external_context"] = externalContext(&st, in.FeatureID, id)
		return out, nil
	case "get_integration_git":
		in := integration(&st, id)
		if in == nil {
			return nil, missing("integration", id)
		}
		obs := []domain.GitObservation{}
		revisions := []domain.IntegrationRevision{}
		for _, o := range st.GitObservations {
			if o.IntegrationID == id {
				obs = append(obs, o)
			}
		}
		for _, r := range st.IntegrationRevisions {
			if r.IntegrationID == id {
				revisions = append(revisions, r)
			}
		}
		return map[string]any{"integration_id": id, "branches": in.Branches, "commits": in.Commits, "revisions": revisions, "observations": obs}, nil
	case "get_environment_state":
		env := environmentByID(&st, id)
		if env == nil {
			return nil, missing("environment", id)
		}
		bindings := []domain.EnvironmentBinding{}
		operations := []domain.ExternalOperation{}
		for _, b := range st.EnvironmentBindings {
			if b.EnvironmentID == id {
				bindings = append(bindings, b)
			}
		}
		for _, op := range st.Operations {
			if op.EnvironmentID == id {
				operations = append(operations, op)
			}
		}
		return map[string]any{"environment": env, "bindings": bindings, "operations": operations, "reconciliation": "not_observed_by_this_milestone"}, nil
	case "get_operation":
		for _, op := range st.Operations {
			if op.ID == id {
				steps := []domain.OperationStep{}
				for _, step := range st.OperationSteps {
					if step.OperationID == id {
						steps = append(steps, step)
					}
				}
				return map[string]any{"operation": op, "steps": steps}, nil
			}
		}
		return nil, missing("operation", id)
	case "list_attention":
		if !productExists(&st, id) {
			return nil, missing("product", id)
		}
		items := []map[string]any{}
		for _, finding := range st.Findings {
			if finding.ProductID == id && finding.Status == "open" && finding.ReviewSource != nil {
				integrationID := ""
				if len(finding.IntegrationIDs) > 0 {
					integrationID = finding.IntegrationIDs[0]
				}
				items = append(items, map[string]any{"id": finding.ID, "reason": "CODE_REVIEW_FINDING", "detail": finding.Body, "feature_id": finding.FeatureID, "integration_ids": finding.IntegrationIDs, "integration_id": integrationID, "priority": finding.ReviewSource.Priority, "url": finding.ReviewSource.Comment.URL})
			}
		}
		for _, op := range st.Operations {
			if op.ProductID == id && (op.Status == "FAILED" || op.DeploymentState == "GITOPS_APPLIED") {
				reason := "DEPLOY_FAILED"
				if op.Kind == "REFRESH_GIT" {
					reason = "GIT_SYNC_FAILED"
				}
				if op.Phase == "BUILD_LOOKUP" || op.Phase == "DISPATCH" {
					reason = "BUILD_FAILED"
				}
				if op.Phase == "INSPECT" {
					reason = "ARTIFACT_MISSING"
				}
				if op.DeploymentState == "GITOPS_APPLIED" {
					reason = "PENDING_RECONCILIATION"
				}
				items = append(items, map[string]any{"id": op.ID, "reason": reason, "detail": op.Detail, "integration_id": op.IntegrationID, "environment_id": op.EnvironmentID})
			}
		}
		latest := map[string]domain.GitObservation{}
		for _, o := range st.GitObservations {
			if o.ProductID == id {
				latest[o.IntegrationID+"/"+o.RepositoryID+"/"+o.Branch] = o
			}
		}
		for _, o := range latest {
			if o.Behind > 0 {
				reason := "BRANCH_BEHIND"
				if o.Ahead > 0 {
					reason = "BRANCH_DIVERGED"
				}
				items = append(items, map[string]any{"id": o.ID, "reason": reason, "integration_id": o.IntegrationID, "repository_id": o.RepositoryID, "branch": o.Branch, "ahead": o.Ahead, "behind": o.Behind})
			}
		}
		sort.Slice(items, func(i, j int) bool { return items[i]["id"].(string) < items[j]["id"].(string) })
		return items, nil
	}
	return nil, invalid("unknown query")
}

func generateRegistryID() string { return id() }
func addDeliveryContext(out map[string]any, st domain.State, featureID, integrationID string) {
	observations := []domain.GitObservation{}
	operations := []domain.ExternalOperation{}
	artifacts := []domain.DeliveryArtifact{}
	builds := []domain.DeliveryBuildRun{}
	operationIDs := map[string]bool{}
	for _, op := range st.Operations {
		if op.FeatureID == featureID && (integrationID == "" || op.IntegrationID == integrationID) {
			operations = append(operations, op)
			operationIDs[op.ID] = true
		}
	}
	for _, v := range st.GitObservations {
		if v.FeatureID == featureID && (integrationID == "" || v.IntegrationID == integrationID) {
			observations = append(observations, v)
		}
	}
	for _, v := range st.DeliveryArtifacts {
		if operationIDs[v.OperationID] {
			artifacts = append(artifacts, v)
		}
	}
	for _, v := range st.DeliveryBuildRuns {
		if operationIDs[v.OperationID] {
			builds = append(builds, v)
		}
	}
	reviewSyncs := []domain.ReviewSync{}
	for _, v := range st.ReviewSyncs {
		if v.FeatureID == featureID && (integrationID == "" || v.IntegrationID == integrationID) {
			reviewSyncs = append(reviewSyncs, v)
		}
	}
	out["review_syncs"] = reviewSyncs
	out["git_observations"] = observations
	out["operations"] = operations
	out["artifacts"] = artifacts
	out["build_runs"] = builds
}
