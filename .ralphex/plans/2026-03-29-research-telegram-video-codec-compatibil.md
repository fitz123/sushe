# Optimize Sushe Bot Split/Re-encode Pipeline

## Goal

Replace the unconditional re-encode in `SplitVideo()` with codec-aware logic to eliminate OOM kills on the 1.9GB RAM server.

**What:** When splitting large videos (>1.9GB) into parts for Telegram upload, use `-c copy` (stream copy) for H264+AAC+8-bit sources instead of re-encoding. Only re-encode when the source codec is incompatible (non-H264, 10-bit, non-AAC audio).

**Why:** The current `SplitVideo()` always re-encodes with `libx264 -preset fast -crf 23`, which consumes excessive RAM on the 1.9GB/2-CPU server and causes OOM kills. The input to `SplitVideo()` is already guaranteed to be H264 (the download pipeline handles codec conversion before splitting), making this re-encode redundant in the vast majority of cases.

**Success criteria:**
1. H264+AAC+8-bit videos split with `-c copy` (zero re-encoding, negligible RAM)
2. Non-H264, 10-bit, or non-AAC sources fall back to full re-encode with memory-safe settings
3. All split parts are playable inline in Telegram with streaming support
4. No OOM kills during split of 2GB+ videos on the production server
5. Existing tests pass, new unit tests cover codec detection and branching logic

**Non-goals:**
- Changing the download/re-encode pipeline (before split) -- that already works correctly, including `ReencodeToH264()`
- Changing upload logic
- Adding new Telegram Bot API features
- Optimizing playlist splitting (same fix applies but out of scope for explicit testing)

## Context

- **Repository:** `github.com/fitz123/sushe` (Go 1.23, testify)
- **Primary file:** `internal/downloader/downloader.go`
- **Engine integration:** `internal/engine/engine.go` (calls `SplitVideo` at line 53)
- **Server:** 1.9GB RAM, 2 CPUs, no swap
- **Existing codec detection:** `GetVideoCodec()` (line 775), `IsH264Compatible()` (line 795)
- **Missing helpers:** No `GetAudioCodec()` or `GetPixelFormat()` functions exist yet
- **Pipeline flow:** `DownloadWithProgress()` -> codec check -> re-encode/faststart -> `NeedsSplit()` -> `SplitVideo()`
- **Current SplitVideo args (lines 923-935):** `-c:v libx264 -preset fast -crf 23 -c:a aac -movflags +faststart -f segment -segment_time X -reset_timestamps 1`
- **Constants:** `MaxUploadSize = 1900 * 1024 * 1024` (line 38), `MaxFileSize = 2000 * 1024 * 1024` (line 37)
- **Dependencies:** ffmpeg, ffprobe (already required by the project)

## Validation Commands

```bash
# Build the project
cd /Users/ninja/src/sushe && go build ./...

# Run all tests
cd /Users/ninja/src/sushe && go test ./...

# Run specific downloader tests
cd /Users/ninja/src/sushe && go test -v ./internal/downloader/...

# Vet and lint
cd /Users/ninja/src/sushe && go vet ./...

# Manual test: send a large video (>1.9GB) through the bot and verify:
# 1. Split parts arrive as inline video (not document)
# 2. Parts play with streaming (no buffer-entire-file delay)
# 3. No OOM in server logs during split
# 4. Check server RAM usage: ssh server "free -m" during split
```

## Decisions

**D1 -- Split strategy for H264 sources:** `-c copy -f segment` (stream copy, no re-encoding). Zero RAM overhead.
Source: User answer Q1 = A.

**D2 -- Per-segment faststart:** Use `-segment_format_options movflags=+faststart` to ensure each segment has moov atom at front.
Source: User answer Q2 = A.

**D3 -- Audio handling:** `-c:a copy` if source audio is AAC; for non-AAC audio, use full re-encode branch (Branch B).
Source: User answer Q3, simplified in round 2 per validator feedback -- collapsed to 2 branches.

**D4 -- Split target size:** 1.7GB (`1700 * 1024 * 1024`) to provide 200MB safety margin for keyframe overshoot with `-c copy`.
Source: User answer Q4 = A.

