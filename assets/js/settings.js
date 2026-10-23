document.addEventListener("DOMContentLoaded", function () {
  setupSettingsHistory()
  setupSettingsSidebar()
  setupModePickers()
  setupAccountColorPickers()
  setupAccountOrder()
  setupAccountSignaturesDialog(document)
  setupEmailLinkHandler()
})

function accountTestDialog(event) {
  var trigger = (event.detail && event.detail.elt) || event.currentTarget || event.target
  var dialogID = trigger && trigger.getAttribute && trigger.getAttribute("data-account-test-dialog")
  return dialogID ? document.getElementById(dialogID) : null
}

function setAccountTestService(section, state, status, detail) {
  if (!section) return

  section.dataset.state = state
  var statusEl = section.querySelector("[data-account-test-service-status]")
  var detailEl = section.querySelector("[data-account-test-service-detail]")
  if (statusEl) statusEl.textContent = status
  if (detailEl) {
    detailEl.textContent = detail || ""
    detailEl.classList.toggle("hidden", !detail)
  }
}

function accountTestRetryButtons(dialog) {
  return dialog ? dialog.querySelectorAll("[data-account-test-retry]") : []
}

function handleAccountTestStart(event) {
  var dialog = accountTestDialog(event)
	if (!dialog) return

	dialog.dataset.accountTestRun = String((Number(dialog.dataset.accountTestRun) || 0) + 1)
	dialog.querySelectorAll("[data-account-test-service]").forEach(function (section) {
    setAccountTestService(section, "testing", section.dataset.accountTestLoadingLabel || "Testing connection...", "")
  })
  accountTestRetryButtons(dialog).forEach(function (button) { button.disabled = true })
}

function handleAccountTestResult(event) {
  var dialog = accountTestDialog(event)
  if (!dialog) return

  var xhr = event.detail && event.detail.xhr
  var results = null
  if (xhr && xhr.status === 200) {
    try {
      var payload = JSON.parse(xhr.responseText)
      if (Array.isArray(payload.results)) results = payload.results
    } catch (_) {}
  }

  var run = Number(dialog.dataset.accountTestRun) || 0
  var sections = Array.from(dialog.querySelectorAll("[data-account-test-service]"))
  var byService = {}
  if (results) {
    results.forEach(function (result) {
      byService[String(result.service || "").toLowerCase()] = result
    })
  }

  sections.forEach(function (section, index) {
    var result = byService[String(section.dataset.accountTestService || "").toLowerCase()]

    window.setTimeout(function () {
      if ((Number(dialog.dataset.accountTestRun) || 0) !== run) return
      if (!results) {
        setAccountTestService(section, "error", "Test could not be completed", "Try again or review the account credentials.")
      } else if (!result) {
        setAccountTestService(section, "error", "No result returned", "The service check did not report a status.")
      } else if (result.success) {
        setAccountTestService(section, "success", result.message || "Connection successful", "")
      } else {
        setAccountTestService(section, "error", result.error || "Connection failed", result.message || "")
      }
    }, index * 160)
  })

  var finishDelay = Math.max(0, (sections.length - 1) * 160) + 220
  window.setTimeout(function () {
    if ((Number(dialog.dataset.accountTestRun) || 0) !== run) return
    accountTestRetryButtons(dialog).forEach(function (button) { button.disabled = false })
  }, finishDelay)
}

function _settingsEventElement(event) {
  return (event && event.detail && event.detail.elt) || (event && event.currentTarget) || (event && event.target)
}

// The edit wizard saves mail changes from its test step (1.d). Saving shows
// progress there; a failure explains itself there and offers to save anyway or
// test again; success offers to close the window or continue to contacts.
function handleEditAccountSaveStart(event) {
  var form = _settingsEventElement(event)
  if (!form || form.id !== "edit-account-form") return
  if (typeof editAccountTest !== "undefined") editAccountTest.saving("Saving changes...")
}

function handleEditAccountSaveResult(event) {
  var form = _settingsEventElement(event)
  if (!form || form.id !== "edit-account-form" || typeof editAccountTest === "undefined") return
  var xhr = event && event.detail && event.detail.xhr
  var status = document.getElementById("edit-account-save-status")
  var errorCard = status && status.querySelector("[data-account-form-error]")
  var ok = !!(event && event.detail && event.detail.successful) && !errorCard && (!xhr || xhr.getResponseHeader("X-Gofer-Status") !== "error")
  if (ok) {
    if (typeof editAccountMarkSaved === "function") editAccountMarkSaved()
    var unsaved = document.querySelector("#edit-account-dialog [data-edit-account-unsaved]")
    if (unsaved) unsaved.hidden = true
    editAccountTest.saved()
    return
  }
  var message = errorCard ? (errorCard.querySelector("p") || errorCard).textContent : (xhr && xhr.responseText ? xhr.responseText.trim() : "")
  if (status) status.textContent = ""
  editAccountTest.saveFailed(message || "Could not save the changes.")
}

function _calendarDiscoveryForm(event) {
  var element = _settingsEventElement(event)
  return element && element.closest ? element.closest("[data-calendar-discovery-form]") : null
}

function toggleCalendarAuth(checkbox) {
  var form = checkbox && checkbox.closest ? checkbox.closest("form") : null
  if (!form) return
  var fields = form.querySelector("[data-calendar-credential-fields]")
  if (!fields) return
  fields.classList.toggle("opacity-50", checkbox.checked)
  fields.querySelectorAll('input[name="username"], input[name="password"]').forEach(function (input) {
    input.disabled = checkbox.checked
  })
}

document.addEventListener("click", function (event) {
  var button = event.target && event.target.closest ? event.target.closest("[data-calendar-autodiscover]") : null
  if (!button) return
  var form = button.closest("[data-calendar-discovery-form]")
  if (!form) return
  event.preventDefault()

  var flag = form.querySelector('input[name="autodiscover"]')
  if (!flag) {
    flag = document.createElement("input")
    flag.type = "hidden"
    flag.name = "autodiscover"
    form.appendChild(flag)
  }
  flag.value = "1"
  if (typeof form.requestSubmit === "function") {
    var submitter = form.querySelector("[data-calendar-discovery-submit]")
    if (submitter) form.requestSubmit(submitter)
    else form.requestSubmit()
  } else if (window.htmx) {
    window.htmx.trigger(form, "submit")
  }
})

function _setCalendarDiscoveryBusy(form, busy) {
  if (!form) return
  var autodiscover = form.querySelector('input[name="autodiscover"][value="1"]')
  var buttons = [
    form.querySelector("[data-calendar-discovery-submit]"),
    form.querySelector("[data-calendar-autodiscover]")
  ].filter(Boolean)
  var button = autodiscover ? form.querySelector("[data-calendar-autodiscover]") : form.querySelector("[data-calendar-discovery-submit]")
  if (!button) return

  var label = button.querySelector(autodiscover ? "[data-calendar-autodiscover-label]" : "[data-calendar-discovery-label]")
  if (busy) {
    var originalLabel = label ? label.textContent.trim() : ""
    button.dataset.calendarDiscoveryOriginalLabel = originalLabel
    if (label) label.textContent = autodiscover ? "Searching..." : originalLabel.indexOf("Discover") === 0 ? "Discovering..." : "Refreshing..."
  } else if (label && button.dataset.calendarDiscoveryOriginalLabel) {
    label.textContent = button.dataset.calendarDiscoveryOriginalLabel
    delete button.dataset.calendarDiscoveryOriginalLabel
  }

  buttons.forEach(function (item) { item.disabled = !!busy })
  button.setAttribute("aria-busy", busy ? "true" : "false")
  button.classList.toggle("cursor-wait", !!busy)
  var icon = button.querySelector("svg")
  if (icon) icon.classList.toggle("animate-spin", !!busy)
}

