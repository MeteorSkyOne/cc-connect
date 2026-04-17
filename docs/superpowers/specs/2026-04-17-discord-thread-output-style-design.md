# Discord Thread Output Style — Design

**Date:** 2026-04-17
**Status:** Proposed
**Scope:** `platform/discord/`, `core/` (session persistence extension)

## Problem

Today the Discord platform supports three rendering modes for tool-use progress: `legacy` (tool output as separate messages), `compact` (single editable message), and `card` (single editable rich embed). In busy channels, all three clutter the main conversation with tool-call noise, and the `compact` / `card` single-message styles lose history as entries scroll off.

Users want a mode where tool-use logs are relocated out of the user-facing conversation entirely, into a dedicated Discord thread per session — so the main channel stays clean while the full log remains searchable.

## Goals

- Add a new Discord-specific output style that sends a session's tool-use log to a dedicated thread in an operator-configured parent channel.
- The user's question and the AI's final answer stay in the original channel.
- One thread per session, reused across turns and across cc-connect daemon restarts.
- Preserve full chronological history (append-only) while also offering at-a-glance status (editable summary).
- Fail closed with clear errors / graceful fallback when config or permissions are wrong.

## Non-Goals

- Cross-platform log sinks. This is Discord-specific; other platforms keep their existing behavior.
- Configurable per-channel mapping (`#dev` → `#dev-logs`, etc.). A single global log channel is sufficient for v1.
- Moving the user's question or final AI answer into the thread.
- Deleting or trimming threads. Discord's auto-archive handles stale threads; the log is the permanent record.
- Cross-session or global summary dashboards.

## User-Visible Behavior

When `progress_style = "thread"` and `log_thread_channel` is configured:

1. User asks a question in, say, `#general`.
2. Session starts. No thread is created yet.
3. On the first tool-use event for that session, cc-connect creates a new public thread in the configured `log_thread_channel`, named after a truncated version of the user's first prompt.
4. The thread's first message is an editable progress summary embed (same style as today's `card`), showing session state, running/completed/failed status, and a recent-items list.
5. Each subsequent tool call, tool result, thinking block, or error is posted as a separate message below the summary, with color-coded embeds (`🔧` / `🧾` / `💭` / `❌`).
6. The AI's final answer replies in `#general` — not in the thread.
7. When the user asks a follow-up in the same session, the existing thread is reused: summary edits continue, new log events append.
8. If the daemon restarts, resumed sessions continue using the same thread (via persisted state).

## Design

### Config

New option values on the Discord platform block:

```toml
[projects.platforms.options]
type = "discord"
token = "..."
progress_style = "thread"          # new value alongside legacy/compact/card
log_thread_channel = "123456789"   # required when progress_style = "thread"
log_thread_archive = 4320          # optional; Discord auto-archive minutes (default 4320 = 3 days)
```

**Validation** at `platform/discord/discord.go`:`New()`:

- `progress_style = "thread"` with empty `log_thread_channel` → fail fast.
- `log_thread_channel` pointing at a thread channel type → detected on first `ThreadStart` attempt (Discord rejects thread-in-thread). Treated as a fallback-triggering error: warn and fall back to `card` for the affected session.
- `log_thread_channel` set but `progress_style != "thread"` → warn and ignore.
- `log_thread_archive` not one of `{60, 1440, 4320, 10080}` → default `4320`.

### Session → Thread Lifecycle

**Persistence.** Extend `sessionSnapshot` in `core/session.go` with a generic platform-keyed state map so we don't hardcode "discord" into core:

```go
type sessionSnapshot struct {
    // ... existing fields ...
    PlatformState map[string]map[string]string `json:"platform_state,omitempty"`
    // platform name → key → value
}
```

New `SessionManager` methods:

- `GetPlatformState(platform, key string) (string, bool)`
- `SetPlatformState(platform, key, value string)`
- `DeletePlatformState(platform, key string)`

These write atomically through the existing `AtomicWriteFile()` path.

**Discord's use of the map** (keys, all prefixed per-session):

- `"<sessionKey>:thread"` → thread ID
- `"<sessionKey>:summary_msg"` → summary embed message ID

In-memory only (not persisted): the last-rendered-index cursor for the append-only log diff. A daemon restart re-renders from current state onward; a modest re-post of recently-seen entries is acceptable over the complexity of persisting an index.

**Creation (lazy).** On the first `SendPreviewStart` call for a session:

1. Look up the thread ID via `GetPlatformState("discord", sessionKey+":thread")`. If present, reuse.
2. Otherwise, fetch the session's first user prompt (see "First-prompt plumbing" below), strip newlines, truncate to 90 chars with an ellipsis.
3. Call `session.ThreadStart(logThreadChannel, name, ChannelTypeGuildPublicThread, logThreadArchive)`.
4. Persist the new thread ID. Post the initial summary embed via `ChannelMessageSendComplex(threadID, ...)`; persist the summary message ID.

**Reuse across turns.** Later turns in the same session hit the cached / persisted thread ID and skip creation.

**Not-found recovery.** On Discord error 10003 (Unknown Channel — thread deleted) or 10008 (Unknown Message — summary message deleted), clear the relevant persisted entry and recreate on the next event. Logged at warn.

**First-prompt plumbing.** The engine already knows the session's latest user message when it invokes progress rendering. To derive the thread name we need the *first* user message of the session. Options:

- Preferred: extend `SendPreviewStart`'s context (via a new optional field or a new capability interface) to carry the session's first user prompt string.
- Alternative: platform looks it up from its own recorded state (it already sees every inbound message). Slightly messier but keeps core unchanged.

Final choice deferred to implementation plan; both are small. The spec does not prescribe.

### Rendering

**Top message — summary embed, edited in place.**

