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

function sendCompose() {
  var form = document.getElementById("compose-form")
  if (!form) return

  var toField = form.querySelector('input[name="to"]')
  if (!toField || !toField.value.trim()) {
    return
  }

  var params = new URLSearchParams()
  var inputs = form.querySelectorAll("input, textarea")
  for (var i = 0; i < inputs.length; i++) {
    params.append(inputs[i].name, inputs[i].value)
  }

  if (window.tui && window.tui.dialog) {
    window.tui.dialog.close("compose-dialog")
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

  var form = document.getElementById("compose-form")
  if (!form) return

  var toField = form.querySelector('input[name="to"]')
  var subjectField = form.querySelector('input[name="subject"]')
  var accountIdField = document.getElementById("compose-account-id")
  var inReplyToField = document.getElementById("compose-in-reply-to")
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
