package message

import "testing"

func TestBaseSubjectDropsReplyAndForwardPrefixes(t *testing.T) {
	for _, subject := range []string{"Re: Plans", "RE: Fwd: Plans", "RV: Plans", "Fw: RV: Plans", "[Fwd: Plans]", "Plans (fwd)", "答复: Plans", "回复：Plans", "转发: 答复: Plans", "Re: 转发：Plans"} {
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
	if got := BaseSubject("回复率：本周"); got != "回复率：本周" || IsReplyOrForwardSubject("回复率：本周") {
		t.Errorf("a subject starting with a word that begins with 回复 is not a reply: BaseSubject = %q", got)
	}
}
