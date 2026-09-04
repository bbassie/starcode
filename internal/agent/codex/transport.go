package codex

import (
	"bytes"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
)

// transport is the byte-level link to one app-server. The real one wraps a
// `codex app-server` process; tests supply pipes.
type transport interface {
	// Reader yields the newline-delimited JSON the server writes.
	Reader() io.Reader
	// Writer takes the newline-delimited JSON we write.
	Writer() io.Writer
	// Close tears the link down. It is safe to call more than once.
	Close() error
	// Wait blocks until the far end is gone and reports why. It is called
	// only after Reader has hit EOF.
	Wait() error
}

type procTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	once sync.Once
}

// startProcess launches the app-server with stdio transport.
func startProcess(binary string, args []string, log *slog.Logger) (transport, error) {
	cmd := exec.Command(binary, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// A writer instead of StderrPipe keeps Wait free of a reader goroutine
	// it would have to synchronize with.
	cmd.Stderr = &lineLogger{log: log}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	log.Debug("codex: app-server started", "binary", binary, "pid", cmd.Process.Pid)
	return &procTransport{cmd: cmd, stdin: stdin, stdout: stdout}, nil
}

func (p *procTransport) Reader() io.Reader { return p.stdout }
func (p *procTransport) Writer() io.Writer { return p.stdin }

func (p *procTransport) Close() error {
	var err error
	p.once.Do(func() {
		p.stdin.Close()
		if p.cmd.Process != nil {
			err = p.cmd.Process.Kill()
		}
	})
	return err
}

func (p *procTransport) Wait() error { return p.cmd.Wait() }

// lineLogger forwards whole stderr lines to the log.
type lineLogger struct {
	log *slog.Logger
	buf bytes.Buffer
}

func (w *lineLogger) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			// Partial line; put it back and wait for the rest.
			w.buf.Reset()
			w.buf.WriteString(line)
			break
		}
		if s := strings.TrimSpace(line); s != "" {
			w.log.Debug("codex: app-server stderr", "line", s)
		}
	}
	return len(p), nil
}
