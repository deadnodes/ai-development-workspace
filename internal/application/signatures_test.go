package application

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/imagesigning"
	"releasecontrol/internal/imagesigning/signingtest"
	"releasecontrol/internal/providers/forgejo"
	"strings"
	"testing"
	"time"
)

type signingForgejo struct {
	*fakeForgejo
	state string
}

func (f *signingForgejo) CommitStatus(context.Context, buildconfig.Connection, string, string) (forgejo.CommitStatus, error) {
	return forgejo.CommitStatus{State: f.state, Statuses: []forgejo.StatusContext{{Context: "ci", Status: f.state, TargetURL: "/o/r/actions/runs/1/jobs/1"}}}, nil
}
func signingFixture(t *testing.T) (*Service, *memoryStore, *signingtest.Fixture, *signingForgejo, domain.Build) {
	t.Helper()
	s, m, base, r := buildFixture(t)
	f := signingtest.New(t)
	cfg := buildConfig()
	cfg.Registries = []buildconfig.Registry{f.RegistryConfig()}
	cfg.Signing = f.Config()
	provider := &signingForgejo{base, "success"}
	if e := s.ConfigureBuilds(cfg, provider, r); e != nil {
		t.Fatal(e)
	}
	s.builds.signer = imagesigning.NewWithClient(f.Client())
	b := queue(t, s, "sign")
	digest := f.Image(b.SourceSHA)
	u, _ := url.Parse(f.Registry.URL)
	images := []domain.BuildImage{{ID: "app", Digest: digest, ImageRef: u.Host + "/o/r:main-aaaaaaaa@" + digest}}
	if e := s.advanceBuild(context.Background(), b, artifactBuild(b, images), false); e != nil {
		t.Fatal(e)
	}
	b, e := s.GetBuild(context.Background(), domain.BuildLookup{ProductID: "p", BuildID: b.ID})
	if e != nil {
		t.Fatal(e)
	}
	return s, m, f, provider, b
}
func TestSigningDispatcherAndDeployVerification(t *testing.T) {
	s, _, f, _, b := signingFixture(t)
	ctx := context.Background()
	if b.SigningStatus != "PENDING" {
		t.Fatal(b)
	}
	if e := s.SignBuildsOnce(ctx); e != nil {
		t.Fatal(e)
	}
	b, e := s.GetBuild(ctx, domain.BuildLookup{ProductID: "p", BuildID: b.ID})
	if e != nil || b.Status != "SUCCEEDED" || b.SigningStatus != "SIGNED" || len(b.Signatures) != 1 {
		t.Fatal(b, e)
	}
	p, _ := s.builds.config.Profile("app")
	s.builds.config.Signing.DeployKeys = map[string][]string{"dev": {"dev"}}
	d := domain.Deployment{Meta: domain.Meta{ProductID: "p"}, BuildID: b.ID, Images: b.Images, EnvironmentID: "dev"}
	if !s.deploymentSignatures(ctx, d, p) {
		t.Fatal("signed build rejected")
	}
	f.Mu.Lock()
	delete(f.Manifests, imagesigning.Tag(b.Images[0].Digest, "sig"))
	f.Mu.Unlock()
	if s.deploymentSignatures(ctx, d, p) {
		t.Fatal("missing signature accepted")
	}
}
func TestSigningRetryAndNoHistoryBackfill(t *testing.T) {
	s, m, f, _, b := signingFixture(t)
	ctx := context.Background()
	f.Unavailable = true
	if e := s.SignBuildsOnce(ctx); e != nil {
		t.Fatal(e)
	}
	b, _ = s.GetBuild(ctx, domain.BuildLookup{ProductID: "p", BuildID: b.ID})
	if b.SigningStatus != "PENDING" || b.SigningAttempts != 1 || !b.SigningNextAttemptAt.After(time.Now()) {
		t.Fatal(b)
	}
	if e := s.SignBuildsOnce(ctx); e != nil {
		t.Fatal(e)
	}
	_ = m.Update(ctx, func(st *domain.State) error {
		for i := range st.Builds {
			st.Builds[i].SigningNextAttemptAt = time.Time{}
			st.Builds[i].SigningStartedAt = time.Now().Add(-31 * time.Minute)
		}
		return nil
	})
	if e := s.SignBuildsOnce(ctx); e != nil {
		t.Fatal(e)
	}
	b, _ = s.GetBuild(ctx, domain.BuildLookup{ProductID: "p", BuildID: b.ID})
	if b.SigningStatus != "FAILED" {
		t.Fatal(b)
	}
	_ = m.Update(ctx, func(st *domain.State) error { st.Builds[0].SigningStatus = ""; return nil })
	f.Unavailable = false
	if e := s.SignBuildsOnce(ctx); e != nil {
		t.Fatal(e)
	}
	if f.Signs != 0 {
		t.Fatal("historical build signed")
	}
}
func TestSigningCIAndRules(t *testing.T) {
	for _, state := range []string{"pending", "failure", "success"} {
		t.Run(state, func(t *testing.T) {
			s, _, f, provider, b := signingFixture(t)
			provider.state = state
			s.builds.config.Signing.Rules[0].RequireStatusContexts = true
			s.builds.config.Profiles[0].RequiredStatusContexts = []string{"ci"}
			if e := s.SignBuildsOnce(context.Background()); e != nil {
				t.Fatal(e)
			}
			b, _ = s.GetBuild(context.Background(), domain.BuildLookup{ProductID: "p", BuildID: b.ID})
			want := map[string]string{"pending": "PENDING", "failure": "SKIPPED", "success": "SIGNED"}[state]
			if b.SigningStatus != want {
				t.Fatal(b)
			}
			if state != "success" && f.Signs != 0 {
				t.Fatal("CI bypass")
			}
		})
	}
}
func TestBackfillScopeReplayAndList(t *testing.T) {
	s, _, f, _, b := signingFixture(t)
	ctx := context.Background()
	u, _ := url.Parse(f.Registry.URL)
	in := domain.SignArtifactRequest{ProductID: "p", ProfileID: "app", ImageID: "app", ImageRef: u.Host + "/o/r@" + b.Images[0].Digest, KeyIDs: []string{"prod"}, Reason: "explicit legacy acceptance", RequestKey: "backfill"}
	out, e := s.SignArtifact(ctx, in)
	if e != nil {
		t.Fatal(e)
	}
	replay, e := s.SignArtifact(ctx, in)
	if e != nil || replay.ID != out.ID {
		t.Fatal(replay, e)
	}
	changed := in
	changed.Reason = "different acceptance"
	if _, e = s.SignArtifact(ctx, changed); e == nil {
		t.Fatal("receipt conflict accepted")
	}
	bad := in
	bad.RequestKey = "bad"
	bad.ImageRef = strings.Replace(in.ImageRef, "/o/r@", "/foreign/r@", 1)
	if _, e = s.SignArtifact(ctx, bad); e == nil {
		t.Fatal("foreign repo accepted")
	}
	bad = in
	bad.ProductID = "foreign"
	if _, e = s.SignArtifact(ctx, bad); e == nil {
		t.Fatal("cross product accepted")
	}
	if e = s.SignBuildsOnce(ctx); e != nil {
		t.Fatal(e)
	}
	page, e := s.ListSignatures(ctx, domain.ListSignaturesRequest{ProductID: "p", Digest: b.Images[0].Digest})
	if e != nil || len(page.Items) != 1 || page.Items[0].Status != "SIGNED" {
		t.Fatal(page, e)
	}
}
func TestSigningAuthorityExcluded(t *testing.T) {
	s, _, f, _, _ := signingFixture(t)
	before := s.builds.buildHashes["app"]
	cfg := cloneSigning(*s.builds.config)
	cfg.Signing.Keys = []buildconfig.SigningKey{f.Key("dev"), f.Key("prod")}
	cfg.Signing.Keys[0].PublicKeyPEM = cfg.Signing.Keys[1].PublicKeyPEM
	if e := s.ConfigureBuilds(&cfg, s.builds.provider, s.builds.registry); e != nil {
		t.Fatal(e)
	}
	if before != s.builds.buildHashes["app"] {
		t.Fatal("key rotation changed build authority")
	}
}

