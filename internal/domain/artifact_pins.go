package domain

import (
	"fmt"
	"net"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ArtifactPin is durable metastore intent. Release preserves the original key
// and request fingerprint; a retry can never reactivate a released pin.
type ArtifactPin struct {
	Meta
	ApplicationID   string     `json:"application_id"`
	ImageRepository string     `json:"image_repository"`
	Digest          string     `json:"digest"`
	Purpose         string     `json:"purpose"`
	RequestKey      string     `json:"request_key"`
	RequestHash     string     `json:"request_hash"`
	ReleasedAt      *time.Time `json:"released_at,omitempty"`
}

var retentionDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var artifactPinKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
var imagePath = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)

// CanonicalImageRepository requires an explicit registry. An unqualified image
// is not evidence of a registry; the retention evaluator has one separate,
// provenance-checked GitHub Actions default-registry path.
func CanonicalImageRepository(raw string) (string, error) {
	if raw == "" || len(raw) > 512 || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "@?#\\\x00\r\n\t") || strings.Contains(raw, "://") {
		return "", fmt.Errorf("explicit canonical image repository required")
	}
	parts := strings.SplitN(raw, "/", 2)
	if len(parts) != 2 || !imagePath.MatchString(parts[1]) {
		return "", fmt.Errorf("invalid image repository path")
	}
	host := parts[0]
	if !strings.ContainsAny(host, ".:") && host != "localhost" {
		return "", fmt.Errorf("explicit registry host required")
	}
	u, e := url.Parse("https://" + host)
	if e != nil || u.Hostname() == "" || u.User != nil || u.Path != "" {
		return "", fmt.Errorf("invalid image registry host")
	}
	name := strings.ToLower(u.Hostname())
	if net.ParseIP(name) == nil {
		for _, label := range strings.Split(name, ".") {
			if !regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`).MatchString(label) {
				return "", fmt.Errorf("invalid image registry hostname")
			}
		}
	}
	port := u.Port()
	if port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("invalid image registry port")
		}
		port = strconv.Itoa(n)
	}
	if port == "443" {
		port = ""
	}
	if port != "" {
		name = net.JoinHostPort(name, port)
	} else if strings.Contains(name, ":") {
		name = "[" + name + "]"
	}
	return name + "/" + parts[1], nil
}
func ArtifactPinRequestHash(product, app, repository, digest, purpose, key string) string {
	return checkHash(struct{ ProductID, ApplicationID, ImageRepository, Digest, Purpose, RequestKey string }{product, app, repository, digest, purpose, key})
}
func ValidArtifactPinRequestKey(key string) bool { return artifactPinKey.MatchString(key) }
func ValidateArtifactPins(st State) error {
	fail := func() error { return fmt.Errorf("invalid artifact pin identity, references or fingerprint") }
	ids := map[string]int{}
	v := reflect.ValueOf(st)
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if f.Kind() != reflect.Slice {
			continue
		}
		for n := 0; n < f.Len(); n++ {
			el := f.Index(n)
			if el.Kind() != reflect.Struct {
				continue
			}
			id := el.FieldByName("ID")
			if id.IsValid() && id.Kind() == reflect.String && id.String() != "" {
				ids[id.String()]++
			}
		}
	}
	keys := map[string]bool{}
	for _, p := range st.ArtifactPins {
		repo, e := CanonicalImageRepository(p.ImageRepository)
		if e != nil || repo != p.ImageRepository || !retentionDigest.MatchString(p.Digest) || !artifactPinKey.MatchString(p.RequestKey) || (p.Purpose != "manual" && p.Purpose != "rollback") || p.ID == "" || ids[p.ID] != 1 || p.ProductID == "" || p.FeatureID != "" || p.CreatedAt.IsZero() || p.UpdatedAt.Before(p.CreatedAt) || p.ReleasedAt != nil && (p.ReleasedAt.IsZero() || p.ReleasedAt.Before(p.CreatedAt) || !p.ReleasedAt.Equal(p.UpdatedAt)) {
			return fail()
		}
		if p.RequestHash != ArtifactPinRequestHash(p.ProductID, p.ApplicationID, p.ImageRepository, p.Digest, p.Purpose, p.RequestKey) {
			return fail()
		}
		releaseEvents := 0
		for _, event := range st.Events {
			if (event.Action != "release_artifact_pin" && event.Data.Action != "release_artifact_pin") || (event.EntityID != p.ID && event.Data.ID != p.ID) {
				continue
			}
			releaseEvents++
			app, ok := event.Data.Data["application_id"].(string)
			cmd := event.Data
			if p.ReleasedAt == nil || event.ID == "" || ids[event.ID] != 1 || event.Action != "release_artifact_pin" || cmd.Action != "release_artifact_pin" || event.EntityID != p.ID || cmd.ID != p.ID || event.ProductID != p.ProductID || cmd.ProductID != p.ProductID || event.FeatureID != "" || cmd.FeatureID != "" || cmd.IntegrationID != "" || cmd.GateID != "" || cmd.CheckID != "" || cmd.ResultID != "" || strings.TrimSpace(p.Actor) == "" || event.Actor != p.Actor || cmd.Actor != p.Actor || !ok || app != p.ApplicationID || len(cmd.Data) != 1 || !event.At.Equal(*p.ReleasedAt) {
				return fail()
			}
		}
		if p.ReleasedAt != nil && releaseEvents != 1 || p.ReleasedAt == nil && releaseEvents != 0 {
			return fail()
		}

		foundProduct, foundApp := false, false
		for _, v := range st.Products {
			foundProduct = foundProduct || v.ID == p.ProductID
		}
		for _, a := range st.Applications {
			foundApp = foundApp || a.ID == p.ApplicationID && a.ProductID == p.ProductID
		}
		if !foundProduct || !foundApp {
			return fail()
		}
		key := p.ProductID + "\x00" + p.RequestKey
		if keys[key] {
			return fail()
		}
		keys[key] = true
	}
	return nil
}
