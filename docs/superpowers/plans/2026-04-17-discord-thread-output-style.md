# Discord Thread Output Style Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a new Discord `progress_style = "thread"` that creates one Discord thread per session in an operator-configured parent channel, with an editable summary embed at the top and an append-only stream of structured tool-use events below. User's question and AI's final answer stay in the original channel.

**Architecture:** Two small core additions (generic `PlatformState` persistence keyed by stable `Session.ID`, and a `ProgressAppender` capability interface that delivers live structured events to platforms before summary truncation) plus a Discord-side implementation. Platform-agnostic core, Discord-specific thread lifecycle and rendering. Lazy thread creation on first tool event; persisted session-id-to-thread-id mapping for restart survival; bounded batching (1 summary edit + 1 append write per 2s tick) to respect Discord rate limits. Fallback to `card` style in DMs / on missing perms / on invalid parent channel.

**Tech Stack:** Go, `github.com/bwmarrin/discordgo`, standard library only for core. Existing patterns: plugin-registry factories, `sync.Map`, `slog` structured logging, JSON session persistence in `~/.cc-connect/sessions/`, table-driven unit tests.

**Reference:** `docs/superpowers/specs/2026-04-17-discord-thread-output-style-design.md`

---

## File Structure

**New files:**
- `platform/discord/thread_log.go` — thread lifecycle (lazy create, reuse, un-archive, fallback), session-id-keyed state helpers, ProgressAppender impl
- `platform/discord/thread_log_render.go` — per-entry embed builders and append-batch coalescing
- `platform/discord/thread_log_test.go` — thread lifecycle tests
- `platform/discord/thread_log_render_test.go` — append embed + batching tests

**Modified files:**
- `core/session.go` — add `PlatformState` field to `sessionSnapshot` + `SessionManager` Get/Set/Delete methods
- `core/session_test.go` — tests for `PlatformState` round-trip
- `core/streaming.go` — add `ProgressMetadata`, `ProgressSessionAware`, `ProgressAppender` interfaces
- `core/progress_compact.go` — accept `"thread"` style; thread ProgressMetadata through `newCompactProgressWriter`; invoke appender on new entries before truncation; use `ProgressSessionAware.SendPreviewStartWithMetadata` when supported
- `core/progress_compact_test.go` — tests for metadata plumbing + pre-truncation append
- `core/engine.go` — pass `session.ID` and first user prompt into `newCompactProgressWriter`
- `platform/discord/discord.go` — parse `log_thread_channel` / `log_thread_archive`; accept `"thread"` as progress style; extend `threadOps` with message send/edit + unarchive; register `progressPlatform` capabilities for thread mode
- `platform/discord/discord_test.go` — config validation tests
- `config.example.toml` — document new options

---

## Task 1: PlatformState persistence in core/session.go

**Files:**
- Modify: `/home/meteorsky/code/cc-connect/core/session.go`
- Modify: `/home/meteorsky/code/cc-connect/core/session_test.go`

Adds a generic per-platform k/v map persisted inside the existing session snapshot. Keyed by (platform, key), values are strings. Atomic writes through the existing `AtomicWriteFile` path.

- [ ] **Step 1: Read existing `sessionSnapshot` and `SessionManager` persistence**

Run: `grep -n "sessionSnapshot\|AtomicWriteFile\|sessionNames\|userMeta" /home/meteorsky/code/cc-connect/core/session.go`

Note: `sessionSnapshot` lives around line 164; `persist()` method writes atomically. Confirm exact field layout before adding the new field.

- [ ] **Step 2: Write failing test for PlatformState round-trip**

Append to `core/session_test.go`:

```go
func TestPlatformState_RoundTrip(t *testing.T) {
    dir := t.TempDir()
    mgr, err := NewSessionManager(dir, "testproj", "/tmp/wd")
    if err != nil {
        t.Fatalf("NewSessionManager: %v", err)
    }

    mgr.SetPlatformState("discord", "s1:thread", "tid-123")
    mgr.SetPlatformState("discord", "s1:first_prompt", "Fix the login bug")
    mgr.SetPlatformState("telegram", "s2:foo", "bar")

    got, ok := mgr.GetPlatformState("discord", "s1:thread")
    if !ok || got != "tid-123" {
        t.Fatalf("GetPlatformState(discord,s1:thread) = %q,%v; want tid-123,true", got, ok)
    }

    // Reload from disk and confirm state survived.
    mgr2, err := NewSessionManager(dir, "testproj", "/tmp/wd")
    if err != nil {
        t.Fatalf("reload: %v", err)
    }
    got, ok = mgr2.GetPlatformState("discord", "s1:first_prompt")
    if !ok || got != "Fix the login bug" {
        t.Fatalf("after reload: %q,%v; want 'Fix the login bug',true", got, ok)
    }
    got, ok = mgr2.GetPlatformState("telegram", "s2:foo")
    if !ok || got != "bar" {
        t.Fatalf("after reload telegram: %q,%v", got, ok)
    }

    mgr2.DeletePlatformState("discord", "s1:thread")
    if _, ok := mgr2.GetPlatformState("discord", "s1:thread"); ok {
        t.Fatalf("DeletePlatformState did not remove entry")
    }
}

func TestPlatformState_EmptyOnMissing(t *testing.T) {
    dir := t.TempDir()
    mgr, err := NewSessionManager(dir, "proj", "/tmp/wd")
    if err != nil {
        t.Fatalf("NewSessionManager: %v", err)
    }
    if got, ok := mgr.GetPlatformState("discord", "missing"); ok || got != "" {
        t.Fatalf("unexpected value on missing key: %q,%v", got, ok)
    }
    // Delete on empty must be a no-op, not a panic.
    mgr.DeletePlatformState("discord", "missing")
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `cd /home/meteorsky/code/cc-connect && go test ./core/ -run TestPlatformState -v`
Expected: compile error — `SetPlatformState`/`GetPlatformState`/`DeletePlatformState` undefined.

- [ ] **Step 4: Add field to sessionSnapshot**

In `core/session.go`, add a new field to `sessionSnapshot`:

```go
type sessionSnapshot struct {
    // ... existing fields ...
    PlatformState map[string]map[string]string `json:"platform_state,omitempty"`
}
```

- [ ] **Step 5: Add field to SessionManager**

Add to `SessionManager` struct (same file):

```go
type SessionManager struct {
    // ... existing fields ...
    platformState map[string]map[string]string
}
```

Initialize `platformState: make(map[string]map[string]string)` in `NewSessionManager`.

Load from snapshot on startup (inside the existing loadFromDisk / restore path):

```go
if snap.PlatformState != nil {
    for plat, kv := range snap.PlatformState {
        copy := make(map[string]string, len(kv))
        for k, v := range kv {
            copy[k] = v
        }
        m.platformState[plat] = copy
    }
}
```

And include it in the snapshot when persisting:

```go
snap.PlatformState = m.platformState
```

- [ ] **Step 6: Add Get/Set/Delete methods**

In `core/session.go`:

```go
// GetPlatformState returns the value stored under (platform, key), or ("", false) if not set.
func (m *SessionManager) GetPlatformState(platform, key string) (string, bool) {
    m.mu.Lock()
    defer m.mu.Unlock()
    if m.platformState == nil {
        return "", false
    }
    kv, ok := m.platformState[platform]
    if !ok {
        return "", false
    }
    v, ok := kv[key]
    return v, ok
}

// SetPlatformState stores value under (platform, key) and persists to disk.
func (m *SessionManager) SetPlatformState(platform, key, value string) {
    m.mu.Lock()
    if m.platformState == nil {
        m.platformState = make(map[string]map[string]string)
    }
    kv, ok := m.platformState[platform]
    if !ok {
        kv = make(map[string]string)
        m.platformState[platform] = kv
    }
    kv[key] = value
    m.mu.Unlock()
    m.persist()
}

// DeletePlatformState removes (platform, key). No-op if absent.
func (m *SessionManager) DeletePlatformState(platform, key string) {
    m.mu.Lock()
    if kv, ok := m.platformState[platform]; ok {
        delete(kv, key)
        if len(kv) == 0 {
            delete(m.platformState, platform)
        }
    }
    m.mu.Unlock()
    m.persist()
}
```

Note: match the exact mutex name and `persist()` method name used by `SessionManager` — verify via `grep "func (m \*SessionManager)" core/session.go`. If `persist` is private and blocks on disk I/O, the existing pattern (see `SetUserMeta`) is the pattern to match.

- [ ] **Step 7: Run tests**

Run: `cd /home/meteorsky/code/cc-connect && go test ./core/ -run TestPlatformState -v`
Expected: PASS.

- [ ] **Step 8: Run full core test suite to confirm no regressions**

Run: `cd /home/meteorsky/code/cc-connect && go test ./core/`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
cd /home/meteorsky/code/cc-connect && git add core/session.go core/session_test.go && \
git commit -m "feat(core): add generic PlatformState persistence

Adds a per-platform key/value map on SessionManager persisted alongside
the session snapshot, keyed by (platform, key) and accessed via
Get/Set/DeletePlatformState. Used by the upcoming Discord thread output
style to survive daemon restarts without hardcoding platform names into
core."
```

---

## Task 2: ProgressMetadata + capability interfaces

**Files:**
- Modify: `/home/meteorsky/code/cc-connect/core/streaming.go`
- Modify: `/home/meteorsky/code/cc-connect/core/interfaces.go` (if progress capability interfaces live there)

Introduces two new optional capability interfaces that platforms may implement to receive session metadata and live-append hooks.

- [ ] **Step 1: Locate where existing progress capability interfaces live**

Run: `grep -n "ProgressStyleProvider\|PreviewStarter\|MessageUpdater" /home/meteorsky/code/cc-connect/core/*.go`

Note the file (likely `core/interfaces.go` for interfaces, `core/streaming.go` for `PreviewStarter`/`PreviewCleaner`).

- [ ] **Step 2: Add ProgressMetadata and new interfaces**

In `core/streaming.go` (place near `PreviewStarter`):

