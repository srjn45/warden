#!/usr/bin/env bash
# Wire git at the tracked .githooks/ directory for warden development.
# Run this once after cloning:  ./scripts/install-hooks.sh
#   (equivalent to `make install-hooks`; the service installer scripts/install.sh
#    also does this automatically.)
#
# The tracked hooks are version-controlled in .githooks/ so every clone gets the
# same gates without copying anything into .git/hooks:
#   • pre-commit — `make fmt-check lint` (gofmt + go vet), fast
#   • pre-push   — `make verify-fast`    (gofmt/vet/web/build)
#
# A relative core.hooksPath resolves per-worktree, so this works across git
# worktrees too. Bypass any hook in a pinch with --no-verify.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

cd "$REPO_ROOT"
git config core.hooksPath .githooks

echo "✅ git hooks installed: core.hooksPath -> .githooks"
echo ""
echo "Now active on commit/push:"
echo "  • pre-commit: make fmt-check lint  (gofmt + go vet)"
echo "  • pre-push:   make verify-fast     (gofmt/vet/web/build)"
echo ""
echo "Bypass in a pinch with: git commit/push --no-verify"
