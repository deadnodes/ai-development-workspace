package application

import (
	"regexp"
	"releasecontrol/internal/domain"
	"sort"
	"strings"
	"time"
)

type GraphRuntime struct {
	Environment string              `json:"environment"`
	Component   string              `json:"component"`
	Workload    string              `json:"workload"`
	Context     string              `json:"context"`
	ObservedAt  time.Time           `json:"observed_at"`
	Health      string              `json:"health"`
	Images      []string            `json:"images"`
	Matches     []GraphRuntimeMatch `json:"matches"`
}
type GraphRuntimeMatch struct {
	Branch   string `json:"branch"`
	Commit   string `json:"commit"`
	Evidence string `json:"evidence"`
	Pod      string `json:"pod"`
}

var tagSHA = regexp.MustCompile(`(?:^|[-_])([0-9a-f]{7,40})$`)
var fullGraphSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func attachGraphRuntime(st *domain.State, g *FeatureGraph) {
	for i := range g.Repositories {
		r := &g.Repositories[i]
		r.Runtime = []GraphRuntime{}
		latest := map[string]domain.RuntimeSnapshot{}
		for _, s := range st.RuntimeSnapshots {
			a := applicationByID(st, s.ApplicationID)
			if s.ProductID != g.Feature.ProductID || a == nil || a.ProductID != g.Feature.ProductID || a.RepositoryID != r.Repository.ID {
				continue
			}
			key := strings.Join([]string{s.EnvironmentID, s.ApplicationID, s.Runtime.Context, s.Runtime.Namespace, s.Runtime.Deployment, s.Runtime.Container}, "\x00")
			old, ok := latest[key]
			if !ok || s.Runtime.ObservedAt.After(old.Runtime.ObservedAt) || (s.Runtime.ObservedAt.Equal(old.Runtime.ObservedAt) && s.ID > old.ID) {
				latest[key] = s
			}
		}
		for _, s := range latest {
			a := applicationByID(st, s.ApplicationID)
			v := GraphRuntime{Environment: s.EnvironmentID, Component: a.Name, Context: s.Runtime.Context, Workload: s.Runtime.Namespace + "/" + s.Runtime.Deployment + " · " + s.Runtime.Container, ObservedAt: s.Runtime.ObservedAt, Health: s.Runtime.Health, Images: []string{}, Matches: []GraphRuntimeMatch{}}
			if e := environmentByID(st, s.EnvironmentID); e != nil && e.ProductID == g.Feature.ProductID {
				v.Environment = e.Name
			}
			// Reference labels describe observed HEADs or recorded PR commits, never inferred ancestry.
			refs := map[string][]string{}
			add := func(sha, label string) {
				if fullGraphSHA.MatchString(sha) && label != "" {
					refs[sha] = append(refs[sha], label)
				}
			}
			for _, b := range r.Branches {
				add(b.HeadCommit, b.Name+" (observed HEAD)")
			}
			for _, p := range r.PullRequests {
				add(p.HeadSHA, p.SourceBranch+" (PR #"+p.ID+" HEAD)")
				if strings.EqualFold(p.Status, "merged") {
					add(p.MergeSHA, p.TargetBranch+" (PR #"+p.ID+" merge)")
				}
			}
			for _, run := range s.ActionsRuns {
				add(run.HeadSHA, run.HeadBranch+" (build commit)")
			}
			for _, sha := range r.Commits {
				if fullGraphSHA.MatchString(sha) {
					if _, ok := refs[sha]; !ok {
						refs[sha] = []string{"linked feature commit"}
					}
				}
			}
			for _, pod := range s.Runtime.Pods {
				if pod.Phase != "Running" {
					continue
				}
				if !graphContains(v.Images, pod.Image) {
					v.Images = append(v.Images, pod.Image)
				}
				if len(s.Runtime.Errors) > 0 {
					continue
				}
				matched := map[string]string{}
				for _, artifact := range s.ArtifactMatches {
					if artifact.ProductID == g.Feature.ProductID && artifact.ApplicationID == s.ApplicationID && artifact.RepositoryID == r.Repository.ID && artifact.Digest != "" && immutableDigest(pod.ImageID) == artifact.Digest && imageRepository(pod.Image) == artifact.ImageRepository {
						matched[artifact.SourceCommit] = "digest_provenance"
					}
				}
				// A SHA suffix is only a hint, and ambiguous short SHAs do not match.
				if len(matched) == 0 && !strings.Contains(pod.Image, "@") {
					tag := pod.Image[strings.LastIndex(pod.Image, ":")+1:]
					m := tagSHA.FindStringSubmatch(tag)
					if len(m) > 0 {
						candidates := []string{}
						for sha := range refs {
							if strings.HasPrefix(sha, m[1]) {
								candidates = append(candidates, sha)
							}
						}
						if len(candidates) == 1 {
							matched[candidates[0]] = "tag_sha_hint"
						}
					}
				}
				for sha, evidence := range matched {
					for _, branch := range refs[sha] {
						v.Matches = append(v.Matches, GraphRuntimeMatch{Branch: branch, Commit: sha, Evidence: evidence, Pod: pod.Name})
					}
				}
			}
			sort.Strings(v.Images)
			sort.Slice(v.Matches, func(i, j int) bool {
				a, b := v.Matches[i], v.Matches[j]
				return a.Branch+a.Commit+a.Pod < b.Branch+b.Commit+b.Pod
			})
			r.Runtime = append(r.Runtime, v)
		}
		sort.Slice(r.Runtime, func(i, j int) bool {
			a, b := r.Runtime[i], r.Runtime[j]
			return a.Environment+a.Component+a.Context+a.Workload < b.Environment+b.Component+b.Context+b.Workload
		})
	}
}
