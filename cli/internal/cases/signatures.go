package cases

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"strings"
)

// decl is a declaration a patch adds: the name tests would use, and its text verbatim.
type decl struct {
	name string
	text string
}

// Signatures lists the declarations the source patch adds (docs/specs/cases.md, "Instructions")
// whose names the added lines of the test patch use, verbatim, in patch order: Go exported funcs,
// methods and types; C# public members and types; TypeScript and JavaScript exported functions,
// classes and types; Python top-level def and class; Java public members and types.
func Signatures(sourcePatch, testPatch []byte) []string {
	source, err := SplitPatch(sourcePatch)
	if err != nil {
		return nil
	}
	tests, err := SplitPatch(testPatch)
	if err != nil {
		return nil
	}
	var used strings.Builder
	for _, t := range tests {
		for _, seg := range addedSegments(t.Body) {
			used.WriteString(strings.Join(seg, "\n"))
			used.WriteByte('\n')
		}
	}
	testText := used.String()
	var out []string
	seen := map[string]bool{}
	for _, f := range source {
		for _, seg := range addedSegments(f.Body) {
			for _, d := range declarations(f.Path, seg) {
				if seen[d.text] || !regexp.MustCompile(`\b`+regexp.QuoteMeta(d.name)+`\b`).MatchString(testText) {
					continue
				}
				seen[d.text] = true
				out = append(out, d.text)
			}
		}
	}
	return out
}

