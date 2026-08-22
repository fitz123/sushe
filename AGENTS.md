# Sushe - Telegram Video Downloader Bot

## Quick Start

```bash
# Build locally
make build

# Deploy to server (first time)
make deploy

# Update bot binary only
make update

# Check service status
make verify
```

## Project Structure

```
sushe/
├── cmd/
│   ├── sushe/main.go           # Entry point: Telegram poller + HTTP API server
│   └── test-split/main.go      # Test utility for video splitting
├── internal/
│   ├── api/api.go              # HTTP API: POST /api/download with bearer auth
│   ├── api/dedup.go            # Request deduplication guard for /api/download
│   ├── api/dedup_test.go       # Tests for dedup guard
│   ├── bot/bot.go              # Telegram handlers, progress updates, uploads
│   ├── downloader/downloader.go      # yt-dlp wrapper, ffprobe, ffmpeg, splitting
│   ├── downloader/downloader_test.go # Unit tests for codec helpers and split logic
│   ├── engine/engine.go        # Core download+transcode+split engine (no upload)
│   ├── logger/logger.go        # Structured logging with slog
│   └── upload/retry.go         # SendWithRetry: 429/FloodError retry helper
├── scripts/
│   ├── deploy.sh               # Full server deployment
│   ├── update.sh               # Quick binary update
│   ├── install-ytdlp.sh        # Install pinned service-owned yt-dlp nightly
│   ├── verify.sh               # Service status check
│   └── build-bot-api.sh        # Build telegram-bot-api server
├── bin/                        # Built binaries (gitignored)
├── .env                        # Secrets (gitignored)
├── .env.example                # Template for .env
└── Makefile
```

## Architecture

### Components

1. **Entry Point** (`cmd/sushe/main.go`)
   - Uses `gopkg.in/telebot.v3`
   - Starts Telegram LongPoller + HTTP API server (if `SUSHE_API_TOKEN` set)
   - Connects to local Telegram Bot API server for 2GB upload support
   - Graceful shutdown for both services

2. **Engine** (`internal/engine/engine.go`)
   - Core download+transcode+split pipeline shared by bot and HTTP API
   - `Process(ctx, url, progressCb)` → `*ProcessResult` (file paths + metadata)
   - `ProcessPlaylist(ctx, url, progressCb)` → `[]*ProcessResult`
   - Engine does NOT upload — returns local file paths; callers handle upload via telebot

3. **HTTP API** (`internal/api/api.go`)
   - `POST /api/download` — download video and send to any Telegram chat/topic
   - Bearer token auth via `SUSHE_API_TOKEN` env
   - Request deduplication by (url, chat_id, thread_id); active jobs do not expire, while successful results have a separate 15-minute cache TTL
   - Streams NDJSON progress events + final result
   - Bounds download/processing/splitting with a two-hour engine deadline derived as `2 * downloader.DefaultTimeout`
   - `GET /health` — service health check
   - Uses engine for download, then telebot `Send()` for upload; each send attempt has the shared 60-minute HTTP client timeout and `SendWithRetry` permits up to three FloodError retries

4. **Bot Handlers** (`internal/bot/bot.go`)
   - `/dl` command + URL auto-detect in messages
   - Real-time progress updates via Telegram message editing
   - Multi-part upload with threaded replies
   - Delegates download to engine, keeps telebot upload logic
   - GENERAL topic guard (ThreadID == 0/1 → warning)

5. **Downloader** (`internal/downloader/downloader.go`)
   - yt-dlp wrapper with format selection preferring H.264
   - Codec detection via ffprobe (video codec, audio codec, pixel format)
   - Conditional re-encoding (VP9/AV1 → H.264) via ffmpeg
   - Codec-aware video splitting for files >1.9GB:
     - Branch A: `-c copy` (stream copy) for H264+AAC+yuv420p — zero RAM overhead
     - Branch B: Full re-encode with memory-safe settings (`ultrafast`, 720p, 1 thread) for incompatible codecs
   - Split target size: 1.7GB (`MaxSplitSize`) with 200MB margin for keyframe overshoot

6. **Upload Retry** (`internal/upload/retry.go`)
   - `SendWithRetry()` wraps telebot `Send()` with 429/FloodError handling
   - Max 3 retries, sleeps for `RetryAfter` seconds
   - Used by both bot handlers and HTTP API

### Video Processing Flow

