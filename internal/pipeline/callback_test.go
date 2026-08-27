package pipeline

import "testing"

// A delegation plan authors notify_owner (pipeline-level) and per-job callback in
// the YAML spec; both must survive ParseSpec so the executor can honor them.
func TestParseSpecDelegatedMonitoringFields(t *testing.T) {
	spec := []byte(`
name: deleg
repo: /r
notify_owner: true
jobs:
  - id: build
    prompt: build it
  - id: decide
    prompt: pick an approach
    depends_on: [build]
    callback: true
`)
	p, err := ParseSpec(spec)
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if !p.NotifyOwner {
		t.Fatalf("notify_owner should parse to true")
	}
	if j := p.Job("build"); j == nil || j.Callback {
		t.Fatalf("build should not be a callback point: %+v", j)
	}
	if j := p.Job("decide"); j == nil || !j.Callback {
		t.Fatalf("decide should be a callback point: %+v", j)
	}
}

// A plain (non-delegated) spec leaves both fields at their zero value: warden wakes
// no one and the DAG behaves exactly as before A4.
func TestParseSpecDefaultsNoDelegation(t *testing.T) {
	p, err := ParseSpec([]byte("name: plain\nrepo: /r\njobs:\n  - id: a\n    prompt: go\n"))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if p.NotifyOwner {
		t.Fatalf("notify_owner should default false")
	}
	if p.Job("a").Callback {
		t.Fatalf("callback should default false")
	}
}