**D5 -- Re-encode fallback preset (split path only):** `-preset ultrafast -threads 1 -vf scale=-2:720 -c:a aac` for the full re-encode branch in `SplitVideo()`. Caps resolution at 720p and limits to 1 thread to keep RAM bounded.
Source: User answer Q5 = A. Clarified in round 2: applies ONLY to the split fallback branch, NOT to the standalone `ReencodeToH264()`.

**D6 -- 10-bit H264:** Re-encode 10-bit H264 (`yuv420p10le`, `yuv422p10le`, etc.) to 8-bit via full re-encode branch (Branch B).
Source: User answer Q6 = B.

**D7 -- Testing approach:** Manual testing with real video through the bot (no automated integration test for Telegram playback).
Source: User answer Q7 = A.

**D8 -- Two branches, not three:** `SplitVideo()` has exactly two code paths: Branch A (`-c copy` for both video and audio) when source is H264 + AAC + 8-bit yuv420p; Branch B (full re-encode with memory-safe settings) for everything else. No middle branch for "copy video + re-encode audio only." Simpler code, fewer edge cases, near-zero practical impact since yt-dlp already prefers AAC sources and the download pipeline re-encodes non-H264 to H264+AAC.
Source: Round 2 validator feedback, user-approved.

**D9 -- Standalone ReencodeToH264() unchanged:** The `ReencodeToH264()` function (download path, not split path) keeps its current `-preset fast` settings. It is NOT modified by this plan. The download pipeline is an explicit non-goal. The ultrafast/720p/threads settings apply only to the split fallback branch (Task 3).
Source: Round 2 validator feedback, user-approved. Resolves Task 5 contradiction with non-goals.

**D10 -- Post-split size validation is warn-only:** After `-c copy` split, log a warning if any part exceeds `MaxUploadSize`. Do NOT implement a re-encode retry fallback. The 200MB safety margin (1.7GB target vs 1.9GB limit) is conservative enough for typical YouTube GOPs. If warnings fire in production, add the retry fallback in a follow-up task.
Source: Round 2 validator feedback, user-approved.

**[ASSUMED] A1 -- `-segment_format_options movflags=+faststart` works reliably with `-c copy -f segment`:** External research confirms this is the documented ffmpeg approach. Cannot verify from source code alone; depends on ffmpeg version on server.
Reason: ffmpeg documentation and community usage confirm this pattern.
Risk if wrong: Split parts won't stream in Telegram (user must tap to play). Mitigation: manual test catches this; fallback is post-processing each part with `ffmpeg -i part.mp4 -c copy -movflags +faststart part_fixed.mp4`.

**[ASSUMED] A2 -- Opus audio in MP4 works in Telegram:** Moot given D8. Non-AAC audio triggers full re-encode (Branch B), so Opus is never passed through with `-c copy`. Retained for documentation only.

**[ASSUMED] A3 -- Keyframe overshoot stays within 200MB:** YouTube H264 videos typically have 2-4 second GOPs at 1080p. At ~5-10 Mbps bitrate, a 4-second overshoot is ~5MB. The 200MB margin is very conservative.
Reason: Typical YouTube encoding parameters.
Risk if wrong: A part exceeds 1.9GB and Telegram rejects the upload. Mitigation: post-split size validation warns in logs (Task 4); upload retry logic already exists.

**[UNVERIFIED] U1 -- Server RAM/CPU specs (1.9GB, 2 CPU, no swap):** From project documentation, not verifiable from code.
**[UNVERIFIED] U2 -- `libx264 -preset fast` causes OOM on 1.9GB server:** Plausible given the specs but requires runtime evidence.
**[UNVERIFIED] U3 -- `ultrafast` preset fits in 1.9GB with 720p cap and `-threads 1`:** Plausible but depends on actual video characteristics.

## Assumptions

1. **Input to SplitVideo is always H264 after download pipeline.** Verified from code: `DownloadWithProgress()` either re-encodes non-H264 to H264 (line 208) or applies faststart to existing H264 (line 237). Evidence: `engine.go:31` calls `DownloadWithProgress`, then `engine.go:52-53` calls `SplitVideo` only after download completes.

