// Package signingtest provides isolated protocol fakes; it never contacts external services.
package signingtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/imagesigning"
	"strings"
	"sync"
	"testing"
)

type Fixture struct {
	Mu                            sync.Mutex
	Keys                          map[string]*ecdsa.PrivateKey
	Blobs                         map[string][]byte
	Manifests                     map[string][]byte
	Vault, Registry               *httptest.Server
	Logins, Signs, Writes         int
	Mismatch, Unavailable, Bearer bool
	Root                          string
}

func New(t *testing.T) *Fixture {
	t.Helper()
	f := &Fixture{Keys: map[string]*ecdsa.PrivateKey{}, Blobs: map[string][]byte{}, Manifests: map[string][]byte{}, Root: t.TempDir()}
	for _, id := range []string{"dev", "prod", "wrong"} {
		k, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		f.Keys[id] = k
	}
	t.Setenv("RCP_SECRET_DIR", f.Root)
	if e := os.WriteFile(filepath.Join(f.Root, "registry.json"), []byte(`{"username":"signer","password":"private"}`), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(f.Root, "jwt"), []byte("projected-jwt"), 0600); e != nil {
		t.Fatal(e)
	}
	f.Vault = server("http://vault.test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.Mu.Lock()
		defer f.Mu.Unlock()
		if f.Unavailable {
			http.Error(w, "private secret", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/login") {
			var v map[string]string
			_ = json.NewDecoder(r.Body).Decode(&v)
			if v["role"] != "signer" || v["jwt"] != "projected-jwt" {
				w.WriteHeader(403)
				return
			}
			f.Logins++
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": "vault-private", "lease_duration": 600}})
			return
		}
		if r.Header.Get("X-Vault-Token") != "vault-private" {
			w.WriteHeader(403)
			return
		}
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) < 5 {
			w.WriteHeader(404)
			return
		}
		key := f.Keys[parts[4]]
		if key == nil {
			w.WriteHeader(404)
			return
		}
		if strings.Contains(r.URL.Path, "/keys/") {
			if f.Mismatch {
				key = f.Keys["wrong"]
			}
			der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"latest_version": 1, "keys": map[string]any{"1": map[string]string{"public_key": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}}}})
			return
		}
		var v struct {
			Input     string `json:"input"`
			Prehashed bool   `json:"prehashed"`
			Algorithm string `json:"marshaling_algorithm"`
		}
		_ = json.NewDecoder(r.Body).Decode(&v)
		sum, e := base64.StdEncoding.DecodeString(v.Input)
		if e != nil || len(sum) != 32 || !v.Prehashed || v.Algorithm != "asn1" || !strings.HasSuffix(r.URL.Path, "/sha2-256") {
			w.WriteHeader(400)
			return
		}
		sig, _ := ecdsa.SignASN1(rand.Reader, key, sum)
		f.Signs++
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"signature": "vault:v1:" + base64.StdEncoding.EncodeToString(sig)}})
	}))
	f.Registry = server("http://registry.test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.Mu.Lock()
		defer f.Mu.Unlock()
		if r.URL.Path == "/token" {
			u, p, ok := r.BasicAuth()
			if !ok || u != "signer" || p != "private" {
				w.WriteHeader(403)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "registry-private"})
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			u, p, ok := r.BasicAuth()
			auth := ok && u == "signer" && p == "private"
			if f.Bearer {
				auth = r.Header.Get("Authorization") == "Bearer registry-private"
			}
			if !auth {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+f.Registry.URL+`/token",service="registry"`)
				w.WriteHeader(401)
				return
			}
			f.Writes++
		}
		if strings.Contains(r.URL.Path, "/blobs/uploads/") {
			if r.Method == "POST" {
				w.Header().Set("Location", r.URL.Path+"upload")
				w.WriteHeader(202)
				return
			}
			b, _ := io.ReadAll(r.Body)
			if imagesigning.Hash(b) != r.URL.Query().Get("digest") {
				w.WriteHeader(400)
				return
			}
			f.Blobs[imagesigning.Hash(b)] = b
			w.WriteHeader(201)
			return
		}
		if _, key, ok := strings.Cut(r.URL.Path, "/blobs/"); ok {
			b, exists := f.Blobs[key]
			if !exists {
				w.WriteHeader(404)
				return
			}
			if r.Method == "GET" {
				_, _ = w.Write(b)
			}
			return
		}
		if _, key, ok := strings.Cut(r.URL.Path, "/manifests/"); ok {
			if r.Method == "PUT" {
				b, _ := io.ReadAll(r.Body)
				f.Manifests[key] = b
				f.Manifests[imagesigning.Hash(b)] = b
				w.WriteHeader(201)
				return
			}
			b, exists := f.Manifests[key]
			if !exists {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Docker-Content-Digest", imagesigning.Hash(b))
			if r.Method == "GET" {
				_, _ = w.Write(b)
			}
			return
		}
		w.WriteHeader(404)
	}))
	return f
}
func (f *Fixture) Key(id string) buildconfig.SigningKey {
	der, _ := x509.MarshalPKIXPublicKey(&f.Keys[id].PublicKey)
	return buildconfig.SigningKey{ID: id, VaultAddr: f.Vault.URL, InsecureHTTP: true, TransitMount: "transit", KeyName: id, PublicKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), Auth: buildconfig.SigningAuth{Method: "kubernetes", Mount: "kubernetes", Role: "signer", TokenFile: filepath.Join(f.Root, "jwt")}, AllowBackfill: id == "prod"}
}
func (f *Fixture) Config() *buildconfig.Signing {
	return &buildconfig.Signing{BuilderID: "https://rcp.example.invalid/builders/forgejo/v1", Keys: []buildconfig.SigningKey{f.Key("dev"), f.Key("prod")}, Rules: []buildconfig.SigningRule{{KeyID: "dev", TagPrefixes: []string{"main-", "br-"}}}, RegistryCredentials: map[string]string{"zot": "registry.json"}}
}
func (f *Fixture) RegistryConfig() buildconfig.Registry {
	return buildconfig.Registry{ID: "zot", Origin: f.Registry.URL, InsecureHTTP: true}
}
func (f *Fixture) Image(revision string) string {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	cfg, _ := json.Marshal(map[string]any{"architecture": "amd64", "os": "linux", "config": map[string]any{"Labels": map[string]string{"org.opencontainers.image.revision": revision}}})
	d := imagesigning.Hash(cfg)
	f.Blobs[d] = cfg
	b, _ := json.Marshal(imagesigning.Manifest{SchemaVersion: 2, MediaType: imagesigning.ManifestType, Config: imagesigning.Descriptor{MediaType: imagesigning.ConfigType, Digest: d, Size: len(cfg)}, Layers: []imagesigning.Descriptor{}})
	digest := imagesigning.Hash(b)
	f.Manifests[digest] = b
	return digest
}

// In-memory HTTP transport exercises request/response handling without listening sockets.
func server(origin string, h http.Handler) *httptest.Server {
	return &httptest.Server{URL: origin, Config: &http.Server{Handler: h}}
}

type transport struct{ f *Fixture }

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	if r.URL.Host == "vault.test" {
		t.f.Vault.Config.Handler.ServeHTTP(w, r)
	} else if r.URL.Host == "registry.test" {
		t.f.Registry.Config.Handler.ServeHTTP(w, r)
	} else {
		return nil, imagesigning.ErrUnavailable
	}
	return w.Result(), nil
}
func (f *Fixture) Client() *http.Client { return &http.Client{Transport: transport{f}} }