function handleCalendarDiscoveryStart(event) {
  var form = _calendarDiscoveryForm(event)
  if (!form) return
  _setCalendarDiscoveryBusy(form, true)
}

function handleCalendarDiscoveryResult(event) {
  var form = _calendarDiscoveryForm(event)
  if (!form) return
  var xhr = event && event.detail && event.detail.xhr
  var ok = !!(event && event.detail && event.detail.successful) && (!xhr || xhr.getResponseHeader("X-Gofer-Status") !== "error")
  if (ok) return

  _setCalendarDiscoveryBusy(form, false)
  var flag = form.querySelector('input[name="autodiscover"]')
  if (flag) flag.remove()
}

function _calendarSourceForm(event) {
  var element = _settingsEventElement(event)
  return element && element.closest ? element.closest("[data-calendar-source-form]") : null
}

function _calendarSourceStatus(form, message, error) {
  if (!form) return
  var status = form.querySelector("[data-calendar-source-save-status]")
  if (!status) return
  status.textContent = ""
  var box = document.createElement("div")
  box.className = error
    ? "rounded-md border border-destructive/20 bg-destructive/10 px-3 py-2 text-xs text-destructive"
    : "rounded-md border border-border bg-background px-3 py-2 text-xs text-muted-foreground"
  if (error) {
    box.textContent = message
  } else {
    box.innerHTML = '<span class="inline-flex items-center gap-2"><span class="size-3.5 border-2 border-muted-foreground/30 border-t-muted-foreground rounded-full animate-spin"></span><span data-calendar-source-save-label></span></span>'
    box.querySelector("[data-calendar-source-save-label]").textContent = message
  }
  status.appendChild(box)
}

function handleCalendarSourceSaveStart(event) {
  var form = _calendarSourceForm(event)
  if (!form) return
  var root = form.closest ? form.closest("#add-account-dialog,#edit-account-dialog") : null
  if (root) form.dataset.calendarWizardRootId = root.id
  _calendarSourceStatus(form, "Saving calendar selection...", false)
}

function handleCalendarSourceSaveResult(event) {
  var form = _calendarSourceForm(event)
  if (!form) return
  var xhr = event && event.detail && event.detail.xhr
  var ok = !!(event && event.detail && event.detail.successful) && (!xhr || xhr.getResponseHeader("X-Gofer-Status") !== "error")
  var root = form.closest ? form.closest("#add-account-dialog,#edit-account-dialog") : null
  if (!root && form.dataset.calendarWizardRootId) root = document.getElementById(form.dataset.calendarWizardRootId)
  var toggle = root && root.querySelector ? root.querySelector('[data-wizard-service-switch="calendar"]') : null
  if (ok) {
    if (toggle) {
      toggle.checked = true
      toggle.dataset.wizardServiceInitialEnabled = "true"
    }
    if (root && typeof wizardServiceAdvance === "function") wizardServiceAdvance(root, 5)
    return
  }

  if (toggle && toggle.dataset.wizardServiceInitialEnabled !== "true") toggle.checked = false
  var message = xhr && xhr.responseText ? xhr.responseText.trim() : "Could not save calendar source selection."
  _calendarSourceStatus(form, message || "Could not save calendar source selection.", true)
}

// Sets every contact conflict in a profile import to the same choice. A
// contact that cannot take that choice keeps its own.
function setProfileConflictChoices(button, value) {
  var list = button && button.closest && button.closest("[data-profile-contact-conflicts]")
  if (!list) return
  list.querySelectorAll('[data-profile-conflict-choice="' + value + '"]').forEach(function (input) {
    if (!input.disabled) input.checked = true
  })
}

// A dialog whose work has started closes when the server says so, and the
// profile export dialog closes once its download begins.
document.addEventListener("gofer-close-dialog", function (event) {
  var id = event.detail && event.detail.value
  if (id && window.tui && window.tui.dialog) window.tui.dialog.close(id)
})

document.addEventListener("submit", function (event) {
  var form = event.target && event.target.closest && event.target.closest("[data-close-dialog-on-submit]")
  if (!form) return
  var id = form.getAttribute("data-close-dialog-on-submit")
  setTimeout(function () {
    if (window.tui && window.tui.dialog) window.tui.dialog.close(id)
  }, 100)
})

function confirmSignatureDelete(name) {
  return window.goferConfirm({
    title: "Delete " + name + "?",
    message: "Accounts using it will stop inserting it.",
    confirmLabel: "Delete signature",
    destructive: true,
  })
}

// Mail import: choosing "New folder…" for a folder of messages asks for the
// new folder's name, and the upload says how far it has got.
document.addEventListener("change", function (event) {
  var select = event.target && event.target.closest && event.target.closest("[data-mail-import-target]")
  if (!select) return
  var row = select.closest("[data-mail-import-source], [data-mail-transfer-folder]")
  var name = row && row.querySelector("[data-mail-import-new-name]")
  if (!name) return
  var isNew = select.value === "new"
  name.classList.toggle("hidden", !isNew)
  name.required = isNew
  if (isNew) name.focus()
})

document.addEventListener("htmx:xhr:progress", function (event) {
  var form = event.target && event.target.closest && event.target.closest("[data-mail-import-upload]")
  var detail = event.detail
  if (!form || !detail || !detail.lengthComputable || !detail.total) return
  var status = form.querySelector("[data-mail-import-upload-status]")
  if (!status) return
  var percent = Math.floor((detail.loaded * 100) / detail.total)
  status.textContent = percent < 100 ? "Uploading… " + percent + "%" : "Reading the messages…"
})

function toggleUnencryptedTransportWarning(input) {
  if (!input || !input.closest) return
  var field = input.closest(".select-container")
  var warning = field && field.parentElement ? field.parentElement.querySelector("[data-unencrypted-transport-warning]") : null
  if (!warning) return
  var active = input.value === "plaintext"
  warning.classList.toggle("hidden", !active)
  warning.classList.toggle("inline-flex", active)
}

// While an account form is filled in, say whether Gofer will refuse the server:
// one on a private network, or an unencrypted connection, without a Mail
// security exception. The check runs once typing pauses, and only the answer
// for the fields' latest values is shown.
var accountServerCheckField = /^(imap|smtp)_(host|port|tls_mode)$/

// Calendar and contacts server addresses are checked the same way.
var accountServerCheckURLField = /^(base_url|caldav_url|addressbook_url)$/

function accountServerURLNote(input) {
  for (var node = input.parentElement, depth = 0; node && depth < 3; node = node.parentElement, depth++) {
    var note = node.querySelector(":scope > [data-server-check-url]")
    if (note) return note
  }
  return null
}

function scheduleAccountServerCheck(event) {
  var input = event.target
  if (input && input.name && input.type === "url" && accountServerCheckURLField.test(input.name)) {
    var note = accountServerURLNote(input)
    if (!note) return
    clearTimeout(note._serverCheckTimer)
    note._serverCheckTimer = setTimeout(function () {
      runAccountServerURLCheck(input, note)
    }, 600)
    return
  }
  if (!input || !input.name || !accountServerCheckField.test(input.name)) return
  var form = input.form || (input.closest && input.closest("form"))
  var protocol = input.name.split("_")[0]
  var target = form ? form.querySelector('[data-server-check="' + protocol + '"]') : null
  if (!target) return
  clearTimeout(target._serverCheckTimer)
  target._serverCheckTimer = setTimeout(function () {
    runAccountServerCheck(form, protocol, target)
  }, 600)
}

