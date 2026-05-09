package downloader

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestCookieArgs(t *testing.T) {
	tests := []struct {
		name string
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