```go
// ProgressMetadata carries stable per-session identity and content hints from
// the engine to progress-aware platforms. Used when a platform wants to key
// external resources (e.g. Discord threads) off Session.ID rather than the
// routing sessionKey.
type ProgressMetadata struct {
    SessionID   string // stable Session.ID (e.g. "s1", "s2")
    FirstPrompt string // first user message of this session, as captured by the engine
}

// ProgressSessionAware is an optional capability interface. When a platform
// implements it, the progress writer calls SendPreviewStartWithMetadata
// instead of SendPreviewStart so the platform can bind the preview to stable
// session identity. Platforms that do not implement this receive the plain
// SendPreviewStart call and lose session metadata.
type ProgressSessionAware interface {
    SendPreviewStartWithMetadata(ctx context.Context, replyCtx any, content string, meta ProgressMetadata) (any, error)
}

// ProgressAppender is an optional capability interface. When a platform
// implements it, the progress writer delivers each newly-seen structured
// entry to the platform before summary truncation is applied, allowing the
// platform to maintain a separate append-only log (e.g. messages in a Discord
// thread) alongside the bounded summary.
//
// entries is the slice of new entries since the last AppendProgressEntries
// call (may be empty). The platform is responsible for its own batching /
// rate-limit coalescing of these entries against its write budget.
type ProgressAppender interface {
    AppendProgressEntries(ctx context.Context, previewHandle any, entries []ProgressCardEntry) error
}
```

- [ ] **Step 3: Build to confirm compile**

Run: `cd /home/meteorsky/code/cc-connect && go build ./core/...`
Expected: PASS (no implementations yet, just interfaces).

- [ ] **Step 4: Commit**

```bash
cd /home/meteorsky/code/cc-connect && git add core/streaming.go && \
git commit -m "feat(core): add ProgressMetadata and ProgressAppender interfaces

Two optional capability interfaces for progress-aware platforms:
ProgressSessionAware lets a platform receive stable Session.ID and the
first user prompt when a preview starts; ProgressAppender delivers new
structured entries before the bounded summary truncation is applied,
for platforms that want a separate append-only log alongside the
bounded summary embed."
```

---

## Task 3: Accept `"thread"` as a valid progress style

**Files:**
- Modify: `/home/meteorsky/code/cc-connect/core/progress_compact.go`
- Modify: `/home/meteorsky/code/cc-connect/core/progress_compact_test.go`

- [ ] **Step 1: Write failing test**

In `core/progress_compact_test.go`:

```go
func TestNormalizeProgressStyle_Thread(t *testing.T) {
    if got := normalizeProgressStyle("thread"); got != "thread" {
        t.Fatalf("normalizeProgressStyle(thread) = %q; want thread", got)
    }
    if got := normalizeProgressStyle("Thread"); got != "thread" {
        t.Fatalf("case-insensitive: got %q; want thread", got)
    }
    if got := normalizeProgressStyle("bogus"); got != progressStyleLegacy {
        t.Fatalf("unknown falls back to legacy: got %q", got)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/meteorsky/code/cc-connect && go test ./core/ -run TestNormalizeProgressStyle_Thread -v`
Expected: FAIL — `normalizeProgressStyle("thread")` returns `"legacy"`.

- [ ] **Step 3: Implement**

In `core/progress_compact.go`, add the constant and case:

```go
const progressStyleThread = "thread"

func normalizeProgressStyle(style string) string {
    switch strings.ToLower(strings.TrimSpace(style)) {
    case "", progressStyleLegacy:
        return progressStyleLegacy
    case progressStyleCompact:
        return progressStyleCompact
    case progressStyleCard:
        return progressStyleCard
    case progressStyleThread:
        return progressStyleThread
    default:
        return progressStyleLegacy
    }
}
```

Also update the `newCompactProgressWriter` enable check:

```go
// OLD: if w.style != progressStyleCompact && w.style != progressStyleCard {
if w.style != progressStyleCompact && w.style != progressStyleCard && w.style != progressStyleThread {
    slog.Debug("progress writer disabled: unsupported style", "platform", p.Name(), "style", w.style)
    return w
}
```

Thread style behaves like `card` for payload purposes, so add at the end of `newCompactProgressWriter`:

```go
if w.style == progressStyleCard || w.style == progressStyleThread {
    if progressCardPayloadForTarget(p, replyCtx) {
        w.usePayload = true
    }
}
```

And in `AppendStructured`, extend the style switch:

```go
case progressStyleCard, progressStyleThread:
    // ... existing card-style logic, unchanged ...
```

- [ ] **Step 4: Run tests**

Run: `cd /home/meteorsky/code/cc-connect && go test ./core/ -run TestNormalizeProgressStyle -v`
Expected: PASS.

Run: `cd /home/meteorsky/code/cc-connect && go test ./core/`
Expected: all tests PASS (card-style tests must still pass since thread reuses the same buffer logic).

- [ ] **Step 5: Commit**

```bash
cd /home/meteorsky/code/cc-connect && git add core/progress_compact.go core/progress_compact_test.go && \
git commit -m "feat(core): accept 'thread' progress style

Adds 'thread' as a new valid progress_style value. Behaves like 'card'
for payload building and summary truncation — Discord will layer append
logic on top via the ProgressAppender interface."
```

---

## Task 4: Plumb ProgressMetadata into compactProgressWriter

**Files:**
- Modify: `/home/meteorsky/code/cc-connect/core/progress_compact.go`
- Modify: `/home/meteorsky/code/cc-connect/core/progress_compact_test.go`
- Modify: `/home/meteorsky/code/cc-connect/core/engine.go`

- [ ] **Step 1: Write failing test**

In `core/progress_compact_test.go`:

```go
// metadataCapturingPlatform records which preview-start method was called
// and what metadata it received.
type metadataCapturingPlatform struct {
    stubPlatformNoProgress
    gotMeta    ProgressMetadata
    metaCalled bool
    plainCalled bool
}

func (p *metadataCapturingPlatform) SendPreviewStart(ctx context.Context, replyCtx any, content string) (any, error) {
    p.plainCalled = true
    return "plain-handle", nil
}
func (p *metadataCapturingPlatform) SendPreviewStartWithMetadata(ctx context.Context, replyCtx any, content string, meta ProgressMetadata) (any, error) {
    p.metaCalled = true
    p.gotMeta = meta
    return "meta-handle", nil
}
func (p *metadataCapturingPlatform) UpdateMessage(ctx context.Context, handle any, content string) error {
    return nil
}
func (p *metadataCapturingPlatform) ProgressStyle() string { return "card" }

func TestProgressWriter_RoutesSessionAwarePlatforms(t *testing.T) {
    p := &metadataCapturingPlatform{}
    meta := ProgressMetadata{SessionID: "s42", FirstPrompt: "Fix the login bug"}
    w := newCompactProgressWriter(context.Background(), p, nil, "claudecode", LangEN, nil, meta)
    if !w.enabled {
        t.Fatalf("writer not enabled")
    }
    if ok := w.AppendStructured(ProgressCardEntry{Kind: ProgressEntryToolUse, Text: "ls", Tool: "Bash"}, "ls"); !ok {
        t.Fatalf("AppendStructured failed")
    }
    if !p.metaCalled {
        t.Fatalf("expected SendPreviewStartWithMetadata to be called")
    }
    if p.plainCalled {
        t.Fatalf("plain SendPreviewStart should not be called when metadata variant exists")
    }
    if p.gotMeta.SessionID != "s42" || p.gotMeta.FirstPrompt != "Fix the login bug" {
        t.Fatalf("metadata mismatch: %+v", p.gotMeta)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/meteorsky/code/cc-connect && go test ./core/ -run TestProgressWriter_RoutesSessionAwarePlatforms -v`
Expected: compile error — `newCompactProgressWriter` has wrong arity.

- [ ] **Step 3: Add metadata field and update constructor signature**

In `core/progress_compact.go`, add to the struct:

```go
type compactProgressWriter struct {
    // ... existing fields ...
    meta          ProgressMetadata
    sessionAware  ProgressSessionAware // nil if platform does not implement it
}
```

Update the constructor:

```go
func newCompactProgressWriter(ctx context.Context, p Platform, replyCtx any, agentName string, lang Language, transform func(string) string, meta ProgressMetadata) *compactProgressWriter {
    w := &compactProgressWriter{
        ctx:        ctx,
        platform:   p,
        replyCtx:   replyCtx,
        transform:  transform,
        style:      progressStyleForTarget(p, replyCtx),
        state:      ProgressCardStateRunning,
        agentName:  normalizeProgressAgentLabel(agentName),
        lang:       lang,
        maxEntries: 10,
        meta:       meta,
    }
    // ... existing throttler + enable checks unchanged ...
    if w.enabled {
        if sa, ok := p.(ProgressSessionAware); ok {
            w.sessionAware = sa
        }
    }
    return w
}
```

- [ ] **Step 4: Route SendPreviewStart through the session-aware path when available**

In `AppendStructured`, replace the existing:

```go
handle, err := w.starter.SendPreviewStart(callCtx, w.replyCtx, w.content)
```

with:

```go
var handle any
var err error
if w.sessionAware != nil {
    handle, err = w.sessionAware.SendPreviewStartWithMetadata(callCtx, w.replyCtx, w.content, w.meta)
} else {
    handle, err = w.starter.SendPreviewStart(callCtx, w.replyCtx, w.content)
}
```

- [ ] **Step 5: Fix every call site of newCompactProgressWriter**

Run: `grep -rn "newCompactProgressWriter" /home/meteorsky/code/cc-connect/`

Update each call site by passing `ProgressMetadata{}` (empty) for now; Task 5 will fill the values from the engine. Expected sites:
- `core/engine.go` — pass `ProgressMetadata{}` temporarily.
- `core/progress_compact_test.go` — pass `ProgressMetadata{}` temporarily in existing tests.

- [ ] **Step 6: Run targeted test**

Run: `cd /home/meteorsky/code/cc-connect && go test ./core/ -run TestProgressWriter_RoutesSessionAwarePlatforms -v`
Expected: PASS.

- [ ] **Step 7: Run full core suite**

Run: `cd /home/meteorsky/code/cc-connect && go test ./core/`
Expected: PASS (existing tests kept working via empty `ProgressMetadata{}`).

- [ ] **Step 8: Commit**

```bash
cd /home/meteorsky/code/cc-connect && git add core/progress_compact.go core/progress_compact_test.go core/engine.go && \
git commit -m "feat(core): plumb ProgressMetadata through compactProgressWriter

Extends newCompactProgressWriter to accept ProgressMetadata and to
prefer ProgressSessionAware.SendPreviewStartWithMetadata over
SendPreviewStart when the platform implements it. Existing call sites
pass an empty ProgressMetadata; the engine will populate it in the
next commit."
```

---

## Task 5: Engine passes session metadata into progress writer

**Files:**
- Modify: `/home/meteorsky/code/cc-connect/core/engine.go`

Engine has access to `*Session` during `processInteractiveEvents` (see engine.go:~2425 per spec). Populate `ProgressMetadata` from it.

- [ ] **Step 1: Locate the `newCompactProgressWriter` call site in engine.go**

