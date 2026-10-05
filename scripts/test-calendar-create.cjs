const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const source = fs.readFileSync(path.join(__dirname, '../assets/js/app.js'), 'utf8')
function helper(name) {
  const start = source.indexOf('function ' + name + '('), end = source.indexOf('\n}', start)
  assert(start > 0 && end > start, 'Missing helper ' + name)
  return source.slice(start, end + 2)
}
const inputs = ['source_id', 'request_id', 'summary', 'start_date', 'end_date', 'start_time', 'end_time', 'timezone', 'all_day', 'version', 'description', 'location'].map(name => ({name, value: name, disabled: false, required: false, checked: false}))
const sourceInput = inputs[0]
const sourceChoice = {dataset: {tuiSelectboxValue: sourceInput.value, tuiSelectboxDisabled: 'false', calendarSourceWritable: 'true', calendarSourceAuthorized: 'true'}}
const sourceTrigger = {disabled: false}
const choices = [sourceChoice]
const sourceSelect = {querySelectorAll() { return choices }, querySelector() { return sourceTrigger }}
const node = () => ({hidden: false, textContent: '', setAttribute(name, value) { this[name] = value }, focus() { this.focused = true }})
const error = node(), access = node(), submit = node(), spinner = node(), label = node(), timezone = node(), help = node()
const times = inputs.filter(input => ['start_time', 'end_time'].includes(input.name)).map(input => ({hidden: false, querySelector() { return input }}))
const pickerTriggers = [{disabled: false}, {disabled: false}, {disabled: false}, {disabled: false}, {disabled: false}]
const endDate = inputs.find(input => input.name === 'end_date')
const monthChanges = []
const monthInput = {value: '', dispatchEvent(event) { monthChanges.push(event.type) }}
const yearInput = {value: ''}
const endCalendar = {dataset: {}, setAttribute(name, value) { this[name] = value }, querySelector(selector) { return selector.includes('month-select') ? monthInput : yearInput }}
endDate.closest = () => ({querySelector() { return endCalendar }})
times.forEach((node, i) => { node.querySelector = selector => selector === 'input' ? inputs.find(input => input.name === ['start_time', 'end_time'][i]) : pickerTriggers[i + 2] })
const form = {
  closest(){return null}, dataset: {}, action: '/api/calendar/events',
  isConnected: true, reportValidity() { return true }, setAttribute(name, value) { this[name] = value },
  querySelector(selector) {
    const name = selector.match(/^\[name="([^"]+)"\]$/)?.[1]
    if (name) return inputs.find(input => input.name === name)
    if (selector === '[data-calendar-create-source-select]') return sourceSelect
    return {'[data-calendar-create-error]': error, '[data-calendar-create-access]': access, '[data-calendar-create-submit]': submit,
      '[data-calendar-create-spinner]': spinner, '[data-calendar-create-submit-label]': label, '[data-calendar-create-timezone]': timezone, '[data-calendar-create-date-help]': help}[selector]
  }, querySelectorAll(selector) {
    if (selector === 'input, select, textarea') return inputs
    if (selector === '[data-calendar-create-time]') return times
    if (selector === '[data-tui-popover-content]') return []
    return pickerTriggers
  },
}
const requests = [], toasts = [], closed = []
let refreshes = 0
const calendar = {dataset: {calendarPeriod: 'week:2026-09-28', calendarView: 'week', calendarDate: '2026-10-02', calendarTodayDate: '2026-10-02', calendarMonth: '2026-10'}}
const formListeners = {}
const context = vm.createContext({initializeCalendarDialogResize(){},
  document: {getElementById() { return calendar }, querySelector() { return form }, addEventListener(type, listener) { formListeners[type] = listener }}, URLSearchParams, Event: class {constructor(type) { this.type = type }},
  FormData: class {constructor() { this.entries = inputs.filter(input => !input.disabled && (input.name !== 'all_day' || input.checked) && (input.name !== 'version' || form.dataset.calendarEventId)).map(input => [input.name, input.value]) } [Symbol.iterator]() { return this.entries[Symbol.iterator]() }},
  window: {crypto: {randomUUID() { return 'fresh-request' }}, tui: {dialog: {close(id) { closed.push(id) }}}},
  fetch(url, options) { return new Promise((resolve, reject) => requests.push({url, options, resolve, reject})) },
  showGoferToast(data) { toasts.push(data) }, scheduleCalendarCacheRefresh() { refreshes++ },
})
vm.runInContext('var _calendarSelectedDay = {period: "week:2026-09-28", date: "2026-10-03"};\n' +
  ['configureCalendarCreateDialog', 'adjustCalendarCreateAllDayRange', 'updateCalendarRecurrenceForm', 'updateCalendarTeamsForm', 'updateCalendarCreateForm', 'initializeCalendarCreateForm', 'validateCalendarCreatePickers', 'setCalendarCreateError', 'syncCalendarDescription', 'submitCalendarCreate'].map(helper).join('\n'), context)
