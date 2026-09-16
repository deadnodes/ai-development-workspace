package runtimeobserver

import (
	"context"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInClusterResolution(t *testing.T) {
	env := func(k string) string {
		if k == "KUBERNETES_SERVICE_HOST" {
			return "fd00::1"
		}
		if k == "KUBERNETES_SERVICE_PORT" {
			return "443"
		}
		return ""
	}
	target := Target{}
	if _, err := resolveAuthentication(&target, env, "/service-account"); err != nil {
		t.Fatal(err)
	}
	if target.APIURL != "https://[fd00::1]:443" || target.Context != "in-cluster" || target.TokenFile != "/service-account/token" {
		t.Fatalf("%+v", target)
	}
	target = Target{APIURL: "https://explicit.example"}
	if _, err := resolveAuthentication(&target, env, "/service-account"); err != nil || target.Context != "" {
		t.Fatal("explicit API overridden", err)
	}
	target = Target{InCluster: true, APIURL: "https://explicit.example"}
	if _, err := resolveAuthentication(&target, env, "/service-account"); err == nil {
		t.Fatal("mixed auth accepted")
	}
	target = Target{InCluster: true}
	if _, err := resolveAuthentication(&target, func(string) string { return "" }, "/service-account"); err == nil {
		t.Fatal("missing cluster accepted")
	}
	target = Target{Kubeconfig: "/does-not-exist", Context: "explicit"}
	if _, err := resolveAuthentication(&target, env, "/service-account"); err == nil || target.APIURL != "" {
		t.Fatal("failed kubeconfig fell back")
	}
}

func TestServiceAccountTLSAndTokenRotation(t *testing.T) {
	expected := "first"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer "+expected {
			t.Error("incorrect read-only request or token")
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), ca, 0600); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte(expected), 0600); err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", host)
	t.Setenv("KUBERNETES_SERVICE_PORT", port)
	target := Target{InCluster: true, ProductID: "p", EnvironmentID: "e", ApplicationID: "a", Namespace: "ns", Deployment: "app", Container: "app"}
	o, err := newObserver(target, dir)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err = o.get(context.Background(), "/test", &out); err != nil {
		t.Fatal(err)
	}
	expected = "rotated"
	if err = os.WriteFile(token, []byte(expected), 0600); err != nil {
		t.Fatal(err)
	}
	if err = o.get(context.Background(), "/test", &out); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(token, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err = o.get(context.Background(), "/test", &out); err == nil || strings.Contains(err.Error(), expected) {
		t.Fatal("empty token did not fail closed")
	}
}
