package buildconfig

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func valid() Config {
	return Config{Connections: []Connection{{ID: "f", Kind: "forgejo", APIURL: "https://git.example", Owner: "o", TokenRef: "bot"}}, Registries: []Registry{{ID: "r", Origin: "http://localhost:5000", InsecureHTTP: true}}, Pools: []Pool{{ID: "pool"}}, Profiles: []Profile{{ID: "app", ProductID: "p", ConnectionID: "f", Repository: "o/r", Workflow: "build.yml", DefaultBranch: "main", PoolID: "pool", RegistryID: "r", Images: []Image{{ID: "app", Repository: "o/r"}}, DispatchRef: "main", TagCandidates: []string{"main-{sha8}", "br-{sha8}"}}}}
}
func TestStrictAuthority(t *testing.T) {
	c := valid()
	b, _ := json.Marshal(c)
	out, e := Decode(strings.NewReader(string(b)))
	if e != nil || out.Pools[0].MaxInFlight != 1 || out.TimeoutSeconds != 7200 {
		t.Fatal(out, e)
	}
	for _, change := range []func(*Config){func(c *Config) { c.Connections[0].TokenRef = "../token" }, func(c *Config) { c.Registries[0].InsecureHTTP = false }, func(c *Config) { c.Profiles[0].PoolID = "missing" }, func(c *Config) { c.Profiles[0].Repository = "else/r" }, func(c *Config) { c.Profiles[0].DispatchRef = "bad ref" }, func(c *Config) { c.Profiles[0].TagCandidates = []string{"latest"} }, func(c *Config) { c.Connections = append(c.Connections, c.Connections[0]) }} {
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

func TestTagCandidatesContract(t *testing.T) {
	for _, templates := range [][]string{nil, {}, {"main-{unknown}"}, {"main-{sha8}-{other}"}, {"latest"}, {"{sha8}/bad"}} {
		c := valid()
		c.Profiles[0].TagCandidates = templates
		if c.Validate() == nil {
			t.Fatal(templates)
		}
	}
	c := valid()
	c.Profiles[0].TagCandidates = []string{"main-{sha8}", "br-{sha}"}
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	if ExpandTag("br-{sha8}-{sha}", strings.Repeat("a", 40)) != "br-aaaaaaaa-"+strings.Repeat("a", 40) {
		t.Fatal("placeholder expansion")
	}
	b, _ := json.Marshal(c)
	for _, field := range []string{`,"tag_template":"main-{sha8}"`, `,"targets":{}`} {
		raw := strings.Replace(string(b), `"dispatch_ref":"main"`, `"dispatch_ref":"main"`+field, 1)
		if _, e := Decode(strings.NewReader(raw)); e == nil {
			t.Fatal("obsolete field accepted")
		}
	}
}

func TestImagesAuthority(t *testing.T) {
	for _, images := range [][]Image{nil, {{ID: "api", Repository: "o/api"}, {ID: "api", Repository: "o/web"}}, {{ID: "api", Repository: "o/api"}, {ID: "web", Repository: "o/api"}}, {{ID: "bad id", Repository: "o/api"}}, {{ID: "api", Repository: "o/../api"}}} {
		c := valid()
		c.Profiles[0].Images = images
		if c.Validate() == nil {
			t.Fatal("invalid images accepted")
		}
	}
}

func TestExampleProfiles(t *testing.T) {
	raw, e := os.ReadFile("../../examples/forgejo-builds.json")
	if e != nil {
		t.Fatal(e)
	}
	c, e := Decode(strings.NewReader(string(raw)))
	if e != nil {
		t.Fatal(e)
	}
	for id, count := range map[string]int{"pp-front": 1, "pp-back": 1, "meteroid": 3} {
		p, ok := c.Profile(id)
		if !ok || len(p.Images) != count {
			t.Fatal("missing example profile images", id)
		}
	}
}

func TestBuildRunLookback(t *testing.T) {
	for _, seconds := range []int{-1, 0, 3600, 31536001} {
		c := valid()
		c.RunLookbackSeconds = seconds
		err := c.Validate()
		if seconds < 0 || seconds > 31536000 {
			if err == nil {
				t.Fatal(seconds)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		expected := seconds
		if expected == 0 {
			expected = 86400
		}
		if c.RunLookbackSeconds != expected {
			t.Fatal(c.RunLookbackSeconds)
		}
	}
}
