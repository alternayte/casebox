package sandbox

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"
)

// counting starts sandboxes that do nothing, and counts how many are live.
type counting struct {
	next, live int
}

func (c *counting) Prepare(context.Context, EnvSpec) (ImageRef, error) { return ImageRef{}, nil }
func (c *counting) Start(context.Context, Ref, StartOptions) (Sandbox, error) {
	c.next++
	c.live++
	return Sandbox{ID: fmt.Sprint(c.next)}, nil
}
func (c *counting) Snapshot(context.Context, Sandbox) (SnapshotRef, error) { return SnapshotRef{}, nil }
func (c *counting) Exec(context.Context, Sandbox, Command) (ExecResult, error) {
	return ExecResult{}, nil
}
func (c *counting) CopyOut(context.Context, Sandbox, string) (io.ReadCloser, error) { return nil, nil }
func (c *counting) Destroy(context.Context, Sandbox) error {
	c.live--
	return nil
}

func TestADoubleDestroyFreesOneSlot(t *testing.T) {
	l := Limit(&counting{}, 2)
	ctx := context.Background()
	a, _ := l.Start(ctx, ImageRef{}, StartOptions{})
	if _, err := l.Start(ctx, ImageRef{}, StartOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = l.Destroy(ctx, a)
	_ = l.Destroy(ctx, a)

	if _, err := l.Start(ctx, ImageRef{}, StartOptions{}); err != nil {
		t.Fatal(err)
	}
	full, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := l.Start(full, ImageRef{}, StartOptions{}); err == nil {
		t.Fatal("a third sandbox started while two were live: a second Destroy of one sandbox freed a slot")
	}
}
