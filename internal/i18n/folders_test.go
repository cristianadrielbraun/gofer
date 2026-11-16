package i18n

import (
	"context"
	"testing"
)

// A special folder still called by the English name sync gave it is
// translated; any other name, or a folder that isn't special, is not.
func TestDefaultFolderName(t *testing.T) {
	zh := WithLocale(context.Background(), "zh-cn")
	for _, test := range []struct {
		role, name, want string
		ok               bool
	}{
		{"inbox", "Inbox", "收件箱", true},
		{" Junk ", "Spam", "垃圾邮件", true},
		{"spam", "Spam", "垃圾邮件", true},
		{"sent", "Sent Items", "", false},
		{"", "Inbox", "", false},
	} {
		got, ok := DefaultFolderName(zh, test.role, test.name)
		if got != test.want || ok != test.ok {
			t.Errorf("DefaultFolderName(%q, %q) = %q, %v; want %q, %v", test.role, test.name, got, ok, test.want, test.ok)
		}
		wantName := test.name
		if test.ok {
			wantName = test.want
		}
		if got := FolderName(zh, test.role, test.name); got != wantName {
			t.Errorf("FolderName(%q, %q) = %q, want %q", test.role, test.name, got, wantName)
		}
	}
}
