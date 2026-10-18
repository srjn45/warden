package main

import (
	"testing"

	"github.com/srjn45/warden/internal/updater"
)

func TestIsMajorRelease(t *testing.T) {
	for v, want := range map[string]bool{"11.0.0": true, "1.0.0": true, "10.0.1": false, "10.1.0": false, "0.0.0": false} {
		s, err := updater.ParseSemver(v)
		if err != nil {
			t.Fatal(err)
		}
		if got := isMajorRelease(s); got != want {
			t.Errorf("isMajorRelease(%s) = %v, want %v", v, got, want)
		}
	}
}
