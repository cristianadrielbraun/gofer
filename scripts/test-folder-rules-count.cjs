const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

// The number of rules beside the Rules item of a folder's menus, loaded when
// the menu opens (loadFolderRulesCount in app.js).
const app = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const start = app.indexOf('function loadFolderRulesCount(')
const end = app.indexOf("// Opening a folder's menu in the account settings' folders dialog", start)
assert(start >= 0 && end > start)

// Each request waits until the test answers it.
const requests = []
const fetch = url => new Promise((resolve, reject) => requests.push({url, answer: count => resolve({ok: true, json: () => Promise.resolve({count})}), fail: status => resolve({ok: false, status})}))
const context = vm.createContext({fetch, Promise, console: {warn() {}}, encodeURIComponent, String})
vm.runInContext(app.slice(start, end), context)

const badge = () => ({
  textContent: '', hidden: false, attrs: {loading: ''},
  setAttribute(name) { this.attrs[name.replace('data-', '')] = '' },
  removeAttribute(name) { delete this.attrs[name.replace('data-', '')] },
})

;(async () => {
  const pill = badge()
  let done = context.loadFolderRulesCount(pill, 'acc_f_receipts')
  assert.equal(requests.at(-1).url, '/api/settings/rules/folder-count?folder=acc_f_receipts')
  assert.equal(pill.hidden, false, 'a blank pill shows while the number loads')
  assert.ok('loading' in pill.attrs)
  requests.at(-1).answer(3)
  await done
  assert.equal(pill.textContent, '3', 'the number shows once it comes')
  assert.ok(!('loading' in pill.attrs))

  // The menu opened again on another folder before the first answer came.
  const first = context.loadFolderRulesCount(pill, 'acc_f_receipts')
  const firstRequest = requests.at(-1)
  const second = context.loadFolderRulesCount(pill, 'acc_f_trips')
  assert.equal(pill.textContent, '', 'opening again clears the old number')
  requests.at(-1).answer(0)
  await second
  firstRequest.answer(5)
  await first
  assert.equal(pill.textContent, '0', 'a late answer for the folder before is dropped, and none is a number too')

  // A failure leaves no badge; the next opening tries again.
  done = context.loadFolderRulesCount(pill, 'acc_f_trips')
  requests.at(-1).fail(500)
  await done
  assert.equal(pill.hidden, true, 'a failure hides the badge')
  done = context.loadFolderRulesCount(pill, 'acc_f_trips')
  assert.equal(pill.hidden, false, 'and the next opening shows it again')
  requests.at(-1).answer(1)
  await done
  assert.equal(pill.textContent, '1')

  // No folder, no request.
  const asked = requests.length
  await context.loadFolderRulesCount(badge(), '')
  assert.equal(requests.length, asked)
  console.log('Folder rules count: number, late answers, failures and no folder passed.')
})().catch(err => { console.error(err); process.exit(1) })
