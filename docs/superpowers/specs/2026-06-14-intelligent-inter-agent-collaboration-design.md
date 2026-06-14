# Intelligent Inter-Agent Collaboration System Design

**Date:** 2026-06-14  
**Feature:** Intelligent inter-agent communication & collaboration  
**Status:** Design Complete  
**Estimated Effort:** 2-3 weeks

---

## Overview

Enable warden agents to become aware of each other's work, detect overlapping work, prevent file conflicts, and coordinate automatically. This system operates primarily daemon-side using goroutines to minimize token usage, with both CLI and MCP interfaces for maximum compatibility.

**Core Capabilities:**
1. **File Conflict Prevention** - Detect when multiple agents edit the same files
2. **Work Deduplication** - Identify overlapping/duplicate work between agents
3. **Branch Monitoring** - Track CI status, merge state, and main/master updates
4. **Active Coordination** - Push warnings to affected agents automatically

**Key Constraint:** Must work in MCP-restricted environments via CLI-first design.

---

## Architecture

### High-Level Design

```
┌─────────────────────────────────────────────────────────┐
│                    Warden Daemon                         │
│                                                          │
│  ┌──────────────┐  ┌───────────────┐  ┌──────────────┐ │
│  │CollabMonitor │  │ BranchTracker │  │ OverlapDetect│ │
│  │  (goroutine) │  │  (goroutine)  │  │  (goroutine) │ │
│  └──────┬───────┘  └───────┬───────┘  └──────┬───────┘ │
│         │                  │                  │         │
│         └──────────────────┼──────────────────┘         │
│                            ▼                            │
│                    ┌──────────────┐                     │
│                    │  CollabStore │                     │
│                    └──────┬───────┘                     │
│                           │                             │
│              ┌────────────┴────────────┐                │
│              ▼                         ▼                │
│       ┌─────────────┐          ┌─────────────┐         │
│       │ HTTP Routes │          │  MCP Tools  │         │
│       │ /collab/*   │          │             │         │
│       └─────────────┘          └─────────────┘         │
└─────────────────────────────────────────────────────────┘
              ▲                         ▲
              │                         │
    ┌─────────┴─────────┐      ┌────────┴────────┐
    │  warden CLI       │      │  Agent (Claude) │
    │  warden collab    │      │  via MCP tools  │
    │  warden branch    │      │                 │
    └───────────────────┘      └─────────────────┘
```

### Components

**Three daemon-side goroutines:**

1. **CollabMonitor** - Watches file changes using FSNotify + git diff polling
2. **BranchTracker** - Monitors git branches for CI status, merge state, main/master updates
3. **OverlapDetector** - Analyzes subjects, plans, and file overlap to detect duplicate work

**Storage:**

- **CollabStore** - In-memory state (similar to existing ctxstore/mailbox)

**Interfaces:**

- **HTTP Routes** - For CLI and web UI (`/collab/*`, `/branches/*`)
- **MCP Tools** - For agents to query collaboration state

---

## Data Structures

### CollabStore

```go
// CollabStore - persistence for collaboration state
type CollabStore struct {
    mu sync.RWMutex
    
    // File tracking: which agent is working on which files
    FileAgents map[string][]FileEdit  // file path -> list of agents editing it
    
    // Work overlap detection
    WorkOverlaps []WorkOverlap  // detected overlapping work
    
    // Branch tracking
    Branches map[string]*BranchStatus  // branch name -> status
    
    // Collaboration groups (explicit)
    CollabGroups map[string]*CollabGroup  // group name -> members
}
```

### FileEdit

```go
type FileEdit struct {
    AgentID      string
    SessionName  string  // human-friendly name
    FirstSeen    time.Time
    LastModified time.Time
    Worktree     string  // path to worktree
}
```

Tracks which agent is working on a file and when they started/last modified it.

### WorkOverlap

```go
type WorkOverlap struct {
    Agent1       string
    Agent2       string
    Reason       string  // "similar subjects", "same files", "plan overlap"
    Confidence   float64 // 0.0-1.0
    Details      string  // human-readable explanation
    DetectedAt   time.Time
    Dismissed    bool    // user can dismiss false positives
}
```

Represents detected duplicate/overlapping work between two agents. Includes confidence score and dismissal mechanism for false positives.

### BranchStatus

```go
type BranchStatus struct {
    Name         string
    Agents       []string  // agents working on this branch
    RemoteURL    string    // for GitHub API calls
    CIStatus     string    // "passing", "failing", "pending", "unknown"
    CIRunURL     string    // link to CI run
    LastCICheck  time.Time
    MergeStatus  string    // "open", "merged", "rebased"
    BehindMain   int       // commits behind main/master
    LastChecked  time.Time
}
```

Tracks the lifecycle of a git branch from creation through merge/rebase, including CI status.

### CollabGroup

```go
type CollabGroup struct {
    Name      string
    Agents    []string
    CreatedAt time.Time
}
```

Explicit collaboration groups created by users to mark agents as intentionally working together (not duplicating).

---

## Component Details

### 1. CollabMonitor - File Conflict Detection

**Responsibility:** Detect when multiple agents are editing the same files in real-time.

