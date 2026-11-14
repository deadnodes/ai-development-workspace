package application

import (
	"context"
	"errors"
	"releasecontrol/internal/domain"
	"strings"
	"time"
)

const autoGitActor = "system/git-sync"
const autoComposeActor = "system/environment-compose"

func repositoryGitInventoryRole(role string) bool {
	switch strings.ToUpper(strings.TrimSpace(role)) {
	case "SOURCE", "APPLICATION", "LIBRARY", "MIXED":
		return true
	default:
		return false
	}
}

// Active scope is explicit work ownership + bound branches, or an open linked PR.
// A whole repository registration alone never enables polling.
func autoGitEligible(st *domain.State, in domain.Integration) bool {
	f := feature(st, in.FeatureID)
	if f == nil || f.ProductID != in.ProductID || f.Status == "archived" || f.Status == "completed" || in.Status == "released" {
		return false
	}
	validRepo := func(id string) bool {
		b := repositoryBinding(st, id)
		if b == nil || b.ProductID != in.ProductID {
			return false
		}
		_, err := scopedConnection(st, b.ConnectionID, in.ProductID)
		return err == nil
	}
	for _, p := range in.PullRequests {
		if (strings.EqualFold(p.Status, "open") || strings.EqualFold(p.Status, "draft")) && validRepo(p.RepositoryID) {
			return true
		}
	}
	if in.Owner == "" || (in.Status != "working" && in.Status != "implemented" && in.Status != "verifying") {
		return false
	}
	for _, b := range in.Branches {
		if validRepo(b.RepositoryID) {
			return true
		}
	}
	return false
}

type gitRefreshHistory struct {
	last    *domain.ExternalOperation
	pending bool
}

type gitRefreshPlan struct {
	operations []domain.ExternalOperation
	events     []domain.Event
}

// Planning only borrows state. Replan under the writer lock before appending.
func (s *Service) scheduleGitPlan(ctx context.Context, now time.Time, interval time.Duration, planner func(*domain.State, time.Time, time.Duration) gitRefreshPlan) error {
	if interval <= 0 || s.provider == nil {
		return nil
	}
	var runnable bool
	if err := s.view(ctx, func(st *domain.State) error {
		runnable = len(planner(st, now, interval).operations) > 0
		return nil
	}); err != nil || !runnable {
		return err
	}
	update := s.store.Update
	if scoped, ok := s.store.(interface {
		UpdateScoped(context.Context, domain.StateMutationScope, func(*domain.State) error) error
	}); ok {
		update = func(ctx context.Context, fn func(*domain.State) error) error {
			// Events are implicitly append-only in the scoped mutation contract.
			return scoped.UpdateScoped(ctx, domain.StateMutationScope{AppendCollections: []string{"operations"}}, fn)
		}
	}
	return update(ctx, func(st *domain.State) error {
		plan := planner(st, now, interval)
		for i := range plan.operations {
			plan.operations[i].ID = id()
			plan.events[i].ID = id()
			plan.events[i].EntityID = plan.operations[i].ID
		}
		st.Operations = append(st.Operations, plan.operations...)
		st.Events = append(st.Events, plan.events...)
		return nil
	})
}

func (s *Service) ScheduleGitRefresh(ctx context.Context, now time.Time, interval time.Duration) error {
	return s.scheduleGitPlan(ctx, now, interval, planGitRefresh)
}

