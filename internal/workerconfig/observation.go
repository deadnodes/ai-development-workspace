package workerconfig

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"releasecontrol/internal/domain"
	"strings"
	"time"
)

type RegistryObserveRecipe struct {
	ID           string                       `json:"id"`
	Revision     string                       `json:"revision"`
	Hash         string                       `json:"hash"`
	ProductID    string                       `json:"product_id"`
	RepositoryID string                       `json:"repository_id"`
	ComponentID  string                       `json:"component_id"`
	Origin       string                       `json:"origin"`
	Repository   string                       `json:"repository"`
	Limits       domain.RegistryObserveLimits `json:"limits"`
	Pull         RegistryRead                 `json:"pull"`
}

func ObservationHash(r RegistryObserveRecipe) string { r.Hash = ""; return Fingerprint(r) }
func ValidateObservation(r RegistryObserveRecipe) error {
	for _, s := range []string{r.ID, r.Revision, r.ProductID, r.RepositoryID, r.ComponentID} {
		if !ident.MatchString(s) {
			return errors.New("invalid registry observation identity")
		}
	}
	u, e := url.Parse(r.Origin)
	if e != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.String() != r.Origin || strings.ToLower(u.Host) != u.Host {
		return errors.New("invalid registry observation origin")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || !r.Pull.FixtureLoopbackHTTP || ip == nil || !ip.IsLoopback() {
			return errors.New("registry observation TLS required")
		}
	}
	if !regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`).MatchString(r.Repository) || len(r.Repository) > 255 || r.Hash != ObservationHash(r) {
		return errors.New("invalid registry observation repository or hash")
	}
	if _, e := domain.CanonicalImageRepository(u.Host + "/" + r.Repository); e != nil {
		return errors.New("noncanonical registry observation image")
	}
	if ValidateObservationLimits(r.Limits) != nil {
		return errors.New("invalid registry observation limits")
	}
	for _, s := range []string{r.Pull.UsernameEnv, r.Pull.PasswordEnv, r.Pull.BearerEnv} {
		if s != "" && !envName.MatchString(s) {
			return errors.New("invalid registry observation credential reference")
		}
	}
	if (r.Pull.UsernameEnv == "") != (r.Pull.PasswordEnv == "") || r.Pull.BearerEnv != "" && r.Pull.UsernameEnv != "" {
		return errors.New("invalid registry observation credentials")
	}
	return nil
}
func ValidateObservationLimits(l domain.RegistryObserveLimits) error {
	return domain.ValidateRegistryObserveLimits(l)
}

func (a *Authority) Observation(id string) (RegistryObserveRecipe, bool) {
	if a != nil {
		for _, r := range a.config.Observations {
			if r.ID == id {
				return r, true
			}
		}
	}
	return RegistryObserveRecipe{}, false
}
func (a *Authority) Observations() []RegistryObserveRecipe {
	if a == nil {
		return []RegistryObserveRecipe{}
	}
	return append([]RegistryObserveRecipe{}, a.config.Observations...)
}

// RegistryObservations is the nonsecret authority projection consumed by the
// pure retention evaluator. It exposes neither credential references nor paths.
func (a *Authority) RegistryObservations(now time.Time) []domain.RegistryObservationAuthority {
	out := []domain.RegistryObservationAuthority{}
	if a == nil {
		return out
	}
	for _, r := range a.config.Observations {
		u, _ := url.Parse(r.Origin)
		image, _ := domain.CanonicalImageRepository(u.Host + "/" + r.Repository)
		current := false
		for _, c := range a.config.Credentials {
			p, e := a.Authenticate(a.lookup(c.TokenEnv), now)
			if e == nil && p.Allows(r.ProductID, r.RepositoryID, "registry_observe", now) {
				current = true
				break
			}
		}
		out = append(out, domain.RegistryObservationAuthority{Scope: domain.RegistryScope{ProductID: r.ProductID, ComponentID: r.ComponentID, RepositoryID: r.RepositoryID, Origin: r.Origin, Repository: r.Repository, ImageRepository: image}, RecipeID: r.ID, RecipeRevision: r.Revision, RecipeHash: r.Hash, ConfigRevision: a.config.Revision, Limits: r.Limits, Current: current})
	}
	return out
}