function runAccountServerURLCheck(input, target) {
  var address = String(input.value || "").trim()
  if (!/^https?:\/\/[^/]/i.test(address)) {
    target._serverCheckKey = ""
    renderAccountServerCheck(target, [])
    return
  }
  fetchAccountServerCheck(target, new URLSearchParams({ url: address }).toString())
}

function runAccountServerCheck(form, protocol, target) {
  function field(name) {
    var input = form.querySelector('[name="' + protocol + "_" + name + '"]')
    return input ? String(input.value || "").trim() : ""
  }
  var host = field("host")
  var port = field("port")
  if (!host || !port) {
    target._serverCheckKey = ""
    renderAccountServerCheck(target, [])
    return
  }
  fetchAccountServerCheck(target, new URLSearchParams({ protocol: protocol, host: host, port: port, tls_mode: field("tls_mode") }).toString())
}

function fetchAccountServerCheck(target, key) {
  if (target._serverCheckKey === key) return
  target._serverCheckKey = key
  if (target._serverCheckAbort) target._serverCheckAbort.abort()
  var controller = typeof AbortController === "function" ? new AbortController() : null
  target._serverCheckAbort = controller
  fetch("/api/accounts/server-check?" + key, { credentials: "same-origin", signal: controller ? controller.signal : undefined })
    .then(function (response) {
      return response.ok ? response.json() : { notes: [] }
    })
    .then(function (data) {
      if (target._serverCheckKey === key) renderAccountServerCheck(target, (data && data.notes) || [])
    })
    .catch(function () {})
}

function renderAccountServerCheck(target, notes) {
  target.replaceChildren()
  notes.forEach(function (note) {
    var line = document.createElement("p")
    line.className = note.level === "blocked"
      ? "text-xs leading-relaxed text-amber-800 dark:text-amber-200"
      : "text-xs leading-relaxed text-emerald-800 dark:text-emerald-200"
    line.textContent = note.text
    if (note.link) {
      var link = document.createElement("a")
      link.href = note.link
      link.target = "_blank"
      link.rel = "noopener"
      link.className = "font-semibold underline underline-offset-2"
      link.textContent = note.link_text || "Open Mail security"
      line.appendChild(document.createTextNode(" "))
      line.appendChild(link)
    }
    target.appendChild(line)
  })
  target.classList.toggle("hidden", notes.length === 0)
}

// Forms opened with servers already filled in, such as editing an account,
// are checked straight away.
function checkAccountServers(root) {
  if (!root || !root.querySelectorAll) return
  root.querySelectorAll("[data-server-check]").forEach(function (target) {
    var form = target.closest("form")
    if (form) runAccountServerCheck(form, target.getAttribute("data-server-check"), target)
  })
  root.querySelectorAll("[data-server-check-url]").forEach(function (target) {
    var input = target.parentElement && target.parentElement.querySelector('input[type="url"]')
    if (input && accountServerCheckURLField.test(input.name)) runAccountServerURLCheck(input, target)
  })
}

document.addEventListener("input", scheduleAccountServerCheck)
document.addEventListener("change", scheduleAccountServerCheck)
document.addEventListener("DOMContentLoaded", function () {
  checkAccountServers(document)
})
document.body.addEventListener("htmx:afterSettle", function (e) {
  checkAccountServers(e.target)
})

