package github

import (
	"errors"
	"gopkg.in/yaml.v3"
	"io"
	"reflect"
	"releasecontrol/internal/delivery"
	"strings"
)

var ErrTargetMissing = errors.New("GITOPS_TARGET_MISSING")

type DeployTag struct {
	Repository, Value string
	MinMatches        int
	Format            string
}

// RenderDeployment reuses the GitOps scalar editor for configured image scalars. Reparsing compares the entire semantic tree after permitted edits.
func RenderDeployment(content string, tags []DeployTag) (string, error) {
	if len(content) > 1<<20 || len(tags) == 0 {
		return "", errors.New("invalid deployment content")
	}
	parse := func(s string) ([]*yaml.Node, error) {
		dec := yaml.NewDecoder(strings.NewReader(s))
		docs := []*yaml.Node{}
		for {
			var n yaml.Node
			e := dec.Decode(&n)
			if e == io.EOF {
				return docs, nil
			}
			if e != nil || len(n.Content) != 1 {
				return nil, errors.New("invalid deployment YAML")
			}
			var v any
			if n.Decode(&v) != nil {
				return nil, errors.New("ambiguous deployment YAML")
			}
			docs = append(docs, &n)
		}
	}
	docs, e := parse(content)
	if e != nil {
		return "", e
	}
	replacements := []imageReplacement{}
	selected := map[*yaml.Node]bool{}
	for _, t := range tags {
		if t.MinMatches < 1 || !validDeployTag(t.Value) {
			return "", errors.New("invalid deployment tag")
		}
		matches := []imageDocument{}
		if t.Format == "container_image" {
			if !strings.Contains(t.Value, "@") || strings.HasPrefix(t.Value, "sha256:") {
				return "", errors.New("container image requires tag and digest")
			}
			for _, doc := range docs {
				nodes, err := containerImageNodes(doc, t.Repository)
				if err != nil {
					return "", err
				}
				for _, n := range nodes {
					if selected[n] {
						return "", errors.New("overlapping image selectors")
					}
					selected[n] = true
					matches = append(matches, imageDocument{doc, n, n})
				}
			}
			if len(matches) < t.MinMatches {
				return "", ErrTargetMissing
			}
			replacements = append(replacements, imageReplacement{matches, t.Repository, t.Repository + ":" + t.Value})
			continue
		}
		if t.Format != "" && t.Format != "helm_values" {
			return "", errors.New("unknown deployment format")
		}
		selector := delivery.GitOpsRequest{ImageField: "spec.values.image.repository", DigestField: "spec.values.image.tag"}
		for _, doc := range docs {
			if !batchDocumentMatches(doc, selector) {
				continue
			}
			parent := doc.Content[0]
			flow := false
			for _, key := range []string{"spec", "values", "image", "repository"} {
				if parent == nil {
					break
				}
				if parent.Style&yaml.FlowStyle != 0 {
					flow = true
				}
				child, err := mappingValue(parent, key)
				if err != nil {
					parent = nil
					break
				}
				parent = child
			}
			if parent == nil || parent.Kind != yaml.ScalarNode || parent.Value != t.Repository {
				continue
			}
			if flow {
				return "", errors.New("flow deployment mapping unsupported")
			}
			repo, tag, err := documentImageFields(doc, selector)
			if err != nil {
				return "", err
			}
			if repo.Value != t.Repository {
				continue
			}
			if selected[tag] {
				return "", errors.New("overlapping image selectors")
			}
			selected[tag] = true
			// Only ordinary block mappings and scalar lines are writable.
			if tag.Line == repo.Line || tag.Style&(yaml.LiteralStyle|yaml.FoldedStyle|yaml.TaggedStyle) != 0 {
				return "", errors.New("unsupported deployment tag")
			}
			matches = append(matches, imageDocument{doc, repo, tag})
		}
		if len(matches) < t.MinMatches {
			return "", ErrTargetMissing
		}
		replacements = append(replacements, imageReplacement{matches, t.Repository, t.Value})
	}
	out, e := replaceImageSelectionsMode(content, replacements, true)
	if e != nil {
		return "", e
	}
	after, e := parse(out)
	if e != nil || len(after) != len(docs) {
		return "", errors.New("invalid rendered YAML")
	}
	for _, r := range replacements {
		for _, d := range r.docs {
			d.digest.Value = r.digest
			d.digest.Tag = "!!str"
		}
	}
	for i := range docs {
		var expected, actual any
		if docs[i].Decode(&expected) != nil || after[i].Decode(&actual) != nil || !reflect.DeepEqual(expected, actual) {
			return "", errors.New("unexpected YAML change")
		}
	}
	return out, nil
}
func validDeployTag(s string) bool {
	if len(s) > 256 || strings.ContainsAny(s, " \t\r\n\"'{}[],#&*!|>\\") {
		return false
	}
	if digestPattern.MatchString(s) {
		return true
	}
	tag, digest, ok := strings.Cut(s, "@")
	return ok && digestPattern.MatchString(digest) && len(tag) > 0 && len(tag) <= 128 && !strings.ContainsAny(tag, ":/")
}

// imageRepository removes the digest and a tag only after the last slash, so
// registry ports remain part of the repository identity.
func imageRepository(value string) string {
	value, _, _ = strings.Cut(value, "@")
	if colon := strings.LastIndex(value, ":"); colon > strings.LastIndex(value, "/") {
		value = value[:colon]
	}
	return value
}

func containerImageNodes(doc *yaml.Node, repository string) ([]*yaml.Node, error) {
	var result []*yaml.Node
	var walk func(*yaml.Node, bool) error
	walk = func(n *yaml.Node, unsafe bool) error {
		unsafe = unsafe || n.Style&yaml.FlowStyle != 0 || n.Anchor != "" || n.Kind == yaml.AliasNode
		if n.Kind == yaml.MappingNode {
			for i := 0; i < len(n.Content); i += 2 {
				key, value := n.Content[i], n.Content[i+1]
				if (key.Value == "containers" || key.Value == "initContainers") && value.Kind == yaml.SequenceNode {
					for _, container := range value.Content {
						image, err := mappingValue(container, "image")
						if err != nil {
							continue
						}
						if image.Kind != yaml.ScalarNode || imageRepository(image.Value) != repository {
							continue
						}
						if unsafe || value.Style&yaml.FlowStyle != 0 || container.Style&yaml.FlowStyle != 0 || container.Anchor != "" || image.Anchor != "" {
							return errors.New("unsupported container image mapping")
						}
						result = append(result, image)
					}
				}
			}
		}
		for _, child := range n.Content {
			if err := walk(child, unsafe); err != nil {
				return err
			}
		}
		return nil
	}
	err := walk(doc, false)
	return result, err
}
