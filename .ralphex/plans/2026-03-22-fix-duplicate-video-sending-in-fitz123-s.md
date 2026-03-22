# Fix Duplicate Video Sending in sushe

## Goal

Eliminate all sources of duplicate video delivery in the sushe Telegram bot. Three issues, one unified fix:

1. **#6 (EOF on large uploads):** Switch from multipart HTTP upload (`FromReader`/`FromDisk`) to `file://` URI scheme (`FromURL`), so the local Bot API server reads files directly from disk. No large HTTP body = no transport EOF = no false failure.
2. **#7 (Client retry dedup):** Add an in-memory dedup guard on the `/api/download` endpoint. Concurrent or retried requests for the same `(url, chat_id, thread_id)` get rejected (409) while in-progress, or return cached result if completed.
3. **Remove all Document fallback code.** With `file://`, failures are genuine. No masking them with Document resends.

**Success criteria:**
- All 6 upload functions use `tele.FromURL("file://" + path)` instead of `FromReader`/`FromDisk`
- Zero Document fallback code remains in the codebase
- API dedup prevents duplicate processing of identical requests
- All existing tests pass; new tests cover dedup logic (including concurrent access with `-race`)
- Bot handlers show static "Uploading..." (no percentage) since progress tracking is incompatible with `file://`

**Non-goals:**
- Estimated/fake progress bars (not worth the complexity)
- Dedup on bot Telegram handlers (user answer: API only — bot handlers go through the same pipeline)
- Retry logic changes in `upload/retry.go` (unchanged, still handles 429)

## Context

- Files involved:
  - `internal/bot/bot.go` — 4 upload functions with Document fallback + ProgressReader
  - `internal/api/api.go` — 2 upload functions with Document fallback, `handleDownload` entry point
  - `internal/api/types.go` — API types (DownloadRequest, ProgressEvent, ResultEvent)
  - `internal/upload/retry.go` — SendWithRetry (unchanged)
  - `internal/engine/types.go` — ProcessResult, PartResult structs
- Related patterns: telebot `tele.FromURL()`, `tele.FromReader()`, `tele.FromDisk()`
- Dependencies: `gopkg.in/telebot.v3 v3.3.8`, local telegram-bot-api with `--local` flag
- Deploy: Both sushe and telegram-bot-api run as user `sushe`, share `/tmp/sushe` (PrivateTmp=false)

**Key source verification (from code reading):**
- `result.FilePath` is always an absolute path: `Downloader.downloadDir` = `"/tmp/sushe"` (const), all paths built with `filepath.Join(d.downloadDir, ...)` and `filepath.Glob(filepath.Join(workDir, ...))`. Same for `part.FilePath` — built via `filepath.Join(dir, baseName+"_part*.mp4")`. No `filepath.Abs()` needed.
- `result.FileSize` exists on `ProcessResult` (engine/types.go:32) — `int64`, populated from `DownloadResult.FileSize`. Use this for status message display.
- `part.FileSize` exists on `PartResult` (engine/types.go:19) — `int64`, populated from `os.Stat` during split.
- `formatSize()` exists in `internal/bot/bot.go` (lines 690-701) — formats bytes to human-readable string (e.g., "1.5 GB"). Available for use in bot upload status messages.
- `handleSingleDownload` signature: `(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, req DownloadRequest)` — writes final `ResultEvent` at lines 141-147.
- `handlePlaylistDownload` signature: `(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, req DownloadRequest, info interface{})` — writes final `ResultEvent` at lines 191-196.
- Streaming headers set in `handleDownload` (api.go lines 85-88): `Content-Type: application/x-ndjson`, `Cache-Control: no-cache`, `X-Accel-Buffering: no`.

## Validation Commands

```bash
# Build
cd >>/REPO_ROOT && go build ./...

# Run all tests
cd >>/REPO_ROOT && go test ./... -race

# Verify no Document fallback remains
grep -rn "tele.Document" >>/REPO_ROOT/internal/
grep -rn "FromReader\|FromDisk" >>/REPO_ROOT/internal/bot/ >>/REPO_ROOT/internal/api/

# Verify all uploads use file:// URI
grep -rn "FromURL" >>/REPO_ROOT/internal/bot/ >>/REPO_ROOT/internal/api/

# Verify dedup exists
grep -rn "dedup\|Dedup" >>/REPO_ROOT/internal/api/

# Lint
cd >>/REPO_ROOT && golangci-lint run ./... 2>/dev/null || go vet ./...
```

