package main

import (
	"fmt"
	"os"

	"github.com/srjn45/warden/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		code := cli.ExitCode(err)
		if code != 2 {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(code)
	}
}
