package agentconnect

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunWhoami(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/me" || r.Header.Get("Authorization") != "Bearer fixture" {
			t.Fatalf("unexpected request: %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"auth_mode":"oidc","user_id":"alice","principal":{"kind":"agent","id":"agent/1","owner":"alice"}}`))
	}))
	defer server.Close()
	t.Setenv("RCP_WHOAMI_TOKEN", "fixture")
	var out bytes.Buffer
	if err := RunWhoami([]string{"--url", server.URL, "--token-env", "RCP_WHOAMI_TOKEN"}, &out); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["user_id"] != "alice" || !strings.Contains(out.String(), "agent/1") {
		t.Fatalf("unexpected identity: %s", out.String())
	}
}
