package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/handler"
	"github.com/cristianadrielbraun/gofer/internal/httpguard"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/notifications"
	"github.com/cristianadrielbraun/gofer/internal/store"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

type managedApplication struct {
	ctx           context.Context
	storage       *managedStorage
	auth          *auth.Manager
	flows         *mailauth.Service
	credentials   *mailauth.UserCredentials
	imap          *mail.UserIMAP
	mailbox       *handler.Handler
	notifications *notifications.Service
	httpConfig    *httpguard.Config
	handler       http.Handler
	setupNotice   bytes.Buffer
	cancel        context.CancelFunc
	requests      sync.WaitGroup
	requestMu     sync.Mutex
	closing       bool
}

func newManagedApplication(parent context.Context, path string, maxOpen int) (result *managedApplication, err error) {
	ctx, cancel := context.WithCancel(parent)
	app := &managedApplication{ctx: ctx, cancel: cancel}
	defer func() {
		if err != nil {
			err = errors.Join(err, app.Close())
		}
	}()
	app.httpConfig, err = httpguard.LoadConfig()
	if err != nil {
		return nil, err
	}
	authConfig := auth.LoadConfig(app.httpConfig.BaseURL)
	if err := authConfig.ValidateMode(); err != nil {
		return nil, err
	}
	if authConfig.AuthenticationMode() != auth.ModeManaged {
		return nil, errors.New("per-user application requires managed authentication")
	}
	authConfig.SecureCookies = app.httpConfig.SecureCookies()
	if err := app.httpConfig.ValidateExposure(authConfig.Enabled); err != nil {
		return nil, err
	}
	if app.httpConfig.WarnInsecureLAN(authConfig.Enabled) {
		log.Printf("WARNING: GOFER_ALLOW_INSECURE_LAN is enabled; sign-in cookies for %s travel unencrypted on the local network", app.httpConfig.BaseURL)
	}
	if app.httpConfig.WarnOutlookWebhooksBehindAllowlist() {
		log.Printf("WARNING: GOFER_ALLOWED_CIDRS applies to %s, so Outlook notifications only arrive if it lists Microsoft's servers; set GOFER_OUTLOOK_WEBHOOKS_SKIP_ALLOWED_CIDRS=true to let them through", httpguard.OutlookWebhookPath)
	}
	app.storage, err = openManagedStorage(ctx, path, maxOpen)
	if err != nil {
		return nil, err
	}
	s := app.storage
	accounts, err := config.NewUserAccountStore(s.routing, s.key)
	if err != nil {
		return nil, err
	}
	if _, err := accounts.RecoverPendingAccountCreations(ctx); err != nil {
		return nil, err
	}
	centralAccounts, err := config.NewAccountStore(s.central, s.key)
	if err != nil {
		return nil, err
	}
	if err := centralAccounts.SecurePasswords(ctx); err != nil {
		return nil, err
	}
	app.auth = auth.NewManager(authConfig, s.central, auth.Dependencies{BucketHashKey: s.key})
	if adopted, err := app.auth.AdoptLocalProfileForManagedSetup(ctx); err != nil {
		return nil, err
	} else if adopted {
		log.Printf("auth: the personal profile is now a regular user; managed setup will create an administrator")
	}
	if err := app.auth.ValidateRuntimeMode(ctx); err != nil {
		return nil, err
	}
	if err := app.auth.SetUserStorage(s.routing); err != nil {
		return nil, err
	}
	if err := provisionInitialSetupToken(ctx, app.auth, authConfig.SetupToken, &app.setupNotice); err != nil {
		return nil, err
	}
	mailConfig := mailauth.LoadConfig(app.httpConfig.BaseURL, authConfig.Enabled)
	app.flows = mailauth.New(mailConfig, s.central, s.key)
	app.credentials, err = mailauth.NewUserCredentials(ctx, mailConfig, s.routing, s.key)
	if err != nil {
		return nil, err
	}
	blobs := store.NewBlobStore(s.layout.BlobDirectory)
	// The orchestrator supplies the common event bus to existing handlers. Only
	// the routed IMAP service starts mailbox work in managed mode.
	syncer := mail.NewSyncOrchestrator(s.central, centralAccounts, blobs, app.flows)
	app.imap, err = mail.NewUserIMAP(ctx, accounts, blobs, syncer.Events())
	if err != nil {
		return nil, err
	}
	app.imap.EnableOutlookWebhooks(app.httpConfig.OutlookWebhookURL(), s.key)
	// Keep the databases of people with Gofer open when room is needed.
	s.stores.SetRetainHint(app.imap.OwnerOnline)
	dataDir := filepath.Dir(s.layout.CentralPath)
	publicKey, privateKey, err := loadVAPIDKeys(filepath.Join(dataDir, "vapid_private.key"), filepath.Join(dataDir, "vapid_public.key"))
	if err != nil {
		return nil, err
	}
	subject := os.Getenv("GOFER_VAPID_SUBJECT")
	if subject == "" {
		subject = "mailto:gofer@gofer.email"
	}
	app.notifications, err = notifications.NewWithUserStorage(s.routing, syncer.Events(), publicKey, privateKey, subject)
	if err != nil {
		return nil, err
	}
	app.mailbox = handler.New(s.central, centralAccounts, syncer, blobs, app.auth, publicKey, app.flows)
	app.mailbox.SetTestPushSender(app.notifications.SendTestPush)
	mux := http.NewServeMux()
	if err := app.mailbox.RegisterUserStorageRoutes(ctx, mux, s.routing, handler.UserStorageOptions{
		Accounts: accounts, IMAP: app.imap, Credentials: app.credentials,
		ContactSync: &handler.UserContactSyncOptions{}, CalendarSync: &handler.UserCalendarSyncOptions{},
	}); err != nil {
		return nil, err
	}
	app.mailbox.RegisterAuthenticationRoutes(mux)
	app.mailbox.RegisterAdministrationRoutes(mux)
	if app.httpConfig.OutlookWebhooks {
		mux.Handle("POST "+httpguard.OutlookWebhookPath, mail.OutlookWebhookHandler(app.imap))
	}
	if err := app.mailbox.StartUserThreading(ctx); err != nil {
		return nil, err
	}
	if err := app.mailbox.AwaitUserThreading(ctx); err != nil {
		return nil, err
	}
	syncOptions, err := configuredSyncCapacity()
	if err != nil {
		return nil, err
	}
	if err := app.imap.Start(syncOptions); err != nil {
		return nil, err
	}
	if err := app.mailbox.StartUserMailRetention(ctx, handler.UserMailRetentionOptions{}); err != nil {
		return nil, err
	}
	app.mailbox.StartAuthenticationEventRetentionWorker(ctx)
	app.auth.StartSessionCleanup(ctx)
	app.flows.StartCleanup(ctx)
	app.notifications.Start(ctx)
	app.handler = app.httpConfig.ClientNetworkMiddleware(authConfig.Enabled, app.httpConfig.Middleware(handler.CompressResponses(app.auth.Middleware(views.WithListViewCookie(mux)))))
	return app, nil
}

