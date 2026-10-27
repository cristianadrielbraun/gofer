package authoperator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/httpguard"
	"github.com/cristianadrielbraun/gofer/internal/runtimeguard"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const usage = `Usage:
  gofer auth status
  gofer auth users list
  gofer auth setup-token rotate
  gofer auth invite create --name <name> --username <username> [--expires-in <duration>]
  gofer auth recover (--user-id <id> | --username <name>) --confirm <same value>
  gofer auth sessions revoke (--user-id <id> | --username <name>) --confirm <same value>
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
	if command.mutating() {
		// A running Gofer holds the runtime lock, so it carries out the change
		// itself. Without one, the command takes the lock and writes directly.
		if handled, exitCode := runOnServer(ctx, ControlSocketPath(databasePath), args, stdout, stderr); handled {
			return exitCode
		}
		return runMutation(ctx, command, func(action func(*Service) error) error {
			return runMutatingCommand(ctx, databasePath, action)
		}, stdout, stderr)
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

// mutationRunner gives a mutating command a Service only while the database
// may be changed: under the operator's runtime lock, or inside the server.
type mutationRunner func(action func(*Service) error) error

func runMutation(ctx context.Context, command parsedCommand, mutate mutationRunner, stdout, stderr io.Writer) int {
	switch command.name {
	case "recover":
		return runRecovery(ctx, command.target, mutate, stdout, stderr)
	case "setup-token rotate":
		return runSetupTokenRotation(ctx, mutate, stdout, stderr)
	case "sessions revoke":
		return runSessionRevocation(ctx, command.target, mutate, stdout, stderr)
	case "invite create":
		return runInvitationCreation(ctx, command.invitation, mutate, stdout, stderr)
	}
	_, _ = fmt.Fprintf(stderr, "auth: %s does not change the database\n", command.name)
	return 2
}

type parsedCommand struct {
	name       string
	target     UserTarget
	invitation invitationOptions
}

func (command parsedCommand) mutating() bool {
	switch command.name {
	case "recover", "setup-token rotate", "sessions revoke", "invite create":
		return true
	}
	return false
}

type invitationOptions struct {
	name     string
	username string
	lifetime time.Duration
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
	if len(args) == 2 && args[0] == "setup-token" && args[1] == "rotate" {
		return parsedCommand{name: "setup-token rotate"}, false, nil
	}
	if len(args) >= 2 && args[0] == "invite" && args[1] == "create" {
		options, err := parseInvitationOptions(args[2:])
		if err != nil {
			return parsedCommand{}, false, err
		}
		return parsedCommand{name: "invite create", invitation: options}, false, nil
	}
	if len(args) == 5 && args[0] == "recover" {
		target, err := parseConfirmedUserOptions(args[1:], "recovery")
		if err != nil {
			return parsedCommand{}, false, err
		}
		return parsedCommand{name: "recover", target: target}, false, nil
	}
	if len(args) == 6 && args[0] == "sessions" && args[1] == "revoke" {
		target, err := parseConfirmedUserOptions(args[2:], "session revocation")
		if err != nil {
			return parsedCommand{}, false, err
		}
		return parsedCommand{name: "sessions revoke", target: target}, false, nil
	}
	return parsedCommand{}, false, fmt.Errorf("invalid command")
}

func parseConfirmedUserOptions(args []string, action string) (UserTarget, error) {
	if len(args) != 4 {
		return UserTarget{}, fmt.Errorf("%s requires --user-id or --username, and --confirm", action)
	}
	values := make(map[string]string, 2)
	for index := 0; index < len(args); index += 2 {
		flag := args[index]
		if flag != "--user-id" && flag != "--username" && flag != "--confirm" {
			return UserTarget{}, fmt.Errorf("unknown %s option %q", action, flag)
		}
		if _, duplicate := values[flag]; duplicate {
			return UserTarget{}, fmt.Errorf("%s option %s was provided more than once", action, flag)
		}
		if args[index+1] == "" {
			return UserTarget{}, fmt.Errorf("%s option %s requires a value", action, flag)
		}
		values[flag] = args[index+1]
	}
	if values["--confirm"] == "" {
		return UserTarget{}, fmt.Errorf("%s requires --confirm", action)
	}
	var target UserTarget
	switch {
	case values["--user-id"] != "":
		target = UserTarget{ID: values["--user-id"]}
	case values["--username"] != "":
		target = UserTarget{Username: values["--username"]}
	default:
		return UserTarget{}, fmt.Errorf("%s requires --user-id or --username", action)
	}
	if values["--user-id"] != "" && values["--username"] != "" {
		return UserTarget{}, fmt.Errorf("%s accepts only one of --user-id and --username", action)
	}
	if values["--confirm"] != target.ID+target.Username {
		return UserTarget{}, fmt.Errorf("--confirm must exactly match the --user-id or --username value")
	}
	return target, nil
}

func parseInvitationOptions(args []string) (invitationOptions, error) {
	if len(args)%2 != 0 {
		return invitationOptions{}, fmt.Errorf("invitation options must each have a value")
	}
	values := make(map[string]string, 3)
	for index := 0; index < len(args); index += 2 {
		flag := args[index]
		if flag != "--name" && flag != "--username" && flag != "--expires-in" {
			return invitationOptions{}, fmt.Errorf("unknown invitation option %q", flag)
		}
		if _, duplicate := values[flag]; duplicate {
			return invitationOptions{}, fmt.Errorf("invitation option %s was provided more than once", flag)
		}
		if args[index+1] == "" {
			return invitationOptions{}, fmt.Errorf("invitation option %s requires a value", flag)
		}
		values[flag] = args[index+1]
	}
	if values["--name"] == "" || values["--username"] == "" {
		return invitationOptions{}, fmt.Errorf("invitation requires --name and --username")
	}
	options := invitationOptions{name: values["--name"], username: values["--username"]}
	if value := values["--expires-in"]; value != "" {
		lifetime, err := time.ParseDuration(value)
		if err != nil || lifetime <= 0 {
			return invitationOptions{}, fmt.Errorf("--expires-in must be a duration such as 30m, 24h or 168h")
		}
		options.lifetime = lifetime
	}
	return options, nil
}

func runInvitationCreation(ctx context.Context, options invitationOptions, mutate mutationRunner, stdout, stderr io.Writer) int {
	// Invitations are redeemed through managed mode's sign-in pages, so check
	// the mode and public address before anything is written.
	if mode := auth.LoadConfig("").AuthenticationMode(); mode != auth.ModeManaged {
		_, _ = fmt.Fprintf(stderr, "auth invite create: invitations need managed mode, but GOFER_AUTH_MODE is %s\n", mode)
		return 1
	}
	httpConfig, err := httpguard.LoadConfig()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "auth invite create: %v\n", err)
		return 1
	}
	err = mutate(func(service *Service) error {
		invitation, err := service.CreateInvitation(ctx, options.name, options.username, options.lifetime)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(stdout,
			"user_id: %s\nusername: %s\nexpires_at: %s\nenrollment_url: %s\ninvitation_token: %s\n",
			printableField(invitation.User.ID), printableField(invitation.User.Username),
			invitation.Token.ExpiresAt.UTC().Format(time.RFC3339),
			strings.TrimRight(httpConfig.BaseURL, "/")+invitationEnrollmentPath, invitation.Token.Token,
		); err != nil {
			return fmt.Errorf("write invitation: %w", err)
		}
		return nil
	})
	var validationErr *auth.AdministratorUserInvitationValidationError
	if errors.As(err, &validationErr) {
		fields := make([]string, 0, len(validationErr.Fields))
		for field := range validationErr.Fields {
			fields = append(fields, field)
		}
		sort.Strings(fields)
		for _, field := range fields {
			_, _ = fmt.Fprintf(stderr, "auth invite create: --%s: %s\n", field, validationErr.Fields[field])
		}
		return 1
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "auth invite create: %v\n", err)
		return 1
	}
	return 0
}

func runRecovery(ctx context.Context, target UserTarget, mutate mutationRunner, stdout, stderr io.Writer) int {
	err := mutate(func(service *Service) error {
		result, err := service.Recover(ctx, target)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout,
			"user_id: %s\ntoken_purpose: %s\nexpires_at: %s\nrevoked_sessions: %d\nreplaced_reset_tokens: %d\nreset_token: %s\n",
			printableField(result.Token.UserID), result.Token.Purpose,
			result.Token.ExpiresAt.UTC().Format(time.RFC3339), result.RevokedSessions,
			result.ReplacedTokens, result.Token.Token,
		)
		if err != nil {
			return fmt.Errorf("write recovery token: %w", err)
		}
		return nil
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "auth recover: %v\n", err)
		return 1
	}
	return 0
}

func runSessionRevocation(ctx context.Context, target UserTarget, mutate mutationRunner, stdout, stderr io.Writer) int {
	err := mutate(func(service *Service) error {
		result, err := service.RevokeSessions(ctx, target)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(stdout, "user_id: %s\nrevoked_sessions: %d\n",
			printableField(result.UserID), result.RevokedSessions,
		); err != nil {
			return fmt.Errorf("write session revocation result: %w", err)
		}
		return nil
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "auth sessions revoke: %v\n", err)
		return 1
	}
	return 0
}

func runSetupTokenRotation(ctx context.Context, mutate mutationRunner, stdout, stderr io.Writer) int {
	err := mutate(func(service *Service) error {
		result, err := service.RotateSetupToken(ctx)
		if err != nil {
			return err
		}
		if result.State.TokenExpiresAt == nil {
			return fmt.Errorf("replacement setup token has no expiry")
		}
		if _, err := fmt.Fprintf(stdout, "expires_at: %s\nsetup_token: %s\n",
			result.State.TokenExpiresAt.UTC().Format(time.RFC3339), result.Token,
		); err != nil {
			return fmt.Errorf("write replacement setup token: %w", err)
		}
		return nil
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "auth setup-token rotate: %v\n", err)
		return 1
	}
	return 0
}

func runMutatingCommand(ctx context.Context, databasePath string, action func(*Service) error) error {
	// Preflight through the query-only connection so a missing or stale database
	// cannot leave an operator lock file behind.
	probe, err := storage.OpenReadOnly(databasePath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	var owned bool
	if err := probe.Read().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name IN ('gofer_storage_layout','gofer_user_store','gofer_account_directory'))`).Scan(&owned); err != nil {
		probe.Close()
		return err
	}
	if err := probe.Close(); err != nil {
		return fmt.Errorf("close database preflight: %w", err)
	}
	var layout storage.UserStorageLayout
	if owned {
		layout, err = storage.ReadUserStorageLayoutMetadata(databasePath)
		if err != nil {
			return fmt.Errorf("completed managed layout required: %w", err)
		}
		sourceLock, err := runtimeguard.Acquire(layout.SourcePath)
		if err != nil {
			return err
		}
		defer sourceLock.Close()
	}
	lock, err := runtimeguard.Acquire(databasePath)
	if err != nil {
		return fmt.Errorf("acquire exclusive database lock: %w", err)
	}
	defer lock.Close()
	if owned {
		managerLock, err := runtimeguard.Acquire(filepath.Join(layout.UserDirectory, "manager"))
		if err != nil {
			return err
		}
		defer managerLock.Close()
		verified, err := storage.LoadUserStorageLayout(ctx, databasePath)
		if err != nil {
			return err
		}
		if verified != layout {
			return fmt.Errorf("managed layout changed while acquiring operator locks")
		}
		if err := storage.VerifyUserStorageRuntimeBoundary(ctx, databasePath); err != nil {
			return err
		}
	} else if err := storage.VerifySharedStorageRuntime(ctx, databasePath); err != nil {
		return err
	}
	db, err := storage.OpenExisting(databasePath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	return action(NewService(db))
}

