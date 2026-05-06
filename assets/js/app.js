document.addEventListener("DOMContentLoaded", function () {
  var virtualMailList = null

  initVirtualScroll()
  setupFolderClickInterception()
  setupEmailSelectionTracking()
  setupSSE()
  setupMailListActions()

  function setupMailListActions() {
    document.addEventListener("click", function (e) {
      var starBtn = e.target.closest(".star-btn")
      if (starBtn) {
        e.preventDefault()
        e.stopPropagation()
        var emailId = starBtn.dataset.emailId
        if (emailId) toggleStar(emailId)
      }
    })
  }

  function setupSSE() {
    if (window.location.pathname.startsWith("/settings")) return

    var source = new EventSource("/api/events")

    source.addEventListener("new-mail", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { return }
      if (!data || !data.folder_id) return

      refreshSidebarUnread()
      if (virtualMailList && virtualMailList.folderID === data.folder_id) {
        virtualMailList.onNewEmail()
      }
    })

    source.addEventListener("send-result", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { return }
      if (!data) return

      if (data.status === "sent") {
        showSendStatus("sent", "Message sent")
      } else if (data.status === "ambiguous") {
        showSendStatus("ambiguous", data.error || "Send status unknown")
      } else {
        showSendStatus("failed", data.error || "Failed to send")
      }
    })

    source.addEventListener("mutation", function (e) {
      refreshSidebarUnread()
    })

    source.onerror = function () {
      source.close()
      setTimeout(setupSSE, 5000)
    }
  }

  function refreshSidebarUnread() {
    fetch("/api/folders/unread").then(function (r) { return r.json() }).then(function (counts) {
      var badges = document.querySelectorAll("[data-folder-unread]")
      for (var i = 0; i < badges.length; i++) {
        var badge = badges[i]
        var id = badge.dataset.folderUnread
        if (counts[id] !== undefined) {
          var n = counts[id]
          badge.textContent = String(n)
          badge.style.display = n > 0 ? "" : "none"
        }
      }
      for (var id in counts) {
        if (counts[id] > 0) {
          var existing = document.querySelector('[data-folder-unread="' + id + '"]')
          if (!existing) {
            var link = document.querySelector('aside a[hx-get="/folder/' + id + '"]')
            if (link) {
              var span = link.querySelector("span.truncate")
              if (span) {
                var badge = document.createElement("span")
                badge.dataset.folderUnread = id
                badge.className = "min-w-5 h-5 px-1.5 flex items-center justify-center rounded-full text-[11px] font-semibold tabular-nums bg-sidebar-accent text-sidebar-foreground/80"
                badge.textContent = String(counts[id])
                link.insertBefore(badge, link.firstChild.nextSibling ? link.firstChild.nextSibling : null)
              }
            }
          }
        }
      }
    }).catch(function () {})
  }

  function initVirtualScroll() {
    var container = document.getElementById("mail-list-scroll")
    if (!container) return

    var folderID = container.dataset.folderId || "inbox"
    virtualMailList = new VirtualMailList(container, { folderID: folderID })
    virtualMailList.hydrateFromDOM()
    container._virtualMailList = virtualMailList

    var selectedId = virtualMailList.selectedEmailId
    var path = "/folder/" + folderID
    if (selectedId) path += "/" + selectedId
    history.replaceState({ folder: folderID, email: selectedId || null }, "", path)
  }

  function setupFolderClickInterception() {
    var sidebar = document.querySelector("aside")
    if (!sidebar) return

    sidebar.addEventListener("click", function (e) {
      var link = e.target.closest('a[hx-get^="/folder/"]')
      if (!link) return

      e.preventDefault()
      e.stopPropagation()
      e.stopImmediatePropagation()

      var folderID = link.getAttribute("hx-get").replace("/folder/", "")

      if (document.querySelector("[data-compose-pane]")) {
        var mailView = document.getElementById("mail-view")
        collapseComposeFullWidth()
        if (mailView) mailView.innerHTML = ""
        _updateComposeBtn(false)
      }

      var sidebarLinks = sidebar.querySelectorAll("a[hx-get^='/folder/']")
      for (var i = 0; i < sidebarLinks.length; i++) {
        sidebarLinks[i].classList.remove(
          "bg-sidebar-accent",
          "text-sidebar-primary",
          "font-medium"
        )
        sidebarLinks[i].classList.add("text-sidebar-foreground")
      }
      link.classList.add(
        "bg-sidebar-accent",
        "text-sidebar-primary",
        "font-medium"
      )
      link.classList.remove("text-sidebar-foreground")

      var mainContent = document.getElementById("main-content")
      var isOnSettings = mainContent && mainContent.querySelector("[data-tui-tabs]")
      if (isOnSettings || !virtualMailList) {
        if (typeof htmx !== "undefined") {
          htmx.ajax("GET", "/folder/" + folderID + "/full", {target: "#main-content", swap: "outerHTML"})
        }
      } else {
        virtualMailList.switchFolder(folderID)
      }
    }, true)
  }

  function setupEmailSelectionTracking() {
    document.body.addEventListener("htmx:beforeRequest", function (evt) {
      if (
        evt.detail.pathInfo &&
        evt.detail.pathInfo.requestPath &&
        evt.detail.pathInfo.requestPath.match(/^\/email\/[^/]+$/)
      ) {
        showMailViewLoading()
      }
    })

    document.body.addEventListener("htmx:afterRequest", function (evt) {
      if (!virtualMailList) return

      if (
        evt.detail.pathInfo &&
        evt.detail.pathInfo.requestPath &&
        evt.detail.pathInfo.requestPath.startsWith("/email/")
      ) {
        var emailId = evt.detail.pathInfo.requestPath.replace("/email/", "")
        virtualMailList.onEmailSelected(emailId)
      }
    })
  }

  function showMailViewLoading() {
    var mailView = document.getElementById("mail-view")
    if (!mailView) return
    mailView.innerHTML =
      '<div class="flex flex-col h-full p-2">' +
        '<div class="surface-paper rounded-md flex flex-col h-full overflow-hidden animate-mail-loading">' +
          '<div class="flex items-center justify-between px-6 py-2.5">' +
            '<div class="flex items-center gap-1">' +
              '<div class="size-8 rounded-md bg-ink/5 animate-pulse"></div>' +
              '<div class="size-8 rounded-md bg-ink/5 animate-pulse"></div>' +
              '<div class="size-8 rounded-md bg-ink/5 animate-pulse"></div>' +
              '<div class="size-8 rounded-md bg-ink/5 animate-pulse"></div>' +
            '</div>' +
            '<div class="flex items-center gap-2">' +
              '<div class="h-5 w-14 rounded bg-ink/5 animate-pulse"></div>' +
              '<div class="size-8 rounded-md bg-ink/5 animate-pulse"></div>' +
            '</div>' +
          '</div>' +
          '<div class="h-px bg-gradient-to-r from-transparent via-amber-900/10 to-transparent"></div>' +
          '<div class="flex-1 overflow-y-auto">' +
            '<div class="max-w-3xl mx-auto px-8 py-6">' +
              '<div class="flex items-start gap-4">' +
                '<div class="size-11 rounded-full bg-ink/8 animate-pulse shrink-0"></div>' +
                '<div class="flex-1 space-y-2">' +
                  '<div class="flex items-center gap-2">' +
                    '<div class="h-4 w-32 rounded bg-ink/6 animate-pulse"></div>' +
                    '<div class="h-3 w-40 rounded bg-ink/4 animate-pulse"></div>' +
                  '</div>' +
                  '<div class="h-3 w-24 rounded bg-ink/4 animate-pulse"></div>' +
                '</div>' +
              '</div>' +
              '<div class="h-px bg-gradient-to-r from-transparent via-ink/10 to-transparent my-6"></div>' +
              '<div class="space-y-3">' +
                '<div class="h-4 w-full rounded bg-ink/5 animate-pulse"></div>' +
                '<div class="h-4 w-5/6 rounded bg-ink/5 animate-pulse"></div>' +
                '<div class="h-4 w-4/5 rounded bg-ink/5 animate-pulse"></div>' +
                '<div class="h-4 w-3/5 rounded bg-ink/5 animate-pulse"></div>' +
                '<div class="h-4 w-full rounded bg-ink/5 animate-pulse"></div>' +
                '<div class="h-4 w-2/3 rounded bg-ink/5 animate-pulse"></div>' +
              '</div>' +
            '</div>' +
          '</div>' +
          '<div class="px-6 py-3 border-t border-ink/6">' +
            '<div class="flex items-center gap-2">' +
              '<div class="flex-1 h-9 rounded-md border border-ink/8 bg-ink/[0.02] animate-pulse"></div>' +
              '<div class="flex-1 h-9 rounded-md border border-ink/8 bg-ink/[0.02] animate-pulse"></div>' +
              '<div class="flex-1 h-9 rounded-md border border-ink/8 bg-ink/[0.02] animate-pulse"></div>' +
            '</div>' +
          '</div>' +
        '</div>' +
      '</div>'
  }

  document.body.addEventListener("htmx:afterSettle", function (evt) {
    var scroll = document.getElementById("mail-list-scroll")
    if (!scroll || scroll._virtualMailList) return
    if (!evt.target || !evt.target.querySelector) return
    if (!evt.target.querySelector("#mail-list-scroll")) return

    var folderID = scroll.dataset.folderId || "inbox"
    virtualMailList = new VirtualMailList(scroll, { folderID: folderID })
    virtualMailList.hydrateFromDOM()
    scroll._virtualMailList = virtualMailList

    var selectedId = virtualMailList.selectedEmailId
    var path = "/folder/" + folderID
    if (selectedId) path += "/" + selectedId
    history.replaceState({ folder: folderID, email: selectedId || null }, "", path)
    if (typeof initResizeHandles === "function") initResizeHandles()
  })

  document.body.addEventListener("htmx:afterSettle", function () {
    if (typeof initResizeHandles === "function") initResizeHandles()
  })
})

