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
// CollabStore - thread-safe persistence for collaboration state
// All methods are thread-safe and handle locking internally
type CollabStore struct {
    mu sync.RWMutex
    
    // File tracking: which agent is working on which files
    // Changed from []FileEdit to map for thread-safe updates
    FileAgents map[string]map[string]FileEdit  // file path -> agent ID -> FileEdit
    
    // Work overlap detection
    // Changed from slice to map for thread-safe dismissal
    WorkOverlaps map[string]*WorkOverlap  // "agent1-agent2" -> overlap
    
    // Branch tracking
    Branches map[string]*BranchStatus  // branch name -> status
    
    // Collaboration groups (explicit)
    CollabGroups map[string]*CollabGroup  // group name -> members
    
    // Git diff result cache (optimization for OverlapDetector)
    gitDiffCache map[string]*cachedGitDiff  // session ID -> cached diff
    cacheTTL     time.Duration              // default: 30s
    
    // Plan file cache (optimization for OverlapDetector)
    planFileCache map[string]*cachedPlanFile  // session ID -> cached plan
    planCacheTTL  time.Duration               // default: 5 minutes
}

// cachedGitDiff - cached git diff results to avoid O(N²) git subprocess spawning
type cachedGitDiff struct {
    files      []string
    fetchedAt  time.Time
}

// cachedPlanFile - cached plan file contents to avoid repeated file I/O
type cachedPlanFile struct {
    content    string
    tokens     []string  // pre-tokenized for similarity comparison
    fetchedAt  time.Time
    exists     bool      // false if plan file not found
}

// Thread-safe methods (all handle locking internally)
func (cs *CollabStore) AddFileEdit(path, agentID string, edit FileEdit)
func (cs *CollabStore) GetFileEdits(path string) []FileEdit
func (cs *CollabStore) RemoveAgentFiles(agentID string)
func (cs *CollabStore) AddOverlap(overlap *WorkOverlap)
func (cs *CollabStore) GetOverlaps() []*WorkOverlap
func (cs *CollabStore) DismissOverlap(id string)
func (cs *CollabStore) UpdateBranchStatus(name string, status *BranchStatus)
func (cs *CollabStore) GetBranchStatus(name string) *BranchStatus
func (cs *CollabStore) GetCachedGitDiff(sessionID string) ([]string, bool)
func (cs *CollabStore) SetCachedGitDiff(sessionID string, files []string)
func (cs *CollabStore) GetCachedPlanFile(sessionID string) (*cachedPlanFile, bool)
func (cs *CollabStore) SetCachedPlanFile(sessionID string, plan *cachedPlanFile)
func (cs *CollabStore) InvalidatePlanCache(sessionID string)
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
    ID           string  // "agent1-agent2" (sorted alphabetically for consistency)
    Agent1       string
    Agent2       string
    Reason       string  // "similar subjects", "same files", "plan overlap"
    Confidence   float64 // 0.0-1.0
    Details      string  // human-readable explanation
    DetectedAt   time.Time
    DismissedAt  time.Time    // zero value = not dismissed
    dismissed    atomic.Bool  // thread-safe dismissal flag (internal use)
}

func (wo *WorkOverlap) IsDismissed() bool {
    return wo.dismissed.Load()
}

func (wo *WorkOverlap) Dismiss() {
    wo.dismissed.Store(true)
    wo.DismissedAt = time.Now()
}
```

Represents detected duplicate/overlapping work between two agents. Includes confidence score and thread-safe dismissal mechanism for false positives.

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
Initialization:
  - Create FSNotify watcher instance
  - Start event handler goroutine (non-blocking)
  - Subscribe to session lifecycle events (termination)

Main loop (with context cancellation):
  Every 10 seconds or on ctx.Done():
    if ctx.Done():
      cleanup all watchers, return
    
    1. Fetch all active sessions from store
    2. Build set of active session IDs
    3. For each session with a worktree:
       a. Run `git diff --name-only HEAD` to get modified files (with timeout)
       b. Call CollabStore.AddFileEdit() for each file (thread-safe)
       c. If watcher doesn't exist for this worktree:
          - Add watcher path
          - Store sessionID -> watcher mapping
    4. For each sessionID in watcher map not in active set:
       - Remove watcher for this worktree
       - Delete from sessionID -> watcher mapping
    5. Detect conflicts (call CollabStore.GetFileEdits per file)
    6. For each conflict:
       a. Build warning message
       b. Send via mailbox WITHOUT holding CollabStore lock
       c. Publish SSE event via hub

FSNotify event handler (runs in separate goroutine):
  for event := range watcher.Events:
    // Non-blocking: dispatch to worker pool
    select {
    case workQueue <- event:
      // Event queued
    default:
      // Queue full, log warning (prevents blocking)
    }

FSNotify worker pool (5 goroutines):
  for event := range workQueue:
    1. Extract file path and session ID from event
    2. Call CollabStore.AddFileEdit() (thread-safe, returns immediately)
    3. Check for conflicts (CollabStore.GetFileEdits)
    4. If conflict: send warning WITHOUT holding any locks
```

**Thread Safety:**
- CollabStore methods handle locking internally
- Never hold locks while sending to mailbox/hub
- FSNotify events processed in separate worker pool (non-blocking)
- Session termination handled via event subscription (no polling race)

**Conflict Warning Format:**

```
⚠️  Collaboration Warning: File Conflict
File: internal/auth/auth.go
Also being edited by: agent-456 (refactor-jwt)
Consider coordinating to avoid merge conflicts.
```

**Implementation Details:**

- FSNotify watches entire worktree recursively (excluding `.git/`)
- Each worktree gets one watcher path (multiple agents may share worktree)
- Watchers cleaned up via session termination event subscription
- Worker pool (5 goroutines) processes FSNotify events non-blocking
- Warnings sent via existing `mailbox.Store` to agent inbox (no locks held)
- SSE events published for web UI real-time updates (after state committed)
- Git diff commands run with 5-second timeout (prevents hanging)
- Watcher map protected by separate `sync.RWMutex` (not CollabStore.mu)

**FSNotify Watch Budget Management (prevents inotify limit exhaustion):**

```go
type CollabMonitor struct {
    watchers        map[string]*fsnotify.Watcher  // sessionID -> watcher
    watcherMu       sync.RWMutex
    totalWatchCount atomic.Int64  // Total files being watched across all agents
    inotifyLimit    int64          // System limit from /proc/sys/fs/inotify/max_user_watches
    watchBudget     float64        // Percentage of limit to use (default: 0.8 = 80%)
}

func (cm *CollabMonitor) getInotifyLimit() int64 {
    // Read /proc/sys/fs/inotify/max_user_watches (Linux)
    // macOS: no limit (uses kqueue), return math.MaxInt64
    data, err := os.ReadFile("/proc/sys/fs/inotify/max_user_watches")
    if err != nil {
        return 8192  // Conservative default
    }
    limit, _ := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
    return limit
}

func (cm *CollabMonitor) CreateWatcher(sessionID, worktree string) error {
    // Check watch budget before creating watcher
    currentWatches := cm.totalWatchCount.Load()
    budgetLimit := int64(float64(cm.inotifyLimit) * cm.watchBudget)
    
    if currentWatches >= budgetLimit {
        log.Warn("FSNotify watch budget exhausted (%d/%d watches, %.0f%% of system limit). Falling back to git diff polling for session %s",
            currentWatches, budgetLimit, cm.watchBudget*100, sessionID)
        
        // Increase git diff polling frequency for this agent as compensation
        cm.increasePollRate(sessionID, 2*time.Second)  // 10s → 2s
        
        // Publish warning to user
        cm.publishWatchBudgetWarning(currentWatches, cm.inotifyLimit)
        
        return fmt.Errorf("watch budget exhausted (consider increasing fs.inotify.max_user_watches)")
    }
    
    // Create watcher
    watcher, err := fsnotify.NewWatcher()
    if err != nil {
        return fmt.Errorf("failed to create watcher: %w", err)
    }
    
    // Add worktree path (recursive watch handled by FSNotify)
    err = watcher.Add(worktree)
    if err != nil {
        watcher.Close()
        return fmt.Errorf("failed to add watch path: %w", err)
    }
    
    // Count files being watched (estimate for Linux)
    fileCount, err := cm.countFiles(worktree)
    if err != nil {
        log.Warn("Failed to count files in %s: %v", worktree, err)
        fileCount = 1000  // Conservative estimate
    }
    
    cm.watcherMu.Lock()
    cm.watchers[sessionID] = watcher
    cm.watcherMu.Unlock()
    
    cm.totalWatchCount.Add(int64(fileCount))
    
    log.Info("Created FSNotify watcher for session %s (%d files, %d total watches, %.1f%% of limit)",
        sessionID, fileCount, cm.totalWatchCount.Load(), 
        float64(cm.totalWatchCount.Load())/float64(cm.inotifyLimit)*100)
    
    return nil
}

func (cm *CollabMonitor) removeWatcher(sessionID string) {
    cm.watcherMu.Lock()
    watcher, ok := cm.watchers[sessionID]
    if ok {
        // Estimate watch count before closing
        fileCount := cm.estimateWatchCount(sessionID)
        
        watcher.Close()  // Stops goroutine, closes file descriptors
        delete(cm.watchers, sessionID)
        cm.totalWatchCount.Add(-int64(fileCount))
        
        log.Info("Removed FSNotify watcher for session %s (%d watches freed, %d remaining)",
            sessionID, fileCount, cm.totalWatchCount.Load())
    }
    cm.watcherMu.Unlock()
}

func (cm *CollabMonitor) countFiles(path string) (int, error) {
    count := 0
    err := filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
        if err != nil {
            return err
        }
        if info.IsDir() && info.Name() == ".git" {
            return filepath.SkipDir  // Exclude .git directory
        }
        if !info.IsDir() {
            count++
        }
        return nil
    })
    return count, err
}

func (cm *CollabMonitor) publishWatchBudgetWarning(current, limit int64) {
    // Send desktop notification
    msg := fmt.Sprintf("FSNotify watch budget exhausted (%d/%d). Some agents using git diff polling. Increase fs.inotify.max_user_watches for better performance.", current, limit)
    
    // Platform-specific notification
    if runtime.GOOS == "linux" {
        exec.Command("notify-send", "Warden Performance Warning", msg).Run()
    } else if runtime.GOOS == "darwin" {
        exec.Command("osascript", "-e", fmt.Sprintf(`display notification "%s" with title "Warden"`, msg)).Run()
    }
    
    // Also log prominently
    log.Warn("===== PERFORMANCE DEGRADATION =====")
    log.Warn(msg)
    log.Warn("Run: sudo sysctl -w fs.inotify.max_user_watches=524288")
    log.Warn("===================================")
}
```

**Why watch budget management:** Prevents silent failures when hitting inotify limits. Users get clear warnings and fallback to more frequent polling ensures conflicts still detected (with ~2s latency instead of <1s).

**Edge Cases:**

- Agent without worktree: skip (prompt-mode agents)
- Watcher creation failure: log error, fallback to git diff polling only
- Large repos: FSNotify may hit OS limits (inotify instances) - document this limitation
- Symlinks: Resolve with `filepath.EvalSymlinks()` before adding to FileAgents (FSNotify may fire events for symlink targets)
- Rename/Delete events: Handle `fsnotify.Rename`, `fsnotify.Remove` in addition to `fsnotify.Write`
- Rapid file churn: Debounce events (group by file, process last event per file per 100ms) to prevent queue overflow
- Worktree deleted externally: Git diff fails with "directory not found" - remove session from tracking
- Detached HEAD state: Skip tracking (no branch name to track)
- Bare repositories: Validate at daemon startup with `git rev-parse --is-bare-repository`, disable CollabMonitor if bare
- Shallow clones: Detect with `git rev-parse --is-shallow-repository`, skip "commits behind" calculation
- Git subprocess timeout: Use `exec.CommandContext(ctx, ...)` and kill process group on timeout

---

### 2. OverlapDetector - Work Deduplication

**Responsibility:** Detect when multiple agents are working on overlapping features before conflicts occur.

**Strategy:**
- Compare agent subjects (string similarity)
- Analyze file overlap (percentage of common files)
- Parse and compare implementation plans

**Why:** Preventative rather than reactive - catch duplication before wasted work happens.

**Algorithm (with O(N²) optimization and parallel execution):**

```
Initialization:
  - Create worker pool (10 goroutines) for parallel comparisons
  - Create comparison work queue (buffered channel, size 500)
  - Start worker pool goroutines

Every 30 seconds or on ctx.Done():
  if ctx.Done():
    return
  
  1. Fetch all active (non-terminal) sessions
  2. Pre-filter: group sessions by collaboration signal
     - Build collaboration graph (pipeline, group, branch, messages)
     - Sessions in same connected component = collaborating (skip comparison)
  3. Generate comparison pairs (i, j) where i < j and not collaborating
  4. Batch comparisons for parallel processing:
     a. Divide pairs into batches of 200 (rate limit per tick)
     b. If total pairs > 200: use round-robin offset (track between ticks)
  5. Dispatch comparisons to worker pool (non-blocking):
     for each pair in current batch:
       select {
       case workQueue <- comparisonTask{agent1, agent2}:
         // Task queued
       default:
         // Queue full, defer to next tick
       }
  6. Workers process comparisons in parallel (10 concurrent):
     a. Early exit optimizations:
        - If subject similarity < 0.3: skip (cheap check first)
        - If no shared file prefixes: skip (heuristic, O(1))
     b. Calculate overlap score:
        - Subject similarity (token-based, 0.0-1.0) - CACHED for 5 minutes
        - File overlap percentage (0.0-1.0) - use CACHED git diff from CollabStore
        - Plan similarity (token-based, 0.0-1.0) - CACHED plan file from CollabStore
     c. Weighted score: subject*0.3 + files*0.4 + plan*0.3
     d. If score > 0.6:
        - Build overlap ID: sorted(agent1, agent2) joined with "-"
        - Check if already exists in CollabStore (dedupe)
        - If new: call CollabStore.AddOverlap() (thread-safe)
        - Send warnings WITHOUT holding any locks
        - Publish SSE event
  7. Cleanup: remove dismissed overlaps older than 24h
  8. Prune similarity cache entries older than 5 minutes

Rate limiting (prevent O(N²) explosion with many agents):
  - Max comparisons per tick: 200
  - Parallel execution: 10 workers process 200 comparisons in ~1 second (vs 10 seconds sequential)
  - If N² > 200: round-robin (track offset, resume next tick)
  - Log warning if backlog building up (suggests too many agents)
  
Worker pool design:
  - 10 goroutines reading from shared work queue
  - Each worker processes one comparison independently
  - Workers never hold locks while computing similarity (CPU-bound)
  - Only acquire CollabStore lock when adding overlap (minimal hold time)
```

