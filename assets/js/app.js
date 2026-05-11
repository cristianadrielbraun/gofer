document.addEventListener("DOMContentLoaded", function () {
  if (!document.getElementById("mail-sync-indeterminate-style")) {
    var style = document.createElement("style")
    style.id = "mail-sync-indeterminate-style"
    style.textContent = "@keyframes mailSyncIndeterminate{0%{transform:translateX(-120%)}50%{transform:translateX(40%)}100%{transform:translateX(240%)}}"
    document.head.appendChild(style)
  }
  var virtualMailList = null
  var pendingSyncEvents = []
  var syncRefreshTimer = null
  var processingStatusHandler = null
  var syncStatesByFolder = Object.create(null)
  var prefetchedBodies = Object.create(null)
  var autoMarkReadTimer = null
  var autoMarkReadEmailId = null
  var suppressEmailUrlPushFor = null

  initVirtualScroll()
  setupFolderClickInterception()
  setupEmailSelectionTracking()
  setupMailListViewToggle()
  setupMailFilters()
  setupMailTableColumnResize()
  setupSSE()
  setupMailListActions()
  setupSidebarAccountCollapse()
  setupProcessingStatus()
  setupBodyPrefetch()
  setupEmailBodyModeTabs()
  refreshSidebarUnread()

  function setupEmailBodyModeTabs() {
    document.addEventListener("click", function (e) {
      var btn = e.target.closest("[data-email-body-mode-button]")
      if (!btn) return
      var toggle = btn.closest("[data-email-body-style-toggle]")
      if (!toggle) return

      var mode = btn.getAttribute("data-email-body-mode-button")
      if (mode !== "dark" && mode !== "light" && mode !== "original") return

      var emailId = toggle.getAttribute("data-email-body-style-toggle")
      var headerToggle = btn.getAttribute("data-email-body-mode-global") === "true"
      if (toggle._emailBodyModeTimer) clearTimeout(toggle._emailBodyModeTimer)
      toggle._emailBodyModeTimer = setTimeout(function () {
        if (headerToggle) {
          setEmailBodyModeForContainer(toggle, mode)
        } else if (emailId) {
          setEmailBodyModeById(emailId, mode)
        }
      }, 240)
    })
  }

  function setEmailBodyModeForContainer(toggle, mode) {
    var scope = toggle.closest("#mail-view") || document
    var frames = scope.querySelectorAll("[data-email-body-frame]")
    if (!frames.length) {
      setEmailBodyMode(mode)
      return
    }
    for (var i = 0; i < frames.length; i++) setEmailBodyModeOnFrame(frames[i], mode)
    for (var j = 0; j < frames.length; j++) applyEmailBodyTheme(frames[j])
  }

  function setupSidebarAccountCollapse() {
    function readState() {
      var raw = window.GoferSettings ? GoferSettings.get("sidebar_account_collapsed") : null
      try {
        return JSON.parse(raw || "{}") || {}
      } catch (_) {
        return {}
      }
    }

    function writeState(state) {
      if (window.GoferSettings) GoferSettings.set("sidebar_account_collapsed", JSON.stringify(state))
    }

    function setCollapsed(section, collapsed) {
      var toggle = section.querySelector("[data-sidebar-account-toggle]")
      section.setAttribute("data-sidebar-account-collapsed", collapsed ? "true" : "false")
      if (toggle) toggle.setAttribute("aria-expanded", collapsed ? "false" : "true")
    }

    function sectionHasActiveFolder(section) {
      return !!section.querySelector('a[hx-get^="/folder/"].bg-sidebar-accent')
    }

    function hydrate(root) {
      var state = readState()
      var sections = (root || document).querySelectorAll("[data-sidebar-account]")
      for (var i = 0; i < sections.length; i++) {
        var section = sections[i]
        var accountId = section.getAttribute("data-sidebar-account")
        var collapsed = state[accountId] === true && !sectionHasActiveFolder(section)
        setCollapsed(section, collapsed)
      }
      var initialStyle = document.querySelector("[data-sidebar-account-collapse-style]")
      if (initialStyle) initialStyle.remove()
    }

    document.addEventListener("click", function (e) {
      var toggle = e.target.closest("[data-sidebar-account-toggle]")
      if (!toggle) return

      e.preventDefault()
      e.stopPropagation()

      var section = toggle.closest("[data-sidebar-account]")
      if (!section) return
      var accountId = section.getAttribute("data-sidebar-account")
      var collapsed = section.getAttribute("data-sidebar-account-collapsed") !== "true"
      var state = readState()
      state[accountId] = collapsed
      writeState(state)
      setCollapsed(section, collapsed)
    })

    document.body.addEventListener("htmx:afterSettle", function (evt) {
      if (evt.target && evt.target.querySelector && evt.target.querySelector("[data-sidebar-account]")) {
        hydrate(evt.target)
      }
    })

    hydrate(document)
  }

  function setupMailListActions() {
    document.addEventListener("click", function (e) {
      var starBtn = e.target.closest(".star-btn")
      if (starBtn) {
      e.preventDefault()
      e.stopPropagation()
      e.stopImmediatePropagation()
        var emailId = starBtn.dataset.emailId
        if (emailId) toggleStar(emailId)
      }

      var deleteBtn = e.target.closest("[data-delete-account-action]")
      if (deleteBtn) {
        var accountId = deleteBtn.getAttribute("data-account-id")
        if (accountId && window.tui && window.tui.dialog) {
          window.tui.dialog.close("delete-account-" + accountId)
        }
      }
    })

    document.body.addEventListener("htmx:afterRequest", function (evt) {
      var path = evt.detail.pathInfo && evt.detail.pathInfo.requestPath
      var match = path && path.match(/^\/api\/accounts\/([^/]+)$/)
      if (!match || !evt.detail.xhr || evt.detail.xhr.status !== 202) return
      markAccountDeleting(match[1])
    })
  }

  function setupMailFilters() {
    var searchTimer = null

    function emptyFilters() {
      return {
        unread: false,
        starred: false,
        attachments: false,
        read: false,
        noAttachments: false,
        hasLabels: false,
        threadsOnly: false,
        from: "",
        to: "",
        subject: "",
        body: "",
        fromDomain: "",
        attachment: "",
        label: "",
        accountId: "",
        query: "",
        afterDate: "",
        beforeDate: "",
      }
    }

    function readFilters() {
      var filters = emptyFilters()
      var form = document.querySelector("[data-mail-filter-form]")
      if (form) {
        var status = form.querySelector('[data-mail-tristate="status"]')
        var attachments = form.querySelector('[data-mail-tristate="attachments"]')
        var statusValue = status ? status.getAttribute("data-mail-tristate-value") : ""
        var attachmentValue = attachments ? attachments.getAttribute("data-mail-tristate-value") : ""
        filters.unread = statusValue === "unread"
        filters.read = statusValue === "read"
        filters.attachments = attachmentValue === "yes"
        filters.noAttachments = attachmentValue === "no"
        filters.starred = !!form.querySelector('input[name="starred"]:checked')
        filters.hasLabels = !!form.querySelector('input[name="has_labels"]:checked')
        filters.threadsOnly = !!form.querySelector('input[name="threads_only"]:checked')
      }
      var advanced = document.querySelector("[data-mail-advanced-filter-form]")
      if (advanced) {
        filters.read = filters.read || !!advanced.querySelector('input[name="read"]:checked')
        filters.noAttachments = filters.noAttachments || !!advanced.querySelector('input[name="no_attachments"]:checked')
        filters.hasLabels = filters.hasLabels || !!advanced.querySelector('input[name="has_labels"]:checked')
        filters.threadsOnly = filters.threadsOnly || !!advanced.querySelector('input[name="threads_only"]:checked')
        filters.from = (advanced.querySelector('input[name="from"]') || {}).value || ""
        filters.to = (advanced.querySelector('input[name="to"]') || {}).value || ""
        filters.subject = (advanced.querySelector('input[name="subject"]') || {}).value || ""
        filters.body = (advanced.querySelector('input[name="body"]') || {}).value || ""
        filters.fromDomain = (advanced.querySelector('input[name="from_domain"]') || {}).value || ""
        filters.attachment = (advanced.querySelector('input[name="attachment"]') || {}).value || ""
        filters.label = (advanced.querySelector('input[name="label"]') || {}).value || ""
        filters.accountId = (advanced.querySelector('input[name="account_id"]') || {}).value || ""
        filters.afterDate = (advanced.querySelector('input[name="after_date"]') || {}).value || ""
        filters.beforeDate = (advanced.querySelector('input[name="before_date"]') || {}).value || ""
      }
      var search = document.querySelector("[data-mail-search-input]")
      if (search) filters.query = search.value || ""
      return filters
    }

    function syncFilterButton(filters) {
      var count = (filters.unread ? 1 : 0) + (filters.starred ? 1 : 0) + (filters.attachments ? 1 : 0) +
        (filters.read ? 1 : 0) + (filters.noAttachments ? 1 : 0) + (filters.hasLabels ? 1 : 0) +
        (filters.threadsOnly ? 1 : 0) + (filters.from ? 1 : 0) + (filters.to ? 1 : 0) +
        (filters.subject ? 1 : 0) + (filters.body ? 1 : 0) + (filters.fromDomain ? 1 : 0) +
        (filters.attachment ? 1 : 0) + (filters.label ? 1 : 0) + (filters.accountId ? 1 : 0) +
        (filters.query ? 1 : 0) + (filters.afterDate ? 1 : 0) + (filters.beforeDate ? 1 : 0)
      var button = document.querySelector("[data-mail-filter-button]")
      var badge = document.querySelector("[data-mail-filter-count]")
      if (button) {
        button.dataset.active = count > 0 ? "true" : "false"
        button.classList.toggle("text-primary", count > 0)
        button.classList.toggle("bg-accent", count > 0)
      }
      if (badge) {
        badge.textContent = String(count)
        badge.classList.toggle("hidden", count === 0)
      }
    }

    function advancedFilterDefs() {
      return [
        { key: "accountId", name: "account_id", label: "Account" },
        { key: "afterDate", name: "after_date", label: "After" },
        { key: "beforeDate", name: "before_date", label: "Before" },
        { key: "from", name: "from", label: "From" },
        { key: "fromDomain", name: "from_domain", label: "From domain" },
        { key: "to", name: "to", label: "To / Cc" },
        { key: "subject", name: "subject", label: "Subject" },
        { key: "body", name: "body", label: "Body" },
        { key: "attachment", name: "attachment", label: "Attachment" },
        { key: "label", name: "label", label: "Label" },
        { key: "read", name: "read", label: "Read" },
        { key: "noAttachments", name: "no_attachments", label: "No attachments" },
        { key: "hasLabels", name: "has_labels", label: "Has labels" },
        { key: "threadsOnly", name: "threads_only", label: "Threads only" },
      ]
    }

    function displayValueForAdvanced(name, value) {
      if (name === "account_id") {
        var item = document.querySelector('[data-tui-selectbox-value="' + value + '"]')
        return item ? item.textContent.trim() : value
      }
      return value
    }

    function renderAdvancedSummary() {
      var summary = document.querySelector("[data-mail-filter-summary]")
      if (!summary) return
      var filters = readFilters()
      var defs = advancedFilterDefs()
      var html = ""
      for (var i = 0; i < defs.length; i++) {
        var def = defs[i]
        var value = filters[def.key]
        if (!value) continue
        var text = typeof value === "boolean" ? def.label : (def.label + ": " + displayValueForAdvanced(def.name, value))
        html += '<button type="button" data-mail-filter-chip-remove="' + def.name + '" class="inline-flex items-center gap-1 rounded-full border border-border bg-background px-2 py-1 text-[11px] font-medium text-foreground hover:bg-accent">' + text.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;") + '<span class="text-muted-foreground">x</span></button>'
      }
      summary.innerHTML = html || '<span class="px-1 py-0.5 text-xs text-muted-foreground">No advanced filters applied</span>'
      summary.classList.toggle("hidden", false)
    }

    function clearInputs(selector) {
      var form = document.querySelector(selector)
      if (!form) return
      var inputs = form.querySelectorAll("input")
      for (var i = 0; i < inputs.length; i++) {
        if (inputs[i].type === "checkbox") inputs[i].checked = false
        else inputs[i].value = ""
      }
      var displays = form.querySelectorAll("[data-mail-date-display]")
      for (var j = 0; j < displays.length; j++) displays[j].textContent = "Any date"
      var calendars = form.querySelectorAll("[data-tui-calendar-container]")
      for (var k = 0; k < calendars.length; k++) {
        calendars[k].removeAttribute("data-tui-calendar-selected-date")
      }
      var selectHidden = form.querySelectorAll("[data-tui-selectbox-hidden-input]")
      for (var s = 0; s < selectHidden.length; s++) {
        selectHidden[s].value = ""
        var selectRoot = selectHidden[s].closest(".select-container")
        if (selectRoot) {
          var valueEl = selectRoot.querySelector("[data-tui-selectbox-placeholder]")
          if (valueEl) valueEl.textContent = valueEl.getAttribute("data-tui-selectbox-placeholder") || ""
          var selectedItems = selectRoot.querySelectorAll("[data-tui-selectbox-selected='true']")
          for (var si = 0; si < selectedItems.length; si++) selectedItems[si].setAttribute("data-tui-selectbox-selected", "false")
        }
      }
      var tristates = form.querySelectorAll("[data-mail-tristate]")
      for (var t = 0; t < tristates.length; t++) setTriState(tristates[t], "")
      renderAdvancedSummary()
    }

    function clearAdvancedFilter(name) {
      var form = document.querySelector("[data-mail-advanced-filter-form]")
      if (!form) return
      var input = form.querySelector('[name="' + name + '"]')
      if (input) {
        if (input.type === "checkbox") input.checked = false
        else input.value = ""
      }
      var dateDisplay = form.querySelector('[data-mail-date-display="' + name + '"]')
      if (dateDisplay) dateDisplay.textContent = "Any date"
      if (name === "account_id") {
        var selectRoot = input && input.closest(".select-container")
        if (selectRoot) {
          var valueEl = selectRoot.querySelector("[data-tui-selectbox-placeholder]")
          if (valueEl) valueEl.textContent = valueEl.getAttribute("data-tui-selectbox-placeholder") || ""
          var selectedItems = selectRoot.querySelectorAll("[data-tui-selectbox-selected='true']")
          for (var i = 0; i < selectedItems.length; i++) selectedItems[i].setAttribute("data-tui-selectbox-selected", "false")
        }
      }
      renderAdvancedSummary()
    }

    function setTriState(control, value) {
      if (!control) return
      control.setAttribute("data-mail-tristate-value", value || "")
      var buttons = control.querySelectorAll("[data-mail-tristate-option]")
      for (var i = 0; i < buttons.length; i++) {
        var active = buttons[i].getAttribute("data-mail-tristate-option") === (value || "")
        buttons[i].classList.toggle("bg-card", active)
        buttons[i].classList.toggle("text-foreground", active)
        buttons[i].classList.toggle("shadow-sm", active)
        buttons[i].classList.toggle("text-muted-foreground", !active)
      }
    }

    function applyCurrentFilters() {
      var filters = readFilters()
      syncFilterButton(filters)
      if (virtualMailList) virtualMailList.applyFilters(filters).catch(function () {})
    }

    function scheduleSearchFilter() {
      if (searchTimer) clearTimeout(searchTimer)
      searchTimer = setTimeout(function () {
        searchTimer = null
        applyCurrentFilters()
      }, 300)
    }

    document.addEventListener("submit", function (e) {
      var form = e.target && e.target.closest && e.target.closest("[data-mail-filter-form]")
      if (!form) return
      e.preventDefault()
    })

    document.addEventListener("change", function (e) {
      var input = e.target && e.target.closest && e.target.closest("[data-mail-filter-input]")
      if (!input) return
      applyCurrentFilters()
    })

    document.addEventListener("click", function (e) {
      var panelButton = e.target && e.target.closest && e.target.closest("[data-mail-filter-panel-button]")
      if (panelButton) {
        e.preventDefault()
        var panel = panelButton.getAttribute("data-mail-filter-panel-button")
        document.querySelectorAll("[data-mail-filter-panel-button]").forEach(function (btn) {
          var active = btn === panelButton
          btn.classList.toggle("bg-accent", active)
          btn.classList.toggle("text-foreground", active)
          btn.classList.toggle("text-muted-foreground", !active)
        })
        document.querySelectorAll("[data-mail-filter-panel]").forEach(function (section) {
          section.classList.toggle("hidden", section.getAttribute("data-mail-filter-panel") !== panel)
        })
        return
      }

      var chipRemove = e.target && e.target.closest && e.target.closest("[data-mail-filter-chip-remove]")
      if (chipRemove) {
        e.preventDefault()
        clearAdvancedFilter(chipRemove.getAttribute("data-mail-filter-chip-remove"))
        return
      }

      var tristateOption = e.target && e.target.closest && e.target.closest("[data-mail-tristate-option]")
      if (tristateOption) {
        e.preventDefault()
        var control = tristateOption.closest("[data-mail-tristate]")
        setTriState(control, tristateOption.getAttribute("data-mail-tristate-option") || "")
        applyCurrentFilters()
        return
      }

      var advancedOpen = e.target && e.target.closest && e.target.closest("[data-mail-advanced-filter-open]")
      if (advancedOpen) {
        e.preventDefault()
        renderAdvancedSummary()
        if (window.tui && window.tui.dialog) window.tui.dialog.open("mail-advanced-filter-dialog")
        return
      }

      var clear = e.target && e.target.closest && e.target.closest("[data-mail-filter-clear]")
      if (!clear) return
      e.preventDefault()
      clearInputs("[data-mail-filter-form]")
      clearInputs("[data-mail-advanced-filter-form]")
      var search = document.querySelector("[data-mail-search-input]")
      if (search) search.value = ""
      applyCurrentFilters()
    })

    document.addEventListener("submit", function (e) {
      var form = e.target && e.target.closest && e.target.closest("[data-mail-advanced-filter-form]")
      if (!form) return
      e.preventDefault()
      applyCurrentFilters()
      if (window.tui && window.tui.dialog) window.tui.dialog.close("mail-advanced-filter-dialog")
    })

    document.addEventListener("click", function (e) {
      var clear = e.target && e.target.closest && e.target.closest("[data-mail-advanced-filter-clear]")
      if (!clear) return
      e.preventDefault()
      clearInputs("[data-mail-advanced-filter-form]")
    })

    document.addEventListener("input", function (e) {
      if (e.target && e.target.matches && e.target.matches("[data-mail-search-input]")) {
        scheduleSearchFilter()
        return
      }
      if (!e.target || !e.target.closest || !e.target.closest("[data-mail-advanced-filter-form]")) return
      renderAdvancedSummary()
    })

    document.addEventListener("search", function (e) {
      if (!e.target || !e.target.matches || !e.target.matches("[data-mail-search-input]")) return
      scheduleSearchFilter()
    })

    document.addEventListener("change", function (e) {
      if (!e.target || !e.target.closest || !e.target.closest("[data-mail-advanced-filter-form]")) return
      renderAdvancedSummary()
    })

    document.addEventListener("calendar-date-selected", function (e) {
      var container = e.target && e.target.closest && e.target.closest("[data-tui-calendar-container]")
      if (!container) return
      var hidden = container.closest("[data-tui-calendar-wrapper]") && container.closest("[data-tui-calendar-wrapper]").querySelector("[data-tui-calendar-hidden-input]")
      if (!hidden || !hidden.name) return
      var display = document.querySelector('[data-mail-date-display="' + hidden.name + '"]')
      if (display) display.textContent = hidden.value || "Any date"
      renderAdvancedSummary()
    })
  }

  function setupBodyPrefetch() {
    var hoverPrefetchDelay = 300
    var scrollPrefetchCooldown = 200
    var hoverPrefetchTimer = null
    var hoverPrefetchRow = null
    var lastMailListScrollAt = 0

    var mailListScroll = document.getElementById("mail-list-scroll")
    if (mailListScroll) {
      mailListScroll.addEventListener("scroll", function () {
        lastMailListScrollAt = Date.now()
        clearHoverPrefetch()
      }, { passive: true })
    }

    function prefetchRow(row) {
      if (window.GoferSettings && GoferSettings.get("prefetch_on_hover") === "false") return
      if (!row) return
      var emailId = row.dataset.emailId
      if (!emailId || prefetchedBodies[emailId]) return
      prefetchedBodies[emailId] = true
      fetch("/api/messages/" + encodeURIComponent(emailId) + "/prefetch-body", { method: "POST" }).catch(function () {
        delete prefetchedBodies[emailId]
      })
    }

    function clearHoverPrefetch(row) {
      if (row && hoverPrefetchRow !== row) return
      if (hoverPrefetchTimer) clearTimeout(hoverPrefetchTimer)
      hoverPrefetchTimer = null
      hoverPrefetchRow = null
    }

    document.addEventListener("pointerover", function (e) {
      if (e.pointerType && e.pointerType !== "mouse") return
      if (Date.now() - lastMailListScrollAt < scrollPrefetchCooldown) return
      var row = e.target && e.target.closest && e.target.closest(".mail-list-item[data-email-id]")
      if (!row || row === hoverPrefetchRow) return
      clearHoverPrefetch()
      hoverPrefetchRow = row
      hoverPrefetchTimer = setTimeout(function () {
        prefetchRow(row)
        clearHoverPrefetch(row)
      }, hoverPrefetchDelay)
    }, { passive: true })

    document.addEventListener("pointerout", function (e) {
      if (e.pointerType && e.pointerType !== "mouse") return
      var row = e.target && e.target.closest && e.target.closest(".mail-list-item[data-email-id]")
      if (!row) return
      var next = e.relatedTarget && e.relatedTarget.closest && e.relatedTarget.closest(".mail-list-item[data-email-id]")
      if (next === row) return
      clearHoverPrefetch(row)
    }, { passive: true })

    document.addEventListener("focusin", function (e) {
      prefetchRow(e.target && e.target.closest && e.target.closest(".mail-list-item[data-email-id]"))
    })
  }

  function markAccountDeleting(accountId) {
    var card = document.getElementById("account-card-" + accountId)
    if (!card) return
    if (window.tui && window.tui.dialog) {
      window.tui.dialog.close("delete-account-" + accountId)
    }
    var row = card.firstElementChild
    if (!row) return
    while (row.children.length > 2) {
      row.removeChild(row.lastElementChild)
    }
    var status = document.createElement("button")
    status.type = "button"
    status.disabled = true
    status.className = "inline-flex items-center gap-1.5 text-xs text-amber-700 dark:text-amber-400 px-2.5 py-1.5 rounded-md border border-amber-300/40 dark:border-amber-500/30 bg-amber-100/50 dark:bg-amber-500/10 cursor-default"
    status.innerHTML = '<svg class="size-3.5 animate-spin" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12a9 9 0 1 1-2.64-6.36"/><path d="M21 3v6h-6"/></svg>Deleting'
    row.appendChild(status)
  }

  function setupProcessingStatus() {
    var minimized = false
    var animating = false
    var expandedBodyText = ""

    function ensureWidget() {
      var existing = document.getElementById("processing-structure-widget")
      if (existing) return existing

      var widget = document.createElement("div")
      widget.id = "processing-structure-widget"
      widget.className = "fixed bottom-3 right-3 z-50 max-w-sm w-[min(92vw,24rem)] origin-bottom-right"
      widget.style.display = "none"
      widget.innerHTML =
        '<button type="button" data-processing-card class="absolute right-0 bottom-0 rounded-[var(--radius)] border border-border bg-card/95 text-card-foreground shadow-lg px-3 py-2.5 text-left transition-all duration-210 ease-in-out origin-bottom-right">' +
          '<div class="flex items-start justify-between gap-3">' +
            '<div class="min-w-0" data-processing-content-wrap>' +
              '<p data-processing-title class="text-[12px] font-semibold leading-5 text-amber-600 dark:text-amber-400 inline-flex items-center gap-1.5"><span class="inline-block size-2 rounded-full bg-amber-500 animate-pulse"></span><span>Processing structure</span></p>' +
              '<p data-processing-text class="text-[11px] leading-4 text-muted-foreground mt-0.5 transition-opacity duration-180 ease-out"></p>' +
              '<p data-processing-mini-text class="hidden text-[11px] leading-4 text-muted-foreground mt-0 items-center gap-1.5"><span class="inline-block size-2 rounded-full bg-amber-500 animate-pulse"></span><span>Processing...</span></p>' +
            '</div>' +
            '<span data-processing-minimize class="shrink-0 rounded p-1 hover:bg-muted" aria-hidden="true">' +
              '<svg xmlns="http://www.w3.org/2000/svg" class="size-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M5 12h14"/></svg>' +
            '</span>' +
          '</div>' +
        '</button>'

      document.body.appendChild(widget)
      widget.style.minHeight = "44px"

      var card = widget.querySelector("[data-processing-card]")

      if (card) {
        card.style.transition = "none"
        card.style.width = "380px"
        card.style.height = "92px"
        card.style.paddingTop = "10px"
        card.style.paddingBottom = "10px"
        card.offsetHeight
        card.style.transition = ""
      }

      if (card) {
        card.addEventListener("click", function () {
          minimized = !minimized
          applyMinimizedState(widget)
        })
      }

      return widget
    }

    function applyMinimizedState(widget) {
      var card = widget.querySelector("[data-processing-card]")
      var text = widget.querySelector("[data-processing-text]")
      var title = widget.querySelector("[data-processing-title]")
      var miniText = widget.querySelector("[data-processing-mini-text]")
      var minimizeIcon = widget.querySelector("[data-processing-minimize]")
      if (!card || !text || !title || !miniText || !minimizeIcon) return
      if (animating) return

      var FADE_MS = 100
      var SIZE_MS = 200
      animating = true

      if (!expandedBodyText) {
        expandedBodyText = text.textContent || ""
      }

      if (minimized) {
        text.style.opacity = "0"
        title.style.opacity = "0"
        minimizeIcon.style.opacity = "0"
        setTimeout(function () {
          title.style.display = "none"
          text.style.display = "none"
          card.style.width = "148px"
          card.style.height = "44px"
          card.style.paddingTop = "8px"
          card.style.paddingBottom = "8px"
          setTimeout(function () {
            miniText.style.display = "inline-flex"
            miniText.style.opacity = "0"
            miniText.style.transition = "opacity 180ms ease-out"
            miniText.offsetHeight
            miniText.style.opacity = "1"
            animating = false
          }, SIZE_MS)
        }, FADE_MS)
      } else {
        miniText.style.opacity = "0"
        setTimeout(function () {
          miniText.style.display = "none"
          card.style.width = "380px"
          card.style.height = "92px"
          card.style.paddingTop = "10px"
          card.style.paddingBottom = "10px"
          setTimeout(function () {
            title.style.display = "inline-flex"
            text.style.display = "block"
            text.textContent = expandedBodyText
            title.style.opacity = "0"
            text.style.opacity = "0"
            minimizeIcon.style.opacity = "0"
            title.offsetHeight
            title.style.opacity = "1"
            text.style.opacity = "1"
            minimizeIcon.style.opacity = "1"
            animating = false
          }, SIZE_MS)
        }, FADE_MS)
      }
    }

    function render(state) {
      var widget = ensureWidget()
      if (!widget) return
      window.__processingState = state
      var active = !!(state && (state.in_progress || ((state.processed || 0) > 0 && (state.total || 0) > 0 && (state.processed || 0) < (state.total || 0))))
      if (!active) {
        widget.style.display = "none"
        return
      }
      var progress = ""
      if (state.total > 0) progress = " (" + (state.processed || 0) + "/" + state.total + ")"
      var text = widget.querySelector("[data-processing-text]")
      if (text) {
        expandedBodyText = "This may take longer the first time while your mailbox is organized." + progress
        if (!minimized) {
          text.textContent = expandedBodyText
        }
      }
      widget.style.display = "block"
      applyMinimizedState(widget)
    }

    processingStatusHandler = {
      render: render,
      startPolling: function () {},
      stopPolling: function () {}
    }
  }

  function setupSSE() {
    if (window.location.pathname.startsWith("/settings")) return

    var source = new EventSource("/api/events")

    source.addEventListener("new-mail", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { return }
      if (!data || !data.folder_id) return

      refreshSidebarUnread()
      withMailListForFolder(data.folder_id, data.folder_role || "", function (vml) { vml.onNewEmail() })
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

    source.addEventListener("processing-status", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { return }
      if (!data || !processingStatusHandler) return
      processingStatusHandler.render(data)
    })

    source.addEventListener("sync-started", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { return }
      if (!data || !data.folder_id) return
      syncStatesByFolder[data.folder_id] = {
        active: true,
        current: data.current || 0,
        total: data.total || 0,
        folderRole: data.folder_role || "",
      }
      withMailListForFolder(data.folder_id, data.folder_role, function (vml) {
        vml.setSyncState(true, data.current || 0, data.total || 0)
      })
      if (virtualMailList) scheduleSyncRefresh(virtualMailList)
    })

    source.addEventListener("sync-progress", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { return }
      if (!data || !data.folder_id) return
      syncStatesByFolder[data.folder_id] = {
        active: true,
        current: data.current || 0,
        total: data.total || 0,
        folderRole: data.folder_role || "",
      }
      withMailListForFolder(data.folder_id, data.folder_role, function (vml) {
        vml.setSyncState(true, data.current || 0, data.total || 0)
        scheduleSyncRefresh(vml)
      })
      if (virtualMailList) scheduleSyncRefresh(virtualMailList)
    })

    source.addEventListener("sync-complete", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { return }
      if (!data || !data.folder_id) return
      syncStatesByFolder[data.folder_id] = {
        active: false,
        current: 0,
        total: 0,
        folderRole: data.folder_role || "",
      }
      refreshSidebarUnread()
      withMailListForFolder(data.folder_id, data.folder_role, function (vml) {
        vml.setSyncState(false, 0, 0)
        vml.refreshCurrentFolder()
      })
      if (virtualMailList) {
        applyActiveFolderSyncState()
        virtualMailList.refreshCurrentFolder()
      }
    })

    source.onerror = function () {
      source.close()
      setTimeout(setupSSE, 5000)
    }
  }

  function scheduleSyncRefresh(vml) {
    if (!vml) return
    if (syncRefreshTimer) return
    syncRefreshTimer = setTimeout(function () {
      syncRefreshTimer = null
      vml.refreshCurrentFolder().catch(function () {})
    }, 700)
  }

  function withMailListForFolder(folderId, folderRole, fn) {
    if (typeof folderRole === "function") {
      fn = folderRole
      folderRole = ""
    }
    if (typeof fn !== "function") return
    if (virtualMailList && matchesActiveFolder(virtualMailList.folderID, folderId, folderRole)) {
      fn(virtualMailList)
      return
    }
    pendingSyncEvents.push({ folderId: folderId, folderRole: folderRole || "", fn: fn })
  }

  function matchesActiveFolder(activeFolderId, eventFolderId, eventFolderRole) {
    if (!activeFolderId) return false
    if (activeFolderId === eventFolderId) return true
    if (!eventFolderRole) return false
    return isRoleFolderID(activeFolderId) && activeFolderId === eventFolderRole
  }

  function isRoleFolderID(folderId) {
    return folderId === "inbox" || folderId === "sent" || folderId === "drafts" || folderId === "trash" || folderId === "archive" || folderId === "spam"
  }

  function applyActiveFolderSyncState() {
    if (!virtualMailList) return
    var state = syncStatesByFolder[virtualMailList.folderID]
    if (state && state.active) {
      virtualMailList.setSyncState(true, state.current || 0, state.total || 0)
      return
    }
    virtualMailList.setSyncState(false, 0, 0)
  }

  function flushPendingSyncEvents() {
    if (!virtualMailList || pendingSyncEvents.length === 0) return
    var remaining = []
    for (var i = 0; i < pendingSyncEvents.length; i++) {
      var event = pendingSyncEvents[i]
      if (matchesActiveFolder(virtualMailList.folderID, event.folderId, event.folderRole)) event.fn(virtualMailList)
      else remaining.push(event)
    }
    pendingSyncEvents = remaining.slice(-50)
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
                link.appendChild(badge)
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
    if (loadInitialFolderContent(container, folderID)) return

    virtualMailList = new VirtualMailList(container, { folderID: folderID, viewMode: container.dataset.viewMode || "cards" })
    virtualMailList.hydrateFromDOM()
    container._virtualMailList = virtualMailList
    flushPendingSyncEvents()
    applyActiveFolderSyncState()
    autoloadFirstEmail(container)
    bindThreadToggle(container)

    var selectedId = virtualMailList.selectedEmailId
    var path = "/folder/" + folderID
    if (selectedId) path += "/" + selectedId
    history.replaceState({ folder: folderID, email: selectedId || null }, "", path)
  }

  function loadInitialFolderContent(container, folderID) {
    if (!container || !container.hasAttribute("data-load-folder")) return false
    container.removeAttribute("data-load-folder")
    if (window.location.pathname !== "/folder/" + folderID) {
      history.replaceState({ folder: folderID, email: null }, "", "/folder/" + folderID)
    }
    if (typeof htmx !== "undefined") {
      htmx.ajax("GET", "/folder/" + folderID + "/full", {target: "#main-content", swap: "outerHTML"})
    }
    return true
  }

  function bindThreadToggle(container) {
    if (!container || container._threadToggleBound) return
    container._threadToggleBound = true
    container.addEventListener("click", function (e) {
      var toggle = e.target.closest("[data-thread-toggle]")
      if (!toggle || !container.contains(toggle)) return
      e.preventDefault()
      e.stopPropagation()
      var emailId = toggle.dataset.threadToggle
      var vml = container._virtualMailList || virtualMailList
      if (vml && emailId) vml.toggleThreadExpand(emailId)
    })
  }

  function autoloadFirstEmail(container) {
    if (!container || !container.hasAttribute("data-autoload-first-email")) return
    container.removeAttribute("data-autoload-first-email")
    var first = container.querySelector(".mail-list-item[data-email-id]")
    if (!first || !first.dataset.emailId || typeof htmx === "undefined") return

    if (virtualMailList) {
      virtualMailList.selectedEmailId = first.dataset.emailId
      virtualMailList.syncSelectionClasses(virtualMailList.itemsContainer)
      virtualMailList.pushUrl()
    }
    if (typeof showMailViewLoading === "function") showMailViewLoading()
    suppressEmailUrlPushFor = first.dataset.emailId
    htmx.ajax("GET", "/email/" + first.dataset.emailId, "#mail-view")
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
        collapseComposeFullWidth()
        setMailViewEmpty()
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
        var badge = sidebarLinks[i].querySelector("[data-folder-unread]")
        if (badge) {
          badge.classList.remove("bg-sidebar-primary/20", "text-sidebar-primary")
          badge.classList.add("bg-sidebar-accent", "text-sidebar-foreground/80")
        }
      }
      link.classList.add(
        "bg-sidebar-accent",
        "text-sidebar-primary",
        "font-medium"
      )
      link.classList.remove("text-sidebar-foreground")
      var activeBadge = link.querySelector("[data-folder-unread]")
      if (activeBadge) {
        activeBadge.classList.remove("bg-sidebar-accent", "text-sidebar-foreground/80")
        activeBadge.classList.add("bg-sidebar-primary/20", "text-sidebar-primary")
      }

      var mainContent = document.getElementById("main-content")
      var isOnSettings = mainContent && mainContent.querySelector("[data-tui-tabs]")
      if (isOnSettings || !virtualMailList) {
        if (typeof htmx !== "undefined") {
          history.pushState({ folder: folderID, email: null }, "", "/folder/" + folderID)
          if (mainContent) showMailContentLoading(mainContent, link)
          htmx.ajax("GET", "/folder/" + folderID + "/full", {target: "#main-content", swap: "outerHTML"})
        }
      } else {
        virtualMailList.switchFolder(folderID).then(function () {
          applyActiveFolderSyncState()
          scheduleSyncRefresh(virtualMailList)
        }).catch(function () {})
      }
    }, true)
  }

  function showMailContentLoading(mainContent, folderLink) {
    var folderName = textFrom(folderLink, ".flex-1") || "Mail"
    var count = textFrom(folderLink, "[data-folder-unread]")
    mainContent.innerHTML = '<div id="mail-list" class="w-full lg:flex flex-col border-r border-border bg-card h-full overflow-hidden">' +
      '<div class="px-4 py-4 space-y-3">' +
      '<div class="flex items-center justify-between">' +
      '<div class="flex items-center gap-2">' +
      '<h2 id="mail-folder-name" class="text-lg font-bold tracking-tight" style="font-family: var(--font-serif)">' + escapeHTML(folderName) + '</h2>' +
      (count ? '<span id="mail-folder-count" class="text-xs text-muted-foreground bg-muted px-2 py-0.5 rounded-full font-medium shadow-[0_1px_2px_rgba(0,0,0,0.06)]">' + escapeHTML(count) + '</span>' : '') +
      '</div>' +
      '<div class="h-8 w-8 rounded-md bg-muted/50"></div>' +
      '</div>' +
      '<div class="flex items-center gap-2">' +
      '<div class="relative groove rounded-lg flex-1 min-w-0">' +
      '<input type="text" placeholder="Quick search" disabled class="h-9 w-full pl-3 pr-3 rounded-lg text-sm bg-background border border-border/50 opacity-60" />' +
      '</div>' +
      '<button type="button" disabled class="inline-flex h-9 shrink-0 items-center rounded-lg border border-border bg-card px-2.5 text-xs font-semibold text-muted-foreground opacity-60">Advanced filters</button>' +
      '</div>' +
      '</div>' +
      '<div class="flex items-center gap-1 px-4 py-1.5 border-y border-border/70">' +
      '<div class="h-7 w-7 rounded-md bg-muted/50"></div>' +
      '<div class="flex-1"></div>' +
      '<div class="h-7 w-20 rounded-lg bg-muted/50"></div>' +
      '</div>' +
      '<div class="flex-1 overflow-y-auto px-2 py-2 flex items-center justify-center">' +
      '<div class="flex items-center gap-2 text-sm text-muted-foreground">' +
      '<div class="size-4 border-2 border-muted-foreground/30 border-t-muted-foreground rounded-full animate-spin"></div>' +
      '<span>Loading content...</span>' +
      '</div>' +
      '</div>' +
      '</div>' +
      '<div class="resize-handle" data-panel="maillist" draggable="false"></div>' +
      '<div id="mail-view" class="hidden lg:flex flex-1 flex-col min-w-0 bg-background surface-desk">' +
      '<div class="flex flex-col items-center justify-center h-full text-center p-8">' +
      '<h3 class="text-lg font-semibold mb-2">Select an email</h3>' +
      '<p class="text-sm text-muted-foreground">Choose a message from the list to read it.</p>' +
      '</div>' +
      '</div>'
    if (typeof initResizeHandles === "function") initResizeHandles()
  }

  function setupEmailSelectionTracking() {
    document.body.addEventListener("htmx:beforeRequest", function (evt) {
      if (
        evt.detail.pathInfo &&
        evt.detail.pathInfo.requestPath &&
          evt.detail.pathInfo.requestPath.match(/^\/email\/[^/?]+(?:\?.*)?$/)
      ) {
        showMailViewLoading(evt.detail.elt)
      }
    })

	document.body.addEventListener("htmx:afterRequest", function (evt) {
	  if (
		evt.detail.pathInfo &&
		evt.detail.pathInfo.requestPath &&
		evt.detail.pathInfo.requestPath.startsWith("/email/")
	  ) {
		var emailId = evt.detail.pathInfo.requestPath.replace("/email/", "").split("?")[0]
		if (virtualMailList) {
		  if (suppressEmailUrlPushFor === emailId) {
		    suppressEmailUrlPushFor = null
		    virtualMailList.selectedEmailId = emailId
		    virtualMailList.syncSelectionClasses(virtualMailList.itemsContainer)
		  } else {
		    virtualMailList.onEmailSelected(emailId)
		  }
		}
		scheduleAutoMarkRead(emailId, evt.detail.elt)
	  }
	})
  }

  function setupMailListViewToggle() {
    document.body.addEventListener("click", function (e) {
      var btn = e.target.closest("[data-mail-list-view-button]")
      if (!btn) return
      e.preventDefault()

      var mode = btn.dataset.mailListViewButton === "table" ? "table" : "cards"
      if (window.GoferSettings) GoferSettings.set("mail_list_view", mode)

      var group = btn.closest("[data-mail-list-view-toggle]")
      if (group) {
        var buttons = group.querySelectorAll("[data-mail-list-view-button]")
        for (var i = 0; i < buttons.length; i++) {
          var isActive = buttons[i] === btn
          buttons[i].classList.toggle("text-foreground", isActive)
          buttons[i].classList.toggle("text-muted-foreground", !isActive)
          buttons[i].classList.toggle("hover:text-foreground", !isActive)
        }
        var indicator = group.querySelector("[data-mail-list-view-indicator]")
        if (indicator) {
          indicator.style.transform = mode === "table" ? "translateX(100%)" : "translateX(0)"
        }
      }

      var scroll = document.getElementById("mail-list-scroll")
      var vml = scroll && scroll._virtualMailList
      if (vml && typeof vml.switchViewMode === "function") {
        vml.switchViewMode(mode).catch(function () {})
      }
    })
  }

  function setupMailTableColumnResize() {
    var columnIds = ["accountMarker", "starred", "attachment", "thread", "from", "to", "subject", "date"]
    var minWidths = [24, 32, 32, 28, 90, 90, 140, 64]
    var fixedWidths = { accountMarker: 24, starred: 24, attachment: 24 }
    var defaultRatios = [0.8, 0.8, 0.8, 1, 3, 3, 5, 2]

    function clamp(value, index) {
      return Math.max(minWidths[index], value)
    }

    function currentRatioSetting() {
      var raw = window.GoferSettings ? GoferSettings.get("mail_table_column_widths") : null
      var parts = raw ? String(raw).split(",") : []
      if (parts.length !== columnIds.length) parts = []
      var values = []
      for (var i = 0; i < columnIds.length; i++) {
        var n = parseFloat(parts[i])
        values.push(isNaN(n) || n <= 0 ? defaultRatios[i] : n)
      }
      return values
    }

    function widthSetting(widths, ids) {
      var values = currentRatioSetting()
      var total = 0
      for (var i = 0; i < widths.length; i++) {
        if (!fixedWidths[ids[i]]) total += widths[i]
      }
      if (total <= 0) return values.join(",")
      for (var j = 0; j < ids.length; j++) {
        var index = columnIds.indexOf(ids[j])
        if (index !== -1 && !fixedWidths[ids[j]]) values[index] = widths[j] / total
      }
      return values.map(function (value) { return value.toFixed(5) }).join(",")
    }

    function currentWidths(header) {
      var cells = header.querySelectorAll("[data-mail-table-column-id]")
      var widths = []
      var ids = []
      for (var i = 0; i < cells.length; i++) {
        if (cells[i].offsetParent === null) continue
        var id = cells[i].dataset.mailTableColumnId
        var index = columnIds.indexOf(id)
        if (index === -1) continue
        widths.push(clamp(Math.round(cells[i].getBoundingClientRect().width), index))
        ids.push(id)
      }
      return widths.length > 1 ? { ids: ids, widths: widths } : null
    }

    function applyWidths(widths, ids, scroll) {
      var value = widthSetting(widths, ids)
      if (typeof window.applyMailTableColumnWidths === "function") {
        window.applyMailTableColumnWidths(value, scroll)
      } else if (scroll) {
        scroll.style.setProperty("--mail-list-table-columns", widths.map(function (w) { return w + "px" }).join(" "))
      }
    }

    document.body.addEventListener("pointerdown", function (e) {
      var handle = e.target.closest("[data-mail-table-resize]")
      if (!handle) return
      var header = handle.closest(".mail-list-table-header")
      var scroll = header && header.closest("#mail-list-scroll")
      if (!header || !scroll) return

      e.preventDefault()
      e.stopPropagation()

      var state = currentWidths(header)
      if (!state) return
      var cell = handle.closest("[data-mail-table-column-id]")
      var visibleIndex = cell ? state.ids.indexOf(cell.dataset.mailTableColumnId) : -1
      if (visibleIndex < 0 || visibleIndex >= state.widths.length - 1) return
      if (fixedWidths[state.ids[visibleIndex]] || fixedWidths[state.ids[visibleIndex + 1]]) return

      var startX = e.clientX
      var widths = state.widths.slice()
      var startLeft = widths[visibleIndex]
      var startRight = widths[visibleIndex + 1]
      var leftIndex = columnIds.indexOf(state.ids[visibleIndex])
      var rightIndex = columnIds.indexOf(state.ids[visibleIndex + 1])
      document.body.classList.add("mail-list-column-resizing")
      handle.setAttribute("data-resizing", "")
      if (handle.setPointerCapture) handle.setPointerCapture(e.pointerId)

      function onMove(moveEvent) {
        var delta = moveEvent.clientX - startX
        var nextLeft = clamp(startLeft + delta, leftIndex)
        var consumed = nextLeft - startLeft
        var nextRight = clamp(startRight - consumed, rightIndex)
        if (nextRight !== startRight - consumed) {
          nextLeft = clamp(startLeft + (startRight - nextRight), leftIndex)
        }
        widths[visibleIndex] = nextLeft
        widths[visibleIndex + 1] = nextRight
        applyWidths(widths, state.ids, scroll)
      }

      function onUp() {
        document.removeEventListener("pointermove", onMove)
        document.removeEventListener("pointerup", onUp)
        document.body.classList.remove("mail-list-column-resizing")
        handle.removeAttribute("data-resizing")
        if (window.GoferSettings) GoferSettings.set("mail_table_column_widths", widthSetting(widths, state.ids))
      }

      document.addEventListener("pointermove", onMove)
      document.addEventListener("pointerup", onUp)
    })

    document.body.addEventListener("click", function (e) {
      var button = e.target.closest("[data-mail-table-column-menu-button]")
      if (button) {
        var root = button.closest("[data-tui-popover-root]")
        var menu = root && root.querySelector("[data-mail-table-column-menu]")
        if (menu) syncColumnMenu(menu)
        return
      }

      var item = e.target.closest("[data-mail-table-column-item]")
      if (item) {
        var menuPanel = item.closest("[data-mail-table-column-menu]")
        if (!menuPanel) return
        var selected = typeof window.getMailTableColumns === "function" ? window.getMailTableColumns().slice() : columnIds.slice()
        var id = item.dataset.mailTableColumnItem
        var index = selected.indexOf(id)
        if (index === -1) {
          selected.push(id)
        } else if (selected.length > 1) {
          selected.splice(index, 1)
        } else {
          return
        }
        selected.sort(function (a, b) { return columnIds.indexOf(a) - columnIds.indexOf(b) })
        if (window.GoferSettings) GoferSettings.set("mail_table_columns", selected.join(","))
        syncColumnMenu(menuPanel)
      }
    })

    function syncColumnMenu(menu) {
      var selected = typeof window.getMailTableColumns === "function" ? window.getMailTableColumns() : columnIds
      for (var i = 0; i < columnIds.length; i++) {
        var check = menu.querySelector('[data-mail-table-column-check="' + columnIds[i] + '"]')
        if (check) check.classList.toggle("opacity-0", selected.indexOf(columnIds[i]) === -1)
      }
    }

  }

  function scheduleAutoMarkRead(emailId, trigger) {
    if (autoMarkReadTimer) clearTimeout(autoMarkReadTimer)
    autoMarkReadTimer = null
    autoMarkReadEmailId = emailId

    var delay = window.GoferSettings ? GoferSettings.get("auto_mark_read_after") : null
    if (!delay) delay = "0"
    if (delay === "never") return

    var delayMs = parseInt(delay, 10)
    if (isNaN(delayMs) || delayMs < 0) delayMs = 0

    var run = function () {
      if (autoMarkReadEmailId !== emailId) return
      markRead(emailId, trigger)
    }

    if (delayMs === 0) {
      run()
    } else {
      autoMarkReadTimer = setTimeout(run, delayMs * 1000)
    }
  }

  function markRead(emailId, trigger) {
    fetch("/api/messages/" + emailId + "/read?state=read", { method: "POST" })
      .then(function (r) { return r.json() })
      .then(function (data) {
        if (!data.is_read) return
        var row = trigger && trigger.closest ? trigger.closest(".mail-list-item") : null
        if (row) {
          var link = row.querySelector("a")
          if (link) {
            link.classList.remove("font-semibold")
          }
        }
        invalidateMailListItem(emailId)
        refreshSidebarUnread()
      })
      .catch(function () {})
  }

  function textFrom(root, selector) {
    var el = root && root.querySelector(selector)
    return el ? el.textContent.trim() : ""
  }

  function escapeHTML(value) {
    return String(value || "").replace(/[&<>'"]/g, function (ch) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", "'": "&#39;", '"': "&quot;" }[ch]
    })
  }

  function getMailRowPreview(trigger) {
    var row = trigger && trigger.closest && trigger.closest(".mail-list-item")
    if (!row) return null
    var avatar = row.querySelector(".size-6")
    return {
      initials: avatar ? avatar.textContent.trim() : "",
      sender: textFrom(row, ".text-sm.truncate"),
      time: textFrom(row, ".tabular-nums"),
      subject: textFrom(row, "p.text-\\[13px\\]"),
      preview: textFrom(row, "p.text-xs"),
    }
  }

  function showMailViewLoading(trigger) {
    var mailView = document.getElementById("mail-view")
    if (!mailView) return
    var preview = getMailRowPreview(trigger) || {}
    var initials = escapeHTML(preview.initials || "")
    var sender = escapeHTML(preview.sender || "Loading message")
    var time = escapeHTML(preview.time || "")
    var subject = escapeHTML(preview.subject || "")
    var bodyHint = escapeHTML(preview.preview || "Fetching message body...")
    mailView.innerHTML =
      '<div class="flex flex-col h-full p-2">' +
        '<div class="surface-paper rounded-md flex flex-col h-full overflow-hidden">' +
          '<div class="flex items-center justify-between px-6 py-2.5">' +
            '<div class="flex items-center gap-1">' +
              '<div class="size-8 rounded-md flex items-center justify-center text-ink/45 bg-ink/[0.03] border border-ink/6">↩</div>' +
              '<div class="size-8 rounded-md flex items-center justify-center text-ink/45 bg-ink/[0.03] border border-ink/6">↪</div>' +
              '<div class="size-8 rounded-md flex items-center justify-center text-ink/45 bg-ink/[0.03] border border-ink/6">⌫</div>' +
              '<div class="size-8 rounded-md flex items-center justify-center text-ink/45 bg-ink/[0.03] border border-ink/6">⋯</div>' +
            '</div>' +
            '<div class="flex items-center gap-2">' +
              '<div class="text-xs text-ink/40">' + time + '</div>' +
              '<div class="size-8 rounded-md flex items-center justify-center text-ink/45 bg-ink/[0.03] border border-ink/6">◐</div>' +
            '</div>' +
          '</div>' +
          '<div class="h-px bg-gradient-to-r from-transparent via-amber-900/10 to-transparent"></div>' +
          '<div class="flex-1 overflow-y-auto">' +
            '<div class="max-w-3xl mx-auto px-8 py-6">' +
              '<div class="flex items-start gap-4">' +
                '<div class="size-11 rounded-full bg-gradient-to-b from-amber-700/70 to-amber-900/70 flex items-center justify-center text-sm font-bold text-amber-100 shrink-0 shadow-[0_2px_6px_rgba(0,0,0,0.2)]">' + initials + '</div>' +
                '<div class="flex-1 space-y-2">' +
                  '<div class="flex items-center gap-2">' +
                    '<div class="font-semibold text-ink">' + sender + '</div>' +
                    '<div class="text-xs text-ink/40">' + time + '</div>' +
                  '</div>' +
                  '<div class="text-xs text-ink/40">Preparing message...</div>' +
                '</div>' +
              '</div>' +
              '<h1 class="text-xl font-bold mt-5 tracking-tight text-ink" style="font-family: var(--font-serif)">' + subject + '</h1>' +
              '<div class="h-px bg-gradient-to-r from-transparent via-ink/10 to-transparent my-6"></div>' +
              '<p class="text-sm text-ink/45 mb-4">' + bodyHint + '</p>' +
              '<div class="space-y-3">' +
                '<div class="h-4 w-full rounded bg-ink/5 animate-pulse"></div>' +
                '<div class="h-4 w-5/6 rounded bg-ink/5 animate-pulse"></div>' +
                '<div class="h-4 w-4/5 rounded bg-ink/5 animate-pulse"></div>' +
              '</div>' +
            '</div>' +
          '</div>' +
          '<div class="px-6 py-3 border-t border-ink/6">' +
            '<div class="flex items-center gap-2">' +
              '<div class="flex-1 h-9 rounded-md border border-ink/8 bg-ink/[0.02] flex items-center justify-center text-[13px] text-ink/45">Reply</div>' +
              '<div class="flex-1 h-9 rounded-md border border-ink/8 bg-ink/[0.02] flex items-center justify-center text-[13px] text-ink/45">Reply All</div>' +
              '<div class="flex-1 h-9 rounded-md border border-ink/8 bg-ink/[0.02] flex items-center justify-center text-[13px] text-ink/45">Forward</div>' +
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
    virtualMailList = new VirtualMailList(scroll, { folderID: folderID, viewMode: scroll.dataset.viewMode || "cards" })
    virtualMailList.hydrateFromDOM()
    scroll._virtualMailList = virtualMailList
    flushPendingSyncEvents()
    applyActiveFolderSyncState()
    autoloadFirstEmail(scroll)
    bindThreadToggle(scroll)

    var selectedId = virtualMailList.selectedEmailId
    var path = "/folder/" + folderID
    if (selectedId) path += "/" + selectedId
    history.replaceState({ folder: folderID, email: selectedId || null }, "", path)
    if (typeof initResizeHandles === "function") initResizeHandles()
  })

  document.body.addEventListener("htmx:afterSwap", function (evt) {
    if (typeof window.applyMailTableColumnSettings !== "function") return
    if (!evt.target || !evt.target.querySelector) return

    var scroll = evt.target.id === "mail-list-scroll"
      ? evt.target
      : evt.target.querySelector("#mail-list-scroll")
    if (scroll) window.applyMailTableColumnSettings(scroll)
  })

  document.body.addEventListener("htmx:afterSettle", function () {
    if (typeof initResizeHandles === "function") initResizeHandles()
  })
})

