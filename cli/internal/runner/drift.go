package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/alternayte/casebox/cli/internal/repo"
)

// Drift finds what a harness names that a case's base does not have (docs/specs/cases.md, "Harness
// overlay and drift"). harnessFiles are the harness under test; baseFiles are every file of the
// base tree; the recipes, targets and scripts are those of the base's justfiles, Makefiles and
// package.json files. refs are the references found, missing those the base lacks, both sorted;
// drift is true when anything is missing.
//
// A reference is:
//   - a command in code (inline code spans, fenced blocks, and the whole of a file that is not
//     Markdown): `just <recipe>`, `make <target>`, `npm run <script>`, `bun run <script>`,
//     `pnpm [run] <script>`, and `dotnet <verb> <project path>`;
//   - a path in code: a token with a known file extension, or with a "/" whose first segment is in
//     the base or that ends with "/";
//   - a path in prose: a token with a known file extension, or one that starts with "./".
//
// URLs, absolute paths, flags, placeholders and tokens with "@", "=", "$" or "{" are not paths. A
// path exists when a base or harness file or directory has it, relative to the root or to the
// harness file's directory; a glob exists when such a file matches it.
func Drift(harnessFiles map[string][]byte, baseFiles []string, justRecipes, makeTargets, npmScripts []string) (bool, []string, []string) {
	// The overlay puts the harness files over the base, so they exist when the agent reads them.
	all := append([]string(nil), baseFiles...)
	for name := range harnessFiles {
		all = append(all, name)
	}
	b := newBase(all)
	sets := map[string]map[string]bool{"just": set(justRecipes), "make": set(makeTargets), "npm": set(npmScripts)}
	found := map[string]bool{}
	missing := map[string]bool{}
	note := func(ref string, ok bool) {
		found[ref] = true
		if !ok {
			missing[ref] = true
		}
	}
	names := make([]string, 0, len(harnessFiles))
	for n := range harnessFiles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		text := harnessFiles[name]
		if bytes.IndexByte(text, 0) >= 0 {
			continue
		}
		dir := path.Dir(name)
		code, prose := split(name, string(text))
		for _, segment := range code {
			for _, line := range strings.Split(segment, "\n") {
				scanCode(line, dir, b, sets, note)
			}
		}
		for _, segment := range prose {
			for _, tok := range tokens(segment) {
				if p, ok := prosePath(tok); ok {
					note(p, b.has(dir, p))
				}
			}
		}
	}
	return len(missing) > 0, sortedKeys(found), sortedKeys(missing)
}

// knownExt are the extensions that make a token a file path.
var knownExt = set([]string{
	"md", "mdc", "mdx", "markdown", "txt", "rst", "adoc",
	"go", "mod", "sum", "work",
	"cs", "csproj", "fs", "fsproj", "vb", "vbproj", "sln", "slnx", "props", "targets", "razor", "cshtml", "resx",
	"js", "jsx", "mjs", "cjs", "ts", "tsx", "mts", "cts", "vue", "svelte", "astro",
	"py", "pyi", "ipynb", "cfg", "ini",
	"java", "kt", "kts", "gradle", "scala", "groovy",
	"rs", "rb", "php", "swift", "c", "h", "cc", "cpp", "hpp", "m", "dart", "ex", "exs", "erl", "lua", "zig", "nix",
	"json", "jsonc", "json5", "yaml", "yml", "toml", "xml", "csv", "tsv", "proto", "graphql", "gql", "sql", "prisma",
	"html", "css", "scss", "sass", "less", "svg",
	"sh", "bash", "zsh", "ps1", "psm1", "bat", "cmd", "mk", "just", "tf", "hcl", "dockerfile", "lock", "lockb", "snap",
})

// knownNames are file names without an extension that are paths wherever they appear in code.
var knownNames = set([]string{"Makefile", "Dockerfile", "justfile", "Justfile", "Containerfile", "Gemfile", "Rakefile", "Procfile"})

// pnpmCommands are pnpm's own commands, which are not scripts.
var pnpmCommands = set([]string{
	"add", "install", "i", "ci", "update", "up", "upgrade", "remove", "rm", "un", "uninstall", "link", "ln", "unlink",
	"import", "rebuild", "rb", "prune", "fetch", "patch", "patch-commit", "audit", "outdated", "list", "ls", "ll",
	"why", "exec", "dlx", "create", "init", "publish", "pack", "store", "env", "setup", "config", "c", "root", "bin",
	"deploy", "doctor", "server", "licenses", "recursive", "multi", "m", "help", "dedupe", "approve-builds", "self-update",
})

