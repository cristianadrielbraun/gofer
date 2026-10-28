const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')

const source = fs.readFileSync(path.join(__dirname, '../assets/js/settings-page.js'), 'utf8')
function between(start, end) {
  const first = source.indexOf(start), last = source.indexOf(end, first)
  assert(first >= 0 && last > first)
  return source.slice(first, last)
}
const footerSource = between('function setWizardFooter(', '// Accounts that send')
const saveSource = between('function saveWizardEmailService(', '// Calendar is the add')
const navigationSource = between('function editWizardEmailEnabled(', '// After a passing test:')
const stepsSource = between('function syncEditWizardEmailSteps(', '// Contacts and calendar stay usable')

function fixture({enabled = true, changed = false} = {}) {
  const requests = [], toasts = [], actions = [{disabled: false}, {disabled: false}]
  const steps = Array.from({length: 5}, () => ({disabled: false, dimmed: false,
    classList: {toggle(name, dimmed) { this.owner.dimmed = dimmed }}}))
  steps.forEach(step => { step.classList.owner = step })
  const toggle = {checked: enabled, disabled: false, dataset: {wizardServiceInitialEnabled: String(enabled)}}
  const switchForm = {dataset: {accountId: 'google/contacts'}}
  const unsaved = {hidden: true}, track = {style: {}}
  const footer = {dataset: {}}
  footer.children = ['0', '1', '2', '3', '4', '5', 'email-off'].map(step => ({
    dataset: {wizardFooterStep: step}, parentElement: footer,
    hasAttribute: name => name === 'data-wizard-footer-step',
    classList: {toggle() {}},
  }))
  const root = {
    id: 'edit-account-dialog',
    querySelector(selector) {
      return {
        '[data-wizard-service-switch="email"]': toggle,
        '[data-wizard-section-steps="0,1,2,3"]': {querySelectorAll: () => steps},
        '[data-wizard-footer-step="email-off"]': {querySelectorAll: () => actions},
        '[data-edit-account-unsaved]': unsaved,
        '.account-wizard-card-footer': footer,
      }[selector] || null
    },
    querySelectorAll: selector => selector === '[data-wizard-footer-step]' ? footer.children : [],
  }
  toggle.closest = selector => selector === '[data-wizard-service-form]' ? switchForm : root
  let resolveRequest, rejectRequest, tests = 0, syncs = 0
  const context = vm.createContext({
    URLSearchParams, _editWizStep: 0, _wizardFooterTimers: {},
    document: {getElementById: id => id === 'edit-wizard-track' ? track : root},
    window: {clearTimeout() {}, setTimeout: callback => callback()},
    wizardSkipSending(step, from, skip) { return step === 2 && skip ? (from > 2 ? 1 : 3) : step },
    editAccountChanged: () => changed,
    reconnectMissingFields: () => false,
    editAccountTest: {invalidate() {}, run() { tests++ }},
    syncWizardServiceContent() { syncs++ },
    showGoferToast(toast) { toasts.push(toast) },
    fetch(url, options) {
      requests.push({url, options})
      return new Promise((resolve, reject) => { resolveRequest = resolve; rejectRequest = reject })
    },
  })
  vm.runInContext(footerSource + saveSource + navigationSource + stepsSource, context)
  return {context, root, toggle, toasts, steps, actions, requests, footer, unsaved,
    resolve: response => resolveRequest(response), reject: error => rejectRequest(error),
    tests: () => tests, syncs: () => syncs,
    activeFooter: () => footer.children.find(item => item.dataset.active === 'true').dataset.wizardFooterStep}
}

async function main() {
  // The same immediate-save path handles disabling and enabling; no checkbox
  // serialization is involved, and users cannot leave via Close mid-save.
  for (const enabled of [false, true]) {
    const f = fixture({enabled: !enabled})
    f.toggle.checked = enabled
    const saving = f.context.saveWizardEmailService(f.toggle)
    assert.equal(f.toggle.disabled, true)
    assert(f.actions.every(button => button.disabled))
    const request = f.requests[0]
    assert.equal(request.url, '/api/accounts/google%2Fcontacts/services')
    assert.equal(request.options.method, 'POST')
    assert.equal(request.options.credentials, 'same-origin')
    assert.equal(request.options.body, `service=email&enabled=${enabled}`)
    f.resolve({ok: true})
    await saving
    assert.equal(f.toggle.dataset.wizardServiceInitialEnabled, String(enabled))
    assert.equal(f.toggle.checked, enabled)
    assert.equal(f.toggle.disabled, false)
    assert(f.actions.every(button => !button.disabled))
    assert.equal(f.activeFooter(), enabled ? '0' : 'email-off')
    assert.equal(f.tests(), 0)
    assert.equal(f.syncs(), 1)
    assert.equal(f.toasts.length, 0, 'Successful saves add no status messages')
    f.context.syncEditWizardEmailSteps(f.root)
    assert(f.steps.every(step => step.disabled === !enabled && step.dimmed === !enabled))
    assert.equal(f.toggle.disabled, false, 'The Email switch stays usable')
    f.toggle.checked = !enabled
    f.context.syncEditWizardEmailSteps(f.root)
    assert(f.steps.every(step => step.disabled === enabled && step.dimmed === enabled))
  }

  for (const initial of [false, true]) {
    for (const networkFailure of [false, true]) {
      const f = fixture({enabled: initial})
      f.toggle.checked = !initial
      const saving = f.context.saveWizardEmailService(f.toggle)
      if (networkFailure) f.reject(new Error('Network unavailable'))
      else f.resolve({ok: false, text: async () => 'Worker refresh failed'})
      await saving
      assert.equal(f.toggle.checked, initial, 'Failed saves restore the previous switch state')
      assert.equal(f.toggle.dataset.wizardServiceInitialEnabled, String(initial))
      assert.equal(f.toggle.disabled, false)
      assert.equal(f.toasts.length, 1)
      assert.equal(f.toasts[0].variant, 'error')
      assert.match(f.toasts[0].description, /Network unavailable|Worker refresh failed/)
      assert.equal(f.activeFooter(), initial ? '0' : 'email-off')
    }
  }

  // With Email off, Next and Back never invoke a mail test. Contacts and
  // Calendar stay reachable even if there are unsubmitted mail fields.
  const off = fixture({enabled: false, changed: true})
  for (const step of [0, 1, 2, 3, 4, 5, 3]) {
    off.context.editWizardGo(step, true)
    assert.equal(off.context._editWizStep, step < 4 ? 0 : step)
    assert.equal(off.activeFooter(), step < 4 ? 'email-off' : String(step))
    assert.equal(off.unsaved.hidden, true)
  }
  assert.equal(off.tests(), 0)

  // Re-enabling Email restores the existing protection for unsaved mail edits.
  off.toggle.checked = true
  off.context.editWizardGo(4, true)
  assert.equal(off.context._editWizStep, 3)
  assert.equal(off.unsaved.hidden, false)
  assert.equal(off.tests(), 1)
  console.log('Account Email service: explicit boolean saves, failures, and Email-off navigation passed')
}
main().catch(error => { console.error(error); process.exitCode = 1 })
