package lifecycle

import (
	"fmt"
	"strings"
)

// safeGitRef rejects a user-supplied git ref / branch / PR value that git or gh
// would parse as an option flag rather than a ref.
//
// Every git/gh invocation in this package runs through ExecRunner, i.e.
// exec.Command with an argv (no shell), so shell metacharacters in a ref are
// inert literal bytes — they cannot inject a command. The one remaining
// argument-injection vector is a value that *begins with "-"*: passed as a
// positional (e.g. `git fetch origin <base>` or `gh pr checkout <pr>`), git/gh
// will treat it as an option. A base of "--upload-pack=<cmd>" turns a sync into
// `git fetch origin --upload-pack=<cmd>`, which can execute <cmd> on some
// transports. Rejecting a leading "-" is both necessary and sufficient to close
// this (git refs legitimately never start with "-").
//
// Empty is allowed: callers treat "" as "use the default / derived ref".
func safeGitRef(ref string) error {
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("invalid git ref %q: must not begin with '-'", ref)
	}
	return nil
}