var sendStatusTimer = null

function setMailViewEmpty() {
  var mailView = document.getElementById("mail-view")
  if (!mailView) return
  mailView.innerHTML =
    '<div class="flex flex-col items-center justify-center h-full text-center">' +
      '<div class="space-y-4 animate-fade-in">' +
        '<div class="size-20 rounded-2xl bg-card flex items-center justify-center mx-auto raised">' +
          '<svg class="size-9 text-muted-foreground/30" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m22 7-8.991 5.727a2 2 0 0 1-2.009 0L2 7"/><rect x="2" y="4" width="20" height="16" rx="2"/></svg>' +
        '</div>' +
        '<div>' +
          '<h3 class="font-semibold mb-1">Select an email</h3>' +
          '<p class="text-sm text-muted-foreground">Choose an email from the list to read it</p>' +
        '</div>' +
      '</div>' +
    '</div>'
}

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
var _activeComposeEditor = null

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
  _markComposeDirty(document.getElementById(prefix + "form"))
}

function resetComposeForm(fromPane) {
  var prefix = fromPane ? "compose-pane-" : "compose-"
  var form = document.getElementById(prefix + "form")
  if (!form) return
  var fields = form.querySelectorAll('input[name="to"], input[name="cc"], input[name="bcc"], input[name="subject"], input[name="draft_id"], input[name="in_reply_to"], input[name="references"], textarea[name="body"], textarea[name="html_body"]')
  for (var i = 0; i < fields.length; i++) fields[i].value = ""
  var editor = form.querySelector("[data-compose-editor]")
  if (editor) editor.innerHTML = ""
  var recipientFields = form.querySelectorAll("[data-compose-recipient-field]")
  for (var i = 0; i < recipientFields.length; i++) renderComposeRecipientField(recipientFields[i], "")
  form.dataset.composeDirty = "false"
  _setComposeDraftButtonState(form, "default")
}

