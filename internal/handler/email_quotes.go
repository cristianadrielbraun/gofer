package handler

import (
	"context"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// emailQuoteScript collapses the quoted history at the end of a message body
// behind a small toggle. In a thread the server has already compared the body
// with the earlier messages and marked where the quote starts
// (message.QuoteStartMarker). Otherwise it relies on what needs no language:
// the markers mail clients put around quotes (Gmail, Apple Mail, Thunderbird,
// Outlook, Yahoo, Proton, Zimbra), ">" lines in plain text, and shapes such as a
// short line ending in a colon above a quote or a drawn rule followed by
// "Label: value" header lines.
//
// Without the server's mark, a quote is only collapsed when it ends the
// message: after an attribution only quoted blocks may follow, so replies
// written between quotes stay visible. Outlook-style headers start a quote that
// runs to the end, since those clients never write inside it. Quotes with
// nothing written above them are left alone.
func emailQuoteScript() []byte {
	return []byte(`<script>(function(){
var HEADERS='#divRplyFwdMsg,#appendonsend,.OutlookMessageHeader,#zwchr,[id$="reference-message-container"],div[style*="border-top:solid #E1E1E1" i],div[style*="border-top: solid #E1E1E1" i],div[style*="border-top:solid #B5C4DF" i],div[style*="border-top: solid #B5C4DF" i]';
var MARKERS=HEADERS+',.gmail_quote_container,.gmail_quote,blockquote[type="cite"],.moz-cite-prefix,.yahoo_quoted,.protonmail_quote';
var RULE=/[-_=*]{5,}/,LABEL=/^\s*[^\s:：]{1,30}(\s[^\s:：]{1,30}){0,2}\s*[:：]\s*\S/;
var body=document.body;if(!body)return;
function text(n){return(n.textContent||'').replace(/\s+/g,' ').trim()}
function attribution(t){return t.length>0&&t.length<=300&&/[:：]$/.test(t)}
function ignored(n){return n.nodeType===1&&(n.tagName==='SCRIPT'||n.tagName==='STYLE'||n.hasAttribute('data-gofer-quote-toggle'))}
function content(n,images){if(n.nodeType===3)return /\S/.test(n.nodeValue);if(n.nodeType!==1||ignored(n))return false;if(/\S/.test(n.textContent||''))return true;return images&&(n.tagName==='IMG'||!!n.querySelector('img'))}
function siblings(el,dir){var out=[];for(var n=el;n&&n!==body;n=n.parentNode){for(var s=n[dir];s;s=s[dir])out.push(s)}return out}
function any(nodes,images){for(var i=0;i<nodes.length;i++)if(content(nodes[i],images))return true;return false}
// Start the quote at the outermost box it opens, so a wrapper's border or
// spacing (Outlook's separator rule) goes with it.
function lift(el){while(el.parentNode&&el.parentNode!==body){var p=el.previousSibling,first=true;for(;p;p=p.previousSibling)if(content(p,true)){first=false;break}if(!first)break;el=el.parentNode}return el}
function start(el){var p=el.previousElementSibling;if(el.matches('blockquote')&&p&&attribution(text(p)))el=p;p=el.previousElementSibling;if(p&&p.tagName==='HR')el=p;return lift(el)}
// quoted reports whether a node holds only quoted blocks, perhaps inside wrappers.
function quoted(n){if(!content(n,false))return true;if(n.nodeType!==1)return false;if(n.matches('blockquote,'+MARKERS))return true;var has=false;for(var c=n.firstChild;c;c=c.nextSibling){if(!content(c,false))continue;if(!quoted(c))return false;has=true}return has}
// A forward opens with a drawn rule ("---------- Forwarded message ---------").
function forwarded(el){return RULE.test(text(el).slice(0,200))}
function usable(el,header){if(!header&&forwarded(el))return false;if(!any(siblings(el,'previousSibling'),true))return false;if(header)return true;var next=siblings(el,'nextSibling');for(var i=0;i<next.length;i++)if(!quoted(next[i]))return false;return true}
function collapse(el,nodes){var hidden=[];nodes.forEach(function(n){if(n.nodeType===3){if(!/\S/.test(n.nodeValue))return;var s=document.createElement('span');n.parentNode.insertBefore(s,n);s.appendChild(n);n=s}if(n.nodeType!==1||ignored(n))return;hidden.push(n)});if(!hidden.length)return;
var wrap=document.createElement('div');wrap.setAttribute('data-gofer-quote-toggle','');wrap.style.cssText='margin:10px 0;line-height:1';
var b=document.createElement('button');b.type='button';b.textContent='•••';b.title='Show quoted text';b.setAttribute('aria-expanded','false');
b.style.cssText='font:700 11px/1 sans-serif;letter-spacing:2px;padding:3px 8px 4px;border-radius:6px;border:1px solid currentColor;background:transparent;color:inherit;opacity:.5;cursor:pointer';
wrap.appendChild(b);el.parentNode.insertBefore(wrap,el);
function set(open){hidden.forEach(function(n){if(open)n.style.removeProperty('display');else n.style.setProperty('display','none','important')});b.setAttribute('aria-expanded',open?'true':'false');b.title=open?'Hide quoted text':'Show quoted text'}
b.addEventListener('click',function(){set(b.getAttribute('aria-expanded')!=='true')});set(false)}
function marked(){var m=body.querySelector('[data-gofer-quote-start]');if(!m)return false;collapse(m,[m].concat(siblings(m,'nextSibling')));return true}
function html(){var found=body.querySelectorAll(MARKERS),seen=[];for(var j=0;j<found.length;j++){var s=start(found[j]);if(seen.indexOf(s)!==-1)continue;seen.push(s);if(usable(s,found[j].matches(HEADERS))){collapse(s,[s].concat(siblings(s,'nextSibling')));return true}}return false}
function quotedFrom(lines,i){var rest=lines.slice(i).filter(function(x){return /\S/.test(x)});return rest.length>0&&rest.every(function(x){return /^\s*>/.test(x)})}
// at finds where a plain-text quote begins: the first ">" line that only ">"
// lines follow, with the short colon-ended attribution above it (which may wrap
// onto two lines), or a drawn rule followed by "Label: value" header lines.
function at(lines){for(var j=1;j<lines.length;j++){var l=lines[j];
if(RULE.test(l)&&LABEL.test(lines[j+1]||'')&&LABEL.test(lines[j+2]||''))return j;
if(/^\s*>/.test(l)&&!/^\s*>/.test(lines[j-1])&&quotedFrom(lines,j)){var k=j-1;if(attribution(lines[k].trim())){if(k>0&&/\S/.test(lines[k-1])&&(lines[k-1]+lines[k]).length<=300)k--;return k}return j}}return -1}
// Plain-text bodies arrive as one <pre>: split it where the quote begins.
function plain(){var pres=body.getElementsByTagName('pre');for(var i=0;i<pres.length;i++){var pre=pres[i],lines=(pre.textContent||'').split('\n'),j=at(lines);
if(j<1||!/\S/.test(lines.slice(0,j).join(''))||any(siblings(pre,'nextSibling'),false))continue;
var tail=pre.cloneNode(false);tail.textContent=lines.slice(j).join('\n');pre.textContent=lines.slice(0,j).join('\n').replace(/\s+$/,'');pre.parentNode.insertBefore(tail,pre.nextSibling);collapse(tail,[tail]);return}}
try{if(!marked()&&!html())plain()}catch(_){}
})();</script>`)
}

// threadQuoteSources returns the messages of emailID's thread that came before
// it, which its body may quote. Copies of the message itself, such as the sent
// copy of a message that also arrived in the inbox, are left out.
func threadQuoteSources(ctx context.Context, db *storage.DB, emailID, userID string) []message.QuoteSource {
	email, err := db.GetEmailByIDForUser(ctx, emailID, userID)
	if err != nil || email == nil || email.ThreadID == "" {
		return nil
	}
	thread, err := db.GetThreadMessagesForUser(ctx, email.AccountID, email.ThreadID, userID)
	if err != nil {
		return nil
	}
	var sources []message.QuoteSource
	for _, item := range thread {
		if item.ID == emailID {
			return sources
		}
		if item.InternetMessageID != "" && item.InternetMessageID == email.InternetMessageID {
			continue
		}
		sources = append(sources, message.QuoteSource{Text: item.TextBody, Name: item.From.Name, Email: item.From.Email})
	}
	return nil
}
