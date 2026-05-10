# Reduce Instagram Account Exposure

## Overview

Reverses the pattern that's actually making things worse: every IG request currently authenticates with cookies, which ties bot traffic to a single Instagram account. Combined with our datacenter IP, this makes the account get flagged after burst volume far faster than anonymous requests would. Anonymous yt-dlp could ride per-IP rate limits that recover in hours; flagged accounts go dark for days and invalidate cookies.

Four changes, in priority order:

0. **IG-specific extractor args: switch to iOS-app fingerprint** for Instagram URLs only. The default `X-IG-App-ID` is the **web app id** (`936619743392459`); web-app sessions originating from a Hetzner datacenter range look maximally suspicious to IG. The iOS app id (`124024574287414`, surfaced via `--extractor-args "instagram:app_id=..."` per yt-dlp PR #12359) shifts the request to a different IG endpoint cluster with a different rate profile. Pair with an iOS-app `--user-agent` (UA must match the app_id or it's worse than the default). Also drop `--retries` AND `--fragment-retries` from 3 → 1 for IG: IG escalates flags after 429-retry storms, so retrying after a rate-limit response actively harms us — fragment retries are the worse case (one bad fragment × 3 retries × N fragments multiplies rate-limit signal). Bump `minIGGap` from 8s → 15s — the observed flag point was ~8 successes per 46s (~10 req/min); 15s spacing gives ~50% headroom under that observed rate (8 successes in 105s, ~4.6 req/min) at negligible UX cost.
1. **Try-without-cookies-first** for download paths. Most Instagram posts are public and don't need auth. Bot tries anonymous first; only on `login required` errors retries with `--cookies`. Cookies become a fallback, not the default — account stays clean for the majority of traffic.
2. **Skip `IsPlaylist` for known single-URL patterns.** Currently every `/reel/`, `/p/`, `/tv/` URL generates two yt-dlp Instagram extractions: one for `IsPlaylist`/`GetPlaylistInfo`, then one for the actual download. That's 2× the IG signal for single-video URLs. URL-pattern check eliminates the preflight for the common case.
3. **Cooldown on first IG rate-limit response.** When yt-dlp returns `Requested content is not available, rate-limit reached or login required`, the bot should back off (e.g. 5 minutes) instead of continuing to hammer with the next user's URL. Currently the bot just keeps trying every 8s, digging the account deeper.

These four together substantially reduce account exposure without proxy or warmup. Per codex/external-research consultation, this is the only high-confidence lever set left given the operator's constraints. **`--impersonate` / curl_cffi is explicitly out of scope** — research confirmed the IG extractor doesn't go through endpoints where TLS fingerprint gating applies.

## Context (from discovery)

- **Files involved**:
  - `internal/downloader/downloader.go` — three yt-dlp invocation sites (`DownloadWithProgress`, `GetPlaylistInfo`, `DownloadPlaylistVideo`). `cookieArgs(d.cookiesPath)` currently prepended unconditionally to every call. `waitForIGSlot` provides per-host gating.
  - `internal/engine/engine.go` — `IsPlaylist` calls `GetPlaylistInfo` for every URL the bot/API receives, before deciding whether to call `Process` or `ProcessPlaylist`.
  - `internal/bot/bot.go:156` — `bs.engine.IsPlaylist(...)` call site for routing.
  - `internal/api/api.go:143` — same `IsPlaylist(...)` call site for HTTP API.
- **Patterns observed**:
  - The `cookieArgs(path)` helper returns `nil` for empty path — already supports the disabled case. The "try-anonymous-first" pattern can pass `""` to a per-call cookies override without touching the helper.
  - Existing yt-dlp error messages on rate-limit are surfaced via `formatYtdlpError` (PR #14). We can detect the rate-limit pattern in the wrapped error.
  - URL host extraction for IG already exists in `waitForIGSlot` — same parser can classify single-post vs. playlist-likely URLs.
- **Production state**: bot currently flagged by IG after a burst. Cookies on prod are valid in file but server-side rejected. Operator will refresh cookies after IG mood improves; this PR makes the next refresh last longer.
- **Scope**: 1 production file modified (`downloader.go`), 1 supporting file (`engine.go` for IsPlaylist short-circuit), 1 test file extended. ~120 lines of code total (Layer 0 added ~40 lines: the helper + 4 wire-up sites + tests + minIGGap bump).

## Development Approach

- **testing approach**: Regular (code first, then tests, in the same task)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run `make test` after each change
- maintain backward compatibility: cookies still work the same when needed; rate limiter behavior unchanged for non-IG; IsPlaylist still routes correctly when called.

## Testing Strategy

- **unit tests**:
  - URL classification helper (`isInstagramSinglePost` or similar) — table-driven, covers `/p/`, `/reel/`, `/tv/` (true), `/u/`, root, `/explore/` (false), non-IG URLs (false), URLs with query strings, malformed URLs.
  - Rate-limit detection helper (`isIGRateLimit(err)`) — table-driven with the actual IG error strings observed in production logs.
  - Cooldown behavior in `waitForIGSlot` — after `noteIGRateLimit()` is called, subsequent IG callers wait the cooldown period (not just `minIGGap`).
- **integration / wiring**: not unit-tested at the exec.Cmd boundary (consistent with cookies + throttle wiring trade-off). Verified by diff inspection at the three call sites.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope

## Solution Overview

**Layer 0 — IG-specific extractor args (cheapest, biggest impact):**

A new helper `igExtractorArgs(rawURL string) []string` returns IG-only extras when the URL is `instagram.com`:
- `--extractor-args "instagram:app_id=124024574287414"` (iOS app id; PR #12359)
- `--user-agent "Instagram 339.0.0.12.95 (iPhone16,1; iOS 18_2; en_US; en_US; scale=3.00; gamut=normal; 1179x2556) AppleWebKit/420+"` (matches the iOS app id; mismatched UA + app_id is itself a flag signal)
- `--retries 1` and `--fragment-retries 1` (override the global `--retries 3` / `--fragment-retries 3` from `throttleArgs`; IG escalates flags after 429-retries — last value wins per yt-dlp arg parsing)

Returns `nil` for non-IG URLs so YouTube / Twitter / TikTok keep the existing throttle stack with the desktop Firefox UA. Prepended to args alongside `cookieArgs(...)` and `throttleArgs()` at all three yt-dlp invocation sites.

**Layer 0 args are URL-host-gated, NOT cookies-state-gated.** Every IG-bound yt-dlp invocation gets the iOS app_id + iOS UA + retries=1 — the anonymous attempts from Layer 1 AND the cookies-fallback retries. Both must show the iOS fingerprint; otherwise we leak the web-app fingerprint on half our IG traffic.

`minIGGap` constant in `downloader.go` bumped from `8 * time.Second` to `15 * time.Second`. Production logs showed flagging after ~8 successes per 46s (~10 req/min); 15s spacing puts 8 successes at 105s (~4.6 req/min) — about 50% headroom under the observed flag rate.

**Layer 1 — try-anonymous-first (`DownloadWithProgress` and `DownloadPlaylistVideo`):**

Refactor each download method so the cookies path is a per-call parameter, not always pulled from `d.cookiesPath`. Wrap the actual yt-dlp invocation in a small retry loop:

1. First attempt: pass `""` for cookies (anonymous). **Layer 0's `igExtractorArgs(url)` is still applied** — anonymous IG attempts also get the iOS app_id + iOS UA + retries=1, otherwise we'd leak the web-app fingerprint on the larger half of our IG traffic.
2. If error is detected as IG-auth-required (per `isIGRateLimit`): retry with `d.cookiesPath`. Same Layer 0 args apply.
3. If still fails: surface the original error.

`GetPlaylistInfo` keeps using cookies for the (rare) playlist URL case — playlists are more likely to need auth, and the path runs less often after Layer 2.

**Layer 2 — skip IsPlaylist for known single-URL patterns:**

In `engine.IsPlaylist` (or in the bot/API call sites — pick the layer): pre-classify the URL. If it matches `/p/`, `/reel/`, `/tv/` on `instagram.com`, return `(false, nil, nil)` immediately without calling `GetPlaylistInfo`. No yt-dlp call, no IG hit.

The check is purely syntactic — no network. For non-Instagram URLs and Instagram URLs that DON'T match the single-post pattern (e.g. `/explore/`, `/<username>/saved/`), fall through to the existing `GetPlaylistInfo` call.

**Layer 3 — cooldown on first IG rate-limit:**

Extend `waitForIGSlot` to honor a separate `igCooldownUntil time.Time` field. When the gate fires, if `time.Now() < igCooldownUntil`, wait until that time (not just `minIGGap`). When yt-dlp returns an IG rate-limit error, set `igCooldownUntil = time.Now() + igCooldown` (default 5 minutes).

The cooldown is set INSIDE the downloader's invocation methods after `formatYtdlpError`, only when the wrapped error matches the IG rate-limit pattern. Concurrent goroutines all see the cooldown via the existing `igMu` mutex.

This is **defense in depth on top of** Layer 1. Even when cookies fall back is triggered, if IG starts rejecting both anonymous AND authenticated requests, the bot pauses instead of continuing the burst.

## Technical Details

**Layer 0 — `igExtractorArgs(rawURL string) []string`** (new free function in `downloader.go`):

```go
const igIosAppID = "124024574287414"
// Canonical iOS-app UA shape. App version 339.0.0.12.95 was a real mid-2024
// release; refresh annually to avoid looking like a stale client. Build-id
// suffix (e.g. "; 580058661") intentionally omitted — the X-IG-App-ID header
// is the dominant signal for IG's endpoint routing.
const igIosUA = "Instagram 339.0.0.12.95 (iPhone16,1; iOS 18_2; en_US; en_US; scale=3.00; gamut=normal; 1179x2556) AppleWebKit/420+"

func igExtractorArgs(rawURL string) []string {
    if !isInstagramHost(rawURL) { return nil }
    return []string{
        "--extractor-args", "instagram:app_id=" + igIosAppID,
        "--user-agent", igIosUA,
        "--retries", "1",
        "--fragment-retries", "1",
    }
}
```

`isInstagramHost(rawURL)` — same host-suffix logic already in `waitForIGSlot` (extract to a free function shared between gate and the new args helper).

Order of args at each yt-dlp call site (URL must remain LAST):
```go
args := append(throttleArgs(), cookieArgs(d.cookiesPath)...)
args = append(args, igExtractorArgs(url)...)  // overrides UA + retries for IG
args = append(args, "--no-playlist", "-f", ..., url)
```

yt-dlp parses flags left-to-right; the last `--user-agent`, `--retries`, and `--fragment-retries` win, so the IG-specific values override the throttle defaults exactly when we want them to. **Verified against yt-dlp 2026.3.17 option parser** (codex review confirmed `_configuration_arg('app_id', ['936619743392459'])` at `instagram.py:49` is the live extractor key).

`minIGGap` const change: `8 * time.Second` → `15 * time.Second`. Trivially update the constant, no other refactor needed.

**Layer 1 — `DownloadWithProgress` and `DownloadPlaylistVideo`** become thin wrappers around an internal `runYtdlp(ctx, url, cookiesPath, args, progressCb)` that does ONE yt-dlp invocation:

```go
func (d *Downloader) DownloadWithProgress(ctx, url, progressCb) (*DownloadResult, error) {
    if err := d.waitForIGSlot(ctx, url, progressCb); err != nil { return nil, err }

    // First attempt: anonymous (no cookies). Public IG content downloads
    // here without ever touching the bot's IG account.
    result, err := d.runYtdlp(ctx, url, "", buildArgs..., progressCb)
    if err == nil { return result, nil }

    // Auth-required (rate-limit / login-required) → retry with cookies.
    // Most flagged content goes through this path, which is why we fall
    // back rather than authenticating up front.
    if isIGRateLimit(err) && d.cookiesPath != "" {
        return d.runYtdlp(ctx, url, d.cookiesPath, buildArgs..., progressCb)
    }
    return nil, err
}
```

Implementation note: the args slice currently includes cookies via `cookieArgs(d.cookiesPath)`. We refactor so the args slice is built without cookies, and the cookies arg is prepended in `runYtdlp` from a parameter. This keeps the cookies decision local to the retry logic.

**Layer 2 — `IsPlaylist` short-circuit**:

```go
// engine.go
func (e *Engine) IsPlaylist(ctx, url) (bool, *PlaylistInfo, error) {
    if isInstagramSinglePost(url) {
        return false, nil, nil  // skip GetPlaylistInfo entirely
    }
    info, err := e.downloader.GetPlaylistInfo(ctx, url)
    ...
}
```

URL classifier `isInstagramSinglePost(url string) bool` lives in `downloader.go` (same package as `waitForIGSlot`'s host parser) and is called from engine. Pattern (Go regex on parsed Path):

```go
// instagram.com/{p|reel|tv}/<id>/...
var igSinglePostRe = regexp.MustCompile(`^/(p|reel|tv)/[^/]+/?`)

func isInstagramSinglePost(rawURL string) bool {
    u, err := url.Parse(rawURL)
    if err != nil { return false }
    host := strings.ToLower(u.Hostname())
    if host != "instagram.com" && !strings.HasSuffix(host, ".instagram.com") {
        return false
    }
    return igSinglePostRe.MatchString(u.Path)
}
```

**Layer 3 — `igCooldownUntil` on Downloader**:

New field `igCooldownUntil time.Time` next to `igLastAt`. New method `(d *Downloader) noteIGRateLimit()` that takes `igMu` and sets `igCooldownUntil = time.Now() + igCooldown`. New constant `const igCooldown = 5 * time.Minute`.

`waitForIGSlot` extended (under `igMu`):

```go
remaining := minIGGap - time.Since(d.igLastAt)
cooldownRemaining := time.Until(d.igCooldownUntil)
if cooldownRemaining > remaining { remaining = cooldownRemaining }
if remaining > 0 { /* sleep with progressCb queued event */ }
```

Cooldown stamp set in `DownloadWithProgress` / `DownloadPlaylistVideo` / `GetPlaylistInfo` after yt-dlp returns an IG-rate-limit error:

```go
result, err := d.runYtdlp(...)
if err != nil && isIGRateLimit(err) {
    d.noteIGRateLimit()
}
return result, err
```

`isIGRateLimit(err)` matches the wrapped error string against:
- `"rate-limit reached or login required"`
- `"HTTP Error 429"`
- `"login required"` (generic — IG returns this for stories etc.)

(Conservative match — false positives just cause extra cooldown, which is fine.)

## What Goes Where

- **Implementation Steps**: code, tests, plan-move. All in-repo.
- **Post-Completion**: smoke test the IG path against fresh cookies after IG releases the soft flag (operator action). No infra changes.

## Implementation Steps

### Task 1: Add URL classifier and rate-limit detector helpers

**Files:**
- Modify: `internal/downloader/downloader.go`
- Modify: `internal/downloader/downloader_test.go`

- [x] add `isInstagramSinglePost(rawURL string) bool` to `downloader.go` near `waitForIGSlot`. Parses URL, host-matches IG (same pattern as gate), then path-matches `^/(p|reel|tv)/[^/]+/?`. Returns false on parse error or non-IG host.
- [x] add `isIGRateLimit(err error) bool` to `downloader.go`. Walks the wrapped error chain (errors.Unwrap or string-search the formatted message), returns true if any of `"rate-limit reached or login required"`, `"HTTP Error 429"`, or `"login required"` substrings present.
- [x] add `TestIsInstagramSinglePost` table-driven: positive cases (`/p/abc/`, `/reel/xyz`, `/tv/123/`, `https://www.instagram.com/p/abc/`, with query string `?igsh=...`); negative (`instagram.com/`, `/explore/`, `/u/name/saved/`, `/stories/...`, non-IG hosts); edge (parse error returns false; `evilinstagram.com/p/abc/` returns false — host mismatch).
- [x] add `TestIsIGRateLimit` table-driven: positive (the three exact production strings), negative (random go errors, nil error returns false, generic exit-status-1 error), edge (wrapped errors via `fmt.Errorf("...: %w", inner)`).
- [x] run `make test` — must pass before Task 2.
- [x] run `go vet ./... && go build ./...`.

### Task 2: IG-specific extractor args (Layer 0) + bump minIGGap

**Files:**
- Modify: `internal/downloader/downloader.go`
- Modify: `internal/downloader/downloader_test.go`

- [x] extract the IG-host-match logic from `waitForIGSlot` into a free function `isInstagramHost(rawURL string) bool` so the gate AND the new args helper share one source of truth. (`waitForIGSlot` calls it internally; behavior unchanged.)
- [x] add `const igIosAppID = "124024574287414"` and `const igIosUA = "Instagram 339.0.0.12.95 (iPhone16,1; iOS 18_2; en_US; en_US; scale=3.00; gamut=normal; 1179x2556) AppleWebKit/420+"` near the other yt-dlp constants.
- [x] add `igExtractorArgs(rawURL string) []string` returning `[]string{"--extractor-args", "instagram:app_id=" + igIosAppID, "--user-agent", igIosUA, "--retries", "1", "--fragment-retries", "1"}` for IG URLs (via `isInstagramHost`), `nil` otherwise. Returns a fresh slice each call (consistent with `cookieArgs` / `throttleArgs`).
- [x] wire `igExtractorArgs(url)` into all three yt-dlp invocation sites in `DownloadWithProgress`, `GetPlaylistInfo`, `DownloadPlaylistVideo`. Order: append AFTER `throttleArgs()` and `cookieArgs(...)` so the IG-specific UA / retries / fragment-retries override the desktop defaults via yt-dlp's last-wins arg parsing.
- [x] bump `minIGGap` from `8 * time.Second` to `15 * time.Second`.
- [x] add `TestIgExtractorArgs` table-driven: IG URL returns the expected slice (fresh each call, asserts `--extractor-args` value, iOS UA, `--retries 1`, `--fragment-retries 1`); non-IG returns nil.
- [x] add `TestIgArgsOverrideThrottle` — full args-order integration test that mirrors what call sites build: `args := append(throttleArgs(), cookieArgs("")...); args = append(args, igExtractorArgs(igURL)...)`. Asserts: (a) `--user-agent` appears exactly twice, with the iOS UA literal LAST; (b) `--retries` appears twice with `"1"` LAST; (c) `--fragment-retries` appears twice with `"1"` LAST; (d) `--extractor-args "instagram:app_id=124024574287414"` is present. Also a non-IG case asserting exactly one `--user-agent` / `--retries` / `--fragment-retries` pair (the throttle defaults). This anchors the load-bearing "last-wins" invariant in tests so future arg-order refactors can't silently break Layer 0.
- [x] add `TestIsInstagramHost` table-driven (since we extracted it): `instagram.com`, `www.instagram.com`, `m.instagram.com` true; `evilinstagram.com`, `youtube.com`, `instagram.com.evil.com` false.
- [x] verify existing `TestWaitForIGSlot` still passes after the host-match extraction (no behavior change expected).
- [x] update `TestThrottleArgs` if needed — throttleArgs unchanged in this task, but tests should be re-run.
- [x] run `make test` — must pass before Task 3.

### Task 3: Add cooldown-on-rate-limit to waitForIGSlot

**Files:**
- Modify: `internal/downloader/downloader.go`
- Modify: `internal/downloader/downloader_test.go`

- [x] add `igCooldownUntil time.Time` field on `Downloader` struct (next to `igLastAt`).
- [x] add `const igCooldown = 5 * time.Minute` near `minIGGap`.
- [x] add `(d *Downloader) noteIGRateLimit()` method: locks `d.igMu`, sets `d.igCooldownUntil = time.Now().Add(igCooldown)`. Idempotent — multiple consecutive rate-limit responses just refresh the deadline (effectively a sliding window of 5 min from last bad signal, which is what we want).
- [x] extend `waitForIGSlot`'s wait calculation to take MAX of `(minIGGap - time.Since(d.igLastAt))` AND `time.Until(d.igCooldownUntil)`. The same projected-stamp + lock-narrow + sleep-outside-lock pattern stays.
- [x] update the queued progress emission to use the cooldown-aware remaining duration so user sees the actual wait, not just minIGGap.
- [x] add tests for cooldown:
  - `noteIGRateLimit` sets the deadline; subsequent `waitForIGSlot` waits the cooldown remaining (not just minIGGap), validates via direct field manipulation + tolerance window.
  - cooldown elapses → next call passes through normally.
  - non-IG URLs ignore cooldown (return immediately even if cooldown active — cooldown is IG-specific).
  - second `noteIGRateLimit` within the cooldown window extends the deadline (sliding behavior).
- [x] run `make test` — must pass before Task 4.

### Task 4: Skip IsPlaylist preflight for single-post URLs

**Files:**
- Modify: `internal/engine/engine.go`
- Modify: `internal/engine/engine_test.go`
- (also: any test that relies on IsPlaylist always calling GetPlaylistInfo for IG URLs)

- [x] in `engine.IsPlaylist`, before calling `e.downloader.GetPlaylistInfo`, call `downloader.IsInstagramSinglePost(url)` (export the helper from Task 1 if needed) and short-circuit `return false, nil, nil` for matches.
- [x] add tests:
  - IG single-post URL (`/reel/...`) → IsPlaylist returns `(false, nil, nil)` without invoking the downloader. Use a downloader stub or check that no GetPlaylistInfo error is returned even when the test would normally hit yt-dlp.
  - Non-single-post IG URL (`/explore/`) → falls through to existing path.
  - Non-IG URL → falls through to existing path.
- [x] run `make test` — must pass before Task 5.

### Task 5: Try-anonymous-first in download methods + cooldown wiring

**Files:**
- Modify: `internal/downloader/downloader.go`
- Modify: `internal/downloader/downloader_test.go`

- [x] refactor cookies arg out of the unconditional `cookieArgs(d.cookiesPath)` prepend in `DownloadWithProgress` and `DownloadPlaylistVideo` arg builders. Build args without cookies first; cookies passed separately to a new internal helper.
- [x] introduce small internal helper `(d *Downloader) runWithCookieFallback(ctx, baseArgs, progressCb)` that:
  1. Runs yt-dlp with `cookieArgs("")` (anonymous).
  2. If err and `isIGRateLimit(err)` and `d.cookiesPath != ""` → retries with `cookieArgs(d.cookiesPath)`.
  3. If retry also fails (or initial fail wasn't IG-rate-limit) → returns the error AND calls `d.noteIGRateLimit()` if `isIGRateLimit(err)` is true (so subsequent goroutines back off).
- [x] wire `DownloadWithProgress` and `DownloadPlaylistVideo` to use `runWithCookieFallback`.
- [x] keep `GetPlaylistInfo` using cookies up front — it's only called for unusual playlist URLs after Layer 2 short-circuits the common case. Add `noteIGRateLimit` call on its error path too.
- [x] extract pure-logic helper `shouldRetryWithCookies(err error, cookiesPath string) bool` that returns `isIGRateLimit(err) && cookiesPath != ""`. Five lines of code; unit-testable without exec.Cmd. The retry loop in `runWithCookieFallback` calls it instead of inlining the boolean.
- [x] add `TestShouldRetryWithCookies` table-driven: nil err → false; non-IG err → false; IG-rate-limit err with empty cookies → false; IG-rate-limit err with cookies path set → true.
- [x] run `make test` — must pass before Task 6.

### Task 6: Verify acceptance criteria

- [x] `make test` clean (existing + all new tests).
- [x] `go vet ./...` clean, `go build ./...` clean.
- [x] read the diff and confirm:
  - `igExtractorArgs(url)` is appended at all three yt-dlp invocation sites and overrides UA / retries for IG only.
  - `minIGGap = 15 * time.Second` (was 8s — Task 2 spec; bumped to 15s per the rationale in lines 99-102 of downloader.go, NOT 12s as the original Task 6 wording suggested).
  - `DownloadWithProgress` and `DownloadPlaylistVideo` call yt-dlp ANONYMOUSLY first; only retry with cookies on `isIGRateLimit(err)`.
  - `GetPlaylistInfo` is NOT called by `IsPlaylist` for `/p/`, `/reel/`, `/tv/` IG URLs.
  - `noteIGRateLimit()` is called on every IG rate-limit error path.
  - `waitForIGSlot` honors `igCooldownUntil` in addition to `minIGGap`.
- [x] confirm no regression for non-IG URLs (YouTube, Twitter, etc.):
  - `igExtractorArgs(non-IG-URL)` returns nil → no UA override, retries=3 stays from throttleArgs.
  - They should not enter any of the new code paths.

### Task 7: Move plan to completed

- [ ] move this plan to `docs/plans/completed/20260511-ig-reduce-account-exposure.md`.

## Post-Completion

**Operator workflow after merge + deploy:**

1. Wait 24-48h for IG to release the current soft flag on the account (or solve the in-app challenge if one appears when logging into instagram.com manually).
2. **Re-export cookies from a mobile-network session** (carrier NAT, not home Wi-Fi) — IG cookies harvested over a residential mobile IP age slower from a datacenter user-agent than cookies from the same browser on a desktop/home Wi-Fi. Use the SAME Firefox profile that has the existing logged-in IG session; just be on a mobile hotspot when you click "export cookies." Save to `www.instagram.com_cookies.txt` in the repo root.
   - Note: Layer 0 sends an iOS-app `User-Agent` and the iOS `X-IG-App-ID`. yt-dlp uses cookies as bearer-style session tokens; at the yt-dlp/IG-protocol layer, desktop-Firefox-exported cookies will still work with the iOS UA pair — the iOS pair affects which IG endpoint cluster yt-dlp talks to, not which cookies are accepted. IG's server-side fraud detection MAY slightly de-rate the session for cookie-origin / UA-fingerprint mismatch, but this is far less severe than the burst-rate flagging that Layer 0 + Layer 3 attack directly.
3. Run `./scripts/install-cookies.sh` to deploy the refreshed cookies.
4. Send a previously-failing IG URL to the bot. Confirm:
   - Bot tries anonymous first (visible in logs as a `Running yt-dlp` line WITHOUT `--cookies` arg).
   - On `login required` failure, retries with `--cookies` (second `Running yt-dlp` line WITH `--cookies` arg).
   - All IG yt-dlp invocations show the iOS UA + `instagram:app_id=124024574287414` extractor arg (Layer 0).
   - Public IG posts download via the anonymous attempt; account-tagged posts download via the cookies retry.
5. Smoke test for cooldown: deliberately request a known-private/unavailable IG URL; observe `Waiting for Instagram rate limit (~5m)...` queued message on the next IG request. Cooldown clears after 5 minutes.

**What this PR does NOT do** (out of scope, documented for clarity):
- Residential proxy support (operator declined).
- Account warmup (operator declined).
- Leaky token bucket replacing the gate. Codex flagged it as "preferred" but with Layers 0-3 above, request volume drops enough that 12s gate + 5min cooldown is sufficient. Re-evaluate if flagging continues after this lands.
- yt-dlp `--impersonate` / curl_cffi TLS spoofing. Confirmed by external research that the IG extractor doesn't go through endpoints where TLS fingerprint matters — IG `/api/v1/...` and `/graphql/query` don't gate on JA3. `curl_cffi` also adds dependency cost (`pip install yt-dlp[curl-cffi]`) and is broken on Linux aarch64 ([yt-dlp#14106](https://github.com/yt-dlp/yt-dlp/issues/14106)). Permanently dropped.
- yt-dlp upgrade. Verified: stable@2026.03.17 (current on prod) has the same IG extractor as nightly/master — no IG-relevant fixes have landed since Feb 2025. Upgrading won't help.

**Long-term:** if IG flagging continues after this lands plus a few weeks of operator operation, the remaining lever is residential proxy. No code change can substitute for IP reputation on a Hetzner datacenter range.
