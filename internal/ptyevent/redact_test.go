package ptyevent

import (
	"bytes"
	"strings"
	"testing"
)

// Secrets are assembled at runtime so no token-shaped literal lives in the
// repository (secret scanners / push protection would flag a fixture file).
func fake(parts ...string) string { return strings.Join(parts, "") }

var (
	fakeAnthropic = fake("sk-", "ant-", "api03-", strings.Repeat("A1b2", 8))
	fakeOpenAI    = fake("sk-", "proj-", strings.Repeat("Zz09", 7))
	fakeGitHub    = fake("gh", "p_", strings.Repeat("aB3", 12))
	fakeAWS       = fake("AK", "IA", "ABCDEFGHIJKLMNOP")
	fakeGoogle    = fake("AI", "za", strings.Repeat("x", 35))
	fakeSlack     = fake("xo", "xb-", "1234567890-abcdefghij")
	fakeJWT       = fake("ey", "JhbGciOiJIUzI1NiJ9", ".", "ey", "JzdWIiOiIxMjM0NTY3ODkwIn0", ".", "SflKxwRJSMeKKF2QT4fwpMeJf36P")
	fakeKey       = fake("-----BEGIN ", "OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\nQUJDREVGR0g=\n-----END ", "OPENSSH PRIVATE KEY-----")
)

