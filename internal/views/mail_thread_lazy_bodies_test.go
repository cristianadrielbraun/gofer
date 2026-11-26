package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"golang.org/x/net/html"
)

// A long conversation used to put every message's body frame in the page, so
// opening it loaded every body at once. Only the open message's body is live;
// the others wait in templates until they are opened.
func TestThreadLoadsOnlyTheOpenMessageBody(t *testing.T) {
	email := &models.Email{ID: "702", Subject: "Lunch on Friday?"}
	thread := []models.ThreadItem{
		{ID: "701", Subject: "Lunch on Friday?", From: models.Contact{Name: "Alex Rivera", Email: "alex@example.com"}},
		{ID: "702", Subject: "Re: Lunch on Friday?", From: models.Contact{Name: "Sam Lee", Email: "sam@example.com"}},
		{ID: "703", Subject: "Re: Lunch on Friday?", From: models.Contact{Name: "Alex Rivera", Email: "alex@example.com"}},
	}
	var out bytes.Buffer
	if err := MailViewThreadContent(email, thread).Render(context.Background(), &out); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	doc, err := html.Parse(strings.NewReader(out.String()))
	if err != nil {
		t.Fatalf("html.Parse() error = %v", err)
	}

	live := map[string]bool{}
	waiting := map[string]bool{}
	var walk func(n *html.Node, inTemplate bool)
	walk = func(n *html.Node, inTemplate bool) {
		if n.Type == html.ElementNode && n.Data == "template" {
			inTemplate = true
		}
		if n.Type == html.ElementNode && n.Data == "iframe" && attr(n, "data-email-body-frame") {
			if inTemplate {
				waiting[attrValue(n, "data-email-id")] = true
			} else {
				live[attrValue(n, "data-email-id")] = true
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inTemplate)
		}
	}
	walk(doc, false)

	if len(live) != 1 || !live["702"] {
		t.Fatalf("live body frames = %v, want only the open message 702", live)
	}
	if len(waiting) != 2 || !waiting["701"] || !waiting["703"] {
		t.Fatalf("templated body frames = %v, want 701 and 703", waiting)
	}
}

func attr(n *html.Node, name string) bool {
	for _, a := range n.Attr {
		if a.Key == name {
			return true
		}
	}
	return false
}

func attrValue(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}
