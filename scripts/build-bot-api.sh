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

echo "Building telegram-bot-api for Linux using Docker..."

# Use a multi-stage Docker build
docker run --rm -v "$BIN_DIR:/output" ubuntu:22.04 bash -c '
set -e
apt-get update
apt-get install -y make git zlib1g-dev libssl-dev gperf cmake g++ curl

cd /tmp
git clone --recursive https://github.com/tdlib/telegram-bot-api.git
cd telegram-bot-api
mkdir build && cd build
cmake -DCMAKE_BUILD_TYPE=Release ..
cmake --build . --target telegram-bot-api -j$(nproc)

cp /tmp/telegram-bot-api/build/telegram-bot-api /output/
echo "Build complete!"
'

chmod +x "$BIN_DIR/telegram-bot-api"
echo "telegram-bot-api built successfully at $BIN_DIR/telegram-bot-api"