var sendStatusTimer = null

function showSendStatus(status, text) {
  var wrapper = document.getElementById("send-status-wrapper")
  var inner = document.getElementById("send-status-inner")
  var iconEl = document.getElementById("send-status-icon")
  var textEl = document.getElementById("send-status-text")
  if (!wrapper || !inner || !iconEl || !textEl) return

  if (sendStatusTimer) {
    clearTimeout(sendStatusTimer)
    sendStatusTimer = null
  }

  textEl.textContent = text

  if (status === "sending") {
    inner.className = "flex items-center gap-2.5 px-3 py-3 rounded-lg text-[13px] font-medium transition-colors duration-500 ease-in-out bg-amber-900/40 text-amber-200 border border-amber-700/40"
    iconEl.innerHTML = '<svg class="size-4 animate-spin" viewBox="0 0 24 24" fill="none"><circle cx="12" cy="12" r="10" stroke="currentColor" stroke-width="3" stroke-linecap="round" opacity="0.3"/><path d="M12 2a10 10 0 0 1 10 10" stroke="currentColor" stroke-width="3" stroke-linecap="round"/></svg>'
    wrapper.style.maxHeight = "60px"
    wrapper.style.opacity = "1"
  } else if (status === "sent") {
    inner.className = "flex items-center gap-2.5 px-3 py-3 rounded-lg text-[13px] font-medium transition-colors duration-500 ease-in-out bg-emerald-900/40 text-emerald-200 border border-emerald-700/40"
    iconEl.innerHTML = '<svg class="size-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"/></svg>'
    wrapper.style.maxHeight = "60px"
    wrapper.style.opacity = "1"
    sendStatusTimer = setTimeout(function () {
      wrapper.style.maxHeight = "0"
      wrapper.style.opacity = "0"
    }, 5000)
  } else if (status === "ambiguous") {
    inner.className = "flex items-center gap-2.5 px-3 py-3 rounded-lg text-[13px] font-medium transition-colors duration-500 ease-in-out bg-amber-900/40 text-amber-200 border border-amber-700/40"
    iconEl.innerHTML = '<svg class="size-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><line x1="12" y1="8" x2="12" y2="12"/><line x1="12" y1="16" x2="12.01" y2="16"/></svg>'
    wrapper.style.maxHeight = "60px"
    wrapper.style.opacity = "1"
    sendStatusTimer = setTimeout(function () {
      wrapper.style.maxHeight = "0"
      wrapper.style.opacity = "0"
    }, 8000)
  } else {
    inner.className = "flex items-center gap-2.5 px-3 py-3 rounded-lg text-[13px] font-medium transition-colors duration-500 ease-in-out bg-red-900/40 text-red-200 border border-red-700/40"
    iconEl.innerHTML = '<svg class="size-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><line x1="15" y1="9" x2="9" y2="15"/><line x1="9" y1="9" x2="15" y2="15"/></svg>'
    wrapper.style.maxHeight = "60px"
    wrapper.style.opacity = "1"
    sendStatusTimer = setTimeout(function () {
      wrapper.style.maxHeight = "0"
      wrapper.style.opacity = "0"
    }, 8000)
  }
}

