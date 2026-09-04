# starcode

A single-binary web console for coding agents. Point it at project folders, start threads with Claude Code or Codex, watch them work, approve or deny tool calls, and read the git diff they leave behind. Works from a phone.

Go + [templ](https://templ.guide) + [Datastar](https://data-star.dev). No Node, no bundler. The only code generator is `templ generate`.

## Run

```sh
make build            # produces ./starcode (static binary, CGO_ENABLED=0)
./starcode            # http://127.0.0.1:4000, data in ~/.starcode
```

Flags (each also reads an env var):

| flag | env | default | meaning |
|---|---|---|---|
| `-addr` | `STARCODE_ADDR` | `127.0.0.1:4000` | listen address |
| `-data` | `STARCODE_DATA` | `~/.starcode` | where `starcode.db` lives |
| `-token` | `STARCODE_TOKEN` | | shared secret; required for any non-loopback `-addr` |
| `-claude` | `STARCODE_CLAUDE` | `claude` | Claude Code binary |
| `-codex` | `STARCODE_CODEX` | `codex` | Codex binary |
| `-fake` | | off | register a scripted agent for UI work without spending tokens |
| `-debug` | | off | debug logging |

From a phone on the same network:

```sh
./starcode -addr 0.0.0.0:4000 -token some-long-secret
```

Browsers get a login page asking for the token; `?token=...` on any URL also works for links. A cookie keeps you signed in for a year.

`./starcode replay` rebuilds the projection tables from the event log.

## Development

```sh
make run     # templ generate + go run . -fake -debug
make dev     # templ --watch + air (go install github.com/air-verse/air@latest)
make test    # go test ./...
```

The adapters' integration tests talk to the real `claude` (haiku) and `codex` binaries and cost a few cents. `go test -short ./...` skips them.

## How it is put together

Writes and reads are separate paths.

- Every user action is a `POST /api/...` called with Datastar's `@post()`. The handler runs a command in `internal/app`, which appends events to the SQLite `events` table, updates the projection tables (`projects`, `threads`, `items`, `approvals`) in the same transaction, and publishes the events on an in-process bus (`internal/bus`).
- Every open page holds one `GET /events` SSE stream (`internal/web/events.go`). On connect it renders the page from the projections. After that it patches only the fragment each event touches: append an item, morph an item by id while it streams, swap the composer when the thread status changes. Streaming deltas are coalesced on a 60ms tick so a burst of tokens costs one morph.
- Agent processes are driven by adapters behind one interface (`internal/agent`). Claude Code runs as `claude -p --input-format stream-json --output-format stream-json --permission-prompt-tool stdio`, one process per thread, kept alive across turns. Codex runs as one shared `codex app-server` process speaking JSON-RPC over stdio. Both map their native events onto the same normalized stream (`TextDelta`, `ToolStarted`, `ApprovalRequested`, `TurnCompleted`, ...), so the app layer and UI never see protocol details.
- Sessions do not survive a restart, but threads do: the agent-side session id is stored on the thread and the next prompt resumes it (`--resume` for Claude, `thread/resume` for Codex).

Approval prompts follow each agent's own rules. Claude asks before every tool the permission mode does not cover. Codex starts threads with `sandbox: workspace-write` and `approvalPolicy: on-request`, so edits and commands inside the project run without asking and only sandbox escapes (network, paths outside the workspace) produce a card.

Each thread has settings: agent, model, reasoning effort and permission mode. The choices come from the agent itself (`agent.Describer`), so they reflect the signed-in account. Claude Code answers an `initialize` control request on its stream-json channel with the model list, display names and effort levels (`--effort`, `--permission-mode` are then passed on launch). Codex answers `model/list`, and its permission modes are combinations of approval policy and sandbox (`default`, `untrusted`, `read-only`, `full-access`); model and effort are repeated on every `turn/start`. Results are cached for 10 minutes.

Changing settings on an idle thread closes its live session; the next prompt resumes the same conversation with the new flags. The agent itself can only change before the first prompt. Whatever the session reports it is actually using is written back onto the thread, so the status bar shows the resolved model.

"Allow for session" is app-side. It remembers the tool name (and for Bash, the command's first word) for the rest of the process lifetime and auto-approves matching requests on that thread.

## UI

Three columns: a sidebar (search, projects with their threads, running threads pinned on top), the thread (breadcrumb header, transcript, composer) and a changes panel with `git status` and per-file diffs. A slim prompt navigator between the sidebar and transcript previews every sent message on hover and jumps to it on click. While a turn runs, the composer queues follow-ups in a persistent FIFO shown above the draft; queued messages can be removed and enter the transcript only when their turn starts. The panel's files tab browses the project and has an upload button that saves picked files (25 MiB per request) into the directory being viewed; a name that already exists gets a `-1` suffix instead of overwriting. Both composers take attachments too, through a paperclip button, pasting into the prompt, or dropping files onto the composer: the files land in `<data>/attachments/<thread>/` (removed with the thread) and each gets an `[Attached image "shot.png" is saved at: …]` line appended to the prompt, so the agent reads them with its own tools. Claude Code renders images it reads from disk, which makes screenshots work. A terminal button in the header opens a resizable xterm.js panel under the transcript: one shell per thread in a pty (`internal/term`), started in the project directory. The shell survives page reloads; reconnecting replays up to 128 KiB of scrollback. Output arrives as base64 chunks on its own SSE stream, keystrokes go out as POSTs, and the shell dies on thread delete, server shutdown, or `exit` (the panel then offers a restart). xterm.js and its fit addon are vendored in `internal/web/static`, loaded only on thread pages. Agent activity between two messages (thinking, tool calls, notices) folds into a "Worked for 12s · 3 steps" block that stays open while it runs. The composer carries the thread's settings as chips: model, reasoning, permission mode (and agent, until the first prompt). On the home page the same composer starts a new thread with its first prompt.

Under 900px the sidebar and changes panel become slide-overs.

## Theming

Every colour and size is a CSS custom property on `:root` in `internal/web/static/app.css`. A theme is a `[data-theme="name"]` block that overrides some of them; add the name to `web.Themes` in `internal/web/server.go` to make it selectable. The choice is stored in a cookie and applied live through a Datastar signal. `dark` and `light` use the system sans-serif face; `amber` and `green` switch `--font-ui` to monospace and drop the rounded corners for a terminal look.

Variables worth knowing: `--bg`, `--bg-2`, `--bg-3`, `--bg-4`, `--fg`, `--fg-dim`, `--border`, `--accent`, `--accent-fg`, `--ok`, `--warn`, `--err`, `--add-bg`, `--del-bg`, `--font-ui`, `--font-mono`, `--font-size`, `--radius`, `--sidebar-w`, `--gitpanel-w`, `--content-w`, `--tap`.

## Layout

```
main.go                     flags, wiring, `replay`
internal/bus                pub/sub fan-out with overflow resync
internal/domain             event types (the log's vocabulary)
internal/store              SQLite: events + projections, migrations embedded
internal/agent              Agent/Session interface and normalized events
internal/agent/claude       Claude Code stream-json adapter
internal/agent/codex        Codex app-server JSON-RPC adapter
internal/agent/fake         scripted agent for UI development
internal/app                commands (write side) and the session pump
internal/gitx               git status/diff via exec
internal/term               pty shell sessions for the terminal panel
internal/web                router, auth, SSE read side, command endpoints
internal/web/views          templ components
internal/web/static         datastar.js (vendored v1.0.3), app.css
```
