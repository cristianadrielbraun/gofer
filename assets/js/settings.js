document.addEventListener("DOMContentLoaded", function () {
  setupTestButtonAnimation()
  setupSettingsHistory()
  setupSettingsSidebar()
  setupModePickers()
  setupAccountColorPickers()
  setupAccountSignaturesDialog(document)

  function setupTestButtonAnimation() {
    document.body.addEventListener("htmx:beforeRequest", function (e) {
      var btn = (e.detail && e.detail.elt) || e.target
      if (!btn || !btn.hasAttribute("data-test-btn")) return
      transitionTestButton(btn, "testing")
    })

    document.body.addEventListener("htmx:afterRequest", function (e) {
      var btn = (e.detail && e.detail.elt) || e.target
      if (!btn || !btn.hasAttribute("data-test-btn")) return

      var xhr = e.detail.xhr
      if (!xhr || xhr.status !== 200) {
        transitionTestButton(btn, "idle")
        return
      }

      try {
        var data = JSON.parse(xhr.responseText)
        var allSuccess = data.results.every(function (r) { return r.success })
        if (allSuccess) {
          transitionTestButton(btn, "success")
          setTimeout(function () { transitionTestButton(btn, "idle") }, 3000)
        } else {
          transitionTestButton(btn, "error", data.results)
        }
      } catch (err) {
        transitionTestButton(btn, "idle")
      }
    })
  }
})

function transitionTestButton(btn, state, results) {
  var content = btn.querySelector("[data-test-btn-content]")
  if (!content) return

  var currentWidth = btn.offsetWidth

  content.style.transition = "opacity 0.15s ease"
  content.style.opacity = "0"

  setTimeout(function () {
    btn.style.width = currentWidth + "px"
    btn.style.whiteSpace = "nowrap"

    btn.className = testBtnClasses(state)
    if (state === "error") btn.style.position = "relative"
    else btn.style.position = ""

    content.innerHTML = testBtnContent(state, btn.dataset.accountId, results)

    var targetWidth = btn.scrollWidth
    btn.style.width = currentWidth + "px"
    void btn.offsetWidth
    btn.style.transition = "width 0.3s ease"
    btn.style.width = targetWidth + "px"

    requestAnimationFrame(function () {
      requestAnimationFrame(function () {
        content.style.transition = "opacity 0.2s ease"
        content.style.opacity = "1"
      })
    })

    setTimeout(function () {
      btn.style.width = ""
      btn.style.whiteSpace = ""
      btn.style.transition = ""
      content.style.transition = ""
      content.style.opacity = ""
    }, 350)
  }, 160)
}

function testBtnClasses(state) {
  var base = "inline-flex items-center gap-1.5 text-xs transition-all duration-300 px-2.5 py-1.5 rounded-md border"
  switch (state) {
    case "idle":
      return base + " text-muted-foreground hover:text-foreground border-border hover:bg-accent cursor-pointer"
    case "testing":
      return base + " text-muted-foreground border-border cursor-wait"
    case "success":
      return base + " text-emerald-600 border-emerald-500/20 bg-emerald-500/10"
    case "error":
      return base + " text-destructive border-destructive/20 bg-destructive/10 hover:bg-destructive/15 cursor-pointer"
  }
  return base + " text-muted-foreground border-border"
}

function svg(cls, paths) {
  return '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="' + cls + '">' + paths + "</svg>"
}

var ICON_WIFI = '<path d="M12 20h.01"/><path d="M2 8.82a15 15 0 0 1 20 0"/><path d="M5 12.859a10 10 0 0 1 14 0"/><path d="M8.5 16.429a5 5 0 0 1 7 0"/>'
var ICON_CHECK = '<circle cx="12" cy="12" r="10"/><path d="m9 12 2 2 4-4"/>'
var ICON_X = '<circle cx="12" cy="12" r="10"/><path d="m15 9-6 6"/><path d="m9 9 6 6"/>'
var ICON_INFO = '<circle cx="12" cy="12" r="10"/><path d="M12 16v-4"/><path d="M12 8h.01"/>'

