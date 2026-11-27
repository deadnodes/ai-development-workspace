package domain

import (
	"strings"
	"testing"
	"time"
)

func TestCodeIndexValidation(t *testing.T) {
	token := "github_pat_" + strings.Repeat("A", 40)
	for _, tc := range []struct {
		name string
		file CodeFile
	}{
		{"name", CodeFile{Path: "config/.env.example", Language: "Text"}},
		{"doc", CodeFile{Path: "README.md", Language: "Markdown", Size: 100, Lines: 1, Doc: token}},
		{"symbol", CodeFile{Path: "app.go", Language: "Go", Size: 100, Lines: 1, Symbols: []CodeSymbol{{Name: "Handler", Kind: "function", Line: 1, Doc: token}}}},
		{"route", CodeFile{Path: "app.ts", Language: "TypeScript", Size: 100, Lines: 1, Routes: []CodeRoute{{Path: "/" + token, Method: "GET", Line: 1}}}},
		{"dependency", CodeFile{Path: "package.json", Language: "JSON", Dependencies: []CodeDependency{{Name: "foo", Kind: "npm", Version: "https://user:password@example.invalid"}}}},
		{"lock", CodeFile{Path: "yarn.lock", Language: "Text", Size: 100, Lines: 1}},
		{"oversized", CodeFile{Path: "app.go", Language: "Go", Size: CodeFileBytes + 1}},
		{"symbols", CodeFile{Path: "app.go", Language: "Go", Symbols: make([]CodeSymbol, CodeMaxSymbols+1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := CodeIndex{RepositoryID: "repo", Branch: "@{unversioned}", Version: CodeIndexVersion, IndexedAt: time.Now(), Files: []CodeFile{tc.file}}
			if err := doc.Validate("repo"); err == nil || strings.Contains(err.Error(), token) {
				t.Fatalf("expected safe validation error, got %v", err)
			}
		})
	}
	doc := CodeIndex{RepositoryID: "repo", Branch: "@{unversioned}", Version: CodeIndexVersion, IndexedAt: time.Now(), Files: make([]CodeFile, CodeMaxFiles+1)}
	if err := doc.Validate("repo"); err == nil || err.Error() != "invalid file list or truncation flags" {
		t.Fatalf("expected file count validation, got %v", err)
	}
}
