package models

import "time"

const (
	MailOperationMessageMutation = "message_mutation"
	MailOperationLabelMutation   = "label_mutation"
	MailOperationIMAPDraft       = "imap_draft"
	MailOperationSentCopy        = "sent_copy"
)

// MailOperationSummary is a safe, user-scoped view of a durable background
// mail operation. It intentionally has no subject, recipient, MIME, or
// provider-token fields.
type MailOperationSummary struct {
	ID                    string    `json:"id"`
	Type                  string    `json:"type"`
	AccountID             string    `json:"account_id"`
	AccountEmail          string    `json:"account_email,omitempty"`
	Provider              string    `json:"provider"`
	MessageID             int64     `json:"message_id,omitempty"`
	FolderID              string    `json:"folder_id,omitempty"`
	FolderName            string    `json:"folder_name,omitempty"`
	DestinationFolderID   string    `json:"destination_folder_id,omitempty"`
	DestinationFolderName string    `json:"destination_folder_name,omitempty"`
	LabelName             string    `json:"label_name,omitempty"`
	Operation             string    `json:"operation"`
	DraftKey              string    `json:"draft_key,omitempty"`
	State                 string    `json:"state"`
	Attempts              int       `json:"attempts"`
	LastError             string    `json:"last_error,omitempty"`
	NextRetryAt           time.Time `json:"next_retry_at,omitempty"`
	CreatedAt             time.Time `json:"created_at,omitempty"`
	UpdatedAt             time.Time `json:"updated_at,omitempty"`
	CanRetry              bool      `json:"can_retry"`
	CanReconcile          bool      `json:"can_reconcile"`
	CanCancel             bool      `json:"can_cancel"`
	Ambiguous             bool      `json:"ambiguous"`
}

type MailOperationsStatus struct {
	Operations     []MailOperationSummary `json:"operations"`
	Total          int                    `json:"total"`
	ActionRequired int                    `json:"action_required"`
}

type MailOperationAdminTypeCount struct {
	Type           string `json:"type"`
	Provider       string `json:"provider"`
	Total          int    `json:"total"`
	ActionRequired int    `json:"action_required"`
}

type MailOperationAdminAccountCount struct {
	AccountID      string `json:"account_id"`
	AccountLabel   string `json:"account_label"`
	Provider       string `json:"provider"`
	Total          int    `json:"total"`
	ActionRequired int    `json:"action_required"`
}

// MailRetentionPruneCounts records what the last maintenance pass removed.
// The counts are intentionally aggregate: admin diagnostics should show queue
// health without exposing message content or durable payloads.
type MailRetentionPruneCounts struct {
	OutgoingSends       int `json:"outgoing_sends"`
	MessageMutations    int `json:"message_mutations"`
	IMAPDraftOperations int `json:"imap_draft_operations"`
	LabelMutations      int `json:"label_mutations"`
}

type MailRetentionDiagnostics struct {
	LastRunAt  time.Time                `json:"last_run_at,omitempty"`
	LastPruned MailRetentionPruneCounts `json:"last_pruned"`
	LastError  string                   `json:"last_error,omitempty"`
}

type MailOperationAdminStateCount struct {
	State string `json:"state"`
	Count int    `json:"count"`
}

type MailOperationAdminKindStateCount struct {
	Kind  string `json:"kind"`
	State string `json:"state"`
	Count int    `json:"count"`
}

type MailOutgoingAdminCounts struct {
	Pending   int `json:"pending"`
	Sending   int `json:"sending"`
	Failed    int `json:"failed"`
	Ambiguous int `json:"ambiguous"`
}

type MailSentCopyAdminCounts struct {
	Pending   int `json:"pending"`
	Copying   int `json:"copying"`
	Failed    int `json:"failed"`
	Ambiguous int `json:"ambiguous"`
}

type MailIdleAdminCounts struct {
	Configured int `json:"configured"`
	Healthy    int `json:"healthy"`
	Fallback   int `json:"fallback"`
	Pending    int `json:"pending"`
}

type MailFolderSyncAdminCounts struct {
	Complete int `json:"complete"`
	Partial  int `json:"partial"`
	Failed   int `json:"failed"`
}

type MailOperationAdminHealth struct {
	Outgoing            MailOutgoingAdminCounts            `json:"outgoing"`
	SentCopy            MailSentCopyAdminCounts            `json:"sent_copy"`
	MessageMutations    []MailOperationAdminKindStateCount `json:"message_mutations"`
	LabelMutations      []MailOperationAdminStateCount     `json:"label_mutations"`
	IMAPDraftOperations []MailOperationAdminStateCount     `json:"imap_draft_operations"`
	IDLE                MailIdleAdminCounts                `json:"idle"`
	FolderSync          MailFolderSyncAdminCounts          `json:"folder_sync"`
	OldestPendingAt     time.Time                          `json:"oldest_pending_at,omitempty"`
	NextRetryAt         time.Time                          `json:"next_retry_at,omitempty"`
}

type MailOperationsAdminStatus struct {
	Total          int                              `json:"total"`
	ActionRequired int                              `json:"action_required"`
	ByType         []MailOperationAdminTypeCount    `json:"by_type"`
	ByAccount      []MailOperationAdminAccountCount `json:"by_account"`
	Retention      MailRetentionDiagnostics         `json:"retention"`
	Health         MailOperationAdminHealth         `json:"health"`
}
