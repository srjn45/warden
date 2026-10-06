package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// confirmYN prints prompt and returns true if the operator answers y/yes.
func confirmYN(in io.Reader, out io.Writer, prompt string) bool {
	fmt.Fprint(out, prompt)
	line, _ := bufio.NewReader(in).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

// confirmOrYes confirms a destructive action. When yes is true it skips the
// prompt. When stdin is a non-interactive terminal it refuses with an error
// telling the caller to pass --yes. Otherwise it prints prompt (which should
// include the "[y/N]" hint) and returns whether the answer was affirmative.
func confirmOrYes(cmd *cobra.Command, yes bool, prompt string) (bool, error) {
	if yes {
		return true, nil
	}
	in := cmd.InOrStdin()
	if f, ok := in.(*os.File); ok && !isTTY(f) {
		return false, fmt.Errorf("confirmation required; re-run with --yes")
	}
	ok := confirmYN(in, cmd.OutOrStdout(), prompt)
	return ok, nil
}
