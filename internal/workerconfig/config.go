// Package workerconfig holds operator-owned execution authority, outside domain state.
package workerconfig

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"reflect"
	"regexp"
	"releasecontrol/internal/domain"
	"slices"
	"strings"
	"time"
)

type Parameter struct {
	Type string   `json:"type"`
	Enum []string `json:"enum,omitempty"`
	Min  int      `json:"min,omitempty"`
	Max  int      `json:"max,omitempty"`
}
type Recipe struct {
	Check         *domain.WorkerCheckBinding `json:"check,omitempty"`
	ID            string                     `json:"id"`
	Revision      string                     `json:"revision"`
	Hash          string                     `json:"hash"`
	ProductID     string                     `json:"product_id"`
	RepositoryID  string                     `json:"repository_id"`
	RepositoryURL string                     `json:"repository_url"`
	Capability    string                     `json:"capability"`
	Trust         string                     `json:"trust"`
	Parameters    map[string]Parameter       `json:"parameters"`
	Execution     *domain.WorkerExecution    `json:"execution"`
}
type Credential struct {
	ID            string    `json:"id"`
	Principal     string    `json:"principal"`
	TokenEnv      string    `json:"token_env"`
	ProductIDs    []string  `json:"product_ids"`
	RepositoryIDs []string  `json:"repository_ids"`
	Capabilities  []string  `json:"capabilities"`
	ExpiresAt     time.Time `json:"expires_at"`
	Revoked       bool      `json:"revoked"`
}
type Config struct {
	Revision     string              `json:"revision"`
	LeaseSeconds int                 `json:"lease_seconds"`
	Publications []PublicationRecipe `json:"publications,omitempty"`
	Recipes      []Recipe            `json:"recipes"`
	Credentials  []Credential        `json:"credentials"`
}

// Authority keeps a private immutable configuration copy. Secret values are only
// resolved during authentication, never persisted or exposed by config queries.
type Authority struct {
	config             Config
	lookup             func(string) string
	controlFingerprint [sha256.Size]byte
}
type Principal struct {
	credential  Credential
	fingerprint [sha256.Size]byte
}

