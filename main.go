package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/authoperator"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/handler"
	"github.com/cristianadrielbraun/gofer/internal/httpguard"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/notifications"
	"github.com/cristianadrielbraun/gofer/internal/runtimeguard"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serverExit := 0
	exitCode := runApplication(ctx, os.Args[1:], os.Stdout, os.Stderr, func() {
		if auth.LoadConfig("").AuthenticationMode() == auth.ModeManaged {
			if err := runManagedServer(ctx, os.Stdout, os.Stderr); err != nil {
				fmt.Fprintf(os.Stderr, "Gofer startup or shutdown failed: %v\n", err)
				serverExit = 1
			}
		} else {
			// The shared runtime retains its existing signal behavior.
			stop()
			runServer()
		}
	})
	if exitCode == 0 {
		exitCode = serverExit
	}
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

func runApplication(ctx context.Context, args []string, stdout, stderr io.Writer, serve func()) int {
	if handled, exitCode := runAuthCommand(ctx, args, stdout, stderr); handled {
		return exitCode
	}
	if handled, exitCode := runStorageCommand(ctx, args, stdout, stderr); handled {
		return exitCode
	}
	serve()
	return 0
}

func runAuthCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (bool, int) {
	if len(args) == 0 || args[0] != "auth" {
		return false, 0
	}
	return true, authoperator.Run(ctx, args[1:], configuredDatabasePath(), stdout, stderr)
}

func configuredDatabasePath() string {
	if dbPath := os.Getenv("GOFER_DB_PATH"); dbPath != "" {
		return dbPath
	}
	return "data/gofer.db"
}

