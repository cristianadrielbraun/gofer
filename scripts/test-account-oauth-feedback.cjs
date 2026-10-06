const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
const start = source.indexOf('  function setupAccountResultFeedback() {')
const end = source.indexOf('  function setupProcessingStatus()', start)
assert(start >= 0 && end > start)

function feedback(search) {
  const toasts = [], replacements = []
  const context = vm.createContext({
    URLSearchParams,
    window: {
      location: {search, pathname: '/settings/accounts', hash: '#mail'},
      history: {state: {keep: true}, replaceState(state, title, url) { replacements.push({state, url}) }},
    },
    showGoferToast(toast) { toasts.push(toast) },
  })
  vm.runInContext(source.slice(start, end) + '\nsetupAccountResultFeedback()', context)
  return {toasts, replacements}
}

for (const error of ['oauth_invalid_state', 'oauth_expired_state', 'oauth_session_mismatch',
  'oauth_no_code', 'oauth_exchange_failed', 'oauth_userinfo_failed', 'oauth_email_mismatch',
  'oauth_identity_mismatch', 'oauth_store_failed', 'oauth_metadata_failed', 'oauth_sync_failed', 'create_failed']) {
  const result = feedback('?error=' + error + '&tab=mail')
  assert.equal(result.toasts.length, 1)
  assert.equal(result.toasts[0].variant, 'error')
  assert.equal(result.toasts[0].title, 'Could not connect account')
  assert.equal(result.replacements[0].url, '/settings/accounts?tab=mail#mail')
  assert.equal(result.replacements[0].state.keep, true)
}
const failedSave = feedback('?error=oauth_store_failed&account_added=1')
assert.match(failedSave.toasts[0].description, /Reconnect/)
assert.equal(failedSave.toasts[0].variant, 'error', 'A failed save must never show success')
for (const success of ['account_added', 'account_reconnected']) {
  const result = feedback('?' + success + '=1&tab=mail')
  assert.equal(result.toasts[0].variant, 'success')
  assert.equal(result.replacements[0].url, '/settings/accounts?tab=mail#mail')
}
const unrelated = feedback('?error=unrelated')
assert.equal(unrelated.toasts.length, 0)
assert.equal(unrelated.replacements.length, 0)
console.log('Account OAuth result feedback checks passed')
