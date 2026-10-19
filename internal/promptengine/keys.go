package promptengine

import (
	"fmt"
	"strings"
)

// tmuxKeys translates the router's keystroke bytes into `tmux send-keys`
// arguments. The router only ever produces digits/letters, Enter, Up and Down;
// any other byte is refused so a malformed answer fails closed instead of being
// typed into someone's terminal.
func tmuxKeys(data []byte) ([]string, error) {
	var keys []string
	s := string(data)
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "\x1b[B"):
			keys, i = append(keys, "Down"), i+3
		case strings.HasPrefix(s[i:], "\x1b[A"):
			keys, i = append(keys, "Up"), i+3
		case s[i] == '\r' || s[i] == '\n':
			keys, i = append(keys, "Enter"), i+1
		case s[i] >= '0' && s[i] <= '9', s[i] >= 'a' && s[i] <= 'z', s[i] >= 'A' && s[i] <= 'Z':
			keys, i = append(keys, string(s[i])), i+1
		default:
			return nil, fmt.Errorf("unsupported keystroke byte %#x", s[i])
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no keystrokes")
	}
	return keys, nil
}
