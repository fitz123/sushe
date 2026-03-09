package bot

import (
	"os"
	"strconv"
	"strings"

	"github.com/fitz123/sushe/internal/logger"
	tele "gopkg.in/telebot.v3"
)

// AllowedUsers holds the set of authorized Telegram user IDs.
// If empty or nil, NO users are allowed (fail-closed).
type AllowedUsers map[int64]struct{}

// LoadAllowedUsers parses the SUSHE_ALLOWED_USERS env variable.
// Expected format: comma-separated user IDs, e.g. "306600687,1352262047"
func LoadAllowedUsers() AllowedUsers {
	raw := os.Getenv("SUSHE_ALLOWED_USERS")
	if raw == "" {
		logger.Warn("SUSHE_ALLOWED_USERS not set — all access denied (fail-closed)")
		return make(AllowedUsers) // empty non-nil map = deny all
	}

	allowed := make(AllowedUsers)
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			logger.Warn("Invalid user ID in SUSHE_ALLOWED_USERS, skipping", "value", s, "error", err)
			continue
		}
		allowed[id] = struct{}{}
	}

	if len(allowed) == 0 {
		logger.Warn("SUSHE_ALLOWED_USERS contains no valid IDs — all access denied (fail-closed)")
		return allowed // empty non-nil map = deny all
	}

	logger.Info("Loaded allowed users whitelist", "count", len(allowed))
	return allowed
}

// AuthMiddleware returns a telebot middleware that restricts access to whitelisted users.
// If allowedUsers is nil or empty, NO users are permitted (fail-closed).
func AuthMiddleware(allowedUsers AllowedUsers) tele.MiddlewareFunc {
	return func(next tele.HandlerFunc) tele.HandlerFunc {
		return func(c tele.Context) error {

			sender := c.Sender()
			if sender == nil {
				return nil // no sender info, skip silently
			}

			if _, ok := allowedUsers[sender.ID]; ok {
				return next(c)
			}

			// Unauthorized — log and ignore
			username := sender.Username
			if username == "" {
				username = strings.TrimSpace(sender.FirstName + " " + sender.LastName)
			}
			logger.Warn("Unauthorized access attempt",
				"user_id", sender.ID,
				"username", username,
			)

			return nil // silently ignore
		}
	}
}
