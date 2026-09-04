// starcode is a single-binary web console for coding agents.
//
//	starcode [-addr 127.0.0.1:4000] [-data ~/.starcode] [-token secret] [-fake]
//	starcode replay   # rebuild projections from the event log
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"starcode/internal/agent"
	"starcode/internal/agent/claude"
	"starcode/internal/agent/codex"
	"starcode/internal/agent/fake"
	"starcode/internal/app"
	"starcode/internal/bus"
	"starcode/internal/store"
	"starcode/internal/web"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "starcode:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("starcode", flag.ExitOnError)
	addr := fs.String("addr", envOr("STARCODE_ADDR", "127.0.0.1:4000"), "listen address")
	data := fs.String("data", envOr("STARCODE_DATA", defaultDataDir()), "data directory")
	token := fs.String("token", os.Getenv("STARCODE_TOKEN"), "shared secret required for non-loopback access")
	withFake := fs.Bool("fake", false, "register the scripted fake agent (for UI development)")
	debug := fs.Bool("debug", false, "debug logging")
	claudeBin := fs.String("claude", envOr("STARCODE_CLAUDE", "claude"), "claude binary")
	codexBin := fs.String("codex", envOr("STARCODE_CODEX", "codex"), "codex binary")

	args := os.Args[1:]
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs.Parse(args)

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if err := os.MkdirAll(*data, 0o755); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(*data, "starcode.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	if sub == "replay" {
		n, err := st.Replay(context.Background())
		if err != nil {
			return err
		}
		fmt.Printf("replayed %d events\n", n)
		return nil
	}
	if sub != "" {
		return fmt.Errorf("unknown command %q", sub)
	}

	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return fmt.Errorf("bad -addr: %w", err)
	}
	if *token == "" && !isLoopback(host) {
		return errors.New("refusing to listen on a non-loopback address without -token")
	}

	b := bus.New(256)
	agents := map[string]agent.Agent{
		"claude": claude.New(claude.WithBinary(*claudeBin), claude.WithLogger(log)),
		"codex":  codex.New(codex.WithBinary(*codexBin), codex.WithLogger(log)),
	}
	if *withFake {
		agents["fake"] = fake.New()
	}
	a := app.New(st, b, agents, log)
	if err := a.Recover(context.Background()); err != nil {
		return err
	}
	defer a.Shutdown()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           web.New(a, log, *token),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Info("listening", "addr", "http://"+*addr, "data", *data, "auth", *token != "")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".starcode"
	}
	return filepath.Join(home, ".starcode")
}

func isLoopback(host string) bool {
	if host == "" || host == "localhost" {
		return host == "localhost"
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
