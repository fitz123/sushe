package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fitz123/sushe/internal/downloader"
	"github.com/fitz123/sushe/internal/engine"
	"github.com/fitz123/sushe/internal/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	logger.Init("error")
}

func newTestService(t *testing.T) *APIService {
	// Create API service with nil bot (tests that don't upload won't need it)
	eng := engine.NewEngine("", "")
	svc := NewAPIService(eng, nil, "test-secret-token")
	t.Cleanup(func() { svc.Close() })
	return svc
}

type fakeAPIProcessor struct {
	mu             sync.Mutex
	processCalls   int
	processCtxEnds []time.Time
	cleanupCalls   int
	processFn      func(context.Context, engine.ProgressCallback, int) (*engine.ProcessResult, error)
}

func (f *fakeAPIProcessor) IsPlaylist(context.Context, string) (bool, *downloader.PlaylistInfo, error) {
	return false, nil, nil
}

func (f *fakeAPIProcessor) Process(ctx context.Context, _ string, progressCb engine.ProgressCallback) (*engine.ProcessResult, error) {
	deadline, _ := ctx.Deadline()
	f.mu.Lock()
	f.processCalls++
	call := f.processCalls
	f.processCtxEnds = append(f.processCtxEnds, deadline)
	processFn := f.processFn
	f.mu.Unlock()

	return processFn(ctx, progressCb, call)
}

func (f *fakeAPIProcessor) ProcessPlaylist(context.Context, string, func(int, int, string, float64)) ([]*engine.ProcessResult, error) {
	return nil, errors.New("unexpected playlist processing")
}

func (f *fakeAPIProcessor) Cleanup(*engine.ProcessResult) {
	f.mu.Lock()
	f.cleanupCalls++
	f.mu.Unlock()
}

func (f *fakeAPIProcessor) snapshot() (int, []time.Time, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.processCalls, append([]time.Time(nil), f.processCtxEnds...), f.cleanupCalls
}

func newAPIRequest(ctx context.Context) *http.Request {
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/download", strings.NewReader(`{"url":"https://example.com/video","chat_id":123}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-secret-token")
	return req
}

func decodeNDJSON(t *testing.T, body string) []map[string]interface{} {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(body), "\n")
	events := make([]map[string]interface{}, 0, len(lines))
	for _, line := range lines {
		var event map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(line), &event))
		events = append(events, event)
	}
	return events
}

func TestDownloadHandlerDefaultDeadlineCompletesAndCaches(t *testing.T) {
	result := &engine.ProcessResult{Title: "Large Video", FileSize: 4_596_000_000}
	processor := &fakeAPIProcessor{
		processFn: func(_ context.Context, progressCb engine.ProgressCallback, _ int) (*engine.ProcessResult, error) {
			progressCb("downloading", 99, "")
			return result, nil
		},
	}
	svc := newTestService(t)
	svc.processor = processor
	uploadCalls := 0
	svc.uploadResultFn = func(*engine.ProcessResult, DownloadRequest) (int, error) {
		uploadCalls++
		return 789, nil
	}

	startedAt := time.Now()
	first := httptest.NewRecorder()
	svc.Handler().ServeHTTP(first, newAPIRequest(context.Background()))

	require.Equal(t, http.StatusOK, first.Code)
	firstEvents := decodeNDJSON(t, first.Body.String())
	require.GreaterOrEqual(t, len(firstEvents), 2)
	firstResult := firstEvents[len(firstEvents)-1]
	assert.Equal(t, "done", firstResult["status"])
	assert.Equal(t, true, firstResult["ok"])
	assert.Equal(t, "Large Video", firstResult["title"])
	assert.Equal(t, float64(789), firstResult["message_id"])

	processCalls, deadlines, cleanupCalls := processor.snapshot()
	require.Len(t, deadlines, 1)
	assert.Greater(t, apiEngineTimeout, 15*time.Minute)
	assert.Equal(t, 2*downloader.DefaultTimeout, apiEngineTimeout)
	assert.WithinDuration(t, startedAt.Add(apiEngineTimeout), deadlines[0], time.Second)
	assert.Equal(t, 1, processCalls)
	assert.Equal(t, 1, cleanupCalls)
	assert.Equal(t, 1, uploadCalls)

	cached := httptest.NewRecorder()
	svc.Handler().ServeHTTP(cached, newAPIRequest(context.Background()))

	require.Equal(t, http.StatusOK, cached.Code)
	cachedEvents := decodeNDJSON(t, cached.Body.String())
	require.Len(t, cachedEvents, 1)
	assert.Equal(t, firstResult, cachedEvents[0])
	processCalls, _, cleanupCalls = processor.snapshot()
	assert.Equal(t, 1, processCalls)
	assert.Equal(t, 1, cleanupCalls)
	assert.Equal(t, 1, uploadCalls)
}

