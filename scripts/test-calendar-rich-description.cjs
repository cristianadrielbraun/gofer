const assert = require('node:assert/strict')
const fs = require('node:fs')
const vm = require('node:vm')
const source = fs.readFileSync(require('node:path').join(__dirname, '../assets/js/app.js'), 'utf8')
function fn(name) {
  const start = source.indexOf('function ' + name + '(')
  const end = source.indexOf('\n}', start)
  assert(start > 0 && end > start)
  return source.slice(start, end + 2)
}
const fields = {description: {value: ''}, description_html: {value: ''}}
const commands = [], restored = [], saved = []
let locked = false, nextURL = 'https://example.com', focused = 0
const editor = {
  innerHTML: '<p><b>Agenda</b></p>', innerText: 'Agenda',
  closest() { return form }, focus() { focused++ },
  getAttribute() { return locked ? 'false' : 'true' },
}
const form = {
  querySelector(selector) { return selector === '[data-calendar-rich-editor]' ? editor : fields[selector.match(/name="([^"]+)"/)[1]] },
  querySelectorAll() { return [] },
}
const runtime = vm.createContext({
  window: {prompt() { return nextURL }},
  document: {
    createElement() { return {innerHTML: '', content: {querySelectorAll() { return [] }}} },
    getElementById() { return form },
    execCommand(...args) { commands.push(args) },
  },
  _sanitizeComposeHTML(html) { return html }, _composeEditorText(node) { return node.innerText },
  _saveComposeSelection(node) { saved.push(node) }, _restoreComposeSelection(node) { restored.push(node) },
})
vm.runInContext(['syncCalendarDescription', 'calendarDescriptionSelection', 'calendarDescriptionExec'].map(fn).join('\n'), runtime)
runtime.syncCalendarDescription(editor)
assert.equal(fields.description_html.value, editor.innerHTML, 'send HTML, not only the visible text')
assert.equal(fields.description.value, 'Agenda', 'retain the plain fallback')
editor.innerHTML = '<p>Meeting ID: 123 — changed by the organizer</p>'
editor.innerText = 'Meeting ID: 123 — changed by the organizer'
runtime.syncCalendarDescription(editor)
assert.equal(fields.description_html.value, editor.innerHTML, 'original meeting description is editable and serialized in full')
runtime.calendarDescriptionExec(editor, 'bold')
assert.equal(commands[0][0], 'bold')
assert.equal(restored[0], editor, 'format the saved selection after toolbar focus')
assert(saved.includes(editor))
runtime.calendarDescriptionExec({closest() { return null }}, 'formatBlock', 'blockquote')
assert.equal(commands.at(-1)[2], 'blockquote', 'portaled dropdown formatting finds the calendar editor, not the mail composer')
nextURL = 'javascript:alert(1)'
const before = commands.length
runtime.calendarDescriptionExec(editor, 'createLink')
assert.equal(commands.length, before, 'reject active URL schemes')
nextURL = 'guest@example.com'
runtime.calendarDescriptionExec(editor, 'createLink')
assert.equal(commands.at(-1)[2], 'mailto:guest@example.com')
locked = true
runtime.calendarDescriptionExec(editor, 'italic')
assert.equal(commands.length, before + 1, 'busy or uncertain forms cannot format content')
assert(focused > 0)
console.log('Calendar rich description: HTML/plain serialization, selection, formatting, portaled toolbar, link safety and frozen saves passed.')