function testBtnContent(state, accountId, results) {
  switch (state) {
    case "idle":
      return svg("size-3.5", ICON_WIFI) + "Test"
    case "testing":
      return '<div class="size-3.5 shrink-0 border-2 border-muted-foreground/30 border-t-muted-foreground rounded-full animate-spin"></div>Testing...'
    case "success":
      return svg("size-3.5", ICON_CHECK) + "Connected"
    case "error":
      var html = svg("size-3.5", ICON_X) + "Failed"
      html += '<span class="inline-flex items-center justify-center size-4 rounded hover:bg-destructive/20 transition-colors ml-0.5 -mr-1" onclick="event.stopPropagation();document.getElementById(\'test-err-' + accountId + '\').classList.toggle(\'hidden\')">'
      html += svg("size-3", ICON_INFO)
      html += "</span>"
      html += '<div id="test-err-' + accountId + '" class="hidden absolute top-full right-0 mt-1 p-3 rounded-lg bg-popover border shadow-lg text-xs max-w-xs z-50 text-popover-foreground">'
      html += '<div class="space-y-2">'
      if (results) {
        for (var i = 0; i < results.length; i++) {
          var r = results[i]
          html += "<div>"
          html += '<p class="font-semibold text-muted-foreground uppercase tracking-wider">' + escapeHtml(r.service) + "</p>"
          if (r.success) {
            html += '<p class="text-emerald-600 mt-0.5">' + escapeHtml(r.message) + "</p>"
          } else {
            html += '<p class="text-destructive mt-0.5">' + escapeHtml(r.error) + "</p>"
            html += '<p class="text-muted-foreground mt-0.5">' + escapeHtml(r.message) + "</p>"
          }
          html += "</div>"
        }
      }
      html += "</div></div>"
      return html
  }
  return "Test"
}

function escapeHtml(text) {
  if (!text) return ""
  var el = document.createElement("span")
  el.textContent = text
  return el.innerHTML
}

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
      if (!/^(cid:|https?:|\/api\/attachments\/|\/api\/inline-content\/|\/compose\/attachments\/)/i.test(src)) {
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
      if (!window.confirm("Delete " + signatureName + "? Accounts using it will stop inserting it.")) return
      fetch("/api/signatures/" + encodeURIComponent(row.getAttribute("data-signature-id") || ""), { method: "DELETE" })
        .then(function (r) { if (!r.ok) throw new Error("Failed to delete signature") })
        .then(reloadDialog)
        .catch(function (err) {
          var status = form && form.querySelector("[data-signature-save-status]")
          if (status) status.textContent = err && err.message ? err.message : "Failed to delete signature"
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
    if (!window.confirm("Delete " + signatureName + "? Accounts using it will stop inserting it.")) return
    fetch("/api/signatures/" + encodeURIComponent(signatureID), { method: "DELETE" })
      .then(function (r) { if (!r.ok) throw new Error("Failed to delete signature") })
      .then(reloadDialog)
      .catch(function (err) {
        if (status) status.textContent = err && err.message ? err.message : "Failed to delete signature"
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
    return GoferSettings.get("theme") || "dark"
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

      buttons.forEach(function (btn) {
        var isActive = btn === activeButton
        btn.className = modeButtonClasses(isActive)
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

function setupSettingsHistory() {
  if (!window.location.pathname.startsWith("/settings")) return
  var parts = window.location.pathname.replace(/\/+$/, "").split("/")
  var tab = parts[2] || "accounts"
  if (tab !== "accounts" && tab !== "sync" && tab !== "contacts" && tab !== "appearance" && tab !== "compose-display" && tab !== "advanced") tab = "accounts"
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

function settingsTabFromLocation() {
  var parts = window.location.pathname.replace(/\/+$/, "").split("/")
  var tab = parts[2] || "accounts"
  if (tab !== "accounts" && tab !== "sync" && tab !== "contacts" && tab !== "appearance" && tab !== "compose-display" && tab !== "advanced") return "accounts"
  return tab
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
