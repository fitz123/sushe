package downloader

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fitz123/sushe/internal/logger"
)

// cookieArgs returns yt-dlp cookie flags when path is non-empty, else nil.
// The path is expected to be already trimmed by the caller (New canonicalizes
// it once at construction). Returns a fresh slice each call so callers can
// safely append onto it.
func cookieArgs(path string) []string {
	if path == "" {
		return nil
	}
	return []string{"--cookies", path}
}

// throttleArgs returns yt-dlp anti-flag throttling flags. Applied at every
// yt-dlp invocation site. The values are tuned for Instagram's anti-automation
// heuristics but harmless on other sites (yt-dlp ignores irrelevant flags).
//
//   - --sleep-requests 2: 2s gap between metadata/GraphQL requests within a
//     single invocation. Instagram's primary flag signal is request burst rate.
//   - --sleep-interval 2 / --max-sleep-interval 5: 2-5s random sleep between
//     downloads (matters for playlists; ignored for single videos).
//   - --retries 3 / --fragment-retries 3: lower than yt-dlp's default of 10.
//     Retry storms after a rate-limit response trigger harder bans; better to
//     fail fast and let the operator refresh cookies than to hammer the API.
//   - --socket-timeout 30: bound network hangs so a stuck request doesn't tie
//     up the IG rate-limit slot.
//   - --user-agent "Mozilla/5.0 ... Firefox/150.0": matches the desktop Firefox
//     browser where cookies were exported. yt-dlp's default UA is stale Chrome
//     95 — both a flag (no real human uses 4yr-old Chrome) and a mismatch with
//     cookies harvested from Firefox (cookie+UA mismatch is itself a flag).
//
// Returns a fresh slice each call so callers can safely append onto it.
func throttleArgs() []string {
	return []string{
		"--sleep-requests", "2",
		"--sleep-interval", "2",
		"--max-sleep-interval", "5",
		"--retries", "3",
		"--fragment-retries", "3",
		"--socket-timeout", "30",
		"--user-agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:150.0) Gecko/20100101 Firefox/150.0",
	}
}

// Progress represents download progress information
type Progress struct {
	Phase      string  // "queued", "downloading", "processing", "merging", "encoding", "splitting", "uploading"
	Percent    float64 // 0-100
	Speed      string  // e.g., "2.50MiB/s"
	ETA        string  // e.g., "00:30"
	Downloaded string  // e.g., "25.00MiB"
	Total      string  // e.g., "50.00MiB"
	PartNum    int     // Current part number (for splitting/uploading)
	TotalParts int     // Total parts (for splitting)
	Codec      string  // Original codec (e.g., "h264", "vp9", "av1") - shown when converting
}

// ProgressCallback is called with progress updates
type ProgressCallback func(Progress)

const (
	// Local Bot API server allows up to 2GB uploads
	MaxFileSize    = 2000 * 1024 * 1024 // 2GB in bytes
	MaxUploadSize  = 1900 * 1024 * 1024 // 1.9GB - threshold for whether to split
	MaxSplitSize   = 1700 * 1024 * 1024 // 1.7GB - split target with keyframe overshoot margin
	DownloadDir    = "/tmp/sushe"
	DefaultTimeout = 60 * time.Minute // Increased for long videos

	// Playlist limits
	MaxPlaylistVideos = 50             // Maximum videos per playlist
	MaxVideoDuration  = 2 * time.Hour  // Skip videos longer than 2 hours

	// minIGGap is the minimum spacing between Instagram-bound yt-dlp invocations
	// across all goroutines. yt-dlp's --sleep-interval only governs intervals
	// within a single invocation; a bot serving multiple Telegram users can
	// still fire N IG requests in 2 seconds when users paste URLs concurrently.
	// Instagram flags such bursts hardest, so a process-wide mutex enforces
	// this gap.
	//
	// 15s baseline: production logs showed flagging after ~8 successes per 46s
	// (~10 req/min). 15s spacing gives ~8 successes in 105s (~4.6 req/min),
	// ~50% headroom under the observed flag rate. Tune up if flagging persists,
	// down if users complain about latency.
	minIGGap = 15 * time.Second

	// igCooldown is the global pause applied to Instagram-bound yt-dlp
	// invocations after yt-dlp surfaces an IG rate-limit / login-required
	// error (see isIGRateLimit). Without a cooldown, the bot keeps hammering
	// IG every minIGGap when the account is already flagged, which deepens
	// the flag. 5 minutes gives IG's rate-limit window time to clear without
	// requiring operator intervention on transient flags.
	//
	// Cooldown is reset (slid forward) every time noteIGRateLimit is called,
	// so consecutive rate-limit responses keep the bot quiet rather than
	// resuming after exactly 5 minutes from the first flag.
	igCooldown = 5 * time.Minute

	// igIosAppID is the X-IG-App-ID value yt-dlp surfaces via
	// `--extractor-args "instagram:app_id=..."` (yt-dlp PR #12359). The default
	// `936619743392459` is the web-app id; web-app sessions from a Hetzner
	// datacenter range look maximally suspicious to IG. `124024574287414` is
	// the iOS-app id, which routes requests to a different IG endpoint cluster
	// with a different rate profile. Must be paired with an iOS-app UA — a
	// mismatched UA + app_id pair is itself a flag signal.
	igIosAppID = "124024574287414"
	// igIosUA matches the iOS-app id above. App version 339.0.0.12.95 was a
	// real mid-2024 release; refresh annually to avoid looking like a stale
	// client. Build-id suffix (e.g. "; 580058661") intentionally omitted — the
	// X-IG-App-ID header is the dominant signal for IG's endpoint routing.
	igIosUA = "Instagram 339.0.0.12.95 (iPhone16,1; iOS 18_2; en_US; en_US; scale=3.00; gamut=normal; 1179x2556) AppleWebKit/420+"
)

// MediaInfo contains video metadata from ffprobe
type MediaInfo struct {
	Duration float64 // seconds
	Bitrate  int64   // bits per second
	FileSize int64   // bytes
	Width    int     // video width in pixels
	Height   int     // video height in pixels
}

// PartInfo describes a split video part
type PartInfo struct {
	FilePath string
	PartNum  int
	FileSize int64
}

// PlaylistInfo contains information about a playlist
type PlaylistInfo struct {
	ID           string            `json:"id"`
	Title        string            `json:"title"`
	PlaylistCount int              `json:"playlist_count"`
	Entries      []PlaylistEntry   `json:"entries"`
}

// PlaylistEntry represents a single video in a playlist
type PlaylistEntry struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	URL      string  `json:"url"`
	Duration float64 `json:"duration"`
}

// DownloadResult contains the result of a download operation
type DownloadResult struct {
	FilePath    string
	FileName    string
	Title       string
	Duration    float64 // video duration in seconds
	FileSize    int64
	Width       int // video width in pixels
	Height      int // video height in pixels
	ContentType string
	IsSplit     bool       // true if video was split into parts
	Parts       []PartInfo // split parts (only if IsSplit is true)
	Error       error
}

