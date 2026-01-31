#!/bin/bash
# Sushe Video Bot Deployment Script
# Deploys sushe to a fresh server with proper service setup
# Includes local Telegram Bot API server for 2GB upload support
# All binaries are built locally and transferred to server
# Idempotent - safe to run multiple times

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"
BIN_DIR="$REPO_DIR/bin"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log() { echo -e "${BLUE}[INFO]${NC} $*"; }
success() { echo -e "${GREEN}[OK]${NC} $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }

# Load .env
load_env() {
    if [[ -f "$REPO_DIR/.env" ]]; then
        source "$REPO_DIR/.env"
        log "Loaded .env"
    else
        error "No .env found. Copy .env.example and configure it."
    fi

    # Validate required variables
    [[ -z "${TELEGRAM_API_ID:-}" ]] && error "TELEGRAM_API_ID not set in .env"
    [[ -z "${TELEGRAM_API_HASH:-}" ]] && error "TELEGRAM_API_HASH not set in .env"
    log "Validation passed"
}

# Build telegram-bot-api locally using Docker
build_telegram_bot_api() {
    log "Building telegram-bot-api for Linux..."

    mkdir -p "$BIN_DIR"

    if [[ -f "$BIN_DIR/telegram-bot-api" ]]; then
        success "telegram-bot-api already built (delete bin/telegram-bot-api to rebuild)"
        return 0
    fi

    log "Building with Docker (this may take 5-10 minutes on first run)..."

    docker run --rm -v "$BIN_DIR:/output" ubuntu:22.04 bash -c '
set -e
apt-get update -qq
apt-get install -y -qq make git zlib1g-dev libssl-dev gperf cmake g++ > /dev/null

cd /tmp
git clone --recursive -q https://github.com/tdlib/telegram-bot-api.git
cd telegram-bot-api
mkdir build && cd build
cmake -DCMAKE_BUILD_TYPE=Release .. > /dev/null
cmake --build . --target telegram-bot-api -j4

cp /tmp/telegram-bot-api/build/telegram-bot-api /output/
'

    chmod +x "$BIN_DIR/telegram-bot-api"
    success "telegram-bot-api built"
}

# Build sushe bot
build_sushe() {
    log "Building sushe bot..."

    cd "$REPO_DIR"
    go mod tidy

    mkdir -p "$BIN_DIR"
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
        go build -ldflags '-s -w' \
        -o "$BIN_DIR/sushe" cmd/sushe/main.go

    success "sushe bot built"
}

# Create user on remote server
setup_user() {
    log "Setting up user $REMOTE_USER on server..."

    ssh "$SSH_HOST" bash << REMOTE
set -e

# Create user if not exists
if ! id "$REMOTE_USER" &>/dev/null; then
    sudo useradd -m -s /bin/bash "$REMOTE_USER"
    echo "User $REMOTE_USER created"
else
    echo "User $REMOTE_USER already exists"
fi

# Setup SSH directory
sudo mkdir -p /home/$REMOTE_USER/.ssh
sudo chmod 700 /home/$REMOTE_USER/.ssh

# Add SSH key (idempotent)
echo "$SSH_PUBLIC_KEY" | sudo tee /home/$REMOTE_USER/.ssh/authorized_keys > /dev/null
sudo chmod 600 /home/$REMOTE_USER/.ssh/authorized_keys
sudo chown -R $REMOTE_USER:$REMOTE_USER /home/$REMOTE_USER/.ssh

echo "SSH key configured"
REMOTE

    success "User setup complete"
}

# Install yt-dlp and ffmpeg on remote server
setup_ytdlp() {
    log "Installing yt-dlp and ffmpeg..."

    ssh "$SSH_HOST" bash << 'REMOTE'
set -e

# Install yt-dlp if not present or update it
if command -v yt-dlp &>/dev/null; then
    echo "yt-dlp already installed, updating..."
    sudo yt-dlp -U || true
else
    echo "Installing yt-dlp..."
    sudo curl -sL https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp -o /usr/local/bin/yt-dlp
    sudo chmod a+rx /usr/local/bin/yt-dlp
fi

yt-dlp --version
echo "yt-dlp ready"

# Install ffmpeg if not present
if ! command -v ffmpeg &>/dev/null; then
    echo "Installing ffmpeg..."
    sudo apt-get update -qq && sudo apt-get install -y -qq ffmpeg
fi
REMOTE

    success "yt-dlp setup complete"
}

# Create directories on remote
setup_directories() {
    log "Setting up directories..."

    ssh "$SSH_HOST" bash << REMOTE
set -e
sudo mkdir -p /home/$REMOTE_USER/sushe/bin
sudo mkdir -p /tmp/sushe
sudo mkdir -p /var/lib/telegram-bot-api
sudo chown -R $REMOTE_USER:$REMOTE_USER /home/$REMOTE_USER/sushe
sudo chown -R $REMOTE_USER:$REMOTE_USER /tmp/sushe
sudo chown -R $REMOTE_USER:$REMOTE_USER /var/lib/telegram-bot-api
REMOTE

    success "Directories ready"
}

# Transfer all binaries to server
transfer_binaries() {
    log "Transferring binaries to server..."

    scp "$BIN_DIR/telegram-bot-api" "${REMOTE_USER}@${SERVER}:/home/$REMOTE_USER/sushe/bin/"
    scp "$BIN_DIR/sushe" "${REMOTE_USER}@${SERVER}:/home/$REMOTE_USER/sushe/bin/"

    ssh "${REMOTE_USER}@${SERVER}" "chmod +x ~/sushe/bin/*"

    success "Binaries transferred"
}

# Setup telegram-bot-api systemd service
setup_telegram_bot_api_service() {
    log "Setting up telegram-bot-api service..."

    ssh "$SSH_HOST" bash << REMOTE
set -e

# Create systemd service file
sudo tee /etc/systemd/system/telegram-bot-api.service > /dev/null << EOF
[Unit]
Description=Telegram Bot API Server
After=network.target

[Service]
Type=simple
User=$REMOTE_USER
Group=$REMOTE_USER
ExecStart=/home/$REMOTE_USER/sushe/bin/telegram-bot-api --api-id=$TELEGRAM_API_ID --api-hash=$TELEGRAM_API_HASH --local --dir=/var/lib/telegram-bot-api
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable telegram-bot-api
sudo systemctl restart telegram-bot-api

sleep 3
systemctl is-active --quiet telegram-bot-api && echo "telegram-bot-api running"
REMOTE

    success "telegram-bot-api service configured"
}

# Setup sushe systemd service
setup_sushe_service() {
    log "Setting up sushe service..."

    ssh "$SSH_HOST" bash << REMOTE
set -e

# Create systemd service file
sudo tee /etc/systemd/system/sushe.service > /dev/null << EOF
[Unit]
Description=Sushe Video Downloader Telegram Bot
After=network.target telegram-bot-api.service
Requires=telegram-bot-api.service

[Service]
Type=simple
User=$REMOTE_USER
Group=$REMOTE_USER
WorkingDirectory=/home/$REMOTE_USER/sushe
ExecStart=/home/$REMOTE_USER/sushe/bin/sushe
Restart=always
RestartSec=5
Environment=TELEGRAM_BOT_TOKEN=$TELEGRAM_BOT_TOKEN
Environment=TELEGRAM_API_URL=http://localhost:8081

# Security hardening
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=/home/$REMOTE_USER/sushe /tmp/sushe
PrivateTmp=false

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable sushe
sudo systemctl restart sushe

sleep 3
systemctl is-active --quiet sushe && echo "sushe running"
REMOTE

    success "sushe service configured"
}

# Verify deployment
verify() {
    log "Verifying deployment..."

    echo ""
    log "Telegram Bot API server:"
    ssh "$SSH_HOST" "sudo systemctl status telegram-bot-api --no-pager | head -10" || true

    echo ""
    log "Sushe bot:"
    ssh "$SSH_HOST" "sudo systemctl status sushe --no-pager | head -10" || true

    if ssh "$SSH_HOST" "systemctl is-active --quiet sushe"; then
        success "All services running!"
    else
        warn "Check logs: ssh $SSH_HOST 'sudo journalctl -u sushe -n 50'"
    fi
}

# Main deployment
main() {
    log "Sushe Video Bot Deployment (with Local Bot API)"
    echo "═══════════════════════════════════════════════════════"

    cd "$REPO_DIR"
    load_env
    log "Starting builds..."

    # Build locally
    build_telegram_bot_api
    build_sushe

    # Check SSH connectivity
    log "Testing SSH connection to $SSH_HOST..."
    ssh -o ConnectTimeout=10 "$SSH_HOST" "echo 'SSH OK'" || error "Cannot connect to $SSH_HOST"
    success "SSH connection OK"

    # Setup remote
    setup_user
    setup_ytdlp
    setup_directories
    transfer_binaries
    setup_telegram_bot_api_service
    setup_sushe_service
    verify

    echo ""
    echo "═══════════════════════════════════════════════════════"
    success "Deployment complete! Upload limit is now 2GB."
}

main "$@"
