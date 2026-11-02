package views

import (
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func hasAccountTags(accounts []models.Account) bool {
	for _, account := range accounts {
		if !account.IsDeleting && len(account.Labels) > 0 {
			return true
		}
	}
	return false
}

func tagAppearanceAccountName(account models.Account) string {
	if name := strings.TrimSpace(account.Name); name != "" {
		return name
	}
	return account.Email
}
