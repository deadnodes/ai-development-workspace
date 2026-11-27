package codeindex

import (
	"cmp"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"releasecontrol/internal/domain"
)

var (
	pythonDecl  = regexp.MustCompile(`^(?:async\s+)?(def|class)\s+(\w+)\s*[(:]`)
	pythonRoute = regexp.MustCompile(`^@[\w.]+\.(get|post|put|patch|delete|head|options|route|api_route|websocket)\(\s*['"]([^'"]+)`)
	pythonMain  = regexp.MustCompile(`^if\s+__name__\s*==\s*['"]__main__['"]\s*:`)
	jsDecl      = regexp.MustCompile(`^(export\s+(?:default\s+)?)?(?:declare\s+)?(?:async\s+)?(function|class|const|interface|type|enum)\s+([A-Za-z_$][\w$]*)`)
	jsRoute     = regexp.MustCompile(`\b(?:app|router|server)\.(get|post|put|patch|delete|head|options|all|use)\(\s*['"\x60](/[^'"\x60]*)`)
	sqlTable    = regexp.MustCompile(`(?im)^[\t ]*CREATE\s+(?:TEMP(?:ORARY)?\s+|UNLOGGED\s+)?TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?((?:[\w.]+|"[^"\n]+"|\x60[^\x60\n]+\x60)+)`)
)

func collect(file *domain.CodeFile, body []byte) {
	text := string(body)
	switch file.Language {
	case "Go":
		collectGo(file, body)
	case "Python", "JavaScript", "TypeScript", "Dockerfile":
		for index, raw := range strings.Split(text, "\n") {
			collectLine(file, strings.TrimSuffix(raw, "\r"), index+1)
		}
	case "SQL":
		line, previous := 1, 0
		for _, match := range sqlTable.FindAllStringSubmatchIndex(text, -1) {
			line += strings.Count(text[previous:match[2]], "\n")
			previous = match[2]
			addSymbol(file, text[match[2]:match[3]], "table", "", "", line)
		}
	}
	collectManifest(file, body)
	slices.SortFunc(file.Dependencies, func(left, right domain.CodeDependency) int {
		return cmp.Or(cmp.Compare(left.Kind, right.Kind), cmp.Compare(left.Name, right.Name), cmp.Compare(left.Version, right.Version))
	})
	file.Dependencies = slices.Compact(file.Dependencies)
	slices.SortFunc(file.Entrypoints, func(left, right domain.CodeEntry) int {
		return cmp.Or(cmp.Compare(left.Kind, right.Kind), cmp.Compare(left.Name, right.Name))
	})
	file.Entrypoints = slices.Compact(file.Entrypoints)
}

func summary(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return domain.CodePrefix(line, 512)
}

func safeText(text string, limit int) bool {
	return len(text) <= limit && !strings.ContainsRune(text, 0) && !domain.CodeHasSecret(text)
}

func addSymbol(file *domain.CodeFile, name, kind, receiver, text string, line int) {
	if strings.TrimSpace(name) != "" && safeText(name, 512) && safeText(receiver, 512) && safeText(text, 512) && len(file.Symbols) <= domain.CodeMaxSymbols {
		file.Symbols = append(file.Symbols, domain.CodeSymbol{Name: name, Kind: kind, Receiver: receiver, Doc: text, Line: line})
	}
}

func addRoute(file *domain.CodeFile, route, method string, line int) {
	if strings.HasPrefix(route, "/") && safeText(route, 1024) && strings.TrimSpace(method) != "" && safeText(method, 24) {
		file.Routes = append(file.Routes, domain.CodeRoute{Path: route, Method: method, Line: line})
	}
}

func addDep(file *domain.CodeFile, kind, name, version string) {
	if strings.TrimSpace(name) != "" && safeText(name, 512) && safeText(version, 512) {
		file.Dependencies = append(file.Dependencies, domain.CodeDependency{Name: name, Version: version, Kind: kind})
	}
}

func addEntry(file *domain.CodeFile, kind, entry string) {
	entry = strings.TrimSpace(entry)
	if entry != "" && safeText(entry, 512) {
		file.Entrypoints = append(file.Entrypoints, domain.CodeEntry{Name: entry, Kind: kind})
	}
}