type Downloader struct {
	downloadDir string
	timeout     time.Duration
	cookiesPath string

	// igMu guards igLastAt and igCooldownUntil and serializes Instagram-bound
	// yt-dlp invocations across all goroutines. See waitForIGSlot for the
	// rationale.
	igMu sync.Mutex
	// igLastAt is the wall-clock time of the most recent Instagram-bound
	// yt-dlp invocation. Zero value means "never seen IG before" — the
	// first IG call passes through immediately.
	igLastAt time.Time
	// igCooldownUntil is the wall-clock deadline until which Instagram-bound
	// yt-dlp invocations must wait, set by noteIGRateLimit after yt-dlp
	// returns an IG rate-limit / login-required error. Zero value (or any
	// time in the past) means "no active cooldown" — waitForIGSlot falls back
	// to the minIGGap-only behavior. See waitForIGSlot for how cooldown and
	// minIGGap combine (whichever requires the longer wait wins).
	igCooldownUntil time.Time
}

// New creates a Downloader. If cookiesPath is non-empty, every yt-dlp invocation
// is passed `--cookies <path>` so authenticated sessions (e.g. Instagram) work.
// Pass "" to disable cookies. The path is trimmed of surrounding whitespace
// (defends against misconfigured systemd Environment= lines with trailing
// space/tab). Logs a warning if cookiesPath is non-empty but unreadable, so
// misconfiguration surfaces at startup instead of as an opaque yt-dlp error.
func New(cookiesPath string) *Downloader {
	// Ensure download directory exists
	os.MkdirAll(DownloadDir, 0755)

	// Canonicalize once: trim surrounding whitespace so all downstream
	// consumers (cookieArgs, the os.Stat check below) see the same value.
	cookiesPath = strings.TrimSpace(cookiesPath)

	// One-time readability check on the cookies file so a misconfigured
	// SUSHE_COOKIES path surfaces clearly at startup instead of as an
	// opaque yt-dlp error on the first download. We layer three checks
	// because each catches a failure mode the others miss:
	//   - os.Stat catches missing path / permission-denied at the directory
	//     level. By itself it misses read-permission failures on the file
	//     (a file owned by root with mode 0600 passes os.Stat but fails at
	//     yt-dlp read time).
	//   - info.Mode().IsRegular() rejects FIFOs / devices / sockets /
	//     directories. os.Open on a FIFO can block; on a directory it
	//     succeeds on Linux, masking a misconfigured path.
	//   - os.Open exercises the actual read permission yt-dlp will need.
	if cookiesPath != "" {
		if info, err := os.Stat(cookiesPath); err != nil {
			logger.Warn("Cookies file not readable; yt-dlp calls will likely fail",
				"path", cookiesPath, "error", err)
		} else if !info.Mode().IsRegular() {
			logger.Warn("Cookies path is not a regular file; yt-dlp calls will likely fail",
				"path", cookiesPath, "mode", info.Mode().String())
		} else if f, err := os.Open(cookiesPath); err != nil {
			logger.Warn("Cookies file not readable; yt-dlp calls will likely fail",
				"path", cookiesPath, "error", err)
		} else {
			f.Close()
		}
	}

	return &Downloader{
		downloadDir: DownloadDir,
		timeout:     DefaultTimeout,
		cookiesPath: cookiesPath,
	}
}

// igSinglePostRe matches the path of an Instagram single-post URL — one of
// /p/<id>, /reel/<id>, or /tv/<id> with an optional trailing slash and any
// suffix (query string, sub-path). Used by IsInstagramSinglePost to decide
// whether IsPlaylist can short-circuit the yt-dlp metadata fetch.
var igSinglePostRe = regexp.MustCompile(`^/(p|reel|tv)/[^/]+/?`)

// isInstagramHost returns true when rawURL's hostname matches instagram.com
// or *.instagram.com (e.g. www.instagram.com, m.instagram.com). Confusables
// like evilinstagram.com and instagram.com.evil.com are excluded.
//
// Shared by waitForIGSlot (rate-limit gate) and igExtractorArgs (Layer 0 args)
// so both use one source of truth — drift between the gate and the args would
// either leak the web-app fingerprint on gated IG traffic or apply the iOS
// fingerprint to non-IG hosts. Parse error or empty host returns false.
func isInstagramHost(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	return host == "instagram.com" || strings.HasSuffix(host, ".instagram.com")
}

// IsInstagramSinglePost returns true when rawURL is an Instagram URL pointing
// at a single post (/p/, /reel/, /tv/). Used by engine.IsPlaylist to skip the
// yt-dlp metadata preflight for URLs that are syntactically guaranteed to be
// single videos — halving the IG signal for the common case.
//
// Host matching uses isInstagramHost (instagram.com or *.instagram.com), so
// confusables like evilinstagram.com are excluded. A parse error or non-IG
// host returns false; the caller falls through to the existing GetPlaylistInfo
// call.
func IsInstagramSinglePost(rawURL string) bool {
	if !isInstagramHost(rawURL) {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return igSinglePostRe.MatchString(u.Path)
}

// igExtractorArgs returns IG-specific yt-dlp flags for Instagram URLs:
//
//   - --extractor-args "instagram:app_id=124024574287414" (iOS app id; yt-dlp
//     PR #12359). Default web-app id `936619743392459` plus a Hetzner-range
//     datacenter IP is maximally suspicious to IG; the iOS app id routes
//     requests to a different IG endpoint cluster with a different rate
//     profile.
//   - --user-agent <iOS UA>. Must match the iOS app id — mismatched UA +
//     app_id pairs are themselves a flag signal.
//   - --retries 1 / --fragment-retries 1. Overrides the global --retries 3 /
//     --fragment-retries 3 from throttleArgs (yt-dlp's last-wins arg parsing).
//     IG escalates flags after 429-retry storms, especially fragment retries
//     where one bad fragment × 3 retries × N fragments multiplies the
//     rate-limit signal. Better to fail fast and let cookies fallback / the
//     cooldown handle the actual recovery.
//
// Returns nil for non-IG URLs so YouTube / Twitter / TikTok keep the existing
// throttle stack with the desktop Firefox UA.
//
// Order at call sites: prepended AFTER throttleArgs() and cookieArgs(...) so
// the IG-specific UA / retries / fragment-retries override the desktop
// defaults via yt-dlp's last-wins arg parsing.
//
// Returns a fresh slice each call so callers can safely append onto it
// (consistent with cookieArgs / throttleArgs).
func igExtractorArgs(rawURL string) []string {
	if !isInstagramHost(rawURL) {
		return nil
	}
	return []string{
		"--extractor-args", "instagram:app_id=" + igIosAppID,
		"--user-agent", igIosUA,
		"--retries", "1",
		"--fragment-retries", "1",
	}
}

// isIGRateLimit returns true when err looks like an Instagram rate-limit /
// auth-required response from yt-dlp. The check is a substring match against
// the formatted error message (which formatYtdlpError populates with the
// captured stderr from yt-dlp, including IG's `Requested content is not
// available, rate-limit reached or login required` string, HTTP 429
// responses, and generic `login required` failures for stories/private
// content). errors.Unwrap chains are covered automatically because err.Error()
// includes wrapped error messages via fmt.Errorf("%w") semantics.
//
// Conservative match — a false positive only triggers an extra cooldown or a
// cookies-fallback retry, both of which are safe. A false negative would let
// the bot keep hammering IG during a flag, which is exactly the failure mode
// this PR exists to prevent, so we err on the side of matching too eagerly.
func isIGRateLimit(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "rate-limit reached or login required") ||
		strings.Contains(msg, "HTTP Error 429") ||
		strings.Contains(msg, "login required")
}

