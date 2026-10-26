package imap

import (
	"context"
	"errors"
	"fmt"

	goimap "github.com/emersion/go-imap/v2"
)

// Delimiter returns the character the server separates folder levels with,
// or 0 when it has only one level.
func (c *Client) Delimiter(ctx context.Context) (rune, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if c.closed {
		return 0, fmt.Errorf("client is closed")
	}
	data, err := c.client.List("", "", nil).Collect()
	if err != nil {
		return 0, fmt.Errorf("list delimiter: %w", err)
	}
	for _, mailbox := range data {
		if mailbox.Delim != 0 {
			return mailbox.Delim, nil
		}
	}
	return 0, nil
}

// CreateMailbox creates a folder. A folder that already exists is left as
// it is.
func (c *Client) CreateMailbox(ctx context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.closed {
		return fmt.Errorf("client is closed")
	}
	if name == "" {
		return fmt.Errorf("mailbox is required")
	}
	err := c.client.Create(name, nil).Wait()
	if err == nil {
		return nil
	}
	var statusErr *goimap.Error
	if !errors.As(err, &statusErr) {
		return fmt.Errorf("create %s: %w", name, err)
	}
	// Not every server says ALREADYEXISTS, so look for the folder.
	existing, listErr := c.client.List("", name, nil).Collect()
	if listErr == nil && len(existing) > 0 {
		return nil
	}
	return fmt.Errorf("create %s: %w", name, err)
}

// roleMailboxes are the folders CreateRoleMailbox makes, by role, with the
// names detectFolderRole knows them by when the server can't mark them.
var roleMailboxes = map[string]struct {
	name string
	attr goimap.MailboxAttr
}{
	"sent":    {"Sent", goimap.MailboxAttrSent},
	"drafts":  {"Drafts", goimap.MailboxAttrDrafts},
	"trash":   {"Trash", goimap.MailboxAttrTrash},
	"junk":    {"Spam", goimap.MailboxAttrJunk},
	"archive": {"Archive", goimap.MailboxAttrArchive},
}

// CreateRoleMailbox creates a folder for a role, such as "Trash", under the
// personal namespace prefix, and returns its name. Where the server can, the
// folder is marked with the role's special-use attribute; a server that
// refuses the mark, often because another folder has it, gets the folder
// without one. Its name gives it the role either way. A folder of that name
// that already exists is left as it is.
func (c *Client) CreateRoleMailbox(ctx context.Context, role string) (string, error) {
	mailbox, ok := roleMailboxes[role]
	if !ok {
		return "", fmt.Errorf("no folder is made for the %q role", role)
	}
	c.mu.Lock()
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return "", err
	}
	if c.closed {
		c.mu.Unlock()
		return "", fmt.Errorf("client is closed")
	}
	name := mailbox.name
	for _, prefix := range c.personalNamespaces() {
		if prefix != "" {
			name = prefix + mailbox.name
			break
		}
	}
	marked := false
	if caps := c.client.Caps(); caps != nil && caps.Has(goimap.CapCreateSpecialUse) {
		marked = c.client.Create(name, &goimap.CreateOptions{SpecialUse: []goimap.MailboxAttr{mailbox.attr}}).Wait() == nil
	}
	c.mu.Unlock()
	if marked {
		return name, nil
	}
	return name, c.CreateMailbox(ctx, name)
}
