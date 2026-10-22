package domain

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ArtifactRetention is advisory. No bytes or provenance records are deleted.
type ArtifactRetention struct {
	KeepLast     int  `json:"keep_last"`
	KeepCurrent  bool `json:"keep_current"`
	KeepPrevious int  `json:"keep_previous"`
}
type EffectiveRetentionPolicy struct {
	ApplicationID      string `json:"application_id"`
	ConfiguredKeepLast int    `json:"configured_keep_last"`
	EffectiveKeepLast  int    `json:"effective_keep_last"`
	KeepCurrent        bool   `json:"keep_current"`
	KeepPrevious       int    `json:"keep_previous"`
}
type RetentionImage struct {
	Identity        string     `json:"identity"`
	ImageRepository string     `json:"image_repository"`
	Digest          string     `json:"digest"`
	ApplicationIDs  []string   `json:"application_ids"`
	Profiles        []string   `json:"profiles"`
	ArtifactIDs     []string   `json:"artifact_ids"`
	Reasons         []string   `json:"reasons"`
	Blockers        []string   `json:"blockers"`
	Protected       bool       `json:"protected"`
	Availability    string     `json:"availability"`
	Freshness       string     `json:"freshness"`
	PublishedAt     *time.Time `json:"published_at,omitempty"`
}
type RetentionReport struct {
	Registry                []RegistryCoverageSummary  `json:"registry"`
	ProtectedObjects        []RegistryProtectedObject  `json:"protected_objects"`
	ProtectedEdges          []RegistryProtectedEdge    `json:"protected_edges"`
	GlobalInventoryComplete bool                       `json:"global_inventory_complete"`
	ProductID               string                     `json:"product_id"`
	EvaluatedAt             time.Time                  `json:"evaluated_at"`
	InputHash               string                     `json:"input_hash"`
	DeletionAllowed         bool                       `json:"deletion_allowed"`
	Policies                []EffectiveRetentionPolicy `json:"policies"`
	Images                  []RetentionImage           `json:"images"`
	Blockers                []string                   `json:"blockers"`
}

const retentionFreshness = 5 * time.Minute

