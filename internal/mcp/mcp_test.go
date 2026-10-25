package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// TestServe runs the handshake, a list and a call against a stand-in for
// the agent API on a socket.
func TestServe(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("s")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /tools", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]Tool{{Name: "echo", InputSchema: map[string]any{"type": "object"}}})
	})
	mux.HandleFunc("POST /call", func(w http.ResponseWriter, r *http.Request) {
		if !Verify(secret, r.Header.Get(HeaderThread), r.Header.Get(HeaderKey)) {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		var c Call
		json.NewDecoder(r.Body).Decode(&c)
		json.NewEncoder(w).Encode(Result{Text: c.Name + " " + string(c.Arguments)})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), inR, outW, NewClient(sock, "t1", Sign(secret, "t1"))) }()
	out := bufio.NewScanner(outR)
	ask := func(line string) map[string]any {
		t.Helper()
		io.WriteString(inW, line+"\n")
		if !out.Scan() {
			t.Fatal("no answer")
		}
		var m map[string]any
		json.Unmarshal(out.Bytes(), &m)
		return m
	}
	init := ask(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	if init["result"].(map[string]any)["protocolVersion"] != "2025-03-26" {
		t.Fatalf("initialize %v", init)
	}
	io.WriteString(inW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	list := ask(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if !strings.Contains(out.Text(), `"echo"`) {
		t.Fatalf("list %v", list)
	}
	res := ask(`{"jsonrpc":"2.0","id":"x","method":"tools/call","params":{"name":"echo","arguments":{"a":1}}}`)
	if res["id"] != "x" || !strings.Contains(out.Text(), `echo {\"a\":1}`) {
		t.Fatalf("call %s", out.Text())
	}
	if e := ask(`{"jsonrpc":"2.0","id":3,"method":"bogus"}`); e["error"] == nil {
		t.Fatalf("bogus %v", e)
	}
	inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