var _composeActive = false

function _updateComposeBtn(disabled) {
  if (!disabled) _composeActive = false
  var btn = document.getElementById("sidebar-compose-btn")
  if (!btn) return
  btn.disabled = disabled
  if (disabled) {
    btn.classList.add("opacity-40", "pointer-events-none")
  } else {
    btn.classList.remove("opacity-40", "pointer-events-none")
  }
}

function selectComposeAccount(el, fromPane) {
  var accountId = el.dataset.accountId
  var email = el.dataset.accountEmail
  var name = el.dataset.accountName
  if (!accountId || !email) return
  var prefix = fromPane ? "compose-pane-" : "compose-"
  var idField = document.getElementById(prefix + "account-id")
  var display = document.getElementById(prefix + "from-display")
  if (idField) idField.value = accountId
  if (display) display.innerHTML = (name ? name + " &lt;" : "") + email + (name ? "&gt;" : "")
}

function sendCompose(fromPane) {
  var formId = fromPane ? "compose-pane-form" : "compose-form"
  var form = document.getElementById(formId)
  if (!form) return

  var toField = form.querySelector('input[name="to"]')
  if (!toField || !toField.value.trim()) {
    return
  }

  var params = new URLSearchParams()
  var inputs = form.querySelectorAll("input, textarea")
  for (var i = 0; inputs && i < inputs.length; i++) {
    if (inputs[i].name) params.append(inputs[i].name, inputs[i].value)
  }

  if (fromPane) {
    var mailView = document.getElementById("mail-view")
    if (mailView) mailView.innerHTML = ""
    _updateComposeBtn(false)
  } else if (window.tui && window.tui.dialog) {
    window.tui.dialog.close("compose-dialog")
    _updateComposeBtn(false)
  }

  showSendStatus("sending", "Sending...")

  fetch("/compose", {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: params.toString()
  }).catch(function () {
    showSendStatus("failed", "Failed to connect to server")
  })
}

