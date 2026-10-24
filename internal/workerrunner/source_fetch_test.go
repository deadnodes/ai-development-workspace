package workerrunner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
	"strings"
	"testing"
)

func sourceFetchConfig(t *testing.T) Config {
	t.Helper()
	e := &domain.WorkerExecution{Kind: "source_check", ToolchainImage: "golang@sha256:" + strings.Repeat("a", 64), Platform: "linux/arm64", CheckArgv: []string{"/bin/true"}, Network: "none", TimeoutSeconds: 30, MemoryMB: 128, StorageMB: 16, CPUs: 1, PIDs: 64, SourceBytes: 1 << 20, OutputBytes: 1 << 20, LogBytes: 4096, SourceFetch: &domain.WorkerSourceFetch{Provider: "github", RepositoryID: 42, TimeoutSeconds: 20, MaxDownloadBytes: 2 << 20, MaxRequests: 100, MaxEntries: 100, MaxDepth: 10}}
	r := workerconfig.Recipe{ID: "fetch", Revision: "v1", ProductID: "p", RepositoryID: "r", RepositoryURL: "https://github.com/fixture/source", Capability: "source_check", Trust: "trusted_source", Execution: e}
	r.Hash = workerconfig.RecipeHash(r)
	return Config{TokenEnv: "RCP_FETCH_WORKER_TOKEN", ProductID: "p", RepositoryID: "r", ConfigRevision: "cfg1", Recipe: r, SourceGitHub: &SourceGitHubAccess{TokenEnv: "RCP_FETCH_SOURCE_TOKEN"}, StateDir: filepath.Join(t.TempDir(), "state")}
}
func TestSourceFetchStrategyAndCredentialSeparation(t *testing.T) {
	c := sourceFetchConfig(t)
	if err := validateConfig(c); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Config){"both": func(c *Config) { c.SourceMirror = t.TempDir() }, "missing access": func(c *Config) { c.SourceGitHub = nil }, "worker ref": func(c *Config) { c.SourceGitHub.TokenEnv = c.TokenEnv }, "control ref": func(c *Config) { c.SourceGitHub.TokenEnv = "RC_TOKEN" }, "implicit anonymous": func(c *Config) { c.SourceGitHub.TokenEnv = "" }, "anonymous token": func(c *Config) { c.SourceGitHub.Anonymous = true }} {
		t.Run(name, func(t *testing.T) {
			c := sourceFetchConfig(t)
			change(&c)
			if validateConfig(c) == nil {
				t.Fatal("invalid strategy/access accepted")
			}
		})
	}
	t.Setenv(c.TokenEnv, "worker-active")
	t.Setenv("RC_TOKEN", "control-active")
	t.Setenv(c.SourceGitHub.TokenEnv, "source-only")
	credential := sourceCredential(c, "worker-active")
	if token, err := credential(); err != nil || token != "source-only" {
		t.Fatal("dedicated source token rejected", err)
	}
	for _, token := range []string{"worker-active", "control-active", "bad\nvalue", ""} {
		t.Setenv(c.SourceGitHub.TokenEnv, token)
		if _, err := credential(); err == nil {
			t.Fatal("credential collision accepted")
		}
	}
	t.Setenv(c.TokenEnv, "worker-rotated")
	t.Setenv(c.SourceGitHub.TokenEnv, "worker-active")
	if _, err := credential(); err == nil {
		t.Fatal("captured worker token reused after rotation")
	}
	t.Setenv(c.SourceGitHub.TokenEnv, "worker-rotated")
	if _, err := credential(); err == nil {
		t.Fatal("new worker credential collision accepted")
	}
	t.Setenv("RC_TOKEN", "control-rotated")
	t.Setenv(c.SourceGitHub.TokenEnv, "control-active")
	if _, err := credential(); err == nil {
		t.Fatal("initial control credential collision accepted")
	}
	t.Setenv(c.SourceGitHub.TokenEnv, "control-rotated")
	if _, err := credential(); err == nil {
		t.Fatal("rotated control credential collision accepted")
	}
	c.SourceGitHub = &SourceGitHubAccess{Anonymous: true}
	if token, err := sourceCredential(c, "worker-active")(); err != nil || token != "" {
		t.Fatal("explicit anonymous mode looked up credentials")
	}
	c.Recipe.Execution.SourceFetch = nil
	c.Recipe.Hash = workerconfig.RecipeHash(c.Recipe)
	c.SourceGitHub = nil
	c.SourceMirror = t.TempDir()
	if err := validateConfig(c); err != nil {
		t.Fatal("explicit mirror strategy rejected", err)
	}
	c.SourceMirror = ""
	if validateConfig(c) == nil {
		t.Fatal("missing source strategy accepted")
	}
}
func TestSourceCredentialRejectedBeforeClaim(t *testing.T) {
	c := sourceFetchConfig(t)
	claims := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { claims++; w.Write([]byte("null")) }))
	defer server.Close()
	c.Endpoint = server.URL
	t.Setenv(c.TokenEnv, strings.Repeat("w", 32))
	t.Setenv(c.SourceGitHub.TokenEnv, strings.Repeat("w", 32))
	if _, err := runOne(context.Background(), c, nil); err == nil || claims != 0 {
		t.Fatal("source collision reached worker claim")
	}
	c.SourceGitHub = &SourceGitHubAccess{Anonymous: true}
	if _, err := runOne(context.Background(), c, nil); err != nil || claims != 1 {
		t.Fatal("explicit anonymous idle claim failed", err)
	}
	entries, _ := os.ReadDir(c.StateDir)
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatal("idle claim created source attempt")
		}
	}
}
