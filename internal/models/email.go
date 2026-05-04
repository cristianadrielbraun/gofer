package models

import "html/template"

type Account struct {
	ID       string
	Name     string
	Email    string
	Color    string
	Initials string
	IsActive bool
	Folders  []Folder
}

type Folder struct {
	ID       string
	Name     string
	Icon     string
	Unread   int
	IsSystem bool
	Children []Folder
}

type Email struct {
	ID                 string
	AccountID          string
	FolderID           string
	From               Contact
	To                 []Contact
	CC                 []Contact
	Subject            string
	Preview            string
	Body               template.HTML
	Date               string
	IsRead             bool
	IsStarred          bool
	HasAttachment      bool
	Labels             []Label
	IsSelected         bool
	ThreadCount        int
	Attachments        []Attachment
	InternetMessageID  string
	References         string
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

type ComposeRequest struct {
	AccountID   string `json:"account_id"`
	To          string `json:"to"`
	CC          string `json:"cc"`
	Bcc         string `json:"bcc"`
	Subject     string `json:"subject"`
	Body        string `json:"body"`
	InReplyTo   string `json:"in_reply_to"`
	References  string `json:"references"`
}

type SendResult int

const (
	SendSuccess   SendResult = iota
	SendFailed
	SendAmbiguous
)