function handleReply(mode) {
  var bar = document.getElementById("reply-bar")
  if (!bar) return

  var messageId = bar.dataset.messageId
  var subject = bar.dataset.subject || ""
  var fromEmail = bar.dataset.fromEmail || ""
  var fromName = bar.dataset.fromName || ""
  var accountId = bar.dataset.accountId || ""

  var inPane = !!document.querySelector("[data-compose-pane]")
  var formId = inPane ? "compose-pane-form" : "compose-form"
  var form = document.getElementById(formId)
  if (!form) return

  var toField = form.querySelector('input[name="to"]')
  var subjectField = form.querySelector('input[name="subject"]')
  var prefix = inPane ? "compose-pane-" : "compose-"
  var accountIdField = document.getElementById(prefix + "account-id")
  var inReplyToField = document.getElementById(prefix + "in-reply-to")
  var bodyField = form.querySelector('textarea[name="body"]')

  if (accountId && accountIdField) {
    accountIdField.value = accountId
  }

  if (mode === "reply" || mode === "reply-all") {
    if (toField) toField.value = fromName ? fromName + " <" + fromEmail + ">" : fromEmail
    if (subjectField) {
      subjectField.value = subject.match(/^Re:/i) ? subject : "Re: " + subject
    }
    if (inReplyToField && messageId) {
      inReplyToField.value = "<" + messageId + ">"
    }
    if (bodyField) {
      bodyField.value = ""
      bodyField.focus()
    }
  } else if (mode === "forward") {
    if (toField) toField.value = ""
    if (subjectField) {
      subjectField.value = subject.match(/^Fwd:/i) ? subject : "Fwd: " + subject
    }
    if (inReplyToField) inReplyToField.value = ""
  }

  if (inPane) return

  var dialog = document.querySelector('#compose-dialog dialog[data-tui-dialog-content]')
  if (dialog && window.tui && window.tui.dialog) {
    window.tui.dialog.open('compose-dialog')
  }

  setTimeout(function () {
    if (mode === "forward" && toField) toField.focus()
  }, 100)
}

