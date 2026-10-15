package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/Dudude-bit/ccswap/internal/api"
)

// heartbeat bounds how long a dead client holds its stream and how stale a change no one announced
// (a window resetting with time) can get.
var heartbeat = 15 * time.Second

// broadcast nudges every subscriber; a nudge nobody has read yet absorbs the next ones.
type broadcast struct {
	mu     sync.Mutex
	subs   map[chan struct{}]struct{}
	closed bool
}

func (b *broadcast) subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		close(ch)
		return ch, func() {}
	}
	if b.subs == nil {
		b.subs = map[chan struct{}]struct{}{}
	}
	b.subs[ch] = struct{}{}
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

func (b *broadcast) publish() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// close ends every subscription, so open streams return before the listener shuts down.
func (b *broadcast) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for ch := range b.subs {
		close(ch)
		delete(b.subs, ch)
	}
}

func snap(core Core) ([]byte, error) {
	ctx := context.Background()
	st, err := core.Status(ctx)
	if err != nil {
		return nil, err
	}
	accts, err := core.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	if accts == nil {
		accts = []api.Account{}
	}
	return json.Marshal(api.Snapshot{Status: st, Accounts: accts})
}

// eventStream answers GET /v1/events: server-sent "state" events, one at once, then one per change.
type eventStream struct{ core Core }

func (s strict) GetEvents(context.Context, api.GetEventsRequestObject) (api.GetEventsResponseObject, error) {
	return eventStream{s.c}, nil
}

// VisitGetEventsResponse starts the stream. Its writer runs after the handler returns, so it must not touch c.
func (e eventStream) VisitGetEventsResponse(c fiber.Ctx) error {
	core := e.core
	changed, cancel := core.Subscribe()
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-store")
	c.Set("X-Accel-Buffering", "no")
	return c.SendStreamWriter(func(w *bufio.Writer) {
		defer cancel()
		tick := time.NewTicker(heartbeat)
		defer tick.Stop()
		var last []byte
		for {
			b, err := snap(core)
			switch {
			case err != nil:
				slog.Error("events: snapshot", "err", err)
				return
			case !bytes.Equal(b, last):
				w.WriteString("event: state\ndata: ")
				w.Write(b)
				w.WriteString("\n\n")
				last = b
			default:
				w.WriteString(": ping\n\n")
			}
			if w.Flush() != nil {
				return
			}
			select {
			case _, ok := <-changed:
				if !ok {
					return
				}
			case <-tick.C:
			}
		}
	})
}
