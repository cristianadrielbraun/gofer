package mail

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// JMAPImport adds messages to a JMAP account's mailboxes, for imports and
// for mail moved in from another account. It signs in on first use and keeps
// the session until a request fails.
type JMAPImport struct {
	config   *models.AccountConfig
	password string

	client    *jmap.Client
	session   *jmap.Session
	account   string
	mailboxes []jmap.Mailbox
	// unanswered is the last import sent that got no answer; the server
	// may have stored it.
	unanswered *jmapUnansweredImport
}

type jmapUnansweredImport struct {
	mailboxID, messageID string
	receivedAt, sent     time.Time
}

// jmapClockSkew is how far the server's clock may be from Gofer's when
// looking for a message the server dated itself.
const jmapClockSkew = 10 * time.Minute

func NewJMAPImport(cfg *models.AccountConfig, password string) *JMAPImport {
	return &JMAPImport{config: cfg, password: password}
}

func (j *JMAPImport) open(ctx context.Context) error {
	if j.session != nil {
		return nil
	}
	client, session, account, err := openJMAPMail(ctx, j.config, j.password)
	if err != nil {
		return err
	}
	j.client, j.session, j.account, j.mailboxes = client, session, account, nil
	return nil
}

// drop forgets the session after a failed request, so the next one signs in
// again. A refusal leaves it: the server answered.
func (j *JMAPImport) drop(err error) {
	var refused *jmap.ImportError
	if err != nil && !errors.As(err, &refused) {
		j.session = nil
	}
}

// EnsureMailbox returns the id of the mailbox at path, its names separated
// by "/", creating the mailboxes along it that don't exist yet.
func (j *JMAPImport) EnsureMailbox(ctx context.Context, path string) (id string, err error) {
	defer func() { j.drop(err) }()
	if err := j.open(ctx); err != nil {
		return "", err
	}
	if j.mailboxes == nil {
		mailboxes, err := j.client.GetMailboxes(ctx, j.session, j.account, nil)
		if err != nil {
			return "", err
		}
		j.mailboxes = mailboxes.List
	}
	parent := ""
	for _, name := range strings.Split(strings.Trim(path, "/"), "/") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		found := ""
		for _, mailbox := range j.mailboxes {
			if mailbox.ParentID != parent {
				continue
			}
			// "INBOX" names the inbox whatever the server calls it.
			if mailbox.Name == name || (parent == "" && strings.EqualFold(name, "inbox") && mailbox.Role == "inbox") {
				found = mailbox.ID
				break
			}
		}
		if found == "" {
			found, err = j.client.CreateMailbox(ctx, j.session, j.account, name, parent)
			if err != nil {
				return "", err
			}
			j.mailboxes = append(j.mailboxes, jmap.Mailbox{ID: found, Name: name, ParentID: parent})
		}
		parent = found
	}
	if parent == "" {
		return "", errors.New("the folder needs a name")
	}
	return parent, nil
}

// Import uploads raw and stores it in the mailbox with the keywords given,
// received at receivedAt when it isn't zero. A message the server already
// has counts as imported. messageID, without angle brackets, lets a retry
// after an import that got no answer find the copy the server may have
// stored, rather than storing it twice.
func (j *JMAPImport) Import(ctx context.Context, mailboxID string, raw []byte, messageID string, keywords []string, receivedAt time.Time) (err error) {
	defer func() { j.drop(err) }()
	if err := j.open(ctx); err != nil {
		return err
	}
	if j.storedBefore(ctx, mailboxID, messageID, receivedAt) {
		return nil
	}
	blob, err := j.client.UploadBlob(ctx, j.session, j.account, jmap.ContentTypeMessage, raw)
	if err != nil {
		if errors.Is(err, jmap.ErrUploadTooLarge) {
			// The session is fine; the message is what's wrong.
			return &jmap.ImportError{Step: "upload", SetError: jmap.SetError{Type: "tooLarge", Description: err.Error()}}
		}
		return err
	}
	sent := time.Now()
	_, err = j.client.ImportEmail(ctx, j.session, j.account, blob, mailboxID, keywords, receivedAt)
	if messageID != "" && jmapImportUnanswered(err) {
		j.unanswered = &jmapUnansweredImport{mailboxID: mailboxID, messageID: messageID, receivedAt: receivedAt, sent: sent}
	}
	return err
}

// storedBefore reports whether this message is the one whose last import got
// no answer, and the server has it after all. When the server can't be asked,
// the import goes ahead: a rare duplicate is better than a lost message.
func (j *JMAPImport) storedBefore(ctx context.Context, mailboxID, messageID string, receivedAt time.Time) bool {
	last := j.unanswered
	j.unanswered = nil
	if last == nil || messageID == "" || last.mailboxID != mailboxID || last.messageID != messageID || !last.receivedAt.Equal(receivedAt) {
		return false
	}
	// The copy has the date it was given, or, on a server that dates
	// messages itself, about the time it was sent.
	windows := []jmap.Window{{From: last.sent.Add(-jmapClockSkew), To: time.Now().Add(jmapClockSkew)}}
	if !receivedAt.IsZero() {
		windows = append(windows, jmap.Window{From: receivedAt.Add(-time.Second), To: receivedAt.Add(time.Second)})
	}
	found, err := j.client.FindEmails(ctx, j.session, j.account, mailboxID, messageID, windows)
	return err == nil && len(found) > 0
}

// jmapImportUnanswered reports whether an import failed without the server
// saying what it did, so it may have stored the message.
func jmapImportUnanswered(err error) bool {
	if err == nil {
		return false
	}
	var refused *jmap.ImportError
	var method *jmap.MethodError
	var status *jmap.HTTPError
	switch {
	case errors.As(err, &refused), errors.As(err, &method):
		return false
	case errors.As(err, &status):
		// A gateway can time out after the server stored the message.
		return status.StatusCode >= 500
	}
	return true
}
