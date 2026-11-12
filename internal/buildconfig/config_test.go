package buildconfig

import (
	"encoding/json"
	"strings"
	"testing"
)

func valid() Config {
	return Config{Connections: []Connection{{ID: "f", Kind: "forgejo", APIURL: "https://git.example", Owner: "o", TokenRef: "bot"}}, Registries: []Registry{{ID: "r", Origin: "http://localhost:5000", InsecureHTTP: true}}, Pools: []Pool{{ID: "pool"}}, Profiles: []Profile{{ID: "app", ProductID: "p", ConnectionID: "f", Repository: "o/r", Workflow: "dev.yml", DefaultBranch: "main", PoolID: "pool", RegistryID: "r", ImageRepository: "o/r", TagTemplate: "forgejo-{sha8}", Targets: map[string]Target{"dev": {Branch: "main"}, "image": {AllowBranches: true}}}}}
}
func TestStrictAuthority(t *testing.T) {
	c := valid()
	b, _ := json.Marshal(c)
	out, e := Decode(strings.NewReader(string(b)))
	if e != nil || out.Pools[0].MaxInFlight != 1 || out.TimeoutSeconds != 7200 {
		t.Fatal(out, e)
	}
	for _, change := range []func(*Config){func(c *Config) { c.Connections[0].TokenRef = "../token" }, func(c *Config) { c.Registries[0].InsecureHTTP = false }, func(c *Config) { c.Profiles[0].PoolID = "missing" }, func(c *Config) { c.Profiles[0].Repository = "else/r" }, func(c *Config) { c.Profiles[0].Targets["dev"] = Target{AllowBranches: true} }, func(c *Config) { c.Profiles[0].TagTemplate = "latest" }, func(c *Config) { c.Connections = append(c.Connections, c.Connections[0]) }} {
		c = valid()
		change(&c)
		if c.Validate() == nil {
			t.Fatal("invalid authority accepted")
		}
	}
	for _, raw := range []string{`null`, `{"unknown":1}`, string(b) + `{}`, strings.Repeat("x", (1<<20)+1)} {
		if _, e := Decode(strings.NewReader(raw)); e == nil {
			t.Fatal("invalid JSON accepted")
		}
	}
}
