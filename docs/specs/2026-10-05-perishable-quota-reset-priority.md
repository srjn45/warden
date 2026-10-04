# Spec: Perishable Quota & Reset-Priority Model Router

## Problem Statement

When assigning an `(aicli, model)` pair at agent spawn or during reactive rate-limit recovery, Warden evaluates candidate models by tier, filters out rate-limited models, and ranks eligible candidates by **maximum remaining headroom** ($100\% - \text{Used}\%$).

However, subscription provider quotas (e.g. Claude 5-hour rolling window, Codex 5-hour window, Antigravity 5-hour pool) are **perishable**:
- If Provider A's window resets in **25 minutes** with **40% headroom**, and
- Provider B's window resets in **4.5 hours** with **80% headroom**,
Warden currently assigns Provider B because $80\% > 40\%$.

This causes **perishable quota waste**:
1. Provider B's long-lasting quota is unnecessarily burned.
2. In 25 minutes, Provider A's remaining 40% simply expires and resets to 100%, meaning that 40% capacity was completely lost.

## Design

### 1. The Perishable Priority Rule
Candidates whose quota windows reset in the near future should be utilized first, preserving long-term quota on other providers, provided they have enough headroom to reliably complete tasks.

### 2. Constraints & Guardrails

1. **Safety Floor (`MinUsableHeadroom = 10%`)**:
   If a bucket resets in 5 minutes but only has 1% headroom remaining, spawning an agent on it risks an immediate rate-limit stall within 20 seconds. Candidates with $< 10\%$ headroom do not qualify for urgent priority.

2. **Urgency Horizon (`UrgentHorizon = 1 hour`)**:
   A 10-minute reset difference between windows that are both 5 hours away is statistically insignificant compared to remaining headroom. Therefore, reset prioritization applies within an urgent horizon ($\le 1\text{ hour}$).

3. **Bottleneck Window Association**:
   When a candidate has multiple overlapping quota limits (e.g. 5-hour short window vs. weekly window), the relevant `ResetsAt` is associated with the **bottleneck limit** (the limit yielding the minimum headroom).

4. **Unknown Telemetry Fallback**:
   Candidates with missing or unknown `ResetsAt` sort cleanly after candidates with known impending resets, using standard headroom ranking.

### 3. Sorting Algorithm

Eligible candidates within the resolved tier are partitioned into two priority classes:

```
Class A (Impending Resets):
  Condition:
    ResetsAt != nil
    AND ResetsAt <= now + 1 hour
    AND ResetsAt > now
    AND Headroom >= 0.10 (10%)
  Sort:
    ORDER BY ResetsAt ASC, Headroom DESC

Class B (Standard Candidates):
  Condition:
    All other eligible candidates (farther reset, unknown reset, or headroom < 0.10)
  Sort:
    ORDER BY Headroom DESC, RoundRobinTieBreaker
```

The winner is selected from Class A if non-empty; otherwise, from Class B.

## Verification
- Unit tests in `internal/router/resolver_test.go` asserting perishable quota selection, safety floor fallback, and tie-breaking.
- Unit tests in `internal/recovery/rank_test.go`.
- `make verify-fast` passing cleanly.
