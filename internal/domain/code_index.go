package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	CodeIndexVersion = "1"
	CodeIndexBytes   = 8 << 20
	CodeFileBytes    = 512 << 10
	CodeDocBytes     = 64 << 10
	CodeDocsBytes    = 2 << 20
	CodeMaxFiles     = 20000
	CodeMaxSymbols   = 100000
	CodeHistorySize  = 20
)

type CodeIndex struct {
	RepositoryID string     `json:"repository_id"`
	Commit       string     `json:"commit"`
	Branch       string     `json:"branch"`
	IndexedAt    time.Time  `json:"indexed_at"`
	Version      string     `json:"indexer_version"`
	Truncated    []string   `json:"truncated,omitempty"`
	Files        []CodeFile `json:"files"`
}

type CodeFile struct {
	Path         string           `json:"path"`
	Language     string           `json:"language"`
	Size         int              `json:"size"`
	Lines        int              `json:"lines"`
	Symbols      []CodeSymbol     `json:"symbols,omitempty"`
	Routes       []CodeRoute      `json:"routes,omitempty"`
	Dependencies []CodeDependency `json:"dependencies,omitempty"`
	Entrypoints  []CodeEntry      `json:"entry_points,omitempty"`
	Doc          string           `json:"doc,omitempty"`
}

type CodeSymbol struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Receiver string `json:"receiver,omitempty"`
	Doc      string `json:"doc,omitempty"`
	Line     int    `json:"line"`
}

type CodeRoute struct {
	Path   string `json:"path"`
	Method string `json:"method"`
	Line   int    `json:"line"`
}

type CodeDependency struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Kind    string `json:"kind"`
}

type CodeEntry struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type CodeIndexWrite struct {
	ProductID    string    `json:"product_id"`
	RepositoryID string    `json:"repository_id"`
	Actor        string    `json:"actor,omitempty"`
	Document     CodeIndex `json:"document"`
}

type CodeIndexStatus struct {
	RepositoryID string    `json:"repository_id"`
	Branch       string    `json:"branch"`
	Commit       string    `json:"commit"`
	Digest       string    `json:"digest"`
	IndexedAt    time.Time `json:"indexed_at"`
	AgeSeconds   int64     `json:"age_seconds"`
	Truncated    []string  `json:"truncated,omitempty"`
}

type CodeStatusRequest struct {
	ProductID    string `json:"product_id"`
	RepositoryID string `json:"repository_id,omitempty"`
	Branch       string `json:"branch,omitempty"`
}

type CodeStatusResult struct {
	Items     []CodeIndexStatus `json:"items"`
	Truncated bool              `json:"truncated"`
}

type CodeIndexWriteResult struct {
	CodeIndexStatus
	Changed bool `json:"changed"`
}

type CodeMapRequest struct {
	ProductID    string `json:"product_id"`
	RepositoryID string `json:"repository_id"`
	Branch       string `json:"branch,omitempty"`
	PathPrefix   string `json:"path_prefix,omitempty"`
	Depth        int    `json:"depth,omitempty"`
	Limit        int    `json:"limit,omitempty"`
}

type CodeOutlineRequest struct {
	ProductID    string `json:"product_id"`
	RepositoryID string `json:"repository_id"`
	Path         string `json:"path"`
}

type CodeSearchRequest struct {
	ProductID    string `json:"product_id"`
	RepositoryID string `json:"repository_id,omitempty"`
	Query        string `json:"query"`
	Kind         string `json:"kind,omitempty" jsonschema:"one of file, symbol, route, dependency, doc"`
	Limit        int    `json:"limit,omitempty"`
}

type CodeCard struct {
	RepositoryID string `json:"repository_id"`
	Branch       string `json:"branch"`
	Commit       string `json:"commit"`
	Path         string `json:"path"`
	Kind         string `json:"kind"`
	Symbol       string `json:"symbol,omitempty"`
	Line         int    `json:"line,omitempty"`
	Snippet      string `json:"snippet,omitempty"`
}

type CodeSearchResult struct {
	Items     []CodeCard `json:"items"`
	Truncated bool       `json:"truncated"`
}

var codeCommit = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)
var codeSecrets = regexp.MustCompile(`(?im)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----|\b(?:gh[pousr]_[a-z0-9]{25,}|github_pat_[a-z0-9_]{35,}|glpat-[a-z0-9_-]{18,}|xox[baprs]-[a-z0-9-]{10,}|(?:AKIA|ASIA)[A-Z0-9]{16}|sk-(?:proj-|live-|test-)?[a-z0-9_-]{20,}|sk_(?:live|test)_[a-z0-9]{12,}|AIza[a-z0-9_-]{35}|eyJ[a-z0-9_-]{6,}\.eyJ[a-z0-9_-]{6,}\.[a-z0-9_-]{8,}|bearer[ \t]+[a-z0-9_.=-]{20,})\b|\b[a-z][a-z0-9+.-]*://[^/\s:@]+:[^/\s@]+@|\b(?:[a-z0-9_-]*(?:api[_-]?key|access[_-]?token|auth[_-]?token|client[_-]?secret|password|passwd|secret|token))['"]?[ \t]*[:=][ \t]*['"]?[a-z0-9_./+=-]{8,}(?:['"]|[ \t]*$)`)

func CodeHasSecret(text string) bool { return codeSecrets.MatchString(text) }

