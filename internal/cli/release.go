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
	releaseAnalyze = func(ctx context.Context, repo, head, targetSHA string) (release.Advice, error) {
		return release.Analyze(ctx, release.Options{Repo: repo, Head: head, TargetSHA: targetSHA})
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
	Target    string         `json:"target,omitempty"`
	TargetSHA string         `json:"target_sha,omitempty"`
	Advice    release.Advice `json:"advice"`
	Changelog string         `json:"changelog"`
	Bump      string         `json:"bump"`
	Next      string         `json:"next"`
	DryRun    bool           `json:"dry_run"`
	Tagged    bool           `json:"tagged"`
	Pushed    bool           `json:"pushed"`
}

func newReleaseCmd() *cobra.Command {
	var yes, push, dryRun, asJSON, noFetch bool
	var target string
	cmd := &cobra.Command{
		Use:   "release",
		Short: "Recommend the next SemVer release tag, then tag and push it on confirmation",
		Long: `Inspect commits since the latest SemVer tag and recommend the next release:
the bump (major/minor/patch), the next vMAJOR.MINOR.PATCH, and a categorized
changelog built from conventional commits and merged PRs.
A squash commit whose subject is not itself releasable (e.g. "autopilot: x (#1)")
is bumped by the conventional "* feat(...): ..." bullets in its body.

By default, the analysis targets origin/main (or origin/master) so releases are
evaluated against canonical upstream commits rather than uncommitted local edits
or active feature/agent worktrees. Pass --target to override.

This is the step after the final PR of a plan is merged.

Unless --no-fetch is given, the command fetches the target ref from origin first.

Interactive by default: asks before creating an annotated tag directly on the
target commit (default N), then — only if --push was given — asks before pushing
it to origin (default N). The tag message carries the rendered changelog. An
existing tag is never overwritten.

  --target <ref> set target ref to release (default: origin/main or origin/master)
  --no-fetch     skip fetching the target ref from origin before analysis
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
			return runRelease(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), dir, target, yes, push, dryRun, asJSON, noFetch)
		},
	}
	cmd.Flags().StringVar(&target, "target", "", "target ref or branch to release (default: origin/main or origin/master)")
	cmd.Flags().BoolVar(&noFetch, "no-fetch", false, "skip fetching target ref from origin before analysis")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the create-tag prompt and create the annotated tag")
	cmd.Flags().BoolVar(&push, "push", false, "push the tag to origin after creating it (prompts unless --yes)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the recommendation only; never tag or push")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the advice and actions taken as JSON")
	return cmd
}

func resolveReleaseTarget(ctx context.Context, dir, explicitTarget string, noFetch bool) (string, string, error) {
	if explicitTarget != "" {
		if strings.HasPrefix(explicitTarget, "origin/") && !noFetch {
			branch := strings.TrimPrefix(explicitTarget, "origin/")
			_, _ = releaseGit(ctx, dir, "fetch", "origin", branch)
		}
		sha, err := releaseGit(ctx, dir, "rev-parse", "--verify", explicitTarget)
		if err != nil {
			return "", "", fmt.Errorf("target ref %q not found: %w", explicitTarget, err)
		}
		return explicitTarget, sha, nil
	}

	remotes, _ := releaseGit(ctx, dir, "remote")
	hasOrigin := false
	for _, r := range strings.Fields(remotes) {
		if r == "origin" {
			hasOrigin = true
			break
		}
	}

	if hasOrigin {
		if !noFetch {
			_, _ = releaseGit(ctx, dir, "fetch", "origin", "main")
		}
		if sha, err := releaseGit(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/main"); err == nil && sha != "" {
			return "origin/main", sha, nil
		}
		if !noFetch {
			_, _ = releaseGit(ctx, dir, "fetch", "origin", "master")
		}
		if sha, err := releaseGit(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/master"); err == nil && sha != "" {
			return "origin/master", sha, nil
		}
		if out, err := releaseGit(ctx, dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil && out != "" {
			ref := strings.TrimSpace(out)
			if sha, err := releaseGit(ctx, dir, "rev-parse", "--verify", "--quiet", ref); err == nil && sha != "" {
				return ref, sha, nil
			}
		}
	}

	if sha, err := releaseGit(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/heads/main"); err == nil && sha != "" {
		return "main", sha, nil
	}
	if sha, err := releaseGit(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/heads/master"); err == nil && sha != "" {
		return "master", sha, nil
	}
	sha, err := releaseGit(ctx, dir, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("cannot resolve release target: %w", err)
	}
	return "HEAD", sha, nil
}

func runRelease(ctx context.Context, in io.Reader, out io.Writer, dir, target string, yes, push, dryRun, asJSON, noFetch bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	targetRef, targetSHA, err := resolveReleaseTarget(ctx, dir, target, noFetch)
	if err != nil {
		return err
	}
	adv, err := releaseAnalyze(ctx, dir, targetRef, targetSHA)
	if err != nil {
		return err
	}
	res := releaseResult{
		Target:    targetRef,
		TargetSHA: targetSHA,
		Advice:    adv,
		Bump:      adv.Bump.String(),
		DryRun:    dryRun,
	}
	if adv.Bump != release.BumpNone {
		res.Next = adv.Next.String()
		res.Changelog = release.RenderChangelog(adv)
	}

	if !asJSON {
		targetDesc := targetRef
		if targetSHA != "" {
			targetDesc = fmt.Sprintf("%s (%s)", targetRef, shortSHA(targetSHA))
		}
		fmt.Fprintf(out, "target:      %s\n", targetDesc)
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
		tagArgs := []string{"tag", "-a", res.Next}
		if targetSHA != "" {
			tagArgs = append(tagArgs, targetSHA)
		}
		tagArgs = append(tagArgs, "-m", res.Changelog)
		if _, err := releaseGit(ctx, dir, tagArgs...); err != nil {
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
