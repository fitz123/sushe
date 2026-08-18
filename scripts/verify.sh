#!/bin/bash
# Verify sushe deployment
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"

# shellcheck source=/dev/null
source "$REPO_DIR/.env"

SERVICE_USER="${REMOTE_USER:-sushe}"
EXPECTED_YTDLP_VERSION="2026.08.17.073947"
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
LIVE_YTDLP="$(run_remote "${PRIVILEGE_PREFIX[@]}" bash -s <<'REMOTE'
set -euo pipefail
pid="$(systemctl show sushe -p MainPID --value)"
test "$pid" != 0
tr '\0' '\n' < "/proc/$pid/environ" | grep '^SUSHE_YTDLP=' || true
REMOTE
)"
if [[ -n "$LIVE_YTDLP" ]]; then
    YTDLP_PATH="${LIVE_YTDLP#SUSHE_YTDLP=}"
    echo "Live process: $LIVE_YTDLP"
else
    YTDLP_PATH="yt-dlp"
    echo "Live process: SUSHE_YTDLP unset (using PATH fallback)"
fi

YTDLP_VERSION="$(run_remote "${PRIVILEGE_PREFIX[@]}" "$YTDLP_PATH" --version)"
echo "Version: $YTDLP_VERSION"
if [[ "$YTDLP_VERSION" != "$EXPECTED_YTDLP_VERSION" ]]; then
    echo "ERROR: expected yt-dlp $EXPECTED_YTDLP_VERSION" >&2
    exit 1
fi
