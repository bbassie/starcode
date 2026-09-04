package bus

import (
	"context"
	"testing"
	"time"
)

func TestFanOutAndResync(t *testing.T) {
	b := New(2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := b.Subscribe(ctx)
	c := b.Subscribe(ctx)

	b.Publish(1, 2)
	if got := []any{<-a, <-a}; got[0] != 1 || got[1] != 2 {
		t.Fatalf("a got %v", got)
	}
	if got := []any{<-c, <-c}; got[0] != 1 || got[1] != 2 {
		t.Fatalf("c got %v", got)
	}

	// Overflow: buffer of 2, publish 5 without reading. The subscriber gets
	// the first two, then a Resync marker once there is room.
	b.Publish(1, 2, 3, 4, 5)
	<-a
	<-a
	select {
	case m := <-a:
		if _, ok := m.(Resync); !ok {
			t.Fatalf("expected Resync, got %v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("no resync delivered")
	}
	// After a resync, publishing works again.
	b.Publish(9)
	select {
	case m := <-a:
		if m != 9 {
			t.Fatalf("got %v want 9", m)
		}
	case <-time.After(time.Second):
		t.Fatal("no message after resync")
	}

	cancel()
	time.Sleep(10 * time.Millisecond)
	if b.Subscribers() != 0 {
		t.Fatalf("subscribers = %d", b.Subscribers())
	}
}
