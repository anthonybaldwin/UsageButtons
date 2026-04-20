#!/usr/bin/env bash
# Install git hooks for UsageButtons.
#
# Points git at the checked-in .githooks/ directory so the repo's
# pre-commit checks run on every commit. Idempotent.
#
# Run this once after cloning:
#     ./scripts/install-hooks.sh
#
# Works on Windows (Git Bash) and macOS. No external dependencies
# beyond a POSIX shell.

set -euo pipefail

cd "$(dirname "$0")/.."

if [[ ! -d .githooks ]]; then
    echo "error: .githooks/ directory missing — run from a repo clone." >&2
    exit 1
fi

# `core.hooksPath` is git >= 2.9. Every supported dev platform ships a
# newer git than that, so we don't fall back to symlinks.
git config core.hooksPath .githooks

# Ensure the hook scripts are executable (Windows git clones leave the
# +x bit off even when the file has a shebang).
chmod +x .githooks/* 2>/dev/null || true

echo "Installed: git config core.hooksPath = .githooks"
echo
echo "Pre-commit will run: go vet, golangci-lint, go build."
echo
echo "If golangci-lint is missing, install it with:"
echo "    go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.1.6"
