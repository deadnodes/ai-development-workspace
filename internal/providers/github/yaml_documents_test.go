package github

import (
	"fmt"
	"os"
	"path/filepath"
	"releasecontrol/internal/delivery"
	"strings"
	"testing"
)

func helmDoc(name, repo, tag string) string {
	return fmt.Sprintf("# preserved header\napiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nmetadata:\n  name: %s\nspec:\n  values:\n    image:\n      repository: %s # repository comment\n      tag: '%s' # tag comment\n    untouched: keep\n", name, repo, tag)
}
func TestMultiDocumentImageReplacement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		count   int
		service bool
	}{{"learning-brain", 1, false}, {"notification-service", 2, false}, {"platform-control-api", 4, true}, {"pp-smart-review-llm", 3, false}} {
		t.Run(tc.name, func(t *testing.T) {
			repo := "ghcr.io/deadnodes/" + tc.name
			r := delivery.GitOpsRequest{ImageRepository: repo, ImageField: "spec.values.image.repository", DigestField: "spec.values.image.tag"}
			content := ""
			for i := 0; i < tc.count; i++ {
				if i > 0 {
					content += "---\n"
				}
				content += helmDoc(fmt.Sprintf("%s-%d", tc.name, i), repo, "dev-old")
			}
			service := "---\n# unrelated Service must remain exact\napiVersion: v1\nkind: Service\nmetadata:\n  name: backend\nspec:\n  ports: [{port: 80}]\n"
			if tc.service {
				content += service
			}
			unrelated := "---\n" + helmDoc("other", "ghcr.io/other/image", "keep")
			content += unrelated
			docs, e := imageDocuments(content, r)
			if e != nil {
				t.Fatal(e)
			}
			if len(docs) != tc.count {
				t.Fatal(len(docs))
			}
			digest := "rcp@sha256:" + strings.Repeat("a", 64)
			updated, e := replaceImageScalars(content, docs, repo, digest)
			if e != nil {
				t.Fatal(e)
			}
			if strings.Count(updated, digest) != tc.count || !strings.Contains(updated, unrelated) || tc.service && !strings.Contains(updated, service) {
				t.Fatal("non-target document changed")
			}
			if strings.Count(updated, "# tag comment") != tc.count+1 {
				t.Fatal("comments lost")
			}
			got, e := imageDocuments(updated, r)
			if e != nil || len(got) != tc.count || got[0].digest.Value != digest {
				t.Fatalf("invalid updated documents: %v", e)
			}
		})
	}
}
func TestMultiDocumentRejectsMixedTargetVersions(t *testing.T) {
	r := delivery.GitOpsRequest{ImageRepository: "ghcr.io/acme/app", ImageField: "spec.values.image.repository", DigestField: "spec.values.image.tag"}
	_, e := imageDocuments(helmDoc("a", r.ImageRepository, "v1")+"---\n"+helmDoc("b", r.ImageRepository, "v2"), r)
	if e == nil {
		t.Fatal("accepted inconsistent target tags")
	}
	_, e = imageDocuments(helmDoc("a", "ghcr.io/acme/other", "v1"), r)
	if e == nil {
		t.Fatal("accepted missing target")
	}
}

func TestLocalGitOpsSamples(t *testing.T) {
	root := os.Getenv("RCP_TEST_GITOPS_SAMPLES")
	if root == "" {
		t.Skip("set RCP_TEST_GITOPS_SAMPLES to opt into local read-only manifest validation")
	}
	for _, name := range []string{"learning-brain", "notification-service", "platform-control-api", "pp-smart-review-llm"} {
		b, e := os.ReadFile(filepath.Join(root, name+".yaml"))
		if e != nil {
			t.Fatal(e)
		}
		r := delivery.GitOpsRequest{ImageRepository: "ghcr.io/deadnodes/" + name, ImageField: "spec.values.image.repository", DigestField: "spec.values.image.tag"}
		docs, e := imageDocuments(string(b), r)
		if e != nil {
			t.Fatalf("%s: %v", name, e)
		}
		result, e := replaceImageScalars(string(b), docs, r.ImageRepository, "rcp@sha256:"+strings.Repeat("a", 64))
		if e != nil {
			t.Fatalf("%s: %v", name, e)
		}
		if _, e = imageDocuments(result, r); e != nil {
			t.Fatal(e)
		}
		t.Logf("%s validated %d HelmRelease targets without writing files", name, len(docs))
	}
}
