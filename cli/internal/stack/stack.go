// Package stack runs the local trial stack: the server, QueueBox and Postgres in Docker, from
// the compose files embedded in the binary.
package stack

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/config"
)

//go:embed assets/compose.yaml assets/queuebox.yml
var assets embed.FS

// Project is the compose project name: casebox for the default home, and a name of its own for a
// home that CASEBOX_HOME points elsewhere, so a scratch home never reuses the real stack's volumes.
func Project() string {
	home := os.Getenv("CASEBOX_HOME")
	if home == "" {
		return "casebox"
	}
	sum := sha256.Sum256([]byte(home))
	return "casebox-" + hex.EncodeToString(sum[:])[:8]
}

// Options configure casebox up.
type Options struct {
	Port    int
	Image   string // overrides the server image; empty uses the CLI's version
	Version string
	// Demo loads a synthetic team into the organisation when it has no session yet.
	Demo bool
}

// ImageTag is the server image tag that matches a CLI version. A release CLI (0.1.0, or a
// pre-release such as 0.1.0-trial.1) runs the image of its own version; a development build runs
// edge, the image of the newest commit on main.
func ImageTag(version string) string {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if v == "" || strings.HasSuffix(v, "-dev") {
		return "edge"
	}
	return v
}

// Env holds the stack's settings and generated secrets, kept in ~/.casebox/stack/.env so a
// restart keeps the same passwords.
type Env map[string]string

// Up writes the compose files, fills in missing secrets and starts the stack, waiting until
// every service is healthy.
func Up(ctx context.Context, opts Options, out io.Writer) (Env, error) {
	dir, err := prepare()
	if err != nil {
		return nil, err
	}
	env, err := loadEnv(filepath.Join(dir, ".env"))
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"CASEBOX_DB_PASSWORD", "CASEBOX_ADMIN_PASSWORD", "CASEBOX_EFFECTS_TOKEN", "CASEBOX_POLL_TOKEN", "CASEBOX_QUEUEBOX_ADMIN_TOKEN"} {
		if env[key] == "" {
			env[key] = secret()
		}
	}
	env["CASEBOX_PORT"] = fmt.Sprint(opts.Port)
	env["CASEBOX_VERSION"] = ImageTag(opts.Version)
	env["CASEBOX_DEMO"] = fmt.Sprint(opts.Demo)
	if opts.Image != "" {
		env["CASEBOX_SERVER_IMAGE"] = opts.Image
	} else {
		delete(env, "CASEBOX_SERVER_IMAGE")
	}
	if err := saveEnv(filepath.Join(dir, ".env"), env); err != nil {
		return nil, err
	}
	if err := compose(ctx, dir, out, "up", "--detach", "--wait", "--no-build", "--pull", "missing"); err != nil {
		return nil, err
	}
	return env, waitForServer(ctx, fmt.Sprintf("http://localhost:%d/api/v1/auth/methods", opts.Port))
}

// waitForServer polls the API: compose reports the server as up once its process runs, before it
// applies migrations and listens.
func waitForServer(ctx context.Context, url string) error {
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the server did not answer within 3 minutes; docker compose -p %s logs server shows why", Project())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// Down stops the stack. With volumes, it also deletes the database.
func Down(ctx context.Context, volumes bool, out io.Writer) error {
	dir, err := prepare()
	if err != nil {
		return err
	}
	args := []string{"down"}
	if volumes {
		args = append(args, "--volumes")
	}
	return compose(ctx, dir, out, args...)
}

func prepare() (string, error) {
	dir, err := config.Path("stack")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	for _, name := range []string{"compose.yaml", "queuebox.yml"} {
		data, err := assets.ReadFile("assets/" + name)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return "", fmt.Errorf("write %s: %w", name, err)
		}
	}
	return dir, nil
}

func compose(ctx context.Context, dir string, out io.Writer, args ...string) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("docker is not installed or not on PATH; casebox up needs Docker with the compose plugin")
	}
	base := []string{"compose", "--project-name", Project(), "--env-file", filepath.Join(dir, ".env"), "--file", filepath.Join(dir, "compose.yaml")}
	cmd := exec.CommandContext(ctx, "docker", append(base, args...)...)
	cmd.Dir = dir
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker compose %s: %w", args[0], err)
	}
	return nil
}

func secret() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func loadEnv(path string) (Env, error) {
	env := Env{}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return env, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			env[key] = value
		}
	}
	return env, scanner.Err()
}

func saveEnv(path string, env Env) error {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# Written by casebox up. The secrets stay the same across restarts.\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, env[k])
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}
