package agentconnect

import (
	"bytes"
	"context"
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
