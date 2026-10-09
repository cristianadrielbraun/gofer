package main

import (
	"context"
	"fmt"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/authoperator"
	"github.com/cristianadrielbraun/gofer/internal/server"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serverExit := 0
	exitCode := runApplication(ctx, os.Args[1:], os.Stdout, os.Stderr, func() {
		if auth.LoadConfig("").AuthenticationMode() == auth.ModeManaged {
			if err := server.RunManaged(ctx, os.Stdout, os.Stderr); err != nil {
				fmt.Fprintf(os.Stderr, "Gofer startup or shutdown failed: %v\n", err)
				serverExit = 1
			}
		} else {
			// The shared runtime retains its existing signal behavior.
			stop()
			server.RunShared()
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
	if handled, exitCode := server.RunStorageCommand(ctx, args, stdout, stderr); handled {
		return exitCode
	}
	serve()
	return 0
}

func runAuthCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (bool, int) {
	if len(args) == 0 || args[0] != "auth" {
		return false, 0
	}
	return true, authoperator.Run(ctx, args[1:], server.ConfiguredDatabasePath(), stdout, stderr)
}
