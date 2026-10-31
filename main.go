// starcode is a single-binary web console for coding agents.
//
//	starcode [-addr 127.0.0.1:4000] [-data ~/.starcode] [-token secret] [-fake]
//	starcode replay   # rebuild projections from the event log
//	starcode compact  # fold streamed deltas into one row per item
package main

import (
	"context"
	"crypto/tls"
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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/klauspost/compress/gzhttp"

	"starcode/internal/agent"
	"starcode/internal/app"
	"starcode/internal/bus"
	"starcode/internal/cli"
	"starcode/internal/keys"
	"starcode/internal/mcp"
	"starcode/internal/providers"
	"starcode/internal/push"
	"starcode/internal/store"
	"starcode/internal/tlsx"
	"starcode/internal/web"
	"starcode/internal/web/views"
)

func main() {
	restart, err := run()
	var exit cli.ExitError
	if errors.As(err, &exit) {
		// `starcode thread ... -wait` has printed how the turn ended.
		os.Exit(exit.Code)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "starcode:", err)
		os.Exit(1)
	}
	if restart != "" {
		// Same arguments, same environment, new binary. Exec keeps the pid,
		// so a service manager sees nothing happen.
		if err := syscall.Exec(restart, os.Args, os.Environ()); err != nil {
			fmt.Fprintln(os.Stderr, "starcode: restart:", err)
			os.Exit(1)
		}
	}
}