function _composeRecipientEmail(value) {
  value = String(value || "").trim()
  var match = value.match(/<([^<>\s]+@[^<>\s]+)>/)
  return (match ? match[1] : value).replace(/^mailto:/i, "").trim().toLowerCase()
}

function _isComposeRecipientValid(value) {
  return /^[^\s@<>]+@[^\s@<>]+\.[^\s@<>]+$/.test(_composeRecipientEmail(value))
}

function _splitComposeRecipients(value) {
  return String(value || "")
    .split(/[;,\n]+/) 
    .map(function (part) { return part.trim() })
    .filter(Boolean)
}

function focusComposeRecipientField(field) {
  var input = field && field.querySelector ? field.querySelector("[data-compose-recipient-input]") : null
  if (input) input.focus()
}

function _composeRecipientValueInput(field) {
  var form = field && field.closest ? field.closest("#compose-form, #compose-pane-form") : null
  return form ? form.querySelector('input[name="' + field.dataset.recipientName + '"]') : null
}

function _composeRecipientValues(field) {
  var chips = field ? field.querySelectorAll("[data-compose-recipient-chip]") : []
  var values = []
  for (var i = 0; i < chips.length; i++) values.push(chips[i].dataset.value || chips[i].textContent.trim())
  return values
}

function _syncComposeRecipientField(field) {
  var hidden = _composeRecipientValueInput(field)
  if (hidden) hidden.value = _composeRecipientValues(field).join(", ")
}