Run: `grep -n "newCompactProgressWriter" /home/meteorsky/code/cc-connect/core/engine.go`

Expected: a single call inside `processInteractiveEvents`, around line 2455.

- [ ] **Step 2: Identify how to get first user prompt**

Run: `grep -n "type Session struct\|History\|Messages\|UserPrompt\|SessionHistory" /home/meteorsky/code/cc-connect/core/session.go | head -20`

Read the `Session.History` slice: entries contain role+content. "first user prompt" is the `content` of the first entry where the role is the user.

- [ ] **Step 3: Add a small helper in engine.go**

In `core/engine.go` (near other session helpers, keep it un-exported):

```go
// firstUserPrompt returns the first user message recorded in the session
// history. Empty if no user message has been recorded yet.
func firstUserPrompt(s *Session) string {
    if s == nil {
        return ""
    }
    for _, h := range s.History {
        if strings.EqualFold(h.Role, "user") {
            return strings.TrimSpace(h.Content)
        }
    }
    return ""
}
```

Verify `Session.History` field name and item shape first — replace `h.Role`/`h.Content` with whatever the actual fields are. Run: `grep -n "History\s*\[\]" /home/meteorsky/code/cc-connect/core/session.go` to confirm the struct.

- [ ] **Step 4: Populate ProgressMetadata at call site**

Replace:

```go
cp := newCompactProgressWriter(e.ctx, state.platform, state.replyCtx, e.agent.Name(), e.i18n.CurrentLang(), workspaceRenderer, ProgressMetadata{})
```

with:

```go
meta := ProgressMetadata{
    SessionID:   session.ID,
    FirstPrompt: firstUserPrompt(session),
}
cp := newCompactProgressWriter(e.ctx, state.platform, state.replyCtx, e.agent.Name(), e.i18n.CurrentLang(), workspaceRenderer, meta)
```

Verify the local variable name holding `*Session` at that site; it may be `session`, `sess`, or reached via `state.session`.

- [ ] **Step 5: Build and run core tests**

Run: `cd /home/meteorsky/code/cc-connect && go build ./... && go test ./core/`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
cd /home/meteorsky/code/cc-connect && git add core/engine.go && \
git commit -m "feat(core): engine populates ProgressMetadata from Session

Engine now passes Session.ID and the first recorded user prompt into
newCompactProgressWriter. Platforms that implement ProgressSessionAware
(Discord in thread mode) will use these for stable per-session keying
and thread naming."
```

---

## Task 6: Call ProgressAppender before truncation

**Files:**
- Modify: `/home/meteorsky/code/cc-connect/core/progress_compact.go`
- Modify: `/home/meteorsky/code/cc-connect/core/progress_compact_test.go`

The current `AppendStructured` pushes to `w.items`, then truncates to `maxEntries` (latest 10). The append hook must run *before* that truncation so the platform sees every entry exactly once — even the ones about to be dropped from the summary.

- [ ] **Step 1: Write failing test**

In `core/progress_compact_test.go`:

```go
type appenderCapturingPlatform struct {
    metadataCapturingPlatform
    appendCalls [][]ProgressCardEntry
    handle      any
}

func (p *appenderCapturingPlatform) AppendProgressEntries(ctx context.Context, previewHandle any, entries []ProgressCardEntry) error {
    cp := make([]ProgressCardEntry, len(entries))
    copy(cp, entries)
    p.appendCalls = append(p.appendCalls, cp)
    p.handle = previewHandle
    return nil
}

func TestProgressWriter_AppenderReceivesEveryEntry(t *testing.T) {
    p := &appenderCapturingPlatform{}
    p.metadataCapturingPlatform.stubPlatformNoProgress = stubPlatformNoProgress{name: "discord"}
    meta := ProgressMetadata{SessionID: "s1", FirstPrompt: "hi"}
    w := newCompactProgressWriter(context.Background(), p, nil, "claudecode", LangEN, nil, meta)
    if !w.enabled {
        t.Fatalf("writer not enabled")
    }

    // Push 15 entries — exceeds maxEntries (10).
    for i := 0; i < 15; i++ {
        ok := w.AppendStructured(ProgressCardEntry{
            Kind: ProgressEntryToolUse, Text: "cmd", Tool: "Bash",
        }, "cmd")
        if !ok {
            t.Fatalf("AppendStructured(%d) failed", i)
        }
    }

    total := 0
    for _, batch := range p.appendCalls {
        total += len(batch)
    }
    if total != 15 {
        t.Fatalf("appender saw %d entries across %d calls; want 15", total, len(p.appendCalls))
    }

    // Summary must be truncated to 10.
    if len(w.items) > w.maxEntries {
        t.Fatalf("items buffer len=%d exceeds maxEntries=%d", len(w.items), w.maxEntries)
    }
}

// thread-style wrapper platform: returns "thread" progress style
func (p *appenderCapturingPlatform) ProgressStyle() string { return "thread" }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/meteorsky/code/cc-connect && go test ./core/ -run TestProgressWriter_AppenderReceivesEveryEntry -v`
Expected: FAIL — `AppendProgressEntries` is never called.

- [ ] **Step 3: Wire the appender call in AppendStructured**

In `core/progress_compact.go`, add to the struct:

```go
type compactProgressWriter struct {
    // ... existing ...
    appender ProgressAppender // nil if platform does not implement it
}
```

In `newCompactProgressWriter`, capture it:

```go
if w.enabled {
    if sa, ok := p.(ProgressSessionAware); ok { w.sessionAware = sa }
    if ap, ok := p.(ProgressAppender); ok { w.appender = ap }
}
```

In `AppendStructured`, just before the existing truncation block (line ~424, but the block that trims `w.items` to `w.maxEntries`), after the item has been normalized (`item.Kind`/`item.Text` filled in), invoke the appender:

```go
// Deliver the live event to the appender BEFORE summary truncation so
// platforms maintaining a separate append-only log see every entry,
// not just the last w.maxEntries.
if w.appender != nil {
    callCtx, cancel := w.withAPITimeout()
    if err := w.appender.AppendProgressEntries(callCtx, w.handle, []ProgressCardEntry{item}); err != nil {
        slog.Warn("progress writer: AppendProgressEntries failed",
            "platform", w.platform.Name(), "error", err)
        // Non-fatal: summary continues regardless.
    }
    cancel()
}
```

Place this block *immediately after* `item.Kind = kind` / `item.Text = text` normalization and *before* the `switch w.style` block that does summary buffering + truncation.

- [ ] **Step 4: Run targeted test**

Run: `cd /home/meteorsky/code/cc-connect && go test ./core/ -run TestProgressWriter_AppenderReceivesEveryEntry -v`
Expected: PASS.

- [ ] **Step 5: Run full core suite**

Run: `cd /home/meteorsky/code/cc-connect && go test ./core/`
Expected: PASS (non-appender platforms see no behavior change; the appender code path only activates when the interface is implemented).

- [ ] **Step 6: Commit**

```bash
cd /home/meteorsky/code/cc-connect && git add core/progress_compact.go core/progress_compact_test.go && \
git commit -m "feat(core): invoke ProgressAppender before summary truncation

The compact progress writer now delivers each new structured entry to
platforms implementing ProgressAppender immediately after normalization
and before the maxEntries=10 truncation is applied. Platforms like
Discord in thread mode can therefore maintain a complete append-only
log even when the summary is bounded to the latest 10 items. Failures
in AppendProgressEntries are logged but non-fatal."
```

---

## Task 7: Discord config — log_thread_channel, log_thread_archive, accept "thread"

**Files:**
- Modify: `/home/meteorsky/code/cc-connect/platform/discord/discord.go`
- Modify: `/home/meteorsky/code/cc-connect/platform/discord/discord_test.go`

- [ ] **Step 1: Write failing test**

In `platform/discord/discord_test.go`:

```go
func TestNew_ThreadStyle_RequiresLogChannel(t *testing.T) {
    _, err := New(map[string]any{
        "token":          "x",
        "progress_style": "thread",
        // log_thread_channel intentionally absent
    })
    if err == nil {
        t.Fatalf("expected error when progress_style=thread without log_thread_channel")
    }
    if !strings.Contains(err.Error(), "log_thread_channel") {
        t.Fatalf("error should mention log_thread_channel; got: %v", err)
    }
}

func TestNew_ThreadStyle_ClampsArchive(t *testing.T) {
    p, err := New(map[string]any{
        "token":              "x",
        "progress_style":     "thread",
        "log_thread_channel": "12345",
        "log_thread_archive": int64(99), // invalid
    })
    if err != nil {
        t.Fatalf("unexpected: %v", err)
    }
    plat := p.(*progressPlatform).Platform
    if plat.logThreadArchive != 4320 {
        t.Fatalf("invalid log_thread_archive should default to 4320; got %d", plat.logThreadArchive)
    }
}

func TestNew_LogThreadChannel_IgnoredWhenStyleNotThread(t *testing.T) {
    // Should not error; should warn (warning is logged, not testable here).
    p, err := New(map[string]any{
        "token":              "x",
        "progress_style":     "card",
        "log_thread_channel": "12345",
    })
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if p == nil {
        t.Fatalf("nil platform")
    }
}
```

Note: the cast to `*progressPlatform` mirrors the existing wrapper pattern in discord.go lines 118–124. Verify the exact exported/unexported name before committing.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/meteorsky/code/cc-connect && go test ./platform/discord/ -run TestNew_ThreadStyle -v`
Expected: FAIL — accepts "thread" as a bogus value, or panics.

- [ ] **Step 3: Extend progress_style parsing in discord.go**

In `platform/discord/discord.go`, locate the progress_style switch (lines 81–91 per spec) and add the `thread` case:

```go
progressStyle := "legacy"
if v, ok := opts["progress_style"].(string); ok {
    switch strings.ToLower(strings.TrimSpace(v)) {
    case "", "legacy":
        progressStyle = "legacy"
    case "compact", "card", "thread":
        progressStyle = strings.ToLower(strings.TrimSpace(v))
    default:
        return nil, fmt.Errorf("discord: invalid progress_style %q (want legacy, compact, card, or thread)", v)
    }
}
```

- [ ] **Step 4: Parse log_thread_channel and log_thread_archive**

Immediately after the progress_style parsing:

