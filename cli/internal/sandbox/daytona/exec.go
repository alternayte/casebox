package daytona

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/sandbox"
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Exec runs one argv. Daytona's process API runs a shell command as the daemon's user (root)
// and returns stdout and stderr merged, so the provider uploads a script that redirects stdin,
// stdout and stderr to files, runs it through su as sandbox.User unless cmd.Root is set, and
// downloads the two output files. Daytona's timeout kills the command's process group and
// answers 408.
func (p *Provider) Exec(ctx context.Context, sb sandbox.Sandbox, cmd sandbox.Command) (sandbox.ExecResult, error) {
	if len(cmd.Args) == 0 {
		return sandbox.ExecResult{}, errors.New("exec needs a command")
	}
	for k := range cmd.Env {
		if !envName.MatchString(k) {
			return sandbox.ExecResult{}, fmt.Errorf("%q is not a valid environment variable name", k)
		}
	}
	dir := cmd.Dir
	if dir == "" {
		dir = sb.Workdir
	}
	if dir == "" {
		dir = sandbox.DefaultWorkdir
	}
	toolbox, err := p.toolbox(ctx, sb)
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	base := "/tmp/casebox-" + randomHex(8)
	defer p.cleanup(ctx, toolbox, base+".sh", base+".in", base+".out", base+".err")

	if err := p.c.upload(ctx, toolbox, base+".sh", strings.NewReader(execScript(cmd, dir, base))); err != nil {
		return sandbox.ExecResult{}, p.lost(ctx, sb, fmt.Errorf("upload the command: %w", err))
	}
	if cmd.Stdin != nil {
		if err := p.c.upload(ctx, toolbox, base+".in", cmd.Stdin); err != nil {
			return sandbox.ExecResult{}, p.lost(ctx, sb, fmt.Errorf("upload the command's stdin: %w", err))
		}
	}
	shell := "sh " + base + ".sh"
	if !cmd.Root {
		shell = "su -p -s /bin/sh " + userName + " -c " + quote(shell)
	}
	timeout := cmd.TimeoutOrDefault()
	started := time.Now()
	res, err := p.run(ctx, toolbox, shell, timeout)
	duration := time.Since(started)
	timedOut := statusOf(err) == http.StatusRequestTimeout
	if err != nil && !timedOut {
		return sandbox.ExecResult{}, p.lost(ctx, sb, err)
	}
	out := sandbox.ExecResult{ExitCode: res.ExitCode, Duration: duration, TimedOut: timedOut}
	if timedOut {
		out.ExitCode = -1
	} else if res.ExitCode < 0 {
		return sandbox.ExecResult{}, fmt.Errorf("daytona could not start the command: %s", strings.TrimSpace(res.Result))
	}

	var truncOut, truncErr bool
	out.Stdout, truncOut, err = p.readCapped(ctx, toolbox, base+".out")
	if statusOf(err) == http.StatusNotFound && !timedOut {
		return sandbox.ExecResult{}, fmt.Errorf("the command did not start (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Result))
	}
	if err != nil && statusOf(err) != http.StatusNotFound {
		return sandbox.ExecResult{}, p.lost(ctx, sb, fmt.Errorf("read the command's stdout: %w", err))
	}
	out.Stderr, truncErr, err = p.readCapped(ctx, toolbox, base+".err")
	if err != nil && statusOf(err) != http.StatusNotFound {
		return sandbox.ExecResult{}, p.lost(ctx, sb, fmt.Errorf("read the command's stderr: %w", err))
	}
	out.Truncated = truncOut || truncErr
	return out, nil
}

// execScript is the script Exec uploads. It runs in sh as the sandbox user or root.
func execScript(cmd sandbox.Command, dir, base string) string {
	stdin := "/dev/null"
	if cmd.Stdin != nil {
		stdin = base + ".in"
	}
	lines := []string{"exec <" + quote(stdin) + " >" + quote(base+".out") + " 2>" + quote(base+".err")}
	if !cmd.Root {
		lines = append(lines, "export HOME=/home/"+userName+" USER="+userName+" LOGNAME="+userName)
	}
	keys := make([]string, 0, len(cmd.Env))
	for k := range cmd.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		lines = append(lines, "export "+k+"="+quote(cmd.Env[k]))
	}
	args := make([]string, len(cmd.Args))
	for i, a := range cmd.Args {
		args[i] = quote(a)
	}
	lines = append(lines, "cd "+quote(dir)+" || exit 1", "exec "+strings.Join(args, " "))
	return strings.Join(lines, "\n") + "\n"
}

