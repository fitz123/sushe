package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fitz123/sushe/internal/downloader"
	"github.com/fitz123/sushe/internal/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	logger.Init("error") // quiet logger for tests
	os.Exit(m.Run())
}

func TestProcessResultFields(t *testing.T) {
	// Test that ProcessResult correctly holds all expected fields
	pr := &ProcessResult{
		FilePath:  "/tmp/test/video.mp4",
		FilePaths: []string{"/tmp/test/video.mp4"},
		FileName:  "video.mp4",
		Title:     "Test Video",
		Duration:  120.5,
		Width:     1920,
		Height:    1080,
		FileSize:  1024 * 1024,
		IsSplit:   false,
		WorkDir:   "/tmp/test",
	}

	assert.Equal(t, "/tmp/test/video.mp4", pr.FilePath)
	assert.Equal(t, "video.mp4", pr.FileName)
	assert.Equal(t, "Test Video", pr.Title)
	assert.Equal(t, 120.5, pr.Duration)
	assert.Equal(t, 1920, pr.Width)
	assert.Equal(t, 1080, pr.Height)
	assert.Equal(t, int64(1024*1024), pr.FileSize)
	assert.False(t, pr.IsSplit)
	assert.Equal(t, "/tmp/test", pr.WorkDir)
	assert.Len(t, pr.FilePaths, 1)
}

func TestProcessResultSplit(t *testing.T) {
	pr := &ProcessResult{
		FilePath: "/tmp/test/video_part001.mp4",
		FilePaths: []string{
			"/tmp/test/video_part001.mp4",
			"/tmp/test/video_part002.mp4",
			"/tmp/test/video_part003.mp4",
		},
		FileName: "video.mp4",
		Title:    "Big Video",
		FileSize: 5 * 1024 * 1024 * 1024, // 5GB
		IsSplit:  true,
		Parts: []PartResult{
			{FilePath: "/tmp/test/video_part001.mp4", PartNum: 1, FileSize: 1900 * 1024 * 1024},
			{FilePath: "/tmp/test/video_part002.mp4", PartNum: 2, FileSize: 1900 * 1024 * 1024},
			{FilePath: "/tmp/test/video_part003.mp4", PartNum: 3, FileSize: 1500 * 1024 * 1024},
		},
		WorkDir: "/tmp/test",
	}

	assert.True(t, pr.IsSplit)
	assert.Len(t, pr.Parts, 3)
	assert.Len(t, pr.FilePaths, 3)
	assert.Equal(t, 1, pr.Parts[0].PartNum)
	assert.Equal(t, 2, pr.Parts[1].PartNum)
	assert.Equal(t, 3, pr.Parts[2].PartNum)
}

func TestCleanupRemovesWorkDir(t *testing.T) {
	// Create a temp directory
	tmpDir := t.TempDir()
	workDir := filepath.Join(tmpDir, "test-work")
	require.NoError(t, os.MkdirAll(workDir, 0755))

	// Create a dummy file
	testFile := filepath.Join(workDir, "test.mp4")
	require.NoError(t, os.WriteFile(testFile, []byte("test"), 0644))

	// Verify the directory exists
	_, err := os.Stat(workDir)
	require.NoError(t, err)

	// Cleanup via engine
	eng := &Engine{}
	result := &ProcessResult{
		FilePath: testFile,
		WorkDir:  workDir,
	}
	eng.Cleanup(result)

	// Verify the directory is removed
	_, err = os.Stat(workDir)
	assert.True(t, os.IsNotExist(err))
}

func TestCleanupNilResult(t *testing.T) {
	eng := &Engine{}
	// Should not panic
	eng.Cleanup(nil)
}

func TestCleanupEmptyWorkDir(t *testing.T) {
	eng := &Engine{}
	result := &ProcessResult{WorkDir: ""}
	// Should not panic
	eng.Cleanup(result)
}

func TestAdaptProgressCbNil(t *testing.T) {
	cb := adaptProgressCb(nil)
	assert.Nil(t, cb)
}

func TestAdaptProgressCbDownloading(t *testing.T) {
	var gotPhase string
	var gotPercent float64
	var gotDetail string

	cb := adaptProgressCb(func(phase string, percent float64, detail string) {
		gotPhase = phase
		gotPercent = percent
		gotDetail = detail
	})

	require.NotNil(t, cb)

	// Simulate a downloading progress from downloader
	cb(downloader.Progress{
		Phase:   "downloading",
		Percent: 45.2,
		Speed:   "3.5MiB/s",
	})

	assert.Equal(t, "downloading", gotPhase)
	assert.Equal(t, 45.2, gotPercent)
	assert.Equal(t, "3.5MiB/s", gotDetail)
}

func TestAdaptProgressCbEncoding(t *testing.T) {
	var gotPhase, gotDetail string

	cb := adaptProgressCb(func(phase string, percent float64, detail string) {
		gotPhase = phase
		gotDetail = detail
	})

	cb(downloader.Progress{
		Phase: "encoding",
		Codec: "vp9",
	})

	assert.Equal(t, "encoding", gotPhase)
	assert.Equal(t, "vp9", gotDetail)
}

func TestAdaptProgressCbSplitting(t *testing.T) {
	var gotPhase, gotDetail string

	cb := adaptProgressCb(func(phase string, percent float64, detail string) {
		gotPhase = phase
		gotDetail = detail
	})

	cb(downloader.Progress{
		Phase:      "splitting",
		PartNum:    2,
		TotalParts: 3,
	})

	assert.Equal(t, "splitting", gotPhase)
	assert.Equal(t, "part 2/3", gotDetail)
}

func TestNewEngine(t *testing.T) {
	eng := NewEngine()
	assert.NotNil(t, eng)
	assert.NotNil(t, eng.downloader)
}
