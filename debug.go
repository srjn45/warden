package main

import (
	"fmt"
	"github.com/srjn45/warden/internal/pipeline"
)

func main() {
	spec := "name: nightly\nrepo: /r\njobs:\n  - id: a\n    prompt: go\n    worktree: none\n"
	p, err := pipeline.ParseSpec([]byte(spec))
	fmt.Printf("err: %v\n", err)
	if p != nil {
		for _, j := range p.Jobs {
			fmt.Printf("job %s: System=%v Prompt=%q\n", j.ID, j.System, j.Prompt)
		}
	}
}