```go
logThreadChannel := ""
if v, ok := opts["log_thread_channel"].(string); ok {
    logThreadChannel = strings.TrimSpace(v)
}

// Default 3 days; Discord only accepts 60, 1440, 4320, 10080.
logThreadArchive := 4320
if v, ok := optInt64(opts, "log_thread_archive"); ok {
    switch v {
    case 60, 1440, 4320, 10080:
        logThreadArchive = int(v)
    default:
        slog.Warn("discord: invalid log_thread_archive; defaulting to 4320",
            "value", v, "valid", []int{60, 1440, 4320, 10080})
    }
}

if progressStyle == "thread" {
    if logThreadChannel == "" {
        return nil, fmt.Errorf("discord: progress_style=thread requires log_thread_channel")
    }
} else if logThreadChannel != "" {
    slog.Warn("discord: log_thread_channel is ignored because progress_style is not 'thread'",
        "progress_style", progressStyle)
}
```

If `optInt64` does not exist as a helper, inline it:

```go
func optInt64(opts map[string]any, key string) (int64, bool) {
    switch v := opts[key].(type) {
    case int:
        return int64(v), true
    case int64:
        return v, true
    case float64:
        return int64(v), true
    default:
        return 0, false
    }
}
```

- [ ] **Step 5: Store on Platform struct**

Add to the `Platform` struct (lines 47–67 per spec):

```go
type Platform struct {
    // ... existing ...
    logThreadChannel string
    logThreadArchive int
    // in-memory cache of sessionID -> threadID for fast lookup without
    // hitting disk on every tool event (SessionManager is the source of truth)
    sessionThreadCache sync.Map
}
```

Wire them in the `New()` return path:

```go
p := &Platform{
    // ... existing fields ...
    logThreadChannel: logThreadChannel,
    logThreadArchive: logThreadArchive,
}
```

- [ ] **Step 6: Run tests**

Run: `cd /home/meteorsky/code/cc-connect && go test ./platform/discord/ -run TestNew_ThreadStyle -v`
Expected: PASS.

Run: `cd /home/meteorsky/code/cc-connect && go test ./platform/discord/`
Expected: PASS (existing tests unchanged).

- [ ] **Step 7: Commit**

```bash
cd /home/meteorsky/code/cc-connect && git add platform/discord/discord.go platform/discord/discord_test.go && \
git commit -m "feat(discord): accept progress_style=thread and log_thread_* options

Adds log_thread_channel (required when style=thread) and
log_thread_archive (one of 60/1440/4320/10080, default 4320) as Discord
platform options. Wired through to the Platform struct; actual thread
lifecycle and rendering land in subsequent commits."
```

---

## Task 8: Extend threadOps with message send/edit/unarchive

**Files:**
- Modify: `/home/meteorsky/code/cc-connect/platform/discord/discord.go`
- Modify: `/home/meteorsky/code/cc-connect/platform/discord/discord_test.go`

- [ ] **Step 1: Locate the existing threadOps interface**

Run: `grep -n "type threadOps\|type sessionThreadOps\|threadOps interface" /home/meteorsky/code/cc-connect/platform/discord/discord.go`

Expected: interface around line 199, real impl `sessionThreadOps` around line 209.

- [ ] **Step 2: Extend the interface**

Add the methods needed by thread-mode logging:

```go
type threadOps interface {
    ResolveChannel(channelID string) (*discordgo.Channel, error)
    StartThread(channelID, messageID, name string, archiveDuration int) (*discordgo.Channel, error)
    StartStandaloneThread(channelID, name string, typ discordgo.ChannelType, archiveDuration int) (*discordgo.Channel, error)

    // Thread-mode additions:
    SendComplex(channelID string, data *discordgo.MessageSend) (*discordgo.Message, error)
    EditComplex(edit *discordgo.MessageEdit) (*discordgo.Message, error)
    Unarchive(threadID string) error
}
```

- [ ] **Step 3: Implement new methods on sessionThreadOps**

```go
func (o sessionThreadOps) SendComplex(channelID string, data *discordgo.MessageSend) (*discordgo.Message, error) {
    return o.s.ChannelMessageSendComplex(channelID, data)
}

func (o sessionThreadOps) EditComplex(edit *discordgo.MessageEdit) (*discordgo.Message, error) {
    return o.s.ChannelMessageEditComplex(edit)
}

func (o sessionThreadOps) Unarchive(threadID string) error {
    archived := false
    _, err := o.s.ChannelEditComplex(threadID, &discordgo.ChannelEdit{Archived: &archived})
    return err
}
```

- [ ] **Step 4: Update fakeThreadOps in discord_test.go**

Extend the existing `fakeThreadOps` struct in `platform/discord/discord_test.go` to cover the new methods with recording fields:

```go
type fakeThreadOps struct {
    // ... existing fields ...
    sentMessages  []fakeSentMessage
    editedMessages []fakeEditedMessage
    unarchivedIDs  []string
    sendErr    error
    editErr    error
    unarchiveErr error
    nextMsgID  int
}

type fakeSentMessage struct {
    ChannelID string
    Data      *discordgo.MessageSend
    MsgID     string
}

type fakeEditedMessage struct {
    ChannelID string
    MessageID string
    Edit      *discordgo.MessageEdit
}

func (f *fakeThreadOps) SendComplex(channelID string, data *discordgo.MessageSend) (*discordgo.Message, error) {
    if f.sendErr != nil {
        return nil, f.sendErr
    }
    f.nextMsgID++
    msgID := fmt.Sprintf("msg%d", f.nextMsgID)
    f.sentMessages = append(f.sentMessages, fakeSentMessage{ChannelID: channelID, Data: data, MsgID: msgID})
    return &discordgo.Message{ID: msgID, ChannelID: channelID}, nil
}

func (f *fakeThreadOps) EditComplex(edit *discordgo.MessageEdit) (*discordgo.Message, error) {
    if f.editErr != nil {
        return nil, f.editErr
    }
    f.editedMessages = append(f.editedMessages, fakeEditedMessage{ChannelID: edit.Channel, MessageID: edit.ID, Edit: edit})
    return &discordgo.Message{ID: edit.ID, ChannelID: edit.Channel}, nil
}

func (f *fakeThreadOps) Unarchive(threadID string) error {
    if f.unarchiveErr != nil {
        return f.unarchiveErr
    }
    f.unarchivedIDs = append(f.unarchivedIDs, threadID)
    return nil
}
```

Note: if existing tests use `fakeThreadOps` by value (not pointer), change the receivers to pointer and update call sites. Keep the receiver style consistent — if existing methods are value receivers, use value receivers for the new ones too and restructure recording fields via an out-of-band recorder pointer.

- [ ] **Step 5: Build and run discord tests**

Run: `cd /home/meteorsky/code/cc-connect && go build ./platform/discord/... && go test ./platform/discord/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
cd /home/meteorsky/code/cc-connect && git add platform/discord/discord.go platform/discord/discord_test.go && \
git commit -m "feat(discord): extend threadOps with send/edit/unarchive

Prerequisites for thread-mode logging: the abstraction already covered
thread creation and channel resolution; thread-mode also needs to send
messages into threads, edit the summary embed, and un-archive an
archived thread before posting. fakeThreadOps gains matching recording
fields for tests."
```

---

## Task 9: Thread lifecycle resolver — create, reuse, fallback

**Files:**
- Create: `/home/meteorsky/code/cc-connect/platform/discord/thread_log.go`
- Create: `/home/meteorsky/code/cc-connect/platform/discord/thread_log_test.go`

Encapsulates the decision: given a sessionID + first prompt + current reply context, return a usable log thread ID (creating one if needed, or signaling fallback if the environment does not support it).

- [ ] **Step 1: Write failing tests first**

Create `platform/discord/thread_log_test.go`:

```go
package discord

import (
    "context"
    "strings"
    "testing"

    "github.com/bwmarrin/discordgo"
)

type fakeSessionState struct {
    store map[string]string
}

func (f *fakeSessionState) Get(platform, key string) (string, bool) {
    v, ok := f.store[platform+"|"+key]
    return v, ok
}
func (f *fakeSessionState) Set(platform, key, value string) {
    if f.store == nil {
        f.store = map[string]string{}
    }
    f.store[platform+"|"+key] = value
}
func (f *fakeSessionState) Delete(platform, key string) {
    delete(f.store, platform+"|"+key)
}

func TestResolveLogThread_CreatesThenReuses(t *testing.T) {
    ops := &fakeThreadOps{}
    state := &fakeSessionState{}

    r := &threadLogResolver{
        ops:          ops,
        state:        state,
        parentChanID: "parent-1",
        archiveMins:  4320,
    }

    tid, err := r.Resolve(context.Background(), "s1", "Fix the login bug that breaks SSO redirects")
    if err != nil {
        t.Fatalf("Resolve: %v", err)
    }
    if tid == "" {
        t.Fatalf("empty thread ID on first resolve")
    }
    if len(ops.createdStandaloneThreads) != 1 {
        t.Fatalf("expected 1 StartStandaloneThread call; got %d", len(ops.createdStandaloneThreads))
    }

    // Second call must reuse, not create.
    tid2, err := r.Resolve(context.Background(), "s1", "ignored on reuse")
    if err != nil {
        t.Fatalf("Resolve 2: %v", err)
    }
    if tid2 != tid {
        t.Fatalf("expected reuse; got %q want %q", tid2, tid)
    }
    if len(ops.createdStandaloneThreads) != 1 {
        t.Fatalf("expected no new thread on reuse; creations=%d", len(ops.createdStandaloneThreads))
    }
}

func TestResolveLogThread_NameTruncated(t *testing.T) {
    ops := &fakeThreadOps{}
    state := &fakeSessionState{}
    r := &threadLogResolver{ops: ops, state: state, parentChanID: "p", archiveMins: 4320}

    long := strings.Repeat("x", 200)
    _, err := r.Resolve(context.Background(), "s1", long+"\nmore")
    if err != nil {
        t.Fatalf("Resolve: %v", err)
    }
    if got := ops.createdStandaloneThreads[0].Name; len(got) > 90 {
        t.Fatalf("thread name len=%d exceeds 90; name=%q", len(got), got)
    }
    if strings.Contains(ops.createdStandaloneThreads[0].Name, "\n") {
        t.Fatalf("thread name contains newline: %q", ops.createdStandaloneThreads[0].Name)
    }
}

func TestResolveLogThread_EmptyPromptFallsBackToSessionID(t *testing.T) {
    ops := &fakeThreadOps{}
    state := &fakeSessionState{}
    r := &threadLogResolver{ops: ops, state: state, parentChanID: "p", archiveMins: 4320}

    _, err := r.Resolve(context.Background(), "s7", "")
    if err != nil {
        t.Fatalf("Resolve: %v", err)
    }
    if !strings.Contains(ops.createdStandaloneThreads[0].Name, "s7") {
        t.Fatalf("expected fallback name with sessionID; got %q", ops.createdStandaloneThreads[0].Name)
    }
}
```

