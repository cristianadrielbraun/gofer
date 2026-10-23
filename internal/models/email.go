package models

import (
	"html/template"
	"strings"
)

type Account struct {
	ID                  string
	Provider            string
	MailService         string // "icloud" for an IMAP account on iCloud Mail
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
	CalendarSyncEnabled bool
	// ReconnectPending is set for an account imported from a profile that
	// still needs its password or sign-in.
	ReconnectPending bool
	// ReconnectNeeds are the logins it still waits for: "mail", "contacts",
	// "calendar", or "signin" for Gmail and Outlook.
	ReconnectNeeds      []string
	ContactAddressBooks []ContactAddressBook
	CalendarSources     []AccountCalendarSource
	Folders             []Folder
	Labels              []Label
}

// AccountCalendarSource is sidebar metadata, not account sync configuration.
type AccountCalendarSource struct {
	ID            string
	Name          string
	Color         string
	IsHidden      bool
	SyncState     string
	SyncAttempt   int
	SyncError     string
	LastSyncedAt  string
	NextAttemptAt string
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
	DateWithTime      string
	DateFull          string
	IsRead            bool
	IsStarred         bool
	HasAttachment     bool
	// AttachmentCount counts this message's attachments, not inline images.
	AttachmentCount   int
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
	// TrashedAt is when the contact went to the Trash, and TrashPurgeAt when
	// it is deleted for good from there, formatted for showing.
	TrashedAt        string
	TrashPurgeAt     string
	MessageCount     int
	CreatedAt        string
	LastSeenAt       string
	UpdatedAt        string
	LastSeenSort     string
	UpdatedSort      string
	SaveTargets      []string
	AvatarHash       string
	AvatarStatus     string
	AvatarSource     string
	AvatarURL        string
	AvatarDataURL    string
	RemoveAvatar     bool
	GoferSyncEnabled bool
	SourceBooks      []ContactAddressBook
	SyncStatus       string
	SyncError        string
	SyncUpdatedAt    string
}

// FallbackName is what a contact is called when it has no name: its email
// address, or failing that its organization or phone number. Contacts don't
// need an email address.
func (c Contact) FallbackName() string {
	for _, value := range []string{c.Name, c.Email, c.Organization, c.Phone} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// HasDetails reports whether a contact has something to identify it by: a
// name, email address, phone number or organization.
func (c Contact) HasDetails() bool {
	return c.FallbackName() != ""
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

// CalendarSource describes one discovered provider calendar in account setup.
type CalendarSource struct {
	ID          string
	RemoteID    string
	Name        string
	Description string
	TimeZone    string
	Color       string
	AccessRole  string
	IsPrimary   bool
	IsSelected  bool
}

// CalendarSyncConfig describes the local Calendar source selection for one
// mailbox account. Provider authorization and discovery create the sources;
// this configuration controls whether selected sources appear in Calendar.
type CalendarSyncConfig struct {
	AccountID                   string
	UserID                      string
	Provider                    string
	CalDAVBaseURL               string
	CalDAVUsername              string
	CalDAVHasPassword           bool
	CalDAVUseAccountCredentials bool
	Enabled                     bool
	SourceCount                 int
	SelectedSourceCount         int
	Sources                     []CalendarSource
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

// ContactTrashRetentionDays is how long a contact stays in the Trash before
// it is deleted for good.
const ContactTrashRetentionDays = 30

type ContactFilters struct {
	// Trash lists the contacts in the Trash instead of the others.
	Trash      bool
	Query      string
	WithEmail  bool // only contacts that can be written to, for address suggestions
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
	// ByMessage lists each message on its own row instead of each
	// conversation once. It is how the list is shown, not a filter.
	ByMessage bool
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
	DateWithTime      string
	DateFull          string
	IsRead            bool
	IsStarred         bool
	IsDraft           bool
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