function toggleRead(emailId) {
  fetch("/api/messages/" + emailId + "/read", { method: "POST" })
    .then(function (r) { return r.json() })
    .then(function (data) {
      var btn = document.querySelector('[data-read-email="' + emailId + '"]')
      if (btn) {
        var svg = btn.querySelector('svg')
        if (svg) {
          if (data.is_read) {
            svg.innerHTML = '<path d="m22 7-8.991 5.727a2 2 0 0 1-2.009 0L2 7"/>\n  <rect x="2" y="4" width="20" height="16" rx="2"/>'
          } else {
            svg.innerHTML = '<path d="M21.2 8.4c.5.38.8.97.8 1.6v10a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V10a2 2 0 0 1 .8-1.6l8-6a2 2 0 0 1 2.4 0l8 6Z"/>\n  <path d="m22 10-8.97 5.7a1.94 1.94 0 0 1-2.06 0L2 10"/>'
          }
        }
      }
      invalidateMailListItem(emailId)
      refreshSidebarUnread()
    })
    .catch(function () {})
}

function toggleStar(emailId) {
  fetch("/api/messages/" + emailId + "/star", { method: "POST" })
    .then(function (r) { return r.json() })
    .then(function (data) {
      var starBtn = document.querySelector('[data-star-email="' + emailId + '"]')
      if (starBtn) {
        var svg = starBtn.querySelector('svg')
        if (svg) {
          if (data.is_starred) {
            svg.setAttribute('class', 'size-4 text-amber-500 fill-amber-500 drop-shadow-[0_1px_1px_rgba(180,120,0,0.3)]')
          } else {
            svg.setAttribute('class', 'size-4 text-ink/30')
          }
        }
      }
      invalidateMailListItem(emailId)
    })
    .catch(function () {})
}

function deleteMessage(emailId) {
  fetch("/api/messages/" + emailId, { method: "DELETE" })
    .then(function () {
      var mailView = document.getElementById("mail-view")
      if (mailView) mailView.innerHTML = ""
      var container = document.getElementById("mail-list-scroll")
      if (container && container._virtualMailList) {
        var vml = container._virtualMailList
        if (vml.selectedEmailId === emailId) vml.selectedEmailId = null
        vml.reset()
        vml.hydrateFromDOM()
        vml.switchFolder(vml.folderID)
      }
      refreshSidebarUnread()
    })
    .catch(function () {})
}

function moveMessage(emailId, folderId) {
  fetch("/api/messages/" + emailId + "/move", {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: "folder_id=" + encodeURIComponent(folderId)
  })
    .then(function () {
      if (virtualMailList) virtualMailList.onNewEmail()
      refreshSidebarUnread()
    })
    .catch(function () {})
}

function invalidateMailListItem(emailId) {
  var container = document.getElementById("mail-list-scroll")
  if (container && container._virtualMailList) {
    container._virtualMailList.invalidateItem(emailId)
  }
}

window.addEventListener("message", function (e) {
  if (e.data && e.data.type === "emailBodyResize") {
    var iframe = document.getElementById("email-body-frame")
    if (iframe) iframe.style.height = e.data.height + "px"
  }
})

