#!/usr/bin/env bash
# Install the pinned yt-dlp nightly used by the Sushe service.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"

# shellcheck source=/dev/null
source "$REPO_DIR/.env"

: "${SSH_HOST:?SSH_HOST not set in .env}"

YTDLP_VERSION="2026.08.17.073947"
YTDLP_URL="https://github.com/yt-dlp/yt-dlp-nightly-builds/releases/download/${YTDLP_VERSION}/yt-dlp_linux"
YTDLP_SHA256="7de6f20acccb99d4f926a5e2665bb13f5074b74225c3c428dbd80ac3df53de31"
SERVICE_USER="${REMOTE_USER:-sushe}"
REMOTE_APP_DIR="/home/$SERVICE_USER/sushe"
REMOTE_YTDLP_PATH="$REMOTE_APP_DIR/bin/yt-dlp"
REMOTE_ENV_PATH="$REMOTE_APP_DIR/.env"

if [[ ! "$SERVICE_USER" =~ ^[a-z_][a-z0-9_-]*[$]?$ ]]; then
    echo "ERROR: invalid REMOTE_USER: $SERVICE_USER" >&2
    exit 1
fi

run_remote() {
    local remote_command
    printf -v remote_command '%q ' "$@"
    # shellcheck disable=SC2029 # printf %q has quoted each remote argument.
    ssh "$SSH_HOST" "$remote_command"
}

LOCAL_YTDLP="$(mktemp "${TMPDIR:-/tmp}/sushe-ytdlp.XXXXXX")"
REMOTE_UPLOAD=""
cleanup() {
    rm -f "$LOCAL_YTDLP"
    if [[ -n "$REMOTE_UPLOAD" ]]; then
        run_remote rm -f -- "$REMOTE_UPLOAD" 2>/dev/null || true
    fi
}
trap cleanup EXIT

echo "==> Downloading pinned yt-dlp nightly $YTDLP_VERSION"
curl --fail --location --silent --show-error "$YTDLP_URL" --output "$LOCAL_YTDLP"

if command -v sha256sum >/dev/null 2>&1; then
    ACTUAL_SHA256="$(sha256sum "$LOCAL_YTDLP" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
    ACTUAL_SHA256="$(shasum -a 256 "$LOCAL_YTDLP" | awk '{print $1}')"
else
    echo "ERROR: sha256sum or shasum is required" >&2
    exit 1
fi

if [[ "$ACTUAL_SHA256" != "$YTDLP_SHA256" ]]; then
    printf 'ERROR: yt-dlp SHA-256 mismatch\n  expected: %s\n  actual:   %s\n' \
        "$YTDLP_SHA256" "$ACTUAL_SHA256" >&2
    exit 1
fi
echo "    SHA-256 verified: $YTDLP_SHA256"

# No remote filesystem mutation occurs before the pinned download passes the
# hard-coded checksum above.
SSH_USER="$(run_remote id -un)"
run_remote_as_service_owner() {
    if [[ "$SSH_USER" == "$SERVICE_USER" ]]; then
        run_remote "$@"
    else
        run_remote sudo "$@"
    fi
}
run_remote_as_service_owner test -f "$REMOTE_ENV_PATH" || {
    echo "ERROR: remote environment file not found: $REMOTE_ENV_PATH" >&2
    exit 1
}

echo "==> Uploading verified binary (ssh user: $SSH_USER, service user: $SERVICE_USER)"
REMOTE_UPLOAD="$(run_remote mktemp /tmp/sushe-ytdlp-XXXXXX)"
scp "$LOCAL_YTDLP" "$SSH_HOST:$REMOTE_UPLOAD"

# The helper stages both files beside their final destinations and renames
# them into place. It reads the existing .env remotely and never places its
# contents in command arguments or output.
run_remote_as_service_owner bash -s -- \
    "$REMOTE_UPLOAD" \
    "$REMOTE_APP_DIR" \
    "$REMOTE_YTDLP_PATH" \
    "$REMOTE_ENV_PATH" \
    "$YTDLP_VERSION" \
    "$SERVICE_USER" <<'REMOTE'
