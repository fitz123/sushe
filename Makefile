# Variables
BINARY_NAME := sushe
LDFLAGS := '-linkmode external -extldflags "-static" -s -w'

# Phony targets
.PHONY: all build build-bot-api deploy update verify clean test run help deps

# Default target
all: update

# Install dependencies
deps:
	@echo "Installing dependencies..."
	go mod tidy

# Build the sushe binary
build: deps
	@echo "Building sushe binary..."
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	go build -ldflags '-s -w' -o bin/$(BINARY_NAME) cmd/$(BINARY_NAME)/main.go

# Build telegram-bot-api using Docker (for 2GB upload support)
build-bot-api:
	@echo "Building telegram-bot-api with Docker..."
	@./scripts/build-bot-api.sh

# Build for local development (macOS)
build-local: deps
	@echo "Building binary for local..."
	@mkdir -p bin
	go build -o bin/$(BINARY_NAME) cmd/$(BINARY_NAME)/main.go

# First-time deployment (sets up user, firewall, systemd)
deploy:
	./scripts/deploy.sh

# Update existing deployment
update: build
	./scripts/update.sh

# Verify deployment
verify:
	./scripts/verify.sh

# Clean up
clean:
	@echo "Cleaning up..."
	rm -rf bin
	rm -rf /tmp/sushe

# Run tests
test:
	@echo "Running tests..."
	go test ./... -count=1

# Run the application locally
run: build-local
	@echo "Running the app..."
	./bin/$(BINARY_NAME)

# Print help information
help:
	@echo "Sushe - Video Downloader Bot - Makefile targets"
	@echo ""
	@echo "Deployment:"
	@echo "  deploy     - First-time deployment (user, firewall, systemd)"
	@echo "  update     - Update existing deployment (default)"
	@echo "  verify     - Check deployment status and logs"
	@echo ""
	@echo "Development:"
	@echo "  build      - Build the binary for Linux"
	@echo "  build-local- Build the binary for local dev"
	@echo "  test       - Run the test suite"
	@echo "  run        - Build and run the application locally"
	@echo "  clean      - Remove the bin directory"
	@echo "  deps       - Install Go dependencies"
	@echo ""
	@echo "Configuration:"
	@echo "  Copy .env.example to .env and configure before deploying"
	@echo ""
	@echo "Requirements:"
	@echo "  - Docker (for building telegram-bot-api)"
	@echo "  - yt-dlp and ffmpeg installed on server automatically"
