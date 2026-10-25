//go:build !race

package ptyhost

// raceSlack scales latency budgets when the race detector is on (see race_on_test.go).
const raceSlack = 1
