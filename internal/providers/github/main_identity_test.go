package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestObservePullRequestPhysicalRepositoryIdentity(t *testing.T) {
	headSHA, mergeSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	mergedAt := time.Date(2026, 10, 21, 12, 34, 56, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		base, head any
		wantBase   int64
		wantHead   int64
	}{
		{"same_repository", map[string]any{"id": 123}, map[string]any{"id": 123}, 123, 123},
		{"fork", map[string]any{"id": 123}, map[string]any{"id": 456}, 123, 456},
		{"foreign_base_is_not_normalized", map[string]any{"id": 789}, map[string]any{"id": 456}, 789, 456},
		{"deleted_fork", map[string]any{"id": 123}, nil, 123, 0},
		{"legacy_missing_identity", nil, nil, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodGet {
					t.Errorf("unexpected mutation: %s", r.Method)
				}
				switch r.URL.Path {
				case "/repositories/123":
					jsonOut(w, map[string]any{"id": 123, "full_name": "acme/source"})
				case "/repos/acme/source/pulls/7":
					jsonOut(w, map[string]any{"number": 7, "state": "closed", "merged": true, "merge_commit_sha": mergeSHA, "merged_at": mergedAt,
						"head": map[string]any{"ref": "deleted-feature", "sha": headSHA, "repo": tc.head},
						"base": map[string]any{"ref": "main", "repo": tc.base}})
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			got, err := p.ObservePullRequest(context.Background(), c, "123", 7)
			if err != nil {
				t.Fatal(err)
			}
			if got.BaseRepositoryID != tc.wantBase || got.HeadRepositoryID != tc.wantHead || got.HeadSHA != headSHA || got.MergeSHA != mergeSHA || !got.MergedAt.Equal(mergedAt) || got.State != "MERGED" || got.Head != "deleted-feature" || got.Base != "main" || requests != 2 {
				t.Fatalf("unexpected observation: %+v, requests=%d", got, requests)
			}
		})
	}
}

func TestObservePullRequestRejectsMalformedRepositoryIdentity(t *testing.T) {
	for _, side := range []string{"base", "head"} {
		for _, invalid := range []any{"123", 123.5, json.Number("9223372036854775808"), true, []int{123}, map[string]any{"id": 123}} {
			t.Run(side+"/"+fmt.Sprintf("%T", invalid), func(t *testing.T) {
				p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/source/pulls/7" {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
						return
					}
					pr := map[string]any{"number": 7, "state": "open", "head": map[string]any{"ref": "feature", "sha": strings.Repeat("a", 40)}, "base": map[string]any{"ref": "main"}}
					pr[side].(map[string]any)["repo"] = map[string]any{"id": invalid}
					jsonOut(w, pr)
				})
				if _, err := p.ObservePullRequest(context.Background(), c, "acme/source", 7); err == nil {
					t.Fatal("malformed repository ID accepted")
				}
			})
		}
	}
}

func TestObservePullRequestRejectsChangedPhysicalRepository(t *testing.T) {
	p, c, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repositories/123" {
			t.Errorf("unexpected request after identity mismatch: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		jsonOut(w, map[string]any{"id": 456, "full_name": "acme/source"})
	})
	if _, err := p.ObservePullRequest(context.Background(), c, "123", 7); err == nil {
		t.Fatal("changed physical repository accepted")
	}
}
