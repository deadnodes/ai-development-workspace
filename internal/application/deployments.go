package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/buildregistry"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/providers/forgejo"
	"releasecontrol/internal/providers/github"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

type DeploymentSource interface {
	CommitStatus(context.Context, buildconfig.Connection, string, string) (forgejo.CommitStatus, error)
}
type DeploymentRegistry interface {
	HeadDigest(context.Context, buildconfig.Registry, string, string) error
}
type DeploymentGitOps interface {
	ProposeDeployment(context.Context, buildconfig.GitOps, github.DeployPlan) (github.DeployProposal, error)
	FindDeployment(context.Context, buildconfig.GitOps, string) (github.DeployPR, error)
	ReadDeployment(context.Context, buildconfig.GitOps, int) (github.DeployPR, error)
	VerifyDeployment(context.Context, buildconfig.GitOps, github.DeployPlan, github.DeployPR) error
	MergeDeployment(context.Context, buildconfig.GitOps, int, string) (string, error)
}
type deployRuntime struct {
	source   DeploymentSource
	registry DeploymentRegistry
	gitops   DeploymentGitOps
	mu       sync.Mutex
}

func (s *Service) ConfigureDeployments(source DeploymentSource, registry DeploymentRegistry, gitops DeploymentGitOps) error {
	if s.builds == nil || source == nil || registry == nil || gitops == nil {
		return invalid("deployment adapters and build authority required")
	}
	s.deployments = &deployRuntime{source: source, registry: registry, gitops: gitops}
	return nil
}
func deploymentTerminal(d domain.Deployment) bool {
	return d.Status == "SUCCEEDED" || d.Status == "FAILED"
}
func deploymentEvent(st *domain.State, d domain.Deployment, action string) {
	st.Events = append(st.Events, domain.Event{ID: id(), Action: action, Actor: d.Actor, At: d.UpdatedAt, EntityID: d.ID, ProductID: d.ProductID, Data: domain.Command{Action: action, ProductID: d.ProductID, ID: d.ID, Actor: d.Actor, Data: map[string]any{"status": d.Status, "failure_code": d.FailureCode, "pr_url": d.PRURL, "merge_commit_sha": d.MergeCommitSHA}}})
}
func (s *Service) DeployBuild(ctx context.Context, in domain.DeployBuildRequest) (domain.Deployment, error) {
	var out domain.Deployment
	if in.ProductID == "" || in.BuildID == "" || in.EnvironmentID != "dev" || len(in.RequestKey) > 128 {
		return out, invalid("deploy_build requires Product, build and dev environment; production rejected")
	}
	if e := authorize(ctx, "write", in.ProductID); e != nil {
		return out, e
	}
	if s.builds == nil || s.deployments == nil {
		return out, invalid("deployments not configured")
	}
	e := s.store.Update(ctx, func(st *domain.State) error {
		for _, r := range st.DeploymentReceipts {
			if in.RequestKey != "" && r.ProductID == in.ProductID && r.RequestKey == in.RequestKey {
				if r.BuildID != in.BuildID || r.EnvironmentID != in.EnvironmentID {
					return invalid("REQUEST_KEY_CONFLICT")
				}
				for _, d := range st.Deployments {
					if d.ID == r.DeploymentID && d.ProductID == in.ProductID {
						out = d
						return nil
					}
				}
				return invalid("deployment receipt missing handle")
			}
		}
		var b domain.Build
		for _, v := range st.Builds {
			if v.ID == in.BuildID && v.ProductID == in.ProductID {
				b = v
				break
			}
		}
		if b.ID == "" {
			return missing("build", in.BuildID)
		}
		p, ok := s.builds.config.Profile(b.ProfileID)
		gitopsAuthority, gitopsOK := s.builds.config.DevGitOps()
		if !ok || p.ProductID != in.ProductID || len(p.Deployments["dev"].Files) == 0 || !gitopsOK {
			return invalid("Dev target not configured")
		}
		if !buildconfig.SHA.MatchString(b.SourceSHA) || b.Status != "SUCCEEDED" || b.RestoreQuarantined || b.AuthorityHash != s.builds.hash || len(b.Images) != len(p.Images) {
			return invalid("successful build with current authority required")
		}
		origin, _ := url.Parse(s.builds.config.Registry(p.RegistryID).Origin)
		identities := []string{}
		seen := map[string]bool{}
		for _, image := range b.Images {
			configured := ""
			for _, i := range p.Images {
				if i.ID == image.ID {
					configured = i.Repository
				}
			}
			prefix := origin.Host + "/" + configured + ":"
			ref, digest, ok := strings.Cut(image.ImageRef, "@")
			if configured == "" || seen[image.ID] || !ok || digest != image.Digest || !deployDigest(image.Digest) || !strings.HasPrefix(ref, prefix) || !validResolvedTag(strings.TrimPrefix(ref, prefix)) {
				return invalid("invalid build image provenance")
			}
			seen[image.ID] = true
			identities = append(identities, image.ID+"@"+image.Digest)
		}
		sort.Strings(identities)
		identity := buildFingerprint(identities)
		bind := func(d domain.Deployment) {
			if in.RequestKey != "" {
				st.DeploymentReceipts = append(st.DeploymentReceipts, domain.DeploymentReceipt{Meta: domain.Meta{ID: id(), ProductID: in.ProductID, Actor: attributedActor(ctx, "agent"), CreatedAt: time.Now().UTC()}, RequestKey: in.RequestKey, BuildID: in.BuildID, EnvironmentID: in.EnvironmentID, DeploymentID: d.ID})
			}
		}
		failed := false
		for _, d := range st.Deployments {
			if d.ProductID == in.ProductID && d.EnvironmentID == "dev" && d.ProfileID == p.ID && d.Identity == identity {
				if d.Status != "FAILED" {
					out = d
					bind(d)
					return nil
				}
				failed = true
			}
		}
		if failed && in.RequestKey == "" {
			return invalid("new request_key required to retry failed deployment")
		}
		now := time.Now().UTC()
		out = domain.Deployment{Meta: domain.Meta{ID: id(), ProductID: in.ProductID, Actor: attributedActor(ctx, "agent"), CreatedAt: now, UpdatedAt: now}, BuildID: b.ID, ProfileID: p.ID, EnvironmentID: "dev", RequestKey: in.RequestKey, AuthorityHash: s.builds.hash, Identity: identity, Status: "PENDING_CHECKS", Images: append([]domain.BuildImage(nil), b.Images...), SourceSHA: b.SourceSHA, RunURL: b.RunURL, Deadline: now.Add(time.Duration(s.builds.config.TimeoutSeconds) * time.Second)}
		out.GitOpsRepository = gitopsAuthority.Repository
		for _, f := range p.Deployments["dev"].Files {
			if !slices.Contains(out.TargetFiles, f.Path) {
				out.TargetFiles = append(out.TargetFiles, f.Path)
			}
		}
		sort.Strings(out.TargetFiles)
		out.Branch = "rcp/deploy/" + out.ID
		st.Deployments = append(st.Deployments, out)
		bind(out)
		deploymentEvent(st, out, "deploy_build")
		return nil
	})
	return out, e
}
func deployDigest(s string) bool {
	if len(s) != 71 || !strings.HasPrefix(s, "sha256:") {
		return false
	}
	for _, c := range s[7:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func validResolvedTag(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_.-", c)) {
			return false
		}
	}
	return s[0] != '.' && s[0] != '-'
}
func (s *Service) GetDeployment(ctx context.Context, in domain.DeploymentLookup) (domain.Deployment, error) {
	if in.ProductID == "" || in.DeploymentID == "" {
		return domain.Deployment{}, invalid("Product and deployment required")
	}
	if e := authorize(ctx, "read", in.ProductID); e != nil {
		return domain.Deployment{}, e
	}
	st, e := s.store.Read(ctx)
	if e != nil {
		return domain.Deployment{}, e
	}
	for _, d := range st.Deployments {
		if d.ProductID == in.ProductID && d.ID == in.DeploymentID {
			return d, nil
		}
	}
	return domain.Deployment{}, missing("deployment", in.DeploymentID)
}

