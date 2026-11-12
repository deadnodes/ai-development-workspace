// Package buildregistry reads anonymous OCI provenance without publishing bytes.
package buildregistry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"releasecontrol/internal/buildconfig"
	"strings"
	"time"
)

var ErrMissing = errors.New("registry artifact missing")
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type Artifact struct{ Digest, Revision string }
type Reader struct{ http *http.Client }

func New() *Reader { return NewWithClient(http.DefaultClient) }
func NewWithClient(h *http.Client) *Reader {
	v := *h
	v.Timeout = 30 * time.Second
	v.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Reader{&v}
}
func hash(b []byte) string { v := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(v[:]) }
func (r *Reader) get(ctx context.Context, c buildconfig.Registry, path, expected string) ([]byte, string, error) {
	u, e := url.Parse(c.Origin)
	if e != nil || u.User != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && c.InsecureHTTP)) {
		return nil, "", errors.New("registry origin denied")
	}
	req, e := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(c.Origin, "/")+path, nil)
	if e != nil {
		return nil, "", errors.New("invalid registry request")
	}
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json")
	res, e := r.http.Do(req)
	if e != nil {
		return nil, "", errors.New("registry read failed")
	}
	defer res.Body.Close()
	if res.StatusCode == 404 {
		return nil, "", ErrMissing
	}
	if res.StatusCode != 200 {
		return nil, "", errors.New("registry read rejected")
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, (4<<20)+1))
	if e != nil || len(b) > 4<<20 {
		return nil, "", errors.New("registry response exceeds bounds")
	}
	d := hash(b)
	header := res.Header.Get("Docker-Content-Digest")
	if expected != "" && expected != d || header != "" && header != d {
		return nil, "", errors.New("registry digest mismatch")
	}
	return b, d, nil
}

type descriptor struct {
	Digest   string `json:"digest"`
	Size     int    `json:"size"`
	Platform struct {
		OS   string `json:"os"`
		Arch string `json:"architecture"`
	} `json:"platform"`
}
type manifest struct {
	Schema    int          `json:"schemaVersion"`
	MediaType string       `json:"mediaType"`
	Manifests []descriptor `json:"manifests"`
	Config    descriptor   `json:"config"`
}

func (r *Reader) Resolve(ctx context.Context, c buildconfig.Registry, repo, tag string) (Artifact, error) {
	var out Artifact
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9_./-]*$`).MatchString(repo) || strings.Contains(repo, "..") || !regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`).MatchString(tag) {
		return out, errors.New("invalid registry reference")
	}
	base := "/v2/" + repo
	b, d, e := r.get(ctx, c, base+"/manifests/"+tag, "")
	if e != nil {
		return out, e
	}
	out.Digest = d
	var m manifest
	if json.Unmarshal(b, &m) != nil || m.Schema != 2 {
		return out, errors.New("invalid OCI manifest")
	}
	if len(m.Manifests) > 0 {
		var selected *descriptor
		for i := range m.Manifests {
			v := &m.Manifests[i]
			if v.Platform.OS == "linux" && v.Platform.Arch == "amd64" {
				if selected != nil {
					return out, errors.New("ambiguous OCI platform")
				}
				selected = v
			}
		}
		if selected == nil || !digestPattern.MatchString(selected.Digest) {
			return out, errors.New("OCI linux/amd64 manifest missing")
		}
		b, _, e = r.get(ctx, c, base+"/manifests/"+selected.Digest, selected.Digest)
		if e != nil {
			return out, e
		}
		if selected.Size != len(b) || json.Unmarshal(b, &m) != nil || m.Schema != 2 {
			return out, errors.New("invalid OCI image manifest")
		}
	}
	if !digestPattern.MatchString(m.Config.Digest) {
		return out, errors.New("invalid OCI config digest")
	}
	b, _, e = r.get(ctx, c, base+"/blobs/"+m.Config.Digest, m.Config.Digest)
	if e != nil {
		return out, e
	}
	var cfg struct {
		OS     string `json:"os"`
		Arch   string `json:"architecture"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if m.Config.Size != len(b) || json.Unmarshal(b, &cfg) != nil || cfg.OS != "linux" || cfg.Arch != "amd64" {
		return out, errors.New("invalid OCI config platform")
	}
	out.Revision = cfg.Config.Labels["org.opencontainers.image.revision"]
	return out, nil
}
