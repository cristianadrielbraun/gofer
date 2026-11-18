package handler

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

type mcpMessagesIn struct {
	MessageIDs []string `json:"message_ids" jsonschema:"message IDs from list_messages or search_messages"`
	Thread     bool     `json:"thread,omitempty" jsonschema:"act on each message's whole conversation"`
}

type mcpMarkIn struct {
	mcpMessagesIn
	Read    *bool `json:"read,omitempty" jsonschema:"true to mark read, false to mark unread"`
	Starred *bool `json:"starred,omitempty" jsonschema:"true to star, false to unstar"`
}

type mcpMoveIn struct {
	mcpMessagesIn
	FolderID string `json:"folder_id" jsonschema:"the folder to move to (list_folders), or inbox, archive, spam or trash for that folder of each message's account"`
}

type mcpDeleteIn struct {
	mcpMessagesIn
	Permanent bool `json:"permanent,omitempty" jsonschema:"delete for good instead of moving to Trash; needs confirm"`
	Confirm   bool `json:"confirm,omitempty" jsonschema:"must be true to delete for good: it cannot be undone"`
}

type mcpSpamIn struct {
	mcpMessagesIn
	NotSpam bool `json:"not_spam,omitempty" jsonschema:"true to move out of Spam back to the inbox"`
}

type mcpTagIn struct {
	mcpMessagesIn
	Tag    string `json:"tag" jsonschema:"the tag (label) name"`
	Remove bool   `json:"remove,omitempty"`
}

type mcpArchiveIn struct {
	mcpMessagesIn
	Unarchive bool `json:"unarchive,omitempty" jsonschema:"true to move archived messages back to the inbox"`
}

type mcpChangeOut struct {
	Result map[string]any `json:"result" jsonschema:"what Gofer reports, such as how many were changed"`
}

type mcpAttachmentIn struct {
	Filename      string `json:"filename"`
	ContentType   string `json:"content_type,omitempty"`
	ContentBase64 string `json:"content_base64" jsonschema:"the file, base64-encoded"`
}

type mcpComposeIn struct {
	AccountID   string            `json:"account_id,omitempty" jsonschema:"the account to send from; by default the one with the From address, or the first"`
	From        string            `json:"from,omitempty" jsonschema:"the From address: the account's own, one of its identities (list_accounts), or another the server allows"`
	FromName    string            `json:"from_name,omitempty" jsonschema:"the From name, for an address that is not an identity"`
	To          []string          `json:"to,omitempty" jsonschema:"addresses, each 'Name <a@b.c>' or 'a@b.c'"`
	CC          []string          `json:"cc,omitempty"`
	BCC         []string          `json:"bcc,omitempty"`
	Subject     string            `json:"subject,omitempty"`
	Body        string            `json:"body,omitempty" jsonschema:"the message as plain text"`
	HTMLBody    string            `json:"html_body,omitempty" jsonschema:"the message as HTML, instead of or besides body"`
	Attachments []mcpAttachmentIn `json:"attachments,omitempty"`
	// Set by the tools that answer or pass on a message.
	DraftID string `json:"draft_id,omitempty" jsonschema:"a draft's ID from save_draft, to send or update that draft"`
}

type mcpSendIn struct {
	mcpComposeIn
	SendAt string `json:"send_at,omitempty" jsonschema:"send later, at this RFC 3339 time, instead of now"`
}

type mcpReplyIn struct {
	MessageID   string            `json:"message_id" jsonschema:"the message to answer"`
	ReplyAll    bool              `json:"reply_all,omitempty" jsonschema:"answer every recipient, not only the sender"`
	Body        string            `json:"body,omitempty" jsonschema:"the reply as plain text; the original is quoted below it"`
	HTMLBody    string            `json:"html_body,omitempty"`
	From        string            `json:"from,omitempty" jsonschema:"by default the address the message was sent to, or the shared mailbox it is in"`
	CC          []string          `json:"cc,omitempty" jsonschema:"more recipients in Cc"`
	Attachments []mcpAttachmentIn `json:"attachments,omitempty"`
	NoQuote     bool              `json:"no_quote,omitempty" jsonschema:"leave the original out of the reply"`
	DraftOnly   bool              `json:"draft_only,omitempty" jsonschema:"save the reply as a draft instead of sending it"`
}

