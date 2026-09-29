package egressproxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
)

var testDeny = Deny{
	Go:     []string{"github.com/Acme/Widget"},
	NPM:    []string{"@acme/widget", "left-pad"},
	Python: []string{"Acme_Widget.Core"},
	NuGet:  []string{"Acme.Widget"},
	Maven:  []string{"com.acme:widget"},
}

func testMirror(t *testing.T) *Mirror {
	t.Helper()
	m, err := NewMirror("http://casebox-egress:3128", testDeny)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMirrorRefused(t *testing.T) {
	m := testMirror(t)
	for path, refused := range map[string]bool{
		// Go: escaped capitals, every endpoint, modules below the denied path, the checksum database.
		"/go/github.com/!acme/!widget/@v/list":                            true,
		"/go/github.com/!acme/!widget/@latest":                            true,
		"/go/github.com/!acme/!widget/@v/v1.2.3.info":                     true,
		"/go/github.com/!acme/!widget/@v/v1.2.3.mod":                      true,
		"/go/github.com/!acme/!widget/@v/v1.2.3.zip":                      true,
		"/go/github.com/acme/widget/@v/list":                              true,
		"/go/github.com/!acme/!widget/v2/@v/list":                         true,
		"/go/github.com/!acme/!widget/internal/x/@latest":                 true,
		"//go/github.com/!acme/!widget/@v/list":                           false, // not a mirror path at all
		"/go//github.com/!acme/!widget/@v/list":                           true,
		"/go/github.com/!acme/!widgetry/@v/list":                          false,
		"/go/github.com/!acme/@v/list":                                    false,
		"/go/golang.org/x/text/@v/v0.14.0.zip":                            false,
		"/go/sumdb/sum.golang.org/lookup/github.com/!acme/!widget@v1.2.3": true,
		"/go/sumdb/sum.golang.org/lookup/golang.org/x/text@v0.14.0":       false,
		"/go/sumdb/sum.golang.org/tile/8/0/001":                           false,
		// npm: plain, scoped (the %2f arrives decoded), tarballs, dist-tags, search.
		"/npm/left-pad":                         true,
		"/npm/Left-Pad":                         true,
		"/npm/left-pad/-/left-pad-1.3.0.tgz":    true,
		"/npm/left-pad-extra":                   false,
		"/npm/@acme/widget":                     true,
		"/npm/@acme/widget/-/widget-1.0.0.tgz":  true,
		"/npm/@acme/widget-2":                   false,
		"/npm/@acme":                            false,
		"/npm/-/package/left-pad/dist-tags":     true,
		"/npm/-/package/@acme/widget/dist-tags": true,
		"/npm/-/v1/search":                      false,
		"/npm/react":                            false,
		// Python: PEP 503 names in the simple index, wheels, sdists and metadata files.
		"/pypi/simple/acme-widget-core/":  true,
		"/pypi/simple/Acme.Widget_Core/":  true,
		"/pypi/simple/acme--widget__core": true,
		"/pypi/simple/acme-widget/":       false,
		"/pypi/simple/":                   false,
		"/pypi/files/packages/ab/cd/ef0123/acme_widget_core-1.0-py3-none-any.whl":          true,
		"/pypi/files/packages/ab/cd/ef0123/acme_widget_core-1.0-py3-none-any.whl.metadata": true,
		"/pypi/files/packages/ab/cd/ef0123/Acme.Widget.Core-1.0.tar.gz":                    true,
		"/pypi/files/packages/source/a/acme-widget-core/acme-widget-core-1.0.zip":          true,
		"/pypi/files/packages/ab/cd/ef0123/requests-2.31.0-py3-none-any.whl":               false,
		"/pypi/files/packages/ab/cd/ef0123/acme-widget-core-extras-1.0.tar.gz":             false,
		// NuGet: flat container and every registration hive, without case.
		"/nuget/v3/index.json":                                              false,
		"/nuget/v3-flatcontainer/acme.widget/index.json":                    true,
		"/nuget/v3-flatcontainer/acme.widget/1.0.0/acme.widget.1.0.0.nupkg": true,
		"/nuget/v3-flatcontainer/acme.widget/1.0.0/acme.widget.nuspec":      true,
		"/nuget/v3/registration5-gz-semver2/acme.widget/index.json":         true,
		"/nuget/v3/registration5-semver1/Acme.Widget/page/1.0.0/2.0.0.json": true,
		"/nuget/v3/registration5-gz-semver2/acme.widget/1.0.0.json":         true,
		"/nuget/v3-flatcontainer/acme.widget.extra/index.json":              false,
		"/nuget/v3-flatcontainer/newtonsoft.json/index.json":                false,
		// Maven: artifacts and metadata below groupId/artifactId.
		"/maven/com/acme/widget/maven-metadata.xml":            true,
		"/maven/com/acme/widget/1.0/widget-1.0.jar":            true,
		"/maven/com/acme/widget/1.0/widget-1.0.pom.sha1":       true,
		"/maven/com/acme/widget-api/1.0/widget-api-1.0.jar":    false,
		"/maven/com/acme/maven-metadata.xml":                   false,
		"/maven/org/apache/commons/commons-lang3/3.14.0/x.jar": false,
	} {
		if got := m.Refused(path) != ""; got != refused {
			t.Errorf("Refused(%q) = %q, want refused %v", path, m.Refused(path), refused)
		}
	}
}

func TestMirrorConfig(t *testing.T) {
	for _, bad := range []Deny{{Maven: []string{"com.acme"}}, {Maven: []string{"com.acme:a:b"}}, {NPM: []string{"a,b"}}, {Go: []string{" "}}} {
		if _, err := NewMirror("http://casebox-egress:3128", bad); err == nil {
			t.Errorf("NewMirror accepted %+v", bad)
		}
	}
	for _, base := range []string{"https://casebox-egress:3128", "http://casebox-egress:3128/x", "casebox-egress:3128"} {
		if _, err := NewMirror(base, Deny{}); err == nil {
			t.Errorf("NewMirror accepted the address %q", base)
		}
	}
}

// fakeRegistries answers as the public registries do, with their own absolute URLs in the bodies.
type fakeRegistries struct {
	mu   sync.Mutex
	seen []string
}

func (f *fakeRegistries) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.String())
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "" {
		return nil, fmt.Errorf("a credential reached the registry")
	}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Request: r}
	body := ""
	switch r.URL.Host + r.URL.Path {
	case "registry.npmjs.org/react":
		resp.Header.Set("Content-Type", "application/vnd.npm.install-v1+json")
		body = `{"name":"react","versions":{"18.0.0":{"dist":{"tarball":"https://registry.npmjs.org/react/-/react-18.0.0.tgz"}}}}`
	case "registry.npmjs.org/@types/node":
		resp.Header.Set("Content-Type", "application/json")
		body = `{"name":"@types/node"}`
	case "pypi.org/simple/requests/":
		resp.Header.Set("Content-Type", "text/html")
		body = `<a href="https://files.pythonhosted.org/packages/ab/cd/requests-2.31.0-py3-none-any.whl#sha256=00">requests-2.31.0-py3-none-any.whl</a>`
	case "pypi.org/simple/Requests/":
		resp.StatusCode = http.StatusMovedPermanently
		resp.Header.Set("Location", "https://pypi.org/simple/requests/")
	case "api.nuget.org/v3/index.json":
		resp.Header.Set("Content-Type", "application/json")
		body = `{"version":"3.0.0","resources":[{"@id":"https://api.nuget.org/v3-flatcontainer/","@type":"PackageBaseAddress/3.0.0"},{"@id":"https://api.nuget.org/v3-index/repository-signatures/5.0.0/index.json","@type":"RepositorySignatures/5.0.0"}]}`
	case "repo.maven.apache.org/maven2/org/example/lib/1.0/lib-1.0.jar":
		resp.Header.Set("Content-Type", "application/java-archive")
		body = "PK https://api.nuget.org/ stays as it is in a binary"
	default:
		resp.StatusCode = http.StatusNotFound
	}
	resp.Body = io.NopCloser(strings.NewReader(body))
	resp.ContentLength = int64(len(body))
	return resp, nil
}

