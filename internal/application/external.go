package application

import (
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
)

var repositoryName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var imageName = regexp.MustCompile(`^ghcr\.io/[a-z0-9][a-z0-9._/-]+$`)

func connection(st *domain.State, i string) *domain.GitHubConnection {
	for n := range st.GitHubConnections {
		if st.GitHubConnections[n].ID == i {
			return &st.GitHubConnections[n]
		}
	}
	return nil
}
func repositoryBinding(st *domain.State, i string) *domain.RepositoryBinding {
	for n := len(st.RepositoryBindings) - 1; n >= 0; n-- {
		if st.RepositoryBindings[n].RepositoryID == i {
			return &st.RepositoryBindings[n]
		}
	}
	return nil
}
func repositoryLocator(st *domain.State, b domain.RepositoryBinding) string {
	for _, repo := range st.Repositories {
		if repo.ID != b.RepositoryID {
			continue
		}
		for _, registered := range st.RegisteredRepositories {
			if registered.ID == repo.RegisteredRepositoryID && registered.ProviderRepositoryID > 0 {
				return strconv.FormatInt(registered.ProviderRepositoryID, 10)
			}
		}
	}
	return b.FullName
}
func configuredBase(binding domain.RepositoryBinding) string {
	if binding.BaseBranch != "" {
		return binding.BaseBranch
	}
	return binding.DefaultBranch
}
func componentBuild(st *domain.State, i string) *domain.ComponentBuild {
	for n := len(st.ComponentBuilds) - 1; n >= 0; n-- {
		if st.ComponentBuilds[n].ApplicationID == i {
			return &st.ComponentBuilds[n]
		}
	}
	return nil
}
func environmentBinding(st *domain.State, env, app string) *domain.EnvironmentBinding {
	for n := len(st.EnvironmentBindings) - 1; n >= 0; n-- {
		v := &st.EnvironmentBindings[n]
		if v.EnvironmentID == env && v.ApplicationID == app {
			return v
		}
	}
	return nil
}
func scopedConnection(st *domain.State, id, product string) (*domain.GitHubConnection, error) {
	c := connection(st, id)
	if c == nil || product == "" {
		return nil, invalid("product connection grant required")
	}
	if c.ProductID == product {
		return c, nil
	}
	for _, g := range st.ConnectionGrants {
		if g.ConnectionID == id && g.ProductID == product {
			return c, nil
		}
	}
	return nil, invalid("product has no grant for instance connection")
}
func registeredRepository(st *domain.State, connectionID, fullName string) *domain.RegisteredRepository {
	return registeredRepositoryIdentity(st, connectionID, fullName, 0)
}
func registeredRepositoryIdentity(st *domain.State, connectionID, fullName string, providerID int64) *domain.RegisteredRepository {
	conn := connection(st, connectionID)
	if conn == nil {
		return nil
	}
	api := strings.TrimRight(conn.Config.APIURL, "/")
	for i := range st.RegisteredRepositories {
		v := &st.RegisteredRepositories[i]
		vAPI := v.ProviderAPIURL
		if vAPI == "" {
			if original := connection(st, v.ConnectionID); original != nil {
				vAPI = strings.TrimRight(original.Config.APIURL, "/")
			}
		}
		if vAPI != api {
			continue
		}
		if providerID > 0 && v.ProviderRepositoryID > 0 {
			if v.ProviderRepositoryID != providerID {
				continue
			}
		} else if !strings.EqualFold(v.FullName, fullName) {
			continue
		}
		return v
	}
	return nil
}
func associateRegistryConnection(v *domain.RegisteredRepository, conn *domain.GitHubConnection) {
	v.ProviderAPIURL = strings.TrimRight(conn.Config.APIURL, "/")
	if !slices.Contains(v.ConnectionIDs, conn.ID) {
		v.ConnectionIDs = append(v.ConnectionIDs, conn.ID)
	}
}