// waitForIGSlot enforces a process-wide minimum gap (minIGGap) between
// Instagram-bound yt-dlp invocations. For non-Instagram URLs (and unparseable
// URLs) it returns nil immediately without acquiring the lock. For IG URLs,
// it briefly locks d.igMu to read+stamp d.igLastAt with the projected wake
// time, then releases the lock and sleeps. Stamping to the projected wake
// time inside the lock — before sleeping — has two benefits:
//
//   - Concurrent callers waiting on igMu read the projected next-slot time
//     and queue themselves minIGGap further out (rather than all racing to
//     the same now+remaining).
//   - On ctx cancellation the stamp is preserved, so retry storms after the
//     gate releases still respect the gap (matches the doc invariant).
//
// progressCb is invoked OUTSIDE the lock so a slow Telegram edit by one
// goroutine cannot block other goroutines waiting for an IG slot.
//
// Host matching delegates to isInstagramHost (instagram.com or
// *.instagram.com via Hostname() with port stripped). Confusables like
// evilinstagram.com and substring-in-path/query false positives are excluded.
//
// A `Progress{Phase: "queued", ETA: <remaining>}` event is emitted via
// progressCb EXACTLY ONCE per call, and only when an actual wait is needed
// AND progressCb is non-nil. This keeps the bot UI quiet for non-IG URLs,
// for warmed-up callers (gap already elapsed), and for callers that don't
// supply a callback (e.g. DownloadPlaylistVideo).
func (d *Downloader) waitForIGSlot(ctx context.Context, rawURL string, progressCb ProgressCallback) error {
	// Cheap early-out: if the caller has already cancelled, don't bother
	// parsing the URL or acquiring the lock.
	if err := ctx.Err(); err != nil {
		return err
	}

	// Host match (parse error / non-IG / empty host all return false and fall
	// through ungated; malformed URLs let yt-dlp report the error so callers
	// don't see two error messages for the same problem).
	if !isInstagramHost(rawURL) {
		return nil
	}

	// Critical section is intentionally narrow: read+stamp, no I/O. The
	// progressCb and the actual sleep happen OUTSIDE this section.
	d.igMu.Lock()
	// Two waits are in play: (a) the minIGGap spacing between consecutive IG
	// invocations, and (b) the cooldown applied after a rate-limit error.
	// Whichever requires the longer remaining wait wins. Cooldown of zero
	// (never tripped, or already elapsed) yields a non-positive
	// cooldownRemaining and effectively no-ops.
	remaining := minIGGap - time.Since(d.igLastAt)
	cooldownRemaining := time.Until(d.igCooldownUntil)
	if cooldownRemaining > remaining {
		remaining = cooldownRemaining
	}
	if remaining > 0 {
		// Stamp the projected wake time so other goroutines see "next slot
		// opens at now+remaining" and queue minIGGap further out. The stamp
		// stays put even on ctx cancellation below, which preserves the
		// "unconditional update" invariant for retry storms.
		d.igLastAt = time.Now().Add(remaining)
	} else {
		d.igLastAt = time.Now()
	}
	d.igMu.Unlock()

	if remaining > 0 {
		if progressCb != nil {
			progressCb(Progress{
				Phase: "queued",
				ETA:   remaining.Round(time.Second).String(),
			})
		}
		select {
		case <-time.After(remaining):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// noteIGRateLimit records an Instagram rate-limit / login-required error from
// yt-dlp by setting the cooldown deadline to now + igCooldown. Subsequent
// IG-bound invocations call waitForIGSlot, which honors the deadline and
// blocks until it expires (or until ctx is cancelled).
//
// Idempotent under concurrent calls: each invocation slides the deadline
// forward to now + igCooldown rather than extending from the previous
// deadline. This gives "5 minutes from the last bad signal" semantics, which
// is what we want — if IG keeps returning rate-limit responses, the bot keeps
// pausing rather than resuming after a fixed window from the first flag.
//
// Holds d.igMu only long enough to assign the deadline. Callers must NOT hold
// d.igMu when calling this method (would deadlock).
func (d *Downloader) noteIGRateLimit() {
	d.igMu.Lock()
	d.igCooldownUntil = time.Now().Add(igCooldown)
	d.igMu.Unlock()
}

// Download downloads a video from the given URL using yt-dlp
func (d *Downloader) Download(ctx context.Context, url string) (*DownloadResult, error) {
	return d.DownloadWithProgress(ctx, url, nil)
}

// DownloadWithProgress downloads a video and reports progress via callback
func (d *Downloader) DownloadWithProgress(ctx context.Context, url string, progressCb ProgressCallback) (*DownloadResult, error) {
	// Enforce process-wide Instagram rate-limit gap before doing any work.
	// No-op for non-IG URLs. Must run before creating the work directory so
	// a cancellation during the wait doesn't leave a stray temp dir.
	if err := d.waitForIGSlot(ctx, url, progressCb); err != nil {
		return nil, err
	}

	// Create unique subdirectory for this download
	downloadID := fmt.Sprintf("%d", time.Now().UnixNano())
	workDir := filepath.Join(d.downloadDir, downloadID)
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create work directory: %w", err)
	}

	// Output template
	outputTemplate := filepath.Join(workDir, "%(title).100s.%(ext)s")

	// Build yt-dlp args WITHOUT cookies — runWithCookieFallback inserts cookies
	// per attempt (anonymous first, then d.cookiesPath on IG-rate-limit retry).
	// Use --newline for parseable progress output. Prefer H.264 sources to
	// avoid re-encoding, but accept any codec (will re-encode later if needed).
	baseArgs := throttleArgs()
	// igExtractorArgs appended AFTER throttleArgs so the IG-specific UA /
	// retries / fragment-retries override throttle defaults via yt-dlp's
	// last-wins arg parsing. Returns nil for non-IG URLs (no effect on
	// YouTube / TikTok / etc).
	baseArgs = append(baseArgs, igExtractorArgs(url)...)
	baseArgs = append(baseArgs,
		"--no-playlist",
		// Prefer H.264 (avc1) video + AAC audio sources to avoid re-encoding
		// Falls back to any codec if H.264 not available
		"-f", "bestvideo[vcodec^=avc1][height<=1080]+bestaudio[acodec^=mp4a]/bestvideo[vcodec^=avc][height<=1080]+bestaudio/bestvideo[height<=1080]+bestaudio/best[height<=1080]/best",
		"--merge-output-format", "mp4",
		// NO forced re-encoding here - we check codec after download and re-encode only if needed
		"-o", outputTemplate,
		"--no-warnings",
		"--progress",
		"--newline",
		url,
	)

	if err := d.runWithCookieFallback(ctx, workDir, baseArgs, progressCb); err != nil {
		logger.Error("yt-dlp failed", "error", err)
		os.RemoveAll(workDir)
		return nil, fmt.Errorf("download failed: %w", err)
	}

	// Find the downloaded file
	files, err := filepath.Glob(filepath.Join(workDir, "*"))
	if err != nil || len(files) == 0 {
		os.RemoveAll(workDir)
		return nil, fmt.Errorf("no file downloaded")
	}

	filePath := files[0]
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		os.RemoveAll(workDir)
		return nil, fmt.Errorf("failed to stat downloaded file: %w", err)
	}

	fileName := filepath.Base(filePath)
	title := strings.TrimSuffix(fileName, filepath.Ext(fileName))

	// Check video codec - re-encode if not H.264 compatible
	codec, err := GetVideoCodec(filePath)
	if err != nil {
		logger.Warn("Failed to get video codec, assuming needs re-encoding", "error", err)
		codec = "unknown"
	}

	logger.Info("Downloaded video codec", "codec", codec, "file", fileName)

	// Re-encode if codec is not H.264 compatible (Telegram requires H.264)
	if !IsH264Compatible(codec) {
		logger.Info("Re-encoding required", "codec", codec, "target", "h264")

		// Notify progress callback about encoding phase
		if progressCb != nil {
			progressCb(Progress{
				Phase:   "encoding",
				Codec:   codec,
				Percent: 0,
			})
		}

		// Re-encode to H.264
		newPath, err := d.ReencodeToH264(ctx, filePath, progressCb)
		if err != nil {
			os.RemoveAll(workDir)
			return nil, fmt.Errorf("failed to re-encode to H.264: %w", err)
		}

		// Remove original, use re-encoded file
		os.Remove(filePath)
		filePath = newPath
		fileName = filepath.Base(filePath)

		// Update file info
		fileInfo, err = os.Stat(filePath)
		if err != nil {
			os.RemoveAll(workDir)
			return nil, fmt.Errorf("failed to stat re-encoded file: %w", err)
		}

		logger.Info("Re-encoding complete", "newSize", fileInfo.Size())
	} else {
		// Video is already H.264, but apply faststart for better streaming (PiP support)
		logger.Info("Applying faststart to H.264 video", "codec", codec)

		// Create output file path for faststart version
		dir := filepath.Dir(filePath)
		baseName := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
		fastStartPath := filepath.Join(dir, baseName+"_faststart.mp4")

		// Apply faststart using ffmpeg with copy (no re-encoding)
		args := []string{
			"-i", filePath,
			"-c", "copy",
			"-movflags", "+faststart",
			"-y", // Overwrite output
			fastStartPath,
		}

		cmd := exec.CommandContext(ctx, "ffmpeg", args...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			logger.Warn("Failed to apply faststart, using original file", "error", err, "output", string(output))
		} else {
			// Replace original with faststart version
			os.Remove(filePath)
			filePath = fastStartPath
			fileName = filepath.Base(filePath)

			// Update file info
			fileInfo, err = os.Stat(filePath)
			if err != nil {
				os.RemoveAll(workDir)
				return nil, fmt.Errorf("failed to stat faststart file: %w", err)
			}

			logger.Info("Faststart applied successfully", "newSize", fileInfo.Size())
		}
	}

	// Get video metadata (duration, dimensions)
	mediaInfo, _ := GetMediaInfo(filePath)
	var duration float64
	var width, height int
	if mediaInfo != nil {
		duration = mediaInfo.Duration
		width = mediaInfo.Width
		height = mediaInfo.Height
	}

	return &DownloadResult{
		FilePath:    filePath,
		FileName:    fileName,
		Title:       title,
		Duration:    duration,
		FileSize:    fileInfo.Size(),
		Width:       width,
		Height:      height,
		ContentType: getContentType(filePath),
		IsSplit:     false,
		Parts:       nil,
	}, nil
}

// runWithProgress runs yt-dlp and parses progress output
func (d *Downloader) runWithProgress(cmd *exec.Cmd, progressCb ProgressCallback) error {
	// Regex patterns for parsing yt-dlp output
	// [download]  45.2% of 50.00MiB at 2.50MiB/s ETA 00:30
	downloadRe := regexp.MustCompile(`\[download\]\s+(\d+\.?\d*)%\s+of\s+~?(\S+)\s+at\s+(\S+)\s+ETA\s+(\S+)`)
	// [download] 100% of 50.00MiB in 00:20
	completeRe := regexp.MustCompile(`\[download\]\s+100%\s+of\s+(\S+)`)
	// [Merger] Merging formats into "file.mp4"
	mergerRe := regexp.MustCompile(`\[Merger\]`)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start yt-dlp: %w", err)
	}

	// Capture stderr while still logging each line at Debug. The buffer is
	// surfaced in the returned error so the user sees yt-dlp's actual cause
	// (rate-limit, login required, etc.) instead of plain "exit status 1".
	// Default bufio.Scanner cap is 64KB — yt-dlp can emit longer error lines
	// (full tracebacks, JSON dumps), so bump to 1MB on both pipes; on overflow
	// Scan returns false and we'd silently drop the rest of stderr (the very
	// data this fix surfaces) plus risk blocking the child on a full pipe.
	const scannerBufMax = 1 << 20 // 1 MB
	var stderrBuf strings.Builder
	var stderrMu sync.Mutex
	var stderrWg sync.WaitGroup

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), scannerBufMax)
	stderrWg.Add(1)
	go func() {
		defer stderrWg.Done()
		stderrScanner := bufio.NewScanner(stderr)
		stderrScanner.Buffer(make([]byte, 64*1024), scannerBufMax)
		for stderrScanner.Scan() {
			line := stderrScanner.Text()
			logger.Debug("yt-dlp stderr", "line", line)
			stderrMu.Lock()
			stderrBuf.WriteString(line)
			stderrBuf.WriteByte('\n')
			stderrMu.Unlock()
		}
		if err := stderrScanner.Err(); err != nil {
			logger.Warn("yt-dlp stderr scanner error", "error", err)
		}
	}()

	for scanner.Scan() {
		line := scanner.Text()
		logger.Debug("yt-dlp output", "line", line)

		// Parse download progress
		if matches := downloadRe.FindStringSubmatch(line); matches != nil {
			var percent float64
			fmt.Sscanf(matches[1], "%f", &percent)
			progressCb(Progress{
				Phase:   "downloading",
				Percent: percent,
				Total:   matches[2],
				Speed:   matches[3],
				ETA:     matches[4],
			})
		} else if completeRe.MatchString(line) {
			progressCb(Progress{
				Phase:   "downloading",
				Percent: 100,
			})
		} else if mergerRe.MatchString(line) {
			progressCb(Progress{
				Phase:   "merging",
				Percent: 100,
			})
		}
	}
	if err := scanner.Err(); err != nil {
		logger.Warn("yt-dlp stdout scanner error", "error", err)
	}

	waitErr := cmd.Wait()
	stderrWg.Wait()
	return formatYtdlpError(waitErr, stderrBuf.String())
}

