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
	"github.com/fitz123/sushe/internal/engine"
	"github.com/fitz123/sushe/internal/logger"
	"github.com/fitz123/sushe/internal/upload"
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
	bot          *tele.Bot
	engine       *engine.Engine
	allowedUsers AllowedUsers
}

func NewBotService(bot *tele.Bot, eng *engine.Engine, allowedUsers AllowedUsers) *BotService {
	bs := &BotService{
		bot:          bot,
		engine:       eng,
		allowedUsers: allowedUsers,
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
	// Apply auth middleware to restrict access to whitelisted users
	bs.bot.Use(AuthMiddleware(bs.allowedUsers))

	bs.bot.Handle("/start", bs.handleStart)
	bs.bot.Handle("/help", bs.handleHelp)
	bs.bot.Handle("/dl", bs.handleDL)

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
			"1. Send me any video URL or playlist URL\n" +
			"2. Wait for the download to complete\n" +
			"3. Receive the video(s) directly in Telegram\n\n" +
			"Supported platforms include YouTube, Twitter, TikTok, Instagram, Reddit, Vimeo, and many others.\n\n" +
			"Features:\n" +
			"- Videos over 1.9GB are automatically split into parts\n" +
			"- Parts are threaded as replies for easy viewing\n" +
			"- Playlist support (max 50 videos per playlist)\n" +
			"- Playlist videos are threaded as reply chain\n" +
			"- Max resolution: 1080p\n\n" +
			"Playlist Limitations:\n" +
			"- Max 50 videos per playlist\n" +
			"- Videos longer than 2 hours are skipped",
	)
}

// handleDL handles the /dl command with GENERAL topic guard
func (bs *BotService) handleDL(c tele.Context) error {
	// GENERAL topic guard (Bot API bug #447)
	if c.Message() != nil && c.Chat() != nil && c.Chat().Type != tele.ChatPrivate {
		threadID := c.Message().ThreadID
		if threadID == 0 || threadID == 1 {
			return c.Send("⚠️ Please use /dl in a named topic (not General)")
		}
	}

	text := c.Message().Payload
	if text == "" {
		return c.Send("Usage: /dl <video URL>")
	}

	urls := downloader.ExtractURLs(text)
	if len(urls) == 0 {
		return c.Send("No video URL detected. Send a valid link after /dl")
	}

	for _, url := range urls {
		if err := bs.processURL(c, url); err != nil {
			logger.Error("Failed to process URL", "url", url, "error", err)
		}
	}

	return nil
}

