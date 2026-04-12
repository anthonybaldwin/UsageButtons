#!/usr/bin/env bash
set -euo pipefail

# Tag and push a release.
#
# Usage:
#   ./scripts/release.sh patch   # 0.0.1 → 0.0.2
#   ./scripts/release.sh minor   # 0.0.1 → 0.1.0
#   ./scripts/release.sh major   # 0.0.1 → 1.0.0
#   ./scripts/release.sh 0.3.0   # explicit version
#
# The actual build + GitHub Release is handled by the release.yml
# workflow that triggers on the v* tag push.

MANIFEST="io.github.anthonybaldwin.UsageButtons.sdPlugin/manifest.json"

# ── Guards ─────────────────────────────────────────────────────

if [[ -n "$(git status --porcelain)" ]]; then
  echo "✗ working tree is dirty — commit or stash first"
  git status --short
  exit 1
fi

BRANCH=$(git rev-parse --abbrev-ref HEAD)
if [[ "$BRANCH" != "main" ]]; then
  echo "✗ releases must be cut from main (currently on $BRANCH)"
  exit 1
fi

git fetch origin main --quiet
LOCAL=$(git rev-parse HEAD)
REMOTE=$(git rev-parse origin/main)
if [[ "$LOCAL" != "$REMOTE" ]]; then
  echo "✗ local main is not up to date with origin — pull first"
  exit 1
fi

# ── Version resolution ─────────────────────────────────────────

# Read current version from manifest.json (format: "0.1.2.0")
CURRENT=$(sed -n 's/.*"Version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$MANIFEST")
IFS='.' read -ra PARTS <<< "$CURRENT"
MAJOR=${PARTS[0]:-0}
MINOR=${PARTS[1]:-0}
PATCH=${PARTS[2]:-0}

ARG="${1:-}"
if [[ -z "$ARG" ]]; then
  echo "Usage: ./scripts/release.sh <patch|minor|major|x.y.z>"
  exit 1
fi

case "$ARG" in
  patch) NEXT="$MAJOR.$MINOR.$((PATCH + 1))" ;;
  minor) NEXT="$MAJOR.$((MINOR + 1)).0" ;;
  major) NEXT="$((MAJOR + 1)).0.0" ;;
  [0-9]*.[0-9]*.[0-9]*) NEXT="$ARG" ;;
  *)
    echo "✗ invalid version argument: $ARG"
    echo "Usage: ./scripts/release.sh <patch|minor|major|x.y.z>"
    exit 1
    ;;
esac

TAG="v$NEXT"

# Check tag doesn't already exist
if git tag -l "$TAG" | grep -q .; then
  echo "✗ tag $TAG already exists"
  exit 1
fi

# ── Bump + tag + push ──────────────────────────────────────────

# Update manifest.json version (4-part format for Stream Deck)
if command -v jq &>/dev/null; then
  jq --arg v "${NEXT}.0" '.Version = $v' "$MANIFEST" > manifest.tmp && mv manifest.tmp "$MANIFEST"
else
  # Fallback: sed
  sed -i "s/\"Version\": \"[^\"]*\"/\"Version\": \"${NEXT}.0\"/" "$MANIFEST"
fi

# Commit the version bump, tag it, push both
git add "$MANIFEST"
git commit -m "chore(release): $NEXT"
git tag -a "$TAG" -m "Release $NEXT"
git push origin main "$TAG"

echo ""
echo "✓ $TAG pushed — release.yml will build and publish the GitHub Release"
