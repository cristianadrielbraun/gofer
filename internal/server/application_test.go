package server

import (
	"context"
	"io"

	"github.com/cristianadrielbraun/gofer/internal/authoperator"
)

// runApplication mirrors main's command dispatch for tests in this package.
func runApplication(ctx context.Context, args []string, stdout, stderr io.Writer, serve func()) int {
	if len(args) != 0 && args[0] == "auth" {
		return authoperator.Run(ctx, args[1:], ConfiguredDatabasePath(), stdout, stderr)
	}
	if handled, exitCode := RunStorageCommand(ctx, args, stdout, stderr); handled {
		return exitCode
	}
	serve()
	return 0
}
