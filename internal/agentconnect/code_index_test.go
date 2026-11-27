package agentconnect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"releasecontrol/internal/domain"
)

func TestCodeIndexCLIDryRun(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# Local index\n"), 0600); err != nil {
		t.Fatal(err)
	}
	export := filepath.Join(root, "snapshot.json")
	var output bytes.Buffer
	args := []string{"--repo-id", "fixture", "--path", root, "--dry-run", "--out", export, "--url", "invalid-url"}
	if err := RunIndex(context.Background(), args, &output); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(export)
	if err != nil {
		t.Fatal(err)
	}
	var doc domain.CodeIndex
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Validate("fixture") != nil || len(doc.Files) != 1 || doc.Files[0].Path != "README.md" || !strings.Contains(output.String(), "dry-run") {
		t.Fatalf("export = %s, output = %q, error = %v", raw, output.String(), err)
	}
	if err := RunIndex(context.Background(), args, &output); err == nil {
		t.Fatal("expected export overwrite to be refused")
	}
}

func TestCodeIndexCLIUntrackedOptIn(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git required for untracked test")
	}
	root, config := t.TempDir(), t.TempDir()
	write := func(file, text string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", config)
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(config, "gitconfig"))
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
		if raw, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s (%v)", args, raw, err)
		}
	}
	excludes := filepath.Join(config, "private ignore")
	write(excludes, "README.local.md\n")
	git("config", "--global", "core.excludesFile", filepath.ToSlash(excludes))
	git("init", "-b", "main")
	write(filepath.Join(root, "README.md"), "tracked\n")
	git("add", "README.md")
	git("commit", "-m", "fixture")
	write(filepath.Join(root, "README.local.md"), "globally ignored\n")
	write(filepath.Join(root, "README.extra.md"), "untracked\n")
	for _, optIn := range []bool{false, true} {
		export := filepath.Join(config, fmt.Sprintf("index-%v.json", optIn))
		args := []string{"--repo-id", "fixture", "--path", root, "--dry-run", "--out", export}
		want := []string{"README.md"}
		if optIn {
			args = append(args, "--include-untracked")
			want = []string{"README.extra.md", "README.md"}
		}
		if err := RunIndex(context.Background(), args, io.Discard); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(export)
		var doc domain.CodeIndex
		if err != nil || json.Unmarshal(raw, &doc) != nil {
			t.Fatalf("read exported index: %v", err)
		}
		var paths []string
		for _, file := range doc.Files {
			paths = append(paths, file.Path)
		}
		if !slices.Equal(paths, want) {
			t.Fatalf("opt-in %v: paths=%v, want %v", optIn, paths, want)
		}
	}
}

func TestDoctorProductCheckDoesNotFollowRedirect(t *testing.T) {
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("doctor followed a redirect away from the Product endpoint")
		w.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing bearer at the intended origin")
		}
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer source.Close()
	if ok, err := productContextCheck(context.Background(), source.URL, "test-token", "fixture"); ok || err == nil || err.Error() != "HTTP 302" {
		t.Fatalf("redirect check = %v, %v", ok, err)
	}
}

func TestCodeIndexRemoteWorkspaceUsesPagedRepositoryProjection(t *testing.T) {
	t.Setenv("RC_TOKEN", "index-fixture-token")
	serv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/code/repositories" || r.URL.Query().Get("product_id") != "fixture" {
			t.Errorf("unexpected workspace-index request: %s", r.URL)
			http.NotFound(w, r)
			return
		}
		page := domain.CodeRepositoryPage{ProductID: "fixture"}
		switch r.URL.Query().Get("cursor") {
		case "":
			page.Items = []domain.CodeRepository{{ID: "first", URL: "https://example.invalid/first"}}
			page.NextCursor = "page-two"
		case "page-two":
			page.Items = []domain.CodeRepository{{ID: "second", URL: "https://example.invalid/second"}}
		default:
			t.Errorf("unexpected cursor: %s", r.URL)
			http.Error(w, "invalid cursor", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer serv.Close()
	var out bytes.Buffer
	args := []string{"--workspace", t.TempDir(), "--product-id", "fixture", "--url", serv.URL, "--dry-run"}
	if err := RunIndex(context.Background(), args, &out); err != nil || !strings.Contains(out.String(), "No checkouts matched") {
		t.Fatalf("workspace dry run: %q, %v", out.String(), err)
	}
	repos, err := indexRepositories(context.Background(), remoteIndex{endpoint: serv.URL}, "fixture")
	if err != nil || len(repos) != 2 || repos[1].ID != "second" {
		t.Fatalf("repositories = %+v, %v", repos, err)
	}
}

func TestCodeIndexBindingCannotSendEnvironmentToken(t *testing.T) {
	root := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("a checkout binding supplied an Authorization header")
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	t.Setenv("RC_TOKEN", "ambient-test-token")
	t.Setenv("RCP_INDEX_TEST_TOKEN", "not-for-the-checkout-binding")
	raw, err := json.Marshal(Binding{URL: server.URL, ProductID: "fixture", TokenEnv: "RCP_INDEX_TEST_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".release-control.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--repo-id", "checkout", "--path", root}
	var output bytes.Buffer
	if err := RunIndex(context.Background(), args, &output); err == nil || !strings.Contains(err.Error(), "no saved agent credential") {
		t.Fatalf("expected a URL-scoped saved profile, got %v", err)
	}
	if err := RunIndex(context.Background(), append(args, "--token-env", "RCP_INDEX_TEST_TOKEN"), &output); err == nil || !strings.Contains(err.Error(), "explicit --url") {
		t.Fatalf("expected explicit URL requirement, got %v", err)
	}
	if err := RunDoctor(context.Background(), []string{"--workspace", root, "--token-env", "RCP_INDEX_TEST_TOKEN"}, &output); err == nil || !strings.Contains(err.Error(), "explicit --url") {
		t.Fatalf("expected explicit URL requirement in doctor, got %v", err)
	}
}
