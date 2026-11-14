package runtimeobserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"releasecontrol/internal/delivery"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// An httptest API transported in process exercises real GET decoding without sockets.
type rolloutTransport struct{ handler http.Handler }

func (r rolloutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	return rec.Result(), nil
}
func TestRolloutKubernetesAPI(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	reference := "main-25dce4d6@" + digest
	repo := "192.168.32.242:5000/deadnodes/pp-back/runtime"
	for _, tc := range []struct{ name, want string }{
		{"ready", "READY"}, {"surge", "PENDING"}, {"condition old failure", "PENDING"}, {"condition old stalled", "PENDING"}, {"condition old ready", "PENDING"}, {"stale failure", "PENDING"}, {"stale stalled", "PENDING"}, {"stale ready", "PENDING"}, {"reference suffix", "PENDING"}, {"deployment stale failure", "PENDING"}, {"deployment old digest failure", "PENDING"}, {"unrelated failed deployment", "READY"}, {"not applied", "PENDING"}, {"generation lag", "PENDING"}, {"updating", "PENDING"}, {"failed", "FAILED"}, {"no deployments", "PENDING"}, {"different repo", "PENDING"}, {"old digest", "PENDING"}, {"one old deployment", "PENDING"}, {"zero replicas", "READY"}, {"stalled", "FAILED"}, {"get error", "PENDING"}, {"deployment ready", "READY"}, {"deployment deadline", "FAILED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.name == "get error" {
					http.Error(w, "private provider response", 503)
					return
				}
				if strings.Contains(r.URL.Path, "helmreleases") {
					applied := reference
					if tc.name == "not applied" {
						applied = "old@sha256:abc"
					}
					attempted := 2
					if strings.HasPrefix(tc.name, "stale ") {
						attempted = 1
					}
					if tc.name == "reference suffix" {
						applied += "extra"
					}
					observed := 2
					if tc.name == "generation lag" {
						observed = 1
					}
					condition := map[string]any{"type": "Ready", "status": "True"}
					if tc.name == "failed" || tc.name == "stale failure" || tc.name == "condition old failure" {
						condition = map[string]any{"type": "Ready", "status": "False", "reason": "UpgradeFailed", "message": "Helm upgrade failed: pre-upgrade hooks failed: job pp-back-migrate BackoffLimitExceeded"}
					}
					if tc.name == "stalled" || tc.name == "stale stalled" || tc.name == "condition old stalled" {
						condition = map[string]any{"type": "Stalled", "status": "True", "reason": "RetriesExceeded", "message": "upgrade retries exhausted"}
					}
					condition["observedGeneration"] = 2
					if strings.HasPrefix(tc.name, "condition old") {
						condition["observedGeneration"] = 1
					}
					json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"generation": 2}, "spec": map[string]any{"values": map[string]any{"image": map[string]any{"tag": applied}}}, "status": map[string]any{"observedGeneration": observed, "lastAttemptedGeneration": attempted, "conditions": []any{condition}}})
					return
				}
				replicas, updated := 2, 2
				if tc.name == "updating" {
					updated = 1
				}
				if tc.name == "zero replicas" {
					replicas = 0
					updated = 0
				}
				image := repo + ":" + reference
				if tc.name == "different repo" {
					image = "other/repo:" + reference
				}
				if tc.name == "old digest" || tc.name == "deployment old digest failure" {
					image = repo + ":old@sha256:old"
				}
				dep := func(image string) map[string]any {
					status := map[string]any{"observedGeneration": 3, "replicas": replicas, "readyReplicas": replicas, "updatedReplicas": updated, "availableReplicas": replicas}
					if tc.name == "surge" {
						status["replicas"] = replicas * 2
					}
					if tc.name == "deployment deadline" || tc.name == "deployment stale failure" || tc.name == "deployment old digest failure" || tc.name == "unrelated failed deployment" {
						status["conditions"] = []any{map[string]any{"type": "Progressing", "status": "False", "reason": "ProgressDeadlineExceeded", "message": "ReplicaSet timed out"}}
					}
					if tc.name == "deployment stale failure" {
						status["observedGeneration"] = 2
					}
					return map[string]any{"metadata": map[string]any{"generation": 3, "name": "pp-back"}, "spec": map[string]any{"replicas": replicas, "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"image": image}}}}}, "status": status}
				}
				if strings.HasPrefix(tc.name, "deployment ") {
					if r.URL.Path != "/apis/apps/v1/namespaces/lp-dev/deployments/pp-back" {
						t.Error(r.URL)
					}
					json.NewEncoder(w).Encode(dep(image))
					return
				}
				if r.URL.Query().Get("labelSelector") != "helm.toolkit.fluxcd.io/name=pp-back" {
					t.Error(r.URL)
				}
				items := []any{dep(image), dep(image)}
				if tc.name == "unrelated failed deployment" {
					items = []any{dep("other/repo:" + reference)}
					good := dep(image)
					good["status"].(map[string]any)["conditions"] = nil
					items = append(items, good)
				}
				if tc.name == "no deployments" {
					items = nil
				}
				if tc.name == "one old deployment" {
					items = append(items, dep(repo+":old@sha256:old"))
				}
				json.NewEncoder(w).Encode(map[string]any{"items": items})
			})
			verifier := &RolloutVerifier{observer: &Observer{Target: Target{APIURL: "https://kube.example"}, client: &http.Client{Transport: rolloutTransport{handler}}}}
			target := delivery.RolloutTarget{Namespace: "lp-dev", HelmRelease: "pp-back", Repository: repo, Digest: digest, Reference: reference}
			if strings.HasPrefix(tc.name, "deployment ") {
				target.HelmRelease = ""
				target.Deployment = "pp-back"
			}
			result, err := verifier.VerifyRollout(context.Background(), delivery.RolloutPlan{Targets: []delivery.RolloutTarget{target}})
			if err != nil || result.Phase != tc.want || len(result.Targets) != 1 {
				t.Fatal(result, err)
			}
			if tc.name == "failed" && !strings.Contains(result.Reason, "BackoffLimitExceeded") {
				t.Fatal(result)
			}
			if strings.HasPrefix(tc.name, "stale ") && result.Reason != "HelmRelease has not attempted the deployed generation" {
				t.Fatal(result)
			}
			if tc.name == "get error" && strings.Contains(result.Reason, "private") {
				t.Fatal("provider body leaked")
			}
		})
	}
}
func TestRolloutReasonBound(t *testing.T) {
	if got := shortReason("UpgradeFailed: " + strings.Repeat("界", 600)); len([]rune(got)) != 315 {
		t.Fatal(fmt.Sprint(len([]rune(got))))
	}
}

