package registryobserver

import (
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var nextLink = regexp.MustCompile(`^<([^<>]+)>;\s*rel="next"$`)

func (s *scanner) next(link, kind, ref string) (url.Values, bool) {
	if link == "" {
		return nil, true
	}
	m := nextLink.FindStringSubmatch(strings.TrimSpace(link))
	if m == nil {
		s.block("registry_pagination")
		return nil, false
	}
	base, _ := url.Parse(s.recipe.Origin + "/v2/" + s.recipe.Repository + "/" + kind + "/" + ref)
	u, e := url.Parse(m[1])
	if e != nil {
		s.block("registry_pagination")
		return nil, false
	}
	u = base.ResolveReference(u)
	if u.Scheme != base.Scheme || u.Host != base.Host || u.Path != base.Path || u.RawPath != "" || u.User != nil || u.Fragment != "" {
		s.block("registry_pagination")
		return nil, false
	}
	q, e := url.ParseQuery(u.RawQuery)
	if e != nil || len(q) == 0 {
		s.block("registry_pagination")
		return nil, false
	}
	for k, v := range q {
		if len(v) != 1 || len(v[0]) > 128 || strings.ContainsAny(v[0], "\r\n") {
			s.block("registry_pagination")
			return nil, false
		}
		switch k {
		case "n":
			n, e := strconv.Atoi(v[0])
			if e != nil || n < 1 || n > 1000 {
				s.block("registry_pagination")
				return nil, false
			}
		case "last":
			if !tagPattern.MatchString(v[0]) && !digestPattern.MatchString(v[0]) {
				s.block("registry_pagination")
				return nil, false
			}
		default:
			s.block("registry_pagination")
			return nil, false
		}
	}
	if q.Get("last") == "" {
		s.block("registry_pagination")
		return nil, false
	}
	return q, true
}
func (s *scanner) page(kind, ref string, q url.Values) ([]byte, string, bool) {
	if s.pages >= s.recipe.Limits.MaxPages {
		s.block("registry_limit")
		return nil, "", false
	}
	s.pages++
	r, ok := s.get(kind, ref, q)
	if !ok {
		return nil, "", false
	}
	if !s.status(r.Status) {
		r.Body.Close()
		return nil, "", false
	}
	if r.Filters != "" {
		s.block("registry_filtered_referrers")
	}
	b, ok := s.read(r)
	return b, r.Link, ok
}
func (s *scanner) tags() []string {
	out := []string{}
	seenTags := map[string]bool{}
	seenPages := map[string]bool{}
	q := url.Values{"n": {"100"}}
	for {
		key := q.Encode()
		if seenPages[key] {
			s.block("registry_pagination")
			break
		}
		seenPages[key] = true
		b, link, ok := s.page("tags", "list", q)
		if !ok {
			break
		}
		var v struct {
			Name string   `json:"name"`
			Tags []string `json:"tags"`
		}
		if !safeJSON(b) || json.Unmarshal(b, &v) != nil || v.Name != s.recipe.Repository {
			s.block("registry_malformed")
			break
		}
		for _, tag := range v.Tags {
			if !tagPattern.MatchString(tag) || seenTags[tag] {
				s.block("registry_pagination")
				return out
			}
			seenTags[tag] = true
			out = append(out, tag)
			if len(out) > s.recipe.Limits.MaxNodes {
				s.block("registry_limit")
				return out[:len(out)-1]
			}
		}
		q, ok = s.next(link, "tags", "list")
		if !ok || q == nil {
			break
		}
		if len(v.Tags) == 0 {
			s.block("registry_pagination")
			break
		}
	}
	sort.Strings(out)
	return out
}
func (s *scanner) referrers(d string, depth int) ([]descriptor, string) {
	out := []descriptor{}
	seen := map[string]bool{}
	pages := map[string]bool{}
	q := url.Values{"n": {"100"}}
	for {
		if s.pages >= s.recipe.Limits.MaxPages {
			s.block("registry_limit")
			break
		}
		s.pages++
		key := q.Encode()
		if pages[key] {
			s.block("registry_pagination")
			break
		}
		pages[key] = true
		r, ok := s.get("referrers", d, q)
		if !ok {
			break
		}
		if r.Status == 404 {
			r.Body.Close()
			s.block("registry_referrers_unknown")
			raw, fallbackDigest, found := s.fetch("sha256-" + strings.TrimPrefix(d, "sha256:"))
			if found {
				var m manifest
				if !safeJSON(raw) || json.Unmarshal(raw, &m) != nil || m.SchemaVersion != 2 || m.MediaType != ociIndex || m.Manifests == nil {
					s.block("registry_malformed")
				} else {
					s.visit(fallbackDigest, nil, depth+1, raw)
					s.edge(d, fallbackDigest, "REFERRERS_INDEX", nil)
					for _, desc := range m.Manifests {
						if s.validDesc(desc) {
							out = append(out, desc)
						}
					}
				}
			}
			break
		}
		if !s.status(r.Status) {
			r.Body.Close()
			break
		}
		if r.Filters != "" {
			s.block("registry_filtered_referrers")
		}
		b, ok := s.read(r)
		if !ok {
			break
		}
		var m manifest
		if !safeJSON(b) || json.Unmarshal(b, &m) != nil || m.SchemaVersion != 2 || m.MediaType != ociIndex || m.Manifests == nil || m.Config != nil || m.Layers != nil {
			s.block("registry_malformed")
			break
		}
		for _, desc := range m.Manifests {
			if !s.validDesc(desc) {
				continue
			}
			if seen[desc.Digest] {
				s.block("registry_pagination")
				continue
			}
			seen[desc.Digest] = true
			out = append(out, desc)
			if len(out) > s.recipe.Limits.MaxNodes {
				s.block("registry_limit")
				out = out[:len(out)-1]
				break
			}
		}
		q, ok = s.next(r.Link, "referrers", d)
		if !ok || q == nil {
			break
		}
		if len(m.Manifests) == 0 {
			s.block("registry_pagination")
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Digest < out[j].Digest })
	b, _ := json.Marshal(out)
	return out, hash(b)
}
