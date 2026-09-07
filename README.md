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
| `-tls` | `STARCODE_TLS` | `auto` | serve HTTPS: `auto` (on for any non-loopback `-addr`), `on`, `off` |
| `-tls-cert`, `-tls-key` | `STARCODE_TLS_CERT`, `STARCODE_TLS_KEY` | | serve this certificate instead of a generated one |
| `-tls-hosts` | `STARCODE_TLS_HOSTS` | | extra names for the generated certificate, comma separated |
| `-fake` | | off | register a scripted agent for UI work without spending tokens |
| `-debug` | | off | debug logging |

From a phone on the same network:

```sh
./starcode -addr 0.0.0.0:4000 -token some-long-secret
```

Browsers get a login page asking for the token; `?token=...` on any URL also works for links. A cookie keeps you signed in for a year.

### HTTPS

Off loopback, starcode serves HTTPS. It needs no certificate from anyone: on first start it makes a certificate authority of its own in `<data>/tls/` and issues a certificate for the machine's hostname, `hostname.local`, `localhost` and every address on its interfaces (plus `-tls-hosts`). The leaf is re-issued when a host is missing or it is a month from expiry; the CA lasts ten years. Plain `http://` requests on the same port get a redirect, so old links keep working.

The first visit from a device shows a certificate warning. The login page links to `/starcode-ca.crt`; install that once (Android: Settings, Security, Install a certificate, CA certificate; iOS: open the file, install the profile, then enable full trust under Settings, General, About, Certificate Trust Settings; desktop browsers take it in their certificate settings, Firefox in its own store) and every certificate the instance issues is trusted on that device. The warning can also just be clicked through; the page is a secure context either way.

The point is not secrecy on a VPN you already trust. Browsers only give a secure context the clipboard (paste in the terminal, the copy buttons), notifications and service workers, so over plain HTTP those do not work. Behind a reverse proxy that does TLS already, run with `-tls off`; with a real certificate, pass `-tls-cert` and `-tls-key`.

`./starcode replay` rebuilds the projection tables from the event log. `./starcode compact` folds the streamed deltas in the log (one row per token while a turn runs) into one row per item; starcode does this itself after every turn and once at startup, so the command is only for a log that predates it.

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

A thread that had activity since it was last on a screen shows a dot and a bold title in the sidebar and on the project cards, and the tab title counts them, so a turn that ended while you were elsewhere is easy to find. A running turn is not unread yet; the mark appears when it ends. The mark is shared: reading a thread on the phone clears it on the desktop too (the `seen` table records the last look). A thread started from another device shows up unread.

Answering an approval with "allow for session" makes a rule for the thread (a Bash command by its first word, any other tool by name) that answers later requests like it on its own. The rules show above the composer, each with a button that revokes it; they are part of the event log, so they survive a restart.

Archive puts a thread out of the way without deleting it: it leaves the project's list and the home cards and moves to a folded "Archived" section at the bottom of the sidebar, which opens while you search or while you are on one of its threads. The transcript stays and a reply (or the unarchive button) moves it back; its terminal shells are ended, as they are on delete. A thread archived while a turn is running stays under Running until the turn ends.

### Context

Next to the setting chips the composer has a ring: how much of the model's context window this conversation takes up. Claude Code states the size of every request on the assistant message it produced and names the window in the turn's result; Codex sends both in `thread/tokenUsage/updated`. Either way the ring fills while the turn runs, not only when it ends. It turns amber at 75% and red at 90%, near where the agents start compacting on their own.

Clicking the ring opens a card with the numbers and two buttons. Compact has the agent fold the conversation so far into a summary and go on from that (`/compact` sent as a turn for Claude Code, `thread/compact/start` for Codex); it runs as a turn, so the transcript gets a note and a result row, and the ring drops as soon as the agent reports the new size. Claude's first number after a fold is low, since it counts the surviving messages and not the system prompt that rides on every request; the next message corrects it. Switch model opens the model picker, where every row shows its window (200K, 1M), so the trade is visible before it is made. Picking another model keeps the token count and forgets the old window until the next turn reports the new one, so the percentage moves as soon as the model does.

The number is the last request's total: the cached prefix, the new input and the reply that joins them. That is what the next request has to fit around, not the sum of the turns so far, which Settings > Usage keeps. An agent that never names a window shows the token count on its own.

## Side panel