function _makeComposeRecipientChip(value) {
  var chip = document.createElement("span")
  chip.dataset.composeRecipientChip = ""
  chip.dataset.value = value
  chip.className = "compose-recipient-chip"
  if (_isComposeRecipientValid(value)) {
    chip.dataset.valid = "true"
  } else {
    chip.dataset.valid = "false"
  }
  var label = document.createElement("span")
  label.className = "truncate"
  label.textContent = value
  var remove = document.createElement("button")
  remove.type = "button"
  remove.className = "compose-recipient-remove"
  remove.setAttribute("aria-label", "Remove recipient")
  remove.textContent = "x"
  remove.onclick = function () {
    var field = chip.closest("[data-compose-recipient-field]")
    removeComposeRecipientChip(chip)
  }
  chip.appendChild(label)
  chip.appendChild(remove)
  return chip
}

function removeComposeRecipientChip(chip) {
  if (!chip || chip.dataset.removing === "true") return
  var field = chip.closest("[data-compose-recipient-field]")
  chip.dataset.removing = "true"
  chip.classList.add("compose-recipient-chip-removing")
  setTimeout(function () {
    chip.remove()
    _syncComposeRecipientField(field)
    _markComposeDirty(_composeFormFrom(field))
  }, 140)
}

function renderComposeRecipientField(field, value) {
  if (!field) return
  var input = field.querySelector("[data-compose-recipient-input]")
  if (!input) return
  var existing = field.querySelectorAll("[data-compose-recipient-chip]")
  for (var i = 0; i < existing.length; i++) existing[i].remove()
  var seen = {}
  var tokens = _splitComposeRecipients(value)
  for (var t = 0; t < tokens.length; t++) {
    var email = _composeRecipientEmail(tokens[t])
    if (seen[email]) continue
    seen[email] = true
    field.insertBefore(_makeComposeRecipientChip(tokens[t]), input)
  }
  input.textContent = ""
  _syncComposeRecipientField(field)
}

