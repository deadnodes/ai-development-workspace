// Package auth provides an isolated, encrypted authentication boundary. Auth
// records are deliberately absent from domain.State and its backup/export APIs.
package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var ErrForbidden = errors.New("forbidden")

type Principal struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	Role        string   `json:"role"`
	Products    []string `json:"products"`
	Permissions []string `json:"permissions"`
}
type principalKey struct{}

func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

type Config struct {
	Mode, PublicURL, IssuerURL, ClientID, ClientSecret string
	RolesClaim                                         string
	AllowedRoles, AdminRoles, Products                 []string
	EncryptionKey                                      []byte
	DatabaseURL, FilePath, BootstrapToken              string
	SessionTTL                                         time.Duration
}

func ConfigFromEnv() (Config, error) {
	c := Config{Mode: os.Getenv("RCP_AUTH_MODE"), PublicURL: os.Getenv("RCP_PUBLIC_URL"), IssuerURL: os.Getenv("RCP_OIDC_ISSUER_URL"), ClientID: os.Getenv("RCP_OIDC_CLIENT_ID"), ClientSecret: os.Getenv("RCP_OIDC_CLIENT_SECRET"), RolesClaim: os.Getenv("RCP_OIDC_ROLES_CLAIM"), AllowedRoles: csv(os.Getenv("RCP_OIDC_ALLOWED_ROLES")), AdminRoles: csv(os.Getenv("RCP_OIDC_ADMIN_ROLES")), Products: csv(os.Getenv("RCP_AUTH_PRODUCTS")), DatabaseURL: os.Getenv("DATABASE_URL"), FilePath: os.Getenv("RCP_AUTH_FILE"), BootstrapToken: os.Getenv("RCP_AUTH_BOOTSTRAP_TOKEN"), SessionTTL: 8 * time.Hour}
	if c.Mode == "" {
		c.Mode = "local"
		if c.IssuerURL != "" || c.ClientID != "" || c.PublicURL != "" {
			return c, errors.New("RCP_AUTH_MODE=oidc must be explicit when OIDC/public URL is configured")
		}
	}
	if key := os.Getenv("RCP_AUTH_ENCRYPTION_KEY"); key != "" {
		var err error
		c.EncryptionKey, err = base64.StdEncoding.DecodeString(key)
		if err != nil {
			return c, errors.New("RCP_AUTH_ENCRYPTION_KEY must be base64")
		}
	}
	if c.RolesClaim == "" {
		c.RolesClaim = "realm_access.roles"
	}
	if len(c.Products) == 0 {
		c.Products = []string{"*"}
	}
	if value := os.Getenv("RCP_AUTH_SESSION_TTL"); value != "" {
		var err error
		c.SessionTTL, err = time.ParseDuration(value)
		if err != nil {
			return c, fmt.Errorf("RCP_AUTH_SESSION_TTL: %w", err)
		}
	}
	if c.Mode == "oidc" && c.DatabaseURL == "" && c.FilePath != "" {
		domainPath := os.Getenv("RCP_DATA_PATH")
		if domainPath == "" {
			dir, err := os.UserConfigDir()
			if err != nil {
				return c, err
			}
			domainPath = filepath.Join(dir, "release-control", "state.db")
		}
		privateAbs, err := filepath.Abs(c.FilePath)
		if err != nil {
			return c, err
		}
		domainAbs, err := filepath.Abs(domainPath)
		if err != nil {
			return c, err
		}
		privateInfo, privateErr := os.Stat(privateAbs)
		domainInfo, domainErr := os.Stat(domainAbs)
		if privateAbs == domainAbs || (privateErr == nil && domainErr == nil && os.SameFile(privateInfo, domainInfo)) {
			return c, errors.New("RCP_AUTH_FILE must differ from RCP_DATA_PATH")
		}
	}
	return c, c.validate()
}
func csv(s string) []string {
	var r []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			r = append(r, v)
		}
	}
	return r
}
func (c Config) validate() error {
	if c.Mode == "local" {
		return nil
	}
	if c.Mode != "oidc" {
		return errors.New("RCP_AUTH_MODE must be local or oidc")
	}
	for name, raw := range map[string]string{"RCP_PUBLIC_URL": c.PublicURL, "RCP_OIDC_ISSUER_URL": c.IssuerURL} {
		u, e := url.Parse(raw)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("%s must be an absolute HTTPS URL without credentials/query/fragment", name)
		}
		if name == "RCP_PUBLIC_URL" && u.Path != "" && u.Path != "/" {
			return errors.New("RCP_PUBLIC_URL must use the origin root")
		}
	}
	if c.ClientID == "" || len(c.EncryptionKey) != 32 || (len(c.AllowedRoles) == 0 && len(c.AdminRoles) == 0) {
		return errors.New("OIDC mode requires client ID, 32-byte encryption key and allowed/admin roles")
	}
	if c.DatabaseURL == "" && c.FilePath == "" {
		return errors.New("OIDC mode requires DATABASE_URL or a separate RCP_AUTH_FILE")
	}
	if c.SessionTTL < time.Minute || c.SessionTTL > 24*time.Hour {
		return errors.New("auth session TTL must be between 1m and 24h")
	}
	if c.BootstrapToken != "" && len(c.BootstrapToken) < 32 {
		return errors.New("bootstrap token must contain at least 32 random characters")
	}
	return nil
}
