package api

import (
	"sync"
	"time"
)

const dedupTTL = 15 * time.Minute

// cleanupInterval controls how often the background goroutine sweeps expired entries.
const cleanupInterval = 5 * time.Minute

type dedupEntry struct {
	status  string // "in_progress" or "completed"
	result  *ResultEvent
	created time.Time
}

type dedupGuard struct {
	mu      sync.Mutex
	entries map[string]*dedupEntry
	stopCh  chan struct{}
}

func newDedupGuard() *dedupGuard {
	d := &dedupGuard{
		entries: make(map[string]*dedupEntry),
		stopCh:  make(chan struct{}),
	}
	go d.cleanupLoop()
	return d
}

// cleanupLoop periodically removes expired entries to prevent unbounded memory growth.
func (d *dedupGuard) cleanupLoop() {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			d.mu.Lock()
			now := time.Now()
			for key, entry := range d.entries {
				if now.Sub(entry.created) > dedupTTL {
					delete(d.entries, key)
				}
			}
			d.mu.Unlock()
		case <-d.stopCh:
			return
		}
	}
}

// TryAcquire attempts to acquire the dedup lock for the given key.
// Returns (cachedResult, acquired):
//   - (nil, true)           — acquired, caller should proceed
//   - (result, false)       — completed cache hit, caller should return cached result
//   - (nil, false)          — in-progress, caller should return 409
func (d *dedupGuard) TryAcquire(key string) (cachedResult *ResultEvent, acquired bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	existing, ok := d.entries[key]
	if !ok {
		d.entries[key] = &dedupEntry{status: "in_progress", created: time.Now()}
		return nil, true
	}

	if time.Since(existing.created) > dedupTTL {
		// Expired — replace with fresh in-progress entry
		d.entries[key] = &dedupEntry{status: "in_progress", created: time.Now()}
		return nil, true
	}

	if existing.status == "completed" {
		return existing.result, false
	}

	// In-progress, not expired
	return nil, false
}

// Complete marks the key as completed with the given result and resets the TTL.
func (d *dedupGuard) Complete(key string, result *ResultEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.entries[key] = &dedupEntry{
		status:  "completed",
		result:  result,
		created: time.Now(),
	}
}

// Release removes the key, allowing future retries after a failure.
func (d *dedupGuard) Release(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.entries, key)
}
