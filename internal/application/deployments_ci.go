package application

import (
	"context"
	"regexp"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/providers/forgejo"
	"slices"
	"sort"
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
		if d.ProductID == product && d.CIEvidence == "pr_tree" && d.MainCIState == "failure" && !buildCIFailedForDeployment(st, d) {
			items = append(items, map[string]any{"id": d.ID, "deployment_id": d.ID, "profile_id": d.ProfileID, "reason": "MAIN_CI_FAILED", "source_sha": d.SourceSHA, "main_ci_checked_at": d.MainCICheckedAt})
		}
	}
	return items
}

func buildCIFailedForDeployment(st domain.State, d domain.Deployment) bool {
	for _, b := range st.Builds {
		if b.ID == d.BuildID && b.CIState == "failure" {
			return true
		}
	}
	return false
}
func buildCIAttention(st domain.State, product string) []map[string]any {
	out := []map[string]any{}
	var selected []domain.Build
	for _, b := range st.Builds {
		if b.ProductID != product || b.SupersededBy != "" || b.Branch == "" || b.CIState != "failure" {
			continue
		}
		suppressed := false
		for _, n := range st.Builds {
			if n.ProductID == b.ProductID && n.ProfileID == b.ProfileID && n.Branch == b.Branch && n.SupersededBy == "" && n.CIState == "success" && n.QueuedAt.After(b.QueuedAt) {
				suppressed = true
				break
			}
		}
		if suppressed {
			continue
		}
		selected = append(selected, b)
	}
	sort.Slice(selected, func(i, j int) bool {
		if selected[i].CIFailedAt == nil {
			return false
		}
		if selected[j].CIFailedAt == nil {
			return true
		}
		return selected[i].CIFailedAt.After(*selected[j].CIFailedAt)
	})
	for _, b := range selected {
		ds := []string{}
		for _, d := range st.Deployments {
			if d.ProductID == product && d.BuildID == b.ID && d.EnvironmentID == "dev" {
				ds = append(ds, d.ID)
			}
		}
		features, integrations := []string{}, []string{}
		if b.CITriage != nil {
			seen := map[string]bool{}
			for _, f := range b.CITriage.Features {
				features = append(features, f.FeatureID)
				for _, id := range f.IntegrationIDs {
					if !seen[id] {
						integrations = append(integrations, id)
						seen[id] = true
					}
				}
			}
		}
		out = append(out, map[string]any{"id": "build-ci:" + b.ID, "build_id": b.ID, "reason": "BUILD_CI_FAILED", "profile_id": b.ProfileID, "branch": b.Branch, "source_sha": b.SourceSHA, "failing_contexts": b.CIFailingContexts, "ci_failed_at": b.CIFailedAt, "deployment_ids": ds, "triage": b.CITriage, "agent_action": map[string]any{"type": "INVESTIGATE_CI_FAILURE", "build_id": b.ID, "feature_ids": features, "integration_ids": integrations, "instructions": []string{"Resume the listed Feature(s) via resume and read the latest handoff.", "Fix forward on a branch, then open a PR and merge; the next main build supersedes this one.", "Do not re-queue or roll back unless asked."}}})
	}
	return out
}