**Thread Safety:**
- Git diff results cached in CollabStore (read from cache, not spawning git each time)
- Subject/plan similarity cached in local map (5-minute TTL)
- CollabStore.AddOverlap() is thread-safe
- Warnings sent after releasing all locks

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

Simple token-based Jaccard similarity (case-insensitive):
```go
func tokenize(s string) []string {
    s = strings.ToLower(s)  // Case-insensitive
    return strings.FieldsFunc(s, func(r rune) bool {
        return !unicode.IsLetter(r) && !unicode.IsNumber(r)
    })
}

tokens1 = tokenize("refactor JWT authentication")  // ["refactor", "jwt", "authentication"]
tokens2 = tokenize("update auth system")            // ["update", "auth", "system"]
similarity = len(intersection(tokens1, tokens2)) / len(union(tokens1, tokens2))
```

**Future Enhancement:** Use embeddings (sentence-transformers) for semantic similarity.

**File Overlap:**

```go
files1 = git diff --name-only HEAD in worktree1
files2 = git diff --name-only HEAD in worktree2

// Handle division by zero (both agents idle with no changes)
union := union(files1, files2)
if len(union) == 0 {
    overlap = 0.0  // Both idle, no overlap
} else {
    overlap = float64(len(intersection(files1, files2))) / float64(len(union))
}

// Special case: 100% overlap = conflict (not just overlap warning)
if overlap == 1.0 && len(files1) > 0 {
    severity = "critical"  // Definite conflict
    confidence = 1.0
} else {
    severity = "warning"   // Potential overlap
    confidence = subject*0.3 + overlap*0.4 + plan*0.3
}
```

**Plan Analysis (with FSNotify invalidation):**

- Look for plan files in `docs/superpowers/specs/`
- Match by agent ID (exact) or session name with word boundaries
- Read plan contents, apply token-based similarity
- If no plan files found, skip (score = 0.0)
- **Cache plan file contents in CollabStore** (TTL: 5 minutes)
- **FSNotify watcher on specs directory** invalidates cache on CREATE/WRITE events

```go
type OverlapDetector struct {
    collabStore  *CollabStore
    planWatcher  *fsnotify.Watcher  // Watches docs/superpowers/specs/
}

func (od *OverlapDetector) init() error {
    // Create FSNotify watcher for plan files directory
    watcher, err := fsnotify.NewWatcher()
    if err != nil {
        log.Warn("Failed to create plan file watcher: %v (falling back to uncached reads)", err)
        return nil  // Non-fatal, just less efficient
    }
    
    specsDir := "docs/superpowers/specs/"
    if err := watcher.Add(specsDir); err != nil {
        log.Warn("Failed to watch %s: %v", specsDir, err)
        watcher.Close()
        return nil
    }
    
    od.planWatcher = watcher
    
    // Start event handler goroutine
    go od.handlePlanFileEvents()
    
    log.Info("Watching plan files directory: %s", specsDir)
    return nil
}

func (od *OverlapDetector) handlePlanFileEvents() {
    for event := range od.planWatcher.Events {
        if event.Op&fsnotify.Write == fsnotify.Write || event.Op&fsnotify.Create == fsnotify.Create {
            // Extract session ID from filename (e.g., "2026-06-14-jwt-refactor-agent-123.md" -> "agent-123")
            sessionID := od.extractSessionID(filepath.Base(event.Name))
            if sessionID != "" {
                od.collabStore.InvalidatePlanCache(sessionID)
                log.Debug("Invalidated plan cache for session %s (file %s modified)", sessionID, event.Name)
            }
        }
    }
}

func (od *OverlapDetector) getPlanContent(sessionID, sessionName string) ([]string, error) {
    // Check cache first
    if cached, ok := od.collabStore.GetCachedPlanFile(sessionID); ok {
        if time.Since(cached.fetchedAt) < 5*time.Minute {
            if !cached.exists {
                return nil, nil  // Previously confirmed non-existent
            }
            return cached.tokens, nil
        }
    }
    
    // Cache miss or expired - search filesystem
    planFile, err := od.findPlanFile(sessionID, sessionName)
    if err != nil || planFile == "" {
        // Cache negative result (avoid repeated searches)
        od.collabStore.SetCachedPlanFile(sessionID, &cachedPlanFile{
            exists:    false,
            fetchedAt: time.Now(),
        })
        return nil, nil
    }
    
    // Read and tokenize plan file
    content, err := os.ReadFile(planFile)
    if err != nil {
        return nil, err
    }
    
    tokens := tokenize(string(content))
    
    // Cache result
    od.collabStore.SetCachedPlanFile(sessionID, &cachedPlanFile{
        content:   string(content),
        tokens:    tokens,
        fetchedAt: time.Now(),
        exists:    true,
    })
    
    return tokens, nil
}

func matchPlanFile(filename, sessionName, agentID string) bool {
    // Try agent ID first (exact match)
    if strings.Contains(filename, agentID) {
        return true
    }
    // Try session name with word boundaries (prevent substring false matches)
    pattern := regexp.MustCompile(`\b` + regexp.QuoteMeta(sessionName) + `\b`)
    return pattern.MatchString(filename)
}
```

**Why plan analysis:** Catch overlap **before** any code is written, based on design intent.

**Why FSNotify + caching:** Eliminates repeated file I/O (50 agents × 30s tick = 100 disk reads/minute → 1 read per plan file update).

---

### 3. BranchTracker - Git Branch Monitoring

**Responsibility:** Monitor branches for CI status, merge state, and detect when main/master updates.

**Strategy:**
- Poll GitHub Actions API every 2 minutes
- Check merge status via `git branch --contains`
- Track commits behind main via `git rev-list`

**Why:** Keep users informed of CI failures and branch drift without manual checking.

**Algorithm (with graceful shutdown and branch grouping optimization):**

```
Initialization:
  - Create GitHub API client with context-aware HTTP client (10s timeout)
  - Create ticker (2 minutes)

Main loop:
  for {
    select {
    case <-ctx.Done():
      ticker.Stop()
      return  // Graceful shutdown
    case <-ticker.C:
      processBranches(ctx)  // Pass context for HTTP request cancellation
    }
  }

processBranches(ctx):
  1. Get all active sessions with worktrees
  
  2. Group sessions by branch (OPTIMIZATION: reduces API calls from O(agents) to O(branches))
     branchAgents := make(map[string][]string)  // branch name -> agent IDs
     branchRemotes := make(map[string]string)   // branch name -> remote URL
     for _, sess := range sessions {
       if sess.Branch != "" && sess.Branch != "HEAD" {
         branchAgents[sess.Branch] = append(branchAgents[sess.Branch], sess.ID)
         if branchRemotes[sess.Branch] == "" {
           branchRemotes[sess.Branch] = getRemoteURL(sess.Worktree)
         }
       }
     }
  
  3. For each unique branch (ONE API call per branch, shared across all agents):
     a. Check CI status via GitHub API (with ctx for cancellation)
        - If ctx.Done(): return immediately (shutdown in progress)
        - Use cached result if < 2 min old
        - Use conditional request with If-None-Match (ETag) header
     b. Check merge status: git branch -r --contains <branch> (with timeout)
     c. Check commits behind: git rev-list --count main..<branch> (with timeout)
     d. Read old status from CollabStore.GetBranchStatus() (thread-safe)
     e. Build new BranchStatus with all agent IDs for this branch
     f. Call CollabStore.UpdateBranchStatus() (thread-safe, returns immediately)
     g. Detect state transitions (compare old vs new):
        - CI passed → failed: queue desktop notification (non-blocking)
        - Commits behind increased: queue agent notifications for ALL agents on this branch
        - Branch merged/rebased: remove from tracking
     h. Send queued notifications WITHOUT holding any locks
     i. Publish SSE events

Example: 50 agents on 10 branches
  - Old approach: 50 API calls (one per agent)
  - New approach: 10 API calls (one per branch)
  - Reduction: 80% fewer API calls (400 req/hr → 80 req/hr)
```

**Thread Safety:**
- GitHub API client is context-aware (cancels on ctx.Done())
- Git commands run with 10-second timeout
- CollabStore methods are thread-safe (no locks held during I/O)
- Desktop notifications queued and sent after state updates
- Graceful shutdown: waits for in-flight HTTP requests to complete (max 10s)

**Branch Discovery:**

Only track branches associated with agent worktrees:
- When agent spawned with worktree, add its branch to tracker
- When agent terminates, check if other agents still on that branch
- If no agents remain and branch is merged, remove from tracker

**GitHub API Integration (with context, caching, retry, and circuit breaker):**

