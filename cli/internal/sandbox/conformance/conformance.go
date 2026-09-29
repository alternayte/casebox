// Package conformance is the one suite every sandbox provider passes (docs/specs/sandboxes.md).
// A provider's test calls Run with a real backend; the suite builds, runs, snapshots and destroys
// sandboxes there and checks what the interface promises.
package conformance

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/alternayte/casebox/cli/internal/sandbox"
	"github.com/alternayte/casebox/cli/internal/sandbox/egressproxy"
)

// Image is the base image the suite builds on: small, with sh, tar, id and nc.
const Image = "alpine:3.20"

// ServiceImage is the service the suite starts beside a sandbox.
const ServiceImage = "redis:7-alpine"

// EgressAllowed is the host the egress subtest allows, and EgressAllowedURL a stable page on it.
const (
	EgressAllowed    = "proxy.golang.org"
	EgressAllowedURL = "https://proxy.golang.org/golang.org/x/text/@v/list"
)

// MirrorImage is the environment of the registry mirror subtest: Go, pinned by digest, with npm
// added by an install step.
var MirrorImage = sandbox.EnvSpec{Image: egressproxy.Builder, Install: []string{"apk add --no-cache npm"}}

// MirrorModule is a Go module, and MirrorNPM an npm package, that the mirror subtest fetches and
// then denies.
const (
	MirrorModule        = "golang.org/x/text"
	MirrorModuleVersion = "v0.14.0"
	MirrorNPM           = "left-pad"
	MirrorNPMVersion    = "1.3.0"
)

