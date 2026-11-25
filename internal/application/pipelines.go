package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"releasecontrol/internal/domain"
)

type pipelineCursor struct {
	Product, Profile, Status, Environment, Query string
	AfterTime                                    time.Time
	AfterID                                      string
}

var pipelineStatuses = map[string]bool{"success": true, "running": true, "pending": true, "failed": true, "cancelled": true, "superseded": true, "attention": true, "skipped": true}

func buildPipelineStage(b domain.Build, canClose, canCancel bool) domain.PipelineStage {
	s := domain.PipelineStage{Kind: "build", EntityID: b.ID, RawStatus: b.Status, FailureCode: b.FailureCode, Status: "pending", Label: "Queued"}
	switch b.Status {
	case "QUEUED":
		if b.WaitingForBuildID != "" {
			s.Label = "Waiting for the running build of this branch"
			s.Hint = "Waiting for build " + b.WaitingForBuildID + "; newer merges replace this build while it waits."
		} else {
			s.Label = "Queued"
		}
	case "DISPATCHED":
		s.Label = "Dispatched to runner"
	case "RUNNING":
		s.Status = "running"
		s.Label = "Running"
	case "SUCCEEDED":
		s.Status = "success"
		s.Label = "Succeeded"
	case "FAILED":
		s.Status = "failed"
		s.Label = "Failed"
		s.Hint = failureHint(b.FailureCode)
		if b.FailureMessage != "" {
			msg := strings.TrimSpace(b.FailureMessage)
			if r := []rune(msg); len(r) > 300 {
				msg = string(r[:300])
			}
			if msg != "" {
				s.Hint += " Details: " + msg
			}
		}
	case "CANCELLED":
		if b.SupersededBy != "" {
			s.Status = "superseded"
			s.Label = "Superseded by a newer commit"
			s.Hint = "Newer build: " + b.SupersededBy
		} else {
			s.Status = "cancelled"
			s.Label = "Cancelled"
		}
	case "RECONCILIATION_REQUIRED":
		s.Status = "attention"
		s.Label = "Needs attention"
		s.Hint = ""
		if b.FailureCode != "" {
			s.Hint = failureHint(b.FailureCode)
		}
		if s.Hint == "" {
			s.Hint = "Provider run state is uncertain; RCP is reconciling by trigger_event. If it stays, an admin may close the build with a reason."
		}
		if b.ProviderUnavailable {
			s.Hint += " Forgejo provider is unavailable."
		}
	default:
		s.Status = "failed"
		s.Label = b.Status
	}
	if canClose && buildCanClose(b) {
		s.AllowedActions = append(s.AllowedActions, "close_build")
	}
	if canCancel && buildCanCancel(b) {
		s.AllowedActions = append(s.AllowedActions, "cancel_build")
	}
	return s
}
func buildChecksStage(b domain.Build) domain.PipelineStage {
	s := domain.PipelineStage{Kind: "checks", EntityID: b.ID, RawStatus: b.CIState, Status: "pending", Label: "Checks pending"}
	switch b.CIState {
	case "success":
		s.Status = "success"
		s.Label = "Checks passed"
	case "pending":
		s.Status = "running"
		s.Label = "Checks running"
	case "failure":
		s.Status = "failed"
		s.Label = "Checks failed"
		keys := make([]string, 0, len(b.CIFailingContexts))
		for k := range b.CIFailingContexts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		s.Hint = strings.Join(keys, ", ")
	case "cancelled":
		s.Status = "cancelled"
		s.Label = "Checks cancelled"
	case "":
		if b.Status == "FAILED" || b.Status == "CANCELLED" {
			s.Status = "skipped"
			s.Label = "Not observed"
		} else {
			s.Status = "pending"
			s.Label = "Waiting for checks"
		}
	}
	return s
}
func deploymentPipelineStage(d domain.Deployment, canClose bool) domain.PipelineStage {
	s := domain.PipelineStage{Kind: "deploy", EnvironmentID: d.EnvironmentID, EntityID: d.ID, RawStatus: d.Status, FailureCode: d.FailureCode, Status: "pending", Label: d.Status}
	switch d.Status {
	case "PENDING_CHECKS":
		s.Label = "Waiting for CI/registry checks"
	case "PROPOSING":
		s.Status = "running"
		s.Label = "Opening Flux PR"
	case "MERGING":
		s.Status = "running"
		s.Label = "Merging Flux PR"
	case "VERIFYING_ROLLOUT":
		s.Status = "running"
		s.Label = "Verifying rollout"
	case "SUCCEEDED":
		s.Status = "success"
		s.Label = "Deployed"
		if !d.RolloutVerified {
			s.Hint = "GitOps applied; runtime rollout not verified."
		}
	case "RECONCILIATION_REQUIRED":
		s.Status = "attention"
		s.Label = "Needs attention"
		s.Hint = failureHint(d.FailureCode)
	case "FAILED":
		if d.FailureCode == "SUPERSEDED" || d.SupersededBy != "" {
			s.Status = "superseded"
			s.Label = "Superseded"
		} else if d.FailureCode == "OPERATOR_CLOSED" {
			s.Status = "cancelled"
			s.Label = "Closed by operator"
			s.Hint = d.CloseReason
		} else {
			s.Status = "failed"
			s.Label = "Failed"
			s.Hint = failureHint(d.FailureCode)
		}
	default:
		s.Status = "failed"
		s.Label = d.Status
	}
	if d.FailureCode != "" && s.Hint == "" {
		s.Hint = failureHint(d.FailureCode)
	}
	if canClose && deploymentSafeToClose(d) {
		s.AllowedActions = append(s.AllowedActions, "close_deployment")
	}
	if canClose && !deploymentTerminal(d) {
		s.AllowedActions = append(s.AllowedActions, "reconcile_deployment")
	}
	return s
}
func failureHint(code string) string {
	hints := map[string]string{
		"MAIN_CI_FAILED":               "Source CI failed. Review the failed workflow and fix or rerun its checks before deploying.",
		"PR_NOT_CREATED":               "RCP could not confirm a Flux PR for this deployment before its deadline (no deploy branch or PR after retries, or no valid merge commit), so it was marked failed. Check the GitOps repository for a leftover branch or PR, then retry with deploy_build.",
		"PR_UNCERTAIN_EXPIRED":         "Reconciliation found no unique Flux PR or deploy branch before the uncertainty window expired, so no GitOps change was accepted. Review provider evidence, then retry with deploy_build.",
		"RECONCILIATION_TIMEOUT":       "The Forgejo run could not be resolved before the reconciliation deadline. Check the run in Forgejo; an admin can close the build if it is no longer actionable.",
		"ARTIFACT_REVISION_MISMATCH":   "The registry artifact does not match the build's recorded source revision. Rebuild and publish from the intended revision, then queue_build.",
		"BUILD_AUTHORITY_CHANGED":      "Build provider authority changed while the run was active; RCP will not accept its artifacts. Review configuration and Forgejo evidence, then have an admin close the build if needed.",
		"BUILD_RESTORED":               "This build was restored from backup and its in-flight provider outcome is quarantined. Check Forgejo, then have an admin close it if appropriate; create a fresh build with queue_build.",
		"BUILD_TIMEOUT":                "The build exceeded its configured timeout. Check the Forgejo run and logs, then queue_build after correcting or cancelling the run.",
		"DISPATCH_REJECTED":            "Forgejo rejected the build dispatch. Review provider configuration and run evidence, then retry with queue_build.",
		"DISPATCH_UNCERTAIN":           "RCP could not determine whether Forgejo accepted the dispatch. Wait for auto-reconcile by trigger_event; an admin can close the build if it stays unresolved.",
		"INVALID_BUILD_SOURCE":         "The requested source revision is invalid or unavailable to the configured Forgejo repository. Correct the source and queue_build.",
		"RUN_FAILED":                   "The Forgejo run ended in failure, cancellation, skip or blocked status. Inspect the run and logs, fix the cause, then queue_build.",
		"RUN_SOURCE_MISMATCH":          "The observed Forgejo run does not match the build's recorded source. Check the run and trigger_event; wait for auto-reconcile or ask an admin to close the build.",
		"RUN_STATUS_UNKNOWN":           "Forgejo returned an unrecognized run status. Check the run in Forgejo and wait for auto-reconcile; an admin can close the build if needed.",
		"SIGNING_AUTHORITY_CHANGED":    "Signing authority changed while signing was in progress, so the signature was rejected. Review signing configuration and request signing again.",
		"SIGNING_KEY_MISMATCH":         "The signer used a key different from the requested key. Correct the key configuration and request signing again.",
		"SIGNING_UNAVAILABLE":          "The signing provider is unavailable. Restore signer access and retry signing the artifact.",
		"DEPLOYMENT_RESTORED":          "This deployment was restored from backup and its in-flight GitOps outcome is quarantined. Inspect the PR and branch; an admin can close it after review.",
		"SECURITY_GATE_MISSING":        "A required security gate is not configured for this artifact. Configure the gate before signing or deploying.",
		"SECURITY_GATE_FAILED":         "A required security gate failed. Review its results, resolve the findings and retry.",
		"SECURITY_GATE_NOT_PASSED":     "The required security gate has not passed. Review gate results and retry after it passes.",
		"ROLLOUT_FAILED":               "GitOps applied the change, but runtime verification reported a failed rollout. Inspect the workload and controller evidence before retrying.",
		"ROLLOUT_TIMEOUT":              "Runtime rollout did not complete before its deadline. Inspect workload health and controller events before retrying.",
		"PR_UNCERTAIN":                 "RCP could not confirm whether the Flux PR was created. It reconciles the deploy branch and PR automatically; a new deploy for this profile is blocked until this one is resolved.",
		"MERGE_UNCERTAIN":              "RCP could not confirm whether the Flux PR merged. It checks the recorded PR and branch; review that evidence before retrying.",
		"CI_NOT_GREEN":                 "Required CI checks did not pass. Review the listed contexts and fix or rerun the failing checks.",
		"CI_UNAVAILABLE":               "RCP could not read CI status. Check provider availability and retry after it recovers.",
		"SIGNATURE_MISSING":            "A required artifact signature is missing. Sign the exact image digest and retry delivery.",
		"REGISTRY_UNAVAILABLE":         "RCP could not read the registry. Restore registry access and retry.",
		"ARTIFACT_MISSING":             "The expected image digest is absent from the registry. Publish or rebuild the artifact before deploying.",
		"ARTIFACT_UNAVAILABLE":         "RCP could not verify the image in the registry. Check registry access and retry.",
		"MERGE_CONFLICT":               "The Flux change conflicts with the target branch. Resolve the conflict and submit a fresh deployment request.",
		"MERGE_UNCERTAIN_EXPIRED":      "RCP could not confirm the Flux merge after three SHA-locked retries before the deployment deadline. Review the PR and submit a fresh deployment request if it remains needed.",
		"GITOPS_SCOPE_CHANGED":         "The GitOps target changed while RCP was preparing the update. Review the current target and retry.",
		"GITOPS_TARGET_MISSING":        "The configured image target was not found. Correct the GitOps file selector and retry.",
		"DEPLOYMENT_AUTHORITY_CHANGED": "Deployment authority changed during this request. An administrator must review the configuration and reconcile this deployment.",
		"SUPERSEDED":                   "A newer deployment replaced this one.", "OPERATOR_CLOSED": "An operator closed this operation. Re-run deploy_build / queue_build when ready.", "STALE_TIMEOUT": "This operation was closed after remaining unchanged for two hours. The Forgejo run may continue; cancel it there if needed. Re-run deploy_build / queue_build when ready.", "REQUEST_KEY_CONFLICT": "This request key was already used for different deployment inputs. Use a fresh key."}
	if h, ok := hints[code]; ok {
		return h
	}
	if code == "" {
		return "Review the failure details and correct the underlying issue before retrying."
	}
	return "Operation failed with " + code + ". Review provider evidence and correct the cause before retrying."
}
func pipelineTime(b domain.Build) time.Time {
	if !b.QueuedAt.IsZero() {
		return b.QueuedAt
	}
	return b.CreatedAt
}
func pipelineCommitURL(b domain.Build) string {
	if b.SourceSHA == "" || b.Repository == "" || b.ConnectionAPIURL == "" {
		return ""
	}
	base := strings.TrimRight(b.ConnectionAPIURL, "/")
	u, e := url.Parse(base)
	if e != nil {
		return ""
	}
	if strings.EqualFold(u.Hostname(), "api.github.com") {
		return "https://github.com/" + strings.TrimLeft(b.Repository, "/") + "/commit/" + b.SourceSHA
	}
	base = strings.TrimSuffix(base, "/api/v1")
	return base + "/" + strings.TrimLeft(b.Repository, "/") + "/commit/" + b.SourceSHA
}
func overallPipeline(build domain.Build, stages []domain.PipelineStage) string {
	for _, s := range stages {
		if s.Kind == "checks" && s.Status == "failed" {
			return "attention"
		}
		if s.Status == "attention" {
			return "attention"
		}
	}
	if build.Status == "FAILED" {
		return "failed"
	}
	for i := range stages {
		if stages[i].Kind == "deploy" && stages[i].Status == "failed" {
			return "failed"
		}
	}
	for _, s := range stages {
		if s.Status == "running" {
			return "running"
		}
	}
	for _, s := range stages {
		if s.Status == "pending" {
			return "pending"
		}
	}
	if build.Status == "CANCELLED" {
		if build.SupersededBy != "" {
			return "superseded"
		}
		return "cancelled"
	}
	return "success"
}
func (s *Service) ListPipelines(ctx context.Context, in domain.ListPipelinesRequest) (domain.PipelinePage, error) {
	out := domain.PipelinePage{Items: []domain.Pipeline{}, Profiles: []string{}, Environments: []string{}}
	in.Query = strings.TrimSpace(in.Query)
	if in.ProductID == "" || in.Limit < 0 || in.Limit > 100 || utf8.RuneCountInString(in.Query) > 128 || in.Status != "" && !pipelineStatuses[in.Status] {
		return out, invalid("invalid pipeline list scope or bounds")
	}
	if e := authorize(ctx, "read", in.ProductID); e != nil {
		return out, e
	}
	if in.Limit == 0 {
		in.Limit = 20
	}
	canClose := authorize(ctx, "admin", "") == nil
	canCancel := authorize(ctx, "write", in.ProductID) == nil
	var after pipelineCursor
	if in.Cursor != "" {
		raw, e := base64.RawURLEncoding.DecodeString(in.Cursor)
		if len(in.Cursor) > 2048 || e != nil || json.Unmarshal(raw, &after) != nil || after.Product != in.ProductID || after.Profile != in.ProfileID || after.Status != in.Status || after.Environment != in.EnvironmentID || after.Query != in.Query {
			return out, invalid("invalid pipeline cursor")
		}
	}
	err := s.view(ctx, func(st *domain.State) error {
		byBuild := map[string][]domain.Deployment{}
		for _, d := range st.Deployments {
			if d.ProductID == in.ProductID {
				byBuild[d.BuildID] = append(byBuild[d.BuildID], copyDeployment(d))
				if d.EnvironmentID != "" {
					out.Environments = append(out.Environments, d.EnvironmentID)
				}
			}
		}
		profiles := map[string]bool{}
		items := []domain.Pipeline{}
		q := strings.ToLower(in.Query)
		for _, raw := range st.Builds {
			if raw.ProductID != in.ProductID {
				continue
			}
			b := copyBuild(raw)
			b.WaitingForBuildID = waitingBuildID(st.Builds, raw)
			profiles[b.ProfileID] = true
			ds := byBuild[b.ID]
			sort.Slice(ds, func(i, j int) bool {
				if !ds[i].CreatedAt.Equal(ds[j].CreatedAt) {
					return ds[i].CreatedAt.After(ds[j].CreatedAt)
				}
				return ds[i].ID > ds[j].ID
			})
			if in.ProfileID != "" && b.ProfileID != in.ProfileID {
				continue
			}
			if in.EnvironmentID != "" {
				found := false
				for _, d := range ds {
					if d.EnvironmentID == in.EnvironmentID {
						found = true
					}
				}
				if !found {
					continue
				}
			}
			if q != "" && !pipelineMatches(b, ds, q) {
				continue
			}
			stages := []domain.PipelineStage{buildPipelineStage(b, canClose, canCancel)}
			checks := buildChecksStage(b)
			if s.builds != nil {
				// Required (push) contexts exist only for the profile default branch.
				if prof, ok := s.builds.config.Profile(b.ProfileID); ok && b.Branch != "" && b.Branch != prof.DefaultBranch {
					checks.Status, checks.Label, checks.Hint = "skipped", "Not observed", "Checks are observed for the default branch only."
				}
			}
			stages = append(stages, checks)
			if b.Status == "SUCCEEDED" || b.SigningStatus != "" {
				ss := domain.PipelineStage{Kind: "sign", EntityID: b.ID, RawStatus: b.SigningStatus, Status: "skipped", Label: "Not required"}
				switch b.SigningStatus {
				case "PENDING":
					ss.Status = "pending"
					if b.Status == "SUCCEEDED" {
						ss.Status = "running"
						ss.Label = "Signing"
					} else {
						ss.Label = "Waiting for build"
					}
				case "SIGNED":
					ss.Status = "success"
					ss.Label = "Signed"
				case "PARTIAL":
					ss.Status = "attention"
					ss.Label = "Partially signed"
				case "FAILED":
					ss.Status = "failed"
					ss.Label = "Signing failed"
				case "SKIPPED":
					ss.Label = "Skipped"
				}
				stages = append(stages, ss)
			}
			if len(ds) == 0 {
				ss := domain.PipelineStage{Kind: "deploy", EnvironmentID: "dev", Status: "skipped", Label: "Not deployed"}
				if b.Status != "SUCCEEDED" && b.Status != "FAILED" && b.Status != "CANCELLED" {
					ss.Status = "pending"
					ss.Label = "Waiting for build"
				}
				stages = append(stages, ss)
			} else {
				latest := map[string]domain.Deployment{}
				for _, d := range ds {
					if prev, ok := latest[d.EnvironmentID]; !ok || d.CreatedAt.After(prev.CreatedAt) || d.CreatedAt.Equal(prev.CreatedAt) && d.ID > prev.ID {
						latest[d.EnvironmentID] = d
					}
				}
				envs := make([]string, 0, len(latest))
				for env := range latest {
					envs = append(envs, env)
				}
				sort.Strings(envs)
				for _, env := range envs {
					stages = append(stages, deploymentPipelineStage(latest[env], canClose))
				}
			}
			status := overallPipeline(b, stages)
			if in.Status != "" && status != in.Status {
				continue
			}
			at := pipelineTime(b)
			if in.Cursor != "" && (at.After(after.AfterTime) || at.Equal(after.AfterTime) && b.ID >= after.AfterID) {
				continue
			}
			pipe := domain.Pipeline{Build: b, Deployments: ds, Stages: stages, Status: status, CommitURL: pipelineCommitURL(b)}
			if !b.QueuedAt.IsZero() {
				v := b.QueuedAt
				pipe.StartedAt = &v
			}
			if status == "success" || status == "failed" || status == "cancelled" || status == "superseded" {
				var finished *time.Time
				if b.FinishedAt != nil {
					finished = b.FinishedAt
				}
				for _, d := range ds {
					if d.FinishedAt != nil && (finished == nil || d.FinishedAt.After(*finished)) {
						finished = d.FinishedAt
					}
				}
				if finished != nil {
					v := *finished
					pipe.FinishedAt = &v
					start := pipelineTime(b)
					pipe.DurationSeconds = int64(v.Sub(start).Seconds())
				}
			}
			items = append(items, pipe)
		}
		sort.Slice(items, func(i, j int) bool {
			a, b := pipelineTime(items[i].Build), pipelineTime(items[j].Build)
			if !a.Equal(b) {
				return a.After(b)
			}
			return items[i].Build.ID > items[j].Build.ID
		})
		for p := range profiles {
			out.Profiles = append(out.Profiles, p)
		}
		sort.Strings(out.Profiles)
		out.Environments = uniqueSorted(out.Environments)
		if len(items) > in.Limit {
			page := items[:in.Limit]
			last := page[len(page)-1].Build
			raw, _ := json.Marshal(pipelineCursor{in.ProductID, in.ProfileID, in.Status, in.EnvironmentID, in.Query, pipelineTime(last), last.ID})
			out.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
			items = page
		}
		out.Items = items
		return nil
	})
	return out, err
}
func uniqueSorted(in []string) []string {
	m := map[string]bool{}
	out := []string{}
	for _, v := range in {
		if v != "" && !m[v] {
			m[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
func pipelineMatches(b domain.Build, ds []domain.Deployment, q string) bool {
	fields := []string{b.ID, b.Branch, b.ProfileID, b.Repository, b.RequestKey}
	for _, x := range fields {
		if strings.Contains(strings.ToLower(x), q) {
			return true
		}
	}
	if strings.HasPrefix(strings.ToLower(b.SourceSHA), q) {
		return true
	}
	for _, im := range b.Images {
		if strings.Contains(strings.ToLower(im.Digest), q) {
			return true
		}
	}
	for _, d := range ds {
		if strings.Contains(strings.ToLower(d.ID), q) || strings.Contains(strings.ToLower(d.Branch), q) || strings.Contains(strings.ToLower(d.SourceSHA), q) {
			return true
		}
		pr := strconv.Itoa(d.PRNumber)
		if d.PRNumber > 0 && (strings.HasPrefix("#"+pr, q) || pr == q) {
			return true
		}
		for _, im := range d.Images {
			if strings.Contains(strings.ToLower(im.Digest), q) {
				return true
			}
		}
	}
	return false
}
