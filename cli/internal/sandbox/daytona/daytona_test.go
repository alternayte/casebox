package daytona

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alternayte/casebox/cli/internal/sandbox"
	"github.com/alternayte/casebox/cli/internal/sandbox/conformance"
)

// TestConformance runs the shared suite against real Daytona. It needs DAYTONA_API_KEY (and
// DAYTONA_API_URL for a self-hosted Daytona); without it the contract tests below cover the
// provider against a fake of Daytona's API.
func TestConformance(t *testing.T) {
	p, err := FromEnv()
	if err != nil {
		t.Skip("DAYTONA_API_KEY is not set; the Daytona provider is covered by contract tests against a fake of Daytona's API only")
	}
	conformance.Run(t, p)
}

func TestPrepareBuildsTheSnapshotOnceAndUploadsTheContext(t *testing.T) {
	f, p := newFake(t)
	spec := sandbox.EnvSpec{
		Image:   "alpine:3.20",
		Install: []string{"cat deps.lock > /opt/installed", "echo \"a\nb\" > /opt/two-lines"},
		Context: map[string][]byte{"deps.lock": []byte("lock 1"), "src/app/packages.lock.json": []byte("{}")},
	}

	ref, err := p.Prepare(context.Background(), spec)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	want := sandbox.ImageRef{ID: "casebox-" + spec.Key()[:16], Key: spec.Key(), Workdir: "/workspace"}
	if !reflect.DeepEqual(ref, want) {
		t.Fatalf("Prepare returned %+v, want %+v", ref, want)
	}

	snap := f.snapshots[ref.ID]
	if snap == nil || snap.req.BuildInfo == nil {
		t.Fatalf("no snapshot was built from a Dockerfile: %+v", snap)
	}
	dockerfile := strings.Join([]string{
		"FROM alpine:3.20",
		"USER root",
		"RUN " + execForm("/bin/sh", "-c", createUser),
		`RUN ["mkdir","-p","/workspace","/results"]`,
		`COPY ["casebox-context/","/workspace/"]`,
		"WORKDIR /workspace",
		`RUN ["/bin/sh","-c","cat deps.lock > /opt/installed"]`,
		`RUN ["/bin/sh","-c","echo \"a\nb\" > /opt/two-lines"]`,
		`RUN ["chown","-R","10001:10001","/workspace","/results"]`,
		`ENTRYPOINT ["sleep","infinity"]`,
	}, "\n") + "\n"
	if got := snap.req.BuildInfo.DockerfileContent; got != dockerfile {
		t.Fatalf("the Dockerfile is\n%s\nwant\n%s", got, dockerfile)
	}

	if len(snap.req.BuildInfo.ContextHashes) != 1 {
		t.Fatalf("context hashes %v, want one", snap.req.BuildInfo.ContextHashes)
	}
	hash := snap.req.BuildInfo.ContextHashes[0]
	archive, ok := f.objects["builds/org1/"+hash+"/context.tar"]
	if !ok {
		t.Fatalf("the context was not uploaded to <bucket>/<org>/<hash>/context.tar; objects: %v", keysOf(f.objects))
	}
	if sum := md5.Sum(archive); hex.EncodeToString(sum[:]) != hash {
		t.Fatal("the context hash is not the MD5 of the uploaded archive")
	}
	files := untar(t, archive)
	if files["casebox-context/deps.lock"] != "lock 1" || files["casebox-context/src/app/packages.lock.json"] != "{}" {
		t.Fatalf("the context archive holds %v", files)
	}
	if !strings.HasPrefix(f.s3Headers.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AK/") || f.s3Headers.Get("X-Amz-Security-Token") != "ST" {
		t.Fatalf("the upload is not signed with the push credentials: %v", f.s3Headers)
	}

	again, err := p.Prepare(context.Background(), spec)
	if err != nil || again.ID != ref.ID {
		t.Fatalf("Prepare again: %v, %v", again, err)
	}
	if n := f.count("POST /snapshots"); n != 1 {
		t.Fatalf("an unchanged spec created %d snapshots", n)
	}

	changed := spec
	changed.Context = map[string][]byte{"deps.lock": []byte("lock 2")}
	other, err := p.Prepare(context.Background(), changed)
	if err != nil || other.ID == ref.ID {
		t.Fatalf("a changed lockfile gave %v, %v", other, err)
	}
}

