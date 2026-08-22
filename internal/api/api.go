package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fitz123/sushe/internal/downloader"
	"github.com/fitz123/sushe/internal/engine"
	"github.com/fitz123/sushe/internal/logger"
	"github.com/fitz123/sushe/internal/upload"
	tele "gopkg.in/telebot.v3"
)

// apiEngineTimeout bounds API engine work (detection, download, processing,
// and splitting). It provides one downloader window plus an equal processing
// envelope. Telegram upload follows afterward under its own per-attempt HTTP
// client timeout and finite retry count.
const apiEngineTimeout = 2 * downloader.DefaultTimeout

var errAPIEngineDeadline = errors.New("API engine deadline exceeded")

// apiProcessor is the engine surface used by the HTTP handler. Keeping the
// seam private lets handler tests use a hermetic processor while preserving
// NewAPIService's concrete public constructor.
type apiProcessor interface {
	IsPlaylist(context.Context, string) (bool, *downloader.PlaylistInfo, error)
	Process(context.Context, string, engine.ProgressCallback) (*engine.ProcessResult, error)
	ProcessPlaylist(context.Context, string, func(int, int, string, float64)) ([]*engine.ProcessResult, error)
	Cleanup(*engine.ProcessResult)
}

// enginePhaseTracker records progress callbacks that may arrive from
// subprocess scanner goroutines while an API handler is inspecting the
// current phase to report a terminal error.
type enginePhaseTracker struct {
	mu    sync.RWMutex
	phase string
}

func (t *enginePhaseTracker) set(phase string) {
	if phase == "" {
		return
	}
	t.mu.Lock()
	t.phase = phase
	t.mu.Unlock()
}

func (t *enginePhaseTracker) current() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.phase == "" {
		return "starting"
	}
	return t.phase
}

// APIService handles HTTP API requests for video downloads.
type APIService struct {
	processor      apiProcessor
	bot            *tele.Bot
	token          string
	dedup          *dedupGuard
	engineTimeout  time.Duration // unexported per-service override for tests
	uploadResultFn func(*engine.ProcessResult, DownloadRequest) (int, error)
}

// NewAPIService creates a new API service.
func NewAPIService(eng *engine.Engine, bot *tele.Bot, token string) *APIService {
	svc := &APIService{
		processor:     eng,
		bot:           bot,
		token:         token,
		dedup:         newDedupGuard(),
		engineTimeout: apiEngineTimeout,
	}
	svc.uploadResultFn = svc.uploadResult
	return svc
}

// Close stops background resources (dedup cleanup goroutine).
func (s *APIService) Close() {
	s.dedup.Stop()
}

// Handler returns an http.Handler with all API routes.
func (s *APIService) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/download", s.handleDownload)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})
	return mux
}