type mcpForwardIn struct {
	MessageID          string            `json:"message_id" jsonschema:"the message to pass on"`
	To                 []string          `json:"to"`
	CC                 []string          `json:"cc,omitempty"`
	Body               string            `json:"body,omitempty" jsonschema:"a note above the forwarded message"`
	From               string            `json:"from,omitempty"`
	WithoutAttachments bool              `json:"without_attachments,omitempty" jsonschema:"leave out the original's attachments"`
	Attachments        []mcpAttachmentIn `json:"attachments,omitempty"`
	DraftOnly          bool              `json:"draft_only,omitempty"`
}

type mcpSendOut struct {
	Status  string `json:"status" jsonschema:"sending, scheduled or saved"`
	SendID  string `json:"send_id,omitempty" jsonschema:"for get_send_status"`
	DraftID string `json:"draft_id,omitempty"`
	From    string `json:"from"`
	To      string `json:"to,omitempty"`
	Subject string `json:"subject"`
}

type mcpDiscardDraftIn struct {
	AccountID string `json:"account_id"`
	DraftID   string `json:"draft_id"`
}

type mcpSendStatusIn struct {
	SendID string `json:"send_id"`
	Cancel bool   `json:"cancel,omitempty" jsonschema:"stop a message still waiting to be sent, such as a scheduled one"`
}

func mcpTargets(in mcpMessagesIn) ([]messageBulkTarget, error) {
	if len(in.MessageIDs) == 0 {
		return nil, errors.New("message_ids is required")
	}
	if len(in.MessageIDs) > 200 {
		return nil, errors.New("at most 200 messages at a time")
	}
	targets := make([]messageBulkTarget, 0, len(in.MessageIDs))
	for _, id := range in.MessageIDs {
		if id = strings.TrimSpace(id); id != "" {
			targets = append(targets, messageBulkTarget{ID: id, Thread: in.Thread})
		}
	}
	return targets, nil
}

// mcpChange runs one of the message actions of the web app's selection bar.
func (h *Handler) mcpChange(ctx context.Context, handler http.HandlerFunc, path string, payload messageBulkRequest) (mcpChangeOut, error) {
	resp, err := h.mcpInvoke(ctx, handler, mcpRequest{Path: path, JSON: payload})
	if err != nil {
		return mcpChangeOut{}, err
	}
	if err := resp.Err(); err != nil {
		return mcpChangeOut{}, err
	}
	out := mcpChangeOut{Result: map[string]any{}}
	_ = resp.Decode(&out.Result)
	return out, nil
}

