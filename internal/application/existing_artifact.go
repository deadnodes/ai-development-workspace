package application

import (
	"reflect"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
)

// Only provider-observed, successful build provenance may become deployment input.
// Tags supplied by callers are not evidence of source identity.
func validatedExistingArtifact(st *domain.State, id, product, app, repo, sha, image string) (*domain.DeliveryArtifact, error) {
	for _, artifact := range st.DeliveryArtifacts {
		if artifact.ID != id {
			continue
		}
		if sha == "" || artifact.ProductID != product || artifact.ApplicationID != app || artifact.RepositoryID != repo || artifact.SourceCommit != sha || artifact.ImageRepository != image || !validArtifactDigest(artifact.Digest) || artifact.Availability != "PRESENT" {
			return nil, invalid("artifact must have successful build provenance for exact product/component/repository/revision")
		}
		if artifact.Provenance == "NATIVE_WORKER" {
			if nativeQuarantined(st, product) || artifact.Native == nil || artifact.BuildRunID != 0 || artifact.Tag != "" || !reflect.DeepEqual(artifact.BuildRequest, delivery.BuildRequest{}) {
				return nil, invalid("invalid or quarantined native provenance")
			}
			n := artifact.Native
			j := workerJob(st, n.PublicationJobID)
			a := workerAttempt(st, n.PublicationAttemptID)
			if j == nil || a == nil || j.Status != "SUCCEEDED" || j.Capability != "registry_publish" || artifact.OperationID != j.OperationID || a.Fence != j.Fence || a.InputHash != j.InputHash || j.ProductID != product || j.InputHash != n.PublicationInputHash || a.JobID != j.ID || j.Inputs.Publication == nil || j.Inputs.Publication.ComponentID != app || j.Inputs.SourceSHA != sha || j.Inputs.RepositoryID != repo || j.Inputs.Publication.Profile != n.Profile || !reflect.DeepEqual(j.Inputs.Publication.Receipt, n.Receipt) || n.Receipt.ManifestDigest != artifact.Digest || n.Observation.ManifestDigest != artifact.Digest || n.Observation.Platform != n.Receipt.Platform || n.Observation.ObservedAt.IsZero() {
				return nil, invalid("invalid native publication evidence")
			}
			if n.Reconciliation != nil {
				if a.Status != "EXPIRED" || a.Report != nil || n.Reconciliation.Actor == "" || n.Reconciliation.At.IsZero() {
					return nil, invalid("invalid native reconciliation evidence")
				}
			} else if a.Status != "SUCCEEDED" || a.Report == nil || a.Report.Publication == nil || a.Report.Result != "PASSED" || a.Report.Publication.ManifestDigest != artifact.Digest {
				return nil, invalid("invalid native worker report")
			}
			copy := artifact
			return &copy, nil
		}
		if artifact.Native != nil || artifact.Provenance != "" && artifact.Provenance != "GITHUB_ACTIONS" || artifact.BuildRunID <= 0 {
			return nil, invalid("mixed artifact provenance")
		}
		for _, build := range st.DeliveryBuildRuns {
			if build.ProductID == product && build.ApplicationID == app && build.Result.ID == artifact.BuildRunID && build.Result.Status == "completed" && build.Result.Conclusion == "success" && build.Request.OperationID == artifact.BuildRequest.OperationID && build.Request.Repository == artifact.BuildRequest.Repository && build.Request.SourceSHA == sha && build.Result.Artifacts["source_sha"] == sha && build.Result.Artifacts["image_repository"] == image && build.Result.Artifacts["digest"] == artifact.Digest {
				copy := artifact
				return &copy, nil
			}
		}
		return nil, invalid("artifact has no matching successful observed workflow run")
	}
	return nil, missing("artifact", id)
}
