package prodsigner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"
	"releasecontrol/internal/imagesigning"
)

type releaseIntent struct {
	Repository string `yaml:"repository"`
	Digest     string `yaml:"digest"`
	ApprovedIn string `yaml:"approved_in"`
	Feature    string `yaml:"feature"`
	Notes      string `yaml:"notes"`
}

func intentReason(r releaseIntent, c Config) string {
	if !imagesigning.DigestPattern.MatchString(r.Digest) {
		return "invalid digest"
	}
	if r.Repository == "" || strings.Contains(r.Repository, "@") || strings.LastIndex(r.Repository, ":") > strings.LastIndex(r.Repository, "/") {
		return "invalid repository"
	}
	own := false
	for _, p := range c.OwnImagePrefixes {
		own = own || strings.HasPrefix(r.Repository, p)
	}
	if !own {
		return "foreign repository"
	}
	u, err := url.Parse(r.ApprovedIn)
	if err != nil || u.Scheme != "https" || u.Host == "" || !u.IsAbs() {
		return "invalid approved_in"
	}
	return ""
}

func collectIntents(c Config, files map[string][]byte, refs map[string]*Ref, s *Summary, log io.Writer) {
	p := c.IntentsFile()
	b, exists := files[p]
	if p == "" || !exists {
		return
	}
	emit := func(v any) {
		if log != nil {
			b, _ := json.Marshal(v)
			fmt.Fprintln(log, string(b))
		}
	}
	var v struct {
		Intents []releaseIntent `yaml:"intents"`
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	err := d.Decode(&v)
	if err == io.EOF {
		return
	}
	if err != nil || d.Decode(new(any)) != io.EOF {
		emit(map[string]string{"release_intents_invalid": p})
		return
	}
	for _, intent := range v.Intents {
		key := intent.Repository + "@" + intent.Digest
		if reason := intentReason(intent, c); reason != "" {
			s.RejectedIntents++
			emit(map[string]string{"rejected_intent": key, "reason": reason})
			continue
		}
		s.Intents++
		r := refs[key]
		if r == nil {
			r = &Ref{Image: key, Repo: intent.Repository, Digest: intent.Digest}
			refs[key] = r
		}
		r.Paths = append(r.Paths, p)
		r.Intent = true
		r.IntentApprovedIn = intent.ApprovedIn
		r.Feature = intent.Feature
	}
}
