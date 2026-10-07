package notifications

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	netmail "net/mail"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type ownedResponseDAVSubmitAPI struct {
	t        *testing.T
	mu       sync.Mutex
	mode     string
	bodies   map[string]string
	accepted map[string]bool
	puts     map[string]int
	reports  map[string]int
}

func (a *ownedResponseDAVSubmitAPI) wrap(base http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner, password, ok := r.BasicAuth()
		if !ok || (owner != "alice" && owner != "bob") || password != owner+"-calendar-secret" {
			a.t.Error("unowned DAV response credential")
			http.Error(w, "bad authentication", http.StatusUnauthorized)
			return
		}
		collection := "/users/" + owner + "/calendars/primary/"
		resource := collection + "invitation.ics"
		a.mu.Lock()
		body, accepted, mode := a.bodies[owner], a.accepted[owner], a.mode
		a.mu.Unlock()
		if r.Method == "OPTIONS" && r.URL.Path == collection {
			if mode != "email" {
				w.Header().Set("DAV", "calendar-access, calendar-auto-schedule")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == "PROPFIND" {
			principal := "/users/" + owner + "/principal/"
			props := `<d:current-user-principal><d:href>` + principal + `</d:href></d:current-user-principal>`
			if r.URL.Path == principal {
				props = `<c:calendar-user-address-set><d:href>mailto:` + owner + `@example.com</d:href></c:calendar-user-address-set>`
			} else if r.URL.Path != collection {
				a.t.Error("response followed another principal", r.URL.Path)
			}
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:response><d:href>%s</d:href><d:propstat><d:prop>%s</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, r.URL.Path, props)
			return
		}
		if r.Method == "REPORT" && r.URL.Path == collection {
			a.mu.Lock()
			a.reports[owner]++
			a.mu.Unlock()
			var escaped bytes.Buffer
			_ = xml.EscapeText(&escaped, []byte(body))
			etag := `&quot;v1&quot;`
			if accepted {
				etag = `&quot;v2&quot;`
			}
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:response><d:href>%s</d:href><d:propstat><d:prop><d:getetag>%s</d:getetag><c:calendar-data>%s</c:calendar-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, resource, etag, escaped.String())
			return
		}
		if r.URL.Path != resource {
			a.t.Error("response touched another DAV resource", r.Method, r.URL.Path)
			http.Error(w, "bad target", http.StatusBadRequest)
			return
		}
		if r.Method == http.MethodPut {
			a.mu.Lock()
			a.puts[owner]++
			a.mu.Unlock()
			if r.Header.Get("If-Match") != `"v1"` || r.Header.Get("If-Schedule-Tag-Match") != `"s1"` {
				a.t.Error("DAV response lost conditional validators", r.Header)
			}
			data, err := io.ReadAll(r.Body)
			if err != nil {
				a.t.Error(err)
				return
			}
			if !strings.Contains(string(data), "PARTSTAT=ACCEPTED") || !strings.Contains(string(data), owner+"@example.com") || strings.Contains(string(data), "METHOD:REPLY") || (mode == "email") != strings.Contains(string(data), "SCHEDULE-AGENT=CLIENT") {
				a.t.Error("DAV response changed identity or scheduling mode", string(data))
			}
			if mode == "rejected" {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			body = string(data)
			if mode != "email" {
				body = strings.Replace(body, "ORGANIZER:", "ORGANIZER;SCHEDULE-STATUS=2.0:", 1)
			}
			a.mu.Lock()
			a.bodies[owner], a.accepted[owner] = body, true
			a.mu.Unlock()
			if mode == "lost-ack" {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					a.t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if accepted {
			if mode == "readback-404" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("ETag", `"v2"`)
			w.Header().Set("Schedule-Tag", `"s2"`)
			w.Header().Set("Content-Type", "text/calendar")
			_, _ = io.WriteString(w, body)
			return
		}
		rec := httptest.NewRecorder()
		base.ServeHTTP(rec, r)
		a.mu.Lock()
		a.bodies[owner] = rec.Body.String()
		a.mu.Unlock()
		for key, values := range rec.Header() {
			w.Header()[key] = values
		}
		w.Header().Set("Schedule-Tag", `"s1"`)
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
}

func TestUserCalendarResponsePostHTTPDAVServerSchedulingAndUnknownWrites(t *testing.T) {
	for _, mode := range []string{"success", "lost-ack", "readback-404", "rejected"} {
		t.Run(mode, func(t *testing.T) {
			api := &ownedResponseDAVSubmitAPI{t: t, mode: mode, bodies: map[string]string{}, accepted: map[string]bool{}, puts: map[string]int{}, reports: map[string]int{}}
			f, _ := newOwnedCalendarResponseFormFixture(t, "caldav", nil, api.wrap)
			r := ownedResponseSubmit(t, f, "alice", `"v1"`)
			var body map[string]any
			_ = json.Unmarshal(r.Body.Bytes(), &body)
			event, n := ownedResponseState(t, f, "alice")
			if mode == "success" {
				// DAV sync preserves attendee PARTSTAT rather than deriving a
				// mailbox-specific response_status. The native form derives it.
				if r.Code != http.StatusOK || body["responded"] != true || body["refresh_pending"] != false || !strings.Contains(event.AttendeesJSON, `"status":"ACCEPTED"`) || event.ETag != `"v2"` || n != 1 {
					t.Fatal("DAV server response did not refresh through fresh scoped work", r.Code, body, event, n)
				}
				if r := ownedResponseSubmit(t, f, "alice", `"v2"`); r.Code != http.StatusOK {
					t.Fatal("DAV already answered", r.Code, r.Body.String())
				}
			} else {
				want := 1
				if mode == "rejected" {
					want = 0
				}
				if r.Code != http.StatusBadGateway || body["uncertain"] != (want == 1) || event.ResponseStatus != "needsAction" || n != want {
					t.Fatal("DAV write outcome/barrier", r.Code, body, event, n)
				}
				if mode != "rejected" {
					if r := ownedResponseSubmit(t, f, "alice", `"v1"`); r.Code != http.StatusConflict {
						t.Fatal("unknown DAV response resent", r.Code, r.Body.String())
					}
				}
			}
			api.mu.Lock()
			puts := api.puts["alice"]
			api.mu.Unlock()
			if puts != 1 {
				t.Fatal("duplicate DAV response write", puts)
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				var sends int
				if err := db.Read().QueryRow(`SELECT COUNT(*) FROM outgoing_sends`).Scan(&sends); err != nil {
					return err
				}
				if sends != 0 {
					t.Fatal("server scheduling fell back to email after dispatch")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			other, n := ownedResponseState(t, f, "bob")
			if other.ResponseStatus != "needsAction" || n != 0 {
				t.Fatal("DAV response crossed owners", other, n)
			}
		})
	}
}

func TestUserCalendarResponsePostHTTPDAVEmailQueuesOriginalMIMEAndDelivers(t *testing.T) {
	api := &ownedResponseDAVSubmitAPI{t: t, mode: "email", bodies: map[string]string{}, accepted: map[string]bool{}, puts: map[string]int{}, reports: map[string]int{}}
	f, _ := newOwnedCalendarResponseFormFixture(t, "caldav", nil, api.wrap)
	smtp := newRoutedSMTPServer(t)
	host, portText, _ := net.SplitHostPort(smtp.listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	if err := f.system.AddPlaintextTransportException(t.Context(), "smtp", host, port, "alice"); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		if err := f.accountStore.WithAccountForUser(t.Context(), owner, f.accounts[owner].ID, func(_ *config.AccountStore, db *storage.DB) error {
			_, err := db.Write().Exec(`UPDATE accounts SET smtp_host=?,smtp_port=?,smtp_tls_mode='plaintext',email_sync_enabled=0 WHERE id=?`, host, port, f.accounts[owner].ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	r := ownedResponseSubmit(t, f, "alice", `"v1"`)
	var result map[string]any
	_ = json.Unmarshal(r.Body.Bytes(), &result)
	id, _ := result["delivery_id"].(string)
	if r.Code != http.StatusOK || result["pending"] != true || result["delivery"] != "email" || id == "" {
		t.Fatal("DAV email response not durably queued", r.Code, result)
	}
	if r := ownedResponseSubmit(t, f, "alice", `"v1"`); r.Code != http.StatusConflict {
		t.Fatal("queued email reply submitted twice", r.Code, r.Body.String())
	}
	deadline := time.Now().Add(8 * time.Second)
	var job storage.CalendarReplyJob
	for {
		if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
			var err error
			job, err = db.GetCalendarReply(t.Context(), "alice", id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if job.SendStatus == "sent" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native email response did not reach SMTP", job)
		}
		time.Sleep(10 * time.Millisecond)
	}
	smtp.mu.Lock()
	raw := append([]string(nil), smtp.accepted["alice"]...)
	smtp.mu.Unlock()
	if len(raw) != 1 || smtp.count("bob") != 0 {
		t.Fatal("wrong or duplicate native RSVP MIME", raw)
	}
	message, err := netmail.ReadMessage(strings.NewReader(raw[0]))
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	if err != nil || params["boundary"] == "" {
		t.Fatal("invalid reply MIME", err)
	}
	parts := multipart.NewReader(message.Body, params["boundary"])
	var calendarBody string
	for {
		part, err := parts.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(part.Header.Get("Content-Type"), "text/calendar") {
			data, err := io.ReadAll(part)
			if err != nil {
				t.Fatal(err)
			}
			calendarBody = string(data)
		}
		_ = part.Close()
	}
	if !strings.Contains(calendarBody, "METHOD:REPLY") || !strings.Contains(calendarBody, "PARTSTAT=ACCEPTED") || !strings.Contains(calendarBody, "UID:private-uid") || !strings.Contains(calendarBody, "ORGANIZER:mailto:organizer@example.com") || !strings.Contains(calendarBody, "mailto:alice@example.com") || strings.Contains(calendarBody, "bob@example.com") {
		t.Fatal("native SMTP received the wrong decoded reply", calendarBody)
	}
	_, n := ownedResponseState(t, f, "alice")
	if n != 1 {
		t.Fatal("SMTP wake released the response reservation", n)
	}
}
