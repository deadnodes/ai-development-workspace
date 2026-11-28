package transport_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"testing"

	"releasecontrol/internal/application"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
)

func TestHealthzReturnsStableMCPToolsFingerprint(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenSQLite(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler := transport.New(application.New(store), transport.Options{})
	read := func() string {
		req := httptest.NewRequest(http.MethodGet, "http://localhost/healthz", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("health status=%d body=%s", rec.Code, rec.Body.String())
		}
		var body struct {
			Status      string `json:"status"`
			Fingerprint string `json:"mcp_tools_fingerprint"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Status != "ok" || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(body.Fingerprint) {
			t.Fatalf("unexpected health response: %+v", body)
		}
		return body.Fingerprint
	}
	first, second := read(), read()
	if first != second {
		t.Fatalf("fingerprint is unstable: %q != %q", first, second)
	}
}