```go
type GitHubClient struct {
    token         string  // from GITHUB_TOKEN env var
    httpClient    *http.Client  // context-aware, 10s timeout
    cacheMu       sync.RWMutex
    cache         map[string]cachedStatus  // branch -> cached result (TTL: 2 min)
    circuitBreaker *CircuitBreaker  // Stop calling API after repeated failures
}

type cachedStatus struct {
    status    string
    runURL    string
    etag      string     // ETag from GitHub API response (for conditional requests)
    fetchedAt time.Time
}

type CircuitBreaker struct {
    mu             sync.Mutex
    failureCount   int
    lastFailure    time.Time
    state          string  // "closed", "open", "half-open"
    threshold      int     // 5 consecutive failures
    cooldownPeriod time.Duration  // 5 minutes
}

func (cb *CircuitBreaker) Allow() bool {
    cb.mu.Lock()
    defer cb.mu.Unlock()
    
    if cb.state == "open" {
        if time.Since(cb.lastFailure) > cb.cooldownPeriod {
            cb.state = "half-open"
            return true  // Try one request
        }
        return false  // Circuit open, reject request
    }
    return true  // Circuit closed or half-open
}

func (cb *CircuitBreaker) RecordSuccess() {
    cb.mu.Lock()
    defer cb.mu.Unlock()
    cb.failureCount = 0
    cb.state = "closed"
}

func (cb *CircuitBreaker) RecordFailure() {
    cb.mu.Lock()
    defer cb.mu.Unlock()
    cb.failureCount++
    cb.lastFailure = time.Now()
    if cb.failureCount >= cb.threshold {
        cb.state = "open"
    }
}

func (gh *GitHubClient) GetCIStatus(ctx context.Context, remoteURL, branch string) (status, runURL string, err error) {
    cacheKey := remoteURL + ":" + branch
    
    // Check cache first (thread-safe read)
    gh.cacheMu.RLock()
    cached, hasCached := gh.cache[cacheKey]
    if hasCached && time.Since(cached.fetchedAt) < 2*time.Minute {
        gh.cacheMu.RUnlock()
        return cached.status, cached.runURL, nil
    }
    etag := ""
    if hasCached {
        etag = cached.etag  // Use ETag for conditional request
    }
    gh.cacheMu.RUnlock()
    
    // Check circuit breaker
    if !gh.circuitBreaker.Allow() {
        return "unknown", "", fmt.Errorf("circuit breaker open (GitHub API unavailable)")
    }
    
    // Parse owner/repo from remoteURL (e.g., git@github.com:user/repo.git)
    owner, repo, err := parseGitHubURL(remoteURL)
    if err != nil {
        return "", "", err
    }
    
    // Retry logic with exponential backoff
    var lastErr error
    for attempt := 0; attempt < 3; attempt++ {
        if attempt > 0 {
            // Exponential backoff: 1s, 2s, 4s
            backoff := time.Duration(1<<uint(attempt-1)) * time.Second
            select {
            case <-time.After(backoff):
            case <-ctx.Done():
                return "", "", ctx.Err()
            }
        }
        
        status, runURL, newEtag, err := gh.makeAPIRequest(ctx, owner, repo, branch, etag)
        
        if err == nil {
            gh.circuitBreaker.RecordSuccess()
            
            // Update cache with new ETag
            gh.cacheMu.Lock()
            gh.cache[cacheKey] = cachedStatus{
                status:    status,
                runURL:    runURL,
                etag:      newEtag,
                fetchedAt: time.Now(),
            }
            gh.cacheMu.Unlock()
            
            return status, runURL, nil
        }
        
        // Classify error
        lastErr = err
        switch {
        case isAuthError(err):  // 401, 403 (invalid token)
            gh.circuitBreaker.RecordFailure()
            return "unknown", "", fmt.Errorf("GitHub auth failed (check GITHUB_TOKEN): %w", err)
        case isNotFoundError(err):  // 404 (branch/repo deleted)
            return "unknown", "", fmt.Errorf("branch/repo not found: %w", err)
        case isRateLimitError(err):  // 429 (rate limited)
            // Don't count as circuit breaker failure (transient)
            continue  // Retry with backoff
        case isServerError(err):  // 5xx (GitHub down)
            gh.circuitBreaker.RecordFailure()
            continue  // Retry
        case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
            return "", "", err  // Don't retry on context cancellation
        default:
            gh.circuitBreaker.RecordFailure()
            continue  // Retry unknown errors
        }
    }
    
    return "unknown", "", fmt.Errorf("GitHub API failed after 3 retries: %w", lastErr)
}

func (gh *GitHubClient) makeAPIRequest(ctx context.Context, owner, repo, branch, etag string) (status, runURL, newEtag string, err error) {
    // 1. Get latest commit SHA for branch
    url := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/refs/heads/%s", owner, repo, branch)
    req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
    req.Header.Set("Authorization", "token "+gh.token)
    
    // Use conditional request with ETag (saves rate limit if unchanged)
    if etag != "" {
        req.Header.Set("If-None-Match", etag)
    }
    
    resp, err := gh.httpClient.Do(req)
    if err != nil {
        return "", "", "", err
    }
    defer resp.Body.Close()
    
    // Extract ETag from response
    newEtag = resp.Header.Get("ETag")
    
    // Check status code
    if resp.StatusCode == 304 {
        // Not Modified - use cached data (GitHub API didn't count against rate limit!)
        return status, runURL, etag, nil
    }
    
    if resp.StatusCode != 200 {
        body, _ := io.ReadAll(resp.Body)
        return "", "", "", &HTTPError{
            StatusCode: resp.StatusCode,
            Body:       string(body),
        }
    }
    
    // Parse JSON
    var refResp struct {
        Object struct {
            SHA string `json:"sha"`
        } `json:"object"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&refResp); err != nil {
        return "", "", "", fmt.Errorf("invalid JSON from GitHub API: %w", err)
    }
    
    sha := refResp.Object.SHA
    
    // 2. Get commit status
    url = fmt.Sprintf("https://api.github.com/repos/%s/%s/commits/%s/status", owner, repo, sha)
    req, _ = http.NewRequestWithContext(ctx, "GET", url, nil)
    req.Header.Set("Authorization", "token "+gh.token)
    
    resp, err = gh.httpClient.Do(req)
    if err != nil {
        return "", "", "", err
    }
    defer resp.Body.Close()
    
    if resp.StatusCode != 200 {
        body, _ := io.ReadAll(resp.Body)
        return "", "", "", &HTTPError{StatusCode: resp.StatusCode, Body: string(body)}
    }
    
    var statusResp struct {
        State string `json:"state"`  // "success", "failure", "pending"
    }
    if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
        return "", "", "", fmt.Errorf("invalid JSON from GitHub API: %w", err)
    }
    
    status = statusResp.State
    
    // 3. Get workflow runs
    url = fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runs?branch=%s&per_page=1", owner, repo, branch)
    req, _ = http.NewRequestWithContext(ctx, "GET", url, nil)
    req.Header.Set("Authorization", "token "+gh.token)
    
    resp, err = gh.httpClient.Do(req)
    if err != nil {
        return "", "", "", err
    }
    defer resp.Body.Close()
    
    if resp.StatusCode != 200 {
        // Workflow runs might not exist (no Actions configured) - not an error
        return status, "", newEtag, nil
    }
    
    var runsResp struct {
        WorkflowRuns []struct {
            HTMLURL string `json:"html_url"`
        } `json:"workflow_runs"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&runsResp); err != nil {
        return status, "", newEtag, nil  // Ignore JSON errors for workflow runs
    }
    
    if len(runsResp.WorkflowRuns) > 0 {
        runURL = runsResp.WorkflowRuns[0].HTMLURL
    }
    
    // Cache update handled by caller (with ETag)
    
    return status, runURL, newEtag, nil
}

type HTTPError struct {
    StatusCode int
    Body       string
}

func (e *HTTPError) Error() string {
    return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}

func isAuthError(err error) bool {
    var httpErr *HTTPError
    if errors.As(err, &httpErr) {
        return httpErr.StatusCode == 401 || httpErr.StatusCode == 403
    }
    return false
}

func isNotFoundError(err error) bool {
    var httpErr *HTTPError
    return errors.As(err, &httpErr) && httpErr.StatusCode == 404
}

func isRateLimitError(err error) bool {
    var httpErr *HTTPError
    return errors.As(err, &httpErr) && httpErr.StatusCode == 429
}

func isServerError(err error) bool {
    var httpErr *HTTPError
    return errors.As(err, &httpErr) && httpErr.StatusCode >= 500
}

func (gh *GitHubClient) Close() {
    gh.httpClient.CloseIdleConnections()
}
```

**Thread Safety:**
- Cache protected by separate `sync.RWMutex` (not shared with CollabStore)
- HTTP client configured with timeout (prevents hanging on shutdown)
- Context passed to all HTTP requests (cancels on daemon shutdown)
- `Close()` method for graceful connection cleanup

**CI Failure Notification (with deduplication):**

Desktop notification (platform-specific):
- **macOS:** `osascript -e 'display notification "..." with title "..."'`
- **Linux:** `notify-send "..." "..."`
- **Fallback:** Store in pending notifications queue, show on next `warden branch notifications`

**Deduplication (prevent spam from flaky tests):**

```go
type NotificationDeduper struct {
    mu       sync.Mutex
    lastSent map[string]time.Time  // branch -> last notification time
}

func (nd *NotificationDeduper) ShouldNotify(branch string, eventType string) bool {
    nd.mu.Lock()
    defer nd.mu.Unlock()
    
    key := branch + ":" + eventType
    last, ok := nd.lastSent[key]
    
    // Only send notification if > 5 minutes since last one for this branch+event
    if !ok || time.Since(last) > 5*time.Minute {
        nd.lastSent[key] = time.Now()
        return true
    }
    return false  // Suppress (too soon)
}
```

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

# Handle detached HEAD
current_branch=$(git -C "$worktree" rev-parse --abbrev-ref HEAD)
if [ "$current_branch" = "HEAD" ]; then
    # Detached HEAD state - skip tracking
    exit 0
fi
```

When branch is merged/rebased:
- Remove from `CollabStore.Branches`
- Stop tracking (no more CI checks)
- Agents on that branch are unaffected (they still work in their worktree)

**Edge cases:**
- Detached HEAD: Skip tracking (can't determine branch name)
- Bare repository: Validate at startup, skip branch tracking if bare
- Shallow clone: Skip "commits behind" calculation (git rev-list fails with truncated history)

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

### Collaboration Routes (with thread-safety annotations)

**Thread Safety:** All handlers use CollabStore's thread-safe methods (which handle locking internally).

```
GET  /collab/status
  Query params: ?scope=files|overlaps|branches
  Returns: JSON with current collaboration state
  Thread-safety: Acquires CollabStore.mu.RLock() for entire read operation

GET  /collab/conflicts
  Returns: JSON array of file conflicts
  Thread-safety: Acquires CollabStore.mu.RLock()

GET  /collab/overlaps
  Returns: JSON array of work overlaps
  Thread-safety: Acquires CollabStore.mu.RLock()

GET  /collab/groups
  Returns: JSON array of collaboration groups
  Thread-safety: Acquires CollabStore.mu.RLock()

POST /collab/groups
  Body: {"name": "auth-team", "agents": ["agent-123", "agent-456"]}
  Returns: Created group
  Thread-safety: Acquires CollabStore.mu.Lock() during creation
  Security: Sanitize group name (alphanumeric + hyphens/underscores only)

DELETE /collab/groups/:name
  Returns: 204 No Content
  Thread-safety: Acquires CollabStore.mu.Lock() during deletion

POST /collab/overlaps/:id/dismiss
  :id format: agent1-agent2
  Returns: 200 OK
  Thread-safety: Calls CollabStore.DismissOverlap() (thread-safe)
  Security: Validate ID format, reject if not matching pattern

GET  /collab/file/:path
  :path = base64-encoded file path
  Returns: List of agents editing this file
  Thread-safety: Calls CollabStore.GetFileEdits() (thread-safe)
  Security: Validate path (reject "..", must be within repo root)
```

**Implementation Pattern:**

```go
func (s *Server) handleCollabStatus(w http.ResponseWriter, r *http.Request) {
    scope := r.URL.Query().Get("scope")
    
    // Acquire read lock, build response, release lock
    // DO NOT call mailbox/hub while holding lock
    s.collabStore.mu.RLock()
    data := buildStatusResponse(s.collabStore, scope)
    s.collabStore.mu.RUnlock()
    
    json.NewEncoder(w).Encode(data)
}
```

### Branch Routes (with thread-safety annotations)

```
GET  /branches
  Returns: JSON array of all tracked branches
  Thread-safety: Acquires CollabStore.mu.RLock()

GET  /branches/:name
  :name = URL-encoded branch name
  Returns: BranchStatus JSON
  Thread-safety: Calls CollabStore.GetBranchStatus() (thread-safe)

GET  /branches/notifications
  Returns: Pending CI failure notifications
  Thread-safety: Separate notifications queue (protected by its own mutex)
```

---

## Integration with Existing Systems

### Mailbox Integration

**How:** Conflict warnings and overlap notifications use existing `mailbox.Store`.

**Why:** Reuse proven messaging infrastructure, no need to build new delivery system.

**CRITICAL - Thread Safety:**
- **NEVER hold CollabStore.mu while calling mailbox.Send()**
- Pattern: 1) Read state with lock, 2) Release lock, 3) Send to mailbox
- Prevents deadlock (agent reading inbox while holding another lock)

**Format:**
```go
// CORRECT: Release lock before sending
cs.mu.RLock()
conflicts := cs.getConflicts()  // Read state
cs.mu.RUnlock()

// Now send WITHOUT holding any locks
for _, conflict := range conflicts {
    mailbox.Send(ctx, Message{
        From: "system",
        To:   conflict.AgentID,
        Body: formatConflictWarning(conflict),
        Sent: time.Now(),
    })
}
```

Agents receive via:
- MCP: `read_inbox` tool
- CLI: `warden inbox` command

**Timeout:** Use context with 5-second timeout for mailbox.Send() to prevent blocking indefinitely.

**Deduplication (prevent spam):**

```go
type MailboxDeduper struct {
    mu       sync.Mutex
    recentMsgs map[string]time.Time  // "agentID:msgType:key" -> last sent time
}

func (md *MailboxDeduper) ShouldSend(agentID, msgType, key string) bool {
    md.mu.Lock()
    defer md.mu.Unlock()
    
    dedupKey := fmt.Sprintf("%s:%s:%s", agentID, msgType, key)
    last, ok := md.recentMsgs[dedupKey]
    
    // Only send if > 1 minute since last identical message
    if !ok || time.Since(last) > 1*time.Minute {
        md.recentMsgs[dedupKey] = time.Now()
        return true
    }
    return false
}

// Background cleanup to prevent memory leak
func (md *MailboxDeduper) cleanupLoop(ctx context.Context) {
    ticker := time.NewTicker(5 * time.Minute)
    defer ticker.Stop()
    
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            md.mu.Lock()
            now := time.Now()
            for key, lastSent := range md.recentMsgs {
                // Remove entries older than 1 hour (well past the 1-minute dedup window)
                if now.Sub(lastSent) > 1*time.Hour {
                    delete(md.recentMsgs, key)
                }
            }
            md.mu.Unlock()
        }
    }
}

// Before sending conflict warning
if mailboxDeduper.ShouldSend(agentID, "conflict", filepath) {
    mailbox.Send(ctx, Message{...})
}
```

### SSE/Hub Integration

**How:** CollabMonitor, BranchTracker, OverlapDetector call `hub.publish()` on state changes.

**Why:** Web UI updates in real-time when conflicts detected, no polling needed.

**CRITICAL - Thread Safety:**
- **State updates and hub.publish() must be atomic (no race window)**
- Pattern: 1) Update state, 2) Release lock, 3) Publish to hub
- Hub sees committed state only (no partial updates visible)

**Implementation:**
```go
// CORRECT: Commit state, then publish
cs.mu.Lock()
cs.addConflict(conflict)
cs.mu.Unlock()

// State is committed, now publish
hub.Publish()  // SSE clients receive update

// Web UI subscribes to /events
// Receives notification, fetches /collab/status via HTTP (which acquires lock)
// Guaranteed to see the new conflict (no race window)
```

**Event Format (with sequence number for replay):**
```json
{
  "type": "collab_update",
  "scope": "conflicts",  // or "overlaps", "branches"
  "timestamp": "2026-06-14T11:00:00Z",
  "sequence": 12345  // Monotonic sequence number
}
```

Web UI receives event, then fetches full state via GET /collab/status (atomic read).

**SSE Reconnection Handling:**

```go
type SSEHub struct {
    mu             sync.Mutex
    sequence       int64  // Atomic sequence counter
    eventHistory   []SSEEvent  // Last 100 events for replay
    historySize    int  // 100
}

type SSEEvent struct {
    Sequence  int64
    Type      string
    Scope     string
    Timestamp time.Time
}

func (h *SSEHub) Publish(eventType, scope string) {
    h.mu.Lock()
    h.sequence++
    seq := h.sequence
    
    event := SSEEvent{
        Sequence:  seq,
        Type:      eventType,
        Scope:     scope,
        Timestamp: time.Now(),
    }
    
    // Add to history (circular buffer)
    h.eventHistory = append(h.eventHistory, event)
    if len(h.eventHistory) > h.historySize {
        h.eventHistory = h.eventHistory[1:]
    }
    h.mu.Unlock()
    
    // Broadcast to all SSE clients
    h.broadcast(event)
}

func (h *SSEHub) ReplayEvents(lastEventID int64) []SSEEvent {
    h.mu.Lock()
    defer h.mu.Unlock()
    
    // Find events after lastEventID
    var replay []SSEEvent
    for _, event := range h.eventHistory {
        if event.Sequence > lastEventID {
            replay = append(replay, event)
        }
    }
    return replay
}

// SSE handler supports Last-Event-ID for reconnection
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
    lastEventID := r.Header.Get("Last-Event-ID")
    
    // Replay missed events
    if lastEventID != "" {
        lastSeq, _ := strconv.ParseInt(lastEventID, 10, 64)
        missedEvents := s.hub.ReplayEvents(lastSeq)
        for _, event := range missedEvents {
            sendSSEEvent(w, event)
        }
    }
    
    // Subscribe to new events
    // ...
}
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

**Startup Sequence (state recovery after daemon crash, with parallel processing):**