func writeStatus(ctx context.Context, output io.Writer, service *Service) error {
	status, err := service.Status(ctx)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output,
		"schema_version: %d\nauthentication_initialized: %t\nowner_user_id: %s\nsetup_token_configured: %t\nsetup_token_expires_at: %s\nsetup_token_attempts: %d\nactive_administrators: %d\n",
		status.SchemaVersion, status.Initialized, printableField(status.OwnerUserID),
		status.SetupTokenConfigured, printableTime(status.SetupTokenExpiresAt),
		status.SetupTokenAttempts, status.ActiveAdministrators,
	)
	return err
}

func writeUsers(ctx context.Context, output io.Writer, service *Service) error {
	users, err := service.ListUsers(ctx)
	if err != nil {
		return err
	}
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "ID\tUSERNAME\tSTATUS\tTYPE\tROLE"); err != nil {
		return err
	}
	for _, user := range users {
		role := "user"
		if user.IsAdmin {
			role = "administrator"
		}
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
			printableField(user.ID), printableField(user.Username), user.Status, user.UserType, role,
		); err != nil {
			return err
		}
	}
	return writer.Flush()
}

// invitationEnrollmentPath is where an invited user redeems their token; it
// matches the route the admin panel shows next to new invitations.
const invitationEnrollmentPath = "/account/enroll"

func printableField(value string) string {
	if value == "" {
		return "-"
	}
	return strconv.QuoteToASCII(value)
}

func printableTime(value *time.Time) string {
	if value == nil {
		return "-"
	}
	return value.UTC().Format(time.RFC3339)
}
