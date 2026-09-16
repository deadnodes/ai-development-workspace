package github

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"releasecontrol/internal/delivery"
)

var operationID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var imageTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

func validateBuild(r delivery.BuildRequest) error {
	if !fullSHA.MatchString(r.SourceSHA) || !operationID.MatchString(r.OperationID) || !imageTag.MatchString(r.ImageTag) || r.Ref == "" || r.Workflow == "" {
		return errors.New("build requires full source SHA, safe operation ID/image tag, workflow and ref")
	}
	if r.WorkflowSHA != "" && !fullSHA.MatchString(r.WorkflowSHA) {
		return errors.New("workflow pin must be full SHA")
	}
	if len(r.Inputs) > 20 {
		return errors.New("too many workflow inputs")
	}
	return nil
}
func (p *Provider) Dispatch(ctx context.Context, c delivery.Connection, r delivery.BuildRequest) (delivery.DispatchResult, error) {
	result := delivery.DispatchResult{Correlation: "rcp-" + r.OperationID}
	if e := validateBuild(r); e != nil {
		return result, e
	}
	repo, e := p.repository(ctx, c, r.Repository)
	if e != nil {
		return result, e
	}
	// Catch ordinary moved-branch races before dispatch. Workflow must independently
	// checkout and report SourceSHA: workflow_dispatch itself cannot atomically pin a branch.
	var commit struct {
		SHA string `json:"sha"`
	}
	if e = p.request(ctx, c, "GET", "/repos/"+repo+"/commits/"+url.PathEscape(r.Ref), nil, &commit); e != nil {
		return result, e
	}
	if r.WorkflowSHA != "" && commit.SHA != r.WorkflowSHA {
		return result, ErrConflict
	}
	inputs := map[string]string{}
	for k, v := range r.Inputs {
		inputs[k] = v
	}
	inputs["source_sha"] = r.SourceSHA
	inputs["image_tag"] = r.ImageTag
	inputs["operation_id"] = r.OperationID
	base, e := apiBase(c)
	if e != nil {
		return result, e
	}
	token, e := p.token(ctx, c)
	if e != nil {
		return result, e
	}
	b, _, status, e := p.do(ctx, "POST", base+"/repos/"+repo+"/actions/workflows/"+url.PathEscape(r.Workflow)+"/dispatches", token, map[string]any{"ref": r.Ref, "inputs": inputs, "return_run_details": true})
	if e != nil {
		return result, e
	}
	if status != 204 {
		var response struct {
			ID int64 `json:"workflow_run_id"`
		}
		if json.Unmarshal(b, &response) != nil || response.ID <= 0 {
			return result, errors.New("invalid dispatch run response; reconcile by operation ID before retry")
		}
		result.ProviderRunID = response.ID
	}
	return result, nil
}
func (p *Provider) FindRun(ctx context.Context, c delivery.Connection, r delivery.BuildRequest) (delivery.RunResult, error) {
	var found delivery.RunResult
	if e := validateBuild(r); e != nil {
		return found, e
	}
	repo, e := p.repository(ctx, c, r.Repository)
	if e != nil {
		return found, e
	}
	for page := 1; page <= 10; page++ {
		var response struct {
			Runs []struct {
				ID           int64     `json:"id"`
				DisplayTitle string    `json:"display_title"`
				Status       string    `json:"status"`
				Conclusion   string    `json:"conclusion"`
				HeadSHA      string    `json:"head_sha"`
				URL          string    `json:"html_url"`
				CreatedAt    time.Time `json:"created_at"`
			} `json:"workflow_runs"`
		}
		path := fmt.Sprintf("/repos/%s/actions/workflows/%s/runs?event=workflow_dispatch&per_page=100&page=%d", repo, url.PathEscape(r.Workflow), page)
		if e = p.request(ctx, c, "GET", path, nil, &response); e != nil {
			return found, e
		}
		for _, run := range response.Runs {
			if run.DisplayTitle != "rcp-"+r.OperationID {
				continue
			}
			if !r.RequestedAt.IsZero() && run.CreatedAt.Before(r.RequestedAt.Add(-time.Minute)) {
				continue
			}
			if found.Found && found.ID != run.ID {
				return delivery.RunResult{}, errors.New("ambiguous workflow operation: multiple matching runs")
			}
			if r.WorkflowSHA != "" && run.HeadSHA != r.WorkflowSHA {
				return delivery.RunResult{}, errors.New("workflow definition SHA differs from pinned intent")
			}
			found = delivery.RunResult{Found: true, ID: run.ID, Status: run.Status, Conclusion: run.Conclusion, HeadSHA: run.HeadSHA, URL: run.URL, Artifacts: map[string]string{}}
		}
		if len(response.Runs) < 100 {
			break
		}
		if page == 10 {
			return delivery.RunResult{}, errors.New("workflow reconciliation pagination limit exceeded")
		}
	}
	if found.Found && found.Status == "completed" && found.Conclusion == "success" {
		report, e := p.resultReport(ctx, c, repo, found.ID, r.OperationID)
		if e != nil {
			return found, e
		}
		if report.SourceSHA != r.SourceSHA || report.ImageTag != r.ImageTag {
			return found, errors.New("workflow report source/tag mismatch")
		}
		found.Artifacts = report.fields()
	}
	return found, nil
}

