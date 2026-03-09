package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fitz123/sushe/internal/downloader"
	"github.com/fitz123/sushe/internal/logger"
)

// Engine encapsulates the download → codec-check → transcode → split pipeline.
// It does NOT upload — it returns local file paths and metadata.
type Engine struct {
	downloader *downloader.Downloader
}

// NewEngine creates a new Engine with a fresh Downloader instance.
func NewEngine() *Engine {
	return &Engine{
		downloader: downloader.New(),
	}
}

// Process downloads and processes a single video URL.
// Returns a ProcessResult with file paths and metadata. Caller is responsible for upload and cleanup.
func (e *Engine) Process(ctx context.Context, url string, progressCb ProgressCallback) (*ProcessResult, error) {
	dlCb := adaptProgressCb(progressCb)

	result, err := e.downloader.DownloadWithProgress(ctx, url, dlCb)
	if err != nil {
		return nil, err
	}

	workDir := filepath.Dir(result.FilePath)

	pr := &ProcessResult{
		FilePath:  result.FilePath,
		FilePaths: []string{result.FilePath},
		FileName:  result.FileName,
		Title:     result.Title,
		Duration:  result.Duration,
		Width:     result.Width,
		Height:    result.Height,
		FileSize:  result.FileSize,
		IsSplit:   false,
		WorkDir:   workDir,
	}

	// Check if splitting is needed
	if downloader.NeedsSplit(result.FileSize) {
		parts, err := e.downloader.SplitVideo(ctx, result.FilePath, dlCb)
		if err != nil {
			// Cleanup on split failure
			os.RemoveAll(workDir)
			return nil, fmt.Errorf("failed to split video: %w", err)
		}

		pr.IsSplit = true
		pr.FilePaths = make([]string, len(parts))
		pr.Parts = make([]PartResult, len(parts))
		for i, p := range parts {
			pr.FilePaths[i] = p.FilePath
			pr.Parts[i] = PartResult{
				FilePath: p.FilePath,
				PartNum:  p.PartNum,
				FileSize: p.FileSize,
			}
		}
	}

	return pr, nil
}

// ProcessPlaylist downloads and processes all videos in a playlist.
// Returns a slice of ProcessResults. Failed individual videos are logged and skipped.
func (e *Engine) ProcessPlaylist(ctx context.Context, url string, progressCb func(videoNum, totalVideos int, phase string, percent float64)) ([]*ProcessResult, error) {
	info, err := e.downloader.GetPlaylistInfo(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("failed to get playlist info: %w", err)
	}

	var results []*ProcessResult

	for i, entry := range info.Entries {
		videoNum := i + 1

		// Per-video progress adapter
		var dlCb downloader.ProgressCallback
		if progressCb != nil {
			dlCb = func(p downloader.Progress) {
				progressCb(videoNum, info.PlaylistCount, p.Phase, p.Percent)
			}
		}

		result, err := e.downloader.DownloadPlaylistVideo(ctx, url, i, dlCb)
		if err != nil {
			logger.Error("Failed to download playlist video", "index", i, "title", entry.Title, "error", err)
			continue
		}

		workDir := filepath.Dir(result.FilePath)
		pr := &ProcessResult{
			FilePath:  result.FilePath,
			FilePaths: []string{result.FilePath},
			FileName:  result.FileName,
			Title:     result.Title,
			Duration:  result.Duration,
			Width:     result.Width,
			Height:    result.Height,
			FileSize:  result.FileSize,
			IsSplit:   false,
			WorkDir:   workDir,
		}

		// Check if splitting is needed
		if downloader.NeedsSplit(result.FileSize) {
			parts, err := e.downloader.SplitVideo(ctx, result.FilePath, dlCb)
			if err != nil {
				logger.Error("Failed to split playlist video", "index", i, "title", entry.Title, "error", err)
				os.RemoveAll(workDir)
				continue
			}

			pr.IsSplit = true
			pr.FilePaths = make([]string, len(parts))
			pr.Parts = make([]PartResult, len(parts))
			for j, p := range parts {
				pr.FilePaths[j] = p.FilePath
				pr.Parts[j] = PartResult{
					FilePath: p.FilePath,
					PartNum:  p.PartNum,
					FileSize: p.FileSize,
				}
			}
		}

		results = append(results, pr)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no videos successfully processed from playlist")
	}

	return results, nil
}

// IsPlaylist checks if a URL is a playlist and returns playlist info if so.
func (e *Engine) IsPlaylist(ctx context.Context, url string) (bool, *downloader.PlaylistInfo, error) {
	info, err := e.downloader.GetPlaylistInfo(ctx, url)
	if err != nil {
		return false, nil, err
	}
	return true, info, nil
}

// Cleanup removes the work directory for a ProcessResult.
func (e *Engine) Cleanup(result *ProcessResult) {
	if result != nil && result.WorkDir != "" {
		os.RemoveAll(result.WorkDir)
		logger.Debug("Cleaned up work directory", "dir", result.WorkDir)
	}
}
