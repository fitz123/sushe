package main

import (
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fitz123/sushe/internal/bot"
	"github.com/fitz123/sushe/internal/logger"
	tele "gopkg.in/telebot.v3"
)

func main() {
	// Initialize logger
	logger.Init("debug")

	// Get token from environment
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		logger.Error("TELEGRAM_BOT_TOKEN environment variable not set")
		os.Exit(1)
	}

	// Local Bot API server URL (allows 2GB uploads instead of 50MB)
	apiURL := os.Getenv("TELEGRAM_API_URL")
	if apiURL == "" {
		apiURL = "http://localhost:8081" // Default local Bot API server
	}

	// Initialize the bot with local API server
	// Custom HTTP client with long timeout for large file uploads (up to 2GB via local Bot API)
	botPref := tele.Settings{
		Token:  token,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
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

	// Initialize bot service
	botService := bot.NewBotService(botInstance, allowedUsers)

	// Start the bot
	go botService.Start()
	logger.Info("Sushe bot started")

	// Handle shutdown signals
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	<-quit
	logger.Info("Received shutdown signal, shutting down gracefully...")

	botService.Stop()
	logger.Info("Bot stopped")
}
