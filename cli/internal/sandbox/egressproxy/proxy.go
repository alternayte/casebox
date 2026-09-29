// Package egressproxy is the HTTP proxy between a sandbox with an egress allow-list and the
// internet (docs/specs/sandboxes.md, "Egress"). It tunnels CONNECT, and forwards absolute-form
// requests (GET http://… and GET https://…, the second as busybox wget sends it, with TLS from the
// proxy to the host), only to the hosts on its list, on ports 80 and 443. It refuses everything
// else.
//
// This file is also compiled on its own, inside a container, into the proxy image the Docker
// provider runs (image.go), so it imports only the standard library and holds the program's Main.
package egressproxy

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"
)

// Ready is the line Main prints once it listens.
const Ready = "casebox egress proxy ready"

// Ports are the destination ports the proxy connects to.
var Ports = map[string]bool{"80": true, "443": true}

// Allow is a parsed allow-list: exact hosts, and "*.domain" wildcards that match every host below
// domain but not domain itself.
type Allow struct {
	exact    map[string]bool
	suffixes []string // ".domain"
}

// ParseAllow reads host names and "*.domain" wildcards. A wildcard needs at least two labels after
// the "*.", so "*.com" is refused. Empty entries are skipped.
func ParseAllow(hosts []string) (Allow, error) {
	a := Allow{exact: map[string]bool{}}
	for _, raw := range hosts {
		h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
		if h == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(h, "*."); ok {
			if !validHost(rest) || !strings.Contains(rest, ".") {
				return Allow{}, fmt.Errorf("the egress entry %q is not *. followed by a domain of two or more labels", raw)
			}
			a.suffixes = append(a.suffixes, "."+rest)
			continue
		}
		if !validHost(h) {
			return Allow{}, fmt.Errorf("the egress entry %q is not a host name or *.domain (no scheme, port or path)", raw)
		}
		a.exact[h] = true
	}
	return a, nil
}

// Permits says whether the proxy may connect to host.
func (a Allow) Permits(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if !validHost(h) {
		return false
	}
	if a.exact[h] {
		return true
	}
	for _, s := range a.suffixes {
		if strings.HasSuffix(h, s) {
			return true
		}
	}
	return false
}

// Empty is true when the list allows nothing.
func (a Allow) Empty() bool { return len(a.exact) == 0 && len(a.suffixes) == 0 }

func validHost(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// Proxy serves CONNECT tunnels and absolute-form requests to permitted hosts.
type Proxy struct {
	Allow Allow
	// Dial connects to a permitted destination. The default refuses loopback, link-local
	// (cloud metadata), multicast and unspecified addresses after resolving the name.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	Log  *log.Logger
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Some clients (busybox wget, nc) close their write side once the request is sent, and the
	// server cancels a request's context when its client's side closes: the upstream request must
	// outlive that. A client that is really gone ends the copy with a write error instead.
	r = r.WithContext(context.WithoutCancel(r.Context()))
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	if !r.URL.IsAbs() || (r.URL.Scheme != "http" && r.URL.Scheme != "https") {
		p.refuse(w, http.StatusBadRequest, r, "only CONNECT and absolute http:// or https:// requests go through this proxy")
		return
	}
	host, port := r.URL.Hostname(), r.URL.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[r.URL.Scheme]
	}
	if msg := p.check(host, port); msg != "" {
		p.refuse(w, http.StatusForbidden, r, msg)
		return
	}
	p.logf("allow %s %s", r.Method, net.JoinHostPort(host, port))
	out := r.Clone(r.Context())
	out.RequestURI = ""
	dropHopHeaders(out.Header)
	transport := &http.Transport{
		DialContext:           p.dial,
		ResponseHeaderTimeout: 5 * time.Minute,
		DisableKeepAlives:     true,
	}
	resp, err := transport.RoundTrip(out)
	if err != nil {
		p.refuse(w, http.StatusBadGateway, r, err.Error())
		return
	}
	defer resp.Body.Close()
	dropHopHeaders(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// dropHopHeaders removes the headers that belong to one connection, not to the request.
func dropHopHeaders(h http.Header) {
	for _, f := range h.Values("Connection") {
		for _, name := range strings.Split(f, ",") {
			h.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(name)
	}
}

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		p.refuse(w, http.StatusBadRequest, r, "CONNECT needs host:port")
		return
	}
	if msg := p.check(host, port); msg != "" {
		p.refuse(w, http.StatusForbidden, r, msg)
		return
	}
	up, err := p.dial(r.Context(), "tcp", net.JoinHostPort(host, port))
	if err != nil {
		p.refuse(w, http.StatusBadGateway, r, err.Error())
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		p.refuse(w, http.StatusInternalServerError, r, "the connection cannot be tunnelled")
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	p.logf("allow CONNECT %s", net.JoinHostPort(host, port))
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		conn.Close()
		up.Close()
		return
	}
	if n := buf.Reader.Buffered(); n > 0 {
		pending, _ := buf.Reader.Peek(n)
		if _, err := up.Write(pending); err != nil {
			conn.Close()
			up.Close()
			return
		}
	}
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if c, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(up, conn)
	go pipe(conn, up)
	<-done
	<-done
	conn.Close()
	up.Close()
}

// check returns why host:port is refused, or "" when it is permitted.
func (p *Proxy) check(host, port string) string {
	if !Ports[port] {
		return fmt.Sprintf("port %s is not 80 or 443", port)
	}
	if !p.Allow.Permits(host) {
		return fmt.Sprintf("host %s is not on the egress allow-list", host)
	}
	return ""
}

func (p *Proxy) refuse(w http.ResponseWriter, code int, r *http.Request, msg string) {
	p.logf("deny %s %s: %s", r.Method, r.Host, msg)
	http.Error(w, "casebox egress: "+msg, code)
}

func (p *Proxy) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if p.Dial != nil {
		return p.Dial(ctx, network, addr)
	}
	d := net.Dialer{Timeout: 30 * time.Second, Control: refuseLocal}
	return d.DialContext(ctx, network, addr)
}

// refuseLocal stops a permitted name that resolves to this host, a link-local address (such as a
// cloud metadata service) or a non-unicast address. Private ranges stay reachable: an internal
// package registry is a normal entry on the list.
func refuseLocal(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("the address %s is not reachable through the egress proxy", host)
	}
	return nil
}

func (p *Proxy) logf(format string, args ...any) {
	if p.Log != nil {
		p.Log.Printf(format, args...)
	}
}

// Main runs the proxy: -listen address, -allow comma-separated hosts. It prints Ready to stdout once
// it listens, and logs every decision to stderr.
func Main(args []string) int {
	fs := flag.NewFlagSet("egress-proxy", flag.ContinueOnError)
	listen := fs.String("listen", ":3128", "the address to listen on")
	allow := fs.String("allow", "", "the hosts to allow, comma-separated; *.domain allows every host below domain")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	a, err := ParseAllow(strings.Split(*allow, ","))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if a.Empty() {
		fmt.Fprintln(os.Stderr, "the allow-list is empty")
		return 2
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	logger := log.New(os.Stderr, "", log.LstdFlags)
	srv := &http.Server{
		Handler:           &Proxy{Allow: a, Log: logger},
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          logger,
	}
	fmt.Println(Ready)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
