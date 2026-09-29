package hooks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/alternayte/casebox/cli/internal/config"
)

// A hook call only queues its payload and starts the processor, so it returns in a few
// milliseconds whatever git, the spool or the network do.
type queued struct {
	Agent   string          `json:"agent"`
	Event   string          `json:"event"`
	At      time.Time       `json:"at"`
	Payload json.RawMessage `json:"payload"`
}

// Enqueue records one hook call and starts the processor in the background.
func Enqueue(agent, event string, stdin io.Reader) error {
	if _, known := actions[agent][event]; !known || config.Paused() {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(stdin, 32<<20))
	if err != nil {
		return err
	}
	if !json.Valid(data) {
		return errors.New("the hook payload is not JSON")
	}
	dir, err := config.Path("queue")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	item, err := json.Marshal(queued{Agent: agent, Event: event, At: time.Now().UTC(), Payload: data})
	if err != nil {
		return err
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	// The name sorts by time, so the processor handles calls in the order they happened.
	name := fmt.Sprintf("%020d-%s.json", time.Now().UnixNano(), hex.EncodeToString(suffix))
	tmp := filepath.Join(dir, "."+name)
	if err := os.WriteFile(tmp, item, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	return Background("capture", "process")
}

// Process handles every queued hook call in order. One processor runs at a time; a second one
// exits at once, because the first drains whatever arrives while it runs.
func Process(ctx context.Context, logf func(error)) error {
	dir, err := config.Path("queue")
	if err != nil {
		return err
	}
	unlock, ok, err := lock(filepath.Join(dir, ".lock"))
	if err != nil || !ok {
		return err
	}
	defer unlock()
	for {
		names, err := pending(dir)
		if err != nil || len(names) == 0 {
			return err
		}
		for _, name := range names {
			path := filepath.Join(dir, name)
			data, err := os.ReadFile(path)
			if err == nil {
				var q queued
				if err = json.Unmarshal(data, &q); err == nil {
					err = handle(ctx, q.Agent, q.Event, q.At, q.Payload)
				}
			}
			if err != nil {
				logf(fmt.Errorf("%s: %w", name, err))
			}
			// A call that fails is logged and dropped: retrying cannot recover a moment that passed.
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
}

func pending(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// lock creates a lock file. A lock older than two minutes belongs to a processor that died.
func lock(path string) (func(), bool, error) {
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return func() { os.Remove(path) }, true, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, false, err
		}
		info, err := os.Stat(path)
		if err != nil || time.Since(info.ModTime()) < 2*time.Minute {
			return nil, false, nil
		}
		_ = os.Remove(path)
	}
	return nil, false, nil
}
