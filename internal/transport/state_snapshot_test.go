package transport_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
	"testing"
)

func TestConditionalSnapshotChangesAndAuthentication(t *testing.T) {
	ctx := context.Background()
	store, e := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	svc := application.New(store)
	server := httptest.NewServer(transport.New(svc, transport.Options{Token: "test-only"}))
	defer server.Close()
	get := func(tag, token string) (int, string, int) {
		t.Helper()
		r, _ := http.NewRequest("GET", server.URL+"/api/state", nil)
		r.Header.Set("If-None-Match", tag)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		v, e := http.DefaultClient.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer v.Body.Close()
		body, _ := io.ReadAll(v.Body)
		if v.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("private snapshot cached")
		}
		return v.StatusCode, v.Header.Get("ETag"), len(body)
	}
	code, tag, n := get("", "test-only")
	if code != 200 || tag == "" || n == 0 {
		t.Fatal("missing initial snapshot")
	}
	code, _, n = get(`"unrelated", W/`+tag, "test-only")
	if code != 304 || n != 0 {
		t.Fatal("unchanged snapshot transmitted")
	}
	code, _, _ = get(tag, "")
	if code != 401 {
		t.Fatal("conditional request bypassed auth")
	}
	_, e = svc.Execute(ctx, domain.Command{Action: "create_product", Actor: "agent", Data: map[string]any{"name": "Changed"}})
	if e != nil {
		t.Fatal(e)
	}
	code, next, n := get(tag, "test-only")
	if code != 200 || next == tag || n == 0 {
		t.Fatal("mutation did not invalidate snapshot")
	}
}