function normalizeAccountColorInput(color) {
  color = (color || "").trim()
  if (/^[0-9a-f]{6}$/i.test(color)) color = "#" + color
  if (!/^#[0-9a-f]{6}$/i.test(color)) return ""
  return color.toLowerCase()
}

function setupAccountColorPickers() {
  if (setupAccountColorPickers.ready) return
  setupAccountColorPickers.ready = true

  document.addEventListener("click", function (e) {
    var swatch = e.target.closest && e.target.closest("[data-account-color-option]")
    if (!swatch) return
    e.preventDefault()
    var picker = swatch.closest("[data-account-color-picker]")
    if (!picker) return
    saveAccountColor(picker, swatch.getAttribute("data-account-color-option"), swatch)
  })

  document.addEventListener("change", function (e) {
    var input = e.target.closest && e.target.closest("[data-account-color-custom]")
    if (!input) return
    var picker = input.closest("[data-account-color-picker]")
    if (!picker) return
    saveAccountColor(picker, input.value, input)
  })
}

function setAccountColorStatus(picker, message, isError) {
  var status = picker && picker.querySelector("[data-account-color-status]")
  if (!status) return
  status.textContent = message || ""
  status.classList.toggle("text-destructive", !!isError)
  status.classList.toggle("text-muted-foreground", !isError)
}

function setAccountColorSaving(picker, saving) {
  if (!picker) return
  picker.querySelectorAll("[data-account-color-option], [data-account-color-custom]").forEach(function (el) {
    el.disabled = !!saving
    el.classList.toggle("opacity-60", !!saving)
  })
}

function updateAccountColorUI(accountId, color) {
  document.querySelectorAll("[data-account-marker]").forEach(function (marker) {
    if (marker.getAttribute("data-account-marker") === accountId) marker.style.backgroundColor = color
  })
  document.querySelectorAll("[data-account-color-picker]").forEach(function (picker) {
    if (picker.getAttribute("data-account-id") !== accountId) return
    var input = picker.querySelector("[data-account-color-custom]")
    if (input) input.value = color
    picker.querySelectorAll("[data-account-color-option]").forEach(function (swatch) {
      var active = normalizeAccountColorInput(swatch.getAttribute("data-account-color-option")) === color
      swatch.setAttribute("data-account-color-active", active ? "true" : "false")
      swatch.setAttribute("aria-pressed", active ? "true" : "false")
    })
  })
}

function closeAccountColorPopover(source) {
  var root = source && source.closest && source.closest("[data-tui-popover-root]")
  if (!root || !root.id || !window.tui || !window.tui.popover) return
  setTimeout(function () { window.tui.popover.close(root.id) }, 180)
}

function saveAccountColor(picker, rawColor, source) {
  var accountId = picker.getAttribute("data-account-id") || ""
  var color = normalizeAccountColorInput(rawColor)
  if (!accountId || !color) {
    setAccountColorStatus(picker, "Invalid", true)
    return
  }

  setAccountColorSaving(picker, true)
  setAccountColorStatus(picker, "Saving", false)
  fetch("/api/accounts/" + encodeURIComponent(accountId) + "/color", {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json" },
    body: new URLSearchParams({ color: color }).toString()
  }).then(function (res) {
    return res.json().catch(function () { return {} }).then(function (data) {
      if (!res.ok) throw new Error(data.error || "Failed to update color")
      return data
    })
  }).then(function (data) {
    var nextColor = normalizeAccountColorInput(data.color) || color
    updateAccountColorUI(accountId, nextColor)
    setAccountColorStatus(picker, "Saved", false)
    closeAccountColorPopover(source)
    setTimeout(function () { setAccountColorStatus(picker, "", false) }, 1800)
  }).catch(function (err) {
    setAccountColorStatus(picker, err && err.message ? err.message : "Failed", true)
  }).finally(function () {
    setAccountColorSaving(picker, false)
  })
}

// Accounts are put in order on the Accounts page by dragging a card's handle,
// with the arrow keys on a focused handle, or with Move up and Move down in
// its menu. Accounts being deleted keep their place and can't be moved.
function accountOrderCards(list) {
  return Array.prototype.filter.call(list.querySelectorAll("[data-settings-account-card]"), function (card) {
    return card.parentNode === list && card.getAttribute("data-account-deleting") !== "true"
  })
}

function accountOrderIDs(list) {
  return accountOrderCards(list).map(function (card) { return card.getAttribute("data-account-id") })
}

function syncAccountOrderControls() {
  document.querySelectorAll("[data-account-order-list]").forEach(function (list) {
    var cards = accountOrderCards(list)
    cards.forEach(function (card, i) {
      var up = document.querySelector('[data-account-actions-menu="' + cssEscapeSettings(card.getAttribute("data-account-id")) + '"] [data-account-move="up"]')
      var down = document.querySelector('[data-account-actions-menu="' + cssEscapeSettings(card.getAttribute("data-account-id")) + '"] [data-account-move="down"]')
      if (up) up.disabled = i === 0
      if (down) down.disabled = i === cards.length - 1
    })
  })
}

function cssEscapeSettings(value) {
  return window.CSS && CSS.escape ? CSS.escape(value) : String(value).replace(/["\\]/g, "\\$&")
}

// moveAccountCard puts card before ref (or last among the accounts when ref
// is null), sliding the cards that make room from where they were.
function moveAccountCard(list, card, ref) {
  var cards = Array.prototype.slice.call(list.querySelectorAll("[data-settings-account-card]"))
  var before = cards.map(function (c) { return c.getBoundingClientRect().top })
  if (ref) {
    list.insertBefore(card, ref)
  } else {
    var movable = accountOrderCards(list).filter(function (c) { return c !== card })
    var last = movable[movable.length - 1]
    list.insertBefore(card, last ? last.nextSibling : null)
  }
  cards.forEach(function (c, i) {
    if (c === card && card.hasAttribute("data-account-order-dragging")) return
    var dy = before[i] - c.getBoundingClientRect().top
    if (!dy) return
    c.style.transition = "none"
    c.style.transform = "translateY(" + dy + "px)"
    c.getBoundingClientRect()
    c.style.transition = ""
    c.style.transform = ""
  })
}

function moveAccountCardBy(card, step) {
  var list = card && card.closest("[data-account-order-list]")
  if (!list) return
  var cards = accountOrderCards(list)
  var at = cards.indexOf(card)
  var to = at + step
  if (at < 0 || to < 0 || to >= cards.length) return
  var previous = accountOrderIDs(list)
  moveAccountCard(list, card, step < 0 ? cards[to] : cards[to + 1] || null)
  saveAccountOrder(list, previous)
}

function restoreAccountOrder(list, ids) {
  var cards = accountOrderCards(list)
  ids.forEach(function (id) {
    var card = cards.filter(function (c) { return c.getAttribute("data-account-id") === id })[0]
    if (card) moveAccountCard(list, card, null)
  })
}

function saveAccountOrder(list, previous) {
  var ids = accountOrderIDs(list)
  syncAccountOrderControls()
  if (ids.join("\n") === previous.join("\n")) return
  var body = new URLSearchParams()
  ids.forEach(function (id) { body.append("account_id", id) })
  fetch("/api/accounts/order", {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: body.toString()
  }).then(function (res) {
    if (res.ok) return
    if (res.status === 409) {
      showGoferToast({ title: "Your accounts changed", description: "The list was reloaded. Put the accounts in order again.", variant: "error", icon: "error", duration: 6000 })
      if (window.htmx) window.htmx.ajax("GET", "/settings/accounts", { target: "#settings-content", select: "#settings-content", swap: "outerHTML" })
      return
    }
    throw new Error("save account order: " + res.status)
  }).catch(function () {
    restoreAccountOrder(list, previous)
    syncAccountOrderControls()
    showGoferToast({ title: "Could not save the account order", description: "Check your connection and try again.", variant: "error", icon: "error", duration: 6000 })
  })
}

function setupAccountOrder() {
  if (setupAccountOrder.ready) return
  setupAccountOrder.ready = true
  var drag = null

  document.addEventListener("click", function (e) {
    var item = e.target.closest && e.target.closest("[data-account-move]")
    if (!item || item.disabled) return
    var menu = item.closest("[data-account-actions-menu]")
    var id = menu && menu.getAttribute("data-account-actions-menu")
    var card = id && document.querySelector('[data-settings-account-card][data-account-id="' + cssEscapeSettings(id) + '"]')
    if (card) moveAccountCardBy(card, item.getAttribute("data-account-move") === "up" ? -1 : 1)
  })

  document.addEventListener("keydown", function (e) {
    var handle = e.target.closest && e.target.closest("[data-account-order-handle]")
    if (!handle || (e.key !== "ArrowUp" && e.key !== "ArrowDown")) return
    e.preventDefault()
    moveAccountCardBy(handle.closest("[data-settings-account-card]"), e.key === "ArrowUp" ? -1 : 1)
    handle.focus()
  })

  document.addEventListener("pointerdown", function (e) {
    var handle = e.target.closest && e.target.closest("[data-account-order-handle]")
    if (!handle || e.button !== 0) return
    var card = handle.closest("[data-settings-account-card]")
    var list = card && card.closest("[data-account-order-list]")
    if (!list) return
    e.preventDefault()
    handle.setPointerCapture(e.pointerId)
    drag = { list: list, card: card, handle: handle, pointerId: e.pointerId, previous: accountOrderIDs(list), grabY: e.clientY - card.getBoundingClientRect().top }
    card.setAttribute("data-account-order-dragging", "")
    list.setAttribute("data-account-order-active", "")
    drag.placeholder = document.createElement("div")
    drag.placeholder.setAttribute("data-account-order-placeholder", "")
    drag.placeholder.setAttribute("aria-hidden", "true")
    followAccountDrag(e.clientY)
    list.appendChild(drag.placeholder)
  })

  // The grabbed card floats under the pointer, a little larger, while an
  // outline shows the place in the list where it would drop.
  function accountCardTop(list, card) {
    return card.offsetParent === list ? card.offsetTop : card.offsetTop - list.offsetTop
  }

  function followAccountDrag(clientY) {
    var y = clientY - drag.list.getBoundingClientRect().top - drag.grabY
    var top = accountCardTop(drag.list, drag.card)
    drag.card.style.transform = "translateY(" + (y - top) + "px) scale(1.01)"
    drag.placeholder.style.top = top + "px"
    drag.placeholder.style.height = drag.card.offsetHeight + "px"
  }

  document.addEventListener("pointermove", function (e) {
    if (!drag || e.pointerId !== drag.pointerId) return
    // Offsets ignore the slide of cards making room, so the place under the
    // pointer doesn't shift while they move.
    var cards = accountOrderCards(drag.list)
    var others = cards.filter(function (c) { return c !== drag.card })
    // A card makes way once the floating card's leading edge passes its middle.
    var at = cards.indexOf(drag.card)
    var floatTop = e.clientY - drag.list.getBoundingClientRect().top - drag.grabY
    var floatBottom = floatTop + drag.card.offsetHeight
    var to = 0
    others.forEach(function (other, k) {
      var middle = accountCardTop(drag.list, other) + other.offsetHeight / 2
      if (k < at ? floatTop >= middle : floatBottom > middle) to++
    })
    if (to !== at) moveAccountCard(drag.list, drag.card, others[to] || null)
    followAccountDrag(e.clientY)
  })

  function endDrag(e) {
    if (!drag || e.pointerId !== drag.pointerId) return
    var done = drag
    drag = null
    // Back in the card's own transition, it settles into its place.
    done.card.removeAttribute("data-account-order-dragging")
    done.list.removeAttribute("data-account-order-active")
    done.placeholder.remove()
    done.card.style.transform = ""
    if (done.handle.hasPointerCapture(e.pointerId)) done.handle.releasePointerCapture(e.pointerId)
    saveAccountOrder(done.list, done.previous)
  }
  document.addEventListener("pointerup", endDrag)
  document.addEventListener("pointercancel", endDrag)

  syncAccountOrderControls()
  document.body.addEventListener("htmx:afterSettle", syncAccountOrderControls)
}

function sanitizeSignatureStyle(style) {
  style = style || ""
  if (/expression\s*\(|javascript\s*:|behavior\s*:|-moz-binding\s*:|url\s*\(/i.test(style)) return ""
  return style
}

function sanitizeSignatureHTML(raw) {
  var doc
  try {
    doc = new DOMParser().parseFromString(raw || "", "text/html")
  } catch (err) {
    doc = document.implementation.createHTMLDocument("")
    doc.body.innerHTML = raw || ""
  }

  var allowed = { A: true, B: true, BIG: true, BLOCKQUOTE: true, BR: true, CENTER: true, CODE: true, COL: true, COLGROUP: true, DIV: true, EM: true, FONT: true, H1: true, H2: true, H3: true, H4: true, H5: true, H6: true, HR: true, I: true, IMG: true, LI: true, OL: true, P: true, PRE: true, S: true, SMALL: true, SPAN: true, STRIKE: true, STRONG: true, SUB: true, SUP: true, TABLE: true, TBODY: true, TD: true, TFOOT: true, TH: true, THEAD: true, TR: true, U: true, UL: true }
  var blocked = doc.body.querySelectorAll("script, style, head, title, iframe, object, embed, form, meta, link")
  for (var b = 0; b < blocked.length; b++) blocked[b].remove()

  var walker = doc.createTreeWalker(doc.body, NodeFilter.SHOW_ELEMENT)
  var nodes = []
  while (walker.nextNode()) nodes.push(walker.currentNode)
  for (var n = nodes.length - 1; n >= 0; n--) {
    var node = nodes[n]
    var tag = node.tagName
    if (!allowed[tag]) {
      var parent = node.parentNode
      while (node.firstChild) parent.insertBefore(node.firstChild, node)
      parent.removeChild(node)
      continue
    }
    for (var a = node.attributes.length - 1; a >= 0; a--) {
      var attr = node.attributes[a]
      var name = attr.name.toLowerCase()
      if (name.indexOf("on") === 0 || name === "class") {
        node.removeAttribute(attr.name)
        continue
      }
      if (name === "style") {
        var safeStyle = sanitizeSignatureStyle(attr.value)
        if (safeStyle) node.setAttribute("style", safeStyle)
        else node.removeAttribute(attr.name)
        continue
      }
      var globalAllowed = { align: true, alt: true, bgcolor: true, border: true, cellpadding: true, cellspacing: true, colspan: true, dir: true, height: true, lang: true, role: true, rowspan: true, title: true, valign: true, width: true }
      if (tag === "A") {
        if (name !== "href" && name !== "target" && name !== "rel" && !globalAllowed[name]) node.removeAttribute(attr.name)
      } else if (tag === "IMG") {
        var imageAllowed = { src: true, alt: true, title: true, width: true, height: true, style: true, "data-remote-src": true }
        if (!imageAllowed[name]) node.removeAttribute(attr.name)
      } else if (!globalAllowed[name]) {
        node.removeAttribute(attr.name)
      }
    }
    if (tag === "A") {
      var href = node.getAttribute("href") || ""
      if (!/^(https?:|mailto:|#)/i.test(href)) node.removeAttribute("href")
      node.setAttribute("rel", "noopener noreferrer")
      if (href && href.charAt(0) !== "#") node.setAttribute("target", "_blank")
    } else if (tag === "IMG") {
      var src = node.getAttribute("src") || node.getAttribute("data-remote-src") || ""
      if (!/^(cid:|https?:|\/api\/attachments\/|\/api\/inline-content\/|\/compose\/attachments\/|data:image\/(png|jpe?g|gif|webp|bmp|svg\+xml)[;,])/i.test(src)) {
        node.remove()
        continue
      }
      if (!node.getAttribute("src")) node.setAttribute("src", src)
    }
  }
  return doc.body.innerHTML
}

function setSignatureModeButtonState(form, mode) {
  var buttons = form.querySelectorAll("[data-signature-mode-button]")
  for (var i = 0; i < buttons.length; i++) {
    var active = buttons[i].getAttribute("data-signature-mode-button") === mode
    buttons[i].classList.toggle("text-foreground", active)
    buttons[i].classList.toggle("bg-card", active)
    buttons[i].classList.toggle("shadow-sm", active)
    buttons[i].classList.toggle("text-muted-foreground", !active)
  }
}

function applySignatureSource(form) {
  var editor = form && form.querySelector("[data-signature-editor]")
  var source = form && form.querySelector("[data-signature-source]")
  var html = form && form.querySelector("[data-signature-html]")
  if (!editor || !source) return ""
  var sanitized = sanitizeSignatureHTML(source.value || "")
  editor.innerHTML = sanitized
  source.value = sanitized
  if (html) html.value = sanitized
  return sanitized
}

function setSignatureEditorMode(form, mode) {
  var editor = form && form.querySelector("[data-signature-editor]")
  var source = form && form.querySelector("[data-signature-source]")
  var html = form && form.querySelector("[data-signature-html]")
  if (!editor || !source) return
  if (mode === "source") {
    syncSignatureEditor(editor)
    source.value = (html && html.value) || editor.innerHTML || ""
    editor.classList.add("hidden")
    source.classList.remove("hidden")
    setSignatureModeButtonState(form, "source")
    source.focus()
    return
  }
  applySignatureSource(form)
  source.classList.add("hidden")
  editor.classList.remove("hidden")
  setSignatureModeButtonState(form, "visual")
  editor.focus()
}

function resetSignatureEditorMode(form) {
  var source = form && form.querySelector("[data-signature-source]")
  if (source) source.value = ""
  setSignatureEditorMode(form, "visual")
}

function syncSignatureEditor(editor) {
  var form = editor && editor.closest ? editor.closest("[data-signature-form]") : null
  var html = form && form.querySelector("[data-signature-html]")
  var source = form && form.querySelector("[data-signature-source]")
  if (html) html.value = editor.innerHTML
  if (source && source.classList.contains("hidden")) source.value = editor.innerHTML
}

function setupAccountSignaturesDialog(root) {
  var managers = (root || document).querySelectorAll("[data-account-signatures-manager]")
  for (var m = 0; m < managers.length; m++) setupAccountSignaturesManager(managers[m])
}

function setupAccountSignaturesManager(manager) {
  if (!manager || manager.dataset.signatureManagerReady === "1") return
  manager.dataset.signatureManagerReady = "1"

  var accountId = manager.getAttribute("data-account-id")
  var refreshURL = manager.getAttribute("data-refresh-url") || (accountId ? "/api/accounts/" + encodeURIComponent(accountId) + "/signatures/manage" : "/settings/compose-display")
  var refreshTarget = manager.getAttribute("data-refresh-target") || "#edit-account-container"
  var refreshSwap = manager.getAttribute("data-refresh-swap") || (refreshTarget === "#edit-account-container" ? "innerHTML" : "outerHTML")
  var form = manager.querySelector("[data-signature-form]")
  var settingsForms = manager.querySelectorAll("[data-account-signature-settings]")
  var accountSelect = manager.querySelector("input[data-signature-account-select]")
  var signatureSelect = manager.querySelector("input[data-signature-select]")
  var editor = manager.querySelector("[data-signature-editor]")

  function selectedSignatureOption() {
    if (!signatureSelect || !signatureSelect.value) return null
    var signatureSelectRoot = signatureSelect.closest ? signatureSelect.closest(".select-container") : null
    var signatureItems = signatureSelectRoot ? signatureSelectRoot.querySelectorAll("[data-signature-option]") : []
    for (var signatureIndex = 0; signatureIndex < signatureItems.length; signatureIndex++) {
      if (signatureItems[signatureIndex].getAttribute("data-tui-selectbox-value") === signatureSelect.value) return signatureItems[signatureIndex]
    }
    return null
  }

  function resetSignatureSelect() {
    if (!signatureSelect) return
    signatureSelect.value = ""
  }

  function loadSignatureOption(option) {
    if (!form || !option) return
    var id = form.querySelector("[data-signature-id]")
    var name = form.querySelector("[data-signature-name]")
    var html = form.querySelector("[data-signature-html]")
    var source = form.querySelector("[data-signature-source]")
    var nextHTML = option.getAttribute("data-signature-html") || ""
    if (id) id.value = option.getAttribute("data-tui-selectbox-value") || ""
    if (name) name.value = option.getAttribute("data-signature-name") || ""
    if (editor) editor.innerHTML = nextHTML
    if (html) html.value = nextHTML
    if (source) source.value = nextHTML
    setSignatureEditorMode(form, "visual")
    if (editor) editor.focus()
  }

  function syncAccountSignaturePanels() {
    if (!accountSelect) return
    var selected = accountSelect.value || (settingsForms[0] && settingsForms[0].getAttribute("data-account-id")) || ""
    var accountSelectRoot = accountSelect.closest ? accountSelect.closest(".select-container") : null
    var accountItems = accountSelectRoot ? accountSelectRoot.querySelectorAll("[data-tui-selectbox-value]") : []
    var selectedItem = null
    for (var itemIndex = 0; itemIndex < accountItems.length; itemIndex++) {
      if (accountItems[itemIndex].getAttribute("data-tui-selectbox-value") === selected) {
        selectedItem = accountItems[itemIndex]
        break
      }
    }
    var marker = manager.querySelector("[data-signature-account-marker]")
    var markerSource = selectedItem && selectedItem.querySelector("[data-signature-account-marker-source]")
    if (marker && markerSource) marker.setAttribute("style", markerSource.getAttribute("style") || "")
    for (var i = 0; i < settingsForms.length; i++) {
      var isActive = settingsForms[i].getAttribute("data-account-id") === selected
      settingsForms[i].classList.toggle("invisible", !isActive)
      settingsForms[i].classList.toggle("pointer-events-none", !isActive)
      settingsForms[i].setAttribute("aria-hidden", isActive ? "false" : "true")
    }
  }

  if (accountSelect) {
    accountSelect.addEventListener("change", syncAccountSignaturePanels)
    syncAccountSignaturePanels()
  }

  if (signatureSelect) {
    signatureSelect.addEventListener("change", function () {
      var option = selectedSignatureOption()
      if (option) loadSignatureOption(option)
      else resetSignatureForm()
    })
  }

  function reloadDialog() {
    if (typeof htmx !== "undefined") {
      window.__goferReopenSignaturesDialog = manager.closest("#account-signatures-dialog") ? "account-signatures-dialog" : "compose-signatures-dialog"
      htmx.ajax("GET", refreshURL, { target: refreshTarget, swap: refreshSwap })
    }
  }

  function resetSignatureForm() {
    if (!form) return
    var id = form.querySelector("[data-signature-id]")
    var name = form.querySelector("[data-signature-name]")
    var html = form.querySelector("[data-signature-html]")
    if (id) id.value = ""
    if (name) name.value = ""
    if (html) html.value = ""
    if (editor) editor.innerHTML = ""
    resetSignatureEditorMode(form)
    resetSignatureSelect()
    var status = form.querySelector("[data-signature-save-status]")
    if (status) status.textContent = ""
    if (name) name.focus()
  }

  manager.addEventListener("click", function (e) {
    var modeBtn = e.target.closest("[data-signature-mode-button]")
    if (modeBtn) {
      var editorForm = modeBtn.closest("[data-signature-form]")
      if (editorForm) setSignatureEditorMode(editorForm, modeBtn.getAttribute("data-signature-mode-button"))
      return
    }

    var newBtn = e.target.closest("[data-signature-new]")
    if (newBtn) {
      resetSignatureForm()
      return
    }

    var row = e.target.closest("[data-signature-row]")
    if (!row) return

    if (e.target.closest("[data-signature-edit]")) {
      var id = form.querySelector("[data-signature-id]")
      var name = form.querySelector("[data-signature-name]")
      var html = form.querySelector("[data-signature-html]")
      if (id) id.value = row.getAttribute("data-signature-id") || ""
      if (name) name.value = row.getAttribute("data-signature-name") || ""
      if (editor) editor.innerHTML = row.getAttribute("data-signature-html") || ""
      if (html) html.value = editor ? editor.innerHTML : (row.getAttribute("data-signature-html") || "")
      var source = form.querySelector("[data-signature-source]")
      if (source) source.value = html ? html.value : (row.getAttribute("data-signature-html") || "")
      setSignatureEditorMode(form, "visual")
      if (editor) editor.focus()
      return
    }

    if (e.target.closest("[data-signature-delete]")) {
      var signatureName = row.getAttribute("data-signature-name") || "this signature"
      var signatureID = row.getAttribute("data-signature-id") || ""
      confirmSignatureDelete(signatureName).then(function (confirmed) {
        if (!confirmed) return
        fetch("/api/signatures/" + encodeURIComponent(signatureID), { method: "DELETE" })
          .then(function (r) { if (!r.ok) throw new Error("Failed to delete signature") })
          .then(reloadDialog)
          .catch(function (err) {
            var status = form && form.querySelector("[data-signature-save-status]")
            if (status) status.textContent = err && err.message ? err.message : "Failed to delete signature"
          })
      })
    }
  })

  manager.addEventListener("click", function (e) {
    var deleteCurrent = e.target.closest("[data-signature-delete-current]")
    if (!deleteCurrent) return
    var id = form && form.querySelector("[data-signature-id]")
    var name = form && form.querySelector("[data-signature-name]")
    var status = form && form.querySelector("[data-signature-save-status]")
    var signatureID = (id && id.value) || ""
    if (!signatureID) {
      if (status) status.textContent = "Choose a signature to delete"
      return
    }
    var signatureName = (name && name.value) || "this signature"
    confirmSignatureDelete(signatureName).then(function (confirmed) {
      if (!confirmed) return
      fetch("/api/signatures/" + encodeURIComponent(signatureID), { method: "DELETE" })
        .then(function (r) { if (!r.ok) throw new Error("Failed to delete signature") })
        .then(reloadDialog)
        .catch(function (err) {
          if (status) status.textContent = err && err.message ? err.message : "Failed to delete signature"
        })
    })
  })

  if (form) {
    form.addEventListener("submit", function (e) {
      e.preventDefault()
      var source = form.querySelector("[data-signature-source]")
      if (source && !source.classList.contains("hidden")) applySignatureSource(form)
      else if (editor) syncSignatureEditor(editor)
      var status = form.querySelector("[data-signature-save-status]")
      if (status) status.textContent = "Saving..."
      fetch("/api/signatures", {
        method: "POST",
        headers: { "Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json" },
        body: new URLSearchParams(new FormData(form)).toString()
      }).then(function (r) {
        if (!r.ok) return r.json().catch(function () { return {} }).then(function (data) { throw new Error(data.error || "Failed to save signature") })
        return r.json()
      }).then(reloadDialog).catch(function (err) {
        if (status) status.textContent = err && err.message ? err.message : "Failed to save signature"
      })
    })
  }

  for (var sf = 0; sf < settingsForms.length; sf++) {
    ;(function (settingsForm) {
    settingsForm.addEventListener("submit", function (e) {
      e.preventDefault()
      var formAccountId = settingsForm.getAttribute("data-account-id") || accountId
      var status = settingsForm.querySelector("[data-signature-settings-status]")
      if (status) status.textContent = "Saving..."
      fetch("/api/accounts/" + encodeURIComponent(formAccountId) + "/signature-settings", {
        method: "POST",
        headers: { "Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json" },
        body: new URLSearchParams(new FormData(settingsForm)).toString()
      }).then(function (r) {
        if (!r.ok) throw new Error("Failed to save assignments")
        if (status) status.textContent = "Saved"
        setTimeout(function () { if (status) status.textContent = "" }, 2000)
      }).catch(function (err) {
        if (status) status.textContent = err && err.message ? err.message : "Failed to save assignments"
      })
    })
    })(settingsForms[sf])
  }

  if (manager.closest("#account-signatures-dialog") && window.tui && window.tui.dialog) {
    setTimeout(function () { window.tui.dialog.open("account-signatures-dialog") }, 20)
  }
}

window.syncSignatureEditor = syncSignatureEditor

function getActiveThemeMode() {
  if (typeof GoferSettings !== "undefined" && GoferSettings.get) {
    return GoferSettings.effectiveTheme()
  }
  return document.documentElement.classList.contains("dark") ? "dark" : "light"
}

function modeButtonClasses(isActive) {
  var base = "inline-flex items-center gap-1.5 px-3 py-1.5 rounded-md text-xs font-medium transition-all duration-200"
  return isActive ? base + " text-foreground" : base + " text-muted-foreground hover:text-foreground"
}

function setupModePickers() {
  document.querySelectorAll("[data-mode-picker]").forEach(function (picker) {
    if (picker.dataset.modePickerReady === "1") return
    picker.dataset.modePickerReady = "1"

    if (getComputedStyle(picker).position === "static") {
      picker.style.position = "relative"
    }

    var indicator = document.createElement("div")
    indicator.setAttribute("data-mode-picker-indicator", "true")
    indicator.style.position = "absolute"
    indicator.style.left = "0"
    indicator.style.top = "0"
    indicator.style.borderRadius = "calc(var(--radius) - 2px)"
    indicator.style.background = "var(--card)"
    indicator.style.border = "1px solid var(--border)"
    indicator.style.boxShadow = "var(--shadow-card)"
    indicator.style.transition = "transform 220ms ease, width 220ms ease, height 220ms ease"
    indicator.style.willChange = "transform, width, height"
    indicator.style.pointerEvents = "none"
    indicator.style.zIndex = "0"
    picker.prepend(indicator)

    var buttons = Array.prototype.slice.call(picker.querySelectorAll("[data-mode]"))
    buttons.forEach(function (btn) {
      btn.style.position = "relative"
      btn.style.zIndex = "1"
      btn.style.background = "transparent"
      btn.style.boxShadow = "none"
      btn.style.borderColor = "transparent"
    })

    function sync(animate) {
      var activeMode = getActiveThemeMode()
      var activeButton = picker.querySelector('[data-mode="' + activeMode + '"]') || buttons[0]
      if (!activeButton) return

      var express = document.documentElement.getAttribute("data-theme") === "express"
      buttons.forEach(function (btn) {
        var isActive = btn === activeButton
        btn.className = modeButtonClasses(isActive)
        btn.setAttribute("aria-pressed", isActive ? "true" : "false")
        if (btn.dataset.mode === "dark") btn.disabled = express
      })

      var pickerRect = picker.getBoundingClientRect()
      var buttonRect = activeButton.getBoundingClientRect()
      var left = buttonRect.left - pickerRect.left
      var top = buttonRect.top - pickerRect.top

      if (!animate) {
        var previousTransition = indicator.style.transition
        indicator.style.transition = "none"
        indicator.style.width = buttonRect.width + "px"
        indicator.style.height = buttonRect.height + "px"
        indicator.style.transform = "translate(" + left + "px, " + top + "px)"
        void indicator.offsetHeight
        indicator.style.transition = previousTransition
        return
      }

      indicator.style.width = buttonRect.width + "px"
      indicator.style.height = buttonRect.height + "px"
      indicator.style.transform = "translate(" + left + "px, " + top + "px)"
    }

    picker.__syncModePicker = sync
    requestAnimationFrame(function () { sync(false) })
  })
}

function refreshModePickers(animate) {
  document.querySelectorAll("[data-mode-picker]").forEach(function (picker) {
    if (typeof picker.__syncModePicker === "function") {
      picker.__syncModePicker(animate !== false)
    }
  })
}

if (typeof MutationObserver !== "undefined" && document.documentElement) {
  var themeObserver = new MutationObserver(function (mutations) {
    for (var i = 0; i < mutations.length; i++) {
      if (mutations[i].type === "attributes") {
        refreshModePickers(true)
        break
      }
    }
  })

  themeObserver.observe(document.documentElement, {
    attributes: true,
    attributeFilter: ["class", "data-theme"],
  })

  document.fonts.ready.then(function () {
    refreshModePickers(false)
  })
}

var settingsTabs = [
  "accounts",
  "sync",
  "operations",
  "contacts",
  "appearance",
  "regional",
  "compose-display",
  "security",
  "advanced",
  "export-import",
  "about",
]

function normalizeSettingsTab(tab) {
  return settingsTabs.indexOf(tab) !== -1 ? tab : "accounts"
}

function setupSettingsHistory() {
  if (!window.location.pathname.startsWith("/settings")) return
  var parts = window.location.pathname.replace(/\/+$/, "").split("/")
  var tab = normalizeSettingsTab(parts[2] || "accounts")
  history.replaceState({ settingsTab: tab }, "", window.location.pathname)
}

function setupSettingsSidebar() {
  if (setupSettingsSidebar.ready) return
  setupSettingsSidebar.ready = true

  document.addEventListener("click", function (e) {
    var link = e.target.closest && e.target.closest("[data-settings-sidebar-link]")
    if (!link) return
    setSettingsSidebarActive(link.getAttribute("data-settings-sidebar-value"))
  })

  window.addEventListener("popstate", function () {
    setSettingsSidebarActive(settingsTabFromLocation())
  })
}

function refreshEmailLinkHandler() {
  var button = document.querySelector("[data-mailto-handler-button]")
  var testButton = document.querySelector("[data-mailto-handler-test-button]")
  var status = document.querySelector("[data-mailto-handler-status]")
  if (!button || !status) return

  if (!window.isSecureContext) {
    button.disabled = true
    if (testButton) testButton.disabled = true
    status.textContent = "Available when Gofer is served over HTTPS or from localhost."
    return
  }
  if (!navigator.registerProtocolHandler) {
    button.disabled = true
    if (testButton) testButton.disabled = true
    status.textContent = "This browser does not support registering web email clients."
    return
  }

  button.disabled = false
  if (testButton) testButton.disabled = false
  var state = readEmailLinkHandlerState()
  if (state === "confirmed") {
    status.textContent = "Previously confirmed on this browser. You can test it again if your browser settings have changed."
  } else if (state === "requested") {
    status.textContent = "Registration was requested on this browser. Test an email link to confirm it."
  } else {
    status.textContent = "Your browser will ask you to confirm. Installed Gofer apps can also handle links from other applications."
  }
}

function readEmailLinkHandlerState() {
  try {
    var stored = JSON.parse(window.localStorage.getItem("gofer_mailto_handler_state") || "null")
    return stored && (stored.status === "requested" || stored.status === "confirmed") ? stored.status : ""
  } catch (_) {
    return ""
  }
}

function writeEmailLinkHandlerState(status) {
  try {
    var current = readEmailLinkHandlerState()
    if (current === "confirmed" && status === "requested") return
    window.localStorage.setItem("gofer_mailto_handler_state", JSON.stringify({ status: status, updated_at: Date.now() }))
  } catch (_) {}
}

function setupEmailLinkHandler() {
  refreshEmailLinkHandler()
  if (setupEmailLinkHandler.ready) return
  setupEmailLinkHandler.ready = true

  document.addEventListener("click", function (event) {
    var testButton = event.target.closest && event.target.closest("[data-mailto-handler-test-button]")
    if (testButton && !testButton.disabled) {
      event.preventDefault()
      var token = ""
      try {
        token = window.crypto && window.crypto.randomUUID ? window.crypto.randomUUID() : String(Date.now()) + "-" + Math.random().toString(36).slice(2)
        window.localStorage.setItem("gofer_mailto_handler_test_token", token)
      } catch (_) {}
      window.location.href = "mailto:?x-gofer-handler-test=" + encodeURIComponent(token)
      return
    }

    var button = event.target.closest && event.target.closest("[data-mailto-handler-button]")
    if (!button || button.disabled) return
    event.preventDefault()

    var status = document.querySelector("[data-mailto-handler-status]")
    try {
      navigator.registerProtocolHandler("mailto", window.location.origin + "/?mailto=%s")
      writeEmailLinkHandlerState("requested")
      refreshEmailLinkHandler()
      if (typeof showGoferToast === "function") {
        showGoferToast({
          id: "mailto-handler-registration",
          title: "Email-link registration requested",
          description: "Confirm the request in your browser to open mail links with Gofer.",
          variant: "success",
          icon: "success",
          position: "bottom-right",
          duration: 6000,
          dismissible: true,
        })
      }
    } catch (error) {
      if (status) status.textContent = "Gofer could not request registration in this browser."
      if (typeof showGoferToast === "function") {
        showGoferToast({
          id: "mailto-handler-registration",
          title: "Could not register Gofer",
          description: error && error.message ? error.message : "Check your browser's protocol-handler settings and try again.",
          variant: "error",
          icon: "error",
          position: "bottom-right",
          duration: 8000,
          dismissible: true,
        })
      }
    }
  })
}

function settingsTabFromLocation() {
  var parts = window.location.pathname.replace(/\/+$/, "").split("/")
  return normalizeSettingsTab(parts[2] || "accounts")
}

function setSettingsSidebarActive(value) {
  if (!value) return
  document.querySelectorAll("[data-settings-sidebar-link]").forEach(function (link) {
    var active = link.getAttribute("data-settings-sidebar-value") === value
    link.classList.toggle("bg-sidebar-accent", active)
    link.classList.toggle("text-sidebar-primary", active)
    link.classList.toggle("font-medium", active)
    link.classList.toggle("text-sidebar-foreground", !active)
    link.classList.toggle("hover:bg-sidebar-accent/60", !active)
    link.classList.toggle("hover:text-sidebar-accent-foreground", !active)
    if (active) link.setAttribute("aria-current", "true")
    else link.removeAttribute("aria-current")
  })
}

document.body.addEventListener("htmx:afterSwap", function (event) {
  var section = event.target
  var request = event.detail && event.detail.requestConfig
  if (!section || section.id !== "settings-content" || !request || request.verb !== "get") return
  animateSectionContent(section)
})

document.body.addEventListener("htmx:afterSettle", function (e) {
  if (!e.target || !e.target.querySelector) return
  var signaturesTarget = e.target.matches && e.target.matches("[data-account-signatures-manager]")
  if (signaturesTarget || e.target.querySelector("[data-account-signatures-manager]")) {
    setupAccountSignaturesDialog(signaturesTarget ? e.target.parentNode || e.target : e.target)
    if (window.__goferReopenSignaturesDialog && window.tui && window.tui.dialog) {
      var dialogID = window.__goferReopenSignaturesDialog
      window.__goferReopenSignaturesDialog = ""
      setTimeout(function () { window.tui.dialog.open(dialogID) }, 20)
    }
  }
  var settingsContentTarget = e.target.id === "settings-content"
  if (!settingsContentTarget && !e.target.querySelector("[data-settings-page]") && !e.target.querySelector("[data-mode-picker]")) return
  setupSettingsHistory()
  setSettingsSidebarActive(settingsTabFromLocation())
  setupModePickers()
  setupEmailLinkHandler()
  requestAnimationFrame(function () {
    refreshModePickers(false)
  })
})

document.addEventListener("click", function (e) {
  var trigger = e.target.closest('[data-tui-tabs-trigger]')
  if (!trigger) return
  var value = trigger.getAttribute('data-tui-tabs-value')
  if (value === 'appearance') {
    requestAnimationFrame(function () {
      if (typeof refreshModePickers === 'function') refreshModePickers(false)
    })
  }
})

// Opens the edit account dialog at its first step once it has been loaded.
function setupEditAccountDialog() {
  var dlg = document.querySelector("#edit-account-dialog dialog[data-tui-dialog-content]")
  if (!dlg) return

  if (window.tui && window.tui.dialog) {
    window.tui.dialog.open("edit-account-dialog")
  }

  requestAnimationFrame(function () {
    var track = document.getElementById("edit-wizard-track")
    if (!track || !dlg.open) return

    track.style.transform = "translateX(0)"
    if (typeof _editWizStep !== "undefined") _editWizStep = 0

    document.querySelectorAll("#edit-account-dialog [data-account-wizard-step]").forEach(function (item) {
      item.dataset.active = String(+item.dataset.accountWizardStep === 0)
    })
  })
}

document.addEventListener("htmx:afterSettle", function (e) {
  if (e.target && e.target.querySelector && e.target.querySelector("#edit-account-dialog")) {
    setTimeout(setupEditAccountDialog, 20)
  }
})

// htmx leaves the page as it was when a request fails, so the About page's
// update check says so itself.
function showAboutUpdateCheckFailure(e) {
  var check = e.target && e.target.closest && e.target.closest("#about-update-check")
  var status = check && check.querySelector("[data-about-update-status]")
  if (!status) return
  var message = document.createElement("p")
  message.className = "text-destructive"
  var xhr = e.detail && e.detail.xhr
  message.textContent = xhr && xhr.status
    ? "Could not check for updates (the server answered " + xhr.status + ")."
    : "Could not reach Gofer to check for updates."
  status.replaceChildren(message)
}
document.addEventListener("htmx:responseError", showAboutUpdateCheckFailure)
document.addEventListener("htmx:sendError", showAboutUpdateCheckFailure)
