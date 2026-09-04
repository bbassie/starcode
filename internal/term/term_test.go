package term

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func waitFor(t *testing.T, ch chan []byte, want string) []byte {
	t.Helper()
	var got []byte
	deadline := time.After(5 * time.Second)
	for {
		if strings.Contains(string(got), want) {
			return got
		}
		select {
		case chunk, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed before %q arrived; got %q", want, got)
			}
			got = append(got, chunk...)
		case <-deadline:
			t.Fatalf("timed out waiting for %q; got %q", want, got)
		}
	}
}

func TestSessionEchoAndReplay(t *testing.T) {
	m := NewManager(nil)
	m.shell = "sh"
	defer m.Shutdown()

	s, err := m.Session("t1", t.TempDir(), 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	_, ch, cancel := s.Subscribe()
	defer cancel()
	if err := s.Write([]byte("echo he\"\"llo\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, "hello")

	// A later subscriber sees the same output through the scrollback.
	replay, _, cancel2 := s.Subscribe()
	defer cancel2()
	if !bytes.Contains(replay, []byte("hello")) {
		t.Fatalf("replay missing output: %q", replay)
	}

	// Same id returns the same live session; a new id does not.
	again, err := m.Session("t1", t.TempDir(), 80, 24)
	if err != nil || again != s {
		t.Fatalf("expected the same session back, got %v %v", again, err)
	}

	if err := s.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	if err := s.Resize(0, 40); err == nil {
		t.Fatal("expected an error for a zero size")
	}
}

func TestSessionExit(t *testing.T) {
	m := NewManager(nil)
	m.shell = "sh"
	defer m.Shutdown()

	s, err := m.Session("t2", t.TempDir(), 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	_, ch, cancel := s.Subscribe()
	defer cancel()
	if err := s.Write([]byte("exit\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				if !s.Exited() {
					t.Fatal("channel closed but session not marked exited")
				}
				if m.Live("t2") != nil {
					t.Fatal("dead session still reported live")
				}
				// The next Session call starts a fresh shell.
				fresh, err := m.Session("t2", t.TempDir(), 80, 24)
				if err != nil {
					t.Fatal(err)
				}
				if fresh == s {
					t.Fatal("got the dead session back")
				}
				return
			}
		case <-deadline:
			t.Fatal("shell did not exit")
		}
	}
}