func (h *Handler) addMCPMailWriteTools(server *mcp.Server) {
	organize := &mcp.ToolAnnotations{OpenWorldHint: boolPtr(false), DestructiveHint: boolPtr(false), IdempotentHint: true}
	destructive := &mcp.ToolAnnotations{OpenWorldHint: boolPtr(false), DestructiveHint: boolPtr(true)}
	sends := &mcp.ToolAnnotations{OpenWorldHint: boolPtr(true), DestructiveHint: boolPtr(false)}

	mcp.AddTool(server, &mcp.Tool{Name: "mark_messages", Title: "Mark read, unread, starred", Annotations: organize,
		Description: "Mark messages read or unread, and star or unstar them. Changes reach the mail server."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpMarkIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeMailOrganize)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			targets, err := mcpTargets(in.mcpMessagesIn)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if err := h.mcpCheckMessages(ctx, userID, in.MessageIDs, in.Thread); err != nil {
				return nil, mcpChangeOut{}, err
			}
			if in.Read == nil && in.Starred == nil {
				return nil, mcpChangeOut{}, errors.New("give read, starred or both")
			}
			out := mcpChangeOut{Result: map[string]any{}}
			if in.Read != nil {
				state := "read"
				if !*in.Read {
					state = "unread"
				}
				r, err := h.mcpChange(ctx, h.handleMarkMessagesRead, "/api/messages/read", messageBulkRequest{Targets: targets, State: state})
				if err != nil {
					return nil, out, err
				}
				out.Result["read"] = r.Result
			}
			if in.Starred != nil {
				state := "starred"
				if !*in.Starred {
					state = "unstarred"
				}
				r, err := h.mcpChange(ctx, h.handleMarkMessagesStarred, "/api/messages/star", messageBulkRequest{Targets: targets, State: state})
				if err != nil {
					return nil, out, err
				}
				out.Result["starred"] = r.Result
			}
			return nil, out, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "move_messages", Title: "Move messages", Annotations: organize,
		Description: "Move messages to another folder, of the same account or another one."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpMoveIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeMailOrganize)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			targets, err := mcpTargets(in.mcpMessagesIn)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if err := h.mcpCheckMessages(ctx, userID, in.MessageIDs, in.Thread); err != nil {
				return nil, mcpChangeOut{}, err
			}
			if strings.TrimSpace(in.FolderID) == "" {
				return nil, mcpChangeOut{}, errors.New("folder_id is required")
			}
			if err := h.mcpCheckFolder(ctx, userID, strings.TrimSpace(in.FolderID), ""); err != nil {
				return nil, mcpChangeOut{}, err
			}
			out, err := h.mcpChange(ctx, h.handleMoveMessages, "/api/messages/move", messageBulkRequest{Targets: targets, FolderID: strings.TrimSpace(in.FolderID)})
			return nil, out, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "archive_messages", Title: "Archive messages", Annotations: organize,
		Description: "Move messages to their account's Archive (or a shared mailbox's own), or back to the inbox with unarchive."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpArchiveIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeMailOrganize)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			targets, err := mcpTargets(in.mcpMessagesIn)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if err := h.mcpCheckMessages(ctx, userID, in.MessageIDs, in.Thread); err != nil {
				return nil, mcpChangeOut{}, err
			}
			if in.Unarchive {
				out, err := h.mcpChange(ctx, h.handleUnarchiveMessages, "/api/messages/unarchive", messageBulkRequest{Targets: targets})
				return nil, out, err
			}
			out, err := h.mcpChange(ctx, h.handleArchiveMessages, "/api/messages/archive", messageBulkRequest{Targets: targets})
			return nil, out, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "delete_messages", Title: "Delete messages", Annotations: destructive,
		Description: "Move messages to Trash; messages already in Trash, or with permanent and confirm, are deleted for good, which cannot be undone."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpDeleteIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeMailOrganize)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			targets, err := mcpTargets(in.mcpMessagesIn)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if err := h.mcpCheckMessages(ctx, userID, in.MessageIDs, in.Thread); err != nil {
				return nil, mcpChangeOut{}, err
			}
			if in.Permanent && !in.Confirm {
				return nil, mcpChangeOut{}, errors.New("deleting for good cannot be undone: ask the user, then call again with confirm: true")
			}
			out, err := h.mcpChange(ctx, h.handleDeleteMessages, "/api/messages/delete", messageBulkRequest{Targets: targets, ForGood: in.Permanent, Permanent: in.Permanent})
			return nil, out, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "mark_spam", Title: "Mark as spam", Annotations: organize,
		Description: "Move messages to Spam and tell the server they are spam, or with not_spam move them back to the inbox."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpSpamIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeMailOrganize)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			targets, err := mcpTargets(in.mcpMessagesIn)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if err := h.mcpCheckMessages(ctx, userID, in.MessageIDs, in.Thread); err != nil {
				return nil, mcpChangeOut{}, err
			}
			if in.NotSpam {
				out, err := h.mcpChange(ctx, h.handleMarkMessagesNotSpam, "/api/messages/not-spam", messageBulkRequest{Targets: targets})
				return nil, out, err
			}
			out, err := h.mcpChange(ctx, h.handleMarkMessagesSpam, "/api/messages/spam", messageBulkRequest{Targets: targets})
			return nil, out, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "tag_messages", Title: "Tag messages", Annotations: organize,
		Description: "Add a tag (label) to messages, making it if needed, or remove one."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpTagIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeMailOrganize)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			targets, err := mcpTargets(in.mcpMessagesIn)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if err := h.mcpCheckMessages(ctx, userID, in.MessageIDs, in.Thread); err != nil {
				return nil, mcpChangeOut{}, err
			}
			if strings.TrimSpace(in.Tag) == "" {
				return nil, mcpChangeOut{}, errors.New("tag is required")
			}
			if in.Remove {
				out, err := h.mcpChange(ctx, h.handleUnlabelMessages, "/api/messages/unlabel", messageBulkRequest{Targets: targets, Label: in.Tag})
				return nil, out, err
			}
			out, err := h.mcpChange(ctx, h.handleLabelMessages, "/api/messages/label", messageBulkRequest{Targets: targets, Label: in.Tag})
			return nil, out, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "send_message", Title: "Send a message", Annotations: sends,
		Description: "Write and send a new message, now or at send_at, from an account or one of its identities (shared mailboxes' included). Its copy goes to the Sent folder of the mailbox it is sent from."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpSendIn) (*mcp.CallToolResult, mcpSendOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeMailSend)
			if err != nil {
				return nil, mcpSendOut{}, err
			}
			out, err := h.mcpCompose(ctx, userID, in.mcpComposeIn, nil, in.SendAt, false)
			return nil, out, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "save_draft", Title: "Save a draft", Annotations: sends,
		Description: "Save a message as a draft, in the Drafts folder of the mailbox it is from, to send later with send_message and its draft_id. With draft_id, update that draft."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpComposeIn) (*mcp.CallToolResult, mcpSendOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeMailSend)
			if err != nil {
				return nil, mcpSendOut{}, err
			}
			out, err := h.mcpCompose(ctx, userID, in, nil, "", true)
			return nil, out, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "discard_draft", Title: "Discard a draft", Annotations: destructive,
		Description: "Delete a draft, here and on the server."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpDiscardDraftIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, _, err := h.mcpBegin(ctx, req, MCPScopeMailSend)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if err := h.mcpCheckDraft(ctx, in.AccountID, in.DraftID); err != nil {
				return nil, mcpChangeOut{}, err
			}
			resp, err := h.mcpInvoke(ctx, h.handleDiscardComposeDraft, mcpRequest{Path: "/compose/draft/discard", Form: url.Values{"account_id": {in.AccountID}, "draft_id": {in.DraftID}}})
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if err := resp.Err(); err != nil {
				return nil, mcpChangeOut{}, err
			}
			return nil, mcpChangeOut{Result: map[string]any{"discarded": in.DraftID}}, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "reply_to_message", Title: "Reply", Annotations: sends,
		Description: "Answer a message, or everyone on it with reply_all, in its conversation. By default it is sent from the address the message came to, so a message in a shared mailbox is answered as that mailbox."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpReplyIn) (*mcp.CallToolResult, mcpSendOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeMailSend)
			if err != nil {
				return nil, mcpSendOut{}, err
			}
			original, account, identityID, err := h.mcpOriginal(ctx, userID, in.MessageID)
			if err != nil {
				return nil, mcpSendOut{}, err
			}
			identities, _ := h.db.ListAccountIdentities(ctx, userID, account.ID)
			own := map[string]bool{}
			for _, address := range ownAddresses(account, identities) {
				own[address] = true
			}
			to := []string{mcpFormatAddress(original.From)}
			var cc []string
			if in.ReplyAll {
				for _, c := range original.To {
					if !own[strings.ToLower(c.Email)] && !strings.EqualFold(c.Email, original.From.Email) {
						to = append(to, mcpFormatAddress(c))
					}
				}
				for _, c := range original.CC {
					if !own[strings.ToLower(c.Email)] {
						cc = append(cc, mcpFormatAddress(c))
					}
				}
			}
			// A message the user sent is answered to its recipients.
			if own[strings.ToLower(original.From.Email)] && len(original.To) > 0 {
				to = nil
				for _, c := range original.To {
					to = append(to, mcpFormatAddress(c))
				}
			}
			compose := mcpComposeIn{
				AccountID: account.ID, From: in.From, To: to, CC: append(cc, in.CC...),
				Subject: mcpPrefixedSubject("Re:", original.Subject), Body: in.Body, HTMLBody: in.HTMLBody, Attachments: in.Attachments,
			}
			if !in.NoQuote {
				compose.Body = in.Body + "\n\n" + mcpQuote(ctx, original)
			}
			thread := url.Values{"in_reply_to": {original.InternetMessageID}, "references": {strings.TrimSpace(original.References + " " + original.InternetMessageID)}}
			if in.From == "" && identityID != "" {
				thread.Set("identity_id", identityID)
			}
			out, err := h.mcpCompose(ctx, userID, compose, thread, "", in.DraftOnly)
			return nil, out, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "forward_message", Title: "Forward", Annotations: sends,
		Description: "Pass a message on to others, with its attachments unless left out, and a note above it."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpForwardIn) (*mcp.CallToolResult, mcpSendOut, error) {
			ctx, userID, err := h.mcpBegin(ctx, req, MCPScopeMailSend)
			if err != nil {
				return nil, mcpSendOut{}, err
			}
			original, account, identityID, err := h.mcpOriginal(ctx, userID, in.MessageID)
			if err != nil {
				return nil, mcpSendOut{}, err
			}
			compose := mcpComposeIn{
				AccountID: account.ID, From: in.From, To: in.To, CC: in.CC,
				Subject: mcpPrefixedSubject("Fwd:", original.Subject), Attachments: in.Attachments,
				Body: strings.TrimSpace(in.Body+"\n\n"+mcpForwardHeader(ctx, original)) + "\n\n" + original.TextBody,
			}
			extra := url.Values{}
			if in.From == "" && identityID != "" {
				extra.Set("identity_id", identityID)
			}
			if !in.WithoutAttachments {
				for _, a := range original.Attachments {
					if !a.Inline {
						extra.Add("existing_attachment_id", strconv.FormatInt(a.ID, 10))
					}
				}
			}
			out, err := h.mcpCompose(ctx, userID, compose, extra, "", in.DraftOnly)
			return nil, out, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "get_send_status", Title: "Sending status", Annotations: &mcp.ToolAnnotations{OpenWorldHint: boolPtr(false)},
		Description: "Whether a message sent with send_message, reply_to_message or forward_message went out, or why not; with cancel, stop one still waiting."},
		func(ctx context.Context, req *mcp.CallToolRequest, in mcpSendStatusIn) (*mcp.CallToolResult, mcpChangeOut, error) {
			ctx, _, err := h.mcpBegin(ctx, req, MCPScopeMailSend)
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			id := strings.TrimSpace(in.SendID)
			if mcpPolicy(ctx).Limited() {
				send, err := h.db.GetOutgoingSend(ctx, id)
				if err != nil {
					return nil, mcpChangeOut{}, errors.New("no message being sent with that ID")
				}
				account, err := h.ownedAccount(ctx, send.AccountID)
				if err != nil || account == nil {
					return nil, mcpChangeOut{}, errors.New("no message being sent with that ID")
				}
				if err := mcpCheckSender(ctx, account, send.EnvelopeFrom); err != nil {
					return nil, mcpChangeOut{}, err
				}
			}
			if in.Cancel {
				resp, err := h.mcpInvoke(ctx, h.handleOutgoingSendCancel, mcpRequest{Path: "/api/outgoing-sends/" + id + "/cancel", PathValues: map[string]string{"id": id}})
				if err != nil {
					return nil, mcpChangeOut{}, err
				}
				if err := resp.Err(); err != nil {
					return nil, mcpChangeOut{}, err
				}
			}
			resp, err := h.mcpInvoke(ctx, h.handleOutgoingSendGet, mcpRequest{Method: http.MethodGet, Path: "/api/outgoing-sends/" + id, PathValues: map[string]string{"id": id}})
			if err != nil {
				return nil, mcpChangeOut{}, err
			}
			if err := resp.Err(); err != nil {
				return nil, mcpChangeOut{}, err
			}
			out := mcpChangeOut{Result: map[string]any{}}
			_ = resp.Decode(&out.Result)
			return nil, out, nil
		})
}

