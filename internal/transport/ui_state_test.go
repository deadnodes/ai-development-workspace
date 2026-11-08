package transport_test

import (
	"context"
	"encoding/json"
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

func TestBrowserSnapshotUsesBoundedProjectionAndAuth(t *testing.T) {
	ctx := context.Background()
	store, e := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "ui.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	svc := application.New(store)
	_, e = svc.Execute(ctx, domain.Command{Action: "create_product", ID: "p", Actor: "test", Data: map[string]any{"name": "Product"}})
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(transport.New(svc, transport.Options{Token: "test-only"}))
	defer server.Close()
	get := func(path, token, etag string) (int, string, []byte) {
		t.Helper()
		r, _ := http.NewRequest("GET", server.URL+path, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		r.Header.Set("If-None-Match", etag)
		resp, e := http.DefaultClient.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("ETag"), b
	}
	code, _, _ := get("/api/ui-state?product_id=p", "", "")
	if code != 401 {
		t.Fatal("UI read bypassed auth")
	}
	code, tag, b := get("/api/ui-state?product_id=p", "test-only", "")
	if code != 200 || tag == "" {
		t.Fatalf("code %d: %s", code, b)
	}
	var out domain.UIState
	if e = json.Unmarshal(b, &out); e != nil {
		t.Fatal(e)
	}
	if out.Projection.Kind != "bounded_browser" || len(out.Products) != 1 || len(out.Events) != 0 {
		t.Fatal("unexpected projection")
	}
	code, _, b = get("/api/ui-state?product_id=p", "test-only", tag)
	if code != 304 || len(b) != 0 {
		t.Fatal("conditional UI snapshot failed")
	}
	code, _, _ = get("/api/ui-state?product_id=p&product_id=q", "test-only", "")
	if code != 400 {
		t.Fatal("ambiguous scope accepted")
	}
	full, e := store.Read(ctx)
	if e != nil || len(full.Events) != 1 {
		t.Fatal("full history not preserved")
	}
}