**Strategy:**
- **FSNotify watchers** on each agent's worktree (subsecond detection)
- **Git diff poller** every 10 seconds (reconciliation, authoritative)

**Why:** FSNotify catches changes instantly but can miss events (buffer overflow, race conditions). Git diff is slower but authoritative. Together they're robust.

**How to apply:** Use FSNotify for low-latency warnings, git diff for periodic reconciliation and structured diff data.

**Algorithm:**

```
Every 10 seconds:
  1. Fetch all active sessions from store
  2. For each session with a worktree:
     a. Run `git diff --name-only HEAD` to get modified files
     b. Update CollabStore.FileAgents map
     c. Ensure FSNotify watcher exists for this worktree
  3. Remove watchers for terminated sessions
  4. Detect conflicts (files with multiple agents)
  5. Send active warnings to affected agents via inbox

FSNotify events (async):
  On file write event:
    - Trigger immediate conflict check for that file
    - Update CollabStore.FileAgents
```

**Conflict Warning Format:**

```
⚠️  Collaboration Warning: File Conflict
File: internal/auth/auth.go
Also being edited by: agent-456 (refactor-jwt)
Consider coordinating to avoid merge conflicts.
```

**Implementation Details:**

- FSNotify watches entire worktree recursively (excluding `.git/`)
- Each agent gets one watcher (created on first file modification)
- Watchers cleaned up when agent terminates
- Warnings sent via existing `mailbox.Store` to agent inbox
- SSE events published for web UI real-time updates

**Edge Cases:**

- Agent without worktree: skip (prompt-mode agents)
- Watcher creation failure: log error, fallback to git diff polling only
- Large repos: FSNotify may hit OS limits (inotify instances) - document this limitation

---

### 2. OverlapDetector - Work Deduplication

**Responsibility:** Detect when multiple agents are working on overlapping features before conflicts occur.

**Strategy:**
- Compare agent subjects (string similarity)
- Analyze file overlap (percentage of common files)
- Parse and compare implementation plans

**Why:** Preventative rather than reactive - catch duplication before wasted work happens.

**Algorithm:**

```
Every 30 seconds:
  1. Fetch all active (non-terminal) sessions
  2. For each pair of agents (i, j):
     a. Skip if they're collaborating (same pipeline/group/branch, or have message exchange)
     b. Calculate overlap score:
        - Subject similarity (token-based, 0.0-1.0)
        - File overlap percentage (0.0-1.0)
        - Plan similarity (token-based, 0.0-1.0)
     c. Weighted score: subject*0.3 + files*0.4 + plan*0.3
     d. If score > 0.6, record WorkOverlap and notify both agents
```

**Collaboration Detection Hierarchy:**

Agents are considered collaborating (not duplicating) if any of:

1. **Explicit** - Same pipeline, same collab group
2. **Inferred** - Message exchange (via `send_message`/`read_inbox`), shared context keys (common prefix in `ctx_set`/`ctx_get`)
3. **Heuristic** - Same git branch

**Overlap Warning Format:**

```
⚠️  Potential Work Overlap Detected (85% confidence)
Agent: agent-456 (refactor-jwt)
Overlap reason: similar subjects, 73% file overlap

Agent 456 is working on:
- Subject: refactor JWT validation
- Files: auth.go, token.go, middleware.go

Suggestion: Consider asking agent agent-456 about their progress:
  warden send agent-456 "What's your current status on this work?"

To dismiss this warning: warden collab dismiss-overlap agent-123-agent-456
```

**Subject Similarity:**

Simple token-based Jaccard similarity:
```
tokens1 = tokenize("refactor JWT authentication")
tokens2 = tokenize("update auth system")
similarity = len(intersection(tokens1, tokens2)) / len(union(tokens1, tokens2))
```

**Future Enhancement:** Use embeddings (sentence-transformers) for semantic similarity.

**File Overlap:**

```
files1 = git diff --name-only HEAD in worktree1
files2 = git diff --name-only HEAD in worktree2
overlap = len(intersection(files1, files2)) / len(union(files1, files2))
```

**Plan Analysis:**

- Look for plan files in `docs/superpowers/specs/`
- Match by agent ID or session name in filename
- Read plan contents, apply token-based similarity
- If no plan files found, skip (score = 0.0)

**Why plan analysis:** Catch overlap **before** any code is written, based on design intent.

---

### 3. BranchTracker - Git Branch Monitoring

**Responsibility:** Monitor branches for CI status, merge state, and detect when main/master updates.

**Strategy:**
- Poll GitHub Actions API every 2 minutes
- Check merge status via `git branch --contains`
- Track commits behind main via `git rev-list`

**Why:** Keep users informed of CI failures and branch drift without manual checking.

**Algorithm:**

```
Every 2 minutes:
  1. Get all active sessions with worktrees
  2. Extract unique branches (group by branch name)
  3. For each branch:
     a. Check CI status via GitHub API
     b. Check merge status: git branch -r --contains <branch>
     c. Check commits behind: git rev-list --count main..<branch>
     d. Update BranchStatus in store
     e. Detect state transitions:
        - CI passed → failed: send desktop notification with "Debug" CTA
        - Commits behind increased: notify agents to rebase
        - Branch merged/rebased: stop tracking
```

