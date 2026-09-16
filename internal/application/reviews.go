package application

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
)

var reviewPriority = regexp.MustCompile(`(?i)\[P([12])(?: Badge)?\]`)

func priority(body string) string {
	matches := reviewPriority.FindAllStringSubmatch(body, -1)
	p := ""
	for _, m := range matches {
		if p == "" || m[1] < p {
			p = m[1]
		}
	}
	if p != "" {
		return "P" + p
	}
	return ""
}
func (s *Service) SyncReview(ctx context.Context, actor, integrationID, repositoryID string, number int, commentKeys []string, preview bool) (any, error) {
	if strings.TrimSpace(actor) == "" || number <= 0 {
		return nil, invalid("actor and positive pull_request required")
	}
	provider, ok := s.provider.(delivery.ReviewProvider)
	if !ok {
		return nil, invalid("review provider not configured")
	}
	st, err := s.State(ctx)
	if err != nil {
		return nil, err
	}
	in := integration(&st, integrationID)
	binding := repositoryBinding(&st, repositoryID)
	if in == nil || binding == nil || binding.ProductID != in.ProductID || binding.Role != "SOURCE" {
		return nil, invalid("integration and SOURCE repository must belong to product")
	}
	f := feature(&st, in.FeatureID)
	if (len(in.Repositories) > 0 && !slices.Contains(in.Repositories, repositoryID)) || (len(f.Repositories) > 0 && !slices.Contains(f.Repositories, repositoryID)) {
		return nil, invalid("repository outside feature/integration scope")
	}
	conn, err := scopedConnection(&st, binding.ConnectionID, in.ProductID)
	if err != nil {
		return nil, err
	}
	observed, err := provider.PullRequestReview(ctx, conn.Config, repositoryLocator(&st, *binding), number)
	if err != nil {
		return nil, invalid("review synchronization failed: %v", err)
	}
	candidates := []map[string]any{}
	knownKeys := map[string]bool{}
	for _, comment := range observed.Comments {
		if p := priority(comment.Body); p != "" {
			key := fmt.Sprintf("%s:%d", comment.Kind, comment.ID)
			knownKeys[key] = true
			candidates = append(candidates, map[string]any{"key": key, "priority": p, "comment": comment})
		}
	}
	if preview {
		return map[string]any{"review": observed, "candidates": candidates, "review_completion": "UNKNOWN"}, nil
	}
	for _, key := range commentKeys {
		if !knownKeys[key] {
			return nil, invalid("selected comment not present in current review: %s", key)
		}
	}
	headBound := false
	for _, branch := range in.Branches {
		if branch.RepositoryID == repositoryID && branch.Name == observed.Head {
			headBound = true
		}
	}
	if !headBound && len(commentKeys) == 0 {
		return nil, invalid("shared or unbound PR requires preview and explicit comment_keys to avoid attributing unrelated findings")
	}
	now := time.Now().UTC()
	sync := domain.ReviewSync{Meta: domain.Meta{ID: id(), ProductID: in.ProductID, FeatureID: in.FeatureID, Actor: actor, CreatedAt: now, UpdatedAt: now}, IntegrationID: in.ID, RepositoryID: repositoryID, Review: observed, Status: "NO_FINDINGS_OBSERVED"}
	imported := []string{}
	err = s.store.Update(ctx, func(current *domain.State) error {
		currentIn := integration(current, integrationID)
		if currentIn == nil || currentIn.FeatureID != in.FeatureID {
			return invalid("integration changed during sync")
		}
		currentFeature := feature(current, currentIn.FeatureID)
		if (len(currentIn.Repositories) > 0 && !slices.Contains(currentIn.Repositories, repositoryID)) || (len(currentFeature.Repositories) > 0 && !slices.Contains(currentFeature.Repositories, repositoryID)) {
			return invalid("repository scope changed during sync")
		}
		for _, comment := range observed.Comments {
			if len(commentKeys) > 0 && !slices.Contains(commentKeys, fmt.Sprintf("%s:%d", comment.Kind, comment.ID)) {
				continue
			}
			p := priority(comment.Body)
			if p == "" {
				continue
			}
			sync.Status = "FINDINGS_OBSERVED"
			var existing *domain.Finding
			for i := range current.Findings {
				candidate := &current.Findings[i]
				r := candidate.ReviewSource
				if candidate.FeatureID == in.FeatureID && r != nil && r.RepositoryID == repositoryID && r.PullRequest == number && r.Comment.Kind == comment.Kind && r.Comment.ID == comment.ID {
					existing = candidate
					break
				}
			}
			if existing != nil {
				if !slices.Contains(existing.IntegrationIDs, integrationID) {
					existing.IntegrationIDs = append(existing.IntegrationIDs, integrationID)
				}
				// A changed comment is fresh review evidence. Old copies remain in ReviewSyncs.
				if existing.ReviewSource.Comment.Body != comment.Body && !comment.UpdatedAt.Before(existing.ReviewSource.Comment.UpdatedAt) {
					existing.Title = fmt.Sprintf("Codex %s · PR #%d · %s", p, number, comment.Path)
					existing.Body = comment.Body
					existing.Status = "open"
					existing.Resolution = ""
					existing.FixCommit = ""
					existing.UpdatedAt = now
					existing.ReviewSource.Comment = comment
					existing.ReviewSource.Priority = p
					existing.Severity = p
				}
				imported = append(imported, existing.ID)
				continue
			}
			finding := domain.Finding{Meta: domain.Meta{ID: id(), ProductID: in.ProductID, FeatureID: in.FeatureID, Actor: actor, CreatedAt: now, UpdatedAt: now}, Title: fmt.Sprintf("Codex %s · PR #%d · %s", p, number, comment.Path), Severity: p, Body: comment.Body, Status: "open", IntegrationIDs: []string{in.ID}, GateIDs: []string{}, BlocksRelease: true, ReviewSource: &domain.ReviewSource{RepositoryID: repositoryID, PullRequest: number, Comment: comment, Priority: p}}
			current.Findings = append(current.Findings, finding)
			imported = append(imported, finding.ID)
		}
		current.ReviewSyncs = append(current.ReviewSyncs, sync)
		current.Events = append(current.Events, domain.Event{ID: id(), Action: "sync_pull_request_review", Actor: actor, At: now, EntityID: sync.ID, ProductID: in.ProductID, FeatureID: in.FeatureID, Data: domain.Command{Action: "sync_pull_request_review", Actor: actor, IntegrationID: in.ID, Data: map[string]any{"review_sync_id": sync.ID, "finding_ids": imported, "repository_id": repositoryID, "pull_request": number}}})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"sync": sync, "finding_ids": imported, "review_completion": "UNKNOWN"}, nil
}
