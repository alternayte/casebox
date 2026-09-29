// Package upload sends spooled events to the server in batches. It acknowledges a batch only
// after the server stored it, so an interrupted upload resumes where it stopped.
package upload

import (
	"context"
	"errors"
	"fmt"
	"github.com/alternayte/casebox/cli/internal/cbx"
	"net/http"
	"strconv"

	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/spool"
)

// Limits of one request; the server accepts at most 2000 events per batch.
const (
	maxEvents = 500
	maxBytes  = 4 << 20
)

// Result counts what one run sent.
type Result struct {
	Batches  int
	Events   int
	Rejected int
}

// ErrCaptureOff means the server accepts nothing until an admin chooses a prompt mode.
var ErrCaptureOff error = &cbx.Error{Code: cbx.NoPromptMode, Err: errors.New("capture is off on the server until an admin chooses a prompt mode (casebox init)")}

// Run uploads until the spool has nothing pending, the context ends, or the server cannot take more.
func Run(ctx context.Context, client *api.Client, s *spool.Spool) (Result, error) {
	var res Result
	for {
		batches, err := s.Pending(ctx, 20, maxEvents, maxBytes)
		if err != nil {
			return res, err
		}
		if len(batches) == 0 {
			return res, nil
		}
		// The server's spool-backlog metric: what this machine has not uploaded yet.
		headers := map[string]string{}
		if st, err := s.Stats(ctx); err == nil {
			headers["X-Casebox-Spool-Backlog"] = strconv.FormatInt(st.Pending, 10)
		}
		for _, b := range batches {
			seqs := make([]int64, len(b.Events))
			for i, e := range b.Events {
				seqs[i] = e.Seq
			}
			err := client.DoWith(ctx, http.MethodPost, "/ingest/v1/sessions", headers, b, nil)
			switch status := api.StatusOf(err); {
			case err == nil:
				if err := s.Ack(ctx, b.Session.ID, seqs); err != nil {
					return res, err
				}
				res.Batches++
				res.Events += len(seqs)
			case status == http.StatusConflict:
				return res, ErrCaptureOff
			case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
				// The server refuses this batch for good; keep it out of the way and say why.
				if err := s.Reject(ctx, b.Session.ID, seqs, err.Error()); err != nil {
					return res, err
				}
				res.Rejected += len(seqs)
			default:
				return res, fmt.Errorf("upload stopped; it resumes on the next run: %w", err)
			}
		}
	}
}