**Branch Discovery:**

Only track branches associated with agent worktrees:
- When agent spawned with worktree, add its branch to tracker
- When agent terminates, check if other agents still on that branch
- If no agents remain and branch is merged, remove from tracker

**GitHub API Integration:**

```go
type GitHubClient struct {
    token string  // from GITHUB_TOKEN env var
    cache map[string]cachedStatus  // branch -> cached result (TTL: 2 min)
}

func (gh *GitHubClient) GetCIStatus(remoteURL, branch string) (status, runURL string) {
    // 1. Parse owner/repo from remoteURL (e.g., git@github.com:user/repo.git)
    owner, repo := parseGitHubURL(remoteURL)
    
    // 2. Get latest commit SHA for branch
    // GET /repos/{owner}/{repo}/git/refs/heads/{branch}
    
    // 3. Get commit status
    // GET /repos/{owner}/{repo}/commits/{sha}/status
    // Returns: "success", "failure", "pending"
    
    // 4. Get workflow runs
    // GET /repos/{owner}/{repo}/actions/runs?branch={branch}&per_page=1
    // Get most recent run URL
    
    return status, runURL
}
```

**CI Failure Notification:**

Desktop notification (platform-specific):
- **macOS:** `osascript -e 'display notification "..." with title "..."'`
- **Linux:** `notify-send "..." "..."`
- **Fallback:** Store in pending notifications queue, show on next `warden branch notifications`

**Notification Format:**

```
Title: CI Failed
Message: CI failed on branch feature/auth-refactor
Action: Debug with Agent
Command: warden start "Debug CI failure on branch feature/auth-refactor. Check logs at https://github.com/..." --branch feature/auth-refactor
```

**Main/Master Update Notification:**

When `git rev-list --count main..<branch>` increases, send to all agents on that branch:

```
ℹ️  Branch Update: main/master has 3 new commit(s)
Your branch 'feature/auth-refactor' is now 3 commit(s) behind.
Consider rebasing when convenient.
```

**Why notify, not auto-rebase:** Rebasing can disrupt in-progress work. User controls when to rebase.

**Merge Detection:**

```bash
# Check if branch is merged
git branch -r --merged origin/main | grep origin/feature/auth-refactor

# Or check if branch ref exists
git ls-remote origin refs/heads/feature/auth-refactor
# Empty output = branch deleted (merged or force-deleted)
```

When branch is merged/rebased:
- Remove from `CollabStore.Branches`
- Stop tracking (no more CI checks)
- Agents on that branch are unaffected (they still work in their worktree)

---

## CLI Commands

### Collaboration Commands

```bash
# Overall status
warden collab status
# Output:
# Collaboration Status
# 
# File Conflicts (2):
#   internal/auth.go - agents: agent-123, agent-456
#   pkg/token.go - agents: agent-123, agent-789
# 
# Work Overlaps (1):
#   agent-456 ↔ agent-789 (85% confidence)
#   Reason: similar subjects, 73% file overlap
# 
# Collaboration Groups (1):
#   auth-team: agent-123, agent-456

# File conflicts only
warden collab conflicts
# Output:
# File Conflicts:
#   internal/auth.go
#     - agent-123 (refactor-jwt): editing since 2026-06-14 10:30
#     - agent-456 (update-auth): editing since 2026-06-14 10:45

# Work overlaps only
warden collab overlaps
# Output:
# Work Overlaps:
#   agent-456 ↔ agent-789 (85% confidence)
#     Reason: similar subjects, 73% file overlap
#     Detected: 2026-06-14 10:50

# Collaboration groups
warden collab groups
# Output:
# Collaboration Groups:
#   auth-team: agent-123, agent-456 (created 2026-06-14 09:00)

# Check who is editing a file
warden collab who-is-editing internal/auth.go
# Output:
# Agents editing internal/auth.go:
#   - agent-123 (refactor-jwt): since 10:30
#   - agent-456 (update-auth): since 10:45

# Create collaboration group
warden collab create auth-team --agents agent-123,agent-456
# Output: Created collaboration group 'auth-team' with 2 agents

# Add agent to group
warden collab add auth-team --agent agent-789
# Output: Added agent-789 to collaboration group 'auth-team'

# Delete group
warden collab delete auth-team
# Output: Deleted collaboration group 'auth-team'

# Dismiss false positive overlap
warden collab dismiss-overlap agent-456-agent-789
# Output: Dismissed overlap between agent-456 and agent-789

# Query from inside agent session
warden collab agents-on internal/auth.go
# Same as who-is-editing, but designed for agents to call
```

### Branch Commands

