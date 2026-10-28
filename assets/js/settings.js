document.addEventListener("DOMContentLoaded", function () {
  setupSettingsHistory()
  setupSettingsSidebar()
  setupModePickers()
  setupAccountColorPickers()
  setupAccountOrder()
  setupMailRuleForm()
  setupAccountSignaturesDialog(document)
  setupEmailLinkHandler()
})

function accountTestDialog(event) {
  var trigger = (event.detail && event.detail.elt) || event.currentTarget || event.target
  var dialogID = trigger && trigger.getAttribute && trigger.getAttribute("data-account-test-dialog")
  return dialogID ? document.getElementById(dialogID) : null
}

// setAccountTestService shows a service's state on its card. A warning, for a
// connection that works with a problem, goes in its own box under the card.
function setAccountTestService(section, state, status, detail, warning) {
  if (!section) return

  section.dataset.state = state
  var warningBox = section.nextElementSibling
  if (warningBox && warningBox.hasAttribute("data-account-test-service-warning")) {
    var warningText = warningBox.querySelector("[data-account-test-service-warning-text]")
    if (warningText) warningText.textContent = warning || ""
    warningBox.classList.toggle("hidden", !warning)
  }
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
        setAccountTestService(section, "success", result.message || "Connection successful", "", result.warning)
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

// An identity's SMTP server fields show while "Another SMTP server" is chosen.
// They open and close smoothly, and the dialog grows and shrinks with them.
function toggleIdentitySMTP(input) {
  var fieldset = input && input.closest ? input.closest("[data-identity-send-through]") : null
  var fields = fieldset ? fieldset.querySelector("[data-identity-smtp-fields]") : null
  if (!fields) return
  var show = input.checked && input.value === "smtp"
  if (fields._sendThroughAnimation) {
    // A change of mind midway: the earlier animation no longer decides.
    var earlier = fields._sendThroughAnimation
    fields._sendThroughAnimation = null
    earlier.cancel()
    fields.style.overflow = ""
  }
  if (show === !fields.classList.contains("hidden")) return
  fields.classList.remove("hidden")
  if (!fields.animate || window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
    fields.classList.toggle("hidden", !show)
    return
  }
  var style = getComputedStyle(fields)
  var open = {
    height: fields.offsetHeight + "px", opacity: 1,
    marginTop: style.marginTop, paddingTop: style.paddingTop, paddingBottom: style.paddingBottom,
    borderTopWidth: style.borderTopWidth, borderBottomWidth: style.borderBottomWidth,
  }
  var closed = { height: "0px", opacity: 0, marginTop: "0px", paddingTop: "0px", paddingBottom: "0px", borderTopWidth: "0px", borderBottomWidth: "0px" }
  fields.style.overflow = "hidden"
  var animation = fields.animate(show ? [closed, open] : [open, closed], { duration: 220, easing: "ease" })
  fields._sendThroughAnimation = animation
  function settle() {
    if (fields._sendThroughAnimation !== animation) return
    fields._sendThroughAnimation = null
    fields.style.overflow = ""
    fields.classList.toggle("hidden", !show)
  }
  animation.onfinish = settle
  animation.oncancel = settle
}

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

// Accounts on the Accounts page and rules on the Rules page are put in order
// by dragging an item's handle, or with the arrow keys on a focused handle.
// Accounts also move with Move up and Move down in their menu; accounts being
// deleted keep their place and can't be moved.
var accountOrder = {
  list: "[data-account-order-list]",
  item: "[data-settings-account-card]",
  handle: "[data-account-order-handle]",
  id: function (item) { return item.getAttribute("data-account-id") },
  movable: function (item) { return item.getAttribute("data-account-deleting") !== "true" },
  save: function (list, previous) { saveAccountOrder(list, previous) }
}

var mailRuleOrder = {
  list: "[data-mail-rule-order-list]",
  item: "[data-mail-rule-row]",
  handle: "[data-mail-rule-order-handle]",
  id: function (item) { return item.getAttribute("data-mail-rule-id") },
  movable: function () { return true },
  save: function (list, previous) { saveMailRuleOrder(list, previous) }
}

var orderKinds = [accountOrder, mailRuleOrder]

function orderKindOf(el) {
  for (var i = 0; i < orderKinds.length; i++) {
    if (el.closest(orderKinds[i].list)) return orderKinds[i]
  }
  return null
}

function orderItems(kind, list) {
  return Array.prototype.filter.call(list.querySelectorAll(kind.item), function (item) {
    return item.parentNode === list && kind.movable(item)
  })
}

function orderIDs(kind, list) {
  return orderItems(kind, list).map(kind.id)
}

function accountOrderCards(list) {
  return orderItems(accountOrder, list)
}

function accountOrderIDs(list) {
  return orderIDs(accountOrder, list)
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

// moveOrderItem puts item before ref (or last among the movable items when
// ref is null), sliding the items that make room from where they were.
function moveOrderItem(kind, list, item, ref) {
  var items = Array.prototype.slice.call(list.querySelectorAll(kind.item))
  var before = items.map(function (c) { return c.getBoundingClientRect().top })
  if (ref) {
    list.insertBefore(item, ref)
  } else {
    var movable = orderItems(kind, list).filter(function (c) { return c !== item })
    var last = movable[movable.length - 1]
    list.insertBefore(item, last ? last.nextSibling : null)
  }
  items.forEach(function (c, i) {
    if (c === item && item.hasAttribute("data-order-dragging")) return
    var dy = before[i] - c.getBoundingClientRect().top
    if (!dy) return
    c.style.transition = "none"
    c.style.transform = "translateY(" + dy + "px)"
    c.getBoundingClientRect()
    c.style.transition = ""
    c.style.transform = ""
  })
}

function moveOrderItemBy(kind, item, step) {
  var list = item && item.closest(kind.list)
  if (!list) return
  var items = orderItems(kind, list)
  var at = items.indexOf(item)
  var to = at + step
  if (at < 0 || to < 0 || to >= items.length) return
  var previous = orderIDs(kind, list)
  moveOrderItem(kind, list, item, step < 0 ? items[to] : items[to + 1] || null)
  kind.save(list, previous)
}

function restoreOrder(kind, list, ids) {
  var items = orderItems(kind, list)
  ids.forEach(function (id) {
    var item = items.filter(function (c) { return kind.id(c) === id })[0]
    if (item) moveOrderItem(kind, list, item, null)
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
    restoreOrder(accountOrder, list, previous)
    syncAccountOrderControls()
    showGoferToast({ title: "Could not save the account order", description: "Check your connection and try again.", variant: "error", icon: "error", duration: 6000 })
  })
}

function saveMailRuleOrder(list, previous) {
  var ids = orderIDs(mailRuleOrder, list)
  if (ids.join("\n") === previous.join("\n")) return
  var body = new URLSearchParams()
  ids.forEach(function (id) { body.append("rule_id", id) })
  fetch("/api/settings/rules/order", {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: body.toString()
  }).then(function (res) {
    if (res.ok) return
    if (res.status === 409) {
      showGoferToast({ title: "Your rules changed", description: "The list was reloaded. Put the rules in order again.", variant: "error", icon: "error", duration: 6000 })
      if (window.htmx) window.htmx.ajax("GET", "/api/settings/rules/panel", { target: "#mail-rules-panel", swap: "outerHTML" })
      return
    }
    throw new Error("save rule order: " + res.status)
  }).catch(function () {
    restoreOrder(mailRuleOrder, list, previous)
    showGoferToast({ title: "Could not save the rule order", description: "Check your connection and try again.", variant: "error", icon: "error", duration: 6000 })
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
    if (card) moveOrderItemBy(accountOrder, card, item.getAttribute("data-account-move") === "up" ? -1 : 1)
  })

  function orderHandle(target) {
    if (!target.closest) return null
    for (var i = 0; i < orderKinds.length; i++) {
      var handle = target.closest(orderKinds[i].handle)
      if (handle) return { kind: orderKinds[i], handle: handle }
    }
    return null
  }

  document.addEventListener("keydown", function (e) {
    var found = orderHandle(e.target)
    if (!found || (e.key !== "ArrowUp" && e.key !== "ArrowDown")) return
    e.preventDefault()
    moveOrderItemBy(found.kind, found.handle.closest(found.kind.item), e.key === "ArrowUp" ? -1 : 1)
    found.handle.focus()
  })

  document.addEventListener("pointerdown", function (e) {
    var found = orderHandle(e.target)
    if (!found || e.button !== 0) return
    var kind = found.kind
    var handle = found.handle
    var card = handle.closest(kind.item)
    var list = card && card.closest(kind.list)
    if (!list) return
    e.preventDefault()
    handle.setPointerCapture(e.pointerId)
    drag = { kind: kind, list: list, card: card, handle: handle, pointerId: e.pointerId, previous: orderIDs(kind, list), grabY: e.clientY - card.getBoundingClientRect().top }
    card.setAttribute("data-order-dragging", "")
    list.setAttribute("data-order-active", "")
    drag.placeholder = document.createElement("div")
    drag.placeholder.setAttribute("data-order-placeholder", "")
    drag.placeholder.setAttribute("aria-hidden", "true")
    followOrderDrag(e.clientY)
    list.appendChild(drag.placeholder)
  })

  // The grabbed item floats under the pointer, a little larger, while an
  // outline shows the place in the list where it would drop.
  function orderItemTop(list, item) {
    return item.offsetParent === list ? item.offsetTop : item.offsetTop - list.offsetTop
  }

  function followOrderDrag(clientY) {
    var y = clientY - drag.list.getBoundingClientRect().top - drag.grabY
    var top = orderItemTop(drag.list, drag.card)
    drag.card.style.transform = "translateY(" + (y - top) + "px) scale(1.01)"
    drag.placeholder.style.top = top + "px"
    drag.placeholder.style.height = drag.card.offsetHeight + "px"
  }

  document.addEventListener("pointermove", function (e) {
    if (!drag || e.pointerId !== drag.pointerId) return
    // Offsets ignore the slide of items making room, so the place under the
    // pointer doesn't shift while they move.
    var items = orderItems(drag.kind, drag.list)
    var others = items.filter(function (c) { return c !== drag.card })
    // An item makes way once the floating item's leading edge passes its middle.
    var at = items.indexOf(drag.card)
    var floatTop = e.clientY - drag.list.getBoundingClientRect().top - drag.grabY
    var floatBottom = floatTop + drag.card.offsetHeight
    var to = 0
    others.forEach(function (other, k) {
      var middle = orderItemTop(drag.list, other) + other.offsetHeight / 2
      if (k < at ? floatTop >= middle : floatBottom > middle) to++
    })
    if (to !== at) moveOrderItem(drag.kind, drag.list, drag.card, others[to] || null)
    followOrderDrag(e.clientY)
  })

  function endDrag(e) {
    if (!drag || e.pointerId !== drag.pointerId) return
    var done = drag
    drag = null
    // Back in the item's own transition, it settles into its place.
    done.card.removeAttribute("data-order-dragging")
    done.list.removeAttribute("data-order-active")
    done.placeholder.remove()
    done.card.style.transform = ""
    if (done.handle.hasPointerCapture(e.pointerId)) done.handle.releasePointerCapture(e.pointerId)
    done.kind.save(done.list, done.previous)
  }
  document.addEventListener("pointerup", endDrag)
  document.addEventListener("pointercancel", endDrag)

  syncAccountOrderControls()
  document.body.addEventListener("htmx:afterSettle", syncAccountOrderControls)
}

// Opens the rule dialog once it has been loaded. Changes made in it swap only
// its inside; saving or running a rule shows the list again and closes it.
document.addEventListener("htmx:afterSettle", function (e) {
  if (e.target && e.target.querySelector && e.target.querySelector("#mail-rule-dialog") && window.tui && window.tui.dialog) {
    setTimeout(function () { window.tui.dialog.open("mail-rule-dialog") }, 20)
  }
})

document.body.addEventListener("mail-rule-dialog-done", function () {
  if (window.tui && window.tui.dialog && document.getElementById("mail-rule-dialog")) window.tui.dialog.close("mail-rule-dialog")
})

// Conditions, groups and actions added to a rule's form grow and fade in;
// removed ones shrink and fade out before the form is shown again. Rules
// added to the list grow in, and a deleted rule shrinks away. Any other
// redraw of the form eases the dialog to its new height. The pace is the
// app's own for things opening and closing.
var mailRuleMotion = { duration: 220, easing: "ease" }

function mailRuleStill() {
  return !Element.prototype.animate || window.matchMedia("(prefers-reduced-motion: reduce)").matches
}

// mailRuleFold grows el in from nothing, or with out, shrinks it away.
function mailRuleFold(el, out) {
  if (!el || mailRuleStill()) return null
  var style = getComputedStyle(el)
  var open = {
    height: el.offsetHeight + "px", opacity: 1,
    marginTop: style.marginTop, marginBottom: style.marginBottom, paddingTop: style.paddingTop, paddingBottom: style.paddingBottom,
    borderTopWidth: style.borderTopWidth, borderBottomWidth: style.borderBottomWidth,
  }
  var closed = { height: "0px", opacity: 0, marginTop: "0px", marginBottom: "0px", paddingTop: "0px", paddingBottom: "0px", borderTopWidth: "0px", borderBottomWidth: "0px" }
  var overflow = el.style.overflow
  el.style.overflow = "hidden"
  var animation = el.animate(out ? [open, closed] : [closed, open], { duration: mailRuleMotion.duration, easing: mailRuleMotion.easing, fill: out ? "forwards" : "none" })
  animation.onfinish = animation.oncancel = function () { if (!out) el.style.overflow = overflow }
  return animation
}

// mailRuleRemoved is what removing with button takes away: its row, or
// the whole group with the "or" beside it when the row is the group's last.
function mailRuleRemoved(button) {
  var row = button.closest("[data-mail-rule-condition], [data-mail-rule-action]")
  if (!row || !row.hasAttribute("data-mail-rule-condition")) return row ? [row] : []
  var group = row.closest("[data-mail-rule-condition-group]")
  if (!group || group.querySelectorAll("[data-mail-rule-condition]").length > 1) return [row]
  var badge = group.previousElementSibling || (group.nextElementSibling && group.nextElementSibling.hasAttribute("data-mail-rule-or") ? group.nextElementSibling : null)
  return badge && badge.hasAttribute("data-mail-rule-or") ? [group, badge] : [group]
}

var mailRuleSwap = null

document.addEventListener("htmx:beforeRequest", function (e) {
  var button = e.detail && e.detail.elt
  var op = button && button.getAttribute && button.getAttribute("data-mail-rule-row-op")
  if (op === "remove_condition" || op === "remove_action") mailRuleRemoved(button).forEach(function (el) { mailRuleFold(el, true) })
})

// A removal the server didn't take shows the row again.
document.addEventListener("htmx:afterRequest", function (e) {
  var button = e.detail && e.detail.elt
  var op = button && button.getAttribute && button.getAttribute("data-mail-rule-row-op")
  if ((op !== "remove_condition" && op !== "remove_action") || e.detail.successful) return
  mailRuleRemoved(button).forEach(function (el) {
    el.getAnimations().forEach(function (animation) { animation.cancel() })
    el.style.overflow = ""
  })
})

document.body.addEventListener("htmx:beforeSwap", function (e) {
  var target = e.detail && e.detail.target
  if (!target || !e.detail.shouldSwap) return
  var elt = e.detail.requestConfig && e.detail.requestConfig.elt
  if (target.id === "mail-rule-dialog-panel") {
    var op = elt && elt.getAttribute && elt.getAttribute("data-mail-rule-row-op")
    var runView = target.querySelector("[data-mail-rule-run]")
    var content = target.closest("[data-tui-dialog-content]")
    mailRuleSwap = {
      panel: true,
      bounds: content ? content.getBoundingClientRect() : null,
      runState: runView ? runView.getAttribute("data-mail-rule-run-state") : "",
      condition: elt && elt.getAttribute && elt.getAttribute("data-mail-rule-add-condition"),
      action: op === "add_action",
      removed: op === "remove_condition" || op === "remove_action",
    }
  } else if (target.id === "mail-rules-panel") {
    mailRuleSwap = { list: true, loaded: target.hasAttribute("data-mail-rules-panel"), height: target.offsetHeight, rules: Array.prototype.map.call(target.querySelectorAll("[data-mail-rule-row]"), function (row) { return row.getAttribute("data-mail-rule-id") }) }
  }
})

document.body.addEventListener("htmx:afterSwap", function () {
  var swap = mailRuleSwap
  mailRuleSwap = null
  if (!swap || mailRuleStill()) return
  if (swap.list) {
    // The first load eases from the loading shape to the list, which
    // fades in, as the app's panels do.
    if (!swap.loaded) {
      var list = document.getElementById("mail-rules-panel")
      if (!list) return
      var height = list.offsetHeight
      if (Math.abs(height - swap.height) > 1) {
        list.style.overflow = "hidden"
        var grow = list.animate([{ height: swap.height + "px" }, { height: height + "px" }], mailRuleMotion)
        grow.onfinish = grow.oncancel = function () { list.style.overflow = "" }
      }
      list.animate([{ opacity: 0, transform: "translateY(5px)" }, { opacity: 1, transform: "translateY(0)" }], { duration: 180, easing: "ease-out" })
      return
    }
    document.querySelectorAll("#mail-rules-panel [data-mail-rule-row]").forEach(function (row) {
      if (swap.rules.indexOf(row.getAttribute("data-mail-rule-id")) < 0) mailRuleFold(row, false)
    })
    return
  }
  var panel = document.getElementById("mail-rule-dialog-panel")
  if (!panel) return
  var added = null
  if (swap.condition === "-1") {
    var groups = panel.querySelectorAll("[data-mail-rule-condition-group]")
    added = groups[groups.length - 1]
    var badge = added && added.previousElementSibling
    if (badge && badge.hasAttribute("data-mail-rule-or")) mailRuleFold(badge, false)
  } else if (swap.condition !== null && swap.condition !== undefined) {
    var rows = panel.querySelectorAll('[data-mail-rule-condition-group="' + swap.condition + '"] [data-mail-rule-condition]')
    added = rows[rows.length - 1]
  } else if (swap.action) {
    var actions = panel.querySelectorAll("[data-mail-rule-action]")
    added = actions[actions.length - 1]
  }
  if (added) {
    mailRuleFold(added, false)
    return
  }
  // A removal has already shrunk away. Anything else that changes the
  // dialog's height eases the dialog to it; what is in it is left alone.
  var content = panel.closest("[data-tui-dialog-content]")
  if (!swap.removed && content && swap.bounds) {
    // A resize still running reports its own height; stop it first, so the
    // new one measures the real height and carries on from where the
    // dialog is.
    if (content._mailRuleResize) content._mailRuleResize.cancel()
    var height = content.offsetHeight
    if (Math.abs(height - swap.bounds.height) >= 1) {
      var resize = content.animate([{ height: swap.bounds.height + "px" }, { height: height + "px" }], mailRuleMotion)
      content._mailRuleResize = resize
      resize.onfinish = resize.oncancel = function () { if (content._mailRuleResize === resize) content._mailRuleResize = null }
    }
  }
  // A run that moved on shows its new state as the app's sections come in.
  // A new count fades its progress in by itself, once it takes a while.
  var runView = panel.querySelector("[data-mail-rule-run]")
  var runState = runView && runView.getAttribute("data-mail-rule-run-state")
  var status = runView && runView.querySelector("[data-mail-rule-run-status]")
  if (status && swap.runState && runState !== swap.runState && runState !== "counting") {
    status.animate([{ opacity: 0, transform: "translateY(5px)" }, { opacity: 1, transform: "translateY(0)" }], { duration: 180, easing: "ease-out" })
  }
})

// A rule run by hand counts, then changes mail, in the background. Its
// progress arrives as events: the open run dialog moves its bar and counts,
// and is drawn again when the run's state changes. A run that ends while
// its dialog is closed says so in a toast. The newest event of each run is
// kept, for a dialog drawn after it arrived.
var mailRuleRunLatest = {}

// mailRuleRunView is the open run dialog of a job; drawn says a dialog that
// was just drawn counts too, though it opens a moment later.
function mailRuleRunView(job, drawn) {
  var view = document.querySelector('#mail-rule-dialog-panel [data-mail-rule-run="' + cssEscapeSettings(job) + '"]')
  var dialog = view && view.closest("dialog")
  return view && dialog && (dialog.open || drawn) ? view : null
}

function showMailRuleRun(run, drawn) {
  var view = mailRuleRunView(run.job, drawn)
  if (!view) return false
  // The dialog may already show a newer state than this report.
  if (Number(run.seq) <= Number(view.getAttribute("data-mail-rule-run-seq"))) return true
  view.setAttribute("data-mail-rule-run-seq", String(run.seq))
  if (view.getAttribute("data-mail-rule-run-state") !== run.state) {
    if (window.htmx) window.htmx.ajax("GET", "/api/settings/rule-runs/" + encodeURIComponent(run.job), { target: "#mail-rule-dialog-panel", swap: "outerHTML" })
    return true
  }
  var bar = view.querySelector("[data-mail-rule-run-progress]")
  if (bar) {
    var value = run.state === "running" ? run.processed : run.scanned
    var max = run.state === "running" ? run.matches : run.total
    bar.setAttribute("aria-valuemax", String(Math.max(max, 1)))
    bar.setAttribute("aria-valuenow", String(value))
    var indicator = bar.querySelector("[data-tui-progress-indicator]")
    if (indicator) indicator.style.width = (max > 0 ? Math.min(100, value * 100 / max) : 0) + "%"
  }
  view.querySelectorAll("[data-mail-rule-run-field]").forEach(function (field) {
    var n = Number(run[field.getAttribute("data-mail-rule-run-field")]) || 0
    field.textContent = n.toLocaleString("en-US")
  })
  return true
}

document.addEventListener("gofer:mail-rule-run", function (e) {
  var run = e.detail
  mailRuleRunLatest[run.job] = run
  if (showMailRuleRun(run)) return
  if (run.state === "done") {
    showGoferToast({ id: "mail-rule-run-" + run.job, title: "“" + run.rule_name + "” finished", description: run.notice, variant: "success", icon: "success", position: "bottom-right", duration: 8000, dismissible: true })
  } else if (run.state === "error") {
    showGoferToast({ id: "mail-rule-run-" + run.job, title: "“" + run.rule_name + "” stopped", description: run.error, variant: "error", icon: "error", position: "bottom-right", duration: 8000, dismissible: true })
  }
})

document.body.addEventListener("htmx:afterSettle", function () {
  var view = document.querySelector("#mail-rule-dialog-panel [data-mail-rule-run]")
  var run = view && mailRuleRunLatest[view.getAttribute("data-mail-rule-run")]
  if (run) showMailRuleRun(run, true)
})

// Deleting a rule asks first.
document.addEventListener("click", function (e) {
  var button = e.target.closest && e.target.closest("[data-mail-rule-delete]")
  if (!button || typeof window.goferConfirm !== "function" || !window.htmx) return
  e.preventDefault()
  window.goferConfirm({
    title: "Delete rule?",
    message: "“" + button.getAttribute("data-mail-rule-name") + "” stops acting on new mail. What it already did to messages stays as it is.",
    confirmLabel: "Delete rule",
    destructive: true,
  }).then(function (accepted) {
    if (!accepted || !document.getElementById("mail-rules-panel")) return
    var row = button.closest("[data-mail-rule-row]")
    var fold = mailRuleFold(row, true)
    var remove = function () { window.htmx.ajax("DELETE", button.getAttribute("data-mail-rule-delete"), { target: "#mail-rules-panel", swap: "outerHTML" }) }
    if (fold) fold.onfinish = remove
    else remove()
  })
})

// New folder… in a rule's Move to folder, or the button beside a folder,
// creates a folder in the rule's account, at the top or inside that folder,
// as the account's Folders dialog does, then chooses it. It is caught before
// the folder picker would take the click as choosing the folder beside it.
document.addEventListener("click", function (e) {
  var button = e.target.closest && e.target.closest("[data-mail-rule-new-folder]")
  if (!button || typeof window.goferCreateMailFolder !== "function") return
  e.preventDefault()
  e.stopPropagation()
  var row = button.closest("[data-mail-rule-action]")
  var input = row && row.querySelector('input[name="act_folder"]')
  var body = button.closest("[data-mail-rule-form-body]")
  var content = button.closest("[popover]")
  if (content && window.tui && window.tui.popover && window.tui.popover.closeElement) window.tui.popover.closeElement(content)
  var parent = button.getAttribute("data-mail-rule-new-folder-parent") || ""
  var parentName = button.getAttribute("data-mail-rule-new-folder-parent-name") || ""
  var title = parent ? "New subfolder in " + parentName : "New folder"
  window.goferCreateMailFolder(button.getAttribute("data-mail-rule-new-folder"), parent, parentName, { title: title }).then(function (folder) {
    if (!folder || !input || !body || !body.isConnected || !window.htmx) return
    input.value = "id:" + folder.id
    window.htmx.trigger(body, "mail-rule-rerender")
  })
}, true)

// A rule's form is shown again when a choice that others depend on changes,
// such as the account its folders come from.
function setupMailRuleForm() {
  if (setupMailRuleForm.ready) return
  setupMailRuleForm.ready = true
  document.addEventListener("change", function (e) {
    var input = e.target
    if (!input.matches || !input.matches("[data-mail-rule-rerender]") || !window.htmx) return
    var body = input.closest("[data-mail-rule-form-body]")
    if (body) window.htmx.trigger(body, "mail-rule-rerender")
  })
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

      var style = document.documentElement.getAttribute("data-theme")
      var express = style === "express"
      // The Custom theme has its own mode.
      var custom = style === "custom"
      buttons.forEach(function (btn) {
        var isActive = btn === activeButton
        btn.className = modeButtonClasses(isActive)
        btn.setAttribute("aria-pressed", isActive ? "true" : "false")
        btn.disabled = custom || (btn.dataset.mode === "dark" && express)
      })
      var customNote = picker.querySelector("[data-mode-custom-note]")
      if (customNote) customNote.classList.toggle("hidden", !custom)

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
  "rules",
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

// Opens an account's identities dialog once it has been loaded. Changes made
// in it swap only its panel, so it stays open.
document.addEventListener("htmx:afterSettle", function (e) {
  if (e.target && e.target.querySelector && e.target.querySelector("#account-identities-dialog") && window.tui && window.tui.dialog) {
    setTimeout(function () { window.tui.dialog.open("account-identities-dialog") }, 20)
  }
})

// Opens an account's folders dialog once it has been loaded. Its list loads
// again after each change made from it, or from the sidebar while it's open.
document.addEventListener("htmx:afterSettle", function (e) {
  if (e.target && e.target.querySelector && e.target.querySelector("#folders-dialog") && window.tui && window.tui.dialog) {
    setTimeout(function () { window.tui.dialog.open("folders-dialog") }, 20)
  }
})

document.addEventListener("gofer:folders-changed", function (e) {
  var panel = document.getElementById("folders-panel")
  if (!panel || !window.htmx) return
  if (e.detail && e.detail.accountID && e.detail.accountID !== panel.getAttribute("data-account-id")) return
  htmx.ajax("GET", panel.getAttribute("data-folders-panel"), { target: "#folders-panel", swap: "outerHTML" })
})

// Opens an IMAP account's folder roles dialog once it has been loaded. Saving
// swaps only its panel, so it stays open.
document.addEventListener("htmx:afterSettle", function (e) {
  if (e.target && e.target.querySelector && e.target.querySelector("#folder-roles-dialog") && window.tui && window.tui.dialog) {
    setTimeout(function () { window.tui.dialog.open("folder-roles-dialog") }, 20)
  }
})

// The Mail operations list counts down to reloading itself. Each reload, or
// a press of Refresh, replaces the whole list, which starts a new countdown.
// While the tab is hidden the countdown waits at zero, and reloads once the
// tab is shown again.
// A reload that fails leaves the list as it was and counts down again.
goferOnContent("#mail-operations-content[data-mail-operations-refresh]", function (content) {
  var label = content.querySelector("[data-mail-operations-countdown]")
  var seconds = parseInt(content.getAttribute("data-mail-operations-refresh"), 10) || 15
  var timer = null
  function countDown(prefix) {
    var left = seconds
    if (label) label.textContent = prefix + "Auto refresh in " + left + "s"
    timer = setInterval(function () {
      if (!content.isConnected) {
        clearInterval(timer)
        return
      }
      if (left > 0) left--
      if (left > 0) {
        if (label) label.textContent = prefix + "Auto refresh in " + left + "s"
        return
      }
      if (document.hidden) return
      clearInterval(timer)
      if (window.htmx) htmx.trigger(content, "mail-operations-refresh")
    }, 1000)
  }
  content.addEventListener("htmx:beforeRequest", function () {
    clearInterval(timer)
    if (label) label.textContent = "Refreshing..."
  })
  content.addEventListener("htmx:afterRequest", function (event) {
    if (event.detail && !event.detail.successful && content.isConnected) countDown("Couldn't refresh. ")
  })
  countDown("")
})

// /settings/accounts?folder_roles=<account> opens that account's folder roles,
// as a delete refused for want of a Trash folder offers.
goferOnContent("#edit-account-container", function () {
  var params = new URLSearchParams(window.location.search)
  var accountID = params.get("folder_roles")
  if (!accountID || !window.htmx) return
  params.delete("folder_roles")
  var query = params.toString()
  history.replaceState(history.state, "", window.location.pathname + (query ? "?" + query : "") + window.location.hash)
  htmx.ajax("GET", "/api/accounts/" + encodeURIComponent(accountID) + "/folder-roles/manage", { target: "#edit-account-container", swap: "innerHTML" })
})

// Switching the identities dialog between its list and its form resizes the
// dialog smoothly, as the calendar's dialog does, rather than jumping.
document.body.addEventListener("htmx:beforeSwap", function (e) {
  var target = e.detail && e.detail.target
  var xhr = e.detail && e.detail.xhr
  if (!xhr || !target || target.id !== "account-identities-panel" || !e.detail.shouldSwap) return
  var content = target.closest("[data-tui-dialog-content]")
  if (content) xhr.goferIdentitiesBounds = content.getBoundingClientRect()
})

document.body.addEventListener("htmx:afterSwap", function (e) {
  var xhr = e.detail && e.detail.xhr
  if (!xhr || !xhr.goferIdentitiesBounds || typeof flipDialogResize !== "function") return
  var from = xhr.goferIdentitiesBounds
  xhr.goferIdentitiesBounds = null
  var content = document.querySelector("#account-identities-dialog [data-tui-dialog-content]")
  if (!content) return
  // Measure the new size without templUI's transition-all in flight.
  var transition = content.style.transitionProperty
  content.style.transitionProperty = "none"
  var to = content.getBoundingClientRect()
  content.style.transitionProperty = transition
  flipDialogResize(content, from, to, function () {
    var field = content.querySelector("[data-account-identity-form] input:not([type=hidden]):not([readonly])")
    if (field) field.focus({ preventScroll: true })
  })
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
