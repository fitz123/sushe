#!/bin/bash
# Verify sushe deployment
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"

source "$REPO_DIR/.env"

echo "=== Telegram Bot API Server ==="
ssh "$SSH_HOST" "sudo systemctl status telegram-bot-api --no-pager | head -15"

echo ""
echo "=== Sushe Bot ==="
ssh "$SSH_HOST" "sudo systemctl status sushe --no-pager | head -15"

echo ""
echo "=== Recent Sushe logs ==="
ssh "$SSH_HOST" "sudo journalctl -u sushe -n 20 --no-pager"

echo ""
echo "=== yt-dlp version ==="
ssh "$SSH_HOST" "yt-dlp --version"