func planGitRefresh(st *domain.State, now time.Time, interval time.Duration) gitRefreshPlan {
	var plan gitRefreshPlan
	queued := 0
	active := 0
	index := map[string]gitRefreshHistory{}
	for i := range st.Operations {
		op := &st.Operations[i]
		if op.Kind != "REFRESH_GIT" {
			continue
		}
		key := op.IntegrationID
		history := index[key]
		if !terminalOperation(op.Status) {
			active++
			history.pending = true
		}
		if history.last == nil || op.CreatedAt.After(history.last.CreatedAt) {
			history.last = op
		}
		index[key] = history
	}
	if active >= 5 {
		return plan
	}
	for _, in := range st.Integrations {
		if !autoGitEligible(st, in) {
			continue
		}
		history := index[in.ID]
		last, pending := history.last, history.pending
		if pending {
			continue
		}
		wait := interval
		if last != nil {
			if last.Status == "FAILED" || last.Status == "BLOCKED" {
				wait = 5 * interval
			}
			since := last.UpdatedAt
			if last.FinishedAt != nil {
				since = *last.FinishedAt
			}
			if since.IsZero() {
				since = last.CreatedAt
			}
			if now.Sub(since) < wait {
				continue
			}
		}
		m := domain.Meta{ProductID: in.ProductID, FeatureID: in.FeatureID, Actor: autoGitActor, CreatedAt: now, UpdatedAt: now}
		plan.operations = append(plan.operations, domain.ExternalOperation{Meta: m, Kind: "REFRESH_GIT", IntegrationID: in.ID, Status: "PENDING", RequestedBy: autoGitActor, Phase: "OBSERVE", NextAttemptAt: now})
		plan.events = append(plan.events, domain.Event{Action: "refresh_integration_git", Actor: autoGitActor, At: now, EntityID: m.ID, ProductID: in.ProductID, FeatureID: in.FeatureID, Data: domain.Command{Action: "refresh_integration_git", Actor: autoGitActor, IntegrationID: in.ID}})
		index[in.ID] = gitRefreshHistory{pending: true}
		queued++
		active++
		if queued >= 5 || active >= 5 {
			break
		}
	}

	return plan
}

// ScheduleRepositoryGitRefresh keeps the repository registry observable even
// when no Integration has been linked yet. It is deliberately bounded: one
// repository scan is a durable operation and the worker performs the provider
// calls outside the state transaction.
func (s *Service) ScheduleRepositoryGitRefresh(ctx context.Context, now time.Time, interval time.Duration) error {
	return s.scheduleGitPlan(ctx, now, interval, planRepositoryGitRefresh)
}

func planRepositoryGitRefresh(st *domain.State, now time.Time, interval time.Duration) gitRefreshPlan {
	var plan gitRefreshPlan
	queued := 0
	active := 0
	index := map[string]gitRefreshHistory{}
	for i := range st.Operations {
		op := &st.Operations[i]
		if op.Kind != "REFRESH_REPOSITORY_GIT" {
			continue
		}
		key := op.ProductID + "/" + op.RepositoryID
		history := index[key]
		if !terminalOperation(op.Status) {
			active++
			history.pending = true
		}
		if history.last == nil || op.CreatedAt.After(history.last.CreatedAt) {
			history.last = op
		}
		index[key] = history
	}
	if active >= 3 {
		return plan
	}
	seen := map[string]bool{}
	for _, binding := range st.RepositoryBindings {
		if !repositoryGitInventoryRole(binding.Role) || binding.ProductID == "" || seen[binding.ProductID+"/"+binding.RepositoryID] {
			continue
		}
		if _, err := scopedConnection(st, binding.ConnectionID, binding.ProductID); err != nil {
			continue
		}
		seen[binding.ProductID+"/"+binding.RepositoryID] = true
		history := index[binding.ProductID+"/"+binding.RepositoryID]
		last, pending := history.last, history.pending
		if pending {
			continue
		}
		wait := interval
		if last != nil {
			if last.Status == "FAILED" || last.Status == "BLOCKED" {
				wait = 5 * interval
			}
			since := last.UpdatedAt
			if last.FinishedAt != nil {
				since = *last.FinishedAt
			}
			if since.IsZero() {
				since = last.CreatedAt
			}
			if now.Sub(since) < wait {
				continue
			}
		}
		m := domain.Meta{ProductID: binding.ProductID, Actor: autoGitActor, CreatedAt: now, UpdatedAt: now}
		plan.operations = append(plan.operations, domain.ExternalOperation{Meta: m, Kind: "REFRESH_REPOSITORY_GIT", RepositoryID: binding.RepositoryID, Status: "PENDING", RequestedBy: autoGitActor, Phase: "SCAN", NextAttemptAt: now})
		plan.events = append(plan.events, domain.Event{Action: "refresh_repository_git", Actor: autoGitActor, At: now, EntityID: m.ID, ProductID: binding.ProductID, Data: domain.Command{Action: "refresh_repository_git", Actor: autoGitActor, ProductID: binding.ProductID, Data: map[string]any{"repository_id": binding.RepositoryID}}})
		index[binding.ProductID+"/"+binding.RepositoryID] = gitRefreshHistory{pending: true}
		queued++
		active++
		if queued >= 3 || active >= 3 {
			break
		}
	}

	return plan
}

