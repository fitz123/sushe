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
#   ./scripts/install-cookies.sh                       # uses www.instagram.com_cookies.txt
#   ./scripts/install-cookies.sh path/to/cookies.txt   # explicit source path

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"

# shellcheck source=/dev/null
source "$REPO_DIR/.env"

: "${SSH_HOST:?SSH_HOST not set in .env}"

LOCAL_COOKIES="${1:-$REPO_DIR/www.instagram.com_cookies.txt}"
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

# Step 3: pre-flight — verify the merged unit has both required directives
# BEFORE we restart. We check:
#   - merged Environment includes SUSHE_COOKIES (cookies path wired)
#   - merged ReadWritePaths includes the cookies dir (yt-dlp can WRITE the
#     refreshed cookies back; the unit hardens /home as ProtectHome=read-only,
#     so without ReadWritePaths covering the cookies dir, yt-dlp crashes
#     after every successful download with OSError: [Errno 30] Read-only
#     file system)
echo "==> Step 3: verify sushe.service unit has cookies wired"
MERGED_ENV="$(ssh "$SSH_HOST" "systemctl show sushe -p Environment --value")"
MERGED_RWPATHS="$(ssh "$SSH_HOST" "systemctl show sushe -p ReadWritePaths --value")"
COOKIES_DIR="$(dirname "$REMOTE_COOKIES_PATH")"

env_ok=true
rwpaths_ok=false
# Use fixed-string grep for SUSHE_COOKIES match. systemctl prints multiple
# Environment vars space-separated on one line, so we just need substring.
grep -qF "SUSHE_COOKIES=$REMOTE_COOKIES_PATH" <<<"$MERGED_ENV" || env_ok=false

# ReadWritePaths is space-separated; tokenize and compare exactly to avoid
# regex escaping pitfalls (the cookies dir contains `.config`, where `.`
# would be a wildcard in ERE and produce false positives).
for path in $MERGED_RWPATHS; do
    if [[ "$path" == "$COOKIES_DIR" ]]; then
        rwpaths_ok=true
        break
    fi
done

if ! $env_ok || ! $rwpaths_ok; then
    printf >&2 'ERROR: sushe.service unit is missing required directives.\n'
    $env_ok     || printf >&2 '  - merged Environment missing SUSHE_COOKIES=%s\n' "$REMOTE_COOKIES_PATH"
    $rwpaths_ok || printf >&2 '  - merged ReadWritePaths missing %s (needed because ProtectHome=read-only;\n    yt-dlp writes refreshed cookies on exit and would crash with EROFS otherwise)\n' "$COOKIES_DIR"
    printf >&2 '\nUnit setup is part of make deploy (scripts/deploy.sh). Run that from a\n'
    printf >&2 'machine whose SSH user has unrestricted sudo on the server (the sushe\n'
    printf >&2 'operator user only has narrow sudo for systemctl restart/status):\n\n'
    printf >&2 '    make deploy\n\n'
    printf >&2 'deploy.sh writes /etc/systemd/system/sushe.service with Environment=\n'
    printf >&2 'SUSHE_COOKIES=... and ReadWritePaths including the cookies dir, removes\n'
    printf >&2 'any obsolete cookies drop-in, daemon-reloads, and restarts the service.\n'
    printf >&2 'After it completes, re-run %s for routine cookies refresh.\n\n' "$0"
    printf >&2 -- '--- current merged Environment ---\n%s\n' "$MERGED_ENV"
    printf >&2 -- '--- current merged ReadWritePaths ---\n%s\n' "$MERGED_RWPATHS"
    exit 1
fi
echo "    OK — Environment has SUSHE_COOKIES, ReadWritePaths includes $COOKIES_DIR"

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
# ONE-TIME UNIT SETUP (NOT performed by this script):
#
# The sushe.service unit needs BOTH:
#   - Environment=SUSHE_COOKIES=...   wires the cookies path into yt-dlp
#   - ReadWritePaths=<cookies-dir>    lets yt-dlp WRITE refreshed cookies back.
#     The unit sets ProtectHome=read-only, so without this entry yt-dlp
#     crashes after every successful download with
#     OSError: [Errno 30] Read-only file system.
#
# Both are written by scripts/deploy.sh (make deploy) directly into the main
# unit at /etc/systemd/system/sushe.service. Run make deploy from a machine
# whose SSH user has unrestricted sudo on the server (the sushe operator
# only has narrow sudo for systemctl restart/status, sushe-logs, etc.).
#
# After deploy.sh has run once (or any subsequent re-run), this script handles
# all routine cookies refreshes when the Instagram session expires.
# ============================================================================
