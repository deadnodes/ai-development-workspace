package workerconfig

import (
	"errors"
	"net"
	"net/url"
	"regexp"
)

type RegistryRead struct {
	CAFile              string `json:"ca_file,omitempty"`
	UsernameEnv         string `json:"username_env,omitempty"`
	PasswordEnv         string `json:"password_env,omitempty"`
	BearerEnv           string `json:"bearer_env,omitempty"`
	FixtureLoopbackHTTP bool   `json:"fixture_loopback_http,omitempty"`
}
type PublicationRecipe struct {
	ID              string       `json:"id"`
	Revision        string       `json:"revision"`
	Hash            string       `json:"hash"`
	ProductID       string       `json:"product_id"`
	RepositoryID    string       `json:"repository_id"`
	ComponentID     string       `json:"component_id"`
	Profile         string       `json:"profile"`
	BuildRecipeHash string       `json:"build_recipe_hash"`
	Origin          string       `json:"origin"`
	Repository      string       `json:"repository"`
	MaxBytes        int64        `json:"max_bytes"`
	TimeoutSeconds  int          `json:"timeout_seconds"`
	Pull            RegistryRead `json:"pull"`
}

func PublicationHash(r PublicationRecipe) string { r.Hash = ""; return Fingerprint(r) }
func ValidatePublication(r PublicationRecipe) error {
	for _, v := range []string{r.ID, r.Revision, r.ProductID, r.RepositoryID, r.ComponentID, r.Profile} {
		if !ident.MatchString(v) {
			return errors.New("invalid publication identity")
		}
	}
	u, e := url.Parse(r.Origin)
	if e != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.String() != r.Origin {
		return errors.New("invalid registry origin")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || !r.Pull.FixtureLoopbackHTTP || ip == nil || !ip.IsLoopback() {
			return errors.New("registry TLS required")
		}
	}
	if !regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`).MatchString(r.Repository) || len(r.Repository) > 255 || !digest.MatchString(r.BuildRecipeHash) || r.Hash != PublicationHash(r) || r.MaxBytes < 1 || r.MaxBytes > 256<<20 || r.TimeoutSeconds < 1 || r.TimeoutSeconds > 600 {
		return errors.New("invalid publication bounds or hash")
	}
	for _, v := range []string{r.Pull.UsernameEnv, r.Pull.PasswordEnv, r.Pull.BearerEnv} {
		if v != "" && !envName.MatchString(v) {
			return errors.New("invalid registry credential reference")
		}
	}
	if (r.Pull.UsernameEnv == "") != (r.Pull.PasswordEnv == "") || r.Pull.BearerEnv != "" && r.Pull.UsernameEnv != "" {
		return errors.New("invalid registry credential combination")
	}
	return nil
}
func (a *Authority) Publication(id string) (PublicationRecipe, bool) {
	if a != nil {
		for _, r := range a.config.Publications {
			if r.ID == id {
				return r, true
			}
		}
	}
	return PublicationRecipe{}, false
}
