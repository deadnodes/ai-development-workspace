package application

import (
	"context"
	"releasecontrol/internal/domain"
	"strings"
	"time"
)

const autoGitActor = "system/git-sync"

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
			if queued >= 5 {
				break
			}
		}
		return nil
	})
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
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
