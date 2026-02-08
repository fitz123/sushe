package bot

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fitz123/sushe/internal/downloader"
	"github.com/fitz123/sushe/internal/logger"
	tele "gopkg.in/telebot.v3"
)

// ProgressReader wraps an io.Reader to track upload progress
type ProgressReader struct {
	reader     io.Reader
	total      int64
	read       int64
	onProgress func(read, total int64)
}

func (pr *ProgressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	pr.read += int64(n)
	if pr.onProgress != nil {
		pr.onProgress(pr.read, pr.total)
	}
	return n, err
}

type BotService struct {
	bot        *tele.Bot
	downloader *downloader.Downloader
}

func NewBotService(bot *tele.Bot) *BotService {
	bs := &BotService{
		bot:        bot,
		downloader: downloader.New(),
	}
	bs.registerHandlers()
	return bs
}

func (bs *BotService) Start() {
	bs.bot.Start()
}

func (bs *BotService) Stop() {
	bs.bot.Stop()
}

func (bs *BotService) registerHandlers() {
	bs.bot.Handle("/start", bs.handleStart)
	bs.bot.Handle("/help", bs.handleHelp)

	// Handle all text messages to auto-detect URLs
	bs.bot.Handle(tele.OnText, bs.handleText)
}

func (bs *BotService) handleStart(c tele.Context) error {
	return c.Send(
		"Welcome to Sushe - Video Downloader Bot!\n\n" +
			"Just send me a video link and I'll download and re-upload it for you.\n\n" +
			"Supported platforms:\n" +
			"- YouTube\n" +
			"- Twitter/X\n" +
			"- TikTok\n" +
			"- Instagram\n" +
			"- Reddit\n" +
			"- And many more!\n\n" +
			"Large videos are automatically split into parts.",
	)
}

func (bs *BotService) handleHelp(c tele.Context) error {
	return c.Send(
		"How to use Sushe:\n\n" +
			"1. Send me any video URL\n" +
			"2. Wait for the download to complete\n" +
			"3. Receive the video directly in Telegram\n\n" +
			"Supported platforms include YouTube, Twitter, TikTok, Instagram, Reddit, Vimeo, and many others.\n\n" +
			"Features:\n" +
			"- Videos over 1.9GB are automatically split into parts\n" +
			"- Parts are threaded as replies for easy viewing\n" +
			"- Max resolution: 1080p\n\n" +
			"Limitations:\n" +
			"- No playlists (only single videos)",
	)
}

func (bs *BotService) handleText(c tele.Context) error {
	text := c.Text()

	// Extract URLs from the message
	urls := downloader.ExtractURLs(text)
	if len(urls) == 0 {
		// No URLs found, ignore the message or send help
		if !strings.HasPrefix(text, "/") {
			return c.Send("No video URL detected. Send me a link to download a video!")
		}
		return nil
	}

	// Process each URL (usually just one)
	for _, url := range urls {
		if err := bs.processURL(c, url); err != nil {
			logger.Error("Failed to process URL", "url", url, "error", err)
			// Error already sent to user in processURL
		}
	}

	return nil
}