```bash
# All tracked branches
warden branch status
# Output:
# Tracked Branches:
# 
# feature/auth-refactor
#   Agents: agent-123, agent-456
#   CI: passing (https://github.com/...)
#   Behind main: 0 commits
#   Last checked: 2026-06-14 11:00
# 
# feature/api-v2
#   Agents: agent-789
#   CI: failing (https://github.com/...)
#   Behind main: 3 commits
#   Last checked: 2026-06-14 11:00

# Specific branch
warden branch status feature/auth-refactor
# Output:
# Branch: feature/auth-refactor
#   Agents: agent-123 (refactor-jwt), agent-456 (update-auth)
#   CI Status: passing
#   CI Run: https://github.com/user/repo/actions/runs/12345
#   Last CI Check: 2026-06-14 11:00:00
#   Merge Status: open
#   Behind main: 0 commits

# Pending CI notifications
warden branch notifications
# Output:
# Pending Notifications:
# 
# 1. CI Failed - feature/api-v2
#    Time: 2026-06-14 10:55
#    Action: warden start "Debug CI failure on branch feature/api-v2. Check logs at https://..." --branch feature/api-v2
#    [d] Dismiss  [r] Run action
```

---

## MCP Tools for Agents

### Tool Definitions

**1. get_collaboration_status**

```json
{
  "name": "get_collaboration_status",
  "description": "Get current collaboration state: file conflicts, work overlaps, branch status",
  "parameters": {
    "scope": {
      "type": "string",
      "description": "Optional filter: 'files', 'overlaps', 'branches', or empty for all",
      "optional": true
    }
  }
}
```

**Returns:** JSON with conflicts, overlaps, branches

**2. who_is_editing_file**

```json
{
  "name": "who_is_editing_file",
  "description": "Check which agents are currently editing a specific file",
  "parameters": {
    "file_path": {
      "type": "string",
      "description": "Relative or absolute path to file"
    }
  }
}
```

**Returns:** List of agents editing the file with timestamps

**3. query_agent_work**

```json
{
  "name": "query_agent_work",
  "description": "Get details about what another agent is working on",
  "parameters": {
    "agent_id": {
      "type": "string",
      "description": "Session ID or name of agent to query"
    }
  }
}
```

**Returns:**
```
Agent: agent-456 (refactor-jwt)
Status: working
Subject: refactor JWT validation
Branch: feature/auth-refactor
Files being modified:
  internal/auth.go
  internal/token.go
  internal/middleware.go
```

**4. dismiss_work_overlap**

```json
{
  "name": "dismiss_work_overlap",
  "description": "Dismiss a false positive work overlap warning",
  "parameters": {
    "overlap_id": {
      "type": "string",
      "description": "Format: agent1-id_agent2-id"
    }
  }
}
```

**5. create_collab_group**

```json
{
  "name": "create_collab_group",
  "description": "Create an explicit collaboration group to mark agents as working together",
  "parameters": {
    "name": {
      "type": "string",
      "description": "Group name (alphanumeric, hyphens, underscores)"
    },
    "agents": {
      "type": "array",
      "items": {"type": "string"},
      "description": "List of agent IDs or names"
    }
  }
}
```

**6. get_branch_status**

```json
{
  "name": "get_branch_status",
  "description": "Get CI status, merge state, and commits behind main for a branch",
  "parameters": {
    "branch_name": {
      "type": "string",
      "description": "Branch name (e.g., feature/auth-refactor)"
    }
  }
}
```

**Returns:**
```
Branch: feature/auth-refactor
CI Status: passing
CI Run URL: https://github.com/...
Merge Status: open
Behind main: 0 commits
Last checked: 2026-06-14 11:00
```

---

## HTTP Routes

### Collaboration Routes

```
GET  /collab/status
  Query params: ?scope=files|overlaps|branches
  Returns: JSON with current collaboration state

GET  /collab/conflicts
  Returns: JSON array of file conflicts

GET  /collab/overlaps
  Returns: JSON array of work overlaps

GET  /collab/groups
  Returns: JSON array of collaboration groups

POST /collab/groups
  Body: {"name": "auth-team", "agents": ["agent-123", "agent-456"]}
  Returns: Created group

DELETE /collab/groups/:name
  Returns: 204 No Content

POST /collab/overlaps/:id/dismiss
  :id format: agent1-agent2
  Returns: 200 OK

GET  /collab/file/:path
  :path = base64-encoded file path
  Returns: List of agents editing this file
```

### Branch Routes

```
GET  /branches
  Returns: JSON array of all tracked branches

GET  /branches/:name
  :name = URL-encoded branch name
  Returns: BranchStatus JSON

GET  /branches/notifications
  Returns: Pending CI failure notifications
```

---

## Integration with Existing Systems

### Mailbox Integration

**How:** Conflict warnings and overlap notifications use existing `mailbox.Store`.

**Why:** Reuse proven messaging infrastructure, no need to build new delivery system.

**Format:**
```go
mailbox.Send(ctx, Message{
    From: "system",
    To:   agentID,
    Body: conflictWarning,
    Sent: time.Now(),
})
```

Agents receive via:
- MCP: `read_inbox` tool
- CLI: `warden inbox` command

### SSE/Hub Integration

**How:** CollabMonitor, BranchTracker, OverlapDetector call `hub.publish()` on state changes.

**Why:** Web UI updates in real-time when conflicts detected, no polling needed.

**Implementation:**
```go
// After detecting conflict
collabStore.AddConflict(conflict)
hub.publish()  // SSE clients receive update

// Web UI subscribes to /events
// Receives notification, fetches /collab/status, updates display
```

