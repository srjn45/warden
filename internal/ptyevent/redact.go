package ptyevent

import (
	"bytes"
	"regexp"
	"sort"
)

// SecretKind classifies a redacted span.
type SecretKind string

const (
	SecretAnthropicKey  SecretKind = "anthropic_key"
	SecretOpenAIKey     SecretKind = "openai_key"
	SecretGitHubToken   SecretKind = "github_token"
	SecretAWSAccessKey  SecretKind = "aws_access_key"
	SecretGoogleAPIKey  SecretKind = "google_api_key"
	SecretSlackToken    SecretKind = "slack_token"
	SecretJWT           SecretKind = "jwt"
	SecretBearer        SecretKind = "bearer_token"
	SecretURLCredential SecretKind = "url_credential"
	SecretAssignment    SecretKind = "secret_assignment" // api_key=…, password: …
	SecretPrivateKey    SecretKind = "private_key"
	SecretTypedInput    SecretKind = "typed_input" // input typed in a secret context (see SecretInput)
)

// Redaction is one replaced span. Offset/Length locate the PLACEHOLDER in the
// redacted payload; OrigLen is how many bytes it replaced. The original bytes
// are never stored anywhere warden persists.
type Redaction struct {
	Kind    SecretKind `json:"kind"`
	Offset  int        `json:"offset"`
	Length  int        `json:"length"`
	OrigLen int        `json:"orig_len"`
}

// Placeholder is the text substituted for a secret of kind k.
func Placeholder(k SecretKind) string { return "[REDACTED:" + string(k) + "]" }

type secretRule struct {
	kind SecretKind
	re   *regexp.Regexp
	// group is the submatch to replace (0 = whole match). Contextual rules
	// (bearer, url credential, assignment) keep their key/prefix visible.
	group int
}

// Rule order matters only for overlap: earlier rules win.
var secretRules = []secretRule{
	{SecretPrivateKey, regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), 0},
	{SecretAnthropicKey, regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`), 0},
	{SecretOpenAIKey, regexp.MustCompile(`sk-(?:proj-)?[A-Za-z0-9_-]{20,}`), 0},
	{SecretGitHubToken, regexp.MustCompile(`(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{22,})`), 0},
	{SecretAWSAccessKey, regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), 0},
	{SecretGoogleAPIKey, regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`), 0},
	{SecretSlackToken, regexp.MustCompile(`xox[abprs]-[A-Za-z0-9-]{10,}`), 0},
	{SecretJWT, regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), 0},
	{SecretBearer, regexp.MustCompile(`(?i)\bbearer\s+([A-Za-z0-9._~+/=-]{16,})`), 1},
	{SecretURLCredential, regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@]+:([^\s@/]+)@`), 1},
	{SecretAssignment, regexp.MustCompile(`(?i)\b(?:api[_-]?key|secret|token|passw(?:or)?d|pwd)\s*[=:]\s*["']?([^\s"']{8,})`), 1},
}

type span struct {
	start, end int
	kind       SecretKind
}

