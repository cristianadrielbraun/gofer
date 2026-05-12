package models

import "time"

type AvatarBackfillState struct {
	InProgress bool      `json:"in_progress"`
	Processed  int       `json:"processed"`
	Total      int       `json:"total"`
	Found      int       `json:"found"`
	Missing    int       `json:"missing"`
	Errors     int       `json:"errors"`
	LastError  string    `json:"last_error,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

type AvatarCacheStats struct {
	Total   int `json:"total"`
	Pending int `json:"pending"`
	Found   int `json:"found"`
	Missing int `json:"missing"`
	Error   int `json:"error"`
	Due     int `json:"due"`
}

type AvatarStatus struct {
	Backfill AvatarBackfillState `json:"backfill"`
	Cache    AvatarCacheStats    `json:"cache"`
}