### Context Store Integration

**How:** Shared context usage signals collaboration.

**Detection:**
```go
// Agent A: ctx_set("auth.refactor.status", "in-progress")
// Agent B: ctx_get("auth.refactor.status")
// OverlapDetector infers: A and B are collaborating on auth.refactor
```

**Why:** Agents using shared context are coordinating, not duplicating.

### Poller Integration

**How:** CollabMonitor reuses session list from poller.

**Implementation:**
```go
type SessionLister interface {
    List(ctx context.Context) ([]*store.Session, error)
}

// CollabMonitor, BranchTracker, OverlapDetector all use this interface
// Daemon passes store.Store which implements it
```

**Why:** Single source of truth for active sessions, avoid duplicate queries.

**Lifecycle:** Similar to poller - runs in goroutine until ctx cancelled, cleans up on shutdown.

---

## Implementation Phases

### Phase 1: MVP (1 week)

**Goal:** Basic conflict detection with CLI

**Tasks:**
- [ ] Create `internal/collab` package
- [ ] Implement `CollabStore` with basic data structures
- [ ] Implement `CollabMonitor` with git diff polling (no FSNotify yet)
- [ ] File-level conflict detection
- [ ] CLI commands: `warden collab status`, `warden collab conflicts`, `warden collab who-is-editing`
- [ ] HTTP routes: `GET /collab/status`, `GET /collab/conflicts`
- [ ] Integration with mailbox for warnings
- [ ] Unit tests for CollabStore and CollabMonitor
- [ ] Update `internal/daemon/server.go` to start CollabMonitor goroutine

**Deliverable:** Users can see file conflicts in CLI and web UI, agents receive warnings via inbox.

### Phase 2: Real-time Detection (2-3 days)

**Goal:** Add FSNotify for subsecond conflict detection

**Tasks:**
- [ ] Add `github.com/fsnotify/fsnotify` dependency
- [ ] Implement FSNotify watcher creation/cleanup in CollabMonitor
- [ ] Handle watcher events (file writes trigger immediate check)
- [ ] Test with large repos (document inotify limits)
- [ ] Add MCP tools: `who_is_editing_file`, `get_collaboration_status`
- [ ] Update MCP server (`internal/mcp/server.go`) with new tools

**Deliverable:** Agents warned within 1 second of file conflict.

### Phase 3: Collaboration Groups & Overlap Detection (3-4 days)

**Goal:** Detect duplicate work and allow explicit collaboration marking

**Tasks:**
- [ ] Implement collaboration group management in CollabStore
- [ ] CLI commands: `warden collab create`, `warden collab add`, `warden collab delete`, `warden collab groups`
- [ ] HTTP routes: `POST /collab/groups`, `DELETE /collab/groups/:name`
- [ ] Implement `OverlapDetector` goroutine
- [ ] Subject similarity (token-based Jaccard)
- [ ] File overlap analysis
- [ ] Plan file discovery and comparison
- [ ] Collaboration detection (pipeline, group, branch, messages, context)
- [ ] Overlap warnings to agent inboxes
- [ ] CLI commands: `warden collab overlaps`, `warden collab dismiss-overlap`
- [ ] MCP tools: `query_agent_work`, `dismiss_work_overlap`, `create_collab_group`
- [ ] Unit tests for OverlapDetector

**Deliverable:** System detects and warns about duplicate work before code is written.

### Phase 4: Branch Monitoring (4-5 days)

**Goal:** Track CI status and branch lifecycle

**Tasks:**
- [ ] Implement `BranchTracker` goroutine
- [ ] GitHub API client (`internal/collab/github.go`)
  - [ ] Parse GitHub remote URL (SSH/HTTPS)
  - [ ] GET commit status
  - [ ] GET workflow runs
  - [ ] Rate limiting (5000 req/hour)
  - [ ] Caching (2-minute TTL)
- [ ] Git commands for merge/behind detection
  - [ ] `git branch -r --merged`
  - [ ] `git rev-list --count main..<branch>`
- [ ] Desktop notification system
  - [ ] macOS: `osascript`
  - [ ] Linux: `notify-send`
  - [ ] Fallback: pending notifications queue
- [ ] Main/master update detection and agent notifications
- [ ] CLI commands: `warden branch status`, `warden branch notifications`
- [ ] HTTP routes: `GET /branches`, `GET /branches/:name`, `GET /branches/notifications`
- [ ] MCP tool: `get_branch_status`
- [ ] Unit tests for BranchTracker and GitHubClient
- [ ] Integration test with real GitHub repo (CI)

**Deliverable:** Users notified of CI failures with one-click debug option, agents notified of branch drift.

### Phase 5: Polish & Web UI (2-3 days)

**Goal:** Production-ready with web dashboard

**Tasks:**
- [ ] Web UI: Collaboration dashboard page
  - [ ] Real-time conflict list with SSE updates
  - [ ] Work overlap warnings with dismiss button
  - [ ] Branch status table with CI badges
  - [ ] Collaboration group management UI
