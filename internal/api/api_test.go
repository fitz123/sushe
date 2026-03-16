package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fitz123/sushe/internal/engine"
	"github.com/fitz123/sushe/internal/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	logger.Init("error")
}

func newTestService() *APIService {
	// Create API service with nil bot (tests that don't upload won't need it)
	eng := engine.NewEngine()
	return NewAPIService(eng, nil, "test-secret-token")
}

func TestAuthMissingToken(t *testing.T) {
	svc := newTestService()
	handler := svc.Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/download", strings.NewReader(`{"url":"https://example.com","chat_id":123}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "unauthorized")
}

func TestAuthWrongToken(t *testing.T) {
	svc := newTestService()
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
	svc := newTestService()
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
	svc := newTestService()
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
	svc := newTestService()
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
	svc := newTestService()
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
	svc := newTestService()
	handler := svc.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/download", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestHealthEndpoint(t *testing.T) {
	svc := newTestService()
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
	svc := newTestService()
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
	eng := engine.NewEngine()
	svc := NewAPIService(eng, nil, "my-token")
	assert.NotNil(t, svc)
	assert.Equal(t, "my-token", svc.token)
	assert.NotNil(t, svc.engine)
}

func TestAuthBearerFormat(t *testing.T) {
	svc := newTestService()
	handler := svc.Handler()

	// Token without "Bearer " prefix should fail
	req := httptest.NewRequest(http.MethodPost, "/api/download", strings.NewReader(`{"url":"https://example.com","chat_id":123}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "test-secret-token")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
