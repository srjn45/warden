package lifecycle

import (
	"context"
	"errors"
	"testing"

	"github.com/srjn45/warden/internal/pressure"
)

// prFakeRunner returns a canned output/error for any command.
type prFakeRunner struct {
	out string
	err error
}

func (f prFakeRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	return f.out, f.err
}

func TestMemoryPressureParsesLevel(t *testing.T) {
	l := New(prFakeRunner{out: "2\n"})
	got, err := l.MemoryPressure(context.Background())
	if err != nil || got != pressure.Warn {
		t.Fatalf("MemoryPressure = (%v,%v), want (warn,nil)", got, err)
	}
}

func TestMemoryPressureDegradesOnError(t *testing.T) {
	// Non-macOS / sysctl missing: command errors → degrade to Normal, no error.
	l := New(prFakeRunner{err: errors.New("exec: sysctl not found")})
	got, err := l.MemoryPressure(context.Background())
	if err != nil || got != pressure.Normal {
		t.Fatalf("MemoryPressure on exec error = (%v,%v), want (normal,nil)", got, err)
	}
}

func TestMemoryPressureDegradesOnGarbage(t *testing.T) {
	l := New(prFakeRunner{out: "not-a-number"})
	got, err := l.MemoryPressure(context.Background())
	if err != nil || got != pressure.Normal {
		t.Fatalf("MemoryPressure on garbage = (%v,%v), want (normal,nil)", got, err)
	}
}
