package docker_test

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/alternayte/casebox/cli/internal/sandbox/conformance"
	"github.com/alternayte/casebox/cli/internal/sandbox/docker"
)

func TestConformance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Skipf("no Docker daemon, so the Docker provider is not tested: docker info: %v: %s", err, out)
	}
	conformance.Run(t, docker.New())
}
