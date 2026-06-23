package pipeline

import (
	"fmt"
	"strings"
	"testing"
)

// bigSpecYAML builds a valid n-job pipeline where each job depends on the
// previous one — a deep chain that exercises the DAG walk and cycle detection
// at scale.
func bigSpecYAML(n int) []byte {
	var b strings.Builder
	b.WriteString("name: big\nrepo: /repo\njobs:\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "  - id: job%d\n    prompt: \"do step %d\"\n    worktree: none\n", i, i)
		if i > 0 {
			fmt.Fprintf(&b, "    depends_on: [job%d]\n", i-1)
		}
	}
	return []byte(b.String())
}

// BenchmarkParseSpec measures the full parse path (YAML decode + default
// application + Validate) for a large spec.
func BenchmarkParseSpec(b *testing.B) {
	data := bigSpecYAML(100)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ParseSpec(data); err != nil {
			b.Fatalf("ParseSpec: %v", err)
		}
	}
}

// BenchmarkValidate isolates DAG validation (id checks, dep resolution, cycle
// detection) from YAML decoding, at a few graph sizes.
func BenchmarkValidate(b *testing.B) {
	for _, n := range []int{10, 100, 500} {
		p, err := ParseSpec(bigSpecYAML(n))
		if err != nil {
			b.Fatalf("setup ParseSpec(%d): %v", n, err)
		}
		b.Run(fmt.Sprintf("jobs=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := Validate(p); err != nil {
					b.Fatalf("Validate: %v", err)
				}
			}
		})
	}
}
