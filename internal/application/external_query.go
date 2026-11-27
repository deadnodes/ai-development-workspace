package application

import (
	"context"
	"encoding/json"
	"fmt"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workspace"
	"slices"
	"sort"
	"strings"
	"time"
)

func executionNextAction(in *domain.Integration, ex *domain.IntegrationExecution, events []domain.ExecutionEvent, group *domain.ExecutionGroup) map[string]string {
	current := in.Status
	desired := "ready"
	code, text := "claim", ""
	if ex != nil {
		desired = ex.DesiredState
		if desired == "" {
			desired = "ready"
		}
	}
	if ex == nil || ex.State == "unassigned" || ex.State == "exited_incomplete" || ex.State == "paused" {
		text = fmt.Sprintf("Integration status is %s; desired_state is %s. Call claim_execution to resume work. Use the returned fence for every transition_integration and record_progress; reach desired_state, then call release_execution(outcome=completed).", current, desired)
	} else if ex.State == "completed" || ex.State == "cancelled" {
		code, text = "none", fmt.Sprintf("Integration status is %s; desired_state is %s. No action required.", current, desired)
	} else if ex.State == "blocked" && ex.Reconciliation.Cause == domain.ExecutionCauseBlockerResolved {
		code, text = "resume", fmt.Sprintf("Call next_work as the current assignee to confirm eligibility, then report_execution with state=running and fence=%d.", ex.Fence)
	} else if ex.State == "waiting_external" || ex.State == "blocked" {
		ref := ex.WaitingRef
		if ref == "" {
			ref = ex.BlockerRef
		}
		code, text = "wait", fmt.Sprintf("Integration status is %s; desired_state is %s. Wait for reference %s, then inspect get_execution_group.", current, desired, ref)
	} else if ex.State == "lost" || ex.State == "stalled" {
		code = "resume"
		var delivered string
		for _, ev := range events {
			if ev.IntegrationID == in.ID && ev.Resolution.ResolvedAt == nil && (ev.Type == "execution_lost" || ev.Type == "execution_stalled") {
				for _, ack := range ev.Acknowledgements {
					if ack.Outcome == "delivered" {
						delivered = ev.ID
						break
					}
				}
			}
			if delivered != "" {
				break
			}
		}
		if delivered != "" && group != nil && group.OrchestratorID != ex.OrchestratorID {
			code = "takeover"
			text = fmt.Sprintf("Integration status is %s; desired_state is %s. Different executor: call takeover_execution with delivered event id %s. Use the returned fence for every transition_integration and record_progress; reach desired_state, then call release_execution(outcome=completed).", current, desired, delivered)
		} else {
			text = fmt.Sprintf("Integration status is %s; desired_state is %s. You hold fence %d. Call heartbeat_execution(integration_id, fence=%d) to resume, finish the work, then transition_integration to %s (current: %s) with execution_fence=%d and call release_execution(outcome=completed).", current, desired, ex.Fence, ex.Fence, desired, current, ex.Fence)
		}
	} else {
		code = "advance"
		steps := remainingExecutionSteps(current, desired)
		text = fmt.Sprintf("Integration status is %s; desired_state is %s. Remaining transition steps: %s. Transition with execution_fence=%d, then call release_execution(outcome=completed).", current, desired, strings.Join(steps, " -> "), ex.Fence)
	}
	return map[string]string{"code": code, "text": text}
}

func remainingExecutionSteps(current, desired string) []string {
	order := []string{"planned", "working", "implemented", "verifying", "ready", "released"}
	i, j := slices.Index(order, current), slices.Index(order, desired)
	if i < 0 || j < 0 || i >= j {
		return []string{}
	}
	return append([]string(nil), order[i+1:j+1]...)
}

