package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	imap "github.com/emersion/go-imap/v2"
)

// This opt-in subprocess uses the real server paths. The legacy shared builder
// is reachable only here for paired comparisons, never through managed startup.
func TestManagedOfficeBenchmarkProcess(t *testing.T) {
	mode := os.Getenv("GOFER_OFFICE_CHILD")
	if mode == "" {
		t.Skip("benchmark subprocess only")
	}
	if os.Getenv("GOFER_OFFICE_DIAGNOSTICS") == "1" {
		// Linux-only benchmark diagnostics; no production signal behavior changes.
		requests := make(chan os.Signal, 1)
		signal.Notify(requests, syscall.Signal(10))
		t.Cleanup(func() { signal.Stop(requests); close(requests) })
		go func() {
			for range requests {
				pprof.Lookup("goroutine").WriteTo(os.Stdout, 2)
			}
		}()
	}
	if mode == "shared" {
		runServer()
		return
	}
	if mode != "per-user" {
		t.Fatal("unknown office child layout")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runManagedServer(ctx, os.Stdout, os.Stderr); err != nil {
		t.Fatal(err)
	}
}

var errOfficeApplication = errors.New("application error response")

type officeAttempt struct {
	StackRequested   bool    `json:"stack_requested,omitempty"`
	StackSignalError string  `json:"stack_signal_error,omitempty"`
	Owner            string  `json:"owner"`
	SendID           string  `json:"send_id,omitempty"`
	Operation        string  `json:"operation"`
	ScheduledMS      float64 `json:"scheduled_ms"`
	QueueMS          float64 `json:"queue_ms"`
	ElapsedMS        float64 `json:"elapsed_ms"`
	ServiceMS        float64 `json:"service_ms"`
	Status           int     `json:"status"`
	Error            string  `json:"error,omitempty"`
	Dropped          bool    `json:"dropped,omitempty"`
	InvalidPayload   bool    `json:"invalid_payload,omitempty"`
	TransportFailure bool    `json:"transport_failure,omitempty"`
}

type officeResource struct {
	ElapsedMS float64 `json:"elapsed_ms"`
	CPUTicks  uint64  `json:"cpu_ticks"`
	RSSKiB    uint64  `json:"rss_kib"`
}

type officeCalibration struct {
	Background           *officeBackground `json:"background,omitempty"`
	ProcessLog           string            `json:"process_log"`
	PayloadErrors        int               `json:"payload_errors"`
	Layout               string            `json:"layout"`
	IdleUsers            int               `json:"idle_users"`
	WarmupSeconds        float64           `json:"warmup_seconds"`
	IdleWatchers         int               `json:"idle_watchers_at_start"`
	ActiveOwnersWithIdle int               `json:"active_owners_with_idle_at_start"`
	ActiveUsers          int               `json:"active_users"`
	Offered              int               `json:"offered"`
	Responses            int               `json:"responses"`
	Successful           int               `json:"successful"`
	TransportErrors      int               `json:"transport_errors"`
	ResponseErrors       int               `json:"response_errors"`
	Dropped              int               `json:"dropped"`
	ArrivalRate          int               `json:"arrival_rate_per_second"`
	LoadSeconds          float64           `json:"load_seconds"`
	ElapsedSeconds       float64           `json:"elapsed_seconds_including_drain"`
	CPUSeconds           float64           `json:"cpu_seconds"`
	PeakRSSKiB           uint64            `json:"peak_rss_kib_sampled"`
	P50MS                float64           `json:"attempt_latency_p50_ms"`
	P95MS                float64           `json:"attempt_latency_p95_ms"`
	P99MS                float64           `json:"attempt_latency_p99_ms"`
	Attempts             []officeAttempt   `json:"attempts"`
	Resources            []officeResource  `json:"resources"`
}

type officeIncoming struct {
	Owner       string  `json:"owner"`
	Subject     string  `json:"subject"`
	ScheduledMS float64 `json:"scheduled_ms"`
	AppendedMS  float64 `json:"appended_ms"`
	Error       string  `json:"error,omitempty"`
	LocalCopies int     `json:"local_copies_at_drain"`
}

type officeOwnerQueues struct {
	ObservedUnixMS    int64          `json:"observed_unix_ms"`
	NextPollUnixMS    int64          `json:"next_poll_unix_ms,omitempty"`
	IdleWatchers      int            `json:"idle_watchers_at_drain"`
	Owner             string         `json:"owner"`
	Drafts            map[string]int `json:"draft_operations"`
	Sends             map[string]int `json:"outgoing_sends"`
	SentCopies        map[string]int `json:"sent_copies"`
	ContactOperations map[string]int `json:"contact_operations"`
	SMTPAccepted      int            `json:"smtp_accepted"`
	RemoteSent        int            `json:"remote_sent_messages"`
}

type officeBackground struct {
	ObserverError               string              `json:"observer_error,omitempty"`
	Incoming                    []officeIncoming    `json:"incoming"`
	Owners                      []officeOwnerQueues `json:"owners"`
	InitialOwners               []officeOwnerQueues `json:"owners_before_drain"`
	IncomingReceivedBeforeDrain int                 `json:"incoming_received_before_drain"`
	IncomingReceived            int                 `json:"incoming_received"`
	SMTPRequested               int                 `json:"smtp_requests_dispatched"`
	SMTPReceipts                int                 `json:"smtp_receipts"`
	SMTPReceiptsComplete        int                 `json:"smtp_receipts_sent_and_copied"`
	IntegrityErrors             []string            `json:"integrity_errors,omitempty"`
	DrainSeconds                float64             `json:"drain_seconds"`
	DrainCPUSeconds             float64             `json:"drain_cpu_seconds"`
	Drained                     bool                `json:"drained"`
}

// Observers are opened only after the timed HTTP window. They never trigger
// synchronization, warm caches, upgrade schemas or write to application stores.
func officeObserveBackground(ctx context.Context, layout, path string, dataset *officeDataset, incoming []officeIncoming, attempts []officeAttempt) (officeBackground, error) {
	result := officeBackground{Incoming: append([]officeIncoming(nil), incoming...)}
	seenReceipts := make(map[string]bool)
	directory := ""
	var central *sql.DB
	if layout == "per-user" {
		metadata, err := storage.ReadUserStorageLayoutMetadata(path)
		if err != nil {
			return result, err
		}
		directory = metadata.UserDirectory
		central, err = sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(1000)"}).String())
		if err != nil {
			return result, err
		}
		central.SetMaxOpenConns(1)
		defer central.Close()
	}
	for _, owner := range dataset.All {
		file := path
		if directory != "" {
			file = filepath.Join(directory, fmt.Sprintf("%x.db", sha256.Sum256([]byte(owner))))
		}
		db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: file, RawQuery: "mode=ro&_pragma=busy_timeout(1000)"}).String())
		if err != nil {
			return result, err
		}
		db.SetMaxOpenConns(1)
		queues, err := func() (officeOwnerQueues, error) {
			defer db.Close()
			queues := officeOwnerQueues{Owner: owner}
			group := func(query, key string) (map[string]int, error) {
				rows, err := db.QueryContext(ctx, query, key)
				if err != nil {
					return nil, err
				}
				defer rows.Close()
				states := make(map[string]int)
				for rows.Next() {
					var state string
					var count int
					if err := rows.Scan(&state, &count); err != nil {
						return nil, err
					}
					states[state] = count
				}
				return states, rows.Err()
			}
			account := dataset.Accounts[owner]
			for _, query := range []struct {
				sql, key    string
				destination *map[string]int
			}{
				{`SELECT status,count(*) FROM imap_draft_operations WHERE account_id=? GROUP BY status`, account, &queues.Drafts},
				{`SELECT status,count(*) FROM outgoing_sends WHERE account_id=? GROUP BY status`, account, &queues.Sends},
				{`SELECT sent_copy_status,count(*) FROM outgoing_sends WHERE account_id=? GROUP BY sent_copy_status`, account, &queues.SentCopies},
				{`SELECT status,count(*) FROM contact_sync_operations WHERE user_id=? GROUP BY status`, owner, &queues.ContactOperations},
			} {
				states, err := group(query.sql, query.key)
				if err != nil {
					return queues, err
				}
				*query.destination = states
			}
			counts, err := group(`SELECT subject,count(*) FROM messages WHERE account_id=? AND subject LIKE '% office incoming %' GROUP BY subject`, account)
			if err != nil {
				return queues, err
			}
			for subject, count := range counts {
				if !strings.HasPrefix(subject, owner+" office incoming ") {
					result.IntegrityErrors = append(result.IntegrityErrors, "foreign incoming: "+subject)
				}
				if count > 1 {
					result.IntegrityErrors = append(result.IntegrityErrors, "duplicate incoming: "+subject)
				}
			}
			for index := range result.Incoming {
				arrival := &result.Incoming[index]
				if arrival.Owner != owner || arrival.Error != "" {
					continue
				}
				arrival.LocalCopies = counts[arrival.Subject]
				if arrival.LocalCopies == 1 {
					result.IncomingReceived++
				}
			}
			for _, attempt := range attempts {
				if attempt.Owner != owner || attempt.Operation != "smtp-send" || attempt.Dropped {
					continue
				}
				result.SMTPRequested++
				if attempt.SendID == "" {
					continue
				}
				result.SMTPReceipts++
				if seenReceipts[attempt.SendID] {
					result.IntegrityErrors = append(result.IntegrityErrors, "reused send receipt: "+attempt.SendID)
				}
				seenReceipts[attempt.SendID] = true
				var storedAccount, status, copyStatus string
				err := db.QueryRowContext(ctx, `SELECT account_id,status,sent_copy_status FROM outgoing_sends WHERE id=?`, attempt.SendID).Scan(&storedAccount, &status, &copyStatus)
				if errors.Is(err, sql.ErrNoRows) {
					result.IntegrityErrors = append(result.IntegrityErrors, "missing send receipt: "+attempt.SendID)
					continue
				}
				if err != nil {
					return queues, err
				}
				if storedAccount != account {
					result.IntegrityErrors = append(result.IntegrityErrors, "foreign send receipt: "+attempt.SendID)
				}
				if status == "sent" && copyStatus == "complete" {
					result.SMTPReceiptsComplete++
				}
			}
			return queues, nil
		}()
		if err != nil {
			return result, err
		}
		queues.ObservedUnixMS = time.Now().UnixMilli()
		queues.IdleWatchers = dataset.Fixture.activity.idleCount(owner)
		if central != nil {
			err := central.QueryRowContext(ctx, `SELECT COALESCE(p.next_due_ms,0) FROM gofer_account_directory d LEFT JOIN gofer_account_poll_schedule p ON p.account_id=d.account_id WHERE d.account_id=? AND d.user_id=?`, dataset.Accounts[owner], owner).Scan(&queues.NextPollUnixMS)
			if err != nil {
				return result, err
			}
		}
		dataset.Fixture.smtp.mu.Lock()
		queues.SMTPAccepted = len(dataset.Fixture.smtp.accepted[owner])
		seen := make(map[string]bool)
		for _, wire := range dataset.Fixture.smtp.accepted[owner] {
			if seen[wire] {
				result.IntegrityErrors = append(result.IntegrityErrors, "duplicate SMTP delivery: "+owner)
			}
			seen[wire] = true
			for _, match := range officeTenantContent.FindAllStringSubmatch(wire, -1) {
				if match[1] != owner {
					result.IntegrityErrors = append(result.IntegrityErrors, "foreign SMTP content: "+owner)
				}
			}
		}
		dataset.Fixture.smtp.mu.Unlock()
		status, err := dataset.Fixture.remote[owner].Status("Sent", &imap.StatusOptions{NumMessages: true})
		if err != nil || status.NumMessages == nil {
			return result, fmt.Errorf("remote Sent status %s: %v", owner, err)
		}
		queues.RemoteSent = int(*status.NumMessages)
		result.Owners = append(result.Owners, queues)
	}
	result.Drained = result.IncomingReceived == len(incoming) && result.SMTPReceiptsComplete == result.SMTPReceipts && len(result.IntegrityErrors) == 0
	for _, q := range result.Owners {
		if len(q.Drafts) != 0 || q.Sends["pending"]+q.Sends["sending"] != 0 || q.SentCopies["pending"]+q.SentCopies["copying"] != 0 || q.ContactOperations["pending"]+q.ContactOperations["running"] != 0 {
			result.Drained = false
		}
		if q.SMTPAccepted != q.Sends["sent"] || q.RemoteSent != q.SentCopies["complete"] {
			result.Drained = false
		}
		for state, count := range q.Sends {
			if state != "sent" && count != 0 {
				result.Drained = false
			}
		}
		for state, count := range q.SentCopies {
			if state != "complete" && count != 0 {
				result.Drained = false
			}
		}
		for state, count := range q.ContactOperations {
			if state != "done" && count != 0 {
				result.Drained = false
			}
		}
	}
	return result, nil
}

