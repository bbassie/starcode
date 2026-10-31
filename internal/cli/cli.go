// Package cli is `starcode thread`: start, message, read and wait on
// threads of the running instance from a shell, a script or a cron job.
// It talks to the instance over the agent socket in the data dir (see
// web.cliRoutes), so it needs neither the token nor the certificate.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"starcode/internal/web"
)

const usage = `usage: starcode thread <command> [flags] [arguments]

  list   [-project P] [-n 20] [-json]
         threads, newest activity first
  start  [-agent A] [-model M] [-effort E] [-mode M] [-title T] [-wait] [-json] <project> <message|->
         start a thread and send it the message; project is an id, a name,
         a path inside a project's folder (. for here), or none
  send   [-steer] [-wait] [-json] <thread> <message|->
         send a message; a busy thread gets it after its turn, or in the
         turn with -steer
  show   [-n 5] [-json] <thread>
         the thread's state and its last messages
  wait   [-json] <thread>
         wait until the thread's turn is over, then show its last reply

A message of - is read from stdin. With -wait, start and send wait for
the turn to end and print the last reply. The exit status is then 1 when
the turn failed and 2 when it waits for an answer from you.

Flags go before or after the arguments. -data names the data dir
(STARCODE_DATA, default ~/.starcode).
`

// Exit codes past 1, for -wait.
const exitWaiting = 2

// ExitError carries the status the process should exit with.
type ExitError struct{ Code int }

func (e ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// Main runs `starcode thread` with args (after "thread") and dataDir as
// the default data dir.
func Main(args []string, dataDir string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "-help" || args[0] == "help" {
		fmt.Fprint(os.Stderr, usage)
		return nil
	}
	cmd, args := args[0], args[1:]
	fs := flag.NewFlagSet("starcode thread "+cmd, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	data := fs.String("data", dataDir, "data directory")
	asJSON := fs.Bool("json", false, "print JSON")
	n := fs.Int("n", 0, "how many")
	project := fs.String("project", "", "only this project")
	agent := fs.String("agent", "", "agent instance")
	model := fs.String("model", "", "model")
	effort := fs.String("effort", "", "reasoning effort")
	mode := fs.String("mode", "", "permission mode")
	title := fs.String("title", "", "thread title")
	wait := fs.Bool("wait", false, "wait for the turn to end")
	steer := fs.Bool("steer", false, "hand the message to the running turn")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	c := newClient(filepath.Join(*data, "agent.sock"))
	ctx := context.Background()
	out := os.Stdout

	need := func(k int) error {
		if len(pos) != k {
			fs.Usage()
			return fmt.Errorf("%s takes %d %s", cmd, k, map[bool]string{true: "argument", false: "arguments"}[k == 1])
		}
		return nil
	}
	switch cmd {
	case "list":
		if err := need(0); err != nil {
			return err
		}
		q := url.Values{}
		if *project != "" {
			q.Set("project", projectRef(*project))
		}
		var ts []web.CLIThread
		if err := c.do(ctx, "GET", "/cli/threads?"+q.Encode(), nil, &ts); err != nil {
			return err
		}
		if *n <= 0 {
			*n = 20
		}
		if len(ts) > *n {
			ts = ts[:*n]
		}
		if *asJSON {
			return printJSON(out, ts)
		}
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		for _, t := range ts {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.ID, t.State, ago(t.Updated), t.Project, t.Title)
		}
		return tw.Flush()
	case "start":
		if err := need(2); err != nil {
			return err
		}
		msg, err := message(pos[1])
		if err != nil {
			return err
		}
		var t web.CLIThread
		if err := c.do(ctx, "POST", "/cli/threads", web.CLIStart{Project: projectRef(pos[0]), Prompt: msg, Title: *title, Agent: *agent, Model: *model, Effort: *effort, Mode: *mode}, &t); err != nil {
			return err
		}
		if *wait {
			return c.waitShow(ctx, out, t.ID, *asJSON)
		}
		if *asJSON {
			return printJSON(out, t)
		}
		fmt.Fprintln(out, t.ID)
		return nil
	case "send":
		if err := need(2); err != nil {
			return err
		}
		msg, err := message(pos[1])
		if err != nil {
			return err
		}
		id := pos[0]
		if err := c.do(ctx, "POST", "/cli/threads/"+url.PathEscape(id)+"/send", web.CLISend{Prompt: msg, Steer: *steer}, nil); err != nil {
			return err
		}
		if *wait {
			return c.waitShow(ctx, out, id, *asJSON)
		}
		return nil
	case "show":
		if err := need(1); err != nil {
			return err
		}
		var t web.CLIShow
		if err := c.do(ctx, "GET", fmt.Sprintf("/cli/threads/%s?messages=%d", url.PathEscape(pos[0]), *n), nil, &t); err != nil {
			return err
		}
		if *asJSON {
			return printJSON(out, t)
		}
		printShow(out, t)
		return nil
	case "wait":
		if err := need(1); err != nil {
			return err
		}
		return c.waitShow(ctx, out, pos[0], *asJSON)
	}
	fs.Usage()
	return fmt.Errorf("unknown command %q", cmd)
}

// parse reads flags wherever they sit among the arguments: the flag
// package stops at the first argument, so it starts again after each.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// projectRef makes a path argument absolute, so the server can match it
// against the projects' folders; an id or a name goes as it is.
func projectRef(ref string) string {
	if ref == "none" {
		return ref
	}
	if strings.HasPrefix(ref, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			ref = filepath.Join(home, ref[2:])
		}
	}
	if ref == "." || ref == ".." || strings.ContainsRune(ref, filepath.Separator) {
		if abs, err := filepath.Abs(ref); err == nil {
			if real, err := filepath.EvalSymlinks(abs); err == nil {
				abs = real
			}
			return abs
		}
	}
	return ref
}

