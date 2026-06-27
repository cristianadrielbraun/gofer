package models

import "time"

type AvatarBackfillState struct {
	InProgress      bool                  `json:"in_progress"`
	CancelRequested bool                  `json:"cancel_requested"`
	Canceled        bool                  `json:"canceled"`
	Mode            string                `json:"mode"`
	Processed       int                   `json:"processed"`
	Total           int                   `json:"total"`
	Found           int                   `json:"found"`
	Missing         int                   `json:"missing"`
	Errors          int                   `json:"errors"`
	ProviderStats   []AvatarProviderStats `json:"provider_stats,omitempty"`
	LastError       string                `json:"last_error,omitempty"`
	StartedAt       time.Time             `json:"started_at,omitempty"`
	FinishedAt      time.Time             `json:"finished_at,omitempty"`
}

type AvatarCacheStats struct {
	Total           int                   `json:"total"`
	Pending         int                   `json:"pending"`
	Found           int                   `json:"found"`
	Missing         int                   `json:"missing"`
	Error           int                   `json:"error"`
	Due             int                   `json:"due"`
	GravatarChecked int                   `json:"gravatar_checked"`
	GravatarFound   int                   `json:"gravatar_found"`
	GravatarMissing int                   `json:"gravatar_missing"`
	GravatarError   int                   `json:"gravatar_error"`
	BIMIChecked     int                   `json:"bimi_checked"`
	BIMIFound       int                   `json:"bimi_found"`
	BIMIMissing     int                   `json:"bimi_missing"`
	BIMIError       int                   `json:"bimi_error"`
	BIMISkipped     int                   `json:"bimi_skipped"`
	OtherFound      int                   `json:"other_found"`
	ProviderStats   []AvatarProviderStats `json:"provider_stats"`
}

type AvatarProviderStats struct {
	Provider string `json:"provider"`
	InUse    int    `json:"in_use"`
	Checked  int    `json:"checked"`
	Found    int    `json:"found"`
	Missing  int    `json:"missing"`
	Skipped  int    `json:"skipped"`
	Error    int    `json:"error"`
}

type AvatarStatus struct {
	Backfill       AvatarBackfillState `json:"backfill"`
	Cache          AvatarCacheStats    `json:"cache"`
	RecentAttempts []AvatarAttemptLog  `json:"recent_attempts"`
	RecentErrors   []AvatarAttemptLog  `json:"recent_errors"`
}

type AvatarAttemptLog struct {
	Email     string    `json:"email"`
	Provider  string    `json:"provider"`
	Status    string    `json:"status"`
	Message   string    `json:"message,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type AvatarProviderState struct {
	Provider  string    `json:"provider"`
	Status    string    `json:"status"`
	Message   string    `json:"message,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
}

type AvatarSenderRow struct {
	Email         string                `json:"email"`
	EmailHash     string                `json:"email_hash"`
	InUse         AvatarInUse           `json:"in_use"`
	Status        string                `json:"status"`
	Source        string                `json:"source"`
	AvatarURL     string                `json:"avatar_url,omitempty"`
	AvatarDataURL string                `json:"avatar_data_url,omitempty"`
	Error         string                `json:"error,omitempty"`
	FetchedAt     time.Time             `json:"fetched_at,omitempty"`
	ExpiresAt     time.Time             `json:"expires_at,omitempty"`
	NextRetryAt   time.Time             `json:"next_retry_at,omitempty"`
	UpdatedAt     time.Time             `json:"updated_at,omitempty"`
	Providers     []AvatarProviderState `json:"providers"`
}

type AvatarInUse struct {
	Status        string    `json:"status"`
	Source        string    `json:"source"`
	AvatarURL     string    `json:"avatar_url,omitempty"`
	AvatarDataURL string    `json:"avatar_data_url,omitempty"`
	Error         string    `json:"error,omitempty"`
	FetchedAt     time.Time `json:"fetched_at,omitempty"`
	ExpiresAt     time.Time `json:"expires_at,omitempty"`
	NextRetryAt   time.Time `json:"next_retry_at,omitempty"`
}

