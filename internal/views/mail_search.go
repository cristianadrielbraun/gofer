package views

import (
	"encoding/json"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func mailSearchScopeJSON(scope *models.MailSearchScope) string {
	if scope == nil {
		return ""
	}
	value, _ := json.Marshal(scope)
	return string(value)
}