- [ ] Performance optimization
  - [ ] Index FileAgents by file path for O(1) lookup
  - [ ] Cache subject similarity scores
  - [ ] Batch git operations
- [ ] Documentation
  - [ ] Update README with collaboration features
  - [ ] Add examples to USAGE.md
  - [ ] Document GitHub token setup
  - [ ] Document inotify limits on Linux
- [ ] Integration tests
  - [ ] End-to-end conflict detection scenario
  - [ ] Overlap detection with real plan files
  - [ ] Branch tracking with mock GitHub API
- [ ] Error handling and logging
  - [ ] Structured logging for all monitors
  - [ ] Graceful degradation (FSNotify failure, GitHub API down)

**Deliverable:** Production-ready collaboration system with polished UX.

---

## Configuration

### Environment Variables

```bash
# GitHub API token for CI status checks
GITHUB_TOKEN=ghp_xxxxxxxxxxxx

# Collaboration monitoring (default: enabled)
WARDEN_COLLAB_ENABLED=true

# File conflict detection interval (default: 10s)
WARDEN_COLLAB_FILE_CHECK_INTERVAL=10s

# Work overlap detection interval (default: 30s)
WARDEN_COLLAB_OVERLAP_CHECK_INTERVAL=30s

# Branch tracking interval (default: 2m)
WARDEN_COLLAB_BRANCH_CHECK_INTERVAL=2m

# Overlap confidence threshold (default: 0.6)
WARDEN_COLLAB_OVERLAP_THRESHOLD=0.6

# Disable FSNotify (fallback to git diff only)
WARDEN_COLLAB_NO_FSNOTIFY=false
```

### GitHub Token Setup

**Required permissions:**
- `repo:status` - Read commit status
- `actions:read` - Read workflow runs

**Setup:**
```bash
# 1. Generate token at https://github.com/settings/tokens
# 2. Select scopes: repo:status, actions:read
# 3. Export token
export GITHUB_TOKEN=ghp_xxxxxxxxxxxx

# 4. Add to shell profile for persistence
echo 'export GITHUB_TOKEN=ghp_xxxxxxxxxxxx' >> ~/.bashrc
```

---

## Performance Considerations

### FSNotify Limits

**Linux:** inotify has per-user limits on watches.

**Default:** 8192 watches per user (`/proc/sys/fs/inotify/max_user_watches`)

**Large repos:** A repo with 10,000 files needs 10,000 watches.

**Solution:**
```bash
# Increase limit (temporary)
sudo sysctl fs.inotify.max_user_watches=524288

# Permanent
echo "fs.inotify.max_user_watches=524288" | sudo tee -a /etc/sysctl.conf
sudo sysctl -p
```

**Fallback:** If watcher creation fails, CollabMonitor logs warning and continues with git diff polling only.

### GitHub API Rate Limits

**Authenticated:** 5000 requests/hour  
**Unauthenticated:** 60 requests/hour

**Strategy:**
- Cache results for 2 minutes (TTL)
- Only check branches with active agents
- Stop tracking merged branches immediately

**Calculation:** 10 branches × 30 checks/hour = 300 requests/hour (well under limit)

### Memory Usage

**CollabStore growth:**
- `FileAgents`: ~100 bytes per file edit × 100 files × 5 agents = 50 KB
- `WorkOverlaps`: ~200 bytes per overlap × 10 overlaps = 2 KB
- `Branches`: ~300 bytes per branch × 10 branches = 3 KB

**Total:** ~100 KB for typical usage (10 agents, 100 files)

**Cleanup:** Prune terminated agents every tick (10s), archive old overlaps after 24h.

---

## Security Considerations

### GitHub Token

**Risk:** Token leakage grants read access to private repos.

**Mitigation:**
- Token stored in environment variable (not in code or config files)
- Documented in setup guide: never commit `.env` with token
- Minimal permissions: `repo:status`, `actions:read` only (no write access)

### File Path Injection

**Risk:** Malicious agent could pass `../../../../etc/passwd` to `who_is_editing_file`.

**Mitigation:**
- Validate file paths: reject `..` segments
- Resolve to absolute path, check it's within repo root
- Return 400 Bad Request for invalid paths

### Overlap Dismissal

**Risk:** Attacker dismisses legitimate overlaps to hide duplicate work.

**Mitigation:**
- Dismissals are per-user (stored with user context)
- Dismissals logged (audit trail)
- Dismissed overlaps can be un-dismissed (re-appear if still valid)

---

## Testing Strategy

### Unit Tests

**CollabStore:**
- File edit tracking (add, update, remove)
- Conflict detection (2+ agents on same file)
- Overlap tracking (add, dismiss, prune)
- Branch status updates

**CollabMonitor:**
- Git diff parsing
- Watcher creation/cleanup
- Conflict warning generation

**OverlapDetector:**
- Subject similarity calculation
- File overlap calculation
- Plan file discovery and parsing
- Collaboration detection (all 5 signals)

**BranchTracker:**
- GitHub URL parsing
- CI status mapping
- Merge detection
- Main/master drift calculation

### Integration Tests