vm.runInContext(source.slice(source.indexOf('document.addEventListener("submit",', source.indexOf('function submitCalendarCreate(')), source.indexOf('function updateCalendarResponseForm(')), context)
for (const picker of ['datepicker', 'timepicker']) {
  const module = fs.readFileSync(path.join(__dirname, '../assets/js/' + picker + '.js'), 'utf8')
  const start = module.indexOf('  function closePopover('), end = module.indexOf('\n  }', start)
  assert(start >= 0 && end > start)
  const calls = []
  const content = {matches() { return true }, hidePopover() { throw new Error('Bypassing popup state prevents reopening with one click') }}
  const root = {querySelector() { return content }}
  const runtime = vm.createContext({findRoot() { return root }, window: {tui: {popover: {closeElement(node) { calls.push(node) }}}}})
  vm.runInContext(module.slice(start, end + 4), runtime)
  runtime.closePopover(root)
  assert.deepEqual(calls, [content], picker + ' must close through the templUI popup controller')
}

function testDialogLifecycle() {
  const handlers = {}, calls = [], notices = [], animations = [], reveals = [], timers = []
  let reducedMotion = false, sameSize = false
  const input = {isConnected: true, focusCount: 0, focus() { this.focusCount++ }}
  const panel = {
    isConnected: true, inert: false, style: {opacity: '', visibility: ''},
    animate(frames, timing) { const animation = {frames, timing}; reveals.push(animation); return animation },
  }
  const calendar = {dataset: {calendarPeriod: 'week:2026-09-28'}, loading: false, hasAttribute() { return this.loading }}
  const details = {
    open: true, isConnected: true, closing: false, attributes: {}, style: {transitionProperty: '', willChange: ''},
    hasAttribute(name) { return name === 'data-tui-dialog-closing' ? this.closing : name in this.attributes },
    setAttribute(name, value) { this.attributes[name] = value }, removeAttribute(name) { delete this.attributes[name] },
    closest() { return detailsRoot }, close() { calls.push('native close'); this.open = false },
    querySelector(selector) { return selector === '[data-tui-dialog-panel]' ? panel : selector === '#calendar-create-summary' ? input : null },
    getBoundingClientRect() {
      if (this.className !== editor.className || sameSize) return {width: 576, height: 320}
      assert.equal(this.style.transitionProperty, 'none', 'Measure the final layout without a CSS width or entrance-scale transition')
      assert.equal(calls.at(-1), 'initialize', 'Measure only after form initialization has settled field visibility')
      return {width: 512, height: 640}
    },
    animate(frames, timing) { const animation = {frames, timing}; animations.push(animation); return animation },
  }
  const detailsRoot = {id: 'calendar-event-details-dialog', querySelector() { return details }}
  const editor = {className: 'editor-dialog-classes', getAttribute(name) { return name === 'aria-labelledby' ? 'calendar-create-title' : null }, querySelector() { return {} }}
  const runtime = vm.createContext({
    document: {
      body: {addEventListener(type, handler) { (handlers[type] ||= []).push(handler) }},
      getElementById(id) { return id === 'calendar-main' ? calendar : id === 'calendar-create-summary' ? input : id === detailsRoot.id ? detailsRoot : null },
    },
    DOMParser: class {parseFromString(html) { return {querySelector() { return html === 'edit response' ? editor : null }} }},
    window: {matchMedia() { return {matches: reducedMotion} }, tui: {dialog: {
      close(id) { calls.push('close ' + id); details.closing = true },
      open(id) { calls.push('open ' + id) },
    }}},
    initializeCalendarCreateForm() { calls.push('initialize') },
    showGoferToast(toast) { notices.push(toast) },
    setTimeout(callback) { timers.push(callback) },
  })
  vm.runInContext(source.slice(source.indexOf('var _calendarCreateDialogRequest = null'), source.indexOf('function adjustCalendarCreateAllDayRange(')), runtime)
  function fire(type, detail) { for (const handler of handlers[type] || []) handler({detail}) }
  function begin(editing = true) {
    const attributes = new Set(['data-calendar-create-trigger', ...(editing ? ['data-calendar-edit-trigger'] : [])])
    const elt = {hasAttribute(name) { return attributes.has(name) }, setAttribute(name) { attributes.add(name) }, removeAttribute(name) { attributes.delete(name) }}
    const detail = {elt, xhr: {}}
    fire('htmx:beforeRequest', detail)
    assert(elt.hasAttribute('aria-busy'))
    return detail
  }
  function complete(detail, successful = true, shouldSwap = successful, serverResponse = 'edit response') {
    const swap = {...detail, shouldSwap, isError: !successful, serverResponse}
    fire('htmx:beforeSwap', swap)
    if (swap.shouldSwap) {
      if (detail.xhr.goferCalendarEdit) {
        assert.equal(swap.target, details, 'HTMX must keep the same native dialog and replace only its contents')
        assert.equal(swap.swapOverride, 'innerHTML')
        assert.equal(swap.selectOverride, '#calendar-create-dialog [data-tui-dialog-content] > [data-tui-dialog-panel]')
        assert.equal(details.open, true, 'The native backdrop must stay open throughout the swap')
        assert.equal(details.closing, false, 'The backdrop must not start its closing animation')
      }
      fire('htmx:afterSwap', {...detail, target: swap.target})
    }
    fire('htmx:afterRequest', {...detail, successful})
    assert.equal(detail.elt.hasAttribute('aria-busy'), false)
    return swap.shouldSwap
  }
  function reset() {
    Object.assign(details, {open: true, isConnected: true, closing: false, className: 'details-dialog-classes', attributes: {id: 'calendar-event-details-content', 'aria-labelledby': 'calendar-event-details-title'}})
    details.style.willChange = ''
    detailsRoot.id = 'calendar-event-details-dialog'
    calendar.dataset.calendarPeriod = 'week:2026-09-28'
    calendar.loading = false
    reducedMotion = sameSize = false
    panel.isConnected = true
    panel.style.opacity = ''
    panel.style.visibility = ''
    panel.inert = false
    input.focusCount = 0
    calls.length = notices.length = animations.length = reveals.length = timers.length = 0
  }

  reset()
  assert.equal(complete(begin()), true)
  assert.deepEqual(calls, ['initialize'], 'Editing must not close or reopen the native dialog or its backdrop')
  assert.equal(details.open, true)
  assert.equal(details.isConnected, true)
  assert.equal(detailsRoot.id, 'calendar-create-dialog', 'Close/save actions must address the reused editor dialog')
  assert.equal(details.className, editor.className, 'The reused dialog must adopt the editor layout')
  assert.equal(details.attributes['aria-labelledby'], 'calendar-create-title')
  assert.equal(details.hasAttribute('id'), false, 'Remove the old details-only content ID')
  assert.equal(animations.length, 1)
  assert.deepEqual(JSON.parse(JSON.stringify(animations[0].frames)), [
    {scale: '1.125 0.5'},
    {scale: '1 1'},
  ], 'Use only composited scale: never animate layout dimensions or override the centering translation')
  assert.equal(details.style.willChange, 'scale')
  assert.equal(animations[0].timing.duration, 220)
  assert.equal(animations[0].timing.easing, 'ease', 'Match the softer templUI tabs cadence rather than a sharp ease-out')
  assert.equal(animations[0].timing.fill, undefined, 'Return to responsive CSS sizing when the animation finishes')
  assert.equal(details.style.transitionProperty, '', 'Restore normal dialog transitions after measuring')
  assert.equal(details.style.width, undefined, 'Do not leave a fixed width on the dialog')
  assert.equal(details.style.height, undefined, 'Do not leave a fixed height on the dialog')
  assert.equal(panel.style.opacity, '0', 'Hide header, body and footer together as one content layer')
  assert.equal(panel.style.visibility, '', 'Do not trigger inherited visibility transitions on templUI buttons')
  assert.equal(panel.inert, true, 'Hidden controls must not be focusable or interactive')
  assert.equal(timers.length, 0, 'Do not schedule early focus while the editor is hidden')
  assert.equal(reveals.length, 0, 'The content must not fade in before resizing finishes')
  animations[0].onfinish()
  assert.equal(details.style.willChange, '', 'Release the compositor hint when resizing finishes')
  assert.equal(panel.style.opacity, '')
  assert.equal(panel.inert, false)
  assert.equal(reveals.length, 1)
  assert.deepEqual(JSON.parse(JSON.stringify(reveals[0].frames)), [{opacity: 0}, {opacity: '1'}])
  assert.equal(reveals[0].timing.duration, 160)
  assert.equal(reveals[0].timing.easing, 'ease', 'Bring the contents back with the same gentle acceleration')
  assert.equal(input.focusCount, 0)
  reveals[0].onfinish()
  assert.equal(input.focusCount, 1, 'Focus the title only after the content is visible again')
  reset()
  complete(begin())
  animations[0].oncancel()
  assert.equal(details.style.willChange, '', 'A cancelled animation must restore the compositor hint')
  assert.equal(panel.style.opacity, '', 'A cancelled resize must not leave hidden content')
  assert.equal(panel.inert, false)
  assert.equal(reveals.length, 0)
  for (const dismiss of [() => { details.closing = true }, () => { details.open = false }, () => { details.isConnected = false }, () => { panel.isConnected = false }]) {
    reset()
    complete(begin())
    dismiss()
    animations[0].onfinish()
    assert.equal(panel.style.opacity, '')
    assert.equal(panel.inert, false)
    assert.equal(reveals.length, 0, 'Do not animate contents of a dismissed or replaced dialog')
    assert.equal(input.focusCount, 0, 'A late animation must not steal focus from another dialog')
  }
  reset()
  reducedMotion = true
  complete(begin())
  assert.equal(animations.length, 0, 'Respect reduced motion without animating the resize')
  assert.equal(details.open, true)
  assert.equal(details.style.transitionProperty, '')
  assert.equal(panel.style.opacity, '', 'Reduced motion must show the editor immediately')
  assert.equal(panel.inert, false)
  reset()
  sameSize = true
  complete(begin())
  assert.equal(animations.length, 0, 'Do not animate an unchanged dialog size')
  assert.equal(panel.style.opacity, '', 'Do not hide contents when no resize is needed')
  reset()
  complete(begin(false))
  assert.deepEqual(calls, ['initialize', 'open calendar-create-dialog'], 'Creation keeps the shared dialog lifecycle')
  assert.equal(animations.length, 0, 'New events must keep the normal entrance animation')
  reset()
  complete(begin(), false)
  assert.equal(details.open, true, 'Failed edit loads must leave details available')
  assert.deepEqual(calls, [])
  assert.equal(notices[0].title, 'Could not open Edit event')
  reset()
  assert.equal(complete(begin(), true, true, 'invalid response'), false)
  assert.equal(detailsRoot.id, 'calendar-event-details-dialog', 'An invalid response must preserve the details dialog')
  assert.equal(details.open, true)
  assert.deepEqual(calls, [])
  reset()
  const aborted = begin()
  fire('htmx:sendAbort', aborted)
  fire('htmx:afterSwap', aborted)
  assert.equal(aborted.xhr.goferCalendarCreateSwapped, undefined, 'Late afterSwap must not revive an aborted editor')
  assert.equal(runtime._calendarCreateDialogRequest, null)
  assert.equal(complete(aborted, false), false)
  assert.deepEqual(notices, [], 'Aborted edit loads must not show errors')

  for (const cancel of [() => { details.open = false }, () => { details.closing = true }, () => { details.isConnected = false },
    () => { calendar.dataset.calendarPeriod = 'week:2026-10-05' }, () => { calendar.loading = true }]) {
    reset()
    const request = begin()
    cancel()
    assert.equal(complete(request), false, 'Dismissed, detached, loading, and navigated edit requests must not swap')
    assert.deepEqual(calls, [])
    assert.deepEqual(notices, [])
  }
  reset()
  const stale = begin(), latest = begin(false)
  fire('htmx:afterSwap', stale)
  assert.equal(stale.xhr.goferCalendarCreateSwapped, undefined, 'Late afterSwap must not revive a replaced editor')
  assert.equal(complete(stale), false)
  assert.equal(runtime._calendarCreateDialogRequest, latest.xhr, 'A stale response must not clear the newer request')
  complete(latest)
  assert.deepEqual(calls, ['initialize', 'open calendar-create-dialog'])
  reset()
  complete(begin(), true, false)
  assert.deepEqual(calls, [], 'A prevented swap must not open a stale dialog')
}

