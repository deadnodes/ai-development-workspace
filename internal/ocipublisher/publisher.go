// Package ocipublisher provides local operator-authorized publication only.
// Observations are not RCP admission, fencing, or deployment authorization.
package ocipublisher

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/ociverify"
)

const manifestType = "application/vnd.oci.image.manifest.v1+json"

var invalid = errors.New("invalid operator publication configuration or receipt")
var unknown = errors.New("registry observation UNKNOWN; retry the same immutable plan")
var ident = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
var repository = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
var digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var pinnedImage = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]{0,190}@sha256:[0-9a-f]{64}$`)
var source = regexp.MustCompile(`^[0-9a-f]{40}$`)
var envName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)

type Plan struct {
	ID           string                `json:"id"`
	Approved     bool                  `json:"approved"`
	ProductID    string                `json:"product_id"`
	ComponentID  string                `json:"component_id"`
	RepositoryID string                `json:"repository_id"`
	Origin       string                `json:"origin"`
	Repository   string                `json:"repository"`
	Receipt      domain.WorkerLocalOCI `json:"receipt"`
}
type Config struct {
	Plan                Plan   `json:"plan"`
	AllowedOrigin       string `json:"allowed_origin"`
	AllowedRepository   string `json:"allowed_repository"`
	Archive             string `json:"archive"`
	StateDir            string `json:"state_dir"`
	MaxArchiveBytes     int64  `json:"max_archive_bytes"`
	TimeoutSeconds      int    `json:"timeout_seconds"`
	CAFile              string `json:"ca_file,omitempty"`
	UsernameEnv         string `json:"username_env,omitempty"`
	PasswordEnv         string `json:"password_env,omitempty"`
	BearerEnv           string `json:"bearer_env,omitempty"`
	FixtureLoopbackHTTP bool   `json:"fixture_loopback_http,omitempty"`
}
type Observation struct {
	Kind           string    `json:"kind"`
	PlanHash       string    `json:"plan_hash"`
	Plan           Plan      `json:"plan"`
	ManifestDigest string    `json:"manifest_digest"`
	VerifiedAt     time.Time `json:"verified_at"`
}

func hash(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }
func decode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		return invalid
	}
	if d.Decode(new(any)) != io.EOF {
		return invalid
	}
	return nil
}
func readBounded(path string, max int64) ([]byte, error) {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, invalid
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Size() > max {
		return nil, invalid
	}
	b, e := io.ReadAll(io.LimitReader(f, max+1))
	if e != nil || int64(len(b)) > max {
		return nil, invalid
	}
	return b, nil
}
func Load(path string) (Config, error) {
	var c Config
	b, e := readBounded(path, 65536)
	if e != nil {
		return c, e
	}
	e = decode(b, &c)
	return c, e
}

func (c Config) validate() (*url.URL, error) {
	p := c.Plan
	r := p.Receipt
	if !p.Approved || !ident.MatchString(p.ID) || !ident.MatchString(p.ProductID) || !ident.MatchString(p.ComponentID) || !ident.MatchString(p.RepositoryID) || p.Origin != c.AllowedOrigin || p.Repository != c.AllowedRepository || len(p.Repository) > 255 || !repository.MatchString(p.Repository) {
		return nil, invalid
	}
	if !ident.MatchString(r.JobID) || !ident.MatchString(r.AttemptID) || !ident.MatchString(r.ConfigRevision) || !digest.MatchString(r.InputHash) || !source.MatchString(r.SourceSHA) || !digest.MatchString(r.RecipeHash) || !pinnedImage.MatchString(r.ToolchainImage) {
		return nil, invalid
	}
	for _, v := range []string{r.ArchiveHash, r.LayoutHash, r.IndexDigest, r.ManifestDigest} {
		if !digest.MatchString(v) {
			return nil, invalid
		}
	}
	if c.MaxArchiveBytes < 1 || c.MaxArchiveBytes > 256<<20 || c.TimeoutSeconds < 1 || c.TimeoutSeconds > 600 || r.ArchiveBytes < 1 || r.ArchiveBytes > c.MaxArchiveBytes || !filepath.IsAbs(c.StateDir) || !filepath.IsAbs(c.Archive) {
		return nil, invalid
	}
	if r.Platform != "linux/amd64" && r.Platform != "linux/arm64" {
		return nil, invalid
	}
	if s := r.RuntimeRootFS; s != nil && (!digest.MatchString(s.Hash) || !digest.MatchString(s.InputsHash) || s.ArchiveBytes < 1 || s.ArchiveBytes > 128<<20 || s.MaxExpandedBytes < 1 || s.MaxExpandedBytes > 128<<20 || s.MaxEntries < 1 || s.MaxEntries > 20000) {
		return nil, invalid
	}
	return validateRegistry(c.Plan.Origin, c.Plan.Repository, c.MaxArchiveBytes, c.TimeoutSeconds, c.UsernameEnv, c.PasswordEnv, c.BearerEnv, c.FixtureLoopbackHTTP)
}
func validateRegistry(origin, repo string, maxBytes int64, timeout int, username, password, bearer string, fixture bool) (*url.URL, error) {
	if len(repo) > 255 || !repository.MatchString(repo) || maxBytes < 1 || maxBytes > 256<<20 || timeout < 1 || timeout > 600 {
		return nil, invalid
	}
	u, e := url.Parse(origin)
	if e != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, invalid
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || !fixture || ip == nil || !ip.IsLoopback() {
			return nil, invalid
		}
	}
	if u.String() != origin {
		return nil, invalid
	}
	for _, s := range []string{username, password, bearer} {
		if s != "" && !envName.MatchString(s) {
			return nil, invalid
		}
	}
	if (username == "") != (password == "") || bearer != "" && username != "" {
		return nil, invalid
	}
	return u, nil
}

// Publish snapshots and validates all local bytes before opening a connection.
// Every invocation GETs and hashes the complete remote closure, including replays.
func Publish(ctx context.Context, c Config) (*Observation, error) {
	return publishWithSync(ctx, c, syncFile)
}

func publishWithSync(ctx context.Context, c Config, sync func(*os.File) error) (*Observation, error) {
	origin, e := c.validate()
	if e != nil {
		return nil, e
	}
	archive, e := readBounded(c.Archive, c.MaxArchiveBytes)
	if e != nil {
		return nil, e
	}
	actual, e := ociverify.VerifyOCIBytes(archive, c.MaxArchiveBytes, c.Plan.Receipt.Platform)
	if e != nil {
		return nil, invalid
	}
	expected := c.Plan.Receipt
	actual.JobID = expected.JobID
	actual.AttemptID = expected.AttemptID
	actual.InputHash = expected.InputHash
	actual.SourceSHA = expected.SourceSHA
	actual.RecipeHash = expected.RecipeHash
	actual.ConfigRevision = expected.ConfigRevision
	actual.ToolchainImage = expected.ToolchainImage
	// Runtime pins are authenticated build assertions, like recipe/toolchain;
	// exact archive/layout/manifest identities above bind all observed bytes.
	actual.RuntimeRootFS = expected.RuntimeRootFS
	if !reflect.DeepEqual(actual, expected) {
		return nil, invalid
	}
	blobs := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, invalid
		}
		if strings.HasPrefix(h.Name, "blobs/sha256/") && h.Typeflag != tar.TypeDir {
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, invalid
			}
			blobs["sha256:"+strings.TrimPrefix(h.Name, "blobs/sha256/")] = data
		}
	}
	manifest := blobs[actual.ManifestDigest]
	delete(blobs, actual.ManifestDigest)
	planBytes, _ := json.Marshal(c.Plan)
	identity := hash(planBytes)
	release, e := intentWithSync(c.StateDir, c.Plan.ID, identity, sync)
	if e != nil {
		return nil, e
	}
	defer release()
	client, e := newClient(c)
	if e != nil {
		return nil, e
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.TimeoutSeconds)*time.Second)
	defer cancel()
	remote := registryClient{client: client, origin: origin, repository: c.Plan.Repository, limit: c.MaxArchiveBytes}
	if c.BearerEnv != "" {
		secret := os.Getenv(c.BearerEnv)
		if secret == "" || strings.ContainsAny(secret, "\r\n") {
			return nil, invalid
		}
		remote.auth = "Bearer " + secret
	}
	if c.UsernameEnv != "" {
		u, p := os.Getenv(c.UsernameEnv), os.Getenv(c.PasswordEnv)
		if u == "" || p == "" || strings.ContainsAny(u+p, "\r\n") || strings.Contains(u, ":") {
			return nil, invalid
		}
		req, _ := http.NewRequest("GET", "https://unused.invalid", nil)
		req.SetBasicAuth(u, p)
		remote.auth = req.Header.Get("Authorization")
	}
	keys := make([]string, 0, len(blobs))
	for d := range blobs {
		keys = append(keys, d)
	}
	sort.Strings(keys)
	// Reconcile manifest first: an already committed manifest must have a complete
	// closure; corrupt/missing remote bytes fail closed and are never overwritten.
	exists, e := remote.observe(ctx, "manifests", actual.ManifestDigest, manifest)
	if e != nil {
		return nil, e
	}
	for _, d := range keys {
		present, err := remote.observe(ctx, "blobs", d, blobs[d])
		if err != nil {
			return nil, err
		}
		if !present {
			if exists {
				return nil, unknown
			}
			if err = remote.upload(ctx, d, blobs[d]); err != nil {
				return nil, err
			}
		}
	}
	if !exists {
		resp, err := remote.request(ctx, "PUT", remote.path("manifests", actual.ManifestDigest), manifest, manifestType)
		if err != nil {
			return nil, err
		}
		if resp.status != 201 {
			return nil, unknown
		}
	}
	// Fresh GET after writes; no HEAD-based or cached success assertions.
	for _, d := range keys {
		ok, err := remote.observe(ctx, "blobs", d, blobs[d])
		if err != nil || !ok {
			return nil, unknown
		}
	}
	ok, e := remote.observe(ctx, "manifests", actual.ManifestDigest, manifest)
	if e != nil || !ok {
		return nil, unknown
	}
	return &Observation{Kind: "PUBLISHER_LOCAL_REGISTRY_BYTES", PlanHash: identity, Plan: c.Plan, ManifestDigest: actual.ManifestDigest, VerifiedAt: time.Now().UTC()}, nil
}
func newClient(c Config) (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.CAFile != "" {
		pem, e := readBounded(c.CAFile, 1<<20)
		if e != nil {
			return nil, invalid
		}
		pool, e := x509.SystemCertPool()
		if e != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, invalid
		}
		tlsConfig.RootCAs = pool
	}
	return &http.Client{Transport: &http.Transport{Proxy: nil, TLSClientConfig: tlsConfig, DisableCompression: true, MaxResponseHeaderBytes: 16384, ResponseHeaderTimeout: 10 * time.Second, DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("registry redirect rejected") }}, nil
}

type registryClient struct {
	client           *http.Client
	origin           *url.URL
	repository, auth string
	limit            int64
}
type response struct {
	status int
	header http.Header
	body   []byte
}

func (r registryClient) path(kind, d string) string {
	return r.origin.String() + "/v2/" + r.repository + "/" + kind + "/" + d
}
func (r registryClient) request(ctx context.Context, method, target string, body []byte, media string) (response, error) {
	var out response
	req, e := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if e != nil {
		return out, unknown
	}
	req.Header.Set("Accept", manifestType)
	if media != "" {
		req.Header.Set("Content-Type", media)
	}
	if r.auth != "" {
		req.Header.Set("Authorization", r.auth)
	}
	resp, e := r.client.Do(req)
	if e != nil {
		return out, unknown
	}
	defer resp.Body.Close()
	limit := r.limit
	if method != "GET" || resp.StatusCode != 200 {
		limit = 8192
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if e != nil || int64(len(b)) > limit {
		return out, unknown
	}
	out = response{resp.StatusCode, resp.Header, b}
	return out, nil
}
func (r registryClient) observe(ctx context.Context, kind, d string, b []byte) (bool, error) {
	resp, e := r.request(ctx, "GET", r.path(kind, d), nil, "")
	if e != nil {
		return false, e
	}
	if resp.status == 404 {
		return false, nil
	}
	if resp.status != 200 || hash(resp.body) != d || !bytes.Equal(resp.body, b) {
		return false, unknown
	}
	return true, nil
}
func (r registryClient) upload(ctx context.Context, d string, b []byte) error {
	resp, e := r.request(ctx, "POST", r.path("blobs/uploads", ""), nil, "")
	if e != nil {
		return e
	}
	if resp.status != 202 {
		return unknown
	}
	location := resp.header.Get("Location")
	u, e := url.Parse(location)
	if e != nil || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return unknown
	}
	u = r.origin.ResolveReference(u)
	prefix := "/v2/" + r.repository + "/blobs/uploads/"
	id := strings.TrimPrefix(u.Path, prefix)
	if u.Scheme != r.origin.Scheme || u.Host != r.origin.Host || !strings.HasPrefix(u.Path, prefix) || !ident.MatchString(id) || u.RawPath != "" {
		return unknown
	}
	q, e := url.ParseQuery(u.RawQuery)
	if e != nil {
		return unknown
	}
	for k, v := range q {
		if k != "_state" || len(v) != 1 || len(v[0]) > 8192 {
			return unknown
		}
	}
	q.Set("digest", d)
	u.RawQuery = q.Encode()
	resp, e = r.request(ctx, "PUT", u.String(), b, "application/octet-stream")
	if e != nil {
		return e
	}
	if resp.status != 201 {
		return unknown
	}
	ok, e := r.observe(ctx, "blobs", d, b)
	if e != nil || !ok {
		return unknown
	}
	return nil
}

// intent retains a single durable plan hash under a process-independent lock.
// A private, existing directory is required; no directory ancestry is created.
func intent(dir, id, identity string) (func(), error) {
	return intentWithSync(dir, id, identity, syncFile)
}

func syncFile(file *os.File) error { return file.Sync() }

func intentWithSync(dir, id, identity string, sync func(*os.File) error) (func(), error) {
	resolved, e := filepath.EvalSymlinks(dir)
	if e != nil || resolved != filepath.Clean(dir) {
		return nil, invalid
	}
	st, e := os.Stat(dir)
	if e != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return nil, invalid
	}
	f, e := os.OpenFile(filepath.Join(dir, id+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, invalid
	}
	release := func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, errors.New("publication already running")
	}
	fail := func() (func(), error) { release(); return nil, invalid }
	syncDirectory := func() error {
		d, err := os.Open(dir)
		if err != nil {
			return err
		}
		defer d.Close()
		return sync(d)
	}
	st, e = f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return fail()
	}
	path := filepath.Join(dir, id+".intent")
	j, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if e == nil {
		b, err := io.ReadAll(io.LimitReader(j, 256))
		st, se := j.Stat()
		if err != nil || se != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || string(b) != identity+"\n" {
			j.Close()
			return fail()
		}
		// A prior process may have died after rename but before directory sync.
		// Visibility alone is insufficient to adopt intent before registry I/O.
		err = sync(j)
		ce := j.Close()
		if err != nil || ce != nil {
			return fail()
		}
		if syncDirectory() != nil {
			return fail()
		}
		return release, nil
	}
	if !os.IsNotExist(e) {
		return fail()
	}
	temp, e := os.CreateTemp(dir, ".intent-")
	if e != nil {
		return fail()
	}
	name := temp.Name()
	defer os.Remove(name)
	_, e = temp.WriteString(identity + "\n")
	if e == nil {
		e = sync(temp)
	}
	ce := temp.Close()
	if e != nil || ce != nil {
		return fail()
	}
	if os.Rename(name, path) != nil {
		return fail()
	}
	if syncDirectory() != nil {
		return fail()
	}
	return release, nil
}
