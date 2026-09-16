package github

import (
	"context"
	"encoding/base64"
	"net/http"
	"releasecontrol/internal/delivery"
	"strings"
	"testing"
)

func TestReadGitOpsWithoutDesiredImageAndPreserveMutableTag(t *testing.T) {
	content := helmDoc("app", "registry.example/team/app", "dev-12345678") + "---\n" + helmDoc("worker", "registry.example/team/app", "dev-12345678") + "---\napiVersion: v1\nkind: Service\nmetadata: {name: app}\n"
	p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("unexpected mutation %s", r.Method)
		}
		if strings.Contains(r.URL.Path, "/git/ref/") {
			jsonOut(w, map[string]any{"object": map[string]string{"sha": strings.Repeat("a", 40)}})
			return
		}
		jsonOut(w, map[string]string{"type": "file", "encoding": "base64", "sha": strings.Repeat("b", 40), "content": base64.StdEncoding.EncodeToString([]byte(content))})
	})
	c.Owner = "example"
	req := delivery.GitOpsRequest{Repository: "example/gitops", Ref: "main", Path: "app.yaml", ImageField: "spec.values.image.repository", DigestField: "spec.values.image.tag"}
	got, err := p.ReadGitOps(context.Background(), c, req)
	if err != nil || got.ImageRepository != "registry.example/team/app" || got.Digest != "dev-12345678" {
		t.Fatalf("observation %+v %v", got, err)
	}
	if err := gitopsPath(req); err == nil {
		t.Fatal("writer accepted missing desired image")
	}
	_, err = p.ApplyGitOps(context.Background(), c, delivery.GitOpsApply{Request: req, ExpectedHeadSHA: strings.Repeat("a", 40), ExpectedBlobSHA: strings.Repeat("b", 40), Digest: "sha256:" + strings.Repeat("c", 64), OperationID: "test"})
	if err == nil {
		t.Fatal("mutation accepted missing desired image")
	}
}

func TestObservedGitOpsRejectsAmbiguousVersions(t *testing.T) {
	r := delivery.GitOpsRequest{ImageField: "spec.values.image.repository", DigestField: "spec.values.image.tag"}
	for _, second := range []string{helmDoc("worker", "ghcr.io/team/app", "dev-two"), helmDoc("worker", "ghcr.io/team/other", "dev-one")} {
		if _, _, err := observedImageFields(helmDoc("app", "ghcr.io/team/app", "dev-one")+"---\n"+second, r); err == nil {
			t.Fatal("ambiguous observation accepted")
		}
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	_, got, err := observedImageFields(helmDoc("app", "ghcr.io/team/app", "rcp@"+digest), r)
	if err != nil || got != digest {
		t.Fatalf("digest normalized incorrectly: %s %v", got, err)
	}
}
