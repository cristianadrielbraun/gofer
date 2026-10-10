package message

import (
	"strings"
	"testing"
)

func TestMarkThreadQuote(t *testing.T) {
	jan := QuoteSource{
		Name:  "Jan Novak",
		Email: "jan@example.com",
		Text:  "Hi Alex,\n\nCould you send me the signed contract and the invoice for September before Friday?\nI also need the bank statement from last month.\n\nThanks,\nJan",
	}
	tests := []struct {
		name   string
		body   string
		before string // text that must stay above the marker; empty means no marker
	}{
		{
			name:   "gmail",
			body:   `<div dir="ltr">Sure, sending everything tomorrow morning.</div><br><div class="gmail_quote"><div dir="ltr" class="gmail_attr">On Fri, Oct 10, 2026 at 5:00 PM Jan Novak &lt;jan@example.com&gt; wrote:<br></div><blockquote class="gmail_quote">Hi Alex,<br><br>Could you send me the signed contract and the invoice for September before Friday?<br>I also need the bank statement from last month.<br><br>Thanks,<br>Jan</blockquote></div>`,
			before: `<div dir="ltr">Sure, sending everything tomorrow morning.</div><br>`,
		},
		{
			name:   "japanese attribution",
			body:   `<div>明日送ります。</div><div class="quote"><div>2026年10月10日(金) 17:00 Jan Novak &lt;jan@example.com&gt;:<br></div><blockquote>Hi Alex,<br>Could you send me the signed contract and the invoice for September before Friday?<br>I also need the bank statement from last month.<br>Thanks,<br>Jan</blockquote></div>`,
			before: `<div>明日送ります。</div>`,
		},
		{
			name:   "outlook header below a signature",
			body:   `<div class="WordSection1"><p>Sending them now.</p><p>Alex Rivera</p><p>Tel: 555 1234</p><div style="border:none;border-top:solid #E1E1E1 1.0pt"><p><b>Von:</b> Jan Novak<br><b>Gesendet:</b> Freitag, 10. Oktober 2026 17:00<br><b>An:</b> Alex<br><b>Betreff:</b> Unterlagen</p></div><p>Hi Alex,</p><p>Could you send me the signed contract and the invoice for September before Friday? I also need the bank statement from last month.</p><p>Thanks,<br>Jan</p></div>`,
			before: `<div class="WordSection1"><p>Sending them now.</p><p>Alex Rivera</p><p>Tel: 555 1234</p>`,
		},
		{
			name:   "outlook web with a rule",
			body:   `<div>Done, see attached.</div><hr style="display:inline-block;width:98%"><div id="divRplyFwdMsg"><b>From:</b> Jan Novak &lt;jan@example.com&gt;<br><b>Sent:</b> Friday<br><b>Subject:</b> Documents</div><div>Hi Alex,<br>Could you send me the signed contract and the invoice for September before Friday?<br>I also need the bank statement from last month.<br>Thanks,<br>Jan</div>`,
			before: `<div>Done, see attached.</div>`,
		},
		{
			name:   "plain text with a wrapped attribution",
			body:   "<pre>Sure, tomorrow.\n\nAlex\n\nOn Fri, Oct 10, 2026 at 5:00 PM Jan Novak &lt;jan@example.com&gt;\nwrote:\n&gt; Hi Alex,\n&gt;\n&gt; Could you send me the signed contract and the invoice for September before Friday?\n&gt; I also need the bank statement from last month.\n&gt;\n&gt; Thanks,\n&gt; Jan</pre>",
			before: "<pre>Sure, tomorrow.\n\nAlex\n\n",
		},
		{
			name: "replies written between quoted lines",
			body: `<p>Answers below.</p><div>On Fri, Jan Novak wrote:</div><blockquote>Could you send me the signed contract and the invoice for September before Friday?</blockquote><p>The contract is signed, I will scan it and send it over tonight with the invoice.</p><blockquote>I also need the bank statement from last month.</blockquote><p>I have to ask the bank for that one, it should arrive next week.</p>`,
		},
		{
			name: "nothing written above the quote",
			body: `<div class="gmail_quote"><div>On Fri, Jan Novak wrote:</div><blockquote>Hi Alex,<br>Could you send me the signed contract and the invoice for September before Friday?<br>I also need the bank statement from last month.</blockquote></div>`,
		},
		{
			name: "quotes something outside the thread",
			body: `<div>FYI</div><div>---------- Forwarded message ---------<br>From: Anna</div><div>The office will be closed on Monday for maintenance of the heating system.</div>`,
		},
		{
			name: "repeats a phrase but quotes nothing",
			body: `<p>You asked for the signed contract and the invoice for September before Friday, and I will have both ready by Thursday evening at the latest, together with everything else you listed.</p>`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := string(MarkThreadQuote([]byte(test.body), []QuoteSource{jan}))
			index := strings.Index(got, QuoteStartMarker)
			if test.before == "" {
				if index >= 0 {
					t.Fatalf("marked a quote in %q", got)
				}
				return
			}
			if index < 0 {
				t.Fatalf("no quote marked in %q", got)
			}
			if got[:index] != test.before {
				t.Fatalf("quote starts after %q, want after %q", got[:index], test.before)
			}
			if strings.Replace(got, QuoteStartMarker, "", 1) != test.body {
				t.Fatalf("body changed beyond the marker: %q", got)
			}
		})
	}
}

