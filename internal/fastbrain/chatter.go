package fastbrain

import (
	"context"
	"fmt"
	"strings"

	"github.com/srjn45/warden/internal/llm"
)

// FastBrainChatter implements llm.Chatter on top of Engine.Decide
// (KindReplTurn, TierFast).
//
// Failure policy: Fast-Brain is best-effort and latency-bounded, so a non-OK
// Decide (timeout, no runner, runner error, invalid JSON) yields an empty,
// non-error Reply rather than an error. The REPL treats an empty reply as
// "nothing to do" and falls back to its existing path; a hard error would make
// Fast-Brain unavailability look like a crash. Only a nil engine or a canceled
// caller context returns an error.
type FastBrainChatter struct {
	eng Engine
}

var _ llm.Chatter = (*FastBrainChatter)(nil)

// NewFastBrainChatter returns a Chatter backed by eng.
func NewFastBrainChatter(eng Engine) *FastBrainChatter { return &FastBrainChatter{eng: eng} }

// Chat renders the conversation and tools into one compact prompt and decodes
// the JSON reply into prose and/or tool calls.
func (c *FastBrainChatter) Chat(ctx context.Context, msgs []llm.Message, tools []llm.ToolSchema) (llm.Reply, error) {
	if c == nil || c.eng == nil {
		return llm.Reply{}, fmt.Errorf("fastbrain: chatter has no engine")
	}
	if err := ctx.Err(); err != nil {
		return llm.Reply{}, err
	}
	resp, err := c.eng.Decide(ctx, Request{Kind: KindReplTurn, Tier: TierFast, Prompt: replPrompt(msgs, tools)})
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return llm.Reply{}, cerr
		}
		return llm.Reply{}, nil // fail open (see type doc)
	}
	turn := ParseReplTurn(resp)
	reply := llm.Reply{Text: turn.Text}
	for _, tc := range turn.ToolCalls {
		reply.ToolCalls = append(reply.ToolCalls, llm.ToolCall{Name: tc.Name, Args: tc.Args})
	}
	return reply, nil
}

func replPrompt(msgs []llm.Message, tools []llm.ToolSchema) string {
	var b strings.Builder
	b.WriteString(replTurnSystem)
	if len(tools) > 0 {
		b.WriteString("\n\nTools:\n")
		for _, t := range tools {
			fmt.Fprintf(&b, "- %s: %s", t.Name, t.Description)
			if props, ok := t.Parameters["properties"].(map[string]any); ok && len(props) > 0 {
				names := make([]string, 0, len(props))
				for k := range props {
					names = append(names, k)
				}
				sortStrings(names)
				fmt.Fprintf(&b, " (args: %s)", strings.Join(names, ", "))
			}
			b.WriteByte('\n')
		}
	}
	b.WriteString("\nConversation:\n")
	for _, m := range msgs {
		switch {
		case m.Role == llm.RoleTool:
			fmt.Fprintf(&b, "[tool %s result] %s\n", m.ToolName, m.Content)
		case len(m.ToolCalls) > 0:
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "[assistant called %s]\n", tc.Name)
			}
			if m.Content != "" {
				fmt.Fprintf(&b, "[assistant] %s\n", m.Content)
			}
		default:
			fmt.Fprintf(&b, "[%s] %s\n", m.Role, m.Content)
		}
	}
	return clipTail(b.String())
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// clipTail keeps the system/tool header and drops the oldest conversation if
// the prompt is huge, favoring the most recent turns.
func clipTail(s string) string {
	const max = 4 * maxPromptInputBytes
	if len(s) <= max {
		return s
	}
	i := strings.Index(s, "\nConversation:\n")
	if i < 0 || i > max/2 {
		return s[len(s)-max:]
	}
	return s[:i] + "\nConversation:\n...\n" + s[len(s)-(max-i-20):]
}