type ContactAdminStatus struct {
	Backfill     ContactBackfillState   `json:"backfill"`
	Total        int                    `json:"total"`
	Manual       int                    `json:"manual"`
	Observed     int                    `json:"observed"`
	Suppressed   int                    `json:"suppressed"`
	AddedToday   int                    `json:"added_today"`
	DeletedToday int                    `json:"deleted_today"`
	LastBackfill time.Time              `json:"last_backfill,omitempty"`
	RecentEvents []ContactActivityEvent `json:"recent_events"`
	AccountSync  []ContactSyncStatus    `json:"account_sync"`
}

type ContactSyncStatus struct {
	AccountID       string    `json:"account_id"`
	AccountName     string    `json:"account_name"`
	AccountEmail    string    `json:"account_email"`
	Provider        string    `json:"provider"`
	Enabled         bool      `json:"enabled"`
	Capable         bool      `json:"capable"`
	Running         bool      `json:"running"`
	LastStartedAt   time.Time `json:"last_started_at,omitempty"`
	LastSuccessAt   time.Time `json:"last_success_at,omitempty"`
	LastImportCount int       `json:"last_import_count"`
	LastError       string    `json:"last_error,omitempty"`
}

type ContactBackfillState struct {
	InProgress bool      `json:"in_progress"`
	Processed  int       `json:"processed"`
	Total      int       `json:"total"`
	Added      int       `json:"added"`
	LastError  string    `json:"last_error,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

type ContactActivityEvent struct {
	Type      string    `json:"type"`
	Email     string    `json:"email,omitempty"`
	Message   string    `json:"message,omitempty"`
	Count     int       `json:"count,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type LabelAdminStatus struct {
	Totals   LabelAdminTotals         `json:"totals"`
	Accounts []LabelAccountSyncStatus `json:"accounts"`
}

type LabelAdminTotals struct {
	Accounts                int `json:"accounts"`
	TotalMessages           int `json:"total_messages"`
	MessagesWithLabels      int `json:"messages_with_labels"`
	MessagesWithoutLabels   int `json:"messages_without_labels"`
	ProviderBackedMessages  int `json:"provider_backed_messages"`
	LocalOnlyMessages       int `json:"local_only_messages"`
	MissingProviderMessages int `json:"missing_provider_messages"`
	MissingIdentityMessages int `json:"missing_identity_messages"`
	KnownLabels             int `json:"known_labels"`
	PendingMutations        int `json:"pending_mutations"`
	MutationErrors          int `json:"mutation_errors"`
	LastRunMissingProvider  int `json:"last_run_missing_provider"`
	LastRunSkipped          int `json:"last_run_skipped"`
	LastRunFailed           int `json:"last_run_failed"`
}

type LabelAccountSyncStatus struct {
	AccountID               string                   `json:"account_id"`
	AccountName             string                   `json:"account_name"`
	AccountEmail            string                   `json:"account_email"`
	AccountProvider         string                   `json:"account_provider"`
	LabelProvider           string                   `json:"label_provider"`
	TotalMessages           int                      `json:"total_messages"`
	MessagesWithLabels      int                      `json:"messages_with_labels"`
	MessagesWithoutLabels   int                      `json:"messages_without_labels"`
	ProviderBackedMessages  int                      `json:"provider_backed_messages"`
	LocalLabelMessages      int                      `json:"local_label_messages"`
	LocalOnlyMessages       int                      `json:"local_only_messages"`
	MissingProviderMessages int                      `json:"missing_provider_messages"`
	MissingIdentityMessages int                      `json:"missing_identity_messages"`
	KnownLabels             int                      `json:"known_labels"`
	ProviderLabels          int                      `json:"provider_labels"`
	LocalLabels             int                      `json:"local_labels"`
	PendingMutations        int                      `json:"pending_mutations"`
	MutationErrors          int                      `json:"mutation_errors"`
	LatestMutationError     string                   `json:"latest_mutation_error,omitempty"`
	Sync                    LabelSyncRunStatus       `json:"sync"`
	TopLabels               []LabelUsageSummary      `json:"top_labels"`
	OutlookGraph            *OutlookGraphDiagnostics `json:"outlook_graph,omitempty"`
	GmailAPI                *GmailAPIDiagnostics     `json:"gmail_api,omitempty"`
}

