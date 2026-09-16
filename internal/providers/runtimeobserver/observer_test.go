package runtimeobserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
