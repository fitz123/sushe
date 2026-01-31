package downloader

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/fitz123/sushe/internal/logger"
)

// Progress represents download progress information
type Progress struct {
	Phase      string  // "downloading", "processing", "merging", "encoding", "splitting", "uploading"
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
	MaxUploadSize  = 1900 * 1024 * 1024 // 1.9GB - target size for splits
	DownloadDir    = "/tmp/sushe"
	DefaultTimeout = 60 * time.Minute // Increased for long videos
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
}

func New() *Downloader {
	// Ensure download directory exists
	os.MkdirAll(DownloadDir, 0755)

	return &Downloader{
		downloadDir: DownloadDir,
		timeout:     DefaultTimeout,
	}
}

// Download downloads a video from the given URL using yt-dlp
func (d *Downloader) Download(ctx context.Context, url string) (*DownloadResult, error) {
	return d.DownloadWithProgress(ctx, url, nil)
}

// DownloadWithProgress downloads a video and reports progress via callback
func (d *Downloader) DownloadWithProgress(ctx context.Context, url string, progressCb ProgressCallback) (*DownloadResult, error) {
	// Create unique subdirectory for this download
	downloadID := fmt.Sprintf("%d", time.Now().UnixNano())
	workDir := filepath.Join(d.downloadDir, downloadID)
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create work directory: %w", err)
	}

	// Output template
	outputTemplate := filepath.Join(workDir, "%(title).100s.%(ext)s")

	// Build yt-dlp command
	// Use --newline for parseable progress output
	// Prefer H.264 sources to avoid re-encoding, but accept any codec (will re-encode later if needed)
	args := []string{
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
	}

	logger.Debug("Running yt-dlp", "args", args)

	// Create context with timeout
	cmdCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "yt-dlp", args...)
	cmd.Dir = workDir

	// If we have a progress callback, stream output; otherwise use simple execution
	if progressCb != nil {
		if err := d.runWithProgress(cmd, progressCb); err != nil {
			logger.Error("yt-dlp failed", "error", err)
			os.RemoveAll(workDir)
			return nil, fmt.Errorf("download failed: %w", err)
		}
	} else {
		output, err := cmd.CombinedOutput()
		if err != nil {
			logger.Error("yt-dlp failed", "error", err, "output", string(output))
			os.RemoveAll(workDir)
			return nil, fmt.Errorf("download failed: %w - %s", err, string(output))
		}
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

	// Read both stdout and stderr
	scanner := bufio.NewScanner(stdout)
	go func() {
		// Drain stderr to prevent blocking
		stderrScanner := bufio.NewScanner(stderr)
		for stderrScanner.Scan() {
			logger.Debug("yt-dlp stderr", "line", stderrScanner.Text())
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

	return cmd.Wait()
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
	return int(math.Ceil(float64(fileSize) / float64(MaxUploadSize)))
}

// SplitVideo splits a video into parts of approximately MaxUploadSize
// It re-encodes for precise cuts at segment boundaries
func (d *Downloader) SplitVideo(ctx context.Context, filePath string, progressCb ProgressCallback) ([]PartInfo, error) {
	// Get media info
	mediaInfo, err := GetMediaInfo(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to get media info: %w", err)
	}

	if mediaInfo.Duration <= 0 {
		return nil, fmt.Errorf("invalid video duration: %f", mediaInfo.Duration)
	}

	// Calculate number of parts and segment duration
	numParts := CalculateNumParts(mediaInfo.FileSize)
	segmentDuration := mediaInfo.Duration / float64(numParts)

	logger.Info("Splitting video",
		"fileSize", mediaInfo.FileSize,
		"duration", mediaInfo.Duration,
		"numParts", numParts,
		"segmentDuration", segmentDuration,
	)

	// Create output pattern
	dir := filepath.Dir(filePath)
	baseName := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
	outputPattern := filepath.Join(dir, baseName+"_part%03d.mp4")

	// Build ffmpeg command for segmented output with re-encoding for precise cuts
	args := []string{
		"-i", filePath,
		"-c:v", "libx264",
		"-preset", "fast",
		"-crf", "23",
		"-c:a", "aac",
		"-movflags", "+faststart",
		"-f", "segment",
		"-segment_time", fmt.Sprintf("%.2f", segmentDuration),
		"-reset_timestamps", "1",
		"-y", // Overwrite output files
		outputPattern,
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
	var parts []PartInfo
	for i, partFile := range partFiles {
		info, err := os.Stat(partFile)
		if err != nil {
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
	return parts, nil
}
