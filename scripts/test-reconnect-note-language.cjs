// After a password is saved in the edit dialog, the note lists the logins that
// still need one in the page's language: "Mail, Calendar and Contacts",
// "邮件、日历 和 联系人".
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const loadI18n = require('./i18n-test-helper.cjs')

const source = fs.readFileSync(path.join(__dirname, '../assets/js/settings-page.js'), 'utf8')
const marker = source.indexOf("var login = xhr && xhr.getResponseHeader('X-Gofer-Reconnect-Resolved');")
const start = source.lastIndexOf("document.addEventListener('htmx:afterRequest'", marker)
const end = source.indexOf("document.addEventListener('htmx:afterSettle'", marker)
assert(marker >= 0 && start >= 0 && end > marker)
const listenerSource = source.slice(start, end)

function note(locale, remaining) {
  let listener
  const text = {textContent: ''}
  const pending = {hidden: false, querySelector: selector => selector === '[data-reconnect-note]' ? text : null}
  const root = {
    querySelectorAll: () => [],
    querySelector: selector => selector === '[data-edit-account-reconnect-pending]' ? pending : null,
  }
  const context = vm.createContext({
    document: {
      addEventListener(type, fn) { if (type === 'htmx:afterRequest') listener = fn },
      getElementById: id => id === 'edit-account-dialog' ? root : null,
    },
    updateReconnectGates() {},
  })
  loadI18n(context, locale)
  vm.runInContext(listenerSource, context)
  const headers = {'X-Gofer-Reconnect-Resolved': 'mail', 'X-Gofer-Reconnect-Remaining': remaining}
  listener({detail: {xhr: {getResponseHeader: name => headers[name] || null}}})
  return text.textContent
}

assert.equal(note('en', 'calendar'), 'Saved. Calendar still needs its password, in the steps marked with !.')
assert.equal(note('en', 'calendar,contacts'), 'Saved. Calendar and Contacts still need their passwords, in the steps marked with !.')
assert.equal(note('en', 'mail,calendar,contacts'), 'Saved. Mail, Calendar and Contacts still need their passwords, in the steps marked with !.')
assert.equal(note('es', 'mail,calendar,contacts'), 'Guardado. Correo, Calendario y Contactos todavía necesitan sus contraseñas, en los pasos marcados con !.')
assert.equal(note('zh-cn', 'calendar,contacts'), '已保存。日历 和 联系人 仍需要密码，请在标有 ! 的步骤中填写。')
assert.equal(note('zh-cn', 'mail,calendar,contacts'), '已保存。邮件、日历 和 联系人 仍需要密码，请在标有 ! 的步骤中填写。')

console.log('reconnect note language: ok')
