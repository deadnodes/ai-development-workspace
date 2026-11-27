package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	CodeIndexVersion        = "1"
	CodeIndexBytes          = 8 << 20
	CodeFileBytes           = 512 << 10
	CodeDocBytes            = 64 << 10
	CodeDocsBytes           = 2 << 20
	CodeMaxFiles            = 20000
	CodeMaxSymbols          = 100000
	CodeHistorySize         = 20
	CodeBranchLimit         = 20
	CodeRepositoryPageSize  = 32
	CodeRepositoryURLBytes  = 4096
	CodeRepositoryPageBytes = 1 << 20
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

type CodeRepository struct {
	ID       string `json:"id"`
	Identity string `json:"identity"`
}

type CodeRepositoryPage struct {
	ProductID  string           `json:"product_id"`
	Items      []CodeRepository `json:"items"`
	NextCursor string           `json:"next_cursor,omitempty"`
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

const codeCredentialKey = `[a-z0-9_-]*(?:token|secret(?:[_-]?key)?|password|passwd|api[_-]?key|access[_-]?key(?:[_-]?id)?|private[_-]?key|credentials?|auth|authorization|bearer)`

var codePrivateKey = regexp.MustCompile(`(?i)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
var codePrivateKeyEnd = regexp.MustCompile(`(?i)-----END [A-Z0-9 ]*PRIVATE KEY-----`)
var codePrivateKeyPayload = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)
var codeToken = regexp.MustCompile(`(?i)\b(?:gh[pousr]_[a-z0-9]{25,}|github_pat_[a-z0-9_]{35,}|glpat-[a-z0-9_-]{18,}|xox[a-z]*-[a-z0-9-]{10,}|(?:AKIA|ASIA)[A-Z0-9]{16}|sk-(?:proj-|live-|test-)?[a-z0-9_-]{20,}|sk_(?:live|test)_[a-z0-9]{12,}|AIza[a-z0-9_-]{35}|eyJ[a-z0-9_-]{6,}\.[a-z0-9_-]{2,}\.[a-z0-9_-]{8,}|bearer[ \t]+[a-z0-9_.=-]{20,})`)
var codeRCPToken = regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])rcpa_[A-Za-z0-9_-]{43}(?:$|[^A-Za-z0-9_-])`)
var codeAssignment = regexp.MustCompile(`(?i)(?:^|[^a-z0-9_-])(` + codeCredentialKey + `|__Host-rcp_(?:session|login)|device_code|user_code|code_verifier)(?:\\?['"\x60‘’“”])?[ \t]*(:=|:|=)`)
var codeQuotedValue = regexp.MustCompile(`^(?:"((?:[^"\\]|\\.)*)"|'((?:[^'\\]|\\.)*)'|\x60([^\x60]*)\x60|“([^”]*)”|‘([^’]*)’)`)
var codeEscapedValue = regexp.MustCompile(`^\\["'](.+?)\\["']`)
var codeBareValue = regexp.MustCompile(`^[A-Za-z0-9_./+~%=-][A-Za-z0-9_./+~%=!$@:-]*(?:$|[ \t#;,'"\x60)\]}])`)
var codeNumericLiteral = regexp.MustCompile(`^(?:[0-9][0-9_]*|0[xX][0-9a-fA-F_]+)$`)
var codeTypeName = regexp.MustCompile(`^[A-Z][A-Za-z0-9_]*$`)
var codeQueryURL = regexp.MustCompile(`(?:\b[a-zA-Z][a-zA-Z0-9+.-]*://|(?:^|[ \t=(<\["'\x60])/)[^\s<>"'\x60]*\?[^\s<>"'\x60]+`)
var codeURLUserinfo = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s<>"\x60/?#]+@`)
var codeQueryKey = regexp.MustCompile(`(?i)^(?:` + codeCredentialKey + `|device_code|user_code|code_verifier|sig|signature|x-amz-signature|x-goog-signature|x-amz-credential|x-goog-credential)$`)

var codeDockerExecutable = regexp.MustCompile(`^[a-zA-Z0-9_./:\\+@-]+$`)

func CodeDockerExecutableValid(name string) bool { return codeDockerExecutable.MatchString(name) }

func CodeHasSecret(text string) bool {
	return filterCodeSecrets(text, nil)
}

func CodeRedactSecrets(text string) string {
	var out strings.Builder
	if !filterCodeSecrets(text, &out) {
		return text
	}
	return out.String()
}

func filterCodeSecrets(text string, out *strings.Builder) bool {
	privateKey, start, offset := false, 0, 0
	for raw := range strings.SplitAfterSeq(text, "\n") {
		line, newline := strings.CutSuffix(raw, "\n")
		if codePrivateKey.MatchString(line) {
			privateKey = true
		}
		end := privateKey && codePrivateKeyEnd.MatchString(line)
		if codeSecretLine(line) || privateKey && (end || codePrivateKeyPayload.MatchString(strings.TrimSpace(line))) {
			if out == nil {
				return true
			}
			out.WriteString(text[start:offset])
			out.WriteString("[...]")
			start = offset + len(line)
		}
		if end {
			privateKey = false
		}
		offset += len(line)
		if newline {
			offset++
		}
	}
	if start == 0 {
		return false
	}
	out.WriteString(text[start:])
	return true
}

func codeSecretLine(line string) bool {
	if codePrivateKey.MatchString(line) || codeToken.MatchString(line) || codeRCPToken.MatchString(line) {
		return true
	}
	if strings.Contains(line, "://") && codeURLUserinfo.MatchString(line) {
		return true
	}
	if strings.Contains(line, "?") {
		for _, candidate := range codeQueryURL.FindAllString(line, -1) {
			if codeSecretQuery(candidate) {
				return true
			}
		}
	}
	for _, match := range codeAssignment.FindAllStringSubmatchIndex(line, -1) {
		if codeLiteralValue(line[match[2]:match[3]], line[match[4]:match[5]], strings.TrimSpace(line[match[1]:])) {
			return true
		}
	}
	return false
}

func codeSecretQuery(candidate string) bool {
	_, query, ok := strings.Cut(candidate, "?")
	if !ok {
		return false
	}
	query, _, _ = strings.Cut(query, "#")
	for _, param := range strings.FieldsFunc(query, func(value rune) bool { return value == '&' || value == ';' }) {
		key, value, _ := strings.Cut(param, "=")
		value = strings.TrimRight(value, ")]")
		decodedValue, valueErr := url.QueryUnescape(value)
		if decoded, err := url.QueryUnescape(key); value != "" && (valueErr != nil || codeLiteralTemplateValue(decodedValue)) && err == nil && codeQueryKey.MatchString(decoded) {
			return true
		}
	}
	return false
}

func codeLiteralValue(key, operator, value string) bool {
	if value == "" || value[0] == '=' {
		return false
	}
	for _, pattern := range []*regexp.Regexp{codeQuotedValue, codeEscapedValue} {
		if match := pattern.FindStringSubmatch(value); match != nil {
			if codeValueContinues(value[len(match[0]):]) {
				return false
			}
			literal := strings.Join(match[1:], "")
			return codeLiteralTemplateValue(literal)
		}
	}
	matched := codeBareValue.FindString(value)
	if matched == "" {
		return false
	}
	bare := strings.TrimRight(matched, " \t#;,'\"`)]}")
	if operator == ":=" {
		return codeNumericLiteral.MatchString(bare)
	}
	switch bare {
	case "nil", "null", "undefined", "None", "string", "str", "number", "boolean", "unknown", "any", "never", "object", "int", "bool", "byte", "error":
		return false
	}
	tail := strings.TrimSpace(value[len(bare):])
	if operator == ":" && strings.HasPrefix(tail, ";") && codeTypeName.MatchString(bare) {
		return false
	}
	if codeValueContinues(tail) {
		return false
	}
	return key == strings.ToUpper(key) || !strings.Contains(bare, ".") || strings.ContainsAny(bare, "/-+%=!$@:")
}

func codeValueContinues(tail string) bool {
	tail = strings.TrimSpace(tail)
	return strings.HasPrefix(tail, "+") || strings.HasPrefix(tail, "||") || strings.HasPrefix(tail, "??")
}

func codeLiteralTemplateValue(value string) bool {
	return value != "" && !strings.HasPrefix(value, "${") && !strings.HasPrefix(value, "{{")
}

func CodeSecretPath(name string) bool {
	for _, part := range strings.Split(strings.ToLower(name), "/") {
		if strings.HasPrefix(part, ".env") || strings.HasPrefix(part, "id_") || strings.HasPrefix(part, "credentials") && !codeCredentialSource(part) || strings.HasSuffix(part, ".pem") || strings.HasSuffix(part, ".key") || part == ".netrc" || part == ".npmrc" || part == ".git-credentials" || part == ".git" || part == ".aws" || part == ".ssh" || part == ".release-control.json" || part == ".mcp.json" {
			return true
		}
	}
	return CodeHasSecret(name)
}

func codeCredentialSource(name string) bool {
	switch path.Ext(name) {
	case ".go", ".py", ".js", ".mjs", ".cjs", ".jsx", ".ts", ".tsx", ".rs", ".sh", ".sql":
		return true
	}
	return false
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
			dockerCommand := entry.Kind == "docker_cmd" || entry.Kind == "docker_entrypoint"
			if !validCodeText(entry.Name, 512, true) || !validCodeText(entry.Kind, 48, true) || dockerCommand && entry.Name != "[...]" && !CodeDockerExecutableValid(entry.Name) {
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