function applyEmailBodyTheme() {
  var iframe = document.getElementById("email-body-frame")
  if (!iframe || !iframe.dataset.emailId) return
  var baseTheme = getEmailBodyBaseTheme()
  var theme = iframe.dataset.forceScheme === "opposite" ? oppositeEmailBodyTheme(baseTheme) : baseTheme
  var palette = readEmailBodyPalette(theme)
  var bg = palette.bg
  var fg = palette.fg
  var link = palette.link
  if (bg) iframe.style.backgroundColor = bg
  var params = new URLSearchParams()
  params.set("theme", theme)
  if (bg) params.set("bg", bg)
  if (fg) params.set("fg", fg)
  if (link) params.set("link", link)
  iframe.src = "/email/" + iframe.dataset.emailId + "/body?" + params.toString()
  updateEmailBodySchemeButton(iframe, baseTheme, theme)
}

function getEmailBodyBaseTheme() {
  if (window.GoferSettings && GoferSettings.get("theme")) return GoferSettings.get("theme")
  return document.documentElement.classList.contains("dark") ? "dark" : "light"
}

function oppositeEmailBodyTheme(theme) {
  return theme === "dark" ? "light" : "dark"
}

function readEmailBodyPalette(theme) {
  var themeStyle = (window.GoferSettings && GoferSettings.get("theme_style")) || document.documentElement.getAttribute("data-theme") || "classic"
  var probe = document.createElement("div")
  probe.setAttribute("data-theme", themeStyle)
  if (theme === "dark") probe.className = "dark"
  probe.style.cssText = "position:absolute;visibility:hidden;pointer-events:none;width:0;height:0;overflow:hidden"
  ;(document.body || document.documentElement).appendChild(probe)
  var cs = getComputedStyle(probe)
  var palette = {
    bg: (cs.getPropertyValue("--paper") || "").trim(),
    fg: (cs.getPropertyValue("--paper-foreground") || "").trim(),
    link: (cs.getPropertyValue("--copper") || "").trim(),
  }
  probe.remove()

  if (!palette.bg || !palette.fg) {
    var rootStyles = getComputedStyle(document.documentElement)
    palette.bg = palette.bg || (rootStyles.getPropertyValue("--paper") || "").trim()
    palette.fg = palette.fg || (rootStyles.getPropertyValue("--paper-foreground") || "").trim()
    palette.link = palette.link || (rootStyles.getPropertyValue("--copper") || "").trim()
  }
  return palette
}

function toggleEmailBodyScheme() {
  var iframe = document.getElementById("email-body-frame")
  if (!iframe || !iframe.dataset.emailId) return
  if (iframe.dataset.forceScheme === "opposite") {
    delete iframe.dataset.forceScheme
  } else {
    iframe.dataset.forceScheme = "opposite"
  }
  applyEmailBodyTheme()
}

function updateEmailBodySchemeButton(iframe, baseTheme, theme) {
  var btn = document.querySelector("[data-force-email-scheme]")
  if (!btn || !iframe) return
  var forced = iframe.dataset.forceScheme === "opposite"
  btn.classList.toggle("bg-ink/5", forced)
  btn.classList.toggle("text-ink/70", forced)
  btn.classList.toggle("text-ink/40", !forced)
  var label = forced ? "Showing " + theme + " email body. Click to use " + baseTheme + "." : "Force " + oppositeEmailBodyTheme(baseTheme) + " email body"
  btn.setAttribute("aria-label", label)
  btn.setAttribute("title", label)
}

document.addEventListener("DOMContentLoaded", function () {
  applyEmailBodyTheme()
})

new MutationObserver(function () {
  var iframe = document.getElementById("email-body-frame")
  if (iframe && iframe.dataset.emailId && !iframe.src) {
    applyEmailBodyTheme()
  }
}).observe(document.body, { childList: true, subtree: true })

function refetchBody(emailId) {
  fetch("/api/messages/" + emailId + "/refetch", { method: "POST" })
    .then(function (r) { return r.json() })
    .then(function (data) {
      if (data.status === "refetched" && typeof htmx !== "undefined") {
        htmx.ajax("GET", "/email/" + emailId, { target: "#mail-view", swap: "innerHTML" })
      }
    })
    .catch(function () {})
}

  document.addEventListener("click", function (e) {
    var el = e.target.closest("[data-refetch-email]")
    if (el) {
      e.preventDefault()
      refetchBody(el.dataset.refetchEmail)
    }
  })

  var _composeObserver = new MutationObserver(function () {
    var root = document.getElementById("compose-dialog")
    if (!root) return
    var open = root.getAttribute("data-tui-dialog-open") === "true"
    if (open) {
      _composeActive = true
      _updateComposeBtn(true)
    }
  })

  function _observeComposeDialog() {
    var root = document.getElementById("compose-dialog")
    if (root) _composeObserver.observe(root, { attributes: true, attributeFilter: ["data-tui-dialog-open"] })
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", _observeComposeDialog)
  } else {
    _observeComposeDialog()
  }

