package runtimereporter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	rr "releasecontrol/internal/runtimereport"
	"strings"
	"testing"
	"time"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type fakeKube struct{ handler http.Handler }

func (k fakeKube) Get(ctx context.Context, path string, out any) error {
	req := httptest.NewRequest("GET", "https://kube"+path, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	k.handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		return fmt.Errorf("HTTP %d", rec.Code)
	}
	return json.Unmarshal(rec.Body.Bytes(), out)
}
func TestCollectAndPush(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	kube := fakeKube{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Fatal(r.Method)
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/pods"):
			fmt.Fprintf(w, `{"items":[{"metadata":{"labels":{"app":"api"}},"spec":{"containers":[{"name":"api"}]},"status":{"containerStatuses":[{"name":"api","ready":true,"imageID":"containerd://%s"}]}},{"metadata":{"labels":{"app":"unrelated"}},"spec":{"containers":[{"name":"api"}]},"status":{"containerStatuses":[{"name":"api","ready":true,"imageID":"sha256:bad"}]}}]}`, digest)
		case strings.HasSuffix(r.URL.Path, "/deployments"):
			fmt.Fprint(w, `{"items":[{"metadata":{"name":"api","generation":2},"spec":{"selector":{"matchLabels":{"app":"api"}},"template":{"spec":{"containers":[{"name":"api","image":"repo:v1"}]}}},"status":{"observedGeneration":2,"replicas":1,"readyReplicas":1}}]}`)
		default:
			fmt.Fprint(w, `{"items":[]}`)
		}
	})}
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c := Config{URL: "https://rcp", TokenFile: file, ProductID: "p", EnvironmentID: "prod", Cluster: "c", Namespaces: []string{"app"}}
	r, err := Collect(context.Background(), kube, c)
	if err != nil || len(r.Workloads) != 1 {
		t.Fatal(r, err)
	}
	container := r.Workloads[0].Containers[0]
	if container.ImageDigest != digest || container.ReadyPods != 1 || container.TotalPods != 1 {
		t.Fatal(container)
	}
	status := 200
	client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Method != "POST" || req.URL.Path != "/api/runtime-reports" || req.Header.Get("Authorization") != "Bearer secret" {
				t.Fatal(req.Method, req.URL, req.Header)
			}
			var got rr.Report
			if err := json.NewDecoder(req.Body).Decode(&got); err != nil || got.Cluster != "c" {
				t.Fatal(got, err)
			}
			w.WriteHeader(status)
		}).ServeHTTP(rec, req)
		return rec.Result(), nil
	})}
	if code, err := Push(context.Background(), client, c, r); err != nil || code != 200 {
		t.Fatal(code, err)
	}
	status = 429
	if code, err := Push(context.Background(), client, c, r); err == nil || code != 429 {
		t.Fatal(code, err)
	}
	if err := os.WriteFile(file, []byte("rotated"), 0600); err != nil {
		t.Fatal(err)
	}
	client.Transport = roundTrip(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "Bearer rotated" {
			t.Fatal("token not rotated")
		}
		return nil, fmt.Errorf("transport failed")
	})
	if _, err := Push(context.Background(), client, c, r); err == nil {
		t.Fatal("expected failure")
	}
}
func TestConfig(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte("secret"), 0600)
	values := map[string]string{"RCP_TOKEN_FILE": file, "PRODUCT_ID": "p", "CLUSTER": "c", "NAMESPACES": "app, jobs,app"}
	c, err := Load(func(k string) string { return values[k] })
	if err != nil || c.Interval != time.Minute || len(c.Namespaces) != 2 || c.EnvironmentID != "prod" {
		t.Fatal(c, err)
	}
	for k, v := range map[string]string{"INTERVAL": "29s", "RCP_URL": "http://rcp", "NAMESPACES": "", "PRODUCT_ID": "", "CLUSTER": "bad!", "RCP_TOKEN_FILE": "missing"} {
		old := values[k]
		values[k] = v
		if _, err := Load(func(k string) string { return values[k] }); err == nil {
			t.Fatal("accepted", k)
		}
		values[k] = old
	}
}
func TestCollectRejectsPartialLists(t *testing.T) {
	k := fakeKube{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"metadata":{"continue":"next"},"items":[]}`)
	})}
	if _, err := Collect(context.Background(), k, Config{Namespaces: []string{"app"}}); err == nil {
		t.Fatal("accepted partial list")
	}
}