The panel on the right of a thread has three tabs. Changes is the working tree: `git status`, a diff per file, and an editor for any text file. Files is the project tree: folders load their children when opened and stay open across reloads (the list of open folders is kept in sessionStorage per thread), each folder has an upload button, and hidden files are shown dimmed. PRs lists the repository's open pull requests through the GitHub CLI (`gh`, signed in), the current branch's first, each with its review state ("changes requested" spelled out) and check results, and a link to open one on GitHub when the branch has none. A row opens the PR in the panel: branch, author, size, labels, merge state, then a "Needs work" block when anything stands in the way of merging: every review that asked for changes with its text, every unresolved review comment with its file and line (the path opens the file in the editor), every failing check with its link, and merge conflicts. Below that the reviews, the checks, the resolved comments and the description fold away. The block has two buttons: "fix in a new thread" opens a thread on the project, named after the PR, with a prompt in its composer that lists every point (branch to check out, reviews, comments, failing checks) for the agent to work through; "draft it here" puts the same prompt in the current thread's composer. Neither sends anything, so the text and the agent can still be changed. The list is cached for a minute per project, a PR for a minute too; the refresh buttons read again. Without `gh`, or in a repository with no GitHub remote, the tab says so.

### Editor

Any text file up to 1 MiB opens in the panel, from the tree, the changes list or the palette. The editor is a textarea over a copy of the text that `highlight.js` (vendored, common languages, mapped from the file extension in `views.editorLang`) colours, with a line number gutter that scrolls with it. Tab indents with the file's own unit (a tab, or the space count already in use), Shift+Tab outdents, Enter keeps the indentation, Ctrl+S saves; a modified buffer shows "unsaved changes" and the browser asks before the page is left. The unfold button in the panel header widens the panel to most of the window for longer lines. Monaco and CodeMirror would need a bundler, which this project does not have, so the editor stays this one.

## Terminal

The terminal button (or Ctrl+`) opens a shell in the project directory under the transcript. Split opens another one beside it; the restart and close buttons act on the pane that last had focus, and closing the last pane hides the panel. Shells run on the server and outlive the page: a reload gets its panes back with their scrollback (`GET /api/term/{id}/panes` lists them), and they end when the thread is deleted or starcode stops.

## Search, commands and keys

Each sent prompt has a copy button and an "edit and resend" button that puts the text back in the composer; each finished reply has a copy button for its markdown. In the terminal, Ctrl+V pastes (xterm.js would otherwise send it to the shell as a control byte), and the paste button in the terminal head does the same for a phone. Copying falls back to a selection copy without a secure context; pasting has no fallback, which is one reason for HTTPS above.

What is typed in a composer is saved on the server (the `drafts` table, per thread and for the home page) on every pause in typing and dropped when sent, so a prompt started on the phone is waiting on the desktop. Unread marks work the same way: the `seen` table records when a thread was last on any screen, and a thread with activity since then gets a dot and a bold title in the sidebar, counted in the tab title. Neither is an event; they are reader state, not history.

Ctrl+K (Cmd+K on a Mac) opens the palette. Typing filters four lists at once: commands for the page you are on (new thread, rename, archive, the panels, settings, themes), threads by title, files of the current thread's project by path (`git ls-files` plus untracked files, or a bounded walk outside a repository), and messages by text, each with a snippet around the match. Enter runs the first row, the arrow keys walk the rest, Escape closes (on a phone, the close button in the search row). A message row opens its thread scrolled to that row. The search box in the sidebar only filters the thread list; the palette is the one that reads transcripts.

Ctrl+/ lists the shortcuts, and Settings > Keys changes them: press change on a row, then the new keys. The set is decided per page by `views.Hotkeys` and rendered as hidden buttons in `#hotkeys`; the key handler in `layout.templ` clicks the one whose combo matches. The actions and their defaults live in `internal/keys`, overrides in `<data>/keybindings.json`, so a change applies to every browser. A combo another action has is refused; a combo without a modifier (say, a bare Y) only fires while no text box has focus. Inside the terminal only the keys marked for it work (toggle terminal, toggle changes, focus the prompt); the shell keeps Ctrl+K and the rest. Pages already open pick up a change when they reload.

Defaults:

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
| Alt+Y / Alt+Shift+Y / Alt+N | allow, allow for session, deny the oldest waiting approval |
| Ctrl+, | settings |
| Enter, Ctrl+Enter | send (Shift+Enter for a new line; on a touch screen Enter breaks the line) |
| Esc | close the palette, a side panel or an open menu |

## Settings

Appearance picks the theme. Usage charts cost and tokens from the CLIs' own transcripts (see `internal/usage`), scanning every instance's config dir and showing each instance as its own series, in its tag colour when it has one. Sessions starcode itself ran are matched to their transcript by session id so nothing is counted twice.

Keys lists every shortcut with a change button; see above.

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