```
URL → Engine.Process() → yt-dlp download → codec check (ffprobe)
    → re-encode if needed (ffmpeg) → split if >1.9GB (codec-aware) → ProcessResult
    ↓ Split: H264+AAC+yuv420p → -c copy | else → re-encode (ultrafast/720p/1 thread)
    ↓ Bot mode: telebot sendInThread (with progress message editing)
    ↓ HTTP API: telebot Send + NDJSON progress stream to caller
```

### Codec Handling

Telegram requires H.264 for inline video playback. VP9/AV1 videos only play audio.

**yt-dlp format selection** (prefers H.264):
```
bestvideo[vcodec^=avc1][height<=1080]+bestaudio[acodec^=mp4a]/
bestvideo[vcodec^=avc][height<=1080]+bestaudio/
bestvideo[height<=1080]+bestaudio/best
```

**Post-download**: If codec is not H.264, re-encode with ffmpeg.

## HTTP API

`POST /api/download` — download video and send to a Telegram chat/topic.

- **Auth:** `Authorization: Bearer <SUSHE_API_TOKEN>` header
- **Port:** `SUSHE_API_PORT` env (default `8082`)
- **Enabled when:** `SUSHE_API_TOKEN` env is set (no token = bot-only mode)

**Request:**
```json
{"url": "https://youtube.com/watch?v=...", "chat_id": -1001234567890, "thread_id": 120}
```

**Response** (`Content-Type: application/x-ndjson`, streamed):
```
{"status":"started","url":"..."}
{"status":"queued","eta":"7s"}
{"status":"downloading","percent":45.2}
{"status":"encoding","percent":80.0,"codec":"vp9"}
{"status":"splitting","part":1,"total":3}
{"status":"uploading","part":1,"total":1}
{"status":"done","ok":true,"title":"Video Title","message_id":789,"file_size":123456}
```

The `queued` event appears only for Instagram URLs when the process-wide rate
limiter delays the download (see "Instagram rate limiting" below). `eta` is a
Go duration string (e.g. `"7s"`) and is present for single-video downloads;
playlist queued events currently omit `eta` because the playlist progress
callback does not carry detail through (tracked as a TODO in
`internal/api/types.go`).

**Lifetime and terminal events:** The API applies a two-hour deadline
(`2 * downloader.DefaultTimeout`) to engine work: playlist detection,
download, transcoding/processing, and splitting. If that deadline fires, the
terminal NDJSON error names the latest phase and the two-hour bound. A caller
or client cancellation is reported separately rather than as a server
deadline.

Telegram upload starts after engine work and does not accept the engine
context. It is bounded separately: the shared telebot HTTP client allows 60
minutes per send attempt, and `SendWithRetry` makes at most three additional
attempts for FloodError responses. Split parts and playlist results are sent
sequentially, so the effective end-to-end request lifetime is the engine bound
plus the finite per-file upload/retry path.

NDJSON clients and reverse proxies must therefore keep their total request and
read/idle lifetimes compatible with that effective lifecycle, including a
potentially long interval after an `uploading` event. A progress event is not
completion: clients must continue reading until a terminal `done` or `error`
event arrives.

**Errors:**
- `401` — missing or invalid bearer token
- `400` — missing `url` or `chat_id`
- `409` — duplicate request already in progress (same url + chat_id + thread_id)
- NDJSON `{"status":"error","ok":false,"error":"..."}` for download/upload failures

**Deduplication:** Requests are deduplicated by (url, chat_id, thread_id). If an identical
request completed within the last 15 minutes, the response contains only the final result
event (no progress events). This completed-result cache TTL is unrelated to job lifetime.
Active entries never expire and return 409 to duplicates. Engine or upload failures release
their entry as soon as the handler finishes, allowing an immediate retry; only full successes
are cached (partial playlist uploads are retryable).

**Health check:** `GET /health` → `OK`

## Deployment

### Server Details

- **Host**: Configured in `~/.ssh/config`
- **User**: `sushe`
- **Services**: `telegram-bot-api.service`, `sushe.service`

### Local Telegram Bot API Server

Required for uploading files >50MB (up to 2GB). Built from `github.com/tdlib/telegram-bot-api` using Docker.

### Environment Variables

Required in `.env`:
```
TELEGRAM_BOT_TOKEN=<token>
TELEGRAM_API_ID=your_api_id
TELEGRAM_API_HASH=your_api_hash
SSH_PUBLIC_KEY=your_ssh_public_key
```

Optional (enables HTTP API):
```
SUSHE_API_TOKEN=your_api_token    # Bearer token for POST /api/download
SUSHE_API_PORT=8082               # HTTP API port (default: 8082)
```

