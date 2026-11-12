// Package buildconfig defines operator-owned Forgejo build authority.
package buildconfig

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Connection struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	APIURL   string `json:"api_url"`
	Owner    string `json:"owner"`
	TokenRef string `json:"token_ref"`
}
type Registry struct {
	ID           string `json:"id"`
	Origin       string `json:"origin"`
	InsecureHTTP bool   `json:"insecure_http"`
}
type Pool struct {
	ID          string `json:"id"`
	MaxInFlight int    `json:"max_in_flight"`
}
type Profile struct {
	ID              string   `json:"id"`
	ProductID       string   `json:"product_id"`
	ConnectionID    string   `json:"connection_id"`
	Repository      string   `json:"repository"`
	Workflow        string   `json:"workflow"`
	DefaultBranch   string   `json:"default_branch"`
	PoolID          string   `json:"pool_id"`
	RegistryID      string   `json:"registry_id"`
	ImageRepository string   `json:"image_repository"`
	DispatchRef     string   `json:"dispatch_ref"`
	TagCandidates   []string `json:"tag_candidates"`
}
type Config struct {
	Connections    []Connection `json:"connections"`
	Registries     []Registry   `json:"registries"`
	Pools          []Pool       `json:"pools"`
	Profiles       []Profile    `json:"profiles"`
	TimeoutSeconds int          `json:"timeout_seconds,omitempty"`
}

var ident = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
var repo = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*/[a-z0-9][a-z0-9_./-]*$`)
var RequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
var SHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func Branch(s string) bool {
	return s != "" && len(s) <= 255 && !strings.ContainsAny(s, " ~^:?*[\\\x00\r\n") && !strings.Contains(s, "..") && !strings.HasPrefix(s, "-") && !strings.HasPrefix(s, "/") && !strings.HasSuffix(s, "/") && !strings.Contains(s, "@{")
}
func endpoint(s string, httpAllowed bool) bool {
	u, e := url.Parse(s)
	return e == nil && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/") && (u.Scheme == "https" || httpAllowed && u.Scheme == "http")
}
func Load(path string) (*Config, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, errors.New("build configuration unavailable")
	}
	defer f.Close()
	return Decode(io.LimitReader(f, 1<<20+1))
}
func Decode(r io.Reader) (*Config, error) {
	b, e := io.ReadAll(io.LimitReader(r, (1<<20)+1))
	if e != nil || len(b) > 1<<20 {
		return nil, errors.New("build configuration exceeds bounds")
	}
	if !strings.HasPrefix(strings.TrimSpace(string(b)), "{") {
		return nil, errors.New("build configuration must be an object")
	}
	var c Config
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid build configuration")
	}
	if e = c.Validate(); e != nil {
		return nil, e
	}
	return &c, nil
}
func (c *Config) Validate() error {
	bad := errors.New("invalid build configuration authority")
	unique := func(ids []string) bool {
		m := map[string]bool{}
		for _, v := range ids {
			if !ident.MatchString(v) || m[v] {
				return false
			}
			m[v] = true
		}
		return true
	}
	ids := []string{}
	con := map[string]Connection{}
	for _, v := range c.Connections {
		ids = append(ids, v.ID)
		if v.Kind != "forgejo" || !endpoint(v.APIURL, false) || !ident.MatchString(v.Owner) || !ident.MatchString(v.TokenRef) || filepath.Base(v.TokenRef) != v.TokenRef {
			return bad
		}
		con[v.ID] = v
	}
	if !unique(ids) {
		return bad
	}
	ids = nil
	regs := map[string]bool{}
	for _, v := range c.Registries {
		ids = append(ids, v.ID)
		if !endpoint(v.Origin, v.InsecureHTTP) {
			return bad
		}
		regs[v.ID] = true
	}
	if !unique(ids) {
		return bad
	}
	ids = nil
	pools := map[string]bool{}
	for i, v := range c.Pools {
		ids = append(ids, v.ID)
		if v.MaxInFlight == 0 {
			c.Pools[i].MaxInFlight = 1
		}
		if v.MaxInFlight < 0 || v.MaxInFlight > 100 {
			return bad
		}
		pools[v.ID] = true
	}
	if !unique(ids) {
		return bad
	}
	ids = nil
	for _, v := range c.Profiles {
		ids = append(ids, v.ID)
		cn, ok := con[v.ConnectionID]
		if !ok || !ident.MatchString(v.ProductID) || !repo.MatchString(v.Repository) || strings.Split(v.Repository, "/")[0] != cn.Owner || strings.Count(v.Repository, "/") != 1 || v.Workflow != "build.yml" || !Branch(v.DispatchRef) || !Branch(v.DefaultBranch) || !pools[v.PoolID] || !regs[v.RegistryID] || !repo.MatchString(v.ImageRepository) || strings.Contains(v.ImageRepository, "..") || len(v.TagCandidates) == 0 || len(v.TagCandidates) > 16 {
			return bad
		}
		for _, template := range v.TagCandidates {
			tag := ExpandTag(template, strings.Repeat("a", 40))
			if !regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`).MatchString(tag) || (!strings.Contains(template, "{sha8}") && !strings.Contains(template, "{sha}")) {
				return bad
			}
		}
	}
	if !unique(ids) {
		return bad
	}
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = 7200
	}
	if c.TimeoutSeconds < 1 || c.TimeoutSeconds > 604800 {
		return bad
	}
	return nil
}
func ExpandTag(template, sha string) string {
	return strings.ReplaceAll(strings.ReplaceAll(template, "{sha8}", sha[:8]), "{sha}", sha)
}
func (c *Config) Profile(id string) (Profile, bool) {
	if c != nil {
		for _, v := range c.Profiles {
			if v.ID == id {
				return v, true
			}
		}
	}
	return Profile{}, false
}
func (c *Config) Connection(id string) Connection {
	for _, v := range c.Connections {
		if v.ID == id {
			return v
		}
	}
	return Connection{}
}
func (c *Config) Registry(id string) Registry {
	for _, v := range c.Registries {
		if v.ID == id {
			return v
		}
	}
	return Registry{}
}
func (c *Config) Capacity(id string) int {
	for _, v := range c.Pools {
		if v.ID == id {
			return v.MaxInFlight
		}
	}
	return 0
}