var ident = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$`)
var envName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
var digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func Fingerprint(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func RecipeHash(r Recipe) string { r.Hash = ""; return Fingerprint(r) }
func Load(path, controlToken string) (*Authority, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	if len(b) > 1<<20 {
		return nil, errors.New("worker configuration too large")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Config
	if e = dec.Decode(&c); e != nil {
		return nil, errors.New("invalid worker configuration")
	}
	if dec.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid worker configuration")
	}
	return New(c, controlToken, os.Getenv)
}
func New(c Config, controlToken string, lookup func(string) string) (*Authority, error) {
	if strings.TrimSpace(controlToken) == "" || lookup == nil {
		return nil, errors.New("worker protocol requires protected control plane")
	}
	if !ident.MatchString(c.Revision) || c.LeaseSeconds < 5 || c.LeaseSeconds > 300 || len(c.Recipes) == 0 || len(c.Recipes) > 100 || len(c.Publications) > 100 || len(c.Credentials) == 0 || len(c.Credentials) > 100 {
		return nil, errors.New("invalid worker configuration bounds")
	}
	seen := map[string]bool{}
	tokens := map[string]bool{Fingerprint(controlToken): true}
	for _, v := range c.Credentials {
		token := lookup(v.TokenEnv)
		if !ident.MatchString(v.ID) || !ident.MatchString(v.Principal) || !envName.MatchString(v.TokenEnv) || seen[v.ID] || v.ExpiresAt.IsZero() || len(token) < 32 || tokens[Fingerprint(token)] || !validGrants(v.ProductIDs) || !validGrants(v.RepositoryIDs) || !validGrants(v.Capabilities) {
			return nil, errors.New("invalid worker credential configuration")
		}
		if slices.Contains(v.Capabilities, "registry_publish") && (slices.Contains(v.Capabilities, "source_check") || slices.Contains(v.Capabilities, "local_oci_build")) {
			return nil, errors.New("publication and source credentials must be separate")
		}
		for _, prior := range c.Credentials {
			if prior.ID != v.ID && prior.Principal == v.Principal && (slices.Contains(v.Capabilities, "registry_publish") != slices.Contains(prior.Capabilities, "registry_publish")) {
				return nil, errors.New("publication and source principals must be separate")
			}
		}
		seen[v.ID] = true
		tokens[Fingerprint(token)] = true
	}
	seen = map[string]bool{}
	for _, r := range c.Recipes {
		if r.Check != nil {
			if r.Capability != "local_oci_build" || r.Trust != "trusted_source" || len(r.Parameters) != 0 || !regexp.MustCompile(`^RCP /[A-Za-z0-9 ._/-]{1,96}$`).MatchString(r.Check.Name) || r.Check.ExpectedAppID < 1 || r.Check.FinalizeSeconds < 60 || r.Check.FinalizeSeconds > 600 {
				return nil, errors.New("invalid operator check binding")
			}
			for _, other := range c.Recipes {
				if other.Check != nil && (other.ID != r.ID || other.ProductID != r.ProductID) && other.Check.Name == r.Check.Name && other.Check.ExpectedAppID == r.Check.ExpectedAppID && CanonicalRepositoryURL(other.RepositoryURL) == CanonicalRepositoryURL(r.RepositoryURL) {
					return nil, errors.New("conflicting physical repository check owner")
				}
			}
		}
		u, e := url.Parse(r.RepositoryURL)
		if !ident.MatchString(r.ID) || !ident.MatchString(r.Revision) || !ident.MatchString(r.ProductID) || !ident.MatchString(r.RepositoryID) || r.Capability != "source_check" && r.Capability != "local_oci_build" || seen[r.ID] || e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || r.Trust != "trusted_source" && r.Trust != "untrusted_source" || !digest.MatchString(r.Hash) || r.Hash != RecipeHash(r) || len(r.Parameters) > 32 {
			return nil, errors.New("invalid immutable nonpublishing worker recipe")
		}
		seen[r.ID] = true
		if err := ValidateExecution(r); err != nil {
			return nil, err
		}
		for name, p := range r.Parameters {
			if !ident.MatchString(name) || p.Type != "boolean" && p.Type != "integer" && p.Type != "enum" || p.Type == "integer" && p.Min > p.Max || p.Type == "enum" && (len(p.Enum) == 0 || len(p.Enum) > 32) {
				return nil, errors.New("invalid recipe parameter")
			}
			for _, v := range p.Enum {
				if !ident.MatchString(v) {
					return nil, errors.New("invalid enum")
				}
			}
		}
	}
	for _, r := range c.Publications {
		if err := ValidatePublication(r); err != nil {
			return nil, err
		}
		if seen[r.ID] {
			return nil, errors.New("duplicate recipe")
		}
		seen[r.ID] = true
		for _, ref := range []string{r.Pull.UsernameEnv, r.Pull.PasswordEnv, r.Pull.BearerEnv} {
			if ref != "" {
				raw := lookup(ref)
				if len(raw) == 0 || tokens[Fingerprint(raw)] {
					return nil, errors.New("registry and control credentials must be separate")
				}
			}
		}
	}
	b, _ := json.Marshal(c)
	var copy Config
	_ = json.Unmarshal(b, &copy)
	return &Authority{config: copy, lookup: lookup, controlFingerprint: sha256.Sum256([]byte(controlToken))}, nil
}

func CanonicalRepositoryURL(raw string) string {
	u, e := url.Parse(strings.ToLower(raw))
	if e != nil {
		return ""
	}
	u.Host = strings.TrimSuffix(u.Host, ":443")
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), ".git")
	return u.String()
}
func (a *Authority) CheckRecipes() []Recipe {
	out := []Recipe{}
	if a != nil {
		for _, r := range a.config.Recipes {
			if r.Check != nil {
				copy, _ := a.Recipe(r.ID)
				out = append(out, copy)
			}
		}
	}
	return out
}

func ValidateExecution(r Recipe) error {
	if r.Check != nil && (!domain.ValidWorkerCheckBinding(r.Check) || r.Capability != "local_oci_build" || r.Trust != "trusted_source" || len(r.Parameters) != 0) {
		return errors.New("invalid operator check binding")
	}
	e := r.Execution
	// Context-only source-check recipes remain claimable by external protocol
	// clients; cmd/worker requires an explicit execution definition.
	if e == nil {
		if r.Capability == "source_check" {
			return nil
		}
		return errors.New("execution definition required")
	}
	pinned := regexp.MustCompile(`^[a-z0-9][a-z0-9./:_-]*@sha256:[0-9a-f]{64}$`)
	if r.Trust != "trusted_source" || e.Kind != r.Capability || !pinned.MatchString(e.ToolchainImage) || e.Network != "none" || e.Platform != "linux/amd64" && e.Platform != "linux/arm64" || e.TimeoutSeconds < 1 || e.TimeoutSeconds > 1800 || e.MemoryMB < 64 || e.MemoryMB > 16384 || e.StorageMB < 16 || e.StorageMB > 4096 || e.CPUs < 1 || e.CPUs > 16 || e.PIDs < 16 || e.PIDs > 1024 || e.SourceBytes < 1 || e.SourceBytes > 256<<20 || e.OutputBytes < 1 || e.OutputBytes > 512<<20 || e.LogBytes < 1 || e.LogBytes > 1<<20 || !validArgv(e.CheckArgv) {
		return errors.New("invalid trusted isolated execution definition")
	}
	if e.Kind == "local_oci_build" {
		if r := e.RuntimeRootFS; r != nil && (!digest.MatchString(r.Hash) || !digest.MatchString(r.InputsHash) || r.ArchiveBytes < 1 || r.ArchiveBytes > 128<<20 || r.MaxExpandedBytes < 1 || r.MaxExpandedBytes > 128<<20 || r.MaxEntries < 1 || r.MaxEntries > 20000) {
			return errors.New("invalid runtime rootfs pins or bounds")
		}
		if !pinned.MatchString(e.BuildKitImage) || !validArgv(e.BuildArgv) || !cleanRelative(e.OutputFile) || !cleanRelative(e.ImagePath) || len(e.Entrypoint) == 0 || !validArgv(e.Entrypoint) {
			return errors.New("invalid local OCI output contract")
		}
	} else if e.RuntimeRootFS != nil || e.BuildKitImage != "" || len(e.BuildArgv) != 0 || e.OutputFile != "" || e.ImagePath != "" || len(e.Entrypoint) != 0 {
		return errors.New("source check cannot declare OCI outputs")
	}
	return nil
}
func validArgv(argv []string) bool {
	if len(argv) == 0 || len(argv) > 32 {
		return false
	}
	for _, v := range argv {
		if v == "" || len(v) > 2048 || strings.ContainsAny(v, "\x00\n\r") {
			return false
		}
	}
	return true
}
func cleanRelative(s string) bool {
	return s != "" && len(s) < 256 && path.Clean(s) == s && s != "." && !strings.HasPrefix(s, "/") && !strings.HasPrefix(s, "../") && !strings.ContainsAny(s, "\x00\n\r\\ ")
}
func validGrants(v []string) bool {
	if len(v) == 0 || len(v) > 100 {
		return false
	}
	for _, s := range v {
		if !ident.MatchString(s) || s == "*" {
			return false
		}
	}
	return true
}
func (a *Authority) Revision() string     { return a.config.Revision }
func (a *Authority) Lease() time.Duration { return time.Duration(a.config.LeaseSeconds) * time.Second }
func (a *Authority) Recipe(id string) (Recipe, bool) {
	for _, v := range a.config.Recipes {
		if v.ID == id {
			b, _ := json.Marshal(v)
			var out Recipe
			_ = json.Unmarshal(b, &out)
			return out, true
		}
	}
	return Recipe{}, false
}
func (a *Authority) IsWorkerToken(token string) bool {
	if a == nil || token == "" {
		return false
	}
	matched := 0
	provided := sha256.Sum256([]byte(token))
	for _, c := range a.config.Credentials {
		v := sha256.Sum256([]byte(a.lookup(c.TokenEnv)))
		matched |= subtle.ConstantTimeCompare(provided[:], v[:])
	}
	return matched == 1
}
func (a *Authority) Authenticate(token string, now time.Time) (Principal, error) {
	if a == nil || token == "" || !a.registryCredentialsSeparated() {
		return Principal{}, errors.New("worker authentication required")
	}
	provided := sha256.Sum256([]byte(token))
	var selected *Credential
	matches := 0
	for i := range a.config.Credentials {
		c := &a.config.Credentials[i]
		raw := a.lookup(c.TokenEnv)
		v := sha256.Sum256([]byte(raw))
		if subtle.ConstantTimeCompare(provided[:], v[:]) == 1 && len(raw) >= 32 {
			selected = c
			matches++
		}
	}
	// Environment-backed secrets can rotate after startup. Recheck separation
	// and uniqueness on every request, including collisions with revoked keys.
	if subtle.ConstantTimeCompare(provided[:], a.controlFingerprint[:]) == 1 || matches != 1 || selected == nil || selected.Revoked || !now.Before(selected.ExpiresAt) {
		return Principal{}, errors.New("worker authentication required")
	}
	for _, r := range a.config.Publications {
		for _, ref := range []string{r.Pull.UsernameEnv, r.Pull.PasswordEnv, r.Pull.BearerEnv} {
			if ref != "" {
				v := sha256.Sum256([]byte(a.lookup(ref)))
				if subtle.ConstantTimeCompare(provided[:], v[:]) == 1 {
					return Principal{}, errors.New("worker authentication required")
				}
			}
		}
	}
	return Principal{credential: *selected, fingerprint: provided}, nil
}
func (p Principal) ID() string { return p.credential.Principal }
func (p Principal) Allows(product, repo, capability string, now time.Time) bool {
	c := p.credential
	return c.Principal != "" && !c.Revoked && now.Before(c.ExpiresAt) && slices.Contains(c.ProductIDs, product) && slices.Contains(c.RepositoryIDs, repo) && slices.Contains(c.Capabilities, capability)
}
func ValidateParameters(r Recipe, params map[string]any) error {
	if len(params) != len(r.Parameters) {
		return errors.New("parameters must exactly match recipe")
	}
	for name, def := range r.Parameters {
		v, ok := params[name]
		if !ok {
			return errors.New("missing parameter")
		}
		switch def.Type {
		case "boolean":
			if _, ok := v.(bool); !ok {
				return errors.New("expected boolean parameter")
			}
		case "enum":
			s, ok := v.(string)
			if !ok || !slices.Contains(def.Enum, s) {
				return errors.New("invalid enum parameter")
			}
		case "integer":
			b, _ := json.Marshal(v)
			var n int
			if json.Unmarshal(b, &n) != nil || n < def.Min || n > def.Max {
				return fmt.Errorf("integer parameter outside bounds")
			}
		default:
			return errors.New("invalid parameter definition")
		}
	}
	return nil
}
func ValidFingerprint(s string) bool { return digest.MatchString(s) }

// Current revalidates a request principal after network I/O, including secret rotation.
func (a *Authority) Current(p Principal, now time.Time) bool {
	if a == nil {
		return false
	}
	for _, c := range a.config.Credentials {
		if c.ID == p.credential.ID {
			raw := a.lookup(c.TokenEnv)
			sum := sha256.Sum256([]byte(raw))
			if sum != p.fingerprint {
				return false
			}
			fresh, err := a.Authenticate(raw, now)
			return err == nil && reflect.DeepEqual(fresh.credential, p.credential)
		}
	}
	return false
}

func ValidIdentifier(s string) bool { return ident.MatchString(s) }
func (a *Authority) AllowsNativeComponent(product, component, profile, image string) bool {
	if a != nil {
		for _, r := range a.config.Publications {
			if r.ProductID == product && r.ComponentID == component && r.Profile == profile && strings.TrimPrefix(strings.TrimPrefix(r.Origin, "https://"), "http://")+"/"+r.Repository == image {
				return true
			}
		}
	}
	return false
}

func (a *Authority) registryCredentialsSeparated() bool {
	if a == nil {
		return false
	}
	tokens := map[[sha256.Size]byte]bool{a.controlFingerprint: true}
	for _, c := range a.config.Credentials {
		tokens[sha256.Sum256([]byte(a.lookup(c.TokenEnv)))] = true
	}
	for _, r := range a.config.Publications {
		for _, ref := range []string{r.Pull.UsernameEnv, r.Pull.PasswordEnv, r.Pull.BearerEnv} {
			if ref != "" {
				raw := a.lookup(ref)
				if raw == "" || tokens[sha256.Sum256([]byte(raw))] {
					return false
				}
			}
		}
	}
	return true
}
