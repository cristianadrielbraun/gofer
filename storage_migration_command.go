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
	if len(args) > 1 && args[1] == "migrate" {
		return true, runStorageMigrationCommand(ctx, args[2:], stdout, stderr)
	}
	if len(args) < 2 || args[1] != "inspect" {
		fmt.Fprintln(stderr, "usage: gofer storage inspect [--db PATH] | migrate [--db PATH] --to PATH [--working-dir PATH] [--retry]")
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

func runStorageMigrationCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("storage migrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	source := flags.String("db", configuredDatabasePath(), "original shared database")
	destination := flags.String("to", "", "new central database in the original data directory")
	workingDirectory := flags.String("working-dir", "", "original Gofer working directory (defaults to current directory)")
	retry := flags.Bool("retry", false, "resume recognized interrupted migration work")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || *source == "" || *destination == "" {
		fmt.Fprintln(stderr, "usage: gofer storage migrate [--db PATH] --to PATH [--working-dir PATH] [--retry]")
		return 2
	}
	key, err := loadExistingMigrationKey(*source)
	if err != nil {
		fmt.Fprintf(stderr, "storage migration failed: %v\n", err)
		return 1
	}
	options := migrationOptions(*source, *destination, key)
	options.WorkingDirectory = *workingDirectory
	options.Retry = *retry
	layout, err := storage.MigrateUserStorage(ctx, options)
	if err != nil {
		fmt.Fprintf(stderr, "storage migration failed: %v\n", err)
		return 1
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(layout); err != nil {
		fmt.Fprintf(stderr, "write storage migration result: %v\n", err)
		return 1
	}
	return 0
}
