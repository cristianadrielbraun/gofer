// Gofer's pages may only run scripts from its own files, so elements don't
// carry inline event handlers. Instead an element names what to do in a data
// attribute, as a JSON list: a function name and its arguments.
//
//   <button data-on-click='["wizardGo", 2]'>
//   <input data-on-change='["uploadComposeAttachments", "$files", "$this"]'>
//
// "$this" is the element, "$event" the event, "$files" the element's files
// and "$checked" whether it is checked. "$closest:<selector>" is the nearest
// ancestor matching the selector, and "$data:<name>" one of the element's
// data-* values. A function that returns false cancels the event, like
// "return f()" in an inline handler.
//
// htmx events work the same way: data-on-htmx-before-request runs on the
// element's htmx:before-request event. Only the functions listed below can be called, so
// injected markup can't use these attributes to reach anything else.
//
// <form data-confirm="Are you sure?"> asks before the form is submitted.
;(function () {
  var actionNames = [
    "addAccountTest.run",
    "applyLabelsDialog",
    "applyMailDiscoveryCandidate",
    "archiveThread",
    "calendarDescriptionExec",
    "calendarDescriptionSelection",
    "changeComposeOneTimeFrom",
    "checkComposeOneTimeFrom",
    "chooseContactProtocol",
    "chooseCalendarProtocol",
    "chooseMessagesToAccountMode",
    "closeComposeSchedulePopover",
    "closeMobileContactView",
    "closeMobileMailView",
    "collapseComposeFullWidth",
    "collapseToDialog",
    "composeChanged",
    "composeCreateLink",
    "composeExec",
    "configureCalendarCacheRequest",
    "configureCalendarSyncRequest",
    "confirmMessagesToAccount",
    "confirmMoveDialog",
    "continueEditingDraft",
    "copyCalendarMeetLink",
    "createDAVItem",
    "createLabelFromSearch",
    "createMoveLabel",
    "cycleLabelOption",
    "deleteMessage",
    "deleteThread",
    "discardComposeDialog",
    "discardComposePane",
    "discardReadPaneDraft",
    "discoverMailSettings",
    "editAccountSaveOrContinue",
    "editAccountTest.run",
    "editToggleSmtpAuth",
    "editWizardGo",
    "expandComposeFullWidth",
    "expandToPane",
    "filterLabelsDialog",
    "filterMoveFolders",
    "findDAVItems",
    "finalizeComposeRecipientInput",
    "focusComposeRecipientField",
    "goferClickElement",
    "goferNavigate",
    "goferPreventDefault",
    "goferShowComposeRecipientField",
    "goferSubmitHTMXForm",
    "goferSwitchDialog",
    "handleAccountTestResult",
    "handleAccountTestStart",
    "handleCalendarCreateResult",
    "handleCalendarCreateStart",
    "handleCalendarDiscoveryResult",
    "handleCalendarDiscoveryStart",
    "handleCalendarGuestKeydown",
    "handleCalendarSourceSaveResult",
    "handleCalendarSourceSaveStart",
    "handleCalendarSyncAbort",
    "handleCalendarSyncResult",
    "handleCalendarSyncStart",
    "handleComposePaste",
    "handleComposeRecipientInput",
    "handleComposeRecipientKeydown",
    "handleContactSidebarSyncResult",
    "handleContactSidebarSyncStart",
    "handleEditAccountSaveResult",
    "handleEditAccountSaveStart",
    "handleMailSidebarSyncResult",
    "handleMailSidebarSyncStart",
    "handleReply",
    "labelsDialogSearchKeydown",
    "manageFolderAction",
    "markNotSpam",
    "markSpam",
    "markThreadNotSpam",
    "markThreadSpam",
    "moveMessage",
    "openAddAccountWizard",
    "openAddMailChooser",
    "openDAVAccountDialog",
    "openMessageLabels",
    "openMessageSource",
    "openMoveDialog",
    "openNewCompose",
    "pasteCalendarDescription",
    "prepareCalendarDescriptionSubmit",
    "prepareComposeSchedule",
    "printEmail",
    "removeLabelMessage",
    "retryCalendarMeetPreview",
    "retryCalendarTeams",
    "retryCalendarTeamsPreview",
    "saveComposeDraft",
    "saveWizardEmailService",
    "scheduleCompose",
    "selectCalendarGuest",
    "selectComposeAccount",
    "selectComposeOneTimeFrom",
    "selectMoveFolder",
    "startAddMail",
    "startMoveFolderCreate",
    "sendCompose",
    "setActiveComposeEditor",
    "setProfileConflictChoices",
    "showComposeSignaturePicker",
    "syncAddContactStepAfterSwap",
    "syncCalendarDescription",
    "syncCalendarGuestDropdownState",
    "syncSignatureEditor",
    "toggleCalendarAuth",
    "toggleCalendarDescription",
    "toggleCalendarMeetRemoval",
    "toggleCalendarTeamsReplacement",
    "toggleCardDAVAuth",
    "toggleComposeAttachments",
    "toggleEmailBodySchemeById",
    "toggleIdentitySMTP",
    "toggleRead",
    "toggleSmtpAuth",
    "toggleStar",
    "toggleThreadRead",
    "toggleThreadStar",
    "toggleUnencryptedTransportWarning",
    "triggerComposeAttachmentUpload",
    "triggerComposeInlineImageUpload",
    "unarchiveThread",
    "updateCalendarGuestDropdown",
    "updateComposeToolbar",
    "uploadComposeAttachments",
    "uploadComposeInlineImages",
    "window.tui.popover.open",
    "wizardGo",
    "wizardServiceNext",
    "wolfEmptyPaneClick",
  ]
  var allowed = Object.create(null)
  actionNames.forEach(function (name) { allowed[name] = true })

  // Resolves an allowed name like "editAccountTest.run" to its function and
  // the object it belongs to.
  function resolve(name) {
    if (!allowed[name]) return null
    var owner = window
    var parts = name.split(".")
    if (parts[0] === "window") parts.shift()
    for (var i = 0; i < parts.length - 1; i++) {
      owner = owner && owner[parts[i]]
    }
    var fn = owner && owner[parts[parts.length - 1]]
    return typeof fn === "function" ? { fn: fn, owner: owner } : null
  }

  function run(element, event, spec) {
    var list
    try { list = JSON.parse(spec) } catch (err) { return }
    if (!Array.isArray(list) || typeof list[0] !== "string") return
    var target = resolve(list[0])
    if (!target) {
      if (window.console) console.warn("Gofer: unknown action", list[0])
      return
    }
    var args = list.slice(1).map(function (arg) {
      if (arg === "$this") return element
      if (arg === "$event") return event
      if (arg === "$files") return element.files
      if (arg === "$checked") return element.checked
      if (typeof arg === "string" && arg.indexOf("$closest:") === 0) return element.closest(arg.slice(9))
      if (typeof arg === "string" && arg.indexOf("$data:") === 0) return element.dataset[arg.slice(6)]
      return arg
    })
    if (target.fn.apply(target.owner, args) === false) {
      event.preventDefault()
      event.stopPropagation()
    }
  }

  // Each element gets its own listener, as an inline handler would have, so
  // actions run at the same moment and aren't stopped by other listeners.
  // Elements added later, by htmx or by scripts, are bound as they appear.
  var htmxEvents = ["before-request", "after-request", "send-abort", "config-request", "after-swap"]
  var eventTypes = ["click", "change", "input", "keydown", "keyup", "mousedown", "mouseup", "paste", "drop", "focus", "blur", "submit", "toggle"]
    .concat(htmxEvents.map(function (name) { return "htmx-" + name }))
  var attributes = eventTypes.map(function (type) { return "data-on-" + type })
  var selector = attributes.map(function (attribute) { return "[" + attribute + "]" }).join(",")

  function bindElement(element) {
    var bound = element._goferActions || (element._goferActions = {})
    eventTypes.forEach(function (type) {
      if (bound[type] || !element.hasAttribute("data-on-" + type)) return
      bound[type] = true
      var domEvent = type.indexOf("htmx-") === 0 ? "htmx:" + type.slice(5) : type
      element.addEventListener(domEvent, function (event) {
        var spec = element.getAttribute("data-on-" + type)
        if (spec) run(element, event, spec)
      })
    })
  }

  function bindWithin(root) {
    if (!root || root.nodeType !== 1) return
    if (root.matches(selector)) bindElement(root)
    root.querySelectorAll(selector).forEach(bindElement)
  }

  // goferOnContent(selector, setup) runs setup(element) once for each element
  // matching selector: those on the page when it loads, and those htmx or
  // scripts add later. It replaces the setup code inline scripts ran when
  // their part of the page arrived.
  var contentSetups = []
  window.goferOnContent = function (selector, setup) {
    var entry = { selector: selector, setup: setup, key: "_goferSetup" + contentSetups.length }
    contentSetups.push(entry)
    if (document.readyState !== "loading") setupContent(entry)
  }
  function setupContent(entry) {
    document.querySelectorAll(entry.selector).forEach(function (element) {
      if (element.closest("[data-navigation-pending]")) return
      if (element[entry.key]) return
      element[entry.key] = true
      entry.setup(element)
    })
  }
  var contentScanQueued = false
  function scanContent() {
    if (contentScanQueued || !contentSetups.length) return
    contentScanQueued = true
    Promise.resolve().then(function () {
      contentScanQueued = false
      contentSetups.forEach(setupContent)
    })
  }
  document.addEventListener("DOMContentLoaded", function () { contentSetups.forEach(setupContent) })

  bindWithin(document.documentElement)
  new MutationObserver(function (mutations) {
    var added = false
    mutations.forEach(function (mutation) {
      if (mutation.type === "attributes") {
        bindElement(mutation.target)
        return
      }
      if (mutation.addedNodes.length) added = true
      mutation.addedNodes.forEach(bindWithin)
    })
    if (added && document.readyState !== "loading") scanContent()
  }).observe(document.documentElement, { childList: true, subtree: true, attributes: true, attributeFilter: attributes })

  // The app asks questions in its own dialogs (AppDialogs in
  // app_dialogs.templ), never the browser's popups. goferConfirm resolves
  // true when confirmed; goferPrompt resolves the text entered, or null.
  // Both show options.message under the title when given.
  function openAppDialog(id, onClosed) {
    var root = document.getElementById(id)
    var content = root && root.querySelector("[data-tui-dialog-content]")
    if (!content || !window.tui || !window.tui.dialog) return false
    content.addEventListener("close", onClosed, { once: true })
    window.tui.dialog.open(id)
    return true
  }

  window.goferConfirm = function (options) {
    options = options || {}
    return new Promise(function (resolve) {
      var root = document.getElementById("app-confirm-dialog")
      if (!root) {
        resolve(false)
        return
      }
      var title = root.querySelector("[data-app-confirm-title]")
      var message = root.querySelector("[data-app-confirm-message]")
      var plain = root.querySelector("[data-app-confirm-accept]")
      var danger = root.querySelector("[data-app-confirm-accept-destructive]")
      var cancel = root.querySelector("[data-app-confirm-cancel]")
      var accept = options.destructive ? danger : plain
      title.textContent = options.title || "Are you sure?"
      message.textContent = options.message || ""
      message.hidden = !options.message
      // Buttons set their own display, which outranks a hidden class.
      plain.style.display = options.destructive ? "none" : ""
      danger.style.display = options.destructive ? "" : "none"
      accept.textContent = options.confirmLabel || (options.destructive ? "Delete" : "Continue")
      cancel.textContent = options.cancelLabel || "Cancel"
      var accepted = false
      accept.onclick = function () {
        accepted = true
        window.tui.dialog.close("app-confirm-dialog")
      }
      var opened = openAppDialog("app-confirm-dialog", function () {
        accept.onclick = null
        resolve(accepted)
      })
      if (!opened) resolve(false)
      else setTimeout(function () { accept.focus() }, 0)
    })
  }

  window.goferPrompt = function (options) {
    options = options || {}
    return new Promise(function (resolve) {
      var root = document.getElementById("app-prompt-dialog")
      if (!root) {
        resolve(null)
        return
      }
      var form = root.querySelector("[data-app-prompt-form]")
      var input = root.querySelector("[data-app-prompt-input]")
      root.querySelector("[data-app-prompt-title]").textContent = options.title || ""
      var message = root.querySelector("[data-app-prompt-message]")
      message.textContent = options.message || ""
      message.hidden = !options.message
      root.querySelector("[data-app-prompt-label]").textContent = options.label || ""
      root.querySelector("[data-app-prompt-accept]").textContent = options.confirmLabel || "OK"
      input.value = options.value || ""
      input.placeholder = options.placeholder || ""
      var value = null
      form.onsubmit = function (event) {
        event.preventDefault()
        value = input.value
        window.tui.dialog.close("app-prompt-dialog")
      }
      var opened = openAppDialog("app-prompt-dialog", function () {
        form.onsubmit = null
        resolve(value)
      })
      if (!opened) resolve(null)
      else setTimeout(function () { input.focus(); input.select() }, 0)
    })
  }

  // The mailbox size dialog (mailbox_size.templ) shows an account's size on
  // the mail server, marking folderID when given. Details are kept once
  // loaded: settings cards bring them with their summary and the sidebar's
  // menu loads them while it's open, so the dialog opens filled. Opening
  // still checks for newer sizes and swaps them in only if they differ.
  var mailboxSizeRequest = 0
  var mailboxSizeKept = {}
  var mailboxSizeLoads = {}
  var mailboxSizeFreshMs = 10000

  // Serialized as the browser would, so copies compare equal.
  function mailboxSizeHTML(html) {
    var template = document.createElement("template")
    template.innerHTML = html
    return template.innerHTML
  }

  function keptMailboxSize(accountID) {
    if (mailboxSizeKept[accountID]) return mailboxSizeKept[accountID]
    var template = document.querySelector('template[data-mailbox-size-prefetched="' + CSS.escape(accountID) + '"]')
    return template ? { html: template.innerHTML, at: 0 } : null
  }

  function loadMailboxSize(accountID) {
    if (mailboxSizeLoads[accountID]) return mailboxSizeLoads[accountID]
    var load = fetch("/api/accounts/" + encodeURIComponent(accountID) + "/mailbox-size", { headers: { Accept: "text/html" } })
      .then(function (response) {
        if (!response.ok) throw new Error("mailbox size: " + response.status)
        return response.text()
      })
      .then(function (html) {
        var kept = { html: mailboxSizeHTML(html), at: Date.now() }
        mailboxSizeKept[accountID] = kept
        return kept
      })
    mailboxSizeLoads[accountID] = load
    var forget = function () { delete mailboxSizeLoads[accountID] }
    load.then(forget, forget)
    return load
  }

  window.goferPrefetchMailboxSize = function (accountID) {
    if (accountID) loadMailboxSize(accountID).catch(function () {})
  }

  function showMailboxSize(root, html, folderID) {
    var body = root.querySelector("[data-mailbox-size-body]")
    var list = body.querySelector("ul")
    var scrollTop = list ? list.scrollTop : 0
    body.innerHTML = html
    var details = body.querySelector("[data-mailbox-size-details]")
    root.querySelector("[data-mailbox-size-account-name]").textContent = details ? details.getAttribute("data-account-name") : ""
    var marked = folderID && body.querySelector('[data-mailbox-size-folder="' + CSS.escape(folderID) + '"]')
    if (marked) {
      marked.setAttribute("data-mailbox-size-marked", "")
      marked.scrollIntoView({ block: "nearest" })
    } else if ((list = body.querySelector("ul"))) {
      list.scrollTop = scrollTop
    }
  }

  window.goferOpenMailboxSize = function (accountID, folderID) {
    var root = document.getElementById("mailbox-size-dialog")
    if (!root || !accountID || !window.tui || !window.tui.dialog) return
    var body = root.querySelector("[data-mailbox-size-body]")
    body.style.transition = ""
    body.style.height = ""
    body.style.overflow = ""
    var kept = keptMailboxSize(accountID)
    var shown = kept ? kept.html : ""
    if (kept) {
      body.innerHTML = ""
      showMailboxSize(root, shown, folderID)
    } else {
      body.innerHTML = root.querySelector("[data-mailbox-size-loading]").innerHTML
      // Keeps the name's line while it loads.
      root.querySelector("[data-mailbox-size-account-name]").textContent = "\u00a0"
    }
    window.tui.dialog.open("mailbox-size-dialog")
    // Sizes loaded a moment ago, as the menu does, are current enough.
    if (kept && Date.now() - kept.at < mailboxSizeFreshMs) return
    var request = ++mailboxSizeRequest
    loadMailboxSize(accountID)
      .then(function (fresh) {
        if (request !== mailboxSizeRequest || fresh.html === shown) return
        resizeMailboxSizeBody(body, function () { showMailboxSize(root, fresh.html, folderID) })
      })
      .catch(function (err) {
        if (request !== mailboxSizeRequest) return
        console.warn(err)
        // Sizes already shown stay; they're only a little out of date.
        if (shown) return
        resizeMailboxSizeBody(body, function () { body.innerHTML = root.querySelector("[data-mailbox-size-error]").innerHTML })
      })
  }
  // Swaps the dialog's content, easing its height from the old to the new.
  function resizeMailboxSizeBody(body, swap) {
    var from = body.offsetHeight
    body.style.transition = ""
    body.style.height = ""
    swap()
    var to = body.offsetHeight
    if (from === to || window.matchMedia("(prefers-reduced-motion: reduce)").matches) return
    body.style.overflow = "hidden"
    body.style.height = from + "px"
    void body.offsetHeight
    body.style.transition = "height 200ms ease-out"
    body.style.height = to + "px"
    body.addEventListener("transitionend", function done(event) {
      if (event.target !== body) return
      body.removeEventListener("transitionend", done)
      body.style.transition = ""
      body.style.height = ""
      body.style.overflow = ""
    })
  }

  // A settings card's size line keeps its words while loading; the numbers
  // ease from the skeleton's width to their own, so the words slide along.
  var mailboxSizeSkeletons = {}
  document.addEventListener("htmx:beforeSwap", function (event) {
    var line = event.detail.target
    if (!line.matches || !line.matches("[data-account-mailbox-size][data-mailbox-size-loading]")) return
    var widths = {}
    line.querySelectorAll("[data-mailbox-size-number]").forEach(function (number) {
      widths[number.getAttribute("data-mailbox-size-number")] = number.getBoundingClientRect().width
    })
    mailboxSizeSkeletons[line.getAttribute("data-account-mailbox-size")] = widths
  })
  document.addEventListener("htmx:afterSwap", function () {
    Object.keys(mailboxSizeSkeletons).forEach(function (accountID) {
      var line = document.querySelector('[data-account-mailbox-size="' + CSS.escape(accountID) + '"]:not([data-mailbox-size-loading])')
      if (!line) return
      var widths = mailboxSizeSkeletons[accountID]
      delete mailboxSizeSkeletons[accountID]
      if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) return
      line.querySelectorAll("[data-mailbox-size-number]").forEach(function (number) {
        var from = widths[number.getAttribute("data-mailbox-size-number")]
        var to = number.getBoundingClientRect().width
        // Hidden on narrow screens.
        if (!from || !to) return
        number.style.clipPath = "inset(0 -1px)"
        number.style.width = from + "px"
        number.style.opacity = "0"
        void number.offsetWidth
        number.style.transition = "width 250ms ease-out, opacity 250ms ease-out"
        number.style.width = to + "px"
        number.style.opacity = ""
        number.addEventListener("transitionend", function done(event) {
          // Opacity always changes; the width may not.
          if (event.propertyName !== "opacity") return
          number.removeEventListener("transitionend", done)
          number.style.transition = ""
          number.style.width = ""
          number.style.clipPath = ""
        })
      })
    })
  })

  document.addEventListener("click", function (event) {
    var control = event.target.closest && event.target.closest("[data-mailbox-size-account]")
    if (!control) return
    event.preventDefault()
    window.goferOpenMailboxSize(control.getAttribute("data-mailbox-size-account"), "")
  })

  // A form with data-confirm asks first. The question's first sentence is the
  // title; data-confirm-action names the button, and data-confirm-destructive
  // makes it red.
  document.addEventListener("submit", function (event) {
    var form = event.target
    if (!form || !form.getAttribute) return
    var question = form.getAttribute("data-confirm")
    if (!question) return
    if (form._goferConfirmed) {
      form._goferConfirmed = false
      return
    }
    event.preventDefault()
    event.stopImmediatePropagation()
    var submitter = event.submitter && event.submitter.form === form ? event.submitter : null
    var split = question.indexOf("? ")
    window.goferConfirm({
      title: split > 0 ? question.slice(0, split + 1) : question,
      message: split > 0 ? question.slice(split + 2) : "",
      confirmLabel: form.getAttribute("data-confirm-action") || "Continue",
      destructive: form.hasAttribute("data-confirm-destructive"),
    }).then(function (confirmed) {
      if (!confirmed) return
      form._goferConfirmed = true
      if (submitter) form.requestSubmit(submitter)
      else form.requestSubmit()
    })
  }, true)

  // A poll marked data-poll-when-visible is skipped while the page is hidden
  // or the element isn't shown, which htmx can't check without running inline
  // code.
  document.addEventListener("htmx:confirm", function (event) {
    var element = event.target
    if (!element || !element.hasAttribute || !element.hasAttribute("data-poll-when-visible")) return
    if (document.visibilityState !== "visible" || element.getClientRects().length === 0) event.preventDefault()
  })

  // Small steps that used to be written inline.
  window.goferNavigate = function (path) {
    if (typeof path === "string" && path.charAt(0) === "/" && path.charAt(1) !== "/") window.location.href = path
  }
  window.goferClickElement = function (selector) {
    var element = document.querySelector(selector)
    if (element) element.click()
  }
  window.goferSwitchDialog = function (from, to) {
    if (window.tui && window.tui.dialog) window.tui.dialog.close(from)
    setTimeout(function () {
      if (window.tui && window.tui.dialog) window.tui.dialog.open(to)
    }, 150)
  }
  window.goferPreventDefault = function (event) {
    event.preventDefault()
  }
  // Submits a setting as soon as it changes, on forms htmx saves.
  window.goferSubmitHTMXForm = function (input) {
    if (input && input.form && input.form.hasAttribute("hx-post")) input.form.requestSubmit()
  }
  window.goferShowComposeRecipientField = function (fieldID, buttonID) {
    var field = document.getElementById(fieldID)
    var button = document.getElementById(buttonID)
    if (field) field.classList.remove("hidden")
    if (button) button.classList.add("hidden")
  }
})()