// addedSegments returns the runs of consecutive added lines of one file's patch, without the "+".
func addedSegments(body []byte) [][]string {
	var out [][]string
	var cur []string
	inHunk := false
	flush := func() {
		if len(cur) > 0 {
			out = append(out, cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case strings.HasPrefix(line, "@@"):
			flush()
			inHunk = true
		case !inHunk:
		case strings.HasPrefix(line, "+"):
			cur = append(cur, line[1:])
		case strings.HasPrefix(line, `\`):
			// "\ No newline at end of file" belongs to the line before it.
		default:
			flush()
		}
	}
	flush()
	return out
}

// declarations finds the declarations of one run of added lines, by the file's language.
func declarations(file string, seg []string) []decl {
	switch ext := strings.ToLower(path.Ext(file)); ext {
	case ".go":
		return goDecls(seg)
	case ".cs":
		return lineDecls(seg, csRules)
	case ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs":
		return lineDecls(seg, tsRules)
	case ".py":
		return lineDecls(seg, pyRules)
	case ".java":
		return lineDecls(seg, javaRules)
	}
	return nil
}

// goDecls parses the run as Go declarations. When it does not parse on its own (a changed
// signature line without its body), each func or type line is read on its own.
func goDecls(seg []string) []decl {
	src := "package p\n" + strings.Join(seg, "\n") + "\n"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil {
		return lineDecls(seg, goRules)
	}
	text := func(from, to token.Pos) string {
		return strings.TrimSpace(src[fset.Position(from).Offset:fset.Position(to).Offset])
	}
	var out []decl
	for _, d := range file.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if !d.Name.IsExported() {
				continue
			}
			end := d.End()
			if d.Body != nil {
				end = d.Body.Lbrace
			}
			out = append(out, decl{name: d.Name.Name, text: text(d.Pos(), end)})
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, s := range d.Specs {
				ts := s.(*ast.TypeSpec)
				if !ts.Name.IsExported() {
					continue
				}
				t := text(d.Pos(), d.End())
				if d.Lparen.IsValid() {
					t = "type " + text(ts.Pos(), ts.End())
				}
				out = append(out, decl{name: ts.Name.Name, text: t})
			}
		}
	}
	return out
}

// rule is a line that starts a declaration: the pattern's first group is the name; a block rule
// keeps the whole braced body (interfaces, enums, type aliases), others keep the header only.
type rule struct {
	pattern *regexp.Regexp
	block   bool
}

var goRules = []rule{
	{pattern: regexp.MustCompile(`^func\s+(?:\([^)]*\)\s*)?([A-Z]\w*)\s*[\[(]`)},
	{pattern: regexp.MustCompile(`^type\s+([A-Z]\w*)\b`)},
}

const csMods = `(?:(?:static|sealed|abstract|partial|readonly|unsafe|new|ref|virtual|override|async|extern|required|file)\s+)*`

var csRules = []rule{
	{pattern: regexp.MustCompile(`^\s*public\s+` + csMods + `(?:interface|enum)\s+(\w+)`), block: true},
	{pattern: regexp.MustCompile(`^\s*public\s+` + csMods + `(?:class|struct|record(?:\s+class|\s+struct)?)\s+(\w+)`)},
	{pattern: regexp.MustCompile(`^\s*public\s+` + csMods + `[\w.]+(?:<[^()]*?>)?(?:\[\])*\??\s+(\w+)\s*(?:<[^()]*?>)?\s*(?:\(|\{|=>)`)},
	{pattern: regexp.MustCompile(`^\s*public\s+(\w+)\s*\(`)},
}

var tsRules = []rule{
	{pattern: regexp.MustCompile(`^\s*export\s+(?:default\s+)?(?:declare\s+)?(?:interface|enum|type)\s+(\w+)`), block: true},
	{pattern: regexp.MustCompile(`^\s*export\s+(?:default\s+)?(?:declare\s+)?(?:abstract\s+)?(?:async\s+)?(?:function\s*\*?|class)\s*(\w+)`)},
	{pattern: regexp.MustCompile(`^\s*export\s+(?:declare\s+)?(?:const|let|var)\s+(\w+)\s*(?::[^=]*)?=\s*(?:async\s+)?(?:function\b|\([^)]*\)\s*(?::[^=]*)?=>|\w+\s*=>|\()`)},
}

var pyRules = []rule{
	{pattern: regexp.MustCompile(`^(?:async\s+)?def\s+(\w+)\s*\(`)},
	{pattern: regexp.MustCompile(`^class\s+(\w+)`)},
}

const javaMods = `(?:(?:static|final|abstract|synchronized|default|native|strictfp|sealed|non-sealed)\s+)*`

var javaRules = []rule{
	{pattern: regexp.MustCompile(`^\s*public\s+` + javaMods + `(?:interface|enum|@interface)\s+(\w+)`), block: true},
	{pattern: regexp.MustCompile(`^\s*public\s+` + javaMods + `(?:class|record)\s+(\w+)`)},
	{pattern: regexp.MustCompile(`^\s*public\s+` + javaMods + `(?:<[^>]+>\s+)?[\w.]+(?:<[^()]*?>)?(?:\[\])*\s+(\w+)\s*\(`)},
	{pattern: regexp.MustCompile(`^\s*public\s+(\w+)\s*\(`)},
}

// lineDecls reads declarations line by line. A header runs until its parentheses close, and is
// cut before its body; a block declaration runs until its braces close.
func lineDecls(seg []string, rules []rule) []decl {
	var out []decl
	for i := 0; i < len(seg); i++ {
		for _, r := range rules {
			m := r.pattern.FindStringSubmatch(seg[i])
			if m == nil {
				continue
			}
			var text string
			var last int
			if r.block {
				text, last = block(seg, i)
			} else {
				text, last = header(seg, i)
			}
			out = append(out, decl{name: m[1], text: text})
			i = last
			break
		}
	}
	return out
}

// header joins lines from i until the parentheses balance, then cuts before a body ("{", "=>" or
// ":" of a Python block) and trims.
func header(seg []string, i int) (string, int) {
	depth := 0
	var lines []string
	last := i
	for j := i; j < len(seg) && j < i+12; j++ {
		lines = append(lines, seg[j])
		last = j
		depth += strings.Count(seg[j], "(") - strings.Count(seg[j], ")")
		if depth <= 0 {
			break
		}
	}
	text := strings.Join(lines, "\n")
	depth = 0
	for k := 0; k < len(text); k++ {
		switch text[k] {
		case '(', '[', '<':
			depth++
		case ')', ']', '>':
			if text[k] == '>' && k > 0 && (text[k-1] == '=' || text[k-1] == '-') {
				if text[k-1] == '-' {
					continue
				}
				if depth == 0 {
					return strings.TrimSpace(text[:k-1]), last
				}
				continue
			}
			depth--
		case '{':
			if depth <= 0 {
				return strings.TrimSpace(text[:k]), last
			}
		}
	}
	return strings.TrimRight(strings.TrimSpace(text), ";"), last
}

// block joins lines from i until the braces the declaration opens close again; a declaration
// without braces (type X = A | B;) is its own line.
func block(seg []string, i int) (string, int) {
	depth, opened := 0, false
	var lines []string
	for j := i; j < len(seg) && j < i+60; j++ {
		lines = append(lines, seg[j])
		depth += strings.Count(seg[j], "{") - strings.Count(seg[j], "}")
		if strings.Contains(seg[j], "{") {
			opened = true
		}
		if (opened && depth <= 0) || (!opened && j == i && strings.HasSuffix(strings.TrimSpace(seg[j]), ";")) {
			return strings.TrimSpace(dedent(lines)), j
		}
	}
	if !opened {
		return strings.TrimSpace(seg[i]), i
	}
	return strings.TrimSpace(dedent(lines)), i + len(lines) - 1
}

// dedent removes the indentation every line shares.
func dedent(lines []string) string {
	prefix, set := "", false
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		ws := l[:len(l)-len(strings.TrimLeft(l, " \t"))]
		if !set {
			prefix, set = ws, true
			continue
		}
		for !strings.HasPrefix(ws, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	var b bytes.Buffer
	for i, l := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(strings.TrimPrefix(l, prefix))
	}
	return b.String()
}
