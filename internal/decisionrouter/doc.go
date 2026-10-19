// Package decisionrouter implements the normalized prompt classifier and decision
// router per contract spec (docs/specs/2026-10-19-pty-runtime-contract.md §9, §10, §11, §14).
//
// It combines:
//   - Backend parsers, versioned learned prompt shapes, and Fast-Brain prompt recognition;
//   - Capability extraction and bounded classification (permission, strategic decision,
//     informational/idle, unknown);
//   - Permission decision engine (ptyevent.Decide) with posture and allowlist tables;
//   - Strategic decision routing (plain agents & pipelines to human inbox; autopilot runs
//     to Tier-1 manager / brain consult);
//   - Downgrade guards (circuit breaker and arbiter, which may only downgrade, never upgrade);
//   - Time-of-check/time-of-use (TOCTOU) revalidation (ptyevent.Revalidate);
//   - Input delivery via arbitration (ptyevent.Arbitrate);
//   - DecisionEvent journaling via Stream.Emit.
package decisionrouter
