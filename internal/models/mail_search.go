package models

type MailSearchScopeKind string

const (
	MailSearchAll     MailSearchScopeKind = "all"
	MailSearchAccount MailSearchScopeKind = "account"
	MailSearchFolders MailSearchScopeKind = "folders"
)

// MailSearchScope describes a search across folders. The current folder/view
// continues to use the ordinary folder list rather than a broad search scope.
type MailSearchScope struct {
	Kind               MailSearchScopeKind `json:"kind"`
	AccountID          string              `json:"account_id,omitempty"`
	FolderIDs          []string            `json:"folder_ids,omitempty"`
	IncludeDescendants bool                `json:"include_descendants,omitempty"`
	ExcludeSpamTrash   bool                `json:"exclude_spam_trash,omitempty"`
}
