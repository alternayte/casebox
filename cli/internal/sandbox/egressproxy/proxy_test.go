package egressproxy

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestAllow(t *testing.T) {
	a, err := ParseAllow([]string{"registry.npmjs.org", "*.pkg.dev", " Proxy.Golang.org. ", ""})
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]bool{
		"registry.npmjs.org":      true,
		"REGISTRY.npmjs.org.":     true,
		"proxy.golang.org":        true,
		"evil.registry.npmjs.org": false,
		"registry.npmjs.org.evil": false,
		"npmjs.org":               false,
		"us-docker.pkg.dev":       true,
		"a.b.pkg.dev":             true,
		"pkg.dev":                 false,
		"evilpkg.dev":             false,
		"github.com":              false,
		"":                        false,
		"registry.npmjs.org:443":  false,
	} {
		if got := a.Permits(host); got != want {
			t.Errorf("Permits(%q) = %v, want %v", host, got, want)
		}
	}
	for _, bad := range []string{"*.com", "https://github.com", "github.com:443", "github.com/x", "*", "a..b", "-a.com"} {
		if _, err := ParseAllow([]string{bad}); err == nil {
			t.Errorf("ParseAllow accepted %q", bad)
		}
	}
}

// The proxy must refuse a host off the list and a port other than 80 or 443 before it dials.
func TestProxyRefusesBeforeDialling(t *testing.T) {
	a, _ := ParseAllow([]string{"allowed.test"})
	var dialled []string
	p := &Proxy{Allow: a, Dial: func(_ context.Context, _, addr string) (net.Conn, error) {
		dialled = append(dialled, addr)
		return nil, fmt.Errorf("no network in tests")
	}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	for _, target := range []string{"github.com:443", "allowed.test:22", "allowed.test:8443", "10.0.0.1:443"} {
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
	proxyURL, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	resp, err := client.Get("http://github.com/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET http://github.com/ answered %d, want 403", resp.StatusCode)
	}
	if len(dialled) != 0 {
		t.Fatalf("the proxy dialled %v", dialled)
	}
}
