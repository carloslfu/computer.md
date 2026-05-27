VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "dev")
BINARY  := vibecraft-daemon
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build web clean test web-test

# build rebuilds the SPA bundle first so //go:embed all:web/dist picks
# up the latest assets, then compiles the daemon binary.
build: web
	CGO_ENABLED=1 go build -ldflags="$(LDFLAGS)" -o $(BINARY) .

web:
	cd web && npm ci && npm run build

clean:
	rm -f $(BINARY)
	rm -rf web/dist/assets
	@echo '<!doctype html><html><body><pre>rebuild required</pre></body></html>' > web/dist/index.html

test: web-test
	go test ./...

# SPA unit tests + lint (D.1.6 raw-fetch ban, Workstream G refresh
# tests). Kept in the daemon test target so `make test` is the single
# gate for the whole binary, UI included.
web-test:
	cd web && npm ci && npm run lint && npm test