func (s *Service) Query(ctx context.Context, name, id string) (any, error) {
	switch name {
	case "list_questions", "get_question", "get_dependency_graph", "next_work", "status_overview":
		return s.queryWork(ctx, name, id)
	}
	if name == "list_branches" {
		return s.listBranches(ctx, id)
	}
	if name == "test_connection" || name == "discover_repositories" {
		if err := authorize(ctx, "admin", ""); err != nil {
			return nil, err
		}
	}
	st, e := s.State(ctx)
	if e != nil {
		return nil, e
	}
	freshness, e := s.runtimeFreshness(ctx, &st)
	if e != nil {
		return nil, e
	}
	request := *s
	request.runtimeFreshnessSnapshot = freshness
	s = &request
	switch name {
	case "get_execution_group":
		for _, g := range st.ExecutionGroups {
			if g.ID == id {
				if g.ProductID == "" {
					return nil, invalid("SCOPE_MISMATCH")
				}
				if !groupIdleOwnerVisible(ctx, g.Owner) {
					return nil, fmt.Errorf("%w: NOT_ASSIGNEE", auth.ErrForbidden)
				}
				ownedMembers := func(ids []string) []string {
					return slices.DeleteFunc(slices.Clone(ids), func(iid string) bool {
						in := integration(&st, iid)
						return in == nil || in.ProductID != g.ProductID || in.Assignee != g.Owner
					})
				}
				g.MemberIntegrationIDs = ownedMembers(g.MemberIntegrationIDs)
				g.Revisions = slices.Clone(g.Revisions)
				for index := range g.Revisions {
					g.Revisions[index].MemberIntegrationIDs = ownedMembers(g.Revisions[index].MemberIntegrationIDs)
				}
				members := []map[string]any{}
				for _, iid := range g.MemberIntegrationIDs {
					in := integration(&st, iid)
					if in == nil {
						continue
					}
					var ex *domain.IntegrationExecution
					for j := range st.IntegrationExecutions {
						if st.IntegrationExecutions[j].IntegrationID == iid {
							ex = &st.IntegrationExecutions[j]
						}
					}
					next := executionNextAction(in, ex, st.ExecutionEvents, &g)
					effectiveTimeout, timeoutSource := 900, "global"
					if ex != nil {
						global := 900
						if s.executionDefaults != nil {
							global = s.executionDefaults.StalledTimeout
						}
						effectiveTimeout, timeoutSource = effectiveStalledTimeout(&st, ex, global)
					}
					openEvents := []domain.ExecutionEvent{}
					for _, ev := range st.ExecutionEvents {
						if ev.IntegrationID == iid && ev.Resolution.ResolvedAt == nil {
							openEvents = append(openEvents, ev)
						}
					}
					members = append(members, map[string]any{"integration": in, "execution": ex, "open_events": openEvents, "effective_stalled_timeout_seconds": effectiveTimeout, "stalled_timeout_source": timeoutSource, "desired_state": func() string {
						if ex != nil {
							return ex.DesiredState
						}
						return ""
					}(), "action_required": ex != nil && ex.Reconciliation.ActionRequired, "reason": func() string {
						if ex != nil {
							return ex.Reconciliation.Reason
						}
						return ""
					}(), "next_action": next})
				}
				return map[string]any{"group": g, "members": members, "idle_evaluation": groupIdleStatus(&st, g, time.Now().UTC())}, nil
			}
		}
		return nil, missing("execution_group", id)
	case "list_execution_events":
		var options struct {
			Cursor         string `json:"cursor"`
			Limit          int    `json:"limit"`
			UnresolvedOnly bool   `json:"unresolved_only"`
			DueOnly        bool   `json:"due_only"`
		}
		productID := id
		if strings.HasPrefix(id, "{") {
			var envelope struct {
				ProductID      string `json:"product_id"`
				Cursor         string `json:"cursor"`
				Limit          int    `json:"limit"`
				UnresolvedOnly bool   `json:"unresolved_only"`
				DueOnly        bool   `json:"due_only"`
			}
			if err := json.Unmarshal([]byte(id), &envelope); err != nil {
				return nil, invalid("invalid event query")
			}
			productID = envelope.ProductID
			options.Cursor = envelope.Cursor
			options.Limit = envelope.Limit
			options.UnresolvedOnly = envelope.UnresolvedOnly
			options.DueOnly = envelope.DueOnly
		}
		if !productExists(&st, productID) {
			return nil, missing("product", productID)
		}
		if options.Limit < 0 || options.Limit > 100 {
			return nil, invalid("limit must be between 1 and 100")
		}
		if options.Limit == 0 {
			options.Limit = 100
		}
		items := []domain.ExecutionEvent{}
		now := time.Now().UTC()
		for _, ev := range st.ExecutionEvents {
			if ev.ProductID != productID {
				continue
			}
			if !executionEventVisibleTo(ctx, &st, ev) {
				continue
			}
			if options.UnresolvedOnly && ev.Resolution.ResolvedAt != nil {
				continue
			}
			if options.DueOnly && (ev.Resolution.ResolvedAt != nil || now.Before(ev.Delivery.NextAttemptAt) || !groupWakeCurrent(&st, ev) || (domain.IsGroupIdleEvent(ev.Type) && !groupIdleEventCurrent(&st, ev, now))) {
				continue
			}
			if options.Cursor != "" {
				cursorTime, cursorID, ok := parseExecutionEventCursor(options.Cursor)
				if !ok {
					return nil, invalid("invalid cursor")
				}
				if ev.CreatedAt.Before(cursorTime) || ev.CreatedAt.Equal(cursorTime) && ev.ID <= cursorID {
					continue
				}
			}
			items = append(items, ev)
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].CreatedAt.Equal(items[j].CreatedAt) {
				return items[i].ID < items[j].ID
			}
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		})
		hasMore := len(items) > options.Limit
		next := ""
		if hasMore {
			items = items[:options.Limit]
			last := items[len(items)-1]
			next = fmt.Sprintf("%s|%s", last.CreatedAt.UTC().Format(time.RFC3339Nano), last.ID)
		}
		return map[string]any{"items": items, "next_cursor": next}, nil
	case "list_executions":
		if !productExists(&st, id) {
			return nil, missing("product", id)
		}
		items := []domain.IntegrationExecution{}
		for _, ex := range st.IntegrationExecutions {
			if ex.ProductID == id {
				items = append(items, ex)
			}
		}
		return items, nil
	case "get_intelligence_task":
		return s.intelligenceQuery(&st, id)
	case "get_feature_graph":
		graph, err := featureGraph(st, id)
		if err != nil {
			return nil, err
		}
		for i := range graph.Deployments {
			for j := range graph.Deployments[i].RuntimeObservations {
				r := &graph.Deployments[i].RuntimeObservations[j]
				r.ObservedAt = domain.RuntimeObservedAt(r.RuntimeObservation, freshness)
			}
		}
		return graph, nil
	case "get_artifact_retention":
		if !productExists(&st, id) {
			return nil, missing("product", id)
		}
		return s.evaluateRetention(st, id, time.Now().UTC(), freshness), nil
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
				if workspace.CloneURLHasCredentials(info.URL) {
					return invalid("discovered repository clone URL contains credentials, query parameters or a fragment")
				}
				registered := registeredRepositoryIdentity(current, id, info.FullName, info.ID)
				if registered == nil {
					current.RegisteredRepositories = append(current.RegisteredRepositories, domain.RegisteredRepository{Meta: domain.Meta{ID: generateRegistryID(), Actor: attributedActor(ctx, "discovery"), CreatedAt: now, UpdatedAt: now}, ConnectionID: id})
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

	case "get_integration_context":
		in := integration(&st, id)
		if in == nil {
			return nil, missing("integration", id)
		}
		featureContext, e := s.resumeFromState(ctx, st, in.FeatureID)
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
		prs := []GraphPullRequest{}
		graph, err := featureGraph(st, in.FeatureID)
		if err != nil {
			return nil, err
		}
		for _, repository := range graph.Repositories {
			for _, pr := range repository.PullRequests {
				if graphContains(pr.IntegrationIDs, id) {
					prs = append(prs, pr)
				}
			}
		}
		return map[string]any{"integration_id": id, "branches": in.Branches, "commits": in.Commits, "revisions": revisions, "observations": obs, "pull_requests": prs}, nil
	case "get_unlinked_git_commits":
		if !productExists(&st, id) {
			return nil, missing("product", id)
		}
		return unlinkedGitCommits(st, id), nil
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
		snapshots := []domain.RuntimeSnapshot{}
		for _, v := range st.RuntimeSnapshots {
			if v.EnvironmentID == id && v.ProductID == env.ProductID {
				snapshots = append(snapshots, v)
			}
		}
		out := s.candidateRuntimeState(&st, id, "")
		for k, v := range s.restorationQuery(&st, id) {
			out[k] = v
		}
		for k, v := range s.baselineQuery(&st, id) {
			out[k] = v
		}
		for k, v := range map[string]any{"dev_reservation": ReservationState(&st, id), "runtime_targets": s.runtimeTargets(env.ProductID, id), "runtime_snapshots": snapshots, "environment": env, "bindings": bindings, "operations": operations, "desired_composition": compositionByID(&st, env.DesiredCompositionID), "runtime_observations": observationViews(observations, freshness), "reconciliation": "Runtime evidence is recorded by the configured read-only Flux/Kubernetes observer or an external executor. Inspect each observation actor, timestamp and details; only exact matching healthy commit/digest evidence confirms reconciliation."} {
			out[k] = v
		}
		return out, nil
	case "get_worker_job":
		j := workerJob(&st, id)
		if j == nil {
			return nil, missing("worker job", id)
		}
		attempts := []domain.WorkerAttempt{}
		for _, a := range st.WorkerAttempts {
			if a.JobID == j.ID {
				attempts = append(attempts, a)
			}
		}
		return map[string]any{"job": j, "attempts": attempts}, nil
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
				activationID := ""
				if op.Kind == devActivationKind {
					activationID = op.ID
				}
				if op.Kind == devRuntimeKind {
					activationID = op.ParentID
				}
				out := s.candidateRuntimeState(&st, op.EnvironmentID, activationID)
				for k, v := range s.restorationQuery(&st, op.EnvironmentID) {
					out[k] = v
				}
				for k, v := range s.baselineQuery(&st, op.EnvironmentID) {
					out[k] = v
				}
				for k, v := range map[string]any{"runtime_observations": observationViews(operationObservations(&st, op.ID), freshness), "operation": op, "steps": steps, "children": children, "feature_contexts": contexts, "dev_reservation": ReservationState(&st, op.EnvironmentID)} {
					out[k] = v
				}
				if action := operationConflictAction(&st, op, time.Now().UTC()); action != nil {
					out["agent_action"] = action
				} else if action := operationBuildFailureAction(&st, op, time.Now().UTC()); action != nil {
					out["agent_action"] = action
				}
				return out, nil
			}
		}
		return nil, missing("operation", id)
	case "list_attention":
		if !productExists(&st, id) {
			return nil, missing("product", id)
		}
		items := retentionAttentionReport(s.evaluateRetention(st, id, time.Now().UTC(), freshness))
		for _, question := range st.Memories {
			if question.ProductID == id && question.Kind == "question" && question.Status == "open" && question.Question != nil && question.Question.AddressedTo == workUser(ctx) {
				items = append(items, map[string]any{"id": "question:" + question.ID, "question_id": question.ID, "feature_id": question.FeatureID, "code": "OPEN_QUESTION", "text": question.Title})
			}
		}
		for i := range st.Integrations {
			in := &st.Integrations[i]
			if in.ProductID == id && in.Assignee != "" && !executionVisibleTo(ctx, in) {
				items = append(items, map[string]any{"id": "integration:" + in.ID, "integration_id": in.ID, "code": "none", "text": "assigned to " + in.Assignee})
			}
		}
		var secretWarnings []map[string]any
		var secretRefreshFailures map[string]bool
		if err := s.view(ctx, func(raw *domain.State) error {
			if err := requireProductRead(ctx, raw, id); err != nil {
				return err
			}
			secretWarnings = connectionSecretWarnings(raw, id)
			secretRefreshFailures = connectionSecretRefreshFailures(raw, id, secretWarnings)
			return nil
		}); err != nil {
			return nil, err
		}
		items = append(items, secretWarnings...)
		items = append(items, unlinkedGitCommits(st, id)...)
		items = append(items, buildReconciliationAttention(st, id, time.Now().UTC())...)
		for _, ex := range st.IntegrationExecutions {
			if ex.ProductID == id && ex.Reconciliation.Enabled && ex.Reconciliation.ActionRequired && executionVisibleTo(ctx, integration(&st, ex.IntegrationID)) {
				items = append(items, map[string]any{"id": "execution:" + ex.IntegrationID, "integration_id": ex.IntegrationID, "reason": ex.Reconciliation.Reason, "state": ex.State, "generation": ex.Reconciliation.Generation})
			}
		}
		_, _, escalationThreshold := executionNotifyConfig(s)
		for _, ev := range st.ExecutionEvents {
			if ev.ProductID == id && ev.Resolution.ResolvedAt == nil && executionEventVisibleTo(ctx, &st, ev) && (ev.EscalationRequired || ev.Delivery.Attempts >= escalationThreshold) {
				items = append(items, map[string]any{"id": "execution-event:" + ev.ID, "execution_event_id": ev.ID, "integration_id": ev.IntegrationID, "reason": "EXECUTION_EVENT_ESCALATION_REQUIRED", "type": ev.Type, "attempts": ev.Delivery.Attempts})
			}
		}
		if s.builds != nil {
			items = append(items, deploymentReconciliationAttention(st, id, time.Now().UTC(), time.Duration(s.builds.config.StaleTimeoutSeconds())*time.Second)...)
		}
		ciBuilds := buildCIAttention(st, id)
		items = append(items, ciBuilds...)
		items = append(items, mainCIAttention(st, id)...)
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
			if secretRefreshFailures[op.ID] {
				continue
			}
			if op.ProductID == id && (op.Status == "FAILED" || op.DeploymentState == "GITOPS_APPLIED") {
				if op.Kind == "REFRESH_GIT" && op.Status == "FAILED" {
					// A feature branch commonly disappears after its PR is merged.
					// Keep that observation in audit history, but do not turn the
					// expected 404 into an actionable attention item.
					if isDeletedBranchObservation(op.Detail) {
						continue
					}
					superseded := false
					for _, later := range st.Operations {
						if later.ProductID == op.ProductID && later.IntegrationID == op.IntegrationID && later.Kind == "REFRESH_GIT" && later.Status == "SUCCEEDED" && later.CreatedAt.After(op.CreatedAt) && later.FinishedAt != nil && op.FinishedAt != nil && later.FinishedAt.After(*op.FinishedAt) {
							superseded = true
							break
						}
					}
					if superseded {
						continue
					}
				}
				reason := "DEPLOY_FAILED"
				if op.Kind == "CHECK_PUBLICATION" {
					reason = "CHECK_PUBLICATION_RECONCILIATION_REQUIRED"
				}
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
					latest := latestRuntimeTargets(st.RuntimeObservations, op.ID)
					observed := len(latest) > 0
					for _, r := range latest {
						observed = observed && r.Healthy && r.EnvironmentID == op.EnvironmentID && op.Artifact != nil && op.GitOpsResult != nil && r.ArtifactDigest == op.Artifact.Digest && r.GitOpsCommit == op.GitOpsResult.CommitSHA
					}
					if observed {
						continue
					}
					reason = "PENDING_RECONCILIATION"
				}
				items = append(items, map[string]any{"id": op.ID, "reason": reason, "detail": op.Detail, "integration_id": op.IntegrationID, "environment_id": op.EnvironmentID})
			}
		}
		// Git branch drift is read-only context, not an Operations task. It is
		// available through get_git_state / the feature graph; a concrete deploy
		// operation still performs its own freshness and divergence checks.
		sort.Slice(items, func(i, j int) bool { return items[i]["id"].(string) < items[j]["id"].(string) })
		return items, nil
	}
	return nil, invalid("unknown query")
}

