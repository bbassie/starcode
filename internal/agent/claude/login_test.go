package claude

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeLoginCLI is a stand-in for `claude auth login`: it prints the link
// the way the CLI does, reads one line and exits 0 on the right code.
func fakeLoginCLI(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in")
	}
	path := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
[ "$1" = auth ] && [ "$2" = login ] || exit 2
echo "Opening browser to sign in…"
echo "If the browser didn't open, visit: https://claude.com/cai/oauth/authorize?code=true&state=abc"
printf 'Paste code here if prompted > '
read code
[ "$code" = "good-code" ] && { echo "Logged in as someone@example.com"; exit 0; }
echo "Invalid authorization code" >&2
exit 1
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitURL(t *testing.T, l interface{ URL() string }) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if u := l.URL(); u != "" {
			return u
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no sign-in URL appeared")
	return ""
}

func TestSignInHandsTheCodeToTheCLI(t *testing.T) {
	a := New(WithBinary(fakeLoginCLI(t)))
	l, err := a.SignIn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u := waitURL(t, l); !strings.HasPrefix(u, "https://claude.com/cai/oauth/authorize?") {
		t.Fatalf("url = %q", u)
	}
	if err := l.Submit("  good-code\n"); err != nil {
		t.Fatal(err)
	}
	<-l.Done()
	if err := l.Err(); err != nil {
		t.Fatalf("err = %v; output %q", err, l.Output())
	}
	if !strings.Contains(l.Output(), "Logged in as") {
		t.Fatalf("output = %q", l.Output())
	}
	if err := l.Submit("again"); err == nil {
		t.Fatal("submit after exit should fail")
	}
}

func TestSignInReportsABadCode(t *testing.T) {
	a := New(WithBinary(fakeLoginCLI(t)))
	l, err := a.SignIn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waitURL(t, l)
	if err := l.Submit(""); err == nil {
		t.Fatal("empty code should be refused")
	}
	if err := l.Submit("bad"); err != nil {
		t.Fatal(err)
	}
	<-l.Done()
	if err := l.Err(); err == nil || !strings.Contains(err.Error(), "Invalid authorization code") {
		t.Fatalf("err = %v", err)
	}
}

func TestSignInCancel(t *testing.T) {
	a := New(WithBinary(fakeLoginCLI(t)))
	l, err := a.SignIn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waitURL(t, l)
	l.Cancel()
	select {
	case <-l.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not end the sign-in")
	}
	if err := l.Err(); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("err = %v", err)
	}
}