// watchBuildCI stores a bounded observation of source CI independently from
// image build and deployment progress.
func (s *Service) watchBuildCI(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	st, err := s.buildSnapshot(ctx)
	if err != nil {
		return err
	}
	slices.SortStableFunc(st.Builds, func(a, b domain.Build) int {
		if a.CICheckedAt == nil {
			if b.CICheckedAt == nil {
				return 0
			}
			return -1
		}
		if b.CICheckedAt == nil {
			return 1
		}
		return a.CICheckedAt.Compare(*b.CICheckedAt)
	})
	type result struct {
		b      domain.Build
		p      buildconfig.Profile
		status forgejo.CommitStatus
		err    error
	}
	results := make(chan result, len(st.Builds))
	slots := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, b := range st.Builds {
		now := time.Now().UTC()
		if checkCtx.Err() != nil {
			break
		}
		if b.Branch == "" || b.RestoreQuarantined || b.SupersededBy != "" || b.QueuedAt.Before(now.Add(-3*time.Hour)) || b.CIState == "success" || b.CIState == "cancelled" || b.CICheckedAt != nil && now.Sub(*b.CICheckedAt) < time.Minute || !(b.Status == "SUCCEEDED" || buildActive(b)) {
			continue
		}
		p, ok := s.builds.config.Profile(b.ProfileID)
		// Required (push) contexts exist only for the default branch; other branches would stay pending.
		if !ok || p.ProductID != b.ProductID || b.Branch != p.DefaultBranch || !s.builds.buildAuthorityMatches(b.ProfileID, b.AuthorityHash) {
			continue
		}
		select {
		case slots <- struct{}{}:
		case <-checkCtx.Done():
			break
		}
		if checkCtx.Err() != nil {
			break
		}
		reserved := false
		err = s.store.Update(ctx, func(state *domain.State) error {
			for i := range state.Builds {
				cur := &state.Builds[i]
				if cur.ID == b.ID && buildFingerprint(*cur) == buildFingerprint(b) {
					cur.CICheckedAt = &now
					b = copyBuild(*cur)
					reserved = true
					break
				}
			}
			return nil
		})
		if err != nil {
			<-slots
			return err
		}
		if !reserved {
			<-slots
			continue
		}
		wg.Add(1)
		go func(b domain.Build, p buildconfig.Profile) {
			defer wg.Done()
			defer func() { <-slots }()
			poll, c := context.WithTimeout(checkCtx, 10*time.Second)
			defer c()
			v, e := s.deployments.source.CommitStatus(poll, s.builds.config.Connection(p.ConnectionID), p.Repository, b.SourceSHA)
			results <- result{b, p, v, e}
		}(b, p)
	}
	wg.Wait()
	close(results)
	for r := range results {
		if r.err != nil {
			continue
		}
		green, failed, contexts := ciVerdict(trustedCIStatus(r.status, r.p), r.p.RequiredStatusContexts)
		state := "pending"
		if failed {
			state = "failure"
		} else if green {
			state = "success"
		} else {
			for k, v := range contexts {
				if v == "cancelled" && (len(r.p.RequiredStatusContexts) == 0 || slices.Contains(r.p.RequiredStatusContexts, k)) {
					state = "cancelled"
					break
				}
			}
		}
		now := *r.b.CICheckedAt
		var updatedBuild domain.Build
		err = s.store.Update(ctx, func(st *domain.State) error {
			for i := range st.Builds {
				cur := &st.Builds[i]
				if cur.ID != r.b.ID || buildFingerprint(*cur) != buildFingerprint(r.b) {
					continue
				}
				prior := cur.CIState
				cur.CIState = state
				cur.CIContexts = contexts
				cur.CIFailingContexts = map[string]string{}
				if failed {
					for k, v := range contexts {
						if (len(r.p.RequiredStatusContexts) == 0 || slices.Contains(r.p.RequiredStatusContexts, k)) && (v == "failure" || v == "error") {
							cur.CIFailingContexts[k] = v
						}
					}
					if prior != "failure" {
						cur.CIFailedAt = &now
						st.Events = append(st.Events, domain.Event{ID: id(), Action: "build_ci_failed", Actor: cur.Actor, At: now, EntityID: cur.ID, ProductID: cur.ProductID, Data: domain.Command{Action: "build_ci_failed", Data: map[string]any{"failing_contexts": cur.CIFailingContexts, "profile_id": cur.ProfileID, "source_sha": cur.SourceSHA, "build_id": cur.ID}}})
					}
				} else if state == "success" && prior == "failure" {
					st.Events = append(st.Events, domain.Event{ID: id(), Action: "build_ci_recovered", Actor: cur.Actor, At: now, EntityID: cur.ID, ProductID: cur.ProductID, Data: domain.Command{Action: "build_ci_recovered", Data: map[string]any{"profile_id": cur.ProfileID, "source_sha": cur.SourceSHA, "build_id": cur.ID}}})
				}
				updatedBuild = copyBuild(*cur)
				break
			}
			return nil
		})
		if err != nil {
			return err
		}
		if state == "failure" {
			for _, prior := range st.Builds {
				if prior.ID == r.b.ID && (prior.CIState != "failure" || prior.CITriage == nil) {
					triage := s.computeBuildCITriage(ctx, r.b, r.p)
					if e := s.store.Update(ctx, func(cur *domain.State) error {
						for i := range cur.Builds {
							x := &cur.Builds[i]
							if x.ID == r.b.ID && buildFingerprint(*x) == buildFingerprint(updatedBuild) && x.CIState == "failure" && x.CITriage == nil {
								x.CITriage = triage
								for j := len(cur.Events) - 1; j >= 0; j-- {
									if cur.Events[j].Action == "build_ci_failed" && cur.Events[j].EntityID == x.ID {
										cur.Events[j].Data.Data["triage"] = triageSummary(triage)
										break
									}
								}
								break
							}
						}
						return nil
					}); e != nil {
						return e
					}
					break
				}
			}
		}
	}
	return nil
}
