package models

import "html/template"

type Account struct {
	ID         string
	Name       string
	Email      string
	Color      string
	Initials   string
	IsActive   bool
	IsDeleting bool
	Folders    []Folder
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
	From              Contact
	To                []Contact
	CC                []Contact
	BCC               []Contact
	Subject           string
	Preview           string
	Body              template.HTML
	HTMLBody          string
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
	Name     string
	Email    string
	Initials string
}

type Label struct {
	Name  string
	Color string
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
	Emails      []Email
	TotalCount  int
	WindowStart int
	WindowEnd   int
	NextCursor  string
	HasMore     bool
}

type EmailFilters struct {
	Unread      bool
	Starred     bool
	Attachments bool
	Read        bool
	NoAttach    bool
	HasLabels   bool
	ThreadsOnly bool
	From        string
	To          string
	Subject     string
	Body        string
	FromDomain  string
	Attachment  string
	Label       string
	AccountID   string
	Query       string
	After       string
	Before      string
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
