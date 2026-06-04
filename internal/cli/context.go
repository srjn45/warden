package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// ctxWriter is the default writer identity: the agent's own session id when set
// (pipeline/agent context), otherwise "human".
func ctxWriter() string {
	if id := os.Getenv("AGENTCTL_SESSION_ID"); id != "" {
		return id
	}
	return "human"
}

// resolveCtxValue picks the value source: --file, --stdin, or the positional arg.
func resolveCtxValue(fileFlag string, useStdin bool, stdin io.Reader, args []string) (string, error) {
	if fileFlag != "" {
		b, err := os.ReadFile(fileFlag)
		return string(b), err
	}
	if useStdin {
		b, err := io.ReadAll(stdin)
		return string(b), err
	}
	if len(args) < 2 {
		return "", errors.New("provide a value argument, --file, or --stdin")
	}
	return args[1], nil
}

func newCtxCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ctx",
		Short: "Read and write the shared context (a namespaced key/value store agents share)",
	}
	cmd.AddCommand(newCtxSetCmd(), newCtxGetCmd(), newCtxListCmd(), newCtxDelCmd())
	return cmd
}

func newCtxSetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set <key> [value]",
		Short: "Set a context key (value inline, or --file / --stdin)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			fileFlag, _ := cmd.Flags().GetString("file")
			useStdin, _ := cmd.Flags().GetBool("stdin")
			value, err := resolveCtxValue(fileFlag, useStdin, cmd.InOrStdin(), args)
			if err != nil {
				return err
			}
			by, _ := cmd.Flags().GetString("as")
			if by == "" {
				by = ctxWriter()
			}
			if _, err := clientFor(cmd).CtxSet(cmd.Context(), args[0], value, by); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "set %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().String("file", "", "read value from a file")
	cmd.Flags().Bool("stdin", false, "read value from stdin")
	cmd.Flags().String("as", "", "writer identity (defaults to $AGENTCTL_SESSION_ID or 'human')")
	return cmd
}

func newCtxGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Print the value at a context key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := clientFor(cmd).CtxGet(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), e.Value)
			return nil
		},
	}
}

func newCtxListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list [prefix]",
		Short: "List context keys (optionally filtered by prefix)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prefix := ""
			if len(args) == 1 {
				prefix = args[0]
			}
			entries, err := clientFor(cmd).CtxList(cmd.Context(), prefix)
			if err != nil {
				return err
			}
			for _, e := range entries {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t(%s, %s)\n", e.Key, e.UpdatedBy, e.UpdatedAt.Format(time.RFC3339))
			}
			return nil
		},
	}
}

func newCtxDelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "del <key>",
		Short: "Delete a context key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := clientFor(cmd).CtxDel(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted %s\n", args[0])
			return nil
		},
	}
}