// dotnetVerbs are the dotnet commands that take a project or solution path.
var dotnetVerbs = set([]string{"build", "test", "run", "restore", "pack", "publish", "clean", "format", "watch"})

func scanCode(line, dir string, b base, sets map[string]map[string]bool, note func(string, bool)) {
	words := strings.Fields(line)
	for i := range words {
		words[i] = strings.Trim(words[i], "`'\"()[]<>,;")
	}
	used := map[int]bool{}
	arg := func(i int) (string, bool) {
		if i >= len(words) || words[i] == "" || strings.HasPrefix(words[i], "-") || strings.ContainsAny(words[i], "=$<>{}|&") {
			return "", false
		}
		return words[i], true
	}
	for i, w := range words {
		switch w {
		case "just", "make":
			if name, ok := arg(i + 1); ok {
				note(w+" "+name, sets[w][name])
				used[i+1] = true
			}
		case "npm", "bun":
			if i+1 < len(words) && words[i+1] == "run" {
				if name, ok := arg(i + 2); ok {
					used[i+2] = true
					if w == "bun" && looksLikeFile(name) {
						note(name, b.has(dir, name))
						continue
					}
					note(w+" run "+name, sets["npm"][name])
				}
			}
		case "pnpm":
			j := i + 1
			if j < len(words) && words[j] == "run" {
				j++
			}
			if name, ok := arg(j); ok && (j > i+1 || !pnpmCommands[name]) {
				note("pnpm "+name, sets["npm"][name])
				used[j] = true
			}
		case "dotnet":
			if i+1 < len(words) && dotnetVerbs[words[i+1]] {
				if p, ok := arg(i + 2); ok {
					note("dotnet "+words[i+1]+" "+p, b.has(dir, strings.TrimPrefix(p, "./")))
					used[i+2] = true
				}
			}
		}
	}
	for i, w := range words {
		if used[i] {
			continue
		}
		if p, ok := codePath(w, b); ok {
			note(p, b.has(dir, p))
		}
	}
}

// codePath says whether a token in code is a repository path, and cleans it.
func codePath(tok string, b base) (string, bool) {
	tok = strings.TrimRight(tok, ".:!?")
	if notPath(tok) {
		return "", false
	}
	clean := strings.TrimPrefix(tok, "./")
	if knownNames[path.Base(strings.TrimSuffix(clean, "/"))] || hasKnownExt(clean) {
		return strings.TrimSuffix(clean, "/"), true
	}
	if !strings.Contains(clean, "/") {
		return "", false
	}
	if strings.HasSuffix(clean, "/") || strings.HasPrefix(tok, "./") {
		return strings.TrimSuffix(clean, "/"), true
	}
	first, _, _ := strings.Cut(clean, "/")
	if b.top[first] {
		return clean, true
	}
	return "", false
}

// prosePath says whether a token in prose is a repository path, and cleans it.
func prosePath(tok string) (string, bool) {
	tok = strings.TrimRight(tok, ".:!?*_")
	tok = strings.TrimLeft(tok, "*_")
	if notPath(tok) {
		return "", false
	}
	if strings.HasPrefix(tok, "./") && len(tok) > 2 {
		return strings.TrimSuffix(strings.TrimPrefix(tok, "./"), "/"), true
	}
	if hasKnownExt(tok) {
		return tok, true
	}
	return "", false
}

func notPath(tok string) bool {
	return tok == "" || strings.Contains(tok, "://") || strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, "~") ||
		strings.HasPrefix(tok, "-") || strings.HasPrefix(tok, "../") || strings.ContainsAny(tok, "@=${}<>|\\") ||
		strings.HasPrefix(tok, "www.") || strings.Contains(tok, "//")
}

func hasKnownExt(p string) bool {
	name := path.Base(p)
	dot := strings.LastIndexByte(name, '.')
	if dot < 0 || dot == len(name)-1 {
		return false
	}
	ext := name[dot+1:]
	if !knownExt[strings.ToLower(ext)] {
		return false
	}
	// "Node.js" and "Vue.js" are names, not files.
	if ext == "js" && !strings.Contains(p, "/") && dot > 0 && name[0] >= 'A' && name[0] <= 'Z' {
		return false
	}
	return dot > 0 || strings.HasPrefix(name, ".")
}

