package handler

import (
	"context"
	"errors"
	"net/http"
	"sort"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// A change that newly requires two-factor sign-in of people, through a
// policy, is refused until the administrator confirms having seen who. The
// page then shows who, and sends the same form again with the answer.

// adminTwoFactorChoice is the answer the form carries, if any.
func adminTwoFactorChoice(r *http.Request) auth.TwoFactorChoice {
	return auth.TwoFactorChoice{Confirmed: r.PostFormValue("confirm_two_factor") == "1", SignOut: r.PostFormValue("sign_out") == "1"}
}

// adminTwoFactorConfirmation is the question to ask about err, or nil when
// err isn't a change waiting for one.
func adminTwoFactorConfirmation(ctx context.Context, r *http.Request, err error) *views.AdminTwoFactorConfirm {
	var waiting *auth.TwoFactorConfirmationError
	if !errors.As(err, &waiting) {
		return nil
	}
	preview := waiting.Preview
	confirm := &views.AdminTwoFactorConfirm{
		Action: r.URL.Path, CSRFToken: auth.CSRFToken(ctx, http.MethodPost, r.URL.Path), Count: preview.People,
		People: views.AdminSignInData{WithoutAppTotal: preview.WithoutAppTotal, SessionsToEnd: preview.SessionsToEnd},
	}
	for _, person := range preview.WithoutApp {
		confirm.People.WithoutApp = append(confirm.People.WithoutApp, views.AdminSignInPerson{ID: person.ID, Username: person.Username})
	}
	names := make([]string, 0, len(r.PostForm))
	for name := range r.PostForm {
		switch name {
		case auth.CSRFFormFieldName, "confirm_two_factor", "sign_out":
		default:
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		for _, value := range r.PostForm[name] {
			confirm.Fields = append(confirm.Fields, views.AdminFormField{Name: name, Value: value})
		}
	}
	return confirm
}
