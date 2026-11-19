package application

import (
	"context"
	"fmt"
	"regexp"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/providers/forgejo"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type commitRangeSource interface {
	CommitRange(context.Context, buildconfig.Connection, string, string, string, int) ([]forgejo.Commit, bool, error)
}
type commitPRSource interface {
	CommitPullRequest(context.Context, buildconfig.Connection, string, string) (forgejo.PR, bool, error)
}

// buildCITriageMatches is intentionally pure so matching rules can be tested independently.
func buildCITriageMatches(st domain.State, b domain.Build, p buildconfig.Profile, commits []domain.BuildCICommit) ([]domain.BuildCIFeature, []domain.BuildCIHandoff) {
	shaSet, prSet := map[string]bool{}, map[int]bool{}
	for _, c := range commits {
		shaSet[c.SHA] = true
		if c.PRNumber > 0 {
			prSet[c.PRNumber] = true
		}
	}
	matched := map[string]map[string]bool{}
	repoIDs := map[string]bool{}
	suffix := strings.ToLower(strings.TrimSuffix(p.Repository, ".git"))
	parts := strings.Split(suffix, "/")
	repo := parts[len(parts)-1]
	for _, r := range st.Repositories {
		u := strings.ToLower(strings.TrimSuffix(strings.TrimRight(r.URL, "/"), ".git"))
		n := strings.ToLower(strings.TrimSuffix(r.Name, ".git"))
		if strings.HasSuffix(u, suffix) || strings.HasSuffix(n, suffix) || n == repo || strings.HasSuffix(u, "/"+repo) {
			repoIDs[r.ID] = true
		}
	}
	for _, in := range st.Integrations {
		if in.ProductID != b.ProductID {
			continue
		}
		add := func(k string) {
			if matched[in.FeatureID] == nil {
				matched[in.FeatureID] = map[string]bool{}
			}
			matched[in.FeatureID][k] = true
		}
		for _, c := range in.Commits {
			if shaSet[c.SHA] && repoIDs[c.RepositoryID] {
				add("commit:" + short(c.SHA))
			}
		}
		for _, rev := range st.IntegrationRevisions {
			if rev.IntegrationID != in.ID || !repoIDs[rev.RepositoryID] {
				continue
			}
			if shaSet[rev.HeadCommit] {
				add("commit:" + short(rev.HeadCommit))
			}
			for _, sha := range rev.Commits {
				if shaSet[sha] {
					add("commit:" + short(sha))
				}
			}
		}
		for _, obs := range st.GitObservations {
			if obs.IntegrationID == in.ID && repoIDs[obs.RepositoryID] {
				for _, commit := range obs.Commits {
					if shaSet[commit.SHA] {
						add("commit:" + short(commit.SHA))
					}
				}
			}
		}
		for _, pr := range in.PullRequests {
			if !repoIDs[pr.RepositoryID] {
				continue
			}
			n := 0
			fmt.Sscanf(pr.ID, "%d", &n)
			if n == 0 {
				re := regexp.MustCompile(`/pulls/([0-9]+)$`)
				m := re.FindStringSubmatch(pr.URL)
				if len(m) > 1 {
					fmt.Sscanf(m[1], "%d", &n)
				}
			}
			if prSet[n] && n > 0 {
				add(fmt.Sprintf("pr:%d", n))
			}
		}
	}
	now := time.Now().UTC()
	memories := append([]domain.Memory(nil), st.Memories...)
	sort.Slice(memories, func(i, j int) bool { return memories[i].CreatedAt.After(memories[j].CreatedAt) })
	if len(memories) > 2000 {
		memories = memories[:2000]
	}
	for _, m := range memories {
		if m.ProductID != b.ProductID || m.CreatedAt.Before(now.Add(-14*24*time.Hour)) {
			continue
		}
		text := strings.ToLower(m.Title + " " + m.Body + " " + strings.Join(m.Completed, " "))
		for _, c := range commits {
			if m.Commit != "" && (m.Commit == c.SHA || (len(m.Commit) >= 12 && strings.HasPrefix(c.SHA, m.Commit))) {
				if matched[m.FeatureID] == nil {
					matched[m.FeatureID] = map[string]bool{}
				}
				matched[m.FeatureID]["memory:"+m.ID] = true
				break
			}
			if c.PRNumber > 0 && strings.Contains(text, fmt.Sprintf("/%s/pulls/%d", strings.ToLower(p.Repository), c.PRNumber)) || len(c.SHA) >= 12 && strings.Contains(text, strings.ToLower(c.SHA[:12])) {
				if matched[m.FeatureID] == nil {
					matched[m.FeatureID] = map[string]bool{}
				}
				matched[m.FeatureID]["memory:"+m.ID] = true
				break
			}
		}
	}
	features := []domain.BuildCIFeature{}
	for fid, keys := range matched {
		if fid == "" {
			continue
		}
		f := domain.BuildCIFeature{FeatureID: fid}
		for _, x := range st.Features {
			if x.ID == fid {
				f.Title = x.Title
				f.Status = x.Status
				break
			}
		}
		direct := false
		for k := range keys {
			direct = direct || strings.HasPrefix(k, "commit:") || strings.HasPrefix(k, "pr:")
		}
		if direct {
			for in := range matchedIntegrations(st, fid, b.ProductID) {
				f.IntegrationIDs = append(f.IntegrationIDs, in)
			}
		}
		for k := range keys {
			f.MatchedBy = append(f.MatchedBy, k)
		}
		sort.Strings(f.MatchedBy)
		sort.Strings(f.IntegrationIDs)
		features = append(features, f)
	}
	sort.Slice(features, func(i, j int) bool { return features[i].FeatureID < features[j].FeatureID })
	if len(features) > 10 {
		features = features[:10]
	}
	selected := map[string]bool{}
	for _, f := range features {
		selected[f.FeatureID] = true
		for _, i := range f.IntegrationIDs {
			selected[i] = true
		}
	}
	handoffs := []domain.BuildCIHandoff{}
	for _, m := range memories {
		if m.ProductID != b.ProductID || m.CreatedAt.Before(now.Add(-14*24*time.Hour)) || m.Kind != "handoff" || !selected[m.FeatureID] && !selected[m.IntegrationID] {
			continue
		}
		handoffs = append(handoffs, domain.BuildCIHandoff{MemoryID: m.ID, FeatureID: m.FeatureID, IntegrationID: m.IntegrationID, Kind: m.Kind, Title: truncateRunes(m.Title, 200), Session: m.Session, Actor: m.Actor, CreatedAt: m.CreatedAt})
		if len(handoffs) == 3 {
			break
		}
	}
	if len(handoffs) == 0 {
		for _, m := range memories {
			if m.ProductID != b.ProductID || m.CreatedAt.Before(now.Add(-14*24*time.Hour)) || m.Kind != "progress" || !selected[m.FeatureID] && !selected[m.IntegrationID] {
				continue
			}
			handoffs = append(handoffs, domain.BuildCIHandoff{MemoryID: m.ID, FeatureID: m.FeatureID, IntegrationID: m.IntegrationID, Kind: m.Kind, Title: truncateRunes(m.Title, 200), Session: m.Session, Actor: m.Actor, CreatedAt: m.CreatedAt})
			if len(handoffs) == 3 {
				break
			}
		}
	}
	return features, handoffs
}
func matchedIntegrations(st domain.State, feature, product string) map[string]string {
	out := map[string]string{}
	for _, i := range st.Integrations {
		if i.FeatureID == feature && i.ProductID == product {
			out[i.ID] = i.ID
		}
	}
	return out
}
func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}
func triageSummary(t *domain.BuildCITriage) map[string]any {
	fs, is := []string{}, []string{}
	for _, f := range t.Features {
		fs = append(fs, f.FeatureID)
		is = append(is, f.IntegrationIDs...)
	}
	return map[string]any{"feature_ids": fs, "integration_ids": is}
}
func (s *Service) computeBuildCITriage(ctx context.Context, b domain.Build, p buildconfig.Profile) *domain.BuildCITriage {
	tri := &domain.BuildCITriage{BuildActor: b.Actor, BuildTrigger: b.Trigger, ComputedAt: time.Now().UTC()}
	st, err := s.State(ctx)
	if err != nil {
		tri.UnavailableReason = "local triage state unavailable"
		return tri
	}
	var base, fallback domain.Build
	found, haveFallback := false, false
	for _, x := range st.Builds {
		if x.ID == b.ID || x.ProductID != b.ProductID || x.ProfileID != b.ProfileID || x.Branch != b.Branch || x.Status != "SUCCEEDED" || x.RestoreQuarantined || !x.QueuedAt.Before(b.QueuedAt) {
			continue
		}
		if !haveFallback || x.QueuedAt.After(fallback.QueuedAt) {
			fallback = x
			haveFallback = true
		}
		if x.CIState != "failure" && (!found || x.QueuedAt.After(base.QueuedAt)) {
			base = x
			found = true
		}
	}
	if !found && haveFallback {
		base = fallback
		found = true
	}
	if found {
		tri.BaseBuildID = base.ID
		tri.BaseSHA = base.SourceSHA
	}
	commits := []forgejo.Commit{{SHA: b.SourceSHA}}
	truncated := false
	providerCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	src, ok := s.builds.provider.(commitRangeSource)
	if !ok {
		src, ok = s.deployments.source.(commitRangeSource)
	}
	if ok {
		v, t, e := src.CommitRange(providerCtx, s.builds.config.Connection(p.ConnectionID), p.Repository, tri.BaseSHA, b.SourceSHA, 50)
		if e != nil {
			tri.UnavailableReason = e.Error()
		} else {
			commits = v
			truncated = t
		}
	} else {
		tri.UnavailableReason = "commit range lookup unavailable"
	}
	if len(commits) > 50 {
		commits = commits[:50]
		truncated = true
	}
	tri.CommitsTruncated = truncated
	converted := make([]domain.BuildCICommit, 0, len(commits))
	for _, c := range commits {
		title := truncateRunes(strings.SplitN(c.Title, "\n", 2)[0], 200)
		converted = append(converted, domain.BuildCICommit{SHA: c.SHA, Title: title, Author: c.Author, CommittedAt: c.CommittedAt})
	}
	pr, ok := s.builds.provider.(commitPRSource)
	if !ok {
		pr, ok = s.deployments.source.(commitPRSource)
	}
	if ok {
		for i := 0; i < len(converted) && i < 10; i++ {
			if x, found, e := pr.CommitPullRequest(providerCtx, s.builds.config.Connection(p.ConnectionID), p.Repository, converted[i].SHA); e == nil && found {
				converted[i].PRNumber = x.Number
			}
		}
	}
	tri.Commits = converted
	features, handoffs := buildCITriageMatches(st, b, p, converted)
	tri.Features = features
	tri.Handoffs = handoffs
	return tri
}
