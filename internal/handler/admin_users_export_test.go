package handler

import (
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
)

func TestAdminUsersExportDownloadsTheListAsCSV(t *testing.T) {
	manager, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	addBulkPeople(t, db, "active", "export-cedar", "export-amber")
	addBulkPeople(t, db, "disabled", "export-birch")
	get := func(path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.AddCookie(sessionCookie)
		response := httptest.NewRecorder()
		stack.ServeHTTP(response, request)
		return response
	}

	// The button downloads the list as its filters keep it.
	page := get("/admin/users?q=export&status=active")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `href="/admin/users/export?q=export&amp;status=active"`) ||
		!strings.Contains(page.Body.String(), "data-admin-users-export") {
		t.Fatalf("Users page export button = %d %q", page.Code, page.Body.String())
	}

	response := get("/admin/users/export?q=export&status=active&page=3")
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/csv; charset=utf-8" ||
		!strings.HasPrefix(response.Header().Get("Content-Disposition"), `attachment; filename="gofer-users-`) {
		t.Fatalf("export = %d %v %q", response.Code, response.Header(), response.Body.String())
	}
	rows, err := csv.NewReader(strings.NewReader(response.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	// Every matching person in the list's order, not only the page asked for.
	if len(rows) != 3 || rows[0][0] != "username" || rows[0][2] != "status" ||
		rows[1][0] != "export-amber" || rows[2][0] != "export-cedar" || rows[1][2] != "active" || rows[1][4] != "webmail_user" {
		t.Fatalf("export rows = %q", rows)
	}
	for _, row := range rows[1:] {
		if len(row) != len(adminUsersCSVHeader) {
			t.Fatalf("row %q has %d columns, want %d", row, len(row), len(adminUsersCSVHeader))
		}
		if _, err := time.Parse(time.RFC3339, row[13]); err != nil {
			t.Fatalf("created_at %q: %v", row[13], err)
		}
	}
	exported, err := audit.List(t.Context(), db.Read(), audit.Filter{Action: "user.list_exported"}, 0, 10)
	if err != nil || len(exported.Records) != 1 || exported.Records[0].Metadata["count"] != float64(2) ||
		exported.Records[0].Metadata["status"] != "active" || exported.Records[0].Metadata["search"] != "export" {
		t.Fatalf("export record = %+v, %v", exported.Records, err)
	}

	// A session that must verify again goes back to the same list, which
	// asks for it and returns there.
	session, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value)
	if err != nil || session == nil {
		t.Fatalf("administrator session = %#v, %v", session, err)
	}
	if _, err := db.Write().ExecContext(t.Context(), `UPDATE sessions SET step_up_at = ? WHERE id = ?`, time.Now().UTC().Add(-11*time.Minute), session.ID); err != nil {
		t.Fatal(err)
	}
	stale := get("/admin/users/export?q=export&status=active")
	if stale.Code != http.StatusSeeOther || stale.Header().Get("Location") != "/admin/users?q=export&status=active&verification_required=1" {
		t.Fatalf("stale export = %d %q %q", stale.Code, stale.Header().Get("Location"), stale.Body.String())
	}
	verify := get(stale.Header().Get("Location"))
	if verify.Code != http.StatusOK || !strings.Contains(verify.Body.String(), "admin-security-verification-dialog") ||
		!strings.Contains(verify.Body.String(), `name="return_to" value="/admin/users?q=export&amp;status=active"`) {
		t.Fatalf("verification after a stale export = %d %q", verify.Code, verify.Body.String())
	}
	if again, _ := audit.List(t.Context(), db.Read(), audit.Filter{Action: "user.list_exported"}, 0, 10); len(again.Records) != 1 {
		t.Fatalf("a refused export was recorded: %+v", again.Records)
	}
}
