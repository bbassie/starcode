package agent

import (
	"testing"
	"time"
)

func TestLimitWindowElapsed(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	w := LimitWindow{Length: 5 * time.Hour, ResetsAt: now.Add(4 * time.Hour)}
	if got, ok := w.Elapsed(now); !ok || got != 0.2 {
		t.Fatalf("one hour into five: %v %v", got, ok)
	}
	w.ResetsAt = now.Add(-time.Minute)
	if got, _ := w.Elapsed(now); got != 1 {
		t.Fatalf("past the reset: %v", got)
	}
	if _, ok := (LimitWindow{ResetsAt: now}).Elapsed(now); ok {
		t.Fatal("no length, no pace")
	}
}
