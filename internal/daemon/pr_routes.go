package daemon

import (
	"context"
	"strings"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/digest"
	"github.com/srjn45/warden/internal/fastbrain"
)

// prTitle picks a human PR title for an agent: its live one-line subject, else
// the digest's parsed task (the first user prompt, truncated), else the branch.
func prTitle(sess *agentstore.Agent, d digest.Digest) string {
	for _, c := range []string{sess.Subject, d.Task} {
		if t := truncateTitle(strings.TrimSpace(c)); t != "" {
			return t
		}
	}
	if d.Branch != "" {
		return d.Branch
	}
	return sess.ID
}

// truncateTitle collapses a candidate to its first line and caps it at 72 chars
// (with an ellipsis) so a long prompt does not become an unwieldy PR title.
func truncateTitle(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	const max = 72
	if len(s) > max {
		return strings.TrimSpace(s[:max-1]) + "…"
	}
	return s
}

// prContent is the single place a PR's title and body are decided, shared by
// every entry point (wd agent done --create-pr, stop_agent pr=true, the
// create-pr route). Precedence per field: an explicitly supplied value always
// wins; else a Fast-Brain draft; else exactly the deterministic prTitle / digest
// body. Any non-OK decision, error or empty field falls back per field. A
// drafted body keeps the attribution footer the digest body carries.
func (s *Server) prContent(ctx context.Context, sess *agentstore.Agent, d digest.Digest, dir, base, explicitTitle, explicitBody string) (title, body string) {
	title, body = prTitle(sess, d), digest.Markdown(&d)
	explicitTitle, explicitBody = strings.TrimSpace(explicitTitle), strings.TrimSpace(explicitBody)
	draftTitle, draftBody := "", ""
	if (explicitTitle == "" || explicitBody == "") && s.fastBrain != nil {
		draftTitle, draftBody = s.draftPR(ctx, sess, d, dir, base)
	}
	switch {
	case explicitTitle != "":
		title = explicitTitle
	case draftTitle != "":
		title = draftTitle
	}
	switch {
	case explicitBody != "":
		body = explicitBody
	case draftBody != "":
		if i := strings.LastIndex(body, "---\n🤖"); i >= 0 {
			if footer := body[i:]; !strings.Contains(draftBody, footer) {
				draftBody += "\n\n" + footer
			}
		}
		body = draftBody
	}
	return title, body
}

// draftPR asks Fast-Brain for a PR title and body. Tier choice: the fast tier
// goes first — it is the cheap, bounded default for every micro-decision and a
// title plus short body fits its budget; the thinking tier is tried only when
// the fast tier yields no usable body (timeout, bad JSON or an empty field).
// Both are best-effort: failure returns ("", "") so the caller falls back.
func (s *Server) draftPR(ctx context.Context, sess *agentstore.Agent, d digest.Digest, dir, base string) (title, body string) {
	task := strings.TrimSpace(d.Task)
	if task == "" {
		task = strings.TrimSpace(sess.Subject)
	}
	stat, commits := s.life.PRContext(ctx, dir, base)
	if task == "" && stat == "" && commits == "" {
		return "", ""
	}
	prompt := fastbrain.PRSummaryPrompt(task, stat, commits)
	for _, tier := range []fastbrain.Tier{fastbrain.TierFast, fastbrain.TierThinking} {
		resp, err := s.fastBrain.Decide(ctx, fastbrain.Request{
			Kind: fastbrain.KindPRSummary, Tier: tier, Prompt: prompt,
			Metadata: map[string]string{"agent": sess.ID},
		})
		if err != nil {
			continue
		}
		t, b := fastbrain.ParsePRSummary(resp)
		if title == "" {
			title = t
		}
		if b != "" {
			return title, b
		}
	}
	return title, ""
}
