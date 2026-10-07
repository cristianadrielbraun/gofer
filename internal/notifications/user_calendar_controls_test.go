package notifications

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserCalendarControlsHTTPSelectionVisibilityAndEnablement(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	seedUserCalendarViews(t, f)
	alice, bob := f.accounts["alice"].ID, f.accounts["bob"].ID
	if response := f.request("alice", "POST", "/api/calendar/sources/same-calendar-id/visibility", `{"visible":false}`); response.Code != 200 {
		t.Fatal("hide", response.Code, response.Body.String())
	}
	if response := f.request("alice", "POST", "/api/calendar/sources/second-calendar-id/visibility", `{"visible":false}`); response.Code != 404 {
		t.Fatal("unselected visibility", response.Code, response.Body.String())
	}
	for _, body := range []string{`{}`, `{"visible":false,"account_id":"` + bob + `"}`, `{"visible":false} {}`} {
		if response := f.request("alice", "POST", "/api/calendar/sources/same-calendar-id/visibility", body); response.Code != 400 {
			t.Fatal("invalid visibility accepted", response.Code, body)
		}
	}
	form := url.Values{"source_id": {"second-calendar-id"}}
	if response := f.request("alice", "POST", "/api/accounts/"+bob+"/calendar/sources", form.Encode()); response.Code != 404 {
		t.Fatal("foreign source account", response.Code, response.Body.String())
	}
	if response := f.request("alice", "POST", "/api/accounts/"+alice+"/calendar/sources", "source_id=missing"); response.Code != 400 {
		t.Fatal("missing source selection", response.Code, response.Body.String())
	}
	if response := f.request("alice", "POST", "/api/accounts/"+alice+"/calendar/sources", form.Encode()); response.Code != 200 || !strings.Contains(response.Body.String(), "Calendar sources saved.") || strings.Contains(response.Body.String(), "bob secondary") {
		t.Fatal("select calendar", response.Code, response.Body.String())
	}
	for _, enabled := range []string{"false", "true"} {
		if response := f.request("alice", "POST", "/api/accounts/"+alice+"/services", "service=calendar&enabled="+enabled); response.Code != 200 {
			t.Fatal("calendar enablement", enabled, response.Code, response.Body.String())
		}
	}
	for _, owner := range []string{"alice", "bob"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			var hidden, selected, events int
			if err := db.Read().QueryRow(`SELECT is_hidden,is_selected FROM calendar_sources WHERE id='same-calendar-id'`).Scan(&hidden, &selected); err != nil {
				return err
			}
			wantHidden := 0
			if owner == "alice" {
				wantHidden = 1
			}
			if hidden != wantHidden || selected != 1 {
				t.Fatal("calendar preference crossed owners or toggle changed visibility", owner, hidden, selected)
			}
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_events WHERE is_deleted=0`).Scan(&events); err != nil || events != 1 {
				t.Fatal("source control lost cached event", events, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	var revision int
	if err := f.system.Read().QueryRow(`SELECT revision FROM gofer_account_service_schedule WHERE account_id=? AND service='calendar'`, alice).Scan(&revision); err != nil || revision != 3 {
		t.Fatal("calendar scheduling hint", revision, err)
	}
	if _, err := f.system.Write().Exec(`CREATE TRIGGER fail_calendar_wake BEFORE UPDATE ON gofer_account_service_schedule WHEN NEW.service='calendar' BEGIN SELECT RAISE(ABORT,'synthetic wake fault'); END`); err != nil {
		t.Fatal(err)
	}
	if response := f.request("alice", "POST", "/api/accounts/"+alice+"/calendar/sources", ""); response.Code != 200 {
		t.Fatal("wake failure hid committed selection", response.Code, response.Body.String())
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		sources, err := db.ListSelectedCalendarSources(t.Context(), "alice")
		if err == nil && len(sources) != 0 {
			return errors.New("empty selection not committed")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserCalendarControlsHTTPRootShutdownJoinsBlockedWriter(t *testing.T) {
	for _, operation := range []string{"visibility", "selection", "enablement"} {
		t.Run(operation, func(t *testing.T) {
			f := newUserStorageFixtureMode(t, true)
			seedUserCalendarViews(t, f)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := f.routing.WithUser(ctx, "alice", func(db *storage.DB) error {
				tx, err := db.Write().BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				path, body := "/api/calendar/sources/same-calendar-id/visibility", `{"visible":false}`
				switch operation {
				case "selection":
					path, body = "/api/accounts/"+f.accounts["alice"].ID+"/calendar/sources", "source_id=second-calendar-id"
				case "enablement":
					path, body = "/api/accounts/"+f.accounts["alice"].ID+"/service", "service=calendar&enabled=true"
				}
				before := db.Write().Stats().WaitCount
				done := make(chan int, 1)
				go func() { done <- f.request("alice", "POST", path, body).Code }()
				tick := time.NewTicker(time.Millisecond)
				defer tick.Stop()
				for db.Write().Stats().WaitCount == before {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-tick.C:
					}
				}
				f.stopIMAP()
				joined := make(chan struct{})
				go func() { f.imap.Wait(); close(joined) }()
				select {
				case status := <-done:
					if status != 503 {
						t.Fatal("shutdown control reported success", status)
					}
				case <-ctx.Done():
					return ctx.Err()
				}
				select {
				case <-joined:
				case <-ctx.Done():
					return ctx.Err()
				}
				var hidden, primary, secondary int
				if err := tx.QueryRow(`SELECT is_hidden,is_selected FROM calendar_sources WHERE id='same-calendar-id'`).Scan(&hidden, &primary); err != nil {
					return err
				}
				if err := tx.QueryRow(`SELECT is_selected FROM calendar_sources WHERE id='second-calendar-id'`).Scan(&secondary); err != nil {
					return err
				}
				if hidden != 0 || primary != 1 || secondary != 0 {
					t.Fatal("shutdown leaked calendar control", hidden, primary, secondary)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarControlsHTTPEnableRequiresDiscoveredSource(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	if response := f.request("alice", "POST", "/api/accounts/"+f.accounts["alice"].ID+"/service", "service=calendar&enabled=true"); response.Code != 409 || !strings.Contains(response.Body.String(), "Discover at least one calendar") {
		t.Fatal("missing calendar discovery", response.Code, response.Body.String())
	}
}