// handleDownload processes POST /api/download requests.
//
// Dedup guard: requests are deduplicated by (url, chat_id, thread_id) key.
// If an identical request is already in progress, returns 409 Conflict.
// In-progress entries never expire — they are completed on full success or
// released immediately when the handler observes a failure, preventing long
// uploads from being swept mid-flight while keeping retries available.
// If an identical request completed within the unrelated cache TTL (15 minutes), returns
// the cached ResultEvent as a single NDJSON line with no preceding progress
// events. On failure (or partial playlist failure), the key is released so
// the client can retry.
func (s *APIService) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Auth check
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") || subtle.ConstantTimeCompare([]byte(authHeader[7:]), []byte(s.token)) != 1 {
		http.Error(w, `{"status":"error","ok":false,"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	// Parse request (limit body to 1MB to prevent DoS)
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req DownloadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"status":"error","ok":false,"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}

	if req.URL == "" {
		http.Error(w, `{"status":"error","ok":false,"error":"missing required field: url"}`, http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		http.Error(w, `{"status":"error","ok":false,"error":"url must use http:// or https:// scheme"}`, http.StatusBadRequest)
		return
	}
	if req.ChatID == 0 {
		http.Error(w, `{"status":"error","ok":false,"error":"missing required field: chat_id"}`, http.StatusBadRequest)
		return
	}

	// GENERAL topic warning (Decision 11)
	if req.ThreadID == 0 || req.ThreadID == 1 {
		logger.Warn("API request targets GENERAL topic (Bot API bug #447)", "chat_id", req.ChatID, "thread_id", req.ThreadID)
	}

	// Dedup guard: prevent duplicate processing of identical requests
	dedupKey := req.URL + "|" + strconv.FormatInt(req.ChatID, 10) + "|" + strconv.Itoa(req.ThreadID)
	cachedResult, acquired := s.dedup.TryAcquire(dedupKey)
	if cachedResult != nil {
		// Cache hit: return only the final ResultEvent, no progress events
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, `{"status":"error","ok":false,"error":"streaming not supported"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, flusher, cachedResult)
		return
	}
	if !acquired {
		http.Error(w, `{"status":"error","ok":false,"error":"duplicate request in progress"}`, http.StatusConflict)
		return
	}

	// Set streaming headers
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // Disable nginx/proxy buffering

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.dedup.Release(dedupKey)
		http.Error(w, `{"status":"error","ok":false,"error":"streaming not supported"}`, http.StatusInternalServerError)
		return
	}

	// The API deadline covers engine work (detection, download, processing, and
	// splitting). Upload does not accept this context; each send attempt is
	// instead bounded by the telebot HTTP client, with a finite retry count.
	engineTimeout := s.engineTimeout
	ctx, cancel := context.WithTimeoutCause(r.Context(), engineTimeout, errAPIEngineDeadline)
	defer cancel()
	phase := &enginePhaseTracker{}

	// Write started event
	writeJSON(w, flusher, ProgressEvent{Status: "started", URL: req.URL})

	// Check if playlist
	phase.set("playlist detection")
	isPlaylist, playlistInfo, playlistErr := s.processor.IsPlaylist(ctx, req.URL)
	if ctx.Err() != nil {
		if playlistErr == nil {
			playlistErr = ctx.Err()
		}
		handleErr := engineTerminalError(ctx, playlistErr, phase.current(), engineTimeout)
		logger.Error("API playlist detection failed", "url", req.URL, "phase", phase.current(), "engine_timeout", engineTimeout, "error", handleErr)
		writeJSON(w, flusher, ResultEvent{Status: "error", OK: false, Error: handleErr.Error()})
		s.dedup.Release(dedupKey)
		return
	}
	if isPlaylist && playlistInfo != nil {
		s.handlePlaylistDownload(ctx, w, flusher, req, playlistInfo, dedupKey, phase, engineTimeout)
		return
	}

	// Single video download
	phase.set("downloading")
	s.handleSingleDownload(ctx, w, flusher, req, dedupKey, phase, engineTimeout)
}

// handleSingleDownload processes a single video URL.
func (s *APIService) handleSingleDownload(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, req DownloadRequest, dedupKey string, phase *enginePhaseTracker, engineTimeout time.Duration) {
	var finalResult *ResultEvent
	var handleErr error
	defer func() {
		if handleErr != nil {
			s.dedup.Release(dedupKey)
		} else if finalResult != nil {
			s.dedup.Complete(dedupKey, finalResult)
		} else {
			s.dedup.Release(dedupKey) // Safety: release if neither set
		}
	}()

	progressCb := func(currentPhase string, percent float64, detail string) {
		if currentPhase != "" {
			// Record before writing so a concurrent cancellation reports the
			// newest phase even if flushing the progress event is slow.
			phase.set(currentPhase)
		}
		evt := ProgressEvent{
			Status:  currentPhase,
			Percent: percent,
		}
		switch currentPhase {
		case "encoding":
			if detail != "" {
				evt.Codec = detail
			}
		case "queued":
			// detail carries the remaining wait duration from waitForIGSlot.
			if detail != "" {
				evt.ETA = detail
			}
		}
		writeJSON(w, flusher, evt)
	}

	result, err := s.processor.Process(ctx, req.URL, progressCb)
	if err != nil {
		handleErr = engineTerminalError(ctx, err, phase.current(), engineTimeout)
		logger.Error("API engine job failed", "url", req.URL, "phase", phase.current(), "engine_timeout", engineTimeout, "error", handleErr)
		writeJSON(w, flusher, ResultEvent{Status: "error", OK: false, Error: handleErr.Error()})
		return
	}
	defer s.processor.Cleanup(result)

	// Upload via telebot
	msgID, err := s.uploadResultFn(result, req)
	if err != nil {
		handleErr = err
		writeJSON(w, flusher, ResultEvent{Status: "error", OK: false, Error: fmt.Sprintf("upload failed: %v", err)})
		return
	}

	finalResult = &ResultEvent{
		Status:    "done",
		OK:        true,
		Title:     result.Title,
		MessageID: msgID,
		FileSize:  result.FileSize,
	}
	writeJSON(w, flusher, finalResult)
}