2. **Existing `GetVideoCodec()` and `IsH264Compatible()` are reliable.** Verified from code: uses ffprobe with standard args (lines 775-798). No known issues.

3. **The `MaxUploadSize` constant is the correct threshold for split target.** Verified: line 38 defines `MaxUploadSize = 1900 * 1024 * 1024`. The new split target (1.7GB) is below this, providing safety margin.

4. **`-f segment` with `-c copy` produces independently playable MP4 files.** Based on ffmpeg documentation. Each segment starts at a keyframe and has proper headers.

5. **Progress callback parsing still works with `-c copy`.** `-c copy` emits `time=` progress lines to stderr just like re-encoding does. The existing progress parsing goroutine (lines 952-983) should work unchanged.

6. **With `-c copy`, actual segment count may differ from requested `numParts`.** FFmpeg's segment muxer cuts at keyframes, potentially producing fewer or more segments than the target. The existing `filepath.Glob` part collection (line 999-1003) already handles variable segment counts, so this is acceptable.

## Risk Register

| Risk | Severity | Mitigation | Rollback |
|------|----------|------------|----------|
| `-c copy` segments lack faststart (moov atom at end) | HIGH | Use `-segment_format_options movflags=+faststart`; verify in manual test | Post-process each part: `ffmpeg -i part -c copy -movflags +faststart part_fixed.mp4` |
| Keyframe-aligned cuts produce parts >1.9GB | MED | Split target reduced to 1.7GB (200MB margin); post-split size warning log | Increase margin further (1.5GB) or fall back to re-encode split in a follow-up |
| `-c copy` progress output differs from re-encode | LOW | Progress parsing uses `time=` regex which ffmpeg emits for both copy and encode modes | Disable progress for copy mode; user sees "splitting..." without percentage |
| Re-encode fallback still OOMs with `ultrafast -threads 1 -vf scale=-2:720` | MED | 720p + 1 thread drastically reduces memory; `ultrafast` uses minimal reference frames | Add `-maxrate 2M -bufsize 4M` to further cap memory; or reject the video with error |
| Audio codec detection returns unexpected value | LOW | Whitelist approach: only `aac` gets copy (Branch A), everything else gets full re-encode (Branch B) | Safe default: re-encode on any detection failure |
| 10-bit pixel format detection via ffprobe fails | LOW | Treat ffprobe failure as "needs re-encode" (safe default) | Manual inspection; the video would still re-encode correctly |
| `filepath.Glob` returns parts in wrong order after split | LOW | Already exists in current code (line 999-1003); Glob returns lexicographic order which matches `%03d` pattern | Sort parts explicitly by filename |

**Overall rollback:** `git revert` the merge commit. The previous unconditional re-encode behavior is the safe fallback.

## Tasks

### Task 1: Add `GetAudioCodec()`, `GetPixelFormat()`, and helper functions [HIGH]

**Goal:** Add ffprobe-based detection functions for audio codec and pixel format, mirroring the existing `GetVideoCodec()` pattern.

**Files:** Modify `internal/downloader/downloader.go`

- [x] Add `GetAudioCodec(filePath string) (string, error)` function after `GetVideoCodec` (after line 792):
  ```go
  // GetAudioCodec returns the audio codec name (e.g., "aac", "opus", "vorbis")
  func GetAudioCodec(filePath string) (string, error) {
      args := []string{
          "-v", "quiet",
          "-select_streams", "a:0",
          "-show_entries", "stream=codec_name",
          "-of", "csv=p=0",
          filePath,
      }
      cmd := exec.Command("ffprobe", args...)
      output, err := cmd.Output()
      if err != nil {
          return "", fmt.Errorf("ffprobe audio codec failed: %w", err)
      }
      return strings.TrimSpace(string(output)), nil
  }
  ```
- [x] Add `GetPixelFormat(filePath string) (string, error)` function:
  ```go
  // GetPixelFormat returns the pixel format (e.g., "yuv420p", "yuv420p10le")
  func GetPixelFormat(filePath string) (string, error) {
      args := []string{
          "-v", "quiet",
          "-select_streams", "v:0",
          "-show_entries", "stream=pix_fmt",
          "-of", "csv=p=0",
          filePath,
      }
      cmd := exec.Command("ffprobe", args...)
      output, err := cmd.Output()
      if err != nil {
          return "", fmt.Errorf("ffprobe pixel format failed: %w", err)
      }
      return strings.TrimSpace(string(output)), nil
  }
  ```
