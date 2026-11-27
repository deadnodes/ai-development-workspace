package domain

import (
	"cmp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

type CodeSearchEntry struct {
	Card    CodeCard
	Title   string
	Body    string
	Snippet string
}

func (doc CodeIndex) SearchEntries(yield func(CodeSearchEntry) bool) {
	for _, file := range doc.Files {
		appendEntry := func(kind, symbol, title, text, snippet string, line int) bool {
			card := CodeCard{RepositoryID: doc.RepositoryID, Branch: doc.Branch, Commit: doc.Commit, Path: file.Path, Kind: kind, Symbol: symbol, Line: line}
			return yield(CodeSearchEntry{card, strings.ToLower(title), strings.ToLower(text), snippet})
		}
		if !appendEntry("file", "", file.Path, file.Path, file.Language, 0) {
			return
		}
		for _, symbol := range file.Symbols {
			if !appendEntry("symbol", symbol.Name, symbol.Name, strings.Join([]string{symbol.Name, symbol.Receiver, symbol.Doc}, " "), strings.TrimSpace(symbol.Receiver+" "+symbol.Doc), symbol.Line) {
				return
			}
		}
		for _, route := range file.Routes {
			if !appendEntry("route", route.Path, route.Path, route.Method+" "+route.Path, route.Method+" "+route.Path, route.Line) {
				return
			}
		}
		for _, dep := range file.Dependencies {
			if !appendEntry("dependency", dep.Name, dep.Name, dep.Name+" "+dep.Version, strings.TrimSpace(dep.Kind+" "+dep.Version), 0) {
				return
			}
		}
		if file.Doc != "" && !appendEntry("doc", "", file.Path, file.Path+"\n"+file.Doc, file.Doc, 0) {
			return
		}
	}
}

func CodeSearchPhrase(query string) string {
	return strings.Join(strings.FieldsFunc(query, func(value rune) bool { return !unicode.IsLetter(value) && !unicode.IsNumber(value) }), " ")
}

func (entry CodeSearchEntry) Score(query string) int {
	if entry.Title == query {
		return 3
	}
	if strings.Contains(entry.Title, query) {
		return 2
	}
	if strings.Contains(entry.Body, query) {
		return 1
	}
	if phrase := CodeSearchPhrase(query); phrase != "" && strings.Contains(" "+CodeSearchPhrase(entry.Body)+" ", " "+phrase+" ") {
		return 1
	}
	return 0
}

func CodeSnippet(snippet, query string) string {
	if len(snippet) <= 480 {
		return snippet
	}
	start := 0
	lower := strings.ToLower(snippet)
	if offset := strings.Index(lower, query); offset > 0 {
		seen := 0
		for index, value := range snippet {
			if seen >= offset {
				start = index
				break
			}
			seen += utf8.RuneLen(unicode.ToLower(value))
		}
	}
	return CodePrefix(snippet[start:], 480)
}

func CompareCodeCards(left, right CodeCard) int {
	return cmp.Or(cmp.Compare(left.RepositoryID, right.RepositoryID), cmp.Compare(left.Path, right.Path), cmp.Compare(left.Kind, right.Kind), cmp.Compare(left.Line, right.Line), cmp.Compare(left.Symbol, right.Symbol))
}

func SortCodeStatuses(items []CodeIndexStatus) {
	slices.SortFunc(items, func(left, right CodeIndexStatus) int {
		return cmp.Or(cmp.Compare(left.RepositoryID, right.RepositoryID), right.IndexedAt.Compare(left.IndexedAt), cmp.Compare(left.Branch, right.Branch))
	})
}
