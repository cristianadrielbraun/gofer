package mail

import (
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserAttachmentMIMEPartKeepsIdenticalSiblingOrder(t *testing.T) {
	p := storage.AttachmentRecoveryPart{Filename: "same.txt", ContentType: "text/plain; charset=utf-8", SizeBytes: 4}
	s := storage.AttachmentRecovery{PartIndex: 1, Parts: []storage.AttachmentRecoveryPart{p, p}}
	parts := []message.AttachmentMeta{{Filename: "same.txt", ContentType: "text/plain", Size: 4}, {Filename: "same.txt", ContentType: "text/plain", Size: 4}}
	if i, err := userAttachmentMIMEPart(s, parts); err != nil || i != 1 {
		t.Fatal("same-name sibling order lost", i, err)
	}
	parts = append(parts, parts[0])
	if _, err := userAttachmentMIMEPart(s, parts); err == nil {
		t.Fatal("changed duplicate layout accepted")
	}
	parts = parts[:2]
	parts[0].Size = 5
	if i, err := userAttachmentMIMEPart(s, parts); err != nil || i != 1 {
		t.Fatal("unique exact part not selected", i, err)
	}
	parts[1].ContentID = "different"
	if _, err := userAttachmentMIMEPart(s, parts); err == nil {
		t.Fatal("wrong content identity accepted")
	}
}