func mcpFormatAddress(c models.Contact) string {
	if strings.TrimSpace(c.Name) == "" || strings.EqualFold(c.Name, c.Email) {
		return c.Email
	}
	return (&mail.Address{Name: c.Name, Address: c.Email}).String()
}

func mcpPrefixedSubject(prefix, subject string) string {
	trimmed := strings.TrimSpace(subject)
	if strings.HasPrefix(strings.ToLower(trimmed), strings.ToLower(prefix)) {
		return trimmed
	}
	return prefix + " " + trimmed
}

func mcpOriginalText(e *models.Email) string {
	if strings.TrimSpace(e.TextBody) != "" {
		return e.TextBody
	}
	return message.TextFromHTML([]byte(e.HTMLBody))
}

func mcpQuote(ctx context.Context, e *models.Email) string {
	var b strings.Builder
	fmt.Fprintf(&b, "On %s, %s wrote:\n", e.DateFull, mcpFormatAddress(e.From))
	for _, line := range strings.Split(strings.TrimRight(mcpOriginalText(e), "\n"), "\n") {
		b.WriteString("> " + line + "\n")
	}
	return b.String()
}

func mcpForwardHeader(ctx context.Context, e *models.Email) string {
	var to []string
	for _, c := range e.To {
		to = append(to, mcpFormatAddress(c))
	}
	return fmt.Sprintf("---------- Forwarded message ----------\nFrom: %s\nDate: %s\nSubject: %s\nTo: %s", mcpFormatAddress(e.From), e.DateFull, e.Subject, strings.Join(to, ", "))
}

