#!/usr/bin/env bash
set -euo pipefail

# Build the plugin and restart Stream Deck.
#
# Usage:
#   ./scripts/dev.sh            # build + restart
#   ./scripts/dev.sh --no-restart  # build only

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

RESTART=true
for arg in "$@"; do
  if [[ "$arg" == "--no-restart" ]]; then
    RESTART=false
  fi
done

case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*|Windows_NT)
    BIN="io.github.anthonybaldwin.UsageButtons.sdPlugin/bin/plugin-win.exe"
    ;;
  Darwin)
    BIN="io.github.anthonybaldwin.UsageButtons.sdPlugin/bin/plugin-mac-$(uname -m)"
    ;;
  *)
    echo "✗ unsupported platform: $(uname -s)"
    exit 1
    ;;
esac

echo "→ building $BIN"
go build -o "$ROOT/$BIN" "$ROOT/cmd/plugin/"
echo "✓ built"

if $RESTART; then
  "$SCRIPT_DIR/install-dev.sh" --restart
fi