// Run checks one provider. Every sandbox it starts is destroyed before it returns.
func Run(t *testing.T, p sandbox.Provider) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	tag := time.Now().UTC().Format("20060102150405.000000000")
	spec := sandbox.EnvSpec{
		Image:   Image,
		Install: []string{"cat deps.lock > /opt/installed", "mkdir -p /opt/tools && echo tool > /opt/tools/version"},
		Context: map[string][]byte{"deps.lock": []byte("lock " + tag)},
	}

	image, err := p.Prepare(ctx, spec)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	t.Run("prepare is cached by lockfile hash", func(t *testing.T) {
		if image.Key != spec.Key() {
			t.Fatalf("the image key is %q, want the spec's %q", image.Key, spec.Key())
		}
		start := time.Now()
		again, err := p.Prepare(ctx, spec)
		if err != nil {
			t.Fatalf("Prepare again: %v", err)
		}
		if again.ID != image.ID {
			t.Fatalf("an unchanged spec built a new environment: %q, then %q", image.ID, again.ID)
		}
		if took := time.Since(start); took > 2*time.Minute {
			t.Fatalf("the cached Prepare took %s", took)
		}
		changed := spec
		changed.Context = map[string][]byte{"deps.lock": []byte("lock changed " + tag)}
		other, err := p.Prepare(ctx, changed)
		if err != nil {
			t.Fatalf("Prepare with a changed lockfile: %v", err)
		}
		if other.ID == image.ID || other.Key == image.Key {
			t.Fatal("a changed lockfile reused the old environment")
		}
		sb := start1(ctx, t, p, other, sandbox.StartOptions{})
		if out := run(ctx, t, p, sb, "cat", "/opt/installed"); strings.TrimSpace(out) != "lock changed "+tag {
			t.Fatalf("the install step read %q, want the changed lockfile", out)
		}
	})

	t.Run("exec", func(t *testing.T) {
		sb := start1(ctx, t, p, image, sandbox.StartOptions{Env: map[string]string{"FROM_START": "yes"}})

		res := exec(ctx, t, p, sb, sandbox.Command{Args: []string{"sh", "-c", "echo out; echo err >&2; exit 3"}})
		if res.ExitCode != 3 || string(res.Stdout) != "out\n" || string(res.Stderr) != "err\n" {
			t.Fatalf("got exit %d, stdout %q, stderr %q; want 3, \"out\\n\", \"err\\n\"", res.ExitCode, res.Stdout, res.Stderr)
		}
		if out := run(ctx, t, p, sb, "id", "-u"); strings.TrimSpace(out) != "10001" {
			t.Fatalf("commands run as uid %q, want 10001", strings.TrimSpace(out))
		}
		if res := exec(ctx, t, p, sb, sandbox.Command{Args: []string{"id", "-u"}, Root: true}); strings.TrimSpace(string(res.Stdout)) != "0" {
			t.Fatalf("a root command ran as uid %q", strings.TrimSpace(string(res.Stdout)))
		}
		if out := run(ctx, t, p, sb, "pwd"); strings.TrimSpace(out) != sandbox.DefaultWorkdir {
			t.Fatalf("the default directory is %q, want %s", strings.TrimSpace(out), sandbox.DefaultWorkdir)
		}
		if out := run(ctx, t, p, sb, "sh", "-c", "touch written && ls written"); strings.TrimSpace(out) != "written" {
			t.Fatal("the non-root user cannot write the working directory")
		}
		res = exec(ctx, t, p, sb, sandbox.Command{Args: []string{"sh", "-c", "pwd; echo $GREETING $FROM_START"}, Dir: "/tmp", Env: map[string]string{"GREETING": "hello"}})
		if string(res.Stdout) != "/tmp\nhello yes\n" {
			t.Fatalf("dir and env: got %q", res.Stdout)
		}
		res = exec(ctx, t, p, sb, sandbox.Command{Args: []string{"cat"}, Stdin: strings.NewReader("from stdin")})
		if string(res.Stdout) != "from stdin" {
			t.Fatalf("stdin: got %q", res.Stdout)
		}
		start := time.Now()
		res = exec(ctx, t, p, sb, sandbox.Command{Args: []string{"sleep", "60"}, Timeout: 2 * time.Second})
		if !res.TimedOut || time.Since(start) > 45*time.Second {
			t.Fatalf("a command past its timeout was not stopped: timed out %v after %s", res.TimedOut, time.Since(start))
		}
		if out := run(ctx, t, p, sb, "cat", "/opt/tools/version"); strings.TrimSpace(out) != "tool" {
			t.Fatalf("the install step's files are missing: %q", out)
		}
	})

	t.Run("files in and out", func(t *testing.T) {
		sb := start1(ctx, t, p, image, sandbox.StartOptions{})
		in := tarOf(t, map[string]string{"src/main.go": "package main\n", "README.md": "hello\n"})
		res := exec(ctx, t, p, sb, sandbox.Command{Args: []string{"tar", "-x", "-f", "-", "-C", sandbox.DefaultWorkdir}, Stdin: bytes.NewReader(in)})
		if res.ExitCode != 0 {
			t.Fatalf("tar -x failed: %s", res.Stderr)
		}
		files := untar(t, copyOut(ctx, t, p, sb, sandbox.DefaultWorkdir+"/src"))
		if files["main.go"] != "package main\n" && files["src/main.go"] != "package main\n" {
			t.Fatalf("CopyOut of a directory returned %v", keys(files))
		}
		files = untar(t, copyOut(ctx, t, p, sb, sandbox.DefaultWorkdir+"/README.md"))
		if len(files) != 1 || firstValue(files) != "hello\n" {
			t.Fatalf("CopyOut of a file returned %v", keys(files))
		}
	})

	t.Run("snapshot and restore", func(t *testing.T) {
		sb := start1(ctx, t, p, image, sandbox.StartOptions{})
		run(ctx, t, p, sb, "sh", "-c", "echo before > state.txt")
		snap, err := p.Snapshot(ctx, sb)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		run(ctx, t, p, sb, "sh", "-c", "echo after > state.txt")
		restored := start1(ctx, t, p, snap, sandbox.StartOptions{})
		if out := run(ctx, t, p, restored, "cat", "state.txt"); strings.TrimSpace(out) != "before" {
			t.Fatalf("the restored sandbox reads %q, want the state at the snapshot", out)
		}
		if out := run(ctx, t, p, sb, "cat", "state.txt"); strings.TrimSpace(out) != "after" {
			t.Fatalf("the original sandbox reads %q after the snapshot", out)
		}
		if out := run(ctx, t, p, restored, "id", "-u"); strings.TrimSpace(out) != "10001" {
			t.Fatal("a restored sandbox does not run as the sandbox user")
		}
	})

	t.Run("services and network", func(t *testing.T) {
		withService := spec
		withService.Services = []sandbox.Service{{Name: "cache", Image: ServiceImage}}
		ref, err := p.Prepare(ctx, withService)
		if err != nil {
			t.Fatalf("Prepare with a service: %v", err)
		}
		sb := start1(ctx, t, p, ref, sandbox.StartOptions{})
		reached := false
		for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
			if res := exec(ctx, t, p, sb, sandbox.Command{Args: []string{"sh", "-c", "printf 'PING\\r\\n' | nc -w 2 cache 6379"}, Timeout: 10 * time.Second}); strings.Contains(string(res.Stdout), "PONG") {
				reached = true
				break
			}
		}
		if !reached {
			t.Fatal("the sandbox cannot reach its service by name")
		}
		if res := exec(ctx, t, p, sb, sandbox.Command{Args: []string{"sh", "-c", "nc -w 3 1.1.1.1 443 </dev/null && echo reached"}, Timeout: 20 * time.Second}); strings.Contains(string(res.Stdout), "reached") {
			t.Fatal("a sandbox with network none reached the internet")
		}
	})

	// A provider that returns sandbox.ErrUnsupported for Egress skips this subtest; any other
	// error fails it.
	t.Run("egress allow-list", func(t *testing.T) {
		withService := spec
		withService.Services = []sandbox.Service{{Name: "cache", Image: ServiceImage}}
		ref, err := p.Prepare(ctx, withService)
		if err != nil {
			t.Fatalf("Prepare with a service: %v", err)
		}
		sb, err := p.Start(ctx, ref, sandbox.StartOptions{Egress: []string{EgressAllowed}})
		if errors.Is(err, sandbox.ErrUnsupported) {
			t.Skipf("the provider does not support egress allow-lists: %v", err)
		}
		if err != nil {
			t.Fatalf("Start with an egress allow-list: %v", err)
		}
		t.Cleanup(func() {
			if err := p.Destroy(context.Background(), sb); err != nil {
				t.Errorf("Destroy: %v", err)
			}
		})
		fetch := func(url string) sandbox.ExecResult {
			return exec(ctx, t, p, sb, sandbox.Command{Args: []string{"wget", "-q", "-O", "/dev/null", "-T", "20", url}, Timeout: 60 * time.Second})
		}
		var last sandbox.ExecResult
		for attempt := 0; attempt < 3; attempt++ {
			if last = fetch(EgressAllowedURL); last.ExitCode == 0 {
				break
			}
			time.Sleep(2 * time.Second)
		}
		if last.ExitCode != 0 {
			t.Fatalf("the allowed host %s is not reachable over HTTPS: exit %d: %s", EgressAllowed, last.ExitCode, last.Stderr)
		}
		for _, url := range []string{"https://github.com/", "http://github.com/", "https://" + EgressAllowed + ":8443/"} {
			if res := fetch(url); res.ExitCode == 0 {
				t.Fatalf("%s was reachable, but it is not on the allow-list", url)
			}
		}
		// wget sends GET https://… to the proxy; most tools send CONNECT, so check that too.
		connect := func(target string) string {
			script := `p=${HTTPS_PROXY#http://}; p=${p%/}; printf 'CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n' "$0" "$0" | nc -w 5 "${p%:*}" "${p##*:}" | head -n 1`
			return string(exec(ctx, t, p, sb, sandbox.Command{Args: []string{"sh", "-c", script, target}, Timeout: 30 * time.Second}).Stdout)
		}
		if line := connect(EgressAllowed + ":443"); !strings.Contains(line, " 200 ") {
			t.Fatalf("CONNECT to the allowed host answered %q, want 200", line)
		}
		if line := connect("github.com:443"); strings.Contains(line, " 200 ") {
			t.Fatalf("CONNECT to github.com answered %q", line)
		}
		if res := exec(ctx, t, p, sb, sandbox.Command{Args: []string{"sh", "-c", "nc -w 3 1.1.1.1 443 </dev/null && echo reached"}, Timeout: 20 * time.Second}); strings.Contains(string(res.Stdout), "reached") {
			t.Fatal("a raw connection to 1.1.1.1:443 went around the proxy")
		}
		if res := exec(ctx, t, p, sb, sandbox.Command{Args: []string{"nslookup", "github.com"}, Timeout: 20 * time.Second}); res.ExitCode == 0 {
			t.Fatalf("the sandbox resolved an outside name: %s", res.Stdout)
		}
		reached := false
		for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
			if res := exec(ctx, t, p, sb, sandbox.Command{Args: []string{"sh", "-c", "printf 'PING\\r\\n' | nc -w 2 cache 6379"}, Timeout: 10 * time.Second}); strings.Contains(string(res.Stdout), "PONG") {
				reached = true
				break
			}
		}
		if !reached {
			t.Fatal("a sandbox with an egress allow-list cannot reach its service by name")
		}
	})

	// A provider that returns sandbox.ErrUnsupported for Mirror skips this subtest.
	t.Run("registry mirror", func(t *testing.T) {
		probe, err := p.Start(ctx, image, sandbox.StartOptions{Egress: []string{EgressAllowed}, Mirror: &sandbox.Mirror{}})
		if errors.Is(err, sandbox.ErrUnsupported) {
			t.Skipf("the provider does not support a registry mirror: %v", err)
		}
		if err != nil {
			t.Fatalf("Start with a registry mirror: %v", err)
		}
		t.Cleanup(func() {
			if err := p.Destroy(context.Background(), probe); err != nil {
				t.Errorf("Destroy: %v", err)
			}
		})
		// The registry itself is out of reach, even though the allow-list names it.
		script := `p=${HTTPS_PROXY#http://}; p=${p%/}; printf 'CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n' "$0" "$0" | nc -w 5 "${p%:*}" "${p##*:}" | head -n 1`
		if line := string(exec(ctx, t, p, probe, sandbox.Command{Args: []string{"sh", "-c", script, EgressAllowed + ":443"}, Timeout: 30 * time.Second}).Stdout); strings.Contains(line, " 200 ") {
			t.Fatalf("CONNECT to the registry %s answered %q with a mirror in place", EgressAllowed, line)
		}
		for _, file := range []string{".nuget/NuGet/NuGet.Config", ".m2/settings.xml", ".gradle/init.d/casebox-mirror.gradle"} {
			if out := run(ctx, t, p, probe, "sh", "-c", `cat "$HOME/$0"`, file); !strings.Contains(out, "/nuget/v3/index.json") && !strings.Contains(out, "/maven/") {
				t.Fatalf("~/%s does not point at the mirror: %s", file, out)
			}
		}

		ref, err := p.Prepare(ctx, MirrorImage)
		if err != nil {
			t.Fatalf("Prepare the Go and npm image: %v", err)
		}
		goGet := []string{"sh", "-c", `cd "$(mktemp -d)" && GOMODCACHE="$(mktemp -d)" GOFLAGS=-modcacherw go mod download -json "$0"`, MirrorModule + "@" + MirrorModuleVersion}
		npmView := []string{"npm", "view", MirrorNPM + "@" + MirrorNPMVersion, "version"}
		attempt := func(sb sandbox.Sandbox, args []string) sandbox.ExecResult {
			var res sandbox.ExecResult
			for i := 0; i < 3; i++ {
				if res = exec(ctx, t, p, sb, sandbox.Command{Args: args, Timeout: 3 * time.Minute}); res.ExitCode == 0 {
					break
				}
				time.Sleep(2 * time.Second)
			}
			return res
		}

		open := start1(ctx, t, p, ref, sandbox.StartOptions{Mirror: &sandbox.Mirror{Denied: sandbox.Denied{Go: []string{"example.com/workspace"}}}})
		if res := attempt(open, goGet); res.ExitCode != 0 {
			t.Fatalf("go mod download through the mirror failed: exit %d: %s%s", res.ExitCode, res.Stdout, res.Stderr)
		}
		if res := attempt(open, npmView); res.ExitCode != 0 || strings.TrimSpace(string(res.Stdout)) != MirrorNPMVersion {
			t.Fatalf("npm view through the mirror: exit %d: %s%s", res.ExitCode, res.Stdout, res.Stderr)
		}

		denied := start1(ctx, t, p, ref, sandbox.StartOptions{Mirror: &sandbox.Mirror{Denied: sandbox.Denied{Go: []string{MirrorModule}, NPM: []string{MirrorNPM}}}})
		if res := exec(ctx, t, p, denied, sandbox.Command{Args: goGet, Timeout: 3 * time.Minute}); res.ExitCode == 0 || !strings.Contains(string(res.Stdout)+string(res.Stderr), "404") {
			t.Fatalf("go mod download of a denied module: exit %d: %s%s", res.ExitCode, res.Stdout, res.Stderr)
		}
		if res := exec(ctx, t, p, denied, sandbox.Command{Args: npmView, Timeout: 3 * time.Minute}); res.ExitCode == 0 || !strings.Contains(string(res.Stdout)+string(res.Stderr), "404") {
			t.Fatalf("npm view of a denied package: exit %d: %s%s", res.ExitCode, res.Stdout, res.Stderr)
		}
	})

	t.Run("destroy", func(t *testing.T) {
		sb, err := p.Start(ctx, image, sandbox.StartOptions{})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := p.Destroy(ctx, sb); err != nil {
			t.Fatalf("Destroy: %v", err)
		}
		if err := p.Destroy(ctx, sb); err != nil {
			t.Fatalf("a second Destroy failed: %v", err)
		}
		if _, err := p.Exec(ctx, sb, sandbox.Command{Args: []string{"true"}}); !errors.Is(err, sandbox.ErrNotFound) {
			t.Fatalf("Exec on a destroyed sandbox returned %v, want ErrNotFound", err)
		}
	})
}