func TestDeployGateFailsBeforeGitOps(t *testing.T) {
	s, _, provider, in := deploymentFixture(t)
	f := signingtest.New(t)
	s.builds.config.Signing = f.Config()
	s.builds.config.Signing.RequireSignatureForDeploy = true
	s.builds.config.Signing.DeployKeys = map[string][]string{"dev": {"dev"}}
	ctx := context.Background()
	d, e := s.DeployBuild(ctx, in)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.DispatchDeploymentsOnce(ctx); e != nil {
		t.Fatal(e)
	}
	d, e = s.GetDeployment(ctx, domain.DeploymentLookup{ProductID: in.ProductID, DeploymentID: d.ID})
	if e != nil || d.Status != "FAILED" || d.FailureCode != "SIGNATURE_MISSING" {
		t.Fatal(d, e)
	}
	_ = provider
}

func TestBackfillRequiresWritePermission(t *testing.T) {
	s, _, _, _, _ := signingFixture(t)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{ID: "reader", Products: []string{"p"}, Permissions: []string{"read"}})
	if _, e := s.SignArtifact(ctx, domain.SignArtifactRequest{ProductID: "p"}); !errors.Is(e, auth.ErrForbidden) {
		t.Fatal(e)
	}
}
func TestSigningMismatchAndDefaultBranchRule(t *testing.T) {
	s, m, f, _, b := signingFixture(t)
	ctx := context.Background()
	f.Mismatch = true
	if e := s.SignBuildsOnce(ctx); e != nil {
		t.Fatal(e)
	}
	b, _ = s.GetBuild(ctx, domain.BuildLookup{ProductID: "p", BuildID: b.ID})
	if b.SigningStatus != "FAILED" || b.Signatures[0].FailureCode != "SIGNING_KEY_MISMATCH" || f.Writes != 0 {
		t.Fatal(b)
	}
	f.Mismatch = false
	s.builds.config.Signing.Rules[0].RequireDefaultBranch = true
	_ = m.Update(ctx, func(st *domain.State) error {
		b := &st.Builds[0]
		b.SigningStatus = "PENDING"
		b.Signatures = nil
		b.SigningNextAttemptAt = time.Time{}
		b.Branch = "feature"
		return nil
	})
	if e := s.SignBuildsOnce(ctx); e != nil {
		t.Fatal(e)
	}
	b, _ = s.GetBuild(ctx, domain.BuildLookup{ProductID: "p", BuildID: b.ID})
	if b.SigningStatus != "SKIPPED" || f.Signs != 0 {
		t.Fatal(b)
	}
}

