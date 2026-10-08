package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/runtimeguard"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func managedTestEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("GOFER_AUTH_MODE", "managed")
	t.Setenv("GOFER_ADDR", "127.0.0.1:8090")
	t.Setenv("GOFER_BASE_URL", "http://127.0.0.1:8090")
	t.Setenv("GOFER_SETUP_TOKEN", "")
	t.Setenv("GOFER_ALLOWED_CIDRS", "")
	t.Setenv("GOFER_TRUSTED_PROXY_CIDRS", "")
	t.Setenv("GOFER_ALLOW_UNAUTHENTICATED_REMOTE", "")
	t.Setenv("GOFER_USER_DB_MAX_OPEN", "")
}

func managedRequest(app *managedApplication, method, path, token, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://127.0.0.1:8090"+path, strings.NewReader(body))
	request.Header.Set("Origin", "http://127.0.0.1:8090")
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.AddCookie(&http.Cookie{Name: "gofer_session", Value: token})
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, request)
	return recorder
}

func TestManagedApplicationUsesOwnedHTTPRoutesAndPersistsAcrossRestart(t *testing.T) {
	managedTestEnvironment(t)
	options, _, _ := migrationCommandFixture(t)
	source, err := storage.OpenExisting(options.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Write().Exec(`UPDATE accounts SET email_sync_enabled=0; INSERT INTO users(id,username,username_normalized,user_type,is_admin) VALUES('owner','owner','owner','management',1); UPDATE auth_system_state SET initialized=1,owner_user_id='owner',initialized_at=CURRENT_TIMESTAMP; INSERT INTO app_settings(user_id,key,value) VALUES('alice','ui_settings','{"theme":"dark"}'),('bob','ui_settings','{"theme":"light"}')`); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.MigrateUserStorage(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	app, err := newManagedApplication(t.Context(), options.DestinationPath, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	sessions := make(map[string]string)
	for _, owner := range []string{"alice", "bob", "owner"} {
		session, err := app.auth.CreateAuthenticatedSession(t.Context(), owner, "managed acceptance", auth.AuthenticationMethodTOTP, auth.AssuranceLevelMultiFactor)
		if err != nil {
			t.Fatal(err)
		}
		sessions[owner] = session.Token
	}
	check := func(owner, expected string) {
		t.Helper()
		response := managedRequest(app, "GET", "/api/settings/ui", sessions[owner], "")
		var settings map[string]string
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &settings) != nil || settings["theme"] != expected {
			t.Fatal("owned settings", owner, response.Code, response.Body.String())
		}
	}
	check("alice", "dark")
	check("bob", "light")
	response := managedRequest(app, "PATCH", "/api/settings/ui", sessions["alice"], `{"theme":"system"}`)
	if response.Code != 200 {
		t.Fatal("owned settings write", response.Code, response.Body.String())
	}
	check("bob", "light")
	if response := managedRequest(app, "GET", "/contacts", sessions["bob"], ""); response.Code != 200 || !strings.Contains(response.Body.String(), "Retained contact") {
		t.Fatal("contacts-only owner", response.Code)
	}
	if response := managedRequest(app, "GET", "/contacts", sessions["alice"], ""); response.Code != 200 || strings.Contains(response.Body.String(), "Retained contact") {
		t.Fatal("contact visibility", response.Code)
	}
	for _, path := range []string{"/", "/calendar", "/api/sidebar/mail", "/settings/accounts"} {
		if response := managedRequest(app, "GET", path, sessions["alice"], ""); response.Code != 200 {
			t.Fatal("full route", path, response.Code, response.Body.String())
		}
	}
	if response := managedRequest(app, "GET", "/api/settings/ui", "", ""); response.Code != http.StatusUnauthorized {
		t.Fatal("anonymous access", response.Code)
	}
	if response := managedRequest(app, "GET", "/api/settings/ui", sessions["owner"], ""); response.Code != http.StatusForbidden {
		t.Fatal("management account got mailbox route", response.Code)
	}
	if response := managedRequest(app, "GET", "/admin/users", sessions["owner"], ""); response.Code != 200 {
		t.Fatal("administration route", response.Code)
	}
	if err := storage.VerifyUserStorageRuntimeBoundary(t.Context(), options.DestinationPath); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	if response := managedRequest(app, "GET", "/contacts", sessions["bob"], ""); response.Code != http.StatusServiceUnavailable {
		t.Fatal("closed runtime admitted request", response.Code)
	}
	app, err = newManagedApplication(t.Context(), options.DestinationPath, 1)
	if err != nil {
		t.Fatal(err)
	}
	check("alice", "system")
	check("bob", "light")
	if err := storage.VerifyUserStorageRuntimeBoundary(t.Context(), options.DestinationPath); err != nil {
		t.Fatal(err)
	}
}

type managedStartupWriter struct{ listening chan string }

func (w managedStartupWriter) Write(data []byte) (int, error) {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "listening on ") {
			w.listening <- strings.TrimPrefix(line, "listening on ")
		}
	}
	return len(data), nil
}

