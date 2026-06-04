package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/srajanpathak/agentctl/internal/client"
)

// resolveSender picks the "from" identity for outgoing messages: --as, else the
// env id, else "human".
func resolveSender(asFlag, env string) string {
	if asFlag != "" {
		return asFlag
	}
	if env != "" {
		return env
	}
	return "human"
}

// resolveSelf picks WHOSE inbox to read: --as, else the env id; errors if
// neither is set (there is no sensible default recipient).
func resolveSelf(asFlag, env string) (string, error) {
	if asFlag != "" {
		return asFlag, nil
	}
	if env != "" {
		return env, nil
	}
	return "", fmt.Errorf("no agent id: pass --as <id> or set AGENTCTL_SESSION_ID")
}

func formatMessage(m client.Message) string {
	flag := ""
	if !m.Read {
		flag = " [unread]"
	}
	return fmt.Sprintf("from %s at %s%s\n  %s", m.From, m.TS.Format(time.RFC3339), flag, m.Body)
}

func newMsgCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "msg",
		Short: "Send and receive directed messages between agents",
	}
	cmd.PersistentFlags().String("as", "", "act as this agent id (defaults to $AGENTCTL_SESSION_ID)")
	cmd.AddCommand(newMsgSendCmd(), newMsgInboxCmd(), newMsgWaitCmd())
	return cmd
}

func newMsgSendCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "send <to> <message...>",
		Short: "Send a message to an agent (wakes it if it's idle/waiting)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			as, _ := cmd.Flags().GetString("as")
			from := resolveSender(as, os.Getenv("AGENTCTL_SESSION_ID"))
			to, body := args[0], strings.Join(args[1:], " ")
			m, woke, err := clientFor(cmd).MsgSend(cmd.Context(), to, from, body)
			if err != nil {
				return err
			}
			out := fmt.Sprintf("sent to %s (id %s)", to, m.ID)
			if woke {
				out += " — woke recipient"
			}
			fmt.Fprintln(cmd.OutOrStdout(), out)
			return nil
		},
	}
}

func newMsgInboxCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inbox",
		Short: "Show this agent's messages (marks them read)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			as, _ := cmd.Flags().GetString("as")
			self, err := resolveSelf(as, os.Getenv("AGENTCTL_SESSION_ID"))
			if err != nil {
				return err
			}
			unread, _ := cmd.Flags().GetBool("unread")
			msgs, err := clientFor(cmd).MsgInbox(cmd.Context(), self, unread)
			if err != nil {
				return err
			}
			if len(msgs) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no messages)")
				return nil
			}
			for _, m := range msgs {
				fmt.Fprintln(cmd.OutOrStdout(), formatMessage(m))
			}
			return nil
		},
	}
	cmd.Flags().Bool("unread", false, "show only unread messages")
	return cmd
}

func newMsgWaitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wait",
		Short: "Block until a message arrives (or timeout), then print it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			as, _ := cmd.Flags().GetString("as")
			self, err := resolveSelf(as, os.Getenv("AGENTCTL_SESSION_ID"))
			if err != nil {
				return err
			}
			from, _ := cmd.Flags().GetString("from")
			timeout, _ := cmd.Flags().GetInt("timeout")
			m, err := clientFor(cmd).MsgWait(cmd.Context(), self, from, timeout)
			if err != nil {
				return err
			}
			if m == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "(no message — timed out)")
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), formatMessage(*m))
			return nil
		},
	}
	cmd.Flags().String("from", "", "only wait for a message from this sender")
	cmd.Flags().Int("timeout", 300, "seconds to wait before giving up")
	return cmd
}