func TestRedact(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"nothing", "go test ./... ok", "go test ./... ok"},
		{"anthropic", "key=" + fakeAnthropic, "key=" + Placeholder(SecretAnthropicKey)},
		{"anthropic before openai rule", "export X " + fakeAnthropic + " done", "export X " + Placeholder(SecretAnthropicKey) + " done"},
		{"openai", "use " + fakeOpenAI, "use " + Placeholder(SecretOpenAIKey)},
		{"github", fakeGitHub + "\n", Placeholder(SecretGitHubToken) + "\n"},
		{"aws", "id " + fakeAWS + " x", "id " + Placeholder(SecretAWSAccessKey) + " x"},
		{"google", fakeGoogle, Placeholder(SecretGoogleAPIKey)},
		{"slack", fakeSlack, Placeholder(SecretSlackToken)},
		{"jwt", "tok " + fakeJWT, "tok " + Placeholder(SecretJWT)},
		{"bearer keeps the scheme", "Authorization: Bearer abcdefghijklmnop1234", "Authorization: Bearer " + Placeholder(SecretBearer)},
		{"url credential keeps user and host", "https://bob:s3cr3tpass@git.example.com/r.git", "https://bob:" + Placeholder(SecretURLCredential) + "@git.example.com/r.git"},
		{"assignment keeps the key", `PASSWORD="correct-horse-battery"`, `PASSWORD="` + Placeholder(SecretAssignment) + `"`},
		{"short assignment value is not a secret", "token: abc", "token: abc"},
		{"private key block", "x\n" + fakeKey + "\ny", "x\n" + Placeholder(SecretPrivateKey) + "\ny"},
		{"two secrets", fakeAWS + " " + fakeGitHub, Placeholder(SecretAWSAccessKey) + " " + Placeholder(SecretGitHubToken)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reds := Redact([]byte(c.in))
			if string(got) != c.want {
				t.Fatalf("Redact = %q, want %q", got, c.want)
			}
			for _, r := range reds {
				if ph := string(got[r.Offset : r.Offset+r.Length]); ph != Placeholder(r.Kind) {
					t.Errorf("span %+v covers %q, want the placeholder", r, ph)
				}
				if r.OrigLen <= 0 {
					t.Errorf("span %+v has no original length", r)
				}
			}
			if err := checkSpans(reds, len(got)); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestRedactorAcrossChunks: a secret split at ANY byte boundary across two
// Feed calls must still be redacted, and the released stream (Feed…Flush)
// must equal the one-shot Redact of the whole input.
func TestRedactorAcrossChunks(t *testing.T) {
	inputs := []string{
		"export ANTHROPIC_API_KEY=" + fakeAnthropic + "\nnext line",
		"cloning https://bob:s3cr3tpass@git.example.com/r.git ...",
		"before\n" + fakeKey + "\nafter",
		"token " + fakeJWT + " end",
		"curl -H 'Authorization: Bearer abcdefghijklmnop1234' https://x\n",
		"\rPassword: correct-horse-battery\r\n",
		strings.Repeat("w ", 300) + "api_key=" + fakeOpenAI + " tail",
	}
	for _, in := range inputs {
		want, _ := Redact([]byte(in))
		for cut := 0; cut <= len(in); cut++ {
			var r Redactor
			a, _ := r.Feed([]byte(in[:cut]))
			b, _ := r.Feed([]byte(in[cut:]))
			c, _ := r.Flush()
			got := string(a) + string(b) + string(c)
			if got != string(want) {
				t.Fatalf("cut %d: got %q, want %q", cut, got, want)
			}
		}
	}
}

func TestRedactorHoldBounds(t *testing.T) {
	var r Redactor
	out, _ := r.Feed([]byte("line one\nhello wor"))
	if string(out) != "line one\n" || r.Held() != 9 {
		t.Fatalf("out=%q held=%d", out, r.Held())
	}
	// A token longer than the hold cap is released (redacted as-is).
	r = Redactor{}
	long := strings.Repeat("x", MaxRedactHold+1)
	out, _ = r.Feed([]byte(long))
	if len(out) != len(long) || r.Held() != 0 {
		t.Fatalf("long token: out=%d held=%d", len(out), r.Held())
	}
	// An unterminated private key is held, then redacted to the end on Flush
	// (fail closed): no key material ever escapes.
	r = Redactor{}
	open := fake("log\n-----BEGIN ", "RSA PRIVATE KEY-----\nMIIEow")
	out, _ = r.Feed([]byte(open))
	if string(out) != "log\n" {
		t.Fatalf("open key released %q", out)
	}
	out, reds := r.Flush()
	if string(out) != Placeholder(SecretPrivateKey) || len(reds) != 1 {
		t.Fatalf("flush = %q %+v", out, reds)
	}
	// A key block bigger than MaxKeyHold is redacted, not leaked.
	r = Redactor{}
	huge := fake("-----BEGIN ", "EC PRIVATE KEY-----\n") + strings.Repeat("Q", MaxKeyHold+1)
	out, _ = r.Feed([]byte(huge))
	if bytes.Contains(out, []byte("QQQQ")) {
		t.Fatal("oversized key block leaked")
	}
	// A certificate is not a key: released normally.
	r = Redactor{}
	cert := "-----BEGIN CERTIFICATE-----\nMIIB\n"
	out, _ = r.Feed([]byte(cert))
	if string(out) != cert {
		t.Fatalf("certificate held/redacted: %q", out)
	}
}

func TestSecretInput(t *testing.T) {
	cases := []struct {
		name string
		mode TTYMode
		cred bool
		want bool
	}{
		{"getpass prompt", TTYMode{Canonical: true, Echo: false}, false, true},
		{"cooked echo", TTYMode{Canonical: true, Echo: true}, false, false},
		{"raw-mode TUI typing is not secret", TTYMode{Canonical: false, Echo: false}, false, false},
		{"raw-mode TUI with credential prompt", TTYMode{Canonical: false, Echo: false}, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SecretInput(c.mode, c.cred); got != c.want {
				t.Fatalf("SecretInput = %v, want %v", got, c.want)
			}
		})
	}
	in, _ := RedactInput([]byte("hunter2-password\r"), OriginHuman, TTYMode{Canonical: true}, false)
	if !in.Secret || in.Data != nil || in.SecretLen != 17 {
		t.Fatalf("typed secret kept data: %+v", in)
	}
	in, reds := RedactInput([]byte("use "+fakeGitHub+"\r"), OriginHuman, TTYMode{}, false)
	if in.Secret || bytes.Contains(in.Data, []byte(fakeGitHub)) || len(reds) != 1 {
		t.Fatalf("pasted token not redacted: %+v", in)
	}
	e := Event{V: SchemaVersion, SessionID: "s", Seq: 1, Kind: KindInput, Input: &in, Redactions: reds, Redacted: true}
	if err := e.Validate(); err != nil {
		t.Fatalf("redacted input event invalid: %v", err)
	}
}
