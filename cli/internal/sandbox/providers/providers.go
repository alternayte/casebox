// Package providers picks the sandbox provider a machine uses: CASEBOX_SANDBOX names it, and Docker
// is the default for local trials (docs/specs/sandboxes.md).
package providers

import (
	"context"
	"fmt"
	"github.com/alternayte/casebox/cli/internal/cbx"
	"os"
	"strconv"

	"github.com/alternayte/casebox/cli/internal/sandbox"
	"github.com/alternayte/casebox/cli/internal/sandbox/daytona"
	"github.com/alternayte/casebox/cli/internal/sandbox/docker"
	"github.com/alternayte/casebox/cli/internal/sandbox/kiln"
)

// FromEnv returns the provider CASEBOX_SANDBOX names, with its name.
func FromEnv() (sandbox.Provider, string, error) {
	name := os.Getenv("CASEBOX_SANDBOX")
	if name == "" {
		name = docker.Name
	}
	switch name {
	case docker.Name:
		return docker.New(), name, nil
	case daytona.Name:
		p, err := daytona.FromEnv()
		if err != nil {
			return nil, "", err
		}
		return p, name, nil
	case kiln.Name:
		p, err := kiln.FromEnv()
		if err != nil {
			return nil, "", err
		}
		return p, name, nil
	default:
		return nil, "", fmt.Errorf("CASEBOX_SANDBOX is %q; use docker, kiln or daytona", name)
	}
}

// Available returns the provider when it answers, limited to CASEBOX_SANDBOX_CONCURRENCY
// sandboxes at once (default 2).
func Available(ctx context.Context) (sandbox.Provider, string, error) {
	p, name, err := FromEnv()
	if err != nil {
		return nil, "", err
	}
	switch name {
	case docker.Name:
		if err := p.(*docker.Provider).Check(ctx); err != nil {
			return nil, "", cbx.Wrap(cbx.NoSandbox, err)
		}
	}
	n, _ := strconv.Atoi(os.Getenv("CASEBOX_SANDBOX_CONCURRENCY"))
	if n <= 0 {
		n = 2
	}
	return sandbox.Limit(p, n), name, nil
}
