package workerrunner

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"
)

func TestCandidateGitOpsHTTPPeerRetainsProposalHistory(t *testing.T) {
	f := newCandidateGitOpsHTTPFixture(t)
	f.configure("fixture/original")
	call := func(method, path string, payload any, out any) {
		t.Helper()
		var body []byte
		if payload != nil {
			body, _ = json.Marshal(payload)
		}
		r, _ := http.NewRequest(method, f.server.URL+"/repos/fixture/gitops/"+path, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer fixture-compose-only")
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			t.Fatalf("peer %s %s HTTP%d %s", method, path, response.StatusCode, raw)
		}
		if out != nil {
			if err := json.Unmarshal(raw, out); err != nil {
				t.Fatal(err)
			}
		}
	}
	makePR := func(branch, message string) map[string]any {
		t.Helper()
		f.mu.Lock()
		parent := f.refs["main"]
		tree := f.commits[parent]["tree"].(map[string]string)["sha"]
		f.mu.Unlock()
		var commit map[string]any
		call("POST", "git/commits", map[string]any{"message": message, "tree": tree, "parents": []string{parent}}, &commit)
		call("POST", "git/refs", map[string]any{"ref": "refs/heads/" + branch, "sha": commit["sha"]}, nil)
		var pr map[string]any
		call("POST", "pulls", map[string]any{"head": branch, "base": "main"}, &pr)
		return pr
	}
	first := makePR("rcp/gitops/first", "candidate")
	f.externalMerge(t)
	var historical map[string]any
	call("GET", "pulls/1", nil, &historical)
	second := makePR("rcp/gitops/second", "restore")
	if first["number"] == second["number"] || first["head"].(map[string]any)["sha"] == second["head"].(map[string]any)["sha"] {
		t.Fatal("distinct proposals share mutable identity")
	}
	var old map[string]any
	call("GET", "pulls/1", nil, &old)
	if !reflect.DeepEqual(old, historical) {
		t.Fatal("later proposal overwrote original PR history")
	}
	var filtered []map[string]any
	call("GET", "pulls?state=all&head=fixture:rcp/gitops/first&base=main", nil, &filtered)
	if len(filtered) != 1 || !reflect.DeepEqual(filtered[0], historical) {
		t.Fatalf("historical head lookup is ambiguous %+v", filtered)
	}
	ancestor := first["head"].(map[string]any)["sha"].(string)
	descendant := second["head"].(map[string]any)["sha"].(string)
	var compare map[string]any
	call("GET", "compare/"+ancestor+"..."+descendant, nil, &compare)
	if compare["status"] != "ahead" || compare["ahead_by"] != float64(1) || compare["behind_by"] != float64(0) {
		t.Fatalf("actual stored commit ancestry not retained %+v", compare)
	}
}
