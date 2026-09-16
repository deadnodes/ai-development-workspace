package application

import (
	"path"
	"regexp"
	"slices"
	"strings"

	"releasecontrol/internal/domain"
)

var fullSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func applicationByID(st *domain.State, id string) *domain.Application {
	for i := range st.Applications {
		if st.Applications[i].ID == id {
			return &st.Applications[i]
		}
	}
	return nil
}
func revisionByID(st *domain.State, id string) *domain.IntegrationRevision {
	for i := range st.IntegrationRevisions {
		if st.IntegrationRevisions[i].ID == id {
			return &st.IntegrationRevisions[i]
		}
	}
	return nil
}
func environmentByID(st *domain.State, id string) *domain.Environment {
	for i := range st.Environments {
		if st.Environments[i].ID == id {
			return &st.Environments[i]
		}
	}
	return nil
}
func safeBranch(ref string) bool {
	if ref == "" || ref == "@" || strings.HasPrefix(ref, "-") || strings.HasSuffix(ref, ".") || strings.ContainsAny(ref, " ~^:?*[\\") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") {
		return false
	}
	for _, r := range ref {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, segment := range strings.Split(ref, "/") {
		if segment == "" || strings.HasPrefix(segment, ".") || strings.HasSuffix(segment, ".lock") {
			return false
		}
	}
	return true
}