async function main() {
  testDialogLifecycle()
  const event = {detail: {parameters: {}}}
  context.configureCalendarCreateDialog(event)
  assert.equal(event.detail.parameters.date, '2026-10-03', 'New event must use the selected day')
  const editEvent = {detail: {parameters: {}, elt: {hasAttribute(name) { return name === 'data-calendar-edit-trigger' }}}}
  context.configureCalendarCreateDialog(editEvent)
  assert.deepEqual(editEvent.detail.parameters, {}, 'Edit GET must not receive creation date/default parameters')
  context.updateCalendarCreateForm(form)
  assert.equal(submit.disabled, false)
  assert.equal(sourceTrigger.disabled, false)
  const meetToggle = {name: 'google_meet_meeting', value: 'true', checked: true, disabled: false}
  const meetDraft = {name: 'google_meet_draft_id', value: '', disabled: false}
  inputs.push(meetToggle, meetDraft)
  context.updateCalendarCreateForm(form)
  assert.equal(submit.disabled, true, 'Save waits for a generated Meet link')
  meetDraft.value = 'ready-meeting'
  context.updateCalendarCreateForm(form)
  assert.equal(submit.disabled, false, 'A ready Meet link enables Save')
  meetToggle.checked = false
  meetDraft.value = ''
  context.updateCalendarCreateForm(form)
  assert.equal(submit.disabled, false, 'Turning Meet off allows an ordinary event')
  inputs.splice(-2)
  sourceChoice.dataset.calendarSourceAuthorized = 'false'
  context.updateCalendarCreateForm(form)
  assert.equal(submit.disabled, true, 'A read-only OAuth grant must not be writable')
  assert.match(access.textContent, /Reconnect.*Accounts/)
  sourceChoice.dataset.calendarSourceAuthorized = 'true'
  sourceChoice.dataset.tuiSelectboxDisabled = 'true'
  context.updateCalendarCreateForm(form)
  assert.equal(submit.disabled, true, 'Read-only templUI items must not enable creation')
  sourceChoice.dataset.tuiSelectboxDisabled = 'false'
  sourceInput.value = ''
  context.updateCalendarCreateForm(form)
  assert.equal(submit.disabled, true, 'An empty templUI selection must not enable creation')
  choices.length = 0
  context.updateCalendarCreateForm(form)
  assert.equal(sourceTrigger.disabled, true, 'A selector without configured calendars must remain disabled')
  choices.push(sourceChoice)
  sourceInput.value = sourceChoice.dataset.tuiSelectboxValue
  inputs.find(input => input.name === 'all_day').checked = true
  context.updateCalendarCreateForm(form)
  assert.ok(times.every(node => node.hidden && node.querySelector('input').disabled && node.querySelector('[data-tui-timepicker]').disabled))
  assert.equal(timezone.hidden, true)
  assert.equal(help.hidden, false)
  const startDate = inputs.find(input => input.name === 'start_date')
  startDate.value = '2026-10-04'
  endDate.value = '2026-10-02'
  context.updateCalendarCreateForm(form)
  assert.equal(endDate.value, '2026-10-04', 'All-day end must move forward to match the start')
  assert.equal(endCalendar['data-tui-calendar-selected-date'], endDate.value, 'The end picker must highlight the adjusted date')
  assert.match(help.textContent, /adjusted.*can’t end earlier/)
  startDate.value = '2027-01-01'
  context.updateCalendarCreateForm(form)
  assert.equal(endDate.value, '2027-01-01', 'Moving the start past the end must adjust across year boundaries')
  assert.equal(yearInput.value, '2027')
  assert.equal(monthInput.value, '0')
  assert.equal(endCalendar.dataset.tuiCalendarCurrentMonth, '0')
  endDate.value = '2026-12-31'
  context.updateCalendarCreateForm(form)
  assert.equal(endDate.value, startDate.value, 'Choosing an earlier end must also correct the range')
  assert.equal(monthChanges.length, 3, 'Corrections must redraw the popup once each, without loops')
  endDate.value = '2027-01-03'
  context.updateCalendarCreateForm(form)
  assert.equal(endDate.value, '2027-01-03', 'Valid multi-day ranges must be preserved')
  assert.match(help.textContent, /end date is included/)
  endDate.value = '2026-12-31'
  for (const lock of ['_calendarCreateBusy', '_calendarCreateUncertain']) {
    form[lock] = true
    context.updateCalendarCreateForm(form)
    assert.equal(endDate.value, '2026-12-31', 'Locked drafts must not be rewritten')
    form[lock] = false
  }
  inputs.find(input => input.name === 'all_day').checked = false
  context.updateCalendarCreateForm(form)
  assert.equal(endDate.value, '2026-12-31', 'Timed event dates must not be adjusted')
  assert.equal(help.hidden, true)
  inputs.find(input => input.name === 'all_day').checked = true
  context.updateCalendarCreateForm(form)
  assert.equal(endDate.value, startDate.value, 'Enabling all-day must normalize an existing invalid range')
  endDate.value = ''
  context.updateCalendarCreateForm(form)
  assert.equal(endDate.value, '', 'An unset end must retain required-field validation')
  endDate.value = '2027-01-01'
  inputs.find(input => input.name === 'all_day').checked = false
  context.updateCalendarCreateForm(form)
  startDate.value = ''
  await context.submitCalendarCreate(form)
  assert.equal(requests.length, 0, 'Hidden picker inputs must retain required-field validation')
  assert.match(error.textContent, /start date/)
  startDate.value = '2026-10-03'
  const first = context.submitCalendarCreate(form)
  context.submitCalendarCreate(form)
  assert.equal(requests.length, 1, 'Double submit must be ignored')
  assert.equal(label.textContent, 'Creating…')
  assert.equal(spinner.hidden, false)
  assert.ok(inputs.every(input => input.disabled))
  assert.equal(sourceTrigger.disabled, true, 'The templUI trigger must be locked while saving')
  assert.ok(pickerTriggers.every(trigger => trigger.disabled), 'Date, time, and timezone triggers must be locked while saving')
  const stablePayload = requests[0].options.body
  assert.equal(requests[0].url, '/api/calendar/events')
  assert.equal(requests[0].options.method, 'POST')
  requests[0].reject(new Error('lost response'))
  await first
  assert.equal(label.textContent, 'Retry safely')
  assert.equal(submit.disabled, false)
  assert.ok(inputs.every(input => input.disabled), 'Uncertain writes must keep the same draft')
  assert.equal(sourceTrigger.disabled, true, 'Safe retry must not allow another calendar to be selected')
  assert.ok(pickerTriggers.every(trigger => trigger.disabled), 'Safe retry must lock all templUI pickers')
  assert.match(error.textContent, /duplicate/)
  const retry = context.submitCalendarCreate(form)
  assert.equal(requests[1].options.body, stablePayload, 'Retry must use the exact request ID and details')
  requests[1].resolve({ok: true, json: async () => ({event_id: 'created', hidden: true})})
  await retry
  assert.deepEqual(closed, ['calendar-create-dialog'])
  assert.equal(refreshes, 1)
  assert.equal(toasts[0].variant, 'success')
  assert.match(toasts[0].description, /hidden calendar/)

  form._calendarCreateUncertain = false
  context.updateCalendarCreateForm(form)
  const rejected = context.submitCalendarCreate(form)
  requests[2].resolve({ok: false, json: async () => ({error: 'Write access denied', uncertain: false})})
  await rejected
  assert.equal(inputs.find(input => input.name === 'request_id').value, 'fresh-request')
  assert.equal(inputs.find(input => input.name === 'summary').disabled, false, 'Definitive failure must keep an editable draft')
  assert.equal(sourceTrigger.disabled, false, 'Definitive failure must unlock the templUI selector')
  assert.ok(pickerTriggers.every(trigger => !trigger.disabled), 'Definitive failure must unlock the date and time pickers')
  assert.equal(error.textContent, 'Write access denied')
  assert.equal(toasts.length, 1, 'Failed requests must not announce success')

  form.isConnected = false
  const detached = context.submitCalendarCreate(form)
  requests[3].resolve({ok: true, json: async () => ({event_id: 'another', teams_unconfirmed: true})})
  await detached
  assert.equal(closed.length, 1, 'Late save must not close a newer dialog')
  assert.equal(toasts.at(-1).variant, 'warning', 'Missing Teams link must not be announced as successful Teams creation')
  assert.match(toasts.at(-1).description, /Event saved.*did not confirm a Teams link/)
  assert.equal(form._calendarCreateUncertain, false, 'A confirmed event without Teams must not offer a duplicate creation retry')

  const field = name => inputs.find(input => input.name === name)
  function resetEdit(allDay = false) {
    Object.assign(form, {isConnected: true, _calendarCreateBusy: false, _calendarCreateUncertain: false, _calendarCreateConflict: false, _calendarCreateDateAdjusted: '', action: '/api/calendar/events/event%2Fwith%20space%3F%23'})
    form.dataset.calendarEventId = 'event/with space?#'
    form.dataset.calendarEditOccurrence = 'false'
    field('request_id').value = '8d29ced2-98b3-4f55-8d83-bddc7470c486'
    field('version').value = '"provider-version-1"'
    field('summary').value = 'Prefilled event'
    field('description').value = 'First line\nSecond line'
    field('location').value = 'Room 2'
    field('start_date').value = '2026-10-24'
    field('end_date').value = allDay ? '2026-10-26' : '2026-10-24'
    field('start_time').value = '09:15'
    field('end_time').value = '10:30'
    field('timezone').value = 'Europe/Prague'
    field('all_day').checked = allDay
    field('all_day').value = 'true'
    context.initializeCalendarCreateForm()
  }

  resetEdit(true)
  assert.equal(field('end_date').value, '2026-10-26', 'Opening an all-day edit must preserve the server-prefilled inclusive end')
  assert.equal(field('start_date').value, '2026-10-24', 'Selected creation date must not overwrite edit dates')
  assert.equal(field('timezone').value, 'Europe/Prague')
  assert.equal(label.textContent, 'Save changes')
  assert.equal(sourceTrigger.disabled, true, 'The original calendar is fixed for edits')
  assert.equal(sourceInput.disabled, false, 'The disabled selector must still submit source_id')
  assert.equal(submit.disabled, false)
  assert.equal(help.hidden, false)
  assert.ok(times.every(node => node.hidden && node.querySelector('input').disabled))
  let requestIndex = requests.length, refreshCount = refreshes
  field('version').disabled = true
  sourceInput.disabled = true
  const edited = context.submitCalendarCreate(form)
  await context.submitCalendarCreate(form)
  assert.equal(requests.length, requestIndex + 1, 'Edits must reject duplicate submits')
  assert.equal(label.textContent, 'Saving...')
  assert.equal(requests[requestIndex].options.method, 'PATCH')
  assert.equal(requests[requestIndex].url, form.action, 'PATCH must use the escaped server-rendered action')
  const editPayload = new URLSearchParams(requests[requestIndex].options.body)
  for (const name of ['source_id', 'version', 'request_id', 'summary', 'description', 'location', 'start_date', 'end_date', 'timezone', 'all_day']) assert.deepEqual(editPayload.getAll(name), [field(name).value], 'Edit payload must submit exactly one ' + name)
  assert.equal(editPayload.has('start_time'), false)
  assert.equal(editPayload.has('end_time'), false)
  requests[requestIndex].resolve({ok: true, json: async () => ({saved: true, event_id: form.dataset.calendarEventId})})
  await edited
  assert.equal(toasts.at(-1).title, 'Event updated')
  assert.equal(refreshes, refreshCount + 1, 'Edit success must schedule the existing grid and Upcoming cache refresh')
  assert.equal(closed.at(-1), 'calendar-create-dialog')

  inputs.push({name: 'edit_scope', value: 'occurrence', disabled: false})
  for (const scope of ['occurrence', 'series', undefined]) {
    resetEdit()
    form.dataset.calendarEditOccurrence = 'true'
    requestIndex = requests.length
    const beforeRefresh = refreshes
    const saving = context.submitCalendarCreate(form)
    assert.equal(new URLSearchParams(requests[requestIndex].options.body).get('edit_scope'), 'occurrence')
    requests[requestIndex].resolve({ok: true, json: async () => ({saved: true, event_id: form.dataset.calendarEventId, scope})})
    await saving
    assert.equal(refreshes, beforeRefresh + (scope === 'occurrence' ? 1 : 0), 'Confirm the occurrence scope before refreshing or dismissing')
    assert.equal(!!form._calendarCreateUncertain, scope !== 'occurrence')
  }
  inputs.pop()

  resetEdit()
  assert.equal(field('start_time').value, '09:15')
  assert.equal(field('end_time').value, '10:30')
  assert.equal(timezone.hidden, false)
  requestIndex = requests.length
  const requestID = field('request_id').value
  const invalidEdit = context.submitCalendarCreate(form)
  requests[requestIndex].resolve({ok: false, json: async () => ({error: 'End must follow start', uncertain: false, conflict: false, request_id: 'ignored'})})
  await invalidEdit
  assert.equal(error.textContent, 'End must follow start')
  assert.equal(field('summary').disabled, false, 'Definitive validation failures retain an editable draft')
  assert.equal(submit.disabled, false)
  assert.equal(sourceTrigger.disabled, true, 'Validation failures must not unlock the original calendar')
  assert.equal(sourceInput.disabled, false)
  assert.equal(field('version').value, '"provider-version-1"')
  assert.equal(field('request_id').value, requestID, 'Edit failures must not rotate creation request IDs')

  for (const failure of ['conflict', 'uncertain', 'no server error', 'network', 'invalid JSON', 'missing saved', 'missing event ID']) {
    resetEdit(true)
    requestIndex = requests.length
    const toastCount = toasts.length, closeCount = closed.length, beforeRefresh = refreshes
    error.focused = false
    const saving = context.submitCalendarCreate(form)
    const request = requests[requestIndex]
    const serverError = (failure === 'conflict' ? 'Event changed elsewhere.' : 'Provider result unknown.') + ' Refresh calendars and reopen this event to verify its current details.'
    if (failure === 'network') request.reject(new Error('lost response'))
    else if (failure === 'invalid JSON') request.resolve({ok: false, json: async () => { throw new Error('invalid JSON') }})
    else if (failure === 'missing saved') request.resolve({ok: true, json: async () => ({event_id: 'edited'})})
    else if (failure === 'missing event ID') request.resolve({ok: true, json: async () => ({saved: true})})
    else request.resolve({ok: false, json: async () => ({error: failure === 'no server error' ? '' : serverError, uncertain: failure !== 'conflict', conflict: failure === 'conflict'})})
    await saving
    assert.equal(submit.disabled, true, failure + ' must require refresh/reopen')
    assert.equal(label.textContent, 'Save changes')
    assert.equal(spinner.hidden, true)
    assert.ok(inputs.every(input => input.disabled), failure + ' must freeze the draft')
    assert.ok(pickerTriggers.every(trigger => trigger.disabled))
    assert.equal(sourceTrigger.disabled, true)
    assert.equal(field('version').value, '"provider-version-1"', 'Conflicts never acquire a new version automatically')
    assert.match(error.textContent, /Refresh calendars and reopen.*verify/)
    if (failure === 'conflict' || failure === 'uncertain') assert.equal(error.textContent, serverError, 'Backend recovery messages must be shown without duplicate guidance')
    assert.equal(error.focused, true, 'Frozen edit recovery must receive accessible focus after Save becomes disabled')
    assert.doesNotMatch(error.textContent, /retry/i)
    const message = error.textContent
    const changed = {matches() { return true }, closest() { return form }}
    formListeners.change({target: changed})
    formListeners.input({target: changed})
    assert.equal(error.textContent, message, 'Delayed picker events must not clear recovery guidance')
    await context.submitCalendarCreate(form)
    assert.equal(requests.length, requestIndex + 1, 'Uncertain or conflicted edits must never silently retry or overwrite')
    assert.equal(toasts.length, toastCount)
    assert.equal(closed.length, closeCount)
    assert.equal(refreshes, beforeRefresh)
    assert.equal(field('end_date').value, '2026-10-26', 'Frozen all-day drafts must retain their inclusive end')
  }

  resetEdit()
  requestIndex = requests.length
  const lateEdit = context.submitCalendarCreate(form)
  form.isConnected = false
  const closeCount = closed.length
  requests[requestIndex].resolve({ok: true, json: async () => ({saved: true, event_id: form.dataset.calendarEventId})})
  await lateEdit
  assert.equal(closed.length, closeCount, 'Late edit success must not close a newer dialog')
  assert.match(source, /source\.addEventListener\("calendar-changed"/, 'Other sessions need cache change events')
  console.log('Calendar create/edit: server prefill, inclusive all-day dates, fixed source, PATCH/version payload, dialog lifecycle guards, conflict/uncertain freeze, create safe retry, cache refresh, and detached saves passed.')
}
main().catch(error => { console.error(error); process.exitCode = 1 })