- [x] Add `Is10Bit(pixFmt string) bool` helper:
  ```go
  // Is10Bit returns true if the pixel format indicates 10-bit or higher color depth
  func Is10Bit(pixFmt string) bool {
      pixFmt = strings.ToLower(pixFmt)
      return strings.Contains(pixFmt, "10le") || strings.Contains(pixFmt, "10be") ||
          strings.Contains(pixFmt, "12le") || strings.Contains(pixFmt, "12be") ||
          strings.Contains(pixFmt, "16le") || strings.Contains(pixFmt, "16be")
  }
  ```
- [x] Add `IsAACCompatible(audioCodec string) bool` helper:
  ```go
  // IsAACCompatible returns true if the audio codec is AAC (safe for copy in Telegram)
  func IsAACCompatible(audioCodec string) bool {
      return strings.ToLower(audioCodec) == "aac"
  }
  ```

### Task 2: Add `MaxSplitSize` constant [HIGH]

**Goal:** Define the split target size with safety margin for keyframe overshoot.

**Files:** Modify `internal/downloader/downloader.go`

- [x] Add `MaxSplitSize` constant in the const block (after line 38):
  ```go
  MaxSplitSize = 1700 * 1024 * 1024 // 1.7GB - split target with keyframe overshoot margin
  ```
- [x] Update `CalculateNumParts` to use `MaxSplitSize` instead of `MaxUploadSize`:
  ```go
  func CalculateNumParts(fileSize int64) int {
      return int(math.Ceil(float64(fileSize) / float64(MaxSplitSize)))
  }
  ```
  Note: `NeedsSplit` should still use `MaxUploadSize` (1.9GB) as the threshold for _whether_ to split. Only the _target part size_ uses the lower `MaxSplitSize`. The `segmentDuration` calculation uses the updated `CalculateNumParts`, which now divides by `MaxSplitSize` instead of `MaxUploadSize`. The existing `outputPattern` and part collection logic remain unchanged.

### Task 3: Rewrite `SplitVideo()` with codec-aware two-branch logic [HIGH]

**Goal:** Replace unconditional re-encode with two branches: stream copy for H264+AAC+8-bit (Branch A), full re-encode for everything else (Branch B).

**Files:** Modify `internal/downloader/downloader.go` (function `SplitVideo`, lines 893-1025)

- [x] Add codec/audio/pixfmt detection at the start of `SplitVideo()`, after `GetMediaInfo`:
  ```go
  // Detect codecs to determine split strategy
  videoCodec, err := GetVideoCodec(filePath)
  if err != nil {
      logger.Warn("Failed to detect video codec, will re-encode", "error", err)
      videoCodec = "unknown"
  }

  audioCodec, err := GetAudioCodec(filePath)
  if err != nil {
      logger.Warn("Failed to detect audio codec, will re-encode audio", "error", err)
      audioCodec = "unknown"
  }

  pixFmt, err := GetPixelFormat(filePath)
  if err != nil {
      logger.Warn("Failed to detect pixel format, will re-encode", "error", err)
      pixFmt = "unknown"
  }

  canStreamCopy := IsH264Compatible(videoCodec) && IsAACCompatible(audioCodec) && !Is10Bit(pixFmt)
  ```