func TestPrepareSurfacesBuildFailuresAndRebuildsAnOldFailure(t *testing.T) {
	t.Run("a build that fails now is an error with Daytona's reason", func(t *testing.T) {
		f, p := newFake(t)
		f.buildResult = func(string) (string, string) { return "build_failed", "RUN cat deps.lock: exit code 1" }
		_, err := p.Prepare(context.Background(), sandbox.EnvSpec{Image: "alpine:3.20", Install: []string{"false"}})
		if err == nil || !strings.Contains(err.Error(), "exit code 1") {
			t.Fatalf("Prepare returned %v, want the build error", err)
		}
	})
	t.Run("a snapshot left failed by an earlier attempt is deleted and built again", func(t *testing.T) {
		f, p := newFake(t)
		spec := sandbox.EnvSpec{Image: "alpine:3.20"}
		name := "casebox-" + spec.Key()[:16]
		f.snapshots[name] = &fakeSnapshot{dto: snapshotDTO{ID: "old", Name: name, State: "error", ErrorReason: "runner lost"}}
		if _, err := p.Prepare(context.Background(), spec); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if f.count("DELETE /snapshots/old") != 1 || f.count("POST /snapshots") != 1 {
			t.Fatalf("calls %v", f.calls)
		}
	})
}

func TestStartMapsOptionsAndStartsServicesAsLinkedSandboxes(t *testing.T) {
	f, p := newFake(t)
	ctx := context.Background()
	spec := sandbox.EnvSpec{Image: "alpine:3.20", Services: []sandbox.Service{{Name: "cache", Image: "registry.test/cache:7", Env: map[string]string{"MODE": "test"}}}}
	ref, err := p.Prepare(ctx, spec)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	var svcSnap *fakeSnapshot
	for name, s := range f.snapshots {
		if strings.HasPrefix(name, "casebox-svc-") {
			svcSnap = s
		}
	}
	if svcSnap == nil || svcSnap.req.ImageName != "registry.test/cache:7" || !reflect.DeepEqual(svcSnap.req.Entrypoint, []string{"docker-entrypoint.sh", "redis-server"}) {
		t.Fatalf("the service snapshot is %+v, want the image with its ENTRYPOINT and CMD", svcSnap)
	}

	sb, err := p.Start(ctx, ref, sandbox.StartOptions{Lifetime: 90 * time.Minute, Env: map[string]string{"FROM_START": "yes"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	reqs := f.created()
	if len(reqs) != 2 {
		t.Fatalf("created %d sandboxes, want the sandbox and its service", len(reqs))
	}
	main, svc := reqs[0], reqs[1]
	want := createSandbox{Snapshot: ref.ID, Env: map[string]string{"FROM_START": "yes"}, Labels: map[string]string{"casebox": "sandbox"}, NetworkBlockAll: true, AutoStopInterval: intp(90), AutoDeleteInterval: intp(90), TTLMinutes: intp(90)}
	if !reflect.DeepEqual(main, want) {
		t.Fatalf("the sandbox was created with %+v, want %+v", main, want)
	}
	if svc.LinkedSandbox != sb.ID || *svc.AutoDeleteInterval != 0 || *svc.AutoStopInterval != 0 || *svc.TTLMinutes != 90 || !svc.NetworkBlockAll || svc.Env["MODE"] != "test" || svc.Snapshot != svcSnap.dto.Name {
		t.Fatalf("the service was created with %+v", svc)
	}
	if got := f.sandboxes[sb.ID].dto; got.CPU != 2 || got.Memory != 4 {
		t.Fatalf("the sandbox has %v CPUs and %v GB, want the defaults 2 and 4", got.CPU, got.Memory)
	}
	if sb.Provider != Name || sb.Workdir != "/workspace" || sb.Meta[metaToolbox] != f.srv.URL+"/toolbox/"+sb.ID {
		t.Fatalf("the sandbox is %+v", sb)
	}
	cmds := f.commands(sb.ID)
	if len(cmds) != 1 || !strings.Contains(cmds[0].Command, "getent hosts 'sb-") || !strings.Contains(cmds[0].Command, `echo "$ip cache # casebox-service" >> /tmp/casebox-hosts`) {
		t.Fatalf("the service name was not mapped in /etc/hosts: %+v", cmds)
	}

	open, err := p.Start(ctx, sandbox.ImageRef{ID: ref.ID}, sandbox.StartOptions{Network: sandbox.NetworkOpen, CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("Start open: %v", err)
	}
	if f.sandboxes[open.ID].req.NetworkBlockAll || f.count("POST /sandbox/"+open.ID+"/resize") != 0 {
		t.Fatal("an open sandbox blocks the network, or a sandbox already large enough was resized")
	}
}

func TestExecRunsTheArgvAsTheSandboxUser(t *testing.T) {
	f, p := newFake(t)
	ctx := context.Background()
	sb := startPlain(t, p)

	var script string
	f.onExecute = func(_, command string, files map[string][]byte) (int, executeResponse) {
		base := scriptBase(command)
		script = string(files[base+".sh"])
		files[base+".out"] = []byte("out " + string(files[base+".in"]) + "\n")
		files[base+".err"] = []byte("err\n")
		return http.StatusOK, executeResponse{ExitCode: 3}
	}
	res, err := p.Exec(ctx, sb, sandbox.Command{Args: []string{"sh", "-c", "it's"}, Dir: "/tmp", Env: map[string]string{"GREETING": "hello world"}, Stdin: strings.NewReader("piped"), Timeout: 1500 * time.Millisecond})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 3 || string(res.Stdout) != "out piped\n" || string(res.Stderr) != "err\n" || res.TimedOut || res.Truncated {
		t.Fatalf("Exec returned %+v", res)
	}
	cmds := f.commands(sb.ID)
	run := cmds[0]
	base := scriptBase(run.Command)
	if run.Command != "su -p -s /bin/sh casebox -c 'sh "+base+".sh'" || run.Timeout != 2 {
		t.Fatalf("the command ran as %q with timeout %d", run.Command, run.Timeout)
	}
	wantScript := strings.Join([]string{
		"exec <'" + base + ".in' >'" + base + ".out' 2>'" + base + ".err'",
		"export HOME=/home/casebox USER=casebox LOGNAME=casebox",
		"export GREETING='hello world'",
		"cd '/tmp' || exit 1",
		`exec 'sh' '-c' 'it'\''s'`,
	}, "\n") + "\n"
	if script != wantScript {
		t.Fatalf("the script is\n%s\nwant\n%s", script, wantScript)
	}
	if last := cmds[len(cmds)-1]; !strings.HasPrefix(last.Command, "rm -f '"+base+".sh'") || len(f.files[sb.ID]) != 0 {
		t.Fatalf("the temporary files were not removed: %q, %v", last.Command, keysOf(f.files[sb.ID]))
	}

	res, err = p.Exec(ctx, sb, sandbox.Command{Args: []string{"id", "-u"}, Root: true})
	if err != nil || res.ExitCode != 3 {
		t.Fatalf("Exec as root: %+v, %v", res, err)
	}
	cmds = f.commands(sb.ID)
	rootRun := cmds[len(cmds)-2]
	if rootRun.Command != "sh "+scriptBase(rootRun.Command)+".sh" || rootRun.Timeout != 600 || strings.Contains(script, "HOME") {
		t.Fatalf("a root command ran as %q with timeout %d and script\n%s", rootRun.Command, rootRun.Timeout, script)
	}
	if !strings.HasPrefix(script, "exec <'/dev/null' ") {
		t.Fatalf("a command without stdin reads %q", strings.SplitN(script, "\n", 2)[0])
	}
}

func TestExecTimeoutAndOutputCap(t *testing.T) {
	f, p := newFake(t)
	sb := startPlain(t, p)

	f.onExecute = func(_, command string, files map[string][]byte) (int, executeResponse) {
		files[scriptBase(command)+".out"] = []byte("partial")
		return http.StatusRequestTimeout, executeResponse{}
	}
	res, err := p.Exec(context.Background(), sb, sandbox.Command{Args: []string{"sleep", "60"}, Timeout: 2 * time.Second})
	if err != nil || !res.TimedOut || res.ExitCode != -1 || string(res.Stdout) != "partial" {
		t.Fatalf("a timed-out command returned %+v, %v", res, err)
	}

	f.onExecute = func(_, command string, files map[string][]byte) (int, executeResponse) {
		files[scriptBase(command)+".out"] = bytes.Repeat([]byte("x"), sandbox.OutputCap+5)
		files[scriptBase(command)+".err"] = nil
		return http.StatusOK, executeResponse{}
	}
	res, err = p.Exec(context.Background(), sb, sandbox.Command{Args: []string{"yes"}})
	if err != nil || !res.Truncated || len(res.Stdout) != sandbox.OutputCap {
		t.Fatalf("an oversized output returned %d bytes, truncated %v, %v", len(res.Stdout), res.Truncated, err)
	}

	f.onExecute = func(_, command string, files map[string][]byte) (int, executeResponse) {
		return http.StatusOK, executeResponse{ExitCode: 127, Result: "su: not found"}
	}
	if _, err := p.Exec(context.Background(), sb, sandbox.Command{Args: []string{"true"}}); err == nil || !strings.Contains(err.Error(), "su: not found") {
		t.Fatalf("a command whose wrapper failed returned %v", err)
	}
}

func TestCopyOutTarsInTheSandboxAndStreamsTheArchive(t *testing.T) {
	f, p := newFake(t)
	sb := startPlain(t, p)
	var tarCmd string
	f.onExecute = func(_, command string, files map[string][]byte) (int, executeResponse) {
		tarCmd = command
		archive := strings.Trim(strings.Fields(command)[3], "'")
		files[archive] = []byte("TAR")
		return http.StatusOK, executeResponse{}
	}
	r, err := p.CopyOut(context.Background(), sb, "src")
	if err != nil {
		t.Fatalf("CopyOut: %v", err)
	}
	data, _ := io.ReadAll(r)
	if err := r.Close(); err != nil || string(data) != "TAR" {
		t.Fatalf("CopyOut streamed %q, close %v", data, err)
	}
	if !strings.HasPrefix(tarCmd, "tar -c -f '/tmp/casebox-") || !strings.HasSuffix(tarCmd, ".tar' -C '/workspace' 'src'") {
		t.Fatalf("the archive was made with %q", tarCmd)
	}
	if len(f.files[sb.ID]) != 0 {
		t.Fatalf("closing the stream left %v", keysOf(f.files[sb.ID]))
	}

	f.onExecute = func(string, string, map[string][]byte) (int, executeResponse) {
		return http.StatusOK, executeResponse{ExitCode: 2, Result: "tar: missing: No such file or directory"}
	}
	if _, err := p.CopyOut(context.Background(), sb, "/workspace/missing"); err == nil || !strings.Contains(err.Error(), "No such file") {
		t.Fatalf("CopyOut of a missing path returned %v", err)
	}
}

func TestSnapshot(t *testing.T) {
	t.Run("a running sandbox is snapshotted in place", func(t *testing.T) {
		f, p := newFake(t)
		sb := startPlain(t, p)
		snap, err := p.Snapshot(context.Background(), sb)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if !strings.HasPrefix(snap.ID, "casebox-snap-") || snap.Workdir != "/workspace" || f.snapshots[snap.ID].dto.State != "active" {
			t.Fatalf("Snapshot returned %+v", snap)
		}
		if f.count("POST /sandbox/"+sb.ID+"/stop") != 0 {
			t.Fatal("a snapshot Daytona takes while running stopped the sandbox")
		}
		if _, err := p.Start(context.Background(), snap, sandbox.StartOptions{}); err != nil {
			t.Fatalf("Start from the snapshot: %v", err)
		}
	})
	t.Run("when Daytona needs a stopped sandbox it is stopped and started again with its services", func(t *testing.T) {
		f, p := newFake(t)
		f.requireStop = true
		ctx := context.Background()
		services := []sandbox.Service{{Name: "cache", Image: "registry.test/cache:7"}}
		ref, err := p.Prepare(ctx, sandbox.EnvSpec{Image: "alpine:3.20", Services: services})
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		sb, err := p.Start(ctx, ref, sandbox.StartOptions{})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		snap, err := p.Snapshot(ctx, sb)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if !reflect.DeepEqual(snap.Services, services) {
			t.Fatalf("the snapshot carries services %+v", snap.Services)
		}
		if f.count("POST /sandbox/"+sb.ID+"/stop") != 1 || f.count("POST /sandbox/"+sb.ID+"/start") != 1 || f.count("POST /sandbox/"+sb.ID+"/snapshot") != 2 {
			t.Fatalf("calls %v", f.calls)
		}
		if f.sandboxes[sb.ID].dto.State != "started" {
			t.Fatalf("the sandbox is %s after its snapshot", f.sandboxes[sb.ID].dto.State)
		}
		if len(f.created()) != 2 || len(f.commands(sb.ID)) != 2 {
			t.Fatal("the running service was replaced, or its name was not mapped again after the restart")
		}
	})
}

func TestDestroyIsIdempotentAndLaterCallsAreNotFound(t *testing.T) {
	f, p := newFake(t)
	ctx := context.Background()
	ref, err := p.Prepare(ctx, sandbox.EnvSpec{Image: "alpine:3.20", Services: []sandbox.Service{{Name: "cache", Image: "registry.test/cache:7"}}})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	sb, err := p.Start(ctx, ref, sandbox.StartOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Destroy(ctx, sb); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(f.sandboxes) != 0 {
		t.Fatalf("sandboxes left: %v", keysOf(f.sandboxes))
	}
	if err := p.Destroy(ctx, sb); err != nil {
		t.Fatalf("a second Destroy: %v", err)
	}
	if _, err := p.Exec(ctx, sb, sandbox.Command{Args: []string{"true"}}); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("Exec after Destroy returned %v", err)
	}
	if _, err := p.CopyOut(ctx, sb, "/workspace"); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("CopyOut after Destroy returned %v", err)
	}
	if _, err := p.Snapshot(ctx, sb); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("Snapshot after Destroy returned %v", err)
	}
}

// TestSignV4 pins the signer to AWS's documented example (S3 API reference, "Signature
// Calculations for the Authorization Header", GET Object).
func TestSignV4(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	signV4(req, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1", time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization is\n%s\nwant\n%s", got, want)
	}
}

func TestParseImage(t *testing.T) {
	for image, want := range map[string]imageRef{
		"redis:7-alpine":                             {"registry-1.docker.io", "library/redis", "7-alpine"},
		"bitnami/postgresql:16":                      {"registry-1.docker.io", "bitnami/postgresql", "16"},
		"mcr.microsoft.com/mssql/server:2022-latest": {"mcr.microsoft.com", "mssql/server", "2022-latest"},
		"localhost:5000/cache@sha256:abc":            {"localhost:5000", "cache", "sha256:abc"},
		"ghcr.io/org/app":                            {"ghcr.io", "org/app", "latest"},
	} {
		got, err := parseImage(image)
		if err != nil || got != want {
			t.Errorf("parseImage(%q) = %+v, %v; want %+v", image, got, err, want)
		}
	}
}

func startPlain(t *testing.T, p *Provider) sandbox.Sandbox {
	t.Helper()
	ref, err := p.Prepare(context.Background(), sandbox.EnvSpec{Image: "alpine:3.20"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	sb, err := p.Start(context.Background(), ref, sandbox.StartOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return sb
}

// scriptBase returns /tmp/casebox-<id> from an Exec command.
func scriptBase(command string) string {
	i := strings.Index(command, "/tmp/casebox-")
	return command[i : i+len("/tmp/casebox-")+16]
}

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
			t.Fatal(err)
		}
		body, _ := io.ReadAll(r)
		out[h.Name] = string(body)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