// handlePlaylistDownload processes a playlist URL.
func (s *APIService) handlePlaylistDownload(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, req DownloadRequest, info interface{}, dedupKey string, phase *enginePhaseTracker, engineTimeout time.Duration) {
	var finalResult *ResultEvent
	var handleErr error
	defer func() {
		if handleErr != nil {
			s.dedup.Release(dedupKey)
		} else if finalResult != nil {
			s.dedup.Complete(dedupKey, finalResult)
		} else {
			s.dedup.Release(dedupKey) // Safety: release if neither set
		}
	}()

	phase.set("playlist processing")
	progressCb := func(videoNum, totalVideos int, currentPhase string, percent float64) {
		phase.set(currentPhase)
		writeJSON(w, flusher, ProgressEvent{
			Status:  currentPhase,
			Percent: percent,
			Video:   videoNum,
			Total:   totalVideos,
		})
	}

	results, err := s.processor.ProcessPlaylist(ctx, req.URL, progressCb)
	if err != nil {
		handleErr = engineTerminalError(ctx, err, phase.current(), engineTimeout)
		logger.Error("API playlist engine job failed", "url", req.URL, "phase", phase.current(), "engine_timeout", engineTimeout, "error", handleErr)
		writeJSON(w, flusher, ResultEvent{Status: "error", OK: false, Error: handleErr.Error()})
		return
	}

	var lastMsgID int
	var uploadedCount int
	for i, result := range results {
		videoNum := i + 1
		writeJSON(w, flusher, ProgressEvent{
			Status: "uploading",
			Video:  videoNum,
			Total:  len(results),
		})

		msgID, err := s.uploadResultFn(result, req)
		s.processor.Cleanup(result)

		if err != nil {
			logger.Error("Failed to upload playlist video", "video", videoNum, "error", err)
			writeJSON(w, flusher, ProgressEvent{
				Status: "upload_failed",
				Video:  videoNum,
				Total:  len(results),
			})
			continue
		}
		lastMsgID = msgID
		uploadedCount++
	}

	if uploadedCount == 0 {
		handleErr = fmt.Errorf("all %d playlist uploads failed", len(results))
		writeJSON(w, flusher, ResultEvent{Status: "error", OK: false, Error: handleErr.Error()})
		return
	}

	result := &ResultEvent{
		Status:    "done",
		OK:        true,
		Title:     fmt.Sprintf("Playlist: %d/%d videos uploaded", uploadedCount, len(results)),
		MessageID: lastMsgID,
	}
	// Only cache fully successful playlists. Partial failures release the dedup key
	// (via the else branch in defer) so the client can retry for missing videos.
	if uploadedCount == len(results) {
		finalResult = result
	}
	writeJSON(w, flusher, result)
}

// engineTerminalError classifies context termination by its cause. When the
// API's own deadline wins, the terminal message names the latest phase and the
// configured bound. Ordinary engine failures remain unchanged so extractor
// and processing details stay intact.
func engineTerminalError(ctx context.Context, engineErr error, phase string, engineTimeout time.Duration) error {
	if errors.Is(context.Cause(ctx), errAPIEngineDeadline) {
		return fmt.Errorf("API engine deadline exceeded during %s (limit %s): %w", phase, engineTimeout, context.DeadlineExceeded)
	}
	if ctx.Err() != nil {
		cause := context.Cause(ctx)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("request deadline exceeded during %s: %w", phase, cause)
		}
		return fmt.Errorf("request canceled by client during %s: %w", phase, cause)
	}
	return engineErr
}

// uploadResult uploads a ProcessResult to a Telegram chat via telebot.
// Returns the message ID of the sent message.
func (s *APIService) uploadResult(result *engine.ProcessResult, req DownloadRequest) (int, error) {
	recipient := chatRecipient{chatID: req.ChatID}
	sendOpts := &tele.SendOptions{}
	if req.ThreadID > 0 {
		sendOpts.ThreadID = req.ThreadID
	}

	if result.IsSplit {
		return s.uploadSplitParts(result, recipient, sendOpts)
	}

	return s.uploadSingleFile(result, result.FilePath, result.FileName, result.Title, recipient, sendOpts)
}

