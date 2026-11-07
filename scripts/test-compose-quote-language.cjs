// Replies and forwards quote the message in the sender's language: the
// "wrote:" line and the forward block's labels. "Re:" and "Fwd:" stay as every
// mail program reads them.
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const loadI18n = require('./i18n-test-helper.cjs')
const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const slice = (start, end) => source.slice(source.indexOf(start), source.indexOf(end))

const code = [
  slice('function composeQuoteAttribution(', 'function composeReferencesForReply('),
  slice('function composeReferencesForReply(', 'function focusComposePrefill('),
].join('\n')

function run(locale) {
  const context = vm.createContext({
    composeAddress: (name, email) => name ? name + ' <' + email + '>' : email,
    composeAccountEmail: () => 'me@example.test',
    composeDedupeAddresses: parts => parts.filter(Boolean).join(', '),
    composeNormalizeMessageID: id => id,
    _sanitizeComposeHTML: html => html,
    _composePlainToHTML: text => text,
    _escapeComposeHTML: text => String(text).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'),
  })
  loadI18n(context, locale)
  vm.runInContext(code, context)
  return context
}

const message = {account_id: 'a', subject: 'Plans', from_name: 'Ana', from_email: 'ana@example.test', date: 'D', to: 'me@example.test', body: 'Hi'}

const en = run('en')
let reply = en.composeValuesFromSource(message, 'reply')
assert.equal(reply.subject, 'Re: Plans')
assert.match(reply.body, /\nOn D, Ana <ana@example\.test> wrote:\n> Hi$/)
let forward = en.composeValuesFromSource(message, 'forward')
assert.equal(forward.subject, 'Fwd: Plans')
assert.match(forward.body, /---------- Forwarded message ----------\nFrom: Ana <ana@example\.test>\nDate: D\nSubject: Plans\nTo: me@example\.test\n\nHi$/)

const es = run('es')
reply = es.composeValuesFromSource(message, 'reply')
assert.equal(reply.subject, 'Re: Plans')
assert.match(reply.body, /\nEl D, Ana <ana@example\.test> escribió:\n> Hi$/)
assert.match(reply.html_body, /El D, Ana &lt;ana@example\.test&gt; escribió:/)
assert.match(es.composeValuesFromSource({...message, date: ''}, 'reply').body, /\nAna <ana@example\.test> escribió:\n/)
forward = es.composeValuesFromSource(message, 'forward')
assert.equal(forward.subject, 'Fwd: Plans')
assert.match(forward.body, /---------- Mensaje reenviado ----------\nDe: Ana <ana@example\.test>\nFecha: D\nAsunto: Plans\nPara: me@example\.test\n\nHi$/)
assert.match(forward.html_body, /Mensaje reenviado/)
// A forward already marked by Outlook in Spanish gets no second prefix.
assert.equal(es.composeValuesFromSource({...message, subject: 'RV: Plans'}, 'forward').subject, 'RV: Plans')

const zh = run('zh-cn')
reply = zh.composeValuesFromSource(message, 'reply')
assert.equal(reply.subject, 'Re: Plans')
assert.match(reply.body, /\nAna <ana@example\.test> 于 D 写道：\n> Hi$/)
assert.match(zh.composeValuesFromSource({...message, date: ''}, 'reply').body, /\nAna <ana@example\.test> 写道：\n/)
forward = zh.composeValuesFromSource(message, 'forward')
assert.equal(forward.subject, 'Fwd: Plans')
assert.match(forward.body, /---------- 转发的邮件 ----------\n发件人: Ana <ana@example\.test>\n日期: D\n主题: Plans\n收件人: me@example\.test\n\nHi$/)
// Replies and forwards already marked by Chinese mail programs, with an ASCII
// or a full-width colon, get no second prefix.
const subjectOf = (subject, mode) => zh.composeValuesFromSource({...message, subject}, mode).subject
assert.equal(subjectOf('答复: Plans', 'reply'), '答复: Plans')
assert.equal(subjectOf('回复：Plans', 'reply'), '回复：Plans')
assert.equal(subjectOf('转发：Plans', 'forward'), '转发：Plans')
assert.equal(subjectOf('转发: Plans', 'reply'), 'Re: 转发: Plans')
assert.equal(subjectOf('答复: Plans', 'forward'), 'Fwd: 答复: Plans')
assert.equal(subjectOf('回复率：本周', 'reply'), 'Re: 回复率：本周')

console.log('compose quote language: ok')
