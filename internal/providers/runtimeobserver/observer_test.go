package runtimeobserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestObservationFailsClosed(t *testing.T) {
	sha := strings.Repeat("a", 40)
	digest := "sha256:" + strings.Repeat("b", 64)
	for _, tc := range []struct {
		name        string
		flux, image string
		pods        int
		ready       bool
		want        bool
	}{
		{"matching", sha, digest, 1, true, true},
		{"wrong revision", strings.Repeat("c", 40), digest, 1, true, false},
		{"wrong image", sha, "sha256:wrong", 1, true, false},
		{"no pods", sha, digest, 0, true, false},
		{"not ready", sha, digest, 1, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Fatal("observer mutation")
				}
				switch {
				case strings.Contains(r.URL.Path, "kustomizations"):
					fmt.Fprintf(w, `{"metadata":{"generation":1},"status":{"observedGeneration":1,"lastAppliedRevision":"main@sha1:%s","conditions":[{"type":"Ready","status":"True"}]}}`, tc.flux)
				case strings.Contains(r.URL.Path, "deployments"):
					fmt.Fprint(w, `{"metadata":{"generation":1},"spec":{"replicas":1,"selector":{"matchLabels":{"app":"test"}}},"status":{"observedGeneration":1,"replicas":1,"updatedReplicas":1,"availableReplicas":1,"readyReplicas":1}}`)
				default:
					if tc.pods == 0 {
						fmt.Fprint(w, `{"items":[]}`)
						return
					}
					fmt.Fprintf(w, `{"items":[{"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"app","imageID":"docker-pullable://image@%s","ready":%t}]}}]}`, tc.image, tc.ready)
				}
			}))
			defer server.Close()
			o := &Observer{Target: Target{APIURL: server.URL, Container: "app"}, client: server.Client()}
			got := o.Observe(context.Background(), sha, digest)
			if got.Healthy != tc.want {
				t.Fatalf("%+v", got)
			}
		})
	}
}
func TestUnavailableUnknown(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer s.Close()
	o := &Observer{Target: Target{APIURL: s.URL}, client: s.Client()}
	got := o.Observe(context.Background(), strings.Repeat("a", 40), "sha256:"+strings.Repeat("b", 64))
	if got.Healthy || !strings.Contains(got.Details, "UNKNOWN") {
		t.Fatalf("%+v", got)
	}
}
func TestConfigRequiresTLSAndCompleteTarget(t *testing.T) {
	if _, e := New(Target{APIURL: "http://localhost"}); e == nil {
		t.Fatal("accepted plaintext")
	}
	if _, e := New(Target{APIURL: "https://localhost"}); e == nil {
		t.Fatal("accepted incomplete target")
	}
}

func TestTargetIdentityWorkloadKey(t *testing.T) {
	o := &Observer{Target: Target{ProductID: "p", EnvironmentID: "e", ApplicationID: "a", Namespace: "ns", Deployment: "deployment", Container: "container"}}
	got := o.TargetIdentity()
	if got.Target != "|/|ns/deployment/container" || got.ProductID != "p" || got.EnvironmentID != "e" || got.ApplicationID != "a" {
		t.Fatalf("wrong target identity: %+v", got)
	}
}

func TestTargetIdentityClusterFluxAndLength(t *testing.T) {
	base := Target{APIURL: "https://cluster-a:6443", Namespace: "ns", Deployment: "app", Container: "web", FluxNamespace: "flux", Kustomization: "app"}
	key := func(t Target) string { return (&Observer{Target: t}).TargetIdentity().Target }
	if key(base) != "cluster-a:6443|flux/app|ns/app/web" {
		t.Fatal(key(base))
	}
	variants := []Target{base, base, base, base}
	variants[0].APIURL = "https://cluster-b:6443"
	variants[1].APIURL = "https://cluster-a:7443"
	variants[2].InCluster = true
	variants[3].Kustomization = "other"
	for _, v := range variants {
		if key(v) == key(base) {
			t.Fatal("target collision")
		}
	}
	renamed := base
	renamed.Context = "context-b"
	if key(renamed) != key(base) {
		t.Fatal("context alias changed cluster identity")
	}
	base.Deployment = strings.Repeat("x", 310)
	a := key(base)
	base.Container = "other"
	b := key(base)
	if len([]rune(a)) > 300 || a == b || b != key(base) {
		t.Fatal("invalid bounded key")
	}
}

func TestTargetIdentityResolvedKubeconfigCluster(t *testing.T) {
	var keys []string
	for _, host := range []string{"cluster-a:6443", "cluster-b:6443", "cluster-a:7443"} {
		path := filepath.Join(t.TempDir(), "config")
		config := fmt.Sprintf(`clusters:
- name: cluster
  cluster: {server: "https://%s"}
contexts:
- name: shared
  context: {cluster: cluster}
`, host)
		if err := os.WriteFile(path, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		o, err := New(Target{Kubeconfig: path, Context: "shared", ProductID: "p", EnvironmentID: "e", ApplicationID: "a", Namespace: "ns", Deployment: "app", Container: "web"})
		if err != nil {
			t.Fatal(err)
		}
		got := o.TargetIdentity().Target
		if got != host+"|/|ns/app/web" {
			t.Fatalf("unresolved cluster identity: %s", got)
		}
		for _, key := range keys {
			if got == key {
				t.Fatal("different resolved clusters collided")
			}
		}
		keys = append(keys, got)
	}
}