func (s *Service) listBranches(ctx context.Context, repositoryID string) (any, error) {
	if err := authorize(ctx, "read", ""); err != nil {
		return nil, err
	}
	var selected domain.GitHubConnection
	var locator string
	if err := s.view(ctx, func(st *domain.State) error {
		principal, restricted := auth.FromContext(ctx)
		for index := len(st.RepositoryBindings) - 1; index >= 0; index-- {
			binding := st.RepositoryBindings[index]
			if binding.RepositoryID != repositoryID || requireProductRead(ctx, st, binding.ProductID) != nil || restricted && !allowedProduct(principal, binding.ProductID) {
				continue
			}
			conn := connection(st, binding.ConnectionID)
			if conn == nil || s.provider == nil {
				return invalid("external provider/connection unavailable")
			}
			selected, locator = *conn, repositoryLocator(st, binding)
			return nil
		}
		return missing("repository binding", repositoryID)
	}); err != nil {
		return nil, err
	}
	branches, err := s.provider.Branches(ctx, selected.Config, locator)
	if err != nil {
		return nil, invalid("branch discovery: %s", err)
	}
	return branches, nil
}

func parseExecutionEventCursor(cursor string) (time.Time, string, bool) {
	parts := strings.SplitN(cursor, "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return time.Time{}, "", false
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	return t, parts[1], err == nil
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