Optional runtime settings:
```
SUSHE_COOKIES=<path>              # absolute path to Netscape-format cookies file; passed to yt-dlp as `--cookies`
SUSHE_YTDLP=<path>                # yt-dlp executable; empty uses `yt-dlp` from PATH
```

Note: `SUSHE_COOKIES` is set on the server via systemd (see "Cookies for authenticated downloads" below), not in `.env`. The pinned updater writes `SUSHE_YTDLP` only to the remote application `.env`; it does not alter the local deploy `.env`, which holds SSH/deploy configuration.

## Key Functions

### engine.go

- `NewEngine(cookiesPath, ytdlpPath string)` - Create engine with downloader instance; empty values disable cookies and use `yt-dlp` from PATH
- `Process(ctx, url, progressCb)` - Download + codec check + transcode + split → ProcessResult
- `ProcessPlaylist(ctx, url, progressCb)` - Process playlist → []ProcessResult
- `IsPlaylist(ctx, url)` - Check if URL is a playlist
- `Cleanup(result)` - Remove work directory

### api.go

- `NewAPIService(engine, bot, token)` - Create API service
- `Handler()` - Returns http.Handler with routes
- `handleDownload(w, r)` - POST /api/download handler (auth + dedup + engine + upload + NDJSON stream)

### dedup.go

- `newDedupGuard()` - Create dedup guard with mutex-protected map and cleanup goroutine
- `TryAcquire(key)` - Acquire dedup lock; returns cached result or in-progress status
- `Complete(key, result)` - Mark key as completed with cached result
- `Release(key)` - Remove key to allow retry after failure

### downloader.go

- `Download(ctx, url)` / `DownloadWithProgress(ctx, url, progressCb)` - Download video with yt-dlp
- `GetVideoCodec(path)` - Get video codec via ffprobe
- `GetAudioCodec(path)` - Get audio codec via ffprobe
- `GetPixelFormat(path)` - Get pixel format via ffprobe
- `IsH264Compatible(codec)` - Check if video codec is H.264
- `IsAACCompatible(codec)` - Check if audio codec is AAC
- `Is420p(pixFmt)` - Check if pixel format is 4:2:0 8-bit
- `CanStreamCopy(videoCodec, audioCodec, pixFmt)` - Check if codecs allow -c copy splitting
- `ReencodeToH264(input, output, progressCb)` - Convert to H.264
- `NeedsSplit(path)` - Check if file >1.9GB (`MaxUploadSize`)
- `CalculateNumParts(fileSize)` - Calculate split parts using 1.7GB target (`MaxSplitSize`)
- `SplitVideo(path, outputDir, progressCb)` - Codec-aware split (stream copy or re-encode)

### bot.go

- `processURL()` - Download via engine + upload via telebot
- `processPlaylist()` - Playlist processing via engine
- `updateProgress()` - Rate-limited status updates

### upload/retry.go

- `SendWithRetry(bot, to, what, opts)` - Send with 429/FloodError retry (max 3)

## Progress Phases

```go
type Progress struct {
    Phase       string   // "queued", "downloading", "merging", "encoding", "splitting", "uploading"
    Percent     float64
    Speed       string
    ETA         string   // For "downloading": yt-dlp ETA. For "queued": remaining wait duration.
    Total       string
    Downloaded  string
    PartNum     int      // Current part (for splitting/uploading)
    TotalParts  int
    Codec       string   // Original codec when encoding
}
```

The `"queued"` phase is emitted by the Instagram rate limiter (`waitForIGSlot`)
when a download is delayed waiting for the inter-request gap to elapse. See
"Instagram rate limiting" below.

## Common Tasks

### Add support for new site

yt-dlp supports 1000+ sites. No code changes needed unless site requires special handling.

### Change video quality limit

Edit format string in `downloader.go`:
```go
"-f", "bestvideo[vcodec^=avc1][height<=1080]..."  // Change 1080 to desired height
```

### Change split threshold

Two constants control splitting in `downloader.go`:
- `MaxUploadSize` (1.9GB) — threshold for whether to split at all
- `MaxSplitSize` (1.7GB) — target part size (with 200MB keyframe overshoot margin for `-c copy`)

```go
MaxUploadSize = 1900 * 1024 * 1024  // 1.9GB - split trigger threshold
MaxSplitSize  = 1700 * 1024 * 1024  // 1.7GB - split target size per part
```

### Debug locally

```bash
# Set environment
export TELEGRAM_BOT_TOKEN=<token>

# Run with local Telegram servers (50MB limit)
go run cmd/sushe/main.go

# Or build and run
make build
./bin/sushe
```

## Dependencies

- Go 1.23+
- yt-dlp (on server)
- ffmpeg/ffprobe (on server)
- telegram-bot-api server (on server, for >50MB uploads)

