package handler

import (
	"bytes"
	"net/mail"
	"slices"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
)

func TestQueuedMessageBccByTransport(t *testing.T) {
	for _, provider := range []string{"imap", "jmap"} {
		t.Run(provider, func(t *testing.T) {
			h, db := newAccountOwnershipTestHandler(t)
			if _, err := db.Write().ExecContext(t.Context(), `UPDATE accounts SET provider=?, jmap_session_url='https://mail.example.test/.well-known/jmap' WHERE id='victim-account'`, provider); err != nil {
				t.Fatal(err)
			}
			msg := &message.OutgoingMessage{
				FromEmail: "owner@example.com", Subject: "Bcc in Sent", TextBody: "Private copy",
				To:  []*mail.Address{{Address: "to@example.com"}},
				CC:  []*mail.Address{{Address: "cc@example.com"}},
				Bcc: []*mail.Address{{Name: "Hidden recipient", Address: "hidden@example.com"}, {Address: "other-hidden@example.com"}},
			}
			queued, err := h.queueOutgoingMessage(t.Context(), "victim-account", 0, "", msg, time.Now(), false)
			if err != nil {
				t.Fatal(err)
			}
			// Read the persisted MIME, as delivery after a restart would.
			queued, err = db.GetOutgoingSend(t.Context(), queued.ID)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := mail.ReadMessage(bytes.NewReader(queued.MIMEData))
			if err != nil {
				t.Fatal(err)
			}
			if provider == "jmap" {
				bcc, err := stored.Header.AddressList("Bcc")
				if err != nil || len(bcc) != 2 || bcc[0].Address != "hidden@example.com" || bcc[0].Name != "Hidden recipient" || bcc[1].Address != "other-hidden@example.com" {
					t.Fatalf("queued JMAP Bcc = %q, %v", stored.Header.Get("Bcc"), err)
				}
			} else if stored.Header.Get("Bcc") != "" {
				t.Fatalf("SMTP MIME exposes Bcc: %q", stored.Header.Get("Bcc"))
			}
			if !slices.Equal(queued.EnvelopeRecipients, []string{"to@example.com", "cc@example.com", "hidden@example.com", "other-hidden@example.com"}) {
				t.Fatalf("envelope recipients = %v", queued.EnvelopeRecipients)
			}
		})
	}
}
