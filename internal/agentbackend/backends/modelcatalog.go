package backends

// This file contains the small amount of shared plumbing used by the provider
// model-catalog adapters. Keeping it here makes the safety boundary auditable:
// these are metadata commands only, with a deadline and a hard output cap.

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"
)

const (
	modelCatalogTimeout = 20 * time.Second
	modelCatalogMaxOut  = 4 << 20
)

var errCatalogOutputLimit = errors.New("model catalog output exceeds limit")

// modelCatalogRun is injectable so parser tests never invoke provider CLIs.
// It intentionally accepts argv rather than a shell string; ids and profiles are
// never interpolated into a shell.
var modelCatalogRun = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	if _, ok := catalogMetadataCommand(name, args); !ok {
		return nil, errors.New("model catalog command is not allowlisted")
	}
	ctx, cancel := context.WithTimeout(ctx, modelCatalogTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// Do not retain unbounded diagnostics. It is deliberately discarded; the
	// classified command outcome is enough for the operator surface.
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	b, readErr := io.ReadAll(io.LimitReader(stdout, modelCatalogMaxOut+1))
	if len(b) > modelCatalogMaxOut {
		// LimitReader deliberately stops before EOF. Kill before Wait so a noisy
		// child cannot remain blocked trying to fill its stdout pipe.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return b[:modelCatalogMaxOut], errCatalogOutputLimit
	}
	waitErr := cmd.Wait() // reaps the child on every non-truncation path.
	if readErr != nil {
		return nil, readErr
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return b, waitErr
}

func catalogMetadataCommand(name string, args []string) (string, bool) {
	key := name + " " + strings.Join(args, " ")
	switch key {
	case "cursor-agent --list-models", "cursor-agent --version", "agy models", "agy --version", "opencode models", "opencode --version", "crush models", "crush --version", "codex --version":
		return key, true
	default:
		return "", false
	}
}

func catalogCommandError(err error) *catalogError {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &catalogError{class: "deadline"}
	}
	if errors.Is(err, context.Canceled) {
		return &catalogError{class: "cancelled"}
	}
	if errors.Is(err, errCatalogOutputLimit) {
		return &catalogError{class: "partial_result"}
	}
	var ee *exec.Error
	if errors.As(err, &ee) {
		return &catalogError{class: "not_installed"}
	}
	return &catalogError{class: "command_failed"}
}

type catalogError struct{ class string }
