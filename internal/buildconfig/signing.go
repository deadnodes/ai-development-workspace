package buildconfig

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net/url"
	"path"
	"path/filepath"
	"strings"
)

type Signing struct {
	BuilderID                 string              `json:"builder_id"`
	RequireSignatureForDeploy bool                `json:"require_signature_for_deploy"`
	Keys                      []SigningKey        `json:"keys"`
	Rules                     []SigningRule       `json:"rules"`
	DeployKeys                map[string][]string `json:"deploy_keys"`
	RegistryCredentials       map[string]string   `json:"registry_credentials"`
	BackfillRepositories      []string            `json:"backfill_repositories,omitempty"`
}
type SigningKey struct {
	ID            string      `json:"id"`
	VaultAddr     string      `json:"vault_addr"`
	InsecureHTTP  bool        `json:"insecure_http,omitempty"`
	TransitMount  string      `json:"transit_mount"`
	KeyName       string      `json:"key_name"`
	PublicKeyPEM  string      `json:"public_key_pem"`
	Auth          SigningAuth `json:"auth"`
	AllowBackfill bool        `json:"allow_backfill"`
}
type SigningAuth struct {
	Method    string `json:"method"`
	Mount     string `json:"mount"`
	Role      string `json:"role"`
	TokenFile string `json:"token_file"`
}
type SigningRule struct {
	KeyID                 string   `json:"key_id"`
	TagPrefixes           []string `json:"tag_prefixes"`
	RequireDefaultBranch  bool     `json:"require_default_branch,omitempty"`
	RequireStatusContexts bool     `json:"require_status_contexts,omitempty"`
}

func PublicKey(value string) (*ecdsa.PublicKey, error) {
	block, rest := pem.Decode([]byte(value))
	if block == nil || block.Type != "PUBLIC KEY" || strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("invalid signing public key")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errors.New("invalid signing public key")
	}
	key, ok := pub.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("signing key must be ECDSA P-256")
	}
	return key, nil
}
func (k SigningKey) Fingerprint() string {
	pub, err := PublicKey(k.PublicKeyPEM)
	if err != nil {
		return ""
	}
	der, _ := x509.MarshalPKIXPublicKey(pub)
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}
func (s *Signing) Key(id string) (SigningKey, bool) {
	if s != nil {
		for _, k := range s.Keys {
			if k.ID == id {
				return k, true
			}
		}
	}
	return SigningKey{}, false
}
func (c *Config) validateSigning() error {
	s := c.Signing
	if s == nil {
		return nil
	}
	bad := errors.New("invalid signing configuration")
	if !endpoint(s.BuilderID, false) { // builder IDs may include a path, but never credentials/query/fragment.
		if !validBuilder(s.BuilderID) {
			return bad
		}
	}
	if len(s.Keys) == 0 || len(s.Keys) > 32 || len(s.Rules) > 64 {
		return bad
	}
	keys := map[string]bool{}
	for _, k := range s.Keys {
		if !ident.MatchString(k.ID) || keys[k.ID] || !endpoint(k.VaultAddr, k.InsecureHTTP) || !ident.MatchString(k.TransitMount) || !ident.MatchString(k.KeyName) || !ident.MatchString(k.Auth.Mount) || !ident.MatchString(k.Auth.Role) || (k.Auth.Method != "jwt" && k.Auth.Method != "kubernetes") || !filepath.IsAbs(k.Auth.TokenFile) {
			return bad
		}
		if _, err := PublicKey(k.PublicKeyPEM); err != nil {
			return err
		}
		keys[k.ID] = true
	}
	for _, r := range s.Rules {
		if !keys[r.KeyID] || len(r.TagPrefixes) == 0 {
			return bad
		}
		for _, p := range r.TagPrefixes {
			if p == "" || len(p) > 128 || strings.ContainsAny(p, " /:@?\r\n") {
				return bad
			}
		}
	}
	for env, ids := range s.DeployKeys {
		found := false
		for _, e := range c.Environments {
			found = found || e.ID == env
		}
		if !found || len(ids) == 0 {
			return bad
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if !keys[id] || seen[id] {
				return bad
			}
			seen[id] = true
		}
	}
	if s.RequireSignatureForDeploy {
		for _, e := range c.Environments {
			if len(s.DeployKeys[e.ID]) == 0 {
				return bad
			}
		}
	}
	for registry, ref := range s.RegistryCredentials {
		if c.Registry(registry).ID == "" || !ident.MatchString(ref) || ref == "." || ref == ".." || filepath.Base(ref) != ref {
			return bad
		}
	}
	for _, p := range c.Profiles {
		if s.RegistryCredentials[p.RegistryID] == "" {
			return bad
		}
	}
	for _, r := range s.BackfillRepositories {
		base := strings.TrimSuffix(r, "/")
		if !repo.MatchString(base) || strings.Contains(base, "..") || path.Clean(base) != base || !strings.HasSuffix(r, "/") {
			return bad
		}
	}
	return nil
}

func validBuilder(s string) bool {
	u, e := url.Parse(s)
	return e == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}