// formatYtdlpError wraps a yt-dlp execution error with captured stderr so
// callers see the underlying cause. Returns err unchanged when stderr is empty.
func formatYtdlpError(err error, stderr string) error {
	if err == nil {
		return nil
	}
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w - %s", err, stderr)
}

// shouldRetryWithCookies returns true when err matches an Instagram rate-limit /
// auth-required response AND a non-empty cookies path is available for the
// retry. Pure function — the retry loop in runWithCookieFallback calls this
// rather than inlining the boolean so the decision is unit-testable without
// touching exec.Cmd.
//
// The "cookies non-empty" guard is what makes "anonymous-first, cookies-as-
// fallback" work safely when the bot is started without SUSHE_COOKIES: a
// non-IG-friendly account is better than no account at all, but no account is
// better than retrying anonymously with the same args and the same outcome.
func shouldRetryWithCookies(err error, cookiesPath string) bool {
	return isIGRateLimit(err) && cookiesPath != ""
}

// runWithCookieFallback runs yt-dlp with try-anonymous-first semantics. baseArgs
// must include throttleArgs, igExtractorArgs (when applicable), all other
// yt-dlp flags, and the URL — but NOT cookieArgs (the helper manages cookies
// across the anonymous attempt and the cookies retry).
//
// Behavior:
//
//  1. First attempt: anonymous (cookieArgs("") → no --cookies flag). Most
//     public Instagram posts download here without ever touching the bot's
//     IG account, keeping that account clean for the majority of traffic.
//  2. If the anonymous attempt fails AND shouldRetryWithCookies(err,
//     d.cookiesPath) → partial output is cleaned from workDir and the
//     invocation is retried with d.cookiesPath.
//  3. If the final returned error is an IG rate-limit, d.noteIGRateLimit()
//     is called so subsequent goroutines back off via waitForIGSlot's
//     cooldown branch — defense in depth on top of the cookies fallback.
//
// Layer 0's igExtractorArgs (iOS app_id + iOS UA + retries=1) must already be
// present in baseArgs and is therefore applied identically on both the
// anonymous attempt and the cookies retry — every IG-bound yt-dlp invocation
// shows the iOS fingerprint, regardless of cookies state.
func (d *Downloader) runWithCookieFallback(ctx context.Context, workDir string, baseArgs []string, progressCb ProgressCallback) error {
	runOnce := func(cookiesPath string) error {
		// Prepend cookies (or nothing, for the anonymous attempt) so the
		// final args order is [cookies, throttle, igExtractor, ..., url].
		// Cookies position relative to throttle/igExtractor does not affect
		// yt-dlp parsing (no flag duplication), and URL must remain last.
		args := append(cookieArgs(cookiesPath), baseArgs...)
		logger.Debug("Running yt-dlp", "args", args, "anonymous", cookiesPath == "")

		cmdCtx, cancel := context.WithTimeout(ctx, d.timeout)
		defer cancel()

		cmd := exec.CommandContext(cmdCtx, "yt-dlp", args...)
		cmd.Dir = workDir

		if progressCb != nil {
			// runWithProgress already wraps with formatYtdlpError, so the
			// returned error carries the IG stderr that isIGRateLimit needs.
			return d.runWithProgress(cmd, progressCb)
		}
		output, err := cmd.CombinedOutput()
		if err != nil {
			return formatYtdlpError(err, string(output))
		}
		return nil
	}

	err := runOnce("")
	if err == nil {
		return nil
	}

	if shouldRetryWithCookies(err, d.cookiesPath) {
		logger.Info("Anonymous yt-dlp attempt hit IG rate-limit; retrying with cookies", "error", err)
		// Remove partial output from the failed anonymous attempt so the
		// caller's glob picks up only the retry's files. ReadDir errors
		// here are non-fatal (the retry's overwrite via -o + the caller's
		// glob will surface any real problem).
		if entries, gerr := os.ReadDir(workDir); gerr == nil {
			for _, e := range entries {
				os.RemoveAll(filepath.Join(workDir, e.Name()))
			}
		}
		err = runOnce(d.cookiesPath)
	}

	if isIGRateLimit(err) {
		d.noteIGRateLimit()
	}
	return err
}

