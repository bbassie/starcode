# Running starcode as a service, and developing it from inside itself

This is the setup on a machine where starcode is used every day and also
worked on, through its own threads. One instance runs as a systemd user
service from the binary in the repo. Rebuilding replaces that binary; the
running instance notices, shows a banner on every open page, and you press
"restart now" when no turn you care about is running.

## Install

```sh
git clone <repo> ~/starcode
cd ~/starcode
make service
```

`make service` does four things:

1. `make build`, which produces `~/starcode/starcode`.
2. Copies `docs/starcode.service` to `~/.config/systemd/user/starcode.service`.
3. Creates `~/.config/starcode.env` with a placeholder `STARCODE_TOKEN` if the file does not exist yet (mode 600). Edit it before exposing the port.
4. `systemctl --user daemon-reload`, then `enable --now starcode`.

Then, once:

```sh
loginctl enable-linger $USER   # keep user services running after logout
```

Open `https://<host>:4000` (plain `http://` redirects there), accept the
certificate warning once, paste the token from `~/.config/starcode.env`. A
cookie keeps you signed in for a year. To lose the warning, install
`https://<host>:4000/starcode-ca.crt` on the device; the README's HTTPS
section says how per platform. If you reach the machine by a name it does
not know about itself (a VPN DNS name), add `STARCODE_TLS_HOSTS=that.name`
to the env file, or the certificate will not match.

The unit file (also in `docs/starcode.service`):

```ini
[Unit]
Description=starcode
After=network.target

[Service]
ExecStart=%h/starcode/starcode -addr 0.0.0.0:4000
WorkingDirectory=%h/starcode
EnvironmentFile=%h/.config/starcode.env
Environment=PATH=%h/.local/bin:%h/.local/share/fnm/aliases/default/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin
Environment=SHELL=/bin/bash
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
```

Things in it worth knowing:

- `PATH` must contain `claude` and `codex` (here `~/.local/bin`) and `go`
  (so a thread on the starcode repo can run `make build`). The fnm entry
  is for npm-installed tools; drop it if you do not use fnm.
- `SHELL` is what the terminal panel starts. Without it, user services get
  no shell variable and you would land in `sh`.
- `-addr 0.0.0.0:4000` listens on every interface, which is why the token
  is required and why HTTPS is on (`-tls auto`): browsers only allow the
  clipboard and notifications on a secure origin. The certificate comes
  from a CA starcode keeps in `~/.starcode/tls/`. On anything public put a
  real certificate in front, or pass one with `-tls-cert` and `-tls-key`.
- Data lives in `~/.starcode` (`starcode.db`, `providers.json`,
  `attachments/`, caches). Change it with `STARCODE_DATA` in the env file.

## Day to day

```sh
systemctl --user status starcode      # is it up
journalctl --user -u starcode -f      # live log
systemctl --user restart starcode     # hard restart, if the banner ever fails
```

Rebuild and restart, from a thread or a terminal:

```sh
make build
```

Within five seconds every open page shows "starcode was rebuilt. Restart to
run the new version." with a count of running turns. "restart now" posts
`/api/restart`; the server drains, re-execs the new binary in place (same
pid, so systemd sees nothing), and pages reconnect within a second or two.
Threads survive: sessions resume, queued prompts start. A turn that was
running at that moment is cut off and its thread gets a note. A failed
build leaves the old binary in place, so nothing happens.

`make dev` (air) is the opposite tool: it restarts on every save, right for
UI work against a scratch `STARCODE_DATA`, wrong for the instance you are
working in.

## Update the CLIs

Settings > Providers shows each CLI's version against the latest release
and has an update button that runs `claude update` or `codex update`. A
badge on the sidebar's Settings link tells you when one is waiting. Signing
in stays in a terminal (`claude /login`, `codex login`), with the
instance's `CLAUDE_CONFIG_DIR` or `CODEX_HOME` set if it has its own.

## Uninstall

```sh
systemctl --user disable --now starcode
rm ~/.config/systemd/user/starcode.service ~/.config/starcode.env
rm -r ~/.starcode      # only if you also want the threads gone
```
