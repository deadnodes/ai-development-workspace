package application

import (
	"context"
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
func (s *Service) ScheduleGitRefresh(ctx context.Context, now time.Time, interval time.Duration) error {
	if interval <= 0 || s.provider == nil {
		return nil
	}
	return s.store.Update(ctx, func(st *domain.State) error {
		queued := 0
		active := 0
		for _, op := range st.Operations {
			if op.Kind == "REFRESH_GIT" && !terminalOperation(op.Status) {
				active++
			}
		}
		if active >= 5 {
			return nil
		}
		for _, in := range st.Integrations {
			if !autoGitEligible(st, in) {
				continue
			}
			var last *domain.ExternalOperation
			pending := false
			for i := range st.Operations {
				o := &st.Operations[i]
				if o.Kind != "REFRESH_GIT" || o.IntegrationID != in.ID {
					continue
				}
				if !terminalOperation(o.Status) {
					pending = true
				}
				if last == nil || o.CreatedAt.After(last.CreatedAt) {
					last = o
				}
			}
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
			m := domain.Meta{ID: id(), ProductID: in.ProductID, FeatureID: in.FeatureID, Actor: autoGitActor, CreatedAt: now, UpdatedAt: now}
			st.Operations = append(st.Operations, domain.ExternalOperation{Meta: m, Kind: "REFRESH_GIT", IntegrationID: in.ID, Status: "PENDING", RequestedBy: autoGitActor, Phase: "OBSERVE", NextAttemptAt: now})
			st.Events = append(st.Events, domain.Event{ID: id(), Action: "refresh_integration_git", Actor: autoGitActor, At: now, EntityID: m.ID, ProductID: in.ProductID, FeatureID: in.FeatureID, Data: domain.Command{Action: "refresh_integration_git", Actor: autoGitActor, IntegrationID: in.ID}})
			queued++
			active++
			if queued >= 5 || active >= 5 {
				break
			}
		}
		return nil
	})
}

// ScheduleRepositoryGitRefresh keeps the repository registry observable even
// when no Integration has been linked yet. It is deliberately bounded: one
// repository scan is a durable operation and the worker performs the provider
// calls outside the state transaction.
func (s *Service) ScheduleRepositoryGitRefresh(ctx context.Context, now time.Time, interval time.Duration) error {
	if interval <= 0 || s.provider == nil {
		return nil
	}
	return s.store.Update(ctx, func(st *domain.State) error {
		queued := 0
		active := 0
		for _, op := range st.Operations {
			if op.Kind == "REFRESH_REPOSITORY_GIT" && !terminalOperation(op.Status) {
				active++
			}
		}
		if active >= 3 {
			return nil
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
			var last *domain.ExternalOperation
			pending := false
			for i := range st.Operations {
				op := &st.Operations[i]
				if op.Kind != "REFRESH_REPOSITORY_GIT" || op.ProductID != binding.ProductID || op.RepositoryID != binding.RepositoryID {
					continue
				}
				if !terminalOperation(op.Status) {
					pending = true
				}
				if last == nil || op.CreatedAt.After(last.CreatedAt) {
					last = op
				}
			}
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
			m := domain.Meta{ID: id(), ProductID: binding.ProductID, Actor: autoGitActor, CreatedAt: now, UpdatedAt: now}
			st.Operations = append(st.Operations, domain.ExternalOperation{Meta: m, Kind: "REFRESH_REPOSITORY_GIT", RepositoryID: binding.RepositoryID, Status: "PENDING", RequestedBy: autoGitActor, Phase: "SCAN", NextAttemptAt: now})
			st.Events = append(st.Events, domain.Event{ID: id(), Action: "refresh_repository_git", Actor: autoGitActor, At: now, EntityID: m.ID, ProductID: binding.ProductID, Data: domain.Command{Action: "refresh_repository_git", Actor: autoGitActor, ProductID: binding.ProductID, Data: map[string]any{"repository_id": binding.RepositoryID}}})
			queued++
			active++
			if queued >= 3 || active >= 3 {
				break
			}
		}
		return nil
	})
}

// ScheduleCompositionRefresh rematerializes a selected DEV/TEST composition
// when background Git observation captures a newer revision. The operation is
// deliberately bounded to one environment per tick and never touches PROD.
func (s *Service) ScheduleCompositionRefresh(ctx context.Context, now time.Time, interval time.Duration) error {
	if interval <= 0 || s.provider == nil {
		return nil
	}
	st, err := s.State(ctx)
	if err != nil {
		return err
	}
	for _, env := range st.Environments {
		if env.DesiredCompositionID == "" {
			continue
		}
		composition := compositionByID(&st, env.DesiredCompositionID)
		if composition == nil || composition.ProductID != env.ProductID {
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
			in := integration(&st, iid)
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
		_, err := s.Execute(ctx, domain.Command{Action: "assemble_environment", Actor: autoComposeActor, ProductID: env.ProductID, Data: map[string]any{
			"environment_id":  env.ID,
			"name":            composition.Name + " (refresh)",
			"integration_ids": selected,
		}})
		return err
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
