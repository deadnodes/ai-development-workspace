package workerrunner

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"releasecontrol/internal/sourcefetch"
)

func validateSourceStrategy(c Config) error {
	remote := c.Recipe.Execution.SourceFetch != nil
	access := c.SourceGitHub
	if !remote {
		if c.SourceMirror == "" || access != nil {
			return errors.New("exactly one explicit source strategy required")
		}
		return nil
	}
	if c.SourceMirror != "" || access == nil {
		return errors.New("remote source requires dedicated explicit access and no mirror")
	}
	if access.Anonymous {
		if access.TokenEnv != "" {
			return errors.New("anonymous source access cannot reference credentials")
		}
		return nil
	}
	if !regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`).MatchString(access.TokenEnv) || access.TokenEnv == c.TokenEnv || access.TokenEnv == "RC_TOKEN" {
		return errors.New("source credential reference must be separate")
	}
	return nil
}

// Capture the active worker credential too: rotating its environment variable
// does not make that still-active credential eligible for source access.
func sourceCredential(c Config, activeWorkerToken string) func() (string, error) {
	activeControlToken := os.Getenv("RC_TOKEN")
	return func() (string, error) {
		if c.SourceGitHub == nil {
			return "", errors.New("source access unavailable")
		}
		if err := validateSourceStrategy(c); err != nil {
			return "", err
		}
		if c.SourceGitHub.Anonymous {
			return "", nil
		}
		token := os.Getenv(c.SourceGitHub.TokenEnv)
		if token == "" || len(token) > 4096 || strings.ContainsFunc(token, func(r rune) bool { return r <= 0x20 || r >= 0x7f }) || token == activeWorkerToken || token == activeControlToken || token == os.Getenv(c.TokenEnv) || token == os.Getenv("RC_TOKEN") {
			return "", errors.New("source credential rejected")
		}
		return token, nil
	}
}

func materializeSource(ctx context.Context, c Config, root, sourceSHA string, client *http.Client, credential func() (string, error)) (string, error) {
	execution := c.Recipe.Execution
	if execution.SourceFetch == nil {
		return materialize(ctx, root, c.SourceMirror, c.Recipe.RepositoryURL, sourceSHA, execution.SourceBytes)
	}
	destination := filepath.Join(root, "source")
	fetchCtx, cancel := context.WithTimeout(ctx, time.Duration(execution.SourceFetch.TimeoutSeconds)*time.Second)
	defer cancel()
	if err := sourcefetch.Materialize(fetchCtx, destination, c.Recipe.RepositoryURL, sourceSHA, *execution.SourceFetch, execution.SourceBytes, credential, client); err != nil {
		return "", err
	}
	// The attempt parent remains private0700. Only the completed tree gets the
	// same compiler-readable permissions as mirror snapshots; the container
	// mounts it readonly and receives no supervisor directory or credential.
	entries := 0
	if err := filepath.WalkDir(destination, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := fetchCtx.Err(); err != nil {
			return err
		}
		entries++
		if entries > execution.SourceFetch.MaxEntries+1 {
			return errors.New("source snapshot entry bound exceeded")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if info.Mode() != os.ModeDir|0700 {
				return errors.New("invalid source directory mode")
			}
			return os.Chmod(path, 0755)
		}
		if info.Mode() != 0400 && info.Mode() != 0500 {
			return errors.New("invalid source file")
		}
		mode := os.FileMode(0444)
		if info.Mode().Perm()&0100 != 0 {
			mode = 0555
		}
		return os.Chmod(path, mode)
	}); err != nil {
		return "", errors.New("source snapshot permissions rejected")
	}
	if fetchCtx.Err() != nil {
		return "", errors.New("source snapshot cancelled")
	}
	return destination, nil
}