type workflowReport struct {
	OperationID     string    `json:"operation_id"`
	SourceSHA       string    `json:"source_sha"`
	ImageRepository string    `json:"image_repository"`
	ImageTag        string    `json:"image_tag"`
	Digest          string    `json:"digest"`
	Available       bool      `json:"available"`
	ObservedAt      time.Time `json:"observed_at"`
}

func (r workflowReport) fields() map[string]string {
	return map[string]string{"operation_id": r.OperationID, "source_sha": r.SourceSHA, "image_repository": r.ImageRepository, "image_tag": r.ImageTag, "digest": r.Digest, "available": fmt.Sprint(r.Available), "observed_at": r.ObservedAt.Format(time.RFC3339Nano)}
}
func (p *Provider) resultReport(ctx context.Context, c delivery.Connection, repo string, runID int64, operation string) (workflowReport, error) {
	var report workflowReport
	if !operationID.MatchString(operation) || runID <= 0 {
		return report, errors.New("invalid workflow evidence locator")
	}
	// Verify the locator refers to the correlated completed run, not an arbitrary artifact.
	var run struct {
		ID           int64  `json:"id"`
		DisplayTitle string `json:"display_title"`
		Status       string `json:"status"`
		Conclusion   string `json:"conclusion"`
	}
	if e := p.request(ctx, c, "GET", fmt.Sprintf("/repos/%s/actions/runs/%d", repo, runID), nil, &run); e != nil {
		return report, e
	}
	if run.ID != runID || run.DisplayTitle != "rcp-"+operation || run.Status != "completed" || run.Conclusion != "success" {
		return report, errors.New("workflow evidence is not a successful correlated run")
	}
	var artifactID int64
	for page := 1; page <= 100; page++ {
		var response struct {
			Artifacts []struct {
				ID      int64  `json:"id"`
				Name    string `json:"name"`
				Expired bool   `json:"expired"`
			} `json:"artifacts"`
		}
		if e := p.request(ctx, c, "GET", fmt.Sprintf("/repos/%s/actions/runs/%d/artifacts?per_page=100&page=%d", repo, runID, page), nil, &response); e != nil {
			return report, e
		}
		for _, a := range response.Artifacts {
			if a.Name == "rcp-result-"+operation && !a.Expired {
				if artifactID != 0 {
					return report, errors.New("ambiguous workflow result artifact")
				}
				artifactID = a.ID
			}
		}
		if len(response.Artifacts) < 100 {
			break
		}
		if page == 100 {
			return report, errors.New("artifact pagination limit exceeded")
		}
	}
	if artifactID <= 0 {
		return report, errors.New("workflow result artifact unavailable")
	}
	base, e := apiBase(c)
	if e != nil {
		return report, e
	}
	token, e := p.token(ctx, c)
	if e != nil {
		return report, e
	}
	body, headers, status, e := p.do(ctx, "GET", fmt.Sprintf("%s/repos/%s/actions/artifacts/%d/zip", base, repo, artifactID), token, nil)
	if status == 302 {
		target := headers.Get("Location")
		u, parseErr := url.Parse(target)
		if parseErr != nil || u.Scheme != "https" || u.User != nil || !(strings.HasSuffix(u.Hostname(), ".blob.core.windows.net") || strings.HasSuffix(u.Hostname(), ".actions.githubusercontent.com")) {
			return report, errors.New("untrusted artifact download destination")
		}
		body, _, _, e = p.do(ctx, "GET", target, "", nil)
	}
	if e != nil {
		return report, e
	}
	archive, e := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if e != nil {
		return report, errors.New("invalid workflow result archive")
	}
	count := 0
	for _, f := range archive.File {
		if f.Name != "result.json" {
			continue
		}
		count++
		if count > 1 || f.UncompressedSize64 > 65536 {
			return report, errors.New("invalid workflow report size/count")
		}
		reader, e := f.Open()
		if e != nil {
			return report, errors.New("cannot read workflow report")
		}
		b, e := io.ReadAll(io.LimitReader(reader, 65537))
		reader.Close()
		var fields map[string]json.RawMessage
		if json.Unmarshal(b, &fields) != nil || (string(fields["available"]) != "true" && string(fields["available"]) != "false") {
			return report, errors.New("workflow report requires explicit availability observation")
		}
		if e != nil || len(b) > 65536 || json.Unmarshal(b, &report) != nil {
			return report, errors.New("invalid workflow report JSON")
		}
	}
	if count != 1 || report.OperationID != operation || !fullSHA.MatchString(report.SourceSHA) || !imageTag.MatchString(report.ImageTag) || report.ObservedAt.IsZero() || report.ObservedAt.After(p.now().Add(time.Minute)) {
		return report, errors.New("invalid workflow result provenance")
	}
	if _, e = registryRepository(report.ImageRepository); e != nil {
		return report, e
	}
	if report.Available && !digestPattern.MatchString(report.Digest) {
		return report, errors.New("workflow report missing immutable digest")
	}
	return report, nil
}