func start1(ctx context.Context, t *testing.T, p sandbox.Provider, from sandbox.Ref, opts sandbox.StartOptions) sandbox.Sandbox {
	t.Helper()
	sb, err := p.Start(ctx, from, opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := p.Destroy(context.Background(), sb); err != nil {
			t.Errorf("Destroy: %v", err)
		}
	})
	return sb
}

func exec(ctx context.Context, t *testing.T, p sandbox.Provider, sb sandbox.Sandbox, cmd sandbox.Command) sandbox.ExecResult {
	t.Helper()
	res, err := p.Exec(ctx, sb, cmd)
	if err != nil {
		t.Fatalf("Exec %v: %v", cmd.Args, err)
	}
	return res
}

func run(ctx context.Context, t *testing.T, p sandbox.Provider, sb sandbox.Sandbox, args ...string) string {
	t.Helper()
	res := exec(ctx, t, p, sb, sandbox.Command{Args: args})
	if res.ExitCode != 0 {
		t.Fatalf("%v exited %d: %s", args, res.ExitCode, res.Stderr)
	}
	return string(res.Stdout)
}

func copyOut(ctx context.Context, t *testing.T, p sandbox.Provider, sb sandbox.Sandbox, path string) []byte {
	t.Helper()
	r, err := p.CopyOut(ctx, sb, path)
	if err != nil {
		t.Fatalf("CopyOut %s: %v", path, err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read CopyOut %s: %v", path, err)
	}
	return data
}

func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for name, body := range files {
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// untar returns the regular files of a tar stream, by name without a leading "./".
func untar(t *testing.T, data []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	r := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("CopyOut returned no valid tar stream: %v", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		out[strings.TrimPrefix(h.Name, "./")] = string(body)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func firstValue(m map[string]string) string {
	for _, v := range m {
		return v
	}
	return ""
}
