package models

import "time"

const (
	MailOperationMessageMutation = "message_mutation"
	MailOperationLabelMutation   = "label_mutation"
	MailOperationIMAPDraft       = "imap_draft"
	MailOperationProviderDraft   = "provider_draft"
	MailOperationSentCopy        = "sent_copy"
)

// MailOperationParkedFolderRemoved is why a parked operation can't finish: a
// folder it needs was deleted on the server.
const MailOperationParkedFolderRemoved = "folder_removed"

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
	// Parked operations can never finish and are not tried again;
	// ParkedReason says why. CanUndo means Gofer can put its own view of the
	// message back to how it was before the change, without the server.
	Parked       bool   `json:"parked"`
	ParkedReason string `json:"parked_reason,omitempty"`
	CanUndo      bool   `json:"can_undo"`
	// The action that queued the operation, when it was recorded: its ID,
	// what it did, who started it ("you" or "rule"), and for a mail rule
	// its ID and current name. A rule deleted since has no name.
	ActionID       string `json:"action_id,omitempty"`
	ActionKind     string `json:"action_kind,omitempty"`
	ActionSource   string `json:"action_source,omitempty"`
	ActionRuleID   string `json:"action_rule_id,omitempty"`
	ActionRuleName string `json:"action_rule_name,omitempty"`
}

// NeedsAttention reports whether the person has to act on the operation:
// retry it, reconcile it, cancel it, undo it, or fix what blocks it.
func (o MailOperationSummary) NeedsAttention() bool {
	return o.CanRetry || o.CanReconcile || o.CanCancel || o.CanUndo || o.Parked || o.State == "blocked"
}

// MailOperationGroup sums up the changes one action queued, such as Empty
// folder or a mail rule's moves. Folders and the label are set only when
// every change shares them. FolderName and DestinationName are the names
// Gofer shows, with their role, so they can be translated as in the sidebar.
type MailOperationGroup struct {
	ActionID, ActionKind, ActionSource string
	ActionRuleID, ActionRuleName       string
	AccountID, AccountEmail            string
	Accounts, Messages, Jobs           int
	Failed, Parked, Blocked, Active    int
	FolderID, FolderName, FolderRole   string
	DestinationFolderID                string
	DestinationName, DestinationRole   string
	LabelName                          string
	CreatedAt, UpdatedAt               time.Time
	Changes                            []MailOperationGroupMessage
	ChangesLoaded                      bool
}

// NeedsAttention reports whether some of the action's changes failed, can't
// be completed, or are blocked.
func (g MailOperationGroup) NeedsAttention() bool {
	return g.Failed+g.Parked+g.Blocked > 0
}

// MailOperationGroupMessage is one change of an action, with the message it
// is about as Gofer shows it.
type MailOperationGroupMessage struct {
	Operation                        MailOperationSummary
	Subject, FromName, FromEmail     string
	FolderName, FolderRole           string
	DestinationName, DestinationRole string
}

// MailOperationsList is what the Mail operations list in Settings shows:
// one row per action, then the changes that have no recorded action.
type MailOperationsList struct {
	Groups     []MailOperationGroup
	Operations []MailOperationSummary
	// ActionRequired counts the rows that need attention; Retryable counts
	// all failed work Retry all failed would retry.
	ActionRequired int
	Retryable      int
}

// Total is how many rows the list shows.
func (l MailOperationsList) Total() int {
	return len(l.Groups) + len(l.Operations)
}

// NoSentFolderError is why a sent copy failed when its IMAP account has no
// Sent folder; the failure offers to choose one.
const NoSentFolderError = "This account has no Sent folder."

type MailOperationsStatus struct {
	Operations     []MailOperationSummary `json:"operations"`
	Total          int                    `json:"total"`
	ActionRequired int                    `json:"action_required"`
	// Retryable counts all failed work that Retry all failed would retry,
	// also beyond the operations listed.
	Retryable int `json:"retryable"`
}

type MailOperationAdminTypeCount struct {
	Type           string `json:"type"`
	Provider       string `json:"provider"`
	Total          int    `json:"total"`
	ActionRequired int    `json:"action_required"`
}

// MailOperationAdminOriginCount counts the work one kind of action queued,
// such as Move or Empty folder, by who started it: a person ("you") or a mail
// rule ("rule"). Work queued before Gofer recorded actions isn't counted.
type MailOperationAdminOriginCount struct {
	Source         string `json:"source"`
	Kind           string `json:"kind"`
	Total          int    `json:"total"`
	ActionRequired int    `json:"action_required"`
	Parked         int    `json:"parked"`
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
	FolderStates        int `json:"folder_states"`
	Messages            int `json:"messages"`
	Threads             int `json:"threads"`
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

// MailSMTPAdminProfile is a process-lifetime baseline for generic SMTP
// delivery. It contains aggregate timings only; no account, recipient, or
// message data is retained.
type MailSMTPAdminProfile struct {
	Samples          int   `json:"samples"`
	Successes        int   `json:"successes"`
	Failures         int   `json:"failures"`
	Ambiguous        int   `json:"ambiguous"`
	Connections      int   `json:"connections"`
	Messages         int   `json:"messages"`
	AvgConnectAuthMs int64 `json:"avg_connect_auth_ms"`
	AvgDataMs        int64 `json:"avg_data_ms"`
	AvgTotalMs       int64 `json:"avg_total_ms"`
	AvgQueueWaitMs   int64 `json:"avg_queue_wait_ms"`
}

type MailOperationAdminHealth struct {
	Outgoing            MailOutgoingAdminCounts            `json:"outgoing"`
	SentCopy            MailSentCopyAdminCounts            `json:"sent_copy"`
	MessageMutations    []MailOperationAdminKindStateCount `json:"message_mutations"`
	LabelMutations      []MailOperationAdminStateCount     `json:"label_mutations"`
	IMAPDraftOperations []MailOperationAdminStateCount     `json:"imap_draft_operations"`
	IDLE                MailIdleAdminCounts                `json:"idle"`
	FolderSync          MailFolderSyncAdminCounts          `json:"folder_sync"`
	SMTPProfile         MailSMTPAdminProfile               `json:"smtp_profile"`
	OldestPendingAt     time.Time                          `json:"oldest_pending_at,omitempty"`
	NextRetryAt         time.Time                          `json:"next_retry_at,omitempty"`
}

// MailOperationsAdminStatus is what the admin Operations page shows. Parked
// counts the work that can't be completed, such as a change in a folder that
// was deleted on the server.
type MailOperationsAdminStatus struct {
	Scope          AdminWebmailScope                `json:"scope"`
	Total          int                              `json:"total"`
	ActionRequired int                              `json:"action_required"`
	Parked         int                              `json:"parked"`
	ByType         []MailOperationAdminTypeCount    `json:"by_type"`
	ByOrigin       []MailOperationAdminOriginCount  `json:"by_origin"`
	ByAccount      []MailOperationAdminAccountCount `json:"by_account"`
	Retention      MailRetentionDiagnostics         `json:"retention"`
	Health         MailOperationAdminHealth         `json:"health"`
}
