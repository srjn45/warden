package decisionrouter

import (
	"path/filepath"
	"strings"

	"github.com/srjn45/warden/internal/ptyevent"
)

// ExtractCapabilities extracts normalized capabilities from subject and target strings.
func ExtractCapabilities(subject, target string, isTrust bool) []ptyevent.Capability {
	if isTrust {
		return []ptyevent.Capability{ptyevent.CapTrust}
	}

	s := strings.ToLower(subject)
	t := strings.ToLower(target)
	capsMap := make(map[ptyevent.Capability]bool)

	// Trust checks
	if strings.Contains(s, "trust") || strings.Contains(t, "trust") {
		capsMap[ptyevent.CapTrust] = true
	}

	// Destructive checks (take priority or augment other capabilities)
	if isDestructiveCommand(t) || strings.Contains(s, "destructive") {
		capsMap[ptyevent.CapDestructive] = true
	}

	// Credential checks
	if isCredentialCommand(t) || strings.Contains(s, "credential") || strings.Contains(s, "auth") {
		capsMap[ptyevent.CapCredential] = true
	}

	// Publish checks
	if isPublishCommand(t) || strings.Contains(s, "publish") {
		capsMap[ptyevent.CapPublish] = true
	}

	// Network checks
	if isNetworkCommand(t) || strings.Contains(s, "net") || strings.Contains(s, "web") || strings.Contains(s, "url") {
		capsMap[ptyevent.CapNetwork] = true
	}

	// Write checks
	if isWriteCommand(s, t) {
		capsMap[ptyevent.CapWrite] = true
	}

	// Read checks
	if isReadCommand(s, t) {
		capsMap[ptyevent.CapRead] = true
	}

	// Execute checks
	if isExecuteCommand(s, t) {
		capsMap[ptyevent.CapExecute] = true
	}

	// If tool / target indicated permission but none of standard mapped:
	if len(capsMap) == 0 {
		if subject != "" || target != "" {
			capsMap[ptyevent.CapOther] = true
		}
	}

	// Return deterministic ordered slice
	var out []ptyevent.Capability
	order := []ptyevent.Capability{
		ptyevent.CapRead,
		ptyevent.CapWrite,
		ptyevent.CapExecute,
		ptyevent.CapNetwork,
		ptyevent.CapCredential,
		ptyevent.CapPublish,
		ptyevent.CapDestructive,
		ptyevent.CapTrust,
		ptyevent.CapOther,
	}
	for _, c := range order {
		if capsMap[c] {
			out = append(out, c)
		}
	}
	return out
}