function renderComposeRecipientFields(form) {
  if (!form) return
  var recipientFields = form.querySelectorAll("[data-compose-recipient-field]")
  for (var i = 0; i < recipientFields.length; i++) {
    var hidden = _composeRecipientValueInput(recipientFields[i])
    renderComposeRecipientField(recipientFields[i], hidden ? hidden.value : "")
  }
}

function finalizeComposeRecipientInput(input) {
  var field = input && input.closest ? input.closest("[data-compose-recipient-field]") : null
  if (!field) return
  var text = input.textContent || ""
  if (!text.trim()) return
  var merged = _composeRecipientValues(field).concat(_splitComposeRecipients(text)).join(", ")
  renderComposeRecipientField(field, merged)
  _markComposeDirty(_composeFormFrom(field))
}

function handleComposeRecipientKeydown(event) {
  var input = event.currentTarget
  var field = input.closest("[data-compose-recipient-field]")
  if (event.key === "Enter" || event.key === "Tab" || event.key === "," || event.key === ";") {
    if ((input.textContent || "").trim()) {
      event.preventDefault()
      finalizeComposeRecipientInput(input)
    }
    return
  }
  if (event.key === "Backspace" && !(input.textContent || "").trim()) {
    var chips = field.querySelectorAll("[data-compose-recipient-chip]")
    if (chips.length) {
      removeComposeRecipientChip(chips[chips.length - 1])
    }
  }
}

function handleComposeRecipientInput(input) {
  var text = input.textContent || ""
  if (/[;,\n]/.test(text)) finalizeComposeRecipientInput(input)
}

function finalizeComposeRecipients(form) {
  if (!form) return true
  var fields = form.querySelectorAll("[data-compose-recipient-field]")
  var valid = true
  for (var i = 0; i < fields.length; i++) {
    var input = fields[i].querySelector("[data-compose-recipient-input]")
    if (input) finalizeComposeRecipientInput(input)
    _syncComposeRecipientField(fields[i])
    var chips = fields[i].querySelectorAll("[data-compose-recipient-chip]")
    for (var c = 0; c < chips.length; c++) {
      if (!_isComposeRecipientValid(chips[c].dataset.value)) valid = false
    }
  }
  return valid
}

function _composeFormFrom(el) {
  if (el && el.closest) {
    var form = el.closest("#compose-form, #compose-pane-form")
    if (form) return form
  }
  if (_activeComposeEditor) return _activeComposeEditor.closest("#compose-form, #compose-pane-form")
  return document.querySelector("[data-compose-pane]") ? document.getElementById("compose-pane-form") : document.getElementById("compose-form")
}

function _composeEditorFrom(el) {
  var form = _composeFormFrom(el)
  return form ? form.querySelector("[data-compose-editor]") : _activeComposeEditor
}

function setActiveComposeEditor(editor) {
  _activeComposeEditor = editor
  _saveComposeSelection(editor)
  updateComposeToolbar(editor)
}

function _saveComposeSelection(editor) {
  if (!editor) return
  var selection = window.getSelection && window.getSelection()
  if (!selection || !selection.rangeCount) return
  var anchor = selection.anchorNode
  if (anchor && editor.contains(anchor)) {
    editor._composeRange = selection.getRangeAt(0).cloneRange()
  }
}

function _restoreComposeSelection(editor) {
  if (!editor || !editor._composeRange) return
  var selection = window.getSelection && window.getSelection()
  if (!selection) return
  selection.removeAllRanges()
  selection.addRange(editor._composeRange)
}

function _escapeComposeHTML(text) {
  return String(text || "")
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
}

