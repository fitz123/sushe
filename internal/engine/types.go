package engine

import (
	"fmt"

	"github.com/fitz123/sushe/internal/downloader"
)

// ProgressCallback is called with progress updates during processing.
// phase: "queued", "downloading", "encoding", "merging", "splitting"
// percent: 0-100 (zero for "queued"; the wait duration is conveyed via detail)
// detail: optional extra info (codec name, speed, queued ETA, etc.)
type ProgressCallback func(phase string, percent float64, detail string)

// PartResult describes a single split video part.
type PartResult struct {
	FilePath string
	PartNum  int
	FileSize int64
}

// ProcessResult contains the result of processing a single video URL.
// The caller is responsible for upload and cleanup.
type ProcessResult struct {
	FilePath  string       // Main file path (or first part if split)
	FilePaths []string     // All file paths (single element or split parts)
	FileName  string
	Title     string
	Duration  float64
	Width     int
	Height    int
	FileSize  int64        // Total size (pre-split original)
	IsSplit   bool
	Parts     []PartResult // Populated if IsSplit is true
	WorkDir   string       // Directory to clean up
}

// adaptProgressCb converts an engine ProgressCallback to a downloader ProgressCallback.
func adaptProgressCb(cb ProgressCallback) downloader.ProgressCallback {
	if cb == nil {
		return nil
	}
	return func(p downloader.Progress) {
		detail := ""
		switch p.Phase {
		case "downloading":
			if p.Speed != "" {
				detail = p.Speed
			}
		case "encoding":
			if p.Codec != "" {
				detail = p.Codec
			}
		case "splitting":
			detail = fmt.Sprintf("part %d/%d", p.PartNum, p.TotalParts)
		case "queued":
			// Forward ETA so the bot/API caller can render the wait duration.
			detail = p.ETA
		}
		cb(p.Phase, p.Percent, detail)
	}
}