## Operator Access

This section describes restricted access for an AI developer agent working on this bot.

### Server

- **Host**: provided separately (not stored in repo)
- **User**: `sushe`
- **SSH alias**: `sushe` (configured in `~/.ssh/config`)
- **SSH key**: `~/.ssh/sushe-operator`

### Paths

| Path | Description |
|------|-------------|
| `/home/sushe/sushe/bin/sushe` | Bot binary |
| `/home/sushe/sushe/bin/yt-dlp` | Checksum-pinned service yt-dlp binary |
| `/tmp/sushe/` | Temp directory for downloads/encoding |
| `/usr/local/bin/yt-dlp` | System yt-dlp fallback when `SUSHE_YTDLP` is empty |

### Systemd Services

| Service | Description |
|---------|-------------|
| `sushe.service` | The bot itself |
| `telegram-bot-api.service` | Local Telegram Bot API server (2GB upload support) |

### Permissions

**Allowed:**
- `sudo systemctl restart sushe` — restart the bot after deploy
- `sudo systemctl status sushe` — check bot status
- `sudo systemctl status telegram-bot-api` — check API server status
- `sudo sushe-logs` — view bot logs (`journalctl -u sushe`)
- `sudo sushe-api-logs` — view API server logs (`journalctl -u telegram-bot-api`)
- `sudo sushe-update-ytdlp` — update yt-dlp to latest version

**Forbidden:**
- `systemctl restart telegram-bot-api` — do NOT restart the API server
- Modifying systemd unit files
- Accessing Telegram secrets (bot token, API ID, API hash)
- Installing system packages (`apt install`, etc.)

### Setup for Operator

1. **SSH config** — add to `~/.ssh/config`:
   ```
   Host sushe
       HostName <SERVER_IP>
       User sushe
       IdentityFile ~/.ssh/sushe-operator
   ```

2. **`.env` file** — create `.env` in the project root with:
   ```
   SERVER="<SERVER_IP>"
   SSH_HOST="sushe"
   REMOTE_USER="sushe"
   ```
   Note: `TELEGRAM_BOT_TOKEN`, `TELEGRAM_API_ID`, `TELEGRAM_API_HASH` are NOT needed for deploy — they are already on the server.

3. **Workflow:**
   - `make build` — cross-compile the bot binary
   - `make update` — build + scp + restart; works for both admin and restricted service-user SSH logins
   - `make update-ytdlp` — install the pinned nightly, update the remote application `.env`, and restart only Sushe
   - `make verify` — check service status, recent logs, live yt-dlp path, and pinned version

### Pinned yt-dlp nightly

`make update-ytdlp` downloads the official Linux standalone nightly
`2026.08.17.073947`, verifies its hard-coded SHA-256 before touching the
server, and atomically installs it as `/home/sushe/sushe/bin/yt-dlp`. The
script preserves every unrelated line in the remote application `.env` while
setting `SUSHE_YTDLP=/home/sushe/sushe/bin/yt-dlp`, restarts only
`sushe.service`, and verifies the exact binary version plus the restarted
application's configured yt-dlp path from its startup log.

The script supports the restricted `sushe` login without privileged file
operations; its only sudo command in that mode is the allowlisted
`systemctl restart sushe`. It also retains an admin-login path for initial
operator setup. To change the pin, update the version, release URL, checksum,
and expected version in `scripts/install-ytdlp.sh` and `scripts/verify.sh`
together.

### Cookies for authenticated downloads

Some platforms (notably Instagram) refuse to serve anonymous traffic and respond with `rate-limit reached or login required`, `HTTP 429`, or `Login required` to yt-dlp. In production, ~85% of Instagram failures fall into this auth/rate-limit class. Authenticated cookies bypass these.

When the `SUSHE_COOKIES` env var is set on the server, the bot passes `--cookies <path>` to every yt-dlp invocation (single video, playlist info, playlist item). When unset, behavior is unchanged.

**File location on the server:**
- Path: `/home/sushe/.config/sushe/cookies.txt`
- Mode: `0600`
- Owner: `sushe`
- Format: Netscape-format cookies (export from a logged-in browser session)

**Important:** the unit hardens `/home/` as read-only via `ProtectHome=read-only` plus a narrow `ReadWritePaths=` list. yt-dlp opens the cookies file for read at startup AND writes updated session cookies back at exit — so the cookies directory MUST be in `ReadWritePaths` or yt-dlp crashes after a successful download with `OSError: [Errno 30] Read-only file system`. `scripts/deploy.sh` writes the unit with both `Environment=SUSHE_COOKIES=...` and `ReadWritePaths=.../.config/sushe` inline, so this comes for free with `make deploy`.

