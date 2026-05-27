VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "dev")
BINARY  := vibecraft
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build clean test sync-docs docs-check

build: sync-docs
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o $(BINARY) .

# The repo-root llms.txt is the single source of truth. cmd/llms_embed.txt
# is a build-time copy go:embed'ed into the binary. Keeping the embed file
# in cmd/ (not at repo root) is a Go constraint — go:embed cannot reach
# above the package directory.
sync-docs:
	@cp ../llms.txt cmd/llms_embed.txt

# CI guard (F12): verify every cobra Use/--flag in cmd/ appears in
# llms.txt at least once. Catches "added a verb, forgot the doc."
docs-check: sync-docs
	@go run ./internal/docscheck

clean:
	rm -f $(BINARY)

test: sync-docs
	go test ./...
