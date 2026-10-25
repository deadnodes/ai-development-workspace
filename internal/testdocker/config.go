// Package testdocker configures only explicitly opted-in, isolated Docker tests.
// It is not an operator or production worker configuration surface.
package testdocker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Config preserves the original OrbStack/ARM64 fixture defaults. Linux CI must
// select both its context and its native platform explicitly.
type Config struct {
	Context       string
	Platform      string
	BuildKitImage string
}

func parse(contextName, platform string) (Config, error) {
	if contextName == "" {
		contextName = "orbstack"
	}
	if platform == "" {
		platform = "linux/arm64"
	}
	if contextName != "orbstack" && contextName != "default" {
		return Config{}, fmt.Errorf("fixture Docker context must be orbstack or default")
	}
	c := Config{Context: contextName, Platform: platform}
	// Both manifests belong to the same v0.29.0-rootless image index. Keep
	// the original ARM64 reference unchanged; never substitute a moving tag.
	switch platform {
	case "linux/arm64":
		c.BuildKitImage = "moby/buildkit@sha256:04cf92d6e86fcfdc7f37b7b5098978081952c11e6ac16769ab1aad95035c2441"
	case "linux/amd64":
		c.BuildKitImage = "moby/buildkit@sha256:78b3b861a6c4d6f2235d1e0ca61d183ff371aa5bd4398051b24815a235b4cf45"
	default:
		return Config{}, fmt.Errorf("fixture platform must be linux/arm64 or linux/amd64")
	}
	return c, nil
}

func validateEndpoint(c Config, home, endpoint string) error {
	expected := "unix:///var/run/docker.sock"
	if c.Context == "orbstack" {
		expected = "unix://" + filepath.Join(home, ".orbstack", "run", "docker.sock")
	}
	if endpoint != expected {
		return fmt.Errorf("fixture context must use its expected local Unix socket")
	}
	return nil
}

func validateNative(platform, daemon string) error {
	switch daemon {
	case "linux/aarch64", "linux/arm64":
		daemon = "linux/arm64"
	case "linux/x86_64", "linux/amd64":
		daemon = "linux/amd64"
	}
	if daemon != platform {
		return fmt.Errorf("fixture requires native %s Docker engine, got %s", platform, daemon)
	}
	return nil
}

func validateOverrides(c Config, home, host, contextName, config, platform string) error {
	if host != "" || (contextName != "" && contextName != c.Context) ||
		(config != "" && config != filepath.Join(home, ".docker")) ||
		(platform != "" && platform != c.Platform) {
		return fmt.Errorf("fixture refuses conflicting Docker environment overrides")
	}
	return nil
}

// Load fails closed before any fixture creates resources. Context names alone
// are insufficient: inspect the endpoint and native daemon architecture too.
func Load(t testing.TB) Config {
	t.Helper()
	c, err := parse(os.Getenv("RCP_TEST_DOCKER_CONTEXT"), os.Getenv("RCP_TEST_DOCKER_PLATFORM"))
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateOverrides(c, home, os.Getenv("DOCKER_HOST"), os.Getenv("DOCKER_CONTEXT"), os.Getenv("DOCKER_CONFIG"), os.Getenv("DOCKER_DEFAULT_PLATFORM")); err != nil {
		t.Fatal(err)
	}
	docker := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker", append([]string{"--config", filepath.Join(home, ".docker"), "--context", c.Context}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("fixture Docker preflight failed: %v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	endpoint := docker("context", "inspect", c.Context, "--format", "{{.Endpoints.docker.Host}}")
	if err := validateEndpoint(c, home, endpoint); err != nil {
		t.Fatal(err)
	}
	if err := validateNative(c.Platform, docker("info", "--format", "{{.OSType}}/{{.Architecture}}")); err != nil {
		t.Fatal(err)
	}
	t.Logf("isolated Docker fixture: context=%s platform=%s", c.Context, c.Platform)
	return c
}
