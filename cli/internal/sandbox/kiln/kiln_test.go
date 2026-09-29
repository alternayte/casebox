package kiln

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	osexec "os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alternayte/casebox/cli/internal/sandbox"
	"github.com/alternayte/casebox/cli/internal/sandbox/conformance"
)

// TestConformance runs the shared suite against a real Kiln host.
func TestConformance(t *testing.T) {
	if os.Getenv("KILN_URL") == "" || os.Getenv("KILN_API_KEY") == "" {
		t.Skip("set KILN_URL and KILN_API_KEY to run the conformance suite against a Kiln host; the contract tests below cover the provider without one")
	}
	p, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	conformance.Run(t, p)
}

func TestFromEnvNeedsBothVariables(t *testing.T) {
	t.Setenv("KILN_URL", "https://kiln.example")
	t.Setenv("KILN_API_KEY", "")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv accepted a missing KILN_API_KEY")
	}
	t.Setenv("KILN_API_KEY", "key")
	if _, err := FromEnv(); err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
}

var spec = sandbox.EnvSpec{
	Image:   "alpine:3.20",
	Install: []string{"cat deps.lock > /opt/installed"},
	Context: map[string][]byte{"deps.lock": []byte("lock 'quoted'\n"), "sub/b.lock": {0, 1, 2}},
}

func TestPrepareBuildsTheTemplateAndPollsUntilReady(t *testing.T) {
	f, p := newFake(t)
	ref, err := p.Prepare(context.Background(), spec)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	name := "casebox-" + spec.Key()[:16]
	if ref.ID != name || ref.Key != spec.Key() || ref.Workdir != sandbox.DefaultWorkdir {
		t.Fatalf("Prepare returned %+v", ref)
	}
	if len(f.templatePosts) != 1 {
		t.Fatalf("Prepare posted %d templates, want 1", len(f.templatePosts))
	}
	req := f.templatePosts[0]
	if req.Name != name || req.Image != "alpine:3.20" || req.VCPUs != 2 || req.MemoryMB != 4096 || req.DiskMB <= 0 {
		t.Fatalf("template request %+v", req)
	}
	if len(*req.EgressAllow) != 0 {
		t.Fatalf("egress_allow is %v, want empty for no network", *req.EgressAllow)
	}
	setup := req.Setup
	if len(setup) != 6 {
		t.Fatalf("setup has %d steps, want user, directories, two context files, one install step and chown:\n%s", len(setup), strings.Join(setup, "\n---\n"))
	}
	if !strings.Contains(setup[0], "useradd -u 10001") || !strings.Contains(setup[0], "adduser -D -u 10001") {
		t.Fatalf("setup[0] does not create user 10001 on both Debian and Alpine: %s", setup[0])
	}
	if setup[1] != "mkdir -p '/workspace' /results /var/lib/casebox && chmod 0700 /var/lib/casebox" {
		t.Fatalf("setup[1] = %q", setup[1])
	}
	for i, want := range map[int]struct {
		path string
		body []byte
	}{2: {"/workspace/deps.lock", spec.Context["deps.lock"]}, 3: {"/workspace/sub/b.lock", spec.Context["sub/b.lock"]}} {
		m := regexp.MustCompile(`^mkdir -p '([^']*)' && printf '%s' '([A-Za-z0-9+/=]*)' \| base64 -d > '([^']*)'$`).FindStringSubmatch(setup[i])
		if m == nil {
			t.Fatalf("setup[%d] does not write a context file: %q", i, setup[i])
		}
		body, err := base64.StdEncoding.DecodeString(m[2])
		if err != nil {
			t.Fatal(err)
		}
		if m[3] != want.path || !bytes.Equal(body, want.body) {
			t.Fatalf("setup[%d] writes %q to %s, want %q to %s", i, body, m[3], want.body, want.path)
		}
	}
	if setup[4] != "cd '/workspace' || exit 1\ncat deps.lock > /opt/installed" {
		t.Fatalf("the install step is %q", setup[4])
	}
	if setup[5] != "chown -R 10001:10001 '/workspace' /results" {
		t.Fatalf("the last step is %q", setup[5])
	}

	again, err := p.Prepare(context.Background(), spec)
	if err != nil {
		t.Fatalf("Prepare again: %v", err)
	}
	if again.ID != ref.ID || again.Key != ref.Key || len(f.templatePosts) != 1 {
		t.Fatalf("an unchanged spec built again: %d posts", len(f.templatePosts))
	}
}