- [x] Build ffmpeg args conditionally with exactly two branches:
  ```go
  var args []string
  if canStreamCopy {
      // Branch A: Stream copy — zero RAM, instant split
      logger.Info("Splitting with stream copy (H264+AAC+8bit)",
          "videoCodec", videoCodec, "audioCodec", audioCodec, "pixFmt", pixFmt)
      args = []string{
          "-i", filePath,
          "-c", "copy",
          "-f", "segment",
          "-segment_time", fmt.Sprintf("%.2f", segmentDuration),
          "-segment_format_options", "movflags=+faststart",
          "-reset_timestamps", "1",
          "-y",
          outputPattern,
      }
  } else {
      // Branch B: Full re-encode with memory-safe settings
      logger.Info("Splitting with full re-encode (incompatible source)",
          "videoCodec", videoCodec, "audioCodec", audioCodec, "pixFmt", pixFmt)
      args = []string{
          "-i", filePath,
          "-c:v", "libx264",
          "-preset", "ultrafast",
          "-crf", "23",
          "-threads", "1",
          "-vf", "scale=-2:720",
          "-pix_fmt", "yuv420p",
          "-c:a", "aac",
          "-movflags", "+faststart",
          "-f", "segment",
          "-segment_time", fmt.Sprintf("%.2f", segmentDuration),
          "-reset_timestamps", "1",
          "-y",
          outputPattern,
      }
  }
  ```
- [x] Update the comment on the `SplitVideo` function to reflect the new behavior:
  ```go
  // SplitVideo splits a video into parts of approximately MaxSplitSize.
  // Uses stream copy (-c copy) for H264+AAC+8-bit sources (zero RAM overhead).
  // Falls back to full re-encode with memory-safe settings for incompatible codecs.
  ```
- [x] Keep the progress callback goroutine and part collection logic (lines 937-1025) unchanged -- they work for both copy and re-encode modes
- [x] Replace the old `-movflags +faststart` (which applies to the whole output, not individual segments) with `-segment_format_options movflags=+faststart` in Branch A (per-segment faststart). Branch B uses `-movflags +faststart` which ffmpeg applies to each segment when used with `-f segment`.

### Task 4: Add post-split size validation (warn-only) [MED]

**Goal:** After `-c copy` split, log a warning if any part exceeds `MaxUploadSize`. No retry/fallback logic -- just observability.

**Files:** Modify `internal/downloader/downloader.go` (inside `SplitVideo`, after part collection)

- [x] After collecting parts (around line 1017), add size validation for the copy branch only:
  ```go
  // Warn if any -c copy part exceeds MaxUploadSize (keyframe overshoot)
  if canStreamCopy {
      for _, p := range parts {
          if p.FileSize > MaxUploadSize {
              logger.Warn("Split part exceeds MaxUploadSize after -c copy split",
                  "part", p.PartNum, "size", p.FileSize,
                  "maxUploadSize", MaxUploadSize, "file", p.FilePath)
          }
      }
  }
  ```
  Note: No re-encode fallback. The 200MB safety margin (1.7GB target vs 1.9GB limit) is conservative enough. If this warning fires in production, a follow-up task will add the retry fallback.

### Task 5: Write unit tests for new helper functions [HIGH]

**Goal:** Test codec detection helpers and the branching logic. Check for existing `*_test.go` files in `internal/downloader/` first before creating a new file.

**Files:** Create `internal/downloader/downloader_test.go` (or add to existing test file)

- [x] Test `IsH264Compatible()` (existing function, tests cover current behavior):
  - Positive: `"h264"`, `"H264"`, `"avc"`, `"avc1"`
  - Negative: `"vp9"`, `"av1"`, `"hevc"`, `""`
- [x] Test `IsAACCompatible()` (new function):
  - Positive: `"aac"`, `"AAC"`
  - Negative: `"opus"`, `"vorbis"`, `"mp3"`, `""`
- [x] Test `Is10Bit()` (new function):
  - Positive: `"yuv420p10le"`, `"yuv422p10le"`, `"yuv420p10be"`, `"yuv444p12le"`
  - Negative: `"yuv420p"`, `"yuv422p"`, `"yuv444p"`, `""`
- [x] Test `CalculateNumParts()` with `MaxSplitSize`:
  - Input 1.7GB -> 1 part
  - Input 3.4GB -> 2 parts
  - Input 3.5GB -> 3 parts (ceil)
  - Input 1.8GB -> 2 parts (just over 1.7GB)
- [x] Test `NeedsSplit()` still uses `MaxUploadSize` (1.9GB threshold):
  - Input 1.9GB -> false
  - Input 1.9GB + 1 -> true
