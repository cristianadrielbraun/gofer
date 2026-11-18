package smtp

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	gosmtp "github.com/emersion/go-smtp"
)

// expectSMTP reads one command, checks it starts with want, and answers.
func expectSMTP(reader *bufio.Reader, conn net.Conn, want, reply string) error {
	line, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.HasPrefix(line, want) {
		return fmt.Errorf("got %q, want %s", line, want)
	}
	_, err = fmt.Fprint(conn, reply)
	return err
}

func TestCheckSenderAcceptedSendsNoData(t *testing.T) {
	client, serverErr := startSMTPConversation(t, "250 test\r\n", func(r *bufio.Reader, c net.Conn) error {
		for _, step := range [][2]string{
			{"MAIL FROM:<support@example.com>", "250 2.1.0 ok\r\n"},
			{"RCPT TO:<ana@example.com>", "250 2.1.5 ok\r\n"},
			{"RSET", "250 2.0.0 ok\r\n"},
		} {
			if err := expectSMTP(r, c, step[0], step[1]); err != nil {
				return err
			}
		}
		return nil
	})
	result, err := client.checkSender(context.Background(), "support@example.com", "ana@example.com")
	if err != nil || !result.Accepted {
		t.Fatalf("checkSender = %+v, %v", result, err)
	}
	waitForSMTPServer(t, serverErr)
}

func TestCheckSenderReportsTheRefusal(t *testing.T) {
	client, serverErr := startSMTPConversation(t, "250 test\r\n", func(r *bufio.Reader, c net.Conn) error {
		return expectSMTP(r, c, "MAIL FROM:<boss@example.com>", "550 5.7.1 You are not allowed to send as boss@example.com\r\n")
	})
	result, err := client.checkSender(context.Background(), "boss@example.com", "ana@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted || result.Stage != "mail" || result.Code != 550 || !strings.Contains(result.Message, "not allowed to send as") {
		t.Fatalf("result = %+v", result)
	}
	waitForSMTPServer(t, serverErr)
}

func TestIsSenderRefused(t *testing.T) {
	refused := classifyPreAcceptanceError(fmt.Errorf("mail from: %w", &gosmtp.SMTPError{Code: 550, Message: "not allowed"}))
	if !IsSenderRefused(refused) {
		t.Fatal("a permanent MAIL FROM refusal is not taken for a refused sender")
	}
	for name, err := range map[string]error{
		"temporary":   classifyPreAcceptanceError(fmt.Errorf("mail from: %w", &gosmtp.SMTPError{Code: 451, Message: "later"})),
		"recipient":   classifyPreAcceptanceError(fmt.Errorf("rcpt to x: %w", &gosmtp.SMTPError{Code: 550, Message: "no such user"})),
		"no response": fmt.Errorf("mail from: connection reset"),
		"nil":         nil,
	} {
		if IsSenderRefused(err) {
			t.Errorf("%s: taken for a refused sender", name)
		}
	}
}
