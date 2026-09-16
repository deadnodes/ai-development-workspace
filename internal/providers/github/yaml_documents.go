package github

import (
	"encoding/json"
	"errors"
	"gopkg.in/yaml.v3"
	"io"
	"releasecontrol/internal/delivery"
	"sort"
	"strings"
)

type imageDocument struct{ doc, image, digest *yaml.Node }

func imageDocuments(content string, r delivery.GitOpsRequest) ([]imageDocument, error) {
	decoder := yaml.NewDecoder(strings.NewReader(content))
	out := []imageDocument{}
	count := 0
	desired, e := registryRepository(r.ImageRepository)
	if e != nil {
		return nil, e
	}
	desired = "ghcr.io/" + desired
	for {
		var doc yaml.Node
		e := decoder.Decode(&doc)
		if e == io.EOF {
			break
		}
		if e != nil || len(doc.Content) != 1 {
			return nil, errors.New("invalid GitOps YAML")
		}
		count++
		if r.ImageField == "spec.values.image.repository" {
			kind, e := mappingValue(doc.Content[0], "kind")
			if e != nil || kind.Value != "HelmRelease" {
				continue
			}
			api, e := mappingValue(doc.Content[0], "apiVersion")
			if e != nil || !strings.HasPrefix(api.Value, "helm.toolkit.fluxcd.io/") {
				continue
			}
		}
		image, digest, e := documentImageFields(&doc, r)
		if e != nil {
			return nil, e
		}
		if r.ImageField == "spec.values.image.repository" && image.Value != desired {
			continue
		}
		if len(out) > 0 && (out[0].image.Value != image.Value || out[0].digest.Value != digest.Value) {
			return nil, errors.New("matching GitOps resources have inconsistent image versions")
		}
		out = append(out, imageDocument{&doc, image, digest})
	}
	if r.ImageField != "spec.values.image.repository" && count != 1 {
		return nil, errors.New("root image mapping requires exactly one document")
	}
	if len(out) == 0 {
		return nil, errors.New("no matching GitOps image resources")
	}
	return out, nil
}

// Replace scalar tokens in-place so unrelated resources, comments and formatting
// remain byte-for-byte intact. Multiline/anchored/tagged/flow tokens fail closed.
func replaceImageScalars(content string, docs []imageDocument, repo, digest string) (string, error) {
	type edit struct {
		start, end int
		value      string
	}
	edits := []edit{}
	for _, doc := range docs {
		for i, n := range []*yaml.Node{doc.image, doc.digest} {
			if n.Style&(yaml.LiteralStyle|yaml.FoldedStyle|yaml.TaggedStyle) != 0 {
				return "", errors.New("unsupported image scalar style")
			}
			start := 0
			for line := 1; line < n.Line; line++ {
				p := strings.IndexByte(content[start:], '\n')
				if p < 0 {
					return "", errors.New("invalid image position")
				}
				start += p + 1
			}
			start += n.Column - 1
			if start >= len(content) {
				return "", errors.New("invalid image position")
			}
			end := start
			if content[start] == '\'' || content[start] == '"' {
				quote := content[start]
				end++
				for end < len(content) {
					if content[end] == '\n' {
						return "", errors.New("multiline image scalar unsupported")
					}
					if quote == '"' && content[end] == '\\' {
						end += 2
						continue
					}
					if content[end] == quote {
						if quote == '\'' && end+1 < len(content) && content[end+1] == '\'' {
							end += 2
							continue
						}
						end++
						break
					}
					end++
				}
			} else {
				for end < len(content) && !strings.ContainsRune(" \t\r\n,}]", rune(content[end])) {
					end++
				}
			}
			var old string
			if end > len(content) || yaml.Unmarshal([]byte(content[start:end]), &old) != nil || old != n.Value {
				return "", errors.New("unsupported image scalar token")
			}
			value := repo
			if i == 1 {
				value = digest
			}
			encoded, _ := json.Marshal(value)
			edits = append(edits, edit{start, end, string(encoded)})
		}
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	last := len(content)
	for _, e := range edits {
		if e.end > last {
			return "", errors.New("overlapping image fields")
		}
		content = content[:e.start] + e.value + content[e.end:]
		last = e.start
	}
	return content, nil
}
