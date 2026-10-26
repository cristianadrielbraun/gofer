package mail

import (
	"context"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
)

// JMAPFolders creates, renames and deletes a JMAP account's mailboxes.
type JMAPFolders struct {
	client  *jmap.Client
	session *jmap.Session
	account string
}

// OpenJMAPFolders signs in to a JMAP account to change its mailboxes.
func OpenJMAPFolders(ctx context.Context, cfg *models.AccountConfig, password string) (*JMAPFolders, error) {
	if !providers.JMAPEnabled() {
		return nil, providers.ErrJMAPDisabled
	}
	client, session, account, err := openJMAPMail(ctx, cfg, password)
	if err != nil {
		return nil, err
	}
	return &JMAPFolders{client: client, session: session, account: account}, nil
}

// Create makes a mailbox inside parentID, or at the top when it's "", and
// returns its id.
func (j *JMAPFolders) Create(ctx context.Context, name, parentID string) (string, error) {
	return j.client.CreateMailbox(ctx, j.session, j.account, name, parentID)
}

func (j *JMAPFolders) Rename(ctx context.Context, id, name string) error {
	return j.client.RenameMailbox(ctx, j.session, j.account, id, name)
}

// Move puts a mailbox inside parentID, or at the top when it's "".
func (j *JMAPFolders) Move(ctx context.Context, id, parentID, name string) error {
	return j.client.MoveMailbox(ctx, j.session, j.account, id, parentID, name)
}

// Delete deletes mailboxes and their emails, children listed first.
func (j *JMAPFolders) Delete(ctx context.Context, ids []string) error {
	return j.client.DestroyMailboxes(ctx, j.session, j.account, ids)
}
