package application

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/domain"
)

func cloneAuthorityConfig(t *testing.T, c *buildconfig.Config) *buildconfig.Config {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var out buildconfig.Config
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func TestProfileAuthorityScope(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		change                      func(*buildconfig.Config)
		buildChanged, deployChanged bool
	}{
		{"other build", func(c *buildconfig.Config) { c.Profiles[1].DispatchRef = "other" }, false, false},
		{"other deployment", func(c *buildconfig.Config) { c.Profiles[1].Deployments["dev"].Files[0].Path = "other.yaml" }, false, false},
		{"required status contexts", func(c *buildconfig.Config) { c.Profiles[0].RequiredStatusContexts = []string{"other-check"} }, false, true},
		{"own build", func(c *buildconfig.Config) { c.Profiles[0].DispatchRef = "other" }, true, true},
		{"own deployment", func(c *buildconfig.Config) { c.Profiles[0].Deployments["dev"].Files[0].Path = "other.yaml" }, false, true},
		{"connection", func(c *buildconfig.Config) { c.Connections[0].TokenRef = "other" }, true, true},
		{"registry", func(c *buildconfig.Config) {
			c.Registries[0].InsecureHTTP = false
			c.Registries[0].Origin = "https://registry.example"
		}, true, true},
		{"pool", func(c *buildconfig.Config) { c.Pools[0].MaxInFlight++ }, true, true},
		{"timeout", func(c *buildconfig.Config) { c.TimeoutSeconds++ }, true, true},
		{"lookback", func(c *buildconfig.Config) { c.RunLookbackSeconds++ }, true, true},
		{"gitops", func(c *buildconfig.Config) { c.GitOps[0].PrivateKeyRef = "other.pem" }, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m, _, in := deploymentFixture(t)
			c := cloneAuthorityConfig(t, s.builds.config)
			other := c.Profiles[0]
			other.ID = "other"
			raw, _ := json.Marshal(other)
			if err := json.Unmarshal(raw, &other); err != nil {
				t.Fatal(err)
			}
			c.Profiles = append(c.Profiles, other)
			provider := &fakeForgejo{head: strings.Repeat("a", 40)}
			registry := &fakeBuildRegistry{}
			configure := func(c *buildconfig.Config) {
				t.Helper()
				if err := s.ConfigureBuilds(c, provider, registry); err != nil {
					t.Fatal(err)
				}
			}
			configure(c)
			oldBuild, oldDeploy := s.builds.buildHashes["app"], s.builds.deploymentHashes["app"]
			oldOther := s.builds.buildHashes["other"]
			legacy := s.builds.legacyGlobal
			m.state.Builds[0].AuthorityHash = oldBuild
			if _, err := s.DeployBuild(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			// A provider-bound build exercises changed-authority reconciliation rather
			// than only queued admission.
			m.state.Builds = append(m.state.Builds, domain.Build{
				Meta:      domain.Meta{ID: "running", ProductID: "p", UpdatedAt: time.Now().UTC()},
				ProfileID: "app", PoolID: "pool", ConnectionID: "forgejo", ConnectionAPIURL: "https://git.example",
				Repository: "o/r", Workflow: "build.yml", AuthorityHash: oldBuild,
				SourceSHA: strings.Repeat("b", 40), Status: "RUNNING", ProviderRunID: 99, QueuedAt: time.Now().UTC(),
			})
			changed := cloneAuthorityConfig(t, c)
			tc.change(changed)
			configure(changed)
			if s.builds.buildAuthorityMatches("app", legacy) || s.builds.deploymentAuthorityMatches("app", legacy) {
				t.Fatal("changed config accepted legacy global authority")
			}
			if (s.builds.buildHashes["app"] != oldBuild) != tc.buildChanged ||
				(s.builds.deploymentHashes["app"] != oldDeploy) != tc.deployChanged {
				t.Fatal("incorrect authority scope")
			}
			if tc.name == "connection" || tc.name == "registry" || tc.name == "pool" {
				if oldOther == s.builds.buildHashes["other"] {
					t.Fatal("shared authority did not affect other profile")
				}
			}
			deployTick(t, s)
			d := m.state.Deployments[0]
			if (d.FailureCode == "DEPLOYMENT_AUTHORITY_CHANGED") != tc.deployChanged {
				t.Fatalf("deployment: %+v", d)
			}
			// Remove the previous intent so the request tests build authority alone.
			m.state.Deployments = nil
			m.state.DeploymentReceipts = nil
			_, err := s.DeployBuild(context.Background(), in)
			if (err != nil) != tc.buildChanged {
				t.Fatalf("finished build deployment: %v", err)
			}
			if err := s.DispatchBuildsOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			running := m.state.Builds[1]
			if (running.FailureCode == "BUILD_AUTHORITY_CHANGED") != tc.buildChanged {
				t.Fatalf("running build: %+v", running)
			}
		})
	}
}

