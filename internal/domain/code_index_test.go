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

func TestCodeIndexRejectsCredentialShapedUploadFields(t *testing.T) {
	for _, text := range []string{
		"export TOKEN=short # comment", `"password": "fixture!",`, "`api_key` = 'value'", "'access_key_id' = hi;",
		`\"private_key\":\"data!\"`, "auth: yes", "auth: FIXTURE", "bearer = no", "credential: x", "passwd='a b'", "“token” = fixture",
		"https://name:pass'word@example.invalid/repo", "https://example.invalid/repo?access_token=x&next=1",
		"https://example.invalid/repo?x=1&%73ignature=x", "https://example.invalid/?X-Amz-Credential=x", "url=https://example.invalid/?%73ig=x",
		"ghp_" + strings.Repeat("a", 36), "gho_" + strings.Repeat("a", 36), "github_pat_" + strings.Repeat("a", 36),
		"glpat-" + strings.Repeat("a", 20), "xoxb-" + strings.Repeat("a", 20), "sk-" + strings.Repeat("a", 25),
		"AKIA" + strings.Repeat("1", 16), "eyJhbGciOiJub25lIn0.e30.abcdefghij",
		"rcpa_" + strings.Repeat("x-_A", 10) + "zzz", `token := "fixture"`, `password := 1234`, `__Host-rcp_session=opaque`,
		"/download?%74oken=fixture", `{"token":"fixture","note":"keep"}`, "-----BEGIN PRIVATE KEY-----\nYWJjZA==\n-----END PRIVATE KEY-----",
	} {
		doc := CodeIndex{RepositoryID: "repo", Branch: "@{unversioned}", Version: CodeIndexVersion, IndexedAt: time.Now(), Files: []CodeFile{{Path: "README.md", Language: "Markdown", Size: len(text), Lines: 1, Doc: text}}}
		if err := doc.Validate("repo"); err == nil || strings.Contains(err.Error(), text) {
			t.Errorf("credential case %q returned %v", text, err)
		}
		doc.Files = []CodeFile{{Path: "package.json", Language: "JSON", Dependencies: []CodeDependency{{Name: "fixture", Kind: "npm", Version: text}}}}
		if err := doc.Validate("repo"); err == nil {
			t.Errorf("credential case %q accepted in a dependency", text)
		}
	}
	if CodeHasSecret("Use https://example.invalid/repo?view=all&mode=brief for details.") {
		t.Fatal("ordinary URL was rejected")
	}
}

func TestCodeSecretsPreserveRuntimeCodeAndEmptyValues(t *testing.T) {
	text := strings.Join([]string{
		`token := os.Getenv("RCP_TOKEN")`, `token = os.Getenv("RCP_TOKEN")`, `token: os.Getenv("RCP_TOKEN")`,
		`if token == "fixture" {}`, `if token != "fixture" {}`, `token === "fixture"`,
		`var token string`, `token: string`, `token: *Token`, `token: Token;`, `token:`, `token = ""`, `api_key = ''`,
		`token = $TOKEN`, `token = "${TOKEN}"`, `token = "" || os.Getenv("RCP_TOKEN")`,
		`authorization = "Bearer " + os.Getenv("RCP_TOKEN")`,
		`https://example.invalid/repo?access_token=&next=1`, `/download?access_token=`, `url=https://example.invalid/?sig=%24%7BSIGNATURE%7D`,
		`rcpa_`, `rcpa_` + strings.Repeat("a", 42), `rcpa_` + strings.Repeat("a", 44),
	}, "\n")
	if got := CodeRedactSecrets(text); got != text || CodeHasSecret(text) {
		t.Fatalf("text without credentials was changed:\n%s", got)
	}
}
