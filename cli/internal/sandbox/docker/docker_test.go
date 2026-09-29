package docker_test

import (
	"context"
	"testing"
	"time"

	"github.com/alternayte/casebox/cli/internal/sandbox/conformance"
	"github.com/alternayte/casebox/cli/internal/sandbox/docker"
)

func TestConformance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p := docker.New()
	if err := p.Check(ctx); err != nil {
		t.Skipf("the Docker provider is not tested here: %v", err)
	}
	conformance.Run(t, p)
}
