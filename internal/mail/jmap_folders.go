package mail

import (
	"context"
	"errors"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// JMAPFolders creates, renames and deletes a JMAP account's mailboxes.
type JMAPFolders struct {
	client  *jmap.Client
	session *jmap.Session
	account string
}

// OpenJMAPFolders signs in to a JMAP account to change its mailboxes.
func OpenJMAPFolders(ctx context.Context, cfg *models.AccountConfig, password string) (*JMAPFolders, error) {
	client, session, account, err := openJMAPMail(ctx, cfg, password)
	if err != nil {
		return nil, err
	}
	return &JMAPFolders{client: client, session: session, account: account}, nil
}

// Create makes a mailbox inside parentID, or at the top when it's "", and
// returns its id.
func (j *JMAPFolders) Create(ctx context.Context, name, parentID string) (string, error) {
	// A folder made inside a shared mailbox's folder is made in that
	// mailbox's JMAP account.
	account := j.account
	if parentID != "" {
		in, id, err := jmapAccountOf(j.session, parentID)
		if err != nil {
			return "", err
		}
		account, parentID = in, id
	}
	created, err := j.client.CreateMailbox(ctx, j.session, account, name, parentID)
	if err != nil {
		return "", err
	}
	return jmapRefIn(j.session, account, created), nil
}

func (j *JMAPFolders) Rename(ctx context.Context, id, name string) error {
	account, id, err := jmapAccountOf(j.session, id)
	if err != nil {
		return err
	}
	return j.client.RenameMailbox(ctx, j.session, account, id, name)
}

// Move puts a mailbox inside parentID, or at the top when it's "".
func (j *JMAPFolders) Move(ctx context.Context, id, parentID, name string) error {
	account, id, err := jmapAccountOf(j.session, id)
	if err != nil {
		return err
	}
	if parentID != "" {
		in, parent, err := jmapAccountOf(j.session, parentID)
		if err != nil {
			return err
		}
		if in != account {
			return errors.New("a folder cannot move into another mailbox's folders")
		}
		parentID = parent
	}
	return j.client.MoveMailbox(ctx, j.session, account, id, parentID, name)
}

// Delete deletes mailboxes and their emails, children listed first.
func (j *JMAPFolders) Delete(ctx context.Context, ids []string) error {
	byAccount := map[string][]string{}
	var order []string
	for _, ref := range ids {
		account, id, err := jmapAccountOf(j.session, ref)
		if err != nil {
			return err
		}
		if _, ok := byAccount[account]; !ok {
			order = append(order, account)
		}
		byAccount[account] = append(byAccount[account], id)
	}
	for _, account := range order {
		if err := j.client.DestroyMailboxes(ctx, j.session, account, byAccount[account]); err != nil {
			return err
		}
	}
	return nil
}
