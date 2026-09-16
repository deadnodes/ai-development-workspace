package application

import (
	"context"
	"releasecontrol/internal/domain"
	"slices"
	"sort"
	"time"
)

func (s *Service) Query(ctx context.Context, name, id string) (any, error) {
	st, e := s.State(ctx)
	if e != nil {
		return nil, e
	}
	switch name {
	case "get_feature_graph":
		return featureGraph(st, id)
	case "get_artifact_retention":
		if !productExists(&st, id) {
			return nil, missing("product", id)
		}
		return domain.EvaluateRetention(st, id), nil
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
		observations := []domain.RuntimeObservation{}
		for _, r := range st.RuntimeObservations {
			if r.EnvironmentID == id {
				observations = append(observations, r)
			}
		}
		return map[string]any{"environment": env, "bindings": bindings, "operations": operations, "desired_composition": compositionByID(&st, env.DesiredCompositionID), "runtime_observations": observations, "reconciliation": "Runtime evidence is recorded by the configured read-only Flux/Kubernetes observer or an external executor. Inspect each observation actor, timestamp and details; only exact matching healthy commit/digest evidence confirms reconciliation."}, nil
	case "get_operation":
		for _, op := range st.Operations {
			if op.ID == id {
				steps := []domain.OperationStep{}
				for _, step := range st.OperationSteps {
					if step.OperationID == id {
						steps = append(steps, step)
					}
				}
				children := []domain.ExternalOperation{}
				for _, child := range st.Operations {
					if child.ParentID == id {
						children = append(children, child)
					}
				}
				contexts := map[string]any{}
				for _, iid := range op.IntegrationIDs {
					in := integration(&st, iid)
					if in == nil {
						continue
					}
					if _, found := contexts[in.FeatureID]; !found {
						value, err := s.Resume(ctx, in.FeatureID)
						if err != nil {
							return nil, err
						}
						contexts[in.FeatureID] = value
					}
				}
				return map[string]any{"operation": op, "steps": steps, "children": children, "feature_contexts": contexts}, nil
			}
		}
		return nil, missing("operation", id)
	case "list_attention":
		if !productExists(&st, id) {
			return nil, missing("product", id)
		}
		items := retentionAttention(st, id)
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
				if op.Phase == "COMPOSE_SOURCE" {
					for _, source := range op.Sources {
						if source.Result != nil && source.Result.Conflict {
							reason = "MERGE_CONFLICT"
						}
					}
				}
				if op.Phase == "INSPECT" {
					reason = "ARTIFACT_MISSING"
				}
				if op.DeploymentState == "GITOPS_APPLIED" {
					observed := false
					for _, r := range st.RuntimeObservations {
						if r.OperationID == op.ID {
							observed = r.Healthy && op.Artifact != nil && op.GitOpsResult != nil && r.ArtifactDigest == op.Artifact.Digest && r.GitOpsCommit == op.GitOpsResult.CommitSHA
						}
					}
					if observed {
						continue
					}
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
	conflicts := []domain.CompositionConflict{}
	for _, conflict := range st.CompositionConflicts {
		for _, in := range conflict.Integrations {
			if in.FeatureID == featureID && (integrationID == "" || in.ID == integrationID) {
				conflicts = append(conflicts, conflict)
				break
			}
		}
	}
	out["composition_conflicts"] = conflicts
	observations := []domain.GitObservation{}
	operations := []domain.ExternalOperation{}
	artifacts := []domain.DeliveryArtifact{}
	builds := []domain.DeliveryBuildRun{}
	operationIDs := map[string]bool{}
	relevantIntegrations := map[string]bool{}
	for _, in := range st.Integrations {
		if in.FeatureID == featureID && (integrationID == "" || in.ID == integrationID) {
			relevantIntegrations[in.ID] = true
		}
	}
	for _, op := range st.Operations {
		for _, iid := range op.IntegrationIDs {
			if relevantIntegrations[iid] {
				operationIDs[op.ID] = true
				for _, cid := range op.ChildIDs {
					operationIDs[cid] = true
				}
			}
		}
	}
	for _, op := range st.Operations {
		if operationIDs[op.ID] || (op.FeatureID == featureID && (integrationID == "" || op.IntegrationID == integrationID)) {
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
	scenarios := []domain.ScenarioVersion{}
	scenarioIDs := []string{}
	f := feature(&st, featureID)
	for _, v := range st.ScenarioVersions {
		if f == nil || v.ProductID != f.ProductID {
			continue
		}
		relevant := len(v.IntegrationIDs) == 0
		for _, iid := range v.IntegrationIDs {
			if relevantIntegrations[iid] {
				relevant = true
			}
		}
		if relevant {
			scenarios = append(scenarios, v)
			scenarioIDs = append(scenarioIDs, v.ID)
		}
	}
	runs := []domain.ScenarioRun{}
	for _, r := range st.ScenarioRuns {
		if slices.Contains(scenarioIDs, r.ScenarioVersionID) {
			runs = append(runs, r)
		}
	}
	runtime := []domain.RuntimeObservation{}
	for _, r := range st.RuntimeObservations {
		if operationIDs[r.OperationID] {
			runtime = append(runtime, r)
		}
	}
	publicationTargets := []domain.PublicationTarget{}
	packageArtifacts := []domain.PackageArtifact{}
	relevantRepos := map[string]bool{}
	for _, in := range st.Integrations {
		if relevantIntegrations[in.ID] {
			for _, rid := range in.Repositories {
				relevantRepos[rid] = true
			}
			for _, b := range in.Branches {
				relevantRepos[b.RepositoryID] = true
			}
		}
	}
	if integrationID == "" && f != nil {
		for _, rid := range f.Repositories {
			relevantRepos[rid] = true
		}
	}
	for _, artifact := range st.PackageArtifacts {
		if f != nil && artifact.ProductID == f.ProductID && relevantIntegrations[artifact.IntegrationID] {
			relevantRepos[artifact.RepositoryID] = true
		}
	}
	libraries := map[string]bool{}
	components := []domain.Application{}
	for _, app := range st.Applications {
		if f != nil && app.ProductID == f.ProductID && relevantRepos[app.RepositoryID] {
			components = append(components, app)
			libraries[app.ID] = true
		}
	}
	for _, v := range st.PublicationTargets {
		if libraries[v.ApplicationID] {
			publicationTargets = append(publicationTargets, v)
		}
	}
	for _, v := range st.PackageArtifacts {
		if libraries[v.ApplicationID] && (v.IntegrationID == "" || relevantIntegrations[v.IntegrationID]) {
			packageArtifacts = append(packageArtifacts, v)
		}
	}
	out["components"] = components
	out["publication_targets"] = publicationTargets
	out["package_artifacts"] = packageArtifacts
	out["scenario_versions"] = scenarios
	out["scenario_runs"] = runs
	out["runtime_observations"] = runtime
	out["review_syncs"] = reviewSyncs
	out["git_observations"] = observations
	out["operations"] = operations
	out["artifacts"] = artifacts
	out["build_runs"] = builds
}
