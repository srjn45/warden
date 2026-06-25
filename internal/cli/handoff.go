package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/store"
)

// handoff is the cross-agent counterpart to rotate. Where rotate hands an
// agent's work to a same-worktree successor and retires the agent, handoff
// delegates a sub-task to a DIFFERENT agent — a brand-new one (default) or an
// existing one (--to) — and the source agent keeps running.
//
// Unlike rotate, the recipient lives in a different worktree/process and cannot
// read the source's temp handoff file by path. So handoff reads the file and
// inlines its CONTENT into the delegate's prompt / the delivered message; the
// handoff file is purely a human-review artifact on the source side.

// readHandoffContent validates the handoff file (reusing validateHandoff) and
// returns its body, which is inlined into the delegate prompt or message.
func readHandoffContent(path string) (string, error) {
	if err := validateHandoff(path); err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read handoff file %q: %w", path, err)
	}
	return string(b), nil
}

// composeDelegatePrompt builds the initial prompt for a brand-new delegate: it
// frames the agent as receiving a delegated task, inlines the handoff context,
// then states the task. The context is inlined (not a file path) because the
// fresh agent runs in its own worktree and can't read the source's temp file.
func composeDelegatePrompt(resumePrompt, handoffContent string) string {
	return fmt.Sprintf("You are a fresh agent receiving a task delegated from another agent that "+
		"continues its own work elsewhere. The handoff context below has the goal, decisions already "+
		"made, and pointers you need — read it first, then carry out the task.\n\n"+
		"--- HANDOFF CONTEXT ---\n%s\n--- END HANDOFF CONTEXT ---\n\nYour task:\n\n%s",
		handoffContent, resumePrompt)
}

// composeHandoffMessage builds the inbox message delivered to an existing agent
// in --to mode. It names the sender for provenance, inlines the context, then
// states the ask.
func composeHandoffMessage(resumePrompt, handoffContent, fromID string) string {
	return fmt.Sprintf("🤝 Handoff from %s — a task is being delegated to you. Read the context, "+
		"then take it on.\n\n--- HANDOFF CONTEXT ---\n%s\n--- END HANDOFF CONTEXT ---\n\nThe ask:\n\n%s",
		fromID, handoffContent, resumePrompt)
}

// buildDelegateParams clones nothing from the source: a delegate is a managed
// spawn (Type set, Worktree/InRepo false) so a write-agent type lands in its own
// isolated worktree by default — never sharing the source's working tree. force
// passes through to spawn past the memory-pressure gate (mirrors `start --force`).
func buildDelegateParams(repo, typ, name, branch, prompt string, force bool) client.SpawnParams {
	return client.SpawnParams{
		Type:   typ,
		Repo:   repo,
		Name:   name,
		Branch: branch,
		Prompt: prompt,
		Force:  force,
	}
}

// resolveHandoffRepo picks the repo for a new delegate: the --repo flag wins,
// else the source session's repo (when handoff runs inside an agent), else the
// caller's cwd — mirroring `warden start`'s repo defaulting.
func resolveHandoffRepo(repoFlag string, self *store.Session) (string, error) {
	if repoFlag != "" {
		return repoFlag, nil
	}
	if self != nil && self.Repo != "" {
		return self.Repo, nil
	}
	return os.Getwd()
}

// handoffClient is the minimal daemon surface handoff needs. It deliberately
// OMITS Terminate: handoff is a delegation, not a succession — the source agent
// must never be reaped. Leaving the method off the interface makes that a
// compile-time guarantee, mirroring rotate's omission of RemoveWorktree.
type handoffClient interface {
	Get(ctx context.Context, id string) (*store.Session, error)
	Spawn(ctx context.Context, p client.SpawnParams) (*store.Session, error)
	MsgSend(ctx context.Context, to, from, body string) (client.Message, bool, error)
}

// Client must satisfy handoffClient.
var _ handoffClient = (*client.Client)(nil)

// runHandoffNew spawns a brand-new delegate seeded with the composed prompt and
// returns it. The source agent is untouched.
func runHandoffNew(ctx context.Context, c handoffClient, p client.SpawnParams) (*store.Session, error) {
	delegate, err := c.Spawn(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("spawn delegate: %w", err)
	}
	return delegate, nil
}

