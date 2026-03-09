package main

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/fitz123/sushe/internal/api"
	"github.com/fitz123/sushe/internal/bot"
	"github.com/fitz123/sushe/internal/engine"
	"github.com/fitz123/sushe/internal/logger"
	tele "gopkg.in/telebot.v3"
)

// loadEnvFile reads KEY=VALUE pairs from the given file and sets them
// as environment variables, but only if they are not already set.
func loadEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}

func main() {
	// Load .env file (env vars from systemd take precedence)
	loadEnvFile(".env")

	// Initialize logger
	logger.Init("debug")

	// Get token from environment
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		logger.Error("TELEGRAM_BOT_TOKEN environment variable not set")
		os.Exit(1)
	}

	// Local Bot API server URL (allows 2GB uploads instead of 50MB)
	// Also check api_url.txt override file for testing
	apiURL := os.Getenv("TELEGRAM_API_URL")
	if apiURL == "" {
		apiURL = "http://localhost:8081" // Default local Bot API server
	}
	if data, err := os.ReadFile("api_url.txt"); err == nil {
		if override := strings.TrimSpace(string(data)); override != "" {
			logger.Info("API URL override from api_url.txt", "url", override)
			apiURL = override
		}
	}

	// Initialize the bot with local API server
	// Custom HTTP client with long timeout for large file uploads (up to 2GB via local Bot API)
	botPref := tele.Settings{
		Token:  token,
		Poller: &tele.LongPoller{
			Timeout:        10 * time.Second,
			AllowedUpdates: []string{"message", "edited_message", "channel_post", "callback_query"},
		},
		URL:    apiURL,
		Client: &http.Client{Timeout: 60 * time.Minute},
	}

	botInstance, err := tele.NewBot(botPref)
	if err != nil {
		logger.Error("Failed to create bot", "error", err)
		os.Exit(1)
	}

	// Load allowed users whitelist from env
	allowedUsers := bot.LoadAllowedUsers()

	// Create shared download engine
	eng := engine.NewEngine()

	// Initialize bot service
	botService := bot.NewBotService(botInstance, eng, allowedUsers)

	// Start the bot
	go botService.Start()
	logger.Info("Sushe bot started")

	// Start HTTP API server if SUSHE_API_TOKEN is set
	apiToken := os.Getenv("SUSHE_API_TOKEN")
	apiPort := os.Getenv("SUSHE_API_PORT")
	if apiPort == "" {
		apiPort = "8082"
	}

	var httpServer *http.Server
	if apiToken != "" {
		apiService := api.NewAPIService(eng, botInstance, apiToken)
		httpServer = &http.Server{
			Addr:    ":" + apiPort,
			Handler: apiService.Handler(),
		}
		go func() {
			logger.Info("HTTP API server starting", "port", apiPort)
			if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("HTTP API server error", "error", err)
			}
		}()
	} else {
		logger.Info("HTTP API disabled (SUSHE_API_TOKEN not set)")
	}

	// Handle shutdown signals
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	<-quit
	logger.Info("Received shutdown signal, shutting down gracefully...")

	// Shutdown HTTP server if running
	if httpServer != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("HTTP server shutdown error", "error", err)
		}
		logger.Info("HTTP API server stopped")
	}

	botService.Stop()
	logger.Info("Bot stopped")
}
