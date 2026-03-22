package api

import (
	"sync"
	"time"
)

const dedupTTL = 15 * time.Minute

type dedupEntry struct {
	status  string // "in_progress" or "completed"
	result  *ResultEvent
	created time.Time
}

type dedupGuard struct {
	entries sync.Map
}

func newDedupGuard() *dedupGuard {
	return &dedupGuard{}
}

// TryAcquire attempts to acquire the dedup lock for the given key.
// Returns (cachedResult, acquired):
//   - (nil, true)           — acquired, caller should proceed
//   - (result, false)       — completed cache hit, caller should return cached result
//   - (nil, false)          — in-progress, caller should return 409
func (d *dedupGuard) TryAcquire(key string) (cachedResult *ResultEvent, acquired bool) {
	for {
		entry := &dedupEntry{status: "in_progress", created: time.Now()}
		actual, loaded := d.entries.LoadOrStore(key, entry)
		if !loaded {
			return nil, true
		}

		existing := actual.(*dedupEntry)
		if time.Since(existing.created) > dedupTTL {
			// Expired — delete and retry
			d.entries.Delete(key)
			continue
		}

		if existing.status == "completed" {
			return existing.result, false
		}

		// In-progress, not expired
		return nil, false
	}
}

// Complete marks the key as completed with the given result and resets the TTL.
func (d *dedupGuard) Complete(key string, result *ResultEvent) {
	d.entries.Store(key, &dedupEntry{
		status:  "completed",
		result:  result,
		created: time.Now(),
	})
}

// Release removes the key, allowing future retries after a failure.
func (d *dedupGuard) Release(key string) {
	d.entries.Delete(key)
}
