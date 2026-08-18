#!/bin/bash
# Verify sushe deployment
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"

# shellcheck source=/dev/null
source "$REPO_DIR/.env"

SERVICE_USER="${REMOTE_USER:-sushe}"
EXPECTED_YTDLP_VERSION="2026.08.17.073947"
REMOTE_APP_DIR="/home/$SERVICE_USER/sushe"
REMOTE_YTDLP_PATH="$REMOTE_APP_DIR/bin/yt-dlp"
REMOTE_ENV_PATH="$REMOTE_APP_DIR/.env"
SSH_USER="$(ssh "$SSH_HOST" 'id -un')"
if [[ "$SSH_USER" == "$SERVICE_USER" ]]; then
    PRIVILEGE_PREFIX=()
else
    PRIVILEGE_PREFIX=(sudo)
fi

run_remote() {
    local remote_command
    printf -v remote_command '%q ' "$@"
    # shellcheck disable=SC2029 # printf %q has quoted each remote argument.
    ssh "$SSH_HOST" "$remote_command"
}

echo "=== Telegram Bot API Server ==="
ssh "$SSH_HOST" "sudo systemctl status telegram-bot-api --no-pager | head -15"

echo ""
echo "=== Sushe Bot ==="
ssh "$SSH_HOST" "sudo systemctl status sushe --no-pager | head -15"

echo ""
echo "=== Recent Sushe logs ==="
if [[ "$SSH_USER" == "$SERVICE_USER" ]]; then
    ssh "$SSH_HOST" "sudo sushe-logs -n 20 --no-pager"
else
    ssh "$SSH_HOST" "sudo journalctl -u sushe -n 20 --no-pager"
fi

echo ""
echo "=== Sushe yt-dlp runtime ==="
REMOTE_CONFIG="$(run_remote "${PRIVILEGE_PREFIX[@]}" grep -Fx \
    "SUSHE_YTDLP=$REMOTE_YTDLP_PATH" "$REMOTE_ENV_PATH")"
STARTED_AT="$(run_remote systemctl show sushe -p ExecMainStartTimestamp --value)"
if [[ "$SSH_USER" == "$SERVICE_USER" ]]; then
    LIVE_CONFIG="$(run_remote sudo sushe-logs --since "$STARTED_AT" \
        --grep 'yt-dlp executable configured' -n 1 --no-pager \
        | grep -F "path=$REMOTE_YTDLP_PATH")"
else
    LIVE_CONFIG="$(run_remote sudo journalctl -u sushe --since "$STARTED_AT" \
        --grep 'yt-dlp executable configured' -n 1 --no-pager \
        | grep -F "path=$REMOTE_YTDLP_PATH")"
fi

YTDLP_VERSION="$(run_remote "${PRIVILEGE_PREFIX[@]}" "$REMOTE_YTDLP_PATH" --version)"
echo "Remote config: $REMOTE_CONFIG"
echo "Live application: $LIVE_CONFIG"
echo "Version: $YTDLP_VERSION"
if [[ "$YTDLP_VERSION" != "$EXPECTED_YTDLP_VERSION" ]]; then
    echo "ERROR: expected yt-dlp $EXPECTED_YTDLP_VERSION" >&2
    exit 1
fi
