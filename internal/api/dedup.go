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
	mu       sync.Mutex
	entries  map[string]*dedupEntry
	stopCh   chan struct{}
	stopOnce sync.Once
}

func newDedupGuard() *dedupGuard {
	d := &dedupGuard{
		entries: make(map[string]*dedupEntry),
		stopCh:  make(chan struct{}),
	}
	go d.cleanupLoop()
	return d
}

// Stop terminates the background cleanup goroutine. Safe to call multiple times.
func (d *dedupGuard) Stop() {
	d.stopOnce.Do(func() {
		close(d.stopCh)
	})
}

// cleanupLoop periodically removes expired completed entries to prevent unbounded memory growth.
// In-progress entries are never expired by the cleanup loop — they are removed by Complete() or
// Release() when the request finishes. This prevents a long-running request's dedup entry from
// being swept while the upload is still in progress.
func (d *dedupGuard) cleanupLoop() {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			d.mu.Lock()
			now := time.Now()
			for key, entry := range d.entries {
				if entry.status == "completed" && now.Sub(entry.created) > dedupTTL {
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

	if existing.status == "completed" {
		if time.Since(existing.created) > dedupTTL {
			// Completed entry expired — replace with fresh in-progress entry
			d.entries[key] = &dedupEntry{status: "in_progress", created: time.Now()}
			return nil, true
		}
		return existing.result, false
	}

	// In-progress entries never expire — they are cleaned up via Complete() or Release()
	// when the request handler finishes. This prevents long-running requests (where upload
	// extends beyond the context timeout) from having their dedup entry swept.
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
