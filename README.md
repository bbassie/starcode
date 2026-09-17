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

A thread working in a worktree of this repository builds into that worktree instead. The running instance watches those too: a binary newer than itself in any worktree of the project it lives in gets a row in the banner, named by the branch and the thread, with "restart into it". That re-execs the branch binary, which knows where home is (`STARCODE_HOME_EXE`), so an amber bar then stays up with "back to main"; the home binary's own rebuilt banner still works meanwhile. Nothing outside the banner touches the home binary, so a branch never becomes the main build without a merge and a `make build` in `~/starcode`. Several worktrees with builds get one row each.

`make dev` (air) is the other way round: it restarts on every save, which is right for UI work on a scratch database and wrong while threads are running.

## Threads

The title in the breadcrumb is a button: click it to rename the thread (Enter saves, Escape cancels). Agents that generate their own titles still set them on the first turn.

A thread that had activity since it was last on a screen shows a dot and a bold title in the sidebar and on the project cards, and the tab title counts them, so a turn that ended while you were elsewhere is easy to find. A running turn is not unread yet; the mark appears when it ends. The mark is shared: reading a thread on the phone clears it on the desktop too (the `seen` table records the last look). A thread started from another device shows up unread.

Answering an approval with "allow for session" makes a rule for the thread (a Bash command by its first word, any other tool by name) that answers later requests like it on its own. The rules show above the composer, each with a button that revokes it; they are part of the event log, so they survive a restart.

A thread's agent process stays open between turns so a follow-up goes straight in, but not forever: a session idle for thirty minutes is closed, and the next prompt starts a new process that resumes the conversation by its id. Every live `claude` shares one credentials file, and when the OAuth token runs out they all try to refresh it at once; the ones that lose that race take the sign-in down with them ("OAuth session expired and could not be refreshed"). Fewer idle processes means fewer contenders.

