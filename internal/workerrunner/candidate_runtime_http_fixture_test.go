package workerrunner

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/providers/runtimeobserver"
)

// Only this HTTPS peer simulates Kubernetes. The production observer performs
// TLS, authentication, configured target reads and controller ownership checks.
type candidateRuntimeHTTPFixture struct {
	mu               sync.Mutex
	server           *httptest.Server
	image, digest    string
	gitops           *candidateGitOpsHTTPFixture
	wrong            bool
	reads, mutations int
	kubeconfig       string
	roleImages       map[string][2]string
}

func newCandidateRuntimeHTTPFixture(t *testing.T, image, digest string, gitops *candidateGitOpsHTTPFixture) (*candidateRuntimeHTTPFixture, []delivery.RuntimeSnapshotObserver) {
	t.Helper()
	f := &candidateRuntimeHTTPFixture{image: image, digest: digest, gitops: gitops}
	f.server = httptest.NewTLSServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.server.Close)
	dir := t.TempDir()
	ca := filepath.Join(dir, "fixture-ca.pem")
	token := filepath.Join(dir, "fixture-token")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("fixture-runtime-only"), 0600); err != nil {
		t.Fatal(err)
	}
	kubeconfig := filepath.Join(dir, "fixture-kubeconfig.json")
	f.kubeconfig = kubeconfig
	config := map[string]any{"apiVersion": "v1", "kind": "Config", "clusters": []any{map[string]any{"name": "fixture", "cluster": map[string]any{"server": f.server.URL, "certificate-authority": ca}}}, "users": []any{map[string]any{"name": "fixture", "user": map[string]any{"tokenFile": token}}}, "contexts": []any{map[string]any{"name": "fixture-dev", "context": map[string]any{"cluster": "fixture", "user": "fixture"}}}, "current-context": "fixture-dev"}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(kubeconfig, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	return f, f.observers(t, "component", []string{"api", "worker"})
}
func (f *candidateRuntimeHTTPFixture) observers(t *testing.T, application string, roles []string) []delivery.RuntimeSnapshotObserver {
	t.Helper()
	observers := []delivery.RuntimeSnapshotObserver{}
	for _, role := range roles {
		observer, err := runtimeobserver.New(runtimeobserver.Target{ProductID: "p", EnvironmentID: "dev", ApplicationID: application, Context: "fixture-dev", Namespace: "dev", Deployment: role, Container: "app", FluxNamespace: "flux-system", Kustomization: "candidate-dev", Kubeconfig: f.kubeconfig, VerificationGroup: "candidate-complete-v1"})
		if err != nil {
			t.Fatal(err)
		}
		observers = append(observers, observer)
	}
	return observers
}
func (f *candidateRuntimeHTTPFixture) setRoleImage(role, image, digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.roleImages == nil {
		f.roleImages = map[string][2]string{}
	}
	f.roleImages[role] = [2]string{image, digest}
}
func (f *candidateRuntimeHTTPFixture) setImage(image, digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.image, f.digest = image, digest
}
func (f *candidateRuntimeHTTPFixture) setWrongDigest(wrong bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wrong = wrong
}
func (f *candidateRuntimeHTTPFixture) assertReadOnly(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reads < 8 || f.mutations != 0 {
		t.Fatalf("runtime IO not full read-only coverage reads=%d mutations=%d", f.reads, f.mutations)
	}
}
func (f *candidateRuntimeHTTPFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		f.mutations++
		http.Error(w, "runtime mutation forbidden", 405)
		return
	}
	f.reads++
	if r.Header.Get("Authorization") != "Bearer fixture-runtime-only" {
		http.Error(w, "wrong scoped fixture identity", 403)
		return
	}
	reply := func(v any) { json.NewEncoder(w).Encode(v) }
	if r.URL.Path == "/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/flux-system/kustomizations/candidate-dev" {
		f.gitops.mu.Lock()
		head := f.gitops.refs["main"]
		f.gitops.mu.Unlock()
		reply(map[string]any{"metadata": map[string]any{"generation": 1}, "status": map[string]any{"observedGeneration": 1, "lastAppliedRevision": "main@sha1:" + head, "conditions": []any{map[string]string{"type": "Ready", "status": "True"}}}})
		return
	}
	role := ""
	if strings.HasPrefix(r.URL.Path, "/apis/apps/v1/namespaces/dev/deployments/") {
		role = strings.TrimPrefix(r.URL.Path, "/apis/apps/v1/namespaces/dev/deployments/")
	} else {
		role = strings.TrimPrefix(r.URL.Query().Get("labelSelector"), "app=")
		if r.URL.Query().Get("limit") != "500" {
			http.Error(w, "missing bounded list", 400)
			return
		}
	}
	configured, extra := f.roleImages[role]
	if role != "api" && role != "worker" && !extra {
		http.Error(w, "unknown configured role", 404)
		return
	}
	repository, digest := f.image, f.digest
	if extra {
		repository, digest = configured[0], configured[1]
	}
	image := repository + "@" + digest
	switch r.URL.Path {
	case "/apis/apps/v1/namespaces/dev/deployments/" + role:
		reply(map[string]any{"metadata": map[string]any{"name": role, "uid": "deployment-" + role, "generation": 2}, "spec": map[string]any{"replicas": 1, "selector": map[string]any{"matchLabels": map[string]string{"app": role}}, "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "app", "image": image, "env": []any{map[string]string{"name": "PRIVATE_APPLICATION_VALUE", "value": "must-not-persist-runtime-payload"}}}}}}}, "status": map[string]any{"observedGeneration": 2, "replicas": 1, "updatedReplicas": 1, "availableReplicas": 1, "readyReplicas": 1}})
	case "/apis/apps/v1/namespaces/dev/replicasets":
		reply(map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"uid": "replicaset-" + role, "ownerReferences": []any{map[string]any{"kind": "Deployment", "uid": "deployment-" + role, "controller": true}}}}}})
	case "/api/v1/namespaces/dev/pods":
		if f.wrong {
			digest = "sha256:" + strings.Repeat("f", 64)
		}
		pod := func(uid, owner string) any {
			return map[string]any{"metadata": map[string]any{"name": uid, "uid": uid, "ownerReferences": []any{map[string]any{"kind": "ReplicaSet", "uid": owner, "controller": true}}}, "spec": map[string]string{"sensitive": "must-not-persist-runtime-payload"}, "status": map[string]any{"phase": "Running", "conditions": []any{map[string]string{"type": "Ready", "status": "True"}}, "containerStatuses": []any{map[string]any{"name": "app", "image": image, "imageID": "docker-pullable://" + repository + "@" + digest, "ready": true}}}}
		}
		reply(map[string]any{"items": []any{pod("pod-"+role, "replicaset-"+role), pod("foreign-"+role, "foreign-replicaset")}})
	default:
		http.Error(w, "unexpected scoped Kubernetes path", 404)
	}
}

