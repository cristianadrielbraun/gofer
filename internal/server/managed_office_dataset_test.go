package server

import (
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type officeWorkloadSpec struct {
	Name                    string `json:"name"`
	ActiveUsers             int    `json:"active_users"`
	IdleUsers               int    `json:"idle_users"`
	MessagesPerActiveUser   int    `json:"messages_per_active_user"`
	MessagesPerIdleUser     int    `json:"messages_per_idle_user"`
	ContactsPerUser         int    `json:"contacts_per_user"`
	CalendarEventsPerUser   int    `json:"calendar_events_per_user"`
	Seconds                 int    `json:"seconds"`
	Rate                    int    `json:"arrival_rate_per_second"`
	Workers                 int    `json:"request_workers"`
	Queue                   int    `json:"request_queue_capacity"`
	TimeoutSeconds          int    `json:"request_timeout_seconds"`
	WarmupSeconds           int    `json:"warmup_timeout_seconds"`
	StreamSeed              int64  `json:"stream_seed"`
	Poisson                 bool   `json:"poisson_arrivals"`
	DiagnosticsAfterSeconds int    `json:"diagnostics_after_seconds"`
	CaptureFirstTimeout     bool   `json:"capture_first_timeout"`
	OnlyLayout              string `json:"only_layout,omitempty"`
	Profile                 string `json:"profile"`
	IncomingRate            int    `json:"incoming_messages_per_second"`
	SMTPEveryRequests       int    `json:"smtp_every_requests"`
	DrainSeconds            int    `json:"background_drain_timeout_seconds"`
	PerUserFirst            bool   `json:"per_user_first"`
	UserDBMaxOpen           int    `json:"user_db_max_open"`
	ProviderOutageStart     int    `json:"provider_outage_start_seconds"`
	ProviderOutageSeconds   int    `json:"provider_outage_seconds"`
}

func (s officeWorkloadSpec) validate() error {
	if s.UserDBMaxOpen < 0 || s.UserDBMaxOpen > 4096 {
		return fmt.Errorf("invalid user database cache limit")
	}
	if s.ActiveUsers < 1 || s.IdleUsers < 0 || s.ActiveUsers+s.IdleUsers > 4096 {
		return fmt.Errorf("invalid user counts")
	}
	if s.MessagesPerActiveUser < 1 || s.MessagesPerActiveUser > 20000 || s.MessagesPerIdleUser < 1 || s.MessagesPerIdleUser > 20000 {
		return fmt.Errorf("invalid mailbox sizes")
	}
	if s.ContactsPerUser < 0 || s.ContactsPerUser > 2000 || s.CalendarEventsPerUser < 0 || s.CalendarEventsPerUser > 500 {
		return fmt.Errorf("invalid contacts/calendar sizes")
	}
	if s.Seconds < 1 || s.Seconds > 600 || s.Rate < 1 || s.Rate > 10000 || s.Workers < 1 || s.Workers > 1024 || s.Queue < 1 || s.Queue > 8192 {
		return fmt.Errorf("invalid load bounds")
	}
	if s.TimeoutSeconds < 1 || s.TimeoutSeconds > 60 || s.WarmupSeconds < 1 || s.WarmupSeconds > 900 {
		return fmt.Errorf("invalid timeout bounds")
	}
	if s.DiagnosticsAfterSeconds < 0 || s.DiagnosticsAfterSeconds > s.Seconds || (s.OnlyLayout != "" && s.OnlyLayout != "shared" && s.OnlyLayout != "per-user") {
		return fmt.Errorf("invalid diagnostic/layout selection")
	}
	if s.Profile != "calibration" && s.Profile != "busy-office" {
		return fmt.Errorf("unknown office profile")
	}
	if s.Profile == "busy-office" && (s.ContactsPerUser < 1 || s.CalendarEventsPerUser < 1) {
		return fmt.Errorf("busy-office requires populated contacts and calendar")
	}
	if s.IncomingRate < 0 || s.IncomingRate > 1000 || s.SMTPEveryRequests < 0 || s.DrainSeconds < 0 || s.DrainSeconds > 600 {
		return fmt.Errorf("invalid background workload bounds")
	}
	if (s.IncomingRate > 0 || s.SMTPEveryRequests > 0 || s.DrainSeconds > 0) && s.Profile != "busy-office" {
		return fmt.Errorf("background workload requires busy-office profile")
	}
	if (s.IncomingRate > 0 || s.SMTPEveryRequests > 0) && s.DrainSeconds == 0 {
		return fmt.Errorf("background workload requires bounded drain")
	}
	if s.ProviderOutageStart < 0 || s.ProviderOutageStart > s.Seconds || s.ProviderOutageSeconds < 0 || s.ProviderOutageSeconds > s.Seconds || s.ProviderOutageStart+s.ProviderOutageSeconds > s.Seconds || (s.ProviderOutageSeconds > 0 && s.Profile != "busy-office") {
		return fmt.Errorf("invalid provider outage window")
	}
	return nil
}

type officeDataset struct {
	Active, Idle, All            []string
	Tokens, Accounts, ContactIDs map[string]string
	MessageCounts                map[string]int
	BodyIDs                      map[string]string
	Fixture                      *managedMailFixture
	CAFile                       string
	Provider                     *managedDAVFixture
}

func newOfficeDataset(t *testing.T, spec officeWorkloadSpec) (*officeDataset, string) {
	t.Helper()
	if err := spec.validate(); err != nil {
		t.Fatal(err)
	}
	dataset := &officeDataset{Tokens: make(map[string]string), ContactIDs: make(map[string]string), MessageCounts: make(map[string]int), BodyIDs: make(map[string]string)}
	for index := 0; index < spec.ActiveUsers+spec.IdleUsers; index++ {
		owner := fmt.Sprintf("office%06d", index)
		dataset.All = append(dataset.All, owner)
		if index < spec.ActiveUsers {
			dataset.Active = append(dataset.Active, owner)
			dataset.MessageCounts[owner] = spec.MessagesPerActiveUser
		} else {
			dataset.Idle = append(dataset.Idle, owner)
			dataset.MessageCounts[owner] = spec.MessagesPerIdleUser
		}
	}
	api := &managedDAVFixture{calls: make(map[string]int), users: make(map[string]bool), calendarEvents: spec.CalendarEventsPerUser, calendarAnchor: time.Now()}
	dataset.Provider = api
	for _, owner := range dataset.All {
		api.users[owner] = true
	}
	server := httptest.NewTLSServer(api)
	t.Cleanup(server.Close)
	caRoot := t.TempDir()
	dataset.CAFile = filepath.Join(caRoot, "fixture-ca.pem")
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(dataset.CAFile, certificate, 0600); err != nil {
		t.Fatal(err)
	}
	baselineRoot := t.TempDir()
	baseline := filepath.Join(baselineRoot, "shared.db")
	key := []byte("0123456789abcdef0123456789abcdef")
	fixture := newManagedMailFixtureOptions(t, managedMailOptions{Owners: dataset.All, Messages: spec.MessagesPerActiveUser, MessageCounts: dataset.MessageCounts, SkipApplication: true}, func(db *storage.DB, accounts *config.AccountStore, ids map[string]string) {
		manager := auth.NewManager(auth.LoadConfig("http://127.0.0.1:8090"), db, auth.Dependencies{BucketHashKey: key})
		for _, owner := range dataset.All {
			session, err := manager.CreateAuthenticatedSession(t.Context(), owner, "office workload", auth.AuthenticationMethodTOTP, auth.AssuranceLevelMultiFactor)
			if err != nil {
				t.Fatal(err)
			}
			dataset.Tokens[owner] = session.Token
			for index := 0; index < spec.ContactsPerUser; index++ {
				contact, err := db.SaveContact(t.Context(), owner, models.Contact{Name: fmt.Sprintf("%s office contact %04d", owner, index), Email: fmt.Sprintf("%s-contact%04d@example.test", owner, index), Phone: fmt.Sprintf("+420555%06d", index), Organization: "Example office", Notes: owner + " retained private notes"})
				if err != nil {
					t.Fatal(err)
				}
				if index == 0 {
					dataset.ContactIDs[owner] = contact.ID
				}
			}
			if spec.Profile == "busy-office" {
				if err := accounts.SaveContactSyncConfig(t.Context(), owner, ids[owner], models.ContactSyncConfig{Provider: "carddav", Enabled: true, Username: owner, BaseURL: server.URL, AddressBooks: []models.ContactAddressBook{{ID: owner + "-book", URL: server.URL + "/books/" + owner + "/", Default: true}}}, owner+"-dav-secret"); err != nil {
					t.Fatal(err)
				}
				if err := accounts.SaveCalDAVConfig(t.Context(), owner, ids[owner], server.URL+"/calendars/"+owner+"/", owner, owner+"-dav-secret", false); err != nil {
					t.Fatal(err)
				}
				if err := db.ReplaceCalendarSources(t.Context(), owner, ids[owner], "caldav", []storage.CalendarSource{{ID: owner + "-calendar", RemoteID: server.URL + "/calendars/" + owner + "/", Name: owner + " native calendar", IsPrimary: true, IsSelected: true, AccessRole: "owner"}}); err != nil {
					t.Fatal(err)
				}
			}
		}
		if _, err := db.Write().ExecContext(t.Context(), `VACUUM INTO ?`, baseline); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(baselineRoot, "secret.key"), key, 0600); err != nil {
			t.Fatal(err)
		}
	})
	dataset.Fixture = fixture
	dataset.Accounts = fixture.accounts
	return dataset, baseline
}