func secretRef(ref string) bool {
	return (strings.HasPrefix(ref, "env:") && len(ref) > 4 && !strings.ContainsAny(ref[4:], " /\n\r\t")) || (strings.HasPrefix(ref, "file:/") && len(ref) > 6 && !strings.ContainsAny(ref, "\n\r"))
}
func terminalOperation(status string) bool {
	return slices.Contains([]string{"GITOPS_APPLIED", "SUCCEEDED", "FAILED", "BLOCKED", "CANCELLED"}, status)
}
func applyExternal(st *domain.State, c domain.Command, m domain.Meta) (any, domain.Meta, error) {
	switch c.Action {
	case "grant_connection":
		var input struct {
			ConnectionID string `json:"connection_id"`
		}
		if e := decode(c.Data, &input); e != nil {
			return nil, m, e
		}
		if !productExists(st, c.ProductID) || connection(st, input.ConnectionID) == nil {
			return nil, m, invalid("existing product and instance connection required")
		}
		for _, g := range st.ConnectionGrants {
			if g.ProductID == c.ProductID && g.ConnectionID == input.ConnectionID {
				return nil, m, invalid("connection grant already exists")
			}
		}
		v := domain.ConnectionGrant{Meta: m, ConnectionID: input.ConnectionID}
		st.ConnectionGrants = append(st.ConnectionGrants, v)
		return v, m, nil
	case "create_github_connection":
		var input struct {
			Name                  string `json:"name"`
			AppID                 int64  `json:"app_id"`
			InstallationID        int64  `json:"installation_id"`
			PrivateKeyRef         string `json:"private_key_ref"`
			Owner                 string `json:"owner"`
			APIURL                string `json:"api_url"`
			RegistryCredentialRef string `json:"registry_credential_ref"`
		}
		if e := decode(c.Data, &input); e != nil {
			return nil, m, e
		}
		if c.ProductID != "" && !productExists(st, c.ProductID) {
			return nil, m, missing("product", c.ProductID)
		}
		if strings.TrimSpace(input.Name) == "" || input.AppID <= 0 || input.InstallationID <= 0 || input.Owner == "" || !secretRef(input.PrivateKeyRef) {
			return nil, m, invalid("name, app_id, installation_id, owner and env:/file: private_key_ref required")
		}
		if input.APIURL == "" {
			input.APIURL = "https://api.github.com"
		}
		u, e := url.Parse(input.APIURL)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, m, invalid("api_url must be an HTTPS API origin/base")
		}
		if input.RegistryCredentialRef != "" {
			return nil, m, invalid("permanent registry credentials are not supported; use the correlated Actions report")
		}
		v := domain.GitHubConnection{Meta: m, Name: input.Name, Config: delivery.Connection{ID: m.ID, AppID: input.AppID, InstallationID: input.InstallationID, PrivateKeyRef: input.PrivateKeyRef, Owner: input.Owner, APIURL: strings.TrimRight(input.APIURL, "/")}}
		v.ProductID = ""
		if c.ProductID != "" {
			gm := m
			gm.ID = id()
			st.ConnectionGrants = append(st.ConnectionGrants, domain.ConnectionGrant{Meta: gm, ConnectionID: v.ID})
		}
		st.GitHubConnections = append(st.GitHubConnections, v)
		return v, m, nil
	case "import_repository":
		var input struct {
			BaseBranch    string `json:"base_branch"`
			ConnectionID  string `json:"connection_id"`
			FullName      string `json:"full_name"`
			Role          string `json:"role"`
			DefaultBranch string `json:"default_branch"`
			URL           string `json:"url"`
		}
		if e := decode(c.Data, &input); e != nil {
			return nil, m, e
		}
		if input.BaseBranch == "" {
			input.BaseBranch = input.DefaultBranch
		}
		if !safeBranch(input.BaseBranch) {
			return nil, m, invalid("valid base_branch required")
		}
		conn, e := scopedConnection(st, input.ConnectionID, c.ProductID)
		if e != nil {
			return nil, m, e
		}
		if !repositoryName.MatchString(input.FullName) || !strings.EqualFold(strings.Split(input.FullName, "/")[0], conn.Config.Owner) || !slices.Contains(domain.RepositoryRoles(), input.Role) || !safeBranch(input.DefaultBranch) {
			return nil, m, invalid("repository owner, valid role and default_branch required")
		}
		for _, r := range st.RepositoryBindings {
			if r.ProductID == c.ProductID && strings.EqualFold(r.FullName, input.FullName) {
				return nil, m, invalid("repository already imported")
			}
		}
		registered := registeredRepository(st, input.ConnectionID, input.FullName)
		if registered == nil {
			rm := m
			rm.ID = id()
			rm.ProductID = ""
			st.RegisteredRepositories = append(st.RegisteredRepositories, domain.RegisteredRepository{Meta: rm, ConnectionID: input.ConnectionID, FullName: input.FullName, DefaultBranch: input.DefaultBranch, URL: input.URL})
			registered = &st.RegisteredRepositories[len(st.RegisteredRepositories)-1]
		}
		associateRegistryConnection(registered, conn)
		repo := domain.Repository{Role: input.Role, RegisteredRepositoryID: registered.ID, Meta: m, Name: input.FullName, URL: input.URL, Provider: "github"}
		if repo.URL == "" {
			repo.URL = "https://github.com/" + input.FullName
		}
		bindingMeta := m
		bindingMeta.ID = id()
		binding := domain.RepositoryBinding{BaseBranch: input.BaseBranch, Meta: bindingMeta, RepositoryID: repo.ID, ConnectionID: input.ConnectionID, FullName: input.FullName, Role: input.Role, DefaultBranch: input.DefaultBranch}
		st.Repositories = append(st.Repositories, repo)
		st.RepositoryBindings = append(st.RepositoryBindings, binding)
		return repo, m, nil
	case "configure_component":
		var input struct {
			RebuildMissing  bool              `json:"rebuild_missing"`
			ApplicationID   string            `json:"application_id"`
			ConnectionID    string            `json:"connection_id"`
			Workflow        string            `json:"workflow"`
			ImageRepository string            `json:"image_repository"`
			WorkflowRef     string            `json:"workflow_ref"`
			Inputs          map[string]string `json:"inputs"`
		}
		if e := decode(c.Data, &input); e != nil {
			return nil, m, e
		}
		app := applicationByID(st, input.ApplicationID)
		if app == nil || app.ProductID != c.ProductID || domain.ComponentKind(*app) != "APPLICATION" {
			return nil, m, invalid("application must belong to product")
		}
		binding := repositoryBinding(st, app.RepositoryID)
		if binding == nil || !domain.IsSourceRole(binding.Role) || binding.ConnectionID != input.ConnectionID {
			return nil, m, invalid("component requires SOURCE repository and its connection")
		}
		if _, e := scopedConnection(st, input.ConnectionID, c.ProductID); e != nil {
			return nil, m, e
		}
		if input.Workflow == "" || strings.ContainsAny(input.Workflow, "\n\r") || !imageName.MatchString(input.ImageRepository) {
			return nil, m, invalid("workflow and ghcr.io image_repository required")
		}
		if input.WorkflowRef != "" && !safeBranch(input.WorkflowRef) {
			return nil, m, invalid("invalid workflow_ref")
		}
		for key := range input.Inputs {
			if slices.Contains([]string{"source_sha", "image_tag", "operation_id", "image_repository"}, key) {
				return nil, m, invalid("reserved workflow input %s", key)
			}
		}
		v := domain.ComponentBuild{RebuildMissing: input.RebuildMissing, Meta: m, ApplicationID: app.ID, ConnectionID: input.ConnectionID, Workflow: input.Workflow, ImageRepository: input.ImageRepository, WorkflowRef: input.WorkflowRef, Inputs: input.Inputs}
		st.ComponentBuilds = append(st.ComponentBuilds, v)
		return v, m, nil
	case "configure_environment":
		var input struct {
			EnvironmentID string `json:"environment_id"`
			ApplicationID string `json:"application_id"`
			Purpose       string `json:"purpose"`
			ConnectionID  string `json:"connection_id"`
			RepositoryID  string `json:"repository_id"`
			Ref           string `json:"ref"`
			Path          string `json:"path"`
			ImageField    string `json:"image_field"`
			DigestField   string `json:"digest_field"`
			AllowDeploy   bool   `json:"allow_deploy"`
		}
		if e := decode(c.Data, &input); e != nil {
			return nil, m, e
		}
		env := environmentByID(st, input.EnvironmentID)
		app := applicationByID(st, input.ApplicationID)
		if env == nil || app == nil || env.ProductID != c.ProductID || app.ProductID != c.ProductID || domain.ComponentKind(*app) != "APPLICATION" {
			return nil, m, invalid("environment and application must belong to product")
		}
		binding := repositoryBinding(st, input.RepositoryID)
		if binding == nil || binding.ProductID != c.ProductID || binding.Role != "GITOPS" || binding.ConnectionID != input.ConnectionID {
			return nil, m, invalid("explicit GITOPS repository and its connection required")
		}
		if _, e := scopedConnection(st, input.ConnectionID, c.ProductID); e != nil {
			return nil, m, e
		}
		if !slices.Contains([]string{"DEV", "TEST", "PROD"}, input.Purpose) || !safeBranch(input.Ref) || input.Path == "" || path.IsAbs(input.Path) || path.Clean(input.Path) != input.Path || strings.HasPrefix(input.Path, "../") || input.ImageField == "" {
			return nil, m, invalid("explicit DEV/TEST/PROD purpose with ref, clean relative path and image_field required")
		}
		v := domain.EnvironmentBinding{Meta: m, EnvironmentID: input.EnvironmentID, ApplicationID: input.ApplicationID, Purpose: input.Purpose, ConnectionID: input.ConnectionID, RepositoryID: input.RepositoryID, Ref: input.Ref, Path: input.Path, ImageField: input.ImageField, DigestField: input.DigestField, AllowDeploy: input.AllowDeploy}
		st.EnvironmentBindings = append(st.EnvironmentBindings, v)
		return v, m, nil
	case "refresh_integration_git":
		var input struct{}
		if e := decode(c.Data, &input); e != nil {
			return nil, m, e
		}
		in := integration(st, c.IntegrationID)
		if in == nil {
			return nil, m, missing("integration", c.IntegrationID)
		}
		v := domain.ExternalOperation{Meta: m, Kind: "REFRESH_GIT", IntegrationID: in.ID, Status: "PENDING", RequestedBy: c.Actor, Phase: "OBSERVE", NextAttemptAt: m.CreatedAt}
		st.Operations = append(st.Operations, v)
		return v, m, nil
	case "deploy_integration", "deploy_existing_artifact":
		var input struct {
			ArtifactID     string `json:"artifact_id"`
			EnvironmentID  string `json:"environment_id"`
			ApplicationID  string `json:"application_id"`
			RevisionID     string `json:"revision_id"`
			ExpectedDigest string `json:"expected_digest"`
		}
		if e := decode(c.Data, &input); e != nil {
			return nil, m, e
		}
		in := integration(st, c.IntegrationID)
		if in == nil {
			return nil, m, missing("integration", c.IntegrationID)
		}
		if input.ApplicationID == "" {
			candidates := map[string]bool{}
			for _, binding := range st.EnvironmentBindings {
				if binding.EnvironmentID == input.EnvironmentID && binding.ProductID == in.ProductID {
					candidates[binding.ApplicationID] = true
				}
			}
			if len(candidates) != 1 {
				return nil, m, invalid("application_id required when environment has multiple components")
			}
			for appID := range candidates {
				input.ApplicationID = appID
			}
		}
		app := applicationByID(st, input.ApplicationID)
		env := environmentByID(st, input.EnvironmentID)
		if app == nil || app.ProductID != in.ProductID || env == nil || env.ProductID != in.ProductID || domain.ComponentKind(*app) != "APPLICATION" {
			return nil, m, invalid("component and environment must belong to integration product")
		}
		if len(in.Repositories) > 0 && !slices.Contains(in.Repositories, app.RepositoryID) {
			return nil, m, invalid("component repository not in integration scope")
		}
		f := feature(st, in.FeatureID)
		if f != nil && len(f.Repositories) > 0 && !slices.Contains(f.Repositories, app.RepositoryID) {
			return nil, m, invalid("component repository not in feature scope")
		}
		revision := revisionByID(st, input.RevisionID)
		if input.RevisionID != "" && (revision == nil || revision.IntegrationID != in.ID || revision.RepositoryID != app.RepositoryID) {
			return nil, m, invalid("revision must match integration and component repository")
		}
		if revision == nil {
			branch := ""
			for _, b := range in.Branches {
				if b.RepositoryID == app.RepositoryID {
					if branch != "" && branch != b.Name {
						return nil, m, invalid("bind one source branch for deployment")
					}
					branch = b.Name
				}
			}
			if branch == "" {
				for n := len(st.IntegrationRevisions) - 1; n >= 0; n-- {
					r := st.IntegrationRevisions[n]
					if r.IntegrationID == in.ID && r.RepositoryID == app.RepositoryID {
						branch = r.Branch
						break
					}
				}
			}
			if branch == "" {
				return nil, m, invalid("bind an integration branch before deployment")
			}
			revision = &domain.IntegrationRevision{Meta: domain.Meta{ProductID: in.ProductID, FeatureID: in.FeatureID}, IntegrationID: in.ID, RepositoryID: app.RepositoryID, Branch: branch}
		}

		build := componentBuild(st, app.ID)
		target := environmentBinding(st, env.ID, app.ID)
		source := repositoryBinding(st, app.RepositoryID)
		if build == nil || target == nil || source == nil || target.Purpose != "DEV" || !target.AllowDeploy {
			return nil, m, invalid("component build and explicit enabled DEV mapping required")
		}
		gitops := repositoryBinding(st, target.RepositoryID)
		sc, e := scopedConnection(st, build.ConnectionID, in.ProductID)
		if e != nil {
			return nil, m, e
		}
		gc, e := scopedConnection(st, target.ConnectionID, in.ProductID)
		if e != nil {
			return nil, m, e
		}
		if gitops != nil && (strings.Contains(repositoryLocator(st, *source), "/") || strings.Contains(repositoryLocator(st, *gitops), "/")) {
			return nil, m, invalid("discover provider repositories before deployment to pin numeric repository identities")
		}
		if gitops == nil {
			return nil, m, invalid("GITOPS repository binding missing")
		}
		if input.ExpectedDigest != "" && !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(input.ExpectedDigest) {
			return nil, m, invalid("expected_digest requires sha256 digest")
		}
		for _, op := range st.Operations {
			if op.Kind == "DEPLOY" && op.EnvironmentID == env.ID && op.ApplicationID == app.ID && !terminalOperation(op.Status) {
				return nil, m, invalid("deployment already active for environment/component")
			}
		}
		inputs := map[string]string{}
		for k, v := range build.Inputs {
			inputs[k] = v
		}
		request := delivery.BuildRequest{Repository: repositoryLocator(st, *source), Workflow: build.Workflow, Ref: build.WorkflowRef, SourceSHA: revision.HeadCommit, ImageTag: "rcp-" + revision.HeadCommit, OperationID: m.ID, Inputs: inputs, RequestedAt: m.CreatedAt}
		if request.Ref == "" {
			request.Ref = source.DefaultBranch
		}
		request.Inputs["source_sha"] = revision.HeadCommit
		request.Inputs["image_tag"] = request.ImageTag
		request.Inputs["operation_id"] = m.ID
		request.Inputs["image_repository"] = build.ImageRepository
		snapshot := &domain.DeliverySnapshot{Revision: *revision, Application: *app, Build: *build, Target: *target, Environment: *env, Source: *source, GitOps: *gitops, SourceConnection: sc.Config, GitOpsConnection: gc.Config, BuildRequest: request, GitOpsRequest: delivery.GitOpsRequest{Repository: repositoryLocator(st, *gitops), Ref: target.Ref, Path: target.Path, ImageRepository: build.ImageRepository, ImageField: target.ImageField, DigestField: target.DigestField}}
		v := domain.ExternalOperation{Meta: m, Kind: "DEPLOY", IntegrationID: in.ID, EnvironmentID: env.ID, ApplicationID: app.ID, Status: "PENDING", RequestedBy: c.Actor, Phase: "PREFLIGHT", Snapshot: snapshot, ExpectedDigest: input.ExpectedDigest, NextAttemptAt: time.Now().UTC()}
		if c.Action == "deploy_existing_artifact" {
			known, err := validatedExistingArtifact(st, input.ArtifactID, in.ProductID, app.ID, app.RepositoryID, revision.HeadCommit, build.ImageRepository)
			if err != nil {
				return nil, m, err
			}
			if input.ExpectedDigest != "" && input.ExpectedDigest != known.Digest {
				return nil, m, invalid("expected digest differs from selected artifact")
			}
			v.ExistingArtifact = known
			v.ExpectedDigest = known.Digest
		} else if input.ArtifactID != "" {
			return nil, m, invalid("artifact_id requires deploy_existing_artifact")
		}
		st.Operations = append(st.Operations, v)
		return v, m, nil
	}
	return nil, m, invalid("unknown external command")
}