// quote makes s one sh word.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// run posts one shell command to the toolbox with a timeout in whole seconds. The HTTP call
// may outlast the timeout by a grace period, for the answer to come back.
func (p *Provider) run(ctx context.Context, toolbox, command string, timeout time.Duration) (executeResponse, error) {
	seconds := int(math.Ceil(timeout.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second+2*time.Minute)
	defer cancel()
	var res executeResponse
	err := p.c.do(ctx, http.MethodPost, toolbox+"/process/execute", executeRequest{Command: command, Timeout: seconds}, &res)
	return res, err
}

// execute runs a shell command as root in a sandbox, for the provider's own steps.
func (p *Provider) execute(ctx context.Context, sb sandbox.Sandbox, command string, seconds int) (executeResponse, error) {
	toolbox, err := p.toolbox(ctx, sb)
	if err != nil {
		return executeResponse{}, err
	}
	res, err := p.run(ctx, toolbox, command, time.Duration(seconds)*time.Second)
	if err != nil {
		return res, p.lost(ctx, sb, err)
	}
	return res, nil
}

// toolbox is the sandbox's toolbox base URL, from its Meta or from Daytona.
func (p *Provider) toolbox(ctx context.Context, sb sandbox.Sandbox) (string, error) {
	if t := sb.Meta[metaToolbox]; t != "" {
		return t, nil
	}
	s, err := p.getSandbox(ctx, sb.ID)
	if err != nil {
		return "", err
	}
	if s.State == "destroyed" || s.State == "destroying" {
		return "", fmt.Errorf("%w: the Daytona sandbox %s is %s", sandbox.ErrNotFound, sb.ID, s.State)
	}
	return toolboxBase(s), nil
}

// lost turns a failed toolbox call into sandbox.ErrNotFound when the sandbox is gone.
func (p *Provider) lost(ctx context.Context, sb sandbox.Sandbox, err error) error {
	s, getErr := p.getSandbox(ctx, sb.ID)
	if errors.Is(getErr, sandbox.ErrNotFound) {
		return getErr
	}
	if getErr == nil && (s.State == "destroyed" || s.State == "destroying") {
		return fmt.Errorf("%w: the Daytona sandbox %s is %s", sandbox.ErrNotFound, sb.ID, s.State)
	}
	return err
}

// readCapped downloads a file up to sandbox.OutputCap bytes and says whether it was longer.
func (p *Provider) readCapped(ctx context.Context, toolbox, file string) ([]byte, bool, error) {
	body, err := p.c.download(ctx, toolbox, file)
	if err != nil {
		return nil, false, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, sandbox.OutputCap+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > sandbox.OutputCap {
		return data[:sandbox.OutputCap], true, nil
	}
	return data, false, nil
}

// cleanup removes the provider's temporary files, even when ctx has ended.
func (p *Provider) cleanup(ctx context.Context, toolbox string, files ...string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	quoted := make([]string, len(files))
	for i, f := range files {
		quoted[i] = quote(f)
	}
	_, _ = p.run(ctx, toolbox, "rm -f "+strings.Join(quoted, " "), 20*time.Second)
}

// CopyOut tars a file or a directory in the sandbox to a temporary file and streams it back.
// Closing the stream removes the temporary file.
func (p *Provider) CopyOut(ctx context.Context, sb sandbox.Sandbox, file string) (io.ReadCloser, error) {
	if file == "" {
		return nil, errors.New("copy out needs a path")
	}
	if !path.IsAbs(file) {
		workdir := sb.Workdir
		if workdir == "" {
			workdir = sandbox.DefaultWorkdir
		}
		file = path.Join(workdir, file)
	}
	file = path.Clean(file)
	dir, name := path.Dir(file), path.Base(file)
	if file == "/" {
		dir, name = "/", "."
	}
	toolbox, err := p.toolbox(ctx, sb)
	if err != nil {
		return nil, err
	}
	archive := "/tmp/casebox-" + randomHex(8) + ".tar"
	res, err := p.run(ctx, toolbox, "tar -c -f "+quote(archive)+" -C "+quote(dir)+" "+quote(name), 10*time.Minute)
	if err != nil {
		p.cleanup(ctx, toolbox, archive)
		return nil, p.lost(ctx, sb, fmt.Errorf("tar %s: %w", file, err))
	}
	if res.ExitCode != 0 {
		p.cleanup(ctx, toolbox, archive)
		return nil, fmt.Errorf("tar %s exited %d: %s", file, res.ExitCode, strings.TrimSpace(res.Result))
	}
	body, err := p.c.download(ctx, toolbox, archive)
	if err != nil {
		p.cleanup(ctx, toolbox, archive)
		return nil, p.lost(ctx, sb, fmt.Errorf("download %s: %w", file, err))
	}
	return &removeOnClose{ReadCloser: body, remove: func() { p.cleanup(ctx, toolbox, archive) }}, nil
}

type removeOnClose struct {
	io.ReadCloser
	remove func()
	done   bool
}

func (r *removeOnClose) Close() error {
	err := r.ReadCloser.Close()
	if !r.done {
		r.done = true
		r.remove()
	}
	return err
}
