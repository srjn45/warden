package brainconsult

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// ModeAnswer is Request.Mode for answering a stuck agent's prompt: the brain
// reads the prompt, its options, the agent's pane tail, the task and the plan
// constraints, and replies with the option to select or the text to type. The
// brain never touches the stuck agent; the daemon delivers the answer.
const ModeAnswer = "answer"

// AnswerKind is the closed set of answers a brain may give to a prompt.
type AnswerKind string

const (
	AnswerApprove      AnswerKind = "approve"
	AnswerReject       AnswerKind = "reject"
	AnswerSelectOption AnswerKind = "select_option"
	AnswerType         AnswerKind = "type"
)

// AnswerContext is everything the brain is given about the stuck prompt.
type AnswerContext struct {
	Question    string
	Options     []string
	PaneTail    string // last ~80 lines of the stuck agent's pane
	Task        string
	Constraints string // plan constraints
	Destructive bool   // deny-by-default: ask for a safe alternative
	Marker      string // what tripped the destructive guard
}

const maxPaneTailBytes = 6 * 1024

func buildAnswerPrompt(a AnswerContext) string {
	var b strings.Builder
	b.WriteString("An autonomous coding agent is blocked on an interactive prompt. Decide how to answer it.\n")
	fmt.Fprintf(&b, "Prompt: %s\n", strings.TrimSpace(a.Question))
	if len(a.Options) > 0 {
		b.WriteString("Options:\n")
		for i, o := range a.Options {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, o)
		}
	}
	if a.Task != "" {
		fmt.Fprintf(&b, "Agent task: %s\n", a.Task)
	}
	if a.Constraints != "" {
		fmt.Fprintf(&b, "Plan constraints: %s\n", a.Constraints)
	}
	tail := a.PaneTail
	if len(tail) > maxPaneTailBytes {
		tail = tail[len(tail)-maxPaneTailBytes:]
	}
	fmt.Fprintf(&b, "Agent pane tail:\n%s\n\n", tail)
	if a.Destructive {
		fmt.Fprintf(&b, "WARNING: this prompt is destructive or irreversible (%s). Never approve it or select the destructive option. Reply with action \"reject\" and put a safe, non-destructive alternative the agent should take in \"text\".\n\n", a.Marker)
	}
	b.WriteString(`Reply with a single JSON object on a line by itself:
{"answer": "approve|reject|select_option|type", "option": <1-based number, for select_option>, "text": "<text to type, for type or a reject alternative>", "reason": "<one line>"}`)
	return b.String()
}

type answerReply struct {
	Answer string `json:"answer"`
	Option int    `json:"option"`
	Text   string `json:"text"`
	Reason string `json:"reason"`
}

// parseAnswer scans output for a valid answer line. Unknown kinds and
// out-of-range options are skipped (never guessed).
func parseAnswer(output string, nOptions int) (Result, bool) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var r answerReply
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		k := AnswerKind(r.Answer)
		switch k {
		case AnswerApprove, AnswerReject:
		case AnswerSelectOption:
			if r.Option < 1 || r.Option > nOptions {
				continue
			}
		case AnswerType:
			if strings.TrimSpace(r.Text) == "" {
				continue
			}
		default:
			continue
		}
		return Result{Answer: k, Option: r.Option, Text: r.Text, Reason: r.Reason}, true
	}
	return Result{}, false
}

func (c *consultor) waitForAnswer(ctx context.Context, brainID, tmuxSess string, a AnswerContext) (Result, error) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return Result{}, ErrNoBrainReply
		case <-ticker.C:
			out, err := c.spawner.Output(ctx, tmuxSess, outputLines)
			if err != nil {
				slog.Warn("brain consult: output capture failed", "brain_id", brainID, "err", err)
				continue
			}
			// The brain's own prompt echoes the JSON template; the template's
			// "answer" is a pipe-list so it never parses as a valid kind.
			if r, ok := parseAnswer(out, len(a.Options)); ok {
				return r, nil
			}
		}
	}
}
