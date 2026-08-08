package authoperator

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/runtimeguard"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const usage = `Usage:
  gofer auth status
  gofer auth users list
  gofer auth recover --user <user-id> --confirm <user-id>
`

func Run(ctx context.Context, args []string, databasePath string, stdout, stderr io.Writer) int {
	command, help, parseErr := parseCommand(args)
	if help {
		_, _ = io.WriteString(stdout, usage)
		return 0
	}
	if parseErr != nil {
		_, _ = fmt.Fprintf(stderr, "auth: %v\n", parseErr)
		_, _ = io.WriteString(stderr, usage)
		return 2
	}
	if command.name == "recover" {
		return runRecovery(ctx, command.userID, databasePath, stdout, stderr)
	}

	db, err := storage.OpenReadOnly(databasePath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "auth %s: open database: %v\n", command.name, err)
		return 1
	}
	defer db.Close()
	service := NewService(db)

	switch command.name {
	case "status":
		if err := writeStatus(ctx, stdout, service); err != nil {
			_, _ = fmt.Fprintf(stderr, "auth status: %v\n", err)
			return 1
		}
	case "users list":
		if err := writeUsers(ctx, stdout, service); err != nil {
			_, _ = fmt.Fprintf(stderr, "auth users list: %v\n", err)
			return 1
		}
	}
	return 0
}

type parsedCommand struct {
	name   string
	userID string
}

func parseCommand(args []string) (command parsedCommand, help bool, err error) {
	if len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		return parsedCommand{}, true, nil
	}
	if len(args) == 1 && args[0] == "status" {
		return parsedCommand{name: "status"}, false, nil
	}
	if len(args) == 2 && args[0] == "users" && args[1] == "list" {
		return parsedCommand{name: "users list"}, false, nil
	}
	if len(args) == 5 && args[0] == "recover" {
		values := make(map[string]string, 2)
		for index := 1; index < len(args); index += 2 {
			flag := args[index]
			if flag != "--user" && flag != "--confirm" {
				return parsedCommand{}, false, fmt.Errorf("unknown recovery option %q", flag)
			}
			if _, duplicate := values[flag]; duplicate {
				return parsedCommand{}, false, fmt.Errorf("recovery option %s was provided more than once", flag)
			}
			if args[index+1] == "" {
				return parsedCommand{}, false, fmt.Errorf("recovery option %s requires a value", flag)
			}
			values[flag] = args[index+1]
		}
		if values["--user"] == "" || values["--confirm"] == "" {
			return parsedCommand{}, false, fmt.Errorf("recovery requires both --user and --confirm")
		}
		if values["--user"] != values["--confirm"] {
			return parsedCommand{}, false, fmt.Errorf("--confirm must exactly match --user")
		}
		return parsedCommand{name: "recover", userID: values["--user"]}, false, nil
	}
	return parsedCommand{}, false, fmt.Errorf("invalid command")
}

func runRecovery(ctx context.Context, userID, databasePath string, stdout, stderr io.Writer) int {
	// Preflight through the query-only connection so a missing or stale database
	// cannot leave an operator lock file behind.
	probe, err := storage.OpenReadOnly(databasePath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "auth recover: open database: %v\n", err)
		return 1
	}
	if err := probe.Close(); err != nil {
		_, _ = fmt.Fprintf(stderr, "auth recover: close database preflight: %v\n", err)
		return 1
	}

	lock, err := runtimeguard.Acquire(databasePath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "auth recover: acquire exclusive database lock: %v\n", err)
		return 1
	}
	defer lock.Close()

	db, err := storage.OpenExisting(databasePath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "auth recover: open database: %v\n", err)
		return 1
	}
	defer db.Close()
	result, err := NewService(db).Recover(ctx, userID)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "auth recover: %v\n", err)
		return 1
	}
	if _, err := fmt.Fprintf(stdout,
		"user_id: %s\ntoken_purpose: %s\nexpires_at: %s\nrevoked_sessions: %d\nreplaced_reset_tokens: %d\nreset_token: %s\n",
		printableField(result.Token.UserID), result.Token.Purpose,
		result.Token.ExpiresAt.UTC().Format(time.RFC3339), result.RevokedSessions,
		result.ReplacedTokens, result.Token.Token,
	); err != nil {
		_, _ = fmt.Fprintf(stderr, "auth recover: write recovery token: %v\n", err)
		return 1
	}
	return 0
}

func writeStatus(ctx context.Context, output io.Writer, service *Service) error {
	status, err := service.Status(ctx)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output,
		"schema_version: %d\nauthentication_initialized: %t\nowner_user_id: %s\nactive_administrators: %d\n",
		status.SchemaVersion, status.Initialized, printableField(status.OwnerUserID), status.ActiveAdministrators,
	)
	return err
}

func writeUsers(ctx context.Context, output io.Writer, service *Service) error {
	users, err := service.ListUsers(ctx)
	if err != nil {
		return err
	}
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "ID\tUSERNAME\tEMAIL\tSTATUS\tROLE"); err != nil {
		return err
	}
	for _, user := range users {
		role := "user"
		if user.IsAdmin {
			role = "administrator"
		}
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
			printableField(user.ID), printableField(user.Username), printableField(user.Email), user.Status, role,
		); err != nil {
			return err
		}
	}
	return writer.Flush()
}

func printableField(value string) string {
	if value == "" {
		return "-"
	}
	return strconv.QuoteToASCII(value)
}