func TestDownloadHandlerAPIDeadlineReportsPhaseAndReleasesDedup(t *testing.T) {
	const testTimeout = 15 * time.Millisecond
	processor := &fakeAPIProcessor{
		processFn: func(ctx context.Context, progressCb engine.ProgressCallback, call int) (*engine.ProcessResult, error) {
			if call == 1 {
				progressCb("splitting", 50, "")
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return &engine.ProcessResult{Title: "Retry succeeded"}, nil
		},
	}
	svc := newTestService(t)
	svc.processor = processor
	svc.engineTimeout = testTimeout
	svc.uploadResultFn = func(*engine.ProcessResult, DownloadRequest) (int, error) { return 900, nil }

	timedOut := httptest.NewRecorder()
	svc.Handler().ServeHTTP(timedOut, newAPIRequest(context.Background()))

	require.Equal(t, http.StatusOK, timedOut.Code)
	events := decodeNDJSON(t, timedOut.Body.String())
	terminal := events[len(events)-1]
	assert.Equal(t, "error", terminal["status"])
	assert.Equal(t, false, terminal["ok"])
	assert.Contains(t, terminal["error"], "API engine deadline exceeded during splitting")
	assert.Contains(t, terminal["error"], "limit "+testTimeout.String())

	retry := httptest.NewRecorder()
	svc.Handler().ServeHTTP(retry, newAPIRequest(context.Background()))

	retryEvents := decodeNDJSON(t, retry.Body.String())
	assert.Equal(t, "done", retryEvents[len(retryEvents)-1]["status"])
	processCalls, _, _ := processor.snapshot()
	assert.Equal(t, 2, processCalls, "a released dedup key must allow an immediate retry")
}

func TestDownloadHandlerCallerCancellationIsNotAPIDeadlineAndReleasesDedup(t *testing.T) {
	entered := make(chan struct{})
	processor := &fakeAPIProcessor{
		processFn: func(ctx context.Context, progressCb engine.ProgressCallback, call int) (*engine.ProcessResult, error) {
			if call == 1 {
				progressCb("encoding", 20, "vp9")
				close(entered)
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return &engine.ProcessResult{Title: "Retry succeeded"}, nil
		},
	}
	svc := newTestService(t)
	svc.processor = processor
	svc.uploadResultFn = func(*engine.ProcessResult, DownloadRequest) (int, error) { return 901, nil }

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	canceled := httptest.NewRecorder()
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		svc.Handler().ServeHTTP(canceled, newAPIRequest(requestCtx))
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("fake processor did not start")
	}
	cancelRequest()
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("handler did not return after caller cancellation")
	}

	events := decodeNDJSON(t, canceled.Body.String())
	terminal := events[len(events)-1]
	assert.Equal(t, "error", terminal["status"])
	assert.Contains(t, terminal["error"], "request canceled by client during encoding")
	assert.NotContains(t, terminal["error"], "API engine deadline exceeded")

	retry := httptest.NewRecorder()
	svc.Handler().ServeHTTP(retry, newAPIRequest(context.Background()))
	retryEvents := decodeNDJSON(t, retry.Body.String())
	assert.Equal(t, "done", retryEvents[len(retryEvents)-1]["status"])
	processCalls, _, _ := processor.snapshot()
	assert.Equal(t, 2, processCalls, "a canceled request must release its dedup key")
}

func TestAuthMissingToken(t *testing.T) {
	svc := newTestService(t)
	handler := svc.Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/download", strings.NewReader(`{"url":"https://example.com","chat_id":123}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "unauthorized")
}

func TestAuthWrongToken(t *testing.T) {
	svc := newTestService(t)
	handler := svc.Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/download", strings.NewReader(`{"url":"https://example.com","chat_id":123}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer wrong-token")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "unauthorized")
}

func TestAuthCorrectTokenProceeds(t *testing.T) {
	svc := newTestService(t)
	handler := svc.Handler()

	// With correct token but missing url — should get 400, not 401
	req := httptest.NewRequest(http.MethodPost, "/api/download", strings.NewReader(`{"chat_id":123}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-secret-token")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "url")
}

func TestMissingURL(t *testing.T) {
	svc := newTestService(t)
	handler := svc.Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/download", strings.NewReader(`{"chat_id":123}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-secret-token")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "url")
}

func TestMissingChatID(t *testing.T) {
	svc := newTestService(t)
	handler := svc.Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/download", strings.NewReader(`{"url":"https://example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-secret-token")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "chat_id")
}

func TestInvalidJSON(t *testing.T) {
	svc := newTestService(t)
	handler := svc.Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/download", strings.NewReader(`not json`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-secret-token")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid JSON")
}

func TestMethodNotAllowed(t *testing.T) {
	svc := newTestService(t)
	handler := svc.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/download", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestHealthEndpoint(t *testing.T) {
	svc := newTestService(t)
	handler := svc.Handler()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "OK", w.Body.String())
}

