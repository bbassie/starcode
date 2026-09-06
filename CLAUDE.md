# starcode

Go + templ + Datastar web console for coding agents. No Node, no bundler.
`README.md` explains the architecture; `docs/dev-setup.md` the service
setup. Read both before changing anything structural.

## You are probably running inside it

On this machine starcode runs as a systemd user service from
`~/starcode/starcode`, and threads on this repo are opened in that same
instance. So:

- Never kill, stop or restart the `starcode` process or unit. The user
  restarts through the banner when they choose; a restart cuts off every
  running turn, including yours.
- To ship a change, run `make build`. That is all. The running instance
  sees the new binary and offers the restart.
- End every task with a build. When you are done, or when you hand over
  to the user to try something, the last command you run is `make build`
  (after `go vet ./...` and `go test -short ./...`), so the restart the
  user presses next brings up exactly the code you described. If the
  build fails, fix it before finishing; a finished task with a broken
  build is not finished.
- Say in your summary that a restart is waiting and what changes with it.
  Do not trigger the restart yourself, not even as the last step: it ends
  the turn that is running, which is yours, and your summary would be
  lost with it.
- Do not run `make dev` or `go run` against `~/.starcode`. For a scratch
  instance use another port and data dir:
  `STARCODE_DATA=/tmp/sc ./starcode -addr 127.0.0.1:4777 -fake`.

## Build and test

```sh
make build            # templ generate + static binary
go test -short ./...  # skips the integration tests that call claude and codex
go vet ./...
```

Templates compile with `templ generate` (part of `make build`). The
generated `*_templ.go` files are committed; regenerate after every `.templ`
edit and commit both.

## templ rules

- Never run `templ fmt`. It reflows `@Icon("x", "ui-icon") label` across
  lines and the generated HTML loses the space between icon and label.
- Never write `@Icon(...) { expr }`. templ reads the braces as a children
  block and the expression renders as nothing. Write
  `@Icon(...) <span>{ expr }</span>`. After generating, this grep must be
  empty: `grep -n 'WriteString([^,]*, [0-9]*, "[a-zA-Z_.]*(' internal/web/views/*_templ.go`
- Datastar attribute names are lowercased by the HTML parser: bind with
  kebab-case (`data-bind:pv-label`) and read the camelCase signal
  (`pvLabel`).
- Elements whose visibility follows a signal are rendered in their
  initial state on the server (`style="display: none"`, the `active` or
  `sel` class) with `data-preserve-attr="style"`, so pages paint without
  a jump. Keep that up for new ones.

## Conventions

- Every user action is a `POST /api/...`; every page has one SSE stream.
  New state goes through `internal/app` commands and domain events, never
  straight into the store from a handler.
- Comments and README prose follow the user's writing rules: plain words,
  no em dashes, say what the code does and why, not how it feels.
