package imap

import (
	"context"
	"fmt"
	"slices"
	"strings"

	goimap "github.com/emersion/go-imap/v2"
)

// NewMailboxName returns the full name of a folder called name made under
// parent, or at the top under the personal namespace prefix when parent is
// empty. A name holding the server's delimiter would make more than one
// level, so it's refused.
func (c *Client) NewMailboxName(ctx context.Context, parent, name string) (string, error) {
	delimiter, err := c.Delimiter(ctx)
	if err != nil {
		return "", err
	}
	if delimiter != 0 && strings.ContainsRune(name, delimiter) {
		return "", fmt.Errorf("a folder name can't contain %q", string(delimiter))
	}
	if parent != "" {
		if delimiter == 0 {
			return "", fmt.Errorf("this server doesn't have subfolders")
		}
		return parent + string(delimiter) + name, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return "", fmt.Errorf("client is closed")
	}
	for _, prefix := range c.personalNamespaces() {
		if prefix != "" {
			return prefix + name, nil
		}
	}
	return name, nil
}

// RenamedMailboxName returns the full name a folder gets when renamed to
// name, keeping it under the same parent.
func (c *Client) RenamedMailboxName(ctx context.Context, mailbox, name string) (string, error) {
	delimiter, err := c.Delimiter(ctx)
	if err != nil {
		return "", err
	}
	if delimiter == 0 {
		return name, nil
	}
	if strings.ContainsRune(name, delimiter) {
		return "", fmt.Errorf("a folder name can't contain %q", string(delimiter))
	}
	if i := strings.LastIndex(mailbox, string(delimiter)); i >= 0 {
		return mailbox[:i+len(string(delimiter))] + name, nil
	}
	return name, nil
}

// CreateSubscribedMailbox creates a folder and subscribes to it, so other
// mail apps show it too. A refused subscription leaves the folder made.
func (c *Client) CreateSubscribedMailbox(ctx context.Context, name string) error {
	if err := c.CreateMailbox(ctx, name); err != nil {
		return err
	}
	c.subscribe(name, true)
	return nil
}

// RenameMailbox renames a folder, and with it the folders under it. Servers
// should move those along; any a server leaves behind are renamed one by
// one.
func (c *Client) RenameMailbox(ctx context.Context, mailbox, newName string) error {
	delimiter, err := c.Delimiter(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.closed {
		return fmt.Errorf("client is closed")
	}
	if mailbox == "" || newName == "" {
		return fmt.Errorf("mailbox is required")
	}
	if err := c.client.Rename(mailbox, newName, nil).Wait(); err != nil {
		return fmt.Errorf("rename %s: %w", mailbox, err)
	}
	c.subscribeLocked(mailbox, false)
	c.subscribeLocked(newName, true)
	if delimiter == 0 {
		return nil
	}
	oldPrefix := mailbox + string(delimiter)
	left, err := c.client.List("", oldPrefix+"*", nil).Collect()
	if err != nil {
		return fmt.Errorf("list folders under %s: %w", mailbox, err)
	}
	// Parents first, so each child's new parent is there.
	slices.SortFunc(left, func(a, b *goimap.ListData) int { return len(a.Mailbox) - len(b.Mailbox) })
	for _, child := range left {
		if !strings.HasPrefix(child.Mailbox, oldPrefix) {
			continue
		}
		moved := newName + string(delimiter) + child.Mailbox[len(oldPrefix):]
		if err := c.client.Rename(child.Mailbox, moved, nil).Wait(); err != nil {
			return fmt.Errorf("rename %s: %w", child.Mailbox, err)
		}
		c.subscribeLocked(child.Mailbox, false)
		c.subscribeLocked(moved, true)
	}
	return nil
}

// DeleteMailbox deletes a folder and the messages in it. Folders under it
// must be deleted first: servers keep a folder that has some.
func (c *Client) DeleteMailbox(ctx context.Context, mailbox string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.closed {
		return fmt.Errorf("client is closed")
	}
	if mailbox == "" {
		return fmt.Errorf("mailbox is required")
	}
	if err := c.client.Delete(mailbox).Wait(); err != nil {
		return fmt.Errorf("delete %s: %w", mailbox, err)
	}
	c.subscribeLocked(mailbox, false)
	return nil
}

func (c *Client) subscribe(mailbox string, on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.subscribeLocked(mailbox, on)
}

// subscribeLocked changes a subscription, ignoring a server that refuses:
// the subscription only tells other mail apps what to show.
func (c *Client) subscribeLocked(mailbox string, on bool) {
	if on {
		_ = c.client.Subscribe(mailbox).Wait()
		return
	}
	_ = c.client.Unsubscribe(mailbox).Wait()
}
