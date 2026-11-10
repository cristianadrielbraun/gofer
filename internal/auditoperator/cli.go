// Package auditoperator is the host operator's view of the administration
// audit log: verifying its chain and exporting it, straight from the
// database, read-only, whether or not Gofer is running.
package auditoperator

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const usage = `Usage:
  gofer admin audit verify
  gofer admin audit export [--format csv|jsonl] [--since <time>] [--until <time>] [--action <action, or kind followed by a dot>]

Times are RFC 3339, such as 2026-11-01T00:00:00Z. Both commands read the
database Gofer uses (GOFER_DB_PATH) and need its secret key
(GOFER_SECRET_KEY, or secret.key next to the database) to check the chain.
`

// Run carries out "gofer admin" with args, the words after it. loadKey
// returns the instance secret the chain key is derived from.
func Run(ctx context.Context, args []string, databasePath string, loadKey func() ([]byte, error), stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		_, _ = io.WriteString(stdout, usage)
		return 0
	}
	if len(args) < 2 || args[0] != "audit" {
		_, _ = fmt.Fprintf(stderr, "admin: unknown command %q\n", strings.Join(args, " "))
		_, _ = io.WriteString(stderr, usage)
		return 2
	}
	switch args[1] {
	case "verify":
		if len(args) != 2 {
			_, _ = io.WriteString(stderr, usage)
			return 2
		}
		return verify(ctx, databasePath, loadKey, stdout, stderr)
	case "export":
		return export(ctx, args[2:], databasePath, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "admin audit: unknown command %q\n", args[1])
		_, _ = io.WriteString(stderr, usage)
		return 2
	}
}

func verify(ctx context.Context, databasePath string, loadKey func() ([]byte, error), stdout, stderr io.Writer) int {
	secret, err := loadKey()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "admin audit verify: %v\n", err)
		return 1
	}
	db, err := storage.OpenReadOnly(databasePath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "admin audit verify: open database: %v\n", err)
		return 1
	}
	defer db.Close()
	result, err := audit.Verify(ctx, db.Read(), audit.DeriveKey(secret))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "admin audit verify: %v\n", err)
		return 1
	}
	if !result.OK() {
		_, _ = fmt.Fprintf(stdout, "The audit log has been changed: event %d doesn't follow from the ones before it.\n", result.BrokenAt)
		return 1
	}
	if result.Checked == 0 {
		_, _ = fmt.Fprintln(stdout, "The audit log is empty.")
		return 0
	}
	_, _ = fmt.Fprintf(stdout, "Verified %d events, %d to %d: none was changed, removed or inserted.\n", result.Checked, result.FirstSeq, result.LastSeq)
	return 0
}

func export(ctx context.Context, args []string, databasePath string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("gofer admin audit export", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	format := flags.String("format", string(audit.FormatCSV), "")
	since := flags.String("since", "", "")
	until := flags.String("until", "", "")
	action := flags.String("action", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		_, _ = io.WriteString(stderr, usage)
		return 2
	}
	filter := audit.Filter{Action: strings.TrimSpace(*action)}
	var err error
	if filter.Since, err = parseTime(*since); err != nil {
		_, _ = fmt.Fprintf(stderr, "admin audit export: --since: %v\n", err)
		return 2
	}
	if filter.Until, err = parseTime(*until); err != nil {
		_, _ = fmt.Fprintf(stderr, "admin audit export: --until: %v\n", err)
		return 2
	}
	switch audit.Format(*format) {
	case audit.FormatCSV, audit.FormatJSONL:
	default:
		_, _ = fmt.Fprintf(stderr, "admin audit export: --format must be csv or jsonl\n")
		return 2
	}
	db, err := storage.OpenReadOnly(databasePath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "admin audit export: open database: %v\n", err)
		return 1
	}
	defer db.Close()
	if _, err := audit.Export(ctx, db.Read(), filter, audit.Format(*format), stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "admin audit export: %v\n", err)
		return 1
	}
	return 0
}

func parseTime(value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, nil
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, errors.New("use an RFC 3339 time, such as 2026-11-01T00:00:00Z")
	}
	return at, nil
}
