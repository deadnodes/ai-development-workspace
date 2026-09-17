package application

import (
	"context"
	"releasecontrol/internal/domain"
	"sort"
	"strings"
	"time"
)

// EventSummary deliberately excludes command payloads already represented in context.
type EventSummary struct {
	ID        string    `json:"id"`
	Action    string    `json:"action"`
	Actor     string    `json:"actor"`
	At        time.Time `json:"at"`
	EntityID  string    `json:"entity_id"`
	ProductID string    `json:"product_id,omitempty"`
	FeatureID string    `json:"feature_id,omitempty"`
}

func (s *Service) Resume(ctx context.Context, featureID string) (any, error) {
	st, e := s.State(ctx)
	if e != nil {
		return nil, e
	}
	f := feature(&st, featureID)
	if f == nil {
		return nil, missing("feature", featureID)
	}
	ins := []domain.Integration{}
	other := []domain.Integration{}
	mem := []domain.Memory{}
	gates := []domain.Gate{}
	checks := []domain.Check{}
	results := []domain.CheckResult{}
	findings := []domain.Finding{}
	envs := []domain.Environment{}
	repos := []domain.Repository{}
	apps := []domain.Application{}
	revisions := []domain.IntegrationRevision{}
	compositions := []domain.Composition{}
	relevantRepos := map[string]bool{}
	events := []EventSummary{}
	latestHandoffs := map[string]domain.Memory{}
	next := []string{}
	ready, released := 0, 0
	for _, i := range st.Integrations {
		if i.FeatureID == featureID {
			ins = append(ins, i)
			if i.Status == "ready" {
				ready++
			}
			if i.Status == "released" {
				released++
			}
		} else if i.ProductID == f.ProductID && i.Status != "released" && i.Status != "ready" {
			other = append(other, i)
		}
	}
	sort.SliceStable(ins, func(i, j int) bool {
		if ins[i].Position == ins[j].Position {
			return ins[i].ID < ins[j].ID
		}
		return ins[i].Position < ins[j].Position
	})
	for _, v := range st.Memories {
		if v.FeatureID == featureID {
			mem = append(mem, v)
			if v.Kind == "handoff" {
				previous, found := latestHandoffs[v.IntegrationID]
				if !found || v.CreatedAt.After(previous.CreatedAt) || (v.CreatedAt.Equal(previous.CreatedAt) && v.ID > previous.ID) {
					latestHandoffs[v.IntegrationID] = v
				}
			}
		}
	}
	for _, v := range st.Gates {
		if v.FeatureID == featureID {
			gates = append(gates, v)
		}
	}
	sort.SliceStable(gates, func(i, j int) bool {
		if gates[i].Position == gates[j].Position {
			return gates[i].ID < gates[j].ID
		}
		return gates[i].Position < gates[j].Position
	})
	seenNext := map[string]bool{}
	appendNext := func(items []string) {
		for _, item := range items {
			item = strings.TrimSpace(item)
			if item != "" && !seenNext[item] {
				next = append(next, item)
				seenNext[item] = true
			}
		}
	}
	if handoff, found := latestHandoffs[""]; found {
		appendNext(handoff.Next)
	}
	for _, in := range ins {
		if handoff, found := latestHandoffs[in.ID]; found {
			appendNext(handoff.Next)
		} else if in.Status != "ready" && in.Status != "released" {
			appendNext(in.Remaining)
		}
	}
	for _, v := range st.Checks {
		if v.FeatureID == featureID {
			checks = append(checks, v)
		}
	}
	for _, v := range st.Results {
		if v.FeatureID == featureID {
			results = append(results, v)
		}
	}
	for _, v := range st.Findings {
		if v.FeatureID == featureID {
			findings = append(findings, v)
		}
	}
	for _, v := range st.Environments {
		if v.ProductID == f.ProductID {
			envs = append(envs, v)
		}
	}
	for _, rid := range f.Repositories {
		relevantRepos[rid] = true
	}
	for _, in := range ins {
		for _, rid := range in.Repositories {
			relevantRepos[rid] = true
		}
		for _, branch := range in.Branches {
			relevantRepos[branch.RepositoryID] = true
		}
		for _, commit := range in.Commits {
			relevantRepos[commit.RepositoryID] = true
		}
	}
	for _, rev := range st.IntegrationRevisions {
		if rev.FeatureID == featureID {
			revisions = append(revisions, rev)
			relevantRepos[rev.RepositoryID] = true
		}
	}
	for _, repository := range st.Repositories {
		if repository.ProductID == f.ProductID && relevantRepos[repository.ID] {
			repos = append(repos, repository)
		}
	}
	relevantApps := map[string]bool{}
	for _, composition := range st.Compositions {
		if composition.ProductID != f.ProductID {
			continue
		}
		relevant := false
		for _, rev := range composition.RevisionSnapshots {
			if rev.FeatureID == featureID {
				relevant = true
				break
			}
		}
		if relevant {
			compositions = append(compositions, composition)
			for _, app := range composition.ApplicationSnapshots {
				relevantApps[app.ID] = true
			}
		}
	}
	for _, app := range st.Applications {
		if app.ProductID == f.ProductID && (relevantRepos[app.RepositoryID] || relevantApps[app.ID]) {
			apps = append(apps, app)
		}
	}
	for _, v := range st.Events {
		if v.FeatureID == featureID {
			events = append(events, EventSummary{ID: v.ID, Action: v.Action, Actor: v.Actor, At: v.At, EntityID: v.EntityID, ProductID: v.ProductID, FeatureID: v.FeatureID})
		}
	}
	eventsTotal := len(events)
	if eventsTotal > 20 {
		events = events[eventsTotal-20:]
	}
	response := map[string]any{"applications": apps, "integration_revisions": revisions, "compositions": compositions, "feature": f, "integrations": ins, "progress": map[string]int{"total": len(ins), "ready": ready, "released": released}, "memories": mem, "gates": gates, "checks": checks, "results": results, "findings": findings, "environments": envs, "repositories": repos, "other_active_work": other, "unlinked_git_commits": unlinkedGitCommits(st, f.ProductID), "next_actions": next, "events": events, "events_total": eventsTotal, "events_truncated": eventsTotal > len(events)}
	response["project_context"], e = s.ProjectContext(ctx, f.ProductID)
	if e != nil {
		return nil, e
	}
	addExternalContext(response, st, featureID)
	addDeliveryContext(response, st, featureID, "")
	return response, nil
}