func runServer() {
	log.Printf("boot: loading configuration")

	httpConfig, err := httpguard.LoadConfig()
	if err != nil {
		log.Fatalf("invalid HTTP configuration: %v", err)
	}
	authConfig := auth.LoadConfig(httpConfig.BaseURL)
	if err := authConfig.ValidateMode(); err != nil {
		log.Fatalf("invalid authentication configuration: %v", err)
	}
	authConfig.SecureCookies = httpConfig.SecureCookies()
	mailboxOAuthConfig := mailauth.LoadConfig(httpConfig.BaseURL, authConfig.Enabled)
	if err := httpConfig.ValidateExposure(authConfig.Enabled); err != nil {
		log.Fatalf("unsafe HTTP configuration: %v", err)
	}
	if httpConfig.WarnUnauthenticatedRemote(authConfig.Enabled) {
		log.Printf("WARNING: unauthenticated remote access is explicitly enabled; anyone who can reach %s can control Gofer", httpConfig.BaseURL)
	}

	dbPath := configuredDatabasePath()
	log.Printf("boot: database path resolved to %s", dbPath)

	dataDir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatalf("failed to create database directory: %v", err)
	}
	runtimeLock, err := runtimeguard.Acquire(dbPath)
	if err != nil {
		log.Fatalf("failed to acquire exclusive database lock: %v", err)
	}
	defer runtimeLock.Close()
	log.Printf("boot: exclusive database lock acquired")
	if err := storage.VerifySharedStorageRuntime(context.Background(), dbPath); err != nil {
		log.Fatalf("incompatible storage layout: %v", err)
	}

	db, err := storage.New(dbPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()
	log.Printf("boot: database opened")

	secretKey := loadOrGenerateSecretKey(filepath.Join(dataDir, "secret.key"))
	log.Printf("boot: encryption key loaded")

	accountStore, err := config.NewAccountStore(db, secretKey)
	if err != nil {
		log.Fatalf("failed to create account store: %v", err)
	}
	log.Printf("boot: account store initialized")

	blobStore := store.NewBlobStore(filepath.Join(dataDir, "accounts"))
	log.Printf("boot: blob store initialized")

	authManager := auth.NewManager(authConfig, db, auth.Dependencies{BucketHashKey: secretKey})
	if err := authManager.ValidateRuntimeMode(context.Background()); err != nil {
		log.Fatalf("incompatible authentication mode: %v", err)
	}
	log.Printf("boot: authentication mode=%s", authConfig.AuthenticationMode())
	var setupNotice bytes.Buffer
	if err := provisionInitialSetupToken(context.Background(), authManager, authConfig.SetupToken, &setupNotice); err != nil {
		log.Fatalf("failed to provision authentication setup token: %v", err)
	}
	mailCredentials := mailauth.New(mailboxOAuthConfig, db, secretKey)
	if err := mailCredentials.SecureOAuthCredentials(context.Background()); err != nil {
		log.Fatalf("failed to secure mailbox OAuth credentials: %v", err)
	}
	log.Printf("boot: mailbox credential service initialized")

	if err := authManager.EnsureDefaultUser(); err != nil {
		log.Fatalf("failed to ensure default user: %v", err)
	}
	log.Printf("boot: default user ensured")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if authManager.IsEnabled() {
		authManager.StartSessionCleanup(ctx)
		mailCredentials.StartCleanup(ctx)
		log.Printf("boot: session cleanup worker started")
	}

	syncer := mail.NewSyncOrchestrator(db, accountStore, blobStore, mailCredentials)
	log.Printf("boot: sync orchestrator initialized")
	vapidPublicKey, vapidPrivateKey := loadOrGenerateVAPIDKeys(filepath.Join(dataDir, "vapid_private.key"), filepath.Join(dataDir, "vapid_public.key"))
	vapidSubject := os.Getenv("GOFER_VAPID_SUBJECT")
	if vapidSubject == "" {
		vapidSubject = "mailto:gofer@gofer.email"
	}
	log.Printf("boot: web push VAPID keys loaded")
	notificationService := notifications.New(db, syncer.Events(), vapidPublicKey, vapidPrivateKey, vapidSubject)
	notificationService.Start(ctx)
	log.Printf("boot: notification worker started")

	mux := http.NewServeMux()
	h := handler.New(db, accountStore, syncer, blobStore, authManager, vapidPublicKey, mailCredentials)

	go func() {
		log.Printf("boot: background threading worker started")
		db.SetThreadingState(storage.ThreadingState{InProgress: true})
		if err := db.EnsureThreading(ctx); err != nil {
			log.Printf("boot: background threading worker failed: %v", err)
			db.SetThreadingState(storage.ThreadingState{InProgress: false})
		} else {
			log.Printf("boot: background threading worker finished")
		}

		log.Printf("boot: sync orchestrator startup launched")
		syncer.Start(ctx)
	}()

	h.StartAvatarBackfill(ctx)
	h.StartContactSync(ctx)
	h.StartCalendarSync(ctx)
	h.StartOutgoingSendWorker(ctx)
	h.StartMessageMutationWorker(ctx)
	h.StartMailRetentionWorker(ctx)
	h.StartAuthenticationEventRetentionWorker(ctx)
	h.RegisterRoutes(mux)
	log.Printf("boot: HTTP routes registered")
	h.StartAccountDeletionCleanup(ctx)

	var handler http.Handler = mux
	handler = authManager.Middleware(handler)
	handler = httpConfig.Middleware(handler)
	handler = httpConfig.ClientNetworkMiddleware(authConfig.Enabled, handler)

	fmt.Printf("Gofer running on %s\n", httpConfig.BaseURL)
	fmt.Printf("listening on %s\n", httpConfig.ListenAddr)
	fmt.Printf("database: %s\n", db.Path())
	if authConfig.Enabled {
		providers := []string{}
		if authManager.HasGoogleLogin() {
			providers = append(providers, "Google")
		}
		if authManager.HasMicrosoftLogin() {
			providers = append(providers, "Microsoft")
		}
		if authManager.HasOIDCLogin() {
			providers = append(providers, authManager.OIDCLoginName())
		}
		appLogin := "none"
		if len(providers) > 0 {
			appLogin = strings.Join(providers, ", ")
		}
		fmt.Printf("auth: %s (application login: %s)\n", authConfig.AuthenticationMode(), appLogin)
	} else {
		fmt.Printf("auth: disabled (local mode)\n")
	}
	// Emit the entire notice together after startup details, rather than burying
	// its one-time secret among initialization messages.
	if _, err := setupNotice.WriteTo(os.Stderr); err != nil {
		log.Fatalf("failed to display setup notice: %v", err)
	}
	log.Fatal(http.ListenAndServe(httpConfig.ListenAddr, handler))
}

