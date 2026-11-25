package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/buildconfig"
)

func TestDeploymentReconcileProviderMethods(t *testing.T) {
	g := buildconfig.GitOps{Kind: "github", AppID: 1, InstallationID: 2, PrivateKeyRef: "github.pem", Repository: "o/flux", Branch: "main"}
	mainSHA := strings.Repeat("a", 40)
	want := "main-aaaaaaaa@sha256:" + strings.Repeat("b", 64)
	content := deployYAML("registry/o/app", want)
	var methods []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		var code int
		var body any
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/git/ref/heads/main"):
			code, body = 200, map[string]any{"object": map[string]string{"sha": mainSHA}}
		case r.Method == "GET" && r.URL.Path == "/repos/o/flux/contents/app.yaml":
			code, body = 200, map[string]string{"type": "file", "encoding": "base64", "sha": gitOpsBlobSHA([]byte(content)), "content": base64.StdEncoding.EncodeToString([]byte(content))}
		case r.Method == "PATCH" && r.URL.Path == "/repos/o/flux/pulls/12":
			code, body = 200, map[string]string{"state": "closed"}
		case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/git/refs/heads/rcp/deploy/"):
			code, body = 204, map[string]any{}
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(code)
		if code != http.StatusNoContent {
			_ = json.NewEncoder(w).Encode(body)
		}
	}))
	defer server.Close()
	g.APIURL = server.URL
	p := NewWithClient(server.Client())
	c, _ := deployConnection(g)
	key := fmt.Sprintf("%s|%d|%d|%s", g.APIURL, g.AppID, g.InstallationID, c.PrivateKeyRef)
	p.tokens[key] = cachedToken{"token", time.Now().Add(time.Hour)}
	plan := DeployPlan{ID: "deployment", Branch: "rcp/deploy/deployment", Files: []DeployFile{{Path: "app.yaml", Tags: []DeployTag{{Repository: "registry/o/app", Value: want, MinMatches: 1}}}}}
	sha, applied, err := p.DeploymentAppliedOnMain(context.Background(), g, plan)
	if err != nil || !applied || sha != mainSHA {
		t.Fatalf("%s %v %v", sha, applied, err)
	}
	if err = p.CloseDeployment(context.Background(), g, 12); err != nil {
		t.Fatal(err)
	}
	if err = p.DeleteDeploymentBranch(context.Background(), g, plan.Branch); err != nil {
		t.Fatal(err)
	}
	if err = p.DeleteDeploymentBranch(context.Background(), g, "user/branch"); err == nil {
		t.Fatal("accepted non deployment branch")
	}
	if len(methods) != 4 {
		t.Fatalf("unexpected requests: %v", methods)
	}
}