func (bs *BotService) processURL(c tele.Context, url string) error {
	// Send initial status (no URL to avoid link preview)
	statusMsg, err := bs.bot.Send(c.Chat(), "Starting download...")
	if err != nil {
		return err
	}

	// Track last update time to avoid Telegram rate limits
	var lastUpdate time.Time
	var lastPercent float64
	var mu sync.Mutex
	const minUpdateInterval = 2 * time.Second

	// Progress callback for download (no URLs to avoid link previews)
	progressCb := func(p downloader.Progress) {
		mu.Lock()
		defer mu.Unlock()

		// Rate limit updates to avoid Telegram API limits
		now := time.Now()
		if now.Sub(lastUpdate) < minUpdateInterval && p.Percent < 100 {
			// Skip if we updated recently, unless it's 100%
			// Also skip if percent hasn't changed much
			if p.Percent-lastPercent < 5 {
				return
			}
		}

		var statusText string
		switch p.Phase {
		case "downloading":
			if p.Speed != "" && p.ETA != "" {
				statusText = fmt.Sprintf("Downloading: %.0f%%\nSize: %s | Speed: %s | ETA: %s",
					p.Percent, p.Total, p.Speed, p.ETA)
			} else {
				statusText = fmt.Sprintf("Downloading: %.0f%%", p.Percent)
			}
		case "merging":
			statusText = "Merging video and audio..."
		case "encoding":
			if p.Codec != "" && p.Percent == 0 {
				statusText = fmt.Sprintf("Downloaded %s format, converting to H.264...", strings.ToUpper(p.Codec))
			} else {
				statusText = fmt.Sprintf("Converting to H.264: %.0f%%", p.Percent)
			}
		case "splitting":
			statusText = fmt.Sprintf("Splitting video: Part %d/%d (%.0f%%)",
				p.PartNum, p.TotalParts, p.Percent)
		default:
			statusText = "Processing..."
		}

		if _, err := bs.bot.Edit(statusMsg, statusText); err != nil {
			logger.Debug("Failed to update status message", "error", err)
		} else {
			lastUpdate = now
			lastPercent = p.Percent
		}
	}

	// Download the video with progress
	ctx := context.Background()
	result, err := bs.downloader.DownloadWithProgress(ctx, url, progressCb)
	if err != nil {
		bs.bot.Edit(statusMsg, fmt.Sprintf("Download failed: %v", err))
		return err
	}
	defer bs.downloader.Cleanup(result)

	// Check if we need to split the video
	if downloader.NeedsSplit(result.FileSize) {
		return bs.handleLargeVideo(c, statusMsg, result, url, progressCb)
	}

	// Single file upload
	return bs.uploadSingleVideo(c, statusMsg, result)
}

// handleLargeVideo splits and uploads a video that exceeds the size limit
func (bs *BotService) handleLargeVideo(c tele.Context, statusMsg *tele.Message, result *downloader.DownloadResult, url string, progressCb downloader.ProgressCallback) error {
	numParts := downloader.CalculateNumParts(result.FileSize)
	bs.bot.Edit(statusMsg, fmt.Sprintf("Video is %s - splitting into %d parts...",
		formatSize(result.FileSize), numParts))

	// Split the video
	ctx := context.Background()
	parts, err := bs.downloader.SplitVideo(ctx, result.FilePath, progressCb)
	if err != nil {
		bs.bot.Edit(statusMsg, fmt.Sprintf("Failed to split video: %v", err))
		return err
	}

	totalParts := len(parts)
	var prevMsg *tele.Message

	// Upload each part
	for i, part := range parts {
		partNum := i + 1
		bs.bot.Edit(statusMsg, fmt.Sprintf("Uploading Part %d/%d: 0%%\n%s | %s",
			partNum, totalParts, result.Title, formatSize(part.FileSize)))

		file, err := os.Open(part.FilePath)
		if err != nil {
			bs.bot.Edit(statusMsg, fmt.Sprintf("Failed to open part %d: %v", partNum, err))
			return err
		}

		// Create progress reader for upload tracking
		var lastUploadUpdate time.Time
		var lastUploadPercent float64
		progressReader := &ProgressReader{
			reader: file,
			total:  part.FileSize,
			onProgress: func(read, total int64) {
				now := time.Now()
				percent := float64(read) / float64(total) * 100

				// Rate limit updates: every 2 seconds or every 10%
				if now.Sub(lastUploadUpdate) < 2*time.Second && percent-lastUploadPercent < 10 {
					return
				}

				statusText := fmt.Sprintf("Uploading Part %d/%d: %.0f%%\n%s | %s/%s",
					partNum, totalParts, percent, result.Title, formatSize(read), formatSize(total))
				if _, err := bs.bot.Edit(statusMsg, statusText); err == nil {
					lastUploadUpdate = now
					lastUploadPercent = percent
				}
			},
		}

		// Create caption with part info
		caption := fmt.Sprintf("%s\n\nPart %d/%d", result.Title, partNum, totalParts)
		partFileName := fmt.Sprintf("%s_part%d.mp4", strings.TrimSuffix(result.FileName, ".mp4"), partNum)

		video := &tele.Video{
			File:      tele.FromReader(progressReader),
			FileName:  partFileName,
			Caption:   caption,
			Width:     result.Width,
			Height:    result.Height,
			Streaming: true,
		}

		// Set up send options for threading
		opts := &tele.SendOptions{}
		if prevMsg != nil {
			opts.ReplyTo = prevMsg
		}

		// Send the video part
		sentMsg, err := bs.bot.Send(c.Chat(), video, opts)
		file.Close()

		if err != nil {
			// Try as document if video fails
			logger.Warn("Failed to send part as video, trying as document", "part", partNum, "error", err)

			file2, err2 := os.Open(part.FilePath)
			if err2 != nil {
				bs.bot.Edit(statusMsg, fmt.Sprintf("Failed to send part %d: %v", partNum, err))
				return err
			}

			doc := &tele.Document{
				File:     tele.FromReader(file2),
				FileName: partFileName,
				Caption:  caption,
			}

			sentMsg, err = bs.bot.Send(c.Chat(), doc, opts)
			file2.Close()

			if err != nil {
				bs.bot.Edit(statusMsg, fmt.Sprintf("Failed to upload part %d: %v", partNum, err))
				return err
			}
		}

		// Use this message as reply target for next part (threading)
		prevMsg = sentMsg

		logger.Info("Uploaded video part",
			"part", partNum,
			"total", totalParts,
			"size", part.FileSize,
		)
	}

	// Delete status message on success
	bs.bot.Delete(statusMsg)

	logger.Info("Successfully processed large video",
		"url", url,
		"title", result.Title,
		"totalSize", result.FileSize,
		"parts", totalParts,
		"user", c.Sender().Username,
	)

	return nil
}

