// Command gendocs regenerates the CLI reference page from the warden cobra
// command tree, so it can never drift from the real `--help` output.
//
// Usage:
//
//	go run ./cmd/gendocs                 # write the default page
//	go run ./cmd/gendocs -o path/to.md   # write to an explicit path
//	go run ./cmd/gendocs -stdout         # print to stdout (no file written)
//
// The Makefile wraps this as `make gendocs` (regenerate) and `make
// gendocs-check` (regenerate + fail if the committed copy is stale, the CI gate).
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/srjn45/warden/internal/cli"
)

// defaultOut is the committed reference page the generator owns.
const defaultOut = "site/src/content/docs/reference/cli.md"

func main() {
	out := flag.String("o", defaultOut, "output file path for the generated CLI reference")
	toStdout := flag.Bool("stdout", false, "write the generated reference to stdout instead of a file")
	flag.Parse()

	doc, err := cli.GenerateReference()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gendocs:", err)
		os.Exit(1)
	}

	if *toStdout {
		if _, err := os.Stdout.WriteString(doc); err != nil {
			fmt.Fprintln(os.Stderr, "gendocs:", err)
			os.Exit(1)
		}
		return
	}

	if err := os.WriteFile(*out, []byte(doc), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gendocs:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "gendocs: wrote", *out)
}
