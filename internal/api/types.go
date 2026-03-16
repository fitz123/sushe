package api

import "fmt"

// DownloadRequest is the JSON body for POST /api/download.
type DownloadRequest struct {
	URL      string `json:"url"`
	ChatID   int64  `json:"chat_id"`
	ThreadID int    `json:"thread_id"`
}

// ProgressEvent is a single NDJSON line streamed during processing.
type ProgressEvent struct {
	Status  string  `json:"status"`
	Percent float64 `json:"percent,omitempty"`
	Part    int     `json:"part,omitempty"`
	Total   int     `json:"total,omitempty"`
	Codec   string  `json:"codec,omitempty"`
	Video   int     `json:"video,omitempty"`
	URL     string  `json:"url,omitempty"`
}

// ResultEvent is the final NDJSON line indicating success or failure.
type ResultEvent struct {
	Status    string `json:"status"`
	OK        bool   `json:"ok"`
	Title     string `json:"title,omitempty"`
	MessageID int    `json:"message_id,omitempty"`
	FileSize  int64  `json:"file_size,omitempty"`
	Error     string `json:"error,omitempty"`
}

// chatRecipient implements tele.Recipient for sending to arbitrary chat IDs.
type chatRecipient struct {
	chatID int64
}

func (r chatRecipient) Recipient() string {
	return fmt.Sprintf("%d", r.chatID)
}