func CodeSecretPath(name string) bool {
	for _, part := range strings.Split(strings.ToLower(name), "/") {
		if strings.HasPrefix(part, ".env") || strings.HasPrefix(part, "id_") || strings.HasPrefix(part, "credentials") || strings.HasSuffix(part, ".pem") || strings.HasSuffix(part, ".key") || part == ".netrc" || part == ".npmrc" || part == ".git-credentials" || part == ".git" || part == ".aws" || part == ".ssh" || part == ".release-control.json" || part == ".mcp.json" {
			return true
		}
	}
	return CodeHasSecret(name)
}

func CodeLockfile(name string) bool {
	switch strings.ToLower(path.Base(name)) {
	case "go.sum", "package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "yarn.lock", "pipfile.lock", "cargo.lock", "composer.lock", "gemfile.lock", "flake.lock", ".terraform.lock.hcl":
		return true
	}
	return strings.HasSuffix(strings.ToLower(name), ".lock")
}

func CodeDocPath(name string) bool {
	base := strings.ToLower(path.Base(name))
	return strings.HasPrefix(base, "readme") || base == "agents.md" || base == "claude.md" || (strings.HasPrefix(strings.ToLower(name), "docs/") && strings.HasSuffix(base, ".md"))
}

func CodePathValid(name string) bool {
	return name != "" && name != "." && len(name) <= 1024 && !strings.HasPrefix(name, "/") && name != ".." && !strings.HasPrefix(name, "../") && path.Clean(name) == name && !strings.ContainsAny(name, "\\:") && !strings.ContainsFunc(name, unicode.IsControl) && utf8.ValidString(name)
}

func CodePrefix(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	for maxBytes > 0 && !utf8.RuneStart(text[maxBytes]) {
		maxBytes--
	}
	return text[:maxBytes]
}

func (doc CodeIndex) Digest() (string, error) {
	doc.IndexedAt = time.Time{}
	data, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func validCodeText(value string, cap int, required bool) bool {
	return (!required || strings.TrimSpace(value) != "") && len(value) <= cap && utf8.ValidString(value) && !strings.ContainsRune(value, 0) && !CodeHasSecret(value)
}

func (doc CodeIndex) Validate(repository string) error {
	if doc.RepositoryID != repository || !validCodeText(repository, 256, true) || !validCodeText(doc.Branch, 256, true) || strings.ContainsFunc(doc.Branch, unicode.IsControl) || doc.IndexedAt.IsZero() || doc.IndexedAt.Year() > 9999 || doc.Version != CodeIndexVersion || (doc.Branch == "@{unversioned}" && doc.Commit != "") || (doc.Branch != "@{unversioned}" && !codeCommit.MatchString(doc.Commit)) {
		return fmt.Errorf("invalid index identity, timestamp or indexer version")
	}
	if doc.Files == nil || len(doc.Files) > CodeMaxFiles || len(doc.Truncated) > 6 {
		return fmt.Errorf("invalid file list or truncation flags")
	}
	previous := ""
	for _, flag := range doc.Truncated {
		if flag <= previous || !strings.Contains("|bytes|doc_bytes|docs_bytes|files|paths|symbols|", "|"+flag+"|") {
			return fmt.Errorf("invalid truncation flags")
		}
		previous = flag
	}
	previous = ""
	symbols, docs := 0, 0
	for _, file := range doc.Files {
		if file.Path <= previous || !CodePathValid(file.Path) || CodeSecretPath(file.Path) || !validCodeText(file.Language, 48, true) || file.Size < 0 || (!CodeLockfile(file.Path) && file.Size > CodeFileBytes) || file.Lines < 0 || file.Lines > file.Size || (file.Doc != "" && (!CodeDocPath(file.Path) || len(file.Doc) > file.Size)) || !validCodeText(file.Doc, CodeDocBytes, false) {
			return fmt.Errorf("invalid or sensitive file metadata")
		}
		previous = file.Path
		symbols += len(file.Symbols)
		docs += len(file.Doc)
		if symbols > CodeMaxSymbols || docs > CodeDocsBytes {
			return fmt.Errorf("symbol or documentation budget exceeded")
		}
		if CodeLockfile(file.Path) && (file.Lines != 0 || len(file.Symbols)+len(file.Routes)+len(file.Entrypoints)+len(file.Dependencies)+len(file.Doc) != 0) {
			return fmt.Errorf("lockfile content is not accepted")
		}
		for _, symbol := range file.Symbols {
			if !validCodeText(symbol.Name, 512, true) || !validCodeText(symbol.Kind, 48, true) || !validCodeText(symbol.Receiver, 512, false) || !validCodeText(symbol.Doc, 512, false) || symbol.Line < 1 || symbol.Line > file.Lines {
				return fmt.Errorf("invalid or sensitive symbol")
			}
		}
		for _, route := range file.Routes {
			if !validCodeText(route.Method, 24, true) || !validCodeText(route.Path, 1024, true) || route.Line < 1 || route.Line > file.Lines {
				return fmt.Errorf("invalid or sensitive route")
			}
		}
		for _, dep := range file.Dependencies {
			if !validCodeText(dep.Name, 512, true) || !validCodeText(dep.Version, 512, false) || !validCodeText(dep.Kind, 48, true) {
				return fmt.Errorf("invalid or sensitive dependency")
			}
		}
		for _, entry := range file.Entrypoints {
			if !validCodeText(entry.Name, 512, true) || !validCodeText(entry.Kind, 48, true) {
				return fmt.Errorf("invalid or sensitive entry point")
			}
		}
	}
	data, err := json.Marshal(doc)
	if err != nil || len(data) > CodeIndexBytes {
		return fmt.Errorf("serialized index exceeds 8 MiB or is invalid")
	}
	return nil
}