// Redact replaces every typed secret in b. It is the single redaction routine
// for the persistence/fan-out boundary (journal, SSE, transcripts excerpts,
// prompt evidence, Fast-Brain inputs, logs). It is deterministic and pure.
func Redact(b []byte) ([]byte, []Redaction) {
	var spans []span
	for _, r := range secretRules {
		for _, m := range r.re.FindAllSubmatchIndex(b, -1) {
			s, e := m[2*r.group], m[2*r.group+1]
			if s < 0 || overlaps(spans, s, e) {
				continue
			}
			spans = append(spans, span{s, e, r.kind})
		}
	}
	if len(spans) == 0 {
		return b, nil
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	out := make([]byte, 0, len(b))
	reds := make([]Redaction, 0, len(spans))
	last := 0
	for _, sp := range spans {
		out = append(out, b[last:sp.start]...)
		ph := Placeholder(sp.kind)
		reds = append(reds, Redaction{Kind: sp.kind, Offset: len(out), Length: len(ph), OrigLen: sp.end - sp.start})
		out = append(out, ph...)
		last = sp.end
	}
	out = append(out, b[last:]...)
	return out, reds
}

func overlaps(spans []span, s, e int) bool {
	for _, sp := range spans {
		if s < sp.end && sp.start < e {
			return true
		}
	}
	return false
}

// MaxRedactHold caps how many trailing bytes the streaming Redactor withholds
// waiting for a line/token to finish; MaxKeyHold caps an open private-key block.
const (
	MaxRedactHold = 512
	MaxKeyHold    = 64 << 10
)

var (
	keyBegin = []byte("-----BEGIN ")
	keyEndRe = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY-----`)
)

// openKeyStart returns the offset of an unterminated private-key block in b
// (or of a "-----BEGIN " header still arriving, which might become one), or -1.
func openKeyStart(b []byte) int {
	idx := bytes.LastIndex(b, keyBegin)
	if idx < 0 {
		return -1
	}
	rest := b[idx+len(keyBegin):]
	hdr := bytes.Index(rest, []byte("-----"))
	if hdr < 0 {
		if len(rest) <= 64 {
			return idx // header still arriving
		}
		return -1
	}
	if !bytes.Contains(rest[:hdr], []byte("PRIVATE KEY")) || keyEndRe.Match(rest[hdr:]) {
		return -1
	}
	return idx
}

// Redactor applies Redact to a byte stream whose secrets may straddle chunk
// boundaries. It withholds the trailing partial line (see releasable) and any open "-----BEGIN … PRIVATE KEY"
// block (up to MaxKeyHold; past that the whole held block is redacted — fail
// closed). Only the journal/fan-out path pays this latency; the live attach
// stream is never delayed by it. The zero value is ready to use.
type Redactor struct {
	held []byte
}

// Feed redacts the releasable prefix of held+chunk.
func (r *Redactor) Feed(chunk []byte) ([]byte, []Redaction) {
	buf := append(r.held, chunk...)
	cut := releasable(buf)
	r.held = bytes.Clone(buf[cut:])
	return redactOrBlock(buf[:cut])
}

// Flush redacts and releases everything held (idle timer, process exit). An
// unterminated private-key block is redacted to the end (fail closed).
func (r *Redactor) Flush() ([]byte, []Redaction) {
	buf := r.held
	r.held = nil
	return redactOrBlock(buf)
}

// Held reports how many bytes are currently withheld.
func (r *Redactor) Held() int { return len(r.held) }

// ctxPrefix matches a contextual secret prefix dangling at the end of the
// releasable text ("Bearer ", "password: "): its value has not arrived yet, so
// the prefix is held with it.
var ctxPrefix = regexp.MustCompile(`(?i)(?:\bbearer|\b(?:api[_-]?key|secret|token|passw(?:or)?d|pwd)\s*[=:]?)\s*["']?$`)

// releasable returns how many leading bytes of buf may be redacted+released
// now. It holds back the trailing partial line (after the last \n or \r —
// TUIs redraw with CR), falling back to the last whitespace when that line
// exceeds MaxRedactHold; never cuts through a complete secret match; and holds
// a dangling contextual prefix together with its upcoming value.
func releasable(buf []byte) int {
	if open := openKeyStart(buf); open >= 0 {
		if len(buf)-open <= MaxKeyHold {
			return open
		}
		return len(buf) // over cap: release; redactOrBlock redacts the block
	}
	cut := bytes.LastIndexAny(buf, "\r\n") + 1
	if len(buf)-cut > MaxRedactHold {
		cut = bytes.LastIndexAny(buf, " \t") + 1
		if len(buf)-cut > MaxRedactHold {
			return len(buf)
		}
	}
	if cut == len(buf) {
		return cut
	}
	for _, r := range secretRules {
		for _, m := range r.re.FindAllIndex(buf, -1) {
			if m[0] < cut && cut < m[1] {
				cut = m[1]
			}
		}
	}
	if m := ctxPrefix.FindIndex(buf[:cut]); m != nil {
		cut = m[0]
	}
	return cut
}

func redactOrBlock(b []byte) ([]byte, []Redaction) {
	if open := openKeyStart(b); open >= 0 {
		return redactOpenKey(b, open)
	}
	return Redact(b)
}

// redactOpenKey redacts the prefix normally and everything from an unterminated
// private-key header onward.
func redactOpenKey(b []byte, open int) ([]byte, []Redaction) {
	out, reds := Redact(b[:open])
	out = bytes.Clone(out)
	ph := Placeholder(SecretPrivateKey)
	reds = append(reds, Redaction{Kind: SecretPrivateKey, Offset: len(out), Length: len(ph), OrigLen: len(b) - open})
	return append(out, ph...), reds
}

// TTYMode is the PTY slave's line discipline as the runtime reads it
// (tcgetattr) when input is written.
type TTYMode struct {
	Canonical bool // ICANON
	Echo      bool // ECHO
}

// SecretInput reports whether input typed now must be treated as a secret
// (Input.Secret: data dropped, only its length kept). Two signals, either
// suffices:
//
//   - a classic getpass prompt: canonical mode with echo off. (Raw-mode TUIs —
//     every AI CLI — run with echo off permanently, so echo-off ALONE is not a
//     secret signal; requiring canonical mode keeps ordinary TUI typing visible.)
//   - the prompt classifier has a credential prompt on screen (API-key entry,
//     OAuth paste-code box): credentialPrompt.
func SecretInput(m TTYMode, credentialPrompt bool) bool {
	return (m.Canonical && !m.Echo) || credentialPrompt
}

// RedactInput turns raw input into the Input payload the journal may store.
func RedactInput(data []byte, origin Origin, m TTYMode, credentialPrompt bool) (Input, []Redaction) {
	if SecretInput(m, credentialPrompt) {
		return Input{Origin: origin, Secret: true, SecretLen: len(data)}, nil
	}
	out, reds := Redact(data)
	return Input{Data: out, Origin: origin}, reds
}