// ScheduleCompositionRefresh rematerializes a selected DEV/TEST composition
// when background Git observation captures a newer revision. It
// refreshes every eligible environment in a pass and never touches PROD.
func (s *Service) ScheduleCompositionRefresh(ctx context.Context, now time.Time, interval time.Duration) error {
	if interval <= 0 || s.provider == nil {
		return nil
	}
	type refreshCommand struct {
		command domain.Command
		origin  composeRefreshOrigin
	}
	var commands []refreshCommand
	err := s.view(ctx, func(st *domain.State) error {
		for _, env := range st.Environments {
			if reservationFree(st, env.ID) != nil {
				continue
			}
			if env.DesiredCompositionID == "" {
				continue
			}
			composition := compositionByID(st, env.DesiredCompositionID)
			if composition == nil || !composition.AutoRefresh || composition.ProductID != env.ProductID {
				continue
			}
			parent := operationByID(st, env.DesiredOperationID)
			if parent == nil || parent.Kind != "COMPOSE" || !sourceRetentionRefreshAllowed(st, *parent) {
				continue
			}
			compositionTarget := false
			for _, target := range st.EnvironmentBindings {
				if target.EnvironmentID == env.ID && target.AllowDeploy && (target.Purpose == "DEV" || target.Purpose == "TEST") {
					compositionTarget = true
					break
				}
			}
			if !compositionTarget || len(composition.RevisionSnapshots) == 0 {
				continue
			}
			selected := selectedIntegrations(*composition)
			activeSelection := true
			for _, iid := range selected {
				in := integration(st, iid)
				if in == nil || in.Status == "released" {
					activeSelection = false
					break
				}
			}
			if !activeSelection {
				continue
			}
			stale := false
			for _, snapshot := range composition.RevisionSnapshots {
				for n := len(st.IntegrationRevisions) - 1; n >= 0; n-- {
					latest := st.IntegrationRevisions[n]
					if latest.IntegrationID == snapshot.IntegrationID && latest.RepositoryID == snapshot.RepositoryID {
						if latest.ID != snapshot.ID {
							stale = true
						}
						break
					}
				}
				if stale {
					break
				}
			}
			if !stale {
				continue
			}
			pending := false
			for _, operation := range st.Operations {
				if operation.Kind == "COMPOSE" && operation.EnvironmentID == env.ID && !terminalOperation(operation.Status) {
					pending = true
					break
				}
			}
			if pending {
				continue
			}
			command := domain.Command{Action: "assemble_environment", Actor: autoComposeActor, ProductID: env.ProductID, Data: map[string]any{
				"environment_id":     env.ID,
				"name":               composition.Name + " (refresh)",
				"integration_ids":    selected,
				"expected_selection": selectionToken(env),
			}}
			commands = append(commands, refreshCommand{command: command, origin: composeRefreshOrigin{Selection: selectionToken(env), ParentID: parent.ID}})
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, refresh := range commands {
		_, err := s.executeCommand(ctx, refresh.command, &refresh.origin)
		if err != nil && !errors.Is(err, ErrSelectionChanged) {
			return err
		}
	}
	return nil
}

func (s *Service) RunGitScheduler(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return nil
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		if err := s.ScheduleGitRefresh(ctx, time.Now().UTC(), interval); err != nil {
			return err
		}
		if err := s.ScheduleRepositoryGitRefresh(ctx, time.Now().UTC(), interval); err != nil {
			return err
		}
		if err := s.ScheduleCompositionRefresh(ctx, time.Now().UTC(), interval); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
