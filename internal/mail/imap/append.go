package imap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	goimap "github.com/emersion/go-imap/v2"
)

type AppendResult struct {
	UID         uint32
	UIDValidity uint32
}

type AppendError struct {
	Err       error
	Ambiguous bool
}

func (e *AppendError) Error() string {
	return e.Err.Error()
}

func (e *AppendError) Unwrap() error {
	return e.Err
}

func IsAppendAmbiguous(err error) bool {
	var appendErr *AppendError
	return errors.As(err, &appendErr) && appendErr.Ambiguous
}

func (c *Client) AppendMessage(ctx context.Context, remoteName string, raw []byte, flags []goimap.Flag, date time.Time) (AppendResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return AppendResult{}, err
	}
	if c.closed {
		return AppendResult{}, fmt.Errorf("client is closed")
	}
	if remoteName == "" {
		return AppendResult{}, fmt.Errorf("mailbox is required")
	}
	if len(raw) == 0 {
		return AppendResult{}, fmt.Errorf("message is empty")
	}

	cmd := c.client.Append(remoteName, int64(len(raw)), &goimap.AppendOptions{Flags: flags, Time: date})
	written, err := cmd.Write(raw)
	if err != nil {
		_ = cmd.Close()
		return AppendResult{}, &AppendError{Err: fmt.Errorf("append %s data: %w", remoteName, err), Ambiguous: true}
	}
	if written != len(raw) {
		_ = cmd.Close()
		return AppendResult{}, &AppendError{Err: fmt.Errorf("append %s data: %w", remoteName, io.ErrShortWrite), Ambiguous: true}
	}
	if err := cmd.Close(); err != nil {
		return AppendResult{}, appendCommandError(remoteName, "close", err)
	}
	data, err := cmd.Wait()
	if err != nil {
		return AppendResult{}, appendCommandError(remoteName, "", err)
	}
	return AppendResult{UID: uint32(data.UID), UIDValidity: data.UIDValidity}, nil
}

func appendCommandError(remoteName, stage string, err error) error {
	label := "append " + remoteName
	if stage != "" {
		label += " " + stage
	}
	ambiguous := true
	var statusErr *goimap.Error
	if errors.As(err, &statusErr) {
		ambiguous = false
	}
	return &AppendError{Err: fmt.Errorf("%s: %w", label, err), Ambiguous: ambiguous}
}