// GetPlaylistInfo checks if a URL is a playlist and returns playlist information.
//
// NOTE: this intentionally does NOT call waitForIGSlot. IsPlaylist always
// precedes the actual download (which IS gated), so gating here would
// double-charge every IG single-video URL by minIGGap (cold-start single
// URLs would wait ~8s for metadata then ~8s again for the download — bad UX
// that the phase-1 fix specifically removed). The metadata fetch is a single
// short request that Instagram does not flag the same way as a burst of
// media downloads, so it is safe to leave ungated in the common single-URL
// case.
//
// KNOWN LIMITATION (multi-user concurrent metadata bursts): if N Telegram
// users send IG URLs simultaneously, all N GetPlaylistInfo metadata fetches
// run concurrently and ungated. The download gate (DownloadWithProgress)
// still serializes the heavier traffic that IG actually flags, but the
// metadata burst is not paced. Empirically this has not caused flagging in
// production, but it is a real gap.
//
// TODO: move the IG rate-limit gate from the downloader layer to a higher
// request-level point (e.g. engine.Process / engine.IsPlaylist) with
// one-gate-per-request semantics. That would protect both metadata and
// download under a single gate without double-charging single URLs. The
// refactor is non-trivial (engine doesn't currently know about IG host
// detection or the gate primitive) so it is deferred. Tracked from codex
// external review 2026-05-10.
func (d *Downloader) GetPlaylistInfo(ctx context.Context, url string) (*PlaylistInfo, error) {
	// Use yt-dlp with --flat-playlist --dump-json to check if it's a playlist
	args := throttleArgs()
	args = append(args, cookieArgs(d.cookiesPath)...)
	// igExtractorArgs appended AFTER throttleArgs/cookieArgs so the IG-specific
	// UA / retries / fragment-retries override throttle defaults via yt-dlp's
	// last-wins arg parsing. Returns nil for non-IG URLs.
	args = append(args, igExtractorArgs(url)...)
	args = append(args,
		"--flat-playlist",
		"--dump-json",
		"--no-warnings",
		url,
	)

	logger.Debug("Checking if URL is playlist", "args", args)

	cmd := exec.CommandContext(ctx, "yt-dlp", args...)
	output, err := cmd.Output()
	if err != nil {
		// cmd.Output() captures stderr into ExitError.Stderr when cmd.Stderr
		// is nil. Surface it through formatYtdlpError so isIGRateLimit can
		// inspect the wrapped message and fire noteIGRateLimit — without
		// this, IG rate-limit responses on the (rare) playlist metadata
		// fetch would not back off the global gate.
		var stderrText string
		if ee, ok := err.(*exec.ExitError); ok {
			stderrText = string(ee.Stderr)
		}
		wrappedErr := formatYtdlpError(err, stderrText)
		if isIGRateLimit(wrappedErr) {
			d.noteIGRateLimit()
		}
		return nil, fmt.Errorf("failed to get playlist info: %w", wrappedErr)
	}

	// Parse the JSON lines output
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 {
		return nil, fmt.Errorf("no output from yt-dlp")
	}

	// Parse each line as a JSON entry
	var entries []PlaylistEntry
	var playlistTitle string
	var playlistID string

	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			logger.Warn("Failed to parse playlist entry", "line", line, "error", err)
			continue
		}

		// Extract basic info
		id, _ := entry["id"].(string)
		title, _ := entry["title"].(string)
		url, _ := entry["url"].(string)
		
		// Handle duration (might be null for unavailable videos)
		var duration float64
		if d, ok := entry["duration"]; ok && d != nil {
			switch v := d.(type) {
			case float64:
				duration = v
			case string:
				fmt.Sscanf(v, "%f", &duration)
			}
		}

		// Get playlist title from first entry (if available)
		if playlistTitle == "" {
			if pt, ok := entry["playlist_title"].(string); ok {
				playlistTitle = pt
			}
		}
		if playlistID == "" {
			if pid, ok := entry["playlist_id"].(string); ok {
				playlistID = pid
			}
		}

		entries = append(entries, PlaylistEntry{
			ID:       id,
			Title:    title,
			URL:      url,
			Duration: duration,
		})
	}

	// If only one entry, it's likely a single video, not a playlist
	if len(entries) <= 1 {
		return nil, fmt.Errorf("not a playlist - single video detected")
	}

	// Apply playlist limits
	if len(entries) > MaxPlaylistVideos {
		logger.Info("Playlist too large, truncating", "total", len(entries), "max", MaxPlaylistVideos)
		entries = entries[:MaxPlaylistVideos]
	}

	// Filter out videos that are too long
	var validEntries []PlaylistEntry
	for _, entry := range entries {
		if entry.Duration > 0 && entry.Duration > MaxVideoDuration.Seconds() {
			logger.Info("Skipping video (too long)", "title", entry.Title, "duration", entry.Duration)
			continue
		}
		validEntries = append(validEntries, entry)
	}

	if len(validEntries) == 0 {
		return nil, fmt.Errorf("no valid videos in playlist after filtering")
	}

	if playlistTitle == "" {
		playlistTitle = "Unknown Playlist"
	}

	return &PlaylistInfo{
		ID:           playlistID,
		Title:        playlistTitle,
		PlaylistCount: len(validEntries),
		Entries:      validEntries,
	}, nil
}

