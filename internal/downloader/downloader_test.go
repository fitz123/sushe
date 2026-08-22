package downloader

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fitz123/sushe/internal/logger"
)

func TestNewYTDLPPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "empty uses PATH", path: "", want: "yt-dlp"},
		{name: "whitespace uses PATH", path: " \t\n", want: "yt-dlp"},
		{name: "configured path is trimmed", path: "  /opt/sushe/yt-dlp \t", want: "/opt/sushe/yt-dlp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := New("", tt.path)
			if d.ytdlpPath != tt.want {
				t.Errorf("New(%q).ytdlpPath = %q, want %q", tt.path, d.ytdlpPath, tt.want)
			}
		})
	}
}

func TestYTDLPTerminalErrorOwnDeadline(t *testing.T) {
	d := &Downloader{timeout: 37 * time.Millisecond}
	ctx, cancel := context.WithDeadlineCause(context.Background(), time.Now().Add(-time.Second), errYTDLPDeadline)
	defer cancel()
	<-ctx.Done()

	err := d.ytdlpTerminalError(ctx, errors.New("signal: killed"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ytdlpTerminalError() = %v, want context deadline exceeded", err)
	}
	if !strings.Contains(err.Error(), d.timeout.String()) {
		t.Fatalf("ytdlpTerminalError() = %q, want bound %q", err, d.timeout)
	}
}

func TestYTDLPTerminalErrorPreservesCallerCancellation(t *testing.T) {
	d := &Downloader{timeout: time.Hour}
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, cancelTimeout := context.WithTimeoutCause(parent, d.timeout, errYTDLPDeadline)
	cancelParent()
	defer cancelTimeout()
	<-ctx.Done()

	want := errors.New("signal: killed")
	if got := d.ytdlpTerminalError(ctx, want); got != want {
		t.Fatalf("ytdlpTerminalError() = %v, want original error %v", got, want)
	}
}

func TestDownloadReportsOwnSubprocessTimeoutBound(t *testing.T) {
	logger.Init("error")

	tmpDir := t.TempDir()
	executablePath := filepath.Join(tmpDir, "yt-dlp-stub")
	stub := "#!/bin/sh\nexec sleep 5\n"
	if err := os.WriteFile(executablePath, []byte(stub), 0755); err != nil {
		t.Fatalf("write yt-dlp stub: %v", err)
	}

	d := New("", executablePath)
	d.downloadDir = filepath.Join(tmpDir, "downloads")
	d.timeout = 25 * time.Millisecond

	startedAt := time.Now()
	_, err := d.Download(context.Background(), "https://example.com/video")
	if err == nil {
		t.Fatal("Download() error = nil, want subprocess deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Download() error = %v, want context deadline exceeded", err)
	}
	if !strings.Contains(err.Error(), "yt-dlp subprocess deadline exceeded after "+d.timeout.String()) {
		t.Fatalf("Download() error = %q, want per-download bound %q", err, d.timeout)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("Download() took %s, subprocess timeout was not enforced promptly", elapsed)
	}
}

func cancelWhenMarkerAppears(ctx context.Context, cancel context.CancelFunc, markerPath string) {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(markerPath); err == nil {
			cancel()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func TestDownloadStopsWhenVideoProbeContextIsCanceled(t *testing.T) {
	logger.Init("error")
	tmpDir := t.TempDir()
	ytdlpPath := filepath.Join(tmpDir, "yt-dlp-stub")
	if err := os.WriteFile(ytdlpPath, []byte("#!/bin/sh\nset -eu\n: > video.mp4\n"), 0755); err != nil {
		t.Fatalf("write yt-dlp stub: %v", err)
	}
	ffprobePath := filepath.Join(tmpDir, "ffprobe")
	probeMarker := filepath.Join(tmpDir, "probe-started")
	if err := os.WriteFile(ffprobePath, []byte("#!/bin/sh\nprintf 'started\\n' > \"$PROBE_MARKER\"\nexec sleep 5\n"), 0755); err != nil {
		t.Fatalf("write ffprobe stub: %v", err)
	}
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PROBE_MARKER", probeMarker)

	d := New("", ytdlpPath)
	d.downloadDir = filepath.Join(tmpDir, "downloads")
	d.timeout = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go cancelWhenMarkerAppears(ctx, cancel, probeMarker)

	startedAt := time.Now()
	_, err := d.Download(ctx, "https://example.com/video")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Download() error = %v, want context canceled", err)
	}
	if !strings.Contains(err.Error(), "video codec probe canceled") {
		t.Fatalf("Download() error = %q, want codec probe cancellation", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 3*time.Second {
		t.Fatalf("Download() took %s, ffprobe did not inherit the caller context", elapsed)
	}
}

func TestDownloadDoesNotIgnoreContextCancellationDuringFaststart(t *testing.T) {
	logger.Init("error")
	tmpDir := t.TempDir()
	ytdlpPath := filepath.Join(tmpDir, "yt-dlp-stub")
	if err := os.WriteFile(ytdlpPath, []byte("#!/bin/sh\nset -eu\n: > video.mp4\n"), 0755); err != nil {
		t.Fatalf("write yt-dlp stub: %v", err)
	}
	ffprobePath := filepath.Join(tmpDir, "ffprobe")
	if err := os.WriteFile(ffprobePath, []byte("#!/bin/sh\nprintf 'h264\\n'\n"), 0755); err != nil {
		t.Fatalf("write ffprobe stub: %v", err)
	}
	ffmpegPath := filepath.Join(tmpDir, "ffmpeg")
	faststartMarker := filepath.Join(tmpDir, "faststart-started")
	if err := os.WriteFile(ffmpegPath, []byte("#!/bin/sh\nprintf 'started\\n' > \"$FASTSTART_MARKER\"\nexec sleep 5\n"), 0755); err != nil {
		t.Fatalf("write ffmpeg stub: %v", err)
	}
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FASTSTART_MARKER", faststartMarker)

	d := New("", ytdlpPath)
	d.downloadDir = filepath.Join(tmpDir, "downloads")
	d.timeout = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go cancelWhenMarkerAppears(ctx, cancel, faststartMarker)

	_, err := d.Download(ctx, "https://example.com/video")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Download() error = %v, want context canceled", err)
	}
	if !strings.Contains(err.Error(), "faststart canceled") {
		t.Fatalf("Download() error = %q, want faststart cancellation instead of original-file fallback", err)
	}
}

func TestReencodeWaitsForProgressReader(t *testing.T) {
	logger.Init("error")
	tmpDir := t.TempDir()
	inputPath := filepath.Join(tmpDir, "video.mp4")
	if err := os.WriteFile(inputPath, []byte("video"), 0644); err != nil {
		t.Fatalf("write input: %v", err)
	}
	ffprobePath := filepath.Join(tmpDir, "ffprobe")
	ffprobeStub := `#!/bin/sh
printf '%s\n' '{"format":{"duration":"10","size":"5","bit_rate":"1"},"streams":[{"codec_type":"video","width":1,"height":1}]}'
`
	if err := os.WriteFile(ffprobePath, []byte(ffprobeStub), 0755); err != nil {
		t.Fatalf("write ffprobe stub: %v", err)
	}
	ffmpegPath := filepath.Join(tmpDir, "ffmpeg")
	if err := os.WriteFile(ffmpegPath, []byte("#!/bin/sh\nprintf 'time=00:00:01.00\\n' >&2\n"), 0755); err != nil {
		t.Fatalf("write ffmpeg stub: %v", err)
	}
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	callbackDone := make(chan struct{})
	d := New("", "")
	_, err := d.ReencodeToH264(context.Background(), inputPath, func(Progress) {
		time.Sleep(50 * time.Millisecond)
		close(callbackDone)
	})
	if err != nil {
		t.Fatalf("ReencodeToH264() error = %v", err)
	}
	select {
	case <-callbackDone:
	default:
		t.Fatal("ReencodeToH264 returned before its progress reader completed")
	}
}

func TestConfiguredYTDLPExecutableIsInvoked(t *testing.T) {
	logger.Init("error")

	tmpDir := t.TempDir()
	markerPath := filepath.Join(tmpDir, "invoked")
	expectedTMPDIR := filepath.Join(tmpDir, "downloads")
	executablePath := filepath.Join(tmpDir, "yt-dlp-stub")
	stub := `#!/bin/sh
set -eu
printf '%s\n' "$TMPDIR" > "$YTDLP_MARKER"
printf '%s\n' '{"id":"video-1","title":"Stub Video","url":"https://example.com/video-1","duration":1,"playlist_title":"Stub Playlist","playlist_id":"stub-playlist"}'
printf '%s\n' '{"id":"video-2","title":"Stub Video 2","url":"https://example.com/video-2","duration":1,"playlist_title":"Stub Playlist","playlist_id":"stub-playlist"}'
`
	if err := os.WriteFile(executablePath, []byte(stub), 0755); err != nil {
		t.Fatalf("write yt-dlp stub: %v", err)
	}
	t.Setenv("YTDLP_MARKER", markerPath)
	t.Setenv("TMPDIR", filepath.Join(tmpDir, "inherited"))

	d := New("", executablePath)
	d.downloadDir = expectedTMPDIR
	info, err := d.GetPlaylistInfo(context.Background(), "https://example.com/playlist")
	if err != nil {
		t.Fatalf("GetPlaylistInfo() error = %v", err)
	}
	if info.PlaylistCount != 2 || len(info.Entries) != 2 {
		t.Fatalf("GetPlaylistInfo() = %+v, want two parsed entries", info)
	}
	gotTMPDIR, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read configured executable marker: %v", err)
	}
	if got := strings.TrimSpace(string(gotTMPDIR)); got != expectedTMPDIR {
		t.Errorf("configured executable TMPDIR = %q, want %q", got, expectedTMPDIR)
	}
}

func TestDefaultYTDLPExecutableUsesPATH(t *testing.T) {
	tmpDir := t.TempDir()
	markerPath := filepath.Join(tmpDir, "invoked")
	executablePath := filepath.Join(tmpDir, "yt-dlp")
	stub := `#!/bin/sh
set -eu
printf 'invoked\n' > "$YTDLP_MARKER"
`
	if err := os.WriteFile(executablePath, []byte(stub), 0755); err != nil {
		t.Fatalf("write yt-dlp stub: %v", err)
	}
	t.Setenv("PATH", tmpDir)
	t.Setenv("YTDLP_MARKER", markerPath)

	d := New("", "")
	if err := d.ytdlpCommand(context.Background(), "").Run(); err != nil {
		t.Fatalf("run default yt-dlp executable: %v", err)
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("default executable was not resolved from PATH: %v", err)
	}
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