func TestManagedOfficePairedWorkload(t *testing.T) {
	encoded := os.Getenv("GOFER_OFFICE_SPEC")
	output := os.Getenv("GOFER_OFFICE_OUTPUT")
	if encoded == "" || output == "" {
		t.Skip("set GOFER_OFFICE_SPEC and GOFER_OFFICE_OUTPUT for populated paired workloads")
	}
	if runtime.GOOS != "linux" {
		t.Skip("process sampling requires Linux")
	}
	var spec officeWorkloadSpec
	if err := json.Unmarshal([]byte(encoded), &spec); err != nil {
		t.Fatal(err)
	}
	dataset, baseline := newOfficeDataset(t, spec)
	ticks := officeClockTicks(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	_, err = io.Copy(hash, binary)
	binary.Close()
	if err != nil {
		t.Fatal(err)
	}
	report := struct {
		Purpose          string              `json:"purpose"`
		GoVersion        string              `json:"go_version"`
		LogicalCPUs      int                 `json:"logical_cpus"`
		GOMAXPROCS       int                 `json:"gomaxprocs"`
		CPUClockTicks    int                 `json:"cpu_clock_ticks_per_second"`
		ExecutableSHA256 string              `json:"executable_sha256"`
		Spec             officeWorkloadSpec  `json:"spec"`
		Results          []officeCalibration `json:"results"`
	}{"Populated paired native IMAP/DAV workload. One paired native workload; capacity conclusions require repeated scenarios, alternating order and fault windows.", runtime.Version(), runtime.NumCPU(), runtime.GOMAXPROCS(0), ticks, fmt.Sprintf("%x", hash.Sum(nil)), spec, nil}
	pairs := []struct{ layout, path string }{{"shared", baseline}, {"per-user", dataset.Fixture.path}}
	if spec.PerUserFirst {
		pairs[0], pairs[1] = pairs[1], pairs[0]
	}
	for _, pair := range pairs {
		if spec.OnlyLayout != "" && spec.OnlyLayout != pair.layout {
			continue
		}
		report.Results = append(report.Results, runOfficeWorkload(t, pair.layout, pair.path, dataset, spec, ticks))
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		temporary := output + ".preparing"
		if err := os.WriteFile(temporary, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temporary, output); err != nil {
			t.Fatal(err)
		}
	}
	results := report.Results
	for _, result := range results {
		t.Logf("%s users=%d active/%d idle offered=%d success=%d errors=%d drops=%d p95=%.1fms cpu=%.2fs peak_rss=%.1fMiB", result.Layout, spec.ActiveUsers, spec.IdleUsers, result.Offered, result.Successful, result.ResponseErrors+result.TransportErrors, result.Dropped, result.P95MS, result.CPUSeconds, float64(result.PeakRSSKiB)/1024)
	}
	for _, result := range results {
		if fault := result.ProviderFault; fault != nil {
			t.Logf("%s provider outage failed_reports=%d recovery_errors=%d", result.Layout, fault.FailedReports, len(fault.RecoveryErrors))
			if len(fault.RecoveryErrors) > 0 {
				t.Errorf("%s provider recovery failed: %v; see %s", result.Layout, fault.RecoveryErrors, output)
			}
		}
		if result.Background != nil {
			b := result.Background
			t.Logf("%s background incoming=%d/%d SMTP=%d/%d drained=%v drain=%.2fs", result.Layout, b.IncomingReceived, len(b.Incoming), b.SMTPReceiptsComplete, b.SMTPReceipts, b.Drained, b.DrainSeconds)
			if b.ObserverError != "" {
				t.Errorf("%s background observer failed: %s; see %s", result.Layout, b.ObserverError, output)
			}
		}
		if result.PayloadErrors != 0 {
			t.Errorf("%s returned %d invalid tenant payloads; see %s", result.Layout, result.PayloadErrors, output)
		}
	}
	// Errors/drops are experimental outcomes, not grounds for suppressing a report.
	// The runner still fails on invalid accounting, tenant payloads, or startup.
}

func officeBusyRequest(operation, revision int, owner string, dataset *officeDataset) (name, method, path, body, contentType string) {
	account := dataset.Accounts[owner]
	switch operation % 16 {
	case 0, 6, 10, 14:
		return officeRequest(0, owner, account)
	case 1, 7, 11:
		return officeRequest(1, owner, account)
	case 2:
		return officeRequest(2, owner, account)
	case 3:
		return officeRequest(3, owner, account)
	case 4:
		return officeRequest(4+revision/16*8, owner, account)
	case 5, 13:
		return officeRequest(5+revision/16*8, owner, account)
	case 8:
		return "mail-body", "GET", "/email/" + dataset.BodyIDs[owner] + "/body", "", ""
	case 9:
		form := url.Values{"name": {owner + " office contact 0000"}, "email": {owner + "-contact0000@example.test"}, "notes": {fmt.Sprintf("%s revision %d", owner, revision)}, "organization": {"Example office"}}
		return "contact-write", "POST", "/api/contacts?id=" + url.QueryEscape(dataset.ContactIDs[owner]), form.Encode(), "application/x-www-form-urlencoded"
	case 12:
		return "calendar-sync", "POST", "/api/calendar/sync", url.Values{"account_id": {account}}.Encode(), "application/x-www-form-urlencoded"
	default:
		return "contacts-sync", "POST", "/api/settings/contacts/accounts/sync", url.Values{"account_id": {account}}.Encode(), "application/x-www-form-urlencoded"
	}
}
