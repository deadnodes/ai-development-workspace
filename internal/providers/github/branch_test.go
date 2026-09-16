package github

import (
	"context"
	"encoding/json"
	"net/http"
	"releasecontrol/internal/delivery"
	"strings"
	"testing"
)

func TestManagedRefNeverOverwrites(t *testing.T) {
	refs := map[string]string{}
	writes := 0
	p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/repos/acme/repo/")
		switch {
		case strings.HasPrefix(path, "git/ref/heads/"):
			branch := strings.TrimPrefix(path, "git/ref/heads/")
			sha, ok := refs[branch]
			if !ok {
				w.WriteHeader(404)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"ref": "refs/heads/" + branch, "object": map[string]string{"type": "commit", "sha": sha}})
		case path == "git/refs" && r.Method == "POST":
			var in struct{ Ref, SHA string }
			json.NewDecoder(r.Body).Decode(&in)
			refs[strings.TrimPrefix(in.Ref, "refs/heads/")] = in.SHA
			writes++
			w.WriteHeader(201)
		default:
			t.Errorf("unexpected mutation %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
		}
	})
	sha := strings.Repeat("a", 40)
	for range 2 {
		if _, e := p.EnsureRef(context.Background(), c, "acme/repo", "rcp/revisions/rev", sha); e != nil {
			t.Fatal(e)
		}
	}
	if writes != 1 {
		t.Fatal("retry wrote again")
	}
	if _, e := p.EnsureRef(context.Background(), c, "acme/repo", "rcp/revisions/rev", strings.Repeat("b", 40)); e == nil {
		t.Fatal("overwrote immutable ref")
	}
	if _, e := p.EnsureRef(context.Background(), c, "acme/repo", "main", sha); e == nil {
		t.Fatal("accepted unmanaged ref")
	}
	if writes != 1 {
		t.Fatal("unexpected write")
	}
}

func TestResolutionRequiresEveryAncestorAndCreatesExactRef(t *testing.T) {
	base, head, fix := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "preserved", true: "missing_side"}[missing], func(t *testing.T) {
			refs := map[string]string{}
			writes := 0
			p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				path := strings.TrimPrefix(r.URL.Path, "/repos/acme/repo/")
				switch {
				case strings.HasPrefix(path, "compare/"):
					parts := strings.Split(strings.TrimPrefix(path, "compare/"), "...")
					behind := 0
					if missing && parts[0] == head {
						behind = 1
					}
					json.NewEncoder(w).Encode(map[string]any{"base_commit": map[string]string{"sha": parts[0]}, "status": "ahead", "behind_by": behind, "ahead_by": 1})
				case strings.HasPrefix(path, "git/ref/heads/"):
					branch := strings.TrimPrefix(path, "git/ref/heads/")
					sha, ok := refs[branch]
					if !ok {
						w.WriteHeader(404)
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"ref": "refs/heads/" + branch, "object": map[string]string{"type": "commit", "sha": sha}})
				case path == "git/refs" && r.Method == "POST":
					var in struct{ Ref, SHA string }
					json.NewDecoder(r.Body).Decode(&in)
					refs[strings.TrimPrefix(in.Ref, "refs/heads/")] = in.SHA
					writes++
					w.WriteHeader(201)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(400)
				}
			})
			result, e := p.ComposeSource(context.Background(), c, delivery.SourcePlan{Repository: "acme/repo", BaseRef: "main", BaseSHA: base, HeadSHAs: []string{head}, TargetBranch: "generated/resolved", OperationID: "op", ResolutionSHA: fix, ResolutionConflictID: "conflict"})
			if missing {
				if e == nil || writes != 0 {
					t.Fatal("missing ancestry wrote generated branch")
				}
			} else if e != nil || result.SHA != fix || writes != 1 {
				t.Fatalf("%+v %v writes=%d", result, e, writes)
			}
		})
	}
}
