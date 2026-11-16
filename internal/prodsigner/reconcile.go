package prodsigner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/imagesigning"
	"sort"
	"strings"
)

const ApprovalType = "https://deadnodes.dev/attestations/prod-approval/v1"

type Summary struct {
	Revision      string `json:"revision"`
	Total         int    `json:"total"`
	AlreadySigned int    `json:"already_signed"`
	Signed        int    `json:"signed"`
	Unapproved    int    `json:"unapproved"`
	Errors        int    `json:"errors"`
}
type provenance struct {
	Type          string `json:"_type"`
	PredicateType string `json:"predicateType"`
	Subject       []struct {
		Name   string            `json:"name"`
		Digest map[string]string `json:"digest"`
	} `json:"subject"`
	Predicate struct {
		BuildDefinition struct {
			BuildType string `json:"buildType"`
			External  struct {
				Tag string `json:"tag"`
				SHA string `json:"source_sha"`
			} `json:"externalParameters"`
			Internal struct {
				BuildID string `json:"build_id"`
			} `json:"internalParameters"`
		} `json:"buildDefinition"`
		RunDetails struct {
			Builder struct {
				ID string `json:"id"`
			} `json:"builder"`
			Byproducts []struct {
				Name        string `json:"name"`
				Annotations struct {
					Required []string          `json:"required"`
					Observed map[string]string `json:"observed"`
				} `json:"annotations"`
			} `json:"byproducts"`
		} `json:"runDetails"`
	} `json:"predicate"`
}

