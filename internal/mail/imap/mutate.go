package imap

import (
	"context"
	"fmt"

	"github.com/emersion/go-imap/v2"
)

func (c *Client) StoreFlags(ctx context.Context, folderRemoteName string, uid uint32, op imap.StoreFlagsOp, flags []imap.Flag) error {
	return c.StoreFlagsBatch(ctx, folderRemoteName, []uint32{uid}, op, flags)
}

func (c *Client) StoreFlagsBatch(ctx context.Context, folderRemoteName string, uids []uint32, op imap.StoreFlagsOp, flags []imap.Flag) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(uids) == 0 {
		return nil
	}
	if c.closed {
		return fmt.Errorf("client is closed")
	}

	_, err := c.client.Select(folderRemoteName, nil).Wait()
	if err != nil {
		return fmt.Errorf("select %s: %w", folderRemoteName, err)
	}
	defer c.client.Unselect()

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
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(uids) == 0 {
		return nil, nil
	}
	if c.closed {
		return nil, fmt.Errorf("client is closed")
	}

	_, err := c.client.Select(folderRemoteName, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("select %s: %w", folderRemoteName, err)
	}
	defer c.client.Unselect()

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
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return fmt.Errorf("client is closed")
	}

	_, err := c.client.Select(folderRemoteName, nil).Wait()
	if err != nil {
		return fmt.Errorf("select %s: %w", folderRemoteName, err)
	}
	defer c.client.Unselect()

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
		return fmt.Errorf("store deleted: %w", err)
	}

	expungeCmd := c.client.UIDExpunge(uidSet)
	_, err = expungeCmd.Collect()
	return err
}
