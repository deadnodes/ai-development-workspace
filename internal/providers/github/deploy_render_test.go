package github

import (
	"errors"
	"strings"
	"testing"
)

func deployYAML(repo, tag string) string {
	return "# keep\napiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nmetadata:\n  name: app\nspec:\n  values:\n    image:\n      repository: \"" + repo + "\"\n      tag: " + tag + " # comment\n      pullPolicy: IfNotPresent\n"
}
func TestDeployRenderPreservesBytes(t *testing.T) {
	for _, quote := range []string{"", "'", "\""} {
		t.Run(quote, func(t *testing.T) {
			old := quote + "old@sha256:" + strings.Repeat("a", 64) + quote
			value := "main-aaaaaaaa@sha256:" + strings.Repeat("b", 64)
			content := deployYAML("registry/o/r", old) + "---\n" + deployYAML("registry/other", old) + "---\n" + deployYAML("registry/o/r", old)
			rendered, e := RenderDeployment(content, []DeployTag{{"registry/o/r", value, 2, ""}})
			if e != nil {
				t.Fatal(e)
			}
			expected := strings.ReplaceAll(deployYAML("registry/o/r", old), old, quote+value+quote) + "---\n" + deployYAML("registry/other", old) + "---\n" + strings.ReplaceAll(deployYAML("registry/o/r", old), old, quote+value+quote)
			if rendered != expected {
				t.Fatalf("changed unrelated bytes:\n%s", rendered)
			}
			again, e := RenderDeployment(rendered, []DeployTag{{"registry/o/r", value, 2, ""}})
			if e != nil || again != rendered {
				t.Fatal(e, "not idempotent")
			}
		})
	}
}
func TestDeployRenderMinimumAndMalformed(t *testing.T) {
	value := "main-a@sha256:" + strings.Repeat("a", 64)
	_, e := RenderDeployment(deployYAML("registry/o/r", "old"), []DeployTag{{"registry/o/r", value, 2, ""}})
	if !errors.Is(e, ErrTargetMissing) {
		t.Fatal(e)
	}
	for _, content := range []string{"bad: [", strings.Replace(deployYAML("registry/o/r", "old"), "tag: old", "tag: &version old", 1), strings.Replace(deployYAML("registry/o/r", "old"), "tag: old", "tag: old\n      tag: another", 1)} {
		if _, e := RenderDeployment(content, []DeployTag{{"registry/o/r", value, 1, ""}}); e == nil {
			t.Fatal("unsafe YAML accepted", content)
		}
	}
}

func TestDeploymentMultipleImagesInOneFile(t *testing.T) {
	a, b := "main-a@sha256:"+strings.Repeat("a", 64), "main-b@sha256:"+strings.Repeat("b", 64)
	unrelated := "---\napiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nspec:\n  values:\n    enabled: true\n"
	content := deployYAML("registry/api", "old") + "---\n" + deployYAML("registry/web", "'old'") + unrelated
	actual, e := RenderDeployment(content, []DeployTag{{"registry/api", a, 1, ""}, {"registry/web", b, 1, ""}})
	if e != nil {
		t.Fatal(e)
	}
	want := deployYAML("registry/api", a) + "---\n" + deployYAML("registry/web", "'"+b+"'") + unrelated
	if actual != want {
		t.Fatalf("unexpected bytes %s", actual)
	}
}

func TestContainerImageRendering(t *testing.T) {
	repo := "192.168.32.242:5000/deadnodes/meteroid-web"
	value := "forgejo-new@sha256:" + strings.Repeat("b", 64)
	for _, quote := range []string{"", "'", "\""} {
		old := quote + repo + ":old@sha256:" + strings.Repeat("a", 64) + quote
		content := "# keep\r\nkind: Deployment\r\nspec:\r\n  template:\r\n    spec:\r\n      containers:\r\n        - name: web\r\n          image: " + old + " # keep\r\n        - name: unrelated\r\n          image: " + repo + "-other:old\r\n      initContainers:\r\n        - name: init\r\n          image: " + old + "\r\n---\r\nkind: Job\r\nspec:\r\n  template:\r\n    spec:\r\n      containers:\r\n        - image: " + old + "\r\n      other:\r\n        image: " + old + "\r\n"
		selector := []DeployTag{{Repository: repo, Value: value, MinMatches: 3, Format: "container_image"}}
		got, err := RenderDeployment(content, selector)
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(content, old, quote+repo+":"+value+quote, 3)
		if got != want {
			t.Fatalf("unexpected bytes:\n%s", got)
		}
		again, err := RenderDeployment(got, selector)
		if err != nil || again != got {
			t.Fatal("not already applied", err)
		}
		selector[0].MinMatches = 4
		if _, err := RenderDeployment(content, selector); !errors.Is(err, ErrTargetMissing) {
			t.Fatal(err)
		}
	}
}
