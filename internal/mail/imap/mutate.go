package imap

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
)

var ErrMutationUIDValidityChanged = errors.New("IMAP UIDVALIDITY changed; waiting for folder resync")

func (c *Client) StoreFlags(ctx context.Context, folderRemoteName string, uid uint32, op imap.StoreFlagsOp, flags []imap.Flag) error {
	return c.StoreFlagsBatch(ctx, folderRemoteName, []uint32{uid}, op, flags)
}

func (c *Client) StoreFlagsBatch(ctx context.Context, folderRemoteName string, uids []uint32, op imap.StoreFlagsOp, flags []imap.Flag) error {
	return c.storeFlagsBatch(ctx, folderRemoteName, uids, op, flags, 0)
}

func (c *Client) StoreFlagsIfUIDValidity(ctx context.Context, folder string, uid uint32, op imap.StoreFlagsOp, flags []imap.Flag, validity uint32) error {
	if validity == 0 {
		return fmt.Errorf("IMAP UIDVALIDITY is unavailable")
	}
	return c.storeFlagsBatch(ctx, folder, []uint32{uid}, op, flags, validity)
}

func (c *Client) storeFlagsBatch(ctx context.Context, folderRemoteName string, uids []uint32, op imap.StoreFlagsOp, flags []imap.Flag, validity uint32) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(uids) == 0 {
		return nil
	}
	if c.closed {
		return fmt.Errorf("client is closed")
	}

	selected, err := c.client.Select(folderRemoteName, nil).Wait()
	if err != nil {
		return fmt.Errorf("select %s: %w", folderRemoteName, err)
	}
	defer c.client.Unselect()
	if validity > 0 && validity != uint32(selected.UIDValidity) {
		return ErrMutationUIDValidityChanged
	}

	var uidSet imap.UIDSet
	for _, uid := range uids {
		uidSet.AddNum(imap.UID(uid))
	}

	storeCmd := c.client.Store(uidSet, &imap.StoreFlags{
		Op:     op,
		Silent: true,
		Flags:  flags,
	}, nil)

	return storeCmd.Close()
}

func (c *Client) StoreKeyword(ctx context.Context, folderRemoteName string, uid uint32, op imap.StoreFlagsOp, keyword string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return fmt.Errorf("keyword is required")
	}
	if c.closed {
		return fmt.Errorf("client is closed")
	}

	selectData, err := c.client.Select(folderRemoteName, nil).Wait()
	if err != nil {
		return fmt.Errorf("select %s: %w", folderRemoteName, err)
	}
	defer c.client.Unselect()

	flag := imap.Flag(keyword)
	if op == imap.StoreFlagsAdd && !supportsPermanentKeyword(selectData.PermanentFlags, flag) {
		return fmt.Errorf("mailbox does not allow persistent keyword %q", keyword)
	}

	var uidSet imap.UIDSet
	uidSet.AddNum(imap.UID(uid))

	storeCmd := c.client.Store(uidSet, &imap.StoreFlags{
		Op:     op,
		Silent: true,
		Flags:  []imap.Flag{flag},
	}, nil)

	return storeCmd.Close()
}

func supportsPermanentKeyword(flags []imap.Flag, keyword imap.Flag) bool {
	for _, flag := range flags {
		if flag == "\\*" || strings.EqualFold(string(flag), string(keyword)) {
			return true
		}
	}
	return false
}

func (c *Client) MoveMessage(ctx context.Context, folderRemoteName string, uid uint32, destFolderRemoteName string) error {
	return c.MoveMessages(ctx, folderRemoteName, []uint32{uid}, destFolderRemoteName)
}

func (c *Client) MoveMessageWithDestUID(ctx context.Context, folderRemoteName string, uid uint32, destFolderRemoteName string) (uint32, error) {
	destUIDs, err := c.MoveMessagesWithDestUIDs(ctx, folderRemoteName, []uint32{uid}, destFolderRemoteName)
	if err != nil || len(destUIDs) == 0 {
		return 0, err
	}
	return destUIDs[0], nil
}

