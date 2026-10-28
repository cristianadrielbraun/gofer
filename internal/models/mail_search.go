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
	Kind               MailSearchScopeKind
	AccountID          string
	FolderIDs          []string
	IncludeDescendants bool
	ExcludeSpamTrash   bool
}
