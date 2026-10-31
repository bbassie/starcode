# starcode

A web console for coding agents, in one Go binary. Point it at your project folders, start threads with Claude Code or Codex, watch them work, answer their permission prompts, and read the diff they leave behind. It runs on the machine where the code lives and the browser can be anywhere, a phone included.

I built it because I wanted something like [T3 Code](https://github.com/pingdotgg/t3code) that I could run on a headless box and open from my phone, without a Node toolchain on the server. Go + [templ](https://templ.guide) + [Datastar](https://data-star.dev). No bundler, no npm; the only code generator is `templ generate`.

## What it does

- Threads per project, with Claude Code and Codex as drivers. Several CLI installs can sit side by side (Settings > Providers), each with its own config dir and colour. A project can name the agent, model and permission mode its new threads start with (Settings > Projects). A thread can also start without a project: it gets a folder of its own under `~/.starcode/scratch`, named for the day and the prompt. "Make it a project" in the thread's menu turns that folder into a project, left where it is.
- Settle a thread, pin it, snooze it until later or mark it unread again. Each of those shows an undo for a few seconds, and Ctrl+Z takes it.
- Rewind: drop a prompt and everything after it, and get the prompt back to edit. The agent forgets those turns too; the files stay as they are.
- Branch a thread, from its end or from before any prompt: the new thread gets a fork of the agent's conversation and works in the same checkout. The original stays as it is.
- Hand a thread off to another agent or CLI install, when one runs out of quota say. The new thread works in the same checkout, gets the conversation as a file, and says where things stand before it changes anything.
- Prompts can go out later. The clock beside the send button sends the message in an hour, tomorrow at 9:00 or at a time you pick, and it waits above the composer until then. Settings > Schedules adds recurring prompts on a cron, each run opening a new thread in its project, and lists everything scheduled with pause, run now and delete. Agents get `schedule`, `list_schedules` and `cancel_schedule` as tools. A run missed while starcode was down happens once when it is back.
- A restart does not lose running turns. They resume when starcode comes back, with a prompt that tells the agent what happened. A turn the plan's usage limit stopped waits on the Snoozed shelf and resumes when the limit resets.
- Attached images go to the agent as images, not only as a path in the prompt. One over 1568 pixels or about 4 MB is scaled down first. Codex threads get `/review` (the uncommitted changes, or `/review main` against a branch) and `/compact` in the `/` menu. A Codex thread in plan mode can put questions to you as cards with options, like Claude's.
- A message sent while a turn runs steers it by default: the agent reads it once the step it is on is done (a command, an edit) and carries on, nothing stopped, and the transcript shows it at that point. Claude Code and Codex both take it. A tap on the chip beside the send button, or Ctrl+Enter, queues one message for after the turn instead; Settings > This device makes queue (or stop and send) the default. Queued messages wait above the composer, where each can be edited in place, moved up or down, steered in with "now" or removed.
- The composer can stash a prompt for later (Ctrl+S), quote a selection from a reply, and link a pull request when you type `#`. Typing `@` offers the project's files and your other threads: a file goes in as `@path`, a thread as a link the agent can open with `read_thread`. A paste of 32 KiB or more goes in as a file.
- Mermaid blocks in replies are drawn as diagrams; a click shows the source. The script for it loads with the first diagram.
- Tool calls read as one line each, with an icon for the kind of call and the file, command or search in words. Edits open as diffs, and a turn ends with a card of the files its edit calls changed and their line counts. While a turn runs, two or more steps in a row fold under a line that says what the agent is doing. Once it is done, everything before its last reply folds under one "Worked for" line, so the prompt, the reply and the files stay in view. A picture a call gave the agent (Claude reading a screenshot, an MCP tool's screenshot, an image Codex looked at or made) shows on the call; starcode keeps a copy, so it is still there after the file is gone.
- Approvals wait as cards under the transcript, with "allow for session" rules and shortcuts for the oldest waiting one. Once answered, the card goes and the call says what you decided; a question's answers stay in the transcript.
- Claude threads have Claude Code's auto mode, where a classifier allows safe actions and blocks risky ones instead of asking. Codex threads have auto-review, where a reviewing agent decides on each request to leave the sandbox. A running subagent has a stop button that ends it alone; the turn goes on with what it had.
- A side panel with the git changes, a file tree and an editor (HTML, PDF and images open rendered), and a terminal under the transcript that survives reloads. Ctrl+F there searches every changed file's diff.
- Pull requests: threads link to the PR they open, the header shows its state, and one page lists your pull requests and everyone else's that involve you or sit in your projects' repositories, open and done, with search, filters and orders like T3 Code's; a row opens the PR beside the list. A thread can watch its PR: when a check fails or they all pass, someone else comments, or the branch conflicts, the agent gets a prompt saying so.
- Agents get starcode as tools (MCP server `starcode`). An agent can rename its thread, link and watch its PR, snooze it with a prompt for later ("check CI in 20 minutes"), schedule prompts, settle threads that are done (say, the ones whose PR merged), find threads by what was said in them, and ask for your attention. It can also start threads of its own, which show under its thread in the sidebar. A child gets the parent's permission mode at most. When it finishes, it notifies you itself, and its reply goes to the parent along with the next prompt the parent gets, so the parent doesn't run a turn just to repeat it. An agent that needs the result to carry on asks for `report: now` and gets a prompt as soon as the child is done; children that finish within a few seconds of each other arrive in one prompt. Turn the tools off under Settings > Threads.
- Usage and plan limits per CLI install, read from the CLIs' own transcripts. A Claude plan with banked resets shows them, with a button that spends one.
- A big conversation left idle for over an hour compacts before the next prompt goes out, unless you turn that off on the chip above the composer. The rename box can have the agent's small model write a title from the conversation.
- Works on a phone: swipes for the panels and a QR code to sign a phone in. Notifications come over Web Push, so they reach a phone with starcode closed: when a turn ends, when an approval or a question waits, and when the agent calls `notify`. A thread that is on screen in a page you are looking at sends none, and each thread's menu chooses between always, only when it needs you, and never.

## Run

You need Go 1.25 and `templ` (`go install github.com/a-h/templ/cmd/templ@latest`), plus the `claude` or `codex` CLI signed in on the same machine.

```sh
make build            # static binary at ./starcode
./starcode            # http://127.0.0.1:4000, data in ~/.starcode
```

To reach it from another device, bind a non-loopback address and set a token:

```sh
./starcode -addr 0.0.0.0:4000 -token some-long-secret
```

The browser gets a login page for the token, or `?token=...` on any URL. Off loopback starcode serves HTTPS with a certificate authority it makes itself under `<data>/tls/`; the login page links to the CA certificate so you can install it once per device, or just click through the warning. Browsers only hand the clipboard, notifications and service workers to a secure context, which is why it bothers. Behind a proxy that already does TLS, pass `-tls off`.

Every flag also reads an environment variable:

| flag | env | default | meaning |
|---|---|---|---|
| `-addr` | `STARCODE_ADDR` | `127.0.0.1:4000` | listen address |
| `-data` | `STARCODE_DATA` | `~/.starcode` | data directory (`starcode.db`, TLS files, keybindings) |
| `-token` | `STARCODE_TOKEN` | | shared secret; required for any non-loopback `-addr` |
| `-claude` | `STARCODE_CLAUDE` | `claude` | Claude Code binary |
| `-codex` | `STARCODE_CODEX` | `codex` | Codex binary |
| `-tls` | `STARCODE_TLS` | `auto` | `auto` (on for a non-loopback `-addr`), `on`, `off` |
| `-tls-cert`, `-tls-key` | `STARCODE_TLS_CERT`, `STARCODE_TLS_KEY` | | serve this certificate instead of the generated one |
| `-tls-hosts` | `STARCODE_TLS_HOSTS` | | extra names for the generated certificate, comma separated |
| `-fake` | | off | add a scripted agent for UI work that costs no tokens |
| `-debug` | | off | debug logging |

Two subcommands work on the database: `starcode replay` rebuilds the projection tables from the event log, and `starcode compact` folds streamed token deltas into one row per item (starcode does this itself after every turn, so it only matters for an old log).

To keep it running, `make service` installs a systemd user unit that runs the binary from the repo. [docs/dev-setup.md](docs/dev-setup.md) walks through that, and through the setup where starcode's own threads edit the repo it runs from: a rebuild puts a restart banner on every page, and nothing restarts until you press it.

## Some notes

- This is a personal tool. I use it every day, and it changes to fit how I work, so expect rough edges and bugs.
- Everything is an event in a SQLite log, and the pages are projections of it. That is what makes replay, unread marks shared across devices and drafts that follow you from phone to desktop cheap.
- Keyboard shortcuts are listed in the app under Ctrl+/ and rebound under Settings > Keys.
- The agent tools run as `starcode mcp`, a stdio MCP server each agent starts. It forwards every call to the running instance over `agent.sock` in the data dir (mode 0600). The tools themselves live in `internal/web/agentapi.go`. Each agent's environment holds its thread id and a key derived from it, so one thread cannot act as another.
- Web Push signs with a key in `vapid.pem` in the data dir; delete it and every device has to turn notifications on again. The push services want a contact address in each request, `mailto:starcode@example.com` unless `STARCODE_PUSH_SUBJECT` says otherwise. The server needs to reach the push services (Google, Mozilla, Apple) over HTTPS.

## Development

```sh
make run              # templ generate + go run . -fake -debug
make dev              # templ --watch + air, restarts on every save
go test -short ./...  # skips the tests that call the real claude and codex
go vet ./...
go test -tags e2e ./e2e/...  # drives the pages in headless Chromium
```

The integration tests for the adapters talk to the real CLIs and cost a few cents; plain `make test` runs them. The e2e tests use [Rod](https://go-rod.github.io) and the scripted fake agent against a server built inside the test; they need a Chrome or Chromium on the machine (`STARCODE_E2E_BROWSER` names one), or Rod downloads its own on the first run. Generated `*_templ.go` files are committed, so regenerate after every `.templ` change and commit both.

```
main.go                     flags, wiring, replay and compact
internal/bus                pub/sub fan-out with overflow resync
internal/domain             event types, the log's vocabulary
internal/store              SQLite: events + projections, migrations embedded
internal/agent              Agent/Session interface and normalized events
internal/agent/claude       Claude Code stream-json adapter
internal/agent/codex        Codex app-server JSON-RPC adapter
internal/agent/fake         scripted agent for UI development
internal/providers          CLI instances: providers.json, adapter construction
internal/app                commands (write side) and the session pump
internal/gitx               git status, diff and GitHub via exec
internal/term               pty shells for the terminal panel
internal/keys               shortcut actions, defaults and overrides
internal/tlsx               the built-in certificate authority
internal/usage              cost and token scanning of the CLIs' transcripts
internal/web                router, auth, SSE read side, command endpoints
internal/web/views          templ components
internal/web/static         app.css and the vendored JS (datastar, xterm, highlight.js)
e2e                         browser tests with Rod (build tag e2e)
```

Every user action is a `POST /api/...` and every page has one SSE stream. New state goes through `internal/app` commands and domain events, never from a handler into the store.

A theme has a dark and a light half, each a block of CSS custom properties in `internal/web/static/app.css` selected by `data-theme` and `data-mode` on `<html>`. The appearance setting (system, dark or light) picks the half, and `ThemeBoot` in `layout.templ` resolves "system" before the first paint. Add a name to `web.Themes` in `internal/web/server.go` to make a theme selectable; a theme without a light block uses Starcode's. Icons are [Lucide](https://lucide.dev), inlined as one SVG sprite.