func TestProfileAuthorityLegacyAndDedupe(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		s, m, _, in := deploymentFixture(t)
		m.state.Builds[0].Workflow = "build.yml"
		if legacy {
			m.state.Builds[0].AuthorityHash = s.builds.legacyGlobal
		}
		b, err := s.QueueBuild(context.Background(), domain.QueueBuildRequest{ProductID: "p", ProfileID: "app", SHA: m.state.Builds[0].SourceSHA})
		if err != nil || b.ID != "build" {
			t.Fatalf("dedupe legacy=%v: %+v %v", legacy, b, err)
		}
		if _, err = s.DeployBuild(context.Background(), in); err != nil {
			t.Fatal(err)
		}
		if legacy {
			m.state.Deployments[0].AuthorityHash = s.builds.legacyGlobal
		}
		deployTick(t, s)
		if m.state.Deployments[0].Status != "SUCCEEDED" {
			t.Fatal(m.state.Deployments[0])
		}

		s2, m2, f, _ := buildFixture(t)
		queued, err := s2.QueueBuild(context.Background(), domain.QueueBuildRequest{ProductID: "p", ProfileID: "app"})
		if err != nil {
			t.Fatal(err)
		}
		if legacy {
			m2.state.Builds[0].AuthorityHash = s2.builds.legacyGlobal
		}
		if err = s2.DispatchBuildsOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if m2.state.Builds[0].FailureCode == "BUILD_AUTHORITY_CHANGED" || f.dispatches != 1 {
			t.Fatalf("dispatch %s: %+v", queued.ID, m2.state.Builds[0])
		}
	}
	s, _, _, _ := buildFixture(t)
	if s.builds.buildAuthorityMatches("unknown", s.builds.legacyGlobal) || s.builds.deploymentAuthorityMatches("unknown", s.builds.legacyGlobal) {
		t.Fatal("unknown profile has authority")
	}
}

func TestDeployBuildStaleAuthorityDedupe(t *testing.T) {
	for _, status := range []string{"PENDING_CHECKS", "RECONCILIATION_REQUIRED"} {
		t.Run(status, func(t *testing.T) {
			s, m, f, in := deploymentFixture(t)
			old, err := s.DeployBuild(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			m.state.Deployments[0].Status = status
			c := cloneAuthorityConfig(t, s.builds.config)
			c.Profiles[0].RequiredStatusContexts = []string{"new-check"}
			if err := s.ConfigureBuilds(c, &fakeForgejo{}, &fakeBuildRegistry{}); err != nil {
				t.Fatal(err)
			}
			// A fresh request must wait; historical receipt replay remains unchanged.
			in.RequestKey = "fresh-request"
			receipts := len(m.state.DeploymentReceipts)
			got, err := s.DeployBuild(context.Background(), in)
			want := "DEPLOYMENT_IN_PROGRESS: deployment " + old.ID + " build " + old.BuildID + " status " + status
			if err == nil || !strings.Contains(err.Error(), want) || got.ID != "" {
				t.Fatalf("got %+v, %v; want %s", got, err, want)
			}
			if len(m.state.Deployments) != 1 || len(m.state.DeploymentReceipts) != receipts {
				t.Fatal("refused request created deployment or receipt")
			}
			deployTick(t, s)
			d := m.state.Deployments[0]
			if d.FailureCode != "DEPLOYMENT_AUTHORITY_CHANGED" || f.proposals != 0 || f.heads != 0 {
				t.Fatalf("stale intent executed: %+v", d)
			}
			if status == "PENDING_CHECKS" {
				if d.Status != "FAILED" || d.FinishedAt == nil {
					t.Fatalf("stale pending intent not terminal after one tick: %+v", d)
				}
				next, err := s.DeployBuild(context.Background(), in)
				if err != nil || next.ID == old.ID || next.AuthorityHash != s.builds.deploymentHashes["app"] {
					t.Fatalf("replacement: %+v %v", next, err)
				}
			}
		})
	}
}
