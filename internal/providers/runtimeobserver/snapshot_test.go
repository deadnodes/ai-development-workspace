package runtimeobserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSnapshotOwnerIdentityAndPartialEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, mode, health string
		count              int
	}{
		{"owned healthy only", "", "HEALTHY", 1},
		{"stale generation", "stale", "NOT_READY", 1},
		{"scaled zero", "zero", "SCALED_ZERO", 0},
		{"pending pod", "pending", "NOT_READY", 1},
		{"pods forbidden", "forbidden", "UNKNOWN", 0},
		{"malformed deployment", "malformed", "UNKNOWN", 0},
		{"missing deployment", "missing", "UNKNOWN", 0},
		{"missing container", "container", "UNKNOWN", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Errorf("unexpected method %s", r.Method)
				}
				dep := `{"metadata":{"uid":"dep","generation":2},"spec":{"replicas":1,"selector":{"matchLabels":{"app":"test"}},"template":{"spec":{"containers":[{"name":"app","image":"ghcr.io/org/app:v2"}]}}},"status":{"observedGeneration":2,"replicas":1,"updatedReplicas":1,"availableReplicas":1,"readyReplicas":1}}`
				switch {
				case strings.Contains(r.URL.Path, "deployments/"):
					switch tc.mode {
					case "missing":
						w.WriteHeader(404)
						return
					case "malformed":
						dep = `{`
					case "stale":
						dep = strings.Replace(dep, `"observedGeneration":2`, `"observedGeneration":1`, 1)
					case "zero":
						dep = strings.ReplaceAll(dep, `:1`, `:0`)
					case "container":
						dep = strings.Replace(dep, `"name":"app"`, `"name":"other"`, 1)
					}
					w.Write([]byte(dep))
				case strings.HasSuffix(r.URL.Path, "replicasets"):
					w.Write([]byte(`{"items":[{"metadata":{"uid":"rs","ownerReferences":[{"kind":"Deployment","uid":"dep","controller":true}]}},{"metadata":{"uid":"foreign-rs","ownerReferences":[{"kind":"Deployment","uid":"foreign","controller":true}]}}]}`))
				case strings.HasSuffix(r.URL.Path, "pods"):
					if tc.mode == "forbidden" {
						w.WriteHeader(403)
						return
					}
					if r.URL.Query().Get("limit") != "500" {
						t.Error("missing bounded list")
					}
					pod := func(name, owner string) map[string]any {
						phase := "Running"
						if tc.mode == "pending" {
							phase = "Pending"
						}
						return map[string]any{"metadata": map[string]any{"name": name, "uid": name, "ownerReferences": []any{map[string]any{"uid": owner, "kind": "ReplicaSet", "controller": true}}}, "spec": map[string]any{"sensitive": "do-not-store"}, "status": map[string]any{"phase": phase, "conditions": []any{map[string]string{"type": "Ready", "status": "True"}}, "containerStatuses": []any{map[string]any{"name": "app", "image": "ghcr.io/org/app:v2", "imageID": "containerd://ghcr.io/org/app@sha256:abc", "ready": true}}}}
					}
					items := []any{pod("foreign", "foreign-rs")}
					if tc.mode != "zero" {
						items = append(items, pod("ours", "rs"))
					}
					json.NewEncoder(w).Encode(map[string]any{"items": items})
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			o := &Observer{Target: Target{ProductID: "p", EnvironmentID: "dev", ApplicationID: "app", APIURL: srv.URL, Namespace: "test", Deployment: "test", Container: "app"}, client: srv.Client()}
			got := o.Snapshot(context.Background())
			if got.Health != tc.health || len(got.Pods) != tc.count {
				t.Fatalf("snapshot %+v", got)
			}
			if tc.mode == "forbidden" && (got.WorkloadImage == "" || len(got.Errors) == 0) {
				t.Fatal("partial evidence lost")
			}
			b, _ := json.Marshal(got)
			if strings.Contains(string(b), "do-not-store") {
				t.Fatal("sensitive pod fields persisted")
			}
		})
	}
}

func TestSnapshotFluxFailureDoesNotHideRuntime(t *testing.T) {
	// Optional Flux failure is evidence about Flux, not proof runtime is absent.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer srv.Close()
	o := &Observer{Target: Target{APIURL: srv.URL, FluxNamespace: "flux-system", Kustomization: "apps"}, client: srv.Client()}
	r := o.Snapshot(context.Background())
	if r.Flux == nil || r.Flux.Status != "UNKNOWN" || r.Flux.Error == "" || len(r.Errors) == 0 {
		t.Fatalf("%+v", r)
	}
}