- Built by reusing the existing `buildDiscordProgressEmbed()` in `platform/discord/format.go` — same color coding (blue=running, green=completed, red=failed), same title/footer/state.
- Edited in place on every progress tick via `ChannelMessageEditComplex(threadID, summaryMsgID, ...)`.
- Throttled by the existing `ProgressUpdateInterval()` mechanism (2s).

**Append-only log — one message per new event.**

- On each `UpdateMessage` call, the platform diffs the incoming `ProgressCardEntry` list against the in-memory cursor and posts only new entries.
- Each new entry becomes a small color-coded embed:
  - Tool call: `🔧 tool_name`, arg preview in description (truncated to ~500 chars), yellow accent.
  - Tool result: `🧾 tool_name`, result preview, green on success / red on error, exit code in footer if non-zero.
  - Thinking block: `💭 Thinking`, text in description, gray.
  - Error: `❌ Error`, error message, red.
- Bodies beyond the limit get a `…(truncated)` suffix. No pagination in v1.

**Final AI answer.** Unchanged. Goes to the original reply context (`#general`) via the existing `Reply()` path. Thread routing affects only the progress-card flow.

**Rate-limit posture.** Append sends piggy-back on the same 2s-throttled progress goroutine. Entries arriving within a single tick are sent in a single loop (respects Discord's per-channel rate limits naturally). No extra backoff logic in v1.

**`SendPreviewStart` / `UpdateMessage` / `DeletePreviewMessage` mapping.**

- `SendPreviewStart`: ensure thread exists → post summary embed → return handle carrying `{threadID, summaryMessageID, cursor=0}`.
- `UpdateMessage`: edit summary embed + append entries since `cursor`; advance cursor.
- `DeletePreviewMessage`: no-op in thread mode. The thread log is the permanent record; the summary stays as the final state marker.

### Fallback & Edge Cases

| Scenario | Behavior |
|---|---|
| DM context (no threads possible) | Silently fall back to `card` style for that session. Debug log. |
| Bot lacks Create Public Threads / Send Messages in Threads perms | Fall back to `card` for that session. Warn log naming the missing permission. |
| `log_thread_channel` not visible / doesn't exist | Same fallback as missing perms. Warn. |
| Thread archived between turns | Discord auto-unarchives on next message. No special handling. |
| Thread locked | Treat as "thread gone" — clear state and recreate. |
| Thread manually deleted (10003) / summary msg deleted (10008) | Clear persisted entry and recreate on next event. Warn. |
| `thread_isolation = true` combined with thread style | Orthogonal. `thread_isolation` determines session keying; `progress_style = "thread"` determines log location. Both apply independently. |
| Cron-triggered sessions | Unchanged user-facing behavior plus a log thread per session. No conflict. |
| `log_thread_channel` points at a thread | Discord rejects on first `ThreadStart`. Warn and fall back to `card`. |
| Multiple concurrent sessions | Each gets its own thread via session-keyed persistence. No cross-session leakage. |

### Testing

**Unit tests** (additions, no framework changes):

- `core/session_test.go`: `GetPlatformState` / `SetPlatformState` / `DeletePlatformState` round-trip through JSON serialization.
- `platform/discord/discord_test.go`:
  - `progress_style = "thread"` without `log_thread_channel` → config error.
  - `log_thread_archive` invalid → clamped to 4320.
  - `log_thread_channel` set but style not thread → warning, option ignored.
  - Thread-name derivation: first prompt truncated to 90 chars with ellipsis; newlines stripped.
  - Session → thread reuse on second event.
  - Thread-gone recovery (simulated 10003) clears and recreates.
  - DM fallback → `card`.
- `platform/discord/format_test.go`:
  - Append embeds (tool call / result / thinking / error) produce expected color, title, truncated body.
  - Diff logic: given cursor and entry list, returns only new entries.

**SDK abstraction for testability.** If no seam exists today for the handful of SDK methods we call (`ThreadStart`, `ChannelMessageSend`, `ChannelMessageSendComplex`, `ChannelMessageEditComplex`), introduce a minimal internal interface in `platform/discord/` so tests can inject a fake. Keep the interface tight — only the methods actually used by this feature.

**Manual smoke checklist** (included in the PR description, not automated):

- [ ] Enable thread mode, run a session with 2–3 tool calls; confirm thread is created in the right channel, named from the first prompt.
- [ ] Summary embed color transitions blue → green on successful completion.
- [ ] Each tool call / result appears as a separate embed below the summary.
- [ ] Final AI answer replies in the original channel, not the thread.
- [ ] Ask a follow-up in the same session; confirm the existing thread is reused.
- [ ] Restart cc-connect; send a follow-up; confirm the existing thread is still reused.
- [ ] Manually delete the thread; send a new event; confirm a new thread is created and logged as warn.
- [ ] Try with a DM; confirm silent fallback to `card`.
- [ ] Try with insufficient perms; confirm fallback + warning.

## Migration

- Default `progress_style` is unchanged (`legacy`). No existing deployments are affected unless an operator opts in.
- The new `PlatformState` field on `sessionSnapshot` is omitempty and backward-compatible: older session files without the field deserialize fine; newer files with it are ignored by older daemons (they'd simply lose the thread mapping and recreate — tolerable).

## Risks & Open Questions

- **First-prompt plumbing choice** (core addition vs. platform-local recording) deferred to the implementation plan. Both are small; pick whichever yields the cleanest diff.
- **Rate limits** under extreme tool-call bursts: the 2s throttle mitigates this, but a session emitting hundreds of events per minute could still hit per-channel limits. If observed in practice, follow up with batching multiple events into a single embed. Out of scope for v1.
- **Thread name quality** for sessions whose first prompt is empty or very short (e.g., "hi"). Falls back to something like `session-<shortkey>` — decided in implementation, not here.