```go
func (d *Daemon) Startup(ctx context.Context) error {
    // 1. Validate git repository health
    if err := validateGitRepo(d.repoRoot); err != nil {
        log.Warn("Git repo validation failed: %v (collaboration features degraded)", err)
        // Continue with degraded mode (disable some monitors)
    }
    
    // 2. Scan for orphaned agent worktrees (agents running before daemon crashed)
    sessions, err := d.store.List(ctx)
    if err != nil {
        return fmt.Errorf("failed to list sessions: %w", err)
    }
    
    log.Info("Recovering state for %d active sessions (parallel recovery starting)", len(sessions))
    
    // 3. Start monitors immediately (before recovery completes) for progressive availability
    d.startMonitors(ctx)
    
    // 4. Parallel recovery using worker pool (10 concurrent)
    type recoveryResult struct {
        sessionID string
        files     []string
        branch    string
        err       error
    }
    
    resultChan := make(chan recoveryResult, len(sessions))
    semaphore := make(chan struct{}, 10)  // Limit to 10 concurrent git operations
    var wg sync.WaitGroup
    
    activeSessions := 0
    for _, sess := range sessions {
        if sess.Status == "working" && sess.Worktree != "" {
            activeSessions++
            wg.Add(1)
            
            // Launch recovery goroutine
            go func(sess *store.Session) {
                defer wg.Done()
                
                // Acquire semaphore (limit concurrent git operations)
                semaphore <- struct{}{}
                defer func() { <-semaphore }()
                
                result := recoveryResult{sessionID: sess.ID}
                
                // Validate worktree still exists
                if _, err := os.Stat(sess.Worktree); os.IsNotExist(err) {
                    result.err = fmt.Errorf("worktree missing")
                    resultChan <- result
                    return
                }
                
                // Validate git repo in worktree
                if err := validateGitRepo(sess.Worktree); err != nil {
                    result.err = fmt.Errorf("worktree invalid: %w", err)
                    resultChan <- result
                    return
                }
                
                // Run git diff to populate FileAgents map
                files, err := runGitDiff(sess.Worktree)
                if err != nil {
                    result.err = fmt.Errorf("git diff failed: %w", err)
                    resultChan <- result
                    return
                }
                result.files = files
                
                // Get branch
                branch, err := getCurrentBranch(sess.Worktree)
                if err == nil && branch != "HEAD" {
                    result.branch = branch
                }
                
                resultChan <- result
            }(sess)
        }
    }
    
    // 5. Collect results and update state
    go func() {
        wg.Wait()
        close(resultChan)
    }()
    
    completed := 0
    for result := range resultChan {
        completed++
        
        // Progress logging (every 10% or every 5 sessions)
        if completed%max(activeSessions/10, 5) == 0 || completed == activeSessions {
            log.Info("Recovery progress: %d/%d sessions (%.0f%%)", completed, activeSessions, 
                float64(completed)/float64(activeSessions)*100)
        }
        
        if result.err != nil {
            log.Warn("Session %s recovery failed: %v", result.sessionID, result.err)
            continue
        }
        
        // Find session object
        var sess *store.Session
        for _, s := range sessions {
            if s.ID == result.sessionID {
                sess = s
                break
            }
        }
        if sess == nil {
            continue
        }
        
        // Re-create FSNotify watcher
        if err := d.collabMonitor.CreateWatcher(sess.ID, sess.Worktree); err != nil {
            log.Warn("Failed to create watcher for session %s: %v (fallback to git diff only)", sess.ID, err)
        }
        
        // Populate FileAgents map
        for _, file := range result.files {
            d.collabStore.AddFileEdit(file, sess.ID, FileEdit{
                AgentID:      sess.ID,
                SessionName:  sess.Name,
                FirstSeen:    time.Now(),  // Approximation (real time unknown)
                LastModified: time.Now(),
                Worktree:     sess.Worktree,
            })
        }
        
        // Re-populate BranchStatus
        if result.branch != "" {
            remoteURL, _ := getRemoteURL(sess.Worktree)
            d.collabStore.UpdateBranchStatus(result.branch, &BranchStatus{
                Name:      result.branch,
                Agents:    []string{sess.ID},
                RemoteURL: remoteURL,
                CIStatus:  "unknown",  // Will be updated on first BranchTracker tick
            })
        }
        
        log.Info("Recovered session %s: %d files, branch %s", sess.ID, len(result.files), result.branch)
    }
    
    log.Info("Startup recovery complete: %d/%d sessions recovered successfully", 
        completed, activeSessions)
    
    return nil
}

func max(a, b int) int {
    if a > b {
        return a
    }
    return b
}

func validateGitRepo(path string) error {
    // Check if bare repo
    cmd := exec.Command("git", "-C", path, "rev-parse", "--is-bare-repository")
    out, err := cmd.Output()
    if err != nil {
        return fmt.Errorf("not a git repository: %w", err)
    }
    if strings.TrimSpace(string(out)) == "true" {
        return fmt.Errorf("bare repository (not supported)")
    }
    
    // Check if .git exists
    gitDir := filepath.Join(path, ".git")
    if _, err := os.Stat(gitDir); err != nil {
        return fmt.Errorf(".git directory missing: %w", err)
    }
    
    return nil
}

func getCurrentBranch(worktree string) (string, error) {
    cmd := exec.Command("git", "-C", worktree, "rev-parse", "--abbrev-ref", "HEAD")
    out, err := cmd.Output()
    if err != nil {
        return "", err
    }
    branch := strings.TrimSpace(string(out))
    if branch == "HEAD" {
        return "", fmt.Errorf("detached HEAD state")
    }
    return branch, nil
}

func getRemoteURL(worktree string) (string, error) {
    cmd := exec.Command("git", "-C", worktree, "remote", "get-url", "origin")
    out, err := cmd.Output()
    if err != nil {
        return "", err
    }
    return strings.TrimSpace(string(out)), nil
}

func runGitDiff(worktree string) ([]string, error) {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    
    cmd := exec.CommandContext(ctx, "git", "-C", worktree, "diff", "--name-only", "HEAD")
    out, err := cmd.Output()
    if err != nil {
        return nil, err
    }
    
    lines := strings.Split(strings.TrimSpace(string(out)), "\n")
    files := make([]string, 0, len(lines))
    for _, line := range lines {
        if line != "" {
            files = append(files, line)
        }
    }
    return files, nil
}
```

**Shutdown Sequence (graceful, prevents goroutine leaks):**
```go
func (d *Daemon) Shutdown(ctx context.Context) error {
    log.Info("Daemon shutdown initiated")
    
    // 1. Cancel context (signals all monitors to stop)
    d.cancelFunc()
    
    // 2. Wait for monitors to finish (with timeout)
    done := make(chan struct{})
    go func() {
        d.wg.Wait()  // Waits for CollabMonitor, OverlapDetector, BranchTracker, cleanup goroutines
        close(done)
    }()
    
    select {
    case <-done:
        // All goroutines stopped cleanly
        log.Info("All monitors stopped gracefully")
    case <-ctx.Done():
        // Timeout exceeded, force shutdown
        return fmt.Errorf("shutdown timeout: monitors did not stop")
    }
    
    // 3. Close FSNotify watchers
    d.collabMonitor.Close()  // Closes all watchers, stops event goroutine
    
    // 4. Close plan file watcher
    d.overlapDetector.Close()  // Closes plan file FSNotify watcher
    
    // 5. Close GitHub API client
    d.githubClient.Close()  // Closes idle HTTP connections
    
    log.Info("Daemon shutdown complete")
    
    return nil
}
```

**Each monitor must:**
- Check ctx.Done() in main loop
- Use sync.WaitGroup (d.wg.Add(1) at start, defer d.wg.Done() at end)
- Clean up resources (close channels, watchers, HTTP clients)
- Return within shutdown timeout (10 seconds)

**Cleanup goroutines registered with WaitGroup:**
- MailboxDeduper.cleanupLoop (5-minute tick)
- NotificationDeduper.cleanupLoop (5-minute tick)
- CollabStore.gitDiffCacheCleanup (5-minute tick)
- CollabStore.planCacheCleanup (5-minute tick)
- OverlapDetector.planFileWatcher event handler

---

## Foundational Layer Hardening (Implemented Messaging & Context Store)

**Status:** Review of the *already-shipped* primitives this collaboration system builds on — `internal/mailbox` (directed per-recipient inboxes) and `internal/ctxstore` (shared key/value blackboard), plus their HTTP/CLI/MCP surface and the `hub` long-poll wake path. The collaboration features above (CollabMonitor/OverlapDetector/BranchTracker) are unbuilt; these primitives are live, so harden them first — every component above sends warnings through the mailbox and infers collaboration from the context store.

The implemented core is sound (atomic temp+rename writes, centralized key/recipient validation, a correctly-reasoned read-then-mark race in `handleInbox`, a clean cap-1 coalescing `hub`). The items below are the gaps found in review, each with a concrete fix and a severity. Fixes are ordered by priority.

### H1. Message & context provenance is unauthenticated (medium)

**Problem:** The `from` field is self-asserted — `handleSendMessage` stores `req.From` verbatim and the MCP `send_message`/`ctx_set` writers come from `ctxWriter()` (env), which an agent controls. Any caller can also *read* an arbitrary inbox via `warden msg inbox --as <id>` or `read_inbox{agent: <id>}`, and write context as anyone. For a localhost single-user daemon this is an acceptable trust model, but it is currently **implicit**, and the collaboration layer is about to start routing automated warnings on top of it.

**Solution — make the trust model explicit and stamp a trusted identity at the daemon edge:**

1. Document the boundary in this spec and in the `mailbox`/`ctxstore` package docs: *"`from`/`updated_by` is advisory provenance, not an authenticated identity; warden assumes a single trusted local user. Do not make security decisions on it."*
2. For messages the daemon itself originates (conflict/overlap/branch warnings), reserve a sender id that agents cannot forge by validation, not crypto: reject caller-supplied `from`/`updated_by` values matching `^(daemon|system|human)$` on the write path, so only the daemon can stamp `from: "daemon"`. Agent-originated values keep flowing through unchanged.

```go
// mailbox.Append / ctxstore.Set write gate
var reservedSenders = map[string]bool{"daemon": true, "system": true}
func sanitizeSender(from string, internal bool) (string, error) {
    if !internal && reservedSenders[from] {
        return "", ErrReservedSender // agents can't impersonate the daemon
    }
    if from == "" { from = "human" }
    return from, nil
}
```

3. (Optional, deferred) If multi-user is ever in scope, gate inbox reads so a session token can only read its own `id`; out of scope for the current single-user design.

### H2. Context store has no atomic read-modify-write (medium)

**Problem:** `ctxstore` exposes only `Get` and `Set`. The collaboration layer (and agents using the blackboard for coordination) will do read-modify-write — e.g. appending to `global.findings` or incrementing a counter — as a `Get` then `Set`, which lost-updates under concurrency. This is the gap most likely to corrupt real multi-agent coordination, since the whole point of the blackboard is concurrent shared state.

**Solution — add a compare-and-swap primitive and an append helper, both atomic under the existing write lock:**

```go
// CompareAndSet writes value only if the current value matches expected
// (expected "" means "key absent"). Returns ErrConflict on mismatch so the
// caller can re-read and retry. All under the single mu.Lock() already held
// by Set — no new lock, no torn read.
func (s *Store) CompareAndSet(key, expected, value, by string) (Entry, error) {
    if !validKey(key) { return Entry{}, ErrBadKey }
    s.mu.Lock()
    defer s.mu.Unlock()
    m, err := s.load()
    if err != nil { return Entry{}, err }
    cur, ok := m[key]
    if (!ok && expected != "") || (ok && cur.Value != expected) {
        return Entry{}, ErrConflict
    }
    e := Entry{Key: key, Value: value, UpdatedBy: by, UpdatedAt: time.Now().UTC()}
    m[key] = e
    if err := s.save(m); err != nil { return Entry{}, err }
    return e, nil
}

// Append atomically concatenates sep+value to an existing value (or creates it),
// the common "accumulate findings" case, with no read-modify-write race.
func (s *Store) Append(key, value, sep, by string) (Entry, error) { /* lock, load, concat, save */ }
```

Expose `CompareAndSet` over HTTP (`POST /context/{key}/cas` with `{expected, value}` → 409 on `ErrConflict`), CLI (`warden ctx cas <key> --expected <v> <value>`), and a `ctx_cas` MCP tool. Agents retry on 409. This keeps the "last write wins" simplicity for plain `Set` while giving coordination code a correct primitive.

### H3. Inboxes grow unbounded and rewrite the full file per op (low–medium)

**Problem:** Messages are only ever appended or marked-read; nothing prunes read history. Because `Append`/`MarkRead`/`Messages` each re-read and rewrite the *entire* inbox JSON, a long-lived agent's inbox grows without bound and every operation gets progressively slower — quadratic write cost over a session. The collaboration layer's automated warnings will accelerate this.

**Solution — bound retention with read-message compaction on write:**

1. On `Append`, after assigning the new id, drop already-read messages beyond a retention window so the file stays small while preserving unread + recent history:

