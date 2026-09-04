package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// callTimeout bounds every request we send. turn/start returns as soon as the
// turn is created, so no request has to wait on model output.
const callTimeout = 60 * time.Second

// errProcessGone is returned once the app-server has exited.
var errProcessGone = errors.New("codex: app-server is not running")

// rpcError is the JSON-RPC error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("codex: rpc error %d: %s", e.Code, e.Message)
}

// wireMessage is any message read from the server. The protocol omits the
// "jsonrpc" field, so we do too.
//
// A message with an id and a method is a server-initiated request, one with an
// id and no method is a response to us, and one without an id is a
// notification.
type wireMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type outRequest struct {
	ID     int64  `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

type outNotification struct {
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

type outResponse struct {
	ID     json.RawMessage `json:"id"`
	Result any             `json:"result"`
}

type outErrorResponse struct {
	ID    json.RawMessage `json:"id"`
	Error rpcError        `json:"error"`
}

// client owns one app-server process and multiplexes every session over it.
type client struct {
	log *slog.Logger
	tr  transport

	wmu sync.Mutex // serializes writes

	mu       sync.Mutex
	nextID   int64
	pending  map[int64]chan wireMessage
	sessions map[string]*session
	dead     bool
	deadErr  error
	stopping bool
}

func newClient(tr transport, log *slog.Logger) *client {
	return &client{
		log:      log,
		tr:       tr,
		pending:  make(map[int64]chan wireMessage),
		sessions: make(map[string]*session),
	}
}

// run starts the reader goroutine.
func (c *client) run() { go c.readLoop() }

// handshake performs the once-per-process initialize exchange.
func (c *client) handshake(ctx context.Context) error {
	params := map[string]any{
		"clientInfo": map[string]any{
			"name":    clientName,
			"title":   clientName,
			"version": clientVersion,
		},
		"capabilities": map[string]any{},
	}
	if _, err := c.call(ctx, "initialize", params); err != nil {
		return err
	}
	return c.notify("initialized", nil)
}

func (c *client) isDead() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dead
}

func (c *client) addSession(s *session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions[s.threadID] = s
}

func (c *client) removeSession(s *session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessions[s.threadID] == s {
		delete(c.sessions, s.threadID)
	}
}

func (c *client) session(threadID string) *session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessions[threadID]
}

// shutdown kills the app-server. Sessions learn about it through the reader
// loop, which then closes them with an error.
func (c *client) shutdown() error {
	c.mu.Lock()
	c.stopping = true
	c.mu.Unlock()
	return c.tr.Close()
}

// call sends a request and waits for its response.
func (c *client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	c.mu.Lock()
	if c.dead {
		err := c.deadErr
		c.mu.Unlock()
		return nil, err
	}
	c.nextID++
	id := c.nextID
	ch := make(chan wireMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.write(outRequest{ID: id, Method: method, Params: params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("codex: %s: %w", method, ctx.Err())
	case msg := <-ch:
		if msg.Error != nil {
			return nil, fmt.Errorf("codex: %s: %w", method, msg.Error)
		}
		return msg.Result, nil
	}
}

func (c *client) notify(method string, params any) error {
	return c.write(outNotification{Method: method, Params: params})
}

func (c *client) respond(id json.RawMessage, result any) error {
	return c.write(outResponse{ID: id, Result: result})
}

func (c *client) respondError(id json.RawMessage, code int, message string) error {
	return c.write(outErrorResponse{ID: id, Error: rpcError{Code: code, Message: message}})
}

func (c *client) write(msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	b = append(b, '\n')

	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.mu.Lock()
	dead := c.dead
	deadErr := c.deadErr
	c.mu.Unlock()
	if dead {
		return deadErr
	}
	if _, err := c.tr.Writer().Write(b); err != nil {
		return fmt.Errorf("codex: write: %w", err)
	}
	return nil
}

func (c *client) readLoop() {
	r := bufio.NewReaderSize(c.tr.Reader(), 64*1024)
	var readErr error
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			c.handleLine(line)
		}
		if err != nil {
			if err != io.EOF {
				readErr = err
			}
			break
		}
	}
	waitErr := c.tr.Wait()
	c.fail(exitError(readErr, waitErr))
}

func (c *client) handleLine(line []byte) {
	var msg wireMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		c.log.Warn("codex: unparsable message", "err", err, "line", string(line))
		return
	}
	switch {
	case len(msg.ID) > 0 && msg.Method == "":
		c.handleResponse(msg)
	case len(msg.ID) > 0:
		c.handleServerRequest(msg)
	default:
		c.handleNotification(msg)
	}
}

func (c *client) handleResponse(msg wireMessage) {
	id, err := strconv.ParseInt(string(bytes.Trim(msg.ID, `"`)), 10, 64)
	if err != nil {
		c.log.Warn("codex: response with unusable id", "id", string(msg.ID))
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ch == nil {
		c.log.Debug("codex: response for unknown request", "id", id)
		return
	}
	ch <- msg
}

// threadOf pulls the routing key out of a params object.
func threadOf(params json.RawMessage) string {
	var p struct {
		ThreadID string `json:"threadId"`
	}
	if len(params) == 0 {
		return ""
	}
	_ = json.Unmarshal(params, &p)
	return p.ThreadID
}

func (c *client) handleNotification(msg wireMessage) {
	threadID := threadOf(msg.Params)
	if threadID == "" {
		// Thread lifecycle notifications carry the thread inside "thread"
		// instead; those are informational, so log and move on.
		c.log.Debug("codex: notification without thread", "method", msg.Method)
		return
	}
	s := c.session(threadID)
	if s == nil {
		c.log.Debug("codex: notification for unknown thread", "method", msg.Method, "thread", threadID)
		return
	}
	s.handleNotification(msg.Method, msg.Params)
}

// handleServerRequest answers requests the server sends us. Every path must
// write exactly one response or the server waits forever.
func (c *client) handleServerRequest(msg wireMessage) {
	switch msg.Method {
	case "currentTime/read":
		if err := c.respond(msg.ID, map[string]any{"currentTimeAt": time.Now().Unix()}); err != nil {
			c.log.Warn("codex: answer currentTime/read", "err", err)
		}
		return
	case "attestation/generate":
		// We have no attestation to offer.
		if err := c.respondError(msg.ID, -32601, "unsupported"); err != nil {
			c.log.Warn("codex: answer attestation/generate", "err", err)
		}
		return
	}

	threadID := threadOf(msg.Params)
	s := c.session(threadID)
	if s == nil {
		c.log.Debug("codex: server request for unknown thread", "method", msg.Method, "thread", threadID)
		c.declineRequest(msg, nil)
		return
	}
	s.handleServerRequest(msg)
}

// declineRequest sends the least surprising refusal for a request we cannot
// or will not answer properly.
func (c *client) declineRequest(msg wireMessage, s *session) {
	var (
		result any
		notice string
	)
	switch msg.Method {
	case "mcpServer/elicitation/request", "item/tool/requestUserInput":
		result = map[string]any{"action": "decline", "content": nil}
		notice = "auto-declined " + msg.Method + ": starcode cannot collect this input"
	case "item/permissions/requestApproval":
		// An empty grant means every requested permission is denied.
		result = map[string]any{"permissions": map[string]any{}}
		notice = "auto-declined a permission request from codex"
	case "item/tool/call":
		result = map[string]any{
			"contentItems": []any{map[string]any{"type": "inputText", "text": "declined"}},
			"success":      false,
		}
		notice = "auto-declined a dynamic tool call from codex"
	default:
		if err := c.respondError(msg.ID, -32601, "unsupported"); err != nil {
			c.log.Warn("codex: decline server request", "method", msg.Method, "err", err)
		}
		if s != nil {
			s.emitNotice("codex asked for " + msg.Method + ", which starcode does not support")
		}
		return
	}
	if err := c.respond(msg.ID, result); err != nil {
		c.log.Warn("codex: decline server request", "method", msg.Method, "err", err)
	}
	if s != nil && notice != "" {
		s.emitNotice(notice)
	}
}

// fail marks the process gone, unblocks every pending call, and closes every
// session bound to it.
func (c *client) fail(err error) {
	c.mu.Lock()
	if c.dead {
		c.mu.Unlock()
		return
	}
	c.dead = true
	if err == nil {
		err = errProcessGone
	}
	c.deadErr = err
	pending := c.pending
	c.pending = make(map[int64]chan wireMessage)
	sessions := make([]*session, 0, len(c.sessions))
	for _, s := range c.sessions {
		sessions = append(sessions, s)
	}
	c.sessions = make(map[string]*session)
	stopping := c.stopping
	c.mu.Unlock()

	if stopping {
		c.log.Debug("codex: app-server stopped", "err", err)
	} else {
		c.log.Warn("codex: app-server exited", "err", err)
	}

	rpc := &rpcError{Code: -32000, Message: err.Error()}
	for _, ch := range pending {
		ch <- wireMessage{Error: rpc}
	}
	for _, s := range sessions {
		s.finish(err)
	}
	_ = c.tr.Close()
}

func exitError(readErr, waitErr error) error {
	switch {
	case waitErr != nil:
		return fmt.Errorf("codex: app-server exited: %w", waitErr)
	case readErr != nil:
		return fmt.Errorf("codex: reading from app-server: %w", readErr)
	}
	return errProcessGone
}
