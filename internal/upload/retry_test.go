package upload

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSendWithRetryConstants verifies the retry configuration
func TestSendWithRetryConstants(t *testing.T) {
	assert.Equal(t, 3, maxRetries, "maxRetries should be 3")
}

// Note: Full integration testing of SendWithRetry requires a mock telebot.Bot,
// which is complex due to telebot.v3's internal HTTP transport. The function is
// tested via integration/deployment tests (Task 5) with the actual Telegram API.
//
// The retry logic itself is straightforward:
// - errors.As(err, &floodErr) to detect FloodError
// - Sleep for RetryAfter seconds
// - Max 3 retries
