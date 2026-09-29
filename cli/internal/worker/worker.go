// Package worker runs `casebox worker`: it leases jobs from the server, heartbeats while it works,
// and posts each result. Workers call the server; the server never calls a worker.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/alternayte/casebox/cli/internal/api"
)

// Job is a leased job.
type Job struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	Payload     json.RawMessage `json:"payload"`
	Attempts    int             `json:"attempts"`
	MaxAttempts int             `json:"maxAttempts"`
}

// Handler runs one kind of job and returns its result.
type Handler func(ctx context.Context, job Job) (any, error)

// Permanent marks an error that retrying cannot fix.
type Permanent struct{ Err error }

func (p Permanent) Error() string { return p.Err.Error() }

// Worker leases jobs of the kinds it has handlers for.
type Worker struct {
	Client   *api.Client
	ID       string
	Version  string
	Handlers map[string]Handler
	Log      io.Writer
	// Scope limits the worker to one CI run's jobs: the inline worker of casebox ci.
	Scope string
	// MaxIdle caps the wait between leases when no job waits (default 30 seconds).
	MaxIdle time.Duration
}

// Run leases and runs jobs until ctx ends. When no job waits it backs off up to 30 seconds.
func (w *Worker) Run(ctx context.Context) error {
	kinds := make([]string, 0, len(w.Handlers))
	for k := range w.Handlers {
		kinds = append(kinds, k)
	}
	maxIdle := w.MaxIdle
	if maxIdle <= 0 {
		maxIdle = 30 * time.Second
	}
	lease := map[string]any{"workerId": w.ID, "version": w.Version, "kinds": kinds}
	if w.Scope != "" {
		lease["scope"] = w.Scope
	}
	idle := time.Second
	for ctx.Err() == nil {
		var job Job
		err := w.Client.Do(ctx, http.MethodPost, "/worker/v1/jobs/lease", lease, &job)
		switch {
		case err != nil:
			fmt.Fprintf(w.Log, "lease failed: %v\n", err)
		case job.ID == "":
			// 204: nothing to do
		default:
			idle = time.Second
			w.runOne(ctx, job)
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(idle):
		}
		if idle < maxIdle {
			idle = min(idle*2, maxIdle)
		}
	}
	return ctx.Err()
}

func (w *Worker) runOne(ctx context.Context, job Job) {
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-jobCtx.Done():
				return
			case <-ticker.C:
				if err := w.Client.Do(jobCtx, http.MethodPost, "/worker/v1/jobs/"+job.ID+"/heartbeat", map[string]string{"workerId": w.ID}, nil); api.StatusOf(err) == http.StatusConflict {
					fmt.Fprintf(w.Log, "job %s: the lease is lost; stopping it\n", job.ID)
					cancel()
					return
				}
			}
		}
	}()

	fmt.Fprintf(w.Log, "job %s (%s) started\n", job.ID, job.Kind)
	result, err := w.Handlers[job.Kind](jobCtx, job)
	if err != nil {
		var permanent Permanent
		retryable := !errors.As(err, &permanent)
		fmt.Fprintf(w.Log, "job %s failed: %v\n", job.ID, err)
		_ = w.Client.Do(ctx, http.MethodPost, "/worker/v1/jobs/"+job.ID+"/fail", map[string]any{"workerId": w.ID, "error": err.Error(), "retryable": retryable}, nil)
		return
	}
	if err := w.Client.Do(ctx, http.MethodPost, "/worker/v1/jobs/"+job.ID+"/complete", map[string]any{"workerId": w.ID, "result": result}, nil); err != nil {
		fmt.Fprintf(w.Log, "job %s: posting the result failed: %v\n", job.ID, err)
		return
	}
	fmt.Fprintf(w.Log, "job %s done\n", job.ID)
}