```go
const (
    maxInboxMessages = 500          // hard cap on total retained
    readRetention    = 24 * time.Hour // keep read msgs this long for inbox/history views
)
// in Append, before save: compact in place
ms = compact(ms, maxInboxMessages, readRetention) // keep all unread; keep read if recent; cap total
```

   Unread messages are **never** dropped (they're undelivered work). Per-inbox ids remain monotonic because they're assigned from a high-water mark, not `len(ms)+1` — switch the id source to `max(existing ids)+1` so compaction can't recycle an id. *(This is a correctness fix to the current `strconv.Itoa(len(ms)+1)` scheme, which already collides if any message is ever removed.)*
2. Note in the package doc that inbox size is bounded by `maxInboxMessages`; the global `/messages` traffic view is best-effort recent history, not an audit log.

### H4. No long-poll wait for MCP agents (low)

**Problem:** `msg wait` (the single-blocking-call, no-busy-poll await-a-reply primitive) exists only on the CLI. MCP-only agents have just `read_inbox` (poll), so they busy-poll across LLM turns — exactly the token waste `messages/wait` was built to avoid. This is internally consistent with the spec's "CLI-first for MCP-restricted environments" stance, but it should be a deliberate, documented choice.

**Solution:** Add a `wait_for_message` MCP tool that proxies the existing `GET /sessions/{id}/messages/wait` long-poll (it already supports `from` filter and a capped timeout, so this is a thin wrapper — no new daemon logic):

```go
mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
    Name:        "wait_for_message",
    Description: "Block until a directed message arrives (or timeout), then return it. One call, no busy-polling across turns.",
}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a waitArgs) (*mcpsdk.CallToolResult, any, error) {
    m, err := s.cl.MsgWait(ctx, selfOr(a.Agent), a.From, a.TimeoutSec)
    // ... jsonResult(m) or "(timed out)"
})
```

If a tool is genuinely not wanted on the MCP surface, document in the MCP tools section that long-poll is intentionally CLI-only and MCP agents should shell out to `warden msg wait`.

### H5. Wake-on-send has a status TOCTOU (low)

**Problem:** `handleSendMessage` reads `sess.Status` from an earlier `store.Get`, then injects a keystroke notice via `s.life.Input` if `parked(...)`. An agent can transition idle→working in that window, so the notice can land in a now-busy pane. Impact is low (a stray line), but it's an unguarded race.

**Solution:** Treat injection as best-effort and re-check status as close to the write as possible — re-`Get` immediately before `Input`, and accept that `Input` may still race (document it). The message itself is already safely in the inbox regardless, so the wake is pure optimization:

```go
// message is durably appended above; waking is best-effort
if fresh, err := s.store.Get(r.Context(), id); err == nil && parked(fresh.Status) {
    if err := s.life.Input(r.Context(), fresh.TmuxSession, notice); err == nil { woke = true }
}
```

Add a one-line comment at the call site stating that wake is best-effort and the inbox is the source of truth — so no one later "hardens" it into a lock that would serialize sends against the poller.

### H6. Global message view aborts on a single corrupt inbox (low)

**Problem:** `mailbox.All()` (backing `GET /messages`) returns the first `load` error, so one unparseable `*.json` takes down the entire global traffic view. Per-inbox reads are correctly isolated; only the aggregate is brittle.

**Solution:** In `All()` only, skip-and-log a corrupt inbox instead of aborting — the aggregate view is observability, not consumption, so partial data beats no data:

```go
ms, err := s.load(filepath.Join(s.dir, ent.Name()))
if err != nil {
    log.Warn("mailbox: skipping unreadable inbox %s: %v", ent.Name(), err)
    continue // aggregate view is best-effort; don't fail the whole request
}
```

Keep `Messages(to)` strict (a caller asking for a specific inbox should hear that it's corrupt).

### Summary

| ID | Issue | Severity | Fix |
|----|-------|----------|-----|
| H1 | Unauthenticated provenance | medium | Document trust model; reserve `daemon`/`system` senders at write gate |
| H2 | No atomic read-modify-write on ctxstore | medium | Add `CompareAndSet` + `Append` (CLI/HTTP/MCP), retry on 409 |
| H3 | Unbounded inbox growth, full rewrite/op | low–med | Read-message compaction + cap; high-water-mark ids |
| H4 | No long-poll for MCP agents | low | `wait_for_message` MCP tool proxying existing wait route |
| H5 | Wake-on-send status TOCTOU | low | Re-`Get` before `Input`; document wake as best-effort |
| H6 | `All()` aborts on one corrupt inbox | low | Skip-and-log in aggregate view only |

H1 and H2 should land before the collaboration layer starts routing automated warnings and inferring collaboration from the blackboard; H3–H6 are incremental robustness.

---

## Concurrency Safety & Architecture

### Critical Design Principles

**1. Lock Hierarchy (prevents deadlocks):**
```
CollabStore.mu
  ↓ (never hold while calling)
Mailbox/Hub
  ↓ (never hold while calling)
HTTP/Git subprocess
```

**Rule:** Never call mailbox.Send(), hub.Publish(), or blocking I/O while holding CollabStore.mu.

**2. Thread-Safe State Access:**
- All CollabStore methods handle locking internally
- Callers never directly access `cs.FileAgents`, `cs.Branches`, etc.
- Read operations return copies (not pointers to internal state)

**3. Non-Blocking Event Handlers:**
- FSNotify events dispatched to worker pool (buffered channel)
- Worker pool processes events without blocking watcher
- If worker pool full, drop event with warning (degraded mode)

**4. Graceful Shutdown:**
- All goroutines check ctx.Done() in main loop
- Use sync.WaitGroup to track goroutine lifecycle
- Shutdown timeout: 10 seconds (hard limit)
- Context-aware HTTP requests (cancelled on shutdown)

**5. Resource Cleanup:**
- FSNotify watchers removed on session termination (via event subscription)
- GitHub API client closes idle connections on shutdown
- Git diff cache pruned every 5 minutes (prevent unbounded growth)

### Data Structure Design Decisions

**Why `map[string]map[string]FileEdit` instead of `map[string][]FileEdit`?**

Problem with slices:
```go
// UNSAFE: append creates new backing array
cs.mu.Lock()
cs.FileAgents["auth.go"] = append(cs.FileAgents["auth.go"], newEdit)
cs.mu.Unlock()

// Reader holding old slice reference won't see new edit
cs.mu.RLock()
edits := cs.FileAgents["auth.go"]  // Old slice
cs.mu.RUnlock()
// edits is stale
```

Solution with nested map:
```go
// SAFE: map operations are atomic updates
cs.mu.Lock()
if cs.FileAgents["auth.go"] == nil {
    cs.FileAgents["auth.go"] = make(map[string]FileEdit)
}
cs.FileAgents["auth.go"][agentID] = newEdit
cs.mu.Unlock()

// Reader always sees committed state
cs.mu.RLock()
agentMap := cs.FileAgents["auth.go"]
edits := make([]FileEdit, 0, len(agentMap))
for _, edit := range agentMap {
    edits = append(edits, edit)  // Copy
}
cs.mu.RUnlock()
```

**Why `atomic.Bool` for `WorkOverlap.dismissed`?**

Problem with plain bool:
```go
// UNSAFE: concurrent read/write is a data race
overlap.Dismissed = true  // Goroutine A
if overlap.Dismissed { ... }  // Goroutine B (data race)
```

Solution with atomic:
```go
overlap.dismissed.Store(true)  // Goroutine A (atomic)
if overlap.dismissed.Load() { ... }  // Goroutine B (atomic)
```

### Goroutine Leak Prevention

**FSNotify Watcher Cleanup:**

Problem: Watchers created per agent, but agents terminate asynchronously.

Solution: Subscribe to session lifecycle events (not polling):
```go
type CollabMonitor struct {
    watchers map[string]*fsnotify.Watcher  // sessionID -> watcher
    watcherMu sync.RWMutex
}

// Subscribe to session termination
store.OnSessionTerminated(func(sessionID string) {
    monitor.removeWatcher(sessionID)
})

func (cm *CollabMonitor) removeWatcher(sessionID string) {
    cm.watcherMu.Lock()
    defer cm.watcherMu.Unlock()
    
    if watcher, ok := cm.watchers[sessionID]; ok {
        watcher.Close()  // Stops goroutine, closes file descriptors
        delete(cm.watchers, sessionID)
    }
}
```

### Race Condition Checklist

Before implementation, verify each goroutine interaction:

- [ ] CollabMonitor never holds CollabStore.mu while sending to mailbox
- [ ] OverlapDetector releases lock before sending warnings
- [ ] BranchTracker releases lock before sending notifications
- [ ] HTTP handlers acquire lock, build response, release lock (never call I/O with lock)
- [ ] SSE events published after state committed (no partial updates visible)
- [ ] FSNotify event handler uses worker pool (non-blocking)
- [ ] All goroutines check ctx.Done() (graceful shutdown)
- [ ] All goroutines registered with sync.WaitGroup (including cleanup loops)
- [ ] GitHub API client uses context-aware requests
- [ ] Git subprocess calls have timeout (5-10 seconds)
- [ ] WorkOverlap.dismissed uses atomic.Bool (not plain bool)
- [ ] CollabStore methods return copies (not pointers to internal state)
- [ ] MailboxDeduper cleanup goroutine prunes old entries (prevent leak)
- [ ] NotificationDeduper cleanup goroutine prunes old entries (prevent leak)
- [ ] Plan file cache invalidated on FSNotify events
- [ ] Git diff cache invalidated on file write events

### Performance Optimizations

**1. Git Diff Caching:**
- Cache git diff results in CollabStore (TTL: 30 seconds)
- Invalidate on file write (FSNotify event)
- Prevents O(N²) git subprocess spawning in OverlapDetector

**2. Subject/Plan Similarity Caching:**
- Cache similarity scores in local map (TTL: 5 minutes)
- Key: sorted(agent1-ID, agent2-ID)
- Reduces CPU for token-based comparison

**3. Rate Limiting (OverlapDetector):**
- Max 200 comparisons per tick (30 seconds)
- If N² > 200: round-robin, resume next tick
- Prevents lock contention with many agents

**4. Early Exit Heuristics:**
- Skip subject comparison if no shared tokens
- Skip file comparison if subjects dissimilar
- Skip plan comparison if no plan files exist

**5. HTTP Handler Optimization:**
- Use RLock for all read-only endpoints
- Build response while holding lock (minimize lock hold time)
- Never call external services (mailbox/hub/git) with lock held

**6. Startup Recovery Optimization:**
- Parallel git operations (goroutine pool, limit 10 concurrent)
- Progressive availability (monitors start before recovery completes)
- Progress logging (prevents "daemon hung" false alarms)

**7. GitHub API Optimization:**
- Branch grouping (one API call per branch, not per agent): 80% reduction
- Conditional requests with ETag (304 Not Modified doesn't count against rate limit)
- Circuit breaker prevents hammering GitHub when it's down

**8. Plan File I/O Optimization:**
- FSNotify watcher on `docs/superpowers/specs/` directory
- Cache invalidation on CREATE/WRITE events
- Eliminates repeated file reads (100/minute → 1 per plan update)

---

## Implementation Phases

### Phase 1: MVP (1 week)

**Goal:** Basic conflict detection with CLI (thread-safe foundation)

**Tasks:**
- [ ] Create `internal/collab` package
- [ ] Implement `CollabStore` with thread-safe methods
  - [ ] Use `map[string]map[string]FileEdit` (not slices)
  - [ ] All methods handle locking internally
  - [ ] Return copies (not pointers to internal state)
- [ ] Implement `CollabMonitor` with git diff polling (no FSNotify yet)
  - [ ] Context-aware main loop (graceful shutdown)
  - [ ] Git diff with 5-second timeout
  - [ ] Never hold CollabStore.mu while sending to mailbox
  - [ ] Register with sync.WaitGroup
- [ ] File-level conflict detection
- [ ] CLI commands: `warden collab status`, `warden collab conflicts`, `warden collab who-is-editing`
- [ ] HTTP routes: `GET /collab/status`, `GET /collab/conflicts`
  - [ ] All handlers acquire appropriate locks
  - [ ] Path validation (reject "..")
- [ ] Integration with mailbox for warnings (with 5s timeout)
- [ ] Unit tests for CollabStore and CollabMonitor
  - [ ] Run with `-race` flag (detect data races)
- [ ] Update `internal/daemon/server.go` to start CollabMonitor goroutine
  - [ ] Pass context for cancellation
  - [ ] Implement graceful shutdown

**Deliverable:** Users can see file conflicts in CLI and web UI, agents receive warnings via inbox. **All code passes race detector.**

### Phase 2: Real-time Detection (2-3 days)

**Goal:** Add FSNotify for subsecond conflict detection (non-blocking, leak-free, watch budget enforcement)

**Tasks:**
- [ ] Add `github.com/fsnotify/fsnotify` dependency
- [ ] **CRITICAL: Implement FSNotify watch budget management**
  - [ ] Read inotify limit from `/proc/sys/fs/inotify/max_user_watches` (Linux)
  - [ ] Track total watch count across all agents (atomic.Int64)
  - [ ] Enforce 80% budget threshold before creating new watchers
  - [ ] Fallback strategy: increase git diff polling frequency (10s → 2s) when budget exhausted
  - [ ] Desktop notification when watch budget exhausted (with sysctl command)
  - [ ] Count files in worktree to estimate watch usage
- [ ] Implement FSNotify watcher creation/cleanup in CollabMonitor
  - [ ] Watcher map protected by separate mutex (not CollabStore.mu)
  - [ ] Subscribe to session termination events (not polling)
  - [ ] Close watchers on session termination (prevent leaks)
  - [ ] Close watchers on shutdown (graceful cleanup)
  - [ ] Decrement watch count when watcher removed
- [ ] Implement worker pool for FSNotify events (5 goroutines)
  - [ ] Buffered work queue (size: 100)
  - [ ] Drop events if queue full (log warning, degraded mode)
  - [ ] Workers never hold locks while sending to mailbox
- [ ] Handle watcher events (file writes trigger immediate check)
- [ ] Test with large repos (document inotify limits)
  - [ ] Verify watch budget enforcement prevents silent failures
  - [ ] Test fallback to increased polling frequency
- [ ] Add MCP tools: `who_is_editing_file`, `get_collaboration_status`
  - [ ] All tools use thread-safe CollabStore methods
- [ ] Update MCP server (`internal/mcp/server.go`) with new tools
- [ ] Integration test: verify no goroutine leaks (multiple agent spawn/terminate cycles)
- [ ] Load test: spawn 50 agents, verify watch budget prevents exhaustion

**Deliverable:** Agents warned within 1 second of file conflict. **No goroutine or FD leaks under load. Watch budget prevents silent failures at scale.**

### Phase 3: Collaboration Groups & Overlap Detection (3-4 days)

**Goal:** Detect duplicate work and allow explicit collaboration marking (with O(N²) mitigation and parallel processing)

**Tasks:**
- [ ] Implement collaboration group management in CollabStore
  - [ ] Thread-safe group add/remove/list methods
- [ ] CLI commands: `warden collab create`, `warden collab add`, `warden collab delete`, `warden collab groups`
- [ ] HTTP routes: `POST /collab/groups`, `DELETE /collab/groups/:name`
  - [ ] Input validation (sanitize group names)
- [ ] **Implement FSNotify watcher on `docs/superpowers/specs/` directory**
  - [ ] Create watcher during OverlapDetector initialization
  - [ ] Handle CREATE/WRITE events to invalidate plan cache
  - [ ] Extract session ID from plan filename
  - [ ] Close watcher on shutdown (register with WaitGroup)
- [ ] **Implement plan file cache in CollabStore**
  - [ ] Add `cachedPlanFile` struct with pre-tokenized content
  - [ ] Thread-safe Get/Set/Invalidate methods
  - [ ] 5-minute TTL with background cleanup goroutine
- [ ] Implement `OverlapDetector` goroutine
  - [ ] Context-aware main loop (graceful shutdown)
  - [ ] **Worker pool (10 goroutines) for parallel comparisons**
  - [ ] Buffered work queue (size: 500) for comparison tasks
  - [ ] Use cached git diff from CollabStore (don't spawn git N² times)
  - [ ] Use cached plan files from CollabStore (don't read disk N² times)
  - [ ] Rate limiting: max 200 comparisons per tick
  - [ ] Round-robin if N² > 200 (track offset between ticks)
  - [ ] Subject/plan similarity caching (5-minute TTL, local map)
  - [ ] Early exit heuristics (skip if subjects dissimilar)
  - [ ] Release lock before sending warnings
  - [ ] Register with sync.WaitGroup
- [ ] Subject similarity (token-based Jaccard, case-insensitive)
- [ ] File overlap analysis (from cache, handle division by zero)
- [ ] Plan file discovery and comparison (from cache)
- [ ] Collaboration detection (pipeline, group, branch, messages, context)
- [ ] Overlap warnings to agent inboxes (no locks held)
- [ ] CLI commands: `warden collab overlaps`, `warden collab dismiss-overlap`
- [ ] MCP tools: `query_agent_work`, `dismiss_work_overlap`, `create_collab_group`
- [ ] Use `atomic.Bool` for `WorkOverlap.dismissed` field
- [ ] Unit tests for OverlapDetector
  - [ ] Run with `-race` flag
  - [ ] Test with 100 agents (verify rate limiting and parallelization works)
  - [ ] Test plan cache invalidation on file write
- [ ] Background cleanup goroutines (all register with WaitGroup):
  - [ ] Prune dismissed overlaps > 24h old (5-minute tick)
  - [ ] Prune similarity cache entries > 5 minutes old (5-minute tick)
  - [ ] Prune plan cache entries > 5 minutes old (5-minute tick)

**Deliverable:** System detects and warns about duplicate work before code is written. **Scales to 100+ agents with parallel processing.**

### Phase 4: Branch Monitoring (4-5 days)

**Goal:** Track CI status and branch lifecycle (graceful shutdown, no HTTP hangs, branch grouping optimization)

**Tasks:**
- [ ] Implement `BranchTracker` goroutine
  - [ ] Context-aware main loop with select on ctx.Done()
  - [ ] **Branch grouping optimization: group sessions by branch name**
  - [ ] **One GitHub API call per unique branch (not per agent)**
  - [ ] Git commands with 10-second timeout
  - [ ] Release lock before sending notifications
  - [ ] Register with sync.WaitGroup
- [ ] GitHub API client (`internal/collab/github.go`)
  - [ ] Context-aware HTTP client (10s timeout per request)
  - [ ] Close idle connections on shutdown
  - [ ] Parse GitHub remote URL (SSH/HTTPS)
  - [ ] **Conditional requests with If-None-Match (ETag) header**
  - [ ] **Handle 304 Not Modified response (doesn't count against rate limit)**
  - [ ] **Store ETag in cache for next request**
  - [ ] GET commit status (with context cancellation and ETag)
  - [ ] GET workflow runs (with context cancellation)
  - [ ] Rate limiting (5000 req/hour, track usage)
  - [ ] Caching (2-minute TTL, thread-safe cache map with ETag)
  - [ ] Circuit breaker (5 consecutive failures → 5-minute cooldown)
  - [ ] Exponential backoff retry (3 attempts: 1s, 2s, 4s)
  - [ ] Error classification (401/403/404/429/5xx different handling)
  - [ ] Error handling (GitHub API down = log warning, continue)
- [ ] Git commands for merge/behind detection
  - [ ] `git branch -r --merged` (with timeout)
  - [ ] `git rev-list --count main..<branch>` (with timeout)
- [ ] Desktop notification system
  - [ ] macOS: `osascript`
  - [ ] Linux: `notify-send`
  - [ ] Fallback: pending notifications queue (thread-safe)
  - [ ] **Deduplication: max 1 notification per branch per 5 minutes**
  - [ ] **Cleanup goroutine: prune deduper entries > 1 hour old**
- [ ] Main/master update detection and agent notifications
- [ ] CLI commands: `warden branch status`, `warden branch notifications`
- [ ] HTTP routes: `GET /branches`, `GET /branches/:name`, `GET /branches/notifications`
  - [ ] All handlers use thread-safe CollabStore methods
- [ ] MCP tool: `get_branch_status`
- [ ] Unit tests for BranchTracker and GitHubClient
  - [ ] Run with `-race` flag
  - [ ] Test graceful shutdown (verify no hanging HTTP requests)
  - [ ] Mock GitHub API for tests
  - [ ] Test branch grouping (verify only 1 API call for N agents on same branch)
  - [ ] Test ETag conditional requests (verify 304 handling)
  - [ ] Test circuit breaker activation and cooldown
- [ ] Integration test with real GitHub repo (CI)
- [ ] Load test: 50 agents on 10 branches, verify < 300 API requests/hour

**Deliverable:** Users notified of CI failures with one-click debug option, agents notified of branch drift. **Daemon shuts down cleanly (no HTTP hangs). GitHub API calls reduced by 80% via branch grouping and ETags.**

### Phase 5: Polish & Web UI (2-3 days)

**Goal:** Production-ready with web dashboard and parallel startup recovery

**Tasks:**
- [ ] **Parallel startup recovery optimization**
  - [ ] Worker pool (10 concurrent) for git operations during startup
  - [ ] Progress logging ("Recovering 50 agents... 42/50 complete")
  - [ ] Start monitors before recovery completes (progressive availability)
  - [ ] Semaphore to limit concurrent git diffs
  - [ ] Test with 100 sessions (verify startup time < 10 seconds)
- [ ] Web UI: Collaboration dashboard page
  - [ ] Real-time conflict list with SSE updates
  - [ ] Work overlap warnings with dismiss button
  - [ ] Branch status table with CI badges
  - [ ] Collaboration group management UI
  - [ ] Watch budget indicator (show current/max watches)
- [ ] Performance optimization verification
  - [ ] ✅ FileAgents already O(1) lookup (nested map)
  - [ ] ✅ Subject similarity already cached (5-minute TTL)
  - [ ] ✅ Git diff already cached (30-second TTL)
  - [ ] ✅ Plan files already cached with FSNotify invalidation
  - [ ] ✅ Branch grouping already implemented
  - [ ] ✅ Parallel overlap detection already implemented
- [ ] Documentation
  - [ ] Update README with collaboration features
  - [ ] Add examples to USAGE.md
  - [ ] Document GitHub token setup
  - [ ] **Document inotify limits on Linux with sysctl command**
  - [ ] **Document watch budget management and fallback behavior**
  - [ ] **Document branch grouping optimization (80% API reduction)**
  - [ ] **Document performance characteristics (scales to 100+ agents)**
- [ ] Integration tests
  - [ ] End-to-end conflict detection scenario
  - [ ] Overlap detection with real plan files
  - [ ] Branch tracking with mock GitHub API
  - [ ] **Watch budget enforcement (spawn agents until budget hit)**
  - [ ] **Plan cache invalidation (modify plan file, verify cache cleared)**
  - [ ] **Parallel startup recovery (100 sessions, measure time)**
- [ ] Error handling and logging
  - [ ] Structured logging for all monitors
  - [ ] Graceful degradation (FSNotify failure, GitHub API down)
  - [ ] **Prominent warnings for watch budget exhaustion**
  - [ ] **Circuit breaker logs when GitHub API down**
- [ ] Final race detector check
  - [ ] Run all tests with `-race` flag
  - [ ] Run integration tests with `-race` flag
  - [ ] Verify no data races detected

**Deliverable:** Production-ready collaboration system with polished UX. **Scales to 100+ concurrent agents with efficient resource usage.**

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
- **Branch grouping: one API call per unique branch (not per agent)**
- **Conditional requests with ETag: 304 Not Modified doesn't count against rate limit**
- Only check branches with active agents
- Stop tracking merged branches immediately
- Circuit breaker stops calling API after repeated failures (5-minute cooldown)

**Calculation without optimizations:** 
- 50 agents × 30 checks/hour = 1,500 requests/hour (30% of limit)

**Calculation with branch grouping:**
- 50 agents on 10 unique branches × 30 checks/hour = 300 requests/hour (6% of limit)

**Calculation with branch grouping + ETags (assuming 80% cache hit rate):**
- 300 requests/hour × 20% = **60 requests/hour (1.2% of limit)**

**Scalability:** With optimizations, system can handle **250+ agents** before approaching rate limit.

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

## Appendix: Architectural Issues Fixed

This section documents critical concurrency bugs identified during design review and their fixes.

### Issue 1: Data Races in CollabStore ❌→✅

**Problem (Original Design):**
- `CollabStore` had mutex but no usage shown in algorithms
- Three goroutines (CollabMonitor, OverlapDetector, BranchTracker) accessing maps concurrently
- Slices in `FileAgents map[string][]FileEdit` are unsafe (append creates new backing array)

**Fix:**
- Changed to `map[string]map[string]FileEdit` (nested map, no slices)
- All CollabStore methods handle locking internally
- Methods return copies (not pointers to internal state)
- Callers never directly access internal fields

### Issue 2: FSNotify Goroutine Leak ❌→✅

**Problem (Original Design):**
- Watchers created per agent, cleaned up via 10-second polling
- If agent terminates between polls, watcher leaks (goroutine + FD)
- No mechanism to detect session termination

**Fix:**
- Subscribe to session termination events (not polling)
- Watcher map: `map[sessionID]*fsnotify.Watcher`
- Close watcher immediately on termination event
- Close all watchers on daemon shutdown

### Issue 3: FSNotify Event Channel Blocking ❌→✅

**Problem (Original Design):**
- FSNotify events processed synchronously
- If "immediate conflict check" is slow (git diff, mailbox send), handler blocks
- Blocked handler → FSNotify can't send more events → deadlock

**Fix:**
- Worker pool (5 goroutines) processes events asynchronously
- Buffered work queue (size: 100)
- Non-blocking dispatch: drop events if queue full (log warning)
- Workers never hold locks while doing I/O

### Issue 4: FileAgents Slice Race ❌→✅

**Problem (Original Design):**
- `append()` to slice creates new backing array
- Goroutine A reads slice with read lock, Goroutine B appends with write lock
- Goroutine A's slice reference is stale (doesn't see new edits)

**Fix:**
- Use nested map: `map[string]map[string]FileEdit` (agent ID as inner key)
- Map updates are atomic (no stale references)
- GetFileEdits() returns copy of slice (safe to iterate)

### Issue 5: OverlapDetector O(N²) Lock Contention ❌→✅

**Problem (Original Design):**
- 100 agents = 10,000 comparisons every 30 seconds
- Each comparison runs git diff (20,000 git subprocesses)
- All holding CollabStore lock → other goroutines blocked

**Fix:**
- Cache git diff results in CollabStore (TTL: 30 seconds)
- Cache subject/plan similarity (TTL: 5 minutes)
- Rate limiting: max 200 comparisons per tick
- Early exit heuristics (skip if subjects dissimilar)
- Release lock before sending warnings

### Issue 6: BranchTracker Shutdown Hang ❌→✅

**Problem (Original Design):**
- No graceful shutdown mechanism
- If daemon shuts down mid-GitHub API call, goroutine hangs
- No timeout on HTTP requests

**Fix:**
- Context-aware main loop (select on ctx.Done())
- HTTP client uses context for cancellation
- 10-second timeout per HTTP request
- CloseIdleConnections() on shutdown

### Issue 7: HTTP Routes Without Mutex ❌→✅

**Problem (Original Design):**
- HTTP handlers run in separate goroutines (one per request)
- No locking shown in route descriptions
- Race detector would panic immediately

**Fix:**
- All handlers acquire appropriate locks (RLock for reads, Lock for writes)
- Use CollabStore's thread-safe methods
- Build response while holding lock, then release

### Issue 8: SSE Publishing Race ❌→✅

**Problem (Original Design):**
- `hub.publish()` called before `collabStore.AddConflict()` finishes
- HTTP handler could read state between AddConflict and publish
- Web UI sees stale data

**Fix:**
- Pattern: 1) Update state with lock, 2) Release lock, 3) Publish to hub
- Hub sees committed state only (no partial updates)
- Web UI fetches full state via HTTP (atomic read with lock)

### Issue 9: Mailbox.Send Blocking Deadlock ❌→✅

**Problem (Original Design):**
- CollabMonitor holds `CollabStore.mu` while calling `mailbox.Send()`
- If mailbox uses channels and inbox is full, Send() blocks
- Agent tries to read inbox while holding another lock
- Deadlock: Monitor waits for agent, agent waits for monitor

**Fix:**
- NEVER hold CollabStore.mu while calling mailbox.Send()
- Pattern: 1) Read state with lock, 2) Release lock, 3) Send to mailbox
- Use context with 5-second timeout for Send() (fail-safe)

