package views

import (
	"context"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
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

func mailSearchFolderSelectionLabel(ctx context.Context, scope *models.MailSearchScope) string {
	if scope == nil || scope.Kind != models.MailSearchFolders {
		return i18n.T(ctx, "mail.search.scope.choose_folders")
	}
	label := i18n.N(ctx, "mail.search.scope.folders", len(scope.FolderIDs))
	if scope.IncludeDescendants {
		label = i18n.T(ctx, "mail.search.scope.with_subfolders", "folders", label)
	}
	return label
}

// mailSearchScopeLabel names the scope on the search bar's scope button.
func mailSearchScopeLabel(ctx context.Context, accounts []models.Account, scope *models.MailSearchScope) string {
	if scope == nil {
		return i18n.T(ctx, "mail.search.scope.current")
	}
	switch scope.Kind {
	case models.MailSearchAll:
		return i18n.T(ctx, "mail.search.scope.all")
	case models.MailSearchAccount:
		for _, account := range accounts {
			if account.ID == scope.AccountID {
				return accountTitle(account)
			}
		}
		return i18n.T(ctx, "mail.search.scope.one_account")
	}
	return i18n.N(ctx, "mail.search.scope.folders", len(scope.FolderIDs))
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
