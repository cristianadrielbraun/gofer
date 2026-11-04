// Checks that goferDates in assets/js/i18n.js writes dates exactly as the
// server's i18n.DateFormatter does (see internal/i18n/dates_test.go).
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const source = fs.readFileSync(path.join(__dirname, '../assets/js/i18n.js'), 'utf8')
function catalog(locale) {
  return JSON.parse(fs.readFileSync(path.join(__dirname, '../internal/i18n/locales/' + locale + '.json'), 'utf8'))
}
function load(locale, dateFormat, timeFormat) {
  const window = { GOFER_I18N: { locale, messages: catalog(locale) } }
  const context = vm.createContext({ window, document: { currentScript: { dataset: { dateFormat, timeFormat } } }, Intl, Date })
  vm.runInContext(source, context)
  return window
}

const sample = new Date(Date.UTC(2026, 10, 2, 15, 4))
const end = new Date(Date.UTC(2026, 10, 9, 16, 30))
const now = new Date(Date.UTC(2026, 10, 2, 18, 0))

let w = load('en', 'auto', 'auto')
assert.equal(w.goferDates.date(sample, 'UTC'), 'Nov 2, 2026')
assert.equal(w.goferDates.shortDate(sample, 'UTC'), 'Nov 2')
assert.equal(w.goferDates.weekdayDate(sample, 'UTC'), 'Mon, Nov 2, 2026')
assert.equal(w.goferDates.longDate(sample, 'UTC'), 'Monday, November 2, 2026')
assert.equal(w.goferDates.monthYear(sample, 'UTC'), 'November 2026')
assert.equal(w.goferDates.time(sample, 'UTC'), '3:04 PM')
assert.equal(w.goferDates.dateTime(sample, 'UTC'), 'Nov 2, 2026, 3:04 PM')
assert.equal(w.goferDates.timeRange(sample, end, 'UTC'), '3:04 PM–4:30 PM')
assert.equal(w.goferDates.dateRange(sample, end, 'UTC'), 'Nov 2–9, 2026')
assert.equal(w.goferDates.clockTime(24, 0), '12:00 AM')
assert.equal(w.goferDates.hourLabel(0), '12 AM')
assert.equal(w.goferDates.hourRange(3, 7), '3–7 AM')
assert.equal(w.goferDates.hourRange(11, 13), '11 AM–1 PM')
assert.equal(w.goferDates.relative(new Date(Date.UTC(2026, 10, 1, 9)), now, 'UTC'), 'Yesterday')
assert.equal(w.goferNumber(1234), '1,234')

w = load('es', 'auto', 'auto')
assert.equal(w.goferDates.date(sample, 'UTC'), '2 nov 2026')
assert.equal(w.goferDates.weekdayDate(sample, 'UTC'), 'lun, 2 nov 2026')
assert.equal(w.goferDates.longDate(sample, 'UTC'), 'lunes, 2 de noviembre de 2026')
assert.equal(w.goferDates.monthYear(sample, 'UTC'), 'noviembre de 2026')
assert.equal(w.goferDates.time(sample, 'UTC'), '15:04')
assert.equal(w.goferDates.dateTime(sample, 'UTC'), '2 nov 2026, 15:04')
assert.equal(w.goferDates.dateRange(sample, end, 'UTC'), '2–9 nov 2026')
assert.equal(w.goferDates.hourRange(3, 7), '03–07')
assert.equal(w.goferDates.relative(new Date(Date.UTC(2026, 10, 1, 9)), now, 'UTC'), 'Ayer')
assert.equal(w.goferNumber(12345), '12.345')

const formats = [
  ['dd-mm-yyyy', '24h', '02-11-2026', '02-11', '02-11-2026, 15:04'],
  ['mm/dd/yyyy', 'auto', '11/02/2026', '11/02', '11/02/2026, 3:04 PM'],
  ['dd.mm.yyyy', 'auto', '02.11.2026', '02.11', '02.11.2026, 3:04 PM'],
  ['yyyy-mm-dd', 'auto', '2026-11-02', '11-02', '2026-11-02, 3:04 PM'],
  ['ddd-d-mmm-yyyy', 'auto', 'Mon, 2 Nov 2026', 'Mon, 2 Nov', 'Mon, 2 Nov 2026, 3:04 PM'],
]
for (const [format, clock, date, short, dateTime] of formats) {
  w = load('en', format, clock)
  assert.equal(w.goferDates.date(sample, 'UTC'), date, format)
  assert.equal(w.goferDates.shortDate(sample, 'UTC'), short, format)
  assert.equal(w.goferDates.dateTime(sample, 'UTC'), dateTime, format)
}

console.log('i18n dates: ok')
