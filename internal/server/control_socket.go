package server

import (
	"context"
	"log"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/authoperator"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// startControlSocket lets `gofer auth` commands that change the database run
// through this server, which holds the runtime lock they would otherwise need.
// Without the socket Gofer still runs; those commands then need it stopped.
func startControlSocket(databasePath string, db *storage.DB) (stop func()) {
	listener, err := authoperator.ListenControl(databasePath)
	if err != nil {
		log.Printf("control socket unavailable, so auth commands that change data need Gofer stopped: %v", err)
		return func() {}
	}
	stopServing := authoperator.ServeControl(listener, db)
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := stopServing(ctx); err != nil {
			log.Printf("control socket shutdown: %v", err)
		}
	}
}