// A sender's signature and disclaimer repeat in each of their messages, so they
// match earlier text too. The quote still starts at the attribution below them.
func TestMarkThreadQuoteKeepsRepeatedSignature(t *testing.T) {
	signature := "Jan Novak\njan@example.com\ntel.: +1 555 0100\n*****************************\n" +
		"NOTICE: This message and its attachments are meant only for the people it is addressed to and may hold private information. " +
		"If it reached you by mistake, please let the sender know and delete it without sharing it with anyone."
	jan := QuoteSource{Name: "Jan Novak", Email: "jan@example.com",
		Text: "Hola Alex,\nte mando el presupuesto de la reforma, decime si te parece bien.\nJan\n\n" + signature}
	alex := QuoteSource{Name: "Alex Rivera", Email: "alex@example.com",
		Text: "Me parece bien, avancemos! Saludos\n\nOn Fri, Oct 9, 2026 at 5:03 PM Jan Novak <jan@example.com> wrote:\n> Hola Alex,\n> te mando el presupuesto de la reforma, decime si te parece bien.\n> Jan\n> " + strings.ReplaceAll(signature, "\n", "\n> ")}
	// Jan's previous reply carried the same signature and a near-identical
	// attribution, so almost everything below his name matches earlier text.
	janAgain := QuoteSource{Name: "Jan Novak", Email: "jan@example.com",
		Text: "Hola Alex, perfecto, empezamos el lunes.\nJan\n\n" + signature +
			"\n\nEl sáb, 10 oct 2026, 16:59, Alex Rivera <alex@example.com> escribió:\n> Jan: el lunes me viene bien."}

	htmlSignature := strings.ReplaceAll(signature, "\n", "<br>")
	before := `<div>Genial, nos vemos el lunes entonces.</div><div>Jan</div><div>` + htmlSignature + `</div>`
	body := before + `<div class="gmail_quote"><div class="gmail_attr">El sáb, 10 oct 2026, 17:09, Alex Rivera &lt;alex@example.com&gt; escribió:<br></div>` +
		`<blockquote>Me parece bien, avancemos! Saludos<br><br><div class="gmail_quote"><div class="gmail_attr">On Fri, Oct 9, 2026 at 5:03 PM Jan Novak &lt;jan@example.com&gt; wrote:<br></div>` +
		`<blockquote>Hola Alex,<br>te mando el presupuesto de la reforma, decime si te parece bien.<br>Jan<br><br>` + htmlSignature + `</blockquote></div></blockquote></div>`

	got := string(MarkThreadQuote([]byte(body), []QuoteSource{jan, janAgain, alex}))
	index := strings.Index(got, QuoteStartMarker)
	if index < 0 {
		t.Fatalf("no quote marked in %q", got)
	}
	if got[:index] != before {
		t.Fatalf("quote starts after %q, want after %q", got[:index], before)
	}
}

// A message sent twice has the same opening as its earlier copy. It quotes
// nothing of it, so its own text stays visible.
func TestMarkThreadQuoteIgnoresEarlierCopy(t *testing.T) {
	jan := QuoteSource{Name: "Jan Novak", Email: "jan@example.com",
		Text: "Hola Alex, gracias por avisar. Cuando vas a tener las llaves del local?\nJan"}
	reply := "Jan: todavía no me dieron las llaves porque el dueño está de viaje, vuelve el jueves.\nAlex\n\nOn Sat, Oct 10, 2026 at 4:35 PM, Jan Novak <jan@example.com> wrote:\n> Hola Alex, gracias por avisar. Cuando vas a tener las llaves del local?\n> Jan"
	copy := QuoteSource{Name: "Alex Rivera", Email: "alex@example.com", Text: reply}
	body := "<pre>" + strings.ReplaceAll(strings.ReplaceAll(reply, "<", "&lt;"), ">", "&gt;") + "</pre>"

	got := string(MarkThreadQuote([]byte(body), []QuoteSource{jan, copy}))
	index := strings.Index(got, QuoteStartMarker)
	if index < 0 {
		t.Fatalf("no quote marked in %q", got)
	}
	if want := "<pre>Jan: todavía no me dieron las llaves porque el dueño está de viaje, vuelve el jueves.\nAlex\n\n"; got[:index] != want {
		t.Fatalf("quote starts after %q, want after %q", got[:index], want)
	}
}
