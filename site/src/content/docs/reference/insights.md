---
title: Insights
description: Mine agent history for recurring patterns, slow work, and parallelization opportunities.
---

`warden insights` aggregates your archived agent history into a report — recurring
task shapes, slow or failure-prone work, and opportunities to parallelize — so you
can see how the fleet actually behaves over time. Gated by the `insights` config
setting (default on).

```sh
warden insights
warden insights --json
```

The aggregation is **deterministic** (pure history math); when `local_llm` is on, a
local model adds a short narrative on top — it never invents numbers, and a missing
model just yields the deterministic report. Also exposed as the `insights` MCP tool
so an orchestrator can ask for the same summary.
