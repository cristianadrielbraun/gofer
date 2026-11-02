// Loads assets/js/i18n.js into a test's vm context, so code that writes
// dates and numbers through goferDates, goferNumber and goferT can run.
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const source = fs.readFileSync(path.join(__dirname, '../assets/js/i18n.js'), 'utf8')

module.exports = function loadI18n(context, locale = 'en', dateFormat = 'auto', timeFormat = 'auto') {
  const messages = JSON.parse(fs.readFileSync(path.join(__dirname, '../internal/i18n/locales/' + locale + '.json'), 'utf8'))
  const window = { GOFER_I18N: { locale, messages } }
  vm.runInContext(source, vm.createContext({ window, document: { currentScript: { dataset: { dateFormat, timeFormat } } }, Intl, Date }))
  for (const name of ['goferT', 'goferTn', 'goferDates', 'goferNumber', 'goferLocale']) {
    context[name] = window[name]
    if (context.window) context.window[name] = window[name]
  }
  return context
}
