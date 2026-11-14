package application

import (
	"context"
	"regexp"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/providers/forgejo"
	"slices"
	"strings"
	"sync"
	"time"
)

// Additional read-only source capabilities; adapters without them retain the main gate.
type prTreeSource interface {
	CommitPullRequest(context.Context, buildconfig.Connection, string, string) (forgejo.PR, bool, error)
	TreesEqual(context.Context, buildconfig.Connection, string, string, string) (bool, error)
}

func (s *Service) acceptPRTree(ctx context.Context, d *domain.Deployment, p buildconfig.Profile) error {
	if p.CIGate != nil && !p.CIGate.AcceptPRTree {
		return nil
	}
	source, ok := s.deployments.source.(prTreeSource)
	if !ok {
		return nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	cn := s.builds.config.Connection(p.ConnectionID)
	pr, found, err := source.CommitPullRequest(checkCtx, cn, p.Repository, d.SourceSHA)
	if err != nil {
		return err
	}
	// The profile default branch is the deployed source branch; DispatchRef only
	// selects the build workflow version and may intentionally differ.
	if !found || !pr.Merged || pr.BaseRef != p.DefaultBranch || pr.Number <= 0 || !buildconfig.SHA.MatchString(pr.HeadSHA) {
		return nil
	}
	equal, err := source.TreesEqual(checkCtx, cn, p.Repository, d.SourceSHA, pr.HeadSHA)
	if err != nil || !equal {
		return err
	}
	required := make([]string, len(p.RequiredStatusContexts))
	for i, key := range p.RequiredStatusContexts {
		required[i] = key
		if strings.HasSuffix(key, " (push)") {
			required[i] = strings.TrimSuffix(key, " (push)") + " (pull_request)"
		}
	}
	status, err := s.deployments.source.CommitStatus(checkCtx, cn, p.Repository, pr.HeadSHA)
	if err != nil {
		return err
	}
	green, _, contexts := ciVerdict(trustedCIStatus(status, p), required)
	if green {
		d.CIEvidence, d.CIPRNumber, d.CIPRHeadSHA = "pr_tree", pr.Number, pr.HeadSHA
		d.CIState, d.CIContexts = "success", contexts
		d.MainCIState = "pending"
		d.MainCICheckedAt = copyTime(d.CheckedAt)
	}
	return nil
}

// ciContextState recognizes only Forgejo's exact cancellation marker.
func ciContextState(s forgejo.StatusContext) string {
	if s.Status == "failure" && s.Description == "Has been cancelled" {
		return "cancelled"
	}
	return s.Status
}

// trustedCIStatus filters before context deduplication, preserving the newest
// trusted observation even if a newer user status shadows that context.
func trustedCIStatus(v forgejo.CommitStatus, p buildconfig.Profile) forgejo.CommitStatus {
	if p.CIGate != nil && p.CIGate.TrustedProducers == "any" {
		return v
	}
	pattern := regexp.MustCompile("^/(?i:" + regexp.QuoteMeta(p.Repository) + ")/actions/runs/[0-9]+/jobs/[0-9]+$")
	out := forgejo.CommitStatus{State: "pending"}
	for _, status := range v.Statuses {
		if status.Creator == nil && pattern.MatchString(status.TargetURL) {
			out.Statuses = append(out.Statuses, status)
		}
	}
	slices.SortStableFunc(out.Statuses, func(a, b forgejo.StatusContext) int {
		if a.ID > b.ID {
			return -1
		}
		if a.ID < b.ID {
			return 1
		}
		return 0
	})
	// Without required contexts, derive the aggregate from trusted contexts only.
	if len(out.Statuses) > 0 {
		out.State = "success"
		seen := map[string]bool{}
		for _, status := range out.Statuses {
			if seen[status.Context] {
				continue
			}
			seen[status.Context] = true
			if ciContextState(status) == "failure" || ciContextState(status) == "error" {
				out.State = "failure"
				break
			}
			if ciContextState(status) != "success" {
				out.State = "pending"
			}
		}
	}
	return out
}

// watchMainCI observes even terminal deployment handles. Reserve the polling
// timestamp before I/O to enforce the minute bound across retries and restarts.
// Both provider I/O and snapshots run outside Store.Update callbacks.
func (s *Service) watchMainCI(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	st, err := s.deploymentSnapshot(ctx)
	if err != nil {
		return err
	}
	slices.SortStableFunc(st.Deployments, func(a, b domain.Deployment) int {
		if a.MainCICheckedAt == nil {
			if b.MainCICheckedAt == nil {
				return 0
			}
			return -1
		}
		if b.MainCICheckedAt == nil {
			return 1
		}
		return a.MainCICheckedAt.Compare(*b.MainCICheckedAt)
	})
	type observation struct {
		d      domain.Deployment
		p      buildconfig.Profile
		status forgejo.CommitStatus
		err    error
	}
	results := make(chan observation, len(st.Deployments))
	slots := make(chan struct{}, 4)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	for _, d := range st.Deployments {
		if checkCtx.Err() != nil {
			break
		}
		select {
		case slots <- struct{}{}:
		case <-checkCtx.Done():
			break
		}
		if checkCtx.Err() != nil {
			break
		}
		now := time.Now().UTC()
		if d.CIEvidence != "pr_tree" || d.MergedAt == nil || !now.Before(d.MergedAt.Add(3*time.Hour)) || d.MainCIState == "success" || d.MainCIState == "failure" || d.MainCIState == "cancelled" || d.MainCICheckedAt != nil && now.Sub(*d.MainCICheckedAt) < time.Minute {
			<-slots
			continue
		}
		p, ok := s.builds.config.Profile(d.ProfileID)
		if !ok || p.ProductID != d.ProductID || !s.builds.deploymentAuthorityMatches(d.ProfileID, d.AuthorityHash) {
			<-slots
			continue
		}
		reserved := false
		err = s.store.Update(ctx, func(st *domain.State) error {
			for i := range st.Deployments {
				current := &st.Deployments[i]
				if current.ID == d.ID && buildFingerprint(*current) == buildFingerprint(d) {
					current.MainCICheckedAt = &now
					d = copyDeployment(*current)
					reserved = true
					break
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if !reserved {
			<-slots
			continue
		}
		workers.Add(1)
		go func(d domain.Deployment, p buildconfig.Profile) {
			defer workers.Done()
			defer func() { <-slots }()
			pollCtx, pollCancel := context.WithTimeout(checkCtx, 10*time.Second)
			defer pollCancel()
			pollCtx, expiryCancel := context.WithDeadline(pollCtx, d.MergedAt.Add(3*time.Hour))
			defer expiryCancel()
			status, e := s.deployments.source.CommitStatus(pollCtx, s.builds.config.Connection(p.ConnectionID), p.Repository, d.SourceSHA)
			results <- observation{d, p, status, e}
		}(d, p)
	}
	workers.Wait()
	close(results)
	for result := range results {
		d, p, status, e := result.d, result.p, result.status, result.err
		now := *d.MainCICheckedAt
		if e != nil {
			continue
		}
		green, failed, contexts := ciVerdict(trustedCIStatus(status, p), p.RequiredStatusContexts)
		state := "pending"
		if failed {
			state = "failure"
		} else if green {
			state = "success"
		} else {
			for key, value := range contexts {
				if value == "cancelled" && (len(p.RequiredStatusContexts) == 0 || slices.Contains(p.RequiredStatusContexts, key)) {
					state = "cancelled"
					break
				}
			}
		}
		err = s.store.Update(ctx, func(st *domain.State) error {
			for i := range st.Deployments {
				current := &st.Deployments[i]
				if current.ID != d.ID || buildFingerprint(*current) != buildFingerprint(d) {
					continue
				}
				current.MainCIState = state
				if failed {
					failures := map[string]string{}
					for key, value := range contexts {
						if (len(p.RequiredStatusContexts) == 0 || slices.Contains(p.RequiredStatusContexts, key)) && (value == "failure" || value == "error") {
							failures[key] = value
						}
					}
					deploymentEvent(st, *current, "main_ci_failed")
					event := &st.Events[len(st.Events)-1]
					event.At = now
					event.Data.Data["failing_contexts"] = failures
					event.Data.Data["profile_id"] = d.ProfileID
					event.Data.Data["source_sha"] = d.SourceSHA
					event.Data.Data["main_ci_state"] = state
				}
				break
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func mainCIAttention(st domain.State, product string) []map[string]any {
	var items []map[string]any
	for _, d := range st.Deployments {
		if d.ProductID == product && d.CIEvidence == "pr_tree" && d.MainCIState == "failure" {
			items = append(items, map[string]any{"id": d.ID, "deployment_id": d.ID, "profile_id": d.ProfileID, "reason": "MAIN_CI_FAILED", "source_sha": d.SourceSHA, "main_ci_checked_at": d.MainCICheckedAt})
		}
	}
	return items
}
