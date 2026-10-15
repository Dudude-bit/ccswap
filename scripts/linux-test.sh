#!/bin/sh
# Module tests and a smoke check in a Linux container. The repo is mounted read-only; the tests run
# as non-root, since some check that a write is refused and root bypasses that.
set -eu
cd "$(dirname "$0")/.."
# Same Go as go.mod pins: the toolchain line, if any, else the go line.
gover=$(awk '/^toolchain go/{sub("go","",$2); print $2; exit}' go.mod)
[ -n "$gover" ] || gover=$(awk '/^go /{print $2; exit}' go.mod)
docker volume create ccswap-gomod >/dev/null
docker run --rm -v ccswap-gomod:/gomod alpine chown -R 1000:1000 /gomod
# The page script against the real texts, in its own image: the Go image has no node.
docker run --rm --network none --cpus=2 --memory=1g --user 1000:1000 -v "$PWD":/src:ro -w /src node:24-bookworm-slim \
  node --test --test-timeout=30000 internal/web/i18n.test.js
exec docker run --rm --cpus=2 --memory=3g --user 1000:1000 \
  -e HOME=/tmp/home -e GOCACHE=/tmp/gocache -e GOMODCACHE=/gomod -e GOFLAGS=-mod=mod \
  --tmpfs /tmp:exec,mode=1777 -v ccswap-gomod:/gomod \
  -v "$PWD":/src:ro -w /src golang:"$gover" sh -c '
set -eu
go vet ./...
go test -p 2 ./...
go build -o /tmp/ccswap ./cmd/ccswap
export HOME=$(mktemp -d) CCSWAP_HOME=$(mktemp -d)
/tmp/ccswap daemon run >"$HOME/daemon.out" 2>&1 &
pid=$!
trap "kill $pid 2>/dev/null || true" EXIT
sleep 2
/tmp/ccswap status >"$HOME/status.out"
LANG=ru_RU.UTF-8 /tmp/ccswap status >>"$HOME/status.out"
cat "$HOME/status.out"
grep -qF "Claude Code credentials file: $HOME/.claude/.credentials.json" "$HOME/status.out"
grep -qF "Файл входа Claude Code: $HOME/.claude/.credentials.json" "$HOME/status.out"
/tmp/ccswap doctor >"$HOME/doctor.out" 2>&1 || true
LANG=ru_RU.UTF-8 /tmp/ccswap doctor >>"$HOME/doctor.out" 2>&1 || true
cat "$HOME/doctor.out"
grep -qF "Claude Code credentials file \"$HOME/.claude/.credentials.json\": missing" "$HOME/doctor.out"
grep -qF "✗ systemd not found: run ccswap daemon run under your own supervisor" "$HOME/doctor.out"
grep -qF "файл входа Claude Code «$HOME/.claude/.credentials.json»: нет" "$HOME/doctor.out"
grep -qF "✗ systemd не найден: запускайте ccswap daemon run под своим супервизором" "$HOME/doctor.out"
kill $pid
echo SMOKE_OK
'