function _readComposeFormValues(form) {
  if (!form) return {}
  var vals = {}
  var inputs = form.querySelectorAll("input, textarea")
  for (var i = 0; inputs && i < inputs.length; i++) {
    if (inputs[i].name) vals[inputs[i].name] = inputs[i].value
  }
  var fromDisplay = form.querySelector("[id$='-from-display']")
  if (fromDisplay) vals._fromDisplay = fromDisplay.innerHTML
  var ccVisible = !!form.querySelector('[id^="pane-cc-field"]') && !document.getElementById("pane-cc-field").classList.contains("hidden")
  var bccVisible = !!form.querySelector('[id^="pane-bcc-field"]') && !document.getElementById("pane-bcc-field").classList.contains("hidden")
  if (form.id === "compose-form") {
    ccVisible = !document.getElementById("cc-field").classList.contains("hidden")
    bccVisible = !document.getElementById("bcc-field").classList.contains("hidden")
  }
  vals._ccVisible = ccVisible
  vals._bccVisible = bccVisible
  return vals
}

function _writeComposeFormValues(form, vals, prefix) {
  if (!form || !vals) return
  var inputs = form.querySelectorAll("input, textarea")
  for (var i = 0; inputs && i < inputs.length; i++) {
    if (inputs[i].name && vals[inputs[i].name] !== undefined) {
      inputs[i].value = vals[inputs[i].name]
    }
  }
  if (vals._fromDisplay) {
    var display = document.getElementById(prefix + "from-display")
    if (display) display.innerHTML = vals._fromDisplay
  }
}

function expandToPane() {
  var dialogForm = document.getElementById("compose-form")
  var vals = _readComposeFormValues(dialogForm)

  if (window.tui && window.tui.dialog) {
    window.tui.dialog.close("compose-dialog")
  }

  _composeActive = true
  _updateComposeBtn(true)

  var mailView = document.getElementById("mail-view")
  if (!mailView) return

  fetch("/compose/pane").then(function (r) { return r.text() }).then(function (html) {
    mailView.innerHTML = html

    var paneForm = document.getElementById("compose-pane-form")
    _writeComposeFormValues(paneForm, vals, "compose-pane-")

    if (vals._ccVisible) {
      var ccField = document.getElementById("pane-cc-field")
      var ccBtn = document.getElementById("pane-cc-btn")
      if (ccField) ccField.classList.remove("hidden")
      if (ccBtn) ccBtn.classList.add("hidden")
    }
    if (vals._bccVisible) {
      var bccField = document.getElementById("pane-bcc-field")
      var bccBtn = document.getElementById("pane-bcc-btn")
      if (bccField) bccField.classList.remove("hidden")
      if (bccBtn) bccBtn.classList.add("hidden")
    }

    var bodyField = paneForm && paneForm.querySelector('textarea[name="body"]')
    if (bodyField) bodyField.focus()
  }).catch(function () {})
}

function collapseToDialog() {
  collapseComposeFullWidth()

  var paneForm = document.getElementById("compose-pane-form")
  var vals = _readComposeFormValues(paneForm)

  var mailView = document.getElementById("mail-view")
  if (mailView) mailView.innerHTML = ""

  var dialogForm = document.getElementById("compose-form")
  _writeComposeFormValues(dialogForm, vals, "compose-")

  if (vals._ccVisible) {
    var ccField = document.getElementById("cc-field")
    var ccBtn = document.getElementById("cc-btn")
    if (ccField) ccField.classList.remove("hidden")
    if (ccBtn) ccBtn.classList.add("hidden")
  }
  if (vals._bccVisible) {
    var bccField = document.getElementById("bcc-field")
    var bccBtn = document.getElementById("bcc-btn")
    if (bccField) bccField.classList.remove("hidden")
    if (bccBtn) bccBtn.classList.add("hidden")
  }

  if (window.tui && window.tui.dialog) {
    window.tui.dialog.open("compose-dialog")
  }
}

