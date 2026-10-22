package ocipublisher

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ReadClient exposes only GET against one operator-owned registry repository.
// It intentionally has no arbitrary URL, redirect, challenge or write method.
type ReadClient struct {
	client                   *http.Client
	origin, repository, auth string
	max                      int64
}
type ReadResponse struct {
	Status                             int
	ContentType, Link, Digest, Filters string
	Body                               io.ReadCloser
}

var observationRef = regexp.MustCompile(`^(?:sha256:[0-9a-f]{64}|[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})$`)

func NewReadClient(c ReadConfig) (*ReadClient, error) {
	if c.MaxBytes < 1024 || c.MaxBytes > 1<<30 || c.TimeoutSeconds < 1 || c.TimeoutSeconds > 900 {
		return nil, invalid
	}
	u, e := validateRegistry(c.Origin, c.Repository, 1024, 1, c.UsernameEnv, c.PasswordEnv, c.BearerEnv, c.FixtureLoopbackHTTP)
	if e != nil {
		return nil, invalid
	}
	h, e := newClient(Config{CAFile: c.CAFile})
	if e != nil {
		return nil, invalid
	}
	h.Timeout = time.Duration(c.TimeoutSeconds) * time.Second
	r := &ReadClient{client: h, origin: u.String(), repository: c.Repository, max: c.MaxBytes}
	if c.BearerEnv != "" {
		v := os.Getenv(c.BearerEnv)
		if v == "" || strings.ContainsAny(v, "\r\n") {
			h.CloseIdleConnections()
			return nil, invalid
		}
		r.auth = "Bearer " + v
	}
	if c.UsernameEnv != "" {
		u, p := os.Getenv(c.UsernameEnv), os.Getenv(c.PasswordEnv)
		if u == "" || p == "" || strings.ContainsAny(u+p, "\r\n") || strings.Contains(u, ":") {
			h.CloseIdleConnections()
			return nil, invalid
		}
		q, _ := http.NewRequest("GET", "https://unused.invalid", nil)
		q.SetBasicAuth(u, p)
		r.auth = q.Header.Get("Authorization")
	}
	return r, nil
}
func (r *ReadClient) Close() { r.client.CloseIdleConnections() }
func (r *ReadClient) Get(ctx context.Context, kind, ref string, q url.Values) (ReadResponse, error) {
	var out ReadResponse
	if kind != "tags" && kind != "manifests" && kind != "blobs" && kind != "referrers" {
		return out, invalid
	}
	if kind == "tags" {
		if ref != "list" {
			return out, invalid
		}
	} else if !observationRef.MatchString(ref) {
		return out, invalid
	}
	if (kind == "blobs" || kind == "referrers") && !digest.MatchString(ref) {
		return out, invalid
	}
	for k, v := range q {
		if len(v) != 1 || len(v[0]) > 128 {
			return out, invalid
		}
		switch k {
		case "n":
			n, e := strconv.Atoi(v[0])
			if e != nil || n < 1 || n > 1000 {
				return out, invalid
			}
		case "last":
			if !observationRef.MatchString(v[0]) {
				return out, invalid
			}
		default:
			return out, invalid
		}
	}
	target := r.origin + "/v2/" + r.repository + "/" + kind + "/" + url.PathEscape(ref)
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if e != nil {
		return out, unknown
	}
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json")
	if r.auth != "" {
		req.Header.Set("Authorization", r.auth)
	}
	resp, e := r.client.Do(req)
	if e != nil {
		return out, unknown
	}
	if len(resp.Header.Values("Docker-Content-Digest")) > 1 || len(resp.Header.Values("Content-Type")) > 1 {
		resp.Body.Close()
		return out, unknown
	}
	limit := r.max
	if resp.StatusCode != 200 {
		limit = 8192
	}
	out = ReadResponse{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Link: strings.Join(resp.Header.Values("Link"), ","), Digest: resp.Header.Get("Docker-Content-Digest"), Filters: resp.Header.Get("OCI-Filters-Applied"), Body: &boundedRegistryBody{body: resp.Body, left: limit}}
	return out, nil
}

type boundedRegistryBody struct {
	body io.ReadCloser
	left int64
}

func (b *boundedRegistryBody) Read(p []byte) (int, error) {
	if b.left < 0 {
		return 0, errors.New("registry_response_limit")
	}
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1]
	}
	n, e := b.body.Read(p)
	b.left -= int64(n)
	if b.left < 0 {
		return n, errors.New("registry_response_limit")
	}
	return n, e
}
func (b *boundedRegistryBody) Close() error { return b.body.Close() }
