package mail

import (
	"context"
	"errors"
	"io"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// FetchJMAPRawMessage downloads one email of a JMAP account as its original
// RFC 5322 message, for the same parser IMAP mail goes through. The email's
// size is checked against the download limit before anything is downloaded,
// and the download itself stops at the limit.
func FetchJMAPRawMessage(ctx context.Context, cfg *models.AccountConfig, password, emailID string) ([]byte, error) {
	if emailID == "" {
		return nil, errors.New("JMAP email identity is unavailable")
	}
	client, session, err := openJMAPSession(ctx, cfg, password)
	if err != nil {
		return nil, err
	}
	account, emailID, err := jmapAccountOf(session, emailID)
	if err != nil {
		return nil, err
	}
	got, err := client.GetEmails(ctx, session, account, []string{emailID}, []string{"id", "blobId", "size"})
	if err != nil {
		return nil, err
	}
	if len(got.List) == 0 || got.List[0].BlobID == "" {
		return nil, errors.New("the JMAP server no longer has this email")
	}
	email := got.List[0]
	limit := message.DownloadLimit(ctx, message.MaxDownloadBytes)
	if email.Size > limit {
		return nil, &message.TooLargeError{Size: email.Size, Limit: limit}
	}
	body, err := client.DownloadBlob(ctx, session, account, email.BlobID, "", jmap.ContentTypeMessage)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, &message.TooLargeError{Limit: limit}
	}
	if len(raw) == 0 {
		return nil, errors.New("the JMAP server returned an empty message")
	}
	return raw, nil
}
