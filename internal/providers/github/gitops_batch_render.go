package github

import (
	"encoding/json"
	"errors"
	"gopkg.in/yaml.v3"
	"io"
	"releasecontrol/internal/delivery"
	"sort"
	"strconv"
	"strings"
)

func validateGitOpsBatch(b delivery.GitOpsBatchApply) error {
	id, e := strconv.ParseInt(b.Repository, 10, 64)
	if e != nil || id <= 0 || strconv.FormatInt(id, 10) != b.Repository || !validSourceBranch(b.Ref) || !fullSHA.MatchString(b.ExpectedHeadSHA) || !operationID.MatchString(b.OperationID) || b.Ref == "rcp/gitops/"+b.OperationID || len(b.Message) > 4096 || len(b.Changes) == 0 || len(b.Changes) > 16 {
		return errors.New("invalid pinned GitOps batch")
	}
	raw, e := json.Marshal(b)
	if e != nil || len(raw) > 64<<10 {
		return errors.New("GitOps batch request oversized")
	}
	ids := map[string]bool{}
	paths := map[string]string{}
	for _, ch := range b.Changes {
		s := ch.Selector
		if !operationID.MatchString(ch.ID) || ids[ch.ID] || !fullSHA.MatchString(ch.ExpectedBlobSHA) || !digestPattern.MatchString(ch.Digest) || s.Repository != b.Repository || s.Ref != b.Ref || (s.ExpectedHeadSHA != "" && s.ExpectedHeadSHA != b.ExpectedHeadSHA) || len(strings.Split(s.Path, "/")) > 16 {
			return errors.New("invalid GitOps batch member")
		}
		ids[ch.ID] = true
		if e := gitopsPath(s); e != nil {
			return e
		}
		current, e := gitopsImageRepository(s.ImageRepository)
		if e != nil || current != s.ImageRepository {
			return errors.New("canonical current image required")
		}
		desired, e := gitopsImageRepository(ch.ImageRepository)
		if e != nil || desired != ch.ImageRepository {
			return errors.New("canonical desired image required")
		}
		if old, ok := paths[s.Path]; ok && old != ch.ExpectedBlobSHA {
			return ErrConflict
		}
		paths[s.Path] = ch.ExpectedBlobSHA
	}
	for path := range paths {
		for other := range paths {
			if strings.HasPrefix(path, other+"/") {
				return ErrConflict
			}
		}
	}
	return nil
}

// Every selector resolves against one original parse, before any replacements.
func renderGitOpsBatchFile(content string, changes []delivery.GitOpsImageChange) (string, error) {
	if len(content) > 1<<20 {
		return "", errors.New("GitOps file oversized")
	}
	decoder := yaml.NewDecoder(strings.NewReader(content))
	docs := []*yaml.Node{}
	for {
		var doc yaml.Node
		e := decoder.Decode(&doc)
		if e == io.EOF {
			break
		}
		if e != nil || len(doc.Content) != 1 {
			return "", errors.New("invalid GitOps YAML")
		}
		docs = append(docs, &doc)
	}
	selected := map[*yaml.Node]bool{}
	replacements := []imageReplacement{}
	finals := map[*yaml.Node]string{}
	for _, ch := range changes {
		s := ch.Selector
		matched := []imageDocument{}
		if s.ImageField != "spec.values.image.repository" && len(docs) != 1 {
			return "", errors.New("root mapping requires exactly one document")
		}
		for _, doc := range docs {
			if !batchDocumentMatches(doc, s) {
				continue
			}
			image, digest, e := documentImageFields(doc, s)
			if e != nil {
				return "", e
			}
			if image.Style&(yaml.LiteralStyle|yaml.FoldedStyle|yaml.TaggedStyle) != 0 || digest.Style&(yaml.LiteralStyle|yaml.FoldedStyle|yaml.TaggedStyle) != 0 {
				return "", errors.New("unsupported mapped image scalar")
			}
			if image.Value != s.ImageRepository {
				continue
			}
			if selected[image] || selected[digest] {
				return "", errors.New("overlapping GitOps selectors")
			}
			if !digestPattern.MatchString(digestValue(digest.Value, s)) {
				return "", errors.New("invalid current digest")
			}
			if len(matched) > 0 && matched[0].digest.Value != digest.Value {
				return "", errors.New("inconsistent selected image versions")
			}
			selected[image] = true
			selected[digest] = true
			matched = append(matched, imageDocument{doc, image, digest})
			finals[image] = ch.ImageRepository
		}
		if len(matched) == 0 {
			return "", errors.New("no current image selector match")
		}
		value := ch.Digest
		if s.DigestField == "spec.values.image.tag" {
			value = "rcp@" + value
		}
		replacements = append(replacements, imageReplacement{matched, ch.ImageRepository, value})
	}
	// A desired selector must select precisely its original document set.
	for i, ch := range changes {
		for _, doc := range docs {
			if !batchDocumentMatches(doc, ch.Selector) {
				continue
			}
			image, _, e := documentImageFields(doc, ch.Selector)
			if e != nil {
				return "", e
			}
			final := image.Value
			if value, ok := finals[image]; ok {
				final = value
			}
			originalMember := false
			for _, d := range replacements[i].docs {
				if d.doc == doc {
					originalMember = true
				}
			}
			if (final == ch.ImageRepository) != originalMember {
				return "", errors.New("desired GitOps selector collision")
			}
		}
	}
	out, e := replaceImageSelections(content, replacements)
	if e != nil {
		return "", e
	}
	if len(out) > 1<<20 {
		return "", errors.New("rendered GitOps file oversized")
	}
	return out, nil
}
func batchDocumentMatches(doc *yaml.Node, s delivery.GitOpsRequest) bool {
	if s.ImageField != "spec.values.image.repository" {
		return true
	}
	kind, e := mappingValue(doc.Content[0], "kind")
	if e != nil || kind.Value != "HelmRelease" {
		return false
	}
	api, e := mappingValue(doc.Content[0], "apiVersion")
	return e == nil && strings.HasPrefix(api.Value, "helm.toolkit.fluxcd.io/")
}
func batchPaths(b delivery.GitOpsBatchApply) []string {
	m := map[string]bool{}
	for _, ch := range b.Changes {
		m[ch.Selector.Path] = true
	}
	out := []string{}
	for p := range m {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
func batchChanges(b delivery.GitOpsBatchApply, path string) []delivery.GitOpsImageChange {
	out := []delivery.GitOpsImageChange{}
	for _, ch := range b.Changes {
		if ch.Selector.Path == path {
			out = append(out, ch)
		}
	}
	return out
}
