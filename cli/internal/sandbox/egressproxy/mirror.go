package egressproxy

// The registry mirror (docs/specs/evaluations.md, "The registry mirror"). A registry can serve a
// public repository at any commit, which would hand an agent the merged fix of its case. In mirror
// mode the proxy also answers plain-HTTP requests under /go/, /npm/, /pypi/, /nuget/ and /maven/,
// forwards them to the public registries over HTTPS, and refuses every request for a package of
// the workspace under test. Direct CONNECT and forwarding to the registry hosts are refused, so
// the mirror is the only way to them.
//
// This file is compiled into the proxy image with proxy.go, so it imports only the standard
// library.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Deny lists the package identities the mirror refuses.
type Deny struct {
	Go     []string // module paths; every module below one is refused too
	NPM    []string // package names, scoped ("@scope/name") or not
	Python []string // project names, compared after PEP 503 normalization
	NuGet  []string // package IDs, compared without case
	Maven  []string // groupId:artifactId
}

// Mirror is the proxy's registry mirror.
type Mirror struct {
	base  string // the URL the sandbox reaches the proxy at, without a trailing slash
	host  string // base's host:port, lower case
	deny  denySets
	pairs []string // upstream URL prefix, mirror URL prefix, … for rewriting bodies and redirects

	// Upstream sends the mirror's requests to the registries. Nil means HTTPS through the
	// proxy's dialer.
	Upstream http.RoundTripper

	once      sync.Once
	transport http.RoundTripper
}

type denySets struct {
	goPrefixes []string // lower case
	npm        map[string]bool
	python     map[string]bool
	nuget      map[string]bool
	maven      [][]string // group segments, then the artifactId
}

// RegistryHosts are the public registries the mirror stands in for. With the mirror on, the proxy
// refuses to reach them directly, whatever the allow-list says.
var RegistryHosts = map[string]bool{
	"proxy.golang.org": true, "sum.golang.org": true, "index.golang.org": true,
	"registry.npmjs.org": true, "registry.npmjs.com": true, "registry.yarnpkg.com": true,
	"pypi.org": true, "pypi.python.org": true, "files.pythonhosted.org": true,
	"api.nuget.org": true, "globalcdn.nuget.org": true, "www.nuget.org": true, "nuget.org": true,
	"repo.maven.apache.org": true, "repo1.maven.org": true,
}

// The mirror's routes: a path prefix on the proxy and the upstream URL it stands for. The first
// match wins, so the checksum database comes before the Go proxy.
var mirrorRoutes = []struct {
	ecosystem string
	prefix    string
	upstream  string
	rewrite   bool // the answers carry upstream URLs that must point at the mirror
}{
	{"gosumdb", "/go/sumdb/sum.golang.org/", "https://sum.golang.org/", false},
	{"go", "/go/", "https://proxy.golang.org/", false},
	{"npm", "/npm/", "https://registry.npmjs.org/", true},
	{"pypi", "/pypi/simple/", "https://pypi.org/simple/", true},
	{"pypifiles", "/pypi/files/", "https://files.pythonhosted.org/", false},
	{"nuget", "/nuget/", "https://api.nuget.org/", true},
	{"maven", "/maven/", "https://repo.maven.apache.org/maven2/", false},
}

// NewMirror returns a mirror that the sandbox reaches at base (such as
// http://casebox-egress:3128) and that refuses the identities in deny.
func NewMirror(base string, deny Deny) (*Mirror, error) {
	u, err := url.Parse(strings.TrimSuffix(base, "/"))
	if err != nil || u.Scheme != "http" || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.User != nil {
		return nil, fmt.Errorf("the mirror address %q is not http://host:port", base)
	}
	sets, err := parseDeny(deny)
	if err != nil {
		return nil, err
	}
	m := &Mirror{base: u.String(), host: strings.ToLower(u.Host), deny: sets}
	for _, r := range mirrorRoutes {
		if r.ecosystem == "gosumdb" || r.ecosystem == "go" || r.ecosystem == "maven" {
			continue
		}
		m.pairs = append(m.pairs, r.upstream, m.base+r.prefix)
	}
	m.pairs = append(m.pairs, "http://registry.npmjs.org/", m.base+"/npm/")
	return m, nil
}

