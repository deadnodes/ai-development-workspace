package runtimeobserver

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func verificationTestTrust(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: "additional-verification-test-trust"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestVerificationScopeResolvedBindingAndGroupJSON(t *testing.T) {
	reads := 0
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		if r.Method != "GET" {
			t.Error("mutation")
		}
		if r.Header.Get("Authorization") != "Bearer private-credential-never-serialize" {
			t.Error("wrong credential")
		}
		w.WriteHeader(403)
	}))
	defer peer.Close()
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer other.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: peer.Certificate().Raw})
	additionalTrust := verificationTestTrust(t)
	dir := t.TempDir()
	config := filepath.Join(dir, "config")
	targets := filepath.Join(dir, "targets.json")
	writeConfig := func(endpoint, serverName, token string, trust []byte) {
		t.Helper()
		b := fmt.Sprintf("clusters:\n- name: c\n  cluster:\n    server: %s\n    certificate-authority-data: %s\n    tls-server-name: %s\nusers:\n- name: u\n  user:\n    token: %s\ncontexts:\n- name: dev\n  context: {cluster: c, user: u}\n", endpoint, base64.StdEncoding.EncodeToString(trust), serverName, token)
		if e := os.WriteFile(config, []byte(b), 0600); e != nil {
			t.Fatal(e)
		}
	}
	writeConfig(peer.URL, "", "private-credential-never-serialize", ca)
	target := Target{Kubeconfig: config, Context: "dev", ProductID: "p", EnvironmentID: "dev", ApplicationID: "app", Namespace: "app", Deployment: "backend", Container: "app", FluxNamespace: "flux-system", Kustomization: "apps", VerificationGroup: "complete-app-v1"}
	b, _ := json.Marshal([]Target{target})
	if e := os.WriteFile(targets, b, 0600); e != nil {
		t.Fatal(e)
	}
	loaded, e := Load(targets)
	if e != nil {
		t.Fatal(e)
	}
	o := loaded[0]
	scope, e := o.RuntimeVerificationScope()
	if e != nil {
		t.Fatal(e)
	}
	if scope.VerificationGroup != "complete-app-v1" || scope.Context != "dev" || scope.BindingHash == "" || scope.FluxNamespace != "flux-system" {
		t.Fatalf("scope %+v", scope)
	}
	again, e := o.RuntimeVerificationScope()
	if e != nil || !reflect.DeepEqual(scope, again) {
		t.Fatal("unstable binding")
	}
	encoded, _ := json.Marshal(scope)
	for _, secret := range []string{peer.URL, dir, "private-credential-never-serialize", "certificate-authority", "token_file", "api_url"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("sensitive binding serialized")
		}
	}
	o.Snapshot(context.Background())
	if reads != 2 {
		t.Fatalf("real HTTPS snapshot did not use configured adapter: %d", reads)
	}
	for _, mode := range []string{"endpoint", "trust", "server-name", "credential"} {
		t.Run(mode, func(t *testing.T) {
			endpoint, name, token, trust := peer.URL, "", "private-credential-never-serialize", ca
			switch mode {
			case "endpoint":
				endpoint = other.URL
			case "trust":
				trust = append(append([]byte(nil), ca...), additionalTrust...)
			case "server-name":
				name = "cluster.example"
			case "credential":
				token = "different-private-credential"
			}
			writeConfig(endpoint, name, token, trust)
			newObserver, e := New(target)
			if e != nil {
				t.Fatal(e)
			}
			changed, e := newObserver.RuntimeVerificationScope()
			if e != nil {
				t.Fatal(e)
			}
			if changed.BindingHash == scope.BindingHash {
				t.Fatal("resolved rebinding retained proof")
			}
			changed.BindingHash = scope.BindingHash
			if !reflect.DeepEqual(changed, scope) {
				t.Fatal("visible scope unexpectedly differs")
			}
		})
	}
	// Existing observers keep their loaded API/TLS material despite later edits to
	// kubeconfig. The next configured instance carries the changed authority.
	retained, e := o.RuntimeVerificationScope()
	if e != nil || retained != scope {
		t.Fatal("loaded observer binding changed by unrelated config read")
	}
}

func TestVerificationScopeTokenRotationAndInformationalTarget(t *testing.T) {
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	os.WriteFile(caFile, verificationTestTrust(t), 0600)
	tokenFile := filepath.Join(dir, "token")
	os.WriteFile(tokenFile, []byte("one-private-token"), 0600)
	o, e := New(Target{APIURL: "https://cluster.example", CAFile: caFile, TokenFile: tokenFile, ProductID: "p", EnvironmentID: "dev", ApplicationID: "app", Namespace: "app", Deployment: "app", Container: "app"})
	if e != nil {
		t.Fatal(e)
	}
	before, e := o.RuntimeVerificationScope()
	if e != nil || before.VerificationGroup != "" {
		t.Fatal("informational target changed")
	}
	os.WriteFile(tokenFile, []byte("two-private-token"), 0600)
	after, e := o.RuntimeVerificationScope()
	if e != nil || before.BindingHash == after.BindingHash {
		t.Fatal("token rebind not detected")
	}
	os.Remove(tokenFile)
	if _, e = o.RuntimeVerificationScope(); e == nil {
		t.Fatal("missing credential certified")
	}
	if _, e = (&Observer{}).RuntimeVerificationScope(); e == nil {
		t.Fatal("unresolved binding certified")
	}
}

func TestVerificationDefaultTrustRemainsInformational(t *testing.T) {
	o, err := New(Target{APIURL: "https://cluster.example", ProductID: "p", EnvironmentID: "dev", ApplicationID: "app", Namespace: "app", Deployment: "app", Container: "app", VerificationGroup: "declared"})
	if err != nil {
		t.Fatal("informational observer startup changed", err)
	}
	if _, err = o.RuntimeVerificationScope(); err == nil {
		t.Fatal("implicit OS trust certified without resolved fingerprint")
	}
	// Informational snapshot retains its ordinary read path; an explicit peer
	// supplies the TLS transport so this test never contacts a real cluster.
	reads := 0
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		if r.Method != "GET" {
			t.Error("mutation")
		}
		w.WriteHeader(403)
	}))
	defer peer.Close()
	o.Target.APIURL = peer.URL
	o.client = peer.Client()
	snapshot := o.Snapshot(context.Background())
	if reads != 1 || len(snapshot.Errors) == 0 {
		t.Fatal("informational snapshot path changed")
	}
}