// DownloadPlaylistVideo downloads a specific video from a playlist.
//
// Per-item IG rate limiting: each playlist item is an actual media download
// (the burst signal Instagram flags), so the gate runs per item. With a 50-item
// IG playlist at minIGGap=8s the floor wait is 50*8s = ~6min of gating — well
// within the 15-minute request timeout, and playlists are inherently slow.
func (d *Downloader) DownloadPlaylistVideo(ctx context.Context, playlistURL string, videoIndex int, progressCb ProgressCallback) (*DownloadResult, error) {
	// Enforce per-item IG rate-limit gap.
	if err := d.waitForIGSlot(ctx, playlistURL, progressCb); err != nil {
		return nil, err
	}

	// Create unique subdirectory for this download
	downloadID := fmt.Sprintf("%d", time.Now().UnixNano())
	workDir := filepath.Join(d.downloadDir, downloadID)
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create work directory: %w", err)
	}

	// Output template
	outputTemplate := filepath.Join(workDir, "%(title).100s.%(ext)s")

	// Build yt-dlp args WITHOUT cookies — runWithCookieFallback inserts cookies
	// per attempt (anonymous first, then d.cookiesPath on IG-rate-limit retry).
	// Remove --no-playlist and use --playlist-items to download specific video.
	baseArgs := throttleArgs()
	// igExtractorArgs appended AFTER throttleArgs so the IG-specific UA /
	// retries / fragment-retries override throttle defaults via yt-dlp's
	// last-wins arg parsing. Returns nil for non-IG URLs.
	baseArgs = append(baseArgs, igExtractorArgs(playlistURL)...)
	baseArgs = append(baseArgs,
		fmt.Sprintf("--playlist-items=%d", videoIndex+1), // yt-dlp uses 1-based indexing
		"-f", "bestvideo[vcodec^=avc1][height<=1080]+bestaudio[acodec^=mp4a]/bestvideo[vcodec^=avc][height<=1080]+bestaudio/bestvideo[height<=1080]+bestaudio/best[height<=1080]/best",
		"--merge-output-format", "mp4",
		"-o", outputTemplate,
		"--no-warnings",
		"--progress",
		"--newline",
		playlistURL,
	)

	if err := d.runWithCookieFallback(ctx, workDir, baseArgs, progressCb); err != nil {
		logger.Error("yt-dlp failed for playlist video", "index", videoIndex, "error", err)
		os.RemoveAll(workDir)
		return nil, fmt.Errorf("download failed: %w", err)
	}

	// Find the downloaded file
	files, err := filepath.Glob(filepath.Join(workDir, "*"))
	if err != nil || len(files) == 0 {
		os.RemoveAll(workDir)
		return nil, fmt.Errorf("no file downloaded")
	}

	filePath := files[0]
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		os.RemoveAll(workDir)
		return nil, fmt.Errorf("failed to stat downloaded file: %w", err)
	}

	fileName := filepath.Base(filePath)
	title := strings.TrimSuffix(fileName, filepath.Ext(fileName))

	// Check video codec and apply same processing as single video download
	codec, err := GetVideoCodec(filePath)
	if err != nil {
		logger.Warn("Failed to get video codec, assuming needs re-encoding", "error", err)
		codec = "unknown"
	}

	logger.Info("Downloaded playlist video codec", "index", videoIndex, "codec", codec, "file", fileName)

	// Re-encode if codec is not H.264 compatible (same logic as single video)
	if !IsH264Compatible(codec) {
		logger.Info("Re-encoding playlist video required", "index", videoIndex, "codec", codec, "target", "h264")

		// Notify progress callback about encoding phase
		if progressCb != nil {
			progressCb(Progress{
				Phase:   "encoding",
				Codec:   codec,
				Percent: 0,
			})
		}

		// Re-encode to H.264
		newPath, err := d.ReencodeToH264(ctx, filePath, progressCb)
		if err != nil {
			os.RemoveAll(workDir)
			return nil, fmt.Errorf("failed to re-encode to H.264: %w", err)
		}

		// Remove original, use re-encoded file
		os.Remove(filePath)
		filePath = newPath
		fileName = filepath.Base(filePath)

		// Update file info
		fileInfo, err = os.Stat(filePath)
		if err != nil {
			os.RemoveAll(workDir)
			return nil, fmt.Errorf("failed to stat re-encoded file: %w", err)
		}

		logger.Info("Re-encoding complete for playlist video", "index", videoIndex, "newSize", fileInfo.Size())
	} else {
		// Apply faststart for better streaming
		logger.Info("Applying faststart to playlist video", "index", videoIndex, "codec", codec)

		dir := filepath.Dir(filePath)
		baseName := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
		fastStartPath := filepath.Join(dir, baseName+"_faststart.mp4")

		// Apply faststart using ffmpeg with copy
		args := []string{
			"-i", filePath,
			"-c", "copy",
			"-movflags", "+faststart",
			"-y",
			fastStartPath,
		}

		cmd := exec.CommandContext(ctx, "ffmpeg", args...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			logger.Warn("Failed to apply faststart to playlist video, using original", "index", videoIndex, "error", err, "output", string(output))
		} else {
			// Replace original with faststart version
			os.Remove(filePath)
			filePath = fastStartPath
			fileName = filepath.Base(filePath)

			// Update file info
			fileInfo, err = os.Stat(filePath)
			if err != nil {
				os.RemoveAll(workDir)
				return nil, fmt.Errorf("failed to stat faststart file: %w", err)
			}

			logger.Info("Faststart applied to playlist video", "index", videoIndex, "newSize", fileInfo.Size())
		}
	}

	// Get video metadata (duration, dimensions)
	mediaInfo, _ := GetMediaInfo(filePath)
	var duration float64
	var width, height int
	if mediaInfo != nil {
		duration = mediaInfo.Duration
		width = mediaInfo.Width
		height = mediaInfo.Height
	}

	return &DownloadResult{
		FilePath:    filePath,
		FileName:    fileName,
		Title:       title,
		Duration:    duration,
		FileSize:    fileInfo.Size(),
		Width:       width,
		Height:      height,
		ContentType: getContentType(filePath),
		IsSplit:     false,
		Parts:       nil,
	}, nil
}

