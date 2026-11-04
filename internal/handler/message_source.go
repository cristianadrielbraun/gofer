package handler

import (
	"bytes"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"log"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// The source dialog shows at most this much of a message; View raw shows
// the rest. A browser struggles with a dialog holding megabytes of text.
const messageSourceDialogMaxBytes = 1 << 20

// readMessageSource returns a message as it was received, or writes the
// error and returns false.
func (h *Handler) readMessageSource(w http.ResponseWriter, r *http.Request) (int64, []byte, bool) {
	ctx := r.Context()
	msgID, _, err := h.getMessageInfo(ctx, r.PathValue("id"))
	if err != nil {
		writeMessageTargetError(w, r, err)
		return 0, nil, false
	}
	data, err := h.fetchRawMessage(ctx, msgID)
	if err != nil || data == nil {
		if err == nil || !writeMessageTooLarge(w, r, err) {
			log.Printf("message source %d: %v", msgID, err)
			http.Error(w, i18n.T(r.Context(), "server.error.this_message_couldn_t_be_downloaded_from"), http.StatusServiceUnavailable)
		}
		return 0, nil, false
	}
	return msgID, data, true
}

// readUserMessageSource is readMessageSource for per-user storage. A message
// downloaded to be shown is not kept.
func (h *Handler) readUserMessageSource(w http.ResponseWriter, r *http.Request) (int64, []byte, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_message_id"), http.StatusBadRequest)
		return 0, nil, false
	}
	data, err := h.userIMAP.ReadRawMessageForExport(r.Context(), h.userID(r.Context()), id, true)
	if err != nil {
		userAccountError(w, r, err)
		return 0, nil, false
	}
	return id, data, true
}

func (h *Handler) handleMessageSource(w http.ResponseWriter, r *http.Request) {
	if id, data, ok := h.readMessageSource(w, r); ok {
		writeMessageSource(w, r, id, data)
	}
}

func (h *Handler) handleUserMessageSource(w http.ResponseWriter, r *http.Request) {
	if id, data, ok := h.readUserMessageSource(w, r); ok {
		writeMessageSource(w, r, id, data)
	}
}

func (h *Handler) handleMessageSourceDialog(w http.ResponseWriter, r *http.Request) {
	if id, data, ok := h.readMessageSource(w, r); ok {
		renderMessageSourceDialog(w, r, id, data)
	}
}

func (h *Handler) handleUserMessageSourceDialog(w http.ResponseWriter, r *http.Request) {
	if id, data, ok := h.readUserMessageSource(w, r); ok {
		renderMessageSourceDialog(w, r, id, data)
	}
}

// writeMessageSource shows a message as it was received, headers and all, as
// plain text. With download set it is saved as an .eml file instead. The text
// is the sender's, so it is never sniffed and runs nothing.
func writeMessageSource(w http.ResponseWriter, r *http.Request, msgID int64, data []byte) {
	header := w.Header()
	header.Set("Cache-Control", "private, no-store")
	header.Set("Content-Security-Policy", mailFileContentSecurityPolicy)
	header.Set("X-Content-Type-Options", "nosniff")
	if r.URL.Query().Get("download") != "" {
		header.Set("Content-Type", "message/rfc822")
		header.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "message-" + strconv.FormatInt(msgID, 10) + ".eml"}))
	} else {
		header.Set("Content-Type", "text/plain; charset=utf-8")
		header.Set("Content-Disposition", "inline")
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}

func renderMessageSourceDialog(w http.ResponseWriter, r *http.Request, msgID int64, data []byte) {
	report := message.AnalyzeSource(data)
	shown, truncated := data, false
	if len(shown) > messageSourceDialogMaxBytes {
		shown, truncated = shown[:messageSourceDialogMaxBytes], true
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	component := views.MailSourceDialog(strconv.FormatInt(msgID, 10), report, strings.ToValidUTF8(string(shown), string(utf8.RuneError)), truncated)
	if err := component.Render(r.Context(), w); err != nil {
		log.Printf("render message source %d: %v", msgID, err)
	}
}
