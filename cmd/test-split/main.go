package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/fitz123/sushe/internal/downloader"
	"github.com/fitz123/sushe/internal/logger"
)

func main() {
	logger.Init("info")

	testFile := "/tmp/sushe-test/short_video.mp4"

	// Check file exists
	info, err := os.Stat(testFile)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		fmt.Println("Run: ffmpeg -y -f lavfi -i testsrc=duration=10:size=1280x720:rate=30 -f lavfi -i sine=frequency=440:duration=10 -c:v libx264 -preset ultrafast -c:a aac /tmp/sushe-test/short_video.mp4")
		os.Exit(1)
	}

	fmt.Println("=== Testing GetMediaInfo ===")
	mediaInfo, err := downloader.GetMediaInfo(testFile)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("File: %s\n", testFile)
	fmt.Printf("Size: %d bytes (%.2f KB)\n", info.Size(), float64(info.Size())/1024)
	fmt.Printf("Duration: %.2f seconds\n", mediaInfo.Duration)
	fmt.Printf("Dimensions: %dx%d\n", mediaInfo.Width, mediaInfo.Height)
	fmt.Printf("Bitrate: %d bps\n", mediaInfo.Bitrate)

	fmt.Println("\n=== Testing NeedsSplit ===")
	fmt.Printf("NeedsSplit (actual): %v\n", downloader.NeedsSplit(info.Size()))
	fmt.Printf("NeedsSplit (1GB): %v\n", downloader.NeedsSplit(1*1024*1024*1024))
	fmt.Printf("NeedsSplit (2GB): %v\n", downloader.NeedsSplit(2*1024*1024*1024))
	fmt.Printf("NeedsSplit (5GB): %v\n", downloader.NeedsSplit(5*1024*1024*1024))

	fmt.Println("\n=== Testing CalculateNumParts ===")
	fmt.Printf("Parts for 1GB: %d\n", downloader.CalculateNumParts(1*1024*1024*1024))
	fmt.Printf("Parts for 2GB: %d\n", downloader.CalculateNumParts(2*1024*1024*1024))
	fmt.Printf("Parts for 5GB: %d\n", downloader.CalculateNumParts(5*1024*1024*1024))
	fmt.Printf("Parts for 10GB: %d\n", downloader.CalculateNumParts(10*1024*1024*1024))

	fmt.Println("\n=== Testing SplitVideo ===")
	fmt.Println("(This will split the 10-second video into 1 part since it's small)")

	d := downloader.New()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	progressCb := func(p downloader.Progress) {
		fmt.Printf("  Progress: %s - Part %d/%d - %.1f%%\n",
			p.Phase, p.PartNum, p.TotalParts, p.Percent)
	}

	start := time.Now()
	parts, err := d.SplitVideo(ctx, testFile, progressCb)
	elapsed := time.Since(start)

	if err != nil {
		fmt.Printf("SplitVideo error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\nSplit completed in %.2f seconds\n", elapsed.Seconds())
	fmt.Printf("Created %d part(s):\n", len(parts))
	for _, p := range parts {
		partInfo, _ := downloader.GetMediaInfo(p.FilePath)
		duration := 0.0
		if partInfo != nil {
			duration = partInfo.Duration
		}
		fmt.Printf("  Part %d: %s\n", p.PartNum, p.FilePath)
		fmt.Printf("          Size: %.2f KB, Duration: %.2fs\n",
			float64(p.FileSize)/1024, duration)
	}

	fmt.Println("\n=== All Tests Passed! ===")
}
