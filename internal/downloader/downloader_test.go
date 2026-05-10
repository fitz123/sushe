package downloader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fitz123/sushe/internal/logger"
)

func init() {
	// The package-level logger is otherwise nil in test runs, which panics
	// from any code path that logs (e.g. runWithCookieFallback's Debug call
	// on every yt-dlp invocation). Initialize at warn to keep test output
	// uncluttered while exercising real logger calls.
	logger.Init("warn")
}

func TestCookieArgs(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		want    []string
		wantNil bool // empty path must return literal nil, not an empty slice
	}{
		{"empty path returns nil", "", nil, true},
		{"non-empty path returns flag pair", "/etc/sushe/cookies.txt", []string{"--cookies", "/etc/sushe/cookies.txt"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cookieArgs(tt.path)
			if tt.wantNil && got != nil {
				t.Errorf("cookieArgs(%q) = %v, want nil (slices.Equal(nil, []string{}) is true, so an explicit nil check is required)", tt.path, got)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("cookieArgs(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

// TestCookieArgsAppendSafe verifies that callers can safely append onto the
// returned slice without aliasing other call sites. Uses the same input for
// both calls so any shared internal cache would cause cross-contamination.
func TestCookieArgsAppendSafe(t *testing.T) {
	a := append(cookieArgs("/x"), "tail-a")
	b := append(cookieArgs("/x"), "tail-b")
	wantA := []string{"--cookies", "/x", "tail-a"}
	wantB := []string{"--cookies", "/x", "tail-b"}
	if !slices.Equal(a, wantA) {
		t.Errorf("a = %v, want %v", a, wantA)
	}
	if !slices.Equal(b, wantB) {
		t.Errorf("b = %v, want %v", b, wantB)
	}
}

// TestThrottleArgs asserts the helper returns the expected static slice. The
// values are load-bearing for Instagram anti-flag posture — accidental edits
// to the magic-string flag values (e.g. raising retries back to 10, or losing
// the Firefox UA) would silently regress the throttling stack. This guard
// makes the values reviewable and prevents drift.
func TestThrottleArgs(t *testing.T) {
	want := []string{
		"--sleep-requests", "2",
		"--sleep-interval", "2",
		"--max-sleep-interval", "5",
		"--retries", "3",
		"--fragment-retries", "3",
		"--socket-timeout", "30",
		"--user-agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:150.0) Gecko/20100101 Firefox/150.0",
	}
	got := throttleArgs()
	if !slices.Equal(got, want) {
		t.Errorf("throttleArgs() = %v, want %v", got, want)
	}

	// Spot-check each key flag is present (independent of order). Catches a
	// regression where someone reorders the slice and the equality check
	// becomes brittle to refactors — at least these key flags must remain.
	keyFlags := []string{
		"--sleep-requests",
		"--sleep-interval",
		"--max-sleep-interval",
		"--retries",
		"--fragment-retries",
		"--socket-timeout",
		"--user-agent",
	}
	for _, flag := range keyFlags {
		if !slices.Contains(got, flag) {
			t.Errorf("throttleArgs() missing required flag %q; got %v", flag, got)
		}
	}

	// Length sanity: 7 flag/value pairs = 14 elements. Catches accidental
	// additions or removals that would slip past the equality test if the
	// "want" slice were also edited.
	if len(got) != 14 {
		t.Errorf("throttleArgs() len = %d, want 14 (7 flag/value pairs)", len(got))
	}
}

// TestThrottleArgsAppendSafe verifies that callers can safely append onto the
// returned slice without aliasing other call sites — same invariant as
// cookieArgs since both helpers are prepended at every yt-dlp invocation site.
func TestThrottleArgsAppendSafe(t *testing.T) {
	a := append(throttleArgs(), "tail-a")
	b := append(throttleArgs(), "tail-b")
	if a[len(a)-1] != "tail-a" {
		t.Errorf("a tail = %q, want tail-a", a[len(a)-1])
	}
	if b[len(b)-1] != "tail-b" {
		t.Errorf("b tail = %q, want tail-b", b[len(b)-1])
	}
	// Sanity: appends must not cross-contaminate.
	if a[len(a)-1] == b[len(b)-1] {
		t.Errorf("throttleArgs() returns shared backing array; appends cross-contaminated")
	}
}

func TestIsH264Compatible(t *testing.T) {
	tests := []struct {
		codec string
		want  bool
	}{
		{"h264", true},
		{"H264", true},
		{"avc", true},
		{"avc1", true},
		{"vp9", false},
		{"av1", false},
		{"hevc", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.codec, func(t *testing.T) {
			if got := IsH264Compatible(tt.codec); got != tt.want {
				t.Errorf("IsH264Compatible(%q) = %v, want %v", tt.codec, got, tt.want)
			}
		})
	}
}

func TestIsAACCompatible(t *testing.T) {
	tests := []struct {
		codec string
		want  bool
	}{
		{"aac", true},
		{"AAC", true},
		{"opus", false},
		{"vorbis", false},
		{"mp3", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.codec, func(t *testing.T) {
			if got := IsAACCompatible(tt.codec); got != tt.want {
				t.Errorf("IsAACCompatible(%q) = %v, want %v", tt.codec, got, tt.want)
			}
		})
	}
}

func TestIs420p(t *testing.T) {
	tests := []struct {
		pixFmt string
		want   bool
	}{
		{"yuv420p", true},
		{"yuvj420p", true},
		{"YUV420P", true},
		{"yuv422p", false},
		{"yuv444p", false},
		{"yuv420p10le", false},
		{"nv12", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.pixFmt, func(t *testing.T) {
			if got := Is420p(tt.pixFmt); got != tt.want {
				t.Errorf("Is420p(%q) = %v, want %v", tt.pixFmt, got, tt.want)
			}
		})
	}
}

func TestCalculateNumParts(t *testing.T) {
	tests := []struct {
		name     string
		fileSize int64
		want     int
	}{
		{"exactly 1.7GB", MaxSplitSize, 1},
		{"exactly 3.4GB", 2 * MaxSplitSize, 2},
		{"3.5GB needs 3 parts", 3500 * 1024 * 1024, 3},
		{"1.8GB needs 2 parts", 1800 * 1024 * 1024, 2},
		{"small file", 100 * 1024 * 1024, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CalculateNumParts(tt.fileSize); got != tt.want {
				t.Errorf("CalculateNumParts(%d) = %d, want %d", tt.fileSize, got, tt.want)
			}
		})
	}
}

func TestNeedsSplit(t *testing.T) {
	tests := []struct {
		name     string
		fileSize int64
		want     bool
	}{
		{"exactly MaxUploadSize", MaxUploadSize, false},
		{"one byte over MaxUploadSize", MaxUploadSize + 1, true},
		{"well under threshold", 1024 * 1024 * 1024, false},
		{"well over threshold", 3 * 1024 * 1024 * 1024, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NeedsSplit(tt.fileSize); got != tt.want {
				t.Errorf("NeedsSplit(%d) = %v, want %v", tt.fileSize, got, tt.want)
			}
		})
	}
}

func TestCanStreamCopyDecision(t *testing.T) {
	tests := []struct {
		name       string
		videoCodec string
		audioCodec string
		pixFmt     string
		want       bool
	}{
		{"H264 + AAC + yuv420p", "h264", "aac", "yuv420p", true},
		{"VP9 + AAC + yuv420p", "vp9", "aac", "yuv420p", false},
		{"H264 + Opus + yuv420p", "h264", "opus", "yuv420p", false},
		{"H264 + AAC + yuv420p10le", "h264", "aac", "yuv420p10le", false},
		{"unknown + unknown + unknown", "unknown", "unknown", "unknown", false},
		{"H264 + AAC + yuv422p", "h264", "aac", "yuv422p", false},
		{"H264 + AAC + yuv444p", "h264", "aac", "yuv444p", false},
		{"H264 + AAC + yuvj420p", "h264", "aac", "yuvj420p", true},
		{"AVC1 + AAC + yuv420p", "avc1", "aac", "yuv420p", true},
		{"H264 + MP3 + yuv420p", "h264", "mp3", "yuv420p", false},
		{"HEVC + AAC + yuv420p", "hevc", "aac", "yuv420p", false},
		{"empty codecs", "", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CanStreamCopy(tt.videoCodec, tt.audioCodec, tt.pixFmt)
			if got != tt.want {
				t.Errorf("canStreamCopy(%q, %q, %q) = %v, want %v",
					tt.videoCodec, tt.audioCodec, tt.pixFmt, got, tt.want)
			}
		})
	}
}

func TestFormatYtdlpError(t *testing.T) {
	baseErr := errors.New("exit status 1")

	tests := []struct {
		name       string
		err        error
		stderr     string
		wantNil    bool
		wantSubstr []string
	}{
		{
			name:    "nil error passes through",
			err:     nil,
			stderr:  "anything",
			wantNil: true,
		},
		{
			name:       "empty stderr returns err unchanged",
			err:        baseErr,
			stderr:     "",
			wantSubstr: []string{"exit status 1"},
		},
		{
			name:       "whitespace-only stderr returns err unchanged",
			err:        baseErr,
			stderr:     "   \n\t  \n",
			wantSubstr: []string{"exit status 1"},
		},
		{
			name:       "non-empty stderr appended to error",
			err:        baseErr,
			stderr:     "ERROR: [Instagram] DXXX: rate-limit reached or login required",
			wantSubstr: []string{"exit status 1", "ERROR: [Instagram] DXXX: rate-limit reached or login required"},
		},
		{
			name:       "stderr is trimmed",
			err:        baseErr,
			stderr:     "\n\nERROR: HTTP 429\n\n",
			wantSubstr: []string{"exit status 1", "ERROR: HTTP 429"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatYtdlpError(tt.err, tt.stderr)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected non-nil error")
			}
			for _, sub := range tt.wantSubstr {
				if !strings.Contains(got.Error(), sub) {
					t.Errorf("expected error to contain %q, got %q", sub, got.Error())
				}
			}
		})
	}

	t.Run("wrapped error remains unwrappable", func(t *testing.T) {
		wrapped := formatYtdlpError(baseErr, "ERROR: something")
		if !errors.Is(wrapped, baseErr) {
			t.Errorf("expected errors.Is to find base error in wrapped result")
		}
	})
}

// progressRecorder is a thread-safe ProgressCallback stand-in for tests.
// Records every Progress event sent through it so test cases can assert on
// the count, phase, and ETA without racing against the goroutine emitting
// the event (waitForIGSlot itself is synchronous, but a defensive mutex
// keeps the recorder usable from future async callers too).
type progressRecorder struct {
	mu     sync.Mutex
	events []Progress
}

func (r *progressRecorder) cb(p Progress) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, p)
}