// mcpOriginal loads a message being answered or passed on, its account, and
// the identity a reply goes out as by default (replyIdentity).
func (h *Handler) mcpOriginal(ctx context.Context, userID, messageID string) (*models.Email, *models.Account, string, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(messageID), 10, 64)
	if err != nil || id <= 0 {
		return nil, nil, "", errors.New("message_id is a message ID from list_messages or search_messages")
	}
	if !h.db.IsBodyFetchedInternal(ctx, id) {
		if info, err := h.db.GetMessageFetchInfoForUser(ctx, id, userID); err == nil && info != nil {
			h.ensureBodyFetched(message.WithDownloadLimit(ctx, message.AutoDownloadMaxBytes), id, info.AccountID)
		}
	}
	email, err := h.db.GetEmailByIDForUser(ctx, strconv.FormatInt(id, 10), userID)
	if err != nil {
		return nil, nil, "", err
	}
	if email == nil {
		return nil, nil, "", errors.New("no message with that ID")
	}
	if err := h.mcpCheckMessage(ctx, userID, id); err != nil {
		return nil, nil, "", err
	}
	if strings.TrimSpace(email.TextBody) == "" || email.TextBody == email.Preview {
		if raw, err := h.db.GetEmailBodyForUser(ctx, email.ID, userID); err == nil && len(raw) > 0 {
			email.HTMLBody = string(raw)
			email.TextBody = ""
		}
	}
	account, err := h.ownedAccount(ctx, email.AccountID)
	if err != nil || account == nil {
		return nil, nil, "", errors.New("the message's account is not available")
	}
	identities, _ := h.db.ListAccountIdentities(ctx, userID, account.ID)
	folderMailbox, _ := h.db.FolderSharedMailbox(ctx, email.FolderID)
	identityID := ""
	if identity, ok := replyIdentity(account, identities, email, folderMailbox); ok {
		identityID = identity.ID
	}
	return email, account, identityID, nil
}

