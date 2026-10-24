package domain

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"releasecontrol/internal/delivery"
	"slices"
	"sort"
	"strings"
	"time"
)

var runtimeCommit = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func runtimeProofHash(p DevRuntimeProof) string   { p.SnapshotHash = ""; return checkHash(p) }
func runtimeResultHash(r DevRuntimeResult) string { r.Hash = ""; return checkHash(r) }
func ValidateDevRuntimeGitOps(c DevRuntimeComponent, g DevRuntimeGitOpsObservation, p delivery.GitOpsRevisionProof) error {
	if !retentionDigest.MatchString(c.AuthorityHash) || g.HeadSHA != p.HeadSHA || !runtimeCommit.MatchString(g.HeadSHA) || !runtimeCommit.MatchString(g.BlobSHA) || !retentionDigest.MatchString(g.ContentHash) || g.ImageRepository != c.ImageRepository || g.Digest != c.Digest || !retentionDigest.MatchString(g.Digest) || p.Repository != c.GitOps.Repository || p.Ref != c.GitOps.Ref || !slices.Contains(p.AncestorSHAs, c.GitOpsCommit) {
		return fmt.Errorf("runtime exact GitOps image and ancestry mismatch")
	}
	return nil
}
func ValidateDevRuntimeRoleAt(c DevRuntimeComponent, scope delivery.RuntimeVerificationScope, r delivery.RuntimeSnapshot, head string, now time.Time) error {

	if r.ProductID != scope.ProductID || r.EnvironmentID != scope.EnvironmentID || r.ApplicationID != scope.ApplicationID || r.Context != scope.Context || r.Namespace != scope.Namespace || r.Deployment != scope.Deployment || r.Container != scope.Container || r.ObservedAt.IsZero() || r.ObservedAt.After(now) || now.Sub(r.ObservedAt) > 5*time.Minute || len(r.Errors) != 0 || r.Generation <= 0 || r.ObservedGeneration != r.Generation || r.DesiredReplicas <= 0 || r.Health != "HEALTHY" || !ExactDevRuntimeImage(r.WorkloadImage, c.ImageRepository, c.Digest) || len(r.Pods) != r.DesiredReplicas || len(r.Pods) > 512 || r.Flux == nil || r.Flux.Status != "READY" || r.Flux.Error != "" || r.Flux.Revision != head && r.Flux.Revision != c.GitOps.Ref+"@sha1:"+head {
		return fmt.Errorf("runtime complete healthy exact role and Flux revision required")
	}
	seen := map[string]bool{}
	names := map[string]bool{}
	for _, pod := range r.Pods {
		if pod.Name == "" || len(pod.Name) > 256 || pod.UID == "" || len(pod.UID) > 256 || seen[pod.UID] || names[pod.Name] || pod.Phase != "Running" || !pod.Ready || !ExactDevRuntimeImage(pod.Image, c.ImageRepository, c.Digest) || !ValidDevRuntimeImageID(pod.ImageID, c.ImageRepository, c.Digest) {
			return fmt.Errorf("runtime all unique Running Ready exact immutable pods required")
		}
		seen[pod.UID] = true
		names[pod.Name] = true
	}
	return nil
}
func ValidDevRuntimeImageID(v, repository, digest string) bool {
	// CRI commonly exposes just its immutable content digest, or a pullable ref.
	if v == digest || v == "containerd://"+digest || v == "docker://"+digest || v == "cri-o://"+digest {
		return true
	}
	return ExactDevRuntimeImage(strings.TrimPrefix(v, "docker-pullable://"), repository, digest)
}
func ValidateDevRuntimeResultAt(p DevRuntimeProof, r DevRuntimeResult, now time.Time) error {
	if len(p.Components) == 0 || len(p.Components) > 16 || p.Generation == 0 || p.SnapshotHash != runtimeProofHash(p) || r.Outcome != "VERIFIED" || r.Reason != "EXACT_CONFIGURED_RUNTIME_MATCH" || r.ExpectedHash != p.SnapshotHash || len(r.Components) != len(p.Components) || r.Hash == "" || r.Hash != runtimeResultHash(r) || r.ObservedAt.IsZero() {
		return fmt.Errorf("runtime result proof mismatch")
	}
	pods := 0
	groups := map[string]string{}
	ancestors := map[string][]string{}
	for _, c := range p.Components {
		key := c.AuthorityHash + "\x00" + c.GitOps.Repository + "\x00" + c.GitOps.Ref
		if !slices.Contains(ancestors[key], c.GitOpsCommit) {
			ancestors[key] = append(ancestors[key], c.GitOpsCommit)
		}
	}
	for key := range ancestors {
		sort.Strings(ancestors[key])
	}
	for i, c := range p.Components {
		cr := r.Components[i]
		if cr.ApplicationID != c.ApplicationID || len(cr.Roles) != len(c.Scopes) {
			return fmt.Errorf("runtime result coverage mismatch")
		}
		key := c.AuthorityHash + "\x00" + c.GitOps.Repository + "\x00" + c.GitOps.Ref
		if prior := groups[key]; prior != "" && prior != cr.Revision.HeadSHA {
			return fmt.Errorf("runtime sibling GitOps head mismatch")
		}
		groups[key] = cr.Revision.HeadSHA
		if !reflect.DeepEqual(cr.Revision.AncestorSHAs, ancestors[key]) {
			return fmt.Errorf("runtime complete sibling ancestry required")
		}
		if err := ValidateDevRuntimeGitOps(c, cr.GitOps, cr.Revision); err != nil {
			return err
		}
		for j, scope := range c.Scopes {
			role := cr.Roles[j]
			if err := ValidateDevRuntimeRoleAt(c, scope, role, cr.Revision.HeadSHA, now); err != nil {
				return err
			}
			pods += len(role.Pods)
		}
	}
	b, err := json.Marshal(struct {
		Proof  DevRuntimeProof
		Result DevRuntimeResult
	}{p, r})
	if err != nil || len(b) > 256*1024 || pods > 512 {
		return fmt.Errorf("runtime encoded proof or pod bounds exceeded")
	}
	return nil
}
func ExactDevRuntimeImage(v, repository, digest string) bool {
	if !retentionDigest.MatchString(digest) || strings.Count(v, "@") != 1 || !strings.HasSuffix(v, "@"+digest) {
		return false
	}
	ref := strings.TrimSuffix(v, "@"+digest)
	if ref == repository {
		return true
	}
	if !strings.HasPrefix(ref, repository+":") {
		return false
	}
	tag := strings.TrimPrefix(ref, repository+":")
	if len(tag) == 0 || len(tag) > 128 {
		return false
	}
	for i, r := range tag {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || i > 0 && (r == '.' || r == '-')) {
			return false
		}
	}
	return true
}
