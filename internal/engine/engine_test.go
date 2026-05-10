package engine

import (
	"context"
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
	eng := NewEngine("")
	assert.NotNil(t, eng)
	assert.NotNil(t, eng.downloader)
}

// TestIsPlaylistShortCircuitsIGSinglePost covers Layer 2 of the IG account
// exposure reduction: canonical Instagram single-post URLs (/p/, /reel/, /tv/)
// must NOT invoke GetPlaylistInfo because the URL pattern is syntactically
// sufficient to classify them as single videos. Doubling the yt-dlp IG hit
// for every common-case URL was the original problem this short-circuit fixes.
//
// We pass a pre-cancelled context: if the short-circuit fails and the call
// falls through to yt-dlp, exec.CommandContext returns a context error. A
// successful short-circuit returns (false, nil, nil) without touching the
// downloader.
func TestIsPlaylistShortCircuitsIGSinglePost(t *testing.T) {
	eng := NewEngine("")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled BEFORE the call — yt-dlp would fail immediately if invoked

	tests := []struct {
		name string
		url  string
	}{
		{"/p/<id>/", "https://www.instagram.com/p/CXXXXX/"},
		{"/reel/<id>/", "https://www.instagram.com/reel/CYYYYY/"},
		{"/tv/<id>/", "https://www.instagram.com/tv/CZZZZZ/"},
		{"/reel/<id> with query", "https://www.instagram.com/reel/CYYYYY/?igsh=abc"},
		{"bare instagram.com /p/", "https://instagram.com/p/CXXXXX/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isPL, info, err := eng.IsPlaylist(ctx, tt.url)
			require.NoError(t, err, "short-circuit must NOT invoke yt-dlp (would return ctx error)")
			assert.False(t, isPL, "IG single-post URL must classify as non-playlist")
			assert.Nil(t, info, "IG single-post short-circuit must not return playlist info")
		})
	}
}

// TestIsPlaylistFallsThroughForNonSinglePostURLs verifies the negative path:
// when the URL is NOT an IG single-post URL (non-IG host, or IG host with a
// non-/p|reel|tv/ path), IsPlaylist must fall through to GetPlaylistInfo. We
// detect fall-through by passing a cancelled context and asserting that an
// error is returned — the only way IsPlaylist returns an error is via the
// downstream GetPlaylistInfo call, which exec.CommandContext will fail under
// a cancelled context.
func TestIsPlaylistFallsThroughForNonSinglePostURLs(t *testing.T) {
	eng := NewEngine("")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		url  string
	}{
		{"IG /explore/", "https://www.instagram.com/explore/"},
		{"IG /<user>/saved/", "https://www.instagram.com/some_user/saved/"},
		{"IG profile only", "https://www.instagram.com/some_user/"},
		{"non-IG youtube playlist", "https://www.youtube.com/playlist?list=PLabc"},
		{"non-IG tiktok", "https://www.tiktok.com/@user/video/12345"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := eng.IsPlaylist(ctx, tt.url)
			assert.Error(t, err, "non-single-post URLs must fall through to yt-dlp (ctx cancelled → error)")
		})
	}
}