## Decisions

**Resolved (from user answers):**

1. **file:// URI format:** `file:///absolute/path` (RFC 8089 standard). User chose option A.
2. **Upload progress:** Static "Uploading..." message, no percentage. User chose option A — user won't see internal upload progress anyway.
3. **Dedup scope:** API only (`/api/download`). User chose option A — bot handlers go through the same pipeline, no need to duplicate dedup at two levels. (Note: research recommended protecting both API and bot handlers, but user's decision is authoritative.)
4. **Cleanup timing:** Delete after `Send` returns. Bot API reads file synchronously before responding. User accepted default A.
5. **Error reporting:** On genuine failure, show the error. No fallback, no retry suggestion. User clarified: with `file://`, false failures are eliminated; if `Send` returns error it's real.

**[ASSUMED] items:**

- **Dedup TTL: 15 minutes.** Reason type: `evidence-backed unknown`. The download+encode+split+upload pipeline has a 15-minute context timeout (api.go:97). TTL should match this to cover the full operation window. Risk-if-wrong: if TTL is too short, a slow operation could allow a duplicate; if too long, memory grows. 15 minutes matches the existing timeout. (Note: research initially suggested 10 minutes; 15 minutes is correct to match the context timeout.)
- **Dedup data structure: `sync.Map` with inline expiry handling.** Reason type: `evidence-backed unknown`. Single-instance bot, low request volume. No need for external deps (Redis, etc). Risk-if-wrong: minimal — `sync.Map` is battle-tested for this pattern in Go.

**[UNVERIFIED] items:**

- **Claim: `tele.FromURL("file:///path")` sends a JSON POST, not multipart.** Verification status: UNVERIFIABLE (telebot library source outside repo scope). Risk-if-wrong: HIGH — if `FromURL` still sends multipart, the EOF problem remains. Mitigation: Task 1 includes a verification step to confirm the code path in the vendored/cached telebot source before committing to this approach. Task 1 also includes contingency approaches if verification fails.
- **Claim: Local Bot API server supports `file://` URI scheme in `--local` mode.** Verification status: UNVERIFIABLE (external C++ server). Risk-if-wrong: HIGH — if not supported, uploads fail entirely. Mitigation: documented in official Bot API docs for local mode; Task 1 includes a manual test step.
- **Claim: telegram-bot-api has 500-second hardcoded timeout.** Verification status: UNVERIFIABLE (external C++ source). Risk-if-wrong: LOW — even if the exact timeout differs, the EOF problem is real and observed in production logs. The fix (file://) eliminates HTTP body transfer entirely regardless of exact timeout value.
- **Claim: Bot API reads file synchronously before responding to sendVideo.** Verification status: UNVERIFIABLE (external server internals). Risk-if-wrong: MEDIUM — if async, cleanup could delete file before Bot API reads it. Mitigation: current code already cleans up after Send returns (bot.go:243 `defer bs.engine.Cleanup(result)`) and works. Same timing applies.

## Tasks

### Task 1: Verify `file://` URI path in telebot library [HIGH]

**Goal:** Confirm that `tele.FromURL("file:///path")` actually avoids multipart upload before changing all upload functions. This is the critical assumption.

**Files:**
- Read only: `~/go/pkg/mod/gopkg.in/telebot.v3@v3.3.8/api.go` (telebot source in module cache)
- Read only: `~/go/pkg/mod/gopkg.in/telebot.v3@v3.3.8/file.go`

- [x] Read telebot `file.go` to verify `FromURL()` sets `FileURL` field on the `File` struct
- [x] Read telebot `api.go` `sendFiles()` function to verify: when `FileURL` is set, it goes into `params` map (not `rawFiles`), and when `rawFiles` is empty, `b.Raw()` is called (JSON POST, no multipart)
- [x] If the code path confirms JSON POST for `FromURL`: document findings as a comment in the PR description
- [ ] If the code path does NOT confirm JSON POST for `FromURL`, pursue fallback approaches in order:
  - (a) Check if telebot's `params` map can be set directly to pass `"file:///path"` as the video field string, bypassing `FromURL` — e.g., via `video.File = tele.File{FileURL: "file:///path"}` or manipulating `sendFiles` input
  - (b) Patch telebot locally (Go module replace directive) to handle `file://` URLs as URL parameters
  - (c) If neither works, STOP and escalate to user for re-planning — the `file://` approach is not viable without telebot cooperation

### Task 2: Switch bot.go uploads to `file://` URI and remove Document fallback [HIGH]

**Goal:** Replace all 4 bot upload functions to use `tele.FromURL("file://" + filePath)` and remove all Document fallback blocks. Remove ProgressReader usage from upload calls (replace with static status messages).

**Files:**
- Modify: `internal/bot/bot.go`

- [x] In `uploadSingleVideo()` (around line 364): replace `tele.FromReader(progressReader)` with `tele.FromURL("file://" + result.FilePath)` in the Video struct
- [x] In `uploadSingleVideo()`: remove the `os.Open` + ProgressReader setup code (lines 337-362) that creates the file handle, declares `lastUploadUpdate`/`lastUploadPercent`, and builds the progress reader for upload. Keep the status message edit but change it to static `"Uploading...\n{title} | {size}"` where size comes from `result.FileSize` via `formatSize()`
- [x] In `uploadSingleVideo()`: remove `defer file.Close()` (no file handle to close)
- [x] In `uploadSingleVideo()`: remove the entire Document fallback block (lines 375-396): the `if err != nil` block that opens file2, creates tele.Document, and retries
- [x] In `uploadSingleVideo()`: after removing fallback, the error handling becomes: if SendWithRetry fails, edit statusMsg with error and return err
- [x] In `uploadSplitVideo()` (around line 448): replace `tele.FromReader(progressReader)` with `tele.FromURL("file://" + part.FilePath)` in the Video struct
- [x] In `uploadSplitVideo()`: remove os.Open + ProgressReader setup per part, replace with static status `"Uploading Part N/M...\n{title} | {size}"` where size comes from `part.FileSize` via `formatSize()`
- [x] In `uploadSplitVideo()`: remove `file.Close()` call after SendWithRetry (no file handle to close)
- [x] In `uploadSplitVideo()`: remove Document fallback block (lines 466-488)
- [x] In `uploadPlaylistSingleVideo()` (around line 544): replace `tele.FromReader(progressReader)` with `tele.FromURL("file://" + result.FilePath)` in the Video struct
- [x] In `uploadPlaylistSingleVideo()`: remove os.Open + ProgressReader setup, use static `"Uploading..."` status. File size from `result.FileSize`
- [x] In `uploadPlaylistSingleVideo()`: remove `defer file.Close()` (no file handle)
- [x] In `uploadPlaylistSingleVideo()`: remove Document fallback block (lines 559-578)
- [x] In `uploadPlaylistSplitVideo()` (around line 624): replace `tele.FromReader(progressReader)` with `tele.FromURL("file://" + part.FilePath)` in the Video struct
- [x] In `uploadPlaylistSplitVideo()`: remove os.Open + ProgressReader setup per part, use static status. File size from `part.FileSize`
- [x] In `uploadPlaylistSplitVideo()`: remove `file.Close()` call after SendWithRetry
- [x] In `uploadPlaylistSplitVideo()`: remove Document fallback block (lines 648-667)
- [x] Remove `ProgressReader` struct and its `Read` method (lines 19-34) — no longer used
- [x] Run `goimports` (or let the build/linter catch unused imports) to clean up any unused imports (`"io"`, `"os"`, `"sync"`, etc.) — do not manually audit each import
- [x] Verify build: `go build ./internal/bot/`

### Task 3: Switch api.go uploads to `file://` URI and remove Document fallback [HIGH]

**Goal:** Replace both API upload functions to use `tele.FromURL("file://" + filePath)` and remove Document fallback.

**Files:**
- Modify: `internal/api/api.go`

- [x] In `uploadSingleFile()` (line 218): replace `tele.FromDisk(filePath)` with `tele.FromURL("file://" + filePath)` in the Video struct
- [x] In `uploadSingleFile()`: remove Document fallback block (lines 228-240): the `if err != nil` block that creates tele.Document with FromDisk and retries
- [x] In `uploadSingleFile()`: after removing fallback, return `0, err` directly on SendWithRetry failure
- [x] In `uploadSplitParts()` (line 255): replace `tele.FromDisk(part.FilePath)` with `tele.FromURL("file://" + part.FilePath)` in the Video struct
- [x] In `uploadSplitParts()`: remove Document fallback block (lines 272-285)
- [x] In `uploadSplitParts()`: after removing fallback, return error directly on failure
- [x] Run `goimports` to clean up any unused imports
- [x] Verify build: `go build ./internal/api/`

### Task 4: Add API dedup guard for `/api/download` [HIGH]

**Goal:** Prevent duplicate processing when the client retries a POST to `/api/download`. Keyed on `(url, chat_id, thread_id)`. In-progress requests return 409; completed requests return cached result with message_id.

**Files:**
- Create: `internal/api/dedup.go`
- Create: `internal/api/dedup_test.go`
- Modify: `internal/api/api.go`

#### Dedup key format

Use plain string concatenation: `url + "|" + strconv.FormatInt(chatID, 10) + "|" + strconv.Itoa(threadID)`. No hashing — simpler, debuggable, equally effective for this low-volume in-memory use case.

#### Dedup data structure

- [x] Create `internal/api/dedup.go` with a `dedupGuard` struct containing:
  - A `sync.Map` keyed by the concatenated string key (see format above)
  - Entry struct: `type dedupEntry struct { status string; result *ResultEvent; created time.Time }` — status is `"in_progress"` or `"completed"`
  - Constructor: `func newDedupGuard() *dedupGuard`
  - Method `TryAcquire(key string) (cachedResult *ResultEvent, acquired bool)`:
    - Use `sync.Map.LoadOrStore(key, &dedupEntry{status: "in_progress", created: time.Now()})` for atomic insert
    - If `loaded` is false: acquired successfully, return `(nil, true)` — caller proceeds
    - If `loaded` is true: cast the loaded value to `*dedupEntry` and check:
      - If entry is expired (`time.Since(entry.created) > dedupTTL`): delete the key, then retry `LoadOrStore` with a fresh `in_progress` entry (loop back). This handles expiry inline without a separate Range-based cleanup, eliminating the race window between cleanup and acquire.
      - If entry status is `"completed"` and not expired: return `(entry.result, false)` — caller writes cached result
      - If entry status is `"in_progress"` and not expired: return `(nil, false)` — caller writes 409
    - **Important:** Do NOT use Range-based cleanup inside TryAcquire. Expiry is handled inline during the LoadOrStore flow as described above. This avoids iterating the entire map on every call and eliminates the race condition where two goroutines both delete an expired entry and both proceed to insert.
  - Method `Complete(key string, result *ResultEvent)`: update entry to `completed` with result, reset `created` to `time.Now()`. Note: resetting `created` on Complete is intentional — the cache/TTL window starts from when the result becomes available for serving, not from when the request first arrived. This means a 15-minute pipeline followed by 15-minute TTL could keep the entry for up to 30 minutes total, which is correct behavior (the cache should be available for the full TTL after completion).
  - Method `Release(key string)`: delete entry (processing failed — allow future retry)
  - TTL constant: `const dedupTTL = 15 * time.Minute`

#### Wiring into handleDownload

- [x] In `api.go`, add a `dedup *dedupGuard` field to `APIService` struct
- [x] In `NewAPIService()`, initialize `dedup: newDedupGuard()`
- [x] In `handleDownload()`, after request parsing and validation (after line 83, before streaming headers at line 85): compute dedup key: `dedupKey := req.URL + "|" + strconv.FormatInt(req.ChatID, 10) + "|" + strconv.Itoa(req.ThreadID)`
- [x] Call `cachedResult, acquired := s.dedup.TryAcquire(dedupKey)`
- [x] If `cachedResult != nil` (completed, cache hit): set the same streaming headers already used by `handleDownload` (read the existing header-setting code at api.go lines 86-88 and replicate: `Content-Type: application/x-ndjson`, `Cache-Control: no-cache`, `X-Accel-Buffering: no`), then write only the final `ResultEvent` as a single NDJSON line (no progress events — client should handle a cache-hit response having no progress stream), then return. Add a code comment: `// Cache hit: return only the final ResultEvent, no progress events`
- [x] If `!acquired && cachedResult == nil` (in-progress): respond with `http.Error(w, '{"status":"error","ok":false,"error":"duplicate request in progress"}', http.StatusConflict)` and return
- [x] If `acquired`: proceed with normal handling, passing `dedupKey` to sub-handlers

#### Wiring into sub-handlers (dedup key flow)

- [x] Change `handleSingleDownload` signature to accept an additional `dedupKey string` parameter: `func (s *APIService) handleSingleDownload(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, req DownloadRequest, dedupKey string)`
- [x] Change `handlePlaylistDownload` signature to accept an additional `dedupKey string` parameter: `func (s *APIService) handlePlaylistDownload(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, req DownloadRequest, info interface{}, dedupKey string)`
- [x] Update the call sites in `handleDownload` to pass `dedupKey`
- [x] In `handleSingleDownload`: use `defer` with error-check pattern for dedup lifecycle:
  ```go
  var finalResult *ResultEvent
  var handleErr error
  defer func() {
      if handleErr != nil {
          s.dedup.Release(dedupKey)
      } else if finalResult != nil {
          s.dedup.Complete(dedupKey, finalResult)
      }
  }()
  ```
  Then set `finalResult` to the `&ResultEvent{...}` just before writing it at the success path (lines 141-147). Set `handleErr` on error returns.
- [x] In `handlePlaylistDownload`: same defer pattern. Set `finalResult` to the final `&ResultEvent{...}` at line 191-196 success path. Set `handleErr` on error at line 163.
- [x] Verify that all error return paths in both functions result in `Release()` being called (via the defer)

#### Cached response behavior (documented)

When `TryAcquire` returns a cached result, the response contains **only** the final `ResultEvent` as a single NDJSON line — no `ProgressEvent` lines. This is intentional: the work was already done, there is no progress to report. API clients must handle responses that start directly with a `ResultEvent` (status=`"done"`) without preceding progress events.

#### Tests

- [x] Write tests in `internal/api/dedup_test.go`:
  - Test: `TryAcquire` on new key returns `(nil, true)` — acquired
  - Test: `TryAcquire` on in-progress key returns `(nil, false)` — rejected
  - Test: `Complete` then `TryAcquire` returns `(cachedResult, false)` with correct data
  - Test: `Release` then `TryAcquire` returns `(nil, true)` — re-acquired after failure
  - Test: expired entries are cleaned up (set short TTL or manipulate `created` time directly on the entry)
  - Test: **concurrent access** — use a starting gate pattern for true concurrency: create a `sync.WaitGroup` and a `start` channel, launch 10 goroutines that each call `wg.Done()` then block on `<-start`, close the `start` channel to release all goroutines simultaneously, each goroutine calls `TryAcquire` with the same key. Verify exactly one returns `acquired=true` and the other 9 get `acquired=false`. This pattern with `-race` flag provides meaningful concurrency coverage.
- [x] Run tests: `go test ./internal/api/ -race`

### Task 5: Verify acceptance criteria [HIGH]

- [ ] Verify all 6 upload functions use `tele.FromURL("file://" + path)` — no `FromReader` or `FromDisk` remains in bot.go or api.go
- [ ] Verify zero `tele.Document` references remain in bot.go and api.go
- [ ] Verify `ProgressReader` struct is removed from bot.go
- [ ] Verify dedup guard exists and is wired into `handleDownload`
- [ ] Run full test suite: `cd >>/REPO_ROOT && go test ./... -race`
- [ ] Run linter: `cd >>/REPO_ROOT && golangci-lint run ./... 2>/dev/null || go vet ./...`
- [ ] Verify build: `cd >>/REPO_ROOT && go build ./cmd/sushe/`
- [ ] Grep for leftover Document/FromReader/FromDisk in upload paths to confirm clean removal

### Task 6: Update documentation [HIGH]

- [ ] Update README.md (if it documents the upload mechanism or Document fallback behavior) to reflect `file://` URI approach
- [ ] Add comment in `api.go` above `handleDownload` explaining the dedup guard: purpose, TTL (15 minutes, matching context timeout), cached response format (single ResultEvent, no progress events)
- [ ] Add comment in bot.go upload functions explaining why `FromURL` with `file://` is used (local Bot API reads from disk, avoids HTTP timeout/EOF on large files)
- [ ] Update or create entries in GitHub issues #6 and #7 referencing the fix PR

## Revision Diff

### Round 1 to Round 2

Changes addressing all 11 validator issues from Round 1:

1. **(Issue #3, MAJOR) Dedup key flow through sub-handlers:** Added explicit instructions to change `handleSingleDownload` and `handlePlaylistDownload` signatures to accept `dedupKey string` parameter. Added `defer` pattern with `finalResult`/`handleErr` variables for `Complete()`/`Release()` lifecycle. Specified exact locations for these calls relative to the existing `ResultEvent` write points.

2. **(Issue #4, MAJOR) Cached response format:** Added "Cached response behavior" section documenting that cache hits return only the final `ResultEvent` as a single NDJSON line with no progress events. Added code comment instruction for the cache-hit path in `handleDownload`.

3. **(Issue #8, MAJOR) Task 1 contingency:** Added three fallback approaches if `FromURL` verification fails: (a) set `File.FileURL` directly, (b) patch telebot via Go module replace directive, (c) escalate to user. Restructured Task 1 to present these as an ordered fallback chain instead of a hard stop.

4. **(Issue #7, MINOR) Dedup key format:** Changed from SHA256 hash to plain string concatenation (`url + "|" + chatID + "|" + threadID`). Simpler, debuggable, no `crypto/sha256` import needed.

5. **(Issue #6, MINOR) Import cleanup:** Changed "Remove unused imports" steps to "Run `goimports`" instead of manual auditing in both Task 2 and Task 3.

6. **(Issue #2, MINOR) FilePath absoluteness:** Added "Key source verification" section in Context confirming `result.FilePath` and `part.FilePath` are always absolute paths (traced through `Downloader.downloadDir` = `"/tmp/sushe"` const, `filepath.Join`, `filepath.Glob`). No `filepath.Abs()` needed.

7. **(Issue #10, MINOR) Concurrent dedup test:** Added concurrent test case to Task 4 tests: "launch 10 goroutines calling TryAcquire with same key, verify exactly one acquires." Added `-race` flag to test commands.

8. **(Issue #11, MINOR) File size source:** Clarified in Task 2 steps that `{size}` comes from `result.FileSize` / `part.FileSize` via `formatSize()`. Added to Context section confirming these fields exist on the types.

9. **(Issue #1, MINOR) TTL discrepancy note:** Added parenthetical note in Dedup TTL assumed item: "research initially suggested 10 minutes; 15 minutes is correct to match the context timeout."

10. **(Issue #5, MINOR) Lazy cleanup sufficiency:** Added note in Dedup data structure assumed item: "Lazy cleanup is sufficient for this low-volume bot — entries don't accumulate between requests in any meaningful way."

11. **(Issue #9, MINOR) Research dedup_scope divergence:** Added parenthetical note in Decision #3: "Note: research recommended protecting both API and bot handlers, but user's decision is authoritative."

### Round 2 to Round 3

Changes addressing all 7 validator issues from Round 2:

1. **(MAJOR — Race condition in TryAcquire):** Replaced the check-then-store algorithm with `sync.Map.LoadOrStore(key, &dedupEntry{...})` for atomic insert. If `loaded` is true, check the loaded entry's status/expiry. If `loaded` is false, acquired. This eliminates the race window where two goroutines could both see the key as absent and both insert.

2. **(MAJOR — Cleanup races with acquire):** Removed the Range-based lazy cleanup from inside TryAcquire entirely. Expiry is now handled inline: after `LoadOrStore` returns a loaded expired entry, delete it and retry `LoadOrStore` with a fresh entry. This avoids iterating the entire map on every call and eliminates the race condition where cleanup deletes an entry that another goroutine is about to acquire. Updated the ASSUMED item description from "lazy cleanup" to "inline expiry handling".

3. **(MAJOR — Streaming headers for cache-hit):** Added explicit instruction to read the existing streaming headers from `handleDownload` (api.go lines 86-88) and replicate them for cache-hit responses. Documented the actual headers (`Content-Type: application/x-ndjson`, `Cache-Control: no-cache`, `X-Accel-Buffering: no`) found in the source. Also added these headers to the Context section under "Key source verification" for reference.

4. **(MINOR — TTL reset on Complete):** Added explanatory note in the `Complete` method description clarifying that resetting `created` on Complete is intentional — the cache window starts from completion, not acquisition. Documented the implication: a 15-minute pipeline + 15-minute TTL = up to 30 minutes total retention, which is correct.

5. **(MINOR — formatSize() verification):** Verified `formatSize()` exists at `internal/bot/bot.go` lines 690-701. Added to "Key source verification" section in Context with exact line numbers.

6. **(MINOR — Approximate line ranges):** Replaced `~337-362` with exact line range `337-362` for the ProgressReader setup in `uploadSingleVideo()`. Confirmed via source reading: line 337 is `file, err := os.Open(result.FilePath)`, line 362 is the closing brace of the ProgressReader struct literal.

7. **(MINOR — Concurrent test starting gate):** Specified the exact `sync.WaitGroup` + channel starting gate pattern for the concurrent dedup test: goroutines call `wg.Done()` then block on `<-start`, main closes `start` channel to release all simultaneously. This ensures true contention when combined with `-race`.
