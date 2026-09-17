# starcode

A web console for coding agents, in one Go binary. Point it at your project folders, start threads with Claude Code or Codex, watch them work, answer their permission prompts, and read the diff they leave behind. It runs on the machine where the code lives and the browser can be anywhere, a phone included.

I built it because I wanted something like [T3 Code](https://github.com/pingdotgg/t3code) that I could run on a headless box and open from my phone, without a Node toolchain on the server. Go + [templ](https://templ.guide) + [Datastar](https://data-star.dev). No bundler, no npm; the only code generator is `templ generate`.

## What it does

- Threads per project, with Claude Code and Codex as drivers. Several CLI installs can sit side by side (Settings > Providers), each with its own config dir and colour.
- Approvals in the transcript, with "allow for session" rules and shortcuts for the oldest waiting one.
- Worktrees: a thread can get its own checkout and branch so agents on one project stay out of each other's way.
- A side panel with the git changes, a file tree and an editor, and a terminal under the transcript that survives reloads.
- Pull requests: threads link to the PR they open, the header shows its state, and one page lists everything on GitHub that wants your attention.
- Usage and plan limits per CLI install, read from the CLIs' own transcripts.
- Works on a phone: swipes for the panels, notifications when a turn ends or an approval waits, and a QR code to sign a phone in.

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

## Development

```sh
make run              # templ generate + go run . -fake -debug
make dev              # templ --watch + air, restarts on every save
go test -short ./...  # skips the tests that call the real claude and codex
go vet ./...
```

The integration tests for the adapters talk to the real CLIs and cost a few cents; plain `make test` runs them. Generated `*_templ.go` files are committed, so regenerate after every `.templ` change and commit both.

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
internal/gitx               git status, diff and worktrees via exec
internal/term               pty shells for the terminal panel
internal/keys               shortcut actions, defaults and overrides
internal/tlsx               the built-in certificate authority
internal/usage              cost and token scanning of the CLIs' transcripts
internal/web                router, auth, SSE read side, command endpoints
internal/web/views          templ components
internal/web/static         app.css and the vendored JS (datastar, xterm, highlight.js)
```

Every user action is a `POST /api/...` and every page has one SSE stream. New state goes through `internal/app` commands and domain events, never from a handler into the store.

Themes are blocks of CSS custom properties in `internal/web/static/app.css`; add a name to `web.Themes` in `internal/web/server.go` to make one selectable. Icons are [Lucide](https://lucide.dev), inlined as one SVG sprite.