// message is the argument, or stdin for -.
func message(arg string) (string, error) {
	if arg != "-" {
		return arg, nil
	}
	b, err := io.ReadAll(os.Stdin)
	return string(b), err
}

// waitShow waits for the thread's turn to end, then prints it with its
// last reply, and maps how it ended to the exit status.
func (c *client) waitShow(ctx context.Context, out io.Writer, id string, asJSON bool) error {
	for {
		var r struct{ Done bool }
		if err := c.do(ctx, "POST", "/cli/threads/"+url.PathEscape(id)+"/wait?timeout=600", nil, &r); err != nil {
			return err
		}
		if r.Done {
			break
		}
	}
	var t web.CLIShow
	if err := c.do(ctx, "GET", "/cli/threads/"+url.PathEscape(id)+"?messages=1", nil, &t); err != nil {
		return err
	}
	if asJSON {
		if err := printJSON(out, t); err != nil {
			return err
		}
	} else {
		printShow(out, t)
	}
	switch t.State {
	case "error":
		return ExitError{1}
	case "awaiting_approval":
		return ExitError{exitWaiting}
	}
	return nil
}

func printShow(out io.Writer, t web.CLIShow) {
	fmt.Fprintf(out, "%s  %s  %s\n%s · %s · %s\n", t.ID, t.State, t.Title, t.Project, t.Agent, ago(t.Updated))
	if t.Detail != "" {
		fmt.Fprintf(out, "It %s.\n", t.Detail)
	}
	for _, m := range t.Messages {
		fmt.Fprintf(out, "\n## %s, %s\n\n%s\n", m.Role, m.At.Local().Format("Jan 2 15:04"), m.Text)
	}
}

func printJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// ago is a short relative age: now, 9m, 3h, 2d.
func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

type client struct{ http *http.Client }

func newClient(socket string) *client {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}
	return &client{http: &http.Client{Transport: tr}}
}

func (c *client) do(ctx context.Context, method, path string, body, out any) error {
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
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return errors.New("starcode is not running here, or not with this data dir (-data)")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return errors.New(strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}
