package claude

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"starcode/internal/agent"
)

// loginTimeout bounds one sign-in: the page has to be opened and the
// code pasted within it.
const loginTimeout = 15 * time.Minute

// loginURL finds the sign-in page in the CLI's output. `claude auth login`
// prints "If the browser didn't open, visit: https://..." and then waits
// for the code on stdin.
var loginURL = regexp.MustCompile(`https://\S+`)

// login runs `claude auth login` without a terminal. The CLI cannot open a
// browser on a server, so it prints the URL and reads the code from
// stdin, which is what makes sign-in possible from the web page.
type login struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	out      bytes.Buffer
	url      string
	err      error
	canceled bool
}

// SignIn starts the CLI's sign-in and returns once it runs; the URL
// arrives a moment later.
func (a *Agent) SignIn(ctx context.Context) (agent.Login, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loginTimeout)
	cmd := exec.CommandContext(ctx, a.binary, "auth", "login")
	// On a box without a display the CLI's browser launch fails and it
	// prints the link instead. Do not set BROWSER=true to skip the
	// launch: the CLI then waits for a callback and prints nothing.
	cmd.Env = childEnv(a.env)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	l := &login{cmd: cmd, stdin: stdin, cancel: cancel, done: make(chan struct{})}
	cmd.Stdout = (*loginWriter)(l)
	cmd.Stderr = (*loginWriter)(l)
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("%s auth login: %w", a.binary, err)
	}
	a.log.Info("sign-in started", "pid", cmd.Process.Pid)
	go func() {
		err := cmd.Wait()
		timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
		cancel()
		l.mu.Lock()
		switch {
		case l.canceled:
			l.err = errors.New("sign-in cancelled")
		case timedOut && err != nil:
			l.err = errors.New("sign-in timed out")
		case err != nil:
			l.err = fmt.Errorf("sign-in failed: %s", lastLineOf(l.out.String(), err.Error()))
		}
		l.mu.Unlock()
		a.log.Info("sign-in finished", "err", l.err)
		close(l.done)
	}()
	return l, nil
}

// loginWriter collects the CLI's output and picks the URL out of it.
type loginWriter login

func (w *loginWriter) Write(p []byte) (int, error) {
	l := (*login)(w)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.out.Write(p)
	if l.url == "" {
		if m := loginURL.FindString(l.out.String()); m != "" {
			l.url = strings.TrimRight(m, ".,)")
		}
	}
	return len(p), nil
}

func (l *login) URL() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.url
}

func (l *login) Output() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.out.String()
}

func (l *login) Submit(code string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return errors.New("paste the code the sign-in page showed")
	}
	select {
	case <-l.done:
		return errors.New("this sign-in has ended; start it again")
	default:
	}
	_, err := io.WriteString(l.stdin, code+"\n")
	return err
}

func (l *login) Done() <-chan struct{} { return l.done }

func (l *login) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

func (l *login) Cancel() {
	l.mu.Lock()
	l.canceled = true
	l.mu.Unlock()
	l.cancel()
}

// lastLineOf is the last non-empty line of text, or fallback.
func lastLineOf(text, fallback string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return fallback
}