### Issue 10: Dismissed Flag Race ❌→✅

**Problem (Original Design):**
- `Dismissed bool` field is not atomic in Go
- Concurrent reads/writes = data race
- User dismisses overlap while OverlapDetector reads it

**Fix:**
- Use `dismissed atomic.Bool` instead of plain bool
- IsDismissed() and Dismiss() methods for thread-safe access

### Issue 11: GitHub API Error Handling ❌→✅

**Problem (Original Design):**
- No retry logic (single failed request = permanent failure)
- No circuit breaker (continues calling API even when GitHub is down)
- No error classification (treats 404, 429, 500 the same)

**Fix:**
- Exponential backoff retry (3 attempts with 1s, 2s, 4s delays)
- Circuit breaker (stop calling after 5 consecutive failures, cooldown 5 minutes)
- Error classification (401/403 = auth, 404 = not found, 429 = rate limit, 5xx = server error)
- Proper HTTP status code checking and JSON parsing error handling

### Issue 12: Daemon Startup Recovery ❌→✅

**Problem (Original Design):**
- No state recovery after daemon crash
- Agents become invisible until they modify files
- FSNotify watchers not recreated
- CollabStore empty on restart

**Fix:**
- Startup() method scans for active sessions
- Re-creates FSNotify watchers for existing worktrees
- Populates FileAgents map via git diff
- Populates BranchStatus map via git commands
- Validates worktrees (skip deleted/corrupted ones)

### Issue 13: File Overlap Edge Cases ❌→✅

**Problem (Original Design):**
- Division by zero when both agents have no files
- 100% overlap not distinguished from partial overlap
- Case-sensitive subject tokenization

**Fix:**
- Check for empty union before division
- Special case 100% overlap as "critical" conflict (not just warning)
- Case-insensitive tokenization with `strings.ToLower()`
- Plan file matching with word boundaries (prevent substring false matches)

### Issue 14: Notification Spam ❌→✅

**Problem (Original Design):**
- Flaky CI test sends 10 notifications in 10 minutes
- Same file conflict warning sent every second via FSNotify
- No deduplication

**Fix:**
- Desktop notification deduper (max 1 per branch per 5 minutes)
- Mailbox message deduper (max 1 identical message per minute)
- Prevents spam while still notifying on genuine issues

### Issue 15: SSE Reconnection ❌→✅

**Problem (Original Design):**
- Client disconnects, misses events during downtime
- No way to replay missed events on reconnect

**Fix:**
- Sequence numbers on all SSE events
- Event history buffer (last 100 events)
- Support for `Last-Event-ID` header on reconnect
- Replay missed events before resuming live stream

---

## CRITICAL DESIGN REVIEW: Simplification Analysis

**Date:** 2026-06-14 (Post-Design Review)  
**Reviewer:** Engineering Review  
**Status:** ⚠️ COMPLEXITY ALERT - Requires Human Decision

### Executive Summary

**Current design: 3000+ lines, 4-6 weeks implementation**  
**Proposed MVP: ~200 lines, 3-5 days implementation**  
**Complexity reduction: 95%**

