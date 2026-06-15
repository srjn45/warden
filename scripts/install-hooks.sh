#!/bin/bash
# Install git hooks for warden development
# Run this script after cloning the repository: ./scripts/install-hooks.sh

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
HOOKS_DIR="$REPO_ROOT/.git/hooks"

echo "🔧 Installing git hooks for warden..."

# Create pre-commit hook
cat > "$HOOKS_DIR/pre-commit" << 'EOF'
#!/bin/bash
# Pre-commit hook for warden
# Runs gofmt, go vet, and go test to ensure code quality

set -e

echo "🔍 Running pre-commit checks..."

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Track if any check fails
FAILED=0

# 1. Check gofmt
echo ""
echo "📝 Checking code formatting (gofmt)..."
UNFORMATTED=$(gofmt -l $(git ls-files '*.go' 2>/dev/null || find . -name '*.go' | grep -v vendor))
if [ -n "$UNFORMATTED" ]; then
    echo -e "${RED}✗ These files are not gofmt-clean:${NC}"
    echo "$UNFORMATTED"
    echo ""
    echo -e "${YELLOW}💡 Run: gofmt -w \$(git ls-files '*.go')${NC}"
    FAILED=1
else
    echo -e "${GREEN}✓ All files are properly formatted${NC}"
fi

# 2. Run go vet
echo ""
echo "🔎 Running go vet..."
if go vet ./... 2>&1 | tee /tmp/go-vet.log; then
    echo -e "${GREEN}✓ go vet passed${NC}"
else
    echo -e "${RED}✗ go vet found issues${NC}"
    cat /tmp/go-vet.log
    FAILED=1
fi

# 3. Run tests
echo ""
echo "🧪 Running tests..."
if go test ./... -short 2>&1 | tee /tmp/go-test.log | tail -20; then
    TEST_COUNT=$(grep -c "^ok" /tmp/go-test.log || echo "0")
    echo -e "${GREEN}✓ All tests passed ($TEST_COUNT packages)${NC}"
else
    echo -e "${RED}✗ Tests failed${NC}"
    echo ""
    echo "Failed tests:"
    grep "FAIL" /tmp/go-test.log || true
    FAILED=1
fi

# 4. Check for common issues
echo ""
echo "🔍 Checking for common issues..."

# Check for println/printf statements (potential debugging code)
PRINTLNS=$(git diff --cached --name-only | grep '\.go$' | xargs grep -n "fmt.Println\|fmt.Printf" 2>/dev/null | grep -v "// OK:" || true)
if [ -n "$PRINTLNS" ]; then
    echo -e "${YELLOW}⚠️  Found fmt.Println/Printf statements (might be debugging code):${NC}"
    echo "$PRINTLNS"
    echo -e "${YELLOW}   Add '// OK: reason' comment if intentional${NC}"
fi

# Check for TODO/FIXME comments in staged files
TODOS=$(git diff --cached --name-only | grep '\.go$' | xargs grep -n "TODO\|FIXME" 2>/dev/null || true)
if [ -n "$TODOS" ]; then
    echo -e "${YELLOW}ℹ️  Found TODO/FIXME comments in staged files:${NC}"
    echo "$TODOS"
fi

# Summary
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
if [ $FAILED -eq 1 ]; then
    echo -e "${RED}✗ Pre-commit checks FAILED${NC}"
    echo ""
    echo "Fix the issues above and try committing again."
    echo "To bypass this hook (not recommended): git commit --no-verify"
    exit 1
else
    echo -e "${GREEN}✓ All pre-commit checks PASSED${NC}"
    echo ""
    echo "Ready to commit!"
fi
EOF

# Make hook executable
chmod +x "$HOOKS_DIR/pre-commit"

echo "✅ Git hooks installed successfully!"
echo ""
echo "The pre-commit hook will now run:"
echo "  • gofmt check"
echo "  • go vet"
echo "  • go test"
echo ""
echo "To bypass the hook (not recommended): git commit --no-verify"