function _composePlainToHTML(text) {
  var lines = String(text || "").replace(/\r\n/g, "\n").replace(/\r/g, "\n").split("\n")
  var html = ""
  for (var i = 0; i < lines.length; i++) {
    html += _escapeComposeHTML(lines[i])
    if (i < lines.length - 1) html += "<br>"
  }
  return html
}

function _sanitizeComposeHTML(html) {
  var template = document.createElement("template")
  template.innerHTML = html || ""
  var blocked = template.content.querySelectorAll("script, iframe, object, embed, form, meta, link")
  for (var i = 0; i < blocked.length; i++) blocked[i].remove()
  var allowed = { A: true, B: true, BLOCKQUOTE: true, BR: true, CODE: true, DIV: true, EM: true, I: true, LI: true, OL: true, P: true, PRE: true, S: true, SPAN: true, STRIKE: true, STRONG: true, U: true, UL: true }
  var walker = document.createTreeWalker(template.content, NodeFilter.SHOW_ELEMENT)
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
      if (name.indexOf("on") === 0 || name === "style" || name === "class") {
        node.removeAttribute(attr.name)
        continue
      }
      if (tag !== "A" || (name !== "href" && name !== "target" && name !== "rel")) {
        node.removeAttribute(attr.name)
      }
    }
    if (tag === "A") {
      var href = node.getAttribute("href") || ""
      if (!/^(https?:|mailto:|#)/i.test(href)) node.removeAttribute("href")
      node.setAttribute("rel", "noopener noreferrer")
      if (href && href.charAt(0) !== "#") node.setAttribute("target", "_blank")
    }
  }
  return template.innerHTML
}

function _composeEditorText(editor) {
  if (!editor) return ""
  return (editor.innerText || "").replace(/\u00a0/g, " ").replace(/\n{3,}/g, "\n\n").trim()
}

function syncComposeEditor(editor) {
  if (!editor) return
  var form = _composeFormFrom(editor)
  if (!form) return
  var plain = form.querySelector('textarea[name="body"]')
  var html = form.querySelector('textarea[name="html_body"]')
  if (plain) plain.value = _composeEditorText(editor)
  if (html) html.value = _sanitizeComposeHTML(editor.innerHTML).trim()
}

function composeChanged(editor) {
  syncComposeEditor(editor)
  _markComposeDirty(_composeFormFrom(editor))
}

function _syncComposeFormEditor(form) {
  if (!form) return
  var editor = form.querySelector("[data-compose-editor]")
  if (editor) syncComposeEditor(editor)
}

function _setComposeEditorValue(form, plain, html) {
  if (!form) return
  var editor = form.querySelector("[data-compose-editor]")
  var plainField = form.querySelector('textarea[name="body"]')
  var htmlField = form.querySelector('textarea[name="html_body"]')
  if (plainField) plainField.value = plain || ""
  if (htmlField) htmlField.value = html || ""
  if (editor) {
    editor.innerHTML = html ? _sanitizeComposeHTML(html) : _composePlainToHTML(plain || "")
  }
}

function composeExec(el, command, value) {
  var editor = _composeEditorFrom(el)
  if (!editor) return
  editor.focus()
  _restoreComposeSelection(editor)
  document.execCommand(command, false, value || null)
  syncComposeEditor(editor)
  updateComposeToolbar(editor)
}

function composeCreateLink(el) {
  var editor = _composeEditorFrom(el)
  if (!editor) return
  editor.focus()
  _restoreComposeSelection(editor)
  var url = window.prompt("Paste a URL or email address")
  if (!url) return
  if (url.indexOf("@") > 0 && !/^[a-z][a-z0-9+.-]*:/i.test(url)) url = "mailto:" + url
  if (!/^(https?:|mailto:)/i.test(url)) url = "https://" + url
  document.execCommand("createLink", false, url)
  syncComposeEditor(editor)
  updateComposeToolbar(editor)
}

function updateComposeToolbar(editor) {
  var form = _composeFormFrom(editor)
  if (!form) return
  _saveComposeSelection(editor)
  var buttons = form.querySelectorAll("[data-compose-command]")
  for (var i = 0; i < buttons.length; i++) {
    var command = buttons[i].dataset.composeCommand
    var active = false
    try { active = document.queryCommandState(command) } catch (e) {}
    buttons[i].classList.toggle("bg-accent", active)
    buttons[i].classList.toggle("text-foreground", active)
  }
}

function handleComposePaste(event) {
  var editor = event.currentTarget
  var clipboard = event.clipboardData || window.clipboardData
  if (!clipboard) return
  event.preventDefault()
  var html = clipboard.getData("text/html")
  var text = clipboard.getData("text/plain")
  document.execCommand("insertHTML", false, html ? _sanitizeComposeHTML(html) : _composePlainToHTML(text))
  syncComposeEditor(editor)
}

document.addEventListener("keydown", function (event) {
  var editor = event.target && event.target.closest ? event.target.closest("[data-compose-editor]") : null
  if (!editor || (!event.ctrlKey && !event.metaKey)) return
  var key = event.key.toLowerCase()
  if (key === "b") {
    event.preventDefault()
    composeExec(editor, "bold")
  } else if (key === "i") {
    event.preventDefault()
    composeExec(editor, "italic")
  } else if (key === "u") {
    event.preventDefault()
    composeExec(editor, "underline")
  } else if (key === "k") {
    event.preventDefault()
    composeCreateLink(editor)
  }
})

document.addEventListener("selectionchange", function () {
  if (_activeComposeEditor) _saveComposeSelection(_activeComposeEditor)
})

document.addEventListener("mousedown", function (event) {
  if (event.target && event.target.closest && event.target.closest("[data-compose-toolbar] button")) {
    event.preventDefault()
  }
})

function composeUnavailable(message) {
  showSendStatus("failed", message)
}

function _composeRootForForm(form) {
  if (!form) return
  return form.id === "compose-pane-form" ? form.closest("[data-compose-pane]") : document.getElementById("compose-dialog")
}

function _composeDraftButton(form) {
  var root = _composeRootForForm(form)
  return root ? root.querySelector("[data-compose-draft-button]") : null
}

function _setComposeDraftButtonState(form, state) {
  var button = _composeDraftButton(form)
  if (!button) return
  var label = button.querySelector("[data-compose-draft-label]")
  clearTimeout(button._composeDraftResetTimer)
  button.dataset.composeDraftState = state || "default"
  button.disabled = state === "saving"

  if (label) {
    if (state === "saving") label.textContent = "Saving..."
    else if (state === "saved") label.textContent = "Saved"
    else if (state === "failed") label.textContent = "Save failed"
    else if (state === "empty") label.textContent = "Nothing to save"
    else label.textContent = "Save Draft"
  }

  if (state === "saved" || state === "failed" || state === "empty") {
    button._composeDraftResetTimer = setTimeout(function () {
      _setComposeDraftButtonState(form, "default")
    }, state === "saved" ? 1400 : 1900)
  }
}

function _composeHasDraftContent(form) {
  if (!form) return false
  _syncComposeFormEditor(form)
  var recipientInputs = form.querySelectorAll("[data-compose-recipient-input]")
  for (var r = 0; r < recipientInputs.length; r++) {
    if ((recipientInputs[r].textContent || "").trim()) return true
  }
  var names = ["to", "cc", "bcc", "subject", "body", "html_body"]
  for (var i = 0; i < names.length; i++) {
    var field = form.querySelector('[name="' + names[i] + '"]')
    if (field && field.value && field.value.trim()) return true
  }
  return false
}

function _markComposeDirty(form) {
  if (!form) return
  form.dataset.composeDirty = "true"
  var button = _composeDraftButton(form)
  if (button && button.dataset.composeDraftState !== "saving") {
    _setComposeDraftButtonState(form, "default")
  }
}

function saveComposeDraft(fromPane, auto) {
  var form = document.getElementById(fromPane ? "compose-pane-form" : "compose-form")
  finalizeComposeRecipients(form)
  if (!form || !_composeHasDraftContent(form)) {
    if (!auto) _setComposeDraftButtonState(form, "empty")
    return Promise.resolve(false)
  }
  _setComposeDraftButtonState(form, "saving")

  var params = new URLSearchParams()
  var inputs = form.querySelectorAll("input, textarea")
  for (var i = 0; inputs && i < inputs.length; i++) {
    if (inputs[i].name) params.append(inputs[i].name, inputs[i].value)
  }

  return fetch("/compose/draft", {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: params.toString()
  }).then(function (r) {
    if (!r.ok) {
      return r.json().catch(function () { return {} }).then(function (data) {
        throw new Error(data.error || "Failed to save draft")
      })
    }
    return r.json()
  }).then(function (data) {
    var draftField = form.querySelector('input[name="draft_id"]')
    if (draftField && data.draft_id) draftField.value = data.draft_id
    form.dataset.composeDirty = "false"
    _setComposeDraftButtonState(form, "saved")
    return true
  }).catch(function (err) {
    form.dataset.composeDirty = "true"
    _setComposeDraftButtonState(form, "failed")
    if (!auto) showSendStatus("failed", err && err.message ? err.message : "Failed to save draft")
    return false
  })
}

function saveActiveComposeDraft(auto) {
  var form = _composeFormFrom(_activeComposeEditor)
  saveComposeDraft(form && form.id === "compose-pane-form", !!auto)
}

function _composeValsFromDraft(draft) {
  return {
    account_id: draft.account_id || "",
    draft_id: draft.draft_id || "",
    to: draft.to || "",
    cc: draft.cc || "",
    bcc: draft.bcc || "",
    subject: draft.subject || "",
    body: draft.body || "",
    html_body: draft.html_body || "",
    in_reply_to: draft.in_reply_to || "",
    references: draft.references || "",
    _ccVisible: !!(draft.cc && draft.cc.trim()),
    _bccVisible: !!(draft.bcc && draft.bcc.trim()),
    _composeDirty: "false"
  }
}

function _showComposeOptionalFields(form, vals) {
  if (!form || !vals) return
  var pane = form.id === "compose-pane-form"
  var ccField = document.getElementById(pane ? "pane-cc-field" : "cc-field")
  var ccBtn = document.getElementById(pane ? "pane-cc-btn" : "cc-btn")
  var bccField = document.getElementById(pane ? "pane-bcc-field" : "bcc-field")
  var bccBtn = document.getElementById(pane ? "pane-bcc-btn" : "bcc-btn")
  if (ccField) ccField.classList.toggle("hidden", !vals._ccVisible)
  if (ccBtn) ccBtn.classList.toggle("hidden", !!vals._ccVisible)
  if (bccField) bccField.classList.toggle("hidden", !vals._bccVisible)
  if (bccBtn) bccBtn.classList.toggle("hidden", !!vals._bccVisible)
  renderComposeRecipientFields(form)
}

function _activeComposeCanBeReplaced() {
  var form = document.querySelector("[data-compose-pane] #compose-pane-form") || document.getElementById("compose-form")
  if (!form || form.dataset.composeDirty !== "true" || !_composeHasDraftContent(form)) return true
  return window.confirm("Replace the current unsaved draft?")
}

function continueEditingDraft(emailId) {
  if (!_activeComposeCanBeReplaced()) return
  fetch("/api/drafts/" + encodeURIComponent(emailId))
    .then(function (r) {
      if (!r.ok) throw new Error("Failed to load draft")
      return r.json()
    })
    .then(function (draft) {
      var vals = _composeValsFromDraft(draft)
      var view = window.GoferSettings ? GoferSettings.get("default_compose_view") : null
      var fullWidth = view === "full"

      if (view === "pane" || view === "full") {
        if (document.getElementById("mail-list") && document.getElementById("mail-view")) {
          fetch("/compose/pane").then(function (r) { return r.text() }).then(function (html) {
            writeComposePane(html, vals, fullWidth, fullWidth)
          })
        } else {
          var dialogForm = document.getElementById("compose-form")
          _writeComposeFormValues(dialogForm, vals, "compose-")
          _showComposeOptionalFields(dialogForm, vals)
          openComposeInMain(fullWidth, fullWidth)
        }
        return
      }

      var form = document.getElementById("compose-form")
      _writeComposeFormValues(form, vals, "compose-")
      _showComposeOptionalFields(form, vals)
      if (window.tui && window.tui.dialog) window.tui.dialog.open("compose-dialog")
    })
    .catch(function (err) {
      showSendStatus("failed", err && err.message ? err.message : "Failed to load draft")
    })
}

function discardReadPaneDraft(emailId) {
  if (!window.confirm("Discard this draft?")) return
  fetch("/api/drafts/" + encodeURIComponent(emailId), { method: "DELETE" })
    .then(function (r) {
      if (!r.ok) throw new Error("Failed to discard draft")
      setMailViewEmpty()
      refreshSidebarUnread()
    })
    .catch(function (err) {
      showSendStatus("failed", err && err.message ? err.message : "Failed to discard draft")
    })
}