func (app *managedApplication) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	app.requestMu.Lock()
	if app.closing {
		app.requestMu.Unlock()
		http.Error(w, "Gofer is shutting down", http.StatusServiceUnavailable)
		return
	}
	app.requests.Add(1)
	app.requestMu.Unlock()
	defer app.requests.Done()
	requestCtx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(app.ctx, cancel)
	defer stop()
	defer cancel()
	app.handler.ServeHTTP(w, r.WithContext(requestCtx))
}

func (app *managedApplication) Close() error {
	app.requestMu.Lock()
	app.closing = true
	app.requestMu.Unlock()
	app.cancel()
	app.requests.Wait()
	if app.imap != nil {
		app.imap.Wait()
	}
	if app.credentials != nil {
		app.credentials.Wait()
	}
	if app.mailbox != nil {
		app.mailbox.WaitUserThreading()
		app.mailbox.WaitUserContactBackfills()
		app.mailbox.WaitUserDeletions()
		app.mailbox.WaitUserMailRetention()
		app.mailbox.WaitAvatarWorkers()
		app.mailbox.WaitAuthenticationEventRetention()
	}
	if app.auth != nil {
		app.auth.WaitSessionCleanup()
	}
	if app.flows != nil {
		app.flows.WaitCleanup()
	}
	if app.notifications != nil {
		app.notifications.Wait()
	}
	return app.storage.Close()
}