func approved(b []byte, r *Ref, c Config) (string, string, bool) {
	var p provenance
	if json.Unmarshal(b, &p) != nil || p.Type != "https://in-toto.io/Statement/v0.1" || p.PredicateType != imagesigning.ProvenanceType {
		return "", "", false
	}
	subject := false
	for _, s := range p.Subject {
		subject = subject || s.Digest["sha256"] == strings.TrimPrefix(r.Digest, "sha256:")
	}
	d := p.Predicate.BuildDefinition
	builder := false
	for _, id := range c.BuilderIDs {
		builder = builder || id == p.Predicate.RunDetails.Builder.ID
	}
	if !subject || !builder || d.BuildType != "https://deadnodes.dev/rcp/forgejo-build/v1" || !strings.HasPrefix(d.External.Tag, "main-") || d.External.SHA == "" {
		return "", "", false
	}
	for _, b := range p.Predicate.RunDetails.Byproducts {
		if b.Name != "required_status_contexts" {
			continue
		}
		for _, name := range b.Annotations.Required {
			if b.Annotations.Observed[name] != "success" {
				return "", "", false
			}
		}
		for _, state := range b.Annotations.Observed {
			if state != "success" {
				return "", "", false
			}
		}
	}
	return d.Internal.BuildID, d.External.SHA, true
}
func registryRepo(c Config, repo string) string {
	keys := []string{}
	for p := range c.Registry.Rewrite {
		keys = append(keys, p)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, p := range keys {
		if strings.HasPrefix(repo, p) {
			return c.Registry.Rewrite[p] + strings.TrimPrefix(repo, p)
		}
	}
	_, rest, _ := strings.Cut(repo, "/")
	return rest
}
func Reconcile(ctx context.Context, c Config, source Source, client *imagesigning.Client, log io.Writer) (Summary, error) {
	var s Summary
	fail := func() (Summary, error) { s.Errors++; return s, imagesigning.ErrUnavailable }
	if c.Validate() != nil {
		return fail()
	}
	a, files, e := source.Load(ctx, c)
	s.Revision = a.Revision
	if e != nil {
		return fail()
	}
	refs, e := collect(c, files)
	if e != nil {
		return fail()
	}
	reg := buildconfig.Registry{Origin: c.Registry.Origin, InsecureHTTP: c.Registry.InsecureHTTP}
	credential := filepath.Base(c.Registry.CredentialsFile)
	keys := []string{}
	for k := range refs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// Work once per digest, retaining all Flux file paths and preferring explicit backfill.
	byDigest := map[string]*Ref{}
	for _, k := range keys {
		r := refs[k]
		if v := byDigest[r.Digest]; v != nil {
			v.Paths = append(v.Paths, r.Paths...)
			if r.Reason != "" {
				v.Reason = r.Reason
				v.ApprovedIn = r.ApprovedIn
				v.Image = r.Image
				v.Repo = r.Repo
			}
		} else {
			byDigest[r.Digest] = r
		}
	}
	digests := []string{}
	for d := range byDigest {
		digests = append(digests, d)
	}
	sort.Strings(digests)
	s.Total = len(digests)
	for _, digest := range digests {
		r := byDigest[digest]
		bodies, e := client.Evidence(ctx, reg, c.SignaturesRepository, credential, digest, "sig", c.ProdKey.PublicKeyPEM)
		if e != nil {
			s.Errors++
			continue
		}
		exists := false
		for _, b := range bodies { // Any pinned prod signature over this digest is sufficient.
			var v struct {
				Critical struct {
					Identity map[string]string `json:"identity"`
				} `json:"critical"`
			}
			if json.Unmarshal(b, &v) == nil && imagesigning.SignatureMatches(b, v.Critical.Identity["docker-reference"], digest) {
				exists = true
			}
		}
		if exists {
			s.AlreadySigned++
			continue
		}
		predicate := map[string]any{"flux_revision": a.Revision, "flux_artifact_digest": a.Digest, "file_paths": r.Paths, "image_ref": r.Image}
		buildID, sha := "", ""
		infrastructureFailure := false
		valid := r.Reason != ""
		if valid {
			predicate["backfill_reason"] = r.Reason
			predicate["approved_in"] = r.ApprovedIn
		} else {
			repo := registryRepo(c, r.Repo)
			sigs, e := client.Evidence(ctx, reg, repo, credential, digest, "sig", c.DevPublicKeyPEM)
			if e != nil {
				s.Errors++
				continue
			}
			signature := false
			for _, b := range sigs {
				var payload struct {
					Critical struct {
						Identity map[string]string `json:"identity"`
					} `json:"critical"`
				}
				if json.Unmarshal(b, &payload) == nil {
					signature = signature || imagesigning.SignatureMatches(b, payload.Critical.Identity["docker-reference"], digest)
				}
			}
			if signature {
				atts, e := client.Evidence(ctx, reg, repo, credential, digest, "att", c.DevPublicKeyPEM)
				if e != nil {
					s.Errors++
					continue
				}
				for _, b := range atts {
					var ok bool
					buildID, sha, ok = approved(b, r, c)
					if !ok {
						continue
					}
					revision, e := client.Inspect(ctx, reg, repo, credential, digest)
					if e != nil {
						s.Errors++
						infrastructureFailure = true
						break
					}
					if revision == sha {
						valid = true
						break
					}
				}
			}
			predicate["dev_build_id"] = buildID
			predicate["source_sha"] = sha
		}
		if infrastructureFailure {
			continue
		}
		if !valid {
			s.Unapproved++
			if log != nil {
				b, _ := json.Marshal(map[string]string{"unapproved": r.Image})
				fmt.Fprintln(log, string(b))
			}
			continue
		}
		_, e = client.PublishIdentity(ctx, reg, c.SignaturesRepository, credential, c.ProdKey, digest, buildID, sha, r.Repo, imagesigning.Predicate{Type: ApprovalType, Body: predicate})
		if e != nil {
			s.Errors++
		} else {
			s.Signed++
		}
	}
	if s.Errors > 0 {
		return s, imagesigning.ErrUnavailable
	}
	return s, nil
}
func WriteMetrics(filename string, s Summary) error {
	if filename == "" {
		return nil
	}
	f, e := os.CreateTemp(filepath.Dir(filename), ".prod-signer-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	_, e = fmt.Fprintf(f, "prod_signer_total %d\nprod_signer_already_signed %d\nprod_signer_signed %d\nprod_signer_unapproved %d\nprod_signer_errors %d\n", s.Total, s.AlreadySigned, s.Signed, s.Unapproved, s.Errors)
	if closeErr := f.Close(); e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	return os.Rename(f.Name(), filename)
}
