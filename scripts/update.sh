#!/bin/bash
# Update sushe to latest version
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"

source "$REPO_DIR/.env"

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
scp bin/sushe "${REMOTE_USER}@${SERVER}:~/sushe/bin/sushe.new"

echo "Swapping binary and restarting..."
ssh "${REMOTE_USER}@${SERVER}" "cd ~/sushe && mv bin/sushe.new bin/sushe && chmod +x bin/sushe"
ssh "$SSH_HOST" "sudo systemctl restart sushe"

sleep 2

echo "Verifying..."
ssh "$SSH_HOST" "systemctl is-active telegram-bot-api && echo 'Bot API running'"
ssh "$SSH_HOST" "systemctl is-active sushe && echo 'Sushe running'"

echo "Update complete!"
