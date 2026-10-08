package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
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
)

// This opt-in subprocess uses the real server paths. The legacy shared builder
// is reachable only here for paired comparisons, never through managed startup.
func TestManagedOfficeBenchmarkProcess(t *testing.T) {
	mode := os.Getenv("GOFER_OFFICE_CHILD")
	if mode == "" {
		t.Skip("benchmark subprocess only")
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
	Operation        string  `json:"operation"`
	ScheduledMS      float64 `json:"scheduled_ms"`
	QueueMS          float64 `json:"queue_ms"`
	ElapsedMS        float64 `json:"elapsed_ms"`
	ServiceMS        float64 `json:"service_ms"`
	Status           int     `json:"status"`
	Error            string  `json:"error,omitempty"`
	Dropped          bool    `json:"dropped,omitempty"`
	TransportFailure bool    `json:"transport_failure,omitempty"`
}

type officeResource struct {
	ElapsedMS float64 `json:"elapsed_ms"`
	CPUTicks  uint64  `json:"cpu_ticks"`
	RSSKiB    uint64  `json:"rss_kib"`
}

type officeCalibration struct {
	Layout          string           `json:"layout"`
	ActiveUsers     int              `json:"active_users"`
	Offered         int              `json:"offered"`
	Responses       int              `json:"responses"`
	Successful      int              `json:"successful"`
	TransportErrors int              `json:"transport_errors"`
	ResponseErrors  int              `json:"response_errors"`
	Dropped         int              `json:"dropped"`
	ArrivalRate     int              `json:"arrival_rate_per_second"`
	LoadSeconds     float64          `json:"load_seconds"`
	ElapsedSeconds  float64          `json:"elapsed_seconds_including_drain"`
	CPUSeconds      float64          `json:"cpu_seconds"`
	PeakRSSKiB      uint64           `json:"peak_rss_kib_sampled"`
	P50MS           float64          `json:"attempt_latency_p50_ms"`
	P95MS           float64          `json:"attempt_latency_p95_ms"`
	P99MS           float64          `json:"attempt_latency_p99_ms"`
	Attempts        []officeAttempt  `json:"attempts"`
	Resources       []officeResource `json:"resources"`
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
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
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
	cmd := exec.Command(executable, "-test.run=^TestManagedOfficeBenchmarkProcess$", "-test.timeout=10m")
	overrides := map[string]string{"GOFER_OFFICE_CHILD": layout, "GOFER_DB_PATH": path, "GOFER_ADDR": address, "GOFER_BASE_URL": base}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replace := overrides[key]; !replace {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	for key, value := range overrides {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	childLog, err := os.Create(path + ".office-process.log")
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
	transport := &http.Transport{MaxIdleConns: 64, MaxIdleConnsPerHost: 64}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// Warm both real server processes to the same imported remote mailbox state.
	deadline := time.Now().Add(45 * time.Second)
	for _, owner := range []string{"alice", "bob"} {
		for {
			status, body, err := officeHTTP(client, base, tokens[owner], "GET", "/search?q="+owner, "", "")
			if err == nil && status == 200 && strings.Contains(body, owner+" native private message") && activity.idleCount(owner) >= 3 {
				break
			}
			select {
			case err := <-done:
				stopped = true
				t.Fatalf("%s child exited during warmup: %v", layout, err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s warmup failed: HTTP %d %v", layout, status, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	startSample, err := officeProcessResource(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	result := officeCalibration{Layout: layout, ActiveUsers: 2, ArrivalRate: rate, LoadSeconds: duration.Seconds()}
	start := time.Now()
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
	type job struct {
		index     int
		scheduled time.Time
	}
	jobs := make(chan job, 32)
	count := int(duration.Seconds() * float64(rate))
	outcomes := make(chan officeAttempt, count)
	var workers sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for work := range jobs {
				owner := []string{"alice", "bob"}[work.index%2]
				operation, method, route, body, contentType := officeRequest(work.index/2, owner, accounts[owner])
				began := time.Now()
				status, payload, err := officeHTTP(client, base, tokens[owner], method, route, body, contentType)
				ended := time.Now()
				outcome := officeAttempt{Operation: operation, Status: status, ScheduledMS: float64(work.scheduled.Sub(start)) / float64(time.Millisecond), QueueMS: float64(began.Sub(work.scheduled)) / float64(time.Millisecond), ElapsedMS: float64(ended.Sub(work.scheduled)) / float64(time.Millisecond), ServiceMS: float64(ended.Sub(began)) / float64(time.Millisecond)}
				if err != nil {
					outcome.TransportFailure = !errors.Is(err, errOfficeApplication)
					outcome.Error = err.Error()
				} else if status != 200 {
					outcome.Error = fmt.Sprintf("HTTP %d", status)
				}
				other := "alice"
				if owner == "alice" {
					other = "bob"
				}
				if outcome.Error == "" && strings.Contains(payload, other+" native private message") {
					outcome.Error = "foreign mailbox content"
				}
				if (operation == "mail-list" || operation == "search") && outcome.Error == "" && !strings.Contains(payload, owner+" native private message") {
					outcome.Error = "expected owner mailbox content missing"
				}
				if operation == "draft-write" && outcome.Error == "" {
					var receipt map[string]string
					if json.Unmarshal([]byte(payload), &receipt) != nil || receipt["status"] != "saved" || receipt["draft_id"] != owner+"-office-draft@example.test" {
						outcome.Error = "invalid draft receipt"
					}
				}
				outcomes <- outcome
			}
		}()
	}
	// Schedule from intended arrival times independently of response completion.
	// Queue saturation is recorded as a drop, never hidden by slowing arrivals.
	for index := 0; index < count; index++ {
		scheduled := start.Add(time.Duration(float64(index) * float64(time.Second) / float64(rate)))
		if wait := time.Until(scheduled); wait > 0 {
			time.Sleep(wait)
		}
		select {
		case jobs <- job{index, scheduled}:
		default:
			operation, _, _, _, _ := officeRequest(index/2, "", "")
			outcomes <- officeAttempt{Operation: operation, ScheduledMS: float64(scheduled.Sub(start)) / float64(time.Millisecond), Dropped: true}
		}
	}
	close(jobs)
	workers.Wait()
	if wait := time.Until(start.Add(duration)); wait > 0 {
		time.Sleep(wait)
	}
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