// Cleanup removes the downloaded file and its directory
func (d *Downloader) Cleanup(result *DownloadResult) {
	if result != nil && result.FilePath != "" {
		dir := filepath.Dir(result.FilePath)
		os.RemoveAll(dir)
		logger.Debug("Cleaned up download", "dir", dir)
	}
}

// IsValidURL checks if the string looks like a valid video URL
func IsValidURL(s string) bool {
	s = strings.TrimSpace(s)
	// Basic URL validation
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		return false
	}

	// Check for common video hosting domains
	supportedDomains := []string{
		"youtube.com", "youtu.be",
		"twitter.com", "x.com",
		"tiktok.com",
		"instagram.com",
		"facebook.com", "fb.watch",
		"vimeo.com",
		"dailymotion.com",
		"twitch.tv",
		"reddit.com", "v.redd.it",
		"streamable.com",
		"imgur.com",
	}

	for _, domain := range supportedDomains {
		if strings.Contains(s, domain) {
			return true
		}
	}

	// Also accept any URL that yt-dlp might support
	// This is a permissive approach - yt-dlp will fail gracefully if unsupported
	return true
}

// ExtractURLs extracts all URLs from a message text
func ExtractURLs(text string) []string {
	var urls []string
	words := strings.Fields(text)
	for _, word := range words {
		// Clean up common URL wrapping
		word = strings.Trim(word, "<>()[]\"'")
		if IsValidURL(word) {
			urls = append(urls, word)
		}
	}
	return urls
}

func getContentType(filePath string) string {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mkv":
		return "video/x-matroska"
	case ".mov":
		return "video/quicktime"
	case ".avi":
		return "video/x-msvideo"
	default:
		return "video/mp4"
	}
}

