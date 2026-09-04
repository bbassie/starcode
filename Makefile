.PHONY: build dev generate test run clean

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
