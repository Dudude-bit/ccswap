VERSION ?= $(shell git describe --tags --always --dirty)
LDFLAGS = -s -w -X main.version=$(VERSION)
PREFIX ?= $(HOME)/.local

.PHONY: test vet build install generate check-gen
test:
	go test ./...
vet:
	go vet ./...
build:
	go build -ldflags "$(LDFLAGS)" -o bin/ccswap ./cmd/ccswap
# install(1) replaces the file (new inode); cp would rewrite the binary the daemon runs in place,
# and macOS kills processes started from a modified signed binary.
install: build
	mkdir -p $(PREFIX)/bin
	install -m 0755 bin/ccswap $(PREFIX)/bin/ccswap
# THIRD_PARTY_LICENSES follows go.mod: regenerate it after a dependency change.
generate:
	go generate ./...
	sh scripts/third-party-licenses.sh
check-gen: generate
	git diff --exit-code internal/api THIRD_PARTY_LICENSES
