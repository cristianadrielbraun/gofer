package main

import (
	"fmt"
	"gofer.email/internal/handler"
	"gofer.email/internal/storage"
	"log"
	"net/http"
	"os"
)

func main() {
	dbPath := os.Getenv("GOFER_DB_PATH")
	if dbPath == "" {
		dbPath = "data/gofer.db"
	}

	db, err := storage.New(dbPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	mux := http.NewServeMux()
	h := handler.New(db)
	h.RegisterRoutes(mux)

	addr := ":8090"
	fmt.Printf("gofer.email running on http://localhost%s\n", addr)
	fmt.Printf("database: %s\n", db.Path())
	log.Fatal(http.ListenAndServe(addr, mux))
}