- [x] Test `canStreamCopy` decision logic (combined condition):
  - H264 + AAC + yuv420p -> true (Branch A)
  - VP9 + AAC + yuv420p -> false (Branch B)
  - H264 + Opus + yuv420p -> false (Branch B)
  - H264 + AAC + yuv420p10le -> false (Branch B)
  - unknown + unknown + unknown -> false (Branch B, safe default)

### Task 6: Verify acceptance criteria [HIGH]

- [ ] `go build ./...` succeeds
- [ ] `go test ./...` passes (all existing + new tests)
- [ ] `go vet ./...` clean
- [ ] Code review: no hardcoded paths, proper error wrapping, structured logging
- [ ] Verify the two split branches (copy vs full re-encode) each produce valid ffmpeg args
- [ ] Verify `MaxSplitSize` is used in `CalculateNumParts` and `MaxUploadSize` is used in `NeedsSplit`
- [ ] Manual test: send a >1.9GB H264+AAC video through the bot on the production server
  - Confirm parts arrive as inline video
  - Confirm streaming playback works (no full-download-first behavior)
  - Confirm server RAM stays low during split (`free -m` or monitoring)
  - Confirm no OOM in `dmesg` or journal

### Task 7: Update documentation [LOW]

- [ ] Update the `SplitVideo` function comment (already in Task 3)
- [ ] If the project has a CLAUDE.md or architecture doc, note the codec-aware split optimization
- [ ] Update any relevant issue/task with the commit hash and results

---

## Revision Diff (Round 1 -> Round 2)

### Removed: Task 5 (ReencodeToH264 preset change)
Round 1 Task 5 modified the standalone `ReencodeToH264()` function to use `ultrafast` preset. This contradicted the explicit non-goal "Changing the download/re-encode pipeline (before split)." The ultrafast/720p/threads settings are only needed in the split fallback branch (Task 3, Branch B), not in the standalone re-encode. Added **D9** to document this decision explicitly.

### Simplified: 3 branches -> 2 branches (Task 3)
Round 1 had three code paths: (1) copy video + copy audio, (2) copy video + re-encode audio, (3) full re-encode. The middle branch handled the near-theoretical edge case of H264+8-bit video with non-AAC audio. Since yt-dlp prefers AAC and the download pipeline re-encodes non-H264 to H264+AAC, this case is extremely rare. Collapsed to two branches: Branch A (`-c copy` for H264+AAC+8-bit) and Branch B (full re-encode for everything else). Added **D8** to document. Updated `canStreamCopy` logic to require all three conditions (H264 + AAC + 8-bit).

### Simplified: Task 4 (post-split size validation)
Round 1 Task 4 had a complex re-encode-fallback retry with a `splitVideoReencode()` helper function. This was over-engineering for the first iteration given the conservative 200MB margin. Simplified to warn-only: log if any part exceeds `MaxUploadSize`, no retry. Added **D10** to document. This also eliminates the completeness validator's concern about the incomplete `splitVideoReencode()` implementation sketch.

### Removed: Open questions
Round 1 Task 5 ended with "Ask Ninja: should the standalone ReencodeToH264() also cap at 720p?" Since Task 5 is removed entirely (D9), the question is moot. No open questions remain.

### Added: D8, D9, D10
Three new decisions documenting the round 2 simplifications.

### Added: Assumption 6 (segment count variance)
Documented that `-c copy` may produce a different number of segments than requested due to keyframe alignment, and that existing `filepath.Glob` handles this.

### Added: Overall rollback strategy
Added "Overall rollback: `git revert` the merge commit" to the Risk Register section.

### Renumbered tasks
Round 1 had 8 tasks. Round 2 has 7 tasks (removed old Task 5, renumbered old Tasks 6->5, 7->6, 8->7).

### Minor fixes
- Updated non-goals to explicitly include `ReencodeToH264()` as out of scope
- Updated D3 wording to reflect 2-branch model (non-AAC triggers Branch B, not a middle branch)
- Updated D5 to clarify it applies only to the split fallback branch
- Added note in Task 2 about `segmentDuration` using updated `CalculateNumParts`
- Added note in Task 5 to check for existing test files before creating new ones
- Added combined `canStreamCopy` test cases to Task 5
- A2 marked as moot given D8
- Task 6 (verification) updated to reference two branches instead of three
