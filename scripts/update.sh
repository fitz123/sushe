#!/bin/bash
# Update sushe to latest version
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"

source "$REPO_DIR/.env"

# Service user on the remote server (where the binary actually lives). The
# SSH login user (SSH_HOST) may be a different admin account with sudo, so
# we cannot scp directly into /home/$REMOTE_USER/.
REMOTE_USER="${REMOTE_USER:-sushe}"

echo "Building and deploying update..."

cd "$REPO_DIR"

# Get dependencies
go mod tidy

# Build for Linux (pure Go, no CGO needed)
mkdir -p bin
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags '-s -w' \
    -o bin/sushe cmd/sushe/main.go

echo "Transferring binary..."
# Use remote-side mktemp for unique paths (local $$ can collide across
# operators running concurrent updates from different machines).
TMP_BIN="$(ssh "$SSH_HOST" 'mktemp /tmp/sushe-update-XXXXXX')"
# Local trap so the remote temp file is cleaned up even if a later step fails.
trap 'ssh "$SSH_HOST" "rm -f $TMP_BIN" 2>/dev/null || true' EXIT
scp bin/sushe "$SSH_HOST:$TMP_BIN"

echo "Pre-flight check..."
ssh "$SSH_HOST" "test -x /home/$REMOTE_USER/sushe/bin/telegram-bot-api" \
    || { echo "ERROR: telegram-bot-api binary missing on server! Run 'make deploy' to restore it."; exit 1; }

echo "Installing binary and restarting..."
# sudo install handles the chown+chmod+atomic-rename in one go. Owner and
# group set to REMOTE_USER so the service user owns its own binary even when
# we ssh in as a different admin account.
ssh "$SSH_HOST" "sudo install -o $REMOTE_USER -g $REMOTE_USER -m 0755 $TMP_BIN /home/$REMOTE_USER/sushe/bin/sushe"
ssh "$SSH_HOST" "sudo systemctl restart sushe"

sleep 2

echo "Verifying..."
ssh "$SSH_HOST" "systemctl is-active telegram-bot-api && echo 'Bot API running'"
ssh "$SSH_HOST" "systemctl is-active sushe && echo 'Sushe running'"

echo "Update complete!"