function discardComposePane() {
  collapseComposeFullWidth()
  var mailView = document.getElementById("mail-view")
  if (mailView) mailView.innerHTML = ""
  _updateComposeBtn(false)
}

function expandComposeFullWidth() {
  var mailList = document.querySelector("#main-content > #mail-list")
  var resizeHandles = document.querySelectorAll('[data-panel="maillist"]')
  if (!mailList || mailList._animating) return

  mailList._animating = true
  mailList._savedWidth = mailList.style.width

  for (var i = 0; i < resizeHandles.length; i++) {
    resizeHandles[i]._savedDisplay = resizeHandles[i].style.display
    resizeHandles[i].style.transition = "opacity 0.25s ease"
    resizeHandles[i].style.opacity = "0"
  }

  mailList.style.transition = "width 0.3s cubic-bezier(0.4,0,0.2,1), opacity 0.25s ease, border-width 0.3s ease"
  mailList.style.overflow = "hidden"
  mailList.style.borderWidth = "0"

  requestAnimationFrame(function () {
    requestAnimationFrame(function () {
      mailList.style.width = "0px"
      mailList.style.opacity = "0"
    })
  })

  var composePane = document.querySelector("[data-compose-pane]")
  if (composePane) {
    composePane.style.animation = "pane-slide-in 0.3s ease-out"
  }

  function onEnd() {
    mailList.removeEventListener("transitionend", onEnd)
    mailList.style.display = "none"
    mailList.style.transition = ""
    mailList.style.opacity = ""
    mailList._animating = false
    for (var i = 0; i < resizeHandles.length; i++) {
      resizeHandles[i].style.display = "none"
      resizeHandles[i].style.transition = ""
      resizeHandles[i].style.opacity = ""
    }
  }
  mailList.addEventListener("transitionend", onEnd)

  var normal = document.getElementById("pane-btns-normal")
  var full = document.getElementById("pane-btns-full")
  if (normal) normal.style.display = "none"
  if (full) full.style.display = "flex"

  var bodyField = document.querySelector("#compose-pane-form textarea[name='body']")
  if (bodyField) bodyField.focus()
}

function collapseComposeFullWidth() {
  var mailList = document.querySelector("#main-content > #mail-list")
  var resizeHandles = document.querySelectorAll('[data-panel="maillist"]')
  if (!mailList || mailList._savedWidth === undefined) return false

  mailList.style.display = ""
  mailList.style.width = "0px"
  mailList.style.opacity = "0"
  mailList.style.overflow = "hidden"
  mailList.style.transition = "width 0.3s cubic-bezier(0.4,0,0.2,1), opacity 0.25s ease, border-width 0.3s ease"

  for (var i = 0; i < resizeHandles.length; i++) {
    resizeHandles[i].style.display = resizeHandles[i]._savedDisplay || ""
    delete resizeHandles[i]._savedDisplay
    resizeHandles[i].style.opacity = "0"
    resizeHandles[i].style.transition = "opacity 0.25s ease 0.1s"
  }

  void mailList.offsetHeight

  requestAnimationFrame(function () {
    mailList.style.width = mailList._savedWidth
    mailList.style.opacity = "1"
    for (var i = 0; i < resizeHandles.length; i++) {
      resizeHandles[i].style.opacity = "1"
    }
  })

  function onEnd() {
    mailList.removeEventListener("transitionend", onEnd)
    mailList.style.transition = ""
    mailList.style.opacity = ""
    mailList.style.overflow = ""
    mailList.style.borderWidth = ""
    delete mailList._savedWidth
    for (var i = 0; i < resizeHandles.length; i++) {
      resizeHandles[i].style.transition = ""
      resizeHandles[i].style.opacity = ""
    }
  }
  mailList.addEventListener("transitionend", onEnd)

  var normal = document.getElementById("pane-btns-normal")
  var full = document.getElementById("pane-btns-full")
  if (normal) normal.style.display = "flex"
  if (full) full.style.display = "none"

  return true
}