func TestRolloutReasonRedactsCredentials(t *testing.T) {
	reason := shortReason("UpgradeFailed: Bearer private-token password=private-password https://user:private-pass@example.test/error")
	for _, secret := range []string{"private-token", "private-password", "private-pass"} {
		if strings.Contains(reason, secret) {
			t.Fatal(reason)
		}
	}
}

func TestRolloutReferenceIdentity(t *testing.T) {
	for _, tc := range []struct {
		value, reference string
		want             bool
	}{
		{"main@sha256:abc", "main@sha256:abc", true},
		{"registry:5000/app:main@sha256:abc", "main@sha256:abc", true},
		{"main@sha256:abcdef", "main@sha256:abc", false},
		{"old-main@sha256:abc", "main@sha256:abc", false},
		{"main@sha256:abc", "", false},
	} {
		raw, _ := json.Marshal(map[string]any{"nested": []any{tc.value}})
		if got := containsReference(raw, tc.reference); got != tc.want {
			t.Fatalf("%q: %v", tc.value, got)
		}
	}
}

func TestRolloutIndependentHelmReleases(t *testing.T) {
	for _, mode := range []string{"ready", "missing", "old digest", "failed"} {
		t.Run(mode, func(t *testing.T) {
			seen := map[string]bool{}
			var seenMu sync.Mutex
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/helmreleases/") {
					name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
					seenMu.Lock()
					seen[name] = true
					seenMu.Unlock()
					condition := map[string]any{"type": "Ready", "status": "True"}
					if name == "consumer" && mode == "failed" {
						condition = map[string]any{"type": "Ready", "status": "False", "reason": "UpgradeFailed"}
					}
					condition["observedGeneration"] = 1
					json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"generation": 1}, "spec": map[string]any{"values": map[string]any{"tag": "sha256:abc"}}, "status": map[string]any{"observedGeneration": 1, "lastAttemptedGeneration": 1, "conditions": []any{condition}}})
					return
				}
				selector := r.URL.Query().Get("labelSelector")
				if selector != "helm.toolkit.fluxcd.io/name=api" && selector != "helm.toolkit.fluxcd.io/name=consumer" {
					t.Error(selector)
				}
				image := "registry/app@sha256:abc"
				if strings.HasSuffix(selector, "=consumer") && mode == "old digest" {
					image = "registry/app@sha256:old"
				}
				items := []any{map[string]any{"metadata": map[string]any{"generation": 1}, "spec": map[string]any{"replicas": 1, "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"image": image}}}}}, "status": map[string]any{"observedGeneration": 1, "replicas": 1, "readyReplicas": 1, "updatedReplicas": 1, "availableReplicas": 1}}}
				if strings.HasSuffix(selector, "=consumer") && mode == "missing" {
					items = nil
				}
				json.NewEncoder(w).Encode(map[string]any{"items": items})
			})
			v := &RolloutVerifier{observer: &Observer{Target: Target{APIURL: "https://kube.example"}, client: &http.Client{Transport: rolloutTransport{handler}}}}
			targets := []delivery.RolloutTarget{}
			for _, name := range []string{"api", "consumer"} {
				targets = append(targets, delivery.RolloutTarget{Namespace: "lp-dev", HelmRelease: name, Repository: "registry/app", Digest: "sha256:abc", Reference: "sha256:abc"})
			}
			result, err := v.VerifyRollout(context.Background(), delivery.RolloutPlan{Targets: targets})
			want := "PENDING"
			if mode == "ready" {
				want = "READY"
			}
			if mode == "failed" {
				want = "FAILED"
			}
			if err != nil || result.Phase != want || len(result.Targets) != 2 || len(seen) != 2 || result.Targets[0].Phase != "READY" {
				t.Fatal(result, err, seen)
			}
		})
	}
}

