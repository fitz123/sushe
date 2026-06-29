#!/bin/bash
# Build telegram-bot-api for Linux using Docker
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"
BIN_DIR="$REPO_DIR/bin"

mkdir -p "$BIN_DIR"

# Check if already built
if [[ -f "$BIN_DIR/telegram-bot-api" ]]; then
    echo "telegram-bot-api already built at $BIN_DIR/telegram-bot-api"
    echo "Delete it to rebuild."
    exit 0
fi

IDLE_TIMEOUT="${TELEGRAM_BOT_API_IDLE_TIMEOUT:-800}"
BUILD_JOBS="${TELEGRAM_BOT_API_BUILD_JOBS:-1}"
PLATFORM="${TELEGRAM_BOT_API_PLATFORM:-linux/amd64}"
[[ "$IDLE_TIMEOUT" =~ ^[0-9]+$ ]] || { echo "TELEGRAM_BOT_API_IDLE_TIMEOUT must be numeric" >&2; exit 1; }
[[ "$BUILD_JOBS" =~ ^[0-9]+$ ]] || { echo "TELEGRAM_BOT_API_BUILD_JOBS must be numeric" >&2; exit 1; }

echo "Building telegram-bot-api for Linux using Docker (platform=${PLATFORM}, IDLE_TIMEOUT=${IDLE_TIMEOUT}s, jobs=${BUILD_JOBS})..."

# Use a multi-stage Docker build
docker run --rm --platform "$PLATFORM" -e IDLE_TIMEOUT="$IDLE_TIMEOUT" -e BUILD_JOBS="$BUILD_JOBS" -v "$BIN_DIR:/output" ubuntu:22.04 bash -c '
set -e
apt-get update
apt-get install -y make git zlib1g-dev libssl-dev gperf cmake g++ curl

cd /tmp
git clone --recursive https://github.com/tdlib/telegram-bot-api.git
cd telegram-bot-api
sed -i -E "s/(static constexpr td::int32 IDLE_TIMEOUT = )[0-9]+;/\1${IDLE_TIMEOUT};/" telegram-bot-api/HttpServer.h
grep "IDLE_TIMEOUT = ${IDLE_TIMEOUT};" telegram-bot-api/HttpServer.h
mkdir build && cd build
cmake -DCMAKE_BUILD_TYPE=Release ..
cmake --build . --target telegram-bot-api -j"${BUILD_JOBS}"

cp /tmp/telegram-bot-api/build/telegram-bot-api /output/
echo "Build complete!"
'

chmod +x "$BIN_DIR/telegram-bot-api"
echo "telegram-bot-api built successfully at $BIN_DIR/telegram-bot-api"
