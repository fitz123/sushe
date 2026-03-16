#!/usr/bin/env bash
# Ralphex → OpenClaw notification bridge
# Receives Result JSON on stdin from ralphex notify system.
# Sends formatted message to configured channel + saves to file for agent pickup.

set -euo pipefail

RESULT_FILE="/tmp/ralphex-last-result.json"

# Resolve notification channel/target:
# 1. Env vars (set by agent or watcher)
# 2. run.meta file (if RALPHEX_RUN_DIR is set)
# 3. Fallback: telegram / YOUR_CHAT_ID
NOTIFY_CHANNEL="${RALPHEX_NOTIFY_CHANNEL:-}"
NOTIFY_TARGET="${RALPHEX_NOTIFY_TARGET:-}"

if [[ -z "$NOTIFY_CHANNEL" || -z "$NOTIFY_TARGET" ]]; then
    _RUN_META="${RALPHEX_RUN_DIR:-}/run.meta"
    if [[ -n "${RALPHEX_RUN_DIR:-}" && -f "$_RUN_META" ]]; then
        _CH=$(grep '^notify_channel=' "$_RUN_META" | cut -d= -f2- | head -1 || true)
        _TG=$(grep '^notify_target=' "$_RUN_META" | cut -d= -f2- | head -1 || true)
        [[ -z "$NOTIFY_CHANNEL" && -n "$_CH" ]] && NOTIFY_CHANNEL="$_CH"
        [[ -z "$NOTIFY_TARGET" && -n "$_TG" ]] && NOTIFY_TARGET="$_TG"
    fi
fi

if [[ -z "$NOTIFY_CHANNEL" || -z "$NOTIFY_TARGET" ]]; then
    NOTIFY_CHANNEL="telegram"
    NOTIFY_TARGET="YOUR_CHAT_ID"
    echo "[WARN] Incomplete routing config, falling back to telegram/YOUR_CHAT_ID" >&2
fi

# Validate NOTIFY_TARGET is a numeric ID (Discord snowflake or Telegram chat_id).
if ! [[ "$NOTIFY_TARGET" =~ ^-?[0-9]+$ ]]; then
    echo "[ERROR] NOTIFY_TARGET is not a numeric ID: ${NOTIFY_TARGET}" >&2
    exit 1
fi

# Read JSON from stdin
JSON=$(cat)
# Atomic write: write to temp file then rename to avoid partial-read races.
_TMP=$(mktemp "${RESULT_FILE}.XXXXXX")
echo "$JSON" > "$_TMP"
mv -f "$_TMP" "$RESULT_FILE"

# Parse fields
STATUS=$(echo "$JSON" | jq -r '.status // "unknown"')
PLAN=$(echo "$JSON" | jq -r '.plan_file // "?"')
BRANCH=$(echo "$JSON" | jq -r '.branch // "?"')
DURATION=$(echo "$JSON" | jq -r '.duration // "?"')
FILES=$(echo "$JSON" | jq -r '.files // 0')
ADDS=$(echo "$JSON" | jq -r '.additions // 0')
DELS=$(echo "$JSON" | jq -r '.deletions // 0')
ERROR=$(echo "$JSON" | jq -r '.error // ""')

# Format message
if [[ "$STATUS" == "success" ]]; then
    MSG="🎉 Ralphex complete
Plan: ${PLAN}
Branch: ${BRANCH}
Duration: ${DURATION}
Changes: ${FILES} files (+${ADDS}/-${DELS})"
else
    MSG="❌ Ralphex failed
Plan: ${PLAN}
Branch: ${BRANCH}
Duration: ${DURATION}
Error: ${ERROR}"
fi

# Send via direct API (openclaw CLI can't resolve SecretRef outside gateway runtime)
escaped_msg=$(printf '%s' "$MSG" | python3 -c 'import sys,json; print(json.dumps(sys.stdin.read()))')

if [[ "$NOTIFY_CHANNEL" == "discord" ]]; then
    local_token=$(security find-generic-password -s "discord-bot-token" -a "openclaw" -w 2>/dev/null) || {
        echo "[WARN] Failed to get Discord token from keychain" >&2
        exit 0
    }
    curl -sf -X POST "https://discord.com/api/v10/channels/${NOTIFY_TARGET}/messages" \
        -H "Authorization: Bot ${local_token}" \
        -H "Content-Type: application/json" \
        -d "{\"content\": ${escaped_msg}}" >/dev/null 2>&1 || \
        echo "[WARN] Failed to send Discord notification" >&2
elif [[ "$NOTIFY_CHANNEL" == "telegram" ]]; then
    local_token=$(security find-generic-password -s "telegram-bot-token" -a "openclaw" -w 2>/dev/null) || {
        local_token=$(security find-generic-password -s "keychain_telegram" -a "openclaw" -w 2>/dev/null) || {
            echo "[WARN] Failed to get Telegram token from keychain" >&2
            exit 0
        }
    }
    curl -sf -X POST "https://api.telegram.org/bot${local_token}/sendMessage" \
        -H "Content-Type: application/json" \
        -d "{\"chat_id\": ${NOTIFY_TARGET}, \"text\": ${escaped_msg}}" >/dev/null 2>&1 || \
        echo "[WARN] Failed to send Telegram notification" >&2
else
    echo "[WARN] Unsupported notify channel: ${NOTIFY_CHANNEL}" >&2
fi

exit 0