func evidenceFreshness(at, now time.Time) string {
	if at.IsZero() || at.After(now) {
		return "UNKNOWN"
	}
	if now.Sub(at) <= retentionFreshness {
		return "FRESH"
	}
	return "STALE"
}
func sortedUnique(v []string) []string {
	sort.Strings(v)
	out := []string{}
	for _, s := range v {
		if len(out) == 0 || s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

type retentionCollector struct {
	st                   State
	product              string
	now                  time.Time
	images               map[string]*RetentionImage
	observed             map[string]time.Time
	availabilityEvidence map[string][]string
	policies             []EffectiveRetentionPolicy
	blockers             []string
	evidence             []string
	ops                  map[string]ExternalOperation
}

func (c *retentionCollector) block(code string) { c.blockers = append(c.blockers, code) }
func (c *retentionCollector) image(repo, digest, app, reason string) *RetentionImage {
	r, e := CanonicalImageRepository(repo)
	if e != nil || !retentionDigest.MatchString(digest) {
		c.block("unresolved_image_reference")
		return nil
	}
	key := r + "@" + digest
	v := c.images[key]
	if v == nil {
		v = &RetentionImage{Identity: key, ImageRepository: r, Digest: digest, ApplicationIDs: []string{}, Profiles: []string{}, ArtifactIDs: []string{}, Reasons: []string{}, Blockers: []string{}, Availability: "UNKNOWN", Freshness: "UNKNOWN"}
		c.images[key] = v
	}
	if app != "" {
		v.ApplicationIDs = append(v.ApplicationIDs, app)
	}
	if reason != "" {
		v.Protected = true
		v.Reasons = append(v.Reasons, reason)
	}
	return v
}
func (c *retentionCollector) ghaOrigin(a DeliveryArtifact) (ExternalOperation, bool) {
	o, ok := c.ops[a.BuildRequest.OperationID]
	if !ok || a.ProductID != c.product || o.ProductID != a.ProductID || o.ApplicationID != a.ApplicationID || o.Kind != "DEPLOY" || o.Snapshot == nil {
		return o, false
	}
	s := o.Snapshot
	if (s.Build.Mode != "" && s.Build.Mode != "GITHUB_ACTIONS") || s.Application.ID != a.ApplicationID || s.Application.ProductID != a.ProductID || s.Build.ApplicationID != a.ApplicationID || s.Build.ProductID != a.ProductID || s.Revision.ProductID != a.ProductID || s.Revision.RepositoryID != a.RepositoryID || s.Revision.HeadCommit != a.SourceCommit || s.Source.ProductID != a.ProductID || s.Source.RepositoryID != a.RepositoryID || !reflect.DeepEqual(s.BuildRequest, a.BuildRequest) || a.BuildRequest.OperationID != o.ID || a.BuildRequest.SourceSHA != a.SourceCommit || a.BuildRequest.Repository == "" || s.Build.ImageRepository != a.ImageRepository {
		return o, false
	}
	if strings.Contains(a.BuildRequest.Repository, "/") && !strings.EqualFold(a.BuildRequest.Repository, s.Source.FullName) {
		return o, false
	}
	return o, true
}
func (c *retentionCollector) artifactRepo(a DeliveryArtifact) string {
	if r, e := CanonicalImageRepository(a.ImageRepository); e == nil {
		return r
	}
	// Only an exact product/application/source original frozen GitHub request
	// establishes the concrete provider's default registry. Copied lineage is not authority.
	o, ok := c.ghaOrigin(a)
	if ok && o.Snapshot.SourceConnection.AppID > 0 && githubAPI(o.Snapshot.SourceConnection.APIURL) {
		if r, e := CanonicalImageRepository("ghcr.io/" + a.ImageRepository); e == nil {
			return r
		}
	}
	return a.ImageRepository
}
func githubAPI(raw string) bool {
	return raw == "" || strings.EqualFold(strings.TrimRight(raw, "/"), "https://api.github.com") || strings.EqualFold(strings.TrimRight(raw, "/"), "https://api.github.com:443")
}
func (c *retentionCollector) observe(v *RetentionImage, at time.Time, availability string) {
	if v == nil {
		return
	}
	old, ok := c.observed[v.Identity]
	if !ok || at.After(old) {
		c.observed[v.Identity] = at
		c.availabilityEvidence[v.Identity] = []string{availability}
	} else if at.Equal(old) {
		c.availabilityEvidence[v.Identity] = append(c.availabilityEvidence[v.Identity], availability)
	}
}
func (c *retentionCollector) chronology(a DeliveryArtifact) (string, time.Time, bool) {
	if a.Native != nil {
		n := a.Native
		if a.Provenance != "NATIVE_WORKER" || n.Reconciliation != nil || !validRetentionProfile(n.Profile) {
			return "", time.Time{}, false
		}
		var j *WorkerJob
		var attempt *WorkerAttempt
		for i := range c.st.WorkerJobs {
			if c.st.WorkerJobs[i].ID == n.PublicationJobID {
				j = &c.st.WorkerJobs[i]
			}
		}
		for i := range c.st.WorkerAttempts {
			if c.st.WorkerAttempts[i].ID == n.PublicationAttemptID {
				attempt = &c.st.WorkerAttempts[i]
			}
		}
		if j == nil || attempt == nil || j.ProductID != a.ProductID || j.Capability != "registry_publish" || a.OperationID != j.OperationID || j.Status != "SUCCEEDED" || j.CancellationRequested || j.InputHash != checkHash(j.Inputs) || j.InputHash != n.PublicationInputHash || j.Inputs.Publication == nil || j.Inputs.RepositoryID != a.RepositoryID || j.Inputs.SourceSHA != a.SourceCommit || attempt.JobID != j.ID || attempt.ProductID != j.ProductID || attempt.Fence != j.Fence || attempt.InputHash != j.InputHash || attempt.Status != "SUCCEEDED" || attempt.UpdatedAt.After(j.Deadline) || attempt.Report == nil {
			return "", time.Time{}, false
		}
		p := j.Inputs.Publication
		r := attempt.Report
		repo := strings.TrimPrefix(strings.TrimPrefix(p.Origin, "https://"), "http://") + "/" + p.Repository
		x, e := CanonicalImageRepository(repo)
		y, ee := CanonicalImageRepository(a.ImageRepository)
		if e != nil || ee != nil || x != y || p.ComponentID != a.ApplicationID || p.Profile != n.Profile || !reflect.DeepEqual(p.Receipt, n.Receipt) || n.Receipt.ManifestDigest != a.Digest || n.Observation.ManifestDigest != a.Digest || r.Result != "PASSED" || r.JobID != j.ID || r.AttemptID != attempt.ID || r.Fence != attempt.Fence || r.InputHash != j.InputHash || r.LocalOCI != nil || r.Publication == nil || r.Publication.ManifestDigest != a.Digest {
			return "", time.Time{}, false
		}
		buildOK := false
		for _, b := range c.st.WorkerJobs {
			if b.ID == p.Receipt.JobID && b.InputHash == checkHash(b.Inputs) && b.ProductID == j.ProductID && b.Inputs.RepositoryID == j.Inputs.RepositoryID && b.Inputs.SourceSHA == j.Inputs.SourceSHA && checkSuccessProvenance(c.st, b) {
				for _, ba := range c.st.WorkerAttempts {
					if ba.ID == p.Receipt.AttemptID && ba.JobID == b.ID && ba.ProductID == b.ProductID && !ba.UpdatedAt.IsZero() && !ba.UpdatedAt.After(attempt.UpdatedAt) && ba.Report != nil && ba.Report.LocalOCI != nil && reflect.DeepEqual(*ba.Report.LocalOCI, p.Receipt) {
						buildOK = true
					}
				}
			}
		}
		return n.Profile, attempt.UpdatedAt, buildOK && !attempt.UpdatedAt.IsZero() && !attempt.UpdatedAt.After(c.now)
	}
	o, ok := c.ghaOrigin(a)
	if !ok || a.BuildRunID <= 0 {
		return "", time.Time{}, false
	}
	profile := o.Snapshot.Build.Profile
	if profile == "" {
		profile = "default"
	}
	if !validRetentionProfile(profile) {
		return "", time.Time{}, false
	}
	earliest := time.Time{}
	for _, run := range c.st.DeliveryBuildRuns {
		r := run.Result
		f := r.Artifacts
		if run.ProductID != a.ProductID || run.OperationID != o.ID || run.ApplicationID != a.ApplicationID || !reflect.DeepEqual(run.Request, a.BuildRequest) || !r.Found || r.ID != a.BuildRunID || r.Status != "completed" || r.Conclusion != "success" || r.HeadSHA != a.SourceCommit || f["operation_id"] != o.ID || f["source_sha"] != a.SourceCommit || f["image_repository"] != a.ImageRepository || f["image_tag"] != a.BuildRequest.ImageTag || f["digest"] != a.Digest || f["available"] != "true" {
			continue
		}
		at, e := time.Parse(time.RFC3339Nano, f["observed_at"])
		if e == nil && !at.IsZero() && !at.After(c.now) && (earliest.IsZero() || at.Before(earliest)) {
			earliest = at
		}
	}
	return profile, earliest, !earliest.IsZero()
}
func validRetentionProfile(p string) bool {
	return regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$`).MatchString(p)
}
func (c *retentionCollector) artifacts() {
	type publication struct {
		image, app, profile string
		at                  time.Time
	}
	pubs := map[string]publication{}
	for _, a := range c.st.DeliveryArtifacts {
		if a.ProductID != c.product {
			continue
		}
		v := c.image(c.artifactRepo(a), a.Digest, a.ApplicationID, "")
		if v == nil {
			continue
		}
		v.ArtifactIDs = append(v.ArtifactIDs, a.ID)
		c.observe(v, a.ObservedAt, a.Availability)
		profile, at, known := c.chronology(a)
		if !known {
			v.Protected = true
			v.Profiles = append(v.Profiles, "unknown")
			v.Reasons = append(v.Reasons, "unknown_publication_chronology")
			v.Blockers = append(v.Blockers, "unknown_publication_chronology")
			c.block("unknown_publication_chronology")
			continue
		}
		v.Profiles = append(v.Profiles, profile)
		if v.PublishedAt == nil || at.Before(*v.PublishedAt) {
			x := at
			v.PublishedAt = &x
		}
		key := a.ApplicationID + "\x00" + profile + "\x00" + v.Identity
		old, exists := pubs[key]
		if !exists || at.Before(old.at) {
			pubs[key] = publication{v.Identity, a.ApplicationID, profile, at}
		}
	}
	groups := map[string][]publication{}
	for _, p := range pubs {
		key := p.app + "\x00" + p.profile
		groups[key] = append(groups[key], p)
	}
	for _, group := range groups {
		sort.Slice(group, func(i, j int) bool {
			if group[i].at.Equal(group[j].at) {
				return group[i].image < group[j].image
			}
			return group[i].at.After(group[j].at)
		})
		keep := 10
		for _, p := range c.policies {
			if p.ApplicationID == group[0].app {
				keep = p.EffectiveKeepLast
			}
		}
		for n, p := range group {
			if n < keep {
				v := c.images[p.image]
				v.Protected = true
				v.Reasons = append(v.Reasons, "keep_last:"+p.app+":"+p.profile)
			}
		}
	}
}
func (c *retentionCollector) ref(value, hint, app, reason string) {
	value = strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(value, "docker-pullable://"), "docker://"), "containerd://")
	if strings.Contains(value, "@") {
		p := strings.SplitN(value, "@", 2)
		c.image(p[0], p[1], app, reason)
		return
	}
	if retentionDigest.MatchString(value) {
		if hint != "" {
			repo := hint
			if i := strings.Index(repo, "@"); i >= 0 {
				repo = repo[:i]
			}
			last := strings.LastIndex(repo, "/")
			if i := strings.LastIndex(repo, ":"); i > last {
				repo = repo[:i]
			}
			c.image(repo, value, app, reason)
			return
		}
		matched := false
		for _, a := range c.st.DeliveryArtifacts {
			if a.ProductID == c.product && a.Digest == value && (app == "" || a.ApplicationID == app) {
				c.image(c.artifactRepo(a), value, app, reason)
				matched = true
			}
		}
		if !matched && app != "" {
			for _, o := range c.st.Operations {
				if o.ProductID == c.product && o.Snapshot != nil && o.Snapshot.Application.ID == app {
					if c.image(c.frozenImageRepository(*o.Snapshot), value, app, reason) != nil {
						matched = true
					}
				}
			}
			for _, b := range c.st.ComponentBuilds {
				if b.ProductID == c.product && b.ApplicationID == app {
					if c.image(b.ImageRepository, value, app, reason) != nil {
						matched = true
					}
				}
			}
		}
		if !matched {
			c.block("unresolved_image_repository")
		}
		return
	}
	if value != "" {
		c.block("mutable_or_unresolved_image_reference")
	}
}
func (c *retentionCollector) snapshot(s DeliverySnapshot, digest, reason string) {
	if digest != "" {
		c.image(c.frozenImageRepository(s), digest, s.Application.ID, reason)
	}
	if s.GitOpsRequest.ImageRepository != "" && digest != "" {
		c.image(s.GitOpsRequest.ImageRepository, digest, s.Application.ID, reason)
	}
}
func terminalRetention(status string) bool {
	return status == "SUCCEEDED" || status == "FAILED" || status == "CANCELLED"
}
func (c *retentionCollector) reconciled(o ExternalOperation) bool {
	for _, r := range c.st.RuntimeObservations {
		if r.ProductID == c.product && r.OperationID == o.ID && r.EnvironmentID == o.EnvironmentID && r.Healthy && o.GitOpsResult != nil && o.Artifact != nil && r.GitOpsCommit == o.GitOpsResult.CommitSHA && r.ArtifactDigest == o.Artifact.Digest && !r.CreatedAt.Before(o.UpdatedAt) && !r.CreatedAt.IsZero() && !r.CreatedAt.After(c.now) {
			return true
		}
	}
	return false
}
func (c *retentionCollector) operation(o ExternalOperation, reason string) {
	if o.Artifact != nil && o.Snapshot != nil {
		c.snapshot(*o.Snapshot, o.Artifact.Digest, reason)
	}
	if o.ExpectedDigest != "" && o.Snapshot != nil {
		c.snapshot(*o.Snapshot, o.ExpectedDigest, reason)
	}
	if o.ExistingArtifact != nil {
		a := *o.ExistingArtifact
		c.image(c.artifactRepo(a), a.Digest, a.ApplicationID, reason)
	}
	if o.GitOpsBase != nil {
		c.image(o.GitOpsBase.ImageRepository, o.GitOpsBase.Digest, o.ApplicationID, "gitops_observed")
	}
	for _, s := range o.PreparedDeployments {
		digest := ""
		for _, a := range c.st.DeliveryArtifacts {
			if a.ProductID == c.product && a.ApplicationID == s.Application.ID && a.SourceCommit == s.Revision.HeadCommit {
				c.image(c.artifactRepo(a), a.Digest, a.ApplicationID, reason)
				digest = a.Digest
			}
		}
		if digest == "" {
			c.block("prepared_deployment_artifact_unresolved")
		}
	}
	for _, id := range o.ChildIDs {
		if child, ok := c.ops[id]; ok && child.ProductID == c.product {
			if child.Artifact != nil && child.Snapshot != nil {
				c.snapshot(*child.Snapshot, child.Artifact.Digest, reason)
			}
			if child.Snapshot != nil && child.ExpectedDigest != "" {
				c.snapshot(*child.Snapshot, child.ExpectedDigest, reason)
			}
		} else {
			c.block("operation_child_unresolved")
		}
	}
}
func (c *retentionCollector) operations() {
	for _, o := range c.st.Operations {
		if o.ProductID != c.product {
			continue
		}
		possible := o.StartedAt != nil || o.Run != nil || o.GitOpsPR != nil || o.GitOpsResult != nil || o.Native != nil || o.Attempts > 0
		reason := ""
		if !terminalRetention(o.Status) || o.Kind == "RELEASE_CANDIDATE" || o.GitOpsPR != nil {
			reason = "operation_inflight"
		}
		if (o.Status == "FAILED" || o.Status == "CANCELLED") && possible && !c.reconciled(o) {
			reason = "uncertain_external_intent"
			c.block("uncertain_external_intent")
		}
		if reason != "" {
			c.operation(o, reason)
		}
		// Every frozen GitOps read is a known root independently of execution success.
		if o.GitOpsBase != nil && o.GitOpsBase.Digest != "" {
			c.image(o.GitOpsBase.ImageRepository, o.GitOpsBase.Digest, o.ApplicationID, "gitops_observed")
		}
	}

	for _, j := range c.st.WorkerJobs {
		if j.ProductID != c.product {
			continue
		}
		if p := j.Inputs.Publication; p != nil {
			repo := strings.TrimPrefix(strings.TrimPrefix(p.Origin, "https://"), "http://") + "/" + p.Repository
			admitted := false
			for _, a := range c.st.DeliveryArtifacts {
				if a.ProductID == c.product && a.Native != nil && a.Native.PublicationJobID == j.ID {
					_, _, valid := c.chronology(a)
					admitted = admitted || valid
				}
			}
			// An unadmitted successful report is still a known physical image. Normal
			// admitted history instead participates in chronology and ordinary roots.
			if j.Status != "SUCCEEDED" || !admitted {
				c.image(repo, p.Receipt.ManifestDigest, p.ComponentID, "worker_publication_intent")
				if j.Status == "SUCCEEDED" {
					c.block("publication_admission_missing_or_invalid")
				} else if j.Status == "FAILED" || j.Status == "CANCELLED" || j.Status == "RECONCILIATION_REQUIRED" {
					c.block("uncertain_publication_intent")
				}
				for _, a := range c.st.WorkerAttempts {
					if a.JobID == j.ID && a.Report != nil && a.Report.Publication != nil {
						c.image(repo, a.Report.Publication.ManifestDigest, p.ComponentID, "worker_publication_receipt")
					}
				}
			}
		} else if j.Capability == "local_oci_build" && checkSuccessProvenance(c.st, j) {
			admitted := false
			for _, a := range c.st.DeliveryArtifacts {
				if a.ProductID == c.product && a.Native != nil && a.Native.Receipt.JobID == j.ID {
					_, _, valid := c.chronology(a)
					admitted = admitted || valid
				}
			}
			if admitted {
				continue
			}
			known := false
			for _, a := range c.st.WorkerAttempts {
				if a.JobID != j.ID || a.Fence != j.Fence || a.Report == nil || a.Report.LocalOCI == nil {
					continue
				}
				receipt := a.Report.LocalOCI
				if proof := j.Inputs.SourceProof; proof != nil {
					if o, ok := c.ops[proof.DeliveryID]; ok && o.ProductID == c.product && o.Snapshot != nil {
						c.snapshot(*o.Snapshot, receipt.ManifestDigest, "accepted_build_receipt")
						known = true
					}
				}
				for _, pub := range c.st.WorkerJobs {
					if pub.ProductID == c.product && pub.Inputs.Publication != nil && pub.Inputs.Publication.Receipt.JobID == j.ID {
						p := pub.Inputs.Publication
						c.image(strings.TrimPrefix(strings.TrimPrefix(p.Origin, "https://"), "http://")+"/"+p.Repository, receipt.ManifestDigest, p.ComponentID, "accepted_build_receipt")
						known = true
					}
				}
			}
			if !known {
				c.block("build_receipt_destination_unknown")
			}
		}
	}

}
func (c *retentionCollector) composition(comp Composition) {
	for _, part := range comp.Components {
		found := false
		for _, a := range c.st.DeliveryArtifacts {
			if a.ProductID != c.product || a.ApplicationID != part.ApplicationID {
				continue
			}
			match := a.SourceCommit == part.BaseCommit
			for _, rev := range comp.RevisionSnapshots {
				for _, id := range part.RevisionIDs {
					if rev.ID == id && rev.HeadCommit == a.SourceCommit {
						match = true
					}
				}
			}
			if match {
				c.image(c.artifactRepo(a), a.Digest, a.ApplicationID, "desired_composition")
				found = true
			}
		}
		if !found {
			c.block("desired_composition_artifact_unresolved")
		}
	}
}
func (c *retentionCollector) environmentMap(v map[string]any, reason string) {
	if len(v) == 0 {
		return
	}
	c.block("untyped_environment_evidence")
	repo, _ := v["image_repository"].(string)
	if digest, ok := v["digest"].(string); ok {
		c.ref(digest, repo, "", reason)
	}
	for k, x := range v {
		if s, ok := x.(string); ok && (k == "image" || k == "workload_image" || k == "image_id") {
			c.ref(s, repo, "", reason)
		}
		if m, ok := x.(map[string]any); ok {
			c.environmentMap(m, reason)
		}
	}
}
func (c *retentionCollector) environments() {
	for _, e := range c.st.Environments {
		if e.ProductID != c.product {
			continue
		}
		if e.DesiredOperationID != "" {
			if o, ok := c.ops[e.DesiredOperationID]; ok && o.ProductID == c.product {
				c.operation(o, "desired_operation")
			} else {
				c.block("desired_operation_unresolved")
			}
		}
		if e.DesiredCompositionID != "" {
			found := false
			for _, comp := range c.st.Compositions {
				if comp.ID == e.DesiredCompositionID && comp.ProductID == c.product {
					c.composition(comp)
					found = true
				}
			}
			if !found {
				c.block("desired_composition_unresolved")
			}
		}
		c.environmentMap(e.Desired, "desired_state")
		c.environmentMap(e.Reconciled, "flux_reconciled")
		c.environmentMap(e.Runtime, "runtime_observed")
	}
	for _, r := range c.st.RuntimeSnapshots {
		if r.ProductID != c.product {
			continue
		}
		at := r.Runtime.ObservedAt
		fresh := evidenceFreshness(at, c.now)
		c.evidence = append(c.evidence, checkHash(struct {
			ID, Environment, App, Freshness string
			Expected                        RuntimeExpected
			Workload                        string
			Pods                            []string
			Flux                            any
			Errors                          bool
		}{r.ID, r.EnvironmentID, r.ApplicationID, fresh, r.Expected, r.Runtime.WorkloadImage, runtimePodEvidence(r), r.Runtime.Flux, len(r.Runtime.Errors) > 0}))
		if fresh != "FRESH" {
			c.block("runtime_evidence_" + strings.ToLower(fresh))
		}
		if len(r.Runtime.Errors) > 0 || r.Expected.Error != "" {
			c.block("runtime_observation_error")
		}
		if r.Runtime.Flux == nil {
			c.block("flux_evidence_missing")
		} else if r.Runtime.Flux.Error != "" {
			c.block("flux_observation_error")
		}
		if r.Runtime.Flux != nil && r.Runtime.Flux.Revision != "" {
			matched := false
			for _, o := range c.st.Operations {
				if o.ProductID == c.product && o.EnvironmentID == r.EnvironmentID && o.ApplicationID == r.ApplicationID && o.GitOpsResult != nil && o.GitOpsResult.CommitSHA != "" && (r.Runtime.Flux.Revision == o.GitOpsResult.CommitSHA || strings.HasSuffix(r.Runtime.Flux.Revision, ":"+o.GitOpsResult.CommitSHA)) && o.Snapshot != nil {
					digest := o.ExpectedDigest
					if o.Artifact != nil {
						digest = o.Artifact.Digest
					}
					if digest != "" {
						c.snapshot(*o.Snapshot, digest, "flux_reconciled")
						matched = true
					}
				}
			}
			if !matched {
				c.block("flux_image_lineage_unresolved")
			}
		}
		c.ref(r.Expected.Value, r.Expected.ImageRepository, r.ApplicationID, "gitops_expected")
		c.ref(r.Runtime.WorkloadImage, "", r.ApplicationID, "runtime_workload")
		for _, p := range r.Runtime.Pods {
			c.ref(p.Image, "", r.ApplicationID, "runtime_pod")
			c.ref(p.ImageID, p.Image, r.ApplicationID, "runtime_pod")
		}
	}
	for _, r := range c.st.RuntimeObservations {
		if r.ProductID != c.product {
			continue
		}
		o, ok := c.ops[r.OperationID]
		if ok && o.Snapshot != nil && o.ProductID == c.product {
			c.image(c.frozenImageRepository(*o.Snapshot), r.ArtifactDigest, o.ApplicationID, "runtime_attestation")
		} else {
			c.ref(r.ArtifactDigest, "", "", "runtime_attestation")
			c.block("runtime_lineage_unresolved")
		}
		if evidenceFreshness(r.CreatedAt, c.now) != "FRESH" {
			c.block("runtime_attestation_stale_or_unknown")
		}
	}
}
func runtimePodEvidence(r RuntimeSnapshot) []string {
	out := []string{}
	for _, p := range r.Runtime.Pods {
		out = append(out, checkHash(p))
	}
	return sortedUnique(out)
}

type retainedRelease struct {
	at        time.Time
	id, scope string
	children  []ExternalOperation
	extra     int
}

func (c *retentionCollector) releases() {
	groups := map[string][]retainedRelease{}
	for _, r := range c.st.Releases {
		if r.ProductID != c.product {
			continue
		}
		parent, hasParent := c.ops[r.ExecutionOperationID]
		children := []ExternalOperation{}
		type root struct {
			snapshot DeliverySnapshot
			digest   string
		}
		unproven := []root{}
		complete := hasParent && parent.ProductID == c.product && parent.Status == "SUCCEEDED" && parent.Kind == "RELEASE_PROMOTION" && !r.RecordedExternally && r.Status == "released" && len(r.ArtifactDigests) > 0 && len(parent.ChildIDs) == len(r.ArtifactDigests) && !r.CreatedAt.IsZero() && !r.CreatedAt.After(c.now)
		scopes := []string{}
		for app, digest := range r.ArtifactDigests {
			matched := []ExternalOperation{}
			// Resolve images before asking whether the release is proven. Missing
			// status/commit/health evidence cannot erase a known frozen repository.
			for _, child := range c.st.Operations {
				if r.ExecutionOperationID == "" || child.ProductID != c.product || child.ParentID != r.ExecutionOperationID || child.ApplicationID != app || child.Snapshot == nil {
					continue
				}
				unproven = append(unproven, root{*child.Snapshot, digest})
				if child.Artifact != nil {
					unproven = append(unproven, root{*child.Snapshot, child.Artifact.Digest})
				}
				listed := false
				for _, id := range parent.ChildIDs {
					if id == child.ID {
						listed = true
					}
				}
				if listed && child.Artifact != nil && child.Artifact.Digest == digest {
					matched = append(matched, child)
				}
			}
			if len(matched) != 1 {
				complete = false
				continue
			}
			child := matched[0]
			children = append(children, child)
			snap := child.Snapshot
			if child.Status != "SUCCEEDED" || child.GitOpsResult == nil || child.GitOpsResult.CommitSHA == "" || child.GitOpsResult.CommitSHA != r.GitOpsCommits[app] || snap.Target.Purpose != "PROD" || snap.Target.EnvironmentID != child.EnvironmentID || snap.Environment.ID != child.EnvironmentID || snap.Application.ID != app || r.SourceCommits[app] != snap.Revision.HeadCommit || snap.Target.RepositoryID == "" || snap.Target.Ref == "" || snap.Target.Path == "" || snap.Build.ImageRepository == "" {
				complete = false
			}
			healthy := false
			if child.GitOpsResult != nil {
				for _, obs := range c.st.RuntimeObservations {
					if obs.ProductID == c.product && obs.OperationID == child.ID && obs.EnvironmentID == child.EnvironmentID && obs.Healthy && obs.GitOpsCommit == child.GitOpsResult.CommitSHA && obs.ArtifactDigest == digest && !obs.CreatedAt.IsZero() && !obs.CreatedAt.After(r.CreatedAt) {
						healthy = true
					}
				}
			}
			if !healthy {
				complete = false
			}
			image := c.frozenImageRepository(*snap)
			scopes = append(scopes, checkHash(struct{ App, Purpose, Repo, Ref, Path, Env, Cluster, Namespace, Image string }{app, snap.Target.Purpose, snap.Target.RepositoryID + "|" + snap.GitOps.FullName, snap.Target.Ref, snap.Target.Path, snap.Environment.ID, snap.Environment.Cluster, snap.Environment.Namespace, image}))
		}
		if !complete {
			c.block("release_lineage_incomplete_or_external")
			for _, root := range unproven {
				c.snapshot(root.snapshot, root.digest, "release_unproven")
			}
			for app, digest := range r.ArtifactDigests {
				known := false
				for _, root := range unproven {
					if root.snapshot.Application.ID == app && root.digest == digest {
						known = true
					}
				}
				if !known {
					c.ref(digest, "", app, "release_unproven")
				}
			}
			continue
		}
		sort.Strings(scopes)
		scope := checkHash(scopes)
		extra := 1
		for _, p := range c.policies {
			for _, child := range children {
				if p.ApplicationID == child.ApplicationID && p.KeepPrevious > extra {
					extra = p.KeepPrevious
				}
			}
		}
		groups[scope] = append(groups[scope], retainedRelease{r.CreatedAt, r.ID, scope, children, extra})
	}
	for _, rs := range groups {
		sort.Slice(rs, func(i, j int) bool {
			if rs[i].at.Equal(rs[j].at) {
				return rs[i].id < rs[j].id
			}
			return rs[i].at.After(rs[j].at)
		})
		for n, r := range rs {
			if n <= r.extra {
				reason := "production_previous"
				if n == 0 {
					reason = "production_current"
				}
				for _, child := range r.children {
					c.snapshot(*child.Snapshot, child.Artifact.Digest, reason)
				}
			}
		}
	}
}
func (c *retentionCollector) evaluate() {
	for _, a := range c.st.Applications {
		if a.ProductID != c.product {
			continue
		}
		p := EffectiveRetentionPolicy{ApplicationID: a.ID, EffectiveKeepLast: 10, KeepCurrent: true, KeepPrevious: 1}
		if a.Retention != nil {
			p.ConfiguredKeepLast = a.Retention.KeepLast
			if a.Retention.KeepLast > 10 {
				p.EffectiveKeepLast = a.Retention.KeepLast
			}
			if a.Retention.KeepPrevious > 1 {
				p.KeepPrevious = a.Retention.KeepPrevious
			}
		}
		c.policies = append(c.policies, p)
	}
	sort.Slice(c.policies, func(i, j int) bool { return c.policies[i].ApplicationID < c.policies[j].ApplicationID })
	c.relevantEvidence()
	c.artifacts()
	c.operations()
	c.environments()
	c.releases()
	for _, p := range c.st.ArtifactPins {
		if p.ProductID == c.product {
			c.evidence = append(c.evidence, checkHash(struct {
				ID, Hash   string
				ReleasedAt *time.Time
			}{p.ID, p.RequestHash, p.ReleasedAt}))
			if p.ReleasedAt == nil {
				c.image(p.ImageRepository, p.Digest, p.ApplicationID, "pin:"+p.Purpose)
			}
		}
	}
	for _, q := range c.st.NativeQuarantines {
		if q.ProductID == c.product {
			c.block("restore_quarantine")
		}
	}
	for key, v := range c.images {
		if at, ok := c.observed[key]; ok {
			v.Freshness = evidenceFreshness(at, c.now)
			av := sortedUnique(c.availabilityEvidence[key])
			if v.Freshness != "UNKNOWN" && len(av) == 1 && (av[0] == "PRESENT" || av[0] == "MISSING") {
				v.Availability = av[0]
			}
			c.evidence = append(c.evidence, checkHash(struct {
				Identity     string
				At           time.Time
				Availability []string
				Freshness    string
			}{key, at, av, v.Freshness}))
		}
		if v.Availability == "UNKNOWN" {
			v.Blockers = append(v.Blockers, "availability_evidence_unknown_or_conflicting")
			c.block("availability_evidence_unknown_or_conflicting")
		}
		if v.Freshness != "FRESH" {
			v.Blockers = append(v.Blockers, "registry_evidence_"+strings.ToLower(v.Freshness))
			c.block("registry_evidence_" + strings.ToLower(v.Freshness))
		}
	}
}
func newRetentionCollector(st State, product string, now time.Time) *retentionCollector {
	c := &retentionCollector{st: st, product: product, now: now, images: map[string]*RetentionImage{}, observed: map[string]time.Time{}, availabilityEvidence: map[string][]string{}, ops: map[string]ExternalOperation{}, policies: []EffectiveRetentionPolicy{}, blockers: []string{}, evidence: []string{}}
	for _, o := range st.Operations {
		c.ops[o.ID] = o
	}
	return c
}

// EvaluateRetention is a pure protected-root inventory. Its fingerprint is an
// advisory input identity, never a deletion capability, lease or completeness proof.
func EvaluateRetention(st State, product string, now time.Time, authorities []RegistryObservationAuthority) RetentionReport {
	c := newRetentionCollector(st, product, now)
	c.evaluate()
	products := map[string]bool{}
	for _, p := range st.Products {
		products[p.ID] = true
	}
	for _, a := range st.DeliveryArtifacts {
		products[a.ProductID] = true
	}
	for _, p := range st.ArtifactPins {
		products[p.ProductID] = true
	}
	for other := range products {
		if other == product {
			continue
		}
		foreign := newRetentionCollector(st, other, now)
		foreign.evaluate()
		localRepositories := map[string]bool{}
		for _, v := range c.images {
			localRepositories[v.ImageRepository] = true
		}
		for _, b := range st.ComponentBuilds {
			if b.ProductID == product {
				if repo, e := CanonicalImageRepository(b.ImageRepository); e == nil {
					localRepositories[repo] = true
				}
			}
		}
		if evidence := foreign.sharedScopeEvidence(localRepositories); evidence != "" {
			c.evidence = append(c.evidence, evidence)
		}
		for key, f := range foreign.images {
			if !f.Protected || !localRepositories[f.ImageRepository] {
				continue
			}
			v := c.images[key]
			if v == nil {
				v = c.image(f.ImageRepository, f.Digest, "", "shared_product_root")
				v.Blockers = append(v.Blockers, "registry_evidence_unknown")
				c.block("registry_evidence_unknown")
			} else {
				v.Protected = true
				v.Reasons = append(v.Reasons, "shared_product_root")
			}
		}

	}
	report := RetentionReport{ProductID: product, EvaluatedAt: now.UTC(), Policies: c.policies, Images: []RetentionImage{}, Blockers: append(c.blockers, "registry_completeness_unavailable", "oci_closure_unavailable", "workload_coverage_unavailable")}
	for _, v := range c.images {
		v.ApplicationIDs = sortedUnique(v.ApplicationIDs)
		v.Profiles = sortedUnique(v.Profiles)
		v.ArtifactIDs = sortedUnique(v.ArtifactIDs)
		v.Reasons = sortedUnique(v.Reasons)
		v.Blockers = sortedUnique(v.Blockers)
		report.Images = append(report.Images, *v)
	}
	sort.Slice(report.Images, func(i, j int) bool { return report.Images[i].Identity < report.Images[j].Identity })
	report.Blockers = sortedUnique(report.Blockers)
	c.evidence = append(c.evidence, registryRetention(st, &report, now, authorities)...)
	hashed := report
	hashed.Policies = append([]EffectiveRetentionPolicy{}, report.Policies...)
	for i := range hashed.Policies {
		hashed.Policies[i].ConfiguredKeepLast = 0
	}
	hashed.EvaluatedAt = time.Time{}
	report.InputHash = checkHash(struct {
		Report   RetentionReport
		Evidence []string
	}{hashed, sortedUnique(c.evidence)})
	return report
}

// Relevant evidence deliberately omits log/detail/actor/credential payloads and
// local artifact insertion times. Each item is a set member, not array position.
func (c *retentionCollector) relevantEvidence() {
	for _, run := range c.st.DeliveryBuildRuns {
		if run.ProductID == c.product {
			c.evidence = append(c.evidence, retentionRunEvidence(run))
		}
	}

	for _, a := range c.st.DeliveryArtifacts {
		if a.ProductID == c.product {
			c.evidence = append(c.evidence, checkHash(struct {
				ID, Operation, App, Repository, Source, Image, Digest, Provenance string
				Native                                                            *NativeArtifact
				BuildRequest                                                      any
				RunID                                                             int64
			}{a.ID, a.OperationID, a.ApplicationID, a.RepositoryID, a.SourceCommit, c.artifactRepo(a), a.Digest, a.Provenance, a.Native, a.BuildRequest, a.BuildRunID}))
		}
	}
	for _, o := range c.st.Operations {
		if o.ProductID != c.product {
			continue
		}
		c.evidence = append(c.evidence, checkHash(struct {
			ID      string
			Sources []FlowSource
		}{o.ID, o.Sources}))
		children := sortedUnique(append([]string{}, o.ChildIDs...))
		snapshots := []string{}
		if o.Snapshot != nil {
			snapshots = append(snapshots, retentionSnapshotHash(*o.Snapshot))
		}
		for _, s := range o.PreparedDeployments {
			snapshots = append(snapshots, retentionSnapshotHash(s))
		}
		c.evidence = append(c.evidence, checkHash(struct {
			ID, Parent, App, Env, Kind, Status, Phase, Expected string
			Children, Snapshots                                 []string
			Artifact                                            any
			Run                                                 any
			Native                                              *NativeDelivery
			HasPR                                               bool
		}{o.ID, o.ParentID, o.ApplicationID, o.EnvironmentID, o.Kind, o.Status, o.Phase, o.ExpectedDigest, children, sortedUnique(snapshots), o.Artifact, o.Run, o.Native, o.GitOpsPR != nil}))
	}
	for _, j := range c.st.WorkerJobs {
		if j.ProductID == c.product {
			c.evidence = append(c.evidence, checkHash(struct {
				ID          string
				Deadline    time.Time
				MaxAttempts int
			}{j.ID, j.Deadline, j.MaxAttempts}))
			c.evidence = append(c.evidence, checkHash(struct {
				ID, Operation, Status, Capability, InputHash, Inputs string
				Fence                                                uint64
				Cancelled                                            bool
			}{j.ID, j.OperationID, j.Status, j.Capability, j.InputHash, checkHash(j.Inputs), j.Fence, j.CancellationRequested}))
		}
	}
	for _, a := range c.st.WorkerAttempts {
		if a.ProductID == c.product {
			c.evidence = append(c.evidence, checkHash(struct {
				ID         string
				LeaseUntil time.Time
			}{a.ID, a.LeaseUntil}))
			c.evidence = append(c.evidence, checkHash(struct {
				ID, Job, Status, InputHash, Report string
				Fence                              uint64
				Completed                          time.Time
			}{a.ID, a.JobID, a.Status, a.InputHash, retentionWorkerReportHash(a.Report), a.Fence, a.UpdatedAt}))
		}
	}
	for _, r := range c.st.Releases {
		if r.ProductID == c.product {
			c.evidence = append(c.evidence, checkHash(struct {
				ID, Status, Execution       string
				Artifacts, Commits, Sources map[string]string
				External                    bool
				At                          time.Time
			}{r.ID, r.Status, r.ExecutionOperationID, r.ArtifactDigests, r.GitOpsCommits, r.SourceCommits, r.RecordedExternally, r.CreatedAt}))
		}
	}
	for _, r := range c.st.RuntimeObservations {
		if r.ProductID == c.product {
			c.evidence = append(c.evidence, checkHash(struct {
				ID, Operation, Environment, Digest, GitOps, Freshness string
				Healthy                                               bool
			}{r.ID, r.OperationID, r.EnvironmentID, r.ArtifactDigest, r.GitOpsCommit, evidenceFreshness(r.CreatedAt, c.now), r.Healthy}))
		}
	}
	for _, e := range c.st.Environments {
		if e.ProductID == c.product {
			c.evidence = append(c.evidence, checkHash(struct{ ID, Operation, Composition, Cluster, Namespace string }{e.ID, e.DesiredOperationID, e.DesiredCompositionID, e.Cluster, e.Namespace}))
		}
	}
	for _, comp := range c.st.Compositions {
		if comp.ProductID == c.product {
			revisions := []string{}
			for _, rev := range comp.RevisionSnapshots {
				revisions = append(revisions, checkHash(struct{ ID, Repository, Base, Head string }{rev.ID, rev.RepositoryID, rev.BaseCommit, rev.HeadCommit}))
			}
			c.evidence = append(c.evidence, checkHash(struct {
				ID        string
				Revisions []string
			}{comp.ID, sortedUnique(revisions)}))
			components := []string{}
			for _, v := range comp.Components {
				// RevisionIDs retain semantic merge order.
				components = append(components, checkHash(v))
			}
			c.evidence = append(c.evidence, checkHash(struct {
				ID, Env, Status string
				Components      []string
			}{comp.ID, comp.EnvironmentID, comp.Status, sortedUnique(components)}))
		}
	}
}
func retentionSnapshotHash(s DeliverySnapshot) string {
	return checkHash(struct {
		App, Mode, Profile, Image, Purpose, Repo, Ref, Path, Env, Cluster, Namespace, Source string
		Native                                                                               *NativeDelivery
		BuildRequest                                                                         any
	}{s.Application.ID, s.Build.Mode, s.Build.Profile, s.Build.ImageRepository, s.Target.Purpose, s.Target.RepositoryID, s.Target.Ref, s.Target.Path, s.Environment.ID, s.Environment.Cluster, s.Environment.Namespace, s.Revision.HeadCommit, s.Native, s.BuildRequest})
}

func (c *retentionCollector) frozenImageRepository(s DeliverySnapshot) string {
	return c.artifactRepo(DeliveryArtifact{Meta: Meta{ProductID: c.product}, ApplicationID: s.Application.ID, RepositoryID: s.Revision.RepositoryID, SourceCommit: s.Revision.HeadCommit, ImageRepository: s.Build.ImageRepository, BuildRequest: s.BuildRequest})
}

// sharedScopeEvidence binds foreign evidence opaquely, independently of its
// coarse public shared_product_root projection. Selection starts at physical
// repositories, then follows only the provenance needed by those images.
// An unrelated foreign repository, event or application is not hashed here.
func (c *retentionCollector) sharedScopeEvidence(repositories map[string]bool) string {
	inScope := func(repo string) bool {
		canonical, e := CanonicalImageRepository(repo)
		return e == nil && repositories[canonical]
	}
	refScope := func(value, hint string) bool {
		value = strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(value, "docker-pullable://"), "docker://"), "containerd://")
		if i := strings.Index(value, "@"); i >= 0 {
			return inScope(value[:i])
		}
		if retentionDigest.MatchString(value) {
			if hint == "" {
				for _, image := range c.images {
					if image.Digest == value && inScope(image.ImageRepository) {
						return true
					}
				}
			}
			value = hint
		}
		if i := strings.LastIndex(value, ":"); i > strings.LastIndex(value, "/") {
			value = value[:i]
		}
		return inScope(value)
	}
	selected := EmptyState()
	apps, ops, jobs, pins := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	evidence := []string{}
	for _, v := range c.images {
		if !inScope(v.ImageRepository) {
			continue
		}
		image := *v
		image.ApplicationIDs = sortedUnique(append([]string{}, v.ApplicationIDs...))
		image.Profiles = sortedUnique(append([]string{}, v.Profiles...))
		image.ArtifactIDs = sortedUnique(append([]string{}, v.ArtifactIDs...))
		image.Reasons = sortedUnique(append([]string{}, v.Reasons...))
		image.Blockers = sortedUnique(append([]string{}, v.Blockers...))
		evidence = append(evidence, checkHash(image))
		for _, app := range image.ApplicationIDs {
			apps[app] = true
		}
	}
	for _, p := range c.st.ArtifactPins {
		if p.ProductID == c.product && inScope(p.ImageRepository) {
			pins[p.ID] = true
			apps[p.ApplicationID] = true
			evidence = append(evidence, checkHash(struct {
				ID, RequestHash string
				ReleasedAt      *time.Time
			}{p.ID, p.RequestHash, p.ReleasedAt}))
		}
	}
	for _, e := range c.st.Events {
		if (pins[e.EntityID] || pins[e.Data.ID]) && (e.Action == "pin_artifact" || e.Action == "release_artifact_pin") {
			evidence = append(evidence, checkHash(e))
		}
	}
	for _, a := range c.st.DeliveryArtifacts {
		if a.ProductID != c.product || !inScope(c.artifactRepo(a)) {
			continue
		}
		selected.DeliveryArtifacts = append(selected.DeliveryArtifacts, a)
		apps[a.ApplicationID] = true
		ops[a.OperationID] = true
		ops[a.BuildRequest.OperationID] = true
		if a.Native != nil {
			jobs[a.Native.PublicationJobID] = true
			jobs[a.Native.Receipt.JobID] = true
		}
	}
	for _, o := range c.st.Operations {
		if o.ProductID != c.product {
			continue
		}
		match := o.Snapshot != nil && inScope(c.frozenImageRepository(*o.Snapshot)) || o.GitOpsBase != nil && inScope(o.GitOpsBase.ImageRepository) || o.ExistingArtifact != nil && inScope(c.artifactRepo(*o.ExistingArtifact))
		for _, s := range o.PreparedDeployments {
			match = match || inScope(c.frozenImageRepository(s))
		}
		if match {
			ops[o.ID] = true
			apps[o.ApplicationID] = true
		}
	}
	// A whole release's completion depends on every executed child. Its other
	// children are relevant lineage; unrelated releases/private repositories are not.
	for _, r := range c.st.Releases {
		if r.ProductID != c.product {
			continue
		}
		parent := c.ops[r.ExecutionOperationID]
		match := ops[parent.ID]
		for _, id := range parent.ChildIDs {
			match = match || ops[id]
		}
		if match {
			selected.Releases = append(selected.Releases, r)
			ops[parent.ID] = true
			for _, id := range parent.ChildIDs {
				ops[id] = true
			}
		}
	}
	for _, j := range c.st.WorkerJobs {
		if j.ProductID != c.product || j.Inputs.Publication == nil {
			continue
		}
		p := j.Inputs.Publication
		repo := strings.TrimPrefix(strings.TrimPrefix(p.Origin, "https://"), "http://") + "/" + p.Repository
		if inScope(repo) {
			jobs[j.ID] = true
			jobs[p.Receipt.JobID] = true
			apps[p.ComponentID] = true
		}
	}
	for _, j := range c.st.WorkerJobs {
		if j.ProductID != c.product {
			continue
		}
		if proof := j.Inputs.SourceProof; proof != nil && ops[proof.DeliveryID] {
			jobs[j.ID] = true
		}
		if jobs[j.ID] {
			selected.WorkerJobs = append(selected.WorkerJobs, j)
			ops[j.OperationID] = true
		}
	}
	for _, a := range c.st.WorkerAttempts {
		if a.ProductID == c.product && jobs[a.JobID] {
			selected.WorkerAttempts = append(selected.WorkerAttempts, a)
		}
	}
	for _, o := range c.st.Operations {
		if o.ProductID == c.product && ops[o.ID] {
			selected.Operations = append(selected.Operations, o)
		}
	}
	for _, run := range c.st.DeliveryBuildRuns {
		if run.ProductID != c.product || !ops[run.OperationID] {
			continue
		}
		evidence = append(evidence, retentionRunEvidence(run))
	}
	for _, r := range c.st.RuntimeObservations {
		if r.ProductID == c.product && ops[r.OperationID] {
			selected.RuntimeObservations = append(selected.RuntimeObservations, r)
		}
	}
	for _, r := range c.st.RuntimeSnapshots {
		if r.ProductID != c.product {
			continue
		}
		pods := []string{}
		workload := refScope(r.Runtime.WorkloadImage, "")
		expected := inScope(r.Expected.ImageRepository) || refScope(r.Expected.Value, r.Expected.ImageRepository)
		for _, pod := range r.Runtime.Pods {
			if refScope(pod.Image, "") || refScope(pod.ImageID, pod.Image) {
				pods = append(pods, checkHash(pod))
			}
		}
		if !workload && !expected && len(pods) == 0 {
			continue
		}
		apps[r.ApplicationID] = true
		// Do not include a sidecar's unrelated private image or raw error text.
		wanted := RuntimeExpected{}
		if expected {
			wanted = r.Expected
		}
		image := ""
		if workload {
			image = r.Runtime.WorkloadImage
		}
		evidence = append(evidence, checkHash(struct {
			ID, Environment, App, Freshness, Workload, Health string
			Expected                                          RuntimeExpected
			Pods                                              []string
			Flux                                              any
			Errors                                            bool
		}{r.ID, r.EnvironmentID, r.ApplicationID, evidenceFreshness(r.Runtime.ObservedAt, c.now), image, r.Runtime.Health, wanted, sortedUnique(pods), r.Runtime.Flux, len(r.Runtime.Errors) > 0}))
	}
	desiredCompositions := map[string]bool{}
	for _, e := range c.st.Environments {
		if e.ProductID == c.product && e.DesiredCompositionID != "" {
			desiredCompositions[e.DesiredCompositionID] = true
		}
	}
	for _, comp := range c.st.Compositions {
		if comp.ProductID != c.product || !desiredCompositions[comp.ID] {
			continue
		}
		match := false
		for _, part := range comp.Components {
			match = match || apps[part.ApplicationID]
		}
		if match {
			selected.Compositions = append(selected.Compositions, comp)
		}
	}
	for _, e := range c.st.Environments {
		if e.ProductID != c.product {
			continue
		}
		match := ops[e.DesiredOperationID]
		for _, comp := range selected.Compositions {
			match = match || e.DesiredCompositionID == comp.ID
		}
		if match {
			selected.Environments = append(selected.Environments, e)
		}
	}
	for _, p := range c.policies {
		if apps[p.ApplicationID] {
			p.ConfiguredKeepLast = 0
			evidence = append(evidence, checkHash(p))
		}
	}
	scoped := *c
	scoped.st = selected
	scoped.evidence = nil
	scoped.relevantEvidence()
	evidence = append(evidence, scoped.evidence...)
	if len(evidence) == 0 {
		return ""
	}
	return checkHash(sortedUnique(evidence))
}

// Both local and scoped foreign fingerprints bind the same typed run facts.
// URLs, arbitrary artifact fields and raw provider/log payloads are excluded.
func retentionRunEvidence(run DeliveryBuildRun) string {
	r := run.Result
	f := r.Artifacts
	return checkHash(struct {
		ID, Operation, App                                                                             string
		Request                                                                                        any
		Found                                                                                          bool
		RunID                                                                                          int64
		Status, Conclusion, Head, ReportedOperation, Source, Image, Tag, Digest, Available, ObservedAt string
	}{run.ID, run.OperationID, run.ApplicationID, run.Request, r.Found, r.ID, r.Status, r.Conclusion, r.HeadSHA, f["operation_id"], f["source_sha"], f["image_repository"], f["image_tag"], f["digest"], f["available"], f["observed_at"]})
}
