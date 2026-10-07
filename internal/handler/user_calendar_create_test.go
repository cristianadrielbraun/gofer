package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type userCalendarCreateTestTransport struct {
	base   http.RoundTripper
	target *url.URL
}

func (x userCalendarCreateTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "www.googleapis.com" {
		return x.base.RoundTrip(r)
	}
	copy := r.Clone(r.Context())
	address := *r.URL
	address.Scheme, address.Host = x.target.Scheme, x.target.Host
	address.Path = strings.TrimPrefix(address.Path, "/calendar/v3")
	address.RawPath = strings.TrimPrefix(address.RawPath, "/calendar/v3")
	copy.URL = &address
	return x.base.RoundTrip(copy)
}

func TestUserCalendarCreateNativeProviderReservationPublicationAndRecovery(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		for _, mode := range []string{"confirmed", "lost-ack", "nonce-before-dispatch", "changed-after-dispatch", "cooldown-after-acceptance"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				var mu sync.Mutex
				var creates, writes int
				var saved map[string]any
				var savedDAV string
				var f *userContactPushFixture
				draft := calendarProviderDraft(t, false)
				native := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					owner := "alice"
					endpoint := "/calendars/primary/events"
					if provider == "outlook" {
						endpoint = "/me" + endpoint
					}
					if provider == "caldav" {
						username, password, ok := r.BasicAuth()
						if !ok || username != owner || password != "alice-calendar-secret" {
							t.Error("wrong copied DAV credentials")
							w.WriteHeader(401)
							return
						}
						endpoint = "/alice/primary/" + draft.RequestID + ".ics"
						if r.URL.Path != endpoint {
							t.Error("wrong create resource", r.URL.Path)
							w.WriteHeader(400)
							return
						}
					} else if r.Header.Get("Authorization") != "Bearer alice-access" {
						t.Error("wrong write-purpose OAuth token")
						w.WriteHeader(401)
						return
					}
					if provider != "caldav" && r.URL.Path != endpoint && !strings.HasPrefix(r.URL.Path, endpoint+"/") {
						t.Error("wrong create collection", r.URL.Path)
						w.WriteHeader(400)
						return
					}
					mu.Lock()
					defer mu.Unlock()
					if r.Method == http.MethodPost || r.Method == http.MethodPut {
						writes++
						if provider == "caldav" {
							if r.Header.Get("If-None-Match") != "*" {
								t.Error("unconditional DAV overwrite")
							}
							if savedDAV != "" {
								w.WriteHeader(412)
								return
							}
							raw, _ := io.ReadAll(r.Body)
							savedDAV = string(raw)
							creates++
						} else {
							var payload map[string]any
							if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
								t.Error(err)
								w.WriteHeader(400)
								return
							}
							if provider == "gmail" {
								if payload["id"] != strings.ReplaceAll(draft.RequestID, "-", "") {
									t.Error("Google idempotency identity changed", payload["id"])
								}
								if saved != nil {
									w.WriteHeader(409)
									return
								}
								payload["etag"], payload["iCalUID"] = `"created-v1"`, "private-uid"
							} else {
								if payload["transactionId"] != draft.RequestID {
									t.Error("Graph idempotency identity changed", payload["transactionId"])
								}
								payload["id"], payload["changeKey"], payload["@odata.etag"], payload["iCalUId"], payload["type"] = "created-event", "created-v1", `W/"created-v1"`, "private-uid", "singleInstance"
							}
							if saved == nil {
								creates++
								saved = payload
							}
						}
						if mode == "changed-after-dispatch" {
							if provider == "caldav" {
								if err := f.h.userAccounts.WithUser(t.Context(), owner, func(_ *config.AccountStore, db *storage.DB) error {
									_, err := db.Write().Exec(`UPDATE account_caldav_configs SET username='replaced'`)
									return err
								}); err != nil {
									t.Error(err)
								}
							} else {
								_, err := f.system.Write().Exec(`UPDATE gofer_mailbox_credentials SET revision=revision+1 WHERE account_id=?`, f.accounts[owner].ID)
								if err != nil {
									t.Error(err)
								}
							}
						}
						if mode == "lost-ack" && writes == 1 {
							conn, _, err := w.(http.Hijacker).Hijack()
							if err != nil {
								t.Error(err)
								return
							}
							_ = conn.Close()
							return
						}
					}
					if provider == "caldav" {
						w.Header().Set("ETag", `"created-v1"`)
						if r.Method == http.MethodPut {
							w.WriteHeader(201)
						} else {
							_, _ = io.WriteString(w, savedDAV)
						}
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(saved)
				})
				fixtureProvider := provider
				if provider == "caldav" {
					fixtureProvider = "carddav"
				}
				f = newUserContactPushFixture(t, fixtureProvider, native)
				previousDefault := http.DefaultTransport
				target, _ := url.Parse(f.server.URL)
				http.DefaultTransport = userCalendarCreateTestTransport{base: previousDefault, target: target}
				t.Cleanup(func() { http.DefaultTransport = previousDefault })

				remote, base := "primary", ""
				if provider == "caldav" {
					server := httptest.NewTLSServer(native)
					t.Cleanup(server.Close)
					previous := calDAVHTTPTransport
					calDAVHTTPTransport = server.Client().Transport
					t.Cleanup(func() { calDAVHTTPTransport = previous })
					base = server.URL + "/"
					remote = base + "alice/primary/"
				} else {
					scope, oauth := mailauth.GoogleCalendarEventsScope, "google"
					if provider == "outlook" {
						scope, oauth = "https://graph.microsoft.com/Calendars.ReadWrite", "microsoft"
					}
					expires := time.Now().Add(time.Hour)
					if err := f.h.userCredentials.UpsertForUser(t.Context(), "alice", f.accounts["alice"].ID, oauth, "subject", "alice-access", "alice-refresh", "Bearer", &expires, scope); err != nil {
						t.Fatal(err)
					}
				}
				if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(local *config.AccountStore, db *storage.DB) error {
					if provider == "caldav" {
						if err := local.SaveCalDAVConfig(t.Context(), "alice", f.accounts["alice"].ID, base, "alice", "alice-calendar-secret", false); err != nil {
							return err
						}
					}
					return db.ReplaceCalendarSources(t.Context(), "alice", f.accounts["alice"].ID, provider, []storage.CalendarSource{{ID: "same-source", RemoteID: remote, AccessRole: "owner", IsSelected: true}})
				}); err != nil {
					t.Fatal(err)
				}
				// The owned context must take the real native adapters even if a shared-mode
				// test callback has been installed. Shared DB/account fields are nil here.
				f.h.calendarCreateEvent = func(context.Context, storage.CalendarSource, calendar.EventDraft) (calendar.RemoteEvent, error) {
					t.Error("shared create callback used")
					return calendar.RemoteEvent{}, errors.New("shared fallback")
				}
				prepare := func() (*userCalendarRequest, context.Context) {
					source, err := f.h.userAccounts.SnapshotCalendarSource(t.Context(), "alice", "same-source")
					if err != nil {
						t.Fatal(err)
					}
					p := &userCalendarRequest{h: f.h, source: source}
					ctx, err := p.actionContext(t.Context(), true)
					if err != nil {
						t.Fatal(err)
					}
					claim, err := p.beginCreate(ctx, draft.RequestID, calendarDraftHash(draft))
					if err != nil || claim.Result() != (storage.CalendarCreateRequest{}) {
						t.Fatal("private create reservation", err)
					}
					return p, ctx
				}
				p, ctx := prepare()
				if mode == "nonce-before-dispatch" {
					if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
						_, err := db.Write().Exec(`UPDATE calendar_create_requests SET request_hash='replaced'`)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				created, err := f.h.createCalendarProviderEvent(ctx, p.source.Source(), draft)
				if mode == "lost-ack" {
					if err == nil {
						t.Fatal("lost create acknowledgment reported success")
					}
					p, ctx = prepare()
					created, err = f.h.createCalendarProviderEvent(ctx, p.source.Source(), draft)
				}
				allowed := mode == "confirmed" || mode == "lost-ack" || mode == "cooldown-after-acceptance"
				if (err == nil) != allowed {
					t.Fatal("native create authority/outcome", err)
				}
				if allowed {
					if mode == "cooldown-after-acceptance" {
						if err := f.h.userStorage.DeferProviderRetry(ctx, "alice", f.accounts["alice"].ID, time.Now().Add(time.Hour)); err != nil {
							t.Fatal(err)
						}
					}
					result, err := p.publishCreate(ctx, calendarStorageEvent("forged", "forged", created), false)
					if err != nil || result.EventID == "" {
						t.Fatal("confirmed owned publication", result, err)
					}
					source, err := f.h.userAccounts.SnapshotCalendarSource(t.Context(), "alice", "same-source")
					if err != nil {
						t.Fatal(err)
					}
					guards := []func() error{}
					if provider != "caldav" {
						guards = append(guards, func() error {
							return f.h.userCredentials.ValidateCalendarAuthorization(ctx, p.authorization, "alice", f.accounts["alice"].ID, true)
						})
					}
					replay, err := f.h.userAccounts.BeginCalendarCreate(ctx, source, draft.RequestID, calendarDraftHash(draft), guards...)
					if err != nil || replay.Result() != result {
						t.Fatal("saved create replay lost", err)
					}
				}
				mu.Lock()
				nativeCreates, nativeWrites := creates, writes
				mu.Unlock()
				expectedWrites := 1
				if mode == "lost-ack" {
					expectedWrites = 2
				} else if mode == "nonce-before-dispatch" {
					expectedWrites = 0
				}
				if nativeWrites != expectedWrites || (nativeWrites > 0 && nativeCreates != 1) {
					t.Fatal("native idempotency/replay", nativeWrites, nativeCreates)
				}
				if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
					var n int
					err := db.Read().QueryRow(`SELECT count(*) FROM calendar_events`).Scan(&n)
					want := 0
					if allowed {
						want = 1
					}
					if err == nil && n != want {
						t.Fatal("wrong confirmed cache", n, want)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestUserCalendarMeetingOwnedOrganizerAndSchedulingAgent(t *testing.T) {
	for _, mode := range []string{"server", "client", "smtp-missing", "principal-mismatch", "previous-none", "server-capability-lost"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			var body string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				username, password, ok := r.BasicAuth()
				if !ok || username != "alice" || password != "alice-calendar-secret" {
					t.Error("wrong DAV principal")
					w.WriteHeader(401)
					return
				}
				collection := "/alice/primary/"
				switch r.Method {
				case "OPTIONS":
					if r.URL.Path != collection {
						t.Error("wrong capability collection", r.URL.Path)
					}
					if mode == "server" || mode == "principal-mismatch" {
						w.Header().Set("DAV", "calendar-access, calendar-auto-schedule")
					}
					w.WriteHeader(204)
				case "PROPFIND":
					principal := "/alice/principal/"
					prop := `<d:current-user-principal><d:href>` + principal + `</d:href></d:current-user-principal>`
					if r.URL.Path == principal {
						address := "alice@example.com"
						if mode == "principal-mismatch" {
							address = "different@example.com"
						}
						prop = `<c:calendar-user-address-set><d:href>mailto:` + address + `</d:href></c:calendar-user-address-set>`
					} else if r.URL.Path != collection {
						t.Error("wrong principal discovery", r.URL.Path)
					}
					w.WriteHeader(207)
					_, _ = io.WriteString(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:response><d:href>`+r.URL.Path+`</d:href><d:propstat><d:prop>`+prop+`</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
				case http.MethodPut:
					raw, _ := io.ReadAll(r.Body)
					if r.Header.Get("If-None-Match") != "*" || !strings.Contains(string(raw), "SCHEDULE-AGENT=SERVER") || !strings.Contains(string(raw), "mailto:alice@example.com") || !strings.Contains(string(raw), "mailto:guest@example.com") {
						t.Error("meeting create lost scheduling/sender authority", r.Header, string(raw))
					}
					mu.Lock()
					body = string(raw)
					mu.Unlock()
					w.Header().Set("ETag", `"created"`)
					w.WriteHeader(201)
				case http.MethodGet:
					mu.Lock()
					saved := body
					mu.Unlock()
					w.Header().Set("ETag", `"created"`)
					_, _ = io.WriteString(w, saved)
				default:
					t.Error("unexpected native meeting method", r.Method)
					w.WriteHeader(405)
				}
			}))
			t.Cleanup(server.Close)
			previous := calDAVHTTPTransport
			calDAVHTTPTransport = server.Client().Transport
			t.Cleanup(func() { calDAVHTTPTransport = previous })
			f := newUserContactPushFixture(t, "carddav", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("shared provider fallback") }))
			if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(local *config.AccountStore, db *storage.DB) error {
				if err := local.SaveCalDAVConfig(t.Context(), "alice", f.accounts["alice"].ID, server.URL+"/", "alice", "alice-calendar-secret", false); err != nil {
					return err
				}
				if mode == "smtp-missing" {
					if _, err := db.Write().Exec(`UPDATE accounts SET smtp_host='' WHERE id=?`, f.accounts["alice"].ID); err != nil {
						return err
					}
				}
				return db.ReplaceCalendarSources(t.Context(), "alice", f.accounts["alice"].ID, "caldav", []storage.CalendarSource{{ID: "same-source", RemoteID: server.URL + "/alice/primary/", AccessRole: "owner", IsSelected: true}})
			}); err != nil {
				t.Fatal(err)
			}
			p, ctx := ownedCalendarAction(t, f, "alice", true)
			credentials, err := p.actionCredentials()
			if err != nil {
				t.Fatal(err)
			}
			previousAgent := ""
			if mode == "previous-none" {
				previousAgent = "NONE"
			} else if mode == "server-capability-lost" {
				previousAgent = "SERVER"
			}
			agent, email, name, err := f.h.calendarMeetingAgent(ctx, p.source.Source(), credentials, previousAgent)
			allowed := mode == "server" || mode == "client"
			if (err == nil) != allowed {
				t.Fatal("owned scheduling selection", mode, agent, err)
			}
			if !allowed {
				return
			}
			want := "CLIENT"
			if mode == "server" {
				want = "SERVER"
			}
			if agent != want || email != "alice@example.com" || name != "alice" {
				t.Fatal("organizer not copied from owner", agent, email, name)
			}
			if mode == "server" {
				draft := calendarProviderDraft(t, false)
				draft.Guests = []calendar.GuestDraft{{Email: "guest@example.com"}}
				if _, err := p.beginCreate(ctx, draft.RequestID, calendarDraftHash(draft)); err != nil {
					t.Fatal(err)
				}
				remote, err := f.h.createCalendarProviderEvent(ctx, p.source.Source(), draft)
				if err != nil {
					t.Fatal("owned native scheduled create", err)
				}
				if _, err := p.publishCreate(ctx, calendarStorageEvent("forged", "forged", remote), false); err != nil {
					t.Fatal(err)
				}
				if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
					var n int
					err := db.Read().QueryRow(`SELECT count(*) FROM outgoing_sends`).Scan(&n)
					if err == nil && n != 0 {
						t.Fatal("SERVER scheduling also queued SMTP")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
