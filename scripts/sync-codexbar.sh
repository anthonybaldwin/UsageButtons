#!/usr/bin/env bash
# Refresh tmp/CodexBar from upstream. The repo is gitignored and used
# as read-only reference material for porting provider concepts into
# this plugin. Do NOT copy CodexBar code into src/.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEST="$ROOT/tmp/CodexBar"
UPSTREAM="https://github.com/steipete/CodexBar.git"

mkdir -p "$ROOT/tmp"

if [ -d "$DEST/.git" ]; then
  echo "→ pulling CodexBar into $DEST"
  git -C "$DEST" fetch --depth 50 origin
  git -C "$DEST" reset --hard origin/HEAD
else
  echo "→ cloning CodexBar into $DEST"
  git clone --depth 50 "$UPSTREAM" "$DEST"
fi

REV=$(git -C "$DEST" rev-parse --short HEAD)
DATE=$(git -C "$DEST" log -1 --format=%cd --date=short)
echo "✓ CodexBar synced @ $REV ($DATE)"
