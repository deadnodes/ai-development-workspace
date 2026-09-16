package runtimeobserver

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKubeconfigExplicitContextAndRelativePaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	config := `current-context: wrong
clusters:
- name: desired
  cluster:
    server: https://desired.example
    certificate-authority: ca.pem
- name: wrong
  cluster:
    server: https://wrong.example
users:
- name: app
  user:
    token: private-value-never-serialize
    client-certificate: cert.pem
    client-key: key.pem
contexts:
- name: dev
  context:
    cluster: desired
    user: app
`
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
	target := Target{Context: "dev"}
	m, err := resolveKubeconfig(&target)
	if err != nil {
		t.Fatal(err)
	}
	if target.APIURL != "https://desired.example" || target.CAFile != filepath.Join(dir, "ca.pem") || target.ClientKeyFile != filepath.Join(dir, "key.pem") || m.token == "" {
		t.Fatalf("bad context selection: %+v", target)
	}
	b, _ := json.Marshal(target)
	if strings.Contains(string(b), "private-value") {
		t.Fatal("serialized credential")
	}
	if _, err := resolveKubeconfig(&Target{}); err == nil {
		t.Fatal("implicit context allowed")
	}
	if _, err := resolveKubeconfig(&Target{Context: "absent"}); err == nil {
		t.Fatal("missing context allowed")
	}
	for _, extra := range []string{"exec: {command: touch}", "auth-provider: {name: oidc}"} {
		cfg := strings.Replace(config, "token: private-value-never-serialize", extra, 1)
		os.WriteFile(path, []byte(cfg), 0600)
		if _, err := resolveKubeconfig(&Target{Context: "dev"}); err == nil {
			t.Fatal("executable authentication accepted")
		}
	}
}
func TestKubeconfigInlineMaterialAndFirstFileWins(t *testing.T) {
	dir := t.TempDir()
	one := filepath.Join(dir, "one")
	two := filepath.Join(dir, "two")
	cfg := `clusters:
- name: c
  cluster:
    server: https://one.example
    certificate-authority-data: ` + base64.StdEncoding.EncodeToString([]byte("ca")) + `
users:
- name: u
  user:
    client-certificate-data: ` + base64.StdEncoding.EncodeToString([]byte("cert")) + `
    client-key-data: ` + base64.StdEncoding.EncodeToString([]byte("key")) + `
contexts:
- name: explicit
  context: {cluster: c, user: u}
`
	os.WriteFile(one, []byte(cfg), 0600)
	os.WriteFile(two, []byte(strings.ReplaceAll(cfg, "one.example", "two.example")), 0600)
	target := Target{Context: "explicit", Kubeconfig: one + string(os.PathListSeparator) + two}
	m, err := resolveKubeconfig(&target)
	if err != nil || target.APIURL != "https://one.example" || string(m.ca) != "ca" || string(m.cert) != "cert" || string(m.key) != "key" {
		t.Fatalf("invalid material: %v", err)
	}
}