set -euo pipefail

uploaded=$1
app_dir=$2
target=$3
env_path=$4
expected_version=$5
service_user=$6
binary_stage=""
env_stage=""
cleanup_remote() {
    rm -f "$uploaded"
    [[ -z "$binary_stage" ]] || rm -f "$binary_stage"
    [[ -z "$env_stage" ]] || rm -f "$env_stage"
}
trap cleanup_remote EXIT

test -f "$env_path"
chmod 0700 "$uploaded"
actual_version="$("$uploaded" --version)"
if [[ "$actual_version" != "$expected_version" ]]; then
    printf 'ERROR: uploaded yt-dlp version is %s, expected %s\n' \
        "$actual_version" "$expected_version" >&2
    exit 1
fi

if [[ "$(id -u)" -eq 0 ]]; then
    install -d -o "$service_user" -g "$service_user" -m 0755 "$app_dir/bin"
else
    install -d -m 0755 "$app_dir/bin"
fi
binary_stage="$(mktemp "$app_dir/bin/.yt-dlp.XXXXXX")"
env_stage="$(mktemp "$app_dir/.env.XXXXXX")"

if [[ "$(id -u)" -eq 0 ]]; then
    install -o "$service_user" -g "$service_user" -m 0755 "$uploaded" "$binary_stage"
else
    install -m 0755 "$uploaded" "$binary_stage"
fi

awk -v ytdlp_path="$target" '
BEGIN { updated = 0 }
{
    if ($0 ~ /^[[:space:]]*(export[[:space:]]+)?SUSHE_YTDLP[[:space:]]*=/) {
        if (!updated) {
            print "SUSHE_YTDLP=" ytdlp_path
        }
        updated = 1
        next
    }
    print
}
END {
    if (!updated) {
        print "SUSHE_YTDLP=" ytdlp_path
    }
}
' "$env_path" > "$env_stage"
chmod --reference="$env_path" "$env_stage"
if [[ "$(id -u)" -eq 0 ]]; then
    chown --reference="$env_path" "$env_stage"
fi

mv -f "$binary_stage" "$target"
binary_stage=""
mv -f "$env_stage" "$env_path"
env_stage=""
printf '    Installed %s at %s\n' "$actual_version" "$target"
REMOTE
REMOTE_UPLOAD=""

REMOTE_CONFIG="$(run_remote_as_service_owner grep -Fx \
    "SUSHE_YTDLP=$REMOTE_YTDLP_PATH" "$REMOTE_ENV_PATH")"
RESTART_STARTED_AT="$(run_remote date --iso-8601=seconds)"
echo "==> Restarting sushe"
run_remote sudo systemctl restart sushe
sleep 1

echo "==> Verifying installed version and live application configuration"
REMOTE_VERSION="$(run_remote_as_service_owner "$REMOTE_YTDLP_PATH" --version)"
if [[ "$REMOTE_VERSION" != "$YTDLP_VERSION" ]]; then
    printf 'ERROR: installed yt-dlp version is %s, expected %s\n' \
        "$REMOTE_VERSION" "$YTDLP_VERSION" >&2
    exit 1
fi

if [[ "$SSH_USER" == "$SERVICE_USER" ]]; then
    LIVE_CONFIG="$(run_remote sudo sushe-logs --since "$RESTART_STARTED_AT" \
        --grep 'yt-dlp executable configured' -n 1 --no-pager \
        | grep -F "path=$REMOTE_YTDLP_PATH")"
else
    LIVE_CONFIG="$(run_remote sudo journalctl -u sushe --since "$RESTART_STARTED_AT" \
        --grep 'yt-dlp executable configured' -n 1 --no-pager \
        | grep -F "path=$REMOTE_YTDLP_PATH")"
fi
run_remote systemctl is-active --quiet sushe

echo "    Remote config: $REMOTE_CONFIG"
echo "    Version: $REMOTE_VERSION"
echo "    Live application: $LIVE_CONFIG"
echo "==> Pinned yt-dlp update complete"
