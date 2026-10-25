package web

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"starcode/internal/push"
	"starcode/internal/store"
)

// pushServer is agentServer with Web Push to a stand-in push service,
// which counts what arrives.
func pushServer(t *testing.T) (*Server, string, *atomic.Int32) {
	t.Helper()
	s, id := agentServer(t)
	var got atomic.Int32
	svc := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(svc.Close)
	ps, err := push.Load(filepath.Join(t.TempDir(), "vapid.pem"))
	if err != nil {
		t.Fatal(err)
	}
	ps.Client = svc.Client()
	s.Push = ps
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	enc := base64.RawURLEncoding.EncodeToString
	if err := s.App.Store.SavePushSub(context.Background(), store.PushSub{Endpoint: svc.URL + "/sub", P256dh: enc(ua.PublicKey().Bytes()), Auth: enc(make([]byte, 16))}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.watchPush(ctx)
	time.Sleep(50 * time.Millisecond) // subscribed to the bus
	return s, id, &got
}

func turn(t *testing.T, s *Server, id string) {
	t.Helper()
	ctx := context.Background()
	if err := s.App.SendPrompt(ctx, id, "go"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		th, _ := s.App.Store.Thread(ctx, id)
		if th.Status == "idle" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status %s", th.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPushOnTurnEnd(t *testing.T) {
	s, id, got := pushServer(t)
	turn(t, s, id)
	eventually(t, "no push for a finished turn", func() bool { return got.Load() == 1 })

	// On screen in a page in view: nothing.
	s.presence.open("p1", id, true)
	turn(t, s, id)
	time.Sleep(200 * time.Millisecond)
	if n := got.Load(); n != 1 {
		t.Fatalf("pushed while on screen: %d", n)
	}
	// The page goes out of view.
	s.presence.set("p1", false)
	turn(t, s, id)
	eventually(t, "no push once hidden", func() bool { return got.Load() == 2 })
	s.presence.close("p1")

	// Only what needs the reader: a finished turn stays quiet, the
	// agent's notify does not.
	s.App.Store.SetSetting(context.Background(), "notify:"+id, store.NotifyInput)
	turn(t, s, id)
	time.Sleep(200 * time.Millisecond)
	if n := got.Load(); n != 2 {
		t.Fatalf("pushed a finished turn at level input: %d", n)
	}
	s.App.Notify(context.Background(), id, "look at this")
	eventually(t, "no push for notify", func() bool { return got.Load() == 3 })
}
