package application

import (
	"context"
	"time"

	"releasecontrol/internal/domain"
)

func (s *Service) observeGit(ctx context.Context, op domain.ExternalOperation, token string) error {
	st, e := s.State(ctx)
	if e != nil {
		return e
	}
	in := integration(&st, op.IntegrationID)
	if in == nil {
		return s.finish(ctx, op, token, "FAILED", "integration missing", nil)
	}
	branches := append([]domain.Branch{}, in.Branches...)
	seen := map[string]bool{}
	for _, b := range branches {
		seen[b.RepositoryID+"/"+b.Name] = true
	}
	for _, r := range st.IntegrationRevisions {
		key := r.RepositoryID + "/" + r.Branch
		if r.IntegrationID == in.ID && !seen[key] {
			branches = append(branches, domain.Branch{RepositoryID: r.RepositoryID, Name: r.Branch})
			seen[key] = true
		}
	}
	if len(branches) == 0 {
		return s.finish(ctx, op, token, "BLOCKED", "link a branch or capture an integration revision first", nil)
	}
	observations := []domain.GitObservation{}
	for _, b := range branches {
		binding := repositoryBinding(&st, b.RepositoryID)
		if binding == nil || binding.ProductID != in.ProductID {
			return s.finish(ctx, op, token, "BLOCKED", "branch repository has no imported provider binding", nil)
		}
		conn, e := scopedConnection(&st, binding.ConnectionID, in.ProductID)
		if e != nil {
			return s.finish(ctx, op, token, "BLOCKED", e.Error(), nil)
		}
		list, e := s.provider.Branches(ctx, conn.Config, repositoryLocator(&st, *binding))
		if e != nil {
			return s.finish(ctx, op, token, "FAILED", "branch observation failed: "+e.Error(), nil)
		}
		var head, main string
		for _, v := range list {
			if v.Name == b.Name {
				head = v.SHA
			}
			if v.Name == configuredBase(*binding) {
				main = v.SHA
			}
		}
		if head == "" || main == "" {
			return s.finish(ctx, op, token, "BLOCKED", "source or default branch missing", nil)
		}
		comparison, e := s.provider.Compare(ctx, conn.Config, repositoryLocator(&st, *binding), main, head)
		if e != nil {
			return s.finish(ctx, op, token, "FAILED", "compare failed: "+e.Error(), nil)
		}
		now := time.Now().UTC()
		observations = append(observations, domain.GitObservation{Commits: comparison.Commits, Meta: domain.Meta{ID: id(), ProductID: in.ProductID, FeatureID: in.FeatureID, Actor: "worker", CreatedAt: now, UpdatedAt: now}, IntegrationID: in.ID, RepositoryID: b.RepositoryID, Branch: b.Name, HeadCommit: head, MainCommit: main, Ahead: comparison.Ahead, Behind: comparison.Behind, Status: comparison.Status, ObservedAt: now})
	}
	e = s.store.Update(ctx, func(st *domain.State) error {
		current := operationByID(st, op.ID)
		if current == nil || current.LeaseOwner != token {
			return invalid("operation lease lost")
		}
		for _, observation := range observations {
			exists := false
			for _, r := range st.IntegrationRevisions {
				if r.IntegrationID == op.IntegrationID && r.RepositoryID == observation.RepositoryID && r.HeadCommit == observation.HeadCommit && r.Branch == observation.Branch {
					exists = true
				}
			}
			if !exists {
				rm := observation.Meta
				rm.ID = id()
				commits := []string{}
				for _, commit := range observation.Commits {
					commits = append(commits, commit.SHA)
				}
				if len(commits) == 0 {
					commits = []string{observation.HeadCommit}
				}
				st.IntegrationRevisions = append(st.IntegrationRevisions, domain.IntegrationRevision{Meta: rm, IntegrationID: op.IntegrationID, RepositoryID: observation.RepositoryID, Branch: observation.Branch, BaseCommit: observation.MainCommit, HeadCommit: observation.HeadCommit, Commits: commits})
			}
		}
		st.GitObservations = append(st.GitObservations, observations...)
		return nil
	})
	if e != nil {
		return e
	}
	return s.finish(ctx, op, token, "SUCCEEDED", "Git branches observed without modifying source history", nil)
}
