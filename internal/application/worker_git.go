package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"releasecontrol/internal/delivery"
	"strconv"
	"strings"
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
	automatic := op.RequestedBy == autoGitActor
	if automatic && !autoGitEligible(&st, *in) {
		return s.finish(ctx, op, token, "SUCCEEDED", "Automatic Git scope is no longer active", nil)
	}
	work := *in
	work.PullRequests = append([]domain.PullRequest{}, in.PullRequests...)
	in = &work
	if automatic {
		open := []domain.PullRequest{}
		for _, p := range in.PullRequests {
			if strings.EqualFold(p.Status, "open") || strings.EqualFold(p.Status, "draft") {
				open = append(open, p)
			}
		}
		in.PullRequests = open
	}
	branches := append([]domain.Branch{}, in.Branches...)
	seen := map[string]bool{}
	for _, b := range branches {
		seen[b.RepositoryID+"/"+b.Name] = true
	}
	for _, r := range st.IntegrationRevisions {
		key := r.RepositoryID + "/" + r.Branch
		if !automatic && r.IntegrationID == in.ID && !seen[key] {
			branches = append(branches, domain.Branch{RepositoryID: r.RepositoryID, Name: r.Branch})
			seen[key] = true
		}
	}
	if len(branches) == 0 && len(in.PullRequests) == 0 {
		return s.finish(ctx, op, token, "BLOCKED", "link a branch, pull request, or capture an integration revision first", nil)
	}
	if scoped, ok := s.provider.(delivery.ScopedGitObserver); ok {
		for _, b := range branches {
			binding := repositoryBinding(&st, b.RepositoryID)
			if binding == nil || binding.ProductID != in.ProductID {
				continue
			}
			conn, err := scopedConnection(&st, binding.ConnectionID, in.ProductID)
			if err != nil {
				return s.finish(ctx, op, token, "BLOCKED", err.Error(), nil)
			}
			found, err := scoped.DiscoverBranchPullRequests(ctx, conn.Config, repositoryLocator(&st, *binding), b.Name)
			if err != nil {
				return s.finish(ctx, op, token, "FAILED", "branch PR discovery failed: "+err.Error(), nil)
			}
			for _, p := range found {
				if automatic {
					closedKnown := false
					for _, old := range integration(&st, in.ID).PullRequests {
						if old.RepositoryID == b.RepositoryID && old.ID == strconv.Itoa(p.Number) && (strings.EqualFold(old.Status, "merged") || strings.EqualFold(old.Status, "closed")) {
							closedKnown = true
						}
					}
					if closedKnown {
						continue
					}
				}
				known := false
				for _, old := range in.PullRequests {
					if old.RepositoryID == b.RepositoryID && old.ID == strconv.Itoa(p.Number) {
						known = true
					}
				}
				if !known {
					in.PullRequests = append(in.PullRequests, domain.PullRequest{RepositoryID: b.RepositoryID, ID: strconv.Itoa(p.Number), URL: p.URL, Status: strings.ToLower(p.State)})
				}
			}
		}
	}
	prs := []GraphPullRequest{}
	if len(in.PullRequests) > 0 {
		provider, ok := s.provider.(delivery.PullRequestObserver)
		if !ok {
			return s.finish(ctx, op, token, "BLOCKED", "provider does not support pull request observation", nil)
		}
		for _, pr := range in.PullRequests {
			binding := repositoryBinding(&st, pr.RepositoryID)
			if binding == nil || binding.ProductID != in.ProductID {
				return s.finish(ctx, op, token, "BLOCKED", "pull request repository has no imported provider binding", nil)
			}
			conn, err := scopedConnection(&st, binding.ConnectionID, in.ProductID)
			if err != nil {
				return s.finish(ctx, op, token, "BLOCKED", err.Error(), nil)
			}
			number, err := strconv.Atoi(pr.ID)
			if err != nil || number <= 0 {
				return s.finish(ctx, op, token, "BLOCKED", "linked pull request requires a positive numeric ID", nil)
			}
			v, err := provider.ObservePullRequest(ctx, conn.Config, repositoryLocator(&st, *binding), number)
			if err != nil {
				return s.finish(ctx, op, token, "FAILED", fmt.Sprintf("pull request %s observation failed: %v", pr.ID, err), nil)
			}
			if v.Number != number || v.Head == "" || v.Base == "" || v.HeadSHA == "" {
				return s.finish(ctx, op, token, "FAILED", "incomplete or mismatched pull request observation", nil)
			}
			prs = append(prs, GraphPullRequest{ID: pr.ID, URL: pr.URL, RepositoryID: pr.RepositoryID, Title: v.Title, Status: v.State, SourceBranch: v.Head, TargetBranch: v.Base, HeadSHA: v.HeadSHA, MergeSHA: v.MergeSHA, CreatedAt: gitTime(v.CreatedAt), MergedAt: gitTime(v.MergedAt), ObservedAt: gitTime(time.Now().UTC()), Evidence: "provider"})
		}
	}
	observations := []domain.GitObservation{}
	deletedBranches := 0
	for _, b := range branches {
		binding := repositoryBinding(&st, b.RepositoryID)
		if binding == nil || binding.ProductID != in.ProductID {
			return s.finish(ctx, op, token, "BLOCKED", "branch repository has no imported provider binding", nil)
		}
		conn, e := scopedConnection(&st, binding.ConnectionID, in.ProductID)
		if e != nil {
			return s.finish(ctx, op, token, "BLOCKED", e.Error(), nil)
		}
		var head, main string
		if scoped, ok := s.provider.(delivery.ScopedGitObserver); ok {
			v, err := scoped.ObserveBranch(ctx, conn.Config, repositoryLocator(&st, *binding), b.Name)
			if err != nil {
				if b.Name != configuredBase(*binding) && errors.Is(err, delivery.ErrNotFound) {
					deletedBranches++
					continue
				}
				closed := false
				for _, p := range prs {
					if p.RepositoryID == b.RepositoryID && p.SourceBranch == b.Name && (p.Status == "MERGED" || p.Status == "CLOSED") {
						closed = true
					}
				}
				if closed {
					continue
				}
				return s.finish(ctx, op, token, "FAILED", "branch observation failed: "+err.Error(), nil)
			}
			head = v.SHA
			if b.Name == configuredBase(*binding) {
				main = head
			} else {
				base, err := scoped.ObserveBranch(ctx, conn.Config, repositoryLocator(&st, *binding), configuredBase(*binding))
				if err != nil {
					return s.finish(ctx, op, token, "FAILED", "base observation failed: "+err.Error(), nil)
				}
				main = base.SHA
			}
		} else {
			if automatic {
				return s.finish(ctx, op, token, "BLOCKED", "provider lacks scoped branch reads", nil)
			}
			list, err := s.provider.Branches(ctx, conn.Config, repositoryLocator(&st, *binding))
			if err != nil {
				return s.finish(ctx, op, token, "FAILED", "branch observation failed: "+err.Error(), nil)
			}
			for _, v := range list {
				if v.Name == b.Name {
					head = v.SHA
				}
				if v.Name == configuredBase(*binding) {
					main = v.SHA
				}
			}
		}
		if head == "" && b.Name != configuredBase(*binding) {
			deletedBranches++
			continue
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
		if len(prs) > 0 {
			now := time.Now().UTC()
			body, err := json.Marshal(prs)
			if err != nil {
				return err
			}
			st.Memories = append(st.Memories, domain.Memory{Meta: domain.Meta{ID: id(), ProductID: in.ProductID, FeatureID: in.FeatureID, Actor: "worker", CreatedAt: now, UpdatedAt: now}, IntegrationID: in.ID, Kind: "progress", Title: "GitHub pull requests synchronized", Body: string(body), Session: op.ID})
			live := integration(st, in.ID)
			if live == nil {
				return invalid("integration removed during observation")
			}
			if live.Status != "released" {
				for _, p := range prs {
					known := false
					for _, old := range live.PullRequests {
						if old.RepositoryID == p.RepositoryID && old.ID == p.ID {
							known = true
						}
					}
					if !known {
						live.PullRequests = append(live.PullRequests, domain.PullRequest{RepositoryID: p.RepositoryID, ID: p.ID, URL: p.URL, Status: strings.ToLower(p.Status)})
					}
				}

				for n := range live.PullRequests {
					for _, p := range prs {
						if live.PullRequests[n].RepositoryID == p.RepositoryID && live.PullRequests[n].ID == p.ID && live.PullRequests[n].URL == p.URL {
							live.PullRequests[n].Status = strings.ToLower(p.Status)
						}
					}
				}
			}
		}
		st.GitObservations = append(st.GitObservations, observations...)
		return nil
	})
	if e != nil {
		return e
	}
	detail := fmt.Sprintf("Observed %d branches and %d pull requests without modifying source history", len(observations), len(prs))
	if deletedBranches > 0 {
		detail += fmt.Sprintf("; skipped %d deleted branch(es)", deletedBranches)
	}
	return s.finish(ctx, op, token, "SUCCEEDED", detail, map[string]string{"branches": strconv.Itoa(len(observations)), "pull_requests": strconv.Itoa(len(prs)), "deleted_branches": strconv.Itoa(deletedBranches)})
}

func gitTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func isDeletedBranchObservation(detail string) bool {
	detail = strings.ToLower(detail)
	return strings.Contains(detail, "branch observation failed:") &&
		(strings.Contains(detail, "http 404") || strings.Contains(detail, "not found"))
}
