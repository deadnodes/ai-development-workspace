package buildregistry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"releasecontrol/internal/buildconfig"
	"strings"
	"testing"
)

type digestTransport func(*http.Request) (*http.Response, error)

func (f digestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestDeploymentFreshDigestHEAD(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, status := range []int{200, 404, 500} {
		calls := 0
		r := NewWithClient(&http.Client{Transport: digestTransport(func(req *http.Request) (*http.Response, error) {
			calls++
			if req.Method != "HEAD" || req.URL.Path != "/v2/o/r/manifests/"+digest {
				t.Fatal(req)
			}
			h := http.Header{}
			h.Set("Docker-Content-Digest", digest)
			return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(""))}, nil
		})})
		err := r.HeadDigest(context.Background(), buildconfig.Registry{Origin: "https://registry.example"}, "o/r", digest)
		if calls != 1 || status == 200 && err != nil || status == 404 && !errors.Is(err, ErrMissing) || status == 500 && err == nil {
			t.Fatal(status, err, calls)
		}
	}
}
