package views

import (
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
)

// accountIsLocalFolders reports whether the account is Local folders, which
// holds mail but has no mail server to send it with or sync it to.
func accountIsLocalFolders(account models.Account) bool {
	return account.Provider == providers.ProviderLocal
}

// sendingAccounts returns the accounts mail can be sent from: those with
// mail, other than Local folders.
func sendingAccounts(accounts []models.Account) []models.Account {
	out := make([]models.Account, 0, len(accounts))
	for _, account := range mailAccounts(accounts) {
		if !accountIsLocalFolders(account) {
			out = append(out, account)
		}
	}
	return out
}

// connectedAccounts returns the accounts connected to a mail, calendar or
// contacts server, leaving out Local folders.
func connectedAccounts(accounts []models.Account) []models.Account {
	out := make([]models.Account, 0, len(accounts))
	for _, account := range accounts {
		if !accountIsLocalFolders(account) {
			out = append(out, account)
		}
	}
	return out
}

// mailboxSizeWhere says where the mail the size dialog counts is kept.
func mailboxSizeWhere(account models.Account) string {
	if accountIsLocalFolders(account) {
		return "in Local folders"
	}
	return "on the mail server"
}

// accountTitle names an account where its address would show: the address,
// or its name for one that has none, such as Local folders.
func accountTitle(account models.Account) string {
	if account.Email != "" {
		return account.Email
	}
	return account.Name
}

// moveTargetAccounts are the accounts the Move dialog offers: all but a
// hidden one, unless the message is already in it.
func moveTargetAccounts(accounts []models.Account, ownID string) []models.Account {
	out := make([]models.Account, 0, len(accounts))
	for _, account := range accounts {
		if !account.Hidden || account.ID == ownID {
			out = append(out, account)
		}
	}
	return out
}

// localFoldersAccount finds the user's Local folders among their accounts.
func localFoldersAccount(accounts []models.Account) (models.Account, bool) {
	for _, account := range accounts {
		if accountIsLocalFolders(account) && !account.IsDeleting {
			return account, true
		}
	}
	return models.Account{}, false
}

// disabledLocalFolders reports whether the user disabled their Local folders.
func disabledLocalFolders(accounts []models.Account) bool {
	local, ok := localFoldersAccount(accounts)
	return ok && local.Hidden
}
