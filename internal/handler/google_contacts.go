package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/providers"
)

type googlePeopleResponse struct {
	Connections   []googlePerson `json:"connections"`
	NextPageToken string         `json:"nextPageToken"`
}

type googlePerson struct {
	Names          []googleName  `json:"names"`
	EmailAddresses []googleEmail `json:"emailAddresses"`
}

type googleName struct {
	DisplayName string `json:"displayName"`
}

type googleEmail struct {
	Value string `json:"value"`
}

type gmailContactSyncAccount struct {
	ID    string
	Email string
}

func (h *Handler) handleSyncGmailContacts(w http.ResponseWriter, r *http.Request) {
	if h.auth == nil || !h.auth.HasGoogleOAuth() {
		htmlStatus(w, http.StatusBadRequest, "Google OAuth is not configured.")
		return
	}
	if err := r.ParseForm(); err != nil {
		htmlStatus(w, http.StatusBadRequest, "Invalid sync request.")
		return
	}

	ctx := r.Context()
	userID := h.userID(ctx)
	accountID := strings.TrimSpace(r.FormValue("account_id"))
	accounts, err := h.gmailContactSyncAccounts(ctx, userID, accountID)
	if err != nil {
		htmlStatus(w, http.StatusInternalServerError, "Could not find Gmail accounts.")
		return
	}
	if len(accounts) == 0 {
		htmlStatus(w, http.StatusBadRequest, "Connect a Gmail account before syncing Gmail contacts.")
		return
	}

	totalImported := 0
	failures := make([]string, 0)
	for _, account := range accounts {
		token, err := h.auth.GetOAuthTokenForAccount(ctx, account.ID)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: reconnect Gmail to grant contact access", account.Email))
			continue
		}

		imported, err := h.syncGooglePeopleConnections(ctx, userID, account.ID, token)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %s", account.Email, err.Error()))
			continue
		}
		totalImported += imported
	}

	if len(failures) == len(accounts) {
		htmlStatus(w, http.StatusBadGateway, "Gmail contact sync failed: "+strings.Join(failures, "; "))
		return
	}

	_ = h.db.LogContactActivity(ctx, userID, "provider_contacts_synced", "", "Gmail contacts synced", totalImported)
	if len(failures) > 0 {
		htmlStatus(w, http.StatusOK, fmt.Sprintf("Gmail contacts partially synced: %d imported or updated. Failed: %s", totalImported, strings.Join(failures, "; ")))
		return
	}
	if len(accounts) == 1 {
		htmlStatus(w, http.StatusOK, fmt.Sprintf("Gmail contacts synced for %s: %d imported or updated.", accounts[0].Email, totalImported))
		return
	}
	htmlStatus(w, http.StatusOK, fmt.Sprintf("Gmail contacts synced across %d accounts: %d imported or updated.", len(accounts), totalImported))
}

func (h *Handler) gmailContactSyncAccounts(ctx context.Context, userID, accountID string) ([]gmailContactSyncAccount, error) {
	if accountID != "" {
		var account gmailContactSyncAccount
		err := h.db.Read().QueryRowContext(ctx, `
		SELECT id, email_address
		FROM accounts
		WHERE id = ? AND user_id = ? AND provider = ? AND COALESCE(is_deleting, 0) = 0`, accountID, userID, providers.ProviderGmail).Scan(&account.ID, &account.Email)
		if err == sql.ErrNoRows {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return []gmailContactSyncAccount{account}, nil
	}

	rows, err := h.db.Read().QueryContext(ctx, `
		SELECT id, email_address
		FROM accounts
		WHERE user_id = ? AND provider = ? AND COALESCE(is_deleting, 0) = 0
		ORDER BY email_address COLLATE NOCASE`, userID, providers.ProviderGmail)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []gmailContactSyncAccount
	for rows.Next() {
		var account gmailContactSyncAccount
		if err := rows.Scan(&account.ID, &account.Email); err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

func (h *Handler) syncGooglePeopleConnections(ctx context.Context, userID, accountID, accessToken string) (int, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	pageToken := ""
	imported := 0

	for {
		values := url.Values{}
		values.Set("personFields", "names,emailAddresses")
		values.Set("pageSize", "1000")
		if pageToken != "" {
			values.Set("pageToken", pageToken)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://people.googleapis.com/v1/people/me/connections?"+values.Encode(), nil)
		if err != nil {
			return imported, err
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)

		resp, err := client.Do(req)
		if err != nil {
			return imported, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return imported, fmt.Errorf("people api returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var people googlePeopleResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&people)
		resp.Body.Close()
		if decodeErr != nil {
			return imported, decodeErr
		}

		for _, person := range people.Connections {
			name := googlePersonName(person)
			for _, email := range person.EmailAddresses {
				value := strings.TrimSpace(email.Value)
				if value == "" {
					continue
				}
				if _, err := h.db.UpsertSyncedContact(ctx, userID, accountID, name, value); err != nil {
					return imported, err
				}
				imported++
			}
		}

		pageToken = people.NextPageToken
		if pageToken == "" {
			break
		}
	}
	return imported, nil
}

func googlePersonName(person googlePerson) string {
	for _, name := range person.Names {
		if strings.TrimSpace(name.DisplayName) != "" {
			return strings.TrimSpace(name.DisplayName)
		}
	}
	return ""
}

func htmlStatus(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if status >= 400 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`<div class="rounded-md border border-border bg-background px-3 py-2 text-xs text-muted-foreground">` + html.EscapeString(message) + `</div>`))
}
