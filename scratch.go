package main

import (
	"fmt"
	"github.com/srjn45/warden/internal/agentbackend/backends"
)

func main() {
	pane := "Bash(ls)\nDo you want to proceed?\n❯ 1. Yes\n  2. No"
	b := backends.Claude{}
	st := b.DetectState(pane)
	ap, ok := b.ParseApproval(pane)
	fmt.Printf("st: %v, ok: %v\n", st, ok)
	if ok {
		fmt.Printf("ap: %+v\n", ap)
	}
}
