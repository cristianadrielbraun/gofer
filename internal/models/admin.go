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
