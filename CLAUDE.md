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
│   ├── sushe/main.go           # Bot entry point
│   └── test-split/main.go      # Test utility for video splitting
├── internal/
│   ├── bot/bot.go              # Telegram handlers, progress updates, uploads
│   ├── downloader/downloader.go # yt-dlp wrapper, ffprobe, ffmpeg, splitting
│   └── logger/logger.go        # Structured logging with slog
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

1. **Telegram Bot** (`cmd/sushe/main.go`)
   - Uses `gopkg.in/telebot.v3`
   - Connects to local Telegram Bot API server for 2GB upload support
   - Handles URL detection and video processing

2. **Bot Handlers** (`internal/bot/bot.go`)
   - URL regex matching for video links
   - Real-time progress updates (download, encode, split, upload phases)
   - Multi-part upload with threaded replies
   - `ProgressReader` for upload progress tracking

3. **Downloader** (`internal/downloader/downloader.go`)
   - yt-dlp wrapper with format selection preferring H.264
   - Codec detection via ffprobe
   - Conditional re-encoding (VP9/AV1 → H.264) via ffmpeg
   - Video splitting for files >1.9GB

### Video Processing Flow

```
URL → yt-dlp download → Check codec → Re-encode if needed → Split if >1.9GB → Upload to Telegram
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
TELEGRAM_BOT_TOKEN=your_bot_token
TELEGRAM_API_ID=your_api_id
TELEGRAM_API_HASH=your_api_hash
SSH_PUBLIC_KEY=your_ssh_public_key
```

## Key Functions

### downloader.go

- `Download(url, outputDir, progressCb)` - Download video with yt-dlp
- `GetVideoCodec(path)` - Get codec via ffprobe
- `IsH264Compatible(codec)` - Check if codec needs re-encoding
- `ReencodeToH264(input, output, progressCb)` - Convert to H.264
- `NeedsSplit(path)` - Check if file >1.9GB
- `SplitVideo(path, outputDir, progressCb)` - Split into parts

### bot.go

- `handleMessage()` - Main URL handler
- `updateProgress()` - Rate-limited status updates
- `ProgressReader` - io.Reader wrapper for upload progress

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
export TELEGRAM_BOT_TOKEN=your_token

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
