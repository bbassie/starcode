// Package mcp is the starcode tools as an MCP server for the agents.
//
// An agent launches `starcode mcp` as a stdio MCP server. That process
// holds no state: it answers the MCP handshake and passes tools/list and
// tools/call on to the running starcode over a Unix socket in the data
// dir, which is where the tools live (web.Server's agent API). The
// environment the app gives the agent says which thread is calling and
// carries a key that proves it, so one thread's agent cannot act as
// another's by accident.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
)

// Environment of the `starcode mcp` process.
const (
	EnvSocket = "STARCODE_SOCKET"
	EnvThread = "STARCODE_THREAD"
	EnvKey    = "STARCODE_AGENT_KEY"
)

// Headers the agent API reads the caller from.
const (
	HeaderThread = "X-Starcode-Thread"
	HeaderKey    = "X-Starcode-Key"
)

// Sign is the key a thread's agent gets: an HMAC of the thread id under
// the running server's secret.
func Sign(secret []byte, threadID string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(threadID))
	return hex.EncodeToString(m.Sum(nil))
}

// Verify checks a key Sign made.
func Verify(secret []byte, threadID, key string) bool {
	return threadID != "" && hmac.Equal([]byte(Sign(secret, threadID)), []byte(key))
}

// Tool is one tool as tools/list describes it.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Call is a tools/call the agent API runs.
type Call struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Result is what a tool call returns: text for the agent, and whether it
// failed.
type Result struct {
	Text    string `json:"text"`
	IsError bool   `json:"isError,omitempty"`
}

// Client talks to the agent API on the socket.
type Client struct {
	Thread, Key string
	http        *http.Client
}

// NewClient dials socket for every request. Calls get no timeout: a
// wait_thread can block for minutes, and the agent cancels what it no
// longer wants.
func NewClient(socket, thread, key string) *Client {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}
	return &Client{Thread: thread, Key: key, http: &http.Client{Transport: tr}}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://starcode"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set(HeaderThread, c.Thread)
	req.Header.Set(HeaderKey, c.Key)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("starcode is not reachable: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("starcode: %s: %s", res.Status, bytes.TrimSpace(msg))
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// Tools lists the tools.
func (c *Client) Tools(ctx context.Context) ([]Tool, error) {
	var ts []Tool
	err := c.do(ctx, http.MethodGet, "/tools", nil, &ts)
	return ts, err
}

// Run calls a tool.
func (c *Client) Run(ctx context.Context, call Call) (Result, error) {
	var r Result
	err := c.do(ctx, http.MethodPost, "/call", call, &r)
	return r, err
}

// Main is `starcode mcp`: it serves stdin and stdout until stdin closes.
func Main() error {
	socket, thread, key := os.Getenv(EnvSocket), os.Getenv(EnvThread), os.Getenv(EnvKey)
	if socket == "" || thread == "" || key == "" {
		return errors.New("starcode mcp is started by starcode for an agent; " + EnvSocket + ", " + EnvThread + " and " + EnvKey + " must be set")
	}
	return Serve(context.Background(), os.Stdin, os.Stdout, NewClient(socket, thread, key))
}

// protocolVersion is what the server answers with when the client asks
// for none.
const protocolVersion = "2025-06-18"

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve speaks MCP over newline-delimited JSON-RPC. Tool calls run
// concurrently, so a long wait does not hold up the rest; a
// notifications/cancelled stops the call it names.
func Serve(ctx context.Context, in io.Reader, out io.Writer, c *Client) error {
	var wmu sync.Mutex
	write := func(m rpcMsg) {
		m.JSONRPC = "2.0"
		b, _ := json.Marshal(m)
		wmu.Lock()
		out.Write(append(b, '\n'))
		wmu.Unlock()
	}
	reply := func(id json.RawMessage, result any, err error) {
		if err != nil {
			write(rpcMsg{ID: id, Error: &rpcError{Code: -32603, Message: err.Error()}})
			return
		}
		write(rpcMsg{ID: id, Result: result})
	}

	var cmu sync.Mutex
	cancels := map[string]context.CancelFunc{}
	var wg sync.WaitGroup
	defer wg.Wait()

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var m rpcMsg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			write(rpcMsg{ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		if len(m.ID) == 0 {
			// A notification. Only a cancel needs anything.
			if m.Method == "notifications/cancelled" {
				var p struct {
					RequestID json.RawMessage `json:"requestId"`
				}
				json.Unmarshal(m.Params, &p)
				cmu.Lock()
				if cancel := cancels[string(p.RequestID)]; cancel != nil {
					cancel()
				}
				cmu.Unlock()
			}
			continue
		}
		switch m.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			json.Unmarshal(m.Params, &p)
			v := p.ProtocolVersion
			if v == "" {
				v = protocolVersion
			}
			reply(m.ID, map[string]any{
				"protocolVersion": v,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "starcode", "version": "1"},
				"instructions":    instructions,
			}, nil)
		case "ping":
			reply(m.ID, map[string]any{}, nil)
		case "tools/list":
			ts, err := c.Tools(ctx)
			reply(m.ID, map[string]any{"tools": ts}, err)
		case "tools/call":
			var call Call
			if err := json.Unmarshal(m.Params, &call); err != nil {
				reply(m.ID, nil, err)
				continue
			}
			cctx, cancel := context.WithCancel(ctx)
			key := string(m.ID)
			cmu.Lock()
			cancels[key] = cancel
			cmu.Unlock()
			wg.Add(1)
			go func(id json.RawMessage) {
				defer wg.Done()
				defer func() {
					cmu.Lock()
					delete(cancels, key)
					cmu.Unlock()
					cancel()
				}()
				r, err := c.Run(cctx, call)
				if err != nil {
					// A failed call is a result the agent can read, not a
					// protocol error.
					r = Result{Text: err.Error(), IsError: true}
				}
				reply(id, map[string]any{
					"content": []map[string]any{{"type": "text", "text": r.Text}},
					"isError": r.IsError,
				}, nil)
			}(m.ID)
		default:
			write(rpcMsg{ID: m.ID, Error: &rpcError{Code: -32601, Message: "method not found: " + m.Method}})
		}
	}
	return sc.Err()
}

// instructions is what the agent reads about the server as a whole.
const instructions = `These tools act on starcode, the web console the user runs you in. Every conversation is a thread there; yours is the "current thread". Use them to keep your thread tidy (title, linked pull request, snooze until something is ready) and to hand work to other threads (spawn_thread, then wait_thread or let starcode wake you when they finish).`