// GetMediaInfo uses ffprobe to get video duration, bitrate, and dimensions
func GetMediaInfo(filePath string) (*MediaInfo, error) {
	// Use ffprobe to get video info in JSON format
	args := []string{
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		filePath,
	}

	cmd := exec.Command("ffprobe", args...)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe failed: %w", err)
	}

	// Parse JSON output
	var result struct {
		Format struct {
			Duration string `json:"duration"`
			Size     string `json:"size"`
			BitRate  string `json:"bit_rate"`
		} `json:"format"`
		Streams []struct {
			CodecType string `json:"codec_type"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
	}

	if err := json.Unmarshal(output, &result); err != nil {
		return nil, fmt.Errorf("failed to parse ffprobe output: %w", err)
	}

	var duration float64
	var size, bitrate int64
	fmt.Sscanf(result.Format.Duration, "%f", &duration)
	fmt.Sscanf(result.Format.Size, "%d", &size)
	fmt.Sscanf(result.Format.BitRate, "%d", &bitrate)

	// Find video stream dimensions
	var width, height int
	for _, stream := range result.Streams {
		if stream.CodecType == "video" {
			width = stream.Width
			height = stream.Height
			break
		}
	}

	return &MediaInfo{
		Duration: duration,
		Bitrate:  bitrate,
		FileSize: size,
		Width:    width,
		Height:   height,
	}, nil
}

// GetVideoCodec returns the video codec name (e.g., "h264", "vp9", "av1")
func GetVideoCodec(filePath string) (string, error) {
	args := []string{
		"-v", "quiet",
		"-select_streams", "v:0",
		"-show_entries", "stream=codec_name",
		"-of", "csv=p=0",
		filePath,
	}

	cmd := exec.Command("ffprobe", args...)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("ffprobe failed: %w", err)
	}

	codec := strings.TrimSpace(string(output))
	return codec, nil
}

// IsH264Compatible returns true if the codec is H.264/AVC (Telegram compatible)
func IsH264Compatible(codec string) bool {
	codec = strings.ToLower(codec)
	return codec == "h264" || codec == "avc" || codec == "avc1"
}

// GetAudioCodec returns the audio codec name (e.g., "aac", "opus", "vorbis")
func GetAudioCodec(filePath string) (string, error) {
	args := []string{
		"-v", "quiet",
		"-select_streams", "a:0",
		"-show_entries", "stream=codec_name",
		"-of", "csv=p=0",
		filePath,
	}
	cmd := exec.Command("ffprobe", args...)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("ffprobe audio codec failed: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// GetPixelFormat returns the pixel format (e.g., "yuv420p", "yuv420p10le")
func GetPixelFormat(filePath string) (string, error) {
	args := []string{
		"-v", "quiet",
		"-select_streams", "v:0",
		"-show_entries", "stream=pix_fmt",
		"-of", "csv=p=0",
		filePath,
	}
	cmd := exec.Command("ffprobe", args...)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("ffprobe pixel format failed: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// IsAACCompatible returns true if the audio codec is AAC (safe for copy in Telegram)
func IsAACCompatible(audioCodec string) bool {
	return strings.ToLower(audioCodec) == "aac"
}

// Is420p returns true if the pixel format is 4:2:0 8-bit (compatible with Telegram inline playback).
// Only yuv420p and yuvj420p (jpeg-range variant) are accepted; other subsampling formats
// like yuv422p or yuv444p are not widely supported by mobile hardware decoders.
func Is420p(pixFmt string) bool {
	pixFmt = strings.ToLower(pixFmt)
	return pixFmt == "yuv420p" || pixFmt == "yuvj420p"
}

// CanStreamCopy returns true if the source codecs are compatible with -c copy splitting.
// Requires H264 video + AAC audio + 4:2:0 8-bit pixel format.
func CanStreamCopy(videoCodec, audioCodec, pixFmt string) bool {
	return IsH264Compatible(videoCodec) && IsAACCompatible(audioCodec) && Is420p(pixFmt)
}

// ReencodeToH264 converts a video to H.264/AAC format for Telegram compatibility
// Returns the path to the new file (original file is kept)
func (d *Downloader) ReencodeToH264(ctx context.Context, filePath string, progressCb ProgressCallback) (string, error) {
	// Get duration for progress calculation
	mediaInfo, err := GetMediaInfo(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to get media info: %w", err)
	}

	// Create output file path
	dir := filepath.Dir(filePath)
	baseName := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
	outputPath := filepath.Join(dir, baseName+"_h264.mp4")

	logger.Info("Re-encoding to H.264", "input", filePath, "output", outputPath)

	// Build ffmpeg command
	args := []string{
		"-i", filePath,
		"-c:v", "libx264",
		"-preset", "fast",
		"-crf", "23",
		"-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-movflags", "+faststart",
		"-y", // Overwrite output
		outputPath,
	}

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)

	// Capture stderr for progress parsing
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("failed to start ffmpeg: %w", err)
	}

	// Parse ffmpeg progress output
	if progressCb != nil {
		go func() {
			scanner := bufio.NewScanner(stderr)
			timeRe := regexp.MustCompile(`time=(\d+):(\d+):(\d+\.?\d*)`)
			for scanner.Scan() {
				line := scanner.Text()
				if matches := timeRe.FindStringSubmatch(line); matches != nil {
					var hours, mins int
					var secs float64
					fmt.Sscanf(matches[1], "%d", &hours)
					fmt.Sscanf(matches[2], "%d", &mins)
					fmt.Sscanf(matches[3], "%f", &secs)
					currentTime := float64(hours*3600+mins*60) + secs
					percent := (currentTime / mediaInfo.Duration) * 100
					if percent > 100 {
						percent = 100
					}
					progressCb(Progress{
						Phase:   "encoding",
						Percent: percent,
					})
				}
			}
		}()
	} else {
		// Drain stderr
		go func() {
			scanner := bufio.NewScanner(stderr)
			for scanner.Scan() {
				logger.Debug("ffmpeg", "line", scanner.Text())
			}
		}()
	}

	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("ffmpeg encoding failed: %w", err)
	}

	logger.Info("Re-encoding complete", "output", outputPath)
	return outputPath, nil
}

// NeedsSplit returns true if the file is larger than MaxUploadSize
func NeedsSplit(fileSize int64) bool {
	return fileSize > MaxUploadSize
}

// CalculateNumParts returns the number of parts needed for splitting
func CalculateNumParts(fileSize int64) int {
	return int(math.Ceil(float64(fileSize) / float64(MaxSplitSize)))
}

// SplitVideo splits a video into parts of approximately MaxSplitSize.
// Uses stream copy (-c copy) for H264+AAC+8-bit sources (zero RAM overhead).
// Falls back to full re-encode with memory-safe settings for incompatible codecs.
func (d *Downloader) SplitVideo(ctx context.Context, filePath string, progressCb ProgressCallback) ([]PartInfo, error) {
	// Get media info
	mediaInfo, err := GetMediaInfo(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to get media info: %w", err)
	}

	if mediaInfo.Duration <= 0 {
		return nil, fmt.Errorf("invalid video duration: %f", mediaInfo.Duration)
	}
	if mediaInfo.FileSize <= 0 {
		return nil, fmt.Errorf("invalid file size from ffprobe: %d", mediaInfo.FileSize)
	}

	// Detect codecs to determine split strategy
	videoCodec, err := GetVideoCodec(filePath)
	if err != nil {
		logger.Warn("Failed to detect video codec, will re-encode", "error", err)
		videoCodec = "unknown"
	}

	audioCodec, err := GetAudioCodec(filePath)
	if err != nil {
		logger.Warn("Failed to detect audio codec, will re-encode audio", "error", err)
		audioCodec = "unknown"
	}

	pixFmt, err := GetPixelFormat(filePath)
	if err != nil {
		logger.Warn("Failed to detect pixel format, will re-encode", "error", err)
		pixFmt = "unknown"
	}

	canStreamCopy := CanStreamCopy(videoCodec, audioCodec, pixFmt)

	// Calculate number of parts and segment duration
	numParts := CalculateNumParts(mediaInfo.FileSize)
	segmentDuration := mediaInfo.Duration / float64(numParts)

	logger.Info("Splitting video",
		"fileSize", mediaInfo.FileSize,
		"duration", mediaInfo.Duration,
		"numParts", numParts,
		"segmentDuration", segmentDuration,
		"canStreamCopy", canStreamCopy,
	)

	// Create output pattern
	dir := filepath.Dir(filePath)
	baseName := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
	outputPattern := filepath.Join(dir, baseName+"_part%03d.mp4")

	// Build ffmpeg args conditionally
	var args []string
	if canStreamCopy {
		// Branch A: Stream copy — zero RAM, instant split
		logger.Info("Splitting with stream copy (H264+AAC+8bit)",
			"videoCodec", videoCodec, "audioCodec", audioCodec, "pixFmt", pixFmt)
		args = []string{
			"-i", filePath,
			"-c", "copy",
			"-f", "segment",
			"-segment_time", fmt.Sprintf("%.2f", segmentDuration),
			"-segment_format_options", "movflags=+faststart",
			"-reset_timestamps", "1",
			"-y",
			outputPattern,
		}
	} else {
		// Branch B: Full re-encode with memory-safe settings
		logger.Info("Splitting with full re-encode (incompatible source)",
			"videoCodec", videoCodec, "audioCodec", audioCodec, "pixFmt", pixFmt)
		args = []string{
			"-i", filePath,
			"-c:v", "libx264",
			"-preset", "ultrafast",
			"-crf", "23",
			"-threads", "1",
			"-vf", "scale=-2:720",
			"-pix_fmt", "yuv420p",
			"-c:a", "aac",
			"-f", "segment",
			"-segment_time", fmt.Sprintf("%.2f", segmentDuration),
			"-segment_format_options", "movflags=+faststart",
			"-reset_timestamps", "1",
			"-y",
			outputPattern,
		}
	}

	logger.Debug("Running ffmpeg split", "args", args)

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)

	// Capture stderr for progress parsing
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start ffmpeg: %w", err)
	}

	// Parse ffmpeg progress output
	if progressCb != nil {
		go func() {
			scanner := bufio.NewScanner(stderr)
			// Match time=00:01:23.45 pattern
			timeRe := regexp.MustCompile(`time=(\d+):(\d+):(\d+\.?\d*)`)
			for scanner.Scan() {
				line := scanner.Text()
				if matches := timeRe.FindStringSubmatch(line); matches != nil {
					var hours, mins int
					var secs float64
					fmt.Sscanf(matches[1], "%d", &hours)
					fmt.Sscanf(matches[2], "%d", &mins)
					fmt.Sscanf(matches[3], "%f", &secs)
					currentTime := float64(hours*3600+mins*60) + secs
					percent := (currentTime / mediaInfo.Duration) * 100
					if percent > 100 {
						percent = 100
					}
					// Calculate which part we're on
					partNum := int(currentTime/segmentDuration) + 1
					if partNum > numParts {
						partNum = numParts
					}
					progressCb(Progress{
						Phase:      "splitting",
						Percent:    percent,
						PartNum:    partNum,
						TotalParts: numParts,
					})
				}
			}
		}()
	} else {
		// Drain stderr to prevent blocking
		go func() {
			scanner := bufio.NewScanner(stderr)
			for scanner.Scan() {
				logger.Debug("ffmpeg", "line", scanner.Text())
			}
		}()
	}

	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("ffmpeg split failed: %w", err)
	}

	// Find all created parts
	pattern := filepath.Join(dir, baseName+"_part*.mp4")
	partFiles, err := filepath.Glob(pattern)
	if err != nil || len(partFiles) == 0 {
		return nil, fmt.Errorf("no split parts found")
	}

	// Sort and create PartInfo list
	sort.Strings(partFiles)
	var parts []PartInfo
	for i, partFile := range partFiles {
		info, err := os.Stat(partFile)
		if err != nil {
			logger.Warn("Failed to stat split part", "file", partFile, "error", err)
			continue
		}
		parts = append(parts, PartInfo{
			FilePath: partFile,
			PartNum:  i + 1,
			FileSize: info.Size(),
		})
	}

	if len(parts) == 0 {
		return nil, fmt.Errorf("failed to get info for split parts")
	}

	logger.Info("Split complete", "numParts", len(parts))

	// Warn if any -c copy part exceeds MaxUploadSize (keyframe overshoot)
	if canStreamCopy {
		for _, p := range parts {
			if p.FileSize > MaxUploadSize {
				logger.Warn("Split part exceeds MaxUploadSize after -c copy split",
					"part", p.PartNum, "size", p.FileSize,
					"maxUploadSize", int64(MaxUploadSize), "file", p.FilePath)
			}
		}
	}

	return parts, nil
}
