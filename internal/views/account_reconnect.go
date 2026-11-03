package views

import (
	"context"
	"io"
	"slices"
	"strings"

	"github.com/a-h/templ"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// An account imported from a profile waits for logins before its services
// sync: its mail password, and a contacts or calendar password when those
// had their own. Gmail and Outlook wait for a new sign-in.
const (
	reconnectMail     = "mail"
	reconnectContacts = "contacts"
	reconnectCalendar = "calendar"
	reconnectSignIn   = "signin"
)

type reconnectMarksKey struct{}

// withReconnectMarks renders the edit wizard's steps with a mark on each step
// where the imported account still needs a password.
func withReconnectMarks(data models.EditAccountData, component templ.Component) templ.Component {
	marks := map[int]string{}
	if slices.Contains(data.ReconnectNeeds, reconnectMail) {
		marks[1] = reconnectMail
		if data.ReconnectSMTP {
			marks[2] = reconnectMail
		}
	}
	if slices.Contains(data.ReconnectNeeds, reconnectContacts) {
		marks[4] = reconnectContacts
	}
	if slices.Contains(data.ReconnectNeeds, reconnectCalendar) {
		marks[5] = reconnectCalendar
	}
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		return component.Render(context.WithValue(ctx, reconnectMarksKey{}, marks), w)
	})
}

// reconnectMark is the login a wizard step still needs, if any.
func reconnectMark(ctx context.Context, step int) string {
	marks, _ := ctx.Value(reconnectMarksKey{}).(map[int]string)
	return marks[step]
}

func editAccountNeeds(data models.EditAccountData, login string) bool {
	return slices.Contains(data.ReconnectNeeds, login)
}

// reconnectLoginName names a login in the page's language.
func reconnectLoginName(ctx context.Context, login string) string {
	switch login {
	case reconnectContacts:
		return i18n.T(ctx, "settings.reconnect.contacts")
	case reconnectCalendar:
		return i18n.T(ctx, "settings.reconnect.calendar")
	default:
		return i18n.T(ctx, "settings.reconnect.mail")
	}
}

func reconnectLoginNames(ctx context.Context, needs []string) string {
	labels := make([]string, 0, len(needs))
	for _, login := range needs {
		labels = append(labels, reconnectLoginName(ctx, login))
	}
	return settingsJoinAnd(ctx, labels)
}

func accountReconnectPendingMessage(ctx context.Context, account models.Account) string {
	needs := account.ReconnectNeeds
	switch {
	case slices.Contains(needs, reconnectSignIn):
		return i18n.T(ctx, "settings.reconnect.pending_sign_in")
	case len(needs) == 1 && needs[0] == reconnectMail:
		return i18n.T(ctx, "settings.reconnect.pending_password")
	case slices.Contains(needs, reconnectMail):
		return i18n.T(ctx, "settings.reconnect.pending_passwords", "services", reconnectLoginNames(ctx, needs))
	case len(needs) == 1:
		return i18n.T(ctx, "settings.reconnect.still_needs_one", "service", reconnectLoginName(ctx, needs[0]))
	default:
		return i18n.T(ctx, "settings.reconnect.still_needs_many", "services", reconnectLoginNames(ctx, needs))
	}
}

func accountReconnectButtonLabel(ctx context.Context, account models.Account) string {
	if len(account.ReconnectNeeds) > 1 {
		return i18n.T(ctx, "settings.reconnect.enter_passwords")
	}
	return i18n.T(ctx, "settings.reconnect.enter_password")
}

// editAccountReconnectSteps names where each missing password goes, for the
// note at the top of the edit dialog.
func editAccountReconnectSteps(ctx context.Context, data models.EditAccountData) []string {
	var steps []string
	for _, login := range data.ReconnectNeeds {
		switch login {
		case reconnectMail:
			if editAccountIsICloud(data) {
				steps = append(steps, i18n.T(ctx, "settings.wizard.mail_sync"))
			} else if data.ReconnectSMTP {
				steps = append(steps, settingsJoinAnd(ctx, []string{i18n.T(ctx, "settings.wizard.incoming"), i18n.T(ctx, "settings.wizard.outgoing")}))
			} else {
				steps = append(steps, i18n.T(ctx, "settings.wizard.incoming"))
			}
		case reconnectContacts:
			steps = append(steps, i18n.T(ctx, "settings.wizard.contacts"))
		case reconnectCalendar:
			steps = append(steps, i18n.T(ctx, "settings.wizard.calendar"))
		}
	}
	return steps
}

func editAccountReconnectNote(ctx context.Context, data models.EditAccountData) string {
	steps := editAccountReconnectSteps(ctx, data)
	if len(steps) <= 1 && editAccountNeeds(data, reconnectMail) {
		return i18n.T(ctx, "settings.reconnect.edit_note_one", "step", strings.Join(steps, ""))
	}
	return i18n.T(ctx, "settings.reconnect.edit_note_many", "steps", settingsJoinAnd(ctx, steps))
}

func reconnectRequiredAttrs(login string, required bool) templ.Attributes {
	if !required {
		return nil
	}
	return templ.Attributes{"data-reconnect-required": login}
}

// reconnectGateAttrs keep a save button unusable until the imported
// account's password fields in form are filled. When service names a
// section switch, the button is only held while that service is on.
func reconnectGateAttrs(form, service string, gated bool) templ.Attributes {
	if !gated {
		return nil
	}
	attrs := templ.Attributes{"data-reconnect-gate": form}
	if service != "" {
		attrs["data-reconnect-gate-switch"] = service
	}
	return attrs
}
