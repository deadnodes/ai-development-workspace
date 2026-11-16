package buildconfig_test

import (
	"encoding/json"
	"os"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/imagesigning/signingtest"
	"strings"
	"testing"
)

func TestSigningValidationStrict(t *testing.T) {
	f := signingtest.New(t)
	raw, e := os.ReadFile("../../examples/forgejo-builds.json")
	if e != nil {
		t.Fatal(e)
	}
	c, e := buildconfig.Decode(strings.NewReader(string(raw)))
	if e != nil {
		t.Fatal(e)
	}
	c.Signing = f.Config()
	c.Signing.RegistryCredentials = map[string]string{}
	for _, r := range c.Registries {
		c.Signing.RegistryCredentials[r.ID] = "registry.json"
	}
	valid, _ := json.Marshal(c)
	if _, e = buildconfig.Decode(strings.NewReader(string(valid))); e != nil {
		t.Fatal(e)
	}
	cases := []func(*buildconfig.Config){func(c *buildconfig.Config) { c.Signing.Keys[0].Auth.Method = "token" }, func(c *buildconfig.Config) { c.Signing.Keys[0].InsecureHTTP = false }, func(c *buildconfig.Config) { c.Signing.Keys[0].Auth.TokenFile = "relative" }, func(c *buildconfig.Config) { c.Signing.Keys[0].PublicKeyPEM = "invalid" }, func(c *buildconfig.Config) { c.Signing.Keys = append(c.Signing.Keys, c.Signing.Keys[0]) }, func(c *buildconfig.Config) { c.Signing.Rules[0].KeyID = "missing" }, func(c *buildconfig.Config) { c.Signing.DeployKeys = map[string][]string{"unknown": {"dev"}} }, func(c *buildconfig.Config) {
		for id := range c.Signing.RegistryCredentials {
			c.Signing.RegistryCredentials[id] = "../secret"
		}
	}}
	for i, mutate := range cases {
		var v buildconfig.Config
		_ = json.Unmarshal(valid, &v)
		mutate(&v)
		if e = v.Validate(); e == nil {
			t.Fatalf("invalid config %d accepted", i)
		}
	}
	unknown := strings.Replace(string(valid), `"builder_id":`, `"unknown":true,"builder_id":`, 1)
	if _, e = buildconfig.Decode(strings.NewReader(unknown)); e == nil {
		t.Fatal("unknown signing field accepted")
	}
}