func officeProcessResource(pid int) (officeResource, error) {
	var sample officeResource
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return sample, err
	}
	// comm is parenthesized and can contain spaces; fields after it begin at #3.
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return sample, fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 13 {
		return sample, fmt.Errorf("short process stat")
	}
	user, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return sample, err
	}
	system, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return sample, err
	}
	sample.CPUTicks = user + system
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return sample, err
	}
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			values := strings.Fields(line)
			if len(values) != 3 || values[2] != "kB" {
				return sample, fmt.Errorf("invalid RSS units")
			}
			sample.RSSKiB, err = strconv.ParseUint(values[1], 10, 64)
			return sample, err
		}
	}
	return sample, fmt.Errorf("RSS unavailable")
}

func officeHTTP(client *http.Client, base, token, method, path, body, contentType string) (int, string, error) {
	request, err := http.NewRequest(method, base+path, strings.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	request.Header.Set("Origin", base)
	if method == "POST" && strings.HasPrefix(path, "/api/contacts?") {
		request.Header.Set("Accept", "application/json")
	}
	if strings.HasPrefix(path, "/folder/") {
		request.Header.Set("HX-Request", "true")
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	request.AddCookie(&http.Cookie{Name: "gofer_session", Value: token})
	response, err := client.Do(request)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return response.StatusCode, "", err
	}
	if response.Header.Get("X-Gofer-Status") == "error" {
		return response.StatusCode, string(data), errOfficeApplication
	}
	return response.StatusCode, string(data), nil
}

func officeRequest(index int, owner, account string) (operation, method, path, body, contentType string) {
	switch index % 8 {
	case 0, 6:
		return "mail-list", "GET", "/folder/" + url.PathEscape(storage.FolderIDForIdentity(account, "imap", "INBOX")), "", ""
	case 1, 7:
		return "search", "GET", "/search?q=" + url.QueryEscape(owner), "", ""
	case 2:
		return "contacts", "GET", "/contacts", "", ""
	case 3:
		return "calendar", "GET", "/calendar", "", ""
	case 4:
		theme := "dark"
		if index%16 >= 8 {
			theme = "light"
		}
		return "settings-write", "PATCH", "/api/settings/ui", `{"theme":"` + theme + `"}`, "application/json"
	default:
		form := url.Values{"account_id": {account}, "to": {"recipient@example.test"}, "draft_id": {owner + "-office-draft@example.test"}, "subject": {owner + " office draft"}, "body": {fmt.Sprintf("%s revision %d", owner, index)}}
		return "draft-write", "POST", "/compose/draft", form.Encode(), "application/x-www-form-urlencoded"
	}
}

func runOfficeCalibration(t *testing.T, layout, path string, tokens, accounts map[string]string, activity *managedIMAPActivity, duration time.Duration, rate, ticks int) officeCalibration {
	t.Helper()
	dataset := &officeDataset{Active: []string{"alice", "bob"}, All: []string{"alice", "bob"}, Tokens: tokens, Accounts: accounts, BodyIDs: make(map[string]string), MessageCounts: map[string]int{"alice": 1, "bob": 1}, Fixture: &managedMailFixture{activity: activity}}
	spec := officeWorkloadSpec{Name: "calibration", ActiveUsers: 2, MessagesPerActiveUser: 1, MessagesPerIdleUser: 1, Seconds: int(duration.Seconds()), Rate: rate, Workers: 16, Queue: 32, TimeoutSeconds: 3, WarmupSeconds: 45, StreamSeed: 1, Profile: "calibration"}
	return runOfficeWorkload(t, layout, path, dataset, spec, ticks)
}

var officeTenantContent = regexp.MustCompile(`\b(office[0-9]{6}|alice|bob) (?:native private (?:message|body)|office (?:contact |incoming |send |draft)|native appointment|native friend)`)

var officeEmailID = regexp.MustCompile(`data-email-id="([0-9]+)"`)

func officeClockTicks(t *testing.T) int {
	t.Helper()
	value, err := exec.Command("getconf", "CLK_TCK").Output()
	if err != nil {
		t.Fatal(err)
	}
	ticks, err := strconv.Atoi(strings.TrimSpace(string(value)))
	if err != nil || ticks <= 0 {
		t.Fatal("invalid process CPU clock frequency")
	}
	return ticks
}

func runOfficeWorkload(t *testing.T, layout, path string, dataset *officeDataset, spec officeWorkloadSpec, ticks int) officeCalibration {
	t.Helper()
	if err := spec.validate(); err != nil {
		t.Fatal(err)
	}
	tokens, accounts, activity := dataset.Tokens, dataset.Accounts, dataset.Fixture.activity
	duration, rate := time.Duration(spec.Seconds)*time.Second, spec.Rate
	if spec.Profile == "busy-office" {
		awaitManagedCondition(t, func() bool {
			for _, owner := range dataset.All {
				if activity.idleCount(owner) != 0 {
					return false
				}
			}
			return true
		})
		for _, owner := range dataset.All {
			for _, folder := range []string{"INBOX", "Drafts", "Sent"} {
				if err := dataset.Fixture.remote[owner].Delete(folder); err != nil {
					t.Fatal(err)
				}
				if err := dataset.Fixture.remote[owner].Create(folder, nil); err != nil {
					t.Fatal(err)
				}
			}
			for _, message := range dataset.Fixture.initialMail[owner] {
				if _, err := dataset.Fixture.remote[owner].Append("INBOX", managedLiteral{strings.NewReader(message.wire), int64(len(message.wire))}, &imap.AppendOptions{Flags: message.flags}); err != nil {
					t.Fatal(err)
				}
			}
		}
		dataset.Fixture.smtp.mu.Lock()
		dataset.Fixture.smtp.accepted = make(map[string][]string)
		dataset.Fixture.smtp.mu.Unlock()
	}
	warmupStart := time.Now()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	base := "http://" + address
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestManagedOfficeBenchmarkProcess$", "-test.timeout=30m")
	overrides := map[string]string{"GOFER_OFFICE_CHILD": layout, "GOFER_DB_PATH": path, "GOFER_ADDR": address, "GOFER_BASE_URL": base}
	if spec.DiagnosticsAfterSeconds > 0 || spec.CaptureFirstTimeout {
		overrides["GOFER_OFFICE_DIAGNOSTICS"] = "1"
	}
	if dataset.CAFile != "" {
		overrides["SSL_CERT_FILE"] = dataset.CAFile
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replace := overrides[key]; !replace {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	for key, value := range overrides {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	logPath := path + ".office-process.log"
	if output := os.Getenv("GOFER_OFFICE_OUTPUT"); output != "" {
		logPath = output + "." + layout + ".process.log"
	}
	childLog, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer childLog.Close()
	cmd.Stdout, cmd.Stderr = childLog, childLog
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	defer func() {
		if !stopped {
			cmd.Process.Kill()
			<-done
		}
	}()
	transport := &http.Transport{MaxIdleConns: spec.Workers, MaxIdleConnsPerHost: spec.Workers}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Duration(spec.TimeoutSeconds) * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// Warm all idle owners first, then active owners. Their HTTP setup is outside
	// measurement, while their native background services remain active inside it.
	deadline := time.Now().Add(time.Duration(spec.WarmupSeconds) * time.Second)
	for _, owner := range append(append([]string(nil), dataset.Idle...), dataset.Active...) {
		for {
			folder := storage.FolderIDForIdentity(accounts[owner], "imap", "INBOX")
			status, body, err := officeHTTP(client, base, tokens[owner], "GET", "/folder/"+url.PathEscape(folder), "", "")
			ready := err == nil && status == 200 && strings.Contains(body, owner+" native private message") && strings.Contains(body, fmt.Sprintf(`data-total-count="%d"`, dataset.MessageCounts[owner]))
			if spec.Profile == "calibration" {
				ready = ready && activity.idleCount(owner) >= 3
			}
			if ready {
				match := officeEmailID.FindStringSubmatch(body)
				if len(match) != 2 {
					t.Fatal("warm mailbox has no browser message ID")
				}
				dataset.BodyIDs[owner] = match[1]
				if spec.Profile == "busy-office" {
					for _, check := range []struct{ path, expected string }{{"/contacts", owner + " office contact 0000"}, {"/calendar", owner + " native appointment"}} {
						status, payload, failure := officeHTTP(client, base, tokens[owner], "GET", check.path, "", "")
						if failure != nil || status != 200 || !strings.Contains(payload, check.expected) {
							ready = false
							break
						}
					}
				}
				if ready {
					break
				}
			}
			select {
			case err := <-done:
				stopped = true
				t.Fatalf("%s child exited during warmup: %v", layout, err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s warmup failed for %s: HTTP %d %v", layout, owner, status, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	// Prepare the deterministic arrival plan before starting resource/latency clocks.
	type job struct {
		index          int
		ownerIndex     int
		operationIndex int
		offset         time.Duration
	}
	jobs := make(chan job, spec.Queue)
	plan := make([]job, 0, int(duration.Seconds()*float64(rate)))
	random := rand.New(rand.NewSource(spec.StreamSeed))
	if spec.Poisson {
		arrival := time.Duration(0)
		for index := 0; ; index++ {
			arrival += time.Duration(random.ExpFloat64() * float64(time.Second) / float64(rate))
			if arrival >= duration {
				break
			}
			plan = append(plan, job{index: index, ownerIndex: random.Intn(len(dataset.Active)), operationIndex: random.Intn(16), offset: arrival})
		}
	} else {
		for index := 0; index < int(duration.Seconds()*float64(rate)); index++ {
			plan = append(plan, job{index: index, ownerIndex: index % len(dataset.Active), operationIndex: index / len(dataset.Active), offset: time.Duration(float64(index) * float64(time.Second) / float64(rate))})
		}
	}
	count := len(plan)
	outcomes := make(chan officeAttempt, count)
	startSample, err := officeProcessResource(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	result := officeCalibration{ProcessLog: logPath, Layout: layout, ActiveUsers: len(dataset.Active), IdleUsers: len(dataset.Idle), ArrivalRate: rate, LoadSeconds: duration.Seconds(), WarmupSeconds: time.Since(warmupStart).Seconds()}
	for _, owner := range dataset.All {
		result.IdleWatchers += activity.idleCount(owner)
	}
	for _, owner := range dataset.Active {
		if activity.idleCount(owner) > 0 {
			result.ActiveOwnersWithIdle++
		}
	}
	start := time.Now()
	incomingDone := make(chan []officeIncoming, 1)
	go func() {
		var incoming []officeIncoming
		for index := 0; index < spec.IncomingRate*spec.Seconds; index++ {
			offset := time.Duration(float64(index) * float64(time.Second) / float64(spec.IncomingRate))
			if wait := time.Until(start.Add(offset)); wait > 0 {
				time.Sleep(wait)
			}
			owner := dataset.All[index%len(dataset.All)]
			subject := fmt.Sprintf("%s office incoming %08d", owner, index)
			wire := fmt.Sprintf("From: sender@example.test\r\nTo: %s@example.test\r\nDate: %s\r\nSubject: %s\r\nMessage-ID: <office-arrival-%08d@example.test>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s native private body\r\n", owner, start.Add(offset).Format(time.RFC1123Z), subject, index, owner)
			_, err := dataset.Fixture.remote[owner].Append("INBOX", managedLiteral{strings.NewReader(wire), int64(len(wire))}, &imap.AppendOptions{})
			arrival := officeIncoming{Owner: owner, Subject: subject, ScheduledMS: float64(offset) / float64(time.Millisecond), AppendedMS: float64(time.Since(start)) / float64(time.Millisecond)}
			if err != nil {
				arrival.Error = err.Error()
			}
			incoming = append(incoming, arrival)
		}
		incomingDone <- incoming
	}()
	var diagnosticTimer *time.Timer
	if spec.DiagnosticsAfterSeconds > 0 {
		diagnosticTimer = time.AfterFunc(time.Duration(spec.DiagnosticsAfterSeconds)*time.Second, func() { cmd.Process.Signal(syscall.Signal(10)) })
		defer diagnosticTimer.Stop()
	}
	sampleStop := make(chan struct{})
	sampleDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			sample, err := officeProcessResource(cmd.Process.Pid)
			if err != nil {
				sampleDone <- err
				return
			}
			sample.ElapsedMS = float64(time.Since(start)) / float64(time.Millisecond)
			result.Resources = append(result.Resources, sample)
			select {
			case <-sampleStop:
				sampleDone <- nil
				return
			case <-ticker.C:
			}
		}
	}()

	var workers sync.WaitGroup
	var firstTimeout sync.Once
	for worker := 0; worker < spec.Workers; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for work := range jobs {
				scheduled := start.Add(work.offset)
				owner := dataset.Active[work.ownerIndex]
				operation, method, route, body, contentType := officeRequest(work.operationIndex, owner, accounts[owner])
				if spec.Profile == "busy-office" {
					operation, method, route, body, contentType = officeBusyRequest(work.operationIndex, work.index, owner, dataset)
				}
				if spec.SMTPEveryRequests > 0 && work.index%spec.SMTPEveryRequests == 0 {
					operation, method, route, body, contentType = officeSMTPRequest(owner, accounts[owner], work.index)
				}
				began := time.Now()
				status, payload, err := officeHTTP(client, base, tokens[owner], method, route, body, contentType)
				ended := time.Now()
				outcome := officeAttempt{Owner: owner, Operation: operation, Status: status, ScheduledMS: float64(work.offset) / float64(time.Millisecond), QueueMS: float64(began.Sub(scheduled)) / float64(time.Millisecond), ElapsedMS: float64(ended.Sub(scheduled)) / float64(time.Millisecond), ServiceMS: float64(ended.Sub(began)) / float64(time.Millisecond)}
				if err != nil {
					outcome.TransportFailure = !errors.Is(err, errOfficeApplication)
					outcome.Error = err.Error()
					var timeout net.Error
					if spec.CaptureFirstTimeout && errors.As(err, &timeout) && timeout.Timeout() {
						firstTimeout.Do(func() {
							outcome.StackRequested = true
							if err := cmd.Process.Signal(syscall.Signal(10)); err != nil {
								outcome.StackSignalError = err.Error()
							}
						})
					}
				} else if status != 200 && !(operation == "smtp-send" && status == http.StatusAccepted) {
					outcome.Error = fmt.Sprintf("HTTP %d", status)
				}
				for _, match := range officeTenantContent.FindAllStringSubmatch(payload, -1) {
					if match[1] != owner && outcome.Error == "" {
						outcome.Error = "foreign tenant content"
						outcome.InvalidPayload = true
						break
					}
				}
				if (operation == "mail-list" || operation == "search") && outcome.Error == "" && !officeOwnMailboxContent(payload, owner) {
					outcome.Error = "expected owner mailbox content missing"
					outcome.InvalidPayload = true
				}
				if operation == "mail-body" && outcome.Error == "" && !strings.Contains(payload, owner+" native private body") {
					outcome.Error = "foreign or missing message body"
					outcome.InvalidPayload = true
				}
				if spec.Profile == "busy-office" && (operation == "contacts" || operation == "calendar") && outcome.Error == "" {
					expected := owner + " office contact 0000"
					if operation == "calendar" {
						expected = owner + " native appointment"
					}
					if !strings.Contains(payload, expected) {
						outcome.Error = "expected tenant data missing"
						outcome.InvalidPayload = true
					}
				}
				if operation == "contact-write" && outcome.Error == "" {
					var receipt struct {
						OK bool   `json:"ok"`
						ID string `json:"contact_id"`
					}
					if json.Unmarshal([]byte(payload), &receipt) != nil || !receipt.OK || receipt.ID != dataset.ContactIDs[owner] {
						outcome.Error = "invalid contact receipt"
						outcome.InvalidPayload = true
					}
				}
				if operation == "smtp-send" && outcome.Error == "" {
					var receipt struct {
						Status string `json:"status"`
						ID     string `json:"send_id"`
					}
					if status != http.StatusAccepted || json.Unmarshal([]byte(payload), &receipt) != nil || receipt.Status != "sending" || receipt.ID == "" {
						outcome.Error, outcome.InvalidPayload = "invalid send receipt", true
					} else {
						outcome.SendID = receipt.ID
					}
				}
				if operation == "draft-write" && outcome.Error == "" {
					var receipt map[string]string
					if json.Unmarshal([]byte(payload), &receipt) != nil || receipt["status"] != "saved" || receipt["draft_id"] != owner+"-office-draft@example.test" {
						outcome.Error = "invalid draft receipt"
						outcome.InvalidPayload = true
					}
				}
				outcomes <- outcome
			}
		}()
	}
	// Schedule from intended arrival times independently of response completion.
	// Queue saturation is recorded as a drop, never hidden by slowing arrivals.
	for _, work := range plan {
		if wait := time.Until(start.Add(work.offset)); wait > 0 {
			time.Sleep(wait)
		}
		select {
		case jobs <- work:
		default:
			operation, _, _, _, _ := officeRequest(work.operationIndex, "", "")
			if spec.Profile == "busy-office" {
				operation, _, _, _, _ = officeBusyRequest(work.operationIndex, work.index, dataset.Active[work.ownerIndex], dataset)
			}
			if spec.SMTPEveryRequests > 0 && work.index%spec.SMTPEveryRequests == 0 {
				operation = "smtp-send"
			}
			outcomes <- officeAttempt{Owner: dataset.Active[work.ownerIndex], Operation: operation, ScheduledMS: float64(work.offset) / float64(time.Millisecond), Dropped: true}
		}
	}
	close(jobs)
	workers.Wait()
	if wait := time.Until(start.Add(duration)); wait > 0 {
		time.Sleep(wait)
	}
	incoming := <-incomingDone
	close(outcomes)
	close(sampleStop)
	if err := <-sampleDone; err != nil {
		t.Fatal(err)
	}
	endSample, err := officeProcessResource(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	result.ElapsedSeconds = time.Since(start).Seconds()
	result.CPUSeconds = float64(endSample.CPUTicks-startSample.CPUTicks) / float64(ticks)
	var latencies []float64
	for outcome := range outcomes {
		result.Attempts = append(result.Attempts, outcome)
		if outcome.InvalidPayload {
			result.PayloadErrors++
		}
		result.Offered++
		if outcome.Dropped {
			result.Dropped++
			continue
		}
		latencies = append(latencies, outcome.ElapsedMS)
		if outcome.TransportFailure {
			result.TransportErrors++
		} else {
			result.Responses++
			if outcome.Error != "" {
				result.ResponseErrors++
			}
		}
		if outcome.Error == "" {
			result.Successful++
		}
	}
	for _, sample := range result.Resources {
		result.PeakRSSKiB = max(result.PeakRSSKiB, sample.RSSKiB)
	}
	sort.Float64s(latencies)
	if len(latencies) > 0 {
		percentile := func(p float64) float64 {
			return latencies[max(0, min(len(latencies)-1, int(math.Ceil(float64(len(latencies))*p))-1))]
		}
		result.P50MS, result.P95MS, result.P99MS = percentile(.50), percentile(.95), percentile(.99)
	}
	if result.Offered != count || result.Offered != result.Responses+result.TransportErrors+result.Dropped {
		t.Fatal("load accounting mismatch")
	}
	if spec.DrainSeconds > 0 {
		drainStart := time.Now()
		deadline := drainStart.Add(time.Duration(spec.DrainSeconds) * time.Second)
		var initial []officeOwnerQueues
		initialReceived := 0
		nextProgress := drainStart
		for {
			ctx, cancel := context.WithDeadline(t.Context(), deadline)
			background, err := officeObserveBackground(ctx, layout, path, dataset, incoming, result.Attempts)
			cancel()
			if err != nil {
				if result.Background == nil {
					result.Background = &background
				}
				result.Background.ObserverError, result.Background.Drained = err.Error(), false
				result.Background.DrainSeconds = time.Since(drainStart).Seconds()
				break
			}
			if initial == nil {
				initial = background.Owners
				initialReceived = background.IncomingReceived
			}
			background.InitialOwners, background.IncomingReceivedBeforeDrain = initial, initialReceived
			background.DrainSeconds = time.Since(drainStart).Seconds()
			result.Background = &background
			if time.Now().After(nextProgress) {
				t.Logf("%s background drain incoming=%d/%d SMTP=%d/%d complete=%v elapsed=%.1fs", layout, background.IncomingReceived, len(incoming), background.SMTPReceiptsComplete, background.SMTPReceipts, background.Drained, background.DrainSeconds)
				nextProgress = time.Now().Add(30 * time.Second)
			}
			if background.Drained || time.Now().Add(250*time.Millisecond).After(deadline) {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		drainSample, err := officeProcessResource(cmd.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		result.Background.DrainCPUSeconds = float64(drainSample.CPUTicks-endSample.CPUTicks) / float64(ticks)
		result.PayloadErrors += len(result.Background.IntegrityErrors)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		stopped = true
		if layout == "per-user" && err != nil {
			t.Fatalf("managed child shutdown: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("office child failed to stop")
	}
	return result
}

func TestManagedOfficePairedCalibration(t *testing.T) {
	output := os.Getenv("GOFER_OFFICE_OUTPUT")
	if os.Getenv("GOFER_OFFICE_SPEC") != "" {
		t.Skip("populated workload selected")
	}
	if output == "" {
		t.Skip("set GOFER_OFFICE_OUTPUT to run the paired calibration")
	}
	if runtime.GOOS != "linux" {
		t.Skip("whole-process sampling currently requires Linux /proc")
	}
	seconds := 5
	if value := os.Getenv("GOFER_OFFICE_SECONDS"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 300 {
			t.Fatal("GOFER_OFFICE_SECONDS must be 1..300")
		}
		seconds = parsed
	}
	rate := 40
	if value := os.Getenv("GOFER_OFFICE_RATE"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 2000 {
			t.Fatal("GOFER_OFFICE_RATE must be 1..2000")
		}
		rate = parsed
	}
	tickText, err := exec.Command("getconf", "CLK_TCK").Output()
	if err != nil {
		t.Fatal(err)
	}
	ticks, err := strconv.Atoi(strings.TrimSpace(string(tickText)))
	if err != nil || ticks <= 0 {
		t.Fatal("invalid process CPU clock frequency")
	}
	baselineRoot := t.TempDir()
	baseline := filepath.Join(baselineRoot, "shared.db")
	tokens := make(map[string]string)
	f := newManagedMailFixture(t, func(db *storage.DB, _ *config.AccountStore, _ map[string]string) {
		manager := auth.NewManager(auth.LoadConfig("http://127.0.0.1:8090"), db, auth.Dependencies{BucketHashKey: []byte("0123456789abcdef0123456789abcdef")})
		for _, owner := range []string{"alice", "bob"} {
			session, err := manager.CreateAuthenticatedSession(t.Context(), owner, "office calibration", auth.AuthenticationMethodTOTP, auth.AssuranceLevelMultiFactor)
			if err != nil {
				t.Fatal(err)
			}
			tokens[owner] = session.Token
		}
		if _, err := db.Write().ExecContext(t.Context(), `VACUUM INTO ?`, baseline); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(baselineRoot, "secret.key"), []byte("0123456789abcdef0123456789abcdef"), 0600); err != nil {
			t.Fatal(err)
		}
	})
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	results := make([]officeCalibration, 0, 2)
	for _, pair := range []struct{ layout, path string }{{"shared", baseline}, {"per-user", f.path}} {
		awaitManagedCondition(t, func() bool { return f.activity.idleCount("alice") == 0 && f.activity.idleCount("bob") == 0 })
		for _, owner := range []string{"alice", "bob"} {
			if err := f.remote[owner].Delete("Drafts"); err != nil {
				t.Fatal(err)
			}
			if err := f.remote[owner].Create("Drafts", nil); err != nil {
				t.Fatal(err)
			}
		}
		results = append(results, runOfficeCalibration(t, pair.layout, pair.path, tokens, f.accounts, f.activity, time.Duration(seconds)*time.Second, rate, ticks))
	}
	report := struct {
		Purpose               string              `json:"purpose"`
		PercentileMethod      string              `json:"percentile_method"`
		Timestamp             time.Time           `json:"timestamp"`
		GoVersion             string              `json:"go_version"`
		CPUClockTicks         int                 `json:"cpu_clock_ticks_per_second"`
		Workers               int                 `json:"request_workers"`
		QueueCapacity         int                 `json:"request_queue_capacity"`
		RequestTimeoutSeconds int                 `json:"request_timeout_seconds"`
		Results               []officeCalibration `json:"results"`
	}{"Harness calibration only: two small native mailboxes, contacts/calendar page reads, draft/settings writes. Not a capacity or complete busy-office benchmark.", "nearest-rank over dispatched attempts, including failures; dropped offers counted separately", time.Now().UTC(), runtime.Version(), ticks, 16, 32, 3, results}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		t.Logf("%s offered=%d success=%d response_errors=%d transport_errors=%d drops=%d p95=%.1fms cpu=%.2fs peak_rss=%.1fMiB", result.Layout, result.Offered, result.Successful, result.ResponseErrors, result.TransportErrors, result.Dropped, result.P95MS, result.CPUSeconds, float64(result.PeakRSSKiB)/1024)
		if result.Successful != result.Offered {
			t.Errorf("%s calibration includes unsuccessful work; inspect %s", result.Layout, output)
		}
	}
}

// A received HTTP status is not a completed request if the body is cut short.
// Keep partial network failures distinct from complete application errors.
func TestOfficeHTTPRequiresCompleteResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/application-error" {
			w.Header().Set("X-Gofer-Status", "error")
			io.WriteString(w, "provider unavailable")
			return
		}
		w.Header().Set("Content-Length", "500")
		io.WriteString(w, "partial")
	}))
	defer server.Close()
	status, _, err := officeHTTP(server.Client(), server.URL, "synthetic", "GET", "/partial", "", "")
	if status != http.StatusOK || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("partial HTTP response was counted as complete", status, err)
	}
	status, _, err = officeHTTP(server.Client(), server.URL, "synthetic", "GET", "/application-error", "", "")
	if status != http.StatusOK || !errors.Is(err, errOfficeApplication) {
		t.Fatal("application error was classified as a network failure", status, err)
	}
}

func officeSMTPRequest(owner, account string, revision int) (string, string, string, string, string) {
	form := url.Values{"account_id": {account}, "to": {"recipient@example.test"}, "subject": {fmt.Sprintf("%s office send %08d", owner, revision)}, "body": {fmt.Sprintf("%s native private body revision %d", owner, revision)}}
	return "smtp-send", "POST", "/compose", form.Encode(), "application/x-www-form-urlencoded"
}

func officeOwnMailboxContent(payload, owner string) bool {
	for _, prefix := range []string{" native private message", " office incoming ", " office send ", " office draft"} {
		if strings.Contains(payload, owner+prefix) {
			return true
		}
	}
	return false
}
