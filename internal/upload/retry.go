package upload

import (
	"errors"
	"fmt"
	"time"

	"github.com/fitz123/sushe/internal/logger"
	tele "gopkg.in/telebot.v3"
)

const maxRetries = 3

// SendWithRetry wraps bot.Send with 429/FloodError retry logic.
// On tele.FloodError, it sleeps for RetryAfter seconds and retries up to maxRetries times.
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

		return nil, err
	}

	return nil, fmt.Errorf("max retries (%d) exceeded for Telegram upload", maxRetries)
}
