package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUserAccountErrorAsksToReconnectRevokedAuthorization(t *testing.T) {
	revoked := fmt.Errorf("refresh mailbox authorization: %w", errors.New("oauth token endpoint returned 400 (invalid_grant)"))
	rec := httptest.NewRecorder()
	userAccountError(rec, httptest.NewRequest(http.MethodPost, "/api/messages/1/prefetch-body", nil), revoked)
	if rec.Code != http.StatusConflict || rec.Header().Get(accountReconnectHeader) != "true" || !strings.Contains(rec.Body.String(), "Reconnect account") {
		t.Fatalf("revoked = %d %q header=%q, want 409 reconnect", rec.Code, rec.Body.String(), rec.Header().Get(accountReconnectHeader))
	}

	rec = httptest.NewRecorder()
	userAccountError(rec, httptest.NewRequest(http.MethodPost, "/api/messages/1/prefetch-body", nil), errors.New("dial tcp: operation was canceled"))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get(accountReconnectHeader) != "" {
		t.Fatalf("temporary = %d header=%q, want plain 503", rec.Code, rec.Header().Get(accountReconnectHeader))
	}
}

func TestEmailBodyErrorPostsFailureToReadingPane(t *testing.T) {
	h := &Handler{}
	for _, tc := range []struct {
		name   string
		err    error
		status int
		kind   string
		detail bool
	}{
		{"reconnect", errors.New("refresh mailbox authorization: oauth token endpoint returned 400 (invalid_grant)"), http.StatusConflict, "reconnect", true},
		{"missing", sql.ErrNoRows, http.StatusNotFound, "missing", false},
		{"unavailable", errors.New("connect to imap.example.test:993: i/o timeout"), http.StatusServiceUnavailable, "unavailable", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.writeEmailBodyError(rec, context.Background(), "owner", "42", "", tc.err)
			if rec.Code != tc.status || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
				t.Fatalf("status = %d type=%q, want %d html", rec.Code, rec.Header().Get("Content-Type"), tc.status)
			}
			body := rec.Body.String()
			start, end := strings.Index(body, "parent.postMessage("), strings.LastIndex(body, ",'*')")
			if start < 0 || end < start {
				t.Fatalf("body = %q, want postMessage", body)
			}
			var failure emailBodyFailure
			if err := json.Unmarshal([]byte(body[start+len("parent.postMessage("):end]), &failure); err != nil {
				t.Fatal(err)
			}
			if failure.Type != "emailBodyError" || failure.EmailID != "42" || failure.Kind != tc.kind || failure.Message == "" || (failure.Detail != "") != tc.detail {
				t.Fatalf("failure = %#v", failure)
			}
		})
	}
}

func TestEmailBodyErrorEscapesScriptContent(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Handler{}).writeEmailBodyError(rec, context.Background(), "owner", "42", "", errors.New("</script><script>alert(1)</script>"))
	if strings.Count(rec.Body.String(), "</script>") != 1 {
		t.Fatalf("body = %q, want error text escaped inside the script", rec.Body.String())
	}
}