func (r *progressRecorder) snapshot() []Progress {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Progress, len(r.events))
	copy(out, r.events)
	return out
}

// fastPathBudget is the elapsed-time tolerance for fast-path waitForIGSlot
// calls (non-IG host, fresh gate, or already-expired gap). 100ms is generous
// enough to absorb scheduler jitter on loaded CI while still catching any
// accidental sleep introduced into the fast-path branches.
const fastPathBudget = 100 * time.Millisecond

// TestWaitForIGSlot covers the host-match logic, the rate-limit timing window,
// and the progress emission contract for the Instagram-specific gate. The
// tests directly manipulate d.igLastAt to simulate prior IG activity without
// having to wait the full minIGGap (8s) in real time.
//
// White-box rationale: these subtests construct `&Downloader{}` directly (no
// NewDownloader) and write to the unexported `igLastAt` field. This is
// intentional — the gate is a private rate-limit primitive whose timing is
// the entire contract under test, and the only way to exercise the recent-
// prior-call branch without waiting the full minIGGap (~8s) per case is to
// pre-stamp the field. Tests live in the same package precisely for this.
func TestWaitForIGSlot(t *testing.T) {
	t.Run("non-IG URL returns immediately and does not update igLastAt", func(t *testing.T) {
		d := &Downloader{}
		ctx := context.Background()
		before := time.Now()
		err := d.waitForIGSlot(ctx, "https://youtube.com/watch?v=abc", nil)
		elapsed := time.Since(before)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if elapsed > fastPathBudget {
			t.Errorf("non-IG URL took %v, want <%v", elapsed, fastPathBudget)
		}
		if !d.igLastAt.IsZero() {
			t.Errorf("igLastAt was updated for non-IG URL: %v", d.igLastAt)
		}
	})

	t.Run("fresh IG URL passes through immediately and stamps igLastAt", func(t *testing.T) {
		d := &Downloader{}
		ctx := context.Background()
		before := time.Now()
		err := d.waitForIGSlot(ctx, "https://www.instagram.com/p/abc/", nil)
		elapsed := time.Since(before)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if elapsed > fastPathBudget {
			t.Errorf("fresh IG URL took %v, want <%v", elapsed, fastPathBudget)
		}
		if d.igLastAt.IsZero() {
			t.Errorf("igLastAt was not updated after first IG call")
		}
		if time.Since(d.igLastAt) > fastPathBudget {
			t.Errorf("igLastAt is stale: %v", d.igLastAt)
		}
	})

	t.Run("IG URL with recent prior call waits the remaining gap", func(t *testing.T) {
		// Set igLastAt to (minIGGap - 500ms) ago so we expect ~500ms of waiting.
		// The 500ms margin (rather than 200ms) gives loaded CI enough headroom
		// for scheduler jitter without timing into the "too short" branch.
		d := &Downloader{}
		d.igLastAt = time.Now().Add(-(minIGGap - 500*time.Millisecond))
		ctx := context.Background()
		before := time.Now()
		err := d.waitForIGSlot(ctx, "https://instagram.com/reel/xyz/", nil)
		elapsed := time.Since(before)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Expect ~500ms wait. Allow generous tolerance for CI scheduler jitter.
		if elapsed < 400*time.Millisecond {
			t.Errorf("expected wait ~500ms, got %v (too short)", elapsed)
		}
		if elapsed > 2500*time.Millisecond {
			t.Errorf("expected wait ~500ms, got %v (too long)", elapsed)
		}
	})

	t.Run("IG URL with gap already elapsed passes through immediately", func(t *testing.T) {
		d := &Downloader{}
		d.igLastAt = time.Now().Add(-(minIGGap + 5*time.Second))
		ctx := context.Background()
		before := time.Now()
		err := d.waitForIGSlot(ctx, "https://instagram.com/p/xyz/", nil)
		elapsed := time.Since(before)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if elapsed > fastPathBudget {
			t.Errorf("warmed-up IG URL took %v, want <%v", elapsed, fastPathBudget)
		}
	})

	t.Run("substring 'instagram' in path or query of non-IG host does not trigger gate", func(t *testing.T) {
		d := &Downloader{}
		ctx := context.Background()
		before := time.Now()
		err := d.waitForIGSlot(ctx, "https://youtube.com/watch?title=instagram-clone", nil)
		elapsed := time.Since(before)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if elapsed > fastPathBudget {
			t.Errorf("youtube URL with 'instagram' substring took %v, want <%v", elapsed, fastPathBudget)
		}
		if !d.igLastAt.IsZero() {
			t.Errorf("igLastAt was incorrectly updated for non-IG host")
		}
	})

	t.Run("confusable host evilinstagram.com is excluded by leading-dot suffix check", func(t *testing.T) {
		d := &Downloader{}
		ctx := context.Background()
		before := time.Now()
		err := d.waitForIGSlot(ctx, "https://evilinstagram.com/p/xyz/", nil)
		elapsed := time.Since(before)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if elapsed > fastPathBudget {
			t.Errorf("evilinstagram.com URL took %v, want <%v", elapsed, fastPathBudget)
		}
		if !d.igLastAt.IsZero() {
			t.Errorf("igLastAt was incorrectly updated for confusable host")
		}
	})

	t.Run("m.instagram.com subdomain triggers gate", func(t *testing.T) {
		d := &Downloader{}
		ctx := context.Background()
		err := d.waitForIGSlot(ctx, "https://m.instagram.com/reel/abc/", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d.igLastAt.IsZero() {
			t.Errorf("igLastAt was not updated for m.instagram.com")
		}
	})

	t.Run("URL with empty host falls through ungated", func(t *testing.T) {
		d := &Downloader{}
		ctx := context.Background()
		before := time.Now()
		// "https:///path" parses successfully but yields an empty Hostname(),
		// so it falls through the `isIG` check without triggering the gate.
		// This exercises the host=="" branch (distinct from the parse-error
		// branch covered below).
		err := d.waitForIGSlot(ctx, "https:///path", nil)
		elapsed := time.Since(before)
		if err != nil {
			t.Fatalf("expected nil for empty-host URL, got %v", err)
		}
		if elapsed > fastPathBudget {
			t.Errorf("empty-host URL took %v, want <%v", elapsed, fastPathBudget)
		}
		if !d.igLastAt.IsZero() {
			t.Errorf("igLastAt was updated for empty-host URL: %v", d.igLastAt)
		}
	})

	t.Run("malformed URL with parse error returns nil and does not gate", func(t *testing.T) {
		d := &Downloader{}
		ctx := context.Background()
		before := time.Now()
		// "http://[::1" has an unbalanced bracket in the IPv6 literal, which
		// triggers a real net/url.Parse error. The function must return nil
		// (let yt-dlp report the error) and must not touch igLastAt. This
		// exercises the err != nil branch.
		err := d.waitForIGSlot(ctx, "http://[::1", nil)
		elapsed := time.Since(before)
		if err != nil {
			t.Fatalf("expected nil for malformed URL with parse error, got %v", err)
		}
		if elapsed > fastPathBudget {
			t.Errorf("malformed URL took %v, want <%v", elapsed, fastPathBudget)
		}
		if !d.igLastAt.IsZero() {
			t.Errorf("igLastAt was updated for malformed URL: %v", d.igLastAt)
		}
	})

	t.Run("context cancellation mid-wait returns ctx.Err and preserves projected stamp", func(t *testing.T) {
		d := &Downloader{}
		// Set prior to "just now" so the full minIGGap (~8s) wait is required.
		priorStamp := time.Now()
		d.igLastAt = priorStamp
		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan error, 1)
		go func() {
			done <- d.waitForIGSlot(ctx, "https://instagram.com/p/abc/", nil)
		}()

		// Give the goroutine a moment to enter the select on time.After.
		// waitForIGSlot stamps igLastAt = priorStamp + minIGGap synchronously
		// inside the lock BEFORE entering the sleep, so once cancel() races
		// the sleep the stamp is already in place.
		time.Sleep(50 * time.Millisecond)
		cancel()

		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("expected context.Canceled, got %v", err)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("waitForIGSlot did not return after cancel()")
		}

		// Verify the projected stamp invariant: igLastAt must be the
		// projected wake time (priorStamp + minIGGap), NOT reset and NOT the
		// original priorStamp. This is what guarantees retry storms after
		// cancellation still respect the gap.
		expectedStamp := priorStamp.Add(minIGGap)
		drift := d.igLastAt.Sub(expectedStamp)
		if drift < -100*time.Millisecond || drift > 100*time.Millisecond {
			t.Errorf("igLastAt drift from projected wake time = %v, want within +/-100ms (prior=%v, stamp=%v, expected=%v)",
				drift, priorStamp, d.igLastAt, expectedStamp)
		}

		// Verify the lock was released: a subsequent (non-blocking) call must
		// succeed without deadlocking. Use a different IG URL to be sure. We
		// reset igLastAt first so this call doesn't have to wait minIGGap
		// for the projected stamp set above to expire.
		ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
		defer cancel2()
		releasedDone := make(chan error, 1)
		go func() {
			d.igLastAt = time.Time{}
			releasedDone <- d.waitForIGSlot(ctx2, "https://instagram.com/p/def/", nil)
		}()
		select {
		case err := <-releasedDone:
			if err != nil {
				t.Errorf("post-cancel call failed: %v", err)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("post-cancel call deadlocked — lock was not released")
		}
	})

	t.Run("pre-cancelled context returns immediately without acquiring lock", func(t *testing.T) {
		d := &Downloader{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancel BEFORE the call
		before := time.Now()
		err := d.waitForIGSlot(ctx, "https://instagram.com/p/abc/", nil)
		elapsed := time.Since(before)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
		if elapsed > 50*time.Millisecond {
			t.Errorf("pre-cancelled call took %v, want <50ms", elapsed)
		}
	})

	t.Run("progress emitted exactly once when wait is required", func(t *testing.T) {
		d := &Downloader{}
		// Leave 500ms remaining (rather than 200ms) so loaded CI scheduler
		// jitter doesn't accidentally consume the entire window before the
		// goroutine reaches the progress emission line.
		d.igLastAt = time.Now().Add(-(minIGGap - 500*time.Millisecond))
		r := &progressRecorder{}
		ctx := context.Background()
		err := d.waitForIGSlot(ctx, "https://instagram.com/p/xyz/", r.cb)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		events := r.snapshot()
		if len(events) != 1 {
			t.Fatalf("expected exactly 1 progress event, got %d: %+v", len(events), events)
		}
		if events[0].Phase != "queued" {
			t.Errorf("expected Phase=queued, got %q", events[0].Phase)
		}
		if events[0].ETA == "" {
			t.Errorf("expected non-empty ETA, got empty")
		}
	})

	t.Run("no progress emitted when warmed-up (no wait needed)", func(t *testing.T) {
		d := &Downloader{}
		d.igLastAt = time.Now().Add(-(minIGGap + 5*time.Second))
		r := &progressRecorder{}
		ctx := context.Background()
		err := d.waitForIGSlot(ctx, "https://instagram.com/p/xyz/", r.cb)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(r.snapshot()) != 0 {
			t.Errorf("expected zero progress events for warmed-up call, got %+v", r.snapshot())
		}
	})

	t.Run("no progress emitted for non-IG URL", func(t *testing.T) {
		d := &Downloader{}
		r := &progressRecorder{}
		ctx := context.Background()
		err := d.waitForIGSlot(ctx, "https://youtube.com/watch?v=abc", r.cb)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(r.snapshot()) != 0 {
			t.Errorf("expected zero progress events for non-IG URL, got %+v", r.snapshot())
		}
	})

	t.Run("nil progressCb is safe when wait is required", func(t *testing.T) {
		d := &Downloader{}
		d.igLastAt = time.Now().Add(-(minIGGap - 500*time.Millisecond))
		ctx := context.Background()
		// Must not panic.
		err := d.waitForIGSlot(ctx, "https://instagram.com/p/xyz/", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// TestIsInstagramHost covers the host-match helper shared by waitForIGSlot
// (rate-limit gate) and igExtractorArgs (Layer 0 args). Table-driven across
// the canonical IG hosts (www, m, bare), confusables that must NOT match
// (evilinstagram.com, instagram.com.evil.com), non-IG hosts, and parse-error
// edges (malformed URL, empty string, empty hostname).
func TestIsInstagramHost(t *testing.T) {
	tests := []struct {
		name   string
		rawURL string
		want   bool
	}{
		// Positive: canonical IG hosts.
		{"www.instagram.com", "https://www.instagram.com/p/abc/", true},
		{"bare instagram.com", "https://instagram.com/p/abc/", true},
		{"m.instagram.com mobile", "https://m.instagram.com/reel/xyz/", true},
		{"instagram.com with port", "https://instagram.com:443/p/abc/", true},
		{"uppercase host normalized", "https://WWW.INSTAGRAM.COM/p/abc/", true},

		// Negative: confusable hosts that must NOT match (security-critical).
		{"evilinstagram.com", "https://evilinstagram.com/p/abc/", false},
		{"instagram.com.evil.com", "https://instagram.com.evil.com/p/abc/", false},
		{"notinstagram.com", "https://notinstagram.com/", false},

		// Negative: unrelated hosts.
		{"youtube.com", "https://www.youtube.com/watch?v=abc", false},
		{"tiktok.com", "https://www.tiktok.com/@user/video/12345", false},
		{"twitter.com", "https://twitter.com/user/status/1", false},

		// Edge: parse errors and empty hosts must return false.
		{"malformed URL with IPv6 bracket", "http://[::1", false},
		{"empty string", "", false},
		{"empty host (https:///path)", "https:///path", false},
		{"substring instagram in path", "https://example.com/instagram.com/p/abc/", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isInstagramHost(tt.rawURL); got != tt.want {
				t.Errorf("isInstagramHost(%q) = %v, want %v", tt.rawURL, got, tt.want)
			}
		})
	}
}

// TestIgExtractorArgs covers the Layer 0 helper that returns IG-specific
// yt-dlp flags (iOS app id + iOS UA + retries=1 + fragment-retries=1) for
// Instagram URLs and nil for everything else.
//
// The exact flag values are load-bearing for IG anti-flag posture — drift in
// the app_id, UA, or retry counts would silently regress the iOS-fingerprint
// strategy. Spot-checking each flag here makes the values reviewable.
func TestIgExtractorArgs(t *testing.T) {
	t.Run("IG URL returns iOS extractor args", func(t *testing.T) {
		got := igExtractorArgs("https://www.instagram.com/reel/abc/")
		want := []string{
			"--extractor-args", "instagram:app_id=124024574287414",
			"--user-agent", "Instagram 339.0.0.12.95 (iPhone16,1; iOS 18_2; en_US; en_US; scale=3.00; gamut=normal; 1179x2556) AppleWebKit/420+",
			"--retries", "1",
			"--fragment-retries", "1",
		}
		if !slices.Equal(got, want) {
			t.Errorf("igExtractorArgs(IG) = %v, want %v", got, want)
		}
	})

	t.Run("bare instagram.com host matches", func(t *testing.T) {
		got := igExtractorArgs("https://instagram.com/p/abc/")
		if len(got) != 8 {
			t.Errorf("expected 8 args, got %d: %v", len(got), got)
		}
	})

	t.Run("m.instagram.com subdomain matches", func(t *testing.T) {
		got := igExtractorArgs("https://m.instagram.com/p/abc/")
		if len(got) != 8 {
			t.Errorf("expected 8 args for m.instagram.com, got %d: %v", len(got), got)
		}
	})

	t.Run("non-IG URL returns nil", func(t *testing.T) {
		for _, u := range []string{
			"https://youtube.com/watch?v=abc",
			"https://www.tiktok.com/@user/video/12345",
			"https://twitter.com/user/status/1",
			"https://evilinstagram.com/p/abc/",
			"https://instagram.com.evil.com/p/abc/",
		} {
			got := igExtractorArgs(u)
			if got != nil {
				t.Errorf("igExtractorArgs(%q) = %v, want nil", u, got)
			}
		}
	})

	t.Run("parse error returns nil", func(t *testing.T) {
		got := igExtractorArgs("http://[::1")
		if got != nil {
			t.Errorf("igExtractorArgs(malformed) = %v, want nil", got)
		}
	})

	t.Run("fresh slice each call (append-safe)", func(t *testing.T) {
		igURL := "https://www.instagram.com/p/abc/"
		a := append(igExtractorArgs(igURL), "tail-a")
		b := append(igExtractorArgs(igURL), "tail-b")
		if a[len(a)-1] != "tail-a" {
			t.Errorf("a tail = %q, want tail-a", a[len(a)-1])
		}
		if b[len(b)-1] != "tail-b" {
			t.Errorf("b tail = %q, want tail-b", b[len(b)-1])
		}
		if a[len(a)-1] == b[len(b)-1] {
			t.Errorf("igExtractorArgs returns shared backing array; appends cross-contaminated")
		}
	})

	t.Run("flag values spot-check", func(t *testing.T) {
		got := igExtractorArgs("https://www.instagram.com/reel/abc/")
		// iOS app_id must be 124024574287414 (PR #12359), not the web-app
		// default 936619743392459.
		foundAppID := false
		for i := 0; i < len(got)-1; i++ {
			if got[i] == "--extractor-args" && got[i+1] == "instagram:app_id=124024574287414" {
				foundAppID = true
			}
		}
		if !foundAppID {
			t.Errorf("expected --extractor-args instagram:app_id=124024574287414, got %v", got)
		}
		// iOS UA must start with "Instagram " — this is what differentiates it
		// from the desktop Firefox UA in throttleArgs.
		foundUA := false
		for i := 0; i < len(got)-1; i++ {
			if got[i] == "--user-agent" && strings.HasPrefix(got[i+1], "Instagram ") {
				foundUA = true
			}
		}
		if !foundUA {
			t.Errorf("expected --user-agent starting with 'Instagram ', got %v", got)
		}
	})
}

// TestIgArgsOverrideThrottle is the load-bearing integration test for the
// "last-wins" arg-parsing invariant: when igExtractorArgs is appended AFTER
// throttleArgs/cookieArgs at a call site (the order the production call sites
// use), yt-dlp's left-to-right arg parser picks up the IG-specific values for
// --user-agent, --retries, and --fragment-retries.
//
// If a future refactor reorders the args, the IG values would stop overriding
// and the web-app UA + retries=3 would leak — the very regression Layer 0
// exists to prevent. This test anchors the invariant in CI.
func TestIgArgsOverrideThrottle(t *testing.T) {
	t.Run("IG URL: last --user-agent / --retries / --fragment-retries wins", func(t *testing.T) {
		igURL := "https://www.instagram.com/reel/abc/"
		args := append(throttleArgs(), cookieArgs("")...)
		args = append(args, igExtractorArgs(igURL)...)

		// --user-agent must appear exactly twice (desktop Firefox from
		// throttleArgs, then iOS from igExtractorArgs). The LAST occurrence
		// must be the iOS UA — that's the one yt-dlp's last-wins parser uses.
		uaIdxs := indexAllOf(args, "--user-agent")
		if len(uaIdxs) != 2 {
			t.Errorf("expected --user-agent to appear exactly 2x, got %d at %v: %v", len(uaIdxs), uaIdxs, args)
		}
		if len(uaIdxs) >= 1 {
			lastUAValue := args[uaIdxs[len(uaIdxs)-1]+1]
			if !strings.HasPrefix(lastUAValue, "Instagram ") {
				t.Errorf("expected LAST --user-agent value to start with 'Instagram ', got %q", lastUAValue)
			}
		}

		// --retries must appear exactly twice (3 from throttleArgs, 1 from
		// igExtractorArgs). Last value must be "1".
		retriesIdxs := indexAllOf(args, "--retries")
		if len(retriesIdxs) != 2 {
			t.Errorf("expected --retries to appear exactly 2x, got %d at %v", len(retriesIdxs), retriesIdxs)
		}
		if len(retriesIdxs) >= 1 {
			lastVal := args[retriesIdxs[len(retriesIdxs)-1]+1]
			if lastVal != "1" {
				t.Errorf("expected LAST --retries value to be %q, got %q", "1", lastVal)
			}
		}

		// --fragment-retries must appear exactly twice. Last value must be "1".
		fragIdxs := indexAllOf(args, "--fragment-retries")
		if len(fragIdxs) != 2 {
			t.Errorf("expected --fragment-retries to appear exactly 2x, got %d at %v", len(fragIdxs), fragIdxs)
		}
		if len(fragIdxs) >= 1 {
			lastVal := args[fragIdxs[len(fragIdxs)-1]+1]
			if lastVal != "1" {
				t.Errorf("expected LAST --fragment-retries value to be %q, got %q", "1", lastVal)
			}
		}

		// --extractor-args "instagram:app_id=124024574287414" must be present.
		extractorIdxs := indexAllOf(args, "--extractor-args")
		if len(extractorIdxs) != 1 {
			t.Errorf("expected --extractor-args to appear exactly 1x, got %d", len(extractorIdxs))
		}
		if len(extractorIdxs) >= 1 {
			val := args[extractorIdxs[0]+1]
			if val != "instagram:app_id=124024574287414" {
				t.Errorf("expected --extractor-args value %q, got %q", "instagram:app_id=124024574287414", val)
			}
		}
	})

	t.Run("non-IG URL: only throttle defaults remain", func(t *testing.T) {
		ytURL := "https://www.youtube.com/watch?v=abc"
		args := append(throttleArgs(), cookieArgs("")...)
		args = append(args, igExtractorArgs(ytURL)...)

		// --user-agent / --retries / --fragment-retries each appear exactly
		// once (only the throttleArgs defaults; igExtractorArgs returns nil
		// for non-IG).
		for _, flag := range []string{"--user-agent", "--retries", "--fragment-retries"} {
			idxs := indexAllOf(args, flag)
			if len(idxs) != 1 {
				t.Errorf("non-IG URL: expected %s to appear exactly 1x, got %d", flag, len(idxs))
			}
		}

		// --extractor-args must NOT appear.
		if len(indexAllOf(args, "--extractor-args")) != 0 {
			t.Errorf("non-IG URL: expected zero --extractor-args, got %v", indexAllOf(args, "--extractor-args"))
		}

		// Sanity: the single --user-agent must be the desktop Firefox value
		// from throttleArgs (i.e. NOT the iOS UA — otherwise the iOS pair
		// would be leaking to non-IG traffic, the inverse regression).
		uaIdxs := indexAllOf(args, "--user-agent")
		if len(uaIdxs) >= 1 {
			val := args[uaIdxs[0]+1]
			if strings.HasPrefix(val, "Instagram ") {
				t.Errorf("non-IG URL: --user-agent leaked iOS value %q", val)
			}
		}
	})
}

// TestMinIGGapValue guards the minIGGap baseline against accidental drift.
// 15s is the production-tuned value (see comment in downloader.go): ~50%
// headroom under the observed flag rate of ~10 req/min. If a future refactor
// drops it back to 8s or below, this test fails — flagging would resume.
func TestMinIGGapValue(t *testing.T) {
	if minIGGap < 15*time.Second {
		t.Errorf("minIGGap = %v, want >= 15s (production-tuned baseline for IG anti-flag posture)", minIGGap)
	}
}

// indexAllOf returns the indices of every occurrence of target in haystack.
// Used by TestIgArgsOverrideThrottle to assert "exactly twice" / "last wins"
// invariants without committing to the exact slice ordering.
func indexAllOf(haystack []string, target string) []int {
	var out []int
	for i, s := range haystack {
		if s == target {
			out = append(out, i)
		}
	}
	return out
}

// TestIsInstagramSinglePost covers the URL classifier used by engine.IsPlaylist
// to skip yt-dlp preflight for guaranteed-single-video IG URLs. Table-driven
// across the two canonical single-video post types (/reel/, /tv/),
// non-single-post IG paths (explore, saved, stories, root), confusable hosts,
// and parse-error edges.
//
// `/p/<id>` URLs are classified as NEGATIVE because Instagram serves both
// single posts and carousel/sidecar posts (multiple media items) under `/p/`.
// Short-circuiting all `/p/` URLs as single-video would silently drop carousel
// items past the first; instead `/p/` URLs go through GetPlaylistInfo so
// ProcessPlaylist can extract all carousel media.
func TestIsInstagramSinglePost(t *testing.T) {
	tests := []struct {
		name   string
		rawURL string
		want   bool
	}{
		// Positive: guaranteed-single-video URLs.
		{"/reel/<id>/", "https://www.instagram.com/reel/CYYYYY/", true},
		{"/reel/<id> no trailing slash", "https://instagram.com/reel/CYYYYY", true},
		{"/tv/<id>/", "https://www.instagram.com/tv/CZZZZZ/", true},
		{"/reel/<id>/ with utm tracking", "https://www.instagram.com/reel/CYYYYY/?utm_source=ig", true},
		{"m.instagram.com mobile subdomain", "https://m.instagram.com/reel/CYYYYY/", true},
		{"bare instagram.com (no www) reel", "https://instagram.com/reel/CYYYYY/", true},
		// Uppercase host coverage. url.URL.Hostname() does NOT normalize
		// case, so without explicit lowercasing these would fall through
		// to GetPlaylistInfo (the Layer-0 gate lowercases, so they ARE
		// IG-classified upstream — mismatched behavior here would re-add
		// the second yt-dlp hit Layer 2 was designed to eliminate).
		{"all-caps host WWW.INSTAGRAM.COM /reel/", "https://WWW.INSTAGRAM.COM/reel/xyz", true},

		// Negative: /p/ URLs — may be carousels (multiple media items),
		// so MUST go through GetPlaylistInfo so ProcessPlaylist can extract
		// all entries. Classifying them as single-video would force the
		// `--no-playlist` download path and silently drop carousel items
		// past the first. Single-post `/p/` URLs pay the extra metadata
		// fetch to make this correct.
		{"/p/<id>/ (may be carousel)", "https://www.instagram.com/p/CXXXXX/", false},
		{"/p/<id> no trailing slash (may be carousel)", "https://www.instagram.com/p/CXXXXX", false},
		{"/p/<id>/ with query string (may be carousel)", "https://www.instagram.com/p/CXXXXX/?igsh=abc123", false},
		{"/p/<id>/sub-path (may be carousel)", "https://www.instagram.com/p/CXXXXX/embed/", false},
		{"m.instagram.com /p/ (may be carousel)", "https://m.instagram.com/p/CXXXXX/", false},
		{"bare instagram.com /p/ (may be carousel)", "https://instagram.com/p/CXXXXX/", false},
		{"capitalized host Instagram.com /p/ (may be carousel)", "https://Instagram.com/p/abc/", false},

		// Negative: IG host but not a single-post path.
		{"root path", "https://www.instagram.com/", false},
		{"/explore/", "https://www.instagram.com/explore/", false},
		{"/<username>/saved/", "https://www.instagram.com/some_user/saved/", false},
		{"/stories/<user>/<id>", "https://www.instagram.com/stories/some_user/123456/", false},
		{"/<username> profile only", "https://www.instagram.com/some_user/", false},
		{"/accounts/login", "https://www.instagram.com/accounts/login/", false},

		// Negative: confusable hosts that must NOT match (security-critical).
		{"evilinstagram.com host", "https://evilinstagram.com/reel/CXXXXX/", false},
		{"instagram.com.evil.com host", "https://instagram.com.evil.com/reel/CXXXXX/", false},
		{"non-IG host with /reel/ path", "https://example.com/reel/CXXXXX/", false},

		// Negative: non-IG hosts entirely.
		{"youtube watch URL", "https://www.youtube.com/watch?v=abc123", false},
		{"tiktok URL", "https://www.tiktok.com/@user/video/12345", false},

		// Edge: parse errors must return false (not panic, not true).
		{"malformed URL with IPv6 bracket", "http://[::1", false},
		{"empty string", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsInstagramSinglePost(tt.rawURL); got != tt.want {
				t.Errorf("IsInstagramSinglePost(%q) = %v, want %v", tt.rawURL, got, tt.want)
			}
		})
	}
}

// TestIsInstagramPotentialCarousel covers the URL classifier used by bot and
// api callers to decide whether an IsPlaylist preflight error must be
// surfaced to the user. The check matches Instagram `/p/<id>` URLs — the
// post type that may be a carousel/sidecar containing multiple media items.
//
// Symmetric with TestIsInstagramSinglePost: the two regexes split Instagram
// post URLs into "guaranteed single" (/reel/, /tv/) vs "maybe carousel"
// (/p/). A URL that lands in neither (profile pages, /explore/, /stories/,
// non-IG hosts) returns false from both.
func TestIsInstagramPotentialCarousel(t *testing.T) {
	tests := []struct {
		name   string
		rawURL string
		want   bool
	}{
		// Positive: /p/<id> URLs — may be carousels, error MUST propagate.
		{"/p/<id>/", "https://www.instagram.com/p/CXXXXX/", true},
		{"/p/<id> no trailing slash", "https://www.instagram.com/p/CXXXXX", true},
		{"/p/<id>/ with utm tracking", "https://www.instagram.com/p/CXXXXX/?utm_source=ig", true},
		{"/p/<id>/ with igsh query", "https://www.instagram.com/p/CXXXXX/?igsh=abc123", true},
		{"/p/<id>/sub-path", "https://www.instagram.com/p/CXXXXX/embed/", true},
		{"m.instagram.com /p/", "https://m.instagram.com/p/CXXXXX/", true},
		{"bare instagram.com /p/", "https://instagram.com/p/CXXXXX/", true},
		// Uppercase host coverage — host matching lowercases via
		// isInstagramHost, so this MUST classify as carousel-candidate.
		{"all-caps host WWW.INSTAGRAM.COM /p/", "https://WWW.INSTAGRAM.COM/p/xyz", true},
		{"capitalized host Instagram.com /p/", "https://Instagram.com/p/abc/", true},

		// Negative: /reel/ and /tv/ are guaranteed-single-video — preflight
		// errors for these are benign because the single-video fallthrough
		// will correctly download the one video.
		{"/reel/<id>/", "https://www.instagram.com/reel/CYYYYY/", false},
		{"/tv/<id>/", "https://www.instagram.com/tv/CZZZZZ/", false},

		// Negative: IG host but not a post path — these never reach
		// IsPlaylist with a preflight error in practice, but the classifier
		// must still return false to avoid spurious error surfacing.
		{"root path", "https://www.instagram.com/", false},
		{"/explore/", "https://www.instagram.com/explore/", false},
		{"/<username>/saved/", "https://www.instagram.com/some_user/saved/", false},
		{"/stories/<user>/<id>", "https://www.instagram.com/stories/some_user/123456/", false},
		{"/<username> profile only", "https://www.instagram.com/some_user/", false},

		// Negative: confusable hosts that must NOT match (security-critical).
		// A `/p/` URL on evilinstagram.com is a non-IG URL — error propagation
		// here would attribute non-IG yt-dlp failures to Instagram.
		{"evilinstagram.com /p/", "https://evilinstagram.com/p/CXXXXX/", false},
		{"instagram.com.evil.com /p/", "https://instagram.com.evil.com/p/CXXXXX/", false},
		{"non-IG host with /p/ path", "https://example.com/p/CXXXXX/", false},

		// Negative: non-IG hosts entirely.
		{"youtube watch URL", "https://www.youtube.com/watch?v=abc123", false},
		{"tiktok URL", "https://www.tiktok.com/@user/video/12345", false},

		// Edge: parse errors must return false (not panic, not true).
		{"malformed URL with IPv6 bracket", "http://[::1", false},
		{"empty string", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsInstagramPotentialCarousel(tt.rawURL); got != tt.want {
				t.Errorf("IsInstagramPotentialCarousel(%q) = %v, want %v", tt.rawURL, got, tt.want)
			}
		})
	}
}

// TestIsIGRateLimit covers the rate-limit detector used by the cookies
// fallback retry and the cooldown wiring. Table-driven across the three
// production IG error strings, wrapped errors (so retry logic detects
// fmt.Errorf("%w") chains), and negative cases that must NOT trigger
// cooldown (random go errors, nil, generic exit-status-1).
func TestIsIGRateLimit(t *testing.T) {
	// The three production strings we must detect. Sourced from yt-dlp's
	// captured stderr surfaced by formatYtdlpError.
	const (
		ratelimitMsg = "rate-limit reached or login required"
		http429Msg   = "HTTP Error 429"
		loginReqMsg  = "login required"
	)

	tests := []struct {
		name string
		err  error
		want bool
	}{
		// Positive: bare matches against the three substring patterns.
		{
			"IG rate-limit string",
			errors.New("ERROR: [Instagram] DXXX: Requested content is not available, rate-limit reached or login required"),
			true,
		},
		{
			"HTTP 429 from yt-dlp",
			errors.New("ERROR: unable to download video data: HTTP Error 429: Too Many Requests"),
			true,
		},
		{
			"generic login required",
			errors.New("ERROR: [Instagram] story: login required to view this story"),
			true,
		},
		// yt-dlp's story extractor capitalizes the L in "Login required"
		// (whereas the post extractor uses lowercase). Both must match —
		// otherwise the capital-L variant silently bypasses the cookies
		// retry and the cooldown for IG story errors.
		{
			"capital-L Login required (story)",
			errors.New("ERROR: [Instagram:story] 12345: Login required to view this content"),
			true,
		},
		// Sanity: mixed-case HTTP error 429.
		{
			"mixed-case HTTP error 429",
			errors.New("ERROR: unable to download video data: http error 429: Too Many Requests"),
			true,
		},

		// Positive: wrapped errors. formatYtdlpError uses fmt.Errorf with %w
		// in production, so the detector must walk the error chain (which
		// err.Error() does implicitly because fmt.Errorf includes the wrapped
		// message in the formatted string).
		{
			"wrapped IG rate-limit",
			fmt.Errorf("download failed: %w", errors.New("rate-limit reached or login required")),
			true,
		},
		{
			"double-wrapped HTTP 429",
			fmt.Errorf("layer 2: %w", fmt.Errorf("layer 1: %w", errors.New("HTTP Error 429"))),
			true,
		},
		{
			"formatYtdlpError-style wrap (mimics production)",
			fmt.Errorf("%w - %s", errors.New("exit status 1"), "ERROR: [Instagram] DXXX: rate-limit reached or login required"),
			true,
		},

		// Negative: nil err is the easy false case.
		{"nil error", nil, false},

		// Negative: generic exit-status / unrelated errors must NOT match.
		{"generic exit status 1", errors.New("exit status 1"), false},
		{"random go error", errors.New("connection refused"), false},
		{"yt-dlp non-rate-limit stderr", fmt.Errorf("%w - %s", errors.New("exit status 1"), "ERROR: [generic] Unsupported URL"), false},
		{"empty error message", errors.New(""), false},
		// Negative regression (codex iter-5): the benign sentinel returned by
		// GetPlaylistInfo when a /p/ URL turns out to be a single post must
		// NOT trip the rate-limit detector. bot/api use IsIGRateLimit to
		// decide whether to surface a /p/ IsPlaylist error vs fall through
		// to single-video Process; surfacing this sentinel would reject
		// every valid single /p/ post with a misleading "failed to check
		// Instagram carousel" error.
		{"not a playlist sentinel", errors.New("not a playlist - single video detected"), false},
		{"wrapped not-a-playlist sentinel", fmt.Errorf("failed to get playlist info: %w", errors.New("not a playlist - single video detected")), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsIGRateLimit(tt.err); got != tt.want {
				t.Errorf("IsIGRateLimit(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}

	// Sanity: ensure each documented substring is independently detected,
	// so adding/removing one constant in the helper trips this test.
	t.Run("each documented substring is independently detected", func(t *testing.T) {
		for _, sub := range []string{ratelimitMsg, http429Msg, loginReqMsg} {
			if !IsIGRateLimit(errors.New("prefix: " + sub + " :suffix")) {
				t.Errorf("documented substring %q not detected by IsIGRateLimit", sub)
			}
		}
	})
}

// TestNoteIGRateLimit covers the cooldown stamp set by noteIGRateLimit and
// how waitForIGSlot consumes it. Direct field manipulation lets us assert
// timing windows without sleeping the full igCooldown (5min) in real time.
func TestNoteIGRateLimit(t *testing.T) {
	t.Run("noteIGRateLimit sets cooldown deadline approximately now+igCooldown", func(t *testing.T) {
		d := &Downloader{}
		before := time.Now()
		d.noteIGRateLimit()
		expected := before.Add(igCooldown)
		drift := d.igCooldownUntil.Sub(expected)
		if drift < -100*time.Millisecond || drift > 100*time.Millisecond {
			t.Errorf("igCooldownUntil drift = %v, want within +/-100ms (expected=%v, got=%v)",
				drift, expected, d.igCooldownUntil)
		}
	})

	t.Run("waitForIGSlot honors active cooldown when minIGGap already elapsed", func(t *testing.T) {
		d := &Downloader{}
		// Gap already elapsed → minIGGap-only path would pass immediately.
		d.igLastAt = time.Now().Add(-(minIGGap + 5*time.Second))
		// But cooldown active with 400ms remaining must force a wait.
		d.igCooldownUntil = time.Now().Add(400 * time.Millisecond)

		ctx := context.Background()
		before := time.Now()
		err := d.waitForIGSlot(ctx, "https://instagram.com/p/abc/", nil)
		elapsed := time.Since(before)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if elapsed < 300*time.Millisecond {
			t.Errorf("expected wait ~400ms (cooldown), got %v (too short)", elapsed)
		}
		if elapsed > 2*time.Second {
			t.Errorf("expected wait ~400ms (cooldown), got %v (too long)", elapsed)
		}
	})

	t.Run("waitForIGSlot uses MAX of minIGGap-remaining and cooldown-remaining", func(t *testing.T) {
		d := &Downloader{}
		// minIGGap would require ~200ms more; cooldown requires ~600ms more.
		// The cooldown remaining is longer, so it must win.
		d.igLastAt = time.Now().Add(-(minIGGap - 200*time.Millisecond))
		d.igCooldownUntil = time.Now().Add(600 * time.Millisecond)

		ctx := context.Background()
		before := time.Now()
		err := d.waitForIGSlot(ctx, "https://instagram.com/p/abc/", nil)
		elapsed := time.Since(before)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Should wait the longer of the two (~600ms), not the shorter (~200ms).
		if elapsed < 500*time.Millisecond {
			t.Errorf("expected wait ~600ms (cooldown wins), got %v (too short — only minIGGap honored?)", elapsed)
		}
		if elapsed > 2*time.Second {
			t.Errorf("expected wait ~600ms (cooldown wins), got %v (too long)", elapsed)
		}
	})

	t.Run("expired cooldown is ignored (falls back to minIGGap-only path)", func(t *testing.T) {
		d := &Downloader{}
		// Both gap elapsed AND cooldown already expired → fast path.
		d.igLastAt = time.Now().Add(-(minIGGap + 5*time.Second))
		d.igCooldownUntil = time.Now().Add(-10 * time.Second)

		ctx := context.Background()
		before := time.Now()
		err := d.waitForIGSlot(ctx, "https://instagram.com/p/abc/", nil)
		elapsed := time.Since(before)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if elapsed > fastPathBudget {
			t.Errorf("expired cooldown caused unexpected wait of %v, want <%v", elapsed, fastPathBudget)
		}
	})

	t.Run("non-IG URLs ignore cooldown entirely", func(t *testing.T) {
		d := &Downloader{}
		// Long active cooldown — non-IG URL must NOT honor it.
		d.igCooldownUntil = time.Now().Add(1 * time.Hour)

		ctx := context.Background()
		before := time.Now()
		err := d.waitForIGSlot(ctx, "https://youtube.com/watch?v=abc", nil)
		elapsed := time.Since(before)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if elapsed > fastPathBudget {
			t.Errorf("non-IG URL waited %v despite cooldown applying only to IG, want <%v", elapsed, fastPathBudget)
		}
	})

	t.Run("second noteIGRateLimit slides deadline by elapsed-since-first, not by igCooldown", func(t *testing.T) {
		d := &Downloader{}
		d.noteIGRateLimit()
		firstDeadline := d.igCooldownUntil

		// Sleep enough that the second call's now() is measurably later than
		// the first's, then verify the deadline moved with it.
		time.Sleep(20 * time.Millisecond)
		d.noteIGRateLimit()
		secondDeadline := d.igCooldownUntil

		if !secondDeadline.After(firstDeadline) {
			t.Errorf("second noteIGRateLimit did not slide deadline forward: first=%v, second=%v",
				firstDeadline, secondDeadline)
		}
		// Sanity: each call sets to now+igCooldown (not first+igCooldown +
		// extra), so the drift between deadlines must roughly match the sleep
		// (~20ms), NOT accumulate the full igCooldown twice.
		drift := secondDeadline.Sub(firstDeadline)
		if drift > 5*time.Second {
			t.Errorf("deadline drift = %v, expected ~20ms (sliding window, not stacking)", drift)
		}
	})

	t.Run("progress emission reflects cooldown-aware wait, not just minIGGap", func(t *testing.T) {
		d := &Downloader{}
		// minIGGap elapsed; cooldown forces a ~1s wait.
		d.igLastAt = time.Now().Add(-(minIGGap + 5*time.Second))
		d.igCooldownUntil = time.Now().Add(1 * time.Second)
		r := &progressRecorder{}

		ctx := context.Background()
		err := d.waitForIGSlot(ctx, "https://instagram.com/p/abc/", r.cb)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		events := r.snapshot()
		if len(events) != 1 {
			t.Fatalf("expected 1 queued event, got %d: %+v", len(events), events)
		}
		if events[0].Phase != "queued" {
			t.Errorf("expected Phase=queued, got %q", events[0].Phase)
		}
		// ETA must reflect ~1s (the cooldown remaining), not ~minIGGap.
		// Parse the rounded duration back; tolerate "1s" or "2s" rounding.
		eta, parseErr := time.ParseDuration(events[0].ETA)
		if parseErr != nil {
			t.Fatalf("unparseable ETA %q: %v", events[0].ETA, parseErr)
		}
		if eta < 500*time.Millisecond || eta > 3*time.Second {
			t.Errorf("ETA=%v, expected ~1s (cooldown-aware), got %v — not minIGGap (%v)",
				eta, eta, minIGGap)
		}
	})

	// Regression test for codex iter-5 race: a goroutine already waiting for
	// a minIGGap slot must observe a cooldown that lands DURING its sleep,
	// rather than waking on its original timer and proceeding into yt-dlp
	// during the new cooldown.
	t.Run("cooldown stamped mid-sleep extends the wait", func(t *testing.T) {
		d := &Downloader{}
		// 400ms remaining on the minIGGap spacing; no cooldown yet.
		d.igLastAt = time.Now().Add(-(minIGGap - 400*time.Millisecond))

		done := make(chan error, 1)
		start := time.Now()
		go func() {
			done <- d.waitForIGSlot(context.Background(), "https://instagram.com/p/abc/", nil)
		}()

		// Wait ~150ms (the goroutine is now sleeping with ~250ms to go),
		// then stamp a cooldown deadline 800ms into the future — this is
		// strictly past the original wake time. Without the re-check loop
		// the goroutine would return at ~start+400ms; WITH the re-check it
		// must keep waiting until at least start+150ms+800ms = ~start+950ms.
		time.Sleep(150 * time.Millisecond)
		d.igMu.Lock()
		d.igCooldownUntil = time.Now().Add(800 * time.Millisecond)
		d.igMu.Unlock()

		select {
		case err := <-done:
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// Must have waited past the cooldown extension. Floor is
			// ~950ms; allow a small jitter window.
			if elapsed < 850*time.Millisecond {
				t.Errorf("waitForIGSlot returned in %v; expected >=850ms (cooldown set mid-sleep was ignored)", elapsed)
			}
			// Ceiling sanity: must not have waited an unbounded extra slot.
			if elapsed > 2*time.Second {
				t.Errorf("waitForIGSlot returned in %v; expected ~1s (cooldown re-check should not spin)", elapsed)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("waitForIGSlot did not return within 3s (deadlock?)")
		}
	})

	// Companion to the race test: confirm that when NO cooldown is stamped
	// during the sleep, the gate returns promptly on the original timer
	// (the re-check loop must not introduce phantom waits).
	t.Run("no cooldown mid-sleep returns on original timer", func(t *testing.T) {
		d := &Downloader{}
		d.igLastAt = time.Now().Add(-(minIGGap - 400*time.Millisecond))

		start := time.Now()
		err := d.waitForIGSlot(context.Background(), "https://instagram.com/p/abc/", nil)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if elapsed < 300*time.Millisecond {
			t.Errorf("waitForIGSlot returned in %v; expected ~400ms (gap honored)", elapsed)
		}
		if elapsed > 700*time.Millisecond {
			t.Errorf("waitForIGSlot returned in %v; expected ~400ms (re-check loop added phantom wait)", elapsed)
		}
	})
}

// TestIGCooldownConstant locks the cooldown constant so accidental edits trip
// the test. The value is load-bearing (operator-tuned for IG flag-recovery
// window per plan).
func TestIGCooldownConstant(t *testing.T) {
	if igCooldown != 5*time.Minute {
		t.Errorf("igCooldown = %v, want 5m", igCooldown)
	}
}

// TestShouldRetryWithCookies pins the pure-logic decision used by
// runWithCookieFallback to gate the cookies retry. The matrix covers the
// four interesting combinations of (err is/isn't IG-rate-limit) × (cookies
// path empty/non-empty) plus a nil-err sanity case.
//
// Behavior contract (per plan, Layer 1):
//   - retry ONLY when the anonymous attempt failed AND we actually have
//     cookies to retry with. Without cookies, retrying anonymously again
//     would just repeat the same outcome — fail fast instead so the caller
//     surfaces the original error to the user.
//   - retry ONLY on IG-rate-limit / login-required errors. Generic network
//     / extractor / unsupported-URL errors are not auth-fixable and would
//     just waste an IG slot.
func TestShouldRetryWithCookies(t *testing.T) {
	const (
		igRLMsg    = "rate-limit reached or login required"
		genericMsg = "ERROR: [generic] Unsupported URL"
	)

	tests := []struct {
		name        string
		err         error
		cookiesPath string
		want        bool
	}{
		// Nil error: caller should not be calling this at all on success,
		// but the helper must still return false defensively.
		{"nil err, no cookies", nil, "", false},
		{"nil err, with cookies", nil, "/path/cookies.txt", false},

		// IG rate-limit + cookies: the one true-case. This is the path that
		// runWithCookieFallback uses to trigger the cookies retry.
		{"IG rate-limit + cookies path", errors.New(igRLMsg), "/path/cookies.txt", true},
		{
			"wrapped IG rate-limit + cookies path",
			fmt.Errorf("download failed: %w", errors.New(igRLMsg)),
			"/path/cookies.txt",
			true,
		},
		{
			"HTTP 429 + cookies path",
			errors.New("HTTP Error 429: Too Many Requests"),
			"/path/cookies.txt",
			true,
		},
		{
			"login required + cookies path",
			errors.New("ERROR: [Instagram] story: login required"),
			"/path/cookies.txt",
			true,
		},

		// IG rate-limit but no cookies configured: retry is pointless.
		// This is the bot-without-SUSHE_COOKIES case — anonymous-only mode
		// must surface the error to the caller rather than spinning.
		{"IG rate-limit, empty cookies", errors.New(igRLMsg), "", false},
		{
			"wrapped IG rate-limit, empty cookies",
			fmt.Errorf("download failed: %w", errors.New(igRLMsg)),
			"",
			false,
		},

		// Non-IG errors: cookies don't help with extractor / network / URL
		// problems, so no retry regardless of cookies-path state.
		{"generic error + cookies path", errors.New(genericMsg), "/path/cookies.txt", false},
		{"generic error, empty cookies", errors.New(genericMsg), "", false},
		{"connection refused + cookies", errors.New("connection refused"), "/path/cookies.txt", false},
		{"exit status 1 + cookies", errors.New("exit status 1"), "/path/cookies.txt", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldRetryWithCookies(tt.err, tt.cookiesPath); got != tt.want {
				t.Errorf("shouldRetryWithCookies(%v, %q) = %v, want %v",
					tt.err, tt.cookiesPath, got, tt.want)
			}
		})
	}
}

// installFakeYtdlp writes a shell script named "yt-dlp" into a temp dir and
// prepends that dir to PATH for the duration of the test. The script writes
// the literal argv it received (one per line) to outFile and exits with
// exitCode (with optional stderr text). The fake never actually downloads
// anything; runWithCookieFallback just sees the exit + stderr like the real
// binary would. Returns the path to outFile so the test can assert on the
// captured args.
//
// This is the load-bearing piece of the codex-iter-1 regression tests: we
// need to see EXACTLY which yt-dlp invocations happen for an IG vs non-IG
// URL (anonymous-first dance vs cookies-up-front) without taking on the
// flakiness of mocking exec.Cmd. PATH-shim is the standard Go approach.
func installFakeYtdlp(t *testing.T, exitCode int, stderr string) string {
	t.Helper()
	dir := t.TempDir()
	outFile := filepath.Join(dir, "calls.log")
	// Each invocation APPENDS its argv block separated by "---\n" so the
	// test can count invocations and inspect each separately. Using `printf`
	// not `echo -e` because the latter is non-portable across /bin/sh
	// implementations.
	script := fmt.Sprintf(`#!/bin/sh
{
  for a in "$@"; do
    printf '%%s\n' "$a"
  done
  printf '%%s\n' '---'
} >> %q
printf '%%s' %q 1>&2
exit %d
`, outFile, stderr, exitCode)
	scriptPath := filepath.Join(dir, "yt-dlp")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake yt-dlp: %v", err)
	}
	// Prepend dir to PATH. t.Setenv handles restore automatically.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return outFile
}

// readFakeYtdlpInvocations parses the call log written by the fake yt-dlp
// into one []string per invocation, in chronological order.
func readFakeYtdlpInvocations(t *testing.T, logPath string) [][]string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read fake yt-dlp log: %v", err)
	}
	var calls [][]string
	var current []string
	for _, line := range strings.Split(string(data), "\n") {
		if line == "---" {
			calls = append(calls, current)
			current = nil
			continue
		}
		if line == "" {
			continue
		}
		current = append(current, line)
	}
	return calls
}

// hasArg returns true if args contains the literal `--cookies` flag.
func hasCookiesArg(args []string) bool {
	return slices.Contains(args, "--cookies")
}

// TestRunWithCookieFallback_NonIGUsesCookiesUpFront pins the regression fix
// for codex iter-1: non-Instagram URLs (YouTube, Twitter, TikTok, etc.) must
// receive `--cookies` on their FIRST and only yt-dlp invocation. Pre-fix,
// every URL was forced through the anonymous-first dance — sites that
// require auth (YouTube Premium, age-gated, login-walled) would fail the
// anonymous attempt and never retry with cookies (because the retry was
// gated on IsIGRateLimit, which doesn't fire for, e.g., generic 403).
//
// This test exits the fake yt-dlp with success so the helper takes the
// happy path; the assertion is purely about which args reached the binary
// on which attempts. A failure of this test means the regression is back.
func TestRunWithCookieFallback_NonIGUsesCookiesUpFront(t *testing.T) {
	logPath := installFakeYtdlp(t, 0, "")
	workDir := t.TempDir()
	d := &Downloader{
		downloadDir: workDir,
		timeout:     30 * time.Second,
		cookiesPath: "/fake/cookies.txt",
	}
	const ytURL = "https://www.youtube.com/watch?v=abc123"

	if err := d.runWithCookieFallback(context.Background(), ytURL, workDir, []string{ytURL}, nil); err != nil {
		t.Fatalf("runWithCookieFallback: %v", err)
	}

	calls := readFakeYtdlpInvocations(t, logPath)
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 yt-dlp invocation for non-IG URL, got %d: %v", len(calls), calls)
	}
	if !hasCookiesArg(calls[0]) {
		t.Errorf("non-IG URL invocation must include --cookies on the first attempt; got args=%v", calls[0])
	}
}

// TestRunWithCookieFallback_IGAnonymousFirst pins the IG-only anonymous-first
// behavior: an IG URL must trigger an anonymous attempt FIRST (no --cookies
// flag), regardless of cookiesPath state. With a success exit, only one
// invocation should occur and it must NOT carry --cookies.
func TestRunWithCookieFallback_IGAnonymousFirst(t *testing.T) {
	logPath := installFakeYtdlp(t, 0, "")
	workDir := t.TempDir()
	d := &Downloader{
		downloadDir: workDir,
		timeout:     30 * time.Second,
		cookiesPath: "/fake/cookies.txt",
	}
	const igURL = "https://www.instagram.com/reel/ABC123/"

	if err := d.runWithCookieFallback(context.Background(), igURL, workDir, []string{igURL}, nil); err != nil {
		t.Fatalf("runWithCookieFallback: %v", err)
	}

	calls := readFakeYtdlpInvocations(t, logPath)
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 yt-dlp invocation for successful IG anonymous attempt, got %d: %v", len(calls), calls)
	}
	if hasCookiesArg(calls[0]) {
		t.Errorf("IG URL anonymous-first invocation must NOT include --cookies; got args=%v", calls[0])
	}
}

// TestRunWithCookieFallback_IGRetriesWithCookies pins the IG fallback path:
// when the anonymous attempt fails with an IG rate-limit error and cookies
// are configured, runWithCookieFallback must retry with --cookies. Both
// invocations must be observed.
func TestRunWithCookieFallback_IGRetriesWithCookies(t *testing.T) {
	logPath := installFakeYtdlp(t, 1, "ERROR: [Instagram] foo: Requested content is not available, rate-limit reached or login required")
	workDir := t.TempDir()
	d := &Downloader{
		downloadDir: workDir,
		timeout:     30 * time.Second,
		cookiesPath: "/fake/cookies.txt",
	}
	const igURL = "https://www.instagram.com/p/abc/"

	// Both attempts will fail (fake exits 1 every time), but the helper must
	// still issue the retry so we can observe it.
	_ = d.runWithCookieFallback(context.Background(), igURL, workDir, []string{igURL}, nil)

	calls := readFakeYtdlpInvocations(t, logPath)
	if len(calls) != 2 {
		t.Fatalf("expected 2 yt-dlp invocations (anonymous + cookies retry), got %d: %v", len(calls), calls)
	}
	if hasCookiesArg(calls[0]) {
		t.Errorf("IG first attempt must be anonymous; got args=%v", calls[0])
	}
	if !hasCookiesArg(calls[1]) {
		t.Errorf("IG retry attempt must include --cookies; got args=%v", calls[1])
	}
}

// TestRunWithCookieFallback_NonIGNoCooldown pins the second regression fix
// for codex iter-1: a non-IG URL emitting a generic rate-limit-like error
// string (HTTP 429 / login required — both common across yt-dlp extractors)
// must NOT trigger noteIGRateLimit. Pre-fix, the bot would stall all future
// IG downloads for 5 minutes any time a YouTube / Twitter / TikTok error
// happened to include those substrings.
func TestRunWithCookieFallback_NonIGNoCooldown(t *testing.T) {
	// Use an error string that DOES match IsIGRateLimit's patterns, to make
	// sure the gate is on URL host, not error content.
	installFakeYtdlp(t, 1, "ERROR: [youtube] foo: HTTP Error 429: Too Many Requests")
	workDir := t.TempDir()
	d := &Downloader{
		downloadDir: workDir,
		timeout:     30 * time.Second,
		cookiesPath: "/fake/cookies.txt",
	}
	const ytURL = "https://www.youtube.com/watch?v=abc"

	_ = d.runWithCookieFallback(context.Background(), ytURL, workDir, []string{ytURL}, nil)

	// igCooldownUntil must remain zero — the non-IG failure must not have
	// stamped the global IG cooldown.
	d.igMu.Lock()
	cooldown := d.igCooldownUntil
	d.igMu.Unlock()
	if !cooldown.IsZero() {
		t.Errorf("non-IG URL emitting 429 must NOT set igCooldownUntil; got %v (now=%v)", cooldown, time.Now())
	}
}

// TestRunWithCookieFallback_IGSetsCooldown pins the positive side of the
// gate: an IG URL hitting an IG-rate-limit error string must set the
// cooldown so subsequent IG callers back off.
func TestRunWithCookieFallback_IGSetsCooldown(t *testing.T) {
	installFakeYtdlp(t, 1, "ERROR: [Instagram] foo: Requested content is not available, rate-limit reached or login required")
	workDir := t.TempDir()
	d := &Downloader{
		downloadDir: workDir,
		timeout:     30 * time.Second,
		// Empty cookiesPath: no retry, single failing attempt, but the
		// cooldown stamp must still fire because the error matches.
		cookiesPath: "",
	}
	const igURL = "https://www.instagram.com/reel/abc/"

	_ = d.runWithCookieFallback(context.Background(), igURL, workDir, []string{igURL}, nil)

	d.igMu.Lock()
	cooldown := d.igCooldownUntil
	d.igMu.Unlock()
	if cooldown.IsZero() {
		t.Fatal("IG URL hitting rate-limit must set igCooldownUntil; got zero")
	}
	// Sanity-check the cooldown lands within (now, now+igCooldown+slack).
	now := time.Now()
	if cooldown.Before(now) || cooldown.After(now.Add(igCooldown+time.Second)) {
		t.Errorf("igCooldownUntil = %v, want within (%v, %v]", cooldown, now, now.Add(igCooldown+time.Second))
	}
}
