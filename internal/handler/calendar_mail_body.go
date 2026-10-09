package handler

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Repair the old parser's cached calendar-as-text result when its owned message
// is opened. Use the saved MIME original, never guess from plain-text ICS alone.
func (h *Handler) repairCachedCalendarBody(ctx context.Context, emailID, accountID, text string) bool {
	if !strings.HasPrefix(strings.TrimSpace(text), "BEGIN:VCALENDAR") || h.blobStore == nil {
		return false
	}
	id, err := strconv.ParseInt(emailID, 10, 64)
	if err != nil || id <= 0 {
		return false
	}
	h.bodyFetchMu.Lock()
	if done, ok := h.bodyFetches[id]; ok {
		h.bodyFetchMu.Unlock()
		select {
		case <-done:
			return true
		case <-ctx.Done():
			return false
		}
	}
	if h.bodyFetches == nil {
		h.bodyFetches = make(map[int64]chan struct{})
	}
	done := make(chan struct{})
	h.bodyFetches[id] = done
	h.bodyFetchMu.Unlock()
	defer func() {
		h.bodyFetchMu.Lock()
		delete(h.bodyFetches, id)
		close(done)
		h.bodyFetchMu.Unlock()
	}()

	// A second concurrent opener may hold the old model. Recheck the cache and
	// refuse to append duplicate attachments or touch provider-managed rows.
	var rawPath, textPath string
	err = h.db.Read().QueryRowContext(ctx, `
		SELECT COALESCE(m.raw_path, ''), COALESCE(m.body_text_path, '')
		FROM messages m
		WHERE m.id = ? AND m.account_id = ?
		AND COALESCE(m.body_html_path, '') = ''
		AND NOT EXISTS (SELECT 1 FROM attachments a WHERE a.message_id = m.id)`, id, accountID).Scan(&rawPath, &textPath)
	if err != nil || rawPath == "" || textPath == "" {
		return false
	}
	file, err := os.Open(rawPath)
	if err != nil {
		return false
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, message.CalendarIncomingMaxSize+1))
	if err != nil || len(raw) > message.CalendarIncomingMaxSize {
		return false
	}
	parsed, err := message.ParseMessage(ctx, bytes.NewReader(raw), h.blobStore, accountID, id)
	if err != nil || parsed == nil || parsed.ParseError != nil {
		return false
	}
	for _, attachment := range parsed.Attachments {
		if attachment.ContentType == "text/calendar" {
			h.storeParsedBody(ctx, parsed, id, accountID)
			return true
		}
	}
	return false
}

// repairUserCalendarBodies is the per-user form of repairCachedCalendarBody.
// The request-local view handler cannot write bodies, so before the view reads
// the store, repair the opened message and its thread from saved MIME.
func (h *Handler) repairUserCalendarBodies(ctx context.Context, owner, emailID string) {
	if h.userStorage == nil || h.userIMAP == nil {
		return
	}
	id, err := strconv.ParseInt(emailID, 10, 64)
	if err != nil || id <= 0 {
		return
	}
	type candidate struct {
		id   int64
		text string
	}
	var candidates []candidate
	err = h.withUserDB(ctx, owner, func(db *storage.DB) error {
		rows, err := db.Read().QueryContext(ctx, `SELECT m.id,m.body_text_path FROM messages m JOIN messages opened ON opened.id=?
 WHERE (m.id=opened.id OR (m.account_id=opened.account_id AND COALESCE(opened.thread_id,'')<>'' AND m.thread_id=opened.thread_id))
 AND COALESCE(m.body_html_path,'')='' AND COALESCE(m.body_text_path,'')<>'' AND COALESCE(m.raw_path,'')<>''
 AND NOT EXISTS(SELECT 1 FROM attachments a WHERE a.message_id=m.id) LIMIT 50`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c candidate
			if err := rows.Scan(&c.id, &c.text); err != nil {
				return err
			}
			candidates = append(candidates, c)
		}
		return rows.Err()
	})
	if err != nil {
		return
	}
	for _, c := range candidates {
		if !cachedCalendarText(c.text) {
			continue
		}
		if _, err := h.userIMAP.RepairCalendarBody(ctx, owner, c.id); err != nil {
			log.Printf("repair cached calendar body: %v", err)
		}
	}
}

// cachedCalendarText reports whether a cached text body starts with raw iCalendar.
func cachedCalendarText(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	start := make([]byte, 512)
	n, _ := io.ReadFull(file, start)
	return strings.HasPrefix(strings.TrimSpace(string(start[:n])), "BEGIN:VCALENDAR")
}
