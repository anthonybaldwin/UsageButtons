#!/usr/bin/env bash
set -euo pipefail

# Dev-install: link the repo's .sdPlugin folder into the Stream Deck
# software's plugin directory so a `go build` is picked up on the next
# Stream Deck restart without copying files around.
#
# Windows: uses a directory junction (mklink /J) — no admin needed.
# macOS:   uses a symlink (ln -s).
#
# Usage:
#   ./scripts/install-dev.sh            # link only
#   ./scripts/install-dev.sh --restart  # also kill + relaunch Stream Deck

PLUGIN_NAME="io.github.anthonybaldwin.UsageButtons.sdPlugin"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
SDPLUGIN="$ROOT/$PLUGIN_NAME"
RESTART=false

for arg in "$@"; do
  if [[ "$arg" == "--restart" ]]; then
    RESTART=true
  fi
done

# Detect platform
case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*|Windows_NT)
    PLATFORM="windows"
    APPDATA="${APPDATA:-$HOME/AppData/Roaming}"
    PLUGINS_DIR="$APPDATA/Elgato/StreamDeck/Plugins"
    ;;
  Darwin)
    PLATFORM="mac"
    PLUGINS_DIR="$HOME/Library/Application Support/com.elgato.StreamDeck/Plugins"
    ;;
  *)
    echo "✗ unsupported platform: $(uname -s)"
    exit 1
    ;;
esac

# Check binary exists
if [[ "$PLATFORM" == "windows" ]]; then
  BIN="$SDPLUGIN/bin/plugin-win.exe"
else
  BIN="$SDPLUGIN/bin/plugin-mac"
fi
if [[ ! -f "$BIN" ]]; then
  echo "✗ missing $BIN"
  echo "  run 'go build' first."
  exit 1
fi

# Stop Stream Deck if --restart
is_running() {
  if [[ "$PLATFORM" == "windows" ]]; then
    tasklist //FI "IMAGENAME eq StreamDeck.exe" 2>/dev/null | grep -qi "streamdeck.exe"
  else
    pgrep -x "Stream Deck" >/dev/null 2>&1
  fi
}

kill_sd() {
  if [[ "$PLATFORM" == "windows" ]]; then
    taskkill //F //IM StreamDeck.exe >/dev/null 2>&1 || true
  else
    pkill -x "Stream Deck" 2>/dev/null || true
  fi
}

start_sd() {
  if [[ "$PLATFORM" == "windows" ]]; then
    SD_EXE="${PROGRAMFILES:-C:/Program Files}/Elgato/StreamDeck/StreamDeck.exe"
    if [[ ! -f "$SD_EXE" ]]; then
      echo "! could not locate StreamDeck.exe — relaunch it yourself"
      return
    fi
    powershell -NoProfile -ExecutionPolicy Bypass -Command "Start-Process -FilePath '$SD_EXE'" 2>/dev/null || true
  else
    open -a "Stream Deck" 2>/dev/null || true
  fi
}

if is_running && $RESTART; then
  echo "→ stopping Stream Deck"
  kill_sd
  sleep 1
fi

# Link plugin
echo "→ linking plugin"
DEST="$PLUGINS_DIR/$PLUGIN_NAME"
mkdir -p "$PLUGINS_DIR"

# Remove existing link/dir
if [[ -e "$DEST" ]] || [[ -L "$DEST" ]]; then
  if [[ "$PLATFORM" == "windows" ]]; then
    # Junctions must be removed with rmdir, not rm -rf (which follows
    # the junction and deletes the target contents).
    WIN_RM=$(cygpath -w "$DEST" 2>/dev/null || echo "$DEST")
    cmd //c rmdir "$WIN_RM" 2>/dev/null || rm -rf "$DEST"
  else
    rm -rf "$DEST"
  fi
fi

if [[ "$PLATFORM" == "windows" ]]; then
  # Convert to Windows paths for mklink.
  # Use //J so bash doesn't eat the flag, and let the shell quote the
  # paths instead of nesting quotes inside cmd's string.
  WIN_DEST=$(cygpath -w "$DEST" 2>/dev/null || echo "$DEST")
  WIN_SRC=$(cygpath -w "$SDPLUGIN" 2>/dev/null || echo "$SDPLUGIN")
  cmd //c mklink //J "$WIN_DEST" "$WIN_SRC" >/dev/null
else
  ln -s "$SDPLUGIN" "$DEST"
  # Fix executable bits + quarantine on macOS
  for f in plugin-mac plugin-mac-arm64 plugin-mac-x64; do
    p="$SDPLUGIN/bin/$f"
    [[ -f "$p" ]] && chmod +x "$p" 2>/dev/null || true
    xattr -d com.apple.quarantine "$p" 2>/dev/null || true
  done
  xattr -cr "$SDPLUGIN" 2>/dev/null || true
fi
echo "✓ linked $DEST → $SDPLUGIN"

# Restart Stream Deck
if $RESTART; then
  echo "→ starting Stream Deck"
  start_sd
  echo "✓ done — Stream Deck will pick up the plugin on launch"
else
  echo "✓ link created. Quit + relaunch Stream Deck to load the plugin."
fi