**One-time / re-deploy admin setup:**

`make deploy` (i.e. `scripts/deploy.sh`) handles the full setup, including:
- creating `/home/sushe/.config/sushe/` (mode 0700, owner sushe)
- transferring `www.instagram.com_cookies.txt` from the repo root if it exists locally (mode 0600)
- writing `/etc/systemd/system/sushe.service` with `Environment=SUSHE_COOKIES=...` and `ReadWritePaths=.../.config/sushe` inline
- removing any obsolete `cookies.conf` drop-in left over from earlier flows
- `daemon-reload` + restart

`scripts/deploy.sh` runs sudo commands on the server and must be invoked from a machine whose SSH user has unrestricted sudo there (NOT the `sushe` operator user — that one only has the narrow allowlist above).

**Routine operator workflow (cookies refresh when session expires):**

Instagram cookies typically last weeks-to-months. When the session expires the bot starts failing with `login required` again. Refresh from the local repo (no sudo on the operator side — only `systemctl restart sushe`, which is in the operator allowlist):

```
./scripts/install-cookies.sh
```

The script uploads the local `www.instagram.com_cookies.txt` to `~/.config/sushe/cookies.txt`, pre-flights `Environment` + `ReadWritePaths` on the server (aborts with a helpful message pointing at `make deploy` if the unit is missing the required directives), restarts via the allowlisted `sudo systemctl restart sushe`, and verifies the live process picked up the new env via `/proc/<MainPID>/environ`.

**Hygiene:** use a dedicated Instagram account for the bot (not your personal one) to avoid the main account being flagged for unusual access patterns.

**Do NOT** put `SUSHE_COOKIES` in `.env` — it is server-side config written into the systemd unit by `scripts/deploy.sh`. `.env` is for local-machine deploy config only.

### Instagram rate limiting

Cookies alone are not enough — Instagram also flags bursty request patterns
regardless of auth state. The bot adds two layers of anti-flag posture:

**Per-invocation throttling (`throttleArgs` in `downloader.go`)** — passed to
every yt-dlp call:

- `--sleep-requests 2`, `--sleep-interval 2`, `--max-sleep-interval 5` — slow
  the request rate within a single yt-dlp invocation.
- `--retries 3`, `--fragment-retries 3` — lowered from yt-dlp's default of 10
  to avoid retry storms after a rate-limit response (which trigger harder bans).
- `--socket-timeout 30` — bound a stuck request so it doesn't tie up the gate.
- `--user-agent Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:150.0) Gecko/20100101 Firefox/150.0`
  — matches the desktop Firefox browser where cookies are exported. yt-dlp's
  default UA is stale Chrome 95 — a flag on its own AND a mismatch with
  Firefox-harvested cookies.

> **Invariant:** the UA pinned in `throttleArgs` MUST match the browser used
> to export the cookies file. Cookie+UA mismatch is itself a flag. When you
> upgrade the browser used to harvest cookies, update the UA string in lockstep.

**Process-wide rate limiter (`waitForIGSlot` in `downloader.go`)** — Instagram
flags concurrent bursts hardest, so a process-wide mutex enforces a minimum
gap between IG-bound yt-dlp invocations across all goroutines:

- `minIGGap = 8 * time.Second` — the gap. Tune up if flagging persists, down
  if users complain about latency.
- Host match uses `Hostname() == "instagram.com" || strings.HasSuffix(host, ".instagram.com")`
  (exact or suffix-with-dot — catches `www`/`m.instagram.com`, excludes
  confusables like `evilinstagram.com`).
- The gate stamps `igLastAt` to the projected wake time inside the lock
  BEFORE sleeping, so concurrent callers queue minIGGap further out and
  retry storms after ctx cancellation still respect the gap.
- The callback is invoked outside the lock so a slow Telegram edit by one
  user doesn't block other goroutines waiting for the slot.
- Gated call sites: `DownloadWithProgress` (single video) and
  `DownloadPlaylistVideo` (per item). `GetPlaylistInfo` is NOT gated — it
  always precedes the actual download (which IS gated), and gating both
  would double-charge every IG URL.

**`queued` progress phase** — when the gate has to wait, a single
`Progress{Phase: "queued", ETA: <remaining>}` event is emitted via the
callback before the sleep. Bot UI renders `Waiting for Instagram rate limit
(~7s)...`; HTTP API streams `{"status":"queued","eta":"7s"}` as an NDJSON line.
