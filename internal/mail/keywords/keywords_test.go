package keywords

import "testing"

func TestValidateAllowsProviderKeywordIDs(t *testing.T) {
	tests := []string{
		"$label2",
		"$VendorFlag",
		"Client",
	}
	for _, input := range tests {
		got, err := Validate(input)
		if err != nil {
			t.Fatalf("Validate(%q) error = %v", input, err)
		}
		if got != input {
			t.Fatalf("Validate(%q) = %q, want same keyword", input, got)
		}
	}
	if _, err := Validate("$Junk"); err == nil {
		t.Fatal("Validate($Junk) error = nil, want status keyword rejection")
	}
}

func TestIsStatusCoversJMAPSpellings(t *testing.T) {
	for _, status := range []string{"\\Seen", "$seen", "$Flagged", "$draft", "$answered", "$Junk", "$IsTrusted", "$X-ME-Annot-2",
		"$HasAttachment", "$hasnoattachment", "$SubmitPending", "$Submitted", "$AutoSent",
		"$Muted", "$Followed", "$Notify", "$Unsubscribed", "$CanUnsubscribe", "$Imported"} {
		if !IsStatus(status) {
			t.Errorf("IsStatus(%q) = false", status)
		}
	}
	for _, label := range []string{"$label1", "Client", "$VendorFlag", "$Important", "$MailFlagBit3"} {
		if IsStatus(label) {
			t.Errorf("IsStatus(%q) = true", label)
		}
	}
}

func TestRFC9979Names(t *testing.T) {
	for _, keyword := range []string{
		"$AutoSent", "$CanUnsubscribe", "$Followed", "$HasAttachment", "$HasMemo", "$HasNoAttachment",
		"$Imported", "$IsTrusted", "$MailFlagBit0", "$MailFlagBit1", "$MailFlagBit2", "$MaskedEmail",
		"$Memo", "$Muted", "$New", "$Notify", "$Unsubscribed",
	} {
		if !IsRFC9979(keyword) || !IsStatus(keyword) {
			t.Errorf("%q did not classify as RFC 9979 status", keyword)
		}
		if _, err := Validate(keyword); err == nil {
			t.Errorf("Validate(%q) accepted a reserved status", keyword)
		}
	}
	for value, want := range map[string]MailboxAttribute{
		"Snoozed": MailboxSnoozed, "SCHEDULED": MailboxScheduled, "memos": MailboxMemos,
		"\\Memos": "", "drafts": "", "": "",
	} {
		if got := RFC9979MailboxAttribute(value); got != want {
			t.Errorf("RFC9979MailboxAttribute(%q) = %q, want %q", value, got, want)
		}
	}
}
