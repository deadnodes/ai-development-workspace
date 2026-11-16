package imagesigning_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"releasecontrol/internal/buildconfig"
	s "releasecontrol/internal/imagesigning"
	"releasecontrol/internal/imagesigning/signingtest"
	"strings"
	"testing"
	"time"
)

// Opt-in only: uses Docker and cosign, and may download the pinned local registry image.
func TestCosignE2E(t *testing.T) {
	if os.Getenv("RCP_COSIGN_E2E") != "1" {
		t.Skip("set RCP_COSIGN_E2E=1 for local Docker/cosign interoperability")
	}
	for _, name := range []string{"docker", "cosign"} {
		if _, e := exec.LookPath(name); e != nil {
			t.Skip(name + " unavailable")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	run := func(name string, args ...string) []byte {
		t.Helper()
		b, e := exec.CommandContext(ctx, name, args...).CombinedOutput()
		if e != nil {
			t.Fatalf("%s failed: %v\n%s", name, e, b)
		}
		return b
	}
	if exec.CommandContext(ctx, "docker", "info").Run() != nil {
		t.Skip("Docker daemon unavailable")
	}
	listener, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Skip("loopback unavailable")
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	name := fmt.Sprintf("rcp-signing-e2e-%d", time.Now().UnixNano())
	config := `{"distSpecVersion":"1.1.0","storage":{"rootDirectory":"/var/lib/registry"},"http":{"address":"0.0.0.0","port":"5000"}}`
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	if e = os.WriteFile(file, []byte(config), 0600); e != nil {
		t.Fatal(e)
	}
	run("docker", "run", "--rm", "-d", "--name", name, "-p", fmt.Sprintf("127.0.0.1:%d:5000", port), "-v", file+":/etc/zot/config.json:ro", "ghcr.io/project-zot/zot:v2.1.23")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	registry := buildconfig.Registry{ID: "zot", Origin: fmt.Sprintf("http://127.0.0.1:%d", port), InsecureHTTP: true}
	client := s.New()
	deadline := time.Now().Add(20 * time.Second)
	for {
		req, _ := http.NewRequestWithContext(ctx, "GET", registry.Origin+"/v2/", nil)
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("zot did not start")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Push a minimal OCI image with the same bounded distribution client; no base-image download.
	f := signingtest.New(t)
	vault := httptest.NewServer(f.Vault.Config.Handler)
	defer vault.Close()
	key := f.Key("dev")
	key.VaultAddr = vault.URL
	cfg := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]},"config":{}}`)
	request := func(method, path, media string, body []byte) (http.Header, int) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, method, registry.Origin+path, bytes.NewReader(body))
		req.Header.Set("Content-Type", media)
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res.Header, res.StatusCode
	}
	header, code := request("POST", "/v2/test/image/blobs/uploads/", "", nil)
	if code != 202 {
		t.Fatal(code)
	}
	location := header.Get("Location")
	if strings.HasPrefix(location, registry.Origin) {
		location = strings.TrimPrefix(location, registry.Origin)
	}
	sep := "?"
	if strings.Contains(location, "?") {
		sep = "&"
	}
	_, code = request("PUT", location+sep+"digest="+s.Hash(cfg), "application/octet-stream", cfg)
	if code != 201 {
		t.Fatal(code)
	}
	manifest, _ := json.Marshal(s.Manifest{SchemaVersion: 2, MediaType: s.ManifestType, Config: s.Descriptor{MediaType: s.ConfigType, Digest: s.Hash(cfg), Size: len(cfg)}, Layers: []s.Descriptor{}})
	digest := s.Hash(manifest)
	_, code = request("PUT", "/v2/test/image/manifests/tiny", s.ManifestType, manifest)
	if code != 201 {
		t.Fatal(code)
	}
	if _, err := client.Publish(ctx, registry, "test/image", "", key, digest, "e2e", "test", s.Predicate{Type: s.ProvenanceType, Body: map[string]any{"buildDefinition": map[string]string{"buildType": "https://deadnodes.dev/rcp/forgejo-build/v1"}}}); err != nil {
		t.Fatal(err)
	}
	public := filepath.Join(dir, "pub.pem")
	if err := os.WriteFile(public, []byte(key.PublicKeyPEM), 0600); err != nil {
		t.Fatal(err)
	}
	ref := strings.TrimPrefix(registry.Origin, "http://") + "/test/image@" + digest
	run("cosign", "verify", "--key", public, "--insecure-ignore-tlog=true", "--allow-http-registry", ref)
	run("cosign", "verify-attestation", "--key", public, "--insecure-ignore-tlog=true", "--allow-http-registry", "--type", "slsaprovenance1", ref)
}
