package api

import (
	"sync"
	"testing"
	"time"
)

func TestTryAcquire_NewKey(t *testing.T) {
	d := newDedupGuard()
	defer close(d.stopCh)
	cached, acquired := d.TryAcquire("key1")
	if cached != nil {
		t.Fatal("expected nil cached result for new key")
	}
	if !acquired {
		t.Fatal("expected acquired=true for new key")
	}
}

func TestTryAcquire_InProgressKey(t *testing.T) {
	d := newDedupGuard()
	defer close(d.stopCh)
	d.TryAcquire("key1")

	cached, acquired := d.TryAcquire("key1")
	if cached != nil {
		t.Fatal("expected nil cached result for in-progress key")
	}
	if acquired {
		t.Fatal("expected acquired=false for in-progress key")
	}
}

func TestComplete_ThenTryAcquire(t *testing.T) {
	d := newDedupGuard()
	defer close(d.stopCh)
	d.TryAcquire("key1")

	expected := &ResultEvent{Status: "done", OK: true, Title: "test", MessageID: 42}
	d.Complete("key1", expected)

	cached, acquired := d.TryAcquire("key1")
	if acquired {
		t.Fatal("expected acquired=false for completed key")
	}
	if cached == nil {
		t.Fatal("expected non-nil cached result")
	}
	if cached.Title != "test" || cached.MessageID != 42 {
		t.Fatalf("cached result mismatch: got %+v", cached)
	}
}

func TestRelease_ThenTryAcquire(t *testing.T) {
	d := newDedupGuard()
	defer close(d.stopCh)
	d.TryAcquire("key1")
	d.Release("key1")

	cached, acquired := d.TryAcquire("key1")
	if cached != nil {
		t.Fatal("expected nil cached result after release")
	}
	if !acquired {
		t.Fatal("expected acquired=true after release")
	}
}

func TestExpiredEntry_Cleanup(t *testing.T) {
	d := newDedupGuard()
	defer close(d.stopCh)
	d.TryAcquire("key1")

	// Manually expire the entry
	d.mu.Lock()
	d.entries["key1"] = &dedupEntry{
		status:  "in_progress",
		created: time.Now().Add(-dedupTTL - time.Second),
	}
	d.mu.Unlock()

	cached, acquired := d.TryAcquire("key1")
	if cached != nil {
		t.Fatal("expected nil cached result for expired key")
	}
	if !acquired {
		t.Fatal("expected acquired=true for expired key")
	}
}

func TestExpiredCompleted_Cleanup(t *testing.T) {
	d := newDedupGuard()
	defer close(d.stopCh)

	d.mu.Lock()
	d.entries["key1"] = &dedupEntry{
		status:  "completed",
		result:  &ResultEvent{Status: "done", OK: true},
		created: time.Now().Add(-dedupTTL - time.Second),
	}
	d.mu.Unlock()

	cached, acquired := d.TryAcquire("key1")
	if cached != nil {
		t.Fatal("expected nil cached result for expired completed key")
	}
	if !acquired {
		t.Fatal("expected acquired=true for expired completed key")
	}
}

func TestConcurrentAccess(t *testing.T) {
	d := newDedupGuard()
	defer close(d.stopCh)
	const goroutines = 10

	var ready sync.WaitGroup
	ready.Add(goroutines)
	start := make(chan struct{})
	var finished sync.WaitGroup
	finished.Add(goroutines)

	results := make([]bool, goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer finished.Done()
			ready.Done()
			<-start
			_, acquired := d.TryAcquire("same-key")
			results[idx] = acquired
		}(i)
	}

	ready.Wait()
	close(start)
	finished.Wait()

	acquiredCount := 0
	for _, got := range results {
		if got {
			acquiredCount++
		}
	}
	if acquiredCount != 1 {
		t.Fatalf("expected exactly 1 goroutine to acquire, got %d", acquiredCount)
	}
}

func TestConcurrentExpiredAccess(t *testing.T) {
	d := newDedupGuard()
	defer close(d.stopCh)
	const goroutines = 10

	// Pre-populate with an expired entry
	d.mu.Lock()
	d.entries["expired-key"] = &dedupEntry{
		status:  "in_progress",
		created: time.Now().Add(-dedupTTL - time.Second),
	}
	d.mu.Unlock()

	var ready sync.WaitGroup
	ready.Add(goroutines)
	start := make(chan struct{})
	var finished sync.WaitGroup
	finished.Add(goroutines)

	results := make([]bool, goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer finished.Done()
			ready.Done()
			<-start
			_, acquired := d.TryAcquire("expired-key")
			results[idx] = acquired
		}(i)
	}

	ready.Wait()
	close(start)
	finished.Wait()

	acquiredCount := 0
	for _, got := range results {
		if got {
			acquiredCount++
		}
	}
	if acquiredCount != 1 {
		t.Fatalf("expected exactly 1 goroutine to acquire expired key, got %d", acquiredCount)
	}
}
