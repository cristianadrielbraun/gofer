package models

type ContactProfile struct {
	ID              string
	UserID          string
	DisplayName     string
	SortName        string
	PrimaryEmail    string
	AvatarURL       string
	Notes           string
	Origin          string
	SyncEnabled     bool
	IsDeleted       bool
	CreatedAt       string
	UpdatedAt       string
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

type ContactSyncSetup struct {
	Contact        Contact
	Phase          string
	SearchMode     string
	SearchQuery    string
	Locations      []ContactSyncSetupLocation
	ConflictFields []ContactField
}

type ContactSyncSetupLocation struct {
	AccountID  string
	Label      string
	Provider   string
	Candidates []ContactSyncSetupCandidate
	Error      string
}

type ContactSyncSetupCandidate struct {
	Key          string
	RemoteID     string
	ContactID    string
	Name         string
	Email        string
	Phone        string
	Organization string
	MatchEmail   bool
	MatchPhone   bool
	MatchName    bool
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
