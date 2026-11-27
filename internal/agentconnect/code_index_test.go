package agentconnect

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