func configuredUserDatabaseLimit() (int, error) {
	value := os.Getenv("GOFER_USER_DB_MAX_OPEN")
	if value == "" {
		return 0, nil // Use the storage manager's default.
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 {
		return 0, errors.New("GOFER_USER_DB_MAX_OPEN must be a positive integer")
	}
	return limit, nil
}

// configuredSyncCapacity reads how many background mail syncs may run at once
// and how many folders may be watched in real time, overall and per mail
// server. Unset values use the mail package defaults.
func configuredSyncCapacity() (mail.UserIMAPBackgroundOptions, error) {
	var options mail.UserIMAPBackgroundOptions
	for _, setting := range []struct {
		name  string
		value *int
	}{
		{"GOFER_SYNC_MAX_CONCURRENT", &options.MaxConcurrentSyncs},
		{"GOFER_SYNC_MAX_PER_SERVER", &options.MaxPerServer},
		{"GOFER_IDLE_MAX", &options.MaxIdleWatchers},
		{"GOFER_IDLE_MAX_PER_SERVER", &options.MaxIdlePerServer},
	} {
		raw := strings.TrimSpace(os.Getenv(setting.name))
		if raw == "" {
			continue
		}
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			return options, fmt.Errorf("%s must be a positive integer", setting.name)
		}
		*setting.value = value
	}
	return options, nil
}

const defaultShutdownTimeout = 20 * time.Second

// configuredShutdownTimeout bounds how long in-flight requests may drain on
// shutdown. Keep it under the pod's terminationGracePeriodSeconds.
func configuredShutdownTimeout() (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv("GOFER_SHUTDOWN_TIMEOUT"))
	if raw == "" {
		return defaultShutdownTimeout, nil
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil || timeout <= 0 {
		return 0, errors.New("GOFER_SHUTDOWN_TIMEOUT must be a positive duration such as 20s")
	}
	return timeout, nil
}

// ready reports readiness: the central database answers and the process is not draining.
func (app *managedApplication) ready(ctx context.Context) error {
	app.requestMu.Lock()
	closing := app.closing
	app.requestMu.Unlock()
	if closing {
		return errors.New("shutting down")
	}
	return app.storage.central.Read().PingContext(ctx)
}

// RunManaged runs managed mode on per-user storage until ctx is canceled.
func RunManaged(ctx context.Context, stdout, stderr io.Writer) (err error) {
	maxOpen, err := configuredUserDatabaseLimit()
	if err != nil {
		return err
	}
	shutdownTimeout, err := configuredShutdownTimeout()
	if err != nil {
		return err
	}
	if _, err := handler.AvatarProvidersFromEnv(); err != nil {
		return err
	}
	diagnostics, err := loadDiagnosticsConfig()
	if err != nil {
		return err
	}
	stopDiagnostics, err := startDiagnostics(diagnostics)
	if err != nil {
		return err
	}
	defer stopDiagnostics()
	app, err := newManagedApplication(ctx, ConfiguredDatabasePath(), maxOpen)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, app.Close()) }()
	listener, err := net.Listen("tcp", app.httpConfig.ListenAddr)
	if err != nil {
		return err
	}
	server := newHTTPServer(withHealth(app, app.ready))
	server.BaseContext = func(net.Listener) context.Context { return app.ctx }
	defer server.Close()
	if _, err := fmt.Fprintf(stdout, "Gofer running on %s\nlistening on %s\ndatabase: %s\nauth: managed\n", app.httpConfig.BaseURL, listener.Addr(), app.storage.central.Path()); err != nil {
		listener.Close()
		return err
	}
	if _, err := app.setupNotice.WriteTo(stderr); err != nil {
		listener.Close()
		return err
	}
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	openBrowserIfEnabled(app.httpConfig.BaseURL)
	select {
	case err := <-finished:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		err := server.Shutdown(shutdownCtx)
		if err != nil {
			err = errors.Join(err, server.Close())
		}
		serveErr := <-finished
		if !errors.Is(serveErr, http.ErrServerClosed) {
			err = errors.Join(err, serveErr)
		}
		return err
	}
}
