package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"releasecontrol/internal/delivery"
	"strings"
	"testing"
)

func TestGitOpsPullRequestOnlyWritesGeneratedBranch(t *testing.T) {
	base := strings.Repeat("a", 40)
	head := strings.Repeat("b", 40)
	blob := strings.Repeat("c", 40)
	digest := "sha256:" + strings.Repeat("d", 64)
	op := "abcdef0123456789"
	branch := "rcp/gitops/" + op
	created := false
	applied := false
	prCreated := false
	writes := 0
	pr := func() map[string]any {
		return map[string]any{"number": 7, "html_url": "https://github.test/pr/7", "state": "open", "head": map[string]string{"ref": branch, "sha": head}, "base": map[string]string{"ref": "main"}}
	}
	p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/git/ref/heads/"):
			if strings.HasSuffix(path, branch) && !created {
				http.Error(w, "not found", 404)
				return
			}
			sha := base
			if applied && strings.HasSuffix(path, branch) {
				sha = head
			}
			json.NewEncoder(w).Encode(map[string]any{"ref": "refs/heads/" + branch, "object": map[string]string{"type": "commit", "sha": sha}})
		case strings.HasSuffix(path, "/git/refs"):
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			if in["ref"] != "refs/heads/"+branch || in["sha"] != base {
				t.Fatal(in)
			}
			created = true
			json.NewEncoder(w).Encode(map[string]string{})
		case strings.Contains(path, "/contents/"):
			if r.Method == "PUT" {
				var in map[string]string
				json.NewDecoder(r.Body).Decode(&in)
				if in["branch"] != branch {
					t.Fatal("writes target branch", in)
				}
				applied = true
				writes++
				json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"sha": head}, "content": map[string]string{"sha": blob}})
				return
			}
			d := "sha256:" + strings.Repeat("e", 64)
			if applied {
				d = digest
			}
			content := "image:\n  repository: ghcr.io/acme/image\n  digest: " + d + "\n"
			json.NewEncoder(w).Encode(map[string]string{"type": "file", "sha": blob, "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(content))})
		case strings.Contains(path, "/git/commits/"):
			json.NewEncoder(w).Encode(map[string]any{"message": "release-control: " + op + " [rcp:" + op + "]", "parents": []map[string]string{{"sha": base}}})
		case strings.HasSuffix(path, "/pulls"):
			if r.Method == "POST" {
				prCreated = true
				json.NewEncoder(w).Encode(pr())
				return
			}
			items := []map[string]any{}
			if prCreated {
				items = append(items, pr())
			}
			json.NewEncoder(w).Encode(items)
		case strings.HasSuffix(path, "/pulls/7"):
			json.NewEncoder(w).Encode(pr())
		default:
			t.Errorf("unexpected %s %s", r.Method, path)
			http.NotFound(w, r)
		}
	})
	req := delivery.GitOpsApply{Request: delivery.GitOpsRequest{Repository: "acme/source", Ref: "main", Path: "app.yaml", ImageRepository: "ghcr.io/acme/image"}, ExpectedHeadSHA: base, ExpectedBlobSHA: blob, Digest: digest, OperationID: op, Message: "release-control: " + op}
	result, err := p.ProposeGitOps(context.Background(), c, req)
	if err != nil || result.Number != 7 || result.Merged {
		t.Fatalf("%+v %v", result, err)
	}
	_, err = p.ProposeGitOps(context.Background(), c, req)
	if err != nil || writes != 1 {
		t.Fatalf("retry duplicated write: %d %v", writes, err)
	}
	result.HeadSHA = base
	if _, err = p.ObserveGitOpsPR(context.Background(), c, req, result); err == nil {
		t.Fatal("tampered head accepted")
	}
}
