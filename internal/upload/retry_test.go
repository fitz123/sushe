package upload

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSendWithRetryConstants verifies the retry configuration
func TestSendWithRetryConstants(t *testing.T) {
	assert.Equal(t, 3, maxRetries, "maxRetries should be 3")
}

func TestSanitizeErrorRedactsTelegramBotToken(t *testing.T) {
	err := errors.New(`telebot: Post "http://localhost:8081/bot1234567890:AAH4secret_token-value/sendVideo": EOF`)

	sanitized := SanitizeError(err)

	assert.Equal(t, `telebot: Post "http://localhost:8081/bot<redacted-token>/sendVideo": EOF`, sanitized.Error())
}

func TestSanitizeErrorNil(t *testing.T) {
	assert.NoError(t, SanitizeError(nil))
}

// Note: Full integration testing of SendWithRetry requires a mock telebot.Bot,
// which is complex due to telebot.v3's internal HTTP transport. The function is
// tested via integration/deployment tests (Task 5) with the actual Telegram API.
//
// The retry logic itself is straightforward:
// - errors.As(err, &floodErr) to detect FloodError
// - Sleep for RetryAfter seconds
// - Max 3 retries