func collectGo(file *domain.CodeFile, body []byte) {
	positions := token.NewFileSet()
	parsed, err := parser.ParseFile(positions, file.Path, body, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return
	}
	line := func(pos token.Pos) int { return positions.PositionFor(pos, false).Line }
	if parsed.Name.Name == "main" && !strings.HasSuffix(file.Path, "_test.go") {
		addEntry(file, "go_main", path.Dir(file.Path))
	}
	if parsed.Doc != nil {
		addSymbol(file, parsed.Name.Name, "package", "", summary(parsed.Doc.Text()), line(parsed.Package))
	}
	for _, declaration := range parsed.Decls {
		switch decl := declaration.(type) {
		case *ast.FuncDecl:
			if !decl.Name.IsExported() {
				continue
			}
			kind, receiver := "function", ""
			if decl.Recv != nil && len(decl.Recv.List) != 0 {
				kind = "method"
				recv := decl.Recv.List[0].Type
				start, end := positions.PositionFor(recv.Pos(), false).Offset, positions.PositionFor(recv.End(), false).Offset
				receiver = strings.Join(strings.Fields(string(body[start:end])), " ")
			}
			addSymbol(file, decl.Name.Name, kind, receiver, summary(decl.Doc.Text()), line(decl.Name.Pos()))
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				value, ok := spec.(*ast.TypeSpec)
				if !ok || !value.Name.IsExported() {
					continue
				}
				doc := value.Doc
				if doc == nil {
					doc = decl.Doc
				}
				addSymbol(file, value.Name.Name, "type", "", summary(doc.Text()), line(value.Name.Pos()))
			}
		}
	}
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		method, argument := strings.ToUpper(selector.Sel.Name), 0
		switch method {
		case "HANDLE", "HANDLEFUNC", "ANY", "ROUTE", "MOUNT", "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS", "CONNECT", "TRACE":
		case "METHOD", "METHODFUNC":
			argument, method = 1, "ANY"
			if literal, ok := call.Args[0].(*ast.BasicLit); ok && literal.Kind == token.STRING {
				method, _ = strconv.Unquote(literal.Value)
			}
		default:
			return true
		}
		if literal, ok := call.Args[argument].(*ast.BasicLit); ok && literal.Kind == token.STRING {
			route, err := strconv.Unquote(literal.Value)
			if err != nil {
				return true
			}
			if method == "HANDLE" || method == "HANDLEFUNC" {
				method = "ANY"
				if verb, pattern, found := strings.Cut(route, " "); found {
					method, route = verb, pattern
				}
			}
			addRoute(file, route, method, line(literal.Pos()))
		}
		return true
	})
}

func collectLine(file *domain.CodeFile, line string, number int) {
	switch file.Language {
	case "Python":
		if match := pythonDecl.FindStringSubmatch(line); match != nil {
			addSymbol(file, match[2], match[1], "", "", number)
		}
		if match := pythonRoute.FindStringSubmatch(line); match != nil {
			addRoute(file, match[2], strings.ToUpper(match[1]), number)
		}
		if pythonMain.MatchString(line) || (number == 1 && path.Base(file.Path) == "__main__.py") {
			addEntry(file, "python_main", file.Path)
		}
	case "TypeScript", "JavaScript":
		if match := jsDecl.FindStringSubmatch(line); match != nil {
			component := (strings.HasSuffix(file.Path, ".tsx") || strings.HasSuffix(file.Path, ".jsx")) && match[3][0] >= 'A' && match[3][0] <= 'Z' && (match[2] == "function" || match[2] == "class" || match[2] == "const")
			kind := match[2]
			if component {
				kind = "component"
			}
			if match[1] != "" || component {
				addSymbol(file, match[3], kind, "", "", number)
			}
		}
		for _, match := range jsRoute.FindAllStringSubmatch(line, -1) {
			addRoute(file, match[2], strings.ToUpper(match[1]), number)
		}
	case "Dockerfile":
		parts := strings.Fields(line)
		if len(parts) < 2 {
			return
		}
		switch strings.ToUpper(parts[0]) {
		case "FROM":
			for _, part := range parts[1:] {
				if !strings.HasPrefix(part, "--") {
					addDep(file, "docker_base", part, "")
					break
				}
			}
		case "CMD", "ENTRYPOINT":
			addEntry(file, "docker_"+strings.ToLower(parts[0]), strings.TrimPrefix(strings.TrimSpace(line), parts[0]))
		}
	}
}
