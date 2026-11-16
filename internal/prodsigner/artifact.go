package prodsigner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"gopkg.in/yaml.v3"
	"io"
	"net/http"
	"os"
	"releasecontrol/internal/imagesigning"
	"strings"
	"time"
)

const maxArtifact = 64 << 20

var errInput = errors.New("artifact or Kubernetes request failed")

type Artifact struct {
	URL      string `json:"url"`
	Revision string `json:"revision"`
	Digest   string `json:"digest"`
}
type Source struct {
	HTTP       *http.Client
	API, Token string
}

func InCluster() (Source, error) {
	ca, e := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if e != nil {
		return Source{}, errInput
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return Source{}, errInput
	}
	token, e := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if e != nil {
		return Source{}, errInput
	}
	return Source{HTTP: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, API: "https://" + os.Getenv("KUBERNETES_SERVICE_HOST") + ":" + os.Getenv("KUBERNETES_SERVICE_PORT"), Token: strings.TrimSpace(string(token))}, nil
}
func (s Source) get(ctx context.Context, u string, auth bool, limit int64) ([]byte, error) {
	req, e := http.NewRequestWithContext(ctx, "GET", u, nil)
	if e != nil {
		return nil, errInput
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	client := *s.HTTP
	client.Timeout = 30 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, e := client.Do(req)
	if e != nil {
		return nil, errInput
	}
	defer res.Body.Close()
	b, e := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if e != nil || int64(len(b)) > limit || res.StatusCode != 200 {
		return nil, errInput
	}
	return b, nil
}
func (s Source) Load(ctx context.Context, c Config) (Artifact, map[string][]byte, error) {
	var a Artifact
	b, e := s.get(ctx, s.API+"/apis/source.toolkit.fluxcd.io/v1/namespaces/"+c.FluxArtifact.Namespace+"/gitrepositories/"+c.FluxArtifact.Name, true, 1<<20)
	if e != nil {
		return a, nil, e
	}
	var v struct {
		Status struct {
			Artifact Artifact `json:"artifact"`
		} `json:"status"`
	}
	if json.Unmarshal(b, &v) != nil {
		return a, nil, errInput
	}
	a = v.Status.Artifact
	if !imagesigning.DigestPattern.MatchString(a.Digest) || a.Revision == "" {
		return a, nil, errInput
	}
	b, e = s.get(ctx, a.URL, false, maxArtifact)
	if e != nil {
		return a, nil, e
	}
	if imagesigning.Hash(b) != a.Digest {
		return a, nil, errInput
	}
	files, e := extract(b, c)
	return a, files, e
}
func extract(b []byte, c Config) (map[string][]byte, error) {
	gz, e := gzip.NewReader(bytes.NewReader(b))
	if e != nil {
		return nil, errInput
	}
	defer gz.Close()
	tr := tar.NewReader(io.LimitReader(gz, maxArtifact+1))
	files := map[string][]byte{}
	var size int64
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, errInput
		}
		name := strings.TrimPrefix(h.Name, "./")
		if h.Typeflag == tar.TypeDir && (name == "" || name == ".") {
			continue
		}
		if !safePath(name) || h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			return nil, errInput
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		size += h.Size
		if size > maxArtifact || h.Size < 0 {
			return nil, errInput
		}
		keep := name == c.BackfillFile
		for _, p := range c.Paths {
			keep = keep || strings.HasPrefix(name, p)
		}
		if !keep {
			continue
		}
		if _, ok := files[name]; ok {
			return nil, errInput
		}
		data, e := io.ReadAll(tr)
		if e != nil {
			return nil, errInput
		}
		files[name] = data
	}
	return files, nil
}

type Ref struct {
	Image, Repo, Digest string
	Paths               []string
	Reason, ApprovedIn  string
}

func parseRef(s string) (Ref, bool) {
	name, digest, ok := strings.Cut(s, "@")
	if !ok || !imagesigning.DigestPattern.MatchString(digest) {
		return Ref{}, false
	}
	repo := name
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	return Ref{Image: s, Repo: repo, Digest: digest}, repo != ""
}
func collect(c Config, files map[string][]byte) (map[string]*Ref, error) {
	refs := map[string]*Ref{}
	add := func(s, p, reason, approval string) {
		r, ok := parseRef(s)
		if !ok {
			return
		}
		own := false
		for _, prefix := range c.OwnImagePrefixes {
			own = own || strings.HasPrefix(r.Repo, prefix)
		}
		if !own {
			return
		}
		// Repository plus digest prevents an approval for one repository authorizing another.
		key := r.Repo + "@" + r.Digest
		if refs[key] == nil {
			refs[key] = &r
		}
		v := refs[key]
		v.Paths = append(v.Paths, p)
		if reason != "" && approval != "" {
			v.Reason = reason
			v.ApprovedIn = approval
		}
	}
	for p, b := range files {
		if p == c.BackfillFile {
			var v struct {
				Digests []struct {
					Ref        string `yaml:"ref"`
					Reason     string `yaml:"reason"`
					ApprovedIn string `yaml:"approved_in"`
				} `yaml:"digests"`
			}
			d := yaml.NewDecoder(bytes.NewReader(b))
			d.KnownFields(true)
			if d.Decode(&v) != nil {
				return nil, errInput
			}
			for _, r := range v.Digests {
				if _, ok := parseRef(r.Ref); !ok || r.Reason == "" || r.ApprovedIn == "" {
					return nil, errInput
				}
				add(r.Ref, p, r.Reason, r.ApprovedIn)
			}
			continue
		}
		if !strings.HasSuffix(p, ".yaml") && !strings.HasSuffix(p, ".yml") {
			continue
		}
		d := yaml.NewDecoder(bytes.NewReader(b))
		for {
			var n yaml.Node
			e := d.Decode(&n)
			if e == io.EOF {
				break
			}
			if e != nil {
				return nil, errInput
			}
			var walk func(*yaml.Node)
			walk = func(n *yaml.Node) {
				if n.Kind == yaml.MappingNode {
					repo, tag := "", ""
					for i := 0; i+1 < len(n.Content); i += 2 {
						k, v := n.Content[i], n.Content[i+1]
						if v.Kind != yaml.ScalarNode {
							continue
						}
						switch k.Value {
						case "image":
							add(v.Value, p, "", "")
						case "repository":
							repo = v.Value
						case "tag":
							tag = v.Value
						}
					}
					if repo != "" && tag != "" {
						add(repo+":"+tag, p, "", "")
					}
				}
				for _, v := range n.Content {
					walk(v)
				}
			}
			walk(&n)
		}
	}
	return refs, nil
}
