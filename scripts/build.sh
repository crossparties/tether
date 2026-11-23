#!/usr/bin/env bash
# Build Tether.
#
# Usage:
#   ./scripts/build.sh              build to ./bin/tether
#   ./scripts/build.sh --install    build, then install to ~/go/bin/tether
#   ./scripts/build.sh -o /path     build to an explicit destination
#
# Default destination is ./bin/tether, NOT ~/go/bin/tether. Installing is opt-in
# because overwriting the installed binary mid-session was a mistake made once:
# it replaced the user's working binary with an in-progress build, and the
# previous file was not recoverable (binaries are untracked by git).

set -euo pipefail

cd "$(dirname "$0")/.."

OUT="bin/tether"
INSTALL=0

while [ $# -gt 0 ]; do
  case "$1" in
    -o|--out)
      [ $# -ge 2 ] || { echo "-o needs a path" >&2; exit 2; }
      OUT="$2"; shift 2 ;;
    --install)
      INSTALL=1; shift ;;
    -h|--help)
      sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
      exit 0 ;;
    *)
      echo "unknown argument: $1 (try --help)" >&2; exit 2 ;;
  esac
done

VERSION=$(sed -n 's/^const version = "\(.*\)"/\1/p' cmd/tether/main.go)
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DIRTY=""
if [ -n "$(git status --porcelain 2>/dev/null)" ]; then
  DIRTY="-dirty"
fi

printf 'building tether %s (%s%s)\n' "$VERSION" "$COMMIT" "$DIRTY"

mkdir -p "$(dirname "$OUT")"

# -trimpath keeps the build reproducible: without it the absolute source path is
# embedded in the binary, so two builds of the same commit differ.
# CGO is off because the SQLite driver is pure Go (modernc.org/sqlite); enabling
# cgo would make the binary depend on a C toolchain at build time.
# No -X main.buildCommit: main.go declares no such variable, and the linker
# ignores -X for missing symbols without warning, so it would look wired when it
# is not. Add the variable first, then the flag.
CGO_ENABLED=0 go build \
  -trimpath \
  -ldflags "-s -w" \
  -o "$OUT" \
  ./cmd/tether

printf '✓ %s\n' "$OUT"

if [ "$INSTALL" -eq 1 ]; then
  DEST="${TETHER_INSTALL_DIR:-$HOME/go/bin}/tether"
  mkdir -p "$(dirname "$DEST")"
  cp "$OUT" "$DEST"
  printf '✓ installed %s\n' "$DEST"
fi