func parseDeny(d Deny) (denySets, error) {
	s := denySets{npm: map[string]bool{}, python: map[string]bool{}, nuget: map[string]bool{}}
	check := func(kind, v string) (string, error) {
		v = strings.TrimSpace(v)
		if v == "" || strings.ContainsAny(v, ", \t\r\n") {
			return "", fmt.Errorf("the denied %s package %q is empty or holds a comma or a space", kind, v)
		}
		return v, nil
	}
	for _, raw := range d.Go {
		v, err := check("Go", raw)
		if err != nil {
			return s, err
		}
		s.goPrefixes = append(s.goPrefixes, strings.ToLower(strings.TrimSuffix(v, "/")))
	}
	for _, raw := range d.NPM {
		v, err := check("npm", raw)
		if err != nil {
			return s, err
		}
		s.npm[strings.ToLower(v)] = true
	}
	for _, raw := range d.Python {
		v, err := check("Python", raw)
		if err != nil {
			return s, err
		}
		s.python[NormalizePython(v)] = true
	}
	for _, raw := range d.NuGet {
		v, err := check("NuGet", raw)
		if err != nil {
			return s, err
		}
		s.nuget[strings.ToLower(v)] = true
	}
	for _, raw := range d.Maven {
		v, err := check("Maven", raw)
		if err != nil {
			return s, err
		}
		group, artifact, ok := strings.Cut(v, ":")
		if !ok || group == "" || artifact == "" || strings.Contains(artifact, ":") || strings.ContainsAny(group+artifact, "/\\") {
			return s, fmt.Errorf("the denied Maven package %q is not groupId:artifactId", v)
		}
		s.maven = append(s.maven, append(strings.Split(group, "."), artifact))
	}
	return s, nil
}

var pythonSeparators = regexp.MustCompile(`[-_.]+`)

// NormalizePython is a Python project name as PEP 503 compares it.
func NormalizePython(name string) string {
	return pythonSeparators.ReplaceAllString(strings.ToLower(name), "-")
}

// Serves says whether r is a request to the mirror: a path on the proxy itself, or an
// absolute-form request whose host is the mirror's own (a tool that ignored NO_PROXY).
func (m *Mirror) Serves(r *http.Request) bool {
	if r.Method == http.MethodConnect {
		return false
	}
	if !r.URL.IsAbs() {
		return true
	}
	return r.URL.Scheme == "http" && strings.EqualFold(r.URL.Host, m.host)
}

// Refused returns the package identity that a request for path (as the proxy received it,
// percent-decoded) asks for, when the mirror refuses it, and "" otherwise.
func (m *Mirror) Refused(path string) string {
	for _, r := range mirrorRoutes {
		if rest, ok := strings.CutPrefix(path, r.prefix); ok {
			return m.deny.refused(r.ecosystem, strings.TrimLeft(rest, "/"))
		}
	}
	return ""
}

func (d denySets) refused(ecosystem, rest string) string {
	switch ecosystem {
	case "go":
		return d.goModule(goModulePath(rest))
	case "gosumdb":
		if lookup, ok := strings.CutPrefix(rest, "lookup/"); ok {
			module, _, _ := strings.Cut(lookup, "@")
			return d.goModule(unescapeGo(module))
		}
	case "npm":
		if name := npmName(rest); name != "" && d.npm[strings.ToLower(name)] {
			return "npm package " + name
		}
	case "pypi":
		name, _, _ := strings.Cut(rest, "/")
		if name != "" && d.python[NormalizePython(name)] {
			return "Python project " + name
		}
	case "pypifiles":
		segs := strings.Split(strings.TrimSuffix(rest, "/"), "/")
		// packages/source/<letter>/<project>/<file> is the legacy form; the others hash the path.
		if len(segs) >= 4 && segs[0] == "packages" && segs[1] == "source" && d.python[NormalizePython(segs[3])] {
			return "Python project " + segs[3]
		}
		if name := pythonDist(segs[len(segs)-1]); name != "" && d.python[NormalizePython(name)] {
			return "Python project " + name
		}
	case "nuget":
		segs := strings.Split(rest, "/")
		id := ""
		switch {
		case len(segs) >= 2 && segs[0] == "v3-flatcontainer":
			id = segs[1]
		case len(segs) >= 3 && segs[0] == "v3" && strings.HasPrefix(segs[1], "registration"):
			id = segs[2]
		}
		if id != "" && d.nuget[strings.ToLower(id)] {
			return "NuGet package " + id
		}
	case "maven":
		segs := strings.Split(rest, "/")
		for _, denied := range d.maven {
			if len(segs) > len(denied) && equalSegs(segs[:len(denied)], denied) {
				return "Maven package " + strings.Join(denied[:len(denied)-1], ".") + ":" + denied[len(denied)-1]
			}
		}
	}
	return ""
}

func (d denySets) goModule(module string) string {
	lower := strings.ToLower(strings.Trim(module, "/"))
	for _, p := range d.goPrefixes {
		if lower == p || strings.HasPrefix(lower, p+"/") {
			return "Go module " + module
		}
	}
	return ""
}

// goModulePath is the module path of a GOPROXY request path (<module>/@v/list, <module>/@latest,
// <module>/@v/<version>.info|.mod|.zip), with its capitals unescaped. A path without "/@" is
// taken whole.
func goModulePath(rest string) string {
	module, _, _ := strings.Cut(rest, "/@")
	return unescapeGo(module)
}

