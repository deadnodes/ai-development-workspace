package agentconnect

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression: connect writes a managed marker after the skill text, so doctor
// must compare against the same bytes or report a false mismatch.
func TestDoctorAcceptsSkillsJustInstalledByConnect(t *testing.T) {
	root := t.TempDir()
	t.Setenv("RCP_DOCTOR_TEST_TOKEN", "doctor-token")
	server := kitServer(t, 1, "doctor-token")
	if _, err := Install(context.Background(), Options{URL: server.URL, Workspace: root, TokenEnv: "RCP_DOCTOR_TEST_TOKEN", Harness: "both"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_ = RunDoctor(context.Background(), []string{"--url", server.URL, "--workspace", root, "--product-id", "p0", "--token-env", "RCP_DOCTOR_TEST_TOKEN"}, &out)
	seen := 0
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "installed skill checksums") {
			seen++
			if !strings.HasPrefix(line, "OK ") {
				t.Errorf("skill check failed right after connect: %s", line)
			}
		}
		if strings.Contains(line, "doctor-token") {
			t.Fatal("doctor printed the token")
		}
	}
	if seen != 2 {
		t.Fatalf("expected codex and claude skill checks, got %d:\n%s", seen, out.String())
	}
}

func TestConnectRefusesUserCodexURLServerWithSameName(t *testing.T) {
	root := t.TempDir()
	t.Setenv("RCP_DOCTOR_TEST_TOKEN", "doctor-token")
	server := kitServer(t, 1, "doctor-token") // sets HOME to a temp dir
	home, _ := os.UserHomeDir()
	writeFile(t, home, ".codex/config.toml", "[mcp_servers.release-control]\nurl = \"https://example.test/mcp\"\nhttp_headers = { \"Authorization\" = \"Bearer literal-secret\" }\n")
	_, err := Install(context.Background(), Options{URL: server.URL, Workspace: root, TokenEnv: "RCP_DOCTOR_TEST_TOKEN", Harness: "both"})
	if err == nil || !strings.Contains(err.Error(), "--name release-control-project") {
		t.Fatalf("expected shadowing refusal, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, ".codex")); statErr == nil {
		t.Fatal("refused connect wrote files")
	}
	if _, err = Install(context.Background(), Options{URL: server.URL, Workspace: root, TokenEnv: "RCP_DOCTOR_TEST_TOKEN", Harness: "both", Name: "release-control-project"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_ = RunDoctor(context.Background(), []string{"--url", server.URL, "--workspace", root, "--product-id", "p0", "--token-env", "RCP_DOCTOR_TEST_TOKEN"}, &out)
	text := out.String()
	if strings.Contains(text, "literal-secret") {
		t.Fatal("doctor printed the literal header")
	}
	if !strings.Contains(text, "OK Codex user config does not shadow project MCP") || !strings.Contains(text, "WARN Codex user config credential") {
		t.Fatalf("unexpected doctor output:\n%s", text)
	}
}
