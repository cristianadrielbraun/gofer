package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/handler"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/notifications"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
	"log"
	"net/http"
	"os"
	"path/filepath"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/joho/godotenv"
)

func main() {
	godotenv.Load()
	log.Printf("boot: loading configuration")

	dbPath := os.Getenv("GOFER_DB_PATH")
	if dbPath == "" {
		dbPath = "data/gofer.db"
	}
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

	authConfig := auth.LoadConfig()
	authManager := auth.NewManager(authConfig, db)
	log.Printf("boot: auth manager initialized (enabled=%t)", authConfig.Enabled)

	if err := authManager.EnsureDefaultUser(); err != nil {
		log.Fatalf("failed to ensure default user: %v", err)
	}
	log.Printf("boot: default user ensured")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if authManager.IsEnabled() {
		authManager.StartSessionCleanup(ctx)
		log.Printf("boot: session cleanup worker started")
	}

	syncer := mail.NewSyncOrchestrator(db, accountStore, blobStore, authManager)
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

	mux := http.NewServeMux()
	h := handler.New(db, accountStore, syncer, blobStore, authManager, vapidPublicKey)
	h.StartAvatarBackfill(ctx)
	h.StartContactSync(ctx)
	h.StartScheduledSendWorker(ctx)
	h.RegisterRoutes(mux)
	log.Printf("boot: HTTP routes registered")

	var handler http.Handler = mux
	handler = authManager.Middleware(handler)

	addr := ":8090"
	fmt.Printf("Gofer running on http://localhost%s\n", addr)
	fmt.Printf("database: %s\n", db.Path())
	if authConfig.Enabled {
		fmt.Printf("auth: enabled (Google OAuth2)\n")
	} else {
		fmt.Printf("auth: disabled (local mode)\n")
	}
	log.Fatal(http.ListenAndServe(addr, handler))
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
