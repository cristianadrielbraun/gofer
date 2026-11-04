package message

import "testing"

func TestBaseSubjectDropsReplyAndForwardPrefixes(t *testing.T) {
	for _, subject := range []string{"Re: Plans", "RE: Fwd: Plans", "RV: Plans", "Fw: RV: Plans", "[Fwd: Plans]", "Plans (fwd)"} {
		if got := BaseSubject(subject); got != "plans" {
			t.Errorf("BaseSubject(%q) = %q, want %q", subject, got, "plans")
		}
		if !IsReplyOrForwardSubject(subject) {
			t.Errorf("IsReplyOrForwardSubject(%q) = false", subject)
		}
	}
	if IsReplyOrForwardSubject("Plans for RV: trips") {
		t.Error("a subject with RV: inside it is not a forward")
	}
}