// Folder appearance uses the same dialog from both the sidebar and Settings.
;(function () {
  var request = 0
  var saving = false

  function applyFolderSymbol(icon, symbol) {
    var previous = icon.querySelector("[data-folder-custom-icon]")
    if (previous) previous.remove()
    icon.toggleAttribute("data-folder-customized", !!symbol)
    if (symbol) {
      var custom = document.createElement("span")
      custom.className = "inline-flex"
      custom.setAttribute("data-folder-custom-icon", "")
      custom.appendChild(symbol.cloneNode(true))
      icon.appendChild(custom)
    }
  }

  function selectedFolderSymbol(choice) {
    if (!choice || !choice.value) return null
    var label = choice.closest("label")
    return (label.querySelector("[data-folder-icon-symbol]") || label.querySelector("svg")).cloneNode(true)
  }

  function preview(form) {
    var icon = form.querySelector("[data-folder-appearance-icon]")
    if (!icon) return
    var color = form.querySelector('input[name="color"]:checked')
    icon.style.color = color ? color.value : ""
    icon.toggleAttribute("data-folder-color", !!(color && color.value))
    var choice = form.querySelector('input[name="icon"]:checked')
    if (choice) applyFolderSymbol(icon, selectedFolderSymbol(choice))
    var style = form.querySelector('input[name="style"]:checked')
    if (style) {
      icon.setAttribute("data-folder-icon-style", style.value)
      var options = form.querySelector("[data-folder-icon-options]")
      if (options) options.setAttribute("data-folder-icon-style", style.value)
    }
  }

  function appearanceURL(id, virtual, tag, unifiedTag) {
    var prefix = tag ? (unifiedTag ? "/api/unified-tags/" : "/api/tags/") : (virtual ? "/api/virtual-folders/" : "/api/folders/")
    return prefix + encodeURIComponent(id) + "/appearance"
  }

  window.goferOpenFolderAppearance = function (target) {
    if (document.documentElement.getAttribute("data-theme") === "express") return
    var root = document.getElementById("folder-appearance-dialog")
    if (!root || !target.folderID || saving || !window.tui || !window.tui.dialog) return
    var token = ++request
    var body = root.querySelector("[data-folder-appearance-body]")
    var title = root.querySelector("[data-folder-appearance-title]")
    var description = root.querySelector("[data-folder-appearance-description]")
    if (title) title.textContent = target.tag ? "Tag appearance" : (target.virtual ? "View appearance" : "Folder appearance")
    if (description) description.textContent = target.tag ? "Customize this tag’s appearance in Gofer." : (target.virtual ? "Customize this view’s appearance in Gofer." : "Customize this folder’s appearance in Gofer.")
    body.textContent = "Loading appearance…"
    window.tui.dialog.open("folder-appearance-dialog")
    fetch(appearanceURL(target.folderID, target.virtual, target.tag, target.unifiedTag), { headers: { Accept: "text/html" } })
      .then(function (response) {
        if (!response.ok) throw new Error(response.status === 404 ? "This item is no longer available." : "Couldn't load appearance. Try again.")
        return response.text()
      })
      .then(function (html) { if (token === request) body.innerHTML = html })
      .catch(function (err) { if (token === request) body.textContent = err.message })
  }

  document.addEventListener("change", function (event) {
    var form = event.target.closest("[data-folder-appearance-form]")
    if (form && (event.target.name === "color" || event.target.name === "icon" || event.target.name === "style")) preview(form)
  })

  document.addEventListener("click", function (event) {
    var reset = event.target.closest("[data-folder-appearance-reset]")
    if (!reset) return
    var form = reset.closest("form")
    form.querySelector('input[name="color"][value=""]').checked = true
    var defaultIcon = form.querySelector('input[name="icon"][value=""]')
    if (defaultIcon) defaultIcon.checked = true
    var defaultStyle = form.querySelector('input[name="style"][value="outlined"]')
    if (defaultStyle) defaultStyle.checked = true
    preview(form)
  })

  document.addEventListener("submit", function (event) {
    var form = event.target.closest("[data-folder-appearance-form]")
    if (!form) return
    event.preventDefault()
    if (saving) return
    var chosen = form.querySelector('input[name="color"]:checked')
    if (!chosen) return
    var error = form.querySelector("[data-folder-appearance-error]")
    var save = form.querySelector("[data-folder-appearance-save]")
    var color = chosen.value
    var chosenIcon = form.querySelector('input[name="icon"]:checked')
    var symbol = selectedFolderSymbol(chosenIcon)
    var chosenStyle = form.querySelector('input[name="style"]:checked')
    var style = chosenStyle ? chosenStyle.value : null
    var token = request
    error.hidden = true
    saving = true
    save.disabled = true
    save.textContent = "Saving…"
    var fields = new URLSearchParams({ color: color })
    if (chosenIcon) fields.set("icon", chosenIcon.value)
    if (style) fields.set("style", style)
    fetch(appearanceURL(form.dataset.folderId, form.hasAttribute("data-folder-appearance-virtual"), form.hasAttribute("data-tag-appearance"), form.hasAttribute("data-tag-appearance-unified")), {
      method: "POST", headers: { "Content-Type": "application/x-www-form-urlencoded", Accept: "application/json" }, body: fields.toString(),
    }).then(function (response) {
      if (!response.ok) throw new Error(response.status === 404 ? "This item is no longer available." : "Couldn't save appearance. Try again.")
      return response.json()
    }).then(function (data) {
      document.querySelectorAll("[data-folder-appearance-icon]").forEach(function (icon) {
        if (icon.getAttribute("data-folder-appearance-icon") !== data.id || icon.hasAttribute("data-folder-appearance-virtual") !== !!data.virtual) return
        if (icon.hasAttribute("data-tag-appearance") !== !!data.tag || icon.hasAttribute("data-tag-appearance-unified") !== !!data.unified_tag) return
        icon.style.color = data.color
        icon.toggleAttribute("data-folder-color", !!data.color)
        if (chosenIcon) applyFolderSymbol(icon, symbol)
        if (data.style) icon.setAttribute("data-folder-icon-style", data.style)
      })
      if (token === request) window.tui.dialog.close("folder-appearance-dialog")
      if (!data.virtual && !data.tag) {
        if (window.goferRefreshSidebarAccount) window.goferRefreshSidebarAccount(data.account_id)
        document.dispatchEvent(new CustomEvent("gofer:folders-changed", { detail: { accountID: data.account_id } }))
      }
    }).catch(function (err) {
      error.textContent = err.message
      error.hidden = false
    }).finally(function () {
      saving = false
      save.disabled = false
      save.textContent = "Save"
    })
  })
})()