// runHandoffTo delivers the handoff into an existing agent's inbox. It verifies
// the target exists first (fail fast, before sending), then sends — returning
// whether the recipient was woken.
func runHandoffTo(ctx context.Context, c handoffClient, targetID, from, body string) (bool, error) {
	if _, err := c.Get(ctx, targetID); err != nil {
		return false, fmt.Errorf("handoff target %q: %w", targetID, err)
	}
	_, woke, err := c.MsgSend(ctx, targetID, from, body)
	if err != nil {
		return false, fmt.Errorf("deliver handoff to %q: %w", targetID, err)
	}
	return woke, nil
}

func newHandoffCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "handoff",
		Short: "Delegate a sub-task to another agent — a brand-new one or an existing one (--to)",
		Long: "Hand a structured context package to a DIFFERENT agent so it can pick up a related " +
			"task. Unlike `rotate`, the source agent keeps running. Phase 1 (writing the handoff " +
			"file + resume prompt) is driven by the /warden skill; this verb performs the delivery. " +
			"Default mode spawns a fresh delegate in its own worktree; --to <id> delivers into an " +
			"already-running agent's inbox (waking it).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			resumeFile, _ := cmd.Flags().GetString("resume-file")
			resumePrompt, _ := cmd.Flags().GetString("resume-prompt")
			if resumeFile == "" || resumePrompt == "" {
				return fmt.Errorf("--resume-file and --resume-prompt are both required")
			}
			content, err := readHandoffContent(resumeFile)
			if err != nil {
				return err
			}
			from := resolveSender(cmd.Flag("as").Value.String(), envID("SESSION_ID"))
			out := cmd.OutOrStdout()
			c := clientFor(cmd)

			// Existing-agent mode: deliver into the target's inbox.
			if to, _ := cmd.Flags().GetString("to"); to != "" {
				body := composeHandoffMessage(resumePrompt, content, from)
				woke, err := runHandoffTo(cmd.Context(), c, to, from, body)
				if err != nil {
					return err
				}
				msg := fmt.Sprintf("handed off to %s", to)
				if woke {
					msg += " — woke recipient"
				}
				fmt.Fprintln(out, msg)
				return nil
			}

			// New-agent mode: resolve the source's repo (when run inside an agent),
			// then spawn an isolated delegate seeded with the inlined context.
			var self *store.Session
			if id := envID("SESSION_ID"); id != "" {
				self, _ = c.Get(cmd.Context(), id) // best-effort; falls back to cwd
			}
			repoFlag, _ := cmd.Flags().GetString("repo")
			repo, err := resolveHandoffRepo(repoFlag, self)
			if err != nil {
				return err
			}
			typ, _ := cmd.Flags().GetString("type")
			name, _ := cmd.Flags().GetString("name")
			branch, _ := cmd.Flags().GetString("branch")
			force, _ := cmd.Flags().GetBool("force")
			prompt := composeDelegatePrompt(resumePrompt, content)
			delegate, err := runHandoffNew(cmd.Context(), c, buildDelegateParams(repo, typ, name, branch, prompt, force))
			if err != nil {
				return err
			}
			nameLabel := ""
			if delegate.Name != "" {
				nameLabel = fmt.Sprintf(" (%s)", delegate.Name)
			}
			fmt.Fprintf(out, "delegated to fresh agent %s%s [%s] — attach with `warden attach %s`\n",
				delegate.ID, nameLabel, delegate.Type, delegate.ID)
			return nil
		},
	}
	cmd.Flags().String("resume-file", "", "path to the handoff notes file whose content is delivered to the recipient")
	cmd.Flags().String("resume-prompt", "", "the recipient's task prompt")
	cmd.Flags().String("to", "", "deliver to this existing agent id instead of spawning a new one")
	cmd.Flags().String("as", "", "act as this agent id for provenance (defaults to $WARDEN_SESSION_ID, else 'human')")
	cmd.Flags().String("type", "development", "task type for a new delegate (ignored with --to)")
	cmd.Flags().String("repo", "", "repo for a new delegate (default: source agent's repo, else cwd; ignored with --to)")
	cmd.Flags().String("name", "", "optional human-friendly name for a new delegate (ignored with --to)")
	cmd.Flags().String("branch", "", "optional branch for a new delegate (ignored with --to)")
	cmd.Flags().Bool("force", false, "spawn the new delegate even when the memory-pressure gate warns (ignored with --to)")
	return cmd
}
