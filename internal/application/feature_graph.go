package application

import (
	"encoding/json"
	"releasecontrol/internal/domain"
	"sort"
	"strings"
	"time"
)

// FeatureGraph is a persisted-evidence projection. It never queries a provider,
// infers ancestry from SHA lists, or treats a Git merge as a deployment.
type FeatureGraph struct {
	Environments      []domain.Environment         `json:"environments"`
	Feature           domain.Feature               `json:"feature"`
	Repositories      []GraphRepository            `json:"repositories"`
	Integrations      []domain.Integration         `json:"integrations"`
	Revisions         []domain.IntegrationRevision `json:"revisions"`
	Compositions      []domain.Composition         `json:"compositions"`
	Builds            []domain.DeliveryBuildRun    `json:"builds"`
	Artifacts         []domain.DeliveryArtifact    `json:"artifacts"`
	Deployments       []GraphDeployment            `json:"deployments"`
	ReportedSnapshots []GraphReportedSnapshot      `json:"reported_snapshots"`
}
type GraphRepository struct {
	Repository   domain.Repository  `json:"repository"`
	Integrations []string           `json:"integrations"`
	Branches     []GraphBranch      `json:"branches"`
	PullRequests []GraphPullRequest `json:"pull_requests"`
	Commits      []string           `json:"commits"`
}
type GraphBranch struct {
	Name       string `json:"name"`
	HeadCommit string `json:"head_commit,omitempty"`
	ObservedAt string `json:"observed_at,omitempty"`
	Evidence   string `json:"evidence"`
}
type GraphPullRequest struct {
	ID             string   `json:"id"`
	URL            string   `json:"url"`
	RepositoryID   string   `json:"repository_id"`
	IntegrationIDs []string `json:"integration_ids"`
	Title          string   `json:"title"`
	Status         string   `json:"status"`
	SourceBranch   string   `json:"source_branch"`
	TargetBranch   string   `json:"target_branch"`
	HeadSHA        string   `json:"head_sha"`
	MergeSHA       string   `json:"merge_sha"`
	CreatedAt      string   `json:"created_at"`
	MergedAt       string   `json:"merged_at"`
	ObservedAt     string   `json:"observed_at"`
	Evidence       string   `json:"evidence"`
	SourceMemoryID string   `json:"source_memory_id,omitempty"`
}
type GraphDeploymentSource struct {
	RepositoryID string `json:"repository_id"`
	Branch       string `json:"branch"`
	Commit       string `json:"commit"`
	BaseCommit   string `json:"base_commit"`
}
type GraphDeployment struct {
	RepositoryID        string                      `json:"repository_id"`
	IntegrationIDs      []string                    `json:"integration_ids"`
	Sources             []GraphDeploymentSource     `json:"sources"`
	CreatedAt           time.Time                   `json:"created_at"`
	UpdatedAt           time.Time                   `json:"updated_at"`
	OperationID         string                      `json:"operation_id"`
	EnvironmentID       string                      `json:"environment_id"`
	ApplicationID       string                      `json:"application_id"`
	Status              string                      `json:"status"`
	DeploymentState     string                      `json:"deployment_state"`
	SourceCommit        string                      `json:"source_commit"`
	Digest              string                      `json:"digest"`
	GitOpsCommit        string                      `json:"gitops_commit"`
	RuntimeObservations []domain.RuntimeObservation `json:"runtime_observations"`
}
type GraphReportedComponent struct {
	RepositoryID string `json:"repository_id"`
	Name         string `json:"name"`
	Commit       string `json:"commit"`
	URL          string `json:"url"`
}
type GraphReportedSnapshot struct {
	MemoryID       string                   `json:"memory_id"`
	ReportedAt     string                   `json:"reported_at"`
	Cluster        string                   `json:"cluster"`
	Namespace      string                   `json:"namespace"`
	ReportedHealth string                   `json:"reported_health"`
	Limits         string                   `json:"limits"`
	Evidence       string                   `json:"evidence"`
	Components     []GraphReportedComponent `json:"components"`
}