func TestPrepareWaitsForAnotherCallersBuild(t *testing.T) {
	f, p := newFake(t)
	f.buildPolls = 3
	name := TemplateName(spec.Key())
	f.templates[name] = &fakeTemplate{req: createTemplateRequest{Name: name, Image: spec.Image, EgressAllow: &[]string{}}, state: "building", polls: 3}
	if _, err := p.Prepare(context.Background(), spec); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(f.templatePosts) != 0 {
		t.Fatal("Prepare started a second build of a template already building")
	}
}

func TestPrepareSurfacesAFailedBuild(t *testing.T) {
	f, p := newFake(t)
	f.buildError = `setup[4] "cd '/workspace' || exit 1\ncat deps.lock > /opt/installed": exit 1 timed_out=false: no such file`
	_, err := p.Prepare(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "failed to build") || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("Prepare returned %v, want the build error", err)
	}
	if len(f.templatePosts) != 1 {
		t.Fatalf("a failed build was retried within one Prepare: %d posts", len(f.templatePosts))
	}
}

func TestPrepareRebuildsAnEarlierFailure(t *testing.T) {
	f, p := newFake(t)
	name := TemplateName(spec.Key())
	f.templates[name] = &fakeTemplate{req: createTemplateRequest{Name: name, Image: spec.Image, EgressAllow: &[]string{}}, state: "failed", err: "pull timed out"}
	if _, err := p.Prepare(context.Background(), spec); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(f.templatePosts) != 1 {
		t.Fatalf("Prepare posted %d builds, want one rebuild", len(f.templatePosts))
	}
}

func TestPrepareRefusesWhatKilnCannotBuild(t *testing.T) {
	_, p := newFake(t)
	withService := spec
	withService.Services = []sandbox.Service{{Name: "cache", Image: "redis:7-alpine"}}
	if _, err := p.Prepare(context.Background(), withService); err == nil || !strings.Contains(err.Error(), "services") {
		t.Fatalf("Prepare with a service returned %v", err)
	}
	escape := spec
	escape.Context = map[string][]byte{"../etc/passwd": []byte("x")}
	if _, err := p.Prepare(context.Background(), escape); err == nil {
		t.Fatal("Prepare wrote a context file outside the working directory")
	}
	big := spec
	big.Context = map[string][]byte{"package-lock.json": bytes.Repeat([]byte("x"), 1<<20)}
	if _, err := p.Prepare(context.Background(), big); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("Prepare with a context over Kiln's body limit returned %v", err)
	}
}

func prepared(t *testing.T, p *Provider) sandbox.ImageRef {
	t.Helper()
	ref, err := p.Prepare(context.Background(), spec)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return ref
}

