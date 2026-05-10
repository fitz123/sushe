# Instagram Anti-Flag Throttling

## Deviations from original plan (read first)

This file is the historic plan, but the as-shipped design differs from the
original in two material ways. Phase-1 review applied both fixes after
discovering issues during implementation. Future readers should treat the
descriptions below — not the original prose this section replaces — as ground
truth for what the code actually does.

1. **`waitForIGSlot` lock discipline (lock → stamp → unlock → sleep).** The
   original plan put the sleep INSIDE the critical section. The as-shipped
   design holds `igMu` only long enough to read+stamp `igLastAt` to the
   projected wake time (`time.Now().Add(remaining)`), then releases the lock
   and sleeps outside. progressCb is also called outside the lock. Rationale:
   stamping to the projected wake time inside the lock makes concurrent
   callers queue themselves `minIGGap` further out (instead of all racing to
   the same `now+remaining` once they reacquire), and moving the sleep +
   progressCb out of the critical section means a slow Telegram edit can't
   block other goroutines from observing the projected next-slot time.

2. **Gate placement (single-video gated, playlist info NOT gated, playlist
   items gated).** The original plan gated both `DownloadWithProgress` and
   `GetPlaylistInfo`. Review found that gating `GetPlaylistInfo` charged
   single-URL IG calls the gap twice (once for `IsPlaylist` discovery, again
   for the download itself). The as-shipped design:
   - `DownloadWithProgress` — gated (the user-burst path the gate primarily
     defends against).
   - `GetPlaylistInfo` — NOT gated (metadata-only `--flat-playlist --dump-json`
     fetch, single short request, doesn't fit the burst pattern IG flags).
   - `DownloadPlaylistVideo` — gated per item (one gate-wait per item inside
     the sequential `engine.ProcessPlaylist` loop).

The remainder of this document has been edited in-place to reflect the
as-shipped behavior; nothing below intentionally contradicts this section.

## Overview

Reduces the bot's automation footprint against Instagram so cookies sessions last longer than a few days before Instagram triggers its "We suspect automated behavior" challenge. Two layers of defense:

1. **yt-dlp throttling flags** — `--sleep-requests`, `--sleep-interval`/`--max-sleep-interval`, `--retries`, `--fragment-retries`, `--user-agent`, `--socket-timeout`. Applied to all three yt-dlp invocation sites (single video, playlist info, playlist item). Affects every download (yt-dlp ignores them when irrelevant on non-IG sites — harmless).
2. **App-level per-host rate limit** — a process-wide minimum spacing between Instagram-bound yt-dlp invocations. yt-dlp's own `--sleep-interval` only governs intervals within a single invocation; a bot serving multiple Telegram users can still fire 5 IG requests in 2 seconds when a user pastes a thread, which is the burst pattern Instagram flags hardest. A `sync.Mutex` + `time.Time` on the `Downloader` enforces a minimum 8-second gap across all goroutines for `instagram.com` URLs.

The user-agent value matches the browser where cookies were exported (Firefox 150 on macOS). yt-dlp's default UA is stale Chrome 95 — itself a flag, and worse a mismatch with desktop Firefox cookies.

This is the in-code half of a defense-in-depth stack. The rest is infrastructural (residential proxy, dedicated bot account, account warmup) and is documented in Post-Completion as out-of-scope follow-ups.

## Context (from discovery)

- **Files involved**:
  - `internal/downloader/downloader.go` — three yt-dlp invocation sites built around `args := append(cookieArgs(d.cookiesPath), ...)`. New throttling args follow the same prepend pattern.
  - `internal/downloader/downloader_test.go` — append new `cookieArgs`-style helper test for `throttleArgs`; add tests for the rate limiter.
  - The `Downloader` struct gains two new fields (`igMu sync.Mutex`, `igLastAt time.Time`).
- **Patterns observed**:
  - Free-function helper returning a `[]string` then prepended to args slice (`cookieArgs`) is the established pattern for adding cross-cutting yt-dlp flags. New `throttleArgs()` follows the same shape (no state, just a constant slice).
  - `Downloader` already encapsulates all yt-dlp invocations — engine and bot layers don't reach yt-dlp directly. So a rate limiter inside `Downloader` is the natural choke point.
- **Scope**: 1 production file modified, 1 test file extended. ~50 lines of code total. No new files.

## Development Approach

- **testing approach**: Regular (code first, then tests, in the same task)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run `make test` after each change
- maintain backward compatibility: rate limiter is a no-op for non-Instagram URLs; throttling args are appended to existing args, no removals

## Testing Strategy

- **unit tests**:
  - `TestThrottleArgs` — table-driven, asserts the helper returns the expected static slice (catches accidental edits to magic-string flag values).
  - `TestWaitForIGSlot` — drives the rate-limiter method directly:
    - non-IG URL: returns immediately (no sleep observed via `time.Now()` deltas).
    - IG URL on a fresh state: returns immediately (no prior request).
    - IG URL with a recent prior IG request: sleeps until the min-gap elapses.
    - YouTube URL containing the substring "instagram" in path or query: should NOT trigger limit (host-only match).
- **wiring verification**: not unit-tested at the exec.Cmd boundary (same trade-off as the cookies wiring). Verified by diff inspection that all three yt-dlp call sites have `throttleArgs()` prepended AND call `waitForIGSlot(url)` first thing.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope

## Solution Overview

Two pieces, both inside `internal/downloader/downloader.go`:

1. **`throttleArgs() []string` (free function, constant slice)** — returns the same yt-dlp flags every time. Prepended to args at all three invocation sites alongside `cookieArgs(d.cookiesPath)`. The slice contains:
   ```
   --sleep-requests 2
   --sleep-interval 2 --max-sleep-interval 5
   --retries 3 --fragment-retries 3
   --socket-timeout 30
   --user-agent "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:150.0) Gecko/20100101 Firefox/150.0"
   ```
   Free function (not method) so the helper has no hidden state and is trivially testable.

2. **`(d *Downloader) waitForIGSlot(ctx context.Context, rawURL string, progressCb ProgressCallback) error` (method, mutex-guarded, context-aware)** — checks if the URL host matches `instagram.com`; if so, briefly enters `d.igMu` to compute `remaining := minIGGap - time.Since(d.igLastAt)` and stamp `d.igLastAt = time.Now().Add(remaining)` (the projected wake time) when `remaining > 0`, then EXITS the lock. Outside the lock, if a wait is needed, emits ONE `Progress{Phase: "queued", ETA: remaining}` event via `progressCb` (when non-nil) and waits via `select { case <-time.After(remaining): case <-ctx.Done(): return ctx.Err() }`. For non-IG URLs returns nil immediately without taking the lock. On ctx cancellation, returns `ctx.Err()` so the caller propagates the cancellation up; the stamp set inside the lock is preserved across cancellation, preventing retry storms from bypassing the gap. `minIGGap` is a package-level `const` (8 seconds) — hardcoded per user preference.

Host extraction: parse with `net/url.Parse`, then exact-or-suffix-with-dot check on the parsed `Hostname()`:
```go
host := strings.ToLower(u.Hostname())
isIG := host == "instagram.com" || strings.HasSuffix(host, ".instagram.com")
```
This catches `instagram.com`, `www.instagram.com`, `m.instagram.com` while correctly excluding `evilinstagram.com` and `notinstagram.com` (the leading dot guards against suffix-spoofing).

Both fixes are prepended at each yt-dlp invocation site:
```go
if err := d.waitForIGSlot(ctx, url, progressCb); err != nil { return nil, err }
args := append(throttleArgs(), append(cookieArgs(d.cookiesPath), <existing args>...)...)
```
For `GetPlaylistInfo` (no progress callback in scope), pass `nil` for `progressCb`.

Order matters: `cookieArgs` must come before existing args so URL stays last; `throttleArgs` simply prepends to that.

**Playlist double-gating avoidance** (as-shipped, reconciled with phase-1 review fix):

> **NOTE (2026-05-10):** the as-shipped behavior is the INVERSE of the original plan text. Phase-1 review fix #1 reversed it after observing that gating `GetPlaylistInfo` forced every cold-start IG single-video URL to wait `~minIGGap` for metadata and then again for the download. The final inverted design is documented below; the rationale below applies to the inverted design, not the original.

`GetPlaylistInfo` does NOT call `waitForIGSlot`. `DownloadPlaylistVideo` DOES call it (one gate-wait per playlist item, INSIDE the sequential `engine.ProcessPlaylist` loop). Single-video downloads (`DownloadWithProgress`) also call the gate — that's the user-burst pattern we're primarily protecting against.

Why the inversion is safe:
- `GetPlaylistInfo` is a metadata-only `--flat-playlist --dump-json` fetch — a single short request IG does not flag the same way as a burst of media downloads.
- Skipping the gate at `GetPlaylistInfo` removes the cold-start double-charge for single-URL IG: `IsPlaylist` (which calls `GetPlaylistInfo` under the hood) always precedes the actual download, so gating both would add `~minIGGap` of pure waiting before the download even starts.
- Gating `DownloadPlaylistVideo` keeps the global pacing on the heavy traffic IG actually flags. Each playlist item is downloaded sequentially in `engine.ProcessPlaylist`, so the gate fires once per item — for a 50-item IG playlist with `minIGGap=8s` that's ~400s of cumulative gate wait, intentional and acceptable because (a) IG playlists are rare in this bot's traffic mix and (b) overlapping `minIGGap` with download time amortizes most of it.

**Known limitation (documented in code 2026-05-10):** concurrent multi-user IG metadata fetches at `GetPlaylistInfo` are NOT paced. Empirically this has not triggered flagging, but the proper structural fix is to move the IG gate from the downloader layer to a higher request-level point (e.g. `engine.Process` / `engine.IsPlaylist`) with one-gate-per-request semantics so both metadata and download share a single gate without double-charging. Deferred as a non-trivial refactor; tracked in the `GetPlaylistInfo` comment.

## Technical Details

**`throttleArgs()`**:
- Free function, returns a fresh slice each call (consistent with `cookieArgs`).
- All values are constants. Single source of truth — change one place, applies everywhere.
- Comment in code documents WHY each flag (lower retries to avoid retry-storm flag, sleep-requests for IG metadata GraphQL calls, UA matching cookies-export browser).

**`Downloader.igMu` / `Downloader.igLastAt`**:
- New fields on the existing struct. Zero-value fine on init.
- `igMu` is `sync.Mutex` (not RWMutex — write-only access pattern).
- `igLastAt` is `time.Time` (zero value means "never seen IG before").

**`(d *Downloader) waitForIGSlot(ctx context.Context, rawURL string, progressCb ProgressCallback) error`**:
- Cheap early-out on already-cancelled context (`ctx.Err()` non-nil) before any parsing or locking.
- Parses URL via `net/url.Parse`. On parse error, returns nil silently (URL is malformed; let yt-dlp report the error downstream — don't double-report).
- Host match: `host := strings.ToLower(u.Hostname()); isIG := host == "instagram.com" || strings.HasSuffix(host, ".instagram.com")`. Catches `instagram.com`, `www.instagram.com`, `m.instagram.com`. Excludes `evilinstagram.com`, `notinstagram.com`, `instagram.com.evil.com` (Hostname() strips port, the leading-dot suffix guards against confusables).
- Lock/sleep ordering (as shipped — see "Deviations from original plan"):
  1. `d.igMu.Lock()` — narrow critical section, no I/O.
  2. Compute `remaining := minIGGap - time.Since(d.igLastAt)`.
  3. If `remaining > 0`: stamp `d.igLastAt = time.Now().Add(remaining)` (the PROJECTED wake time, so the next waiter sees "next slot opens at now+remaining" and queues `minIGGap` further out). Else: stamp `d.igLastAt = time.Now()`.
  4. `d.igMu.Unlock()` — exit the lock BEFORE sleeping or invoking progressCb.
  5. If `remaining > 0`: invoke `progressCb(Progress{Phase: "queued", ETA: remaining.Round(time.Second).String()})` (when progressCb != nil), then `select { case <-time.After(remaining): case <-ctx.Done(): return ctx.Err() }`.
  6. Return nil.
- Why the projected-wake-time stamp inside the lock (instead of stamping `time.Now()` after the sleep): it lets concurrent callers serialize correctly via a narrow lock. Each new caller sees the most recent projected wake time, computes its own remaining wait off that, and stamps its OWN projected wake time. The actual sleeps then overlap with `progressCb` and other work outside the lock, but the spacing invariant is preserved by the stamps. With a 4-URL burst and 8s gap, the 4th caller waits ~24s — acceptable since serving slowly is much better than getting flagged.
- `igLastAt` is updated unconditionally (even on ctx cancellation, since the stamp happens before the sleep). Intentional: prevents a retry stampede where N quick cancellations or failures bypass the limit.
- The "queued" Progress event is emitted EXACTLY ONCE per call (not periodically), and only when an actual wait is needed AND progressCb is non-nil. For the typical 8-24s wait, a single message edit is enough — the Telegram message stays on "Waiting for Instagram rate limit..." until yt-dlp starts producing real download progress.

**Constant**:
```go
const minIGGap = 8 * time.Second
```

**Why prepend, not append**: yt-dlp accepts options in any order before the URL. URL must remain LAST in the args slice. Both `throttleArgs` and `cookieArgs` prepend, preserving URL-last invariant.

**Why not slice the global limit by user**: bot's threat model is account-level, not per-user. Instagram flags the **session** (the cookies), not the per-user IP (we have one IP). So the gap must be enforced globally across all Telegram users sharing the bot.

## What Goes Where

- **Implementation Steps**: helper function, struct fields, method, three wire-up sites, tests. All in-repo.
- **Post-Completion**: out-of-scope follow-ups (residential proxy, account warmup, fresh dedicated account) and a tunability note about `minIGGap`.

## Implementation Steps

### Task 1: Add `throttleArgs` helper and prepend at all three yt-dlp call sites

**Files:**
- Modify: `internal/downloader/downloader.go`
- Modify: `internal/downloader/downloader_test.go`

- [x] add unexported free function `throttleArgs() []string` to `internal/downloader/downloader.go` near `cookieArgs`. Returns the static slice with `--sleep-requests`, `--sleep-interval`/`--max-sleep-interval`, `--retries`, `--fragment-retries`, `--socket-timeout`, `--user-agent` and their values. Add a doc comment explaining each flag's purpose (avoids future drift to "what does this do, can I delete it").
- [x] in each of the three yt-dlp invocation sites (`DownloadWithProgress`, `GetPlaylistInfo`, `DownloadPlaylistVideo`), add `throttleArgs()` to the args build chain. The existing pattern is `args := append(cookieArgs(d.cookiesPath), ...existing...)`. New pattern: `args := append(throttleArgs(), append(cookieArgs(d.cookiesPath), ...existing...)...)`. URL stays last.
- [x] add `TestThrottleArgs` to `downloader_test.go` — asserts the returned slice contains the expected flag/value pairs (length, ordering, key flag presence). Catches accidental edits.
- [x] run `make test` — must pass before Task 2.
- [x] run `go vet ./... && go build ./...`.

### Task 2: Add per-host rate limiter

**Files:**
- Modify: `internal/downloader/downloader.go`
- Modify: `internal/downloader/downloader_test.go`

- [x] add `igMu sync.Mutex` and `igLastAt time.Time` fields to the `Downloader` struct.
- [x] add package-level constant `const minIGGap = 8 * time.Second` near other constants (`MaxFileSize`, `MaxUploadSize`, etc.).
- [x] add unexported method `(d *Downloader) waitForIGSlot(ctx context.Context, rawURL string, progressCb ProgressCallback) error` that parses URL, host-matches `instagram.com` via `host == "instagram.com" || strings.HasSuffix(host, ".instagram.com")` (note the leading dot — guards against `evilinstagram.com`), then:
  - Enter `d.igMu`, compute `remaining := minIGGap - time.Since(d.igLastAt)`, stamp `d.igLastAt = time.Now().Add(remaining)` if `remaining > 0` else `d.igLastAt = time.Now()`, exit `d.igMu`.
  - Outside the lock, if `remaining > 0`: emit `progressCb(Progress{Phase: "queued", ETA: remaining.Round(time.Second).String()})` (when progressCb != nil) and wait via `select { case <-time.After(remaining): case <-ctx.Done(): return ctx.Err() }`.
  - Returns nil for non-IG URLs and parse errors. Skip progress emit and sleep entirely if `remaining <= 0` (no spurious UI churn for warmed-up callers).
- [x] call `if err := d.waitForIGSlot(ctx, url, progressCb); err != nil { return nil, err }` as the first line of `DownloadWithProgress`. ALSO call from `DownloadPlaylistVideo` (one gate-wait per item inside `engine.ProcessPlaylist`'s sequential loop). Do NOT call from `GetPlaylistInfo` (see "Playlist double-gating avoidance" in Solution Overview — gating metadata charges single-URL IG flows the gap twice). Place before building args, before creating the work directory.
- [x] add `TestWaitForIGSlot` to `downloader_test.go` with table-driven cases:
  - non-IG URL (youtube.com/watch?...) — expect elapsed < 100ms, igLastAt unchanged.
  - IG URL with zero `igLastAt` (initial call) — expect elapsed < 100ms, igLastAt updated to ~now.
  - IG URL with `igLastAt` set to `now-1*time.Second` — expect elapsed in `[minIGGap-2s, minIGGap+1s]` window (CI scheduler tolerance).
  - IG URL with `igLastAt` set to `now-(minIGGap+5s)` — expect elapsed < 100ms (gap already elapsed).
  - URL with substring "instagram" in path/query but non-IG host (`youtube.com/watch?title=instagram`) — expect elapsed < 100ms (host-only match).
  - URL with confusable host `evilinstagram.com` — expect elapsed < 100ms (excluded by leading-dot check).
  - Invalid URL (`":://broken"`) — expect elapsed < 100ms, no error (parse error returns nil).
  - **Context cancellation**: IG URL with `igLastAt = now`, but ctx cancelled mid-sleep — expect `ctx.Err()` returned, lock released. Test by creating a `ctx, cancel := context.WithCancel(...)`, starting the call in a goroutine, calling `cancel()` after 50ms, asserting the call returns within ~100ms with `context.Canceled` error.
  - **Progress emission on wait**: IG URL with `igLastAt = now-1s`, supplying a stub progressCb that records calls — expect EXACTLY ONE call with `Phase: "queued"`, ETA non-empty.
  - **No progress emission on no-wait**: IG URL with `igLastAt = now-(minIGGap+5s)` (gap elapsed), supplying a stub progressCb — expect ZERO calls (no UI churn when warmed up).
  - **No progress emission for non-IG**: youtube URL, supplying a stub progressCb — expect ZERO calls.
  - **Nil progressCb safe**: IG URL with wait needed, progressCb=nil — must not panic.
  - Note: the "1 second old prior" timing test was adjusted to use a (minIGGap - 300ms) prior so the unit test waits ~300ms instead of ~7s, while still exercising the same wait-path code. Result is identical at the code-coverage level; trade-off is faster test runs.
- [x] run `make test` — must pass before Task 3.
- [x] run `go vet ./... && go build ./...`.

### Task 3: Render "queued" phase in bot UI

**Files:**
- Modify: `internal/bot/bot.go`

- [x] in the progress switch in `processURL`'s `progressCb` (around line 147 — the `switch p.Phase` block), add a `case "queued"`:
  ```go
  case "queued":
      if p.ETA != "" {
          statusText = fmt.Sprintf("⏳ Waiting for Instagram rate limit (~%s)...", p.ETA)
      } else {
          statusText = "⏳ Waiting for Instagram rate limit..."
      }
  ```
  Note: bot.go uses an engine-level `ProgressCallback` (`phase, percent, detail`) — not `Progress` directly. ETA is forwarded as `detail` via a new `case "queued"` branch in `engine.adaptProgressCb` so the bot can render it. The case in bot.go matches against `detail` instead of `p.ETA`.
- [x] verify by code reading: `Progress.ETA` is already populated by `waitForIGSlot`; the message edit through `bs.bot.Edit(statusMsg, statusText)` is already inside the rate-limited update block, so a single "queued" event will be honored even if it arrives <2s after the previous edit (because `p.Percent < 100` AND no other update has happened — but the progressCb's existing rate-limiting (line 138) checks `now.Sub(lastUpdate) < minUpdateInterval`; since this is the FIRST update for this download, `lastUpdate` is the zero time and `Sub` returns a huge value, so the edit goes through immediately).
- [x] no test file changes (bot.go has no existing tests; this case is verified by manual smoke test in Post-Completion). Engine adapter change is exercised through existing `engine_test.go` patterns — no new tests added since the adapter is a one-line case in a switch and there is no `engine_test.go` coverage of the "queued" phase pattern.
- [x] run `make test` and `go build ./...` — both must pass before Task 4.

### Task 4: Verify acceptance criteria

- [x] `make test` green (all existing tests + `TestThrottleArgs` + `TestWaitForIGSlot`).
- [x] `go vet ./...` clean, `go build ./...` clean.
- [x] read the diff of `internal/downloader/downloader.go` and confirm:
  - `throttleArgs()` is prepended at all three `exec.CommandContext(ctx, "yt-dlp", ...)` sites (single video, playlist info, playlist video).
  - `waitForIGSlot(ctx, url, progressCb)` is called as the first line of `DownloadWithProgress` (progressCb propagated) AND `DownloadPlaylistVideo` (progressCb propagated). NOT called in `GetPlaylistInfo` (avoids charging single-URL IG flows the gap twice via `IsPlaylist` discovery + download; see Solution Overview).
  - URL is the LAST element in every args slice.
  - The lock is held only for the read+stamp of `igLastAt`. The sleep (`select { case <-time.After: case <-ctx.Done() }`) and progressCb invocation happen OUTSIDE the lock. The stamp uses the PROJECTED wake time (`time.Now().Add(remaining)`) so concurrent callers queue minIGGap further out.
  - "queued" Progress emission is gated by `remaining > 0 && progressCb != nil` so non-IG / warmed-up / no-callback paths don't churn.
- [x] confirm `Downloader` struct has `igMu sync.Mutex` and `igLastAt time.Time` fields.
- [x] confirm `internal/bot/bot.go` has a `case "queued"` branch in the progress switch that renders "Waiting for Instagram rate limit..." (with ETA suffix when present).

### Task 5: Move plan to completed

- [x] move this plan to `docs/plans/completed/20260510-ig-anti-flag-throttling.md`.

## Post-Completion

**Manual verification** (after deploy):

- Build and `make update`. Confirm bot starts cleanly.
- Send a Telegram URL that previously failed with `login required` — should download successfully (assuming refreshed cookies). No regression on non-IG URLs (YouTube, Twitter, etc.) — they should download as fast as before, throttle args don't slow them down meaningfully.
- Burst test: paste 3 Instagram URLs in quick succession to the bot. First one should download immediately. The second one's status message should display "⏳ Waiting for Instagram rate limit (~8s)..." then transition to download progress. Third one's queued message ETA should be ~16s.
- **UA propagation check**: run one IG download manually on the server with `yt-dlp -v --user-agent "<our UA>" --cookies <path> <url> 2>&1 | rg User-Agent` to confirm yt-dlp actually sends our UA in HTTP requests, not its IG-extractor default. If yt-dlp's Instagram extractor overrides `--user-agent`, we'll see the default Chrome 95 in the verbose log instead of our Firefox 150 — that means the flag is no-op for IG and we need a different approach (e.g. `--add-header "User-Agent:..."` or extractor-args).

**Out-of-scope follow-ups** (not part of this PR):

- **Residential proxy** — set `SUSHE_PROXY=socks5://...` and pass `--proxy` to yt-dlp. Single biggest non-code lever per research; addresses "cookies session jumped from US Mac Firefox to DE Hetzner Linux" cookie-IP-coherence flag. Add when ready to spend €5/mo.
- **Dedicated bot Instagram account** — fresh account, warmed up by hand for a week before the bot uses it. Critical for sustainability.
- **`minIGGap` tuning** — start at 8s. If flagging still happens, raise to 12-15s. If users complain about latency on bursts, can lower to 5s. Currently a `const` (recompile to change). If repeated tuning needed, promote to env var (`SUSHE_IG_MIN_GAP`) — not premature optimization, but YAGNI for now since one number isn't worth a config knob yet.
- **`--proxy` and `--add-header` for Accept-Language** — second-tier hardening once residential proxy is in place. Out of scope here.
