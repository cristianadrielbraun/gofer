package authoperator

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const usage = `Usage:
  gofer auth status
  gofer auth users list
`

func Run(ctx context.Context, args []string, databasePath string, stdout, stderr io.Writer) int {
	command, help, valid := parseCommand(args)
	if help {
		_, _ = io.WriteString(stdout, usage)
		return 0
	}
	if !valid {
		_, _ = io.WriteString(stderr, usage)
		return 2
	}

	db, err := storage.OpenReadOnly(databasePath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "auth %s: open database: %v\n", command, err)
		return 1
	}
	defer db.Close()
	service := NewService(db)

	switch command {
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

func parseCommand(args []string) (command string, help, valid bool) {
	if len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		return "", true, true
	}
	if len(args) == 1 && args[0] == "status" {
		return "status", false, true
	}
	if len(args) == 2 && args[0] == "users" && args[1] == "list" {
		return "users list", false, true
	}
	return "", false, false
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
