// Package buildconfig defines operator-owned Forgejo build authority.
package buildconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
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
type GitOps struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	APIURL         string `json:"api_url"`
	AppID          int64  `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	PrivateKeyRef  string `json:"private_key_ref"`
	Repository     string `json:"repository"`
	Branch         string `json:"branch"`
}
type Environment struct {
	ID             string `json:"id"`
	GitOpsID       string `json:"gitops_id"`
	AllowAutoMerge bool   `json:"allow_auto_merge"`
}
type DeploymentTarget struct {
	Files []DeploymentFile `json:"files"`
}
type RolloutTarget struct {
	Namespace    string   `json:"namespace"`
	HelmReleases []string `json:"helm_releases,omitempty"`
	Deployments  []string `json:"deployments,omitempty"`
}
type RolloutConfig struct {
	InCluster      bool `json:"in_cluster"`
	TimeoutSeconds int  `json:"timeout_seconds,omitempty"`
}
type DeploymentFile struct {
	Rollout         *RolloutTarget `json:"rollout,omitempty"`
	Format          string         `json:"format,omitempty"`
	Path            string         `json:"path"`
	ImageID         string         `json:"image_id"`
	ImageRepository string         `json:"image_repository"`
	TagFormat       string         `json:"tag_format"`
	MinMatches      int            `json:"min_matches"`
}
type CIGate struct {
	TrustedProducers string `json:"trusted_producers,omitempty"`
	AcceptPRTree     bool   `json:"accept_pr_tree"`
	DevMode          string `json:"dev_mode,omitempty"`
}

type Profile struct {
	RequireSecurityGate    bool                        `json:"require_security_gate,omitempty"`
	AutoBuild              *bool                       `json:"auto_build,omitempty"`
	AutoDeployDev          *bool                       `json:"auto_deploy_dev,omitempty"`
	CIGate                 *CIGate                     `json:"ci_gate,omitempty"`
	RequiredStatusContexts []string                    `json:"required_status_contexts,omitempty"`
	Deployments            map[string]DeploymentTarget `json:"deployments,omitempty"`
	ID                     string                      `json:"id"`
	ProductID              string                      `json:"product_id"`
	ConnectionID           string                      `json:"connection_id"`
	Repository             string                      `json:"repository"`
	Workflow               string                      `json:"workflow"`
	DefaultBranch          string                      `json:"default_branch"`
	PoolID                 string                      `json:"pool_id"`
	RegistryID             string                      `json:"registry_id"`
	Images                 []Image                     `json:"images"`
	DispatchRef            string                      `json:"dispatch_ref"`
	TagCandidates          []string                    `json:"tag_candidates"`
}
type Image struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
}
type Config struct {
	Signing                   *Signing      `json:"signing,omitempty"`
	Rollout                   RolloutConfig `json:"rollout,omitempty"`
	GitOps                    []GitOps      `json:"gitops,omitempty"`
	Environments              []Environment `json:"environments,omitempty"`
	Connections               []Connection  `json:"connections"`
	Registries                []Registry    `json:"registries"`
	Pools                     []Pool        `json:"pools"`
	Profiles                  []Profile     `json:"profiles"`
	TimeoutSeconds            int           `json:"timeout_seconds,omitempty"`
	RunLookbackSeconds        int           `json:"run_lookback_seconds,omitempty"`
	StaleOperationSeconds     int           `json:"stale_operation_seconds,omitempty"`
	ReconcileInterval         int           `json:"reconcile_interval,omitempty"`
	HeartbeatInterval         int           `json:"heartbeat_interval,omitempty"`
	LeaseTTL                  int           `json:"lease_ttl,omitempty"`
	StalledTimeout            int           `json:"stalled_timeout,omitempty"`
	NotifyRetrySeconds        int           `json:"notify_retry_seconds,omitempty"`
	MaxBackoffSeconds         int           `json:"max_backoff_seconds,omitempty"`
	EscalateAfterAttempts     int           `json:"escalate_after_attempts,omitempty"`
	EscalateAfterDelivered    int           `json:"escalate_after_delivered,omitempty"`
	TurnEndDebounceSeconds    int           `json:"turn_end_debounce_seconds,omitempty"`
	staleOperationSecondsSet  bool
	reconcileIntervalSet      bool
	heartbeatIntervalSet      bool
	leaseTTLSet               bool
	stalledTimeoutSet         bool
	notifyRetrySecondsSet     bool
	maxBackoffSecondsSet      bool
	escalateAfterAttemptsSet  bool
	turnEndDebounceSecondsSet bool
}

func (c Config) StaleTimeoutSeconds() int {
	if c.StaleOperationSeconds == 0 && !c.staleOperationSecondsSet {
		return 7200
	}
	return c.StaleOperationSeconds
}

func (c Config) StaleOperationSecondsConfigured() bool { return c.staleOperationSecondsSet }

func (c *Config) MarkStaleOperationSecondsConfigured() { c.staleOperationSecondsSet = true }

func (c *Config) UnmarshalJSON(data []byte) error {
	type configJSON Config
	var decoded configJSON
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*c = Config(decoded)
	_, c.staleOperationSecondsSet = fields["stale_operation_seconds"]
	_, c.reconcileIntervalSet = fields["reconcile_interval"]
	_, c.heartbeatIntervalSet = fields["heartbeat_interval"]
	_, c.leaseTTLSet = fields["lease_ttl"]
	_, c.stalledTimeoutSet = fields["stalled_timeout"]
	_, c.notifyRetrySecondsSet = fields["notify_retry_seconds"]
	_, c.maxBackoffSecondsSet = fields["max_backoff_seconds"]
	_, c.escalateAfterAttemptsSet = fields["escalate_after_attempts"]
	_, c.turnEndDebounceSecondsSet = fields["turn_end_debounce_seconds"]
	return nil
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
	if err := c.validateSigning(); err != nil {
		return err
	}
	bad := errors.New("invalid build configuration authority")
	if c.Rollout.TimeoutSeconds == 0 {
		c.Rollout.TimeoutSeconds = 900
	}
	if c.Rollout.TimeoutSeconds < 60 || c.Rollout.TimeoutSeconds > 3600 {
		return bad
	}
	dns := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	name := func(v string) bool { return len(v) <= 63 && dns.MatchString(v) }
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
	gitops := map[string]bool{}
	for _, g := range c.GitOps {
		ids = append(ids, g.ID)
		if g.Kind != "github" || !endpoint(g.APIURL, false) || g.AppID <= 0 || g.InstallationID <= 0 || !ident.MatchString(g.PrivateKeyRef) || g.PrivateKeyRef == "." || g.PrivateKeyRef == ".." || !repo.MatchString(g.Repository) || strings.Count(g.Repository, "/") != 1 || strings.Contains(g.Repository, "..") || g.Branch != "main" {
			return bad
		}
		gitops[g.ID] = true
	}
	if !unique(ids) {
		return bad
	}
	ids = nil
	for _, env := range c.Environments {
		ids = append(ids, env.ID)
		if env.ID != "dev" || !env.AllowAutoMerge || !gitops[env.GitOpsID] {
			return bad
		}
	}
	if !unique(ids) {
		return bad
	}
	ids = nil
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
		if v.CIGate != nil && v.CIGate.DevMode != "" && v.CIGate.DevMode != "observe" && v.CIGate.DevMode != "block" {
			return errors.New("ci_gate.dev_mode must be observe or block")
		}
		if v.CIGate != nil && v.CIGate.TrustedProducers != "" && v.CIGate.TrustedProducers != "forgejo_actions" && v.CIGate.TrustedProducers != "any" {
			return errors.New("ci_gate.trusted_producers must be forgejo_actions or any")
		}
		if ((v.AutoBuild != nil && *v.AutoBuild) || (v.AutoDeployDev != nil && *v.AutoDeployDev)) && v.DispatchRef != v.DefaultBranch {
			return fmt.Errorf("profile %s: auto_build/auto_deploy_dev require dispatch_ref to equal default_branch", v.ID)
		}
		ids = append(ids, v.ID)
		cn, ok := con[v.ConnectionID]
		if !ok || !ident.MatchString(v.ProductID) || !repo.MatchString(v.Repository) || strings.Split(v.Repository, "/")[0] != cn.Owner || strings.Count(v.Repository, "/") != 1 || v.Workflow != "build.yml" || !Branch(v.DispatchRef) || !Branch(v.DefaultBranch) || !pools[v.PoolID] || !regs[v.RegistryID] || len(v.Images) == 0 || len(v.Images) > 64 || len(v.TagCandidates) == 0 || len(v.TagCandidates) > 16 {
			return bad
		}
		imageIDs := []string{}
		repositories := map[string]bool{}
		for _, image := range v.Images {
			imageIDs = append(imageIDs, image.ID)
			if !repo.MatchString(image.Repository) || strings.Contains(image.Repository, "..") || repositories[image.Repository] {
				return bad
			}
			repositories[image.Repository] = true
		}
		if !unique(imageIDs) {
			return bad
		}
		contexts := map[string]bool{}
		for _, context := range v.RequiredStatusContexts {
			if len(context) == 0 || len(context) > 256 || strings.ContainsAny(context, "\r\n") || contexts[context] {
				return bad
			}
			contexts[context] = true
		}
		if len(contexts) > 100 {
			return bad
		}
		for env, target := range v.Deployments {
			if env != "dev" || len(c.Environments) != 1 || len(target.Files) == 0 || len(target.Files) > 16 {
				return bad
			}
			seen := map[string]bool{}
			for _, f := range target.Files {
				if r := f.Rollout; r != nil {
					if !name(r.Namespace) || (r.HelmReleases != nil) == (r.Deployments != nil) {
						return bad
					}
					names := r.HelmReleases
					if names == nil {
						names = r.Deployments
					}
					if len(names) == 0 || len(names) > 20 || !unique(names) {
						return bad
					}
					for _, workload := range names {
						if !name(workload) {
							return bad
						}
					}
				}
				imageRepo := ""
				for _, image := range v.Images {
					if image.ID == f.ImageID {
						imageRepo = image.Repository
					}
				}
				registryURL, _ := url.Parse(c.Registry(v.RegistryID).Origin)
				key := f.Path + "|" + f.ImageRepository
				if imageRepo == "" || f.ImageRepository != registryURL.Host+"/"+imageRepo || seen[key] || f.MinMatches < 1 || f.MinMatches > 100 || f.Path == "" || path.IsAbs(f.Path) || path.Clean(f.Path) != f.Path || strings.HasPrefix(f.Path, "../") || f.Path == ".." || strings.ContainsAny(f.Path, "\\\r\n\t\x00?#%") || len(f.Path) > 1024 || (f.Format != "" && f.Format != "helm_values" && f.Format != "container_image") || (f.Format != "container_image" && f.TagFormat != "{tag}@{digest}" && f.TagFormat != "{digest}") {
					return bad
				}
				seen[key] = true
			}
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
	if c.RunLookbackSeconds == 0 {
		c.RunLookbackSeconds = 86400
	}
	if c.RunLookbackSeconds < 1 || c.RunLookbackSeconds > 31536000 {
		return bad
	}
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = 7200
	}
	if c.TimeoutSeconds < 1 || c.TimeoutSeconds > 604800 {
		return bad
	}
	if !c.staleOperationSecondsSet && c.StaleOperationSeconds == 0 {
		c.StaleOperationSeconds = 7200
	}
	if c.StaleOperationSeconds < 0 || c.StaleOperationSeconds > 604800 {
		return bad
	}
	if !c.reconcileIntervalSet && c.ReconcileInterval == 0 {
		c.ReconcileInterval = 30
	}
	if c.ReconcileInterval < 0 || c.ReconcileInterval > 3600 {
		return bad
	}
	if !c.heartbeatIntervalSet && c.HeartbeatInterval == 0 {
		c.HeartbeatInterval = 60
	}
	if c.HeartbeatInterval < 1 || c.HeartbeatInterval > 3600 {
		return bad
	}
	if !c.leaseTTLSet && c.LeaseTTL == 0 {
		c.LeaseTTL = 300
	}
	if c.LeaseTTL < 30 || c.LeaseTTL > 86400 {
		return bad
	}
	if !c.stalledTimeoutSet && c.StalledTimeout == 0 {
		c.StalledTimeout = 900
	}
	if c.StalledTimeout < 0 || c.StalledTimeout > 604800 {
		return bad
	}
	if !c.notifyRetrySecondsSet && c.NotifyRetrySeconds == 0 {
		c.NotifyRetrySeconds = 30
	}
	if !c.maxBackoffSecondsSet && c.MaxBackoffSeconds == 0 {
		c.MaxBackoffSeconds = 900
	}
	if !c.escalateAfterAttemptsSet && c.EscalateAfterAttempts == 0 {
		c.EscalateAfterAttempts = 10
	}
	if c.EscalateAfterDelivered == 0 {
		c.EscalateAfterDelivered = 3
	}
	if c.NotifyRetrySeconds < 1 || c.NotifyRetrySeconds > 86400 || c.MaxBackoffSeconds < 1 || c.MaxBackoffSeconds > 604800 || c.EscalateAfterAttempts < 1 || c.EscalateAfterAttempts > 1000 {
		return bad
	}
	if c.EscalateAfterDelivered < 1 || c.EscalateAfterDelivered > 1000 {
		return bad
	}
	if !c.turnEndDebounceSecondsSet && c.TurnEndDebounceSeconds == 0 {
		c.TurnEndDebounceSeconds = 20
	}
	if c.TurnEndDebounceSeconds < 1 || c.TurnEndDebounceSeconds > 3600 {
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

func (c *Config) DevGitOps() (GitOps, bool) {
	if c != nil {
		for _, e := range c.Environments {
			if e.ID == "dev" && e.AllowAutoMerge {
				for _, g := range c.GitOps {
					if g.ID == e.GitOpsID {
						return g, true
					}
				}
			}
		}
	}
	return GitOps{}, false
}
