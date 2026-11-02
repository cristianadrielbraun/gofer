const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const root = path.join(__dirname, '..')
const source = fs.readFileSync(path.join(root, 'assets/js/app.js'), 'utf8')
const start = source.indexOf('  function newMailNotificationAppearance(')
const end = source.indexOf('  function notificationAppearanceExample(', start)
assert(start > 0 && end > start)
const context = { window: {}, GoferSettings: { get(key) { return context.settings[key] } }, settings: {} }
context.window.GoferSettings = context.GoferSettings
vm.createContext(context)
vm.runInContext(source.slice(start, end), context)
const cases = JSON.parse(fs.readFileSync(path.join(root, 'internal/notifications/testdata/appearance.json'), 'utf8'))
for (const tc of cases) {
  context.settings = tc.settings
  const actual = context.newMailNotificationAppearance(tc.payload, tc.count)
  assert.deepEqual(JSON.parse(JSON.stringify(actual)), { title: tc.title, body: tc.body, icon: tc.icon }, tc.name)
}

// An intentionally empty body must stay empty through the service worker.
let onPush
let onInstall
let skippedWaiting = false
let displayed
let pending
vm.runInNewContext(fs.readFileSync(path.join(root, 'assets/js/sw.js'), 'utf8'), {
  importScripts() {},
  self: {
    skipWaiting() { skippedWaiting = true; return Promise.resolve() },
    GoferNotificationAvatar: { render() { return Promise.resolve('data:image/png;base64,circle') } },
    addEventListener(name, handler) {
      if (name === 'push') onPush = handler
      if (name === 'install') onInstall = handler
    },
    registration: { showNotification(title, options) { displayed = { title, options }; return Promise.resolve() } },
  },
})
onPush({ data: { json() { return { title: 'Gofer', body: '' } } }, waitUntil(promise) { pending = promise } })
pending.then(async function () {
  assert.equal(displayed.options.body, '', 'Hidden content must not be replaced by fallback text')
  assert.equal(displayed.options.icon, 'data:image/png;base64,circle', 'Push must use the rendered circular icon')
  let installation
  onInstall({ waitUntil(promise) { installation = promise } })
  await installation
  assert(skippedWaiting, 'Notification worker updates should activate while Gofer is open')

  const workerStart = source.indexOf('  function waitForNotificationWorker(')
  const workerEnd = source.indexOf('  function ensureWebPushSubscription(', workerStart)
  vm.runInContext(source.slice(workerStart, workerEnd), context)
  let listener
  let timeout
  context.setTimeout = function (callback) { timeout = callback; return 1 }
  context.clearTimeout = function () { timeout = null }
  const worker = {
    state: 'installing',
    addEventListener(name, handler) { listener = handler },
    removeEventListener() { listener = null },
  }
  const registration = { installing: worker, active: { state: 'activated' } }
  let activated = false
  const ready = context.waitForNotificationWorker(registration).then(function (value) { activated = true; return value })
  await Promise.resolve()
  assert.equal(activated, false, 'An old active worker must not bypass its installing replacement')
  worker.state = 'activated'
  listener()
  assert.equal(await ready, registration)
  assert.equal(listener, null)
  assert.equal(timeout, null)
  worker.state = 'installing'
  const failed = context.waitForNotificationWorker(registration)
  worker.state = 'redundant'
  listener()
  await assert.rejects(failed, /could not activate/)
  worker.state = 'installing'
  const delayed = context.waitForNotificationWorker(registration)
  timeout()
  await assert.rejects(delayed, /taking too long/)
  console.log(`Notification appearance: ${cases.length} shared browser/push cases and circular push icon passed.`)
}).catch(function (err) { console.error(err); process.exitCode = 1 })
