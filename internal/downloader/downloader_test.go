package downloader

import (
	"testing"
)

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

func TestIs10Bit(t *testing.T) {
	tests := []struct {
		pixFmt string
		want   bool
	}{
		{"yuv420p10le", true},
		{"yuv422p10le", true},
		{"yuv420p10be", true},
		{"yuv444p12le", true},
		{"yuv420p12be", true},
		{"yuv420p16le", true},
		{"yuv420p16be", true},
		{"yuv420p14le", true},
		{"yuv420p14be", true},
		{"yuv420p", false},
		{"yuv422p", false},
		{"yuv444p", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.pixFmt, func(t *testing.T) {
			if got := Is10Bit(tt.pixFmt); got != tt.want {
				t.Errorf("Is10Bit(%q) = %v, want %v", tt.pixFmt, got, tt.want)
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
