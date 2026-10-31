package handler

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// emailBodyPrinting reports whether a message body document is asked for to
// print: with the message's details on top, opening the print dialog itself.
func emailBodyPrinting(r *http.Request) bool {
	return r.URL.Query().Get("print") == "1"
}

// emailPrintDocumentPolicy is the body document's policy, also letting it
// open the print dialog. Only the reader's own scripts run in it, so the
// sender's HTML still can't open anything.
func emailPrintDocumentPolicy(nonce string) string {
	return strings.Replace(emailBodyDocumentPolicy(nonce), "sandbox allow-scripts", "sandbox allow-scripts allow-modals", 1)
}

// emailPrintScript opens the print dialog once the document and its images
// have loaded. Printing shows every page, whatever the reader cut off.
const emailPrintScript = `<style>@media print{html,body{overflow:visible !important;height:auto !important}}</style>` +
	`<script>(function(){function p(){setTimeout(function(){window.print()},50)}if(document.readyState==='complete'){p()}else{window.addEventListener('load',p)}})();</script>`

// printBodyDocument makes doc, a message body document, the one that prints:
// the message's details first, and the print dialog once it has loaded.
func (h *Handler) printBodyDocument(ctx context.Context, w http.ResponseWriter, userID, emailID string, doc []byte, nonce string) []byte {
	w.Header().Set("Content-Security-Policy", emailPrintDocumentPolicy(nonce))
	var email *models.Email
	err := h.withUserDB(ctx, userID, func(db *storage.DB) error {
		ctx := storage.WithTimezone(ctx, db.GetUISettings(ctx, userID)["timezone"])
		var err error
		if email, err = db.GetEmailByIDForUser(ctx, emailID, userID); err == nil && email != nil {
			var header bytes.Buffer
			if err = views.EmailPrintHeader(email).Render(ctx, &header); err == nil {
				doc = insertAfterBodyOpen(doc, header.Bytes())
			}
		}
		return err
	})
	if err != nil {
		log.Printf("print message %s: header: %v", emailID, err)
	}
	return append(doc, scriptWithNonce([]byte(emailPrintScript), nonce)...)
}

// insertAfterBodyOpen puts content at the start of the document's body, or
// at its start when it has no body tag.
func insertAfterBodyOpen(doc, content []byte) []byte {
	lower := bytes.ToLower(doc)
	start := bytes.Index(lower, []byte("<body"))
	if start != -1 {
		if end := bytes.IndexByte(doc[start:], '>'); end != -1 {
			at := start + end + 1
			return append(append(append([]byte{}, doc[:at]...), content...), doc[at:]...)
		}
	}
	return append(append([]byte{}, content...), doc...)
}
