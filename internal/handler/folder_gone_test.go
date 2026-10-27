package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A folder page whose folder was deleted on the server sends the browser to
// the inbox instead of a 404.
func TestDeletedFolderPageRedirectsToInbox(t *testing.T) {
	h, _ := newAccountOwnershipTestHandler(t)

	tests := []struct {
		name   string
		path   string
		email  string
		handle http.HandlerFunc
	}{
		{name: "folder", path: "/folder/victim-account:Gone", handle: h.handleFolderPartial},
		{name: "folder full", path: "/folder/victim-account:Gone/full", handle: h.handleFolderFull},
		{name: "folder with email", path: "/folder/victim-account:Gone/101", email: "101", handle: h.handleFolderWithEmail},
		{name: "index", path: "/?folder=victim-account:Gone", handle: h.handleIndex},
	}
	for _, tt := range tests {
		for _, htmx := range []bool{false, true} {
			name := tt.name
			if htmx {
				name += " htmx"
			}
			t.Run(name, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodGet, tt.path, nil)
				if tt.name != "index" {
					req.SetPathValue("id", "victim-account:Gone")
				}
				if tt.email != "" {
					req.SetPathValue("email", tt.email)
				}
				if htmx {
					req.Header.Set("HX-Request", "true")
				}
				rec := httptest.NewRecorder()

				tt.handle(rec, ownerRequest(req))

				if htmx {
					if rec.Code != http.StatusNoContent || rec.Header().Get("HX-Redirect") != "/" {
						t.Fatalf("status = %d HX-Redirect = %q, want 204 to /", rec.Code, rec.Header().Get("HX-Redirect"))
					}
					return
				}
				if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
					t.Fatalf("status = %d Location = %q, want 303 to /", rec.Code, rec.Header().Get("Location"))
				}
			})
		}
	}
}
