#!/usr/bin/env bash
# install-cookies.sh — operator-side refresh of yt-dlp cookies on the bot server.
#
# This script does NOT install the systemd drop-in. The drop-in is a one-time
# admin step that requires unrestricted sudo (which the `sushe` operator user
# does NOT have). See "ONE-TIME ADMIN STEP" at the bottom of this file.
#
# What this script does (operator-only, narrow sudo):
#   1. Validate local cookies file exists
#   2. Upload to ~/.config/sushe/cookies.txt (mode 0600, owner sushe)
#   3. Verify the systemd drop-in is already installed (Environment includes
#      SUSHE_COOKIES) — fails with helpful instructions if not
#   4. sudo systemctl restart sushe (this command IS in the operator allowlist)
#   5. Verify the live process picked up SUSHE_COOKIES via /proc/<MainPID>/environ
#      (works without sudo because the service runs as the sushe user)
#
# Idempotent: re-run on cookie session expiry to refresh the file and restart.
#
# Usage:
#   ./scripts/install-cookies.sh                       # uses instagram-cookies.txt
#   ./scripts/install-cookies.sh path/to/cookies.txt   # explicit source path

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"

# shellcheck source=/dev/null
source "$REPO_DIR/.env"

: "${SSH_HOST:?SSH_HOST not set in .env}"

LOCAL_COOKIES="${1:-$REPO_DIR/instagram-cookies.txt}"
REMOTE_COOKIES_PATH="/home/sushe/.config/sushe/cookies.txt"

# Step 1: validate local cookies file
if [[ ! -f "$LOCAL_COOKIES" ]]; then
    echo "ERROR: cookies file not found: $LOCAL_COOKIES" >&2
    exit 1
fi
echo "==> Local cookies file: $LOCAL_COOKIES ($(wc -c <"$LOCAL_COOKIES") bytes)"

# Step 2: upload cookies file (no sudo)
echo "==> Step 2: upload cookies file"
ssh "$SSH_HOST" "mkdir -p ~/.config/sushe && chmod 700 ~/.config/sushe"
scp "$LOCAL_COOKIES" "$SSH_HOST:.config/sushe/cookies.txt"
ssh "$SSH_HOST" "chmod 600 $REMOTE_COOKIES_PATH && ls -la $REMOTE_COOKIES_PATH"

# Step 3: pre-flight — verify drop-in is already installed (BEFORE restart)
echo "==> Step 3: verify systemd drop-in is installed"
MERGED_ENV="$(ssh "$SSH_HOST" "systemctl show sushe -p Environment --value")"
if ! grep -q "SUSHE_COOKIES=$REMOTE_COOKIES_PATH" <<<"$MERGED_ENV"; then
    # Use printf with explicit %s so the inline heredoc syntax in the printed
    # admin instructions stays literal. Avoids confusion about variable
    # expansion across the outer (printing) heredoc and the inner (instructional)
    # heredoc — the printed text shows exactly what the admin should paste.
    printf >&2 'ERROR: systemd drop-in not installed. Merged unit Environment is missing\n'
    printf >&2 'SUSHE_COOKIES=%s.\n\n' "$REMOTE_COOKIES_PATH"
    printf >&2 'The drop-in install is a ONE-TIME ADMIN STEP that requires unrestricted sudo\n'
    printf >&2 '(the sushe operator only has narrow sudo for systemctl restart/status). Ask\n'
    printf >&2 'the server admin to run, AS ROOT on the server:\n\n'
    printf >&2 '    mkdir -p /etc/systemd/system/sushe.service.d\n'
    printf >&2 "    cat > /etc/systemd/system/sushe.service.d/cookies.conf <<'CONF'\n"
    printf >&2 '    [Service]\n'
    printf >&2 '    Environment=SUSHE_COOKIES=%s\n' "$REMOTE_COOKIES_PATH"
    printf >&2 '    CONF\n'
    printf >&2 '    chmod 0644 /etc/systemd/system/sushe.service.d/cookies.conf\n'
    printf >&2 '    systemctl daemon-reload\n'
    printf >&2 '    # Verify before restart:\n'
    printf >&2 '    systemctl show sushe -p Environment | grep SUSHE_COOKIES\n'
    printf >&2 '    # Then re-run this script as the sushe operator.\n\n'
    printf >&2 'After the admin completes those steps, re-run %s as the sushe operator.\n' "$0"
    printf >&2 'The operator path (cookies refresh + restart + verify) does not need root.\n\n'
    printf >&2 -- '--- current merged Environment ---\n%s\n' "$MERGED_ENV"
    exit 1
fi
echo "    OK — merged unit has SUSHE_COOKIES=$REMOTE_COOKIES_PATH"

# Step 4: restart (sudo systemctl restart sushe — IS in operator allowlist)
echo "==> Step 4: sudo systemctl restart sushe"
ssh "$SSH_HOST" "sudo systemctl restart sushe"

# Step 5: verify live process env (no sudo: service runs as sushe, so the
# operator can read /proc/<MainPID>/environ for its own service)
echo "==> Step 5: verify live process /proc/<MainPID>/environ contains SUSHE_COOKIES"
sleep 1  # let systemd respawn the process so MainPID points at the new instance
LIVE_ENV="$(ssh "$SSH_HOST" "PID=\$(systemctl show sushe -p MainPID --value); test \"\$PID\" != 0 && cat /proc/\$PID/environ | tr '\0' '\n' | grep '^SUSHE_COOKIES='" || true)"
if [[ -z "$LIVE_ENV" ]]; then
    echo "ERROR: could not verify live process picked up SUSHE_COOKIES." >&2
    echo "Possible causes: service failed to start, MainPID==0, or the process runs as a non-sushe user." >&2
    echo "--- last 30 lines of journal ---" >&2
    ssh "$SSH_HOST" "sudo sushe-logs -n 30 --no-pager" >&2 || true
    exit 1
fi
echo "    OK — live process: $LIVE_ENV"

echo
echo "==> Cookies refresh complete. Smoke-test by sending a previously-failing Instagram URL to the bot."

# ============================================================================
# ONE-TIME ADMIN STEP (NOT performed by this script):
#
# The systemd drop-in must be installed once by a user with unrestricted sudo
# (NOT the sushe operator). Run on the server, as root or via someone with full
# sudo:
#
#     mkdir -p /etc/systemd/system/sushe.service.d
#     cat > /etc/systemd/system/sushe.service.d/cookies.conf <<'CONF'
#     [Service]
#     Environment=SUSHE_COOKIES=/home/sushe/.config/sushe/cookies.txt
#     CONF
#     chmod 0644 /etc/systemd/system/sushe.service.d/cookies.conf
#     systemctl daemon-reload
#     # Verify BEFORE restarting (Config Change Safety):
#     systemctl show sushe -p Environment | grep SUSHE_COOKIES
#     # Restart only after the verify line printed the expected value:
#     systemctl restart sushe
#
# Once that's done once, this script handles all routine refreshes (when the
# Instagram session expires and a new cookies.txt needs to be deployed).
# ============================================================================
