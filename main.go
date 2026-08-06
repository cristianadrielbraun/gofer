package main

import (
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
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()
	if exitCode := runApplication(context.Background(), os.Args[1:], os.Stdout, os.Stderr, runServer); exitCode != 0 {
		os.Exit(exitCode)
	}
}

func runApplication(ctx context.Context, args []string, stdout, stderr io.Writer, serve func()) int {
	if handled, exitCode := runAuthCommand(ctx, args, stdout, stderr); handled {
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
	authConfig.SecureCookies = httpConfig.SecureCookies()
	if err := httpConfig.ValidateExposure(authConfig.Enabled); err != nil {
		log.Fatalf("unsafe HTTP configuration: %v", err)
	}
	if httpConfig.WarnUnauthenticatedRemote(authConfig.Enabled) {
		log.Printf("WARNING: unauthenticated remote access is explicitly enabled; anyone who can reach %s can control Gofer", httpConfig.BaseURL)
	}

	dbPath := configuredDatabasePath()
	log.Printf("boot: database path resolved to %s", dbPath)

	dataDir := filepath.Dir(dbPath)

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
	log.Printf("boot: auth manager initialized (enabled=%t)", authConfig.Enabled)
	mailCredentials := mailauth.New(&mailauth.Config{
		Enabled: authConfig.Enabled, BaseURL: authConfig.BaseURL,
		GoogleClient: authConfig.GoogleClient, MicrosoftClient: authConfig.MicrosoftClient,
	}, db)
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
	h.StartOutgoingSendWorker(ctx)
	h.StartMessageMutationWorker(ctx)
	h.StartMailRetentionWorker(ctx)
	h.RegisterRoutes(mux)
	log.Printf("boot: HTTP routes registered")
	h.StartAccountDeletionCleanup(ctx)

	var handler http.Handler = mux
	handler = authManager.Middleware(handler)
	handler = httpConfig.Middleware(handler)

	fmt.Printf("Gofer running on %s\n", httpConfig.BaseURL)
	fmt.Printf("listening on %s\n", httpConfig.ListenAddr)
	fmt.Printf("database: %s\n", db.Path())
	if authConfig.Enabled {
		fmt.Printf("auth: enabled (Google OAuth2)\n")
	} else {
		fmt.Printf("auth: disabled (local mode)\n")
	}
	log.Fatal(http.ListenAndServe(httpConfig.ListenAddr, handler))
}

func loadOrGenerateVAPIDKeys(privatePath, publicPath string) (string, string) {
	if envPrivate, envPublic := os.Getenv("GOFER_VAPID_PRIVATE_KEY"), os.Getenv("GOFER_VAPID_PUBLIC_KEY"); envPrivate != "" && envPublic != "" {
		return envPublic, envPrivate
	}

	privateBytes, privateErr := os.ReadFile(privatePath)
	publicBytes, publicErr := os.ReadFile(publicPath)
	if privateErr == nil && publicErr == nil && len(privateBytes) > 0 && len(publicBytes) > 0 {
		return string(publicBytes), string(privateBytes)
	}

	privateKey, publicKey, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		log.Fatalf("generate VAPID keys: %v", err)
	}
	os.MkdirAll(filepath.Dir(privatePath), 0755)
	if err := os.WriteFile(privatePath, []byte(privateKey), 0600); err != nil {
		log.Fatalf("write VAPID private key: %v", err)
	}
	if err := os.WriteFile(publicPath, []byte(publicKey), 0644); err != nil {
		log.Fatalf("write VAPID public key: %v", err)
	}
	log.Printf("generated new VAPID key pair in %s", filepath.Dir(privatePath))
	return publicKey, privateKey
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