You will need to add a `createdStandaloneThreads` recording slice to `fakeThreadOps`:

```go
type fakeStandaloneThreadCreation struct {
    ChannelID       string
    Name            string
    Type            discordgo.ChannelType
    ArchiveDuration int
}

// in fakeThreadOps:
createdStandaloneThreads []fakeStandaloneThreadCreation

func (f *fakeThreadOps) StartStandaloneThread(channelID, name string, typ discordgo.ChannelType, archiveDuration int) (*discordgo.Channel, error) {
    f.createdStandaloneThreads = append(f.createdStandaloneThreads, fakeStandaloneThreadCreation{
        ChannelID: channelID, Name: name, Type: typ, ArchiveDuration: archiveDuration,
    })
    return &discordgo.Channel{ID: fmt.Sprintf("thread-%d", len(f.createdStandaloneThreads))}, nil
}
```

Replace the existing `StartStandaloneThread` on `fakeThreadOps` with this recording version (merging with any existing return-value logic).

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /home/meteorsky/code/cc-connect && go test ./platform/discord/ -run TestResolveLogThread -v`
Expected: FAIL (compile error — `threadLogResolver` undefined).

- [ ] **Step 3: Implement thread_log.go**

Create `platform/discord/thread_log.go`:

```go
package discord

import (
    "context"
    "fmt"
    "strings"
    "sync"

    "github.com/bwmarrin/discordgo"
)

// sessionStateStore is the small slice of core.SessionManager the thread-log
// resolver needs. Kept as a local interface so the discord package does not
// import core directly for this concern, and so tests can inject a fake.
type sessionStateStore interface {
    Get(platform, key string) (string, bool)
    Set(platform, key, value string)
    Delete(platform, key string)
}

// threadLogResolver returns a usable Discord thread ID for a given session,
// creating one lazily on first use and persisting the mapping so daemon
// restarts reuse the same thread.
type threadLogResolver struct {
    ops          threadOps
    state        sessionStateStore
    parentChanID string
    archiveMins  int

    cacheMu sync.Mutex
    cache   map[string]string // sessionID -> threadID
}

const (
    threadNameMaxRunes = 90
    stateKeyThread     = ":thread"
    stateKeySummary    = ":summary_msg"
    stateKeyFirst      = ":first_prompt"
    platformKey        = "discord"
)

func (r *threadLogResolver) Resolve(ctx context.Context, sessionID, firstPrompt string) (string, error) {
    if sessionID == "" {
        return "", fmt.Errorf("discord thread log: empty sessionID")
    }

    // 1. In-memory cache.
    r.cacheMu.Lock()
    if r.cache == nil {
        r.cache = make(map[string]string)
    }
    if tid, ok := r.cache[sessionID]; ok {
        r.cacheMu.Unlock()
        return tid, nil
    }
    r.cacheMu.Unlock()

    // 2. Persistent store.
    if tid, ok := r.state.Get(platformKey, sessionID+stateKeyThread); ok && tid != "" {
        r.cacheMu.Lock()
        r.cache[sessionID] = tid
        r.cacheMu.Unlock()
        return tid, nil
    }

    // 3. Create a new thread.
    name := threadNameFromPrompt(firstPrompt, sessionID)
    ch, err := r.ops.StartStandaloneThread(r.parentChanID, name, discordgo.ChannelTypeGuildPublicThread, r.archiveMins)
    if err != nil {
        return "", fmt.Errorf("discord thread log: create thread: %w", err)
    }

    r.state.Set(platformKey, sessionID+stateKeyThread, ch.ID)
    if firstPrompt != "" {
        r.state.Set(platformKey, sessionID+stateKeyFirst, firstPrompt)
    }
    r.cacheMu.Lock()
    r.cache[sessionID] = ch.ID
    r.cacheMu.Unlock()

    return ch.ID, nil
}

// threadNameFromPrompt strips newlines, trims, and truncates the prompt into
// a Discord-safe thread name (max 100 runes; we use 90 to leave headroom and
// space for an ellipsis). Empty/very-short prompts fall back to "session-<id>".
func threadNameFromPrompt(prompt, sessionID string) string {
    s := strings.ReplaceAll(prompt, "\n", " ")
    s = strings.ReplaceAll(s, "\r", " ")
    s = strings.TrimSpace(s)
    if s == "" {
        return "session-" + sessionID
    }
    runes := []rune(s)
    if len(runes) > threadNameMaxRunes {
        runes = runes[:threadNameMaxRunes-1]
        return string(runes) + "…"
    }
    return s
}
```

- [ ] **Step 4: Run tests**

Run: `cd /home/meteorsky/code/cc-connect && go test ./platform/discord/ -run TestResolveLogThread -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /home/meteorsky/code/cc-connect && git add platform/discord/thread_log.go platform/discord/thread_log_test.go platform/discord/discord_test.go && \
git commit -m "feat(discord): thread log resolver — lazy create + reuse

Introduces threadLogResolver: given a Session.ID and the first user
prompt, returns a Discord thread ID in the configured parent channel.
Creates the thread lazily on first call, persists the mapping via the
core PlatformState store, and reuses it on subsequent calls (within the
same session or after daemon restart). Falls back to 'session-<id>' for
empty or whitespace-only prompts. Thread name is stripped of newlines
and truncated to 90 runes with an ellipsis."
```

---

## Task 10: Thread recovery — unarchive + not-found recreation

**Files:**
- Modify: `/home/meteorsky/code/cc-connect/platform/discord/thread_log.go`
- Modify: `/home/meteorsky/code/cc-connect/platform/discord/thread_log_test.go`

- [ ] **Step 1: Write failing tests**

Append to `platform/discord/thread_log_test.go`:

```go
func TestPostToThread_UnarchivesWhenArchived(t *testing.T) {
    ops := &fakeThreadOps{archivedThreads: map[string]bool{"t-old": true}}
    state := &fakeSessionState{}
    r := &threadLogResolver{ops: ops, state: state, parentChanID: "p", archiveMins: 4320}
    // Pre-populate cached thread.
    r.cache = map[string]string{"s1": "t-old"}
    state.Set(platformKey, "s1:thread", "t-old")

    _, err := r.sendWithRecovery(context.Background(), "s1", "hello", &discordgo.MessageSend{Content: "hi"})
    if err != nil {
        t.Fatalf("sendWithRecovery: %v", err)
    }
    if len(ops.unarchivedIDs) != 1 || ops.unarchivedIDs[0] != "t-old" {
        t.Fatalf("expected 1 unarchive of t-old; got %v", ops.unarchivedIDs)
    }
}

func TestPostToThread_RecreatesOnThreadGone(t *testing.T) {
    ops := &fakeThreadOps{
        sendErrForThread: map[string]error{
            "t-gone": &discordgo.RESTError{Message: &discordgo.APIErrorMessage{Code: 10003}},
        },
    }
    state := &fakeSessionState{}
    state.Set(platformKey, "s1:thread", "t-gone")
    r := &threadLogResolver{ops: ops, state: state, parentChanID: "p", archiveMins: 4320, cache: map[string]string{"s1": "t-gone"}}

    _, err := r.sendWithRecovery(context.Background(), "s1", "first prompt", &discordgo.MessageSend{Content: "hi"})
    if err != nil {
        t.Fatalf("sendWithRecovery: %v", err)
    }
    if len(ops.createdStandaloneThreads) != 1 {
        t.Fatalf("expected thread to be recreated after 10003; creations=%d", len(ops.createdStandaloneThreads))
    }
    if got, _ := state.Get(platformKey, "s1:thread"); got == "t-gone" || got == "" {
        t.Fatalf("expected state to now hold the new thread id, got %q", got)
    }
}
```

Extend `fakeThreadOps` to support selective per-thread errors:

```go
// in fakeThreadOps:
archivedThreads   map[string]bool            // if set, SendComplex returns an archive error for these IDs unless Unarchive has been called
sendErrForThread  map[string]error           // selective send errors keyed by channelID

// Adjust SendComplex:
func (f *fakeThreadOps) SendComplex(channelID string, data *discordgo.MessageSend) (*discordgo.Message, error) {
    if err, ok := f.sendErrForThread[channelID]; ok {
        return nil, err
    }
    if f.archivedThreads[channelID] {
        // Simulate the Discord REST error for archived threads.
        code := 50083
        return nil, &discordgo.RESTError{Message: &discordgo.APIErrorMessage{Code: code}}
    }
    // fall through to existing recording logic
    ...
}

