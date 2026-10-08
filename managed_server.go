package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	app.auth = auth.NewManager(authConfig, s.central, auth.Dependencies{BucketHashKey: s.key})
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
	mux := http.NewServeMux()
	if err := app.mailbox.RegisterUserStorageRoutes(ctx, mux, s.routing, handler.UserStorageOptions{
		Accounts: accounts, IMAP: app.imap, Credentials: app.credentials,
		ContactSync: &handler.UserContactSyncOptions{}, CalendarSync: &handler.UserCalendarSyncOptions{},
	}); err != nil {
		return nil, err
	}
	app.mailbox.RegisterAuthenticationRoutes(mux)
	app.mailbox.RegisterAdministrationRoutes(mux)
	if err := app.mailbox.StartUserThreading(ctx); err != nil {
		return nil, err
	}
	if err := app.mailbox.AwaitUserThreading(ctx); err != nil {
		return nil, err
	}
	if err := app.imap.Start(mail.UserIMAPBackgroundOptions{}); err != nil {
		return nil, err
	}
	if err := app.mailbox.StartUserMailRetention(ctx, handler.UserMailRetentionOptions{}); err != nil {
		return nil, err
	}
	app.mailbox.StartAuthenticationEventRetentionWorker(ctx)
	app.auth.StartSessionCleanup(ctx)
	app.flows.StartCleanup(ctx)
	app.notifications.Start(ctx)
	app.handler = app.httpConfig.ClientNetworkMiddleware(authConfig.Enabled, app.httpConfig.Middleware(app.auth.Middleware(mux)))
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

func runManagedServer(ctx context.Context, stdout, stderr io.Writer) (err error) {
	app, err := newManagedApplication(ctx, configuredDatabasePath(), 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, app.Close()) }()
	listener, err := net.Listen("tcp", app.httpConfig.ListenAddr)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: app, BaseContext: func(net.Listener) context.Context { return app.ctx }}
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
	select {
	case err := <-finished:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
