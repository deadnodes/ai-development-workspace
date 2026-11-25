package transport

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTurnEndHookPayloadShapes(t *testing.T) {
	for _, tc := range []struct {
		name, kind, payload, expected string
		stdin                         bool
	}{
		{"codex", "codex", `{"type":"agent-turn-complete","thread-id":"thread-123"}`, "thread-123", false},
		{"claude", "claude_code", `{"session_id":"session-456"}`, "session-456", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/commands" || r.Header.Get("Authorization") != "Bearer secret-value" {
					t.Errorf("request path/auth = %s / %q", r.URL.Path, r.Header.Get("Authorization"))
				}
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode request: %v", err)
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			fakeTmux := filepath.Join(bin, "tmux")
			if err := os.WriteFile(fakeTmux, []byte("#!/bin/sh\nprintf 'tmux-session'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			token := filepath.Join(dir, "token")
			if err := os.WriteFile(token, []byte("secret-value"), 0600); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(dir, "hook.log")
			cmd := exec.Command(filepath.Join("..", "..", "scripts", "hooks", "rcp-turn-end.sh"), tc.payload)
			cmd.Env = append(os.Environ(), "RCP_URL="+srv.URL, "RCP_TOKEN_FILE="+token, "RCP_PRODUCT_ID=product", "RCP_HARNESS_KIND="+tc.kind, "RCP_HOOK_LOG="+log, "TMUX=fake", "PATH="+bin+":"+os.Getenv("PATH"))
			if tc.stdin {
				cmd.Stdin = strings.NewReader(tc.payload)
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("hook failed: %v: %s", err, out)
			}
			data := got["data"].(map[string]any)
			if got["action"] != "report_turn_end" || got["product_id"] != "product" || data["harness_kind"] != tc.kind || data["session_ref"] != tc.expected || data["tmux_target"] != "tmux-session" {
				t.Fatalf("unexpected body: %#v", got)
			}
			if strings.Contains(string(out), "secret-value") {
				t.Fatal("hook printed token")
			}
		})
	}
}

func TestTurnEndHookKeepsTokenOutOfCurlArgv(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	argv := filepath.Join(dir, "argv")
	curl := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + argv + "\"\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(curl), 0700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(dir, "token")
	secret := "never-on-argv"
	if err := os.WriteFile(token, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join("..", "..", "scripts", "hooks", "rcp-turn-end.sh"), `{"type":"agent-turn-complete","thread-id":"thread-123"}`)
	cmd.Env = append(os.Environ(), "RCP_URL=http://example.invalid", "RCP_TOKEN_FILE="+token, "RCP_PRODUCT_ID=product", "RCP_HARNESS_KIND=codex", "RCP_HOOK_LOG="+filepath.Join(dir, "hook.log"), "PATH="+bin+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hook failed: %v: %s", err, out)
	}
	got, err := os.ReadFile(argv)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), secret) {
		t.Fatalf("token leaked in curl argv: %s", got)
	}
	if !strings.Contains(string(got), "@") {
		t.Fatalf("curl did not receive header file argument: %s", got)
	}
}