// Adjust Unarchive:
func (f *fakeThreadOps) Unarchive(threadID string) error {
    if f.unarchiveErr != nil {
        return f.unarchiveErr
    }
    f.unarchivedIDs = append(f.unarchivedIDs, threadID)
    if f.archivedThreads != nil {
        delete(f.archivedThreads, threadID)
    }
    return nil
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /home/meteorsky/code/cc-connect && go test ./platform/discord/ -run "TestPostToThread" -v`
Expected: FAIL (`sendWithRecovery` undefined).

- [ ] **Step 3: Implement sendWithRecovery**

Add to `platform/discord/thread_log.go`:

```go
const (
    discordErrUnknownChannel = 10003
    discordErrUnknownMessage = 10008
    discordErrThreadArchived = 50083
)

// sendWithRecovery sends a message into the session's log thread, handling:
//   - archived threads (auto-unarchive then retry)
//   - deleted threads (clear state + recreate + retry once)
// Returns the created Message on success; on final failure returns the
// underlying Discord error from the second attempt.
func (r *threadLogResolver) sendWithRecovery(ctx context.Context, sessionID, firstPrompt string, msg *discordgo.MessageSend) (*discordgo.Message, error) {
    tid, err := r.Resolve(ctx, sessionID, firstPrompt)
    if err != nil {
        return nil, err
    }

    sent, err := r.ops.SendComplex(tid, msg)
    if err == nil {
        return sent, nil
    }

    code := discordErrorCode(err)
    switch code {
    case discordErrThreadArchived:
        if uerr := r.ops.Unarchive(tid); uerr != nil {
            return nil, fmt.Errorf("unarchive: %w", uerr)
        }
        return r.ops.SendComplex(tid, msg)
    case discordErrUnknownChannel:
        // Thread was deleted. Clear state and recreate.
        r.invalidate(sessionID)
        tid, err = r.Resolve(ctx, sessionID, firstPrompt)
        if err != nil {
            return nil, err
        }
        return r.ops.SendComplex(tid, msg)
    }
    return nil, err
}

func (r *threadLogResolver) invalidate(sessionID string) {
    r.cacheMu.Lock()
    delete(r.cache, sessionID)
    r.cacheMu.Unlock()
    r.state.Delete(platformKey, sessionID+stateKeyThread)
    r.state.Delete(platformKey, sessionID+stateKeySummary)
}

func discordErrorCode(err error) int {
    if err == nil {
        return 0
    }
    if re, ok := err.(*discordgo.RESTError); ok && re.Message != nil {
        return re.Message.Code
    }
    return 0
}
```

- [ ] **Step 4: Run tests**

Run: `cd /home/meteorsky/code/cc-connect && go test ./platform/discord/ -run "TestPostToThread" -v`
Expected: PASS.

Run: `cd /home/meteorsky/code/cc-connect && go test ./platform/discord/`
Expected: PASS (no regressions).

- [ ] **Step 5: Commit**

```bash
cd /home/meteorsky/code/cc-connect && git add platform/discord/thread_log.go platform/discord/thread_log_test.go platform/discord/discord_test.go && \
git commit -m "feat(discord): recover from archived / deleted log threads

Adds postOrRecreate: on SendComplex failure, inspect the Discord REST
error code and recover if possible. Code 50083 (archived) triggers an
un-archive + single retry. Code 10003 (unknown channel — thread was
deleted) clears the persisted mapping and creates a fresh thread before
retrying. Any other error is returned as-is."
```

---

## Task 11: Append embed rendering and batch coalescing

**Files:**
- Create: `/home/meteorsky/code/cc-connect/platform/discord/thread_log_render.go`
- Create: `/home/meteorsky/code/cc-connect/platform/discord/thread_log_render_test.go`

- [ ] **Step 1: Write failing tests**

Create `platform/discord/thread_log_render_test.go`:

```go
package discord

import (
    "strings"
    "testing"

    "github.com/bwmarrin/discordgo"
    "github.com/meteorsky/cc-connect/core"
)

func TestBuildAppendEmbed_ToolUse(t *testing.T) {
    entry := core.ProgressCardEntry{
        Kind: core.ProgressEntryToolUse,
        Tool: "Bash",
        Text: "ls -la",
    }
    em := buildAppendEmbed(entry)
    if !strings.Contains(em.Title, "Bash") {
        t.Fatalf("title should mention tool name; got %q", em.Title)
    }
    if !strings.Contains(em.Title, "🔧") {
        t.Fatalf("tool_use title should use wrench emoji; got %q", em.Title)
    }
    if !strings.Contains(em.Description, "ls -la") {
        t.Fatalf("description missing input; got %q", em.Description)
    }
}

func TestBuildAppendEmbed_ErrorIsRed(t *testing.T) {
    entry := core.ProgressCardEntry{Kind: core.ProgressEntryError, Text: "boom"}
    em := buildAppendEmbed(entry)
    if em.Color != colorAppendError {
        t.Fatalf("error embed color = %d; want %d", em.Color, colorAppendError)
    }
}

func TestBuildAppendEmbed_TruncatesLongBody(t *testing.T) {
    entry := core.ProgressCardEntry{Kind: core.ProgressEntryInfo, Text: strings.Repeat("a", 5000)}
    em := buildAppendEmbed(entry)
    if len(em.Description) > appendBodyMaxChars+32 { // +32 for truncation suffix
        t.Fatalf("description len=%d exceeds max+suffix", len(em.Description))
    }
    if !strings.Contains(em.Description, "truncated") {
        t.Fatalf("expected truncation marker; got tail %q", em.Description[len(em.Description)-40:])
    }
}

func TestBuildAppendBatch_ConsolidatesExcess(t *testing.T) {
    entries := make([]core.ProgressCardEntry, 12)
    for i := range entries {
        entries[i] = core.ProgressCardEntry{Kind: core.ProgressEntryToolUse, Tool: "T", Text: "x"}
    }
    embeds, overflow := buildAppendBatch(entries, appendMaxPerTick)
    if len(embeds) > appendMaxPerTick {
        t.Fatalf("batch emitted %d embeds; cap is %d", len(embeds), appendMaxPerTick)
    }
    if len(overflow) == 0 {
        t.Fatalf("expected overflow slice to carry entries beyond the cap")
    }
    if len(embeds)+len(overflow) != len(entries) {
        t.Fatalf("embeds+overflow=%d; want 12", len(embeds)+len(overflow))
    }
}

func TestBuildConsolidationEmbed(t *testing.T) {
    em := buildConsolidationEmbed(7)
    if !strings.Contains(em.Description, "7 more") {
        t.Fatalf("consolidation description should mention the count; got %q", em.Description)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/meteorsky/code/cc-connect && go test ./platform/discord/ -run TestBuildAppend -v`
Expected: FAIL (compile error — types undefined).

- [ ] **Step 3: Implement thread_log_render.go**

```go
package discord

import (
    "fmt"
    "strings"

    "github.com/bwmarrin/discordgo"
    "github.com/meteorsky/cc-connect/core"
)

const (
    colorAppendToolUse    = 0xE5A700 // amber
    colorAppendToolResult = 0x2ECC71 // green
    colorAppendToolError  = 0xE74C3C // red
    colorAppendThinking   = 0x95A5A6 // gray
    colorAppendError      = 0xE74C3C // red
    colorAppendInfo       = 0x3498DB // blue

    appendBodyMaxChars = 500
    appendMaxPerTick   = 5
    appendTruncSuffix  = "\n…(truncated)"
)

// buildAppendEmbed renders a single structured entry as a compact Discord
// embed for the thread append-only log.
func buildAppendEmbed(e core.ProgressCardEntry) *discordgo.MessageEmbed {
    em := &discordgo.MessageEmbed{}

    switch e.Kind {
    case core.ProgressEntryToolUse:
        em.Title = "🔧 " + nonEmpty(e.Tool, "tool")
        em.Description = truncateForAppend(e.Text)
        em.Color = colorAppendToolUse
    case core.ProgressEntryToolResult:
        em.Title = "🧾 " + nonEmpty(e.Tool, "result")
        em.Description = truncateForAppend(e.Text)
        if e.ExitCode != 0 {
            em.Footer = &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("exit %d", e.ExitCode)}
            em.Color = colorAppendToolError
        } else {
            em.Color = colorAppendToolResult
        }
    case core.ProgressEntryThinking:
        em.Title = "💭 Thinking"
        em.Description = truncateForAppend(e.Text)
        em.Color = colorAppendThinking
    case core.ProgressEntryError:
        em.Title = "❌ Error"
        em.Description = truncateForAppend(e.Text)
        em.Color = colorAppendError
    default:
        em.Title = "• " + nonEmpty(e.Tool, "info")
        em.Description = truncateForAppend(e.Text)
        em.Color = colorAppendInfo
    }
    return em
}

// buildAppendBatch selects at most maxPerTick entries for immediate append,
// returning the rest as overflow to be flushed on subsequent ticks. If the
// count of entries is within the cap, all entries are emitted and overflow
// is empty. Caller is responsible for emitting a consolidation embed when
// overflow is non-empty.
func buildAppendBatch(entries []core.ProgressCardEntry, maxPerTick int) (embeds []*discordgo.MessageEmbed, overflow []core.ProgressCardEntry) {
    if maxPerTick <= 0 || len(entries) <= maxPerTick {
        embeds = make([]*discordgo.MessageEmbed, 0, len(entries))
        for i := range entries {
            embeds = append(embeds, buildAppendEmbed(entries[i]))
        }
        return embeds, nil
    }
    embeds = make([]*discordgo.MessageEmbed, 0, maxPerTick)
    for i := 0; i < maxPerTick; i++ {
        embeds = append(embeds, buildAppendEmbed(entries[i]))
    }
    overflow = make([]core.ProgressCardEntry, len(entries)-maxPerTick)
    copy(overflow, entries[maxPerTick:])
    return embeds, overflow
}

// buildConsolidationEmbed renders a single-line "… and K more events"
// notice used when a tick produced more entries than the per-tick cap.
func buildConsolidationEmbed(deferred int) *discordgo.MessageEmbed {
    return &discordgo.MessageEmbed{
        Description: fmt.Sprintf("… and %d more events pending — will flush on the next update", deferred),
        Color:       colorAppendInfo,
    }
}

func truncateForAppend(s string) string {
    s = strings.TrimSpace(s)
    runes := []rune(s)
    if len(runes) <= appendBodyMaxChars {
        return s
    }
    return string(runes[:appendBodyMaxChars]) + appendTruncSuffix
}

func nonEmpty(s, fallback string) string {
    s = strings.TrimSpace(s)
    if s == "" {
        return fallback
    }
    return s
}
```

- [ ] **Step 4: Run tests**

Run: `cd /home/meteorsky/code/cc-connect && go test ./platform/discord/ -run TestBuildAppend -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /home/meteorsky/code/cc-connect && git add platform/discord/thread_log_render.go platform/discord/thread_log_render_test.go && \
git commit -m "feat(discord): append embed rendering + batch coalescing

Per-event embed builders for the thread-log append stream
(tool_use/tool_result/thinking/error/info), with color-coded left bar
and bounded descriptions (500 char body). buildAppendBatch enforces
the per-tick cap of 5 embeds and returns overflow for the next tick;
buildConsolidationEmbed renders the '… and K more events' marker."
```

---

## Task 12: Wire thread-mode progress — implement ProgressSessionAware + ProgressAppender

**Files:**
- Modify: `/home/meteorsky/code/cc-connect/platform/discord/discord.go`
- Modify: `/home/meteorsky/code/cc-connect/platform/discord/thread_log.go`
- Modify: `/home/meteorsky/code/cc-connect/platform/discord/thread_log_test.go`

This is the final wiring: when `progress_style = "thread"`, intercept `SendPreviewStart`, `UpdateMessage`, `DeletePreviewMessage`, and implement `AppendProgressEntries` using the resolver + embed builders from Tasks 9–11.

- [ ] **Step 1: Define the thread-mode handle**

In `platform/discord/thread_log.go`:

```go
// threadPreviewHandle is the previewHandle value returned by the thread-mode
// SendPreviewStartWithMetadata implementation. Carries the session id and
// current thread + summary message ids, plus in-memory state for per-tick
// batching of append events.
type threadPreviewHandle struct {
    sessionID    string
    firstPrompt  string
    threadID     string
    summaryMsgID string

    mu              sync.Mutex
    pendingOverflow []core.ProgressCardEntry
}
```

- [ ] **Step 2: Implement thread-mode SendPreviewStartWithMetadata**

Add to `platform/discord/thread_log.go`:

```go
// StartThreadPreview creates (or reuses) the log thread and posts the initial
// summary embed.
func (r *threadLogResolver) StartThreadPreview(ctx context.Context, meta core.ProgressMetadata, initialPayload string) (*threadPreviewHandle, error) {
    if meta.SessionID == "" {
        return nil, fmt.Errorf("discord thread preview: empty sessionID")
    }

    // Persist first prompt on first sight; do not overwrite on subsequent
    // calls (e.g. after daemon restart).
    if meta.FirstPrompt != "" {
        if _, ok := r.state.Get(platformKey, meta.SessionID+stateKeyFirst); !ok {
            r.state.Set(platformKey, meta.SessionID+stateKeyFirst, meta.FirstPrompt)
        }
    }

    // After a daemon restart, the engine may not have rebuilt the
    // first-prompt context yet. Fall back to the persisted value so the
    // thread can still be recreated with its original name on recovery.
    firstPrompt := meta.FirstPrompt
    if firstPrompt == "" {
        if got, ok := r.state.Get(platformKey, meta.SessionID+stateKeyFirst); ok {
            firstPrompt = got
        }
    }

    tid, err := r.Resolve(ctx, meta.SessionID, firstPrompt)
    if err != nil {
        return nil, err
    }

    // Reuse existing summary message if persisted.
    if msgID, ok := r.state.Get(platformKey, meta.SessionID+stateKeySummary); ok && msgID != "" {
        return &threadPreviewHandle{
            sessionID: meta.SessionID, firstPrompt: firstPrompt,
            threadID: tid, summaryMsgID: msgID,
        }, nil
    }

    summary, err := r.ops.SendComplex(tid, buildThreadSummaryMessage(initialPayload))
    if err != nil {
        // Handle "thread archived" on first post by un-archiving + retry.
        if discordErrorCode(err) == discordErrThreadArchived {
            if uerr := r.ops.Unarchive(tid); uerr != nil {
                return nil, fmt.Errorf("unarchive on summary post: %w", uerr)
            }
            summary, err = r.ops.SendComplex(tid, buildThreadSummaryMessage(initialPayload))
        }
    }
    if err != nil {
        return nil, fmt.Errorf("post summary: %w", err)
    }

    r.state.Set(platformKey, meta.SessionID+stateKeySummary, summary.ID)

    return &threadPreviewHandle{
        sessionID: meta.SessionID, firstPrompt: firstPrompt,
        threadID: tid, summaryMsgID: summary.ID,
    }, nil
}

// buildThreadSummaryMessage turns a payload (either a progress card payload
// or plain content) into a Discord MessageSend. Uses the existing
// buildDiscordPreviewMessage to produce the embed for the summary.
func buildThreadSummaryMessage(payload string) *discordgo.MessageSend {
    // Reuse existing build path — it parses ProgressCardPayload and produces
    // an embed, falling back to plain content otherwise.
    send := buildDiscordPreviewMessage(payload)
    return send
}
```

Note: `buildDiscordPreviewMessage` exists in `platform/discord/format.go` or `progress.go` (the spec places it in `progress.go`). Confirm its signature: `func buildDiscordPreviewMessage(content string) *discordgo.MessageSend`. If it returns `(*discordgo.MessageSend, error)`, adapt.

- [ ] **Step 3: Implement UpdateMessage (summary edit) and AppendProgressEntries on the resolver**

Add to `platform/discord/thread_log.go`:

```go
func (r *threadLogResolver) EditSummary(ctx context.Context, h *threadPreviewHandle, payload string) error {
    embed := buildDiscordSummaryEmbed(payload)
    edit := &discordgo.MessageEdit{
        Channel: h.threadID,
        ID:      h.summaryMsgID,
        Embeds:  []*discordgo.MessageEmbed{embed},
    }
    _, err := r.ops.EditComplex(edit)
    if err == nil {
        return nil
    }

    code := discordErrorCode(err)
    switch code {
    case discordErrThreadArchived:
        if uerr := r.ops.Unarchive(h.threadID); uerr != nil {
            return fmt.Errorf("unarchive on edit: %w", uerr)
        }
        _, err = r.ops.EditComplex(edit)
        return err
    case discordErrUnknownMessage:
        // Summary gone; re-post and update handle.
        r.state.Delete(platformKey, h.sessionID+stateKeySummary)
        send := buildThreadSummaryMessage(payload)
        msg, perr := r.ops.SendComplex(h.threadID, send)
        if perr != nil {
            return perr
        }
        h.summaryMsgID = msg.ID
        r.state.Set(platformKey, h.sessionID+stateKeySummary, msg.ID)
        return nil
    case discordErrUnknownChannel:
        r.invalidate(h.sessionID)
        return err
    }
    return err
}

// AppendEntries flushes at most appendMaxPerTick embeds into the thread;
// any excess is combined with the previous tick's overflow and held on the
// handle for the next tick, with a consolidation embed posted at the tail of
// the current tick.
func (r *threadLogResolver) AppendEntries(ctx context.Context, h *threadPreviewHandle, entries []core.ProgressCardEntry) error {
    if h == nil {
        return fmt.Errorf("nil handle")
    }
    h.mu.Lock()
    combined := append(h.pendingOverflow, entries...)
    h.pendingOverflow = nil
    h.mu.Unlock()

    if len(combined) == 0 {
        return nil
    }

    embeds, overflow := buildAppendBatch(combined, appendMaxPerTick)
    if len(overflow) > 0 {
        embeds = append(embeds, buildConsolidationEmbed(len(overflow)))
    }

    // One write per tick: all embeds in one MessageSend.
    send := &discordgo.MessageSend{Embeds: embeds}
    sent, err := r.sendWithRecovery(ctx, h.sessionID, h.firstPrompt, send)
    if err != nil {
        // On failure, re-queue this tick's entries for retry on the next
        // tick so they are not lost.
        h.mu.Lock()
        h.pendingOverflow = append(combined, h.pendingOverflow...)
        h.mu.Unlock()
        return err
    }
    if sent != nil && sent.ChannelID != h.threadID {
        // Thread was recreated — update the handle to point at the new id.
        h.threadID = sent.ChannelID
    }

    if len(overflow) > 0 {
        h.mu.Lock()
        h.pendingOverflow = append(h.pendingOverflow, overflow...)
        h.mu.Unlock()
    }
    return nil
}
```

Note: `buildDiscordSummaryEmbed(payload string) *discordgo.MessageEmbed` may need to be extracted from the existing `buildDiscordPreviewMessage` in `progress.go`. If `buildDiscordPreviewMessage` already returns the embed in its `Embeds` field, factor it out:

```go
// In platform/discord/progress.go (if not already present):
func buildDiscordSummaryEmbed(payload string) *discordgo.MessageEmbed {
    send := buildDiscordPreviewMessage(payload)
    if len(send.Embeds) > 0 {
        return send.Embeds[0]
    }
    return &discordgo.MessageEmbed{Description: send.Content}
}
```

- [ ] **Step 4: Wire into Platform — conditional thread mode**

In `platform/discord/discord.go`, inside `New()` after the progressStyle/log_thread_* parsing, build the resolver when in thread mode:

```go
if progressStyle == "thread" {
    p.threadLogResolver = &threadLogResolver{
        ops:          sessionThreadOps{s: p.session}, // real discordgo-backed ops
        state:        discordSessionStateAdapter{mgr: sessionManagerFromEngine(p)}, // see note below
        parentChanID: logThreadChannel,
        archiveMins:  logThreadArchive,
    }
}
```

And add to the `Platform` struct:

```go
threadLogResolver *threadLogResolver
```

**Session manager plumbing.** The resolver needs a `sessionStateStore`; the discord Platform does not today receive the engine's `SessionManager` directly. Add a setter that the engine wires during `Start()`:

- Add to `Platform`:

```go
func (p *Platform) SetSessionManager(mgr *core.SessionManager) {
    if p.threadLogResolver != nil {
        p.threadLogResolver.state = &discordSessionStateAdapter{mgr: mgr}
    }
}

type discordSessionStateAdapter struct{ mgr *core.SessionManager }

func (a *discordSessionStateAdapter) Get(platform, key string) (string, bool) {
    return a.mgr.GetPlatformState(platform, key)
}
func (a *discordSessionStateAdapter) Set(platform, key, value string) {
    a.mgr.SetPlatformState(platform, key, value)
}
func (a *discordSessionStateAdapter) Delete(platform, key string) {
    a.mgr.DeletePlatformState(platform, key)
}
```

- In the engine (`core/engine.go`), during platform startup, after platforms are constructed:

```go
type sessionManagerReceiver interface {
    SetSessionManager(*SessionManager)
}

for _, p := range platforms {
    if r, ok := p.(sessionManagerReceiver); ok {
        r.SetSessionManager(e.sessionManager)
    }
}
```

Verify the engine field name `e.sessionManager` — confirm via `grep -n "sessionManager\|SessionManager" /home/meteorsky/code/cc-connect/core/engine.go | head -20`.

- [ ] **Step 5: Implement the two capability interfaces on progressPlatform**

`progressPlatform` is the wrapper used for compact/card. Extend it — or introduce a parallel `threadProgressPlatform` wrapper with the interfaces implemented — for thread mode.

In `platform/discord/discord.go`:

```go
// SendPreviewStartWithMetadata: thread-mode implementation of
// core.ProgressSessionAware. Creates (or reuses) the log thread and posts
// the initial summary.
func (p *progressPlatform) SendPreviewStartWithMetadata(ctx context.Context, replyCtx any, content string, meta core.ProgressMetadata) (any, error) {
    base := p.Platform
    if base.logThreadResolver() == nil {
        // fallback to non-thread behavior
        return base.SendPreviewStart(ctx, replyCtx, content)
    }
    if isDMReplyCtx(replyCtx) {
        slog.Debug("discord: thread style unavailable in DM; falling back to card", "replyCtx", replyCtx)
        return base.SendPreviewStart(ctx, replyCtx, content)
    }
    h, err := base.threadLogResolver.StartThreadPreview(ctx, meta, content)
    if err != nil {
        slog.Warn("discord: thread start failed; falling back to card", "error", err, "session", meta.SessionID)
        return base.SendPreviewStart(ctx, replyCtx, content)
    }
    return h, nil
}

// UpdateMessage: thread-mode variant edits the summary embed in the thread.
func (p *progressPlatform) UpdateMessage(ctx context.Context, previewHandle any, content string) error {
    if h, ok := previewHandle.(*threadPreviewHandle); ok {
        return p.Platform.threadLogResolver.EditSummary(ctx, h, content)
    }
    // Non-thread handle — delegate to base Platform.
    return p.Platform.UpdateMessage(ctx, previewHandle, content)
}

// DeletePreviewMessage: no-op in thread mode (the log is permanent).
func (p *progressPlatform) DeletePreviewMessage(ctx context.Context, previewHandle any) error {
    if _, ok := previewHandle.(*threadPreviewHandle); ok {
        return nil
    }
    return p.Platform.DeletePreviewMessage(ctx, previewHandle)
}

// AppendProgressEntries: thread-mode implementation of core.ProgressAppender.
func (p *progressPlatform) AppendProgressEntries(ctx context.Context, previewHandle any, entries []core.ProgressCardEntry) error {
    h, ok := previewHandle.(*threadPreviewHandle)
    if !ok {
        return nil // non-thread handle has no append log
    }
    return p.Platform.threadLogResolver.AppendEntries(ctx, h, entries)
}

func (p *Platform) logThreadResolver() *threadLogResolver { return p.threadLogResolver }

// isDMReplyCtx inspects the replyCtx for DM indicators. DMs have no parent
// guild; a Discord DM channel ID begins with no guild context. The concrete
// detection follows the existing pattern — check for the platform's DM-marker
// type asserted through replyContext.
func isDMReplyCtx(replyCtx any) bool {
    // Use the existing replyContext.guildID field — DMs have empty guildID.
    if rc, ok := replyCtx.(*replyContext); ok {
        return rc.guildID == ""
    }
    return false
}
```

Confirm `replyContext` field name(s) — if `guildID` is absent today, add it during platform message dispatch or fall back to checking the channel type on cache.

- [ ] **Step 6: Register thread-mode capabilities on the wrapper**

Locate where `progressPlatform` is returned from `New()` (lines 118–124 per spec):

```go
if progressStyle == "compact" || progressStyle == "card" || progressStyle == "thread" {
    return &progressPlatform{Platform: p}, nil
}
return p, nil
```

- [ ] **Step 7: Write an integration-style unit test**

Append to `platform/discord/thread_log_test.go`:

```go
func TestThreadMode_FirstPromptPersistedOnFirstSight(t *testing.T) {
    ops := &fakeThreadOps{}
    state := &fakeSessionState{}
    r := &threadLogResolver{ops: ops, state: state, parentChanID: "p", archiveMins: 4320}

    _, err := r.StartThreadPreview(context.Background(),
        core.ProgressMetadata{SessionID: "s1", FirstPrompt: "hello"}, "initial payload")
    if err != nil {
        t.Fatalf("StartThreadPreview: %v", err)
    }
    got, _ := state.Get(platformKey, "s1:first_prompt")
    if got != "hello" {
        t.Fatalf("first_prompt not persisted; got %q", got)
    }

    // Second call must not overwrite first_prompt.
    _, err = r.StartThreadPreview(context.Background(),
        core.ProgressMetadata{SessionID: "s1", FirstPrompt: "a different prompt"}, "payload 2")
    if err != nil {
        t.Fatalf("StartThreadPreview 2: %v", err)
    }
    got, _ = state.Get(platformKey, "s1:first_prompt")
    if got != "hello" {
        t.Fatalf("first_prompt was overwritten on second call; got %q", got)
    }
}

func TestAppendEntries_SingleTickCoalesces(t *testing.T) {
    ops := &fakeThreadOps{}
    state := &fakeSessionState{}
    r := &threadLogResolver{ops: ops, state: state, parentChanID: "p", archiveMins: 4320}
    h, err := r.StartThreadPreview(context.Background(),
        core.ProgressMetadata{SessionID: "s1", FirstPrompt: "hi"}, "p")
    if err != nil {
        t.Fatalf("StartThreadPreview: %v", err)
    }
    // Drop the initial summary post from the record.
    ops.sentMessages = nil

    entries := make([]core.ProgressCardEntry, 3)
    for i := range entries {
        entries[i] = core.ProgressCardEntry{Kind: core.ProgressEntryToolUse, Tool: "T", Text: "x"}
    }
    if err := r.AppendEntries(context.Background(), h, entries); err != nil {
        t.Fatalf("AppendEntries: %v", err)
    }

    if len(ops.sentMessages) != 1 {
        t.Fatalf("expected exactly one send for 3 entries; got %d", len(ops.sentMessages))
    }
    if n := len(ops.sentMessages[0].Data.Embeds); n != 3 {
        t.Fatalf("expected 3 embeds; got %d", n)
    }
}

func TestAppendEntries_SpillsOverflowToNextTick(t *testing.T) {
    ops := &fakeThreadOps{}
    state := &fakeSessionState{}
    r := &threadLogResolver{ops: ops, state: state, parentChanID: "p", archiveMins: 4320}
    h, _ := r.StartThreadPreview(context.Background(),
        core.ProgressMetadata{SessionID: "s1", FirstPrompt: "hi"}, "p")
    ops.sentMessages = nil

    // 8 entries: 5 sent this tick + consolidation embed; 3 held for next tick.
    entries := make([]core.ProgressCardEntry, 8)
    for i := range entries {
        entries[i] = core.ProgressCardEntry{Kind: core.ProgressEntryToolUse, Tool: "T", Text: "x"}
    }
    if err := r.AppendEntries(context.Background(), h, entries); err != nil {
        t.Fatalf("AppendEntries tick 1: %v", err)
    }
    if len(ops.sentMessages) != 1 {
        t.Fatalf("tick 1: expected 1 send; got %d", len(ops.sentMessages))
    }
    if got := len(ops.sentMessages[0].Data.Embeds); got != appendMaxPerTick+1 {
        t.Fatalf("tick 1: expected %d embeds (5 + 1 consolidation); got %d", appendMaxPerTick+1, got)
    }

    // Next tick with no new entries — spillover should flush.
    if err := r.AppendEntries(context.Background(), h, nil); err != nil {
        t.Fatalf("AppendEntries tick 2: %v", err)
    }
    if len(ops.sentMessages) != 2 {
        t.Fatalf("tick 2: expected a second send for spillover; got %d total", len(ops.sentMessages))
    }
    if got := len(ops.sentMessages[1].Data.Embeds); got != 3 {
        t.Fatalf("tick 2: expected 3 embeds; got %d", got)
    }
}
```

- [ ] **Step 8: Run all tests**

Run: `cd /home/meteorsky/code/cc-connect && go test ./...`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
cd /home/meteorsky/code/cc-connect && git add platform/discord/discord.go platform/discord/thread_log.go platform/discord/thread_log_test.go platform/discord/thread_log_render.go core/engine.go && \
git commit -m "feat(discord): wire thread-mode preview & append hooks

progressPlatform now implements ProgressSessionAware and ProgressAppender
for thread mode:
- SendPreviewStartWithMetadata creates or reuses the session's log thread
  and posts (or reuses) the summary embed at its top
- UpdateMessage edits that summary embed in place
- DeletePreviewMessage is a no-op (thread log is permanent)
- AppendProgressEntries coalesces per-tick bursts into a single send,
  spilling excess to subsequent ticks with a consolidation marker
DM context, missing perms, or thread-creation failure all fall back
gracefully to card-style rendering in the original channel."
```

---

## Task 13: Update config example + docs

**Files:**
- Modify: `/home/meteorsky/code/cc-connect/config.example.toml`
- Modify: `/home/meteorsky/code/cc-connect/CLAUDE.md` (if it documents Discord options — check first)

- [ ] **Step 1: Update config.example.toml**

Run: `grep -n "progress_style\|\[\[projects.platforms\]\]" /home/meteorsky/code/cc-connect/config.example.toml | head -20`

Locate the Discord platform example block. Extend the options:

```toml
[projects.platforms.options]
# ... existing options ...

# progress_style — how tool-use progress is rendered in Discord.
#   "legacy"  — one message per tool event (default)
#   "compact" — single editable text message
#   "card"    — single editable rich embed
#   "thread"  — per-session log thread in log_thread_channel; keeps the
#               main channel conversation clean. See log_thread_channel.
progress_style = "legacy"

# log_thread_channel — REQUIRED when progress_style = "thread".
# Parent channel ID where per-session log threads are created. The bot
# needs Create Public Threads and Send Messages In Threads permissions.
# Must be a regular text channel, NOT a thread.
# log_thread_channel = "123456789012345678"

# log_thread_archive — optional. Auto-archive duration for log threads,
# in minutes. Must be one of: 60, 1440, 4320, 10080. Default: 4320 (3 days).
# log_thread_archive = 4320
```

- [ ] **Step 2: Build to confirm no syntax errors in example**

Run: `cd /home/meteorsky/code/cc-connect && go build ./...`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
cd /home/meteorsky/code/cc-connect && git add config.example.toml && \
git commit -m "docs: document progress_style=thread in config.example.toml

Adds commented examples for log_thread_channel (required for thread
mode) and log_thread_archive (optional; 60/1440/4320/10080 minutes)."
```

---

## Task 14: Manual smoke checklist in PR description

**Files:** no file changes — this is a PR body reminder.

Include the following checklist in the pull request description when landing this work:

- [ ] Enable thread mode on a test guild; run a session with 2–3 tool calls; confirm a thread is created in `log_thread_channel`, named from the first prompt.
- [ ] Summary embed color transitions blue → green on successful completion.
- [ ] Each tool call / result appears as a separate embed below the summary.
- [ ] Final AI answer replies in the original channel, not the thread.
- [ ] Ask a follow-up in the same session; confirm the existing thread is reused.
- [ ] Restart cc-connect; send a follow-up; confirm the existing thread is still reused (PlatformState persistence).
- [ ] Manually delete the thread; send a new event; confirm a new thread is created and the deletion is logged as warn.
- [ ] Try with a DM; confirm silent fallback to `card`.
- [ ] Try with insufficient bot permissions; confirm fallback + warning log naming the missing permission.
- [ ] Send a burst of >5 events within 2 seconds; confirm at most 1 send per tick with a "... and K more events pending" consolidation embed.

---

## Post-Implementation Self-Check

After all tasks are complete, before opening a PR:

- [ ] Full test suite green: `cd /home/meteorsky/code/cc-connect && go test ./...`
- [ ] Race detector clean on core and discord: `go test -race ./core/ ./platform/discord/`
- [ ] Build works with selective compilation excluding discord: `make build EXCLUDE=discord` (confirms core changes did not introduce Discord coupling)
- [ ] `grep -n "\"discord\"" core/*.go` returns zero hits (core did not learn the name "discord")
- [ ] `config.example.toml` parses without errors (exercise via unit test or `cc-connect --check config.example.toml` if such a flag exists)
