package views

import (
	"encoding/json"
	"fmt"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func mailSearchScopeJSON(scope *models.MailSearchScope) string {
	if scope == nil {
		return ""
	}
	value, _ := json.Marshal(scope)
	return string(value)
}

func mailSearchFolderSelectionLabel(scope *models.MailSearchScope) string {
	if scope == nil || scope.Kind != models.MailSearchFolders {
		return "Choose folders…"
	}
	label := fmt.Sprintf("%d folders", len(scope.FolderIDs))
	if len(scope.FolderIDs) == 1 {
		label = "1 folder"
	}
	if scope.IncludeDescendants {
		label += " + subfolders"
	}
	return label
}

// mailSearchScopeLabel names the scope on the search bar's scope button.
func mailSearchScopeLabel(accounts []models.Account, scope *models.MailSearchScope) string {
	if scope == nil {
		return "This folder"
	}
	switch scope.Kind {
	case models.MailSearchAll:
		return "All accounts"
	case models.MailSearchAccount:
		for _, account := range accounts {
			if account.ID == scope.AccountID {
				return account.Email
			}
		}
		return "One account"
	}
	if len(scope.FolderIDs) == 1 {
		return "1 folder"
	}
	return fmt.Sprintf("%d folders", len(scope.FolderIDs))
}

// mailSearchScopeKind names a scope as the scope menu's options do.
func mailSearchScopeKind(scope *models.MailSearchScope) string {
	if scope == nil {
		return "current"
	}
	return string(scope.Kind)
}

// mailSearchShown adds "hidden" to classes when the element isn't shown.
func mailSearchShown(classes string, shown bool) string {
	if shown {
		return classes
	}
	return classes + " hidden"
}

func mailSearchBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func mailSearchFolderIndent(depth int) string {
	return fmt.Sprintf("padding-left: %dpx", 24+min(depth, 8)*16)
}

// MailSearchLocation names the account and complete folder path for a result.
func MailSearchLocation(accounts []models.Account, email models.Email) string {
	var path func([]models.Folder, string) string
	path = func(folders []models.Folder, parent string) string {
		for _, folder := range folders {
			name := folder.Name
			if parent != "" {
				name = parent + " / " + name
			}
			if folder.ID == email.FolderID {
				return name
			}
			if found := path(folder.Children, name); found != "" {
				return found
			}
		}
		return ""
	}
	for _, account := range accounts {
		if account.ID == email.AccountID {
			if folder := path(account.Folders, ""); folder != "" {
				return account.Email + " · " + folder
			}
			return account.Email
		}
	}
	return ""
}
