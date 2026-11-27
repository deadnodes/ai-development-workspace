package application

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
)

func TestConnectionSecretOutsideDir(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "secrets")
	sibling := filepath.Join(base, "secrets-other")
	for _, dir := range []string{root, sibling} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "key"), []byte("do-not-log-key"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(sibling, "key"), filepath.Join(root, "escape")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(base, "alias")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}
	t.Setenv("RCP_SECRET_DIR", root)
	for _, tc := range []struct {
		ref  string
		want bool
	}{
		{"file:" + filepath.Join(root, "key"), false},
		{"file:" + filepath.Join(sibling, "key"), true},
		{"file:" + filepath.Join(root, "escape"), true},
		{"file:" + filepath.Join(base, "alias", "key"), false},
		{"file:" + filepath.Join(sibling, "missing"), true},
		{"file:" + filepath.Join(root, "missing"), false},
		{"env:RCP_KEY", false}, {"key", false},
	} {
		if got := connectionSecretOutsideDir(tc.ref); got != tc.want {
			t.Errorf("%s: %v", tc.ref, got)
		}
	}
}

func TestConnectionSecretAttentionAndStartupWarning(t *testing.T) {
	s, m, _, _ := buildFixture(t)
	root := t.TempDir()
	t.Setenv("RCP_SECRET_DIR", root)
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("do-not-log-key"), 0600); err != nil {
		t.Fatal(err)
	}
	m.state.GitHubConnections = []domain.GitHubConnection{
		{Meta: domain.Meta{ID: "bad"}, Name: "Broken", Config: delivery.Connection{PrivateKeyRef: "file:" + path, RegistryCredentialRef: "file:" + path}},
		{Meta: domain.Meta{ID: "other", ProductID: "other"}, Name: "Other", Config: delivery.Connection{PrivateKeyRef: "file:" + path}},
		{Meta: domain.Meta{ID: "good", ProductID: "p"}, Name: "Good", Config: delivery.Connection{PrivateKeyRef: "file:" + filepath.Join(root, "key")}},
	}
	m.state.ConnectionGrants = []domain.ConnectionGrant{{Meta: domain.Meta{ProductID: "p"}, ConnectionID: "bad"}, {Meta: domain.Meta{ProductID: "p"}, ConnectionID: "bad"}}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	if err := s.CheckConnectionSecrets(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Count(logs.String(), "level=WARN") != 2 || strings.Contains(logs.String(), "do-not-log-key") || !strings.Contains(logs.String(), path) {
		t.Fatal(logs.String())
	}
	for _, ctx := range []context.Context{context.Background(), auth.WithPrincipal(context.Background(), auth.Principal{ID: "member", Kind: "human", Role: "member", Permissions: []string{"read"}, Products: []string{"p"}})} {
		value, err := s.Query(ctx, "list_attention", "p")
		if err != nil {
			t.Fatal(err)
		}
		items := value.([]map[string]any)
		count := 0
		for _, item := range items {
			if item["reason"] == "CONNECTION_SECRET_OUTSIDE_DIR" {
				count++
				if item["connection_id"] != "bad" {
					t.Fatal(item)
				}
			}
		}
		if count != 1 {
			t.Fatal(items)
		}
	}
	// Resolving configuration clears the derived warning without deleting history.
	m.state.GitHubConnections[0].Config.PrivateKeyRef = "env:RCP_KEY"
	m.state.GitHubConnections[0].Config.RegistryCredentialRef = ""
	if got := connectionSecretWarnings(&m.state, "p"); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestConnectionSecretAttentionCollapsesRefreshFailures(t *testing.T) {
	s, m := fixture(t)
	t.Setenv("RCP_SECRET_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "missing-key")
	m.state.GitHubConnections = []domain.GitHubConnection{{Meta: domain.Meta{ID: "bad", ProductID: "p"}, Config: delivery.Connection{PrivateKeyRef: "file:" + path}}}
	m.state.RepositoryBindings = []domain.RepositoryBinding{{Meta: domain.Meta{ProductID: "p"}, RepositoryID: "repo", ConnectionID: "bad"}}
	m.state.Integrations[0].Branches = []domain.Branch{{RepositoryID: "repo", Name: "work"}}
	for i := range 50 {
		m.state.Operations = append(m.state.Operations, domain.ExternalOperation{Meta: domain.Meta{ID: fmt.Sprint(i), ProductID: "p"}, Kind: "REFRESH_GIT", IntegrationID: "i", Status: "FAILED", Detail: "branch observation failed: secret file outside configured directory"})
	}
	m.state.Operations = append(m.state.Operations, domain.ExternalOperation{Meta: domain.Meta{ID: "unrelated", ProductID: "p"}, Kind: "REFRESH_GIT", IntegrationID: "i", Status: "FAILED", Detail: "GitHub returned HTTP 500"})
	value, err := s.Query(context.Background(), "list_attention", "p")
	if err != nil {
		t.Fatal(err)
	}
	items := value.([]map[string]any)
	if len(items) != 2 {
		t.Fatal(items)
	}
	if len(m.state.Operations) != 51 {
		t.Fatal("audit changed")
	}
}