// uploadSingleFile uploads a single video file.
// Uses file:// URI so the local Bot API server reads directly from disk,
// avoiding HTTP multipart upload timeouts/EOF on large files.
func (s *APIService) uploadSingleFile(result *engine.ProcessResult, filePath, fileName, caption string, recipient tele.Recipient, opts *tele.SendOptions) (int, error) {
	video := &tele.Video{
		File:      tele.FromURL("file://" + filePath),
		FileName:  fileName,
		Caption:   caption,
		Width:     result.Width,
		Height:    result.Height,
		Duration:  int(result.Duration),
		Streaming: true,
	}

	logger.Info("Starting video upload",
		"file", fileName,
		"size", result.FileSize,
		"split", false,
	)
	started := time.Now()
	msg, err := upload.SendWithRetry(s.bot, recipient, video, opts)
	elapsed := time.Since(started)
	if err != nil {
		logger.Error("Video upload failed",
			"file", fileName,
			"size", result.FileSize,
			"elapsed_seconds", elapsed.Seconds(),
			"throughput_mib_s", upload.ThroughputMiBPerSecond(result.FileSize, elapsed),
			"error", err,
		)
		return 0, err
	}

	logger.Info("Video upload complete",
		"file", fileName,
		"size", result.FileSize,
		"elapsed_seconds", elapsed.Seconds(),
		"throughput_mib_s", upload.ThroughputMiBPerSecond(result.FileSize, elapsed),
		"message_id", msg.ID,
	)
	return msg.ID, nil
}

// uploadSplitParts uploads split video parts sequentially, threading each as a reply.
// Uses file:// URI so the local Bot API server reads directly from disk.
func (s *APIService) uploadSplitParts(result *engine.ProcessResult, recipient tele.Recipient, baseOpts *tele.SendOptions) (int, error) {
	var firstMsgID int
	var prevMsg *tele.Message

	for _, part := range result.Parts {
		caption := fmt.Sprintf("%s\n\nPart %d/%d", result.Title, part.PartNum, len(result.Parts))
		partFileName := fmt.Sprintf("%s_part%d.mp4", strings.TrimSuffix(result.FileName, ".mp4"), part.PartNum)

		video := &tele.Video{
			File:      tele.FromURL("file://" + part.FilePath),
			FileName:  partFileName,
			Caption:   caption,
			Width:     result.Width,
			Height:    result.Height,
			Duration:  int(result.Duration),
			Streaming: true,
		}

		opts := &tele.SendOptions{}
		if baseOpts != nil {
			opts.ThreadID = baseOpts.ThreadID
		}
		if prevMsg != nil {
			opts.ReplyTo = prevMsg
		}

		logger.Info("Starting split video part upload",
			"file", partFileName,
			"part", part.PartNum,
			"total_parts", len(result.Parts),
			"size", part.FileSize,
		)
		started := time.Now()
		msg, err := upload.SendWithRetry(s.bot, recipient, video, opts)
		elapsed := time.Since(started)
		if err != nil {
			logger.Error("Split video part upload failed",
				"file", partFileName,
				"part", part.PartNum,
				"total_parts", len(result.Parts),
				"size", part.FileSize,
				"elapsed_seconds", elapsed.Seconds(),
				"throughput_mib_s", upload.ThroughputMiBPerSecond(part.FileSize, elapsed),
				"error", err,
			)
			return firstMsgID, fmt.Errorf("failed to upload part %d: %w", part.PartNum, err)
		}

		logger.Info("Split video part upload complete",
			"file", partFileName,
			"part", part.PartNum,
			"total_parts", len(result.Parts),
			"size", part.FileSize,
			"elapsed_seconds", elapsed.Seconds(),
			"throughput_mib_s", upload.ThroughputMiBPerSecond(part.FileSize, elapsed),
			"message_id", msg.ID,
		)
		if part.PartNum == 1 {
			firstMsgID = msg.ID
		}
		prevMsg = msg
	}

	return firstMsgID, nil
}

// writeJSON writes a JSON object as an NDJSON line and flushes.
func writeJSON(w http.ResponseWriter, flusher http.Flusher, v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		logger.Error("Failed to marshal NDJSON event", "error", err)
		return
	}
	w.Write(data)
	w.Write([]byte("\n"))
	flusher.Flush()
}