func TestMirrorServes(t *testing.T) {
	m := testMirror(t)
	up := &fakeRegistries{}
	m.Upstream = up
	allow, _ := ParseAllow([]string{"proxy.golang.org", "example.org"})
	srv := httptest.NewServer(&Proxy{Allow: allow, Mirror: m})
	defer srv.Close()

	get := func(method, path string, header http.Header) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		for k, v := range header {
			req.Header[k] = v
		}
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, string(body)
	}

	resp, body := get("GET", "/npm/react", http.Header{"Authorization": {"Bearer secret"}, "Accept": {"application/vnd.npm.install-v1+json"}})
	if resp.StatusCode != 200 || !strings.Contains(body, `"tarball":"http://casebox-egress:3128/npm/react/-/react-18.0.0.tgz"`) {
		t.Errorf("npm packument: %d %s", resp.StatusCode, body)
	}
	if resp, _ := get("GET", "/npm/@types%2fnode", nil); resp.StatusCode != 200 {
		t.Errorf("a scoped npm package with %%2f answered %d", resp.StatusCode)
	}
	resp, body = get("GET", "/pypi/simple/requests/", nil)
	if !strings.Contains(body, `href="http://casebox-egress:3128/pypi/files/packages/ab/cd/requests-2.31.0-py3-none-any.whl#sha256=00"`) {
		t.Errorf("pypi simple index: %d %s", resp.StatusCode, body)
	}
	resp, _ = get("GET", "/pypi/simple/Requests/", nil)
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusMovedPermanently || loc != "http://casebox-egress:3128/pypi/simple/requests/" {
		t.Errorf("pypi redirect: %d %q", resp.StatusCode, loc)
	}
	_, body = get("GET", "/nuget/v3/index.json", nil)
	if !strings.Contains(body, `"@id":"http://casebox-egress:3128/nuget/v3-flatcontainer/"`) || strings.Contains(body, "RepositorySignatures") || !strings.Contains(body, `"version":"3.0.0"`) {
		t.Errorf("NuGet service index: %s", body)
	}
	_, body = get("GET", "/maven/org/example/lib/1.0/lib-1.0.jar", nil)
	if body != "PK https://api.nuget.org/ stays as it is in a binary" {
		t.Errorf("a Maven jar was changed: %q", body)
	}
	if resp, _ := get("GET", "/go/sumdb/sum.golang.org/supported", nil); resp.StatusCode != 200 {
		t.Errorf("the checksum database is not supported through the mirror: %d", resp.StatusCode)
	}

	up.seen = nil
	for _, path := range []string{"/npm/left-pad", "/npm/@acme%2fwidget", "/go/github.com/!acme/!widget/@v/list", "/pypi/simple/acme-widget-core/", "/nuget/v3-flatcontainer/acme.widget/index.json", "/maven/com/acme/widget/maven-metadata.xml"} {
		if resp, body := get("GET", path, nil); resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "workspace under test") {
			t.Errorf("GET %s answered %d %q, want 404 with the reason", path, resp.StatusCode, body)
		}
	}
	for _, path := range []string{"/npm/react/../left-pad", "/npm/react/%2e%2e/left-pad", "/maven/com//acme/widget/1.0/widget-1.0.jar", "/npm/react\\..\\left-pad"} {
		if resp, _ := get("GET", path, nil); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s answered %d, want 400", path, resp.StatusCode)
		}
	}
	if resp, _ := get("PUT", "/npm/react", nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT answered %d, want 405", resp.StatusCode)
	}
	if resp, _ := get("GET", "/elsewhere/x", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a path outside the mirror answered %d", resp.StatusCode)
	}
	if len(up.seen) != 0 {
		t.Fatalf("refused requests reached the registries: %v", up.seen)
	}

	// A tool that ignores NO_PROXY sends absolute-form requests for the mirror's own host.
	proxyURL, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	resp2, err := client.Get("http://casebox-egress:3128/npm/react")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Errorf("an absolute-form request to the mirror answered %d", resp2.StatusCode)
	}

	// The registry hosts are refused directly even when the allow-list names them.
	for _, target := range []string{"proxy.golang.org:443", "registry.npmjs.org:443", "Files.PythonHosted.org.:443"} {
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		conn.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("CONNECT %s answered %d, want 403", target, resp.StatusCode)
		}
	}
	resp3, err := client.Get("http://registry.npmjs.org/react")
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusForbidden {
		t.Errorf("GET http://registry.npmjs.org/ answered %d, want 403", resp3.StatusCode)
	}
}

func TestRewriteCopyAcrossReads(t *testing.T) {
	pairs := []string{"https://registry.npmjs.org/", "http://m/npm/", "https://api.nuget.org/", "http://m/nuget/"}
	in := strings.Repeat(`x"https://registry.npmjs.org/a" https://api.nuget.org/v3 https://registry.npmjs.or`, 50)
	want := strings.NewReplacer(pairs...).Replace(in)
	var out bytes.Buffer
	if err := rewriteCopy(&out, iotest.OneByteReader(strings.NewReader(in)), pairs); err != nil {
		t.Fatal(err)
	}
	if out.String() != want {
		t.Fatalf("rewriteCopy gave\n%s\nwant\n%s", out.String(), want)
	}
}
