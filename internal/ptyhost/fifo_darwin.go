package ptyhost

// fifoPollable: kqueue does not handle FIFOs reliably on Darwin (Go issue
// 24164), so the mirror's FIFO is read with plain blocking reads there. A
// read then ends only when every writer (the pipe-pane cat and the keeper)
// has closed — which Shadow.Stop arranges.
const fifoPollable = false
