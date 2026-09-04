// Package term runs one shell per thread in a pty and fans its output out
// to any number of SSE subscribers. Sessions survive page reloads: the
// scrollback buffer is replayed on connect, and the shell only dies when it
// exits, is killed, or the server shuts down.
package term

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
)

const (
	scrollback = 128 << 10 // bytes of output replayed on connect
	subBuffer  = 256       // chunks buffered per subscriber before it is dropped
)

type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	log      *slog.Logger
	shell    string
}

func NewManager(log *slog.Logger) *Manager {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "sh"
	}
	return &Manager{sessions: map[string]*Session{}, log: log, shell: shell}
}

// Session returns the live session for id, starting a fresh shell in dir
// when there is none (or only a dead one).
func (m *Manager) Session(id, dir string, cols, rows int) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[id]; s != nil && !s.Exited() {
		return s, nil
	}
	s, err := start(m.shell, dir, cols, rows, m.log)
	if err != nil {
		return nil, err
	}
	m.sessions[id] = s
	return s, nil
}

// Live returns the running session for id, or nil.
func (m *Manager) Live(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[id]; s != nil && !s.Exited() {
		return s
	}
	return nil
}

// Kill ends the session for id, if any.
func (m *Manager) Kill(id string) {
	m.mu.Lock()
	s := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if s != nil {
		s.Kill()
	}
}

// Shutdown kills every shell. Ptys get their own process session, so they
// would outlive the server otherwise.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.sessions = map[string]*Session{}
	m.mu.Unlock()
	for _, s := range all {
		s.Kill()
	}
}

type Session struct {
	mu     sync.Mutex
	ptmx   *os.File
	cmd    *exec.Cmd
	buf    []byte
	subs   map[chan []byte]bool
	exited chan struct{}
}

func start(shell, dir string, cols, rows int, log *slog.Logger) (*Session, error) {
	if cols <= 0 || rows <= 0 {
		cols, rows = 80, 24
	}
	cmd := exec.Command(shell)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	s := &Session{ptmx: ptmx, cmd: cmd, subs: map[chan []byte]bool{}, exited: make(chan struct{})}
	go s.read(log)
	return s, nil
}

// read pumps pty output into the scrollback and to every subscriber. A
// subscriber that cannot keep up loses its channel and reconnects through
// the scrollback replay instead of stalling the shell.
func (s *Session) read(log *slog.Logger) {
	buf := make([]byte, 4096)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			s.mu.Lock()
			s.buf = append(s.buf, chunk...)
			if len(s.buf) > scrollback {
				s.buf = append([]byte(nil), s.buf[len(s.buf)-scrollback:]...)
			}
			for ch := range s.subs {
				select {
				case ch <- chunk:
				default:
					delete(s.subs, ch)
					close(ch)
				}
			}
			s.mu.Unlock()
		}
		if err != nil {
			break
		}
	}
	s.mu.Lock()
	for ch := range s.subs {
		close(ch)
	}
	s.subs = nil
	close(s.exited)
	s.mu.Unlock()
	s.ptmx.Close()
	if err := s.cmd.Wait(); err != nil && log != nil {
		log.Debug("terminal shell exited", "err", err)
	}
}

func (s *Session) Exited() bool {
	select {
	case <-s.exited:
		return true
	default:
		return false
	}
}

// Subscribe returns the scrollback so far and a channel of later output.
// The channel is closed when the shell exits or the subscriber lags.
func (s *Session) Subscribe() (replay []byte, ch chan []byte, cancel func()) {
	ch = make(chan []byte, subBuffer)
	s.mu.Lock()
	replay = append([]byte(nil), s.buf...)
	if s.subs == nil {
		close(ch)
	} else {
		s.subs[ch] = true
	}
	s.mu.Unlock()
	return replay, ch, func() {
		s.mu.Lock()
		if s.subs != nil && s.subs[ch] {
			delete(s.subs, ch)
			close(ch)
		}
		s.mu.Unlock()
	}
}

func (s *Session) Write(p []byte) error {
	if s.Exited() {
		return errors.New("terminal session has ended")
	}
	_, err := s.ptmx.Write(p)
	return err
}

func (s *Session) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 || cols > 1000 || rows > 1000 {
		return errors.New("bad terminal size")
	}
	if s.Exited() {
		return errors.New("terminal session has ended")
	}
	return pty.Setsize(s.ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (s *Session) Kill() {
	if s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
	s.ptmx.Close()
}
