package ocipublisher

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func readerConfig(c Config) ReadConfig {
	return ReadConfig{Origin: c.Plan.Origin, Repository: c.Plan.Repository, CAFile: c.CAFile, UsernameEnv: c.UsernameEnv, PasswordEnv: c.PasswordEnv, BearerEnv: c.BearerEnv, MaxBytes: c.MaxArchiveBytes, TimeoutSeconds: c.TimeoutSeconds, FixtureLoopbackHTTP: c.FixtureLoopbackHTTP}
}
func TestReadCompleteClosureTLSAuthAndZeroWrites(t *testing.T) {
	m := &mockRegistry{data: map[string][]byte{}, auth: "Bearer pull-fixture"}
	s := httptest.NewTLSServer(m)
	defer s.Close()
	c := fixture(t, s.URL)
	c.CAFile = filepath.Join(c.StateDir, "ca.pem")
	os.WriteFile(c.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0600)
	c.BearerEnv = "READ_FIXTURE_TOKEN"
	t.Setenv(c.BearerEnv, "pull-fixture")
	if _, e := Publish(context.Background(), c); e != nil {
		t.Fatal(e)
	}
	writes := m.puts.Load()
	before := m.gets.Load()
	rc := readerConfig(c)
	o, e := Read(context.Background(), rc, c.Plan.Receipt.ManifestDigest, c.Plan.Receipt.Platform)
	if e != nil || o.ManifestDigest != c.Plan.Receipt.ManifestDigest || o.ConfigDigest == "" || o.LayerDigest == "" || o.Bytes <= 0 || m.gets.Load()-before != 3 || m.puts.Load() != writes {
		t.Fatal("read did not independently verify exact closure", o, e)
	}
	rc.CAFile = ""
	if _, e = Read(context.Background(), rc, c.Plan.Receipt.ManifestDigest, c.Plan.Receipt.Platform); e == nil {
		t.Fatal("untrusted TLS accepted")
	}
	rc.CAFile = c.CAFile
	t.Setenv(c.BearerEnv, "wrong")
	if _, e = Read(context.Background(), rc, c.Plan.Receipt.ManifestDigest, c.Plan.Receipt.Platform); e == nil {
		t.Fatal("401 accepted")
	}
	t.Setenv(c.BearerEnv, "pull-fixture")
	if _, e = Read(context.Background(), rc, c.Plan.Receipt.ManifestDigest, "linux/amd64"); e == nil {
		t.Fatal("wrong platform accepted")
	}
	if m.puts.Load() != writes {
		t.Fatal("read performed writes")
	}
}
func TestReadRejectsRedirectsAndOversizedManifest(t *testing.T) {
	hits := 0
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer foreign.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, foreign.URL, 307) }))
	defer s.Close()
	c := fixture(t, s.URL)
	if _, e := Read(context.Background(), readerConfig(c), c.Plan.Receipt.ManifestDigest, c.Plan.Receipt.Platform); e == nil || hits != 0 {
		t.Fatal("read followed redirect")
	}
}