// mcpCompose sends, schedules or saves a message as the web app's compose
// form does. extra adds form values (threading, a chosen identity,
// forwarded attachments).
func (h *Handler) mcpCompose(ctx context.Context, userID string, in mcpComposeIn, extra url.Values, sendAt string, draft bool) (mcpSendOut, error) {
	form := url.Values{}
	accounts, err := h.db.GetAccounts(ctx, userID)
	if err != nil {
		return mcpSendOut{}, err
	}
	from := strings.ToLower(strings.TrimSpace(in.From))
	var account *models.Account
	for i := range accounts {
		a := &accounts[i]
		if in.AccountID != "" && a.ID != in.AccountID {
			continue
		}
		if in.AccountID != "" || from == "" || strings.EqualFold(a.Email, from) {
			account = a
			break
		}
		for _, identity := range a.Identities {
			if strings.EqualFold(identity.Email, from) {
				account = a
				break
			}
		}
		if account != nil {
			break
		}
	}
	if account == nil {
		if in.AccountID != "" {
			return mcpSendOut{}, errors.New("no account with that ID (see list_accounts)")
		}
		if len(accounts) == 0 {
			return mcpSendOut{}, errors.New("there is no account to send from")
		}
		account = &accounts[0]
	}
	// A limited token sends by default from the first account whose own
	// mailbox it may reach.
	if policy := mcpPolicy(ctx); policy.Limited() && in.AccountID == "" && from == "" && len(extra["identity_id"]) == 0 && !policy.Allows(mcpOwnMailboxKey(account.ID)) {
		account = nil
		for i := range accounts {
			if policy.Allows(mcpOwnMailboxKey(accounts[i].ID)) {
				account = &accounts[i]
				break
			}
		}
		if account == nil {
			return mcpSendOut{}, errors.New("say which address to send from (from): this token may send only as its shared mailboxes")
		}
	}
	form.Set("account_id", account.ID)
	sender := account.Email
	switch {
	case from == "" || strings.EqualFold(account.Email, from):
	default:
		matched := false
		for _, identity := range account.Identities {
			if strings.EqualFold(identity.Email, from) {
				if !identity.Usable() {
					return mcpSendOut{}, fmt.Errorf("the identity %s is turned off or its mailbox is no longer shared", identity.Email)
				}
				form.Set("identity_id", identity.ID)
				matched = true
				break
			}
		}
		if !matched {
			form.Set("from_email", in.From)
			form.Set("from_name", in.FromName)
		}
		sender = in.From
	}
	for key, values := range extra {
		if key == "identity_id" && form.Get("identity_id") == "" && form.Get("from_email") == "" {
			form.Set(key, values[0])
			for _, identity := range account.Identities {
				if identity.ID == values[0] {
					sender = identity.Email
				}
			}
			continue
		}
		if key != "identity_id" {
			form[key] = append(form[key], values...)
		}
	}
	if err := mcpCheckSender(ctx, account, sender); err != nil {
		return mcpSendOut{}, err
	}
	form.Set("to", strings.Join(in.To, ", "))
	form.Set("cc", strings.Join(in.CC, ", "))
	form.Set("bcc", strings.Join(in.BCC, ", "))
	form.Set("subject", in.Subject)
	form.Set("body", in.Body)
	if strings.TrimSpace(in.HTMLBody) != "" {
		form.Set("html_body", in.HTMLBody)
	} else {
		form.Set("compose_format", "plain")
	}
	if in.DraftID != "" {
		form.Set("draft_id", in.DraftID)
	}
	for _, a := range in.Attachments {
		id, err := h.mcpUploadAttachment(ctx, a)
		if err != nil {
			return mcpSendOut{}, err
		}
		form.Add("attachment_id", id.ID)
		form.Add("attachment_filename", id.Filename)
		form.Add("attachment_content_type", id.ContentType)
		form.Add("attachment_size", strconv.Itoa(id.Size))
	}
	out := mcpSendOut{From: sender, To: form.Get("to"), Subject: in.Subject}

	if !draft && len(in.To)+len(in.CC)+len(in.BCC) == 0 {
		return out, errors.New("give at least one recipient")
	}
	handler, path := h.handleCompose, "/compose"
	switch {
	case draft:
		handler, path = h.handleComposeDraft, "/compose/draft"
	case strings.TrimSpace(sendAt) != "":
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(sendAt))
		if err != nil {
			return out, errors.New("send_at is an RFC 3339 time, such as 2026-12-01T09:00:00+08:00")
		}
		at = at.UTC()
		form.Set("schedule_timezone", "UTC")
		form.Set("schedule_date", at.Format("2006-01-02"))
		form.Set("schedule_hour", strconv.Itoa(at.Hour()))
		form.Set("schedule_minute", strconv.Itoa(at.Minute()))
		handler, path = h.handleComposeSchedule, "/compose/schedule"
	}
	resp, err := h.mcpInvoke(ctx, handler, mcpRequest{Path: path, Form: form})
	if err != nil {
		return out, err
	}
	if err := resp.Err(); err != nil {
		return out, err
	}
	var result map[string]any
	_ = resp.Decode(&result)
	out.Status, _ = result["status"].(string)
	out.SendID, _ = result["send_id"].(string)
	if out.SendID == "" {
		out.SendID, _ = result["id"].(string)
	}
	out.DraftID, _ = result["draft_id"].(string)
	switch {
	case draft:
		out.Status = "saved"
	case sendAt != "":
		out.Status = "scheduled"
	case out.Status == "":
		out.Status = "sending"
	}
	return out, nil
}

type mcpUploaded struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
}

func (h *Handler) mcpUploadAttachment(ctx context.Context, a mcpAttachmentIn) (mcpUploaded, error) {
	name := strings.TrimSpace(a.Filename)
	if name == "" {
		return mcpUploaded{}, errors.New("an attachment needs a filename")
	}
	content, err := base64.StdEncoding.DecodeString(strings.TrimSpace(a.ContentBase64))
	if err != nil {
		return mcpUploaded{}, fmt.Errorf("attachment %s: content_base64 is not base64", name)
	}
	resp, err := h.mcpInvoke(ctx, h.handleComposeAttachmentUpload, mcpRequest{Path: "/compose/attachments", Multipart: &mcpMultipart{Field: "attachment", FileName: name, Content: content}})
	if err != nil {
		return mcpUploaded{}, err
	}
	if err := resp.Err(); err != nil {
		return mcpUploaded{}, fmt.Errorf("attachment %s: %w", name, err)
	}
	var up mcpUploaded
	if err := resp.Decode(&up); err != nil || up.ID == "" {
		return mcpUploaded{}, fmt.Errorf("attachment %s was not stored", name)
	}
	if a.ContentType != "" {
		up.ContentType = a.ContentType
	}
	return up, nil
}