func TestStartCreatesAnEphemeralSandboxWithItsLifetime(t *testing.T) {
	f, p := newFake(t)
	ref := prepared(t, p)
	sb, err := p.Start(context.Background(), ref, sandbox.StartOptions{Lifetime: 90*time.Minute + 500*time.Millisecond, Env: map[string]string{"A": "1"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if sb.Provider != "kiln" || sb.Workdir != sandbox.DefaultWorkdir || sb.ID == "" {
		t.Fatalf("Start returned %+v", sb)
	}
	req := f.sandboxPosts[0]
	if req.Template != ref.ID || req.Lifecycle != "ephemeral" || *req.TTLSeconds != 5401 || *req.IdleSeconds != 5401 {
		t.Fatalf("create request %+v", req)
	}
	if req.Env != nil || req.Secrets != nil {
		t.Fatal("Start sent env to Kiln, which makes the sandbox secret-bearing and its snapshots unrestorable")
	}

	if _, err := p.Start(context.Background(), ref, sandbox.StartOptions{Network: sandbox.NetworkOpen}); err == nil {
		t.Fatal("Start accepted network open, which Kiln cannot express")
	}
	if _, err := p.Start(context.Background(), ref, sandbox.StartOptions{CPUs: 8}); err == nil {
		t.Fatal("Start accepted a CPU count the template does not have")
	}
}

// running returns a started sandbox and clears the requests Prepare and Start made.
func running(t *testing.T, f *fakeKiln, p *Provider, env map[string]string) sandbox.Sandbox {
	t.Helper()
	sb, err := p.Start(context.Background(), prepared(t, p), sandbox.StartOptions{Env: env})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.execs = nil
	return sb
}

func TestExecRunsAsTheSandboxUserWithStdinDirEnvAndTimeout(t *testing.T) {
	f, p := newFake(t)
	sb := running(t, f, p, map[string]string{"FROM_START": "yes", "GREETING": "start"})
	var gotStdin []byte
	f.run = func(req execRequest, stdin []byte) ([]byte, []byte, execResponse) {
		gotStdin = stdin
		return []byte("out\x00\xff"), []byte("err\n"), execResponse{ExitCode: 3}
	}
	res, err := p.Exec(context.Background(), sb, sandbox.Command{
		Args:    []string{"sh", "-c", "echo 'it''s' \"$GREETING\""},
		Dir:     "/tmp/a b",
		Env:     map[string]string{"GREETING": "hello"},
		Stdin:   strings.NewReader("from stdin"),
		Timeout: 2500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 3 || string(res.Stdout) != "out\x00\xff" || string(res.Stderr) != "err\n" || res.Truncated || res.TimedOut {
		t.Fatalf("Exec returned %+v", res)
	}
	if string(gotStdin) != "from stdin" {
		t.Fatalf("the command read stdin %q", gotStdin)
	}
	if len(f.execs) != 2 {
		t.Fatalf("Exec made %d Kiln exec calls, want the command and the cleanup", len(f.execs))
	}
	run := f.execs[0]
	if run.TimeoutSeconds != 3 {
		t.Fatalf("timeout_seconds is %d, want 3", run.TimeoutSeconds)
	}
	if run.Cwd != "" {
		t.Fatalf("cwd is %q; the directory is entered as the sandbox user", run.Cwd)
	}
	want := map[string]string{"HOME": "/home/casebox", "USER": "casebox", "LOGNAME": "casebox", "FROM_START": "yes", "GREETING": "hello"}
	if len(run.Env) != len(want) {
		t.Fatalf("env is %v, want %v", run.Env, want)
	}
	for k, v := range want {
		if run.Env[k] != v {
			t.Fatalf("env is %v, want %v", run.Env, want)
		}
	}
	m := regexp.MustCompile(`^mkdir -p -m 0700 '(/var/lib/casebox/exec-[0-9a-f]{16})' \|\| exit 125\n`).FindStringSubmatch(run.Cmd[2])
	if len(run.Cmd) != 3 || run.Cmd[0] != "/bin/sh" || run.Cmd[1] != "-c" || m == nil {
		t.Fatalf("the command is %q", run.Cmd)
	}
	dir := m[1]
	wantScript := "mkdir -p -m 0700 '" + dir + "' || exit 125\n" +
		"{ exec su -m -s /bin/sh -c " + quote("cd "+quote("/tmp/a b")+" && exec "+quoteAll([]string{"sh", "-c", "echo 'it''s' \"$GREETING\""})) + " casebox; }" +
		" <'" + dir + "/stdin' >'" + dir + "/stdout' 2>'" + dir + "/stderr'"
	if run.Cmd[2] != wantScript {
		t.Fatalf("the script is\n%s\nwant\n%s", run.Cmd[2], wantScript)
	}
	if clean := f.execs[1]; len(clean.Cmd) != 3 || clean.Cmd[0] != "rm" || clean.Cmd[2] != dir {
		t.Fatalf("the cleanup is %q", clean.Cmd)
	}
	for name := range f.sandboxes[sb.ID].files {
		if strings.HasPrefix(name, dir) {
			t.Fatalf("Exec left %s behind", name)
		}
	}
}

func TestExecAsRootSkipsSuAndTheUserHome(t *testing.T) {
	f, p := newFake(t)
	sb := running(t, f, p, nil)
	f.run = func(execRequest, []byte) ([]byte, []byte, execResponse) {
		return []byte("0\n"), nil, execResponse{}
	}
	res, err := p.Exec(context.Background(), sb, sandbox.Command{Args: []string{"id", "-u"}, Root: true})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if string(res.Stdout) != "0\n" {
		t.Fatalf("stdout %q", res.Stdout)
	}
	run := f.execs[0]
	if strings.Contains(run.Cmd[2], "su ") || len(run.Env) != 0 || run.TimeoutSeconds != 600 {
		t.Fatalf("a root command ran as %q with env %v and timeout %d", run.Cmd[2], run.Env, run.TimeoutSeconds)
	}
	if !strings.Contains(run.Cmd[2], "\n{ cd '/workspace' && exec 'id' '-u'; } <'/dev/null' >'") {
		t.Fatalf("the root script is %q", run.Cmd[2])
	}
}

func TestExecReportsATimeout(t *testing.T) {
	f, p := newFake(t)
	sb := running(t, f, p, nil)
	f.run = func(execRequest, []byte) ([]byte, []byte, execResponse) {
		return nil, nil, execResponse{ExitCode: -1, TimedOut: true}
	}
	res, err := p.Exec(context.Background(), sb, sandbox.Command{Args: []string{"sleep", "60"}, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("Exec returned %+v, want a timeout", res)
	}
	if _, err := p.Exec(context.Background(), sb, sandbox.Command{Args: []string{"true"}, Timeout: 2 * time.Hour}); err == nil {
		t.Fatal("Exec accepted a timeout over Kiln's one hour limit")
	}
}

func TestExecCapsOutput(t *testing.T) {
	f, p := newFake(t)
	sb := running(t, f, p, nil)
	f.run = func(execRequest, []byte) ([]byte, []byte, execResponse) {
		return bytes.Repeat([]byte("x"), sandbox.OutputCap+10), []byte("small"), execResponse{}
	}
	res, err := p.Exec(context.Background(), sb, sandbox.Command{Args: []string{"yes"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if len(res.Stdout) != sandbox.OutputCap || !res.Truncated || string(res.Stderr) != "small" {
		t.Fatalf("stdout %d bytes, truncated %v, stderr %q", len(res.Stdout), res.Truncated, res.Stderr)
	}
}

func TestCopyOutStreamsATarAndRemovesIt(t *testing.T) {
	f, p := newFake(t)
	sb := running(t, f, p, nil)
	f.sandboxes[sb.ID].files["/workspace/src/main.go"] = []byte("package main\n")
	f.sandboxes[sb.ID].files["/workspace/README.md"] = []byte("hello\n")

	for path, want := range map[string]string{"/workspace/src": "src/main.go", "README.md": "README.md"} {
		r, err := p.CopyOut(context.Background(), sb, path)
		if err != nil {
			t.Fatalf("CopyOut %s: %v", path, err)
		}
		data, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		tr := tar.NewReader(bytes.NewReader(data))
		h, err := tr.Next()
		if err != nil || h.Name != want {
			t.Fatalf("CopyOut %s returned %v, %v; want %s", path, h, err, want)
		}
	}
	for name := range f.sandboxes[sb.ID].files {
		if strings.HasPrefix(name, stateDir) {
			t.Fatalf("CopyOut left %s behind", name)
		}
	}
	if _, err := p.CopyOut(context.Background(), sb, "/missing"); err == nil || !strings.Contains(err.Error(), "No such file") {
		t.Fatalf("CopyOut of a missing path returned %v", err)
	}
}

func TestSnapshotAndRestore(t *testing.T) {
	f, p := newFake(t)
	sb := running(t, f, p, map[string]string{"A": "1"})
	f.sandboxes[sb.ID].files["/workspace/state.txt"] = []byte("before")
	snap, err := p.Snapshot(context.Background(), sb)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if f.snapshotPosts[0].Stop {
		t.Fatal("Snapshot stopped the original sandbox")
	}
	if snap.ID == "" || snap.Workdir != sb.Workdir {
		t.Fatalf("Snapshot returned %+v", snap)
	}
	restored, err := p.Start(context.Background(), snap, sandbox.StartOptions{Env: map[string]string{"B": "2"}})
	if err != nil {
		t.Fatalf("Start from a snapshot: %v", err)
	}
	if got := f.restorePosts[0]; got.Count != 1 || got.AllowSecretFork {
		t.Fatalf("restore request %+v", got)
	}
	if restored.ID == sb.ID || restored.Workdir != sb.Workdir || string(f.sandboxes[restored.ID].files["/workspace/state.txt"]) != "before" {
		t.Fatalf("restored %+v", restored)
	}
	f.run = func(execRequest, []byte) ([]byte, []byte, execResponse) { return nil, nil, execResponse{} }
	f.execs = nil
	if _, err := p.Exec(context.Background(), restored, sandbox.Command{Args: []string{"true"}}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if env := f.execs[0].Env; env["B"] != "2" || env["A"] != "" {
		t.Fatalf("a restored sandbox's command has env %v, want its own start env", env)
	}
}

func TestDestroyIsIdempotentAndLaterCallsAreNotFound(t *testing.T) {
	f, p := newFake(t)
	sb := running(t, f, p, nil)
	f.run = func(execRequest, []byte) ([]byte, []byte, execResponse) { return nil, nil, execResponse{} }
	if err := p.Destroy(context.Background(), sb); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if err := p.Destroy(context.Background(), sb); err != nil {
		t.Fatalf("a second Destroy: %v", err)
	}
	if _, err := p.Exec(context.Background(), sb, sandbox.Command{Args: []string{"true"}}); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("Exec on a destroyed sandbox returned %v", err)
	}
	if _, err := p.Exec(context.Background(), sb, sandbox.Command{Args: []string{"cat"}, Stdin: strings.NewReader("x")}); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("Exec with stdin on a destroyed sandbox returned %v", err)
	}
	if _, err := p.CopyOut(context.Background(), sb, "/workspace"); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("CopyOut on a destroyed sandbox returned %v", err)
	}
	if _, err := p.Snapshot(context.Background(), sb); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("Snapshot of a destroyed sandbox returned %v", err)
	}

	unknown := sandbox.Sandbox{ID: "sb-never", Provider: "kiln", Workdir: sandbox.DefaultWorkdir}
	if err := p.Destroy(context.Background(), unknown); err != nil {
		t.Fatalf("Destroy of an unknown sandbox: %v", err)
	}
	if _, err := p.Exec(context.Background(), unknown, sandbox.Command{Args: []string{"true"}}); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("Exec on an unknown sandbox returned %v", err)
	}
}

func TestAWrapperFailureIsNotNotFound(t *testing.T) {
	f, p := newFake(t)
	sb := running(t, f, p, nil)
	// Exit 125 is the wrapper failing before its redirects, so no output file exists.
	f.run = func(execRequest, []byte) ([]byte, []byte, execResponse) {
		return nil, nil, execResponse{ExitCode: 125, Stderr: "mkdir: can't create directory: No space left on device\n"}
	}
	_, err := p.Exec(context.Background(), sb, sandbox.Command{Args: []string{"true"}})
	if err == nil || errors.Is(err, sandbox.ErrNotFound) || !strings.Contains(err.Error(), "No space left") {
		t.Fatalf("a wrapper failure returned %v", err)
	}
}

// TestQuotingSurvivesTheShell runs the argv quoting through a real sh.
func TestQuotingSurvivesTheShell(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh on this machine")
	}
	args := []string{"it's", `"double" $HOME \n`, "", "a b\tc", "'", "--"}
	out, err := osexec.Command("/bin/sh", "-c", "printf '%s\\n' "+quoteAll(args)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n"); strings.Join(got, "|") != strings.Join(args, "|") {
		t.Fatalf("sh saw %q, want %q", got, args)
	}
}

// TestTheRootWrapperRunsInSh runs the command wrapper, as root commands get it, through a real sh.
func TestTheRootWrapperRunsInSh(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh on this machine")
	}
	base := t.TempDir()
	files := base + "/state/exec-1"
	if err := os.MkdirAll(files, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files+"/stdin", []byte("from stdin"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := execScript(files, files+"/stdin", base, []string{"sh", "-c", `pwd; cat; echo " $GREETING"; echo err >&2; exit 3`}, true)
	cmd := osexec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "GREETING=hello")
	err := cmd.Run()
	var exit *osexec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("the wrapper returned %v, want exit 3", err)
	}
	stdout, _ := os.ReadFile(files + "/stdout")
	stderr, _ := os.ReadFile(files + "/stderr")
	if lines := strings.SplitN(string(stdout), "\n", 2); len(lines) != 2 || !strings.HasSuffix(lines[0], base[strings.LastIndex(base, "/"):]) || lines[1] != "from stdin hello\n" {
		t.Fatalf("stdout %q", stdout)
	}
	if string(stderr) != "err\n" {
		t.Fatalf("stderr %q", stderr)
	}
}