func TestManagedServerListensAndReleasesLocksAfterCancellation(t *testing.T) {
	managedTestEnvironment(t)
	t.Setenv("GOFER_USER_DB_MAX_OPEN", "1")
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addressHint := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOFER_ADDR", addressHint)
	t.Setenv("GOFER_SECRET_KEY", "")
	path := filepath.Join(t.TempDir(), "central.db")
	t.Setenv("GOFER_DB_PATH", path)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	listening := make(chan string, 1)
	finished := make(chan error, 1)
	var stderr bytes.Buffer
	go func() { finished <- runManagedServer(ctx, managedStartupWriter{listening}, &stderr) }()
	var address string
	select {
	case address = <-listening:
	case err := <-finished:
		t.Fatal("server did not start", err)
	case <-time.After(20 * time.Second):
		t.Fatal("server startup timed out")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	request, err := http.NewRequest("GET", "http://"+address+"/setup", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "127.0.0.1:8090"
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("actual setup HTTP", response.StatusCode)
	}
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal("server shutdown", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("server workers failed to join")
	}
	if !strings.Contains(stderr.String(), "SETUP REQUIRED") {
		t.Fatal("missing setup notice")
	}
	for _, target := range []string{path, path + ".shared", filepath.Join(path+".users", "manager")} {
		lock, err := runtimeguard.Acquire(target)
		if err != nil {
			t.Fatal("runtime did not release lock", err)
		}
		lock.Close()
	}
	if _, err := os.Stat(path + ".layout.json"); err != nil {
		t.Fatal(err)
	}
}

func TestManagedServerRejectsInvalidDatabaseLimitBeforeCreatingStorage(t *testing.T) {
	for _, value := range []string{"0", "-1", "many", "1.5", "999999999999999999999999999999"} {
		t.Run(value, func(t *testing.T) {
			managedTestEnvironment(t)
			path := filepath.Join(t.TempDir(), "central.db")
			t.Setenv("GOFER_DB_PATH", path)
			t.Setenv("GOFER_USER_DB_MAX_OPEN", value)
			err := runManagedServer(t.Context(), io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "GOFER_USER_DB_MAX_OPEN") {
				t.Fatalf("invalid limit was not rejected: %v", err)
			}
			entries, err := os.ReadDir(filepath.Dir(path))
			if err != nil || len(entries) != 0 {
				t.Fatalf("invalid limit created storage: entries=%v error=%v", entries, err)
			}
		})
	}
}

func TestManagedMainProcessHelper(t *testing.T) {
	if os.Getenv("GOFER_MANAGED_MAIN_PROCESS") != "1" {
		return
	}
	main()
	os.Exit(0)
}

func TestManagedMainProcessServesPopulatedUsersAndJoinsOnSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM acceptance requires a Unix host")
	}
	managedTestEnvironment(t)
	options, _, _ := migrationCommandFixture(t)
	source, err := storage.OpenExisting(options.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Write().Exec(`UPDATE accounts SET email_sync_enabled=0; INSERT INTO users(id,username,username_normalized,user_type,is_admin) VALUES('owner','owner','owner','management',1); UPDATE auth_system_state SET initialized=1,owner_user_id='owner',initialized_at=CURRENT_TIMESTAMP; INSERT INTO app_settings(user_id,key,value) VALUES('alice','ui_settings','{"theme":"dark"}'),('bob','ui_settings','{"theme":"light"}')`); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.MigrateUserStorage(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	app, err := newManagedApplication(t.Context(), options.DestinationPath, 1)
	if err != nil {
		t.Fatal(err)
	}
	tokens := make(map[string]string)
	for _, owner := range []string{"alice", "bob"} {
		session, err := app.auth.CreateAuthenticatedSession(t.Context(), owner, "main process acceptance", auth.AuthenticationMethodTOTP, auth.AssuranceLevelMultiFactor)
		if err != nil {
			app.Close()
			t.Fatal(err)
		}
		tokens[owner] = session.Token
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOFER_ADDR", reservation.Addr().String())
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOFER_DB_PATH", options.DestinationPath)
	t.Setenv("GOFER_MANAGED_MAIN_PROCESS", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestManagedMainProcessHelper$")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = command.Wait(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			command.Process.Kill()
			<-done
		}
	})
	listening := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if line := scanner.Text(); strings.HasPrefix(line, "listening on ") {
				listening <- strings.TrimPrefix(line, "listening on ")
				return
			}
		}
	}()
	var address string
	select {
	case address = <-listening:
	case <-done:
		t.Fatal("main failed before HTTP startup", waitErr)
	case <-time.After(20 * time.Second):
		t.Fatal("main startup timed out")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	for _, owner := range []string{"alice", "bob", "alice", "bob"} {
		request, err := http.NewRequest("GET", "http://"+address+"/api/settings/ui", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Host = "127.0.0.1:8090"
		request.AddCookie(&http.Cookie{Name: "gofer_session", Value: tokens[owner]})
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		var settings map[string]string
		err = json.NewDecoder(response.Body).Decode(&settings)
		response.Body.Close()
		expected := "dark"
		if owner == "bob" {
			expected = "light"
		}
		if err != nil || response.StatusCode != 200 || settings["theme"] != expected {
			t.Fatal("actual main owner routing", owner, response.StatusCode, err)
		}
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		if waitErr != nil {
			t.Fatal("main did not exit cleanly", waitErr)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("main shutdown timed out")
	}
	for _, target := range []string{options.SourcePath, options.DestinationPath, filepath.Join(options.DestinationPath+".users", "manager")} {
		lock, err := runtimeguard.Acquire(target)
		if err != nil {
			t.Fatal("main retained runtime lock", err)
		}
		lock.Close()
	}
}

func TestManagedStartupFailureJoinsServicesAndAllowsRestart(t *testing.T) {
	managedTestEnvironment(t)
	t.Setenv("GOFER_SECRET_KEY", "")
	path := filepath.Join(t.TempDir(), "central.db")
	s, err := openManagedStorage(t.Context(), path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	privatePath := filepath.Join(filepath.Dir(path), "vapid_private.key")
	if err := os.Mkdir(privatePath, 0700); err != nil {
		t.Fatal(err)
	}
	if app, err := newManagedApplication(t.Context(), path, 1); err == nil {
		app.Close()
		t.Fatal("invalid VAPID destination accepted")
	}
	if err := os.Remove(privatePath); err != nil {
		t.Fatal(err)
	}
	app, err := newManagedApplication(t.Context(), path, 1)
	if err != nil {
		t.Fatal("failed startup leaked lifecycle or locks", err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagedCloseCancelsAndJoinsInFlightRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	entered, released := make(chan struct{}), make(chan struct{})
	app := &managedApplication{ctx: ctx, cancel: cancel, handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(released)
	})}
	requestDone := make(chan struct{})
	go func() {
		app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://127.0.0.1/", nil))
		close(requestDone)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not start")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- app.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close did not cancel in-flight request")
	}
	select {
	case <-released:
	default:
		t.Fatal("close returned before request was released")
	}
	<-requestDone
}
