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

LDFLAGS=()
case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*|Windows_NT)
    BIN="io.github.anthonybaldwin.UsageButtons.sdPlugin/bin/plugin-win.exe"
    NATIVE_HOST="io.github.anthonybaldwin.UsageButtons.sdPlugin/bin/usagebuttons-native-host-win.exe"
    # GUI subsystem so neither binary flashes a console window when
    # Stream Deck launches the plugin or Chrome launches the native-host.
    LDFLAGS=(-ldflags "-H=windowsgui")
    ;;
  Darwin)
    BIN="io.github.anthonybaldwin.UsageButtons.sdPlugin/bin/plugin-mac-$(uname -m)"
    NATIVE_HOST="io.github.anthonybaldwin.UsageButtons.sdPlugin/bin/usagebuttons-native-host-mac-$(uname -m | sed 's/x86_64/x64/')"
    ;;
  *)
    echo "✗ unsupported platform: $(uname -s)"
    exit 1
    ;;
esac

echo "→ building $BIN"
go build ${LDFLAGS[@]+"${LDFLAGS[@]}"} -o "$ROOT/$BIN" "$ROOT/cmd/plugin/"
echo "✓ built"

echo "→ building $NATIVE_HOST"
go build ${LDFLAGS[@]+"${LDFLAGS[@]}"} -o "$ROOT/$NATIVE_HOST" "$ROOT/cmd/native-host/"
echo "✓ built"

if $RESTART; then
  "$SCRIPT_DIR/install-dev.sh" --restart
fi
