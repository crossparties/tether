#!/usr/bin/env bash
# Build and verify Tether.
#
# Why this exists: verification used to be ad-hoc `go build` commands typed by
# hand, and one of them ran `go build -o ~/go/bin/tether` and overwrote the
# installed binary mid-session. A script makes the destinations explicit and
# keeps CI-shaped checks separate from installing.
#
# Usage:
#   ./scripts/verify.sh            check only (default: never writes outside ./bin)
#   ./scripts/verify.sh --install  also install to $TETHER_INSTALL_DIR (default ~/go/bin)
#
# The default mode deliberately does not touch ~/go/bin. Installing is opt-in
# and always says where the binary went.

set -euo pipefail

cd "$(dirname "$0")/.."

INSTALL=0
for arg in "$@"; do
  case "$arg" in
    --install) INSTALL=1 ;;
    -h|--help)
      sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "unknown argument: $arg (try --help)" >&2
      exit 2
      ;;
  esac
done

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; }
fail() { printf '  \033[31m✗\033[0m %s\n' "$*"; exit 1; }

step "gofmt"
# Only report files that are not already clean. Pre-existing unformatted files
# are listed, not fixed: reformatting unrelated files buries the real diff.
unformatted=$(gofmt -l ./cmd ./internal 2>/dev/null || true)
if [ -n "$unformatted" ]; then
  fail "these files need gofmt:
$unformatted"
fi
ok "all Go files formatted"

step "go vet"
go vet ./... || fail "go vet reported problems"
ok "vet clean"

step "build"
OUT_DIR="bin"
mkdir -p "$OUT_DIR"
go build -o "$OUT_DIR/tether" ./cmd/tether || fail "build failed"
ok "built $OUT_DIR/tether"

step "test"
go test ./... || fail "tests failed"
ok "all tests passed"

if [ "$INSTALL" -eq 1 ]; then
  step "install"
  DEST="${TETHER_INSTALL_DIR:-$HOME/go/bin}"
  mkdir -p "$DEST"
  cp "$OUT_DIR/tether" "$DEST/tether" || fail "install failed"
  ok "installed to $DEST/tether"
else
  printf '\n  (skipped install; run with --install to copy to ~/go/bin)\n'
fi

step "done"
