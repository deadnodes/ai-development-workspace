package workerconfig

import (
	"strings"
	"testing"
	"time"
)

func testConfig() Config {
	r := Recipe{ID: "check", Revision: "v1", ProductID: "p", RepositoryID: "r", RepositoryURL: "https://github.com/test/repo", Capability: "source_check", Trust: "untrusted_source", Parameters: map[string]Parameter{"count": {Type: "integer", Min: 1, Max: 5}, "mode": {Type: "enum", Enum: []string{"test", "lint"}}, "race": {Type: "boolean"}}}
	r.Hash = RecipeHash(r)
	return Config{Revision: "cfg1", LeaseSeconds: 10, Recipes: []Recipe{r}, Credentials: []Credential{{ID: "runner", Principal: "worker/test", TokenEnv: "WORKER_FIXTURE_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"r"}, Capabilities: []string{"source_check"}, ExpiresAt: time.Now().Add(time.Hour)}}}
}
func TestIndependentScopedExpiringAuthority(t *testing.T) {
	token := strings.Repeat("w", 32)
	c := testConfig()
	a, e := New(c, "control", func(string) string { return token })
	if e != nil {
		t.Fatal(e)
	}
	p, e := a.Authenticate(token, time.Now())
	if e != nil || !p.Allows("p", "r", "source_check", time.Now()) || p.Allows("other", "r", "source_check", time.Now()) || p.Allows("p", "other", "source_check", time.Now()) || p.Allows("p", "r", "deploy", time.Now()) {
		t.Fatal("bad scoped authority")
	}
	if _, e = a.Authenticate("control", time.Now()); e == nil {
		t.Fatal("global token authenticates worker")
	}
	if _, e = a.Authenticate(token, time.Now().Add(2*time.Hour)); e == nil {
		t.Fatal("expired token accepted")
	}
	token = strings.Repeat("x", 32)
	if _, e = a.Authenticate(strings.Repeat("w", 32), time.Now()); e == nil {
		t.Fatal("rotated token accepted")
	}
	if _, e = a.Authenticate(token, time.Now()); e != nil {
		t.Fatal("rotated credential not resolved")
	}
	c.Credentials[0].Revoked = true
	a, e = New(c, "control", func(string) string { return token })
	if e != nil {
		t.Fatal(e)
	}
	if _, e = a.Authenticate(token, time.Now()); e == nil {
		t.Fatal("revoked credential accepted")
	}
	if !a.IsWorkerToken(token) {
		t.Fatal("revoked token must remain denied on ordinary channel")
	}
}
func TestFailClosedConfigAndTypedParameters(t *testing.T) {
	token := strings.Repeat("w", 32)
	for _, mutate := range []func(*Config){func(c *Config) {
		c.Recipes[0].RepositoryURL = "https://user:password@github.com/test/repo"
		c.Recipes[0].Hash = RecipeHash(c.Recipes[0])
	}, func(c *Config) { c.Recipes[0].Hash = "sha256:" + strings.Repeat("a", 64) }, func(c *Config) { c.Recipes[0].Capability = "deploy"; c.Recipes[0].Hash = RecipeHash(c.Recipes[0]) }, func(c *Config) { c.Credentials[0].ProductIDs = []string{"*"} }, func(c *Config) { c.Credentials = append(c.Credentials, c.Credentials[0]) }, func(c *Config) { c.LeaseSeconds = 0 }} {
		c := testConfig()
		mutate(&c)
		if _, e := New(c, "control", func(string) string { return token }); e == nil {
			t.Fatal("unsafe config accepted")
		}
	}
	if _, e := New(testConfig(), "", func(string) string { return token }); e == nil {
		t.Fatal("unprotected control plane accepted")
	}
	if _, e := New(testConfig(), token, func(string) string { return token }); e == nil {
		t.Fatal("global credential reused")
	}
	r := testConfig().Recipes[0]
	if e := ValidateParameters(r, map[string]any{"count": 2, "mode": "lint", "race": true}); e != nil {
		t.Fatal(e)
	}
	for _, p := range []map[string]any{{"count": 2.5, "mode": "lint", "race": true}, {"count": 9, "mode": "lint", "race": true}, {"count": 2, "mode": "unapproved-program", "race": true}, {"count": 2, "mode": "lint", "race": "true"}, {"count": 2, "mode": "lint", "race": true, "command": "unapproved"}} {
		if ValidateParameters(r, p) == nil {
			t.Fatal("invalid typed parameters accepted")
		}
	}
}

func TestRotationCannotReuseControlCredential(t *testing.T) {
	control := strings.Repeat("c", 32)
	resolved := strings.Repeat("w", 32)
	a, err := New(testConfig(), control, func(string) string { return resolved })
	if err != nil {
		t.Fatal(err)
	}
	resolved = control
	if _, err = a.Authenticate(control, time.Now()); err == nil {
		t.Fatal("rotating worker secret to control credential granted worker authority")
	}
	if !a.IsWorkerToken(control) {
		t.Fatal("colliding credential must remain rejected on ordinary routes")
	}
	resolved = strings.Repeat("n", 32)
	if _, err = a.Authenticate(resolved, time.Now()); err != nil {
		t.Fatal("independent rotation did not recover", err)
	}
}

func TestRotationCannotMakeWorkerIdentityAmbiguous(t *testing.T) {
	c := testConfig()
	second := c.Credentials[0]
	second.ID = "runner-two"
	second.Principal = "worker/two"
	second.TokenEnv = "WORKER_FIXTURE_TWO"
	second.ProductIDs = []string{"privileged-product"}
	c.Credentials = append(c.Credentials, second)
	firstToken, secondToken := strings.Repeat("a", 32), strings.Repeat("b", 32)
	a, err := New(c, "control", func(name string) string {
		if name == "WORKER_FIXTURE_TWO" {
			return secondToken
		}
		return firstToken
	})
	if err != nil {
		t.Fatal(err)
	}
	secondToken = firstToken
	if _, err = a.Authenticate(firstToken, time.Now()); err == nil {
		t.Fatal("ambiguous rotated token selected an arbitrary worker")
	}
	if !a.IsWorkerToken(firstToken) {
		t.Fatal("ambiguous token must remain rejected on ordinary routes")
	}
	secondToken = strings.Repeat("d", 32)
	for token, principal := range map[string]string{firstToken: "worker/test", secondToken: "worker/two"} {
		p, err := a.Authenticate(token, time.Now())
		if err != nil || p.ID() != principal {
			t.Fatal("independent credentials did not recover after collision", err)
		}
	}
}

func TestRotationRejectsCollisionsWithInactiveCredentials(t *testing.T) {
	for _, inactive := range []string{"revoked", "expired"} {
		t.Run(inactive, func(t *testing.T) {
			c := testConfig()
			other := c.Credentials[0]
			other.ID = "inactive"
			other.Principal = "worker/inactive"
			other.TokenEnv = "WORKER_FIXTURE_INACTIVE"
			if inactive == "revoked" {
				other.Revoked = true
			} else {
				other.ExpiresAt = time.Now().Add(-time.Hour)
			}
			// Active credential comes last: a last-match implementation would
			// incorrectly accept it despite the inactive credential collision.
			c.Credentials = append([]Credential{other}, c.Credentials...)
			activeToken, otherToken := strings.Repeat("a", 32), strings.Repeat("b", 32)
			a, err := New(c, "control", func(name string) string {
				if name == "WORKER_FIXTURE_INACTIVE" {
					return otherToken
				}
				return activeToken
			})
			if err != nil {
				t.Fatal(err)
			}
			otherToken = activeToken
			if _, err = a.Authenticate(activeToken, time.Now()); err == nil {
				t.Fatal("collision with inactive credential accepted")
			}
		})
	}
}
