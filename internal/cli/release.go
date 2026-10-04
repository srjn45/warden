package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/release"
)

// releaseAnalyze and releaseGit are seams so tests never touch a real repo.
var (
	releaseAnalyze = func(ctx context.Context, repo string) (release.Advice, error) {
		return release.Analyze(ctx, release.Options{Repo: repo})
	}
	releaseGit = func(ctx context.Context, dir string, args ...string) (string, error) {
		c := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		out, err := c.CombinedOutput()
		s := strings.TrimSpace(string(out))
		if err != nil {
			return s, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, s)
		}
		return s, nil
	}
)

// releaseResult is the --json payload: the advice plus what was done.
type releaseResult struct {
	Advice    release.Advice `json:"advice"`
	Changelog string         `json:"changelog"`
	Bump      string         `json:"bump"`
	Next      string         `json:"next"`
	DryRun    bool           `json:"dry_run"`
	Tagged    bool           `json:"tagged"`
	Pushed    bool           `json:"pushed"`
}

func newReleaseCmd() *cobra.Command {
	var yes, push, dryRun, asJSON bool
	cmd := &cobra.Command{
		Use:   "release",
		Short: "Recommend the next SemVer release tag, then tag and push it on confirmation",
		Long: `Inspect the current repo since its latest SemVer tag and recommend the next
release: the bump (major/minor/patch), the next vMAJOR.MINOR.PATCH, and a
categorized changelog built from conventional commits and merged PRs.

By default the command is interactive: it asks before creating an annotated tag
(default N), then — only if --push was given — asks before pushing it to origin
(default N). The tag message carries the rendered changelog. An existing tag is
never overwritten.

  --dry-run      print the recommendation only; never tag or push
  --yes          skip the create-tag prompt and create the annotated tag
  --push         push the tag to origin after it exists (prompts unless --yes)
  --yes --push   tag and push with no prompts
  --json         emit the advice and the actions taken as JSON

Agents should run ` + "`wd release --dry-run`" + `; pass --yes only when the operator
explicitly asked for a tag. Pushing a v* tag triggers the release pipeline.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, _ := gitTarget()
			return runRelease(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), dir, yes, push, dryRun, asJSON)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the create-tag prompt and create the annotated tag")
	cmd.Flags().BoolVar(&push, "push", false, "push the tag to origin after creating it (prompts unless --yes)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the recommendation only; never tag or push")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the advice and actions taken as JSON")
	return cmd
}

func runRelease(ctx context.Context, in io.Reader, out io.Writer, dir string, yes, push, dryRun, asJSON bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	adv, err := releaseAnalyze(ctx, dir)
	if err != nil {
		return err
	}
	res := releaseResult{Advice: adv, Bump: adv.Bump.String(), DryRun: dryRun}
	if adv.Bump != release.BumpNone {
		res.Next = adv.Next.String()
		res.Changelog = release.RenderChangelog(adv)
	}

	if !asJSON {
		latest := adv.LatestTag
		if latest == "" {
			latest = "(none)"
		}
		fmt.Fprintf(out, "latest tag:  %s (current %s)\n", latest, adv.Current)
		if adv.Bump == release.BumpNone {
			fmt.Fprintln(out, "recommended: none — nothing to release since the latest tag")
		} else {
			fmt.Fprintf(out, "recommended: %s bump → %s\n\n%s\n", adv.Bump, res.Next, res.Changelog)
		}
	}
	if adv.Bump == release.BumpNone || dryRun {
		if asJSON {
			return emitJSON(cmdOut(out), res)
		}
		return nil
	}

	if existing, _ := releaseGit(ctx, dir, "tag", "-l", res.Next); strings.TrimSpace(existing) != "" {
		return fmt.Errorf("tag %s already exists; refusing to overwrite", res.Next)
	}

	r := bufio.NewReader(in)
	ask := func(q string) bool {
		if asJSON { // no interactive prompts in machine mode
			return false
		}
		fmt.Fprintf(out, "%s [y/N] ", q)
		line, _ := r.ReadString('\n')
		l := strings.ToLower(strings.TrimSpace(line))
		return l == "y" || l == "yes"
	}

	if yes || ask(fmt.Sprintf("Create annotated tag %s?", res.Next)) {
		if _, err := releaseGit(ctx, dir, "tag", "-a", res.Next, "-m", res.Changelog); err != nil {
			return err
		}
		res.Tagged = true
		if !asJSON {
			fmt.Fprintf(out, "created tag %s\n", res.Next)
		}
		if push && (yes || ask(fmt.Sprintf("Push %s to origin?", res.Next))) {
			if _, err := releaseGit(ctx, dir, "push", "origin", res.Next); err != nil {
				return err
			}
			res.Pushed = true
			if !asJSON {
				fmt.Fprintf(out, "pushed tag %s to origin\n", res.Next)
			}
		}
	} else if !asJSON {
		fmt.Fprintln(out, "no tag created")
	}
	if asJSON {
		return emitJSON(cmdOut(out), res)
	}
	return nil
}

// cmdOut adapts a writer for emitJSON, which takes a command.
func cmdOut(w io.Writer) *cobra.Command {
	c := &cobra.Command{}
	c.SetOut(w)
	return c
}
