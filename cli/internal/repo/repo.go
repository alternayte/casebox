// Package repo reads what capture needs from a git repository and its .casebox/casebox.yml.
package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ConfigPath is where a repository enrols in Casebox.
const ConfigPath = ".casebox/casebox.yml"

// Config is the part of casebox.yml the CLI reads. The server and later commands read the rest.
type Config struct {
	Version   int      `yaml:"version"`
	Server    string   `yaml:"server"`
	Workspace string   `yaml:"workspace"`
	Repos     []string `yaml:"repos"`
	WorkItems struct {
		Jira *struct {
			URL      string   `yaml:"url"`
			Projects []string `yaml:"projects"`
		} `yaml:"jira"`
		GitHubIssues bool `yaml:"github_issues"`
	} `yaml:"work_items"`
	Harness struct {
		Globs  []string `yaml:"globs"`
		Shared string   `yaml:"shared,omitempty"`
	} `yaml:"harness"`
	Capture struct {
		Redact []string `yaml:"redact"`
	} `yaml:"capture"`
	Environment *Recipe          `yaml:"environment"`
	Cases       Cases            `yaml:"cases"`
	Evaluation  Evaluation       `yaml:"evaluation"`
	Prices      map[string]Price `yaml:"prices"`
}

// ErrNotEnrolled means the directory is not inside a repository with .casebox/casebox.yml.
var ErrNotEnrolled = errors.New("this repository is not enrolled; run casebox init")

// Root returns the top directory of the git repository that contains dir.
func Root(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git repository", dir)
	}
	return filepath.FromSlash(out), nil
}

// LoadConfig reads .casebox/casebox.yml of the repository that contains dir.
func LoadConfig(ctx context.Context, dir string) (string, Config, error) {
	root, err := Root(ctx, dir)
	if err != nil {
		return "", Config{}, ErrNotEnrolled
	}
	data, err := os.ReadFile(filepath.Join(root, ConfigPath))
	if errors.Is(err, os.ErrNotExist) {
		return root, Config{}, ErrNotEnrolled
	}
	if err != nil {
		return root, Config{}, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return root, Config{}, fmt.Errorf("read %s: %w", ConfigPath, err)
	}
	return root, c, nil
}

// State is the repository's position at one moment.
type State struct {
	Repo   string // github.com/owner/name, from the origin remote
	Branch string
	Head   string
}

// Current reads the origin remote, the branch and HEAD. Missing parts stay empty.
func Current(ctx context.Context, root string) State {
	var s State
	if url, err := git(ctx, root, "remote", "get-url", "origin"); err == nil {
		s.Repo = NormalizeRemote(url)
	}
	s.Branch, _ = git(ctx, root, "rev-parse", "--abbrev-ref", "HEAD")
	s.Head, _ = git(ctx, root, "rev-parse", "HEAD")
	return s
}

var scpRemote = regexp.MustCompile(`^[\w.-]+@([\w.-]+):(.+)$`)

// NormalizeRemote turns an https, ssh or scp-style remote into host/owner/name, lower case.
func NormalizeRemote(url string) string {
	u := strings.TrimSpace(url)
	if m := scpRemote.FindStringSubmatch(u); m != nil {
		u = m[1] + "/" + m[2]
	}
	for _, p := range []string{"https://", "http://", "ssh://", "git://"} {
		u = strings.TrimPrefix(u, p)
	}
	if at := strings.Index(u, "@"); at >= 0 && at < strings.Index(u+"/", "/") {
		u = u[at+1:]
	}
	if host, rest, ok := strings.Cut(u, "/"); ok {
		if h, _, hasPort := strings.Cut(host, ":"); hasPort {
			host = h
		}
		u = host + "/" + rest
	}
	return strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git"))
}

// UserEmail is the developer's git user.email, which identifies them as the session's person.
func UserEmail(ctx context.Context, root string) (string, error) {
	email, err := git(ctx, root, "config", "user.email")
	if err != nil || email == "" {
		return "", errors.New("git user.email is not set; set it so Casebox can count distinct people without knowing who they are")
	}
	return email, nil
}

// AuthorNames lists the distinct author names in the last commits, for identity marking.
func AuthorNames(ctx context.Context, root string, commits int) []string {
	out, err := git(ctx, root, "log", fmt.Sprintf("-%d", commits), "--format=%an%n%cn")
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var names []string
	for _, n := range strings.Split(out, "\n") {
		n = strings.TrimSpace(n)
		if n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	return names
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}
