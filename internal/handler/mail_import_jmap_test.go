package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// The JMAP writer uploads into the mailbox id behind a folder, with the
// message's state as keywords, and a refused message is not tried again.
func TestJMAPImportWriter(t *testing.T) {
	folder := storage.MailExportFolder{RemoteID: "Archive/2024", ProviderRemoteID: "mb-2024"}
	if remote := mailImportFolderRemote(providers.ProviderJMAP, folder); remote != "mb-2024" {
		t.Fatalf("folder remote = %q", remote)
	}
	var imported []map[string]any
	refuse := false
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/jmap":
			_, _ = fmt.Fprintf(w, `{"capabilities": {%q: {}, %q: {}}, "accounts": {"a1": {"name": "robin"}},
				"primaryAccounts": {%q: "a1"}, "apiUrl": %q, "uploadUrl": %q, "state": "s1"}`,
				jmap.CapabilityCore, jmap.CapabilityMail, jmap.CapabilityMail, server.URL+"/jmap/", server.URL+"/upload/{accountId}/")
		case "/upload/a1/":
			_, _ = io.WriteString(w, `{"accountId": "a1", "blobId": "b1", "type": "message/rfc822", "size": 1}`)
		case "/jmap/":
			var request struct {
				MethodCalls [][]json.RawMessage `json:"methodCalls"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			var args struct {
				Emails map[string]map[string]any `json:"emails"`
			}
			_ = json.Unmarshal(request.MethodCalls[0][1], &args)
			imported = append(imported, args.Emails["email"])
			result := `"created": {"email": {"id": "e1"}}`
			if refuse {
				result = `"notCreated": {"email": {"type": "invalidEmail"}}`
			}
			_, _ = io.WriteString(w, `{"methodResponses": [["Email/import", {"accountId": "a1", `+result+`}, "0"]], "sessionState": "s1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg := &models.AccountConfig{Provider: providers.ProviderJMAP, JMAPSessionURL: server.URL + "/.well-known/jmap",
		Username: "robin@example.test", JMAPAllowPlaintext: true, JMAPAllowPrivate: true}
	writer := &jmapImportWriter{mail.NewJMAPImport(cfg, "secret")}
	ctx := context.Background()
	msg := mailImportMessage{Raw: []byte("Subject: hi\r\n\r\nhi\r\n"), Date: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), Read: true, Answered: true}
	if err := writer.upload(ctx, "mb-2024", msg); err != nil {
		t.Fatal(err)
	}
	if got := imported[0]; !reflect.DeepEqual(got["mailboxIds"], map[string]any{"mb-2024": true}) ||
		!reflect.DeepEqual(got["keywords"], map[string]any{"$seen": true, "$answered": true}) || got["receivedAt"] != "2024-01-02T03:04:05Z" {
		t.Fatalf("import = %v", got)
	}
	refuse = true
	if err := writer.upload(ctx, "mb-2024", msg); err == nil || !mailImportFinalError(err) {
		t.Fatalf("refused upload = %v, final %v", err, mailImportFinalError(err))
	}
}