// uploadSingleVideo uploads a video that doesn't need splitting
func (bs *BotService) uploadSingleVideo(c tele.Context, statusMsg *tele.Message, result *downloader.DownloadResult) error {
	// Update status for upload phase
	bs.bot.Edit(statusMsg, fmt.Sprintf("Uploading: 0%%\n%s | %s",
		result.Title, formatSize(result.FileSize)))

	// Open the file
	file, err := os.Open(result.FilePath)
	if err != nil {
		bs.bot.Edit(statusMsg, fmt.Sprintf("Failed to open downloaded file: %v", err))
		return err
	}
	defer file.Close()

	// Create progress reader for upload tracking
	var lastUploadUpdate time.Time
	var lastUploadPercent float64
	progressReader := &ProgressReader{
		reader: file,
		total:  result.FileSize,
		onProgress: func(read, total int64) {
			now := time.Now()
			percent := float64(read) / float64(total) * 100

			// Rate limit updates: every 2 seconds or every 10%
			if now.Sub(lastUploadUpdate) < 2*time.Second && percent-lastUploadPercent < 10 {
				return
			}

			statusText := fmt.Sprintf("Uploading: %.0f%%\n%s | %s/%s",
				percent, result.Title, formatSize(read), formatSize(total))
			if _, err := bs.bot.Edit(statusMsg, statusText); err == nil {
				lastUploadUpdate = now
				lastUploadPercent = percent
			}
		},
	}

	// Create video with dimensions for proper display
	video := &tele.Video{
		File:      tele.FromReader(progressReader),
		FileName:  result.FileName,
		Caption:   result.Title,
		Width:     result.Width,
		Height:    result.Height,
		Streaming: true,
	}

	// Send the video
	_, err = bs.bot.Send(c.Chat(), video)
	if err != nil {
		// If video fails, try sending as document
		logger.Warn("Failed to send as video, trying as document", "error", err)

		// Re-open file (reader was consumed)
		file2, err2 := os.Open(result.FilePath)
		if err2 != nil {
			bs.bot.Edit(statusMsg, fmt.Sprintf("Failed to send video: %v", err))
			return err
		}
		defer file2.Close()

		doc := &tele.Document{
			File:     tele.FromReader(file2),
			FileName: result.FileName,
			Caption:  result.Title,
		}

		_, err = bs.bot.Send(c.Chat(), doc)
		if err != nil {
			bs.bot.Edit(statusMsg, fmt.Sprintf("Failed to upload: %v", err))
			return err
		}
	}

	// Delete status message on success
	bs.bot.Delete(statusMsg)

	logger.Info("Successfully processed video",
		"title", result.Title,
		"size", result.FileSize,
		"user", c.Sender().Username,
	)

	return nil
}

// formatSize formats bytes into human readable format
func formatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