func featureGraph(st domain.State, id string) (*FeatureGraph, error) {
	f := feature(&st, id)
	if f == nil {
		return nil, missing("feature", id)
	}
	g := &FeatureGraph{Environments: []domain.Environment{}, Feature: *f, Repositories: []GraphRepository{}, Integrations: []domain.Integration{}, Revisions: []domain.IntegrationRevision{}, Compositions: []domain.Composition{}, Builds: []domain.DeliveryBuildRun{}, Artifacts: []domain.DeliveryArtifact{}, Deployments: []GraphDeployment{}, ReportedSnapshots: []GraphReportedSnapshot{}}
	repos := map[string]*GraphRepository{}
	allRepos := map[string]domain.Repository{}
	ins := map[string]bool{}
	revisions := map[string]bool{}
	ops := map[string]bool{}
	for _, r := range st.Repositories {
		if r.ProductID == f.ProductID {
			allRepos[r.ID] = r
		}
	}
	addRepo := func(rid string) *GraphRepository {
		r, ok := allRepos[rid]
		if !ok {
			return nil
		}
		if repos[rid] == nil {
			repos[rid] = &GraphRepository{Repository: r, Integrations: []string{}, Branches: []GraphBranch{}, PullRequests: []GraphPullRequest{}, Commits: []string{}}
		}
		return repos[rid]
	}
	for _, rid := range f.Repositories {
		addRepo(rid)
	}
	for _, in := range st.Integrations {
		if in.FeatureID != id || in.ProductID != f.ProductID {
			continue
		}
		ins[in.ID] = true
		g.Integrations = append(g.Integrations, in)
		for _, rid := range in.Repositories {
			if r := addRepo(rid); r != nil {
				r.Integrations = graphAppend(r.Integrations, in.ID)
			}
		}
		for _, c := range in.Commits {
			if r := addRepo(c.RepositoryID); r != nil {
				r.Commits = graphAppend(r.Commits, c.SHA)
			}
		}
		for _, b := range in.Branches {
			if r := addRepo(b.RepositoryID); r != nil {
				r.Branches = append(r.Branches, GraphBranch{Name: b.Name, Evidence: "recorded"})
			}
		}
		for _, p := range in.PullRequests {
			if r := addRepo(p.RepositoryID); r != nil {
				found := false
				for n := range r.PullRequests {
					if r.PullRequests[n].URL == p.URL && p.URL != "" {
						r.PullRequests[n].IntegrationIDs = graphAppend(r.PullRequests[n].IntegrationIDs, in.ID)
						found = true
					}
				}
				if !found {
					r.PullRequests = append(r.PullRequests, GraphPullRequest{ID: p.ID, URL: p.URL, RepositoryID: p.RepositoryID, IntegrationIDs: []string{in.ID}, Status: strings.ToUpper(p.Status), Evidence: "recorded"})
				}
			}
		}
	}
	// Historical imports carry typed PR provenance in progress memories. A memory
	// can enrich only an existing linked PR in its own integration and product.
	for _, m := range st.Memories {
		if m.FeatureID != id || m.ProductID != f.ProductID {
			continue
		}
		if ins[m.IntegrationID] {
			var records []GraphPullRequest
			if json.Unmarshal([]byte(m.Body), &records) == nil {
				for _, p := range records {
					if p.URL == "" || p.SourceBranch == "" || p.TargetBranch == "" {
						continue
					}
					for _, r := range repos {
						for n := range r.PullRequests {
							old := &r.PullRequests[n]
							if old.URL != p.URL || !graphContains(old.IntegrationIDs, m.IntegrationID) {
								continue
							}
							if old.SourceMemoryID != "" && (graphTimeAfter(old.ObservedAt, p.ObservedAt) || (old.ObservedAt == p.ObservedAt && old.SourceMemoryID > m.ID)) {
								continue
							}
							p.ID = old.ID
							p.RepositoryID = old.RepositoryID
							p.IntegrationIDs = old.IntegrationIDs
							p.Evidence = "recorded"
							p.SourceMemoryID = m.ID
							*old = p
						}
					}
				}
			}
		}
		var report struct {
			ReportedAt     string `json:"reported_at"`
			Cluster        string `json:"cluster"`
			Namespace      string `json:"namespace"`
			ReportedHealth string `json:"reported_health"`
			Limits         string `json:"limits"`
			Sources        map[string]struct {
				Verified struct {
					SHA string `json:"sha"`
					URL string `json:"url"`
				} `json:"verified"`
			} `json:"source_commit_identities_verified_on_github"`
		}
		if json.Unmarshal([]byte(m.Body), &report) == nil && report.ReportedAt != "" && len(report.Sources) > 0 {
			snap := GraphReportedSnapshot{MemoryID: m.ID, ReportedAt: report.ReportedAt, Cluster: report.Cluster, Namespace: report.Namespace, ReportedHealth: report.ReportedHealth, Limits: report.Limits, Evidence: "reported", Components: []GraphReportedComponent{}}
			for name, c := range report.Sources {
				for _, r := range repos {
					if c.Verified.SHA != "" && strings.TrimSuffix(strings.TrimSuffix(r.Repository.URL, ".git"), "/")+"/commit/"+c.Verified.SHA == c.Verified.URL {
						snap.Components = append(snap.Components, GraphReportedComponent{RepositoryID: r.Repository.ID, Name: name, Commit: c.Verified.SHA, URL: c.Verified.URL})
						break
					}
				}
			}
			sort.Slice(snap.Components, func(i, j int) bool { return snap.Components[i].Name < snap.Components[j].Name })
			g.ReportedSnapshots = append(g.ReportedSnapshots, snap)
		}
	}
	for _, v := range st.IntegrationRevisions {
		if v.ProductID == f.ProductID && ins[v.IntegrationID] {
			g.Revisions = append(g.Revisions, v)
			revisions[v.ID] = true
		}
	}
	for _, c := range st.Compositions {
		if c.ProductID != f.ProductID {
			continue
		}
		included := false
		for _, v := range c.RevisionSnapshots {
			if ins[v.IntegrationID] {
				included = true
			}
		}
		for _, a := range c.Components {
			for _, rid := range a.RevisionIDs {
				if revisions[rid] {
					included = true
				}
			}
		}
		if included {
			g.Compositions = append(g.Compositions, c)
		}
	}
	for _, o := range st.Operations {
		if o.ProductID != f.ProductID {
			continue
		}
		included := ins[o.IntegrationID]
		for _, iid := range o.IntegrationIDs {
			included = included || ins[iid]
		}
		if !included {
			continue
		}
		ops[o.ID] = true
		if o.EnvironmentID == "" {
			continue
		}
		d := GraphDeployment{IntegrationIDs: []string{}, Sources: []GraphDeploymentSource{}, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt, OperationID: o.ID, EnvironmentID: o.EnvironmentID, ApplicationID: o.ApplicationID, Status: o.Status, DeploymentState: o.DeploymentState, Digest: o.ExpectedDigest, RuntimeObservations: []domain.RuntimeObservation{}}
		if ins[o.IntegrationID] {
			d.IntegrationIDs = append(d.IntegrationIDs, o.IntegrationID)
		}
		for _, iid := range o.IntegrationIDs {
			if ins[iid] {
				d.IntegrationIDs = graphAppend(d.IntegrationIDs, iid)
			}
		}
		sort.Strings(d.IntegrationIDs)
		for _, a := range st.Applications {
			if a.ProductID == f.ProductID && a.ID == o.ApplicationID && repos[a.RepositoryID] != nil {
				d.RepositoryID = a.RepositoryID
			}
		}
		for _, source := range o.Sources {
			if repos[source.RepositoryID] == nil {
				continue
			}
			v := GraphDeploymentSource{RepositoryID: source.RepositoryID, Branch: source.Plan.TargetBranch, BaseCommit: source.Plan.BaseSHA}
			if source.Result != nil {
				v.Commit = source.Result.SHA
				v.Branch = source.Result.Branch
			}
			d.Sources = append(d.Sources, v)
		}
		if d.DeploymentState == "" {
			d.DeploymentState = "UNKNOWN"
		}
		if o.Snapshot != nil {
			d.SourceCommit = o.Snapshot.Revision.HeadCommit
			if repos[o.Snapshot.Revision.RepositoryID] != nil {
				d.RepositoryID = o.Snapshot.Revision.RepositoryID
			}
			if d.RepositoryID != "" {
				d.Sources = append(d.Sources, GraphDeploymentSource{RepositoryID: d.RepositoryID, Branch: o.Snapshot.Revision.Branch, Commit: d.SourceCommit, BaseCommit: o.Snapshot.Revision.BaseCommit})
			}
		}
		if o.Artifact != nil {
			d.Digest = o.Artifact.Digest
		}
		if o.GitOpsResult != nil {
			d.GitOpsCommit = o.GitOpsResult.CommitSHA
		}
		for _, v := range st.RuntimeObservations {
			if v.ProductID == f.ProductID && v.OperationID == o.ID && v.EnvironmentID == o.EnvironmentID {
				d.RuntimeObservations = append(d.RuntimeObservations, v)
			}
		}
		if d.RepositoryID == "" && len(d.Sources) == 1 {
			d.RepositoryID = d.Sources[0].RepositoryID
			d.SourceCommit = d.Sources[0].Commit
		}
		for _, source := range d.Sources {
			if d.SourceCommit == "" && source.RepositoryID == d.RepositoryID {
				d.SourceCommit = source.Commit
			}
		}
		sort.Slice(d.Sources, func(i, j int) bool { return d.Sources[i].RepositoryID < d.Sources[j].RepositoryID })
		g.Deployments = append(g.Deployments, d)
	}
	for _, b := range st.DeliveryBuildRuns {
		if b.ProductID == f.ProductID && ops[b.OperationID] {
			g.Builds = append(g.Builds, b)
		}
	}
	for _, a := range st.DeliveryArtifacts {
		if a.ProductID == f.ProductID && ops[a.OperationID] {
			g.Artifacts = append(g.Artifacts, a)
		}
	}
	for _, o := range st.GitObservations {
		if o.ProductID == f.ProductID && ins[o.IntegrationID] {
			if r := repos[o.RepositoryID]; r != nil {
				r.Branches = append(r.Branches, GraphBranch{Name: o.Branch, HeadCommit: o.HeadCommit, ObservedAt: o.ObservedAt.Format("2006-01-02T15:04:05Z07:00"), Evidence: "observed"})
			}
		}
	}
	envs := map[string]bool{}
	for _, c := range g.Compositions {
		envs[c.EnvironmentID] = true
	}
	for _, d := range g.Deployments {
		envs[d.EnvironmentID] = true
	}
	for _, e := range st.Environments {
		if e.ProductID == f.ProductID && envs[e.ID] {
			g.Environments = append(g.Environments, e)
		}
	}
	for _, r := range repos {
		for _, p := range r.PullRequests {
			for _, name := range []string{p.SourceBranch, p.TargetBranch} {
				if name != "" {
					r.Branches = append(r.Branches, GraphBranch{Name: name, Evidence: "historical_pr"})
				}
			}
		}
		sort.SliceStable(r.Branches, func(i, j int) bool {
			if r.Branches[i].Name == r.Branches[j].Name {
				return graphTimeAfter(r.Branches[i].ObservedAt, r.Branches[j].ObservedAt)
			}
			return r.Branches[i].Name < r.Branches[j].Name
		})
		unique := []GraphBranch{}
		for _, b := range r.Branches {
			if len(unique) == 0 || unique[len(unique)-1].Name != b.Name {
				unique = append(unique, b)
			}
		}
		r.Branches = unique
		sort.Strings(r.Commits)
		sort.Strings(r.Integrations)
		sort.Slice(r.PullRequests, func(i, j int) bool {
			if r.PullRequests[i].CreatedAt == r.PullRequests[j].CreatedAt {
				return r.PullRequests[i].URL < r.PullRequests[j].URL
			}
			return graphTimeAfter(r.PullRequests[j].CreatedAt, r.PullRequests[i].CreatedAt)
		})
		g.Repositories = append(g.Repositories, *r)
	}
	sort.Slice(g.Repositories, func(i, j int) bool { return g.Repositories[i].Repository.Name < g.Repositories[j].Repository.Name })
	sort.Slice(g.Integrations, func(i, j int) bool {
		if g.Integrations[i].Position == g.Integrations[j].Position {
			return g.Integrations[i].ID < g.Integrations[j].ID
		}
		return g.Integrations[i].Position < g.Integrations[j].Position
	})
	sort.Slice(g.ReportedSnapshots, func(i, j int) bool {
		if g.ReportedSnapshots[i].ReportedAt == g.ReportedSnapshots[j].ReportedAt {
			return g.ReportedSnapshots[i].MemoryID < g.ReportedSnapshots[j].MemoryID
		}
		return g.ReportedSnapshots[i].ReportedAt < g.ReportedSnapshots[j].ReportedAt
	})
	return g, nil
}
func graphContains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
func graphAppend(xs []string, s string) []string {
	if !graphContains(xs, s) {
		return append(xs, s)
	}
	return xs
}

func graphTimeAfter(a, b string) bool {
	at, ae := time.Parse(time.RFC3339Nano, a)
	bt, be := time.Parse(time.RFC3339Nano, b)
	if ae == nil && be == nil {
		return at.After(bt)
	}
	return ae == nil && be != nil
}
