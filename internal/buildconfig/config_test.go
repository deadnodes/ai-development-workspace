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

func deliveryConfig() Config {
	c := valid()
	c.GitOps = []GitOps{{ID: "flux", Kind: "github", APIURL: "https://api.github.com", AppID: 4967151, InstallationID: 162235198, PrivateKeyRef: "github-app.pem", Repository: "deadnodes/flux", Branch: "main"}}
	c.Environments = []Environment{{ID: "dev", GitOpsID: "flux", AllowAutoMerge: true}}
	c.Profiles[0].RequiredStatusContexts = []string{"verify"}
	c.Profiles[0].Deployments = map[string]DeploymentTarget{"dev": {Files: []DeploymentFile{{Path: "clusters/dev/app.yaml", ImageID: "app", ImageRepository: "localhost:5000/o/r", TagFormat: "{tag}@{digest}", MinMatches: 1}}}}
	return c
}
func TestDeploymentAuthority(t *testing.T) {
	c := deliveryConfig()
	raw, _ := json.Marshal(c)
	if _, e := Decode(strings.NewReader(string(raw))); e != nil {
		t.Fatal(e)
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Environments[0].ID = "prod" }, func(c *Config) { c.Environments[0].ID = "production" }, func(c *Config) { c.Environments[0].AllowAutoMerge = false }, func(c *Config) { c.Environments[0].GitOpsID = "missing" }, func(c *Config) { c.GitOps[0].PrivateKeyRef = "../key" }, func(c *Config) { c.GitOps[0].Branch = "prod" }, func(c *Config) { c.GitOps[0].AppID = 0 }, func(c *Config) { c.GitOps[0].APIURL = "http://api.github.com" }, func(c *Config) { c.Profiles[0].Deployments["prod"] = c.Profiles[0].Deployments["dev"] }, func(c *Config) { v := c.Profiles[0].Deployments["dev"]; v.Files[0].Path = "../escape.yaml" }, func(c *Config) { v := c.Profiles[0].Deployments["dev"]; v.Files[0].ImageID = "missing" }, func(c *Config) { v := c.Profiles[0].Deployments["dev"]; v.Files[0].ImageRepository = "other/o/r" }, func(c *Config) { v := c.Profiles[0].Deployments["dev"]; v.Files[0].MinMatches = 0 }, func(c *Config) { v := c.Profiles[0].Deployments["dev"]; v.Files[0].TagFormat = "latest" },
	} {
		c = deliveryConfig()
		mutate(&c)
		if e := c.Validate(); e == nil {
			t.Fatal("invalid delivery config accepted")
		}
	}
	for _, changed := range []string{strings.Replace(string(raw), `"allow_auto_merge":true`, `"allow_auto_merge":true,"unknown":true`, 1), strings.Replace(string(raw), `"min_matches":1`, `"min_matches":1,"unknown":true`, 1)} {
		if _, e := Decode(strings.NewReader(changed)); e == nil {
			t.Fatal("unknown nested field accepted")
		}
	}
}

func TestDeploymentFormats(t *testing.T) {
	for _, format := range []string{"", "helm_values", "container_image", "unknown"} {
		c := deliveryConfig()
		f := &c.Profiles[0].Deployments["dev"].Files[0]
		f.Format = format
		if format == "container_image" {
			f.TagFormat = "ignored"
		}
		raw, _ := json.Marshal(c)
		_, err := Decode(strings.NewReader(string(raw)))
		if (err != nil) != (format == "unknown") {
			t.Fatalf("format %q: %v", format, err)
		}
	}
}