func TestDeploymentReplicaConvergence(t *testing.T) {
	var d rolloutDeployment
	if err := json.Unmarshal([]byte(`{"metadata":{"generation":2},"spec":{"replicas":2,"template":{"spec":{"containers":[{"image":"repo/app@sha256:abc"}]}}},"status":{"observedGeneration":3,"replicas":2,"updatedReplicas":2,"readyReplicas":2,"availableReplicas":2}}`), &d); err != nil {
		t.Fatal(err)
	}
	target := delivery.RolloutTarget{Repository: "repo/app", Digest: "sha256:abc"}
	for _, field := range []string{"complete", "surge", "updated", "ready", "available", "unavailable", "generation"} {
		t.Run(field, func(t *testing.T) {
			copy := d
			switch field {
			case "surge":
				copy.Status.Replicas = 4 // old pods remain available while new pods are not ready
			case "updated":
				copy.Status.UpdatedReplicas = 1
			case "ready":
				copy.Status.ReadyReplicas = 1
			case "available":
				copy.Status.AvailableReplicas = 1
			case "unavailable":
				copy.Status.UnavailableReplicas = 1
			case "generation":
				copy.Status.ObservedGeneration = 1
			}
			_, phase, _ := checkDeployment(copy, target)
			want := "PENDING"
			if field == "complete" {
				want = "READY"
			}
			if phase != want {
				t.Fatal(phase)
			}
		})
	}
}

func TestRolloutManySlowHealthyTargets(t *testing.T) {
	var active, peak atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 10*time.Second {
			t.Error("missing per-target timeout")
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, `{"metadata":{"generation":1},"spec":{"replicas":1,"template":{"spec":{"containers":[{"image":"registry/app@sha256:abc"}]}}},"status":{"observedGeneration":1,"replicas":1,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1}}`)
	})
	v := &RolloutVerifier{observer: &Observer{Target: Target{APIURL: "https://kube.example"}, client: &http.Client{Transport: rolloutTransport{handler}}}}
	var plan delivery.RolloutPlan
	for i := range 12 {
		plan.Targets = append(plan.Targets, delivery.RolloutTarget{Namespace: "dev", Deployment: fmt.Sprint(i), Repository: "registry/app", Digest: "sha256:abc"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	result, err := v.VerifyRollout(ctx, plan)
	if err != nil || result.Phase != "READY" || len(result.Targets) != 12 {
		t.Fatal(result, err)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatal(elapsed)
	}
	if got := peak.Load(); got != 4 {
		t.Fatalf("peak concurrency = %d, want 4", got)
	}
	for i, evidence := range result.Targets {
		if evidence.Target != plan.Targets[i] || evidence.Phase != "READY" {
			t.Fatal(evidence)
		}
	}
}

func TestRolloutAggregateFailureDominatesPending(t *testing.T) {
	for _, names := range [][]string{{"pending", "failed"}, {"failed", "pending"}} {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/pending") {
				http.Error(w, "unavailable", 503)
				return
			}
			fmt.Fprint(w, `{"metadata":{"generation":1},"spec":{"template":{"spec":{"containers":[{"image":"registry/app@sha256:abc"}]}}},"status":{"observedGeneration":1,"conditions":[{"type":"Progressing","status":"False","reason":"ProgressDeadlineExceeded","message":"timed out"}]}}`)
		})
		v := &RolloutVerifier{observer: &Observer{Target: Target{APIURL: "https://kube.example"}, client: &http.Client{Transport: rolloutTransport{handler}}}}
		var plan delivery.RolloutPlan
		for _, name := range names {
			plan.Targets = append(plan.Targets, delivery.RolloutTarget{Namespace: "dev", Deployment: name, Repository: "registry/app", Digest: "sha256:abc"})
		}
		result, err := v.VerifyRollout(context.Background(), plan)
		if err != nil || result.Phase != "FAILED" || !strings.Contains(result.Reason, "ProgressDeadlineExceeded") {
			t.Fatal(result, err)
		}
	}
}
