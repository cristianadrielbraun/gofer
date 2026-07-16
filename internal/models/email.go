package models

import "html/template"

type Account struct {
	ID                  string
	Provider            string
	Name                string
	Email               string
	Color               string
	Initials            string
	IsActive            bool
	IsDeleting          bool
	EmailSyncEnabled    bool
	EmailSyncError      string
	EmailSyncErrorAt    string
	ContactSyncEnabled  bool
	ContactSyncProvider string
	ContactAddressBooks []ContactAddressBook
	Folders             []Folder
	Labels              []Label
}

type Folder struct {
	ID       string
	Name     string
	Icon     string
	Role     string
	Unread   int
	IsSystem bool
	Children []Folder
}

type Email struct {
	ID                string
	AccountID         string
	AccountColor      string
	FolderID          string
	FolderRole        string
	From              Contact
	To                []Contact
	CC                []Contact
	BCC               []Contact
	Subject           string
	Preview           string
	Body              template.HTML
	HTMLBody          string
	OriginalHTMLBody  string
	TextBody          string
	Date              string
	DateFull          string
	IsRead            bool
	IsStarred         bool
	HasAttachment     bool
	Labels            []Label
	IsSelected        bool
	ThreadCount       int
	ThreadID          string
	Attachments       []Attachment
	InternetMessageID string
	InReplyTo         string
	References        string
	IsDraft           bool
}

type Contact struct {
	ID                    string
	Name                  string
	Email                 string
	EmailLabel            string
	AdditionalEmails      []string
	AdditionalEmailLabels []string
	Phone                 string
	PhoneLabel            string
	AdditionalPhones      []string
	AdditionalPhoneLabels []string
	Organization          string
	Title                 string
	Notes                 string
	Initials              string
	Source                string
	IsManual              bool
	IsDeleted             bool
	MessageCount          int
	CreatedAt             string
	LastSeenAt            string
	UpdatedAt             string
	LastSeenSort          string
	UpdatedSort           string
	SaveTargets           []string
	AvatarHash            string
	AvatarStatus          string
	AvatarSource          string
	AvatarURL             string
	AvatarDataURL         string
	RemoveAvatar          bool
	GoferSyncEnabled      bool
	SourceBooks           []ContactAddressBook
	SyncStatus            string
	SyncError             string
	SyncUpdatedAt         string
}

type ContactSyncConfig struct {
	AccountID      string
	UserID         string
	Provider       string
	Enabled        bool
	BaseURL        string
	AddressBookURL string
	AddressBooks   []ContactAddressBook
	Username       string
	HasPassword    bool
	LastSyncToken  string
	LastError      string
	LastSuccessAt  string
	UpdatedAt      string
}

type ContactAddressBook struct {
	ID            string `json:"id,omitempty"`
	AccountID     string `json:"account_id,omitempty"`
	AccountName   string `json:"account_name,omitempty"`
	Name          string `json:"name"`
	URL           string `json:"url"`
	Selected      bool   `json:"selected,omitempty"`
	Default       bool   `json:"default,omitempty"`
	LastSyncToken string `json:"-"`
}

type ContactFilters struct {
	Query      string
	Source     string
	SaveTarget string
	Activity   string
	View       string
	SortBy     string
	SortOrder  string
}

type Label struct {
	ID           string
	AccountID    string
	Name         string
	Color        string
	ProviderID   string
	ProviderType string
}

type Attachment struct {
	ID          int64
	Filename    string
	ContentType string
	SizeBytes   int64
	ContentID   string
	Inline      bool
	StoragePath string
}

type EmailPage struct {
	Emails            []Email
	TotalCount        int
	DisplayTotalCount int
	WindowStart       int
	WindowEnd         int
	NextCursor        string
	HasMore           bool
}

type EmailFilters struct {
	Unread          bool
	Starred         bool
	Attachments     bool
	Read            bool
	NoAttach        bool
	HasTags         bool
	NoTags          bool
	ThreadsOnly     bool
	NoThreads       bool
	Participant     string
	From            string
	To              string
	RecipientType   string
	RecipientDomain string
	Subject         string
	Body            string
	FromDomain      string
	Attachment      string
	AttachmentType  string
	AttachmentExt   string
	MinSizeBytes    int64
	MaxSizeBytes    int64
	Tag             string
	AccountID       string
	TagAccountID    string
	TagProviderID   string
	TagProviderType string
	Query           string
	After           string
	Before          string
	SortBy          string
	SortOrder       string
}

type ThreadItem struct {
	ID                string
	AccountID         string
	AccountColor      string
	From              Contact
	To                []Contact
	CC                []Contact
	Subject           string
	Preview           string
	TextBody          string
	Date              string
	DateFull          string
	IsRead            bool
	IsStarred         bool
	HasAttachment     bool
	FolderName        string
	FolderID          string
	FolderRole        string
	Labels            []Label
	Attachments       []Attachment
	InternetMessageID string
	References        string
}

type ComposeRequest struct {
	AccountID  string `json:"account_id"`
	To         string `json:"to"`
	CC         string `json:"cc"`
	Bcc        string `json:"bcc"`
	Subject    string `json:"subject"`
	Body       string `json:"body"`
	InReplyTo  string `json:"in_reply_to"`
	References string `json:"references"`
}

type SendResult int

const (
	SendSuccess SendResult = iota
	SendFailed
	SendAmbiguous
)
