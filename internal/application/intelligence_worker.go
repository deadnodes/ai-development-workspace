package application

import (
	"encoding/json"
	"reflect"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/intelligencepatch"
	"releasecontrol/internal/workerconfig"
	"slices"
)

func (s *Service) intelligencePatch(st *domain.State, j *domain.WorkerJob) (json.RawMessage, error) {
	b := j.Inputs.IntelligencePatch
	if b == nil {
		return nil, nil
	}
	op := operationByID(st, b.TaskID)
	if op == nil || op.ProductID != j.ProductID || op.Status != "SUCCEEDED" || op.Intelligence == nil || op.Intelligence.Kind != "conflict_patch" || op.Intelligence.ResultFingerprint != b.ResultFingerprint || j.Capability != "source_check" || j.Inputs.Check != nil || j.Inputs.SourceSHA != b.HeadSHA || j.Inputs.Execution == nil || j.Inputs.Execution.Kind != "source_check" {
		return nil, invalid("patch worker binding invalid")
	}
	in, e := s.taskInput(st, *op)
	if e != nil {
		return nil, e
	}
	var result intelligenceResult
	if s.intelligence.Open(intelligenceAAD(*op, "result"), op.Intelligence.ResultCiphertext, &result) != nil || s.intelligence.Fingerprint(result) != b.ResultFingerprint || s.intelligence.ValidateOutput(result) != nil || result.Decision != "patch_proposed" || result.Patch == nil {
		return nil, invalid("patch result unavailable")
	}
	if intelligencepatch.Validate(*result.Patch, b.BaseSHA, b.HeadSHA, b.AllowedPaths) != nil {
		return nil, invalid("patch no longer admitted")
	}
	var context struct {
		RepositoryID string `json:"repository_id"`
	}
	if json.Unmarshal(in.Context, &context) != nil || context.RepositoryID != j.Inputs.RepositoryID {
		return nil, invalid("patch repository mismatch")
	}
	r, ok := s.externalWorkers.Recipe(j.Inputs.RecipeID)
	if !ok || r.Capability != "source_check" || r.Trust != "trusted_source" || r.Check != nil || !reflect.DeepEqual(r.IntelligencePaths, b.AllowedPaths) {
		return nil, invalid("patch check recipe not enrolled")
	}
	return json.Marshal(result.Patch)
}

func (s *Service) bindIntelligencePatch(st *domain.State, recipe workerconfig.Recipe, taskID, product, sourceSHA, integrationID string) (*domain.IntelligencePatchBinding, error) {
	if taskID == "" {
		return nil, nil
	}
	if recipe.Capability != "source_check" || recipe.Trust != "trusted_source" || recipe.Check != nil || len(recipe.IntelligencePaths) == 0 {
		return nil, invalid("model patches require an explicit nonpublishing source_check recipe")
	}
	op := operationByID(st, taskID)
	if op == nil || op.ProductID != product || op.Status != "SUCCEEDED" || op.Intelligence == nil || op.Intelligence.Kind != "conflict_patch" {
		return nil, invalid("verified model task required")
	}
	input, e := s.taskInput(st, *op)
	if e != nil {
		return nil, e
	}
	var result intelligenceResult
	if s.intelligence.Open(intelligenceAAD(*op, "result"), op.Intelligence.ResultCiphertext, &result) != nil || s.intelligence.ValidateOutput(result) != nil || result.Patch == nil || result.Decision != "patch_proposed" || result.Patch.HeadSHA != sourceSHA {
		return nil, invalid("model patch source mismatch")
	}
	matched := false
	for _, c := range st.CompositionConflicts {
		if c.ID == input.Request.ConflictID && c.RepositoryID == recipe.RepositoryID {
			for _, v := range c.Integrations {
				if v.ID == integrationID {
					matched = true
				}
			}
		}
	}
	if !matched {
		return nil, invalid("worker integration is not a conflict participant")
	}
	if intelligencepatch.Validate(*result.Patch, result.Patch.BaseSHA, sourceSHA, recipe.IntelligencePaths) != nil {
		return nil, invalid("patch exceeds worker path grant")
	}
	for _, path := range recipe.IntelligencePaths {
		if !slices.Contains(s.intelligence.Paths(recipe.RepositoryID), path) {
			return nil, invalid("patch path authority mismatch")
		}
	}
	return &domain.IntelligencePatchBinding{TaskID: taskID, ResultFingerprint: op.Intelligence.ResultFingerprint, BaseSHA: result.Patch.BaseSHA, HeadSHA: sourceSHA, AllowedPaths: append([]string(nil), recipe.IntelligencePaths...)}, nil
}

func (s *Service) attachIntelligencePatch(st *domain.State, lease *domain.WorkerLease) error {
	if lease == nil || lease.Job.Inputs.IntelligencePatch == nil {
		return nil
	}
	raw, e := s.intelligencePatch(st, &lease.Job)
	if e != nil {
		return e
	}
	lease.IntelligencePatch = raw
	return nil
}
