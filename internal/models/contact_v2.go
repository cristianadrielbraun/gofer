package models

type ContactProfile struct {
	ID              string
	UserID          string
	DisplayName     string
	SortName        string
	PrimaryEmail    string
	AvatarURL       string
	Notes           string
	IsDeleted       bool
	Cards           []ContactCard
	Fields          []ContactField
	SyncMemberships []ContactSyncMembership
	Insights        []ContactInsight
}

// ContactSyncMembership describes where Gofer should actively replicate a
// profile. It is deliberately separate from ContactCard, which describes a
// copy that already exists in a provider or in Gofer Local.
type ContactSyncMembership struct {
	ID            string
	UserID        string
	ProfileID     string
	AccountID     string
	AddressBookID string
	Enabled       bool
	Status        string
	LastError     string
}

type ContactInsight struct {
	Kind     string
	Severity string
	Title    string
	Message  string
	Field    string
	Source   string
	Count    int
}

type ContactCard struct {
	ID             string
	UserID         string
	ProfileID      string
	Kind           string
	Provider       string
	AccountID      string
	AddressBookID  string
	RemoteID       string
	Etag           string
	RawPayload     string
	RawPayloadType string
	SyncStatus     string
	LastError      string
	IsDeleted      bool
}

type ContactField struct {
	ID              string
	UserID          string
	ProfileID       string
	CardID          string
	Kind            string
	Label           string
	Value           string
	NormalizedValue string
	IsPrimary       bool
	Ordinal         int
	Source          string
	Confidence      float64
}