func looksLikeFile(s string) bool { return strings.Contains(s, "/") || hasKnownExt(s) }

// base is the set of a base tree's files and directories.
type base struct {
	files map[string]bool
	dirs  map[string]bool
	top   map[string]bool
	list  []string
}

func newBase(files []string) base {
	b := base{files: map[string]bool{}, dirs: map[string]bool{}, top: map[string]bool{}, list: files}
	for _, f := range files {
		b.files[f] = true
		first, _, _ := strings.Cut(f, "/")
		b.top[first] = true
		for d := path.Dir(f); d != "."; d = path.Dir(d) {
			b.dirs[d] = true
		}
	}
	return b
}

// has says whether p names a file or directory of the base, from the root or from dir; a p with
// "*" or "?" is a glob that must match a file.
func (b base) has(dir, p string) bool {
	candidates := []string{path.Clean(p)}
	if dir != "." {
		candidates = append(candidates, path.Join(dir, p))
	}
	for _, c := range candidates {
		if strings.ContainsAny(c, "*?[") {
			for _, f := range b.list {
				if repo.MatchGlob(c, f) {
					return true
				}
			}
			continue
		}
		if b.files[c] || b.dirs[c] {
			return true
		}
	}
	return false
}

// split separates a harness file into code and prose. A file that is not Markdown is all code.
func split(name, text string) (code, prose []string) {
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".mdc", ".mdx", ".markdown":
	default:
		return []string{text}, nil
	}
	var block, para strings.Builder
	fence := ""
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(trimmed, fence) {
				code = append(code, block.String())
				block.Reset()
				fence = ""
				continue
			}
			block.WriteString(line + "\n")
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fence = trimmed[:3]
			continue
		}
		parts := strings.Split(line, "`")
		for i, part := range parts {
			if i%2 == 1 && i < len(parts)-1 {
				code = append(code, part)
			} else {
				para.WriteString(part + " ")
			}
		}
		para.WriteString("\n")
	}
	if fence != "" {
		code = append(code, block.String())
	}
	return code, []string{para.String()}
}

var tokenSplit = regexp.MustCompile(`[\s"'()\[\]<>,;|]+`)

func tokens(s string) []string {
	var out []string
	for _, t := range tokenSplit.Split(s, -1) {
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

func set(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, i := range items {
		m[i] = true
	}
	return m
}

var (
	justRecipe = regexp.MustCompile(`^@?([A-Za-z_][A-Za-z0-9_-]*)`)
	justAlias  = regexp.MustCompile(`^alias\s+([A-Za-z_][A-Za-z0-9_-]*)\s*:=`)
)

// JustRecipes lists the recipes and aliases a justfile defines.
func JustRecipes(justfile []byte) []string {
	var out []string
	for _, line := range strings.Split(string(justfile), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' || line[0] == '[' {
			continue
		}
		if m := justAlias.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
			continue
		}
		m := justRecipe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		switch m[1] {
		case "set", "export", "import", "mod", "alias":
			if !strings.HasPrefix(strings.TrimSpace(line[len(m[0]):]), ":") {
				continue
			}
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 || (colon+1 < len(line) && line[colon+1] == '=') {
			continue
		}
		if eq := strings.Index(line, ":="); eq >= 0 && eq < colon {
			continue
		}
		out = append(out, m[1])
	}
	return out
}

var makeRule = regexp.MustCompile(`^([^\s:#=][^:#=]*?)\s*::?(?:[^=]|$)`)

// MakeTargets lists the explicit targets a Makefile defines: not pattern rules, not special targets
// such as .PHONY, and not targets built from variables.
func MakeTargets(makefile []byte) []string {
	var out []string
	for _, line := range strings.Split(string(makefile), "\n") {
		line = strings.TrimRight(line, "\r")
		m := makeRule.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		for _, t := range strings.Fields(m[1]) {
			if strings.ContainsAny(t, "%$") || strings.HasPrefix(t, ".") {
				continue
			}
			out = append(out, t)
		}
	}
	return out
}

// PackageScripts lists the scripts of a package.json.
func PackageScripts(packageJSON []byte) ([]string, error) {
	var pkg struct {
		Scripts map[string]json.RawMessage `json:"scripts"`
	}
	if err := json.Unmarshal(packageJSON, &pkg); err != nil {
		return nil, fmt.Errorf("read package.json: %w", err)
	}
	out := make([]string, 0, len(pkg.Scripts))
	for name := range pkg.Scripts {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}
