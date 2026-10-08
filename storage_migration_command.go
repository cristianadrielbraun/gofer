package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func runStorageCommand(ctx context.Context, args []string, stdout, stderr io.Writer) (bool, int) {
	if len(args) == 0 || args[0] != "storage" {
		return false, 0
	}
	if len(args) < 2 || args[1] != "inspect" {
		fmt.Fprintln(stderr, "usage: gofer storage inspect [--db PATH]")
		return true, 2
	}
	flags := flag.NewFlagSet("storage inspect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("db", configuredDatabasePath(), "shared database to inspect")
	if err := flags.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return true, 0
		}
		return true, 2
	}
	if flags.NArg() != 0 || *path == "" {
		fmt.Fprintln(stderr, "usage: gofer storage inspect [--db PATH]")
		return true, 2
	}
	report, err := storage.InspectUserStorageMigration(ctx, *path)
	if err != nil {
		fmt.Fprintf(stderr, "storage inspection failed: %v\n", err)
		return true, 1
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		fmt.Fprintf(stderr, "write storage inspection: %v\n", err)
		return true, 1
	}
	return true, 0
}
