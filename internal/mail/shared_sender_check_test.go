package mail

import (
	"errors"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/mail/smtp"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Only a permanent refusal counts as refused: a temporary one, or a server
// that could not be asked, leaves the answer unknown.
func TestSharedSenderCheckState(t *testing.T) {
	cases := []struct {
		name    string
		result  smtp.SenderCheck
		err     error
		state   string
		message string
	}{
		{"accepted", smtp.SenderCheck{Accepted: true}, nil, storage.SenderCheckAccepted, ""},
		{"refused for good", smtp.SenderCheck{Stage: "mail", Code: 553, Message: "5.7.1 Sender address rejected"}, nil, storage.SenderCheckRejected, "553 5.7.1 Sender address rejected"},
		{"greylisted", smtp.SenderCheck{Stage: "mail", Code: 451, Message: "4.7.1 Try again later"}, nil, storage.SenderCheckUnknown, "451 4.7.1 Try again later"},
		{"busy", smtp.SenderCheck{Stage: "rcpt", Code: 421, Message: "4.3.2 Service shutting down"}, nil, storage.SenderCheckUnknown, "421 4.3.2 Service shutting down"},
		{"unreachable", smtp.SenderCheck{}, errors.New("dial tcp: i/o timeout"), storage.SenderCheckUnknown, "dial tcp: i/o timeout"},
	}
	for _, c := range cases {
		state, message := sharedSenderCheckState(c.result, c.err)
		if state != c.state || message != c.message {
			t.Errorf("%s: %q %q, want %q %q", c.name, state, message, c.state, c.message)
		}
	}
}