// unescapeGo undoes the module proxy's case encoding: "!x" stands for "X".
func unescapeGo(s string) string {
	var b strings.Builder
	bang := false
	for _, c := range s {
		switch {
		case c == '!' && !bang:
			bang = true
			continue
		case bang && c >= 'a' && c <= 'z':
			c -= 'a' - 'A'
		}
		bang = false
		b.WriteRune(c)
	}
	return b.String()
}

// npmName is the package a registry path names: <name>…, @scope/<name>… (the slash may come
// percent-encoded; the path is decoded already) or -/package/<name>/…. Other /-/ paths (search,
// audit) name no package.
func npmName(rest string) string {
	segs := strings.Split(rest, "/")
	if segs[0] == "-" {
		if len(segs) >= 3 && segs[1] == "package" {
			segs = segs[2:]
		} else {
			return ""
		}
	}
	if strings.HasPrefix(segs[0], "@") {
		if len(segs) < 2 || segs[1] == "" {
			return segs[0]
		}
		return segs[0] + "/" + segs[1]
	}
	return segs[0]
}

// pythonDist is the distribution name in a file name from files.pythonhosted.org: a wheel
// (<name>-<version>-….whl), an sdist (<name>-<version>.tar.gz and the like), an egg, or the
// PEP 658 metadata file of one of them.
func pythonDist(file string) string {
	file = strings.TrimSuffix(file, ".metadata")
	lower := strings.ToLower(file)
	if strings.HasSuffix(lower, ".whl") || strings.HasSuffix(lower, ".egg") {
		name, _, _ := strings.Cut(file, "-")
		return name
	}
	for _, ext := range []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tgz", ".tar", ".zip", ".exe", ".msi", ".rpm"} {
		if strings.HasSuffix(lower, ext) {
			file = file[:len(file)-len(ext)]
			break
		}
	}
	for i := len(file) - 2; i > 0; i-- {
		if file[i] == '-' && file[i+1] >= '0' && file[i+1] <= '9' {
			return file[:i]
		}
	}
	return file
}

// plainPath refuses what a registry could read differently from the mirror's checks: dot
// segments, doubled slashes and backslashes.
func plainPath(rest string) bool {
	if strings.Contains(rest, "\\") {
		return false
	}
	segs := strings.Split(strings.TrimSuffix(rest, "/"), "/")
	for i, seg := range segs {
		if seg == "." || seg == ".." || (seg == "" && !(i == 0 && len(segs) == 1)) {
			return false
		}
	}
	return true
}

func equalSegs(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// serveMirror answers a request to the mirror.
func (p *Proxy) serveMirror(w http.ResponseWriter, r *http.Request) {
	m := p.Mirror
	path := r.URL.Path
	escaped := r.URL.EscapedPath()
	var route = -1
	for i, rt := range mirrorRoutes {
		if strings.HasPrefix(path, rt.prefix) && strings.HasPrefix(escaped, rt.prefix) {
			route = i
			break
		}
	}
	if route < 0 {
		p.refuseMirror(w, http.StatusNotFound, r, "the registry mirror serves /go/, /npm/, /pypi/simple/, /pypi/files/, /nuget/ and /maven/ only")
		return
	}
	rt := mirrorRoutes[route]
	rest := strings.TrimLeft(strings.TrimPrefix(escaped, rt.prefix), "/")
	if !plainPath(strings.TrimLeft(strings.TrimPrefix(path, rt.prefix), "/")) {
		p.refuseMirror(w, http.StatusBadRequest, r, "the path has an empty, . or .. segment or a backslash")
		return
	}
	switch {
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
	case r.Method == http.MethodPost && rt.ecosystem == "npm" && strings.HasPrefix(rest, "-/npm/v1/security/"):
	default:
		p.refuseMirror(w, http.StatusMethodNotAllowed, r, "the registry mirror only reads")
		return
	}
	if identity := m.Refused(path); identity != "" {
		p.refuseMirror(w, http.StatusNotFound, r, identity+" belongs to the workspace under test; the registry mirror refuses it, so it cannot come from the registry")
		return
	}
	if rt.ecosystem == "gosumdb" && rest == "supported" {
		p.logf("mirror %s %s", r.Method, path)
		w.WriteHeader(http.StatusOK)
		return
	}
	target := rt.upstream + rest
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		p.refuseMirror(w, http.StatusBadRequest, r, err.Error())
		return
	}
	out.ContentLength = r.ContentLength
	for k, vs := range r.Header {
		out.Header[k] = append([]string(nil), vs...)
	}
	dropHopHeaders(out.Header)
	// The upstream answer is decompressed by the transport so it can be rewritten; no credential
	// of the sandbox goes to a registry.
	for _, h := range []string{"Accept-Encoding", "Authorization", "Cookie", "Host"} {
		out.Header.Del(h)
	}
	if rt.rewrite {
		out.Header.Del("Range")
		out.Header.Del("If-Range")
	}
	resp, err := m.upstream(p.dial).RoundTrip(out)
	if err != nil {
		p.refuseMirror(w, http.StatusBadGateway, r, err.Error())
		return
	}
	defer resp.Body.Close()
	p.logf("mirror %s %s: %d", r.Method, path, resp.StatusCode)
	dropHopHeaders(resp.Header)
	rewrite := rt.rewrite && r.Method != http.MethodHead && textual(resp.Header.Get("Content-Type"))
	if rewrite {
		resp.Header.Del("Content-Length")
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Content-MD5")
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			if k == "Location" {
				v = strings.NewReplacer(m.pairs...).Replace(v)
			}
			w.Header().Add(k, v)
		}
	}
	var body io.Reader = resp.Body
	if rt.ecosystem == "nuget" && rest == "v3/index.json" && resp.StatusCode == http.StatusOK && r.Method == http.MethodGet {
		index, err := nugetIndex(resp.Body)
		if err != nil {
			p.refuseMirror(w, http.StatusBadGateway, r, "the NuGet service index: "+err.Error())
			return
		}
		body = bytes.NewReader(index)
	}
	w.WriteHeader(resp.StatusCode)
	if rewrite {
		_ = rewriteCopy(w, body, m.pairs)
		return
	}
	_, _ = io.Copy(w, body)
}

