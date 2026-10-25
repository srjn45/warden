//go:build race

package ptyhost

// raceSlack scales latency budgets when the race detector (~5-10x slower) is on;
// the budgets themselves are defined for an uninstrumented build.
const raceSlack = 8