func (bs *BotService) handleText(c tele.Context) error {
	// In group chats, silently ignore non-URL text (avoid spam)
	if c.Chat() != nil && c.Chat().Type != tele.ChatPrivate {
		// GENERAL topic guard (Bot API bug #447) — silently ignore
		if c.Message() != nil {
			threadID := c.Message().ThreadID
			if threadID == 0 || threadID == 1 {
				return nil
			}
		}
	}

	text := c.Text()

	// Extract URLs from the message
	urls := downloader.ExtractURLs(text)
	if len(urls) == 0 {
		// No URLs found — only send help in private chats
		if c.Chat() != nil && c.Chat().Type == tele.ChatPrivate && !strings.HasPrefix(text, "/") {
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// First check if this is a playlist
	isPlaylist, playlistInfo, _ := bs.engine.IsPlaylist(ctx, url)
	if isPlaylist && playlistInfo != nil {
		return bs.processPlaylist(c, url, playlistInfo)
	}

	// Not a playlist, process as single video
	statusMsg, err := bs.bot.Send(c.Chat(), "Starting download...", &tele.SendOptions{ThreadID: c.Message().ThreadID})
	if err != nil {
		return err
	}

	// Progress callback for download — updates Telegram status message
	var lastUpdate time.Time
	var lastPercent float64
	var mu sync.Mutex
	const minUpdateInterval = 2 * time.Second

	progressCb := func(phase string, percent float64, detail string) {
		mu.Lock()
		defer mu.Unlock()

		now := time.Now()
		if now.Sub(lastUpdate) < minUpdateInterval && percent < 100 {
			if percent-lastPercent < 5 {
				return
			}
		}

		var statusText string
		switch phase {
		case "downloading":
			if detail != "" {
				statusText = fmt.Sprintf("Downloading: %.0f%% | %s", percent, detail)
			} else {
				statusText = fmt.Sprintf("Downloading: %.0f%%", percent)
			}
		case "merging":
			statusText = "Merging video and audio..."
		case "encoding":
			if detail != "" && percent == 0 {
				statusText = fmt.Sprintf("Downloaded %s format, converting to H.264...", strings.ToUpper(detail))
			} else {
				statusText = fmt.Sprintf("Converting to H.264: %.0f%%", percent)
			}
		case "splitting":
			if detail != "" {
				statusText = fmt.Sprintf("Splitting video: %s (%.0f%%)", detail, percent)
			} else {
				statusText = fmt.Sprintf("Splitting video: %.0f%%", percent)
			}
		default:
			statusText = "Processing..."
		}

		if _, err := bs.bot.Edit(statusMsg, statusText); err != nil {
			logger.Debug("Failed to update status message", "error", err)
		} else {
			lastUpdate = now
			lastPercent = percent
		}
	}

	// Download and process via engine
	result, err := bs.engine.Process(ctx, url, progressCb)
	if err != nil {
		bs.bot.Edit(statusMsg, fmt.Sprintf("Download failed: %v", err))
		return err
	}
	defer bs.engine.Cleanup(result)

	// Upload
	if result.IsSplit {
		return bs.uploadSplitVideo(c, statusMsg, result, nil)
	}
	return bs.uploadSingleVideo(c, statusMsg, result)
}

// processPlaylist handles downloading and uploading playlist videos
func (bs *BotService) processPlaylist(c tele.Context, playlistURL string, playlistInfo *downloader.PlaylistInfo) error {
	playlistMsg := fmt.Sprintf("Playlist: %s — %d videos", playlistInfo.Title, playlistInfo.PlaylistCount)
	statusMsg, err := bs.bot.Send(c.Chat(), playlistMsg, &tele.SendOptions{ThreadID: c.Message().ThreadID})
	if err != nil {
		return err
	}

	// Progress callback for playlist downloads
	progressCb := func(videoNum, totalVideos int, phase string, percent float64) {
		var statusText string
		switch phase {
		case "downloading":
			statusText = fmt.Sprintf("Video %d/%d: Downloading %.0f%%", videoNum, totalVideos, percent)
		case "encoding":
			statusText = fmt.Sprintf("Video %d/%d: Converting to H.264: %.0f%%", videoNum, totalVideos, percent)
		case "splitting":
			statusText = fmt.Sprintf("Video %d/%d: Splitting: %.0f%%", videoNum, totalVideos, percent)
		default:
			statusText = fmt.Sprintf("Video %d/%d: Processing...", videoNum, totalVideos)
		}
		bs.bot.Edit(statusMsg, statusText)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	results, err := bs.engine.ProcessPlaylist(ctx, playlistURL, progressCb)
	if err != nil {
		bs.bot.Edit(statusMsg, fmt.Sprintf("Playlist download failed: %v", err))
		return err
	}

	var lastReplyMsg *tele.Message

	for i, result := range results {
		videoNum := i + 1

		// Update status for upload phase
		bs.bot.Edit(statusMsg, fmt.Sprintf("Video %d/%d: Uploading...\n%s | %s",
			videoNum, len(results), result.Title, formatSize(result.FileSize)))

		var uploadedMsg *tele.Message
		var uploadErr error

		if result.IsSplit {
			uploadedMsg, uploadErr = bs.uploadPlaylistSplitVideo(c, statusMsg, result, videoNum, len(results), lastReplyMsg)
		} else {
			uploadedMsg, uploadErr = bs.uploadPlaylistSingleVideo(c, statusMsg, result, videoNum, len(results), lastReplyMsg)
		}

		bs.engine.Cleanup(result)

		if uploadErr != nil {
			logger.Error("Failed to upload playlist video", "index", i, "title", result.Title, "error", uploadErr)
			bs.bot.Edit(statusMsg, fmt.Sprintf("Video %d/%d: Upload failed - %v\n%s",
				videoNum, len(results), uploadErr, result.Title))
			time.Sleep(2 * time.Second)
			continue
		}

		lastReplyMsg = uploadedMsg

		logger.Info("Successfully processed playlist video",
			"index", i+1,
			"title", result.Title,
			"size", result.FileSize,
			"user", c.Sender().Username)
	}

	bs.bot.Delete(statusMsg)

	logger.Info("Successfully processed playlist",
		"title", playlistInfo.Title,
		"videos", playlistInfo.PlaylistCount,
		"user", c.Sender().Username)

	return nil
}

// uploadSingleVideo uploads a non-split video result
func (bs *BotService) uploadSingleVideo(c tele.Context, statusMsg *tele.Message, result *engine.ProcessResult) error {
	sendOpts := &tele.SendOptions{ThreadID: c.Message().ThreadID}
	bs.bot.Edit(statusMsg, fmt.Sprintf("Uploading: 0%%\n%s | %s",
		result.Title, formatSize(result.FileSize)))

	file, err := os.Open(result.FilePath)
	if err != nil {
		bs.bot.Edit(statusMsg, fmt.Sprintf("Failed to open downloaded file: %v", err))
		return err
	}
	defer file.Close()

	var lastUploadUpdate time.Time
	var lastUploadPercent float64
	progressReader := &ProgressReader{
		reader: file,
		total:  result.FileSize,
		onProgress: func(read, total int64) {
			now := time.Now()
			percent := float64(read) / float64(total) * 100
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

	video := &tele.Video{
		File:      tele.FromReader(progressReader),
		FileName:  result.FileName,
		Caption:   result.Title,
		Width:     result.Width,
		Height:    result.Height,
		Duration:  int(result.Duration),
		Streaming: true,
	}

	_, err = upload.SendWithRetry(bs.bot, c.Chat(), video, sendOpts)
	if err != nil {
		logger.Warn("Failed to send as video, trying as document", "error", err)

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

		_, err = upload.SendWithRetry(bs.bot, c.Chat(), doc, sendOpts)
		if err != nil {
			bs.bot.Edit(statusMsg, fmt.Sprintf("Failed to upload: %v", err))
			return err
		}
	}

	bs.bot.Delete(statusMsg)

	logger.Info("Successfully processed video",
		"title", result.Title,
		"size", result.FileSize,
		"user", c.Sender().Username,
	)

	return nil
}

// uploadSplitVideo uploads a split video (multiple parts) with threading
func (bs *BotService) uploadSplitVideo(c tele.Context, statusMsg *tele.Message, result *engine.ProcessResult, replyTo *tele.Message) error {
	totalParts := len(result.Parts)
	var prevMsg *tele.Message = replyTo

	for _, part := range result.Parts {
		partNum := part.PartNum
		bs.bot.Edit(statusMsg, fmt.Sprintf("Uploading Part %d/%d: 0%%\n%s | %s",
			partNum, totalParts, result.Title, formatSize(part.FileSize)))

		file, err := os.Open(part.FilePath)
		if err != nil {
			bs.bot.Edit(statusMsg, fmt.Sprintf("Failed to open part %d: %v", partNum, err))
			return err
		}

		var lastUploadUpdate time.Time
		var lastUploadPercent float64
		progressReader := &ProgressReader{
			reader: file,
			total:  part.FileSize,
			onProgress: func(read, total int64) {
				now := time.Now()
				percent := float64(read) / float64(total) * 100
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

		caption := fmt.Sprintf("%s\n\nPart %d/%d", result.Title, partNum, totalParts)
		partFileName := fmt.Sprintf("%s_part%d.mp4", strings.TrimSuffix(result.FileName, ".mp4"), partNum)

		video := &tele.Video{
			File:      tele.FromReader(progressReader),
			FileName:  partFileName,
			Caption:   caption,
			Width:     result.Width,
			Height:    result.Height,
			Duration:  int(result.Duration),
			Streaming: true,
		}

		opts := &tele.SendOptions{ThreadID: c.Message().ThreadID}
		if prevMsg != nil {
			opts.ReplyTo = prevMsg
		}

		sentMsg, err := upload.SendWithRetry(bs.bot, c.Chat(), video, opts)
		file.Close()

		if err != nil {
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

			sentMsg, err = upload.SendWithRetry(bs.bot, c.Chat(), doc, opts)
			file2.Close()

			if err != nil {
				bs.bot.Edit(statusMsg, fmt.Sprintf("Failed to upload part %d: %v", partNum, err))
				return err
			}
		}

		prevMsg = sentMsg

		logger.Info("Uploaded video part",
			"part", partNum,
			"total", totalParts,
			"size", part.FileSize,
		)
	}

	bs.bot.Delete(statusMsg)

	logger.Info("Successfully processed split video",
		"title", result.Title,
		"totalSize", result.FileSize,
		"parts", totalParts,
		"user", c.Sender().Username,
	)

	return nil
}

// uploadPlaylistSingleVideo uploads a single video from a playlist
func (bs *BotService) uploadPlaylistSingleVideo(c tele.Context, statusMsg *tele.Message, result *engine.ProcessResult, videoNum, totalVideos int, replyTo *tele.Message) (*tele.Message, error) {
	statusText := fmt.Sprintf("Video %d/%d: Uploading 0%%\n%s | %s",
		videoNum, totalVideos, result.Title, formatSize(result.FileSize))
	bs.bot.Edit(statusMsg, statusText)

	file, err := os.Open(result.FilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open downloaded file: %w", err)
	}
	defer file.Close()

	var lastUploadUpdate time.Time
	var lastUploadPercent float64
	progressReader := &ProgressReader{
		reader: file,
		total:  result.FileSize,
		onProgress: func(read, total int64) {
			now := time.Now()
			percent := float64(read) / float64(total) * 100
			if now.Sub(lastUploadUpdate) < 2*time.Second && percent-lastUploadPercent < 10 {
				return
			}
			statusText := fmt.Sprintf("Video %d/%d: Uploading %.0f%%\n%s | %s/%s",
				videoNum, totalVideos, percent, result.Title, formatSize(read), formatSize(total))
			if _, err := bs.bot.Edit(statusMsg, statusText); err == nil {
				lastUploadUpdate = now
				lastUploadPercent = percent
			}
		},
	}

	caption := fmt.Sprintf("%s\n\nVideo %d/%d", result.Title, videoNum, totalVideos)
	video := &tele.Video{
		File:      tele.FromReader(progressReader),
		FileName:  result.FileName,
		Caption:   caption,
		Width:     result.Width,
		Height:    result.Height,
		Duration:  int(result.Duration),
		Streaming: true,
	}

	opts := &tele.SendOptions{ThreadID: c.Message().ThreadID}
	if replyTo != nil {
		opts.ReplyTo = replyTo
	}

	sentMsg, err := upload.SendWithRetry(bs.bot, c.Chat(), video, opts)
	if err != nil {
		logger.Warn("Failed to send playlist video as video, trying as document", "video", videoNum, "error", err)

		file2, err2 := os.Open(result.FilePath)
		if err2 != nil {
			return nil, fmt.Errorf("failed to send video: %w", err)
		}
		defer file2.Close()

		doc := &tele.Document{
			File:     tele.FromReader(file2),
			FileName: result.FileName,
			Caption:  caption,
		}

		sentMsg, err = upload.SendWithRetry(bs.bot, c.Chat(), doc, opts)
		if err != nil {
			return nil, fmt.Errorf("failed to upload: %w", err)
		}
	}

	return sentMsg, nil
}

// uploadPlaylistSplitVideo uploads a split video from a playlist (multiple parts)
func (bs *BotService) uploadPlaylistSplitVideo(c tele.Context, statusMsg *tele.Message, result *engine.ProcessResult, videoNum, totalVideos int, replyTo *tele.Message) (*tele.Message, error) {
	totalParts := len(result.Parts)
	var lastPartMsg *tele.Message
	var firstPartMsg *tele.Message

	for _, part := range result.Parts {
		partNum := part.PartNum
		statusText := fmt.Sprintf("Video %d/%d: Uploading Part %d/%d: 0%%\n%s | %s",
			videoNum, totalVideos, partNum, totalParts, result.Title, formatSize(part.FileSize))
		bs.bot.Edit(statusMsg, statusText)

		file, err := os.Open(part.FilePath)
		if err != nil {
			return lastPartMsg, fmt.Errorf("failed to open part %d: %v", partNum, err)
		}

		var lastUploadUpdate time.Time
		var lastUploadPercent float64
		progressReader := &ProgressReader{
			reader: file,
			total:  part.FileSize,
			onProgress: func(read, total int64) {
				now := time.Now()
				percent := float64(read) / float64(total) * 100
				if now.Sub(lastUploadUpdate) < 2*time.Second && percent-lastUploadPercent < 10 {
					return
				}
				statusText := fmt.Sprintf("Video %d/%d: Uploading Part %d/%d: %.0f%%\n%s | %s/%s",
					videoNum, totalVideos, partNum, totalParts, percent, result.Title, formatSize(read), formatSize(total))
				if _, err := bs.bot.Edit(statusMsg, statusText); err == nil {
					lastUploadUpdate = now
					lastUploadPercent = percent
				}
			},
		}

		caption := fmt.Sprintf("%s\n\nVideo %d/%d - Part %d/%d", result.Title, videoNum, totalVideos, partNum, totalParts)
		partFileName := fmt.Sprintf("%s_part%d.mp4", strings.TrimSuffix(result.FileName, ".mp4"), partNum)

		video := &tele.Video{
			File:      tele.FromReader(progressReader),
			FileName:  partFileName,
			Caption:   caption,
			Width:     result.Width,
			Height:    result.Height,
			Duration:  int(result.Duration),
			Streaming: true,
		}

		opts := &tele.SendOptions{ThreadID: c.Message().ThreadID}
		if partNum == 1 {
			if replyTo != nil {
				opts.ReplyTo = replyTo
			}
		} else {
			if lastPartMsg != nil {
				opts.ReplyTo = lastPartMsg
			}
		}

		sentMsg, err := upload.SendWithRetry(bs.bot, c.Chat(), video, opts)
		file.Close()

		if err != nil {
			logger.Warn("Failed to send playlist video part as video, trying as document", "video", videoNum, "part", partNum, "error", err)

			file2, err2 := os.Open(part.FilePath)
			if err2 != nil {
				return lastPartMsg, fmt.Errorf("failed to send part %d: %v", partNum, err)
			}

			doc := &tele.Document{
				File:     tele.FromReader(file2),
				FileName: partFileName,
				Caption:  caption,
			}

			sentMsg, err = upload.SendWithRetry(bs.bot, c.Chat(), doc, opts)
			file2.Close()

			if err != nil {
				return lastPartMsg, fmt.Errorf("failed to upload part %d: %v", partNum, err)
			}
		}

		if partNum == 1 {
			firstPartMsg = sentMsg
		}
		lastPartMsg = sentMsg

		logger.Info("Uploaded playlist video part",
			"video", videoNum,
			"part", partNum,
			"totalParts", totalParts,
			"size", part.FileSize,
		)
	}

	if firstPartMsg != nil {
		return firstPartMsg, nil
	}
	return lastPartMsg, nil
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
