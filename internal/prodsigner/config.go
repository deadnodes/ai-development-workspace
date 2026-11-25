// Package prodsigner reconciles production Flux intent against independently signed build evidence.
package prodsigner

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"path"
	"path/filepath"
	"releasecontrol/internal/buildconfig"
	"strings"
)

type Cache struct {
	ConfigMap  string `json:"configmap"`
	TTLSeconds int    `json:"ttl_seconds"`
}

type Config struct {
	RCP struct {
		URL           string `json:"url"`
		TokenFile     string `json:"token_file"`
		ProductID     string `json:"product_id"`
		EnvironmentID string `json:"environment_id"`
	} `json:"rcp,omitempty"`
	Cache               Cache `json:"cache,omitempty"`
	RequireSecurityScan bool  `json:"require_security_scan,omitempty"`
	FluxArtifact        struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		Kind      string `json:"kind"`
	} `json:"flux_artifact"`
	Paths            []string `json:"paths"`
	OwnImagePrefixes []string `json:"own_image_prefixes"`
	Registry         struct {
		Origin          string            `json:"origin"`
		InsecureHTTP    bool              `json:"insecure_http"`
		CredentialsFile string            `json:"credentials_file"`
		Rewrite         map[string]string `json:"rewrite"`
	} `json:"registry"`
	SignaturesRepository string                 `json:"signatures_repository"`
	DevPublicKeyPEM      string                 `json:"dev_public_key_pem"`
	ProdKey              buildconfig.SigningKey `json:"prod_key"`
	BuilderIDs           []string               `json:"builder_ids"`
	BackfillFile         string                 `json:"backfill_file"`
	MetricsFile          string                 `json:"metrics_file"`
}

func Decode(r io.Reader) (Config, error) {
	return DecodeMode(r, "flux")
}
func DecodeMode(r io.Reader, mode string) (Config, error) {
	var c Config
	d := json.NewDecoder(io.LimitReader(r, 1<<20))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF {
		return c, errors.New("invalid config")
	}
	return c, c.ValidateMode(mode)
}
func safePath(p string) bool {
	return p != "" && !strings.HasPrefix(p, "/") && !strings.Contains(p, "\\") && path.Clean(p) == strings.TrimSuffix(p, "/") && p != "." && !strings.HasPrefix(p, "../")
}
func origin(s string, httpOK bool) bool {
	u, e := url.Parse(s)
	return e == nil && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/") && (u.Scheme == "https" || httpOK && u.Scheme == "http")
}
func (c Config) Validate() error {
	return c.ValidateMode("flux")
}
func (c Config) ValidateMode(mode string) error {
	bad := errors.New("invalid config")
	if mode != "flux" && mode != "promote" {
		return bad
	}
	if len(c.OwnImagePrefixes) == 0 || len(c.BuilderIDs) == 0 {
		return bad
	}
	if c.Cache.ConfigMap == "" {
		if c.Cache.TTLSeconds != 0 {
			return bad
		}
	} else if len(c.Cache.ConfigMap) > 253 || !dnsSubdomain(c.Cache.ConfigMap) || c.Cache.TTLSeconds < 60 || c.Cache.TTLSeconds > 86400 {
		return bad
	}
	if mode == "flux" && (c.FluxArtifact.Kind != "GitRepository" || c.FluxArtifact.Namespace == "" || c.FluxArtifact.Name == "" || strings.ContainsAny(c.FluxArtifact.Namespace+c.FluxArtifact.Name, "/ ?#") || len(c.Paths) == 0) {
		return bad
	}
	if mode == "promote" {
		u, e := url.Parse(c.RCP.URL)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !filepath.IsAbs(c.RCP.TokenFile) || c.RCP.ProductID == "" || c.RCP.EnvironmentID == "" {
			return bad
		}
	}
	for _, p := range c.Paths {
		if !safePath(p) || !strings.HasSuffix(p, "/") {
			return bad
		}
	}
	if mode == "flux" && c.BackfillFile != "" && !safePath(c.BackfillFile) {
		return bad
	}
	for _, p := range c.OwnImagePrefixes {
		if !strings.HasSuffix(p, "/") {
			return bad
		}
	}
	if !origin(c.Registry.Origin, c.Registry.InsecureHTTP) || !safePath(c.SignaturesRepository) || strings.Contains(c.SignaturesRepository, ":") || !filepath.IsAbs(c.Registry.CredentialsFile) {
		return bad
	}
	if _, e := buildconfig.PublicKey(c.DevPublicKeyPEM); e != nil {
		return bad
	}
	if _, e := buildconfig.PublicKey(c.ProdKey.PublicKeyPEM); e != nil {
		return bad
	}
	k := c.ProdKey
	if !origin(k.VaultAddr, true) || k.Auth.Method != "kubernetes" || !filepath.IsAbs(k.Auth.TokenFile) {
		return bad
	}
	for _, s := range []string{k.TransitMount, k.KeyName, k.Auth.Mount, k.Auth.Role} {
		if s == "" || strings.ContainsAny(s, "/ ?#") {
			return bad
		}
	}
	return nil
}

func dnsSubdomain(s string) bool {
	if s == "" {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if part == "" || len(part) > 63 || !(part[0] >= 'a' && part[0] <= 'z' || part[0] >= '0' && part[0] <= '9') {
			return false
		}
		last := part[len(part)-1]
		if !(last >= 'a' && last <= 'z' || last >= '0' && last <= '9') {
			return false
		}
		for _, c := range part {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
