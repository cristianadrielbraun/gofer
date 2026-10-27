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
	"strings"
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
		} else if err := server.RunShared(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "Gofer startup or shutdown failed: %v\n", err)
			serverExit = 1
		}
	})
	if exitCode == 0 {
		exitCode = serverExit
	}
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

const usage = `Usage:
  gofer                 start the server
  gofer help            show this help
  gofer healthcheck     probe the local /healthz endpoint (container HEALTHCHECK)

Authentication:
  gofer auth status
  gofer auth users list
  gofer auth setup-token rotate
  gofer auth invite create --name <name> --username <username> [--expires-in <duration>]
  gofer auth recover (--user-id <id> | --username <name>) --confirm <same value>
  gofer auth sessions revoke (--user-id <id> | --username <name>) --confirm <same value>

Storage:
  gofer storage inspect [--db PATH]
  gofer storage migrate [--db PATH] --to PATH [--working-dir PATH] [--retry]
  gofer storage compress [--dir PATH] [--dry-run]
`

func runApplication(ctx context.Context, args []string, stdout, stderr io.Writer, serve func()) int {
	if len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		_, _ = io.WriteString(stdout, usage)
		return 0
	}
	if handled, exitCode := runAuthCommand(ctx, args, stdout, stderr); handled {
		return exitCode
	}
	if handled, exitCode := server.RunStorageCommand(ctx, args, stdout, stderr); handled {
		return exitCode
	}
	if len(args) == 1 && args[0] == "healthcheck" {
		if err := server.ProbeHealth(os.Getenv("GOFER_ADDR")); err != nil {
			_, _ = fmt.Fprintf(stderr, "gofer: unhealthy: %v\n", err)
			return 1
		}
		return 0
	}
	if len(args) != 0 {
		_, _ = fmt.Fprintf(stderr, "gofer: unknown command %q\n\n", strings.Join(args, " "))
		_, _ = io.WriteString(stderr, usage)
		return 2
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