func provisionInitialSetupToken(ctx context.Context, manager *auth.Manager, configuredToken string, console io.Writer) error {
	if !manager.IsEnabled() {
		return nil
	}
	provision, err := manager.EnsureSetupToken(ctx, configuredToken)
	if err != nil {
		return err
	}
	if provision.State.Initialized {
		return nil
	}
	var notice strings.Builder
	fmt.Fprintf(&notice, "\n────────────────────────────────────────────────────────────\n %s SETUP REQUIRED\n\n Open: %s/setup\n", strings.ToUpper(string(manager.Config().AuthenticationMode())), strings.TrimRight(manager.Config().BaseURL, "/"))
	if provision.Created && !provision.Configured {
		if provision.Token == "" || provision.State.TokenExpiresAt == nil {
			return fmt.Errorf("generated setup token is incomplete")
		}
		fmt.Fprintf(&notice, "\n Token (shown once):\n setup_token: %s\n", provision.Token)
	} else if provision.Configured {
		notice.WriteString("\n Use the token you supplied in GOFER_SETUP_TOKEN.\n")
	} else {
		notice.WriteString("\n The setup token was issued earlier and cannot be displayed again.\n")
	}
	if provision.State.TokenExpiresAt != nil {
		fmt.Fprintf(&notice, " Expires: %s (server local time)\n", provision.State.TokenExpiresAt.Local().Format("2006-01-02 15:04:05 MST (UTC-07:00)"))
	}
	notice.WriteString("\n Lost or expired token? Stop Gofer, then run:\n ./gofer auth setup-token rotate\n Use the same GOFER_DB_PATH, then restart Gofer.\n")
	if err := writeLocalProfileNotice(ctx, manager, &notice); err != nil {
		return err
	}
	notice.WriteString("────────────────────────────────────────────────────────────\n\n")
	if _, err := io.WriteString(console, notice.String()); err != nil {
		return fmt.Errorf("write setup notice to local console: %w", err)
	}
	return nil
}

// A converted open or personal installation keeps its profile as a regular
// user. Setup creates a separate administrator; a profile without credentials
// gets a fresh link to set its first password on each start until setup ends.
func writeLocalProfileNotice(ctx context.Context, manager *auth.Manager, notice *strings.Builder) error {
	profile, err := manager.PendingLocalProfile(ctx)
	if err != nil || profile == nil {
		return err
	}
	base := strings.TrimRight(manager.Config().BaseURL, "/")
	notice.WriteString("\n This installation was converted from open or personal mode.\n Setup creates a new administrator account. Your existing profile\n")
	fmt.Fprintf(notice, " %q keeps its mailboxes and becomes a regular user.\n", profile.Username)
	if profile.HasCredentials {
		notice.WriteString(" It signs in with the same credentials as before.\n")
		return nil
	}
	token, err := manager.IssueLocalProfilePasswordLink(ctx)
	if err != nil {
		return fmt.Errorf("issue local profile password token: %w", err)
	}
	fmt.Fprintf(notice, "\n It has no password yet. To set one, open: %s/account/enroll\n reset_token: %s\n Expires: %s (server local time)\n",
		base, token.Token, token.ExpiresAt.Local().Format("2006-01-02 15:04:05 MST (UTC-07:00)"))
	notice.WriteString(" Restarting Gofer before setup issues a new token; afterwards, an\n administrator can send one from the Users page.\n")
	return nil
}

func loadOrGenerateVAPIDKeys(privatePath, publicPath string) (string, string) {
	public, private, err := loadVAPIDKeys(privatePath, publicPath)
	if err != nil {
		log.Fatalf("web push keys: %v", err)
	}
	return public, private
}

func loadVAPIDKeys(privatePath, publicPath string) (string, string, error) {
	if envPrivate, envPublic := os.Getenv("GOFER_VAPID_PRIVATE_KEY"), os.Getenv("GOFER_VAPID_PUBLIC_KEY"); envPrivate != "" && envPublic != "" {
		return envPublic, envPrivate, nil
	}

	privateBytes, privateErr := os.ReadFile(privatePath)
	publicBytes, publicErr := os.ReadFile(publicPath)
	if privateErr == nil && publicErr == nil && len(privateBytes) > 0 && len(publicBytes) > 0 {
		return string(publicBytes), string(privateBytes), nil
	}

	privateKey, publicKey, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return "", "", fmt.Errorf("generate VAPID keys: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(privatePath), 0755); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(privatePath, []byte(privateKey), 0600); err != nil {
		return "", "", fmt.Errorf("write VAPID private key: %w", err)
	}
	if err := os.WriteFile(publicPath, []byte(publicKey), 0644); err != nil {
		return "", "", fmt.Errorf("write VAPID public key: %w", err)
	}
	log.Printf("generated new VAPID key pair in %s", filepath.Dir(privatePath))
	return publicKey, privateKey, nil
}

func loadOrGenerateSecretKey(path string) []byte {
	if envKey := os.Getenv("GOFER_SECRET_KEY"); envKey != "" {
		key, err := hex.DecodeString(envKey)
		if err != nil || len(key) != 32 {
			log.Fatalf("invalid GOFER_SECRET_KEY: must be 64 hex characters (32 bytes)")
		}
		return key
	}

	data, err := os.ReadFile(path)
	if err == nil && len(data) == 32 {
		return data
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		log.Fatalf("generate secret key: %v", err)
	}

	os.MkdirAll(filepath.Dir(path), 0755)
	if err := os.WriteFile(path, key, 0600); err != nil {
		log.Fatalf("write secret key: %v", err)
	}

	log.Printf("generated new secret key at %s", path)
	return key
}
