package docker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/sandbox"
	"github.com/alternayte/casebox/cli/internal/sandbox/egressproxy"
)

// ProxyHost is the name the egress proxy answers to on a sandbox's network.
const ProxyHost = "casebox-egress"

// startProxy runs the egress proxy of sandbox id: a container on its own open network, joined to
// the sandbox's internal network as ProxyHost. The sandbox has no route to the open network; the
// proxy is its only way out. With a mirror, the proxy also serves the registry mirror and refuses
// the registry hosts themselves. It returns the environment that points commands at the proxy and
// the mirror.
func (p *Provider) startProxy(ctx context.Context, id string, labels []string, hosts []string, mirror *sandbox.Mirror, services []sandbox.Service) (map[string]string, error) {
	image, err := p.ensureProxyImage(ctx)
	if err != nil {
		return nil, err
	}
	outside := id + "-egress"
	if _, err := p.docker(ctx, nil, append(append([]string{"network", "create"}, labels...), outside)...); err != nil {
		return nil, err
	}
	name := id + "-" + ProxyHost
	args := append([]string{
		"create", "--name", name,
		"--network", outside,
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", "256",
		"--memory", "256m",
	}, labels...)
	args = append(args, image, "-listen", ":"+egressproxy.Port, "-allow", strings.Join(hosts, ","))
	if mirror != nil {
		args = append(args, mirrorArgs(*mirror)...)
	}
	if _, err := p.docker(ctx, nil, args...); err != nil {
		return nil, fmt.Errorf("egress proxy: %w", err)
	}
	if _, err := p.docker(ctx, nil, "network", "connect", "--alias", ProxyHost, id, name); err != nil {
		return nil, fmt.Errorf("egress proxy: %w", err)
	}
	if _, err := p.docker(ctx, nil, "start", name); err != nil {
		return nil, fmt.Errorf("egress proxy: %w", err)
	}
	if err := p.waitReady(ctx, name); err != nil {
		return nil, err
	}
	url := "http://" + ProxyHost + ":" + egressproxy.Port
	noProxy := []string{"localhost", "127.0.0.1", "::1"}
	if mirror != nil {
		noProxy = append(noProxy, ProxyHost)
	}
	for _, svc := range services {
		noProxy = append(noProxy, svc.Name)
	}
	env := map[string]string{
		"HTTPS_PROXY": url, "https_proxy": url,
		"HTTP_PROXY": url, "http_proxy": url,
		"NO_PROXY": strings.Join(noProxy, ","), "no_proxy": strings.Join(noProxy, ","),
	}
	if mirror != nil {
		for k, v := range MirrorEnv() {
			env[k] = v
		}
	}
	return env, nil
}

// waitReady waits until the proxy prints egressproxy.Ready, or fails with its output when it
// stops first.
func (p *Provider) waitReady(ctx context.Context, name string) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		cmd := exec.CommandContext(ctx, p.bin(), "logs", name)
		out, _ := cmd.CombinedOutput()
		if strings.Contains(string(out), egressproxy.Ready) {
			return nil
		}
		running, err := p.docker(ctx, nil, "container", "inspect", "--format", "{{.State.Running}}", name)
		if err != nil {
			return fmt.Errorf("egress proxy: %w", err)
		}
		if strings.TrimSpace(string(running)) != "true" {
			return fmt.Errorf("the egress proxy stopped: %s", lastLines(string(out), 20))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the egress proxy did not start in 30s: %s", lastLines(string(out), 20))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// ensureProxyImage builds the proxy image from the source this binary embeds, once per machine
// and proxy version.
func (p *Provider) ensureProxyImage(ctx context.Context) (string, error) {
	tag := egressproxy.ImageTag()
	found, err := p.imageExists(ctx, tag)
	if err != nil || found {
		return tag, err
	}
	dir, err := os.MkdirTemp("", "casebox-egress-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	for name, body := range egressproxy.BuildContext() {
		target := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(target, body, 0o644); err != nil {
			return "", err
		}
	}
	cmd := exec.CommandContext(ctx, p.bin(), "build", "--tag", tag, dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("build the egress proxy image %s: %v: %s", tag, err, lastLines(string(out), 40))
	}
	return tag, nil
}

// checkEgress refuses an allow-list or a mirror the proxy cannot honour, before anything starts.
func checkEgress(opts sandbox.StartOptions, services []sandbox.Service) error {
	if len(opts.Egress) == 0 && opts.Mirror == nil {
		return nil
	}
	if opts.Network != sandbox.NetworkNone {
		return fmt.Errorf("an egress allow-list or a registry mirror needs network none, not %q", opts.Network)
	}
	if len(opts.Egress) > 0 {
		allow, err := egressproxy.ParseAllow(opts.Egress)
		if err != nil {
			return err
		}
		if allow.Empty() {
			return fmt.Errorf("the egress allow-list %s names no host", strconv.Quote(strings.Join(opts.Egress, ",")))
		}
	}
	if opts.Mirror != nil {
		if _, err := egressproxy.NewMirror(MirrorURL, egressDeny(*opts.Mirror)); err != nil {
			return err
		}
	}
	for _, svc := range services {
		if svc.Name == ProxyHost {
			return fmt.Errorf("the service name %q is taken by the egress proxy", ProxyHost)
		}
	}
	return nil
}
