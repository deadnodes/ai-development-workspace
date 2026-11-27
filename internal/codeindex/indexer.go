package codeindex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/workspace"
)

var generated = regexp.MustCompile(`(?im)^(?:.{0,12})?(?:code generated .*do not edit|@generated\b|automatically generated\b|this file (?:was|is) (?:auto|automatically) generated)`)

type Options struct {
	IncludeUntracked bool
	Exclude          []string
}

func Build(ctx context.Context, repository, checkout string, now time.Time, options Options) (domain.CodeIndex, error) {
	var doc domain.CodeIndex
	checkout, err := filepath.Abs(checkout)
	if err != nil {
		return doc, err
	}
	root, err := os.OpenRoot(checkout)
	if err != nil {
		return doc, err
	}
	defer root.Close()
	commit, branch, paths, partial, err := workspace.CodeIndexPaths(ctx, checkout, options.IncludeUntracked)
	if err != nil {
		return doc, err
	}
	doc = domain.CodeIndex{RepositoryID: repository, Commit: commit, Branch: branch, IndexedAt: now.UTC(), Version: domain.CodeIndexVersion, Files: []domain.CodeFile{}}
	flags := map[string]bool{}
	if partial {
		flags["paths"] = true
	}
	totalBytes, totalDocs, totalSymbols := 4096, 0, 0
	directories := map[string]bool{".": true}
	var regularParent func(string) bool
	regularParent = func(dir string) bool {
		if trusted, seen := directories[dir]; seen {
			return trusted
		}
		trusted := false
		if regularParent(path.Dir(dir)) {
			info, err := root.Lstat(dir)
			trusted = err == nil && info.IsDir()
		}
		directories[dir] = trusted
		return trusted
	}
	for _, name := range paths {
		if err := ctx.Err(); err != nil {
			return doc, err
		}
		if slices.Contains(options.Exclude, name) || !domain.CodePathValid(name) || domain.CodeSecretPath(name) || generatedPath(name) || !regularParent(path.Dir(name)) {
			continue
		}
		info, err := root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return doc, err
		}
		lock := domain.CodeLockfile(name)
		if !info.Mode().IsRegular() || (!lock && info.Size() > 2*domain.CodeFileBytes) {
			continue
		}
		file := domain.CodeFile{Path: name, Language: language(name), Size: int(info.Size())}
		if lock && commit != "" {
			if size, ok := workspace.CodeIndexTrackedSize(ctx, checkout, name); ok {
				file.Size = size
			}
		} else if !lock {
			content, err := readFile(root, name)
			if err != nil {
				return doc, err
			}
			content = bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
			if len(content) > domain.CodeFileBytes || !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 || generated.Match(content[:min(len(content), 4096)]) || strings.ContainsFunc(string(content), binaryControl) || minified(file.Language, content) {
				continue
			}
			file.Size = len(content)
			file.Lines = bytes.Count(content, []byte{'\n'})
			if len(content) > 0 && content[len(content)-1] != '\n' {
				file.Lines++
			}
			if domain.CodeDocPath(name) {
				text := domain.CodeRedactSecrets(string(content))
				if len(text) > domain.CodeDocBytes {
					flags["doc_bytes"] = true
				}
				if min(len(text), domain.CodeDocBytes) > domain.CodeDocsBytes-totalDocs {
					flags["docs_bytes"] = true
				}
				file.Doc = domain.CodeRedactSecrets(domain.CodePrefix(text, min(domain.CodeDocBytes, domain.CodeDocsBytes-totalDocs)))
			}
			collect(&file, content)
		}
		if len(doc.Files) == domain.CodeMaxFiles {
			flags["files"] = true
			break
		}
		if totalSymbols+len(file.Symbols) > domain.CodeMaxSymbols {
			flags["symbols"] = true
			file.Symbols = file.Symbols[:domain.CodeMaxSymbols-totalSymbols]
		}
		encoded, err := json.Marshal(file)
		if err != nil {
			return doc, err
		}
		if totalBytes+len(encoded)+1 > domain.CodeIndexBytes {
			flags["bytes"] = true
			break
		}
		totalBytes += len(encoded) + 1
		totalDocs += len(file.Doc)
		totalSymbols += len(file.Symbols)
		doc.Files = append(doc.Files, file)
	}
	for flag := range flags {
		doc.Truncated = append(doc.Truncated, flag)
	}
	slices.Sort(doc.Truncated)
	return doc, doc.Validate(repository)
}

func readFile(root *os.Root, name string) ([]byte, error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, 2*domain.CodeFileBytes+1))
}

func binaryControl(value rune) bool {
	return unicode.IsControl(value) && !strings.ContainsRune("\t\n\r\f", value)
}

func generatedPath(name string) bool {
	base := strings.ToLower(path.Base(name))
	return strings.Contains(base, ".min.") || strings.Contains(base, ".generated.") || strings.HasSuffix(base, ".pb.go") || strings.HasSuffix(base, ".map") || strings.HasSuffix(base, ".bundle.js")
}

func minified(language string, content []byte) bool {
	if len(content) < 4096 || language != "JavaScript" && language != "TypeScript" && language != "CSS" {
		return false
	}
	lines := bytes.Count(content, []byte{'\n'}) + 1
	return len(content)/lines >= 1024 && 16*(bytes.Count(content, []byte{' '})+bytes.Count(content, []byte{'\t'})) < len(content)
}

func language(name string) string {
	base := strings.ToLower(path.Base(name))
	if base == "dockerfile" || strings.HasPrefix(base, "dockerfile.") || strings.HasSuffix(base, ".dockerfile") {
		return "Dockerfile"
	}
	if kind := map[string]string{".go": "Go", ".py": "Python", ".ts": "TypeScript", ".tsx": "TypeScript", ".js": "JavaScript", ".mjs": "JavaScript", ".cjs": "JavaScript", ".jsx": "JavaScript", ".sql": "SQL", ".md": "Markdown", ".json": "JSON", ".yaml": "YAML", ".yml": "YAML", ".toml": "TOML", ".rs": "Rust", ".sh": "Shell", ".html": "HTML", ".css": "CSS"}[path.Ext(base)]; kind != "" {
		return kind
	}
	return "Text"
}
