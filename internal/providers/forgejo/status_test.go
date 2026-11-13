package forgejo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"releasecontrol/internal/buildconfig"
	"strconv"
	"strings"
	"testing"
)

type statusTransport func(*http.Request) (*http.Response, error)

func (f statusTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestDeploymentCombinedStatusHTTP(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RCP_SECRET_DIR", dir)
	os.WriteFile(filepath.Join(dir, "token"), []byte("private-token"), 0600)
	sha := strings.Repeat("a", 40)
	for _, body := range []string{`{"state":"success","statuses":[{"context":"verify","status":"success"}]}`, `{"state":"pending","statuses":[]}`, `{"state":"failure","statuses":[{"context":"verify","status":"failure"}]}`} {
		c := NewWithClient(&http.Client{Transport: statusTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" || r.URL.Path != "/api/v1/repos/o/r/commits/"+sha+"/status" || r.Header.Get("Authorization") != "token private-token" {
				t.Fatal(r)
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})})
		status, e := c.CommitStatus(context.Background(), buildconfig.Connection{APIURL: "https://forgejo.example", TokenRef: "token"}, "o/r", sha)
		if e != nil || status.State == "" {
			t.Fatal(status, e)
		}
	}
}

func TestDeploymentStatusPagination(t *testing.T) {
	for _, total := range []int{51, 1001} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("RCP_SECRET_DIR", dir)
			if err := os.WriteFile(filepath.Join(dir, "token"), []byte("private-token"), 0600); err != nil {
				t.Fatal(err)
			}
			sha := strings.Repeat("a", 40)
			calls := 0
			c := NewWithClient(&http.Client{Transport: statusTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Query().Get("page") != strconv.Itoa(calls) || r.URL.Query().Get("limit") != "50" {
					t.Fatal(r.URL)
				}
				v := CommitStatus{SHA: sha, State: "success", TotalCount: total}
				for i := (calls - 1) * 50; i < calls*50 && i < total; i++ {
					name := fmt.Sprintf("check-%d", i)
					if i == 50 {
						name = "required-on-page-2"
					}
					v.Statuses = append(v.Statuses, StatusContext{Context: name, Status: "success"})
				}
				body, err := json.Marshal(v)
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			})})
			v, err := c.CommitStatus(context.Background(), buildconfig.Connection{APIURL: "https://forgejo.example", TokenRef: "token"}, "o/r", sha)
			if total > 1000 {
				if !errors.Is(err, ErrStatusPageLimit) || calls != 20 {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
				return
			}
			if err != nil || calls != 2 || v.State != "success" || len(v.Statuses) != total || v.TotalCount != total {
				t.Fatalf("status=%+v calls=%d err=%v", v, calls, err)
			}
			if v.Statuses[50].Context != "required-on-page-2" || v.Statuses[50].Status != "success" {
				t.Fatal(v.Statuses[50])
			}
		})
	}
}
