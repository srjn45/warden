//go:build !darwin

package ptyhost

// fifoPollable: the mirror's FIFO is read through the runtime poller, so
// closing it also interrupts a pending read.
const fifoPollable = true
