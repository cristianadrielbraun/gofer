package message

import (
	"strings"
	"testing"
)

func sourceReportWarnings(report SourceReport) string {
	var texts []string
	for _, warning := range report.Warnings {
		texts = append(texts, warning.Text)
	}
	return strings.Join(texts, "\n")
}

func TestAnalyzeSourceAuthenticatedMessage(t *testing.T) {
	raw := "Return-Path: <bounce@mail.example.com>\r\n" +
		"Authentication-Results: mx.example.net;\r\n" +
		"\tspf=pass smtp.mailfrom=mail.example.com;\r\n" +
		"\tdkim=pass header.d=example.com header.s=s1;\r\n" +
		"\tdmarc=pass header.from=example.com\r\n" +
		"Authentication-Results: forged.example.org; spf=fail\r\n" +
		"DKIM-Signature: v=1; a=rsa-sha256; d=example.com; s=s1;\r\n\tb=abc\r\n" +
		"Received: from a by b\r\n" +
		"Received: from c by d\r\n" +
		"From: Ada Example <ada@example.com>\r\n" +
		"Message-ID: <1@example.com>\r\n" +
		"Date: Mon, 2 Jan 2006 15:04:05 +0000\r\n" +
		"Subject: Hello\r\n\r\nBody\r\n"
	report := AnalyzeSource([]byte(raw))
	if len(report.Checks) != 3 || !report.Checks[0].Passed() || report.Checks[0].Method != "SPF" || report.Checks[2].Method != "DMARC" {
		t.Fatalf("checks = %+v", report.Checks)
	}
	if report.Hops != 2 {
		t.Fatalf("hops = %d", report.Hops)
	}
	if len(report.Warnings) != 0 {
		t.Fatalf("unexpected warnings:\n%s", sourceReportWarnings(report))
	}
	if len(report.Headers) != 10 || report.Headers[1].Value != "mx.example.net; spf=pass smtp.mailfrom=mail.example.com; dkim=pass header.d=example.com header.s=s1; dmarc=pass header.from=example.com" {
		t.Fatalf("headers = %+v", report.Headers)
	}
}

func TestAnalyzeSourceWarnsAboutMismatches(t *testing.T) {
	raw := "Return-Path: <x@bulk.example.org>\r\n" +
		"Authentication-Results: mx.example.net; spf=softfail smtp.mailfrom=bulk.example.org; dkim=pass header.d=example.org\r\n" +
		"From: \"support@bank.example\" <alerts@example.biz>\r\n" +
		"Reply-To: <collect@example.info>\r\n" +
		"X-Spam-Flag: YES\r\n" +
		"Subject: Hi\r\n\r\nBody\r\n"
	text := sourceReportWarnings(AnalyzeSource([]byte(raw)))
	for _, want := range []string{"SPF check result is \"softfail\"", "envelope sender (bulk.example.org)", "Replies go to a different domain (example.info)", "DKIM signature is from example.org", "shows the address support@bank.example", "no Message-ID", "marked this message as spam"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing warning %q in:\n%s", want, text)
		}
	}
}

func TestAnalyzeSourceUsesReceivedSPF(t *testing.T) {
	raw := "Received-SPF: Fail (example.net: domain does not designate 192.0.2.1)\r\nFrom: a@example.com\r\nMessage-ID: <x@y>\r\n\r\n"
	report := AnalyzeSource([]byte(raw))
	if len(report.Checks) != 1 || report.Checks[0].Result != "fail" {
		t.Fatalf("checks = %+v", report.Checks)
	}
}

// Pages in other languages write each warning from its key and values.
func TestAnalyzeSourceWarningsCarryTheirMessage(t *testing.T) {
	raw := "Return-Path: <x@bulk.example.org>\r\n" +
		"Authentication-Results: mx.example.net; spf=softfail smtp.mailfrom=bulk.example.org\r\n" +
		"From: \"support@bank.example\" <alerts@example.biz>\r\n" +
		"Reply-To: <collect@example.info>\r\n" +
		"X-Spam-Flag: YES\r\n\r\nBody\r\n"
	report := AnalyzeSource([]byte(raw))
	for _, warning := range report.Warnings {
		if warning.Key == "" || len(warning.Args)%2 != 0 {
			t.Errorf("warning %q has key %q and args %v", warning.Text, warning.Key, warning.Args)
		}
	}
	for _, fact := range report.Facts {
		if fact.Label == "Envelope sender" && fact.Key != "mail.source.fact.envelope_sender" {
			t.Errorf("envelope sender fact key = %q", fact.Key)
		}
	}
}