Pin (in the header's menu) keeps a thread at the top of its project's list, on every device; pinning is not activity, so it does not mark the thread unread. A thread whose linked pull request the poll sees merged settles on its own once its turn is over, so the list trims itself; a reply or the unsettle button brings it back.

Settle (T3 Code's word; the API still says archive) puts a thread out of the way without deleting it: it leaves the list and the home cards and moves to the folded "Settled" section at the bottom of the sidebar, ten rows at a time, which opens while you search or while you are on one of its threads. The transcript stays and a reply (or the unsettle button) moves it back; its terminal shells are ended, as they are on delete. A thread settled while a turn is running stays listed until the turn ends.

The sidebar is an inbox by default: one list with what needs a look on top, threads waiting for an approval, then running ones, then pinned, then by last activity, each row with its project's mark and name, the worktree branch when it has one, and the agent. Settings > General switches it to grouping by project. The search row has the palette, add project and a new-thread button for the project you are in. With a pointer, hovering a row swaps the age for quick actions (pin, settle; unsettle in the Settled section), and hovering the agent mark at the end of the title opens a card with the title, project, branch, model and agent, and the error when the last turn failed.

Two settings under Settings > General decide when a thread settles on its own: "Settle merged threads" is the merged pull request rule above, on by default, and "Settle inactive threads after" is a number of days (three by default, 0 to keep every thread) after which a thread with no activity leaves the list; pinned threads and threads mid-turn stay. Both apply to the whole instance, and a reply brings a settled thread back either way.

### Subagents

A Task the agent spawns (Claude Code's subagents, T3's "agents" panel) is one row in the transcript: the subagent's type and brief in the summary, its own tool calls and text folded under it, and its report at the end. It opens while it runs and its summary counts the steps so far, and the busy bar says how many agents are at work. Claude Code marks everything a subagent does with the Task's id (`parent_tool_use_id`); the items carry that as their parent, so the nesting survives reloads and streams into the right place.

### Context

Next to the setting chips the composer has a ring: how much of the model's context window this conversation takes up. Claude Code states the size of every request on the assistant message it produced and names the window in the turn's result; Codex sends both in `thread/tokenUsage/updated`. Either way the ring fills while the turn runs, not only when it ends. Until the agent has named a window, the model picker's own figure for the model stands in. It turns amber at 75% and red at 90%, near where the agents start compacting on their own.

Clicking the ring opens a card with the numbers and two buttons. Compact has the agent fold the conversation so far into a summary and go on from that (`/compact` sent as a turn for Claude Code, `thread/compact/start` for Codex); it runs as a turn, so the transcript gets a note and a result row, and the ring drops as soon as the agent reports the new size. Claude's first number after a fold is low, since it counts the surviving messages and not the system prompt that rides on every request; the next message corrects it. Switch model opens the model picker, where every row shows its window (200K, 1M), so the trade is visible before it is made. Picking another model keeps the token count and forgets the old window until the next turn reports the new one, so the percentage moves as soon as the model does.

The number is the last request's total: the cached prefix, the new input and the reply that joins them. That is what the next request has to fit around, not the sum of the turns so far, which Settings > Usage keeps. An agent that never names a window shows the token count on its own.

## Worktrees

A thread can work in a checkout of its own, so two agents on one project do not trample each other's working tree. The "worktrees" button on a project's card (home page) sets the project's default: with it on, every new thread gets `git worktree add` on a new branch (`starcode/<thread id>`) from the repository's default branch (the remote's HEAD, fetched first, or the branch checked out now), under `<data>/worktrees/<project>/`. The project sheet in the home composer has a "Start in" section that overrides the default for one thread: the project checkout, a new worktree with the branch it starts from picked from the repository's local and remote branches, or the worktree of the project's last thread that has one. The agent runs there, the terminal opens there, the side panel's changes, files and PR tab read there, and the header shows the branch with a worktree mark. "Remove worktree" in the header's menu puts the thread back on the project's checkout and removes the directory when it holds no changes; the branch stays either way. Deleting a thread removes a clean worktree and leaves a dirty one on disk. Once the thread has a title the branch is renamed after it (`starcode/fix-the-login-page`), the way T3 Code renames its throwaway branch; a branch name taken already gets a number. A `t3.json` at the project root, T3 Code's file, is honoured: the first script with `runOnWorktreeCreate` runs in the new checkout through the user's shell with `T3CODE_PROJECT_ROOT` and `T3CODE_WORKTREE_PATH` set, its last lines land in the transcript, and with `"async": false` the first turn waits for it. "New thread in this worktree" in the header's menu opens a second thread on the same checkout; a worktree two threads share is never removed with one of them. A worktree deleted outside starcode is put back on its branch before the next turn. A project that is not a git repository starts the thread on the project itself and says so in the transcript.

## Side panel

The panel on the right of a thread has three tabs. Its width scales with the window (a third of it, between 380 and 620 pixels); its left edge drags it wider or narrower on a docked window, the way the terminal's top edge does, and the unfold button in its header widens it to most of the window. Both are kept in localStorage, so the next thread opens the panel the same way; a double click on the edge, or the unfold button, puts the default width back. Changes is the working tree: `git status`, a diff per file, and an editor for any text file. Diff lines wrap rather than scroll out of view. Files is the project tree: folders load their children when opened and stay open across reloads (the list of open folders is kept in sessionStorage per thread), each folder has an upload button, and hidden files are shown dimmed. PRs lists the repository's open pull requests through the GitHub CLI (`gh`, signed in), the current branch's first, each with its review state ("changes requested" spelled out) and check results, and a link to open one on GitHub when the branch has none. A row opens the PR in the panel: branch, author, size, labels, merge state, then a "Needs work" block when anything stands in the way of merging: every review that asked for changes with its text, every unresolved review comment with its file and line (the path opens the file in the editor), every failing check with its link, and merge conflicts. Below that the reviews, the checks, the resolved comments and the description fold away. Under them a review card acts on the PR through `gh`: a comment box with comment, approve and request changes buttons, and a merge row with the method (squash, merge, rebase), a "when checks pass" switch that sets auto-merge, an update-branch button when the PR is behind its base, mark ready for a draft, and close. Nothing here deletes a branch or force pushes. The Pull requests page has the same card on its detail. The block has two buttons: "fix in a new thread" opens a thread on the project, named after the PR and linked to it (see below), with a prompt in its composer that lists every point (branch to check out, reviews, comments, failing checks) for the agent to work through; "draft it here" puts the same prompt in the current thread's composer. Neither sends anything, so the text and the agent can still be changed. The detail's head also has "link to thread", which ties the thread on screen to the PR by hand. The list is cached for a minute per project, a PR for a minute too; the refresh buttons read again. Without `gh`, or in a repository with no GitHub remote, the tab says so.

## Pull requests

A thread gets linked to the pull request it produced: when a tool call whose command contains `gh pr create` finishes and its output has the new PR's URL, the thread records the repository and number (`thread.pr_linked` in the log). A thread on a worktree branch is linked by the branch as well: after each of its turns, and with the five-minute poll for the recent ones, `gh pr view <branch>` looks for a PR with that head, so one opened on GitHub by hand finds its thread too. "Fix in a new thread" links the thread it opens, and the link button in the panel's PR detail does it by hand; the unlink button next to the chip in the thread header clears it. The link itself is not activity, so it does not mark the thread unread.

A linked thread shows a chip with the PR number on its sidebar row, on the project card and in the header: green while open, grey for a draft, purple once merged, red when closed, and red with a comment mark when a reviewer asked for changes, which is the state that wants the thread back. GitHub keeps "changes requested" until the reviewer reviews again or is dismissed, so once every reviewer who asked has a pending re-review request the chip turns amber with an eye, "re-review requested": the PR waits on them, not on you. The panel's PR detail says the same and leaves those reviews out of "Needs work" and the fix prompt. In the header the chip spells the state out and opens the panel on the PR (or the PR's page on GitHub when it lives in another repository). The state comes from a background poll: one GraphQL call for every linked PR at startup and every five minutes, plus a read of just that PR a few seconds after it is linked and after each turn on its thread, so a push shows up without waiting. What the poll found is kept in the `pr_state` table, reader state like `seen`, so chips are right straight after a restart. Without `gh` the chip shows the number alone.

The Pull requests page (above Settings in the sidebar, Alt+P) is the signed-in user's view across GitHub rather than one repository's: open PRs assigned to you, PRs waiting for your review, and PRs you opened, from one search call cached for a minute. Each row carries the comment count (conversation and review comments, as GitHub's own list counts them), the review and check badges, and a GitHub button; under it are the threads linked to that PR, and, when the repository is one of the projects here, a "new thread" button that opens a thread on it exactly like "fix in a new thread" does. A repository is a project's when any of its remotes points there, so a clone of a fork answers for the upstream. The refresh button reads the search again and polls every linked PR.

A row opens the PR in place, in the same shape as the panel's detail: state, the "Needs work" block, reviews, checks, resolved comments and the description, with the linked threads in its head. Every review that asks for changes and every review thread, open or resolved, has a button that opens a thread on just that point: the prompt names the PR and its branch, quotes only that review or that comment thread, and asks for a summary. The panel has the same per-point buttons, plus one that drafts the prompt into the current thread's composer. When the repository is not a remote of any project, a picker above the block chooses the project the thread opens in, since a checkout may live under another name.

### Editor

Any text file up to 1 MiB opens in the panel, from the tree, the changes list or the palette. The editor is a textarea over a copy of the text that `highlight.js` (vendored, common languages, mapped from the file extension in `views.editorLang`) colours, with a line number gutter that scrolls with it. Tab indents with the file's own unit (a tab, or the space count already in use), Shift+Tab outdents, Enter keeps the indentation, Ctrl+S saves; a modified buffer shows "unsaved changes" and the browser asks before the page is left. The unfold button in the panel header widens the panel to most of the window for longer lines. Monaco and CodeMirror would need a bundler, which this project does not have, so the editor stays this one.

## Terminal

The terminal button (or Ctrl+`) opens a shell in the project directory under the transcript. Split opens another one beside it; the restart and close buttons act on the pane that last had focus, and closing the last pane hides the panel. Shells run on the server and outlive the page: a reload gets its panes back with their scrollback (`GET /api/term/{id}/panes` lists them), and they end when the thread is deleted or starcode stops.

## Search, commands and keys

Each sent prompt has a copy button and an "edit and resend" button that puts the text back in the composer; each finished reply has a copy button for its markdown. Selecting part of a reply (or a PR body) and pressing Ctrl+C also copies markdown: `static/mdcopy.js` walks the selected HTML back into headings, emphasis, links, lists, fences and tables, keeps the rendered HTML in the rich-text flavor, and copies a selection inside a code block as bare code. In the terminal, Ctrl+V pastes (xterm.js would otherwise send it to the shell as a control byte), and the paste button in the terminal head does the same for a phone. Copying falls back to a selection copy without a secure context; pasting has no fallback, which is one reason for HTTPS above.

A long thread opens on its last stretch: the last forty or so items, from the prompt that started them, with a button at the top that loads the rest (`GET /api/threads/{id}/items?before=`). A link into an earlier row, from the palette or the message rail, loads it first. That bounds what a phone downloads when it opens a thread and again every time its stream reconnects after the screen was off. While a reply streams the stream appends the new text to the row instead of resending the row on every tick, with a full redraw of the markdown twice a second and one at the end; tool output only appends.

Settings > General has a notifications switch. Once allowed on a device, a thread that ends a turn or asks for an approval while starcode is not on screen posts a system notification; tapping it opens the thread. The sidebar's unread marks and the thread's status pill are what the page watches, so it works from any page and for threads started elsewhere. Notifications go through a service worker (`/sw.js`), which browsers only register in a secure context with a trusted certificate, so install the CA first (above). iOS shows them only for the app installed to the home screen.

A message sent while a turn runs waits in the queue above the composer; the first queued message has a "now" button that stops the running turn so it goes out at once, the way T3 Code steers. On a keyboard, ArrowUp in an empty composer brings back the last sent prompt, and again the one before; ArrowDown walks forward and past the newest restores what was typed.

Typing `/` at the start of an empty composer opens a menu of commands: Claude Code's own (`/compact`, `/review`, `/init` and the rest), the commands in the instance's config dir and the project's `.claude/commands`, and the skills under `.claude/skills` in both, marked as such, each with its description. Typing filters, Enter picks, an unknown `/name` still sends as typed. Codex threads get no list: its app server takes no slash commands.

What is typed in a composer is saved on the server (the `drafts` table, per thread and for the home page) on every pause in typing and dropped when sent, so a prompt started on the phone is waiting on the desktop. Unread marks work the same way: the `seen` table records when a thread was last on any screen, and a thread with activity since then gets a dot and a bold title in the sidebar, counted in the tab title. Neither is an event; they are reader state, not history.

Ctrl+K (Cmd+K on a Mac) opens the palette. Typing filters four lists at once: commands for the page you are on (new thread, rename, archive, the panels, settings, themes), threads by title, files of the current thread's project by path (`git ls-files` plus untracked files, or a bounded walk outside a repository), and messages by text, each with a snippet around the match. Enter runs the first row, the arrow keys walk the rest, Escape closes (on a phone, the close button in the search row). A message row opens its thread scrolled to that row. The search box in the sidebar only filters the thread list; the palette is the one that reads transcripts.

On a phone the panels are swipes. A swipe right pulls in the thread list and a swipe left the changes panel; with one open, the swipe the other way pushes it out. The panel follows the finger, and letting go past a third of the way (or a flick) finishes the move. The swipe has to start a little away from the screen edge, which belongs to the phone's back gesture, in a browser tab and in the app installed to the home screen (share, add to home screen) alike. A swipe on a code block that still has room to scroll that way scrolls the block instead.

On a phone the row under the prompt is a plus for attachments, the model chip and the send button, the way T3 Code lays it out; the effort and permission mode chips are two rows at the foot of the model sheet instead. The thread header keeps the title on a phone: the project name drops out, archive and delete move behind the "more" button, and the status pill opens the reason behind a status (an error, what the agent waits on) under the header when it has one. Below the transcript the terminal gets a row of keys a touch keyboard lacks: Esc, Tab, a Ctrl that holds for the next letter, ^C, ^D, ^Z, the arrows, Home and End.

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
| Alt+P | pull requests |
| Ctrl+, | settings |
| Enter, Ctrl+Enter | send (Shift+Enter for a new line; on a touch screen Enter breaks the line) |
| Esc | close the palette, a side panel or an open menu |

## Settings

General holds the thread settings (the sidebar layout, when threads settle), this device's notifications and the phone pairing link. Appearance picks the theme. Usage charts cost and tokens from the CLIs' own transcripts (see `internal/usage`), scanning every instance's config dir and showing each instance as its own series, in its tag colour when it has one. Sessions starcode itself ran are matched to their transcript by session id so nothing is counted twice.

Above the chart, Plan limits shows what each instance's subscription has used: the 5-hour session window and the weekly one (and the model-scoped weekly, "Weekly · Fable", where the plan has it), each with its percentage and when it resets. Claude Code answers a `get_usage` request over the same control channel the model list comes from, Codex its app server's `account/rateLimits/read`; both cost a process launch and no tokens. The read runs with the provider check in the background and on the section's refresh button, and the windows an agent streams during a turn (Claude's `rate_limit_event`, Codex's `account/rateLimits/updated`) move the bars as it works. The same bars sit at the bottom of the composer's context card, so "can I start another task before the reset" has an answer next to the prompt.

Keys lists every shortcut with a change button; see above.

Providers is where the agent CLIs are set up. Each row is an instance: a driver (Claude Code or Codex), a binary path, a config directory (`CLAUDE_CONFIG_DIR` or `CODEX_HOME`), extra environment variables, a display name and a colour that tags its threads in the sidebar. The built-in `claude` and `codex` instances run the binaries from the `-claude` and `-codex` flags and can be disabled but not removed; added instances get a name of their own, which is what their threads store, so a second Claude signed into another account is just another row with its own config dir. The list lives in `<data>/providers.json` (owner-readable, since the environment may hold keys). Saving an instance closes its idle sessions so the next prompt runs with the new settings; running turns finish on the old ones.

Every instance shows its version against the latest release on npm and whether it is signed in (`claude auth status --json`, `codex login status`). When a newer release exists the sidebar's Settings link carries a badge and the instance gets an "update to x.y.z" button, which runs the CLI's own updater (`claude update`, `codex update`) with that instance's environment and shows the output when it finishes. A Claude instance signs in from this page: the sign-in button runs `claude auth login` without a terminal, which prints a link instead of opening a browser; open it on any device, sign in, and paste the code it shows back into the page. Codex still signs in from a terminal (`codex login` with the right `CODEX_HOME`), since its flow needs a browser on the same machine. An expired Claude OAuth session shows up here as "not signed in", and the sidebar's Settings badge counts it; a thread whose turn failed on it gets a sign-in link next to its status. Claude Code loses its session about once a day when several of its processes refresh the same token at the same moment (anthropics/claude-code issues 48786 and 54443), and starcode runs several, so expect the button to see use until that is fixed upstream. The Models tab lists the instance's catalog; hidden ids leave the composer's model picker and typed ids join it under "Added" (the picker's search box also takes any id as typed). Under Advanced, the check interval sets how often versions, sign-in state and catalogs are rechecked in the background (default an hour, 0 for startup only).

## Pairing a phone

With a token set, Settings > General shows a QR code of the sign-in link (the instance's address with the token in the query, `/?token=...`); scanning it on the phone signs it in with nothing to type. The address is the one the browser used to open Settings, or the machine's first private IPv4 when that was localhost. Anyone with the link is signed in, so it stays on that page.

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
