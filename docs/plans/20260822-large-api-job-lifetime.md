# Plan: Large API job lifetime

## Goal

A legitimate large single-video `POST /api/download` job can complete download, processing, splitting, and Telegram upload without the old fixed 15-minute API cancellation. The synchronous NDJSON lifecycle remains explicitly bounded by the API/engine deadline plus the existing bounded upload client/retry path, timeout errors name the limiting phase and bound, failures release dedup state, and successes remain cacheable.

## Non-goals

- No unbounded job or background queue.
- No silent 1080p quality fallback or extractor/format-selection changes.
- No local `telegram-bot-api` timeout change.
- No change to the unrelated 15-minute completed-result `dedupTTL`.
- No change to Telegram bot-handler contexts; issue #31 is the HTTP API path.
- No production operations in Ralphex task checkboxes.

## Context

- `internal/api/api.go` currently wraps the whole request in `context.WithTimeout(r.Context(), 15*time.Minute)`.
- `downloader.DefaultTimeout` already permits a single yt-dlp subprocess to run for 60 minutes. A real 4.596 GB 1080p source projected close to that bound and was canceled at 15 minutes before processing/upload.
- ffprobe/ffmpeg phases inherit the API context. Telegram sends do not accept that context, but each attempt is separately bounded by the bot HTTP client's 60-minute timeout and `SendWithRetry`'s finite retry count. The effective lifecycle is therefore a documented composition, not an unbounded request.
- In-progress dedup entries intentionally never expire; the handler's deferred `Complete`/`Release` remains authoritative.
- Minime review of the Fable draft replaced its unsupported 30-minute processing guess with a source-derived default of `2 * downloader.DefaultTimeout`: one full allowed download window plus an equal processing/splitting envelope. Tests must pin this relationship. Upload remains separately bounded and must be documented truthfully.
- Progress callbacks can arrive from subprocess scanner goroutines, so phase tracking must be race-safe.

## Validation Commands

```bash
go test ./... -count=1
go test -race ./internal/api/... -count=1
go vet ./...
go build ./...
```

## Tasks

### Task 1: Replace the old API timeout with a source-derived bounded lifecycle and truthful errors
**Goal:** Large API jobs cross the former 15-minute boundary while deadline/cancellation output identifies the active phase and actual bound.
**Serves:** Ninja's requirement that legitimate large jobs finish under bounded custody or fail with a clear phase/bound result.

- [x] Define one named API engine/job deadline derived from `downloader.DefaultTimeout` (`2 *` the 60-minute subprocess bound), expose only an unexported per-service test override, and replace the fixed 15-minute context.
- [x] Track the latest engine progress phase race-safely for single-video and playlist paths.
- [x] Distinguish the named API deadline from caller/client cancellation using context cause/state rather than error-string matching; emit/log an actionable terminal NDJSON error naming phase and bound while preserving ordinary engine errors.
- [x] Make yt-dlp subprocess deadline errors name their separate per-download bound when that bound—not the caller context—fires.
- [x] Preserve deferred dedup completion/release behavior and keep the unrelated cache TTL unchanged.
- [x] Run focused API/downloader tests before Task 2.

### Task 2: Add hermetic regression coverage for boundary, cancellation, and dedup behavior
**Goal:** Fast tests prove the old limit is gone, all relevant terminal classifications are truthful, and dedup state is correct.
**Serves:** The required regression, timeout/cancellation, and safe-retry acceptance evidence without real long sleeps or external services.

- [x] Add only the minimum unexported processor/upload seams needed for handler-level tests; keep `NewAPIService`'s public signature unchanged.
- [x] Add a fake processor that records the context deadline, scripts progress/result/error behavior, and avoids yt-dlp/Telegram.
- [x] Test that the default deadline is greater than 15 minutes and equals the documented source-derived relationship to `downloader.DefaultTimeout`, then complete a fake single-video request with `done/ok:true` and cached result.
- [x] With a millisecond test override, prove an API deadline names the last phase and bound and releases the dedup key.
- [x] Prove caller cancellation is reported as client/request cancellation rather than the API bound and releases the dedup key.
- [x] Add a focused downloader test proving its own subprocess timeout names the per-download bound.
- [x] Run focused tests plus `go test -race ./internal/api/... -count=1` before Task 3.

### Task 3: Document and verify effective API lifetime semantics
**Goal:** Operators and clients understand the composed bounds and no stale API 15-minute claim remains.
**Serves:** The requirement for actionable lifetime semantics and reliable long-running NDJSON clients.

- [ ] Update `AGENTS.md` and touched source comments: the API deadline bounds download/processing/splitting; upload attempts are separately bounded by the finite telebot HTTP timeout/retry path; deadline errors name phase/bound; failures release dedup immediately; successes use the unrelated 15-minute cache TTL.
- [ ] State that NDJSON clients/proxies must keep their read/idle lifetime compatible with the effective server lifecycle and wait for a terminal event.
- [ ] Remove stale references to the old 15-minute API request timeout while leaving `dedupTTL` and bot-handler contexts intact.
- [ ] Run all validation commands and inspect the final diff for issue #31 scope only.

## Post-Completion

Parent-owned lifecycle after Ralphex: open/review/merge the public PR, deploy merged `main` through repository scripts, verify services/logs, and perform a real large-source Telegram delivery smoke. These are not Ralphex tasks.
