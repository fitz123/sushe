package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fitz123/sushe/internal/engine"
	"github.com/fitz123/sushe/internal/logger"
	"github.com/fitz123/sushe/internal/upload"
	tele "gopkg.in/telebot.v3"
)

// APIService handles HTTP API requests for video downloads.
type APIService struct {
	engine *engine.Engine
	bot    *tele.Bot
	token  string
	dedup  *dedupGuard
}

// NewAPIService creates a new API service.
func NewAPIService(eng *engine.Engine, bot *tele.Bot, token string) *APIService {
	return &APIService{
		engine: eng,
		bot:    bot,
		token:  token,
		dedup:  newDedupGuard(),
	}
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
// In-progress entries never expire — they are cleaned up when the request
// completes or fails, preventing long uploads from being swept mid-flight.
// If an identical request completed within the TTL (15 minutes), returns
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

	// Request timeout: 15 minutes
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()

	// Write started event
	writeJSON(w, flusher, ProgressEvent{Status: "started", URL: req.URL})

	// Check if playlist
	isPlaylist, playlistInfo, _ := s.engine.IsPlaylist(ctx, req.URL)
	if isPlaylist && playlistInfo != nil {
		s.handlePlaylistDownload(ctx, w, flusher, req, playlistInfo, dedupKey)
		return
	}

	// Single video download
	s.handleSingleDownload(ctx, w, flusher, req, dedupKey)
}

// handleSingleDownload processes a single video URL.
func (s *APIService) handleSingleDownload(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, req DownloadRequest, dedupKey string) {
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

	progressCb := func(phase string, percent float64, detail string) {
		evt := ProgressEvent{
			Status:  phase,
			Percent: percent,
		}
		if phase == "encoding" && detail != "" {
			evt.Codec = detail
		}
		writeJSON(w, flusher, evt)
	}

	result, err := s.engine.Process(ctx, req.URL, progressCb)
	if err != nil {
		handleErr = err
		writeJSON(w, flusher, ResultEvent{Status: "error", OK: false, Error: err.Error()})
		return
	}
	defer s.engine.Cleanup(result)

	// Upload via telebot
	msgID, err := s.uploadResult(result, req)
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
func (s *APIService) handlePlaylistDownload(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, req DownloadRequest, info interface{}, dedupKey string) {
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

	progressCb := func(videoNum, totalVideos int, phase string, percent float64) {
		writeJSON(w, flusher, ProgressEvent{
			Status:  phase,
			Percent: percent,
			Video:   videoNum,
			Total:   totalVideos,
		})
	}

	results, err := s.engine.ProcessPlaylist(ctx, req.URL, progressCb)
	if err != nil {
		handleErr = err
		writeJSON(w, flusher, ResultEvent{Status: "error", OK: false, Error: err.Error()})
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

		msgID, err := s.uploadResult(result, req)
		s.engine.Cleanup(result)

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
	// so the client can retry and deliver the missing videos.
	if uploadedCount == len(results) {
		finalResult = result
	}
	writeJSON(w, flusher, result)
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

	msg, err := upload.SendWithRetry(s.bot, recipient, video, opts)
	if err != nil {
		return 0, err
	}

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

		msg, err := upload.SendWithRetry(s.bot, recipient, video, opts)
		if err != nil {
			return firstMsgID, fmt.Errorf("failed to upload part %d: %w", part.PartNum, err)
		}

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

