package pipeline

import "testing"

// FuzzParseSpec exercises the pipeline YAML decoder against arbitrary input.
// ParseSpec parses untrusted user-authored YAML, so the contract under fuzzing
// is: never panic, and never return a *Pipeline together with a nil error
// unless that pipeline independently re-validates. The latter guards against a
// parse path that builds a structurally-invalid DAG (cycle, dangling dep,
// unsafe id) but forgets to run it through Validate.
func FuzzParseSpec(f *testing.F) {
	seeds := []string{
		sampleYAML,
		"name: p\nrepo: /r\njobs:\n  - id: a\n    prompt: x\n",
		"name: p\nrepo: /r\njobs:\n  - id: a\n    prompt: x\n    depends_on: [ghost]\n",
		"name: p\nrepo: /r\njobs:\n  - id: a\n    prompt: x\n    depends_on: [a]\n",
		"not: valid: yaml: [",
		"",
		"name: ../escape\nrepo: /r\njobs: []\n",
		"name: p\nrepo: /r\njobs:\n  - id: a\n    prompt: x\n    worktree: from:ghost\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := ParseSpec(data)
		if err != nil {
			if p != nil {
				t.Fatalf("ParseSpec returned a pipeline alongside an error: %+v", p)
			}
			return
		}
		if p == nil {
			t.Fatal("ParseSpec returned nil pipeline with nil error")
		}
		// A successfully parsed spec must be a valid DAG: Validate is the
		// authority, so re-running it must agree with the parse verdict.
		if err := Validate(p); err != nil {
			t.Fatalf("ParseSpec accepted a spec that Validate rejects: %v\nspec:\n%s", err, data)
		}
		// Job lookups over the parsed ids must not panic and must round-trip.
		for i := range p.Jobs {
			id := p.Jobs[i].ID
			if got := p.Job(id); got == nil {
				t.Fatalf("Job(%q) returned nil for an id present in Jobs", id)
			}
		}
	})
}
