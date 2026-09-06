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

### Working on starcode from inside starcode

Run one instance as a systemd user service from the binary in the repo (`make service` installs it; `docs/dev-setup.md` has the details) and let the agents edit the repo underneath it. `make build` renames a fresh `./starcode` over the old one; the running process notices within five seconds and every open page gets a banner: "starcode was rebuilt. Restart to run the new version." Nothing restarts by itself. The banner says how many turns are running, and "restart now" (a `POST /api/restart`) shuts the server down and re-execs the new binary in place, same pid, same flags, same environment. Pages reconnect on their own, reload when the static assets changed, and pick up where they were: sessions resume, queued prompts start, only a turn that was mid-flight is cut off (its thread gets the usual "restarted while this turn was running" note). The build failing leaves the old binary running. `CLAUDE.md` tells an agent working in that instance not to touch the process.

`make dev` (air) is the other way round: it restarts on every save, which is right for UI work on a scratch database and wrong while threads are running.

## Threads

The title in the breadcrumb is a button: click it to rename the thread (Enter saves, Escape cancels). Agents that generate their own titles still set them on the first turn.

Archive puts a thread out of the way without deleting it: it leaves the project's list and the home cards and moves to a folded "Archived" section at the bottom of the sidebar, which opens while you search or while you are on one of its threads. Nothing else changes; the transcript stays, and a reply (or the unarchive button) moves it back. A thread archived while a turn is running stays under Running until the turn ends.

## Side panel

The panel on the right of a thread has three tabs. Changes is the working tree: `git status`, a diff per file, and an editor for any text file. Files browses the project directory, with uploads into the folder shown. PRs lists the repository's open pull requests through the GitHub CLI (`gh`, signed in), the current branch's first with its review state and check results, and a link to open one on GitHub when the branch has none. The list is cached for a minute per project; the refresh button in the tab bar reads it again. Without `gh`, or in a repository with no GitHub remote, the tab says so.

## Terminal

The terminal button (or Ctrl+`) opens a shell in the project directory under the transcript. Split opens another one beside it; the restart and close buttons act on the pane that last had focus, and closing the last pane hides the panel. Shells run on the server and outlive the page: a reload gets its panes back with their scrollback (`GET /api/term/{id}/panes` lists them), and they end when the thread is deleted or starcode stops.

## Search, commands and keys

Ctrl+K (Cmd+K on a Mac) opens the palette. Typing filters three lists at once: commands for the page you are on (new thread, rename, archive, the panels, settings, themes), threads by title, and messages by text, each with a snippet around the match. Enter runs the first row, the arrow keys walk the rest, Escape closes. A message row opens its thread scrolled to that row. The search box in the sidebar only filters the thread list; the palette is the one that reads transcripts.

Ctrl+/ lists the shortcuts. The set is decided per page by `views.Hotkeys` and rendered as hidden buttons in `#hotkeys`; the key handler in `layout.templ` clicks the one whose combo matches, so adding a shortcut is adding a row there. Inside the terminal only the keys marked for it work (toggle terminal, toggle changes, focus the prompt); the shell keeps Ctrl+K and the rest.

| key | does |
|---|---|
| Ctrl+K | search and commands |
| Ctrl+/ | keyboard shortcuts |
| Ctrl+B | toggle the sidebar |
| Alt+Up / Alt+Down | previous / next thread |
| Ctrl+Shift+O | new thread in the current project |
| Ctrl+` | toggle the terminal |
| Ctrl+Shift+G | toggle the changes panel |
| Ctrl+Shift+F | focus the prompt |
| Ctrl+, | settings |
| Enter, Ctrl+Enter | send (Shift+Enter for a new line; on a touch screen Enter breaks the line) |
| Esc | close the palette, a side panel or an open menu |

## Settings

Appearance picks the theme. Usage charts cost and tokens from the CLIs' own transcripts (see `internal/usage`), scanning every instance's config dir and showing each instance as its own series, in its tag colour when it has one. Sessions starcode itself ran are matched to their transcript by session id so nothing is counted twice.

Providers is where the agent CLIs are set up. Each row is an instance: a driver (Claude Code or Codex), a binary path, a config directory (`CLAUDE_CONFIG_DIR` or `CODEX_HOME`), extra environment variables, a display name and a colour that tags its threads in the sidebar. The built-in `claude` and `codex` instances run the binaries from the `-claude` and `-codex` flags and can be disabled but not removed; added instances get a name of their own, which is what their threads store, so a second Claude signed into another account is just another row with its own config dir. The list lives in `<data>/providers.json` (owner-readable, since the environment may hold keys). Saving an instance closes its idle sessions so the next prompt runs with the new settings; running turns finish on the old ones.

Every instance shows its version against the latest release on npm and whether it is signed in (`claude auth status --json`, `codex login status`). When a newer release exists the sidebar's Settings link carries a badge and the instance gets an "update to x.y.z" button, which runs the CLI's own updater (`claude update`, `codex update`) with that instance's environment and shows the output when it finishes. Signing in stays in a terminal: `claude /login` with the right `CLAUDE_CONFIG_DIR`, `codex login` with the right `CODEX_HOME`. An expired Claude OAuth session shows up here as "not signed in" before a turn fails on it. The Models tab lists the instance's catalog; hidden ids leave the composer's model picker and typed ids join it under "Added" (the picker's search box also takes any id as typed). Under Advanced, the check interval sets how often versions, sign-in state and catalogs are rechecked in the background (default an hour, 0 for startup only).

## Theming

Every colour and size is a CSS custom property on `:root` in `internal/web/static/app.css`. A theme is a `[data-theme="name"]` block that overrides some of them; add the name to `web.Themes` in `internal/web/server.go` to make it selectable. The choice is stored in a cookie and applied live through a Datastar signal. `dark` and `light` use the system sans-serif face; `amber` and `green` switch `--font-ui` to monospace and drop the rounded corners for a terminal look.

Driver marks (the Claude sunburst, the OpenAI flower) are inline SVGs from Simple Icons in `internal/web/views/brand.templ`; other icons are lucide.

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
internal/providers          agent instances: providers.json, adapter construction
internal/app                commands (write side) and the session pump
internal/gitx               git status/diff via exec
internal/term               pty shell sessions for the terminal panel
internal/web                router, auth, SSE read side, command endpoints
internal/web/views          templ components
internal/web/static         datastar.js (vendored v1.0.3), app.css
```
