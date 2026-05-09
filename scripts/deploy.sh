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

# Service account name on remote server
REMOTE_USER="sushe"

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
sudo chown $REMOTE_USER:$REMOTE_USER /home/$REMOTE_USER/.ssh

# Add SSH key idempotently. APPEND if missing — never overwrite the file.
# An overwrite is destructive when REMOTE_USER points at an existing human
# account with multiple admin keys in authorized_keys (real incident from
# this repo: a deploy run with the wrong REMOTE_USER value clobbered the
# admin's ssh keys and locked us out).
sudo touch /home/$REMOTE_USER/.ssh/authorized_keys
sudo chmod 600 /home/$REMOTE_USER/.ssh/authorized_keys
sudo chown $REMOTE_USER:$REMOTE_USER /home/$REMOTE_USER/.ssh/authorized_keys
if ! sudo grep -qxF "$SSH_PUBLIC_KEY" /home/$REMOTE_USER/.ssh/authorized_keys; then
    echo "$SSH_PUBLIC_KEY" | sudo tee -a /home/$REMOTE_USER/.ssh/authorized_keys > /dev/null
    echo "SSH key added"
else
    echo "SSH key already present"
fi
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
sudo mkdir -p /home/$REMOTE_USER/.config/sushe
sudo chown -R $REMOTE_USER:$REMOTE_USER /home/$REMOTE_USER/sushe
sudo chown -R $REMOTE_USER:$REMOTE_USER /tmp/sushe
sudo chown -R $REMOTE_USER:$REMOTE_USER /var/lib/telegram-bot-api
# Scope ownership tightly: the .config dir itself + its sushe subtree only,
# so re-runs don't clobber unrelated app configs that may live under ~/.config.
sudo chown $REMOTE_USER:$REMOTE_USER /home/$REMOTE_USER/.config
sudo chown -R $REMOTE_USER:$REMOTE_USER /home/$REMOTE_USER/.config/sushe
sudo chmod 700 /home/$REMOTE_USER/.config/sushe
REMOTE

    success "Directories ready"
}

# Transfer cookies file if it exists locally (optional — bot still starts
# cleanly without cookies; SUSHE_COOKIES env var just produces a startup
# warning if the file is unreadable).
transfer_cookies() {
    local local_cookies="$REPO_DIR/instagram-cookies.txt"
    if [[ ! -f "$local_cookies" ]]; then
        warn "No instagram-cookies.txt in repo root — bot will start without cookies (Instagram downloads will fail with login_required until cookies are deployed via scripts/install-cookies.sh)"
        return 0
    fi
    log "Transferring cookies file..."
    # Stage to /tmp on the server (the SSH login user can write there
    # regardless of who they are) then sudo-install to the absolute target
    # path with sushe ownership and mode 0600. Direct scp to ~/.config/sushe
    # would land in the SSH login user's home (e.g. /root/.config/...) when
    # make deploy is run as an admin/root SSH user, NOT in /home/sushe/.config.
    local tmp_remote="/tmp/sushe-cookies-$$"
    scp "$local_cookies" "$SSH_HOST:$tmp_remote"
    ssh "$SSH_HOST" "sudo install -o $REMOTE_USER -g $REMOTE_USER -m 0600 $tmp_remote /home/$REMOTE_USER/.config/sushe/cookies.txt && rm $tmp_remote"
    success "Cookies file deployed to /home/$REMOTE_USER/.config/sushe/cookies.txt"
}

# Transfer all binaries to server.
# Stage to /tmp on the server (the SSH login user can write there regardless
# of identity), then sudo install to the absolute target with REMOTE_USER
# ownership and exec mode. Direct scp to /home/$REMOTE_USER/sushe/bin/ fails
# when the SSH login user (admin) differs from REMOTE_USER (service user).
transfer_binaries() {
    log "Transferring binaries to server..."

    local tmp_tba="/tmp/sushe-tba-$$"
    local tmp_bot="/tmp/sushe-bin-$$"

    scp "$BIN_DIR/telegram-bot-api" "$SSH_HOST:$tmp_tba"
    scp "$BIN_DIR/sushe" "$SSH_HOST:$tmp_bot"

    ssh "$SSH_HOST" "
        sudo install -o $REMOTE_USER -g $REMOTE_USER -m 0755 $tmp_tba /home/$REMOTE_USER/sushe/bin/telegram-bot-api &&
        sudo install -o $REMOTE_USER -g $REMOTE_USER -m 0755 $tmp_bot /home/$REMOTE_USER/sushe/bin/sushe &&
        rm -f $tmp_tba $tmp_bot
    "

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

# Create systemd service file. Cookies env + cookies dir in ReadWritePaths
# are inline so the deploy is single-source-of-truth — no separate drop-in.
# yt-dlp opens cookies file for read AND writes refreshed session cookies
# back at exit, so the cookies dir MUST be in ReadWritePaths or yt-dlp
# crashes after every download with OSError [Errno 30] Read-only file system
# (because of ProtectHome=read-only).
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
Environment=SUSHE_COOKIES=/home/$REMOTE_USER/.config/sushe/cookies.txt

# Security hardening
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=/home/$REMOTE_USER/sushe /tmp/sushe /home/$REMOTE_USER/.config/sushe
PrivateTmp=false

[Install]
WantedBy=multi-user.target
EOF

# Remove any pre-existing cookies drop-in left over from the older
# admin-installed flow — the directives above supersede it.
if [[ -f /etc/systemd/system/sushe.service.d/cookies.conf ]]; then
    sudo rm -f /etc/systemd/system/sushe.service.d/cookies.conf
    sudo rmdir --ignore-fail-on-non-empty /etc/systemd/system/sushe.service.d 2>/dev/null || true
fi

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
    transfer_cookies
    setup_telegram_bot_api_service
    setup_sushe_service
    verify

    echo ""
    echo "═══════════════════════════════════════════════════════"
    success "Deployment complete! Upload limit is now 2GB."
}

main "$@"