func TestCandidateRuntimeHTTPPeerUsesProductionObserver(t *testing.T) {
	gitops := newCandidateGitOpsHTTPFixture(t)
	image := "127.0.0.1:5000/fixture/app"
	gitops.configure(image)
	digest := "sha256:" + strings.Repeat("d", 64)
	peer, observers := newCandidateRuntimeHTTPFixture(t, image, digest, gitops)
	for _, observer := range observers {
		scope, err := observer.(delivery.RuntimeVerificationObserver).RuntimeVerificationScope()
		if err != nil || scope.Context != "fixture-dev" || scope.Namespace != "dev" || scope.VerificationGroup != "candidate-complete-v1" || len(scope.BindingHash) != 64 {
			t.Fatalf("scoped HTTPS fixture invalid %+v %v", scope, err)
		}
		snapshot := observer.Snapshot(context.Background())
		if snapshot.Health != "HEALTHY" || len(snapshot.Errors) != 0 || len(snapshot.Pods) != 1 || snapshot.Pods[0].ImageID != "docker-pullable://"+image+"@"+digest || snapshot.Flux == nil || snapshot.Flux.Status != "READY" {
			t.Fatalf("production HTTPS observer fixture not healthy %+v", snapshot)
		}
	}
	peer.assertReadOnly(t)
}
