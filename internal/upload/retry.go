package upload

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/fitz123/sushe/internal/logger"
	tele "gopkg.in/telebot.v3"
)

const maxRetries = 3

var telegramBotTokenPattern = regexp.MustCompile(`bot[0-9]+:[A-Za-z0-9_-]+`)

// SanitizeError redacts secrets from errors before they are logged or shown to users.
func SanitizeError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(telegramBotTokenPattern.ReplaceAllString(err.Error(), "bot<redacted-token>"))
}

// SendWithRetry wraps bot.Send with 429/FloodError retry logic.
// Each attempt remains subject to the telebot client's configured HTTP timeout.
// On tele.FloodError, it sleeps for RetryAfter seconds and retries up to maxRetries
// times, keeping the upload retry path finite.
func SendWithRetry(bot *tele.Bot, to tele.Recipient, what interface{}, opts ...interface{}) (*tele.Message, error) {
	for attempt := 0; attempt <= maxRetries; attempt++ {
		msg, err := bot.Send(to, what, opts...)
		if err == nil {
			return msg, nil
		}

		var floodErr tele.FloodError
		if errors.As(err, &floodErr) && attempt < maxRetries {
			logger.Warn("Telegram 429 rate limit, retrying",
				"retry_after", floodErr.RetryAfter,
				"attempt", attempt+1,
			)
			time.Sleep(time.Duration(floodErr.RetryAfter) * time.Second)
			continue
		}

		return nil, SanitizeError(err)
	}

	return nil, fmt.Errorf("max retries (%d) exceeded for Telegram upload", maxRetries)
}
