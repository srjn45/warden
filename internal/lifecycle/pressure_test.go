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

// darwinLifecycle pins the platform seams to the sysctl path regardless of the
// host the tests run on.
func darwinLifecycle(r Runner) *Lifecycle {
	l := New(r, &FakeConfig{})
	l.goos = "darwin"
	return l
}

// linuxLifecycle pins the platform seams to the PSI path with an injected read.
func linuxLifecycle(psi string, err error) *Lifecycle {
	l := New(prFakeRunner{err: errors.New("sysctl must not be called on linux")}, &FakeConfig{})
	l.goos = "linux"
	l.readPSI = func() (string, error) { return psi, err }
	return l
}

func TestMemoryPressureParsesLevel(t *testing.T) {
	l := darwinLifecycle(prFakeRunner{out: "2\n"})
	got, err := l.MemoryPressure(context.Background())
	if err != nil || got != pressure.Warn {
		t.Fatalf("MemoryPressure = (%v,%v), want (warn,nil)", got, err)
	}
}

func TestMemoryPressureDegradesOnError(t *testing.T) {
	// sysctl missing: command errors → degrade to Normal, no error.
	l := darwinLifecycle(prFakeRunner{err: errors.New("exec: sysctl not found")})
	got, err := l.MemoryPressure(context.Background())
	if err != nil || got != pressure.Normal {
		t.Fatalf("MemoryPressure on exec error = (%v,%v), want (normal,nil)", got, err)
	}
}

func TestMemoryPressureDegradesOnGarbage(t *testing.T) {
	l := darwinLifecycle(prFakeRunner{out: "not-a-number"})
	got, err := l.MemoryPressure(context.Background())
	if err != nil || got != pressure.Normal {
		t.Fatalf("MemoryPressure on garbage = (%v,%v), want (normal,nil)", got, err)
	}
}

func TestMemoryPressureLinuxPSI(t *testing.T) {
	cases := []struct {
		name string
		psi  string
		want pressure.Level
	}{
		{"quiet system", "some avg10=0.00 avg60=0.00 avg300=0.00 total=0\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n", pressure.Normal},
		{"some stalls elevated", "some avg10=31.50 avg60=12.00 avg300=3.00 total=900000\nfull avg10=2.10 avg60=0.80 avg300=0.10 total=90000\n", pressure.Warn},
		{"thrashing", "some avg10=88.00 avg60=70.00 avg300=40.00 total=9000000\nfull avg10=45.30 avg60=30.00 avg300=12.00 total=4000000\n", pressure.Critical},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := linuxLifecycle(tc.psi, nil)
			got, err := l.MemoryPressure(context.Background())
			if err != nil || got != tc.want {
				t.Fatalf("MemoryPressure = (%v,%v), want (%v,nil)", got, err, tc.want)
			}
		})
	}
}

func TestMemoryPressureLinuxDegrades(t *testing.T) {
	// PSI read error (file missing / psi=0 kernel) → Normal, no error, and the
	// runner (sysctl) is never consulted on linux.
	l := linuxLifecycle("", errors.New("open /proc/pressure/memory: operation not supported"))
	got, err := l.MemoryPressure(context.Background())
	if err != nil || got != pressure.Normal {
		t.Fatalf("MemoryPressure on PSI error = (%v,%v), want (normal,nil)", got, err)
	}

	// Unparseable PSI content degrades the same way.
	l = linuxLifecycle("garbage content\n", nil)
	got, err = l.MemoryPressure(context.Background())
	if err != nil || got != pressure.Normal {
		t.Fatalf("MemoryPressure on PSI garbage = (%v,%v), want (normal,nil)", got, err)
	}
}