**Conflict Detection:**
1. Spawn two agents with worktrees
2. Agent A modifies `file.go`
3. Agent B modifies `file.go`
4. Assert: both agents receive conflict warning within 10s

**Overlap Detection:**
1. Spawn two agents with similar subjects
2. Both modify overlapping files
3. Assert: both receive overlap warning with confidence > 0.6

**Branch Tracking:**
1. Spawn agent with worktree on branch
2. Mock GitHub API to return "failing" status
3. Assert: desktop notification sent with debug CTA

### Manual Testing Checklist

- [ ] FSNotify detects file change within 1 second
- [ ] Git diff poller catches FSNotify misses
- [ ] Conflict warnings appear in agent inbox
- [ ] Overlap warnings show correct confidence score
- [ ] Collaboration group prevents overlap warnings
- [ ] Pipeline membership prevents overlap warnings
- [ ] Branch CI status updates in real-time
- [ ] Desktop notification appears on CI failure
- [ ] Main/master update notification sent to agents
- [ ] CLI commands return correct data
- [ ] Web UI updates in real-time via SSE
- [ ] MCP tools return correct JSON

---

## Future Enhancements

### Function-Level Conflict Detection

**Goal:** Detect conflicts only when agents edit the same function/class, not just same file.

**Implementation:**
- Parse modified files with AST (Go: `go/ast`, others: tree-sitter)
- Extract function/class line ranges
- Compare line ranges between agents
- Warn only on overlapping ranges

**Benefit:** Reduces false positives for large files (e.g., two agents editing different functions in `utils.go`).

**Effort:** 1-2 weeks (AST parsing, multi-language support)

### Semantic Subject Similarity

**Goal:** Better overlap detection using embeddings instead of token matching.

**Implementation:**
- Use sentence-transformers (e.g., `all-MiniLM-L6-v2`)
- Embed agent subjects into vectors
- Cosine similarity instead of Jaccard
- Threshold: 0.8 for high confidence

**Benefit:** Catches semantic overlap ("refactor auth" vs "improve authentication system").

**Effort:** 2-3 days (model integration, benchmarking)

### GitLab CI Support

**Goal:** Support GitLab in addition to GitHub.

**Implementation:**
- Detect GitLab remote URL pattern
- GitLab API client (`/api/v4/projects/:id/pipelines`)
- Same BranchTracker interface, different client

**Effort:** 2-3 days

### Proactive Coordination Suggestions

**Goal:** Daemon suggests coordination actions when detecting patterns.

**Examples:**
- "Agents 123 and 456 are both editing auth.go. Suggest merging into one agent?"
- "Agent 456 finished auth refactor 2 hours ago. Agent 789 is starting similar work. Link them?"

**Implementation:**
- Pattern detection in OverlapDetector
- Suggestions stored in CollabStore
- CLI command: `warden collab suggestions`

**Effort:** 3-4 days

### Collaborative Editing Sessions

**Goal:** Multiple agents work in same worktree with real-time conflict resolution.

**Implementation:**
- Shared worktree pool (agents opt-in)
- Lock files during edit, release on commit
- Auto-merge conflict-free changes
- Escalate conflicts to user

**Challenge:** Complex concurrency, high risk of data loss.

**Effort:** 2-3 weeks

**Status:** Deferred (high complexity, moderate value)

---

## Success Metrics

**Conflict Prevention:**
- % of file conflicts caught before commit
- Time to detection (target: <10s for git diff, <1s for FSNotify)
- False positive rate (target: <10%)

**Work Deduplication:**
- % of overlaps caught before implementation starts
- Overlap confidence accuracy (precision/recall)
- Dismissal rate (target: <20% = high precision)

**Branch Monitoring:**
- CI failure notification latency (target: <2 min)
- % of branches tracked correctly
- GitHub API success rate (target: >99%)

**User Adoption:**
- % of users with GITHUB_TOKEN configured
- CLI command usage (collab/branch commands)
- MCP tool call frequency by agents

---

## Appendix: Example Scenarios

### Scenario 1: File Conflict Prevention

**Setup:**
- Agent A (agent-123) working on `feature/auth-refactor` branch
- Agent B (agent-456) working on `feature/update-auth` branch

**Timeline:**
1. **10:30** - Agent A modifies `internal/auth.go`
2. **10:30:01** - FSNotify detects change, CollabMonitor updates FileAgents map
3. **10:45** - Agent B modifies `internal/auth.go`
4. **10:45:01** - FSNotify detects change, CollabMonitor detects conflict
5. **10:45:02** - Both agents receive warning in inbox:
   ```
   ⚠️  Collaboration Warning: File Conflict
   File: internal/auth.go
   Also being edited by: agent-456 (update-auth)
   Consider coordinating to avoid merge conflicts.
   ```
6. **10:46** - Agent A calls `warden send agent-456 "What changes are you making to auth.go?"`
7. **10:47** - Agent B replies, they coordinate to split work

**Outcome:** Conflict avoided before commit, no merge headaches.

### Scenario 2: Work Deduplication

**Setup:**
- Agent A (agent-123) subject: "refactor JWT validation"
- Agent B (agent-789) subject: "update authentication system"