This section presents radical simplification recommendations based on the 80/20 principle. The core question: **What's the minimum viable feature set that delivers real value?**

---

### Complexity Analysis

#### Current Design Metrics

| Category | Count | Complexity Driver |
|----------|-------|-------------------|
| **Total lines of spec** | 3000+ | 3 major systems + optimizations |
| **Estimated LOC** | 3000-4000 | FSNotify, GitHub API, overlap detection |
| **Goroutines** | 13+ | 3 monitors + 10 workers + cleanup loops |
| **Dependencies** | 1 (fsnotify) | External C bindings, OS-specific |
| **Data structures** | 10+ | Caches, dedupers, circuit breakers |
| **Edge cases documented** | 50+ | inotify limits, ETags, reconnection |
| **Implementation phases** | 5 | 2-3 weeks estimated |

#### Minimal MVP Metrics

| Category | Count | Simplification |
|----------|-------|----------------|
| **Total lines of spec** | This section (~500 lines) | 85% reduction |
| **Estimated LOC** | ~200 | Single monitor, git polling only |
| **Goroutines** | 1 | Simple ticker loop |
| **Dependencies** | 0 | Pure Go, git CLI only |
| **Data structures** | 0 new | Reuse existing store.Session |
| **Edge cases documented** | 2 | Git timeout, missing worktree |
| **Implementation phases** | 1 | 3-5 days |

---

### Radical Simplification Recommendations

#### 🔪 CUT FROM MVP

##### 1. FSNotify - REMOVE ENTIRELY ❌

**Current complexity:**
- Watch budget management (150+ lines)
- inotify limit detection and enforcement
- Watcher creation/cleanup lifecycle
- Worker pool for event processing
- File counting in worktrees
- Desktop notifications for budget exhaustion
- OS-specific behavior (Linux vs macOS)
- Goroutine leak prevention
- File descriptor leak prevention

**Why cut:**
- Adds ~40% of implementation complexity
- 10-second git diff polling is **good enough** for conflict detection
- Real question: When do two agents edit the same file within 10 seconds? **Rare.**
- FSNotify doesn't scale (inotify limits hit at ~10 agents in large repos)
- Requires OS-specific workarounds and user configuration

**Alternative (20 lines):**
```go
func (m *Monitor) Run(ctx context.Context) {
    ticker := time.NewTicker(10 * time.Second)
    defer ticker.Stop()
    
    for {
        select {
        case <-ticker.C:
            m.checkConflicts(ctx)
        case <-ctx.Done():
            return
        }
    }
}
```

**User impact:** File conflicts detected in 10 seconds instead of 1 second. **This is acceptable.**

**Decision point:** If users complain about 10s latency (they won't), add FSNotify in Phase 2.

---

##### 2. OverlapDetector - CUT FOR MVP ❌

**Current complexity:**
- O(N²) comparison algorithm requiring mitigation
- 10-worker goroutine pool for parallelization
- Subject tokenization and Jaccard similarity
- File overlap calculation with caching
- Plan file discovery and parsing
- Plan file FSNotify watcher with cache invalidation
- Collaboration detection hierarchy (5 signals)
- Rate limiting (max 200 comparisons/tick)
- Round-robin offset tracking
- Similarity score caching (3 separate caches)
- Background cleanup goroutines (3 separate)

**Why cut:**
- Adds ~35% of implementation complexity
- High false positive risk (70% confidence in example scenario is **not trustworthy**)
- Token-based similarity is crude ("refactor auth" vs "update authentication" = 43.5% confidence)
- O(N²) scaling requires extensive mitigation even with worker pools
- Plan file parsing assumes agents write plan files (unvalidated assumption)
- Preventative value is speculative

**Real-world check:**
- How often do users accidentally spawn duplicate agents?
- Do agents actually write plan files in `docs/superpowers/specs/`?
- Would you trust a 70% confidence warning?

**Alternative for MVP:**
Users can explicitly check who's working on what:
```bash
warden list --verbose  # shows all agents and their subjects
warden collab agents   # proposed: list agents with their work
```

If two agents have similar subjects, **the user will notice** when listing agents.

**Coordination mechanism already exists:**
```bash
warden send agent-456 "What are you working on?"
warden inbox  # read responses
```

**Decision point:** Collect data first. If users report duplicate work problems, build overlap detection in Phase 2 with validated requirements.

---

##### 3. BranchTracker - DEFER TO PHASE 2 ❌

**Current complexity:**
- GitHub API client with context, timeouts, ETags
- Branch grouping optimization
- Circuit breaker with state machine (closed/open/half-open)
- Exponential backoff retry (3 attempts with backoff)
- Error classification (5 error types)
- Conditional HTTP requests with If-None-Match
- Desktop notifications (OS-specific)
- Notification deduplication (5-minute window)
- Git merge detection
- Commits-behind calculation
- Main/master update notifications
- Branch discovery and lifecycle tracking

**Why defer:**
- Adds ~25% of implementation complexity
- Completely orthogonal to file conflict detection (different problem domain)
- GitHub already emails on CI failure
- Users already have `gh pr checks` for CI status
- Desktop notifications require platform-specific code
- Requires GITHUB_TOKEN setup (adoption barrier)

**Alternative:**
Users manually check CI status:
```bash
gh pr checks           # GitHub CLI built-in
gh run view            # view specific run
gh run watch           # watch live
```

**Reality check:**
- How many users will configure GITHUB_TOKEN?
- How many users want daemon to monitor CI (vs checking manually)?
- Is CI monitoring a warden responsibility or a GitHub/IDE responsibility?

**Decision point:** This is a **separate feature**, not part of conflict detection. Build as standalone feature in Phase 2+ if users request it.

---

##### 4. All Caching & Optimization - CUT FOR MVP ❌

**Current complexity being cut:**

- **Git diff caching** (30-second TTL, cache invalidation)
- **Plan file caching** (5-minute TTL, FSNotify invalidation)
- **Subject similarity caching** (5-minute TTL, local map)
- **GitHub API caching** (2-minute TTL, ETag support)
- **3-4 background cleanup goroutines** (prevent memory leaks)

**Why cut:**
- Premature optimization
- Caching adds ~10% complexity for marginal gains
- MVP with 5-10 agents doesn't need caching
- Running `git diff` every 10 seconds on 10 worktrees is **trivial cost**

**Alternative:**
Just run the git commands. No caching. Simple.

**Decision point:** Add caching when profiling shows it's needed (it won't).

---

##### 5. All Deduplication - CUT FOR MVP ❌

**Current complexity being cut:**

- **MailboxDeduper** (1-minute window, cleanup goroutine)
- **NotificationDeduper** (5-minute window, cleanup goroutine)

**Why cut:**
- Premature optimization for a problem that doesn't exist yet
- If spam becomes a problem, add it **after observing the problem**
- Adds complexity (2 more goroutines, 2 more data structures)

**Alternative:**
Send notifications. If users complain about spam, add deduplication.