type OutlookGraphDiagnostics struct {
	GraphBackedMessages              int  `json:"graph_backed_messages"`
	IMAPBackedMessages               int  `json:"imap_backed_messages"`
	MessageParityDelta               int  `json:"message_parity_delta"`
	GraphParityReady                 bool `json:"graph_parity_ready"`
	MessagesMissingGraphID           int  `json:"messages_missing_graph_id"`
	MissingGraphIDWithInternetID     int  `json:"missing_graph_id_with_internet_id"`
	MissingGraphIDWithoutInternetID  int  `json:"missing_graph_id_without_internet_id"`
	MissingGraphIDWithoutGraphFolder int  `json:"missing_graph_id_without_graph_folder"`
	LocalFolders                     int  `json:"local_folders"`
	GraphBackedFolders               int  `json:"graph_backed_folders"`
	FoldersMissingGraphID            int  `json:"folders_missing_graph_id"`
}

type GmailAPIDiagnostics struct {
	APIBackedMessages               int       `json:"api_backed_messages"`
	IMAPBackedMessages              int       `json:"imap_backed_messages"`
	MessageParityDelta              int       `json:"message_parity_delta"`
	APIParityReady                  bool      `json:"api_parity_ready"`
	MessagesMissingGmailID          int       `json:"messages_missing_gmail_id"`
	MissingGmailIDWithInternetID    int       `json:"missing_gmail_id_with_internet_id"`
	MissingGmailIDWithoutInternetID int       `json:"missing_gmail_id_without_internet_id"`
	MissingGmailIDWithoutGmailLabel int       `json:"missing_gmail_id_without_gmail_label"`
	LocalFolders                    int       `json:"local_folders"`
	GmailBackedFolders              int       `json:"gmail_backed_folders"`
	FoldersMissingGmailID           int       `json:"folders_missing_gmail_id"`
	HistoryCursor                   string    `json:"history_cursor,omitempty"`
	HasHistoryCursor                bool      `json:"has_history_cursor"`
	PollProfileHistoryID            string    `json:"poll_profile_history_id,omitempty"`
	LastPollAt                      time.Time `json:"last_poll_at,omitempty"`
	LastPollChangeAt                time.Time `json:"last_poll_change_at,omitempty"`
	LastPollError                   string    `json:"last_poll_error,omitempty"`
	PollConsecutiveErrors           int       `json:"poll_consecutive_errors"`
}

type LabelSyncRunStatus struct {
	LastFullSyncAt              time.Time `json:"last_full_sync_at,omitempty"`
	LastSuccessAt               time.Time `json:"last_success_at,omitempty"`
	LastRunStartedAt            time.Time `json:"last_run_started_at,omitempty"`
	LastRunFinishedAt           time.Time `json:"last_run_finished_at,omitempty"`
	LastError                   string    `json:"last_error,omitempty"`
	Cursor                      string    `json:"cursor,omitempty"`
	LastTotalMessages           int       `json:"last_total_messages"`
	LastSyncedMessages          int       `json:"last_synced_messages"`
	LastWithLabels              int       `json:"last_with_labels"`
	LastWithoutLabels           int       `json:"last_without_labels"`
	LastMissingProviderMessages int       `json:"last_missing_provider_messages"`
	LastSkippedMessages         int       `json:"last_skipped_messages"`
	LastFailedMessages          int       `json:"last_failed_messages"`
	LastPendingMutations        int       `json:"last_pending_mutations"`
}

type LabelUsageSummary struct {
	Name         string `json:"name"`
	ProviderType string `json:"provider_type"`
	Count        int    `json:"count"`
}
