package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/imagesigning"
	"releasecontrol/internal/providers/forgejo"
	"slices"
	"sort"
	"strings"
	"time"
)

func signingRepository(p buildconfig.Profile, imageID string) string {
	for _, i := range p.Images {
		if i.ID == imageID {
			return i.Repository
		}
	}
	return ""
}
func chosenTag(b domain.Build) string {
	if len(b.Images) == 0 {
		return ""
	}
	ref := strings.Split(b.Images[0].ImageRef, "@")[0]
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon > slash {
		return ref[colon+1:]
	}
	return ""
}
func signatureDone(entries []domain.BuildSignature, image, key string) bool {
	for _, v := range entries {
		if v.ImageID == image && v.KeyID == key && (v.Status == "SIGNED" || v.Status == "SKIPPED" || v.FailureCode == "SIGNING_KEY_MISMATCH") {
			return true
		}
	}
	return false
}
func signatureStatus(entries []domain.BuildSignature, pending bool) string {
	if pending {
		return "PENDING"
	}
	signed, failed := 0, 0
	for _, v := range entries {
		if v.Status == "SIGNED" {
			signed++
		}
		if v.Status == "FAILED" {
			failed++
		}
	}
	if signed > 0 && signed < len(entries) {
		return "PARTIAL"
	}
	if failed > 0 {
		return "FAILED"
	}
	if signed > 0 {
		return "SIGNED"
	}
	return "SKIPPED"
}
func signingEntry(image domain.BuildImage, k buildconfig.SigningKey) domain.BuildSignature {
	return domain.BuildSignature{ImageID: image.ID, ImageRef: image.ImageRef, Digest: image.Digest, KeyID: k.ID, KeyFingerprint: k.Fingerprint(), SignatureTag: imagesigning.Tag(image.Digest, "sig"), Attestations: []string{}}
}
func signingFailure(entry *domain.BuildSignature, err error) {
	entry.Status = "FAILED"
	entry.FailureCode = "SIGNING_UNAVAILABLE"
	if errors.Is(err, imagesigning.ErrKeyMismatch) {
		entry.FailureCode = "SIGNING_KEY_MISMATCH"
	}
	entry.LastError = entry.FailureCode
}
func putSignature(entries []domain.BuildSignature, v domain.BuildSignature) []domain.BuildSignature {
	for i, x := range entries {
		if x.ImageID == v.ImageID && x.KeyID == v.KeyID {
			entries[i] = v
			return entries
		}
	}
	return append(entries, v)
}
func signatureEvent(st *domain.State, meta domain.Meta, entry domain.BuildSignature) {
	action := "build.signing_failed"
	if entry.Status == "SIGNED" {
		action = "build.signed"
	}
	now := time.Now().UTC()
	st.Events = append(st.Events, domain.Event{ID: id(), Action: action, Actor: meta.Actor, At: now, EntityID: meta.ID, ProductID: meta.ProductID, Data: domain.Command{Action: action, ID: meta.ID, ProductID: meta.ProductID, Actor: meta.Actor, Data: map[string]any{"signature": entry}}})
}
func nextSigning(attempt int) time.Time {
	if attempt > 7 {
		attempt = 7
	}
	delay := 15 * (1 << attempt)
	if delay > 1800 {
		delay = 1800
	}
	return time.Now().UTC().Add(time.Duration(delay) * time.Second)
}
func cloneSigning[T any](v T) T {
	b, _ := json.Marshal(v)
	var out T
	_ = json.Unmarshal(b, &out)
	return out
}
func (s *Service) signingCI(ctx context.Context, b domain.Build, p buildconfig.Profile) (bool, bool, map[string]string) {
	source, ok := s.builds.provider.(interface {
		CommitStatus(context.Context, buildconfig.Connection, string, string) ( /* shared provider response */ forgejo.CommitStatus, error)
	})
	if !ok {
		return false, false, map[string]string{}
	}
	v, e := source.CommitStatus(ctx, s.builds.config.Connection(p.ConnectionID), p.Repository, b.SourceSHA)
	if e != nil {
		return false, false, map[string]string{}
	}
	return ciVerdict(trustedCIStatus(v, p), p.RequiredStatusContexts)
}
func (s *Service) buildPredicate(b domain.Build, p buildconfig.Profile, contexts map[string]string, imageID string) imagesigning.Predicate {
	if contexts == nil {
		contexts = map[string]string{}
	}
	cn := s.builds.config.Connection(p.ConnectionID)
	branch := b.Branch
	if branch == "" {
		branch = p.DefaultBranch
	}
	invocation := b.RunURL
	if invocation == "" {
		invocation = fmt.Sprint(b.ProviderRunID)
	}
	start := b.StartedAt
	if start == nil {
		start = &b.QueuedAt
	}
	byproducts := []any{map[string]any{"name": "required_status_contexts", "annotations": map[string]any{"required": p.RequiredStatusContexts, "observed": contexts}}}
	if p.RequireSecurityGate {
		for _, sec := range b.Security {
			if sec.ImageID == imageID {
				byproducts = append(byproducts, map[string]any{"name": "security_scan", "digest": map[string]string{"sha256": strings.TrimPrefix(sec.SummaryDigest, "sha256:")}, "annotations": map[string]any{"schema": "deadnodes.security-gate/v1", "verdict": "PASS", "summary_digest": sec.SummaryDigest, "sbom_digest": sec.SBOMDigest, "gitleaks_findings": 0, "critical": sec.Critical, "high_fixable": sec.HighFixable, "blocking": 0}})
				break
			}
		}
	}
	return imagesigning.Predicate{Type: imagesigning.ProvenanceType, Body: map[string]any{"buildDefinition": map[string]any{"buildType": "https://deadnodes.dev/rcp/forgejo-build/v1", "externalParameters": map[string]string{"repository": p.Repository, "workflow": p.Workflow, "dispatch_ref": p.DispatchRef, "source_sha": b.SourceSHA, "tag": chosenTag(b)}, "internalParameters": map[string]string{"profile_id": p.ID, "build_id": b.ID}, "resolvedDependencies": []any{map[string]any{"uri": "git+" + strings.TrimRight(cn.APIURL, "/") + "/" + p.Repository + "@refs/heads/" + branch, "digest": map[string]string{"gitCommit": b.SourceSHA}}}}, "runDetails": map[string]any{"builder": map[string]string{"id": s.builds.config.Signing.BuilderID}, "metadata": map[string]any{"invocationId": invocation, "startedOn": start, "finishedOn": b.FinishedAt}, "byproducts": byproducts}}}
}

