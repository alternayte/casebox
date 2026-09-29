package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/alternayte/casebox/cli/internal/sandbox"
)

// IsManifest says whether a repository path is a file DeniedPackages reads: go.mod, package.json,
// pyproject.toml, pom.xml or a .NET project file, outside dependency and fixture folders
// (node_modules, vendor, testdata), whose manifests name other people's packages.
func IsManifest(name string) bool {
	for _, seg := range strings.Split(path.Dir(name), "/") {
		if seg == "node_modules" || seg == "vendor" || seg == "testdata" {
			return false
		}
	}
	switch base := path.Base(name); base {
	case "go.mod", "package.json", "pyproject.toml", "pom.xml":
		return true
	default:
		switch path.Ext(base) {
		case ".csproj", ".fsproj", ".vbproj":
			return true
		}
	}
	return false
}

// DeniedPackages reads the package identities of the workspace from its repositories' manifests
// (repository path to contents; paths that are not manifests are skipped): the module line of
// every go.mod, the name of every package.json, the project name of every pyproject.toml, the
// PackageId, AssemblyName and file name of every .NET project, and the groupId:artifactId of every
// pom.xml. The registry mirror refuses them, since a registry could serve them at any commit.
func DeniedPackages(files map[string][]byte) sandbox.Mirror {
	sets := map[string]map[string]bool{}
	add := func(kind, v string) {
		v = strings.TrimSpace(v)
		if v == "" || strings.ContainsAny(v, ", \t\r\n") || strings.Contains(v, "$(") {
			return
		}
		if sets[kind] == nil {
			sets[kind] = map[string]bool{}
		}
		sets[kind][v] = true
	}
	for name, body := range files {
		if !IsManifest(name) {
			continue
		}
		base := path.Base(name)
		switch {
		case base == "go.mod":
			add("go", goModule(body))
		case base == "package.json":
			var pkg struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(body, &pkg) == nil {
				add("npm", pkg.Name)
			}
		case base == "pyproject.toml":
			for _, n := range pyprojectNames(body) {
				add("python", n)
			}
		case base == "pom.xml":
			if g, a := pomIdentity(body); g != "" && a != "" && !strings.Contains(g+a, "${") {
				add("maven", g+":"+a)
			}
		default:
			add("nuget", strings.TrimSuffix(base, path.Ext(base)))
			for _, id := range projectIDs(body) {
				add("nuget", id)
			}
		}
	}
	list := func(kind string) []string {
		var out []string
		for v := range sets[kind] {
			out = append(out, v)
		}
		sort.Strings(out)
		return out
	}
	return sandbox.Mirror{Denied: sandbox.Denied{
		Go:     list("go"),
		NPM:    list("npm"),
		Python: list("python"),
		NuGet:  list("nuget"),
		Maven:  list("maven"),
	}}
}

// goModule is the path on a go.mod's module line.
func goModule(body []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "//")
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "module" {
			continue
		}
		if unquoted, err := strconv.Unquote(fields[1]); err == nil {
			return unquoted
		}
		return fields[1]
	}
	return ""
}

var (
	tomlTable = regexp.MustCompile(`^\[\s*([A-Za-z0-9_.\-" ]+?)\s*\]\s*(#.*)?$`)
	tomlName  = regexp.MustCompile(`^name\s*=\s*(?:"([^"]*)"|'([^']*)')\s*(#.*)?$`)
)

// pyprojectNames are the names in a pyproject.toml's [project] and [tool.poetry] tables.
func pyprojectNames(body []byte) []string {
	var names []string
	table := ""
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if m := tomlTable.FindStringSubmatch(line); m != nil {
			table = strings.ReplaceAll(strings.ReplaceAll(m[1], `"`, ""), " ", "")
			continue
		}
		if table != "project" && table != "tool.poetry" {
			continue
		}
		if m := tomlName.FindStringSubmatch(line); m != nil {
			names = append(names, m[1]+m[2])
		}
	}
	return names
}

// pomIdentity is a pom.xml's own groupId (inherited from its parent when it has none) and
// artifactId.
func pomIdentity(body []byte) (string, string) {
	var pom struct {
		GroupID    string `xml:"groupId"`
		ArtifactID string `xml:"artifactId"`
		Parent     struct {
			GroupID string `xml:"groupId"`
		} `xml:"parent"`
	}
	if xml.Unmarshal(body, &pom) != nil {
		return "", ""
	}
	group := strings.TrimSpace(pom.GroupID)
	if group == "" {
		group = strings.TrimSpace(pom.Parent.GroupID)
	}
	return group, strings.TrimSpace(pom.ArtifactID)
}

// projectIDs are the PackageId and AssemblyName values of a .NET project file.
func projectIDs(body []byte) []string {
	var ids []string
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.Strict = false
	want := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return ids
		}
		switch t := tok.(type) {
		case xml.StartElement:
			want = t.Name.Local == "PackageId" || t.Name.Local == "AssemblyName"
		case xml.CharData:
			if want {
				ids = append(ids, strings.TrimSpace(string(t)))
			}
		case xml.EndElement:
			want = false
		}
	}
}