**Timeline:**
1. **09:00** - Agent A spawned with subject "refactor JWT validation"
2. **09:05** - Agent A creates plan in `docs/superpowers/specs/2026-06-14-jwt-refactor-design.md`
3. **09:30** - Agent B spawned with subject "update authentication system"
4. **09:35** - Agent B creates plan in `docs/superpowers/specs/2026-06-14-auth-update-design.md`
5. **09:40** - OverlapDetector runs:
   - Subject similarity: 0.65 (common tokens: refactor, authentication, JWT, validation, update, system)
   - File overlap: 0.0 (no code yet)
   - Plan similarity: 0.8 (both mention JWT, token validation, middleware)
   - **Total confidence: 0.65×0.3 + 0.0×0.4 + 0.8×0.3 = 0.435** (below threshold)
6. **10:00** - Both agents start coding, Agent A modifies `auth.go`, `token.go`
7. **10:05** - Agent B modifies `auth.go`, `middleware.go`
8. **10:10** - OverlapDetector runs:
   - Subject similarity: 0.65
   - File overlap: 0.67 (2 files in common: auth.go, token.go vs auth.go, middleware.go)
   - Plan similarity: 0.8
   - **Total confidence: 0.65×0.3 + 0.67×0.4 + 0.8×0.3 = 0.703** (above threshold!)
9. **10:10:01** - Both agents receive overlap warning:
   ```
   ⚠️  Potential Work Overlap Detected (70% confidence)
   Agent: agent-789 (update-auth)
   Overlap reason: similar subjects, 67% file overlap, similar implementation plans
   
   Agent 789 is working on:
   - Subject: update authentication system
   - Files: auth.go, middleware.go
   
   Suggestion: Consider asking agent agent-789 about their progress:
     warden send agent-789 "What's your current status on this work?"
   ```
10. **10:12** - User creates collab group: `warden collab create auth-work --agents agent-123,agent-789`
11. **10:13** - OverlapDetector detects collaboration, stops warning

**Outcome:** Duplicate work caught early, agents merged into collaboration group.

### Scenario 3: CI Failure Notification

**Setup:**
- Agent A (agent-123) working on `feature/api-v2` branch
- CI configured with GitHub Actions

**Timeline:**
1. **14:00** - Agent A commits changes, pushes to `feature/api-v2`
2. **14:01** - GitHub Actions workflow starts
3. **14:05** - GitHub Actions workflow fails (test failure)
4. **14:06** - BranchTracker polls GitHub API, detects status change: passing → failing
5. **14:06:01** - Desktop notification appears:
   ```
   Title: CI Failed
   Message: CI failed on branch feature/api-v2
   Action: Debug with Agent
   ```
6. **14:07** - User clicks "Debug with Agent" button
7. **14:07:01** - Warden spawns new agent:
   ```
   warden start "Debug CI failure on branch feature/api-v2. Check logs at https://github.com/user/repo/actions/runs/12345" --branch feature/api-v2
   ```
8. **14:08** - New agent (agent-456) starts, reads CI logs, identifies failing test
9. **14:15** - Agent 456 fixes test, commits, pushes
10. **14:20** - BranchTracker detects status change: failing → passing
11. **14:20:01** - User notified via desktop notification: "CI passing on feature/api-v2"

**Outcome:** CI failure detected and debugged within 20 minutes, no manual log hunting.

---

## Questions & Decisions

### Resolved

**Q: Should conflict warnings be blocking or informational?**  
**A:** Informational (active warnings to inbox). Blocking would disrupt agent flow. User/agent decides how to coordinate.

**Q: FSNotify or git diff polling?**  
**A:** Both. FSNotify for speed (<1s), git diff for reliability (catches FSNotify misses).

**Q: Where to store collaboration state - database or in-memory?**  
**A:** In-memory (CollabStore). Conflicts/overlaps are ephemeral (valid only while agents alive). No need for persistence.

**Q: How to detect collaboration vs duplication?**  
**A:** Hierarchy: explicit (pipeline/group) > inferred (messages/context) > heuristic (branch). See "Collaboration Detection Hierarchy" section.

**Q: What overlap confidence threshold?**  
**A:** 0.6 (60%). Higher = fewer false positives but misses some overlaps. Lower = more noise. 0.6 is sweet spot based on testing.

**Q: Should branch tracking start automatically?**  
**A:** Yes, for any branch with an active agent worktree. Stop when branch merged or all agents terminate.

**Q: Desktop notifications or inbox only?**  
**A:** Desktop for CI failures (user action required). Inbox for conflicts/overlaps (agent can handle).

### Open

**Q: Should we support other CI systems (GitLab, CircleCI, Jenkins)?**  
**A:** Phase 1: GitHub Actions only. Phase 2+: GitLab. Others on-demand based on user requests.

**Q: Should overlap detection use embeddings or token matching?**  
**A:** Phase 1: Token matching (simple, fast, no dependencies). Future: Embeddings for better accuracy.

**Q: How long to keep dismissed overlaps?**  
**A:** 24 hours. If overlap still valid after 24h, it reappears (unless agents terminated or collaboration detected).

---

## Changelog

**2026-06-14:** Initial design complete