func TestSigningProvenanceShape(t *testing.T) {
	s, _, _, _, b := signingFixture(t)
	p, _ := s.builds.config.Profile("app")
	predicate := s.buildPredicate(b, p, map[string]string{"ci": "success"})
	body := predicate.Body.(map[string]any)
	definition := body["buildDefinition"].(map[string]any)
	if predicate.Type != imagesigning.ProvenanceType || definition["buildType"] != "https://deadnodes.dev/rcp/forgejo-build/v1" {
		t.Fatal(predicate)
	}
	external := definition["externalParameters"].(map[string]string)
	if external["source_sha"] != b.SourceSHA || external["workflow"] != p.Workflow || external["dispatch_ref"] != p.DispatchRef || external["tag"] != "main-aaaaaaaa" {
		t.Fatal(external)
	}
	dependencies := definition["resolvedDependencies"].([]any)
	dep := dependencies[0].(map[string]any)
	if dep["uri"] != "git+https://git.example/o/r@refs/heads/main" || dep["digest"].(map[string]string)["gitCommit"] != b.SourceSHA {
		t.Fatal(dep)
	}
	details := body["runDetails"].(map[string]any)
	if details["builder"].(map[string]string)["id"] != s.builds.config.Signing.BuilderID {
		t.Fatal(details)
	}
	byproduct := details["byproducts"].([]any)[0].(map[string]any)
	if byproduct["annotations"].(map[string]any)["observed"].(map[string]string)["ci"] != "success" {
		t.Fatal(byproduct)
	}
}

func TestDeployProposingSignatureSnapshot(t *testing.T) {
	for _, signed := range []bool{true, false} {
		t.Run(fmt.Sprint("signed=", signed), func(t *testing.T) {
			s, m, fake, in := deploymentFixture(t)
			fixture := signingtest.New(t)
			s.builds.config.Registries = []buildconfig.Registry{fixture.RegistryConfig()}
			s.builds.config.Signing = fixture.Config()
			s.builds.config.Signing.RequireSignatureForDeploy = true
			s.builds.config.Signing.DeployKeys = map[string][]string{"dev": {"dev"}}
			s.builds.signer = imagesigning.NewWithClient(fixture.Client())
			b := &m.state.Builds[0]
			digest := fixture.Image(b.SourceSHA)
			b.Images = []domain.BuildImage{{ID: "app", Digest: digest, ImageRef: "registry.test/o/r:main-aaaaaaaa@" + digest}}
			b.SigningStatus = "PENDING"
			b.SigningStartedAt = time.Now().UTC()
			ctx := context.Background()
			if signed {
				if err := s.SignBuildsOnce(ctx); err != nil {
					t.Fatal(err)
				}
				if m.state.Builds[0].SigningStatus != "SIGNED" {
					t.Fatal(m.state.Builds[0])
				}
			}
			d, err := s.DeployBuild(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Update(ctx, func(st *domain.State) error { st.Deployments[0].Status = "PROPOSING"; return nil }); err != nil {
				t.Fatal(err)
			}
			// Keep the PR awaiting verification so the MERGING checkpoint is visible.
			fake.verifyErr = errors.New("checks pending")
			deployTick(t, s)
			got := m.state.Deployments[0]
			if got.ID != d.ID {
				t.Fatal(got)
			}
			if signed {
				if got.Status != "MERGING" || !got.SignatureVerified || fake.proposals != 1 {
					t.Fatal(got, fake.proposals)
				}
			} else if got.Status != "FAILED" || got.FailureCode != "SIGNATURE_MISSING" || got.SignatureVerified || fake.proposals != 0 {
				t.Fatal(got, fake.proposals)
			}
		})
	}
}