**Decision point:** YAGNI (You Aren't Gonna Need It). Build when needed.

---

##### 6. Parallel Startup Recovery - CUT FOR MVP ❌

**Current complexity being cut:**

- Worker pool with semaphore (10 concurrent git operations)
- Progress logging ("Recovering 42/50 sessions...")
- Progressive availability (monitors start before recovery completes)

**Why cut:**
- Adds ~5% complexity
- Startup recovery is **not the hot path**
- How often does daemon crash? Rarely.
- Simple sequential recovery is easier to debug and verify correctness

**Alternative:**
```go
for _, sess := range sessions {
    if sess.Worktree != "" {
        files := gitDiffFiles(sess.Worktree)
        // populate state...
    }
}
```

Sequential. Simple. Obviously correct.

**Decision point:** Only optimize startup if users complain about slow daemon restarts (they won't).

---

##### 7. SSE Reconnection & Event Replay - CUT FOR MVP ❌

**Current complexity being cut:**

- Sequence numbers on events
- Event history buffer (last 100 events)
- Last-Event-ID header support
- Replay missed events logic

**Why cut:**
- Adds ~5% complexity
- Web UI can just poll `/collab/status` every 5 seconds
- SSE is nice-to-have, not critical path
- Polling is simpler and works everywhere

**Alternative:**
```javascript
// Web UI: simple polling
setInterval(() => {
    fetch('/collab/status').then(r => r.json()).then(update);
}, 5000);
```

**Decision point:** SSE is optimization. Start with polling. Add SSE later if needed.

---

##### 8. Collaboration Groups - CUT FOR MVP ❌

**Current complexity being cut:**

- CollabGroup data structure
- 4 CLI commands (create, add, delete, groups)
- 2 HTTP routes (POST, DELETE)
- Group membership tracking
- Collaboration detection hierarchy

**Why cut:**
- Premature feature
- Users can just **dismiss false positives** instead
- Adds ~5% complexity for marginal value
- No validation that users will want this

**Alternative:**
```bash
warden collab dismiss-overlap agent-123-agent-456
```

If dismissal becomes tedious, **then** add collaboration groups.

**Decision point:** Build when users complain about too many dismissals.

---

#### ✅ KEEP IN MVP

##### 1. File Conflict Detection ✅

**Core value proposition:**
> "Tell me when two agents are editing the same file so I can coordinate before merge conflicts."

**Implementation (~200 lines):**
```go
package collab

import (
    "context"
    "fmt"
    "os/exec"
    "strings"
    "time"
)

type Monitor struct {
    store   store.Store
    mailbox mailbox.Store
}

func (m *Monitor) Run(ctx context.Context) {
    ticker := time.NewTicker(10 * time.Second)
    defer ticker.Stop()
    
    for {
        select {
        case <-ticker.C:
            m.checkConflicts(ctx)
        case <-ctx.Done():
            return
        }
    }
}

func (m *Monitor) checkConflicts(ctx context.Context) {
    sessions, err := m.store.List(ctx)
    if err != nil {
        return
    }
    
    // Build map: file -> agents editing it
    fileAgents := make(map[string][]AgentInfo)
    
    for _, sess := range sessions {
        if sess.Worktree == "" || sess.Status != "working" {
            continue
        }
        
        files := gitDiffFiles(sess.Worktree)
        for _, file := range files {
            fileAgents[file] = append(fileAgents[file], AgentInfo{
                ID:   sess.ID,
                Name: sess.Name,
            })
        }
    }
    
    // Detect and notify conflicts
    for file, agents := range fileAgents {
        if len(agents) > 1 {
            m.notifyConflict(ctx, file, agents)
        }
    }
}

func gitDiffFiles(worktree string) []string {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    
    cmd := exec.CommandContext(ctx, "git", "-C", worktree, 
        "diff", "--name-only", "HEAD")
    out, err := cmd.Output()
    if err != nil {
        return nil
    }
    
    lines := strings.Split(strings.TrimSpace(string(out)), "\n")
    var files []string
    for _, line := range lines {
        if line != "" {
            files = append(files, line)
        }
    }
    return files
}

func (m *Monitor) notifyConflict(ctx context.Context, file string, agents []AgentInfo) {
    for i, agent := range agents {
        others := make([]string, 0, len(agents)-1)
        for j, other := range agents {
            if i != j {
                others = append(others, fmt.Sprintf("%s (%s)", other.ID, other.Name))
            }
        }
        
        msg := fmt.Sprintf(
            "⚠️  Collaboration Warning: File Conflict\n"+
            "File: %s\n"+
            "Also being edited by: %s\n"+
            "Consider coordinating to avoid merge conflicts.",
            file, strings.Join(others, ", "))
        
        m.mailbox.Send(ctx, agent.ID, msg)
    }
}

type AgentInfo struct {
    ID   string
    Name string
}
```

**CLI commands (2):**
```bash
warden collab conflicts             # list current conflicts
warden collab who-is-editing FILE   # check specific file
```

**HTTP routes (1):**
```
GET /collab/conflicts  # for web UI
```

**MCP tools (2):**
```
who_is_editing_file
get_collaboration_status (conflicts only)
```

**Edge cases (2):**
- Git diff timeout (5 seconds)
- Missing/deleted worktree (skip gracefully)

**That's it. Ship it.**

---

### Comparison Table

| Feature | Full Design | Minimal MVP | Removed Complexity |
|---------|-------------|-------------|-------------------|
| **FSNotify watchers** | ✅ With budget enforcement | ❌ Git polling only | 40% complexity |
| **Overlap detection** | ✅ With worker pools | ❌ Defer to Phase 2 | 35% complexity |
| **Branch/CI tracking** | ✅ With GitHub API | ❌ Defer to Phase 2 | 25% complexity |
| **Git diff caching** | ✅ 30s TTL | ❌ Just run git | 3% complexity |
| **Plan file caching** | ✅ FSNotify invalidation | ❌ N/A (no overlap detector) | 3% complexity |
| **Subject similarity cache** | ✅ 5min TTL | ❌ N/A (no overlap detector) | 2% complexity |
| **GitHub API cache** | ✅ ETag support | ❌ N/A (no branch tracker) | 3% complexity |
| **Mailbox deduper** | ✅ With cleanup | ❌ Just send | 2% complexity |
| **Notification deduper** | ✅ 5min window | ❌ N/A (no notifications) | 2% complexity |
| **Parallel startup** | ✅ 10 workers | ❌ Sequential | 2% complexity |
| **SSE reconnection** | ✅ Event replay | ❌ Polling | 2% complexity |
| **Collaboration groups** | ✅ Full CRUD | ❌ Dismiss only | 3% complexity |
| **Circuit breaker** | ✅ 3-state FSM | ❌ N/A (no GitHub API) | 2% complexity |
| **Worker pools** | ✅ 10+5 goroutines | ❌ Single goroutine | 3% complexity |
| **Cleanup goroutines** | ✅ 4 separate | ❌ None needed | 2% complexity |
| | | | |
| **Total LOC** | ~3500 | ~200 | **95% reduction** |
| **Time to ship** | 4-6 weeks | 3-5 days | **85% faster** |
| **Goroutines** | 13+ | 1 | **92% simpler** |
| **Dependencies** | fsnotify | none | **100% removed** |

---

### The Real Question

**If you had to ship something useful in 3 days, what would it be?**

**Answer:** File conflict detection. That's it.

Not:
- ❌ Watch budget enforcement
- ❌ Circuit breakers
- ❌ ETag caching
- ❌ Worker pools
- ❌ Plan file caching
- ❌ Notification dedupers
- ❌ SSE reconnection
- ❌ Overlap detection
- ❌ CI tracking

Just:
- ✅ "Tell me when two agents are editing the same file"

---

### Implementation Timeline Comparison

#### Full Design Timeline (4-6 weeks)

**Week 1:** Phase 1 MVP
- CollabStore with thread-safe nested maps
- CollabMonitor with git diff polling
- CLI commands (3)
- HTTP routes (2)
- Unit tests with race detector

**Week 2:** Phase 2 FSNotify
- Watch budget management
- Watcher creation/cleanup
- Worker pool for events
- Load testing with 50 agents
- inotify limit handling

**Week 3:** Phase 3 Overlap Detection
- OverlapDetector with worker pool
- Plan file caching with FSNotify
- Subject/file/plan similarity
- Collaboration detection
- Rate limiting and caching

**Week 4-5:** Phase 4 Branch Tracking
- GitHub API client
- Circuit breaker + retry logic
- Branch grouping optimization
- Desktop notifications
- ETag conditional requests

**Week 6:** Phase 5 Polish
- Parallel startup recovery
- SSE reconnection
- Web UI dashboard
- Documentation
- Integration tests

**Total: 6 weeks, ~3500 LOC**

---

#### Minimal MVP Timeline (3-5 days)

**Day 1:**
- [ ] Create `internal/collab/monitor.go` (~150 lines)
- [ ] Implement git diff polling loop
- [ ] File conflict detection logic
- [ ] Mailbox integration

**Day 2:**
- [ ] CLI commands: `warden collab conflicts`, `warden collab who-is-editing` (~50 lines)
- [ ] HTTP route: `GET /collab/conflicts` (~30 lines)
- [ ] Hook into daemon startup/shutdown

**Day 3:**
- [ ] Unit tests (~100 lines)
- [ ] Integration test (spawn 2 agents, verify conflict detected)
- [ ] Basic documentation

**Day 4 (optional):**
- [ ] MCP tools: `who_is_editing_file`, `get_collaboration_status`
- [ ] Web UI basic table (if time permits)

**Day 5 (buffer):**
- [ ] Bug fixes
- [ ] Manual testing
- [ ] Polish

**Total: 3-5 days, ~200 LOC**

---

### Validation Questions

Before implementing the full design, answer these questions **with data**:

#### FSNotify
1. **How often do two agents edit the same file within 10 seconds?**
   - If answer is "rarely", 10s polling is fine
   - If answer is "often", FSNotify is justified

2. **How many concurrent agents do users actually run?**
   - If < 10: inotify limits not a problem
   - If > 50: need to validate FSNotify even works at scale

3. **What's the 95th percentile repo size (file count)?**
   - If < 5000 files: inotify limits not hit
   - If > 10000 files: FSNotify won't work anyway

#### Overlap Detection
1. **Have users reported duplicate work problems?**
   - If no: don't build it
   - If yes: what was the actual use case?

2. **Do agents write plan files in `docs/superpowers/specs/`?**
   - If no: plan file parsing is useless
   - If yes: what percentage of agents?

3. **What's an acceptable false positive rate?**
   - Token similarity gives ~50-70% confidence
   - Would users trust that?

#### Branch Tracking
1. **What percentage of users use GitHub Actions?**
   - If < 50%: not worth building
   - If > 80%: maybe worth it

2. **Do users want daemon to monitor CI?**
   - Survey users before building
   - Maybe they prefer `gh` CLI or IDE plugins

3. **How many branches per user?**
   - If 1-2: tracking overhead not justified
   - If 10+: maybe valuable

---

### Migration Path: MVP → Full Design

If MVP proves valuable, here's how to incrementally add features:

**After MVP ships and users validate core value:**

**Phase 2A: Real-time Detection (if users request faster detection)**
- Add FSNotify (without budget enforcement initially)
- Measure actual inotify usage
- Add budget enforcement only if limits are hit

**Phase 2B: Work Deduplication (if users report duplicate work)**
- Start with subject-only comparison (simplest)
- Skip file overlap and plan parsing initially
- Add complexity only if simple version has too many false negatives

**Phase 2C: CI Monitoring (if users request it)**
- Start with simple GitHub API polling (no circuit breaker, no caching)
- Measure actual API usage
- Add optimizations only if approaching rate limits

**Principle:** Add complexity incrementally, driven by real user needs, not speculation.

---

### Recommendation Summary

**For MVP (Week 1):**
- ✅ SHIP: File conflict detection with git diff polling
- ❌ CUT: FSNotify, OverlapDetector, BranchTracker
- ❌ CUT: All caching, all deduplication, all optimization
- ❌ CUT: Collaboration groups, parallel startup, SSE reconnection

**Rationale:**
- Ship in 3-5 days instead of 4-6 weeks
- Validate core value proposition first
- Gather real usage data before optimizing
- 95% less code = 95% fewer bugs
- Single goroutine = no concurrency bugs
- No dependencies = easier deployment

**After MVP:**
- Measure: How often are conflicts detected?
- Survey: Do users want faster detection?
- Collect: Real usage patterns (repo sizes, agent counts, file churn)
- Prioritize: Features based on actual user requests, not speculation

**Decision required:** Should we build the full design (4-6 weeks) or start with MVP (3-5 days)?

---

### Appendix: Minimal MVP Implementation

Complete implementation reference for the minimal viable product.

#### File Structure
```
internal/collab/
  monitor.go      (~150 lines - main logic)
  http.go         (~30 lines - HTTP handlers)
  
cmd/warden/
  collab.go       (~50 lines - CLI commands)
```

#### monitor.go
```go
package collab

import (
    "context"
    "fmt"
    "os/exec"
    "strings"
    "time"
    
    "warden/internal/mailbox"
    "warden/internal/store"
)

type Monitor struct {
    store   store.Store
    mailbox mailbox.Store
}

func NewMonitor(store store.Store, mailbox mailbox.Store) *Monitor {
    return &Monitor{
        store:   store,
        mailbox: mailbox,
    }
}

func (m *Monitor) Run(ctx context.Context) {
    ticker := time.NewTicker(10 * time.Second)
    defer ticker.Stop()
    
    for {
        select {
        case <-ticker.C:
            m.checkConflicts(ctx)
        case <-ctx.Done():
            return
        }
    }
}

func (m *Monitor) checkConflicts(ctx context.Context) {
    sessions, err := m.store.List(ctx)
    if err != nil {
        return // silent failure, logged elsewhere
    }
    
    // Build map: file -> agents editing it
    fileAgents := make(map[string][]AgentInfo)
    
    for _, sess := range sessions {
        if sess.Worktree == "" || sess.Status != "working" {
            continue
        }
        
        files := gitDiffFiles(sess.Worktree)
        for _, file := range files {
            fileAgents[file] = append(fileAgents[file], AgentInfo{
                ID:       sess.ID,
                Name:     sess.Name,
                Worktree: sess.Worktree,
            })
        }
    }
    
    // Detect and notify conflicts
    for file, agents := range fileAgents {
        if len(agents) > 1 {
            m.notifyConflict(ctx, file, agents)
        }
    }
}

func gitDiffFiles(worktree string) []string {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    
    cmd := exec.CommandContext(ctx, "git", "-C", worktree, 
        "diff", "--name-only", "HEAD")
    out, err := cmd.Output()
    if err != nil {
        return nil
    }
    
    lines := strings.Split(strings.TrimSpace(string(out)), "\n")
    var files []string
    for _, line := range lines {
        if line != "" {
            files = append(files, line)
        }
    }
    return files
}

func (m *Monitor) notifyConflict(ctx context.Context, file string, agents []AgentInfo) {
    for i, agent := range agents {
        others := make([]string, 0, len(agents)-1)
        for j, other := range agents {
            if i != j {
                others = append(others, fmt.Sprintf("%s (%s)", 
                    other.ID, other.Name))
            }
        }
        
        msg := fmt.Sprintf(
            "⚠️  Collaboration Warning: File Conflict\n"+
            "File: %s\n"+
            "Also being edited by: %s\n"+
            "Consider coordinating to avoid merge conflicts.",
            file, strings.Join(others, ", "))
        
        // Fire and forget - don't block on mailbox
        go m.mailbox.Send(ctx, agent.ID, msg)
    }
}

// GetConflicts returns current file conflicts (for CLI/HTTP)
func (m *Monitor) GetConflicts(ctx context.Context) ([]Conflict, error) {
    sessions, err := m.store.List(ctx)
    if err != nil {
        return nil, err
    }
    
    fileAgents := make(map[string][]AgentInfo)
    for _, sess := range sessions {
        if sess.Worktree == "" || sess.Status != "working" {
            continue
        }
        
        files := gitDiffFiles(sess.Worktree)
        for _, file := range files {
            fileAgents[file] = append(fileAgents[file], AgentInfo{
                ID:       sess.ID,
                Name:     sess.Name,
                Worktree: sess.Worktree,
            })
        }
    }
    
    var conflicts []Conflict
    for file, agents := range fileAgents {
        if len(agents) > 1 {
            conflicts = append(conflicts, Conflict{
                File:   file,
                Agents: agents,
            })
        }
    }
    
    return conflicts, nil
}

type AgentInfo struct {
    ID       string
    Name     string
    Worktree string
}

type Conflict struct {
    File   string
    Agents []AgentInfo
}
```

#### http.go
```go
package collab

import (
    "encoding/json"
    "net/http"
)

func (m *Monitor) HandleConflicts(w http.ResponseWriter, r *http.Request) {
    conflicts, err := m.GetConflicts(r.Context())
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(conflicts)
}
```

#### collab.go (CLI)
```go
package main

import (
    "fmt"
    "github.com/spf13/cobra"
)

var collabCmd = &cobra.Command{
    Use:   "collab",
    Short: "Collaboration and conflict detection",
}

var conflictsCmd = &cobra.Command{
    Use:   "conflicts",
    Short: "List current file conflicts between agents",
    RunE: func(cmd *cobra.Command, args []string) error {
        conflicts, err := client.GetConflicts(cmd.Context())
        if err != nil {
            return err
        }
        
        if len(conflicts) == 0 {
            fmt.Println("No conflicts detected")
            return nil
        }
        
        fmt.Printf("File Conflicts (%d):\n\n", len(conflicts))
        for _, c := range conflicts {
            fmt.Printf("%s\n", c.File)
            for _, agent := range c.Agents {
                fmt.Printf("  - %s (%s)\n", agent.ID, agent.Name)
            }
            fmt.Println()
        }
        
        return nil
    },
}

var whoIsEditingCmd = &cobra.Command{
    Use:   "who-is-editing <file>",
    Short: "Check which agents are editing a specific file",
    Args:  cobra.ExactArgs(1),
    RunE: func(cmd *cobra.Command, args []string) error {
        file := args[0]
        
        conflicts, err := client.GetConflicts(cmd.Context())
        if err != nil {
            return err
        }
        
        for _, c := range conflicts {
            if c.File == file {
                fmt.Printf("Agents editing %s:\n", file)
                for _, agent := range c.Agents {
                    fmt.Printf("  - %s (%s)\n", agent.ID, agent.Name)
                }
                return nil
            }
        }
        
        fmt.Printf("No agents currently editing %s\n", file)
        return nil
    },
}

func init() {
    collabCmd.AddCommand(conflictsCmd)
    collabCmd.AddCommand(whoIsEditingCmd)
    rootCmd.AddCommand(collabCmd)
}
```

#### Total: ~200 lines, 3-5 days to ship

---

## Changelog

**2026-06-14:** Initial design complete  
**2026-06-14 (revision 2):** Fixed all critical concurrency issues, added thread-safety design principles, updated implementation phases with race detector requirements  
**2026-06-14 (revision 3):** Added comprehensive error handling (GitHub API retry/circuit breaker), daemon startup recovery, FSNotify edge cases (symlinks/renames/debouncing), notification deduplication, SSE reconnection support, git validation (detached HEAD/bare repo/shallow clone), division by zero fixes, case-insensitive tokenization  
**2026-06-14 (revision 4):** Performance optimization overhaul based on scalability analysis:
- **FSNotify watch budget enforcement** (prevents silent failures at inotify limits)
- **GitHub API branch grouping** (80% reduction in API calls: agents → branches)
- **Conditional HTTP requests with ETag** (304 Not Modified doesn't count against rate limit)
- **Parallel overlap detection** (10-worker goroutine pool for O(N²) comparisons)
- **Plan file cache with FSNotify invalidation** (eliminates repeated disk I/O)
- **Parallel startup recovery** (10-concurrent git diffs, progress logging)
- **Mailbox deduper cleanup goroutine** (prevents memory leak)
- **Multiple cache cleanup goroutines** (git diff, plan files, similarity scores)
- Updated scalability: **100+ concurrent agents** (up from ~20 in revision 3)
- Updated GitHub API headroom: **1.2% of rate limit** (down from 30% in revision 3)  
**2026-06-14 (revision 5):** **CRITICAL DESIGN REVIEW** - Added comprehensive simplification analysis:
- Identified 95% complexity reduction opportunity (3500 LOC → 200 LOC)
- Proposed minimal MVP: file conflict detection only (3-5 days vs 4-6 weeks)
- Cut 8 major features from MVP (FSNotify, OverlapDetector, BranchTracker, all caching/optimization)
- Documented validation questions to answer before building full design
- Added complete minimal implementation reference (~200 lines)
- Preserved all original design details for human review and decision
- **Status: PENDING HUMAN DECISION** on MVP vs full implementation path  
**2026-06-21 (revision 6):** Added **Foundational Layer Hardening** section reviewing the already-shipped primitives this system builds on (`mailbox`, `ctxstore`, hub long-poll). Six findings with concrete fixes:
- H1 (medium): unauthenticated `from`/`updated_by` provenance — document trust model, reserve `daemon`/`system` senders at the write gate
- H2 (medium): no atomic read-modify-write on context store — add `CompareAndSet` + `Append` primitives (CLI/HTTP/MCP), retry on 409
- H3 (low–med): unbounded inbox growth with full-file rewrite per op — read-message compaction, size cap, high-water-mark ids (also fixes the existing `len+1` id-collision bug)
- H4 (low): no long-poll wait for MCP agents — add `wait_for_message` tool proxying the existing wait route
- H5 (low): wake-on-send status TOCTOU — re-`Get` before tmux `Input`, document wake as best-effort
- H6 (low): `mailbox.All()` aborts on one corrupt inbox — skip-and-log in the aggregate view only
- Recommend landing H1/H2 before the collaboration layer routes automated warnings; H3–H6 incremental
