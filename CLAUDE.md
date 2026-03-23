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
│   ├── downloader/downloader.go # yt-dlp wrapper, ffprobe, ffmpeg, splitting
│   ├── engine/engine.go        # Core download+transcode+split engine (no upload)
│   ├── logger/logger.go        # Structured logging with slog
│   └── upload/retry.go         # SendWithRetry: 429/FloodError retry helper
├── scripts/
│   ├── deploy.sh               # Full server deployment
│   ├── update.sh               # Quick binary update
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
   - Request deduplication by (url, chat_id, thread_id) with 15-minute TTL
   - Streams NDJSON progress events + final result
   - `GET /health` — service health check
   - Uses engine for download, telebot `Send()` for upload, `SendWithRetry` for 429 handling

4. **Bot Handlers** (`internal/bot/bot.go`)
   - `/dl` command + URL auto-detect in messages
   - Real-time progress updates via Telegram message editing
   - Multi-part upload with threaded replies
   - Delegates download to engine, keeps telebot upload logic
   - GENERAL topic guard (ThreadID == 0/1 → warning)

5. **Downloader** (`internal/downloader/downloader.go`)
   - yt-dlp wrapper with format selection preferring H.264
   - Codec detection via ffprobe
   - Conditional re-encoding (VP9/AV1 → H.264) via ffmpeg
   - Video splitting for files >1.9GB

6. **Upload Retry** (`internal/upload/retry.go`)
   - `SendWithRetry()` wraps telebot `Send()` with 429/FloodError handling
   - Max 3 retries, sleeps for `RetryAfter` seconds
   - Used by both bot handlers and HTTP API

### Video Processing Flow

```
URL → Engine.Process() → yt-dlp download → codec check (ffprobe)
    → re-encode if needed (ffmpeg) → split if >1.9GB → ProcessResult
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
{"status":"downloading","percent":45.2}
{"status":"encoding","percent":80.0,"codec":"vp9"}
{"status":"splitting","part":1,"total":3}
{"status":"uploading","part":1,"total":1}
{"status":"done","ok":true,"title":"Video Title","message_id":789,"file_size":123456}
```

**Errors:**
- `401` — missing or invalid bearer token
- `400` — missing `url` or `chat_id`
- `409` — duplicate request already in progress (same url + chat_id + thread_id)
- NDJSON `{"status":"error","ok":false,"error":"..."}` for download/upload failures

**Deduplication:** Requests are deduplicated by (url, chat_id, thread_id). If an identical
request completed within the last 15 minutes, the response contains only the final result
event (no progress events). If an identical request is currently in progress, returns 409.

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

## Key Functions

### engine.go

- `NewEngine()` - Create engine with downloader instance
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

- `Download(url, outputDir, progressCb)` - Download video with yt-dlp
- `GetVideoCodec(path)` - Get codec via ffprobe
- `IsH264Compatible(codec)` - Check if codec needs re-encoding
- `ReencodeToH264(input, output, progressCb)` - Convert to H.264
- `NeedsSplit(path)` - Check if file >1.9GB
- `SplitVideo(path, outputDir, progressCb)` - Split into parts

### bot.go

- `processURL()` - Download via engine + upload via telebot
- `processPlaylist()` - Playlist processing via engine
- `updateProgress()` - Rate-limited status updates

### upload/retry.go

- `SendWithRetry(bot, to, what, opts)` - Send with 429/FloodError retry (max 3)

## Progress Phases

```go
type Progress struct {
    Phase       string   // "downloading", "merging", "encoding", "splitting", "uploading"
    Percent     float64
    Speed       string
    ETA         string
    Total       string
    Downloaded  string
    PartNum     int      // Current part (for splitting/uploading)
    TotalParts  int
    Codec       string   // Original codec when encoding
}
```

## Common Tasks

### Add support for new site

yt-dlp supports 1000+ sites. No code changes needed unless site requires special handling.

### Change video quality limit

Edit format string in `downloader.go`:
```go
"-f", "bestvideo[vcodec^=avc1][height<=1080]..."  // Change 1080 to desired height
```

### Change split threshold

Edit `MaxTelegramFileSize` constant in `downloader.go`:
```go
const MaxTelegramFileSize = 1.9 * 1024 * 1024 * 1024  // 1.9GB
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

- Go 1.21+
- yt-dlp (on server)
- ffmpeg/ffprobe (on server)
- telegram-bot-api server (on server, for >50MB uploads)

## Operator Access

This section describes restricted access for an AI developer agent working on this bot.

### Server

- **Host**: provided separately (not stored in repo)
- **User**: `sushe`
- **SSH alias**: `sushe-bot` (configured in `~/.ssh/config`)
- **SSH key**: `~/.ssh/sushe-operator`

### Paths

| Path | Description |
|------|-------------|
| `/home/sushe/sushe/bin/sushe` | Bot binary |
| `/tmp/sushe/` | Temp directory for downloads/encoding |
| `/usr/local/bin/yt-dlp` | yt-dlp binary |

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
   Host sushe-bot
       HostName <SERVER_IP>
       User sushe
       IdentityFile ~/.ssh/sushe-operator
   ```

2. **`.env` file** — create `.env` in the project root with:
   ```
   SERVER="<SERVER_IP>"
   SSH_HOST="sushe-bot"
   REMOTE_USER="sushe"
   ```
   Note: `TELEGRAM_BOT_TOKEN`, `TELEGRAM_API_ID`, `TELEGRAM_API_HASH` are NOT needed for deploy — they are already on the server.

3. **Workflow:**
   - `make build` — cross-compile the bot binary
   - `make update` — build + scp + restart (uses `.env` for SSH)
   - `make verify` — check service status and recent logs