func (c *Client) MoveMessages(ctx context.Context, folderRemoteName string, uids []uint32, destFolderRemoteName string) error {
	_, err := c.MoveMessagesWithDestUIDs(ctx, folderRemoteName, uids, destFolderRemoteName)
	return err
}

func (c *Client) MoveMessagesWithDestUIDs(ctx context.Context, folderRemoteName string, uids []uint32, destFolderRemoteName string) ([]uint32, error) {
	return c.moveMessagesWithDestUIDs(ctx, folderRemoteName, uids, destFolderRemoteName, 0)
}

func (c *Client) MoveMessageIfUIDValidity(ctx context.Context, source string, uid uint32, destination string, validity uint32) (uint32, error) {
	if validity == 0 {
		return 0, fmt.Errorf("IMAP UIDVALIDITY is unavailable")
	}
	uids, err := c.moveMessagesWithDestUIDs(ctx, source, []uint32{uid}, destination, validity)
	if err != nil || len(uids) == 0 {
		return 0, err
	}
	return uids[0], nil
}

func (c *Client) moveMessagesWithDestUIDs(ctx context.Context, folderRemoteName string, uids []uint32, destFolderRemoteName string, validity uint32) ([]uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(uids) == 0 {
		return nil, nil
	}
	if c.closed {
		return nil, fmt.Errorf("client is closed")
	}

	selected, err := c.client.Select(folderRemoteName, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("select %s: %w", folderRemoteName, err)
	}
	defer c.client.Unselect()
	if validity > 0 && validity != uint32(selected.UIDValidity) {
		return nil, ErrMutationUIDValidityChanged
	}

	var uidSet imap.UIDSet
	for _, uid := range uids {
		uidSet.AddNum(imap.UID(uid))
	}

	moveCmd := c.client.Move(uidSet, destFolderRemoteName)
	data, err := moveCmd.Wait()
	if err != nil {
		return nil, err
	}
	if data == nil || data.DestUIDs == nil {
		return nil, nil
	}
	destUIDSet, ok := data.DestUIDs.(imap.UIDSet)
	if !ok {
		return nil, nil
	}
	destUIDs, ok := destUIDSet.Nums()
	if !ok {
		return nil, nil
	}
	out := make([]uint32, 0, len(destUIDs))
	for _, destUID := range destUIDs {
		if destUID > 0 {
			out = append(out, uint32(destUID))
		}
	}
	return out, nil
}

func (c *Client) DeleteMessages(ctx context.Context, folderRemoteName string, uids []uint32) error {
	_, err := c.DeleteMessagesIfUIDValidity(ctx, folderRemoteName, uids, 0)
	return err
}

func (c *Client) DeleteMessagesIfUIDValidity(ctx context.Context, folderRemoteName string, uids []uint32, expectedUIDValidity uint32) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(uids) == 0 {
		return false, nil
	}
	if c.closed {
		return false, fmt.Errorf("client is closed")
	}

	selectData, err := c.client.Select(folderRemoteName, nil).Wait()
	if err != nil {
		return false, fmt.Errorf("select %s: %w", folderRemoteName, err)
	}
	defer c.client.Unselect()
	if expectedUIDValidity > 0 && expectedUIDValidity != uint32(selectData.UIDValidity) {
		return true, nil
	}

	var uidSet imap.UIDSet
	for _, uid := range uids {
		uidSet.AddNum(imap.UID(uid))
	}

	storeCmd := c.client.Store(uidSet, &imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Silent: true,
		Flags:  []imap.Flag{imap.FlagDeleted},
	}, nil)
	if err := storeCmd.Close(); err != nil {
		return false, fmt.Errorf("store deleted: %w", err)
	}

	expungeCmd := c.client.UIDExpunge(uidSet)
	_, err = expungeCmd.Collect()
	return false, err
}