function _deleteComposeDraft(form) {
  if (!form) return Promise.resolve(false)
  var draftField = form.querySelector('input[name="draft_id"]')
  var accountField = form.querySelector('input[name="account_id"]')
  if (!draftField || !draftField.value) return Promise.resolve(false)
  var params = new URLSearchParams()
  params.append("draft_id", draftField.value)
  if (accountField) params.append("account_id", accountField.value)
  draftField.value = ""
  form.dataset.composeDirty = "false"
  _setComposeDraftButtonState(form, "default")
  return fetch("/compose/draft/discard", {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: params.toString()
  }).catch(function () { return false })
}

function _confirmComposeDiscard(form) {
  if (!form || !_composeHasDraftContent(form)) return true
  var draftField = form.querySelector('input[name="draft_id"]')
  var hasSavedDraft = !!(draftField && draftField.value)
  if (form.dataset.composeDirty === "true") return window.confirm("Discard this unsaved draft?")
  if (hasSavedDraft) return window.confirm("Discard this saved draft?")
  return window.confirm("Discard this message?")
}

function discardComposeDialog() {
  var form = document.getElementById("compose-form")
  if (!_confirmComposeDiscard(form)) return
  _deleteComposeDraft(form)
  resetComposeForm(false)
  if (window.tui && window.tui.dialog) {
    window.tui.dialog.close("compose-dialog")
  }
  _updateComposeBtn(false)
}

document.addEventListener("input", function (event) {
  var form = event.target && event.target.closest ? event.target.closest("#compose-form, #compose-pane-form") : null
  if (!form || event.target.matches("[data-compose-editor]")) return
  _markComposeDirty(form)
})

window.addEventListener("beforeunload", function (event) {
  var forms = [document.getElementById("compose-form"), document.getElementById("compose-pane-form")]
  for (var i = 0; i < forms.length; i++) {
    if (forms[i] && forms[i].dataset.composeDirty === "true" && _composeHasDraftContent(forms[i])) {
      event.preventDefault()
      event.returnValue = ""
      return ""
    }
  }
})

function sendCompose(fromPane) {
  var formId = fromPane ? "compose-pane-form" : "compose-form"
  var form = document.getElementById(formId)
  if (!form) return
  if (!finalizeComposeRecipients(form)) {
    showSendStatus("failed", "Fix invalid recipient addresses before sending")
    return
  }
  _syncComposeFormEditor(form)

  var toField = form.querySelector('input[name="to"]')
  if (!toField || !toField.value.trim()) {
    return
  }

  var params = new URLSearchParams()
  var inputs = form.querySelectorAll("input, textarea")
  for (var i = 0; inputs && i < inputs.length; i++) {
    if (inputs[i].name) params.append(inputs[i].name, inputs[i].value)
  }

  showSendStatus("sending", "Sending...")

  fetch("/compose", {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: params.toString()
  }).then(function (r) {
    if (!r.ok) {
      return r.json().catch(function () { return {} }).then(function (data) {
        throw new Error(data.error || "Failed to send message")
      })
    }
    if (fromPane) {
      setMailViewEmpty()
      _updateComposeBtn(false)
    } else if (window.tui && window.tui.dialog) {
      window.tui.dialog.close("compose-dialog")
      _updateComposeBtn(false)
    }
  }).catch(function (err) {
    showSendStatus("failed", err && err.message ? err.message : "Failed to connect to server")
  })
}

function handleReply(el, mode) {
  var bar = el && el.closest ? el.closest("[data-thread-reply-data]") : null
  if (!bar) bar = document.getElementById("reply-bar")
  if (!bar) return

  var messageId = bar.dataset.messageId
  var references = bar.dataset.references || ""
  var subject = bar.dataset.subject || ""
  var fromEmail = bar.dataset.fromEmail || ""
  var fromName = bar.dataset.fromName || ""
  var accountId = bar.dataset.accountId || ""
  var date = bar.dataset.date || ""
  var to = bar.dataset.to || ""
  var cc = bar.dataset.cc || ""
  var body = bar.dataset.body || ""

  var inPane = !!document.querySelector("[data-compose-pane]")
  var formId = inPane ? "compose-pane-form" : "compose-form"
  var form = document.getElementById(formId)
  if (!form) return

  var toField = form.querySelector('input[name="to"]')
  var ccField = form.querySelector('input[name="cc"]')
  var subjectField = form.querySelector('input[name="subject"]')
  var prefix = inPane ? "compose-pane-" : "compose-"
  var accountIdField = document.getElementById(prefix + "account-id")
  var inReplyToField = document.getElementById(prefix + "in-reply-to")
  var referencesField = document.getElementById(prefix + "references")
  var bodyField = form.querySelector('[data-compose-editor]')

  if (accountId && accountIdField) {
    accountIdField.value = accountId
  }

  var fromLine = fromName ? fromName + " <" + fromEmail + ">" : fromEmail

  if (mode === "reply" || mode === "reply-all") {
    if (toField) toField.value = fromLine
    if (subjectField) {
      subjectField.value = subject.match(/^Re:/i) ? subject : "Re: " + subject
    }
    if (inReplyToField && messageId) {
      inReplyToField.value = messageId.charAt(0) === "<" ? messageId : "<" + messageId + ">"
    }
    if (referencesField && messageId) {
      var parentMessageId = messageId.charAt(0) === "<" ? messageId : "<" + messageId + ">"
      referencesField.value = references ? references + " " + parentMessageId : parentMessageId
    }
    if (bodyField) {
      var quotedBody = body.split("\n").map(function(line) { return "> " + line }).join("\n")
      var header = date ? "On " + date + ", " + fromLine + " wrote:" : fromLine + " wrote:"
      _setComposeEditorValue(form, "\n\n" + header + "\n" + quotedBody, "")
      bodyField.focus()
      var range = document.createRange()
      range.setStart(bodyField, 0)
      range.collapse(true)
      var selection = window.getSelection()
      selection.removeAllRanges()
      selection.addRange(range)
    }
  } else if (mode === "forward") {
    if (toField) toField.value = ""
    if (ccField) ccField.value = ""
    if (subjectField) {
      subjectField.value = subject.match(/^Fwd:/i) ? subject : "Fwd: " + subject
    }
    if (inReplyToField) inReplyToField.value = ""
    if (referencesField) referencesField.value = ""
    if (bodyField) {
      var fwdHeader = "\n\n---------- Forwarded message ----------"
      if (fromLine) fwdHeader += "\nFrom: " + fromLine
      if (date) fwdHeader += "\nDate: " + date
      if (subject) fwdHeader += "\nSubject: " + subject
      if (to) fwdHeader += "\nTo: " + to
      if (cc) fwdHeader += "\nCc: " + cc
      _setComposeEditorValue(form, fwdHeader + "\n\n" + body, "")
    }
  }

  renderComposeRecipientFields(form)

  if (inPane) return

  var dialog = document.querySelector('#compose-dialog dialog[data-tui-dialog-content]')
  if (dialog && window.tui && window.tui.dialog) {
    window.tui.dialog.open('compose-dialog')
  }

  setTimeout(function () {
    if (mode === "forward" && toField) toField.focus()
  }, 100)
}

function openNewCompose() {
  resetComposeForm(false)
  var view = window.GoferSettings ? GoferSettings.get("default_compose_view") : null
  if (view === "pane" || view === "full") {
    openComposeInMain(view === "full", view === "full")
    return
  }
  if (window.tui && window.tui.dialog) {
    window.tui.dialog.open("compose-dialog")
  }
}

