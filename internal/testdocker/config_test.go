package testdocker

import "testing"

func TestDockerOverridesFailClosed(t *testing.T) {
	c, _ := parse("default", "linux/amd64")
	for _, tc := range []struct {
		host, context, config, platform string
		valid                           bool
	}{
		{valid: true}, {context: "default", config: "/home/fixture/.docker", platform: "linux/amd64", valid: true},
		{host: "unix:///var/run/docker.sock"}, {host: "tcp://production:2376"},
		{context: "production"}, {config: "/tmp/other-docker-config"}, {platform: "linux/arm64"},
	} {
		if err := validateOverrides(c, "/home/fixture", tc.host, tc.context, tc.config, tc.platform); (err == nil) != tc.valid {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
}

func TestFixtureConfiguration(t *testing.T) {
	for _, tc := range []struct {
		context, platform string
		valid             bool
	}{
		{"", "", true}, {"orbstack", "linux/arm64", true}, {"default", "linux/amd64", true},
		{"production", "linux/amd64", false}, {"tcp://localhost:2375", "linux/amd64", false},
		{" default", "linux/amd64", false}, {"default;docker", "linux/amd64", false},
		{"default", "linux/amd64 ", false}, {"default", "linux/386", false},
	} {
		c, err := parse(tc.context, tc.platform)
		if (err == nil) != tc.valid {
			t.Fatalf("parse(%q,%q): %v", tc.context, tc.platform, err)
		}
		if tc.context == "" && (c.Context != "orbstack" || c.Platform != "linux/arm64" || c.BuildKitImage != "moby/buildkit@sha256:04cf92d6e86fcfdc7f37b7b5098978081952c11e6ac16769ab1aad95035c2441") {
			t.Fatal("original defaults changed", c)
		}
	}
}

func TestOnlyExpectedLocalEndpoint(t *testing.T) {
	for _, name := range []string{"default", "orbstack"} {
		c, _ := parse(name, "linux/arm64")
		expected := "unix:///var/run/docker.sock"
		if name == "orbstack" {
			expected = "unix:///home/fixture/.orbstack/run/docker.sock"
		}
		for _, endpoint := range []string{expected, "tcp://localhost:2375", "tcp://production:2376", "ssh://production", "unix:///other/docker.sock", ""} {
			err := validateEndpoint(c, "/home/fixture", endpoint)
			if (err == nil) != (endpoint == expected) {
				t.Fatalf("%s endpoint %q: %v", name, endpoint, err)
			}
		}
	}
}

func TestNativeArchitectureRequired(t *testing.T) {
	for _, tc := range []struct {
		platform, daemon string
		valid            bool
	}{
		{"linux/arm64", "linux/aarch64", true}, {"linux/arm64", "linux/arm64", true},
		{"linux/amd64", "linux/x86_64", true}, {"linux/amd64", "linux/amd64", true},
		{"linux/amd64", "linux/aarch64", false}, {"linux/arm64", "linux/x86_64", false},
		{"linux/amd64", "windows/amd64", false}, {"linux/amd64", "", false},
	} {
		if err := validateNative(tc.platform, tc.daemon); (err == nil) != tc.valid {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
}