type deploymentCursor struct{ Product, Environment, Profile, After string }

func (s *Service) ListDeployments(ctx context.Context, in domain.ListDeploymentsRequest) (domain.DeploymentPage, error) {
	out := domain.DeploymentPage{Items: []domain.Deployment{}}
	if in.ProductID == "" || in.Limit < 0 || in.Limit > 100 || in.EnvironmentID != "" && in.EnvironmentID != "dev" {
		return out, invalid("invalid deployment scope or bounds")
	}
	if e := authorize(ctx, "read", in.ProductID); e != nil {
		return out, e
	}
	if in.Limit == 0 {
		in.Limit = 25
	}
	after := ""
	if in.Cursor != "" {
		raw, e := base64.RawURLEncoding.DecodeString(in.Cursor)
		var c deploymentCursor
		if len(in.Cursor) > 2048 || e != nil || json.Unmarshal(raw, &c) != nil || c.Product != in.ProductID || c.Environment != in.EnvironmentID || c.Profile != in.ProfileID {
			return out, invalid("invalid deployment cursor")
		}
		after = c.After
	}
	st, e := s.store.Read(ctx)
	if e != nil {
		return out, e
	}
	sort.Slice(st.Deployments, func(i, j int) bool { return st.Deployments[i].ID < st.Deployments[j].ID })
	for _, d := range st.Deployments {
		if d.ProductID != in.ProductID || in.EnvironmentID != "" && d.EnvironmentID != in.EnvironmentID || in.ProfileID != "" && d.ProfileID != in.ProfileID || d.ID <= after {
			continue
		}
		if len(out.Items) == in.Limit {
			raw, _ := json.Marshal(deploymentCursor{in.ProductID, in.EnvironmentID, in.ProfileID, out.Items[len(out.Items)-1].ID})
			out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
			break
		}
		out.Items = append(out.Items, d)
	}
	return out, nil
}
func (s *Service) advanceDeployment(ctx context.Context, old domain.Deployment, next *domain.Deployment, claim bool) error {
	return s.store.Update(ctx, func(st *domain.State) error {
		for i, current := range st.Deployments {
			if current.ID != old.ID {
				continue
			}
			if buildFingerprint(current) != buildFingerprint(old) {
				return invalid("deployment changed")
			}
			if claim {
				for _, other := range st.Deployments {
					if other.ID != old.ID && !deploymentTerminal(other) && other.Status != "PENDING_CHECKS" && deploymentOverlap(s.builds.config, old, other) {
						return invalid("deployment target busy")
					}
				}
			}
			next.UpdatedAt = time.Now().UTC()
			st.Deployments[i] = *next
			deploymentEvent(st, *next, "observe_deployment")
			return nil
		}
		return missing("deployment", old.ID)
	})
}
func deploymentOverlap(_ *buildconfig.Config, a, b domain.Deployment) bool {
	if a.EnvironmentID != b.EnvironmentID {
		return false
	}
	if a.GitOpsRepository != "" && b.GitOpsRepository != "" && a.GitOpsRepository != b.GitOpsRepository {
		return false
	}
	if len(a.TargetFiles) == 0 || len(b.TargetFiles) == 0 {
		return true
	}
	for _, path := range a.TargetFiles {
		if slices.Contains(b.TargetFiles, path) {
			return true
		}
	}
	return false
}
func ciVerdict(v forgejo.CommitStatus, required []string) (green, failed bool, contexts map[string]string) {
	contexts = map[string]string{}
	for _, s := range v.Statuses {
		if _, exists := contexts[s.Context]; !exists {
			contexts[s.Context] = s.Status
		}
	}
	if len(required) == 0 {
		return v.State == "success", v.State == "failure" || v.State == "error", contexts
	}
	green = true
	for _, key := range required {
		state := contexts[key]
		if state != "success" {
			green = false
		}
		if state == "failure" || state == "error" {
			failed = true
		}
	}
	return
}
func deploymentPlan(d domain.Deployment, p buildconfig.Profile) github.DeployPlan {
	plan := github.DeployPlan{ID: d.ID, Branch: d.Branch, ExpectedHead: d.HeadSHA}
	evidence, _ := json.Marshal(d.CIContexts)
	plan.Body = fmt.Sprintf("RCP Dev deployment %s\nBuild: %s\nSource SHA: %s\nRun: %s\nCI state: %s\nCI contexts: %s\nChecked: %v\nRegistry checked: %v\n", d.ID, d.BuildID, d.SourceSHA, d.RunURL, d.CIState, evidence, d.CheckedAt, d.RegistryCheckedAt)
	for _, i := range d.Images {
		plan.Body += fmt.Sprintf("Image %s: %s; digest %s\n", i.ID, i.ImageRef, i.Digest)
	}
	byPath := map[string][]github.DeployTag{}
	for _, f := range p.Deployments["dev"].Files {
		for _, i := range d.Images {
			if i.ID == f.ImageID {
				ref, _, _ := strings.Cut(i.ImageRef, "@")
				tag := ref[strings.LastIndex(ref, ":")+1:]
				value := strings.ReplaceAll(strings.ReplaceAll(f.TagFormat, "{tag}", tag), "{digest}", i.Digest)
				if f.Format == "container_image" {
					value = tag + "@" + i.Digest
				}
				byPath[f.Path] = append(byPath[f.Path], github.DeployTag{Repository: f.ImageRepository, Value: value, MinMatches: f.MinMatches, Format: f.Format})
			}
		}
	}
	paths := []string{}
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		plan.Files = append(plan.Files, github.DeployFile{Path: path, Tags: byPath[path]})
	}
	return plan
}
func failedDeployment(d domain.Deployment, code string) domain.Deployment {
	d.Status = "FAILED"
	d.FailureCode = code
	now := time.Now().UTC()
	d.FinishedAt = &now
	return d
}
func succeededDeployment(d domain.Deployment) domain.Deployment {
	d.Status = "SUCCEEDED"
	d.FailureCode = ""
	d.UncertainAction = ""
	now := time.Now().UTC()
	d.FinishedAt = &now
	return d
}
func mergeConflict(e error) bool {
	var api *github.APIError
	return errors.As(e, &api) && (api.Status == 405 || api.Status == 409)
}
func (s *Service) DispatchDeploymentsOnce(ctx context.Context) error {
	a := s.deployments
	if a == nil || !a.mu.TryLock() {
		return nil
	}
	defer a.mu.Unlock()
	st, e := s.store.Read(ctx)
	if e != nil {
		return e
	}
	sort.SliceStable(st.Deployments, func(i, j int) bool {
		if st.Deployments[i].CreatedAt.Equal(st.Deployments[j].CreatedAt) {
			return st.Deployments[i].ID < st.Deployments[j].ID
		}
		return st.Deployments[i].CreatedAt.Before(st.Deployments[j].CreatedAt)
	})
	for _, d := range st.Deployments {
		if deploymentTerminal(d) {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p, ok := s.builds.config.Profile(d.ProfileID)
		g, gok := s.builds.config.DevGitOps()
		if !ok || !gok || d.AuthorityHash != s.builds.hash || p.ProductID != d.ProductID || d.EnvironmentID != "dev" {
			if d.FailureCode != "DEPLOYMENT_AUTHORITY_CHANGED" {
				next := d
				if d.Status == "PENDING_CHECKS" {
					next = failedDeployment(d, "DEPLOYMENT_AUTHORITY_CHANGED")
				} else {
					next.Status = "RECONCILIATION_REQUIRED"
					next.FailureCode = "DEPLOYMENT_AUTHORITY_CHANGED"
				}
				if e = s.advanceDeployment(ctx, d, &next, false); e != nil {
					return e
				}
			}
			continue
		}
		if e = s.processDeployment(ctx, d, p, g); e != nil {
			return e
		}
	}
	return nil
}
func (s *Service) processDeployment(ctx context.Context, d domain.Deployment, p buildconfig.Profile, g buildconfig.GitOps) error {
	a := s.deployments
	save := func(next domain.Deployment, claim bool) error {
		e := s.advanceDeployment(ctx, d, &next, claim)
		if e == nil {
			d = next
		}
		return e
	}
	if d.Status == "PENDING_CHECKS" {
		v, e := a.source.CommitStatus(ctx, s.builds.config.Connection(p.ConnectionID), p.Repository, d.SourceSHA)
		if e != nil {
			next := d
			next.FailureCode = "CI_UNAVAILABLE"
			if time.Now().After(d.Deadline) && !errors.Is(e, forgejo.ErrStatusPageLimit) {
				next = failedDeployment(next, "CI_NOT_GREEN")
			}
			return save(next, false)
		}
		green, failed, contexts := ciVerdict(v, p.RequiredStatusContexts)
		next := d
		now := time.Now().UTC()
		next.CheckedAt = &now
		next.CIState = v.State
		next.CIContexts = contexts
		next.FailureCode = ""
		if failed || !green && now.After(d.Deadline) {
			return save(failedDeployment(next, "CI_NOT_GREEN"), false)
		}
		if !green {
			return save(next, false)
		}
		for _, i := range d.Images {
			repo := ""
			for _, image := range p.Images {
				if image.ID == i.ID {
					repo = image.Repository
				}
			}
			if e = a.registry.HeadDigest(ctx, s.builds.config.Registry(p.RegistryID), repo, i.Digest); e != nil {
				if errors.Is(e, buildregistry.ErrMissing) {
					return save(failedDeployment(next, "ARTIFACT_MISSING"), false)
				}
				next.FailureCode = "REGISTRY_UNAVAILABLE"
				if time.Now().After(d.Deadline) {
					next = failedDeployment(next, "ARTIFACT_UNAVAILABLE")
				}
				return save(next, false)
			}
		}
		registryNow := time.Now().UTC()
		next.RegistryCheckedAt = &registryNow
		next.Status = "PROPOSING"
		if e = save(next, true); e != nil {
			return nil
		}
	}
	if d.Status == "PROPOSING" {
		// Persist uncertainty before every mutating compound request.
		next := d
		next.Status = "RECONCILIATION_REQUIRED"
		next.UncertainAction = "propose"
		next.FailureCode = "PR_UNCERTAIN"
		if e := save(next, false); e != nil {
			return e
		}
		proposal, e := a.gitops.ProposeDeployment(ctx, g, deploymentPlan(d, p))
		if e != nil {
			if errors.Is(e, github.ErrTargetMissing) {
				return save(failedDeployment(d, "GITOPS_TARGET_MISSING"), false)
			}
			return nil
		}
		next = d
		next.BaseSHA = proposal.BaseSHA
		if proposal.AlreadyApplied {
			next.AlreadyApplied = true
			return save(succeededDeployment(next), false)
		}
		next.PRNumber = proposal.PR.Number
		next.PRURL = proposal.PR.URL
		next.HeadSHA = proposal.HeadSHA
		next.Status = "MERGING"
		next.UncertainAction = ""
		next.FailureCode = ""
		if e = save(next, false); e != nil {
			return e
		}
	}
	if d.Status == "RECONCILIATION_REQUIRED" {
		if d.UncertainAction == "restored" {
			return nil
		}
		pr, e := a.gitops.FindDeployment(ctx, g, d.Branch)
		if e != nil {
			return nil
		}
		if pr.Head.Ref != d.Branch || pr.Base.Ref != "main" || pr.Head.Repo.FullName != g.Repository || pr.Base.Repo.FullName != g.Repository {
			return nil
		}
		if d.HeadSHA != "" && d.UncertainAction == "merge" && pr.Head.SHA != d.HeadSHA {
			return nil
		}
		next := d
		next.PRNumber = pr.Number
		next.PRURL = pr.URL
		if pr.Merged {
			if !buildconfig.SHA.MatchString(pr.MergeSHA) {
				return nil
			}
			next.HeadSHA = pr.Head.SHA
			if d.UncertainAction != "merge" {
				if e := a.gitops.VerifyDeployment(ctx, g, deploymentPlan(next, p), pr); e != nil {
					return nil
				}
			}
			next.MergeCommitSHA = pr.MergeSHA
			return save(succeededDeployment(next), false)
		}
		// A lost merge response is read-only forever until merged evidence arrives.
		if d.UncertainAction == "merge" {
			return nil
		}
		if pr.State != "open" {
			return save(failedDeployment(next, "MERGE_CONFLICT"), false)
		}
		next.HeadSHA = pr.Head.SHA
		next.BaseSHA = pr.Base.SHA
		next.Status = "MERGING"
		next.UncertainAction = ""
		next.FailureCode = ""
		if e = save(next, false); e != nil {
			return e
		}
	}
	if d.Status != "MERGING" {
		return nil
	}
	pr, e := a.gitops.ReadDeployment(ctx, g, d.PRNumber)
	if e != nil {
		return nil
	}
	if pr.Merged {
		if pr.Head.SHA != d.HeadSHA || !buildconfig.SHA.MatchString(pr.MergeSHA) {
			return nil
		}
		next := d
		next.MergeCommitSHA = pr.MergeSHA
		return save(succeededDeployment(next), false)
	}
	e = a.gitops.VerifyDeployment(ctx, g, deploymentPlan(d, p), pr)
	if e == nil {
		next := d
		next.Status = "RECONCILIATION_REQUIRED"
		next.UncertainAction = "merge"
		next.FailureCode = "MERGE_UNCERTAIN"
		if e = save(next, false); e != nil {
			return e
		}
		sha, err := a.gitops.MergeDeployment(ctx, g, d.PRNumber, d.HeadSHA)
		if err == nil {
			next = d
			next.MergeCommitSHA = sha
			return save(succeededDeployment(next), false)
		}
		if !mergeConflict(err) {
			return nil
		}
		e = err
	}
	if mergeConflict(e) {
		if d.RetryCount >= 1 {
			return save(failedDeployment(d, "MERGE_CONFLICT"), false)
		}
		next := d
		next.RetryCount++
		next.Status = "PROPOSING"
		next.UncertainAction = ""
		next.FailureCode = ""
		return save(next, false)
	}
	if errors.Is(e, github.ErrConflict) {
		return save(failedDeployment(d, "GITOPS_SCOPE_CHANGED"), false)
	}
	return nil
}