// run serves until interrupted. It returns the executable to re-exec when
// the UI asked for a restart, otherwise "".
func run() (string, error) {
	fs := flag.NewFlagSet("starcode", flag.ExitOnError)
	addr := fs.String("addr", envOr("STARCODE_ADDR", "127.0.0.1:4000"), "listen address")
	data := fs.String("data", envOr("STARCODE_DATA", defaultDataDir()), "data directory")
	token := fs.String("token", os.Getenv("STARCODE_TOKEN"), "shared secret required for non-loopback access")
	withFake := fs.Bool("fake", false, "register the scripted fake agent (for UI development)")
	debug := fs.Bool("debug", false, "debug logging")
	claudeBin := fs.String("claude", envOr("STARCODE_CLAUDE", "claude"), "claude binary")
	codexBin := fs.String("codex", envOr("STARCODE_CODEX", "codex"), "codex binary")
	tlsMode := fs.String("tls", envOr("STARCODE_TLS", "auto"), "serve HTTPS: auto (on for a non-loopback -addr), on, off")
	tlsCert := fs.String("tls-cert", os.Getenv("STARCODE_TLS_CERT"), "certificate file to serve instead of the generated one (with -tls-key)")
	tlsKey := fs.String("tls-key", os.Getenv("STARCODE_TLS_KEY"), "key file for -tls-cert")
	tlsHosts := fs.String("tls-hosts", os.Getenv("STARCODE_TLS_HOSTS"), "extra names for the generated certificate, comma separated")

	args := os.Args[1:]
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	if sub == "thread" {
		// The command line for threads has flags of its own and talks
		// to the running instance, like `starcode mcp`.
		return "", cli.Main(args, envOr("STARCODE_DATA", defaultDataDir()))
	}
	fs.Parse(args)
	if sub == "mcp" {
		// An agent's tool server: it talks to the running instance over
		// its socket and must not touch the database itself.
		return "", mcp.Main()
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if err := os.MkdirAll(*data, 0o755); err != nil {
		return "", err
	}
	st, err := store.Open(filepath.Join(*data, "starcode.db"))
	if err != nil {
		return "", err
	}
	defer st.Close()

	if sub == "replay" {
		n, err := st.Replay(context.Background())
		if err != nil {
			return "", err
		}
		fmt.Printf("replayed %d events\n", n)
		return "", nil
	}
	if sub == "compact" {
		n, err := st.Compact(context.Background(), "")
		if err != nil {
			return "", err
		}
		fmt.Printf("removed %d delta rows\n", n)
		return "", nil
	}
	if sub != "" {
		return "", fmt.Errorf("unknown command %q", sub)
	}

	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return "", fmt.Errorf("bad -addr: %w", err)
	}
	if *token == "" && !isLoopback(host) {
		return "", errors.New("refusing to listen on a non-loopback address without -token")
	}

	// A page that falls behind by this many events gets a full redraw;
	// an agent turn streams deltas in bursts of hundreds, so the buffer is
	// generous to keep those redraws rare.
	b := bus.New(1024)
	// Agent instances come from <data>/providers.json; the built-in
	// "claude" and "codex" run the binaries named by the flags unless the
	// file says otherwise.
	prov, err := providers.Open(filepath.Join(*data, "providers.json"), map[string]string{"claude": *claudeBin, "codex": *codexBin}, *withFake, log)
	if err != nil {
		return "", err
	}
	agents := map[string]agent.Agent{}
	for _, in := range prov.Instances() {
		if !in.Enabled {
			continue
		}
		ag, err := prov.Build(in)
		if err != nil {
			log.Warn("provider instance skipped", "instance", in.Name, "err", err)
			continue
		}
		agents[in.Name] = ag
	}
	a := app.New(st, b, agents, log)
	a.AttachDir = filepath.Join(*data, "attachments")
	a.ScratchDir = filepath.Join(*data, "scratch")
	if err := a.Recover(context.Background()); err != nil {
		return "", err
	}
	defer a.Shutdown()
	// Threads that ended before compaction existed still carry a row per
	// token; fold them once, off the startup path.
	go func() {
		if n, err := st.Compact(context.Background(), ""); err != nil {
			log.Warn("compact", "err", err)
		} else if n > 0 {
			log.Info("compacted event log", "rows", n)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	h := web.New(a, log, *token, filepath.Join(*data, "attachments"), prov)
	defer h.Close()
	kb, err := keys.Open(filepath.Join(*data, "keybindings.json"))
	if err != nil {
		return "", err
	}
	h.Keys = kb
	// Web Push needs a key of the instance's own; without one the
	// notifications fall back to the page's (an open tab only).
	if ps, err := push.Load(filepath.Join(*data, "vapid.pem")); err != nil {
		log.Warn("web push off", "err", err)
	} else {
		if sub := os.Getenv("STARCODE_PUSH_SUBJECT"); sub != "" {
			ps.Subject = sub
		}
		h.Push = ps
	}
	views.SetKeys(kb)
	// HTTPS is on wherever a phone could be on the other end. The
	// certificate comes from a CA of this instance's own unless files are
	// given; devices install the CA once and the warning goes away.
	var tlsCfg *tls.Config
	useTLS := *tlsMode == "on" || (*tlsMode == "auto" && !isLoopback(host))
	switch {
	case *tlsMode != "on" && *tlsMode != "off" && *tlsMode != "auto":
		return "", fmt.Errorf("bad -tls %q: want auto, on or off", *tlsMode)
	case useTLS && *tlsCert != "":
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			return "", fmt.Errorf("load -tls-cert: %w", err)
		}
		tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1"}, MinVersion: tls.VersionTLS12}
	case useTLS:
		hosts := tlsx.LocalHosts(strings.Split(*tlsHosts, ",")...)
		cert, ca, err := tlsx.Load(filepath.Join(*data, "tls"), hosts)
		if err != nil {
			return "", err
		}
		h.CA = ca
		tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1"}, MinVersion: tls.VersionTLS12}
		log.Info("tls certificate", "hosts", hosts, "ca", filepath.Join(*data, "tls", "ca.pem"))
	}
	h.Secure = tlsCfg != nil
	h.Update = web.NewSelfUpdate(log)
	var restart atomic.Bool
	h.OnRestart = func() {
		restart.Store(true)
		stop()
	}
	h.Watch(ctx)

	// The agents reach the starcode tools through `starcode mcp`, which
	// calls back on a socket of their own in the data dir.
	if exe, err := os.Executable(); err != nil {
		log.Warn("agent tools off: no executable path", "err", err)
	} else if stopTools, err := serveAgentAPI(ctx, filepath.Join(*data, "agent.sock"), h.AgentHandler(), log); err != nil {
		log.Warn("agent tools off", "err", err)
	} else {
		defer stopTools()
		sock := filepath.Join(*data, "agent.sock")
		a.Tools = func(threadID string) *agent.MCPServer {
			return &agent.MCPServer{Command: exe, Args: []string{"mcp"}, Env: map[string]string{
				mcp.EnvSocket: sock, mcp.EnvThread: threadID, mcp.EnvKey: h.AgentKey(threadID),
			}}
		}
	}

	// Requests inherit reqCtx so cancelling it ends every SSE stream at
	// once; otherwise Shutdown would sit out its timeout waiting on them.
	reqCtx, cancelReqs := context.WithCancel(context.Background())
	defer cancelReqs()
	// Pages, fragments and SSE streams go out gzipped when the browser
	// accepts it. gzhttp flushes the compressor on every Flush, so each
	// SSE event still reaches the page as soon as it is sent, and the
	// stream keeps one compression window for its whole life.
	srv := &http.Server{
		Addr:              *addr,
		Handler:           gzhttp.GzipHandler(h),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return reqCtx },
		// Over HTTPS a page puts its stream, reconnects and posts on one
		// HTTP/2 connection. A network change (a VPN switching paths) can
		// kill it without either end hearing, and TCP alone takes many
		// minutes to give up while the heartbeats sit unacknowledged. A
		// ping that goes unanswered closes it after about 45s, so the
		// device's next packet on it meets a reset and the browser opens
		// a new connection instead of queueing on the dead one.
		HTTP2: &http.HTTP2Config{
			SendPingTimeout:  30 * time.Second,
			PingTimeout:      15 * time.Second,
			WriteByteTimeout: 30 * time.Second,
		},
	}
	go func() {
		<-ctx.Done()
		cancelReqs()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return "", err
	}
	scheme := "http://"
	if tlsCfg != nil {
		ln = tlsx.Listen(ln, tlsCfg)
		scheme = "https://"
	}
	log.Info("listening", "addr", scheme+*addr, "data", *data, "auth", *token != "")
	// Everything is wired and the port is ours: the sweeps can run, and
	// the turns the last restart cut off pick up again.
	a.Start()
	go a.ResumeCutOff(context.Background())
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return "", err
	}
	if restart.Load() {
		log.Info("restarting", "exe", h.Update.Home)
		return h.Update.Home, nil
	}
	return "", nil
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

// serveAgentAPI serves the agent API on a Unix socket only this user can
// open. A socket left by an instance that died is replaced.
func serveAgentAPI(ctx context.Context, path string, h http.Handler, log *slog.Logger) (func(), error) {
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("agent api", "err", err)
		}
	}()
	return func() { srv.Close(); os.Remove(path) }, nil
}
