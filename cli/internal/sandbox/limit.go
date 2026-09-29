package sandbox

import (
	"context"
	"sync"
)

// Limited caps how many sandboxes of one provider run at once on this host (SDD section 8, cost
// control). Start waits for a free slot; Destroy frees it, once per sandbox.
type Limited struct {
	Provider
	slots chan struct{}
	mu    sync.Mutex
	live  map[string]bool
}

// Limit wraps p so at most n of its sandboxes run at once.
func Limit(p Provider, n int) *Limited {
	if n < 1 {
		n = 1
	}
	return &Limited{Provider: p, slots: make(chan struct{}, n), live: map[string]bool{}}
}

func (l *Limited) Start(ctx context.Context, from Ref, opts StartOptions) (Sandbox, error) {
	select {
	case l.slots <- struct{}{}:
	case <-ctx.Done():
		return Sandbox{}, ctx.Err()
	}
	sb, err := l.Provider.Start(ctx, from, opts)
	if err != nil {
		<-l.slots
		return sb, err
	}
	l.mu.Lock()
	l.live[sb.ID] = true
	l.mu.Unlock()
	return sb, nil
}

func (l *Limited) Destroy(ctx context.Context, sb Sandbox) error {
	err := l.Provider.Destroy(ctx, sb)
	if err == nil {
		l.mu.Lock()
		if l.live[sb.ID] {
			delete(l.live, sb.ID)
			<-l.slots
		}
		l.mu.Unlock()
	}
	return err
}
