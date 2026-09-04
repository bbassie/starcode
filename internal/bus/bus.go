// Package bus is an in-process publish/subscribe fan-out. Every subscriber
// gets every message. A subscriber that falls behind gets a Resync marker
// instead of blocking the publisher, and is expected to re-read state from
// the store.
package bus

import (
	"context"
	"sync"
)

// Resync is delivered to a subscriber whose buffer overflowed. Consumers
// should treat it as "state unknown, reload from the store".
type Resync struct{}

type subscriber struct {
	ch      chan any
	done    chan struct{}
	dropped bool
	wg      sync.WaitGroup // pending resync senders
}

type Bus struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
	size int
}

// New creates a bus whose subscriber channels hold size messages.
func New(size int) *Bus {
	if size < 1 {
		size = 64
	}
	return &Bus{subs: map[*subscriber]struct{}{}, size: size}
}

// Subscribe returns a channel that receives every message published after
// the call. The subscription ends when ctx is done; the channel is closed.
func (b *Bus) Subscribe(ctx context.Context) <-chan any {
	s := &subscriber{ch: make(chan any, b.size), done: make(chan struct{})}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	go func() {
		<-ctx.Done()
		b.mu.Lock()
		delete(b.subs, s)
		b.mu.Unlock()
		close(s.done)
		s.wg.Wait() // no resync sender may touch ch after this
		close(s.ch)
	}()
	return s.ch
}

// Publish delivers msgs to every subscriber without blocking.
func (b *Bus) Publish(msgs ...any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		for _, m := range msgs {
			if s.dropped {
				// Already scheduled a Resync for this subscriber; anything
				// published until it catches up is covered by the reload.
				continue
			}
			select {
			case s.ch <- m:
			default:
				s.dropped = true
				// The buffer is full, so the marker cannot be pushed now.
				// Deliver it once space frees up.
				s.wg.Add(1)
				go func(s *subscriber) {
					defer s.wg.Done()
					select {
					case s.ch <- Resync{}:
						b.mu.Lock()
						s.dropped = false
						b.mu.Unlock()
					case <-s.done:
					}
				}(s)
			}
		}
	}
}

// Subscribers returns the current subscriber count (for diagnostics).
func (b *Bus) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
