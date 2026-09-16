package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
	"releasecontrol/internal/delivery"
)

func gitopsPath(r delivery.GitOpsRequest) error {
	if r.Path == "" || path.IsAbs(r.Path) || path.Clean(r.Path) != r.Path || r.Path == ".." || strings.HasPrefix(r.Path, "../") || strings.Contains(r.Path, "\\") || r.Ref == "" {
		return errors.New("GitOps requires branch and clean repository-relative file path")
	}
	if !((r.ImageField == "" || r.ImageField == "image.repository") && (r.DigestField == "" || r.DigestField == "image.digest")) && !(r.ImageField == "spec.values.image.repository" && r.DigestField == "spec.values.image.tag") {
		return fmt.Errorf("%w: mapping supports root image.repository/digest or HelmRelease spec.values.image.repository/tag only", ErrUnsupported)
	}
	_, e := registryRepository(r.ImageRepository)
	return e
}
func mappingValue(n *yaml.Node, key string) (*yaml.Node, error) {
	if n.Kind != yaml.MappingNode {
		return nil, errors.New("GitOps YAML field must be a mapping")
	}
	var found *yaml.Node
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			if found != nil {
				return nil, errors.New("duplicate GitOps YAML key")
			}
			found = n.Content[i+1]
		}
	}
	if found == nil {
		return nil, errors.New("required GitOps YAML field missing")
	}
	return found, nil
}
func imageFields(content string, request delivery.GitOpsRequest) (*yaml.Node, *yaml.Node, *yaml.Node, error) {
	var doc yaml.Node
	decoder := yaml.NewDecoder(strings.NewReader(content))
	if e := decoder.Decode(&doc); e != nil || len(doc.Content) != 1 {
		return nil, nil, nil, errors.New("invalid GitOps YAML")
	}
	var extra yaml.Node
	if decoder.Decode(&extra) != io.EOF {
		return nil, nil, nil, errors.New("GitOps supports exactly one YAML document")
	}
	parent := doc.Content[0]
	if request.ImageField == "spec.values.image.repository" {
		for _, key := range []string{"spec", "values"} {
			var err error
			parent, err = mappingValue(parent, key)
			if err != nil {
				return nil, nil, nil, err
			}
		}
	}
	image, e := mappingValue(parent, "image")
	if e != nil {
		return nil, nil, nil, e
	}
	repo, e := mappingValue(image, "repository")
	if e != nil {
		return nil, nil, nil, e
	}
	digestKey := "digest"
	if request.DigestField == "spec.values.image.tag" {
		digestKey = "tag"
	}
	digest, e := mappingValue(image, digestKey)
	if e != nil {
		return nil, nil, nil, e
	}
	if repo.Kind != yaml.ScalarNode || digest.Kind != yaml.ScalarNode || repo.Anchor != "" || digest.Anchor != "" {
		return nil, nil, nil, errors.New("GitOps image fields must be direct scalar values")
	}
	return &doc, repo, digest, nil
}
func (p *Provider) ReadGitOps(ctx context.Context, c delivery.Connection, r delivery.GitOpsRequest) (delivery.GitOpsSnapshot, error) {
	var out delivery.GitOpsSnapshot
	if e := gitopsPath(r); e != nil {
		return out, e
	}
	repo, e := p.repository(ctx, c, r.Repository)
	if e != nil {
		return out, e
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if e = p.request(ctx, c, "GET", "/repos/"+repo+"/git/ref/heads/"+url.PathEscape(r.Ref), nil, &ref); e != nil {
		return out, e
	}
	if !fullSHA.MatchString(ref.Object.SHA) {
		return out, errors.New("invalid GitOps branch head")
	}
	if r.ExpectedHeadSHA != "" && ref.Object.SHA != r.ExpectedHeadSHA {
		return out, ErrConflict
	}
	var file struct {
		Type     string `json:"type"`
		SHA      string `json:"sha"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if e = p.request(ctx, c, "GET", "/repos/"+repo+"/contents/"+url.PathEscape(r.Path)+"?ref="+url.QueryEscape(ref.Object.SHA), nil, &file); e != nil {
		return out, e
	}
	if file.Type != "file" || file.Encoding != "base64" || !fullSHA.MatchString(file.SHA) {
		return out, errors.New("unsupported GitOps file response")
	}
	raw, e := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if e != nil || len(raw) > 1<<20 {
		return out, errors.New("invalid or oversized GitOps file")
	}
	_, image, digest, e := imageFields(string(raw), r)
	if e != nil {
		return out, e
	}
	return delivery.GitOpsSnapshot{ImageRepository: image.Value, HeadSHA: ref.Object.SHA, BlobSHA: file.SHA, Content: string(raw), Digest: digestValue(digest.Value, r)}, nil
}
func (p *Provider) ApplyGitOps(ctx context.Context, c delivery.Connection, r delivery.GitOpsApply) (delivery.GitOpsResult, error) {
	var out delivery.GitOpsResult
	if !fullSHA.MatchString(r.ExpectedHeadSHA) || !fullSHA.MatchString(r.ExpectedBlobSHA) || !digestPattern.MatchString(r.Digest) || !operationID.MatchString(r.OperationID) {
		return out, errors.New("GitOps mutation requires full expected head/blob, digest and operation ID")
	}
	request := r.Request
	request.ExpectedHeadSHA = r.ExpectedHeadSHA
	snapshot, e := p.ReadGitOps(ctx, c, request)
	if e != nil {
		return out, e
	}
	if snapshot.BlobSHA != r.ExpectedBlobSHA {
		return out, ErrConflict
	}
	doc, image, digest, e := imageFields(snapshot.Content, request)
	if e != nil {
		return out, e
	}
	desiredRepo, _ := registryRepository(request.ImageRepository)
	desiredRepo = "ghcr.io/" + desiredRepo
	if image.Value == desiredRepo && digestValue(digest.Value, request) == r.Digest {
		return delivery.GitOpsResult{CommitSHA: snapshot.HeadSHA, BlobSHA: snapshot.BlobSHA, AlreadyApplied: true}, nil
	}
	image.Value = desiredRepo
	image.Tag = "!!str"
	digest.Value = r.Digest
	if request.DigestField == "spec.values.image.tag" {
		digest.Value = "rcp@" + r.Digest
	}
	digest.Tag = "!!str"
	var b bytes.Buffer
	encoder := yaml.NewEncoder(&b)
	encoder.SetIndent(2)
	if e = encoder.Encode(doc); e != nil {
		return out, errors.New("cannot encode GitOps YAML")
	}
	encoder.Close()
	repo, e := p.repository(ctx, c, request.Repository)
	if e != nil {
		return out, e
	}
	message := r.Message
	if message == "" {
		message = "Release Control Plane image update"
	}
	message += " [rcp:" + r.OperationID + "]"
	var response struct {
		Content struct {
			SHA string `json:"sha"`
		} `json:"content"`
		Commit struct {
			SHA string `json:"sha"`
			URL string `json:"html_url"`
		} `json:"commit"`
	}
	e = p.request(ctx, c, "PUT", "/repos/"+repo+"/contents/"+url.PathEscape(request.Path), map[string]any{"branch": request.Ref, "sha": r.ExpectedBlobSHA, "content": base64.StdEncoding.EncodeToString(b.Bytes()), "message": message}, &response)
	if e != nil {
		return out, e
	}
	if !fullSHA.MatchString(response.Commit.SHA) || !fullSHA.MatchString(response.Content.SHA) {
		return out, errors.New("invalid GitOps mutation response; reconcile before retry")
	}
	return delivery.GitOpsResult{CommitSHA: response.Commit.SHA, BlobSHA: response.Content.SHA, URL: response.Commit.URL}, nil
}

func digestValue(value string, r delivery.GitOpsRequest) string {
	if r.DigestField == "spec.values.image.tag" {
		_, digest, ok := strings.Cut(value, "@")
		if !ok || !digestPattern.MatchString(digest) {
			return ""
		}
		return digest
	}
	return value
}