function openComposeInMain(fullWidth, instantFullWidth) {
  if (document.getElementById("mail-list") && document.getElementById("mail-view")) {
    expandToPane(fullWidth, instantFullWidth)
    return
  }

  if (typeof htmx === "undefined") {
    window.location.href = "/"
    return
  }

  var vals = _readComposeFormValues(document.getElementById("compose-form"))
  var paneHTML = null
  var mainReady = false

  function openWhenReady() {
    if (!mainReady || paneHTML === null) return
    writeComposePane(paneHTML, vals, fullWidth, instantFullWidth)
  }

  function afterMainContentSettle(evt) {
    if (!evt.target || evt.target.id !== "main-content") return
    document.body.removeEventListener("htmx:afterSettle", afterMainContentSettle)
    mainReady = true
    openWhenReady()
  }

  document.body.addEventListener("htmx:afterSettle", afterMainContentSettle)
  fetch("/compose/pane").then(function (r) { return r.text() }).then(function (html) {
    paneHTML = html
    openWhenReady()
  }).catch(function () {})
  htmx.ajax("GET", "/folder/inbox/full", { target: "#main-content", swap: "outerHTML" })
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

function toggleThreadRead(emailId) {
  fetch("/api/messages/" + emailId + "/thread/read", { method: "POST" })
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
      if (mailView) setMailViewEmpty()
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

function archiveThread(emailId) {
  fetch("/api/messages/" + emailId + "/thread/archive", { method: "POST" })
    .then(function () {
      var mailView = document.getElementById("mail-view")
      if (mailView) setMailViewEmpty()
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

function deleteThread(emailId) {
  fetch("/api/messages/" + emailId + "/thread", { method: "DELETE" })
    .then(function () {
      var mailView = document.getElementById("mail-view")
      if (mailView) setMailViewEmpty()
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
  if (!e.data || !e.data.type) return
  if (e.data.type === "emailBodyResize") {
    var iframe = e.data.emailId ? document.querySelector('[data-email-body-frame][data-email-id="' + e.data.emailId + '"]') : document.getElementById("email-body-frame")
    if (iframe) {
      iframe.style.height = e.data.height + "px"
      iframe.classList.remove("opacity-0")
      var loader = e.data.emailId ? document.querySelector('[data-email-body-loading="' + e.data.emailId + '"]') : null
      if (loader) loader.remove()
    }
  }
  if (e.data.type === "remoteContentBlocked" && e.data.emailId) {
    var banner = document.querySelector('[data-remote-content-banner="' + e.data.emailId + '"]')
    if (banner) banner.classList.remove("hidden")
  }
})

function applyEmailBodyTheme(targetFrame) {
  if (!targetFrame) {
    var frames = document.querySelectorAll("[data-email-body-frame]")
    if (!frames.length) {
      var single = document.getElementById("email-body-frame")
      if (single) frames = [single]
    }
    for (var i = 0; i < frames.length; i++) applyEmailBodyTheme(frames[i])
    return
  }
  var iframe = targetFrame
  if (!iframe || !iframe.dataset.emailId) return
  iframe.classList.add("opacity-0")
  var loader = document.querySelector('[data-email-body-loading="' + iframe.dataset.emailId + '"]')
  if (loader) loader.classList.remove("hidden")
  var baseTheme = getEmailBodyBaseTheme()
  var bodyMode = iframe.dataset.bodyMode || (iframe.dataset.forceScheme === "opposite" ? oppositeEmailBodyTheme(baseTheme) : baseTheme)
  var original = bodyMode === "original"
  var theme = bodyMode === "dark" || bodyMode === "light" ? bodyMode : baseTheme
  var palette = readEmailBodyPalette(theme)
  var bg = palette.bg
  var fg = palette.fg
  var link = palette.link
  if (original) {
    iframe.style.backgroundColor = ""
  } else if (bg) {
    iframe.style.backgroundColor = bg
  }
  var params = new URLSearchParams()
  params.set("theme", theme)
  if (original) params.set("mode", "original")
  if (!original && bg) params.set("bg", bg)
  if (!original && fg) params.set("fg", fg)
  if (!original && link) params.set("link", link)
  if (iframe.dataset.remoteLoaded === "true") params.set("remote", "true")
  iframe.src = "/email/" + iframe.dataset.emailId + "/body?" + params.toString()
  updateEmailBodySchemeButton(iframe, baseTheme, theme, bodyMode)
}

function loadRemoteContent(emailId) {
  var iframe = document.querySelector('[data-email-body-frame][data-email-id="' + emailId + '"]')
  if (!iframe) return
  var src = iframe.src
  if (!src) return
  var url = new URL(src, window.location.origin)
  url.searchParams.set("remote", "true")
  iframe.src = url.toString()
  var banner = document.querySelector('[data-remote-content-banner="' + emailId + '"]')
  if (banner) banner.remove()
  iframe.dataset.remoteLoaded = "true"
}

function allowRemoteContent(emailId, mode) {
  var iframe = document.querySelector('[data-email-body-frame][data-email-id="' + emailId + '"]')
  var banner = document.querySelector('[data-remote-content-banner="' + emailId + '"]')

  fetch("/api/remote-content/" + emailId + "/allow", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ mode: mode }),
  })
    .then(function (r) { return r.json() })
    .then(function () {
      if (banner) banner.remove()
      if (iframe) iframe.dataset.remoteLoaded = "true"
      if (iframe && iframe.src) {
        var url = new URL(iframe.src, window.location.origin)
        url.searchParams.set("remote", "true")
        iframe.src = url.toString()
      }
    })
    .catch(function () {})
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
  var frames = document.querySelectorAll("[data-email-body-frame]")
  if (!frames.length) {
    var single = document.getElementById("email-body-frame")
    if (single) frames = [single]
  }
  for (var i = 0; i < frames.length; i++) {
    advanceEmailBodyMode(frames[i])
  }
  applyEmailBodyTheme()
}

function setEmailBodyMode(mode) {
  var frames = document.querySelectorAll("[data-email-body-frame]")
  if (!frames.length) {
    var single = document.getElementById("email-body-frame")
    if (single) frames = [single]
  }
  for (var i = 0; i < frames.length; i++) setEmailBodyModeOnFrame(frames[i], mode)
  applyEmailBodyTheme()
}

function setEmailBodyModeById(emailId, mode) {
  var frame = document.querySelector('[data-email-body-frame][data-email-id="' + emailId + '"]')
  if (!frame) return
  setEmailBodyModeOnFrame(frame, mode)
  applyEmailBodyTheme(frame)
}

function setEmailBodyModeOnFrame(frame, mode) {
  if (!frame) return
  delete frame.dataset.forceScheme
  frame.dataset.bodyMode = mode === "dark" || mode === "light" || mode === "original" ? mode : getEmailBodyBaseTheme()
}

function advanceEmailBodyMode(frame) {
  var baseTheme = getEmailBodyBaseTheme()
  var mode = frame.dataset.bodyMode || (frame.dataset.forceScheme === "opposite" ? oppositeEmailBodyTheme(baseTheme) : baseTheme)
  delete frame.dataset.forceScheme
  if (mode === "dark") {
    frame.dataset.bodyMode = "light"
  } else if (mode === "light") {
    frame.dataset.bodyMode = "original"
  } else {
    frame.dataset.bodyMode = "dark"
  }
}

function toggleEmailBodySchemeById(emailId) {
  var frame = document.querySelector('[data-email-body-frame][data-email-id="' + emailId + '"]')
  if (!frame) return
  advanceEmailBodyMode(frame)
  applyEmailBodyTheme(frame)
}

function updateEmailBodySchemeButton(iframe, baseTheme, theme, bodyMode) {
  if (!iframe) return
  var emailId = iframe.dataset.emailId
  var btn = emailId ? document.querySelector('[data-force-email-scheme="' + emailId + '"]') : document.querySelector("[data-force-email-scheme]")
  if (!btn) return
  var mode = bodyMode || iframe.dataset.bodyMode || (iframe.dataset.forceScheme === "opposite" ? oppositeEmailBodyTheme(baseTheme) : baseTheme)
  if (mode !== "dark" && mode !== "light" && mode !== "original") mode = baseTheme
  var label = "Showing " + theme + " email body."
  if (mode === "original") label = "Showing original email style."
  btn.setAttribute("aria-label", label)
  updateEmailBodyModeToggle(emailId, mode, label)
  var tooltipEl = btn.closest("[data-tui-popover-root]")
  if (tooltipEl) {
    var tipText = tooltipEl.querySelector("[data-email-scheme-tooltip]")
    if (tipText) tipText.textContent = label
  }
}

function updateEmailBodyModeToggle(emailId, mode, label) {
  var toggles = document.querySelectorAll('[data-email-body-style-toggle="' + emailId + '"]')
  for (var i = 0; i < toggles.length; i++) {
    var toggle = toggles[i]
    var tabsId = toggle.getAttribute("data-tui-tabs-id")
    if (tabsId && window.tui && window.tui.tabs && typeof window.tui.tabs.setActive === "function") {
      window.tui.tabs.setActive(tabsId, mode, true)
    }
    if (!label) continue
    var activeButton = toggle.querySelector('[data-email-body-mode-button="' + mode + '"]')
    if (activeButton) activeButton.setAttribute("aria-label", label)
  }
}

document.addEventListener("DOMContentLoaded", function () {
  applyEmailBodyTheme()
})

new MutationObserver(function () {
  var frames = document.querySelectorAll("[data-email-body-frame]")
  for (var i = 0; i < frames.length; i++) {
    var iframe = frames[i]
    if (iframe && iframe.dataset.emailId && !iframe.src) {
      applyEmailBodyTheme(iframe)
    }
  }
  var legacy = document.getElementById("email-body-frame")
  if (legacy && legacy.dataset.emailId && !legacy.src) {
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

  document.addEventListener("click", function (e) {
    var el = e.target.closest("[data-load-remote]")
    if (el) {
      e.preventDefault()
      loadRemoteContent(el.dataset.loadRemote)
    }
  })

  document.addEventListener("click", function (e) {
    var el = e.target.closest("[data-allow-remote]")
    if (el) {
      e.preventDefault()
      allowRemoteContent(el.dataset.allowRemote, el.dataset.allowMode)
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
  finalizeComposeRecipients(form)
  _syncComposeFormEditor(form)
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
  vals._composeDirty = form.dataset.composeDirty || "false"
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
  renderComposeRecipientFields(form)
  if (vals._fromDisplay) {
    var display = document.getElementById(prefix + "from-display")
    if (display) display.innerHTML = vals._fromDisplay
  }
  _setComposeEditorValue(form, vals.body || "", vals.html_body || "")
  form.dataset.composeDirty = vals._composeDirty || "false"
  _setComposeDraftButtonState(form, "default")
}

function expandToPane(fullWidth, instantFullWidth) {
  var dialogForm = document.getElementById("compose-form")
  var vals = _readComposeFormValues(dialogForm)

  if (window.tui && window.tui.dialog) {
    window.tui.dialog.close("compose-dialog")
  }

  _composeActive = true
  _updateComposeBtn(true)

  fetch("/compose/pane").then(function (r) { return r.text() }).then(function (html) {
    writeComposePane(html, vals, fullWidth, instantFullWidth)
  }).catch(function () {})
}

function writeComposePane(html, vals, fullWidth, instantFullWidth) {
  var mailView = document.getElementById("mail-view")
  if (!mailView) return

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

  var bodyField = paneForm && paneForm.querySelector('[data-compose-editor]')
  if (bodyField) bodyField.focus()

  if (fullWidth) {
    if (instantFullWidth) {
      applyComposeFullWidthInstant()
    } else {
      expandComposeFullWidth()
    }
  }
}

function collapseToDialog() {
  collapseComposeFullWidth()

  var paneForm = document.getElementById("compose-pane-form")
  var vals = _readComposeFormValues(paneForm)

  var mailView = document.getElementById("mail-view")
  if (mailView) setMailViewEmpty()

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
  var paneForm = document.getElementById("compose-pane-form")
  if (!_confirmComposeDiscard(paneForm)) return
  _deleteComposeDraft(paneForm)
  collapseComposeFullWidth()
  var mailView = document.getElementById("mail-view")
  if (mailView) setMailViewEmpty()
  _updateComposeBtn(false)
}

function applyComposeFullWidthInstant() {
  var mailList = document.querySelector("#main-content > #mail-list")
  var resizeHandles = document.querySelectorAll('[data-panel="maillist"]')
  if (!mailList || mailList._savedWidth !== undefined) return

  mailList._savedWidth = mailList.style.width
  mailList.style.display = "none"
  mailList.style.width = "0px"
  mailList.style.opacity = "0"
  mailList.style.overflow = "hidden"
  mailList.style.borderWidth = "0"

  for (var i = 0; i < resizeHandles.length; i++) {
    resizeHandles[i]._savedDisplay = resizeHandles[i].style.display
    resizeHandles[i].style.display = "none"
    resizeHandles[i].style.opacity = "0"
  }

  var normal = document.getElementById("pane-btns-normal")
  var full = document.getElementById("pane-btns-full")
  if (normal) normal.style.display = "none"
  if (full) full.style.display = "flex"

  var bodyField = document.querySelector("#compose-pane-form [data-compose-editor]")
  if (bodyField) bodyField.focus()
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

  var bodyField = document.querySelector("#compose-pane-form [data-compose-editor]")
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

(function () {
  var DURATION = '0.2s'
  var EASING = 'cubic-bezier(0.4,0,0.2,1)'
  var FADE = '0.15s'

  function clearStyles(ct) {
    ct.style.height = ''
    ct.style.overflow = ''
    ct.style.transition = ''
    ct.style.opacity = ''
    ct.style.willChange = ''
  }

  function fadeIframes(ct, show) {
    var iframes = ct.querySelectorAll('iframe')
    for (var j = 0; j < iframes.length; j++) {
      if (show) {
        iframes[j].style.visibility = ''
        iframes[j].style.opacity = '0'
        iframes[j].style.transition = 'opacity ' + FADE + ' ease-out'
        void iframes[j].offsetHeight
        iframes[j].style.opacity = '1'
      } else {
        iframes[j].style.opacity = '1'
        iframes[j].style.transition = 'opacity ' + FADE + ' ease-out'
        void iframes[j].offsetHeight
        iframes[j].style.opacity = '0'
      }
      ;(function (iframe) {
        function done() {
          iframe.removeEventListener('transitionend', done)
          iframe.style.transition = ''
          iframe.style.opacity = ''
          if (!show) iframe.style.visibility = 'hidden'
        }
        iframe.addEventListener('transitionend', done)
      })(iframes[j])
    }
  }

  function collapseDetails(el) {
    var ct = el.querySelector('.thread-details-content')
    if (!ct || el._threadAnimating) return
    el._threadAnimating = true
    ct.style.willChange = 'height, opacity'

    var h = ct.scrollHeight
    ct.style.height = h + 'px'
    ct.style.overflow = 'hidden'
    ct.style.transition = 'none'
    void ct.offsetHeight

    requestAnimationFrame(function () {
      requestAnimationFrame(function () {
        fadeIframes(ct, false)
        ct.style.transition = 'height ' + DURATION + ' ' + EASING + ', opacity ' + DURATION + ' ease-out'
        ct.style.height = '0px'
        ct.style.opacity = '0'

        function onEnd(ev) {
          if (ev.propertyName !== 'height') return
          ct.removeEventListener('transitionend', onEnd)
          el.open = false
          clearStyles(ct)
          el._threadAnimating = false
        }
        ct.addEventListener('transitionend', onEnd)
      })
    })
  }

  function expandDetails(el) {
    var ct = el.querySelector('.thread-details-content')
    if (!ct || el._threadAnimating) return
    el._threadAnimating = true
    ct.style.willChange = 'height, opacity'

    el.open = true
    ct.style.height = '0px'
    ct.style.overflow = 'hidden'
    ct.style.opacity = '0'
    ct.style.transition = 'none'
    void ct.offsetHeight

    requestAnimationFrame(function () {
      requestAnimationFrame(function () {
        ct.style.transition = 'height ' + DURATION + ' ' + EASING + ', opacity ' + DURATION + ' ease-out'
        ct.style.height = ct.scrollHeight + 'px'
        ct.style.opacity = '1'

        function onEnd(ev) {
          if (ev.propertyName !== 'height') return
          ct.removeEventListener('transitionend', onEnd)
          clearStyles(ct)
          fadeIframes(ct, true)
          el._threadAnimating = false
        }
        ct.addEventListener('transitionend', onEnd)
      })
    })
  }

  function getSiblings(el) {
    var parent = el.parentElement
    if (!parent) return []
    var siblings = []
    var details = parent.querySelectorAll('details[data-thread-details]')
    for (var i = 0; i < details.length; i++) {
      if (details[i] !== el) siblings.push(details[i])
    }
    return siblings
  }

  function initThreadDetails(root) {
    var details = root.querySelectorAll('details[data-thread-details]')
    for (var i = 0; i < details.length; i++) {
      if (details[i]._threadInit) continue
      details[i]._threadInit = true

      details[i].addEventListener('click', function (e) {
        var el = this
        var target = e.target

        if (target.closest('[data-thread-show-exclusive]')) {
          e.preventDefault()
          e.stopPropagation()
          expandDetails(el)
          var siblings = getSiblings(el)
          for (var j = 0; j < siblings.length; j++) {
            if (siblings[j].open) collapseDetails(siblings[j])
          }
          return
        }

        if (target.closest('[data-thread-hide-others]')) {
          e.preventDefault()
          e.stopPropagation()
          var siblings = getSiblings(el)
          for (var j = 0; j < siblings.length; j++) {
            if (siblings[j].open) collapseDetails(siblings[j])
          }
          return
        }

        var summary = target.closest('summary')
        if (!summary || summary.parentElement !== el) return

        e.preventDefault()
        if (el.open) {
          collapseDetails(el)
        } else {
          expandDetails(el)
        }
      })
    }
  }

  initThreadDetails(document.body)
  new MutationObserver(function () { initThreadDetails(document.body) }).observe(document.body, { childList: true, subtree: true })
})()