func TestChatRecipient(t *testing.T) {
	r := chatRecipient{chatID: -1001234567890}
	assert.Equal(t, "-1001234567890", r.Recipient())
}

func TestChatRecipientPositive(t *testing.T) {
	r := chatRecipient{chatID: 123456}
	assert.Equal(t, "123456", r.Recipient())
}

func TestWriteJSON(t *testing.T) {
	w := httptest.NewRecorder()
	flusher := w // httptest.ResponseRecorder implements http.Flusher

	evt := ProgressEvent{Status: "downloading", Percent: 45.2}
	writeJSON(w, flusher, evt)

	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	require.Len(t, lines, 1)

	var parsed ProgressEvent
	err := json.Unmarshal([]byte(lines[0]), &parsed)
	require.NoError(t, err)
	assert.Equal(t, "downloading", parsed.Status)
	assert.Equal(t, 45.2, parsed.Percent)
}

func TestWriteJSONResultEvent(t *testing.T) {
	w := httptest.NewRecorder()
	flusher := w

	evt := ResultEvent{Status: "done", OK: true, Title: "Test Video", MessageID: 789, FileSize: 123456}
	writeJSON(w, flusher, evt)

	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	require.Len(t, lines, 1)

	var parsed ResultEvent
	err := json.Unmarshal([]byte(lines[0]), &parsed)
	require.NoError(t, err)
	assert.Equal(t, "done", parsed.Status)
	assert.True(t, parsed.OK)
	assert.Equal(t, "Test Video", parsed.Title)
	assert.Equal(t, 789, parsed.MessageID)
	assert.Equal(t, int64(123456), parsed.FileSize)
}

func TestInvalidURLScheme(t *testing.T) {
	svc := newTestService(t)
	handler := svc.Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/download", strings.NewReader(`{"url":"file:///etc/passwd","chat_id":123}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-secret-token")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "http://")
}

func TestNewAPIService(t *testing.T) {
	eng := engine.NewEngine("", "")
	svc := NewAPIService(eng, nil, "my-token")
	assert.NotNil(t, svc)
	assert.Equal(t, "my-token", svc.token)
	assert.NotNil(t, svc.processor)
	assert.NotNil(t, svc.dedup)
	assert.Equal(t, 2*downloader.DefaultTimeout, apiEngineTimeout)
	assert.Equal(t, apiEngineTimeout, svc.engineTimeout)
	assert.NotNil(t, svc.uploadResultFn)
}

func TestEnginePhaseTrackerConcurrentAccess(t *testing.T) {
	tracker := &enginePhaseTracker{}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			tracker.set("encoding")
		}()
		go func() {
			defer wg.Done()
			_ = tracker.current()
		}()
	}
	wg.Wait()

	tracker.set("splitting")
	assert.Equal(t, "splitting", tracker.current())
}

func TestEngineTerminalErrorAPIDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadlineCause(context.Background(), time.Now().Add(-time.Second), errAPIEngineDeadline)
	defer cancel()
	<-ctx.Done()

	err := engineTerminalError(ctx, errors.New("signal: killed"), "splitting", apiEngineTimeout)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "API engine deadline exceeded")
	assert.Contains(t, err.Error(), "splitting")
	assert.Contains(t, err.Error(), apiEngineTimeout.String())
}

func TestEngineTerminalErrorCallerCancellation(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, cancelTimeout := context.WithTimeoutCause(parent, apiEngineTimeout, errAPIEngineDeadline)
	cancelParent()
	defer cancelTimeout()
	<-ctx.Done()

	err := engineTerminalError(ctx, errors.New("signal: killed"), "encoding", apiEngineTimeout)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, err.Error(), "request canceled by client")
	assert.Contains(t, err.Error(), "encoding")
	assert.NotContains(t, err.Error(), "API engine deadline exceeded")
}

func TestEngineTerminalErrorPreservesOrdinaryError(t *testing.T) {
	want := errors.New("extractor failed")
	got := engineTerminalError(context.Background(), want, "downloading", apiEngineTimeout)
	assert.Same(t, want, got)
}

func TestAuthBearerFormat(t *testing.T) {
	svc := newTestService(t)
	handler := svc.Handler()

	// Token without "Bearer " prefix should fail
	req := httptest.NewRequest(http.MethodPost, "/api/download", strings.NewReader(`{"url":"https://example.com","chat_id":123}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "test-secret-token")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