// nugetIndex drops the RepositorySignatures resources from NuGet's service index: NuGet refuses
// them over HTTP, and the mirror serves HTTP. Package hashes and author signatures are still
// checked.
func nugetIndex(r io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 4<<20))
	if err != nil {
		return nil, err
	}
	var index map[string]json.RawMessage
	if err := json.Unmarshal(raw, &index); err != nil {
		return nil, err
	}
	var resources []map[string]any
	if err := json.Unmarshal(index["resources"], &resources); err != nil {
		return nil, err
	}
	kept := resources[:0]
	for _, res := range resources {
		if kind, _ := res["@type"].(string); strings.HasPrefix(kind, "RepositorySignatures/") {
			continue
		}
		kept = append(kept, res)
	}
	if index["resources"], err = json.Marshal(kept); err != nil {
		return nil, err
	}
	return json.Marshal(index)
}

func (p *Proxy) refuseMirror(w http.ResponseWriter, code int, r *http.Request, msg string) {
	p.logf("mirror deny %s %s: %s", r.Method, r.URL.Path, msg)
	http.Error(w, "casebox registry mirror: "+msg, code)
}

func (m *Mirror) upstream(dial func(ctx context.Context, network, addr string) (net.Conn, error)) http.RoundTripper {
	if m.Upstream != nil {
		return m.Upstream
	}
	m.once.Do(func() {
		m.transport = &http.Transport{
			DialContext:           dial,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 5 * time.Minute,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   16,
		}
	})
	return m.transport
}

// textual says whether a content type is a document that may carry registry URLs.
func textual(contentType string) bool {
	ct := strings.ToLower(contentType)
	return strings.Contains(ct, "json") || strings.Contains(ct, "html") || strings.Contains(ct, "xml") || strings.HasPrefix(ct, "text/")
}

// rewriteCopy copies src to dst and replaces each pairs[2i] with pairs[2i+1] as it goes, holding
// back only as many bytes as a match could still need.
func rewriteCopy(dst io.Writer, src io.Reader, pairs []string) error {
	from := make([][]byte, 0, len(pairs)/2)
	longest := 0
	for i := 0; i < len(pairs); i += 2 {
		from = append(from, []byte(pairs[i]))
		longest = max(longest, len(pairs[i]))
	}
	var pending []byte
	buf := make([]byte, 64<<10)
	for {
		n, rerr := src.Read(buf)
		pending = append(pending, buf[:n]...)
		eof := errors.Is(rerr, io.EOF)
		if rerr != nil && !eof {
			return rerr
		}
		for {
			at, which := -1, -1
			for i, f := range from {
				if j := bytes.Index(pending, f); j >= 0 && (at < 0 || j < at) {
					at, which = j, i
				}
			}
			if at < 0 {
				break
			}
			if _, err := dst.Write(pending[:at]); err != nil {
				return err
			}
			if _, err := io.WriteString(dst, pairs[2*which+1]); err != nil {
				return err
			}
			pending = pending[at+len(from[which]):]
		}
		keep := 0
		if !eof {
			keep = min(len(pending), longest-1)
		}
		if _, err := dst.Write(pending[:len(pending)-keep]); err != nil {
			return err
		}
		pending = append(pending[:0], pending[len(pending)-keep:]...)
		if eof {
			return nil
		}
	}
}
