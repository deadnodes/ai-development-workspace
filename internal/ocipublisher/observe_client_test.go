package ocipublisher

import (
	"context"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestObservationGETClientTLSAuthBoundsAndNoRedirect(t *testing.T) {
	var calls, foreign atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreign.Add(1) }))
	defer target.Close()
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" {
			t.Error("registry write")
		}
		if r.Header.Get("Authorization") != "Bearer private-fixture-read" {
			w.WriteHeader(401)
			return
		}
		if strings.Contains(r.URL.Path, "/manifests/redirect") {
			http.Redirect(w, r, target.URL, 302)
			return
		}
		w.Write([]byte(strings.Repeat("x", 2048)))
	}))
	defer s.Close()
	path := filepath.Join(t.TempDir(), "public-ca.pem")
	os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0600)
	t.Setenv("OBSERVER_READ_TOKEN", "private-fixture-read")
	cfg := ReadConfig{Origin: s.URL, Repository: "team/image", CAFile: path, BearerEnv: "OBSERVER_READ_TOKEN", MaxBytes: 1024, TimeoutSeconds: 1}
	c, e := NewReadClient(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	r, e := c.Get(context.Background(), "manifests", "valid", nil)
	if e != nil {
		t.Fatal(e)
	}
	_, e = io.ReadAll(r.Body)
	r.Body.Close()
	if e == nil {
		t.Fatal("oversize stream accepted")
	}
	if _, e = c.Get(context.Background(), "manifests", "redirect", nil); e == nil {
		t.Fatal("redirect accepted")
	}
	if foreign.Load() != 0 {
		t.Fatal("redirect leaked authority")
	}
	before := calls.Load()
	for _, in := range []struct {
		k, r string
		q    url.Values
	}{{"delete", "valid", nil}, {"manifests", "../escape", nil}, {"tags", "list", url.Values{"artifactType": {"private"}}}, {"blobs", "tag", nil}} {
		if _, e = c.Get(context.Background(), in.k, in.r, in.q); e == nil {
			t.Fatal("unsafe GET target accepted")
		}
	}
	if calls.Load() != before {
		t.Fatal("invalid input contacted registry")
	}
	cfg.CAFile = ""
	bad, e := NewReadClient(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer bad.Close()
	if _, e = bad.Get(context.Background(), "manifests", "valid", nil); e == nil {
		t.Fatal("untrusted TLS accepted")
	}
	cfg.CAFile = path
	cfg.MaxBytes = 1 << 30
	cfg.TimeoutSeconds = 900
	if max, e := NewReadClient(cfg); e != nil {
		t.Fatal("valid observer maxima rejected")
	} else {
		max.Close()
	}
}