func applyComposition(st *domain.State, c domain.Command, m domain.Meta) (any, domain.Meta, error) {
	switch c.Action {
	case "create_application":
		var input struct {
			Name         string `json:"name"`
			Kind         string `json:"kind"`
			RepositoryID string `json:"repository_id"`
			Path         string `json:"path"`
		}
		if e := decode(c.Data, &input); e != nil {
			return nil, m, e
		}
		if !productExists(st, c.ProductID) {
			return nil, m, missing("product", c.ProductID)
		}
		if strings.TrimSpace(input.Name) == "" {
			return nil, m, invalid("application name required")
		}
		if e := repositories(st, []string{input.RepositoryID}, c.ProductID); e != nil {
			return nil, m, e
		}
		if input.Path != "" && (path.IsAbs(input.Path) || path.Clean(input.Path) != input.Path || input.Path == ".." || strings.HasPrefix(input.Path, "../") || strings.Contains(input.Path, "\\")) {
			return nil, m, invalid("application path must be a clean repository-relative path")
		}
		if input.Kind == "" {
			input.Kind = "APPLICATION"
		}
		if !slices.Contains(domain.ComponentKinds(), input.Kind) {
			return nil, m, invalid("invalid component kind")
		}
		if !domain.RepositoryAllows(repositoryRole(st, input.RepositoryID), input.Kind) {
			return nil, m, invalid("repository purpose does not support this component kind")
		}
		v := domain.Application{Meta: m, Kind: input.Kind, Name: input.Name, RepositoryID: input.RepositoryID, Path: input.Path}
		st.Applications = append(st.Applications, v)
		return v, m, nil
	case "record_integration_revision":
		var input struct {
			RepositoryID string   `json:"repository_id"`
			Branch       string   `json:"branch"`
			BaseCommit   string   `json:"base_commit"`
			HeadCommit   string   `json:"head_commit"`
			Commits      []string `json:"commits"`
		}
		if e := decode(c.Data, &input); e != nil {
			return nil, m, e
		}
		in := integration(st, c.IntegrationID)
		if in == nil {
			return nil, m, missing("integration", c.IntegrationID)
		}
		if e := repositories(st, []string{input.RepositoryID}, in.ProductID); e != nil {
			return nil, m, e
		}
		if f := feature(st, in.FeatureID); f != nil && len(f.Repositories) > 0 && !slices.Contains(f.Repositories, input.RepositoryID) {
			return nil, m, invalid("revision repository outside feature scope")
		}
		if len(in.Repositories) > 0 && !slices.Contains(in.Repositories, input.RepositoryID) {
			return nil, m, invalid("revision repository must be declared by integration")
		}
		if !safeBranch(input.Branch) {
			return nil, m, invalid("valid source branch required")
		}
		if !fullSHA.MatchString(input.BaseCommit) || !fullSHA.MatchString(input.HeadCommit) || len(input.Commits) == 0 {
			return nil, m, invalid("base_commit, head_commit and nonempty commits require full lowercase SHA40 or SHA64")
		}
		seen := map[string]bool{}
		for _, sha := range input.Commits {
			if !fullSHA.MatchString(sha) || len(sha) != len(input.HeadCommit) || seen[sha] {
				return nil, m, invalid("invalid, mixed-format or duplicate commit")
			}
			seen[sha] = true
		}
		if len(input.BaseCommit) != len(input.HeadCommit) || !seen[input.HeadCommit] {
			return nil, m, invalid("head_commit must occur in commits, using the base commit hash format")
		}
		v := domain.IntegrationRevision{Meta: m, IntegrationID: in.ID, RepositoryID: input.RepositoryID, Branch: input.Branch, BaseCommit: input.BaseCommit, HeadCommit: input.HeadCommit, Commits: input.Commits}
		st.IntegrationRevisions = append(st.IntegrationRevisions, v)
		return v, m, nil
	case "plan_composition":
		var input struct {
			EnvironmentID string                        `json:"environment_id"`
			Name          string                        `json:"name"`
			Components    []domain.CompositionComponent `json:"components"`
		}
		if e := decode(c.Data, &input); e != nil {
			return nil, m, e
		}
		if !productExists(st, c.ProductID) {
			return nil, m, missing("product", c.ProductID)
		}
		environment := environmentByID(st, input.EnvironmentID)
		if environment == nil || environment.ProductID != c.ProductID {
			return nil, m, invalid("environment must belong to product")
		}
		if strings.TrimSpace(input.Name) == "" || len(input.Components) == 0 {
			return nil, m, invalid("composition name and components required")
		}
		v := domain.Composition{Meta: m, Name: input.Name, EnvironmentID: input.EnvironmentID, Status: "planned", Components: input.Components, ApplicationSnapshots: []domain.Application{}, RevisionSnapshots: []domain.IntegrationRevision{}, EnvironmentSnapshot: *environment}
		apps := map[string]bool{}
		revisions := map[string]bool{}
		selected := map[string]bool{}
		repoPlans := map[string]domain.CompositionComponent{}
		integrationRepo := map[string]string{}
		for _, component := range input.Components {
			app := applicationByID(st, component.ApplicationID)
			if app == nil || app.ProductID != c.ProductID || apps[app.ID] || domain.ComponentKind(*app) != "APPLICATION" {
				return nil, m, invalid("component applications must be unique and belong to product")
			}
			apps[app.ID] = true
			if !safeBranch(component.BaseRef) || !fullSHA.MatchString(component.BaseCommit) {
				return nil, m, invalid("component base_ref and full base_commit required")
			}
			if !safeBranch(component.TargetBranch) || !strings.HasPrefix(component.TargetBranch, "generated/") || component.TargetBranch == component.BaseRef {
				return nil, m, invalid("target_branch must be a separate generated/ branch")
			}
			if prior, exists := repoPlans[app.RepositoryID]; exists && (prior.BaseRef != component.BaseRef || prior.BaseCommit != component.BaseCommit || prior.TargetBranch != component.TargetBranch || !slices.Equal(prior.RevisionIDs, component.RevisionIDs)) {
				return nil, m, invalid("applications sharing a repository must use the same base, target branch and ordered revisions")
			}
			repoPlans[app.RepositoryID] = component
			v.ApplicationSnapshots = append(v.ApplicationSnapshots, *app)
			componentSeen := map[string]bool{}
			for _, rid := range component.RevisionIDs {
				rev := revisionByID(st, rid)
				if rev == nil || rev.ProductID != c.ProductID || rev.RepositoryID != app.RepositoryID || componentSeen[rid] {
					return nil, m, invalid("revision must be unique within component and match application repository/product")
				}
				componentSeen[rid] = true
				key := rev.IntegrationID + "/" + rev.RepositoryID
				if prior, exists := integrationRepo[key]; exists && prior != rid {
					return nil, m, invalid("only one revision per integration and repository may be selected")
				}
				integrationRepo[key] = rid
				selected[rev.IntegrationID] = true
				if !revisions[rid] {
					v.RevisionSnapshots = append(v.RevisionSnapshots, *rev)
					revisions[rid] = true
				}
			}
		}
		for iid := range selected {
			in := integration(st, iid)
			if in == nil {
				return nil, m, invalid("revision integration no longer exists")
			}
			for _, dep := range in.Dependencies {
				dependency := integration(st, dep)
				if dependency == nil || (dependency.Status != "released" && !selected[dep]) {
					return nil, m, invalid("composition must include dependency %s or it must already be released", dep)
				}
			}
		}
		st.Compositions = append(st.Compositions, v)
		return v, m, nil
	case "select_composition":
		var input struct{}
		if e := decode(c.Data, &input); e != nil {
			return nil, m, e
		}
		for _, v := range st.Compositions {
			if v.ID != c.ID {
				continue
			}
			if c.ProductID != "" && c.ProductID != v.ProductID {
				return nil, m, invalid("composition product mismatch")
			}
			environment := environmentByID(st, v.EnvironmentID)
			if environment == nil || environment.ProductID != v.ProductID {
				return nil, m, invalid("composition environment missing")
			}
			if environment.Cluster != v.EnvironmentSnapshot.Cluster || environment.Namespace != v.EnvironmentSnapshot.Namespace {
				return nil, m, invalid("environment target changed; create a new composition plan")
			}
			if err := selectionAllowed(st, environment.ID); err != nil {
				return nil, m, err
			}
			environment.DesiredOperationID = ""
			environment.DesiredCompositionID = v.ID
			environment.UpdatedAt = m.UpdatedAt
			environment.Actor = c.Actor
			m.ProductID = v.ProductID
			return *environment, m, nil
		}
		return nil, m, missing("composition", c.ID)
	}
	return nil, m, invalid("unknown composition action")
}

func repositoryRole(st *domain.State, repoID string) string {
	if binding := repositoryBinding(st, repoID); binding != nil {
		return binding.Role
	}
	for _, r := range st.Repositories {
		if r.ID == repoID {
			if r.Role != "" {
				return r.Role
			}
			return "SOURCE"
		}
	}
	return ""
}