func isDestructiveCommand(t string) bool {
	markers := []string{
		"rm ", "rm -", "rmdir", "delete", "remove", "drop ", "truncate",
		"reset --hard", "wipe", "destroy", "format", "purge", "--force",
		" -f ", " -rf ", "mkfs", "dd if=",
	}
	for _, m := range markers {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

func isCredentialCommand(t string) bool {
	markers := []string{
		"ssh-add", "keychain", "vault", "secret", "login", "auth",
		"token", "passwd", "credential", "id_rsa", "id_ed25519",
	}
	for _, m := range markers {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

func isPublishCommand(t string) bool {
	markers := []string{
		"git push", "npm publish", "docker push", "cargo publish",
		"twine upload", "mvn deploy", "gradle publish", "helm push",
		"release", "deploy",
	}
	for _, m := range markers {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

func isNetworkCommand(t string) bool {
	markers := []string{
		"curl", "wget", "http://", "https://", "fetch", "git clone",
		"git fetch", "git pull", "git push", "ssh ", "ping ",
		"nc ", "netcat", "socat", "socket", "api.", "download",
	}
	for _, m := range markers {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

func isWriteCommand(s, t string) bool {
	if strings.Contains(s, "edit") || strings.Contains(s, "write") || strings.Contains(s, "patch") {
		return true
	}
	markers := []string{
		"touch ", "mkdir", "echo ", "tee ", "sed -i", "mv ", "cp ",
	}
	for _, m := range markers {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

func isReadCommand(s, t string) bool {
	if strings.Contains(s, "read") || strings.Contains(s, "view") || strings.Contains(s, "glob") {
		return true
	}
	markers := []string{
		"cat ", "less ", "more ", "head ", "tail ", "grep ", "find ", "ls ", "dir ",
	}
	for _, m := range markers {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

func isExecuteCommand(s, t string) bool {
	if strings.Contains(s, "bash") || strings.Contains(s, "command") || strings.Contains(s, "exec") ||
		strings.Contains(s, "sh") || strings.Contains(s, "cli") {
		return true
	}
	markers := []string{
		"git ", "make ", "go ", "python", "node ", "npm ", "cargo ",
		"pytest", "yarn ", "pip ", "docker ", "sh ",
	}
	for _, m := range markers {
		if strings.Contains(t, m) {
			return true
		}
	}
	return false
}

// IsTargetInWorkspace checks if target path resides within the session workdir.
func IsTargetInWorkspace(target, workdir string) bool {
	if workdir == "" || target == "" {
		return true
	}
	// Extract first path-like argument from target if it contains arguments
	cleanTarget := strings.TrimSpace(target)
	fields := strings.Fields(cleanTarget)
	for _, f := range fields {
		if strings.Contains(f, "/") || strings.Contains(f, "\\") {
			cleanTarget = f
			break
		}
	}

	cleanWorkdir := filepath.Clean(workdir)
	if !filepath.IsAbs(cleanTarget) {
		// Relative path inside worktree
		cleanTarget = filepath.Clean(filepath.Join(cleanWorkdir, cleanTarget))
	} else {
		cleanTarget = filepath.Clean(cleanTarget)
	}

	rel, err := filepath.Rel(cleanWorkdir, cleanTarget)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..") && rel != ".."
}

// InferOptionSemantic determines the semantics of an option label given class and context.
// Invariant: Never infer an answer merely from y/n text without semantic verification.
func InferOptionSemantic(class ptyevent.PromptClass, idx int, label string, totalOptions int) ptyevent.OptionSemantic {
	sem, _ := inferOptionSemantic(class, idx, label, totalOptions)
	return sem
}

// inferOptionSemantic is InferOptionSemantic plus a flag that is true when the
// meaning came from the option's POSITION alone (first = allow, last = deny)
// rather than from its wording. A positional reading is a guess; the
// normalizer treats a permission menu that needed one as ambiguous.
func inferOptionSemantic(class ptyevent.PromptClass, idx int, label string, totalOptions int) (ptyevent.OptionSemantic, bool) {
	if class == ptyevent.ClassDecision {
		l := strings.ToLower(strings.TrimSpace(label))
		if strings.HasPrefix(l, "type ") || strings.Contains(l, "freeform") || strings.Contains(l, "custom") {
			return ptyevent.SemFreeform, false
		}
		return ptyevent.SemChoice, false
	}

	l := strings.ToLower(strings.TrimSpace(label))

	// Explicit standing allow tokens
	if strings.Contains(l, "always") || strings.Contains(l, "don't ask again") ||
		strings.Contains(l, "remember") || strings.Contains(l, "trust this folder") ||
		strings.Contains(l, "trust this directory") || strings.Contains(l, "trust project") {
		return ptyevent.SemAllowAlways, false
	}

	// Explicit session grant tokens
	if strings.Contains(l, "for the rest of this session") || strings.Contains(l, "this session") ||
		strings.Contains(l, "allow session") {
		return ptyevent.SemAllowSession, false
	}

	// Feedback tokens
	if strings.Contains(l, "tell ") || strings.Contains(l, "feedback") {
		return ptyevent.SemDenyFeedback, false
	}

	// Cancel / Abort tokens
	if strings.Contains(l, "cancel") || strings.Contains(l, "abort") || strings.Contains(l, "exit") {
		return ptyevent.SemCancel, false
	}

	// Freeform input tokens
	if strings.HasPrefix(l, "type ") || strings.Contains(l, "freeform") || strings.Contains(l, "other") {
		return ptyevent.SemFreeform, false
	}

	// Explicit Deny tokens
	if strings.HasPrefix(l, "deny") || strings.Contains(l, "reject") || strings.Contains(l, "refuse") ||
		strings.Contains(l, "don't allow") || strings.Contains(l, "do not allow") {
		return ptyevent.SemDeny, false
	}

	// Standard Allow tokens (qualified)
	if strings.HasPrefix(l, "allow") || strings.HasPrefix(l, "approve") ||
		strings.Contains(l, "proceed") || strings.Contains(l, "confirm") ||
		strings.Contains(l, "run command") || strings.Contains(l, "yes, proceed") ||
		strings.Contains(l, "yes, continue") {
		return ptyevent.SemAllowOnce, false
	}

	// Sole "Yes" or "No" labels when structured in a standard 2-option permission menu
	// Note: We only treat them as allow/deny if context is a validated permission class
	if l == "yes" || strings.HasPrefix(l, "yes,") {
		return ptyevent.SemAllowOnce, false
	}
	if l == "no" || strings.HasPrefix(l, "no,") {
		return ptyevent.SemDeny, false
	}

	// If unmapped in a multi-option permission, default to choice/unknown
	if idx == 1 && totalOptions > 1 {
		return ptyevent.SemAllowOnce, true
	}
	if idx == totalOptions && totalOptions > 1 {
		return ptyevent.SemDeny, true
	}

	return ptyevent.SemChoice, true
}
