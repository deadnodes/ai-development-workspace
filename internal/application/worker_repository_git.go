package application

import (
	"context"
	"fmt"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"strings"
	"time"
)

// observeRepositoryGit inventories every non-default branch of one attached
// code repository. A branch is associated with an Integration only when the
// branch binding is explicit; otherwise its commits remain unlinked evidence
// for an agent to review and attach later.
func (s *Service) observeRepositoryGit(ctx context.Context, op domain.ExternalOperation, token string) error {
	st, err := s.State(ctx)
	if err != nil {
		return err
	}
	binding := repositoryBinding(&st, op.RepositoryID)
	if binding == nil || binding.ProductID != op.ProductID || !repositoryGitInventoryRole(binding.Role) {
		return s.finish(ctx, op, token, "BLOCKED", "attached source repository missing", nil)
	}
	conn, err := scopedConnection(&st, binding.ConnectionID, op.ProductID)
	if err != nil {
		return s.finish(ctx, op, token, "BLOCKED", err.Error(), nil)
	}
	branches, err := s.provider.Branches(ctx, conn.Config, repositoryLocator(&st, *binding))
	if err != nil {
		return s.finish(ctx, op, token, "FAILED", "repository branch discovery failed: "+err.Error(), nil)
	}
	baseName := configuredBase(*binding)
	var base delivery.BranchInfo
	for _, branch := range branches {
		if branch.Name == baseName {
			base = branch
			break
		}
	}
	if base.Name == "" || base.SHA == "" {
		return s.finish(ctx, op, token, "BLOCKED", "configured base branch is missing", nil)
	}
	linked := map[string]string{}
	for _, in := range st.Integrations {
		if in.ProductID != op.ProductID {
			continue
		}
		for _, branch := range in.Branches {
			if branch.RepositoryID == op.RepositoryID {
				linked[branch.Name] = in.ID
			}
		}
	}
	now := time.Now().UTC()
	observations := make([]domain.GitObservation, 0, len(branches))
	for _, branch := range branches {
		if branch.Name == baseName || branch.SHA == "" {
			continue
		}
		comparison, compareErr := s.provider.Compare(ctx, conn.Config, repositoryLocator(&st, *binding), base.SHA, branch.SHA)
		if compareErr != nil {
			return s.finish(ctx, op, token, "FAILED", fmt.Sprintf("compare %s failed: %v", branch.Name, compareErr), nil)
		}
		integrationID := linked[branch.Name]
		featureID := ""
		if integrationID != "" {
			if in := integration(&st, integrationID); in != nil {
				featureID = in.FeatureID
			}
		}
		meta := domain.Meta{ID: id(), ProductID: op.ProductID, FeatureID: featureID, Actor: "worker/git-inventory", CreatedAt: now, UpdatedAt: now}
		observations = append(observations, domain.GitObservation{Meta: meta, IntegrationID: integrationID, RepositoryID: op.RepositoryID, Branch: branch.Name, HeadCommit: branch.SHA, MainCommit: base.SHA, Ahead: comparison.Ahead, Behind: comparison.Behind, Status: comparison.Status, Commits: comparison.Commits, ObservedAt: now})
	}
	if err := s.store.Update(ctx, func(current *domain.State) error {
		lease := operationByID(current, op.ID)
		if lease == nil || lease.LeaseOwner != token {
			return invalid("operation lease lost")
		}
		for _, observation := range observations {
			duplicate := false
			for _, old := range current.GitObservations {
				if old.ProductID == observation.ProductID && old.RepositoryID == observation.RepositoryID && old.Branch == observation.Branch && old.HeadCommit == observation.HeadCommit && old.MainCommit == observation.MainCommit {
					duplicate = true
					break
				}
			}
			if !duplicate {
				current.GitObservations = append(current.GitObservations, observation)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	unlinked := 0
	for _, observation := range observations {
		if observation.IntegrationID == "" {
			for _, commit := range observation.Commits {
				if strings.TrimSpace(commit.SHA) != "" {
					unlinked++
				}
			}
		}
	}
	return s.finish(ctx, op, token, "SUCCEEDED", fmt.Sprintf("Scanned %d branches; recorded %d unlinked commits", len(observations), unlinked), map[string]string{"branches": fmt.Sprintf("%d", len(observations)), "unlinked_commits": fmt.Sprintf("%d", unlinked)})
}
