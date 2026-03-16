# Plan: Group Chat & Forum Topic Support for Sushe Bot

## Goal
Enable sushe bot (@spushelbot) to work in Telegram group chats with forum topics. Users invoke the bot via `/dl` command with a URL. Privacy mode stays ON — bot only receives commands, not all messages.

## Context
- Repo: github.com/fitz123/sushe (Go, telebot.v3 v3.3.8)
- Branch: feat/group-forum-support
- Root cause: Telegram privacy mode ON = bot receives only `/command@botname`, NOT @mentions
- Bot uses local Telegram Bot API server (localhost:8081) for 2GB uploads
- Group: Minime HQ (supergroup with forum topics, is_forum: true)
- Key files: `internal/bot/bot.go`, `internal/bot/auth.go`, `cmd/sushe/main.go`

## Constraints
- Privacy mode must stay ON (bot should NOT receive all group messages)
- Backward compatibility: existing DM flow (paste URL → bot downloads) must keep working
- Forum topics: all bot replies must go to the same topic where the command was sent
- Auth: allowed_users.txt fallback must work (sushe user has no sudo to edit systemd env)

## Validation Commands
```bash
# Build
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags '-s -w' -o bin/sushe cmd/sushe/main.go

# Vet
go vet ./...
```

## Tasks

### Task 1: Add `/dl` command handler
- [ ] Add `/dl` handler in `registerHandlers()` that extracts URL(s) from the command arguments
- [ ] Handler should parse `c.Message().Payload` (telebot puts everything after the command there)
- [ ] If payload is empty or has no URLs, send usage hint: "Usage: /dl <video_url>"
- [ ] If URL found, call existing `processURL(c, url)` 
- [ ] In group chats, silently ignore empty /dl (no spam)

### Task 2: Forum topic support (ThreadID propagation)
- [ ] Add helper function `threadOpts(c tele.Context, extra ...*tele.SendOptions) *tele.SendOptions` that reads `c.Message().ThreadID` and sets it on SendOptions
- [ ] Add helper `sendInThread(c, what, opts)` that wraps `bs.bot.Send(c.Chat(), what, threadOpts(c, opts...))`
- [ ] Replace all `bs.bot.Send(c.Chat(), ...)` calls with `bs.sendInThread(c, ...)` to propagate ThreadID
- [ ] This ensures status messages, video uploads, and error messages go to the correct forum topic

### Task 3: Silent in groups for handleText
- [ ] In `handleText`: if chat type is not private AND no URLs found, return nil (don't send "No video URL detected")
- [ ] DM behavior unchanged: still sends help hint in private chats

### Task 4: Auth improvements
- [ ] Keep `allowed_users.txt` fallback in `LoadAllowedUsers()` (reads from working directory when env var not set)
- [ ] Remove any debug logging added during development (the "DEBUG incoming update" lines)
- [ ] Auth middleware should only log unauthorized attempts, not every incoming message

### Task 5: Register /dl command with BotFather via API
- [ ] In bot startup (after NewBot, before Start), call `bot.SetCommands()` to register:
  - `/dl` — "Download video from URL"
  - `/start` — "Start the bot"  
  - `/help` — "Show help"
- [ ] This makes /dl appear in Telegram's command suggestions menu

### Task 6: Update /help text
- [ ] Add group usage instructions to help text:
  - "In groups: /dl <video_url>"
  - "In DMs: just send the URL directly"

### Task 7: Clean up development artifacts  
- [ ] Remove `api_url.txt` override logic from `main.go` (development-only, not needed in production)
- [ ] Remove `strings` import if no longer needed in `main.go`
- [ ] Ensure `AllowedUpdates` in LongPoller is sensible (include "message" at minimum)
