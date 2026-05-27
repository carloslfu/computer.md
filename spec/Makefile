# SPDX-License-Identifier: Apache-2.0
#
# spec/ — the COMPUTER.md spec, reference parser, and CLI.
#
# Canonical examples live in spec/examples/. The CLI binary
# (spec/cmd/computer-md/) needs to embed them, and Go's `//go:embed`
# directive cannot reach into a sibling directory, so we sync them
# into spec/cmd/computer-md/examples/ at build time.

.PHONY: build sync-examples test fmt vet clean

EXAMPLES_SRC := examples
EXAMPLES_DST := cmd/computer-md/examples

build: sync-examples
	go build -o computer-md ./cmd/computer-md

sync-examples:
	@mkdir -p $(EXAMPLES_DST)
	@rsync -a --delete $(EXAMPLES_SRC)/ $(EXAMPLES_DST)/

test: sync-examples
	go test ./...

fmt:
	go fmt ./...

vet: sync-examples
	go vet ./...

clean:
	rm -f computer-md
	rm -rf $(EXAMPLES_DST)
