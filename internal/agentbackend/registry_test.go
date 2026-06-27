package agentbackend

import (
	"io"
	"testing"
)

// stub is a minimal Backend for exercising the registry. Only ID matters here;
// the rest satisfy the interface.
type stub struct{ id string }

func (s stub) ID() string                                 { return s.id }
func (stub) DisplayName() string                          { return "Stub" }
func (stub) Binary() string                               { return "stub" }
func (stub) InstallHint() string                          { return "" }
func (stub) LaunchCmd(LaunchOpts) string                  { return "" }
func (stub) ResumeCmd(ResumeOpts) (string, bool)          { return "", false }
func (stub) HeadlessCmd(string) ([]string, bool)          { return nil, false }
func (stub) TranscriptPath(_, _, _ string) (string, bool) { return "", false }
func (stub) ParseTranscript(io.Reader) ([]Turn, error)    { return nil, nil }
func (stub) DetectState(string) State                     { return StateUnknown }
func (stub) ParseApproval(string) (*Approval, bool)       { return nil, false }
func (stub) SystemPromptFlag(string) (string, bool)       { return "", false }
func (stub) Pricing() (PricingTable, bool)                { return PricingTable{}, false }
func (stub) Capabilities() Caps                           { return Caps{} }

func TestRegisterAndGet(t *testing.T) {
	Register(stub{id: "stubby"})
	got, err := Get("stubby")
	if err != nil {
		t.Fatalf("Get(stubby): %v", err)
	}
	if got.ID() != "stubby" {
		t.Errorf("got id %q, want stubby", got.ID())
	}
}

func TestGetUnknownErrors(t *testing.T) {
	if _, err := Get("definitely-not-registered"); err == nil {
		t.Error("Get(unknown) = nil error, want error")
	}
}

func TestGetEmptyResolvesDefault(t *testing.T) {
	Register(stub{id: DefaultID}) // stand in for the real Claude backend
	got, err := Get("")
	if err != nil {
		t.Fatalf("Get(\"\"): %v", err)
	}
	if got.ID() != DefaultID {
		t.Errorf("Get(\"\") id = %q, want %q", got.ID(), DefaultID)
	}
}

func TestDefaultReturnsRegisteredDefault(t *testing.T) {
	Register(stub{id: DefaultID})
	if d := Default(); d == nil || d.ID() != DefaultID {
		t.Errorf("Default() = %v, want backend with id %q", d, DefaultID)
	}
}
