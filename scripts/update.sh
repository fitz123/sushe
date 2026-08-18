#!/bin/bash
# Update sushe to latest version
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"

# shellcheck source=/dev/null
source "$REPO_DIR/.env"

# Service user on the remote server (where the binary actually lives). The
# SSH login user (SSH_HOST) may be a different admin account with sudo, so
# we cannot scp directly into /home/$REMOTE_USER/.
REMOTE_USER="${REMOTE_USER:-sushe}"
REMOTE_BIN_DIR="/home/$REMOTE_USER/sushe/bin"

run_remote() {
    local remote_command
    printf -v remote_command '%q ' "$@"
    # shellcheck disable=SC2029 # printf %q has quoted each remote argument.
    ssh "$SSH_HOST" "$remote_command"
}

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
TMP_BIN="$(run_remote mktemp /tmp/sushe-update-XXXXXX)"
# Local trap so the remote temp file is cleaned up even if a later step fails.
cleanup() {
    run_remote rm -f -- "$TMP_BIN" 2>/dev/null || true
}
trap cleanup EXIT
scp bin/sushe "$SSH_HOST:$TMP_BIN"

SSH_USER="$(run_remote id -un)"

echo "Pre-flight check..."
if [[ "$SSH_USER" == "$REMOTE_USER" ]]; then
    run_remote test -x "$REMOTE_BIN_DIR/telegram-bot-api" \
        || { echo "ERROR: telegram-bot-api binary missing on server! Run 'make deploy' to restore it."; exit 1; }
else
    # Admin-login mode retains the sudo pre-flight needed when the admin
    # cannot traverse the service user's home directory.
    run_remote sudo test -x "$REMOTE_BIN_DIR/telegram-bot-api" \
        || { echo "ERROR: telegram-bot-api binary missing on server! Run 'make deploy' to restore it."; exit 1; }
fi

echo "Installing binary and restarting..."
if [[ "$SSH_USER" == "$REMOTE_USER" ]]; then
    # Restricted service-user mode: install in the user-owned directory and
    # use no sudo except the allowlisted Sushe restart below.
    run_remote bash -s -- "$TMP_BIN" "$REMOTE_BIN_DIR/sushe" <<'REMOTE'
set -euo pipefail
source_path=$1
target_path=$2
stage_path="$(mktemp "${target_path}.XXXXXX")"
trap 'rm -f "$source_path" "$stage_path"' EXIT
install -m 0755 "$source_path" "$stage_path"
mv -f "$stage_path" "$target_path"
REMOTE
else
    # Existing admin-login mode: sudo installs with service-user ownership.
    run_remote sudo install -o "$REMOTE_USER" -g "$REMOTE_USER" -m 0755 "$TMP_BIN" "$REMOTE_BIN_DIR/sushe"
fi
run_remote sudo systemctl restart sushe

sleep 2

echo "Verifying..."
run_remote systemctl is-active telegram-bot-api
echo "Bot API running"
run_remote systemctl is-active sushe
echo "Sushe running"

echo "Update complete!"
