package models

import "time"

// IdleConnectionLimit is what Gofer has learned about how many folders of one
// mailbox it may watch in real time (IMAP IDLE), each on its own connection,
// before the mail server refuses more.
type IdleConnectionLimit struct {
	// Limit is how many folders may be watched now.
	Limit int
	// FailedAt is how many connections Gofer was opening when the server
	// last refused one, at FailedTime; zero when it never has.
	FailedAt   int
	FailedTime time.Time
	// StableAt is the most connections held without a refusal since
	// StableSince; zero until Limit connections were first held.
	StableAt    int
	StableSince time.Time
}