// SignBuildsOnce admits only durable PENDING records, never historical unsigned successes.
func (s *Service) SignBuildsOnce(ctx context.Context) error {
	a := s.builds
	if a == nil {
		return nil
	}
	if err := s.observeBuildSecurity(ctx); err != nil {
		return err
	}
	if a.config.Signing == nil {
		return nil
	}
	if !a.signingMu.TryLock() {
		return nil
	}
	defer a.signingMu.Unlock()
	snapshot, e := s.buildSnapshot(ctx)
	if e != nil {
		return e
	}
	for _, b := range snapshot.Builds {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if b.Status != "SUCCEEDED" || b.SigningStatus != "PENDING" || b.RestoreQuarantined || time.Now().Before(b.SigningNextAttemptAt) {
			continue
		}
		p, ok := a.config.Profile(b.ProfileID)
		if !ok || p.ProductID != b.ProductID || !a.buildAuthorityMatches(p.ID, b.AuthorityHash) {
			continue
		}
		if p.RequireSecurityGate && (b.SecurityStatus == "" || b.SecurityStatus == "PENDING") {
			continue
		}
		next := copyBuild(b)
		next.SigningAttempts++
		pending := false
		events := []domain.BuildSignature{}
		contexts := map[string]string(nil)
		for _, k := range a.config.Signing.Keys {
			eligible, needCI := false, false
			for _, r := range a.config.Signing.Rules {
				if r.KeyID != k.ID {
					continue
				}
				matches := false
				for _, prefix := range r.TagPrefixes {
					matches = matches || strings.HasPrefix(chosenTag(b), prefix)
				}
				if r.RequireDefaultBranch {
					matches = matches && strings.HasPrefix(chosenTag(b), "main-") && (b.Branch == "" || b.Branch == p.DefaultBranch)
				}
				if matches {
					eligible = true
					needCI = needCI || r.RequireStatusContexts
				}
			}
			if !eligible {
				continue
			}
			green, failed := true, false
			if needCI {
				green, failed, contexts = s.signingCI(ctx, b, p)
			}
			for _, image := range b.Images {
				if signatureDone(next.Signatures, image.ID, k.ID) {
					continue
				}
				entry := signingEntry(image, k)
				if p.RequireSecurityGate && (b.SecurityStatus == "FAIL" || b.SecurityStatus == "ERROR" || b.SecurityStatus == "MISSING") {
					entry.Status = "SKIPPED"
					if b.SecurityStatus == "MISSING" {
						entry.FailureCode = "SECURITY_GATE_MISSING"
					} else {
						entry.FailureCode = "SECURITY_GATE_FAILED"
					}
				} else if failed {
					entry.Status = "SKIPPED"
					entry.FailureCode = "CI_NOT_GREEN"
				} else if !green {
					pending = true
					continue
				} else {
					version, err := a.signer.Publish(ctx, a.config.Registry(p.RegistryID), signingRepository(p, image.ID), a.config.Signing.RegistryCredentials[p.RegistryID], k, image.Digest, b.ID, b.SourceSHA, s.buildPredicate(b, p, contexts, image.ID))
					entry.KeyVersion = version
					if err != nil {
						signingFailure(&entry, err)
						if !errors.Is(err, imagesigning.ErrKeyMismatch) && time.Since(b.SigningStartedAt) < 30*time.Minute {
							pending = true
						}
					} else {
						now := time.Now().UTC()
						entry.Status = "SIGNED"
						entry.SignedAt = &now
						entry.Attestations = []string{imagesigning.ProvenanceType}
					}
				}
				next.Signatures = putSignature(next.Signatures, entry)
				events = append(events, entry)
			}
		}
		next.SigningStatus = signatureStatus(next.Signatures, pending)
		next.SigningNextAttemptAt = nextSigning(next.SigningAttempts)
		next.UpdatedAt = time.Now().UTC()
		if e = s.store.Update(ctx, func(st *domain.State) error {
			for i, v := range st.Builds {
				if v.ID == b.ID && v.UpdatedAt.Equal(b.UpdatedAt) {
					st.Builds[i] = next
					for _, entry := range events {
						signatureEvent(st, b.Meta, entry)
					}
					return nil
				}
			}
			return invalid("build signing changed")
		}); e != nil {
			return e
		}
	}
	return s.signArtifactsOnce(ctx)
}
func (s *Service) deploymentSignatures(ctx context.Context, d domain.Deployment, p buildconfig.Profile) bool {
	a := s.builds
	if a == nil || a.config.Signing == nil {
		return false
	}
	var b domain.Build
	if s.view(ctx, func(st *domain.State) error {
		for _, v := range st.Builds {
			if v.ID == d.BuildID && v.ProductID == d.ProductID {
				b = copyBuild(v)
				return nil
			}
		}
		return ErrNotFound
	}) != nil {
		return false
	}
	if len(d.Images) == 0 {
		return false
	}
	for _, image := range d.Images {
		verified := false
		for _, entry := range b.Signatures {
			if entry.ImageID != image.ID || entry.Digest != image.Digest || entry.Status != "SIGNED" || !slices.Contains(a.config.Signing.DeployKeys[d.EnvironmentID], entry.KeyID) {
				continue
			}
			k, ok := a.config.Signing.Key(entry.KeyID)
			if ok && entry.KeyFingerprint == k.Fingerprint() && a.signer.Verify(ctx, a.config.Registry(p.RegistryID), signingRepository(p, image.ID), a.config.Signing.RegistryCredentials[p.RegistryID], image.Digest, k) {
				verified = true
				break
			}
		}
		if !verified {
			return false
		}
	}
	return true
}
func artifactRef(reg buildconfig.Registry, ref string) (string, string, bool) {
	u, e := url.Parse(reg.Origin)
	parts := strings.Split(ref, "@")
	if e != nil || len(parts) != 2 || !strings.HasPrefix(parts[0], u.Host+"/") || !imagesigning.DigestPattern.MatchString(parts[1]) {
		return "", "", false
	}
	repo := strings.TrimPrefix(parts[0], u.Host+"/")
	if strings.ContainsAny(repo, ":?#%\\") || strings.Contains(repo, "..") || strings.Contains(repo, "//") || strings.HasSuffix(repo, "/") {
		return "", "", false
	}
	return repo, parts[1], true
}
func (s *Service) SignArtifact(ctx context.Context, in domain.SignArtifactRequest) (domain.ArtifactSignature, error) {
	var out domain.ArtifactSignature
	if in.ProductID == "" {
		return out, invalid("product_id required")
	}
	if e := authorize(ctx, "write", in.ProductID); e != nil {
		return out, e
	}
	a := s.builds
	if a == nil || a.config.Signing == nil {
		return out, invalid("signing disabled")
	}
	p, ok := a.config.Profile(in.ProfileID)
	if !ok || p.ProductID != in.ProductID {
		return out, invalid("profile not configured for Product")
	}
	in.Reason = strings.TrimSpace(in.Reason)
	in.KeyIDs = slices.Clone(in.KeyIDs)
	sort.Strings(in.KeyIDs)
	if len(in.RequestKey) == 0 || len(in.RequestKey) > 128 || len(in.Reason) < 8 || len(in.Reason) > 500 || len(in.KeyIDs) == 0 || len(in.KeyIDs) > 32 {
		return out, invalid("invalid signing request")
	}
	repo, digest, ok := artifactRef(a.config.Registry(p.RegistryID), in.ImageRef)
	configuredRepo := signingRepository(p, in.ImageID)
	if !ok || configuredRepo == "" {
		return out, invalid("invalid image reference")
	}
	allowed := repo == configuredRepo
	for _, prefix := range a.config.Signing.BackfillRepositories {
		allowed = allowed || strings.HasPrefix(repo, prefix)
	}
	if !allowed {
		return out, invalid("backfill repository denied")
	}
	for i, keyID := range in.KeyIDs {
		k, ok := a.config.Signing.Key(keyID)
		ruleKey := false
		for _, r := range a.config.Signing.Rules {
			ruleKey = ruleKey || r.KeyID == keyID
		}
		if !ok || (!k.AllowBackfill && !ruleKey) || (i > 0 && in.KeyIDs[i-1] == keyID) {
			return out, invalid("backfill key denied")
		}
	}
	replay := func(st *domain.State) (bool, error) {
		for _, v := range st.ArtifactSignatures {
			if v.ProductID == in.ProductID && v.Request.RequestKey == in.RequestKey {
				if !reflect.DeepEqual(v.Request, in) {
					return true, invalid("REQUEST_KEY_CONFLICT")
				}
				out = cloneSigning(v)
				return true, nil
			}
		}
		return false, nil
	}
	found := false
	e := s.view(ctx, func(st *domain.State) error { var e error; found, e = replay(st); return e })
	if e != nil || found {
		return out, e
	}
	revision, e := a.signer.Inspect(ctx, a.config.Registry(p.RegistryID), repo, a.config.Signing.RegistryCredentials[p.RegistryID], digest)
	if e != nil {
		return out, invalid("artifact observation unavailable")
	}
	e = s.store.Update(ctx, func(st *domain.State) error {
		if yes, e := replay(st); yes {
			return e
		}
		if !productExists(st, in.ProductID) {
			return missing("product", in.ProductID)
		}
		now := time.Now().UTC()
		out = domain.ArtifactSignature{Meta: domain.Meta{ID: id(), ProductID: in.ProductID, Actor: attributedActor(ctx, "agent"), CreatedAt: now, UpdatedAt: now}, Request: in, Status: "PENDING", StartedAt: now, Revision: revision}
		st.ArtifactSignatures = append(st.ArtifactSignatures, out)
		st.Events = append(st.Events, domain.Event{ID: id(), Action: "sign_artifact", Actor: out.Actor, At: now, EntityID: out.ID, ProductID: in.ProductID, Data: domain.Command{Action: "sign_artifact", ProductID: in.ProductID, ID: out.ID, Actor: out.Actor, Data: map[string]any{"request": in}}})
		return nil
	})
	return out, e
}
func (s *Service) signArtifactsOnce(ctx context.Context) error {
	a := s.builds
	var records []domain.ArtifactSignature
	if e := s.view(ctx, func(st *domain.State) error { records = cloneSigning(st.ArtifactSignatures); return nil }); e != nil {
		return e
	}
	for _, record := range records {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if record.Status != "PENDING" || time.Now().Before(record.NextAttemptAt) {
			continue
		}
		p, ok := a.config.Profile(record.Request.ProfileID)
		if !ok || p.ProductID != record.ProductID {
			continue
		}
		repo, digest, ok := artifactRef(a.config.Registry(p.RegistryID), record.Request.ImageRef)
		if !ok {
			continue
		}
		next := cloneSigning(record)
		next.Attempts++
		pending := false
		events := []domain.BuildSignature{}
		for _, keyID := range record.Request.KeyIDs {
			if signatureDone(next.Signatures, record.Request.ImageID, keyID) {
				continue
			}
			k, ok := a.config.Signing.Key(keyID)
			allowed := repo == signingRepository(p, record.Request.ImageID)
			for _, prefix := range a.config.Signing.BackfillRepositories {
				allowed = allowed || strings.HasPrefix(repo, prefix)
			}
			ruleKey := false
			for _, rule := range a.config.Signing.Rules {
				ruleKey = ruleKey || rule.KeyID == keyID
			}
			if !ok || !allowed || (!k.AllowBackfill && !ruleKey) {
				entry := domain.BuildSignature{ImageID: record.Request.ImageID, ImageRef: record.Request.ImageRef, Digest: digest, KeyID: keyID, Status: "FAILED", FailureCode: "SIGNING_AUTHORITY_CHANGED", LastError: "SIGNING_AUTHORITY_CHANGED"}
				next.Signatures = putSignature(next.Signatures, entry)
				events = append(events, entry)
				continue
			}
			entry := signingEntry(domain.BuildImage{ID: record.Request.ImageID, ImageRef: record.Request.ImageRef, Digest: digest}, k)
			predicate := imagesigning.Predicate{Type: imagesigning.ProvenanceType, Body: map[string]any{"buildDefinition": map[string]any{"buildType": "https://deadnodes.dev/rcp/backfill/v1", "externalParameters": map[string]string{"reason": record.Request.Reason, "actor": record.Actor, "revision": record.Revision}}, "runDetails": map[string]any{"builder": map[string]string{"id": a.config.Signing.BuilderID}, "metadata": map[string]any{"invocationId": record.ID, "startedOn": record.StartedAt, "finishedOn": record.StartedAt}}}}
			version, err := a.signer.Publish(ctx, a.config.Registry(p.RegistryID), repo, a.config.Signing.RegistryCredentials[p.RegistryID], k, digest, record.ID, record.Revision, predicate)
			entry.KeyVersion = version
			if err != nil {
				signingFailure(&entry, err)
				pending = pending || (!errors.Is(err, imagesigning.ErrKeyMismatch) && time.Since(record.StartedAt) < 30*time.Minute)
			} else {
				now := time.Now().UTC()
				entry.Status = "SIGNED"
				entry.SignedAt = &now
				entry.Attestations = []string{imagesigning.ProvenanceType}
			}
			next.Signatures = putSignature(next.Signatures, entry)
			events = append(events, entry)
		}
		next.Status = signatureStatus(next.Signatures, pending)
		next.NextAttemptAt = nextSigning(next.Attempts)
		next.UpdatedAt = time.Now().UTC()
		if e := s.store.Update(ctx, func(st *domain.State) error {
			for i, v := range st.ArtifactSignatures {
				if v.ID == record.ID && v.UpdatedAt.Equal(record.UpdatedAt) {
					st.ArtifactSignatures[i] = next
					// Explicit re-signing also repairs matching Build evidence; provenance
					// remains append-only in the explicit request and audit journal.
					for j := range st.Builds {
						b := &st.Builds[j]
						if b.ProductID != record.ProductID || b.ProfileID != record.Request.ProfileID || b.Status != "SUCCEEDED" {
							continue
						}
						for _, image := range b.Images {
							if image.ID != record.Request.ImageID || image.Digest != digest {
								continue
							}
							for _, entry := range events {
								if entry.Status == "SIGNED" {
									b.Signatures = putSignature(b.Signatures, entry)
								}
							}
							if b.SigningStatus != "PENDING" {
								b.SigningStatus = signatureStatus(b.Signatures, false)
							}
							b.UpdatedAt = next.UpdatedAt
						}
					}
					for _, entry := range events {
						signatureEvent(st, record.Meta, entry)
					}
					return nil
				}
			}
			return invalid("signature changed")
		}); e != nil {
			return e
		}
	}
	return nil
}
func (s *Service) ListSignatures(ctx context.Context, in domain.ListSignaturesRequest) (domain.SignaturePage, error) {
	out := domain.SignaturePage{Items: []domain.ArtifactSignature{}}
	if in.ProductID == "" || in.Limit < 0 || in.Limit > 100 || (in.Digest != "" && !imagesigning.DigestPattern.MatchString(in.Digest)) {
		return out, invalid("invalid signature scope or bounds")
	}
	if e := authorize(ctx, "read", in.ProductID); e != nil {
		return out, e
	}
	if in.Limit == 0 {
		in.Limit = 25
	}
	after := ""
	type cursor struct{ Product, Digest, After string }
	if in.Cursor != "" {
		b, e := base64.RawURLEncoding.DecodeString(in.Cursor)
		var c cursor
		if len(in.Cursor) > 2048 || e != nil || json.Unmarshal(b, &c) != nil || c.Product != in.ProductID || c.Digest != in.Digest {
			return out, invalid("invalid signature cursor")
		}
		after = c.After
	}
	e := s.view(ctx, func(st *domain.State) error {
		if err := requireProductRead(ctx, st, in.ProductID); err != nil {
			return err
		}
		records := slices.Clone(st.ArtifactSignatures)
		sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
		for _, v := range records {
			if v.ProductID != in.ProductID || v.ID <= after || (in.Digest != "" && !strings.HasSuffix(v.Request.ImageRef, "@"+in.Digest)) {
				continue
			}
			if len(out.Items) == in.Limit {
				b, _ := json.Marshal(cursor{in.ProductID, in.Digest, out.Items[len(out.Items)-1].ID})
				out.NextCursor = base64.RawURLEncoding.EncodeToString(b)
				break
			}
			out.Items = append(out.Items, cloneSigning(v))
		}
		return nil
	})
	return out, e
}
