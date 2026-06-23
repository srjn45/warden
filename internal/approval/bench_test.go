package approval

import (
	"strings"
	"testing"
)

// realisticPane is a captured-pane-sized buffer ending in a permission prompt:
// a screenful of agent output above the option run, matching what the poller
// hands Parse on every tick.
var realisticPane = strings.Repeat("the agent is explaining its reasoning in prose\n", 40) +
	"Bash(rm -rf node_modules)\n" +
	"Do you want to proceed?\n" +
	"❯ 1. Yes\n" +
	"  2. Yes, and don't ask again for Bash commands\n" +
	"  3. No, and tell Claude what to do differently"

// BenchmarkParse measures the prompt detector on a realistic full-pane buffer.
// Parse runs against every active agent's pane on each poll tick, so its
// per-call cost multiplies by fleet size times poll frequency.
func BenchmarkParse(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := Parse(realisticPane); !ok {
			b.Fatal("benchmark pane should parse as a prompt")
		}
	}
}

// BenchmarkParseNoMatch measures the common case: a pane with no prompt, where
// Parse must scan to the bottom and bail. This is the cost paid for every
// working (non-waiting) agent on every tick.
func BenchmarkParseNoMatch(b *testing.B) {
	pane := strings.Repeat("just streaming output, no prompt here\n", 50)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := Parse(pane); ok {
			b.Fatal("benchmark pane should not parse as a prompt")
		}
	}
}
