package application

import "releasecontrol/internal/domain"

// Only provider-observed, successful build provenance may become deployment input.
// Tags supplied by callers are not evidence of source identity.
func validatedExistingArtifact(st *domain.State, id, product, app, repo, sha, image string) (*domain.DeliveryArtifact, error) {
	for _, artifact := range st.DeliveryArtifacts {
		if artifact.ID != id {
			continue
		}
		if sha == "" || artifact.ProductID != product || artifact.ApplicationID != app || artifact.RepositoryID != repo || artifact.SourceCommit != sha || artifact.ImageRepository != image || !validArtifactDigest(artifact.Digest) || artifact.BuildRunID <= 0 || artifact.Availability != "PRESENT" {
			return nil, invalid("artifact must have successful build provenance for exact product/component/repository/revision")
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
