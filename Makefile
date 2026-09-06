.PHONY: build dev generate test run clean service

BIN ?= starcode

generate:
	templ generate

build: generate
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) .

run: generate
	go run . -fake -debug

# Regenerates templ files on change and restarts the server. Needs `air`
# (go install github.com/air-verse/air@latest) and `templ`.
dev:
	templ generate --watch --cmd="air"

test: generate
	go test ./...

clean:
	rm -f $(BIN)
	find . -name '*_templ.go' -delete

# Installs (or updates) the systemd user service that runs the binary in
# this directory. See docs/dev-setup.md.
service: build
	mkdir -p $(HOME)/.config/systemd/user
	cp docs/starcode.service $(HOME)/.config/systemd/user/starcode.service
	@test -f $(HOME)/.config/starcode.env || (umask 077 && printf 'STARCODE_TOKEN=change-me-%s\n' "$$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')" > $(HOME)/.config/starcode.env && echo "wrote $(HOME)/.config/starcode.env; edit the token")
	systemctl --user daemon-reload
	systemctl --user enable --now starcode.service
	systemctl --user --no-pager status starcode.service | head -5
