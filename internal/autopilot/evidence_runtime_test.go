package autopilot

import "testing"

// EvidenceRuntime is optional: a Runtime that does not implement it is simply not
// evidence-capable, leaving the guardian untouched.
func TestEvidenceRuntimeIsOptional(t *testing.T) {
	var rt Runtime = struct{ Runtime }{}
	if _, ok := rt.(EvidenceRuntime); ok {
		t.Fatal("bare runtime must not satisfy EvidenceRuntime")
	}
}
