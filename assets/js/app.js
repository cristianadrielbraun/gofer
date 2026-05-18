var suppressNextAutoloadFirstEmail = false

document.addEventListener("DOMContentLoaded", function () {
  if (!document.getElementById("mail-sync-indeterminate-style")) {
    var style = document.createElement("style")
    style.id = "mail-sync-indeterminate-style"
    style.textContent = "@keyframes mailSyncIndeterminate{0%{transform:translateX(-120%)}50%{transform:translateX(40%)}100%{transform:translateX(240%)}}"
    document.head.appendChild(style)
  }
  var virtualMailList = null
  var virtualContactsList = null
  var pendingSyncEvents = []
  var syncRefreshTimer = null
  var processingStatusHandler = null
  var syncStatesByFolder = Object.create(null)
  var appEventSource = null
  var contactGmailSyncEmail = ""
  var prefetchedBodies = Object.create(null)
  var avatarWarmupTimer = null
  var avatarWarmupSent = Object.create(null)
  var autoMarkReadTimer = null
  var autoMarkReadEmailId = null
  var suppressEmailUrlPushFor = null
  var preserveMailListSelectionFor = null

  initVirtualScroll()
  setupFolderClickInterception()
  setupEmailSelectionTracking()
  setupMailListViewToggle()
  setupSidebarAppNavToggle()
  setupContactsList()
  setupMailFilters()
  setupMailTableColumnResize()
  setupSSE()
  setupContactAvatarImages()
  setupAvatarWarmup()
  setupMailListActions()
  setupSidebarAccountCollapse()
  setupProcessingStatus()
  setupBodyPrefetch()
  setupEmailBodyModeTabs()
  refreshSidebarUnread()

  function setupContactsList() {
    var searchTimer = null

    function init(root) {
      var scroll = root && root.id === "contacts-list-scroll" ? root : document.getElementById("contacts-list-scroll")
      if (!scroll || scroll._virtualContactsList || typeof VirtualContactsList === "undefined") return
      var selected = scroll.querySelector(".mail-list-item[data-contact-id] .envelope-active")
      var selectedRow = selected && selected.closest("[data-contact-id]")
      virtualContactsList = new VirtualContactsList(scroll, {
        viewMode: scroll.dataset.viewMode || "cards",
        selectedContactId: selectedRow ? selectedRow.dataset.contactId : null,
      })
      virtualContactsList.hydrateFromDOM({ animate: true })
      scroll._virtualContactsList = virtualContactsList
    }

    function currentList() {
      var scroll = document.getElementById("contacts-list-scroll")
      if (!scroll) return null
      return scroll._virtualContactsList || virtualContactsList
    }

    function readFilters() {
      var search = document.querySelector("[data-contact-search-input]")
      var form = document.querySelector("[data-contact-filter-form]")
      return {
        query: search ? search.value || "" : "",
        source: form && form.querySelector('[name="source"]') ? form.querySelector('[name="source"]').value || "" : "",
        saveTarget: form && form.querySelector('[name="save_target"]') ? form.querySelector('[name="save_target"]').value || "" : "",
        activity: form && form.querySelector('[name="activity"]') ? form.querySelector('[name="activity"]').value || "" : "",
      }
    }

    function syncFilterUI(filters) {
      var count = (filters.source ? 1 : 0) + (filters.saveTarget ? 1 : 0) + (filters.activity ? 1 : 0)
      var button = document.querySelector('[aria-label="Filter contacts"]')
      if (button) {
        button.dataset.active = count > 0 ? "true" : "false"
        var badge = button.querySelector("span")
        if (badge) badge.textContent = String(count)
      }
    }

    function contactSidebarTargetFromURL(url) {
      try {
        return new URL(url || window.location.href, window.location.origin).searchParams.get("save_target") || ""
      } catch (_) {
        return ""
      }
    }

    function setContactsSidebarActive(target) {
      var sidebar = document.querySelector('[data-app-sidebar]')
      if (!sidebar) return

      var activeLink = null
      var links = sidebar.querySelectorAll("[data-contact-sidebar-link]")
      for (var i = 0; i < links.length; i++) {
        var link = links[i]
        var active = (link.getAttribute("data-contact-sidebar-target") || "") === (target || "")
        link.classList.toggle("bg-sidebar-accent", active)
        link.classList.toggle("text-sidebar-primary", active)
        link.classList.toggle("font-medium", active)
        link.classList.toggle("text-sidebar-foreground", !active)
        link.classList.toggle("hover:bg-sidebar-accent/60", !active)
        link.classList.toggle("hover:text-sidebar-accent-foreground", !active)
        if (active) activeLink = link
      }

      var sections = sidebar.querySelectorAll("[data-sidebar-account]")
      for (var s = 0; s < sections.length; s++) {
        var sectionID = sections[s].getAttribute("data-sidebar-account") || ""
        if (sectionID.indexOf("contacts:") === 0) sections[s].removeAttribute("data-sidebar-account-active")
      }

      var activeSection = activeLink && activeLink.closest('[data-sidebar-account^="contacts:"]')
      if (activeSection) {
        activeSection.setAttribute("data-sidebar-account-active", "")
        activeSection.setAttribute("data-sidebar-account-collapsed", "false")
        var toggle = activeSection.querySelector("[data-sidebar-account-toggle]")
        if (toggle) toggle.setAttribute("aria-expanded", "true")
      }
    }

    function applyFilters() {
      var list = currentList()
      if (!list) return
      var filters = readFilters()
      syncFilterUI(filters)
      setContactsSidebarActive(filters.saveTarget)
      list.applyFilters(filters).catch(function () {})
    }

    function scheduleSearch() {
      if (searchTimer) clearTimeout(searchTimer)
      searchTimer = setTimeout(function () {
        searchTimer = null
        applyFilters()
      }, 250)
    }

    init(document)

    document.addEventListener("click", function (e) {
      var viewBtn = e.target.closest && e.target.closest("[data-contact-list-view-button]")
      if (viewBtn) {
        e.preventDefault()
        var mode = viewBtn.getAttribute("data-contact-list-view-button") === "table" ? "table" : "cards"
        if (window.GoferSettings) GoferSettings.set("contacts_list_view", mode)
        var group = viewBtn.closest("[data-contact-list-view-toggle]")
        if (group) {
          var buttons = group.querySelectorAll("[data-contact-list-view-button]")
          for (var i = 0; i < buttons.length; i++) {
            var active = buttons[i] === viewBtn
            buttons[i].classList.toggle("text-foreground", active)
            buttons[i].classList.toggle("text-muted-foreground", !active)
            buttons[i].classList.toggle("hover:text-foreground", !active)
          }
          var indicator = group.querySelector("[data-contact-list-view-indicator]")
          if (indicator) indicator.style.transform = mode === "table" ? "translateX(100%)" : "translateX(0)"
        }
        var list = currentList()
        if (list) list.switchViewMode(mode).catch(function () {})
        return
      }

      var clear = e.target.closest && e.target.closest("[data-contact-filter-clear]")
      if (clear) {
        e.preventDefault()
        var search = document.querySelector("[data-contact-search-input]")
        if (search) search.value = ""
        var form = document.querySelector("[data-contact-filter-form]")
        if (form) {
          var inputs = form.querySelectorAll("input")
          for (var ci = 0; ci < inputs.length; ci++) inputs[ci].value = ""
          var selectedItems = form.querySelectorAll("[data-tui-selectbox-selected='true']")
          for (var si = 0; si < selectedItems.length; si++) selectedItems[si].setAttribute("data-tui-selectbox-selected", "false")
          var placeholders = form.querySelectorAll("[data-tui-selectbox-placeholder]")
          for (var pi = 0; pi < placeholders.length; pi++) placeholders[pi].textContent = placeholders[pi].getAttribute("data-tui-selectbox-placeholder") || ""
        }
        applyFilters()
        return
      }

      var sidebarLink = e.target.closest && e.target.closest("[data-contact-sidebar-link]")
      if (sidebarLink) {
        setContactsSidebarActive(sidebarLink.getAttribute("data-contact-sidebar-target") || "")
        return
      }

      var contactItem = e.target.closest && e.target.closest("[data-contact-list-item]")
      if (!contactItem) return
      var row = contactItem.closest("[data-contact-id]")
      var list = currentList()
      if (list && row && row.dataset.contactId) list.onContactSelected(row.dataset.contactId)
    }, true)

    document.addEventListener("submit", function (e) {
      if (e.target && e.target.matches("[data-contact-editor-form]")) {
        e.preventDefault()
        saveContactEditor(e.target)
        return
      }
      if (!e.target || (!e.target.matches("[data-contact-filter-form]") && !e.target.matches("[data-contact-search-form]"))) return
      e.preventDefault()
      applyFilters()
    })

    function saveContactEditor(form) {
      if (!form || form.dataset.saving === "true") return
      form.dataset.saving = "true"
      var submit = form.querySelector('button[type="submit"]')
      if (submit) submit.disabled = true
      var formData = new FormData(form)

      fetch(form.action, {
        method: "POST",
        body: new URLSearchParams(formData),
        headers: {
          "Accept": "application/json",
          "Content-Type": "application/x-www-form-urlencoded",
        },
      }).then(function (res) {
        if (!res.ok) {
          return res.text().then(function (text) { throw new Error(text || "Save failed") })
        }
        return res.json()
      }).then(function (data) {
        if (data && data.contact_id) {
          form.action = "/api/contacts?id=" + encodeURIComponent(data.contact_id)
          if (data.location) history.replaceState({ contacts: true, contact: data.contact_id }, "", data.location)
        }
        if (data && data.gmail_sync_queued) {
          setupSSE()
          var emailInput = form.querySelector('[name="email"]')
          contactGmailSyncEmail = emailInput ? emailInput.value || "" : ""
          showGoferToast({
            id: "contact-sync-toast",
            title: "Contact saved",
            description: "Syncing contact with Gmail...",
            variant: "info",
            icon: "spinner",
            position: "bottom-right",
            duration: 0,
            dismissible: false,
          })
        } else {
          showGoferToast({
            id: "contact-save-toast",
            title: "Contact saved locally",
            description: "Changes are stored in Gofer.",
            variant: "success",
            icon: "success",
            position: "top-center",
            duration: 3000,
            dismissible: true,
          })
        }
      }).catch(function (err) {
        showGoferToast({
          id: "contact-save-toast",
          title: "Contact save failed",
          description: err && err.message ? err.message : "Could not save contact.",
          variant: "error",
          icon: "error",
          position: "top-center",
          duration: 8000,
          dismissible: true,
        })
      }).finally(function () {
        form.dataset.saving = "false"
        if (submit) submit.disabled = false
      })
    }

    document.addEventListener("input", function (e) {
      if (!e.target || !e.target.matches("[data-contact-search-input]")) return
      scheduleSearch()
    })

    document.addEventListener("search", function (e) {
      if (!e.target || !e.target.matches("[data-contact-search-input]")) return
      scheduleSearch()
    })

    document.addEventListener("change", function (e) {
      if (!e.target || !e.target.closest("[data-contact-filter-form]")) return
      applyFilters()
    })

    document.body.addEventListener("htmx:afterSettle", function (evt) {
      if (!evt.target || !evt.target.querySelector) return
      if (evt.target.id === "main-content" || evt.target.querySelector("#contacts-list-scroll")) {
        init(evt.target)
        setContactsSidebarActive(contactSidebarTargetFromURL(window.location.href))
      }
    })

    window.addEventListener("popstate", function () {
      setContactsSidebarActive(contactSidebarTargetFromURL(window.location.href))
    })
  }

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
      return section.hasAttribute("data-sidebar-account-active") || !!section.querySelector('a[hx-get^="/folder/"].bg-sidebar-accent')
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
        buttons[i].classList.toggle("text-foreground", active)
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
    if (appEventSource) return

    var source = new EventSource("/api/events")
    appEventSource = source

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
        handleComposeSendResult("sent")
      } else if (data.status === "ambiguous") {
        showSendStatus("ambiguous", data.error || "Send status unknown")
        handleComposeSendResult("ambiguous")
      } else {
        showSendStatus("failed", data.error || "Failed to send")
        handleComposeSendResult("failed")
      }
    })

    source.addEventListener("mutation", function (e) {
      refreshSidebarUnread()
    })

    source.addEventListener("avatar-updated", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { return }
      if (!data || !data.avatar_hash || !data.avatar_url) return
      updateVisibleAvatars(data.avatar_hash, data.avatar_url)
    })

    source.addEventListener("processing-status", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { return }
      if (!data || !processingStatusHandler) return
      processingStatusHandler.render(data)
    })

    source.addEventListener("contact-activity", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { return }
      if (!data || !data.event_type) return
      handleContactActivityEvent(data)
    })

    source.addEventListener("manual-sync-started", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { return }
      handleMailManualSyncEvent("started", data)
    })

    source.addEventListener("manual-sync-progress", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { return }
      handleMailManualSyncEvent("progress", data)
    })

    source.addEventListener("manual-sync-complete", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { return }
      handleMailManualSyncEvent("complete", data)
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
        scheduleSyncRefresh(vml, { noAnimation: true })
      }, false)
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
        scheduleSyncRefresh(vml, { noAnimation: true })
      }, false)
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
        scheduleSyncRefresh(vml, { noAnimation: true })
      }, false)
    })

    source.onerror = function () {
      source.close()
      if (appEventSource === source) appEventSource = null
      setTimeout(setupSSE, 5000)
    }
  }

  function handleContactActivityEvent(data) {
    if (data.event_type !== "gmail_contact_synced" && data.event_type !== "gmail_contact_sync_failed") return
    if (contactGmailSyncEmail && data.email && data.email !== contactGmailSyncEmail) return
    var failed = data.event_type === "gmail_contact_sync_failed"
    showGoferToast({
      id: "contact-sync-toast",
      title: failed ? "Gmail sync failed" : "Gmail sync complete",
      description: data.message || (failed ? "Could not sync contact to Gmail." : "Contact synced to Gmail."),
      variant: failed ? "error" : "success",
      icon: failed ? "error" : "success",
      position: "bottom-right",
      duration: failed ? 7000 : 3500,
      dismissible: true,
    })
    contactGmailSyncEmail = ""
  }

  function updateVisibleAvatars(hash, avatarURL) {
    patchVirtualMailListAvatarCache(hash, avatarURL)
    var nodes = document.querySelectorAll("[data-contact-avatar][data-avatar-hash]")
    for (var i = 0; i < nodes.length; i++) {
      var node = nodes[i]
      if (node.getAttribute("data-avatar-hash") !== hash) continue
      applyAvatarURLToNode(node, avatarURL)
    }
  }

  function applyAvatarURLToNode(node, avatarURL) {
    hideContactAvatarFallback(node)

    var img = node.querySelector("img[data-avatar-image]")
    if (!img) {
      img = document.createElement("img")
      img.setAttribute("data-avatar-image", "")
      img.decoding = "async"
      img.alt = ""
      img.className = "absolute inset-0 h-full w-full object-cover"
      node.appendChild(img)
    }
    if (img.getAttribute("src") !== avatarURL) {
      img.setAttribute("src", avatarURL)
    }
  }

  function patchVirtualMailListAvatarCache(hash, avatarURL) {
    var lists = []
    if (virtualMailList) lists.push(virtualMailList)
    document.querySelectorAll("#mail-list-scroll").forEach(function (container) {
      if (container._virtualMailList && lists.indexOf(container._virtualMailList) === -1) {
        lists.push(container._virtualMailList)
      }
    })

    for (var i = 0; i < lists.length; i++) {
      var list = lists[i]
      var changed = false
      if (list.cache && typeof list.cache.forEach === "function") {
        list.cache.forEach(function (item) {
          if (!item || !item.html) return
          var patched = patchAvatarHTML(item.html, hash, avatarURL)
          if (patched !== item.html) {
            item.html = patched
            changed = true
          }
        })
      }
      if (list.expandedThreads && typeof list.expandedThreads.forEach === "function") {
        list.expandedThreads.forEach(function (thread) {
          if (!thread || !thread.html) return
          var patched = patchAvatarHTML(thread.html, hash, avatarURL)
          if (patched !== thread.html) {
            thread.html = patched
            changed = true
          }
        })
      }
      if (changed && typeof list.render === "function") {
        list.prevFirst = null
        list.prevLast = null
        list.render()
      }
    }
  }

  function patchAvatarHTML(html, hash, avatarURL) {
    var template = document.createElement("template")
    template.innerHTML = html || ""
    var changed = false
    template.content.querySelectorAll("[data-contact-avatar][data-avatar-hash]").forEach(function (node) {
      if (node.getAttribute("data-avatar-hash") !== hash) return
      applyAvatarURLToNode(node, avatarURL)
      changed = true
    })
    return changed ? template.innerHTML : html
  }

  function setupContactAvatarImages() {
    document.addEventListener("load", function (e) {
      if (!e.target.matches || !e.target.matches("img[data-avatar-image]")) return
      var node = e.target.closest("[data-contact-avatar]")
      if (node) hideContactAvatarFallback(node)
      e.target.classList.remove("hidden")
    }, true)

    document.addEventListener("error", function (e) {
      if (!e.target.matches || !e.target.matches("img[data-avatar-image]")) return
      e.target.classList.add("hidden")
      var node = e.target.closest("[data-contact-avatar]")
      if (node) showContactAvatarFallback(node)
    }, true)

    document.querySelectorAll("[data-contact-avatar]").forEach(function (node) {
      var img = node.querySelector("img[data-avatar-image]")
      if (!img) {
        showContactAvatarFallback(node)
        return
      }
      if (img.complete && img.naturalWidth === 0) {
        img.classList.add("hidden")
        showContactAvatarFallback(node)
      } else {
        hideContactAvatarFallback(node)
      }
    })
  }

  function hideContactAvatarFallback(node) {
    var fallback = node.querySelector("[data-avatar-fallback]")
    if (!fallback) return
    fallback.classList.add("hidden")
    fallback.classList.remove("flex")
  }

  function showContactAvatarFallback(node) {
    var fallback = node.querySelector("[data-avatar-fallback]")
    if (!fallback) return
    fallback.classList.remove("hidden")
    fallback.classList.add("flex")
  }

  function setupAvatarWarmup() {
    scheduleAvatarWarmup()
    document.addEventListener("scroll", scheduleAvatarWarmup, true)
    window.addEventListener("resize", scheduleAvatarWarmup)
    var observer = new MutationObserver(scheduleAvatarWarmup)
    observer.observe(document.body, { childList: true, subtree: true })
  }

  function scheduleAvatarWarmup() {
    if (avatarWarmupTimer) return
    avatarWarmupTimer = setTimeout(function () {
      avatarWarmupTimer = null
      warmupVisibleAvatars()
    }, 500)
  }

  function warmupVisibleAvatars() {
    var now = Date.now()
    var emails = []
    document.querySelectorAll("[data-contact-avatar][data-avatar-email]").forEach(function (node) {
      if (emails.length >= 25) return
      if (!isAvatarWarmupVisible(node)) return
      if (node.querySelector("img[data-avatar-image]:not(.hidden)")) return
      var email = (node.getAttribute("data-avatar-email") || "").trim().toLowerCase()
      if (!email || email.indexOf("@") < 1) return
      if (avatarWarmupSent[email] && now - avatarWarmupSent[email] < 10 * 60 * 1000) return
      avatarWarmupSent[email] = now
      emails.push(email)
    })
    if (!emails.length) return
    fetch("/api/avatars/warmup", {
      method: "POST",
      headers: { "Content-Type": "application/json", "Accept": "application/json" },
      body: JSON.stringify({ emails: emails }),
      keepalive: true,
    }).catch(function () {})
  }

  function isAvatarWarmupVisible(node) {
    var rect = node.getBoundingClientRect()
    return rect.width > 0 && rect.height > 0 && rect.bottom >= 0 && rect.right >= 0 && rect.top <= window.innerHeight && rect.left <= window.innerWidth
  }

  function scheduleSyncRefresh(vml, options) {
    if (!vml) return
    if (syncRefreshTimer) return
    syncRefreshTimer = setTimeout(function () {
      syncRefreshTimer = null
      vml.refreshCurrentFolder(options || {}).catch(function () {})
    }, 700)
  }

  function withMailListForFolder(folderId, folderRole, fn, queueIfInactive) {
    if (typeof folderRole === "function") {
      queueIfInactive = fn
      fn = folderRole
      folderRole = ""
    }
    if (queueIfInactive === undefined) queueIfInactive = true
    if (typeof fn !== "function") return
    if (virtualMailList && matchesActiveFolder(virtualMailList.folderID, folderId, folderRole)) {
      fn(virtualMailList)
      return
    }
    if (!queueIfInactive) return
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
    virtualMailList.hydrateFromDOM({ animate: true })
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
    var initialEmailId = document.body ? (document.body.getAttribute("data-initial-email-id") || "") : ""
    if (document.body) document.body.removeAttribute("data-initial-email-id")
    var path = "/folder/" + folderID + (initialEmailId ? "/" + initialEmailId : "")
    if (window.location.pathname !== path) {
      history.replaceState({ folder: folderID, email: initialEmailId || null }, "", path)
    }
    if (typeof htmx !== "undefined") {
      if (initialEmailId) {
        var loadEmailAfterShell = function (evt) {
          if (!evt.target || evt.target.id !== "main-content") return
          document.body.removeEventListener("htmx:afterSettle", loadEmailAfterShell)
          preserveMailListSelectionFor = initialEmailId
          htmx.ajax("GET", "/email/" + initialEmailId, "#mail-view")
        }
        document.body.addEventListener("htmx:afterSettle", loadEmailAfterShell)
      }
      var url = "/folder/" + folderID + "/full"
      if (initialEmailId) url += "?selected=" + encodeURIComponent(initialEmailId)
      htmx.ajax("GET", url, {target: "#main-content", swap: "outerHTML"})
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
    if (suppressNextAutoloadFirstEmail) {
      suppressNextAutoloadFirstEmail = false
      return
    }
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

    function clearFolderActiveState() {
      var sidebarLinks = sidebar.querySelectorAll("a[hx-get^='/folder/']")
      for (var i = 0; i < sidebarLinks.length; i++) {
        sidebarLinks[i].classList.remove("bg-sidebar-accent", "text-sidebar-primary", "font-medium")
        sidebarLinks[i].classList.add("text-sidebar-foreground")
        var badge = sidebarLinks[i].querySelector("[data-folder-unread]")
        if (badge) {
          badge.classList.remove("bg-sidebar-primary/20", "text-sidebar-primary")
          badge.classList.add("bg-sidebar-accent", "text-sidebar-foreground/80")
        }
      }
    }

    function setContactsActive(active) {
      var contactsLink = sidebar.querySelector("[data-sidebar-contacts-link]")
      if (!contactsLink) return
      contactsLink.classList.toggle("bg-sidebar-accent", active)
      contactsLink.classList.toggle("text-sidebar-primary", active)
      contactsLink.classList.toggle("font-medium", active)
      contactsLink.classList.toggle("text-sidebar-foreground", !active)
    }

    sidebar.addEventListener("click", function (e) {
      var contactsLink = e.target.closest("[data-sidebar-contacts-link]")
      if (contactsLink) {
        clearFolderActiveState()
        setContactsActive(true)
        virtualMailList = null
        return
      }

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

      clearFolderActiveState()
      setContactsActive(false)
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
      var isOnSettings = mainContent && mainContent.querySelector("[data-settings-page]")
      var isOnContacts = !!document.getElementById("contacts-list-scroll")
      var mailListDetached = !virtualMailList || !virtualMailList.container || !document.body.contains(virtualMailList.container)
      if (isOnSettings || isOnContacts || mailListDetached) {
        if (typeof htmx !== "undefined") {
          virtualContactsList = null
          history.pushState({ folder: folderID, email: null }, "", "/folder/" + folderID)
          if (mainContent) showMailContentLoading(mainContent, link)
          htmx.ajax("GET", "/folder/" + folderID + "/full", {target: "#main-content", swap: "outerHTML"})
        }
      } else {
        virtualMailList.switchFolder(folderID).then(function () {
          applyActiveFolderSyncState()
          scheduleSyncRefresh(virtualMailList, { noAnimation: true })
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
		  if (preserveMailListSelectionFor === emailId) {
		    preserveMailListSelectionFor = null
		    virtualMailList.syncSelectionClasses(virtualMailList.itemsContainer)
		  } else if (suppressEmailUrlPushFor === emailId) {
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

      var scroll = document.getElementById("mail-list-scroll")
      if (scroll && scroll.dataset.viewSwitchPending === "true") return

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

      var vml = scroll && scroll._virtualMailList
      if (vml && typeof vml.switchViewMode === "function") {
        vml.switchViewMode(mode).catch(function () {})
      }
    })
  }

  function setupSidebarAppNavToggle() {
    document.body.addEventListener("click", function (e) {
      var btn = e.target.closest && e.target.closest("[data-sidebar-app-button]")
      if (!btn || btn.disabled || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return

      var href = btn.getAttribute("href")
      if (!href) return
      e.preventDefault()

      var mode = btn.getAttribute("data-sidebar-app-button")
      var group = btn.closest("[data-sidebar-app-nav]")
      if (group) {
        var buttons = group.querySelectorAll("[data-sidebar-app-button]")
        for (var i = 0; i < buttons.length; i++) {
          var isActive = buttons[i] === btn
          buttons[i].classList.toggle("text-sidebar-accent-foreground", isActive)
          buttons[i].classList.toggle("text-sidebar-foreground", !isActive && !buttons[i].disabled)
          buttons[i].classList.toggle("hover:text-sidebar-accent-foreground", !isActive && !buttons[i].disabled)
          if (isActive) buttons[i].setAttribute("aria-current", "true")
          else buttons[i].removeAttribute("aria-current")
        }
        var indicator = group.querySelector("[data-sidebar-app-indicator]")
        if (indicator) {
          if (mode === "contacts") indicator.style.transform = "translateX(calc(100% + 2px))"
          else if (mode === "calendar") indicator.style.transform = "translateX(calc(200% + 4px))"
          else indicator.style.transform = "translateX(0)"
        }
      }

      if (btn.getAttribute("aria-current") === "true" && window.location.pathname === href) return
      window.setTimeout(function () { window.location.href = href }, 140)
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
    virtualMailList.hydrateFromDOM({ animate: true })
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
var _sendStatusToast = null

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
  if (sendStatusTimer) {
    clearTimeout(sendStatusTimer)
    sendStatusTimer = null
  }

  var config = _composeToastConfig(status)
  var duration = status === "sending" ? 0 : (status === "sent" ? 5000 : 8000)
  _sendStatusToast = showGoferToast({
    id: "compose-status-toast",
    title: config.title,
    description: text,
    status: status,
    variant: config.variant,
    icon: config.icon,
    position: "top-center",
    duration: duration,
    dismissible: status !== "sending"
  })
}

function hideSendStatus() {
  if (sendStatusTimer) {
    clearTimeout(sendStatusTimer)
    sendStatusTimer = null
  }
  if (_sendStatusToast) dismissGoferToast(_sendStatusToast)
  _sendStatusToast = null
}

function _composeToastConfig(status) {
  if (status === "sent") return { title: "Message sent", variant: "success", icon: "success" }
  if (status === "sending") return { title: "Working...", variant: "info", icon: "spinner" }
  if (status === "ambiguous") return { title: "Needs review", variant: "warning", icon: "warning" }
  return { title: "Action failed", variant: "error", icon: "error" }
}

function _goferToastIcon(icon) {
  if (icon === "success") return '<svg class="gofer-toast-icon-success size-[22px] mr-3 flex-shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><path d="m9 12 2 2 4-4"/></svg>'
  if (icon === "warning") return '<svg class="size-[22px] text-muted-foreground mr-3 flex-shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><path d="m21.73 18-8-14a2 2 0 0 0-3.46 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3"/><path d="M12 9v4"/><path d="M12 17h.01"/></svg>'
  if (icon === "error") return '<svg class="size-[22px] text-destructive mr-3 flex-shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><path d="m15 9-6 6"/><path d="m9 9 6 6"/></svg>'
  if (icon === "spinner") return '<svg class="size-[22px] text-muted-foreground mr-3 flex-shrink-0 animate-spin" viewBox="0 0 24 24" fill="none"><circle cx="12" cy="12" r="10" stroke="currentColor" stroke-width="3" stroke-linecap="round" opacity="0.25"/><path d="M12 2a10 10 0 0 1 10 10" stroke="currentColor" stroke-width="3" stroke-linecap="round"/></svg>'
  return '<svg class="size-[22px] text-muted-foreground mr-3 flex-shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><path d="M12 16v-4"/><path d="M12 8h.01"/></svg>'
}

function dismissGoferToast(toast) {
  if (!toast || !toast.isConnected) return
  if (toast._goferToastTimer) clearTimeout(toast._goferToastTimer)
  toast.style.transition = "opacity 300ms, transform 300ms"
  toast.style.opacity = "0"
  toast.style.transform = "translateY(1rem)"
  setTimeout(function () { if (toast.isConnected) toast.remove() }, 300)
}

function showGoferToast(opts) {
  opts = opts || {}
  var id = opts.id || "gofer-toast-" + Date.now()
  var existing = document.getElementById(id)
  if (existing) dismissGoferToast(existing)
  var duration = Number(opts.duration || 0)
  var toast = document.createElement("div")
  toast.id = id
  toast.dataset.tuiToast = ""
  toast.dataset.tuiToastDuration = String(duration)
  toast.dataset.position = opts.position || "top-center"
  toast.dataset.variant = opts.variant || "default"
  toast.className = "z-50 fixed pointer-events-auto p-4 w-fit max-w-[calc(100vw-2rem)] md:max-w-[680px] animate-in fade-in slide-in-from-bottom-4 duration-300 data-[position=top-right]:top-0 data-[position=top-right]:right-0 data-[position=top-left]:top-0 data-[position=top-left]:left-0 data-[position=top-center]:top-0 data-[position=top-center]:left-1/2 data-[position=top-center]:-translate-x-1/2 data-[position=bottom-right]:bottom-0 data-[position=bottom-right]:right-0 data-[position=bottom-left]:bottom-0 data-[position=bottom-left]:left-0 data-[position=bottom-center]:bottom-0 data-[position=bottom-center]:left-1/2 data-[position=bottom-center]:-translate-x-1/2 data-[position*=top]:slide-in-from-top-4 data-[position*=bottom]:slide-in-from-bottom-4"
  toast.innerHTML =
    '<div class="gofer-toast-card" data-variant="' + _escapeComposeHTML(opts.variant || "default") + '">' +
      (duration > 0 ? '<div class="gofer-toast-progress-wrap"><div class="toast-progress gofer-toast-progress" data-variant="' + _escapeComposeHTML(opts.variant || "default") + '"></div></div>' : '') +
      _goferToastIcon(opts.icon || "info") +
      '<span class="flex-1 min-w-0">' +
        (opts.title ? '<p class="text-sm font-semibold truncate">' + _escapeComposeHTML(opts.title) + '</p>' : '') +
        (opts.description ? '<p class="text-sm opacity-90 mt-1">' + _escapeComposeHTML(opts.description) + '</p>' : '') +
      '</span>' +
      (opts.dismissible ? '<button type="button" class="gofer-toast-dismiss" aria-label="Close" data-tui-toast-dismiss>x</button>' : '') +
    '</div>'
  var dismiss = toast.querySelector("[data-tui-toast-dismiss]")
  if (dismiss) dismiss.addEventListener("click", function () { dismissGoferToast(toast) })
  document.body.appendChild(toast)
  if (duration > 0) {
    var progress = toast.querySelector(".gofer-toast-progress")
    if (progress) {
      progress.style.width = "100%"
      progress.offsetWidth
      progress.style.transition = "width " + duration + "ms linear"
      progress.style.width = "0px"
    }
    toast._goferToastTimer = setTimeout(function () { dismissGoferToast(toast) }, duration)
  }
  return toast
}

var _mailSyncRunID = ""
var _mailSyncActive = false

function _goferResponseText(xhr, fallback) {
  if (xhr && xhr.responseText) {
    var div = document.createElement("div")
    div.innerHTML = xhr.responseText
    return (div.textContent || "").trim() || fallback
  }
  return fallback
}

function _mailSyncCount(value) {
  var n = Number(value || 0)
  return isFinite(n) && n > 0 ? n : 0
}

function _mailSyncAccountsLabel(count) {
  return count === 1 ? "1 account" : count + " accounts"
}

function _setMailSyncButtonBusy(busy) {
  var buttons = document.querySelectorAll("[data-mail-sidebar-sync-button]")
  for (var i = 0; i < buttons.length; i++) {
    buttons[i].dataset.syncing = busy ? "true" : "false"
    var icon = buttons[i].querySelector("svg")
    if (icon) icon.classList.toggle("animate-spin", !!busy)
  }
}

function handleMailSidebarSyncStart() {
  _mailSyncRunID = ""
  _mailSyncActive = true
  _setMailSyncButtonBusy(true)
  showGoferToast({
    id: "mail-sync-toast",
    title: "Syncing mail",
    description: "Checking connected mailboxes...",
    variant: "info",
    icon: "spinner",
    position: "bottom-right",
    duration: 0,
    dismissible: false,
  })
}

function handleMailSidebarSyncResult(event) {
  var xhr = event && event.detail ? event.detail.xhr : null
  var ok = !!(event && event.detail && event.detail.successful) && (!xhr || xhr.getResponseHeader("X-Gofer-Status") !== "error")
  var message = _goferResponseText(xhr, "Mail sync started.")
  if (!ok) {
    _mailSyncActive = false
    _mailSyncRunID = ""
    _setMailSyncButtonBusy(false)
    showGoferToast({
      id: "mail-sync-toast",
      title: "Mail sync failed",
      description: message,
      variant: "error",
      icon: "error",
      position: "bottom-right",
      duration: 8000,
      dismissible: true,
    })
    return
  }

  if (xhr && xhr.getResponseHeader("X-Gofer-Mail-Sync-Running") === "true") {
    _mailSyncActive = false
    _mailSyncRunID = ""
    _setMailSyncButtonBusy(false)
    showGoferToast({
      id: "mail-sync-toast",
      title: "Mail sync already running",
      description: message,
      variant: "info",
      icon: "info",
      position: "bottom-right",
      duration: 4500,
      dismissible: true,
    })
    return
  }

  _mailSyncRunID = xhr ? (xhr.getResponseHeader("X-Gofer-Mail-Sync-Run-ID") || "") : ""
  showGoferToast({
    id: "mail-sync-toast",
    title: "Syncing mail",
    description: message,
    variant: "info",
    icon: "spinner",
    position: "bottom-right",
    duration: 0,
    dismissible: false,
  })
}

function handleMailManualSyncEvent(phase, data) {
  if (!data) return
  var runID = data.run_id || ""
  if (!_mailSyncActive && phase !== "started") return
  if (_mailSyncRunID && runID && _mailSyncRunID !== runID) return
  if (!_mailSyncRunID && runID) _mailSyncRunID = runID
  _mailSyncActive = true
  _setMailSyncButtonBusy(true)

  var total = _mailSyncCount(data.accounts_total)
  var done = _mailSyncCount(data.accounts_done)
  var index = _mailSyncCount(data.account_index)
  var parallelism = _mailSyncCount(data.parallelism)
  var failures = _mailSyncCount(data.failures)
  var skipped = _mailSyncCount(data.skipped)
  var notDone = _mailSyncCount(data.not_done)

  if (phase === "complete") {
    var status = data.status || "ok"
    var title = status === "error" ? "Mail sync failed" : (status === "partial" ? "Mail sync partly finished" : "Mail synced")
    var variant = status === "error" ? "error" : (status === "partial" ? "warning" : "success")
    var icon = status === "error" ? "error" : (status === "partial" ? "warning" : "success")
    var description = "Checked " + _mailSyncAccountsLabel(total || done) + "."
    if (failures > 0 && skipped > 0) description += " " + failures + " failed, " + skipped + " already running."
    else if (failures > 0) description += " " + failures + " failed."
    else if (skipped > 0) description += " " + skipped + " already running."
    if (notDone > 0) description += " " + notDone + " did not finish."
    _mailSyncActive = false
    _mailSyncRunID = ""
    _setMailSyncButtonBusy(false)
    showGoferToast({
      id: "mail-sync-toast",
      title: title,
      description: description,
      variant: variant,
      icon: icon,
      position: "bottom-right",
      duration: status === "ok" ? 4500 : 8000,
      dismissible: true,
    })
    return
  }

  var message = "Checking " + _mailSyncAccountsLabel(total || 1) + "..."
  if (phase === "started" && parallelism > 1 && total > 1) {
    message = "Checking " + _mailSyncAccountsLabel(total) + ", up to " + parallelism + " at a time..."
  }
  if (phase === "progress") {
    if (data.status === "syncing" && index && total) message = "Checking account " + index + " of " + total + "..."
    else if (data.status === "skipped") message = "Account already syncing (" + done + " / " + total + ")."
    else if (data.status === "error") message = "Account failed (" + done + " / " + total + ")."
    else if (done && total) message = "Checked " + done + " of " + total + " accounts."
  }
  showGoferToast({
    id: "mail-sync-toast",
    title: "Syncing mail",
    description: message,
    variant: "info",
    icon: "spinner",
    position: "bottom-right",
    duration: 0,
    dismissible: false,
  })
}

function handleContactSidebarSyncStart() {
  showGoferToast({
    id: "contact-sync-toast",
    title: "Syncing contacts",
    description: "Checking connected address books...",
    variant: "info",
    icon: "spinner",
    position: "bottom-right",
    duration: 0,
    dismissible: false,
  })
}

function handleContactSidebarSyncResult(event) {
  var xhr = event && event.detail ? event.detail.xhr : null
  var ok = !!(event && event.detail && event.detail.successful) && (!xhr || xhr.getResponseHeader("X-Gofer-Status") !== "error")
  var message = "Contact sync finished."
  if (xhr && xhr.responseText) {
    var div = document.createElement("div")
    div.innerHTML = xhr.responseText
    message = (div.textContent || "").trim() || message
  }
  showGoferToast({
    id: "contact-sync-toast",
    title: ok ? "Contacts synced" : "Contact sync failed",
    description: message,
    variant: ok ? "success" : "error",
    icon: ok ? "success" : "error",
    position: "bottom-right",
    duration: ok ? 4500 : 8000,
    dismissible: true,
  })
  if (ok && window.htmx) {
    htmx.ajax("GET", window.location.pathname + window.location.search, { target: "#main-content", swap: "outerHTML" })
  }
}

  var _composeActive = false
  var _activeComposeEditor = null
  var _composeSendState = null
  var _composeSignatureCache = Object.create(null)
  var _composeSignatureMenu = null

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
  applyDefaultComposeSignature(document.getElementById(prefix + "form"), true)
}

function resetComposeForm(fromPane, skipCleanup) {
  var prefix = fromPane ? "compose-pane-" : "compose-"
  var form = document.getElementById(prefix + "form")
  if (!form) return
  cancelComposeAutosave(form)
  if (!skipCleanup) cleanupComposeStagedUploads(form)
  var fields = form.querySelectorAll('input[name="to"], input[name="cc"], input[name="bcc"], input[name="subject"], input[name="draft_id"], input[name="in_reply_to"], input[name="references"], textarea[name="body"], textarea[name="html_body"]')
  for (var i = 0; i < fields.length; i++) fields[i].value = ""
  var modeField = form.querySelector('input[name="compose_mode"]')
  if (modeField) modeField.value = "new"
  var editor = form.querySelector("[data-compose-editor]")
  if (editor) editor.innerHTML = ""
  syncComposeInlineImageInputs(form)
  var recipientFields = form.querySelectorAll("[data-compose-recipient-field]")
  for (var i = 0; i < recipientFields.length; i++) renderComposeRecipientField(recipientFields[i], "")
  renderComposeAttachments(form, [])
  form.dataset.composeUploadsPending = "0"
  form.dataset.composeSending = "false"
  delete form.dataset.composeUploadFailed
  form.dataset.composeDirty = "false"
  updateComposeSendState(form)
  _setComposeDraftButtonState(form, "default")
}

function composeModeForForm(form) {
  var field = form && form.querySelector('input[name="compose_mode"]')
  return (field && field.value) || "new"
}

function setComposeMode(form, mode) {
  var field = form && form.querySelector('input[name="compose_mode"]')
  if (field) field.value = mode || "new"
}

function composeSignatureCacheKey(form) {
  var account = form && form.querySelector('input[name="account_id"]')
  if (!account || !account.value) return ""
  return account.value + "::" + composeModeForForm(form)
}

function loadComposeSignatures(form, refresh) {
  var account = form && form.querySelector('input[name="account_id"]')
  if (!account || !account.value) return Promise.resolve(null)
  var key = composeSignatureCacheKey(form)
  if (!refresh && _composeSignatureCache[key]) return Promise.resolve(_composeSignatureCache[key])
  var params = new URLSearchParams()
  params.set("mode", composeModeForForm(form))
  return fetch("/api/accounts/" + encodeURIComponent(account.value) + "/signatures?" + params.toString())
    .then(function (r) { if (!r.ok) throw new Error("Failed to load signatures"); return r.json() })
    .then(function (data) { _composeSignatureCache[key] = data; return data })
    .catch(function () { return null })
}

function composeSignatureHTML(sig, source) {
  var html = sig && sig.html_body ? _sanitizeComposeHTML(sig.html_body) : _composePlainToHTML((sig && sig.text_body) || "")
  return '<div data-gofer-signature="' + source + '" data-signature-id="' + _escapeComposeHTML(sig.id || "") + '" data-signature-html="' + _escapeComposeHTML(html) + '">' + html + '</div>'
}

function existingComposeSignature(editor) {
  return editor && editor.querySelector('[data-gofer-signature]')
}

function autoComposeSignatureWasEdited(node) {
  if (!node || node.getAttribute("data-gofer-signature") !== "auto") return false
  return (node.getAttribute("data-signature-html") || "") !== node.innerHTML
}

function autoComposeSignatureSpacerHTML() {
  return '<p data-gofer-signature-cursor="true"><br></p><p><br></p><p><br></p><p><br></p>'
}

function placeComposeCursorBeforeSignature(editor, signatureNode) {
  if (!editor || !signatureNode) return
  editor.focus()
  var target = editor.querySelector('[data-gofer-signature-cursor]')
  if (!target) target = signatureNode.previousSibling
  if (!target || target.nodeType !== Node.ELEMENT_NODE) {
    signatureNode.insertAdjacentHTML("beforebegin", autoComposeSignatureSpacerHTML())
    target = editor.querySelector('[data-gofer-signature-cursor]') || signatureNode.previousSibling
  }
  if (target.removeAttribute) target.removeAttribute("data-gofer-signature-cursor")
  var range = document.createRange()
  if (target.nodeType === Node.ELEMENT_NODE) {
    range.selectNodeContents(target)
    range.collapse(true)
  } else {
    range.setStartBefore(signatureNode)
    range.collapse(true)
  }
  var selection = window.getSelection()
  if (selection) {
    selection.removeAllRanges()
    selection.addRange(range)
  }
}

function insertComposeSignature(form, sig, source) {
  return insertComposeSignatureWithPlacement(form, sig, source, "before")
}

function insertComposeSignatureWithPlacement(form, sig, source, placement) {
  var editor = form && form.querySelector("[data-compose-editor]")
  if (!editor || !sig) return false
  var existing = existingComposeSignature(editor)
  var html = composeSignatureHTML(sig, source)
  if (existing) {
    if (source === "auto" && autoComposeSignatureWasEdited(existing)) return false
    if (source === "auto") {
      existing.remove()
      existing = null
    } else {
      existing.outerHTML = html
      syncComposeEditor(editor)
      return true
    }
  }
  if (source === "auto" && placement === "after" && (composeModeForForm(form) === "reply" || composeModeForForm(form) === "reply-all" || composeModeForForm(form) === "forward")) {
    editor.insertAdjacentHTML("beforeend", autoComposeSignatureSpacerHTML() + html)
  } else if (source === "auto") {
    var first = editor.firstElementChild
    if (first && first.tagName === "P" && !first.textContent.trim() && !first.querySelector("img")) {
      first.insertAdjacentHTML("afterend", autoComposeSignatureSpacerHTML() + html)
    } else {
      editor.insertAdjacentHTML(editor.textContent.trim() ? "afterbegin" : "beforeend", autoComposeSignatureSpacerHTML() + html)
    }
  } else {
    editor.focus()
    _restoreComposeSelection(editor)
    document.execCommand("insertHTML", false, html)
  }
  if (source === "auto") placeComposeCursorBeforeSignature(editor, existingComposeSignature(editor))
  syncComposeEditor(editor)
  return true
}

function composeSignaturePlacement(form, data) {
  var mode = composeModeForForm(form)
  var settings = (data && data.settings) || {}
  if ((mode === "reply" || mode === "reply-all") && settings.reply_placement === "after") return "after"
  if (mode === "forward" && settings.forward_placement === "after") return "after"
  return "before"
}

function applyDefaultComposeSignature(form, refresh) {
  if (!form) return Promise.resolve(false)
  return loadComposeSignatures(form, refresh).then(function (data) {
    if (!data || !data.default_signature) return
    return insertComposeSignatureWithPlacement(form, data.default_signature, "auto", composeSignaturePlacement(form, data))
  })
}

function applyDefaultComposeSignatureWhenReady(form, refresh) {
  if (!form) return
  requestAnimationFrame(function () {
    applyDefaultComposeSignature(form, refresh)
  })
}

function closeComposeSignatureMenu() {
  if (_composeSignatureMenu) _composeSignatureMenu.remove()
  _composeSignatureMenu = null
}

function showComposeSignaturePicker(el) {
  closeComposeSignatureMenu()
  var form = _composeFormFrom(el)
  if (!form) return
  loadComposeSignatures(form, true).then(function (data) {
    var menu = document.createElement("div")
    menu.className = "compose-attachment-menu"
    var signatures = (data && data.signatures) || []
    if (!signatures.length) {
      var empty = document.createElement("div")
      empty.className = "px-3 py-2 text-xs text-muted-foreground"
      empty.textContent = "No signatures configured"
      menu.appendChild(empty)
    }
    for (var i = 0; i < signatures.length; i++) {
      ;(function (sig) {
        var btn = document.createElement("button")
        btn.type = "button"
        btn.textContent = sig.name || "Signature"
        btn.onclick = function () {
          closeComposeSignatureMenu()
          if (insertComposeSignature(form, sig, "manual")) _markComposeDirty(form)
        }
        menu.appendChild(btn)
      })(signatures[i])
    }
    document.body.appendChild(menu)
    _composeSignatureMenu = menu
    var rect = el.getBoundingClientRect()
    menu.style.top = Math.min(window.innerHeight - menu.offsetHeight - 8, rect.bottom + 6) + "px"
    menu.style.left = Math.max(8, Math.min(rect.left, window.innerWidth - menu.offsetWidth - 8)) + "px"
    setTimeout(function () { document.addEventListener("mousedown", closeComposeSignatureMenu, { once: true }) }, 0)
  })
}

window.showComposeSignaturePicker = showComposeSignaturePicker

function cleanupComposeStagedUploads(form) {
  if (!form) return
  readComposeAttachments(form).forEach(function (att) {
    if (att.id && !att.existing) fetch("/compose/attachments/" + encodeURIComponent(att.id), { method: "DELETE" }).catch(function () {})
  })
  readComposeInlineImages(form).forEach(function (att) {
    if (att.id && !att.existing) fetch("/compose/attachments/" + encodeURIComponent(att.id), { method: "DELETE" }).catch(function () {})
  })
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

var _composeRecipientSuggestTimer = null
var _composeRecipientSuggestSeq = 0

function _composeRecipientInitials(name, email) {
  var text = String(name || email || "").trim()
  if (!text) return "?"
  var parts = text.split(/\s+/).filter(Boolean)
  if (parts.length > 1) return (parts[0].charAt(0) + parts[1].charAt(0)).toUpperCase()
  return text.slice(0, 2).toUpperCase()
}

function _composeRecipientSuggestionBox(field) {
  if (!field) return null
  var box = field.querySelector("[data-compose-recipient-suggestions]")
  if (box) return box
  box = document.createElement("div")
  box.className = "compose-recipient-suggestions"
  box.dataset.composeRecipientSuggestions = ""
  box.hidden = true
  field.appendChild(box)
  return box
}

function _hideComposeRecipientSuggestions(field) {
  var box = field && field.querySelector ? field.querySelector("[data-compose-recipient-suggestions]") : null
  if (!box) return
  box.hidden = true
  box.innerHTML = ""
  field.dataset.suggestionIndex = "-1"
}

function _activeComposeRecipientSuggestion(field) {
  var box = field && field.querySelector ? field.querySelector("[data-compose-recipient-suggestions]") : null
  if (!box || box.hidden) return null
  var idx = parseInt(field.dataset.suggestionIndex || "-1", 10)
  var items = box.querySelectorAll("[data-compose-recipient-suggestion]")
  if (idx < 0 || idx >= items.length) return null
  return items[idx]
}

function _setComposeRecipientSuggestionIndex(field, next) {
  var box = field && field.querySelector ? field.querySelector("[data-compose-recipient-suggestions]") : null
  if (!box || box.hidden) return
  var items = box.querySelectorAll("[data-compose-recipient-suggestion]")
  if (!items.length) return
  if (next < 0) next = items.length - 1
  if (next >= items.length) next = 0
  field.dataset.suggestionIndex = String(next)
  for (var i = 0; i < items.length; i++) items[i].dataset.active = i === next ? "true" : "false"
  items[next].scrollIntoView({ block: "nearest" })
}

function _selectComposeRecipientSuggestion(input, item) {
  if (!input || !item) return
  var field = input.closest("[data-compose-recipient-field]")
  if (!field) return
  var value = item.dataset.value || ""
  if (!value) return
  renderComposeRecipientField(field, _composeRecipientValues(field).concat([value]).join(", "))
  _hideComposeRecipientSuggestions(field)
  _markComposeDirty(_composeFormFrom(field))
  input.focus()
}

function _renderComposeRecipientSuggestions(input, results) {
  var field = input && input.closest ? input.closest("[data-compose-recipient-field]") : null
  var box = _composeRecipientSuggestionBox(field)
  if (!field || !box) return
  box.innerHTML = ""
  if (!results || !results.length) {
    _hideComposeRecipientSuggestions(field)
    return
  }
  results.forEach(function (item, idx) {
    var btn = document.createElement("button")
    btn.type = "button"
    btn.className = "compose-recipient-suggestion"
    btn.dataset.composeRecipientSuggestion = ""
    btn.dataset.value = item.value || item.email || ""
    btn.dataset.active = idx === 0 ? "true" : "false"
    btn.onmousedown = function (event) {
      event.preventDefault()
      _selectComposeRecipientSuggestion(input, btn)
    }

    var avatar = document.createElement("span")
    avatar.className = "compose-recipient-suggestion-avatar"
    avatar.textContent = _composeRecipientInitials(item.name, item.email)
    var main = document.createElement("span")
    main.className = "compose-recipient-suggestion-main"
    var name = document.createElement("span")
    name.className = "compose-recipient-suggestion-name"
    name.textContent = item.name || item.email || "Contact"
    var email = document.createElement("span")
    email.className = "compose-recipient-suggestion-email"
    email.textContent = item.email || ""
    main.appendChild(name)
    main.appendChild(email)
    btn.appendChild(avatar)
    btn.appendChild(main)
    box.appendChild(btn)
  })
  field.dataset.suggestionIndex = "0"
  box.hidden = false
}

function _scheduleComposeRecipientSuggestions(input) {
  var text = String((input && input.textContent) || "").trim()
  var field = input && input.closest ? input.closest("[data-compose-recipient-field]") : null
  if (!field) return
  if (_composeRecipientEmail(text).indexOf("@") >= 0 || text.length < 2) {
    _hideComposeRecipientSuggestions(field)
    return
  }
  if (_composeRecipientSuggestTimer) clearTimeout(_composeRecipientSuggestTimer)
  var seq = ++_composeRecipientSuggestSeq
  _composeRecipientSuggestTimer = setTimeout(function () {
    fetch("/api/contacts/search?q=" + encodeURIComponent(text), { headers: { "Accept": "application/json" } })
      .then(function (r) { return r.ok ? r.json() : { results: [] } })
      .then(function (data) {
        if (seq !== _composeRecipientSuggestSeq) return
        _renderComposeRecipientSuggestions(input, data.results || [])
      })
      .catch(function () { _hideComposeRecipientSuggestions(field) })
  }, 140)
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
  _hideComposeRecipientSuggestions(field)
  var text = input.textContent || ""
  if (!text.trim()) return
  var merged = _composeRecipientValues(field).concat(_splitComposeRecipients(text)).join(", ")
  renderComposeRecipientField(field, merged)
  _markComposeDirty(_composeFormFrom(field))
}

function handleComposeRecipientKeydown(event) {
  var input = event.currentTarget
  var field = input.closest("[data-compose-recipient-field]")
  var activeSuggestion = _activeComposeRecipientSuggestion(field)
  if (event.key === "ArrowDown" || event.key === "ArrowUp") {
    var box = field && field.querySelector ? field.querySelector("[data-compose-recipient-suggestions]") : null
    if (box && !box.hidden) {
      event.preventDefault()
      var idx = parseInt(field.dataset.suggestionIndex || "0", 10)
      _setComposeRecipientSuggestionIndex(field, idx + (event.key === "ArrowDown" ? 1 : -1))
      return
    }
  }
  if (event.key === "Enter" && activeSuggestion) {
    event.preventDefault()
    _selectComposeRecipientSuggestion(input, activeSuggestion)
    return
  }
  if (event.key === "Escape") {
    _hideComposeRecipientSuggestions(field)
    return
  }
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
  else _scheduleComposeRecipientSuggestions(input)
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

function _sanitizeComposeImageStyle(style) {
  var out = []
  var width = String(style || "").match(/(?:^|;)\s*width\s*:\s*(\d{1,4})(px|%)\s*(?:;|$)/i)
  if (width) out.push("width: " + Math.min(1200, Math.max(1, Number(width[1]))) + width[2])
  var transform = String(style || "").match(/(?:^|;)\s*transform\s*:[^;]*rotate\(\s*(-?\d{1,4})deg\s*\)/i)
  var rotate = transform ? "rotate(" + (Number(transform[1]) % 360) + "deg)" : ""
  var flip = /(?:^|;)\s*transform\s*:.*scaleX\(\s*-1\s*\)/i.test(String(style || "")) ? "scaleX(-1)" : ""
  if (rotate || flip) out.push("transform: " + [rotate, flip].filter(Boolean).join(" "))
  return out.join("; ")
}

function _sanitizeComposeStyle(style) {
  var safe = []
  var allowed = {
    "background": true, "background-color": true, "border": true, "border-bottom": true, "border-collapse": true,
    "border-left": true, "border-radius": true, "border-right": true, "border-spacing": true, "border-top": true,
    "color": true, "display": true, "font": true, "font-family": true, "font-size": true, "font-style": true,
    "font-weight": true, "height": true, "letter-spacing": true, "line-height": true, "margin": true,
    "margin-bottom": true, "margin-left": true, "margin-right": true, "margin-top": true, "max-height": true,
    "max-width": true, "min-height": true, "min-width": true, "mso-line-height-rule": true, "opacity": true,
    "overflow": true, "padding": true, "padding-bottom": true, "padding-left": true, "padding-right": true, "padding-top": true,
    "text-align": true, "text-decoration": true, "text-transform": true, "vertical-align": true, "white-space": true,
    "width": true, "word-break": true, "word-wrap": true
  }
  String(style || "").split(";").forEach(function (part) {
    var idx = part.indexOf(":")
    if (idx <= 0) return
    var prop = part.slice(0, idx).trim().toLowerCase()
    var value = part.slice(idx + 1).trim()
    if (!allowed[prop] || !value) return
    if (/expression\s*\(|javascript:|vbscript:|-moz-binding|behavior\s*:/i.test(value)) return
    if (/url\s*\(/i.test(value) && !/url\s*\(\s*['"]?https?:/i.test(value)) return
    safe.push(prop + ": " + value)
  })
  return safe.join("; ")
}

function _mergeComposeStyle(el, styleText) {
  var safeStyle = _sanitizeComposeStyle(styleText)
  if (!safeStyle) return
  var existing = el.getAttribute("style") || ""
  el.setAttribute("style", existing ? existing + "; " + safeStyle : safeStyle)
}

function _inlineComposeStyleRules(root) {
  var styles = root.querySelectorAll("style")
  for (var i = 0; i < styles.length; i++) {
    var css = styles[i].textContent || ""
    if (!css.trim()) continue
    var parserDoc = document.implementation.createHTMLDocument("")
    var styleEl = parserDoc.createElement("style")
    styleEl.textContent = css
    parserDoc.head.appendChild(styleEl)
    try {
      var rules = styleEl.sheet ? styleEl.sheet.cssRules : []
      for (var r = 0; r < rules.length; r++) {
        if (!rules[r].selectorText || !rules[r].style) continue
        var styleText = rules[r].style.cssText || ""
        var selectors = rules[r].selectorText.split(",")
        for (var s = 0; s < selectors.length; s++) {
          var selector = selectors[s].trim()
          if (!selector || /:(?!first-child|last-child)/.test(selector)) continue
          try {
            if (/^(html|body)$/i.test(selector)) {
              var body = root.querySelector("body")
              var targets = body ? body.children : root.children
              for (var t = 0; t < targets.length; t++) _mergeComposeStyle(targets[t], styleText)
              continue
            }
            var nodes = root.querySelectorAll(selector)
            for (var n = 0; n < nodes.length; n++) _mergeComposeStyle(nodes[n], styleText)
          } catch (e) {}
        }
      }
    } catch (e) {}
  }
}

function _sanitizeComposeHTML(html) {
  var template = document.createElement("template")
  template.innerHTML = html || ""
  _inlineComposeStyleRules(template.content)
  var blocked = template.content.querySelectorAll("script, style, head, title, iframe, object, embed, form, meta, link")
  for (var i = 0; i < blocked.length; i++) blocked[i].remove()
  var allowed = { A: true, B: true, BIG: true, BLOCKQUOTE: true, BR: true, CENTER: true, CODE: true, COL: true, COLGROUP: true, DIV: true, EM: true, FONT: true, H1: true, H2: true, H3: true, H4: true, H5: true, H6: true, HR: true, I: true, IMG: true, LI: true, OL: true, P: true, PRE: true, S: true, SMALL: true, SPAN: true, STRIKE: true, STRONG: true, SUB: true, SUP: true, TABLE: true, TBODY: true, TD: true, TFOOT: true, TH: true, THEAD: true, TR: true, U: true, UL: true }
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
      if (name.indexOf("on") === 0 || name === "class") {
        node.removeAttribute(attr.name)
        continue
      }
      if (tag === "IMG") {
        var imgAllowed = { src: true, alt: true, title: true, width: true, height: true, style: true, "data-compose-inline-image": true, "data-attachment-id": true, "data-existing-attachment-id": true, "data-content-id": true, "data-filename": true, "data-content-type": true, "data-size": true, "data-preview-url": true, "data-remote-src": true }
        if (!imgAllowed[name]) node.removeAttribute(attr.name)
        if (name === "style") {
          var safeStyle = node.hasAttribute("data-compose-inline-image") ? _sanitizeComposeImageStyle(attr.value) : _sanitizeComposeStyle(attr.value)
          if (safeStyle) node.setAttribute("style", safeStyle)
          else node.removeAttribute("style")
        }
        continue
      }
      if (name === "style") {
        var safeNodeStyle = _sanitizeComposeStyle(attr.value)
        if (safeNodeStyle) node.setAttribute("style", safeNodeStyle)
        else node.removeAttribute("style")
        continue
      }
      var globalAllowed = { align: true, bgcolor: true, border: true, cellpadding: true, cellspacing: true, colspan: true, dir: true, height: true, lang: true, role: true, rowspan: true, title: true, valign: true, width: true }
      if (tag !== "A" && !globalAllowed[name]) {
        node.removeAttribute(attr.name)
      } else if (tag === "A" && name !== "href" && name !== "target" && name !== "rel" && !globalAllowed[name]) {
        node.removeAttribute(attr.name)
      }
    }
    if (tag === "A") {
      var href = node.getAttribute("href") || ""
      if (!/^(https?:|mailto:|#)/i.test(href)) node.removeAttribute("href")
      node.setAttribute("rel", "noopener noreferrer")
      if (href && href.charAt(0) !== "#") node.setAttribute("target", "_blank")
    } else if (tag === "IMG") {
      var src = node.getAttribute("src") || ""
      var remoteSrc = node.getAttribute("data-remote-src") || ""
      if (!src && /^https?:/i.test(remoteSrc)) {
        src = remoteSrc
        node.setAttribute("src", src)
      }
      if (!/^(cid:|https?:|\/api\/attachments\/|\/api\/inline-content\/|\/compose\/attachments\/|\/api\/remote-assets\/)/i.test(src)) {
        node.remove()
        continue
      }
      node.removeAttribute("data-remote-src")
      var width = Number(node.getAttribute("width") || 0)
      if (width) node.setAttribute("width", String(Math.min(1200, Math.max(1, Math.round(width)))))
      var height = Number(node.getAttribute("height") || 0)
      if (height) node.setAttribute("height", String(Math.min(1200, Math.max(1, Math.round(height)))))
      if (!width && node.hasAttribute("width")) node.removeAttribute("width")
      if (!height && node.hasAttribute("height")) node.removeAttribute("height")
    }
  }
  return template.innerHTML
}

function _composeEditorText(editor) {
  if (!editor) return ""
  return (editor.innerText || "").replace(/\u00a0/g, " ").replace(/\n{3,}/g, "\n\n").trim()
}

function _composeHTMLForSending(editor) {
  if (!editor) return ""
  var template = document.createElement("template")
  template.innerHTML = editor.innerHTML || ""
  var imgs = template.content.querySelectorAll("img[data-compose-inline-image]")
  for (var i = 0; i < imgs.length; i++) {
    var cid = imgs[i].dataset.contentId || ""
    if (!cid) {
      imgs[i].remove()
      continue
    }
    imgs[i].setAttribute("src", "cid:" + cid)
    imgs[i].removeAttribute("data-compose-inline-image")
    imgs[i].removeAttribute("data-attachment-id")
    imgs[i].removeAttribute("data-existing-attachment-id")
    imgs[i].removeAttribute("data-content-id")
    imgs[i].removeAttribute("data-filename")
    imgs[i].removeAttribute("data-content-type")
    imgs[i].removeAttribute("data-size")
    imgs[i].removeAttribute("data-preview-url")
    imgs[i].classList.remove("compose-inline-image-selected")
  }
  return _sanitizeComposeHTML(template.innerHTML).trim()
}

function _composeHTMLForEditor(html, inlineImages) {
  var template = document.createElement("template")
  template.innerHTML = html || ""
  var byCID = {}
  for (var i = 0; inlineImages && i < inlineImages.length; i++) {
    if (inlineImages[i].content_id) byCID[inlineImages[i].content_id] = inlineImages[i]
  }
  var imgs = template.content.querySelectorAll("img[src]")
  for (var j = 0; j < imgs.length; j++) {
    var src = imgs[j].getAttribute("src") || ""
    if (src.toLowerCase().indexOf("cid:") !== 0) continue
    var cid = src.slice(4)
    var att = byCID[cid]
    if (!att || !att.preview_url) continue
    imgs[j].dataset.composeInlineImage = ""
    imgs[j].dataset.attachmentId = att.id || ""
    imgs[j].dataset.existingAttachmentId = att.existing ? String(att.id || "") : ""
    imgs[j].dataset.contentId = cid
    imgs[j].dataset.filename = att.filename || "image"
    imgs[j].dataset.contentType = att.content_type || "image/png"
    imgs[j].dataset.size = String(att.size || 0)
    imgs[j].dataset.previewUrl = att.preview_url || ""
    imgs[j].src = att.preview_url
    if (!imgs[j].alt) imgs[j].alt = att.filename || "Inline image"
  }
  return template.innerHTML
}

function syncComposeEditor(editor) {
  if (!editor) return
  var form = _composeFormFrom(editor)
  if (!form) return
  var plain = form.querySelector('textarea[name="body"]')
  var html = form.querySelector('textarea[name="html_body"]')
  if (plain) plain.value = _composeEditorText(editor)
  if (html) html.value = _composeHTMLForSending(editor)
  syncComposeInlineImageInputs(form)
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

function _setComposeEditorValue(form, plain, html, inlineImages) {
  if (!form) return
  var editor = form.querySelector("[data-compose-editor]")
  var plainField = form.querySelector('textarea[name="body"]')
  var htmlField = form.querySelector('textarea[name="html_body"]')
  if (plainField) plainField.value = plain || ""
  if (htmlField) htmlField.value = html || ""
  if (editor) {
    editor.innerHTML = html ? _sanitizeComposeHTML(_composeHTMLForEditor(html, inlineImages || [])) : _composePlainToHTML(plain || "")
  }
  syncComposeInlineImageInputs(form)
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
  var pastedImages = _composeImageFilesFromClipboard(clipboard)
  if (pastedImages.length) {
    event.preventDefault()
    _saveComposeSelection(editor)
    _showComposeImageDropChoice(_composeFormFrom(editor), pastedImages)
    return
  }
  event.preventDefault()
  var html = clipboard.getData("text/html")
  var text = clipboard.getData("text/plain")
  document.execCommand("insertHTML", false, html ? _sanitizeComposeHTML(html) : _composePlainToHTML(text))
  syncComposeEditor(editor)
}

document.addEventListener("keydown", function (event) {
  var editor = event.target && event.target.closest ? event.target.closest("[data-compose-editor]") : null
  var form = event.target && event.target.closest ? event.target.closest("#compose-form, #compose-pane-form") : null
  if (form && (event.ctrlKey || event.metaKey)) {
    if (event.key === "Enter") {
      event.preventDefault()
      sendCompose(form.id === "compose-pane-form")
      return
    }
    if (event.key.toLowerCase() === "s") {
      event.preventDefault()
      saveComposeDraft(form.id === "compose-pane-form", false)
      return
    }
  }
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
  if (event.target && event.target.closest && (event.target.closest("[data-compose-toolbar] button") || event.target.closest(".compose-inline-image-toolbar"))) {
    event.preventDefault()
  }
})

document.addEventListener("click", function (event) {
  if (!event.target || !event.target.closest) return
  var img = event.target.closest("[data-compose-editor] img[data-compose-inline-image]")
  if (img) {
    event.preventDefault()
    selectComposeInlineImage(img)
    return
  }
  if (event.target.closest(".compose-inline-image-toolbar")) return
  hideComposeInlineImageToolbar()
})

window.addEventListener("resize", positionComposeInlineImageToolbar)
window.addEventListener("scroll", positionComposeInlineImageToolbar, true)

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
  if (form.querySelector("[data-compose-attachment]")) return true
  if (form.querySelector("[data-compose-editor] img[data-compose-inline-image]")) return true
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
  scheduleComposeAutosave(form)
}

function composeAutosaveEligible(form) {
  if (!form || form.dataset.composeDirty !== "true") return false
  if (composeAutosaveSetting("compose_autosave_enabled", "true") === "false") return false
  if (_composePendingUploads(form) > 0 || form.dataset.composeSending === "true") return false
  _syncComposeFormEditor(form)
  var conditions = composeAutosaveConditions()
  if (!conditions.length) return false
  var text = ""
  var fields = form.querySelectorAll('input[name="to"], input[name="cc"], input[name="bcc"], input[name="subject"], textarea[name="body"]')
  for (var i = 0; i < fields.length; i++) text += " " + (fields[i].value || "")
  var recipientInputs = form.querySelectorAll('[data-recipient-name="to"] [data-compose-recipient-input]')
  for (var r = 0; r < recipientInputs.length; r++) text += " " + (recipientInputs[r].textContent || "")
  var checks = {
    chars: text.replace(/\s+/g, "").length >= composeAutosaveMinChars(),
    attachment: !!form.querySelector("[data-compose-attachment], [data-compose-editor] img[data-compose-inline-image]"),
    to: !!String((form.querySelector('input[name="to"]') || {}).value || "").trim() || !!String((form.querySelector('[data-recipient-name="to"] [data-compose-recipient-input]') || {}).textContent || "").trim()
  }
  for (var c = 0; c < conditions.length; c++) {
    if (checks[conditions[c]]) return true
  }
  return false
}

function scheduleComposeAutosave(form) {
  if (!form || !composeAutosaveEligible(form)) return
  clearTimeout(form._composeAutosaveTimer)
  form._composeAutosaveTimer = setTimeout(function () {
    if (!composeAutosaveEligible(form) || form._composeAutosaveInFlight) return
    form._composeAutosaveInFlight = true
    saveComposeDraft(form.id === "compose-pane-form", true).finally(function () {
      form._composeAutosaveInFlight = false
    })
  }, composeAutosaveDebounceMS())
}

function cancelComposeAutosave(form) {
  if (form && form._composeAutosaveTimer) clearTimeout(form._composeAutosaveTimer)
}

function composeAutosaveSetting(key, fallback) {
  return window.GoferSettings ? (GoferSettings.get(key) || fallback) : fallback
}

function composeAutosaveConditions() {
  var raw = composeAutosaveSetting("compose_autosave_conditions", "chars,attachment")
  return String(raw || "").split(",").map(function (part) { return part.trim() }).filter(Boolean)
}

function composeAutosaveMinChars() {
  var n = parseInt(composeAutosaveSetting("compose_autosave_min_chars", "30"), 10)
  if (isNaN(n) || n < 1) return 30
  return Math.min(1000, n)
}

function composeAutosaveDebounceMS() {
  var seconds = parseInt(composeAutosaveSetting("compose_autosave_debounce", "5"), 10)
  if (isNaN(seconds) || seconds < 1) seconds = 5
  return Math.min(60, seconds) * 1000
}

function _composeSendButton(form) {
  if (!form) return null
  return document.getElementById(form.id === "compose-pane-form" ? "compose-pane-send-btn" : "compose-send-btn")
}

function _composePendingUploads(form) {
  return Number((form && form.dataset.composeUploadsPending) || 0)
}

function _setComposeSending(form, sending) {
  if (!form) return
  form.dataset.composeSending = sending ? "true" : "false"
  updateComposeSendState(form)
}

function updateComposeSendState(form) {
  if (!form) return
  var button = _composeSendButton(form)
  if (!button) return
  var pending = _composePendingUploads(form)
  var sending = form.dataset.composeSending === "true"
  var disabled = pending > 0 || sending
  button.disabled = disabled
  button.setAttribute("aria-busy", sending || pending > 0 ? "true" : "false")
  button.classList.toggle("opacity-60", disabled)
  button.classList.toggle("cursor-not-allowed", disabled)
  if (sending) {
    button.title = "Sending..."
  } else if (pending > 0) {
    button.title = "Waiting for uploads to finish"
  } else {
    button.removeAttribute("title")
  }
}

function changeComposeUploadCount(form, delta, label) {
  if (!form) return
  var pending = Math.max(0, _composePendingUploads(form) + delta)
  form.dataset.composeUploadsPending = String(pending)
  updateComposeSendState(form)
}

function composeUploadFailed(form, message) {
  if (form) form.dataset.composeUploadFailed = "true"
  showSendStatus("failed", message || "Upload failed")
}

function finishComposeSendSuccess(state) {
  if (!state) return
  var form = document.getElementById(state.formId)
  if (form) _setComposeSending(form, false)
  _composeSendState = null
  setTimeout(function () {
    if (state.fromPane) {
      setMailViewEmpty()
      _updateComposeBtn(false)
    } else {
      resetComposeForm(false)
      if (window.tui && window.tui.dialog) window.tui.dialog.close("compose-dialog")
      _updateComposeBtn(false)
    }
  }, 300)
}

function handleComposeSendResult(status) {
  if (!_composeSendState) return
  var state = _composeSendState
  var form = document.getElementById(state.formId)
  if (status === "sent") {
    finishComposeSendSuccess(state)
    return
  }
  if (form) {
    _setComposeSending(form, false)
    form.dataset.composeDirty = "true"
  }
  _composeSendState = null
}

function saveComposeDraft(fromPane, auto) {
  var form = document.getElementById(fromPane ? "compose-pane-form" : "compose-form")
  if (form && auto && form._composeManualDraftSave) return Promise.resolve(false)
  finalizeComposeRecipients(form)
  if (!form || !_composeHasDraftContent(form)) {
    if (!auto) _setComposeDraftButtonState(form, "empty")
    return Promise.resolve(false)
  }
  if (!validateComposeMessageSize(form)) {
    if (!auto) _setComposeDraftButtonState(form, "failed")
    return Promise.resolve(false)
  }
  _setComposeDraftButtonState(form, "saving")
  if (form) form._composeManualDraftSave = !auto

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
    form._composeManualDraftSave = false
    return true
  }).catch(function (err) {
    form.dataset.composeDirty = "true"
    _setComposeDraftButtonState(form, "failed")
    form._composeManualDraftSave = false
    showSendStatus("failed", err && err.message ? err.message : "Failed to save draft")
    return false
  })
}

function saveActiveComposeDraft(auto) {
  var form = _composeFormFrom(_activeComposeEditor)
  saveComposeDraft(form && form.id === "compose-pane-form", !!auto)
}

function triggerComposeAttachmentUpload(el) {
  var form = _composeFormFrom(el)
  var input = form && form.querySelector("[data-compose-attachment-input]")
  if (input) input.click()
}

function triggerComposeInlineImageUpload(el) {
  var form = _composeFormFrom(el)
  var editor = _composeEditorFrom(el)
  if (editor) _saveComposeSelection(editor)
  var input = form && form.querySelector("[data-compose-inline-input]")
  if (input) input.click()
}

function uploadComposeAttachments(files, input) {
  var form = _composeFormFrom(input)
  if (!form || !files || !files.length) return
  Array.prototype.forEach.call(files, function (file) {
    if (!validateComposeUploadFile(form, file)) return
    var pendingChip = addComposePendingAttachment(form, file, false)
    changeComposeUploadCount(form, 1)
    uploadComposeAttachmentFile(file, pendingChip)
      .then(function (att) {
        if (pendingChip && pendingChip._composeUploadCancelled) return
        removeComposePendingAttachment(pendingChip)
        addComposeAttachment(form, att)
        _markComposeDirty(form)
        changeComposeUploadCount(form, -1)
      })
      .catch(function (err) {
        if (pendingChip && pendingChip._composeUploadCancelled) {
          removeComposePendingAttachment(pendingChip)
          changeComposeUploadCount(form, -1, "cancelled")
          return
        }
        failComposePendingAttachment(pendingChip)
        changeComposeUploadCount(form, -1, "failed")
        composeUploadFailed(form, composeUploadErrorMessage("attach", file, err))
      })
  })
  if (input && "value" in input) input.value = ""
}

function uploadComposeInlineImages(files, input) {
  var form = _composeFormFrom(input)
  if (!form || !files || !files.length) return
  Array.prototype.forEach.call(files, function (file) {
    if (!_composeFileLooksImage(file)) {
      composeUploadFailed(form, "Could not insert " + ((file && file.name) || "file") + " inline: only image files can be inserted inline. Attach this file instead.")
      return
    }
    if (!validateComposeUploadFile(form, file)) return
    var pendingChip = addComposePendingAttachment(form, file, true)
    changeComposeUploadCount(form, 1)
    uploadComposeAttachmentFile(file, pendingChip)
      .then(function (att) {
        if (pendingChip && pendingChip._composeUploadCancelled) return
        if (!att.preview_url) throw new Error("That image type cannot be previewed inline")
        removeComposePendingAttachment(pendingChip)
        insertComposeInlineImage(form, att)
        _markComposeDirty(form)
        changeComposeUploadCount(form, -1)
      })
      .catch(function (err) {
        if (pendingChip && pendingChip._composeUploadCancelled) {
          removeComposePendingAttachment(pendingChip)
          changeComposeUploadCount(form, -1, "cancelled")
          return
        }
        failComposePendingAttachment(pendingChip)
        changeComposeUploadCount(form, -1, "failed")
        composeUploadFailed(form, composeUploadErrorMessage("insert", file, err))
      })
  })
  if (input && "value" in input) input.value = ""
}

var _composeDropForm = null
var _composeDropClearTimer = null
var _composeDropChoice = null
var COMPOSE_ATTACHMENT_MAX_BYTES = 25 * 1024 * 1024
var COMPOSE_MESSAGE_MAX_BYTES = 35 * 1024 * 1024

function _composeUploadLimitLabel() {
  return formatComposeAttachmentSize(COMPOSE_ATTACHMENT_MAX_BYTES)
}

function _composeMessageLimitLabel() {
  return formatComposeAttachmentSize(COMPOSE_MESSAGE_MAX_BYTES)
}

function estimateComposeEncodedSize(form) {
  if (!form) return 0
  _syncComposeFormEditor(form)
  var body = form.querySelector('textarea[name="body"]')
  var html = form.querySelector('textarea[name="html_body"]')
  var total = String((body && body.value) || "").length + String((html && html.value) || "").length + 4096
  function addAttachment(att) {
    var size = Number((att && att.size) || 0)
    var encoded = Math.ceil(size / 3) * 4
    total += encoded + Math.floor(encoded / 76) * 2 + 1024
  }
  readComposeAttachments(form).forEach(addAttachment)
  readComposeInlineImages(form).forEach(addAttachment)
  return total
}

function validateComposeMessageSize(form) {
  var estimated = estimateComposeEncodedSize(form)
  if (estimated <= COMPOSE_MESSAGE_MAX_BYTES) return true
  showSendStatus("failed", "Message is too large: estimated " + formatComposeAttachmentSize(estimated) + " after encoding. The send limit is " + _composeMessageLimitLabel() + " total, including attachments.")
  return false
}

function validateComposeUploadFile(form, file) {
  if (!file) return false
  if (file.size > COMPOSE_ATTACHMENT_MAX_BYTES) {
    composeUploadFailed(form, (file.name || "File") + " is too large: " + formatComposeAttachmentSize(file.size) + ". The limit is " + _composeUploadLimitLabel() + " per file.")
    return false
  }
  return true
}

function composeUploadErrorMessage(action, file, err) {
  var name = (file && file.name) || "file"
  var verb = action === "insert" ? "insert" : "attach"
  var reason = err && err.message ? String(err.message) : "The upload did not complete."
  if (/too large/i.test(reason)) {
    return "Could not " + verb + " " + name + ": the file exceeds the " + _composeUploadLimitLabel() + " per-file limit."
  }
  if (/cancel/i.test(reason)) return "Could not " + verb + " " + name + ": the upload was cancelled."
  if (/previewed inline/i.test(reason)) return "Could not insert " + name + " inline: this image type cannot be previewed inline. Attach it instead."
  if (/network|failed/i.test(reason)) return "Could not " + verb + " " + name + ": upload failed before the server accepted it. Check your connection and try again."
  return "Could not " + verb + " " + name + ": " + reason
}

function uploadComposeAttachmentFile(file, pendingChip) {
  return new Promise(function (resolve, reject) {
    var data = new FormData()
    data.append("attachment", file)
    var xhr = new XMLHttpRequest()
    if (pendingChip) {
      pendingChip._composeUploadXhr = xhr
      pendingChip._composeCancelUpload = function () {
        pendingChip._composeUploadCancelled = true
        xhr.abort()
      }
    }
    xhr.open("POST", "/compose/attachments")
    xhr.upload.onprogress = function (event) {
      if (event.lengthComputable) updateComposePendingAttachment(pendingChip, Math.round((event.loaded / event.total) * 100))
    }
    xhr.onload = function () {
      var payload = {}
      try { payload = JSON.parse(xhr.responseText || "{}") } catch (e) {}
      if (xhr.status < 200 || xhr.status >= 300) {
        reject(new Error(payload.error || "Upload failed"))
        return
      }
      resolve(payload)
    }
    xhr.onerror = function () { reject(new Error("Upload failed")) }
    xhr.onabort = function () { reject(new Error("Upload cancelled")) }
    xhr.send(data)
  })
}

function _composeClipboardFileName(file, index) {
  if (file && file.name) return file.name
  var ext = "png"
  var type = String((file && file.type) || "").toLowerCase()
  if (type === "image/jpeg") ext = "jpg"
  else if (type === "image/gif") ext = "gif"
  else if (type === "image/webp") ext = "webp"
  else if (type === "image/svg+xml") ext = "svg"
  return "pasted-image" + (index > 0 ? "-" + (index + 1) : "") + "." + ext
}

function _composeImageFilesFromClipboard(clipboard) {
  var files = []
  var items = clipboard && clipboard.items
  for (var i = 0; items && i < items.length; i++) {
    if (!items[i] || items[i].kind !== "file" || String(items[i].type || "").indexOf("image/") !== 0) continue
    var file = items[i].getAsFile && items[i].getAsFile()
    if (!file) continue
    if (!file.name && window.File) {
      file = new File([file], _composeClipboardFileName(file, files.length), { type: file.type || "image/png" })
    }
    files.push(file)
  }
  return files
}

function _composeEventHasFiles(event) {
  var types = event && event.dataTransfer && event.dataTransfer.types
  if (!types) return false
  for (var i = 0; i < types.length; i++) {
    if (types[i] === "Files") return true
  }
  return false
}

function _composeFilesFromTransfer(dataTransfer) {
  var files = dataTransfer && dataTransfer.files
  if (!files || !files.length) return []
  return Array.prototype.slice.call(files).filter(function (file) { return !!file })
}

function _composeFileLooksImage(file) {
  var type = String((file && file.type) || "").toLowerCase()
  var name = String((file && file.name) || "").toLowerCase()
  return type.indexOf("image/") === 0 || /\.(png|jpe?g|svg|webp|gif|bmp|ico)$/.test(name)
}

function _setComposeDropActive(form, active) {
  if (!form) return
  form.classList.toggle("compose-drop-active", !!active)
  var pane = form.id === "compose-pane-form" && form.closest ? form.closest("[data-compose-pane]") : null
  if (pane) pane.classList.toggle("compose-drop-active", !!active)
  if (active) _composeDropForm = form
  else if (_composeDropForm === form) _composeDropForm = null
}

function _composeDropFormFromEvent(event) {
  if (!event || !event.target || !event.target.closest) return null
  var form = event.target.closest("#compose-form, #compose-pane-form")
  if (form) return form
  var pane = event.target.closest("[data-compose-pane]")
  if (pane) return pane.querySelector("#compose-pane-form")
  var dialog = event.target.closest("#compose-dialog")
  if (dialog) return dialog.querySelector("#compose-form")
  return null
}

function _clearComposeDropActiveSoon(form) {
  clearTimeout(_composeDropClearTimer)
  _composeDropClearTimer = setTimeout(function () {
    _setComposeDropActive(form || _composeDropForm, false)
  }, 80)
}

function _saveComposeDropSelection(form, event) {
  var editor = form && form.querySelector("[data-compose-editor]")
  if (!editor) return
  var target = event && event.target && event.target.closest ? event.target.closest("[data-compose-editor]") : null
  var range = null
  if (target === editor) {
    if (document.caretRangeFromPoint) {
      range = document.caretRangeFromPoint(event.clientX, event.clientY)
    } else if (document.caretPositionFromPoint) {
      var pos = document.caretPositionFromPoint(event.clientX, event.clientY)
      if (pos) {
        range = document.createRange()
        range.setStart(pos.offsetNode, pos.offset)
        range.collapse(true)
      }
    }
  }
  if (range && editor.contains(range.startContainer)) {
    var selection = window.getSelection && window.getSelection()
    if (selection) {
      selection.removeAllRanges()
      selection.addRange(range)
    }
    editor._composeRange = range.cloneRange()
    return
  }
  _saveComposeSelection(editor)
  if (!editor._composeRange) {
    range = document.createRange()
    range.selectNodeContents(editor)
    range.collapse(false)
    editor._composeRange = range
  }
}

function _closeComposeDropChoice() {
  var choice = _composeDropChoice
  _composeDropChoice = null
  if (!choice) return
  if (choice._composeKeyHandler) document.removeEventListener("keydown", choice._composeKeyHandler)
  choice.remove()
}

function _composeDropChoiceButton(label, action) {
  var button = document.createElement("button")
  button.type = "button"
  button.textContent = label
  button.dataset.composeDropAction = action
  return button
}

function _showComposeImageDropChoice(form, images) {
  _closeComposeDropChoice()
  if (!form || !images || !images.length) return
  var choice = document.createElement("div")
  choice.className = "compose-drop-choice-backdrop"
  choice.setAttribute("role", "presentation")

  var panel = document.createElement("div")
  panel.className = "compose-drop-choice"
  panel.setAttribute("role", "dialog")
  choice.setAttribute("aria-label", "Choose how to add dropped images")

  var label = document.createElement("span")
  label.textContent = images.length === 1 ? "Add image as" : "Add " + images.length + " images as"
  panel.appendChild(label)
  var limit = document.createElement("span")
  limit.className = "compose-drop-choice-limit"
  limit.textContent = "Max " + _composeUploadLimitLabel() + " per file, " + _composeMessageLimitLabel() + " total"
  panel.appendChild(limit)
  panel.appendChild(_composeDropChoiceButton("Insert inline", "inline"))
  panel.appendChild(_composeDropChoiceButton("Attach", "attach"))
  panel.appendChild(_composeDropChoiceButton("Cancel", "cancel"))
  choice.appendChild(panel)

  panel.addEventListener("mousedown", function (e) {
    e.preventDefault()
    e.stopPropagation()
  })
  choice.addEventListener("click", function (e) {
    if (e.target === choice) _closeComposeDropChoice()
  })
  choice.addEventListener("click", function (e) {
    var button = e.target && e.target.closest ? e.target.closest("[data-compose-drop-action]") : null
    if (!button) return
    e.preventDefault()
    var action = button.dataset.composeDropAction
    _closeComposeDropChoice()
    if (action === "inline") uploadComposeInlineImages(images, form)
    else if (action === "attach") uploadComposeAttachments(images, form)
  })
  choice._composeKeyHandler = function (e) {
    if (e.key === "Escape") _closeComposeDropChoice()
  }
  document.addEventListener("keydown", choice._composeKeyHandler)

  document.body.appendChild(choice)
  _composeDropChoice = choice
}

function _handleComposeDroppedFiles(form, files, event) {
  if (!form || !files || !files.length) return
  var images = []
  var attachments = []
  for (var i = 0; i < files.length; i++) {
    if (_composeFileLooksImage(files[i])) images.push(files[i])
    else attachments.push(files[i])
  }
  if (attachments.length) uploadComposeAttachments(attachments, form)
  if (images.length) _showComposeImageDropChoice(form, images, event)
}

document.addEventListener("dragenter", function (event) {
  if (!_composeEventHasFiles(event) || !event.target || !event.target.closest) return
  var form = _composeDropFormFromEvent(event)
  if (!form) return
  event.preventDefault()
  clearTimeout(_composeDropClearTimer)
  if (_composeDropForm && _composeDropForm !== form) _setComposeDropActive(_composeDropForm, false)
  _setComposeDropActive(form, true)
})

document.addEventListener("dragover", function (event) {
  if (!_composeEventHasFiles(event) || !event.target || !event.target.closest) return
  var form = _composeDropFormFromEvent(event)
  if (!form) return
  event.preventDefault()
  clearTimeout(_composeDropClearTimer)
  _setComposeDropActive(form, true)
  if (event.dataTransfer) event.dataTransfer.dropEffect = "copy"
})

document.addEventListener("dragleave", function (event) {
  if (!_composeEventHasFiles(event)) return
  _clearComposeDropActiveSoon(_composeDropForm)
})

document.addEventListener("drop", function (event) {
  if (!_composeEventHasFiles(event) || !event.target || !event.target.closest) return
  var form = _composeDropFormFromEvent(event)
  if (!form) {
    if (_composeDropForm) event.preventDefault()
    _setComposeDropActive(_composeDropForm, false)
    return
  }
  event.preventDefault()
  event.stopPropagation()
  clearTimeout(_composeDropClearTimer)
  _setComposeDropActive(form, false)
  _saveComposeDropSelection(form, event)
  _handleComposeDroppedFiles(form, _composeFilesFromTransfer(event.dataTransfer), event)
})

function composeInlineContentID(att) {
  var id = att && att.id ? String(att.id) : String(Date.now())
  return "inline-" + id.replace(/[^a-z0-9._-]/gi, "") + "@gofer"
}

function insertComposeInlineImage(form, att) {
  var editor = form && form.querySelector("[data-compose-editor]")
  if (!editor) return
  editor.focus()
  _restoreComposeSelection(editor)

  var img = document.createElement("img")
  img.src = att.preview_url
  img.alt = att.filename || "Inline image"
  img.loading = "lazy"
  img.dataset.composeInlineImage = ""
  img.dataset.attachmentId = att.id || ""
  img.dataset.existingAttachmentId = att.existing ? String(att.id || "") : ""
  img.dataset.contentId = att.content_id || composeInlineContentID(att)
  img.dataset.filename = att.filename || "image"
  img.dataset.contentType = att.content_type || "image/png"
  img.dataset.size = String(att.size || 0)
  img.dataset.previewUrl = att.preview_url || ""
  img.onload = function () { setComposeInlineImageDefaultSize(img) }

  var selection = window.getSelection && window.getSelection()
  if (selection && selection.rangeCount) {
    var range = selection.getRangeAt(0)
    range.deleteContents()
    range.insertNode(img)
    var spacer = document.createTextNode(" ")
    img.parentNode.insertBefore(spacer, img.nextSibling)
    range.setStartAfter(spacer)
    range.setEndAfter(spacer)
    selection.removeAllRanges()
    selection.addRange(range)
  } else {
    editor.appendChild(img)
    editor.appendChild(document.createTextNode(" "))
  }
  syncComposeEditor(editor)
  updateComposeToolbar(editor)
  selectComposeInlineImage(img)
  if (img.complete) setComposeInlineImageDefaultSize(img)
}

var _selectedComposeInlineImage = null
var _composeInlineImageToolbar = null
var COMPOSE_INLINE_IMAGE_MIN_WIDTH = 120
var COMPOSE_INLINE_IMAGE_MAX_WIDTH = 1440
var COMPOSE_INLINE_IMAGE_WIDTH_STEP = 80
var COMPOSE_INLINE_IMAGE_DEFAULT_SIZE = 450

function _composeInlineImageEditor(img) {
  return img && img.closest ? img.closest("[data-compose-editor]") : null
}

function _composeInlineImageCurrentWidth(img) {
  var width = Number(img && img.getAttribute("width"))
  if (!width && img) width = Math.round(img.getBoundingClientRect().width)
  if (!width) width = 360
  return width
}

function setComposeInlineImageDefaultSize(img) {
  if (!img || img.getAttribute("width") || img.getAttribute("height")) return
  var naturalWidth = img.naturalWidth || 0
  var naturalHeight = img.naturalHeight || 0
  var width = COMPOSE_INLINE_IMAGE_DEFAULT_SIZE
  if (naturalWidth && naturalHeight && naturalHeight > naturalWidth) {
    width = Math.round((naturalWidth / naturalHeight) * COMPOSE_INLINE_IMAGE_DEFAULT_SIZE)
  }
  width = Math.max(COMPOSE_INLINE_IMAGE_MIN_WIDTH, Math.min(COMPOSE_INLINE_IMAGE_MAX_WIDTH, width))
  img.setAttribute("width", String(width))
  img.removeAttribute("height")
  var editor = _composeInlineImageEditor(img)
  if (editor) syncComposeEditor(editor)
  updateComposeInlineImageToolbarState()
}

function _composeInlineImageRotate(img) {
  var match = String((img && img.style && img.style.transform) || "").match(/rotate\((-?\d+)deg\)/i)
  return match ? Number(match[1]) : 0
}

function _composeInlineImageFlipped(img) {
  return /scaleX\(\s*-1\s*\)/i.test(String((img && img.style && img.style.transform) || ""))
}

function _composeInlineImageToolbarButton(action, label, path) {
  var button = document.createElement("button")
  button.type = "button"
  button.dataset.composeInlineAction = action
  button.setAttribute("aria-label", label)
  var svg = document.createElementNS("http://www.w3.org/2000/svg", "svg")
  svg.setAttribute("viewBox", "0 0 24 24")
  svg.setAttribute("aria-hidden", "true")
  var iconPath = document.createElementNS("http://www.w3.org/2000/svg", "path")
  iconPath.setAttribute("d", path)
  svg.appendChild(iconPath)
  var text = document.createElement("span")
  text.textContent = label
  button.appendChild(svg)
  button.appendChild(text)
  return button
}

function _ensureComposeInlineImageToolbar() {
  if (_composeInlineImageToolbar) return _composeInlineImageToolbar
  var toolbar = document.createElement("div")
  toolbar.className = "compose-inline-image-toolbar hidden"
  toolbar.setAttribute("contenteditable", "false")
  toolbar.appendChild(_composeInlineImageToolbarButton("smaller", "Smaller", "M5 12h14"))
  toolbar.appendChild(_composeInlineImageToolbarButton("larger", "Larger", "M12 5v14M5 12h14"))
  toolbar.appendChild(_composeInlineImageToolbarButton("rotate", "Rotate", "M21 12a9 9 0 1 1-2.64-6.36M21 3v6h-6"))
  toolbar.appendChild(_composeInlineImageToolbarButton("flip", "Flip", "M12 3v18M5 7l5 5-5 5V7Zm14 0l-5 5 5 5V7Z"))
  toolbar.appendChild(_composeInlineImageToolbarButton("attach", "Attach", "M21.44 11.05 12 20.5a6 6 0 0 1-8.49-8.49l9.9-9.9a4 4 0 0 1 5.66 5.66l-9.9 9.9a2 2 0 1 1-2.83-2.83l8.49-8.49"))
  toolbar.appendChild(_composeInlineImageToolbarButton("remove", "Remove", "M18 6 6 18M6 6l12 12"))
  toolbar.addEventListener("mousedown", function (event) { event.preventDefault() })
  toolbar.addEventListener("click", function (event) {
    var button = event.target && event.target.closest ? event.target.closest("[data-compose-inline-action]") : null
    if (!button) return
    event.preventDefault()
    if (button.disabled) return
    applyComposeInlineImageAction(button.dataset.composeInlineAction)
  })
  document.body.appendChild(toolbar)
  _composeInlineImageToolbar = toolbar
  return toolbar
}

function positionComposeInlineImageToolbar() {
  if (!_selectedComposeInlineImage || !_selectedComposeInlineImage.isConnected) return hideComposeInlineImageToolbar()
  var toolbar = _ensureComposeInlineImageToolbar()
  var rect = _selectedComposeInlineImage.getBoundingClientRect()
  var top = Math.max(8, rect.top - toolbar.offsetHeight - 8)
  var left = Math.max(8, Math.min(rect.left, window.innerWidth - toolbar.offsetWidth - 8))
  toolbar.style.top = top + "px"
  toolbar.style.left = left + "px"
}

function updateComposeInlineImageToolbarState() {
  if (!_composeInlineImageToolbar || !_selectedComposeInlineImage) return
  var width = _composeInlineImageCurrentWidth(_selectedComposeInlineImage)
  var smaller = _composeInlineImageToolbar.querySelector('[data-compose-inline-action="smaller"]')
  var larger = _composeInlineImageToolbar.querySelector('[data-compose-inline-action="larger"]')
  if (smaller) smaller.disabled = width <= COMPOSE_INLINE_IMAGE_MIN_WIDTH
  if (larger) larger.disabled = width >= COMPOSE_INLINE_IMAGE_MAX_WIDTH
}

function selectComposeInlineImage(img) {
  if (!img || !img.matches || !img.matches("img[data-compose-inline-image]")) return
  if (_selectedComposeInlineImage && _selectedComposeInlineImage !== img) {
    _selectedComposeInlineImage.classList.remove("compose-inline-image-selected")
  }
  _selectedComposeInlineImage = img
  img.classList.add("compose-inline-image-selected")
  var toolbar = _ensureComposeInlineImageToolbar()
  toolbar.classList.remove("hidden")
  positionComposeInlineImageToolbar()
  updateComposeInlineImageToolbarState()
}

function hideComposeInlineImageToolbar() {
  if (_selectedComposeInlineImage) _selectedComposeInlineImage.classList.remove("compose-inline-image-selected")
  _selectedComposeInlineImage = null
  if (_composeInlineImageToolbar) _composeInlineImageToolbar.classList.add("hidden")
}

function _syncSelectedComposeInlineImage() {
  var img = _selectedComposeInlineImage
  var editor = _composeInlineImageEditor(img)
  if (!editor) return
  syncComposeEditor(editor)
  _markComposeDirty(_composeFormFrom(editor))
  positionComposeInlineImageToolbar()
  updateComposeInlineImageToolbarState()
}

function applyComposeInlineImageAction(action) {
  var img = _selectedComposeInlineImage
  if (!img) return
  if (action === "smaller" || action === "larger") {
    var width = _composeInlineImageCurrentWidth(img) + (action === "larger" ? COMPOSE_INLINE_IMAGE_WIDTH_STEP : -COMPOSE_INLINE_IMAGE_WIDTH_STEP)
    width = Math.max(COMPOSE_INLINE_IMAGE_MIN_WIDTH, Math.min(COMPOSE_INLINE_IMAGE_MAX_WIDTH, width))
    img.setAttribute("width", String(width))
    img.removeAttribute("height")
    _syncSelectedComposeInlineImage()
  } else if (action === "rotate") {
    var rotate = (_composeInlineImageRotate(img) + 90) % 360
    var flipped = _composeInlineImageFlipped(img)
    img.style.transform = [rotate ? "rotate(" + rotate + "deg)" : "", flipped ? "scaleX(-1)" : ""].filter(Boolean).join(" ")
    _syncSelectedComposeInlineImage()
  } else if (action === "flip") {
    var rotateNow = _composeInlineImageRotate(img)
    var flipNow = !_composeInlineImageFlipped(img)
    img.style.transform = [rotateNow ? "rotate(" + rotateNow + "deg)" : "", flipNow ? "scaleX(-1)" : ""].filter(Boolean).join(" ")
    _syncSelectedComposeInlineImage()
  } else if (action === "remove") {
    var editor = _composeInlineImageEditor(img)
    img.remove()
    hideComposeInlineImageToolbar()
    if (editor) {
      syncComposeEditor(editor)
      _markComposeDirty(_composeFormFrom(editor))
    }
  } else if (action === "attach") {
    convertComposeInlineImageToAttachment(img)
  }
}

function convertComposeInlineImageToAttachment(img) {
  var editor = _composeInlineImageEditor(img)
  var form = _composeFormFrom(editor)
  if (!editor || !form) return
  addComposeAttachment(form, {
    id: img.dataset.existingAttachmentId || img.dataset.attachmentId || "",
    existing: !!img.dataset.existingAttachmentId,
    filename: img.dataset.filename || img.alt || "image",
    content_type: img.dataset.contentType || "image/png",
    size: Number(img.dataset.size || 0),
    preview_url: img.dataset.previewUrl || img.src || ""
  })
  img.remove()
  hideComposeInlineImageToolbar()
  syncComposeEditor(editor)
  _markComposeDirty(form)
}

function composeAttachmentKind(att) {
  var filename = (att && att.filename ? att.filename : "").toLowerCase()
  var contentType = (att && att.content_type ? att.content_type : "").toLowerCase().split(";")[0]
  function hasExt(exts) {
    for (var i = 0; i < exts.length; i++) {
      if (filename.endsWith(exts[i])) return true
    }
    return false
  }
  if (contentType.indexOf("image/") === 0 || hasExt([".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".bmp", ".ico"])) return { kind: "image", label: "IMG", title: "Image file" }
  if (contentType === "application/pdf" || hasExt([".pdf"])) return { kind: "pdf", label: "PDF", title: "PDF document" }
  if (contentType.indexOf("spreadsheet") >= 0 || contentType.indexOf("excel") >= 0 || contentType === "text/csv" || hasExt([".xls", ".xlsx", ".csv", ".ods"])) return { kind: "sheet", label: hasExt([".csv"]) ? "CSV" : "XLS", title: "Spreadsheet" }
  if (contentType.indexOf("word") >= 0 || hasExt([".doc", ".docx", ".odt", ".rtf"])) return { kind: "doc", label: "DOC", title: "Document" }
  if (contentType.indexOf("presentation") >= 0 || contentType.indexOf("powerpoint") >= 0 || hasExt([".ppt", ".pptx", ".odp"])) return { kind: "deck", label: "PPT", title: "Presentation" }
  if (contentType.indexOf("zip") >= 0 || contentType.indexOf("compressed") >= 0 || contentType.indexOf("tar") >= 0 || hasExt([".zip", ".rar", ".7z", ".tar", ".gz", ".tgz", ".bz2"])) return { kind: "archive", label: "ZIP", title: "Archive" }
  if (contentType.indexOf("audio/") === 0 || hasExt([".mp3", ".wav", ".m4a", ".ogg", ".flac"])) return { kind: "audio", label: "AUD", title: "Audio file" }
  if (contentType.indexOf("video/") === 0 || hasExt([".mp4", ".mov", ".avi", ".webm", ".mkv"])) return { kind: "video", label: "VID", title: "Video file" }
  if (hasExt([".json", ".xml", ".html", ".css", ".js", ".ts", ".go", ".py", ".rb", ".java", ".c", ".cpp", ".sh"])) return { kind: "code", label: "DEV", title: "Code file" }
  if (contentType.indexOf("text/") === 0 || hasExt([".txt", ".md", ".log"])) return { kind: "text", label: "TXT", title: "Text file" }
  return { kind: "file", label: "FILE", title: "File" }
}

function toggleComposeAttachments(el) {
  var form = _composeFormFrom(el)
  var wrap = form && form.querySelector("[data-compose-attachments]")
  var list = form && form.querySelector("[data-compose-attachment-list]")
  if (!wrap || !list) return
  var collapsed = wrap.dataset.composeAttachmentsCollapsed !== "true"
  wrap.dataset.composeAttachmentsCollapsed = collapsed ? "true" : "false"
  list.classList.toggle("hidden", collapsed)
  updateComposeAttachmentSummary(form)
}

function updateComposeAttachmentSummary(form) {
  var wrap = form && form.querySelector("[data-compose-attachments]")
  if (!wrap) return
  var summary = wrap.querySelector("[data-compose-attachment-summary]")
  var toggle = wrap.querySelector("[data-compose-attachment-toggle]")
  var attachments = form.querySelectorAll("[data-compose-attachment]")
  var pending = form.querySelectorAll("[data-compose-upload-pending]")
  var totalCount = attachments.length + pending.length
  var totalSize = 0
  for (var i = 0; i < attachments.length; i++) totalSize += Number(attachments[i].dataset.size || 0)
  for (var p = 0; p < pending.length; p++) totalSize += Number(pending[p].dataset.size || 0)
  if (summary) {
    if (totalCount) {
      summary.textContent = totalCount + " " + (totalCount === 1 ? "attachment" : "attachments") + " · " + formatComposeAttachmentSize(totalSize) + (pending.length ? " · " + pending.length + " uploading" : "") + " · Max 35 MB total"
    } else {
      summary.textContent = "Max 25 MB per file, 35 MB total"
    }
  }
  if (toggle) {
    var collapsed = wrap.dataset.composeAttachmentsCollapsed === "true"
    toggle.textContent = collapsed ? "Show" : "Hide"
    toggle.classList.toggle("hidden", totalCount === 0)
  }
}

function _composeAttachmentDataFromItem(item) {
  if (!item) return null
  var existing = item.dataset.existingAttachmentId
  return {
    id: existing || item.dataset.attachmentId || "",
    existing: !!existing,
    filename: item.dataset.filename || "attachment",
    content_type: item.dataset.contentType || "application/octet-stream",
    size: Number(item.dataset.size || 0),
    preview_url: item.dataset.previewUrl || ""
  }
}

function _composeAttachmentLooksInlineable(att) {
  return !!(att && att.preview_url && composeAttachmentKind(att).kind === "image")
}

function addComposeAttachment(form, att) {
  var wrap = form.querySelector("[data-compose-attachments]")
  var list = form.querySelector("[data-compose-attachment-list]")
  if (!wrap || !list) return
  wrap.classList.remove("hidden")

  var item = document.createElement("span")
  item.className = "compose-attachment-chip"
  item.dataset.composeAttachment = ""
  item.dataset.attachmentId = att.id || ""
  item.dataset.existingAttachmentId = att.existing ? String(att.id || "") : ""
  item.dataset.filename = att.filename || "attachment"
  item.dataset.contentType = att.content_type || "application/octet-stream"
  item.dataset.size = String(att.size || 0)
  item.dataset.previewUrl = att.preview_url || ""

  var hiddenName = att.existing ? "existing_attachment_id" : "attachment_id"
  item.appendChild(_composeHiddenInput(hiddenName, att.id || ""))
  if (!att.existing) {
    item.appendChild(_composeHiddenInput("attachment_filename", att.filename || "attachment"))
    item.appendChild(_composeHiddenInput("attachment_content_type", att.content_type || "application/octet-stream"))
    item.appendChild(_composeHiddenInput("attachment_size", String(att.size || 0)))
  }

  if (att.preview_url) {
    var preview = document.createElement("img")
    preview.className = "compose-attachment-preview"
    preview.src = att.preview_url
    preview.alt = ""
    preview.loading = "lazy"
    item.appendChild(preview)
  } else {
    var kind = composeAttachmentKind(att)
    var icon = document.createElement("span")
    icon.className = "compose-attachment-icon compose-attachment-icon-" + kind.kind
    icon.textContent = kind.label
    icon.title = kind.title
    icon.setAttribute("aria-hidden", "true")
    item.appendChild(icon)
  }

  var label = document.createElement("span")
  label.className = "truncate"
  label.textContent = (att.filename || "attachment") + (att.size ? " (" + formatComposeAttachmentSize(att.size) + ")" : "")
  var remove = document.createElement("button")
  remove.type = "button"
  remove.className = "compose-attachment-remove"
  remove.setAttribute("aria-label", "Remove attachment")
  remove.textContent = "x"
  remove.onclick = function () { removeComposeAttachment(item) }
  item.appendChild(label)
  if (_composeAttachmentLooksInlineable(att)) {
    var actions = document.createElement("button")
    actions.type = "button"
    actions.className = "compose-attachment-actions"
    actions.setAttribute("aria-label", "Attachment actions")
    actions.textContent = "⋯"
    actions.onclick = function (event) {
      event.preventDefault()
      event.stopPropagation()
      showComposeAttachmentActions(item)
    }
    item.appendChild(actions)
  }
  item.appendChild(remove)
  list.appendChild(item)
  updateComposeAttachmentSummary(form)
}

var _composeAttachmentMenu = null

function closeComposeAttachmentActions() {
  if (_composeAttachmentMenu) _composeAttachmentMenu.remove()
  _composeAttachmentMenu = null
}

function showComposeAttachmentActions(item) {
  closeComposeAttachmentActions()
  var att = _composeAttachmentDataFromItem(item)
  if (!_composeAttachmentLooksInlineable(att)) return
  var menu = document.createElement("div")
  menu.className = "compose-attachment-menu"
  var convert = document.createElement("button")
  convert.type = "button"
  convert.textContent = "Insert inline"
  convert.onclick = function () {
    closeComposeAttachmentActions()
    convertComposeAttachmentToInline(item)
  }
  menu.appendChild(convert)
  document.body.appendChild(menu)
  _composeAttachmentMenu = menu
  var rect = item.getBoundingClientRect()
  menu.style.top = Math.min(window.innerHeight - menu.offsetHeight - 8, rect.bottom + 6) + "px"
  menu.style.left = Math.max(8, Math.min(rect.left, window.innerWidth - menu.offsetWidth - 8)) + "px"
  setTimeout(function () { document.addEventListener("mousedown", closeComposeAttachmentActions, { once: true }) }, 0)
}

function convertComposeAttachmentToInline(item) {
  var form = _composeFormFrom(item)
  var att = _composeAttachmentDataFromItem(item)
  if (!form || !_composeAttachmentLooksInlineable(att)) return
  var editor = form.querySelector("[data-compose-editor]")
  if (editor) _saveComposeSelection(editor)
  removeComposeAttachment(item, true)
  insertComposeInlineImage(form, att)
  _markComposeDirty(form)
}

function addComposePendingAttachment(form, file, inline) {
  var wrap = form && form.querySelector("[data-compose-attachments]")
  var list = form && form.querySelector("[data-compose-attachment-list]")
  if (!wrap || !list) return null
  wrap.classList.remove("hidden")

  var item = document.createElement("span")
  item.className = "compose-attachment-chip compose-attachment-uploading"
  item.dataset.composeUploadPending = ""
  item.dataset.size = String((file && file.size) || 0)

  var spinner = document.createElement("span")
  spinner.className = "compose-attachment-spinner"
  spinner.setAttribute("aria-hidden", "true")
  item.appendChild(spinner)

  var label = document.createElement("span")
  label.className = "truncate"
  label.dataset.composeUploadLabel = ""
  label.textContent = (inline ? "Inserting " : "Uploading ") + ((file && file.name) || "file")
  item.appendChild(label)

  var progress = document.createElement("span")
  progress.className = "compose-attachment-progress"
  progress.dataset.composeUploadProgress = ""
  progress.textContent = "0%"
  item.appendChild(progress)

  var cancel = document.createElement("button")
  cancel.type = "button"
  cancel.className = "compose-attachment-cancel"
  cancel.setAttribute("aria-label", "Cancel upload")
  cancel.textContent = "Cancel"
  cancel.onclick = function () {
    if (item._composeCancelUpload) item._composeCancelUpload()
  }
  item.appendChild(cancel)

  list.appendChild(item)
  updateComposeAttachmentSummary(form)
  return item
}

function updateComposePendingAttachment(item, percent) {
  if (!item) return
  var progress = item.querySelector("[data-compose-upload-progress]")
  if (progress) progress.textContent = Math.max(0, Math.min(100, Number(percent) || 0)) + "%"
}

function removeComposePendingAttachment(item) {
  if (!item) return
  var form = _composeFormFrom(item)
  item.remove()
  var wrap = form && form.querySelector("[data-compose-attachments]")
  var list = form && form.querySelector("[data-compose-attachment-list]")
  if (wrap && list && !list.children.length) wrap.classList.add("hidden")
  updateComposeAttachmentSummary(form)
}

function failComposePendingAttachment(item) {
  if (!item) return
  var form = _composeFormFrom(item)
  delete item.dataset.composeUploadPending
  item.classList.remove("compose-attachment-uploading")
  item.classList.add("compose-attachment-failed")
  var label = item.querySelector("[data-compose-upload-label]")
  var progress = item.querySelector("[data-compose-upload-progress]")
  if (label) label.textContent = label.textContent.replace(/^(Uploading|Inserting)\s+/, "Failed ")
  if (progress) progress.textContent = "Failed"
  if (!item.querySelector("button")) {
    var remove = document.createElement("button")
    remove.type = "button"
    remove.className = "compose-attachment-remove"
    remove.setAttribute("aria-label", "Dismiss failed upload")
    remove.textContent = "x"
    remove.onclick = function () { removeComposePendingAttachment(item) }
    item.appendChild(remove)
  }
  updateComposeAttachmentSummary(form)
}

function _composeHiddenInput(name, value, inline) {
  var input = document.createElement("input")
  input.type = "hidden"
  input.name = name
  input.value = value
  if (inline) input.dataset.composeInlineHidden = ""
  return input
}

function removeComposeAttachment(item, keepFile) {
  var form = _composeFormFrom(item)
  var id = item.dataset.attachmentId
  var existing = item.dataset.existingAttachmentId
  item.classList.add("compose-attachment-removing")
  setTimeout(function () {
    item.remove()
    var wrap = form && form.querySelector("[data-compose-attachments]")
    var list = form && form.querySelector("[data-compose-attachment-list]")
    if (wrap && list && !list.children.length) wrap.classList.add("hidden")
    updateComposeAttachmentSummary(form)
    _markComposeDirty(form)
  }, 140)
  if (id && !existing && !keepFile) fetch("/compose/attachments/" + encodeURIComponent(id), { method: "DELETE" }).catch(function () {})
}

function formatComposeAttachmentSize(size) {
  size = Number(size || 0)
  if (size >= 1024 * 1024) return (size / (1024 * 1024)).toFixed(1) + " MB"
  if (size >= 1024) return Math.round(size / 1024) + " KB"
  return size + " B"
}

function renderComposeAttachments(form, attachments) {
  if (!form) return
  var list = form.querySelector("[data-compose-attachment-list]")
  var wrap = form.querySelector("[data-compose-attachments]")
  if (!list || !wrap) return
  list.innerHTML = ""
  for (var i = 0; attachments && i < attachments.length; i++) addComposeAttachment(form, attachments[i])
  wrap.classList.toggle("hidden", !attachments || !attachments.length)
  updateComposeAttachmentSummary(form)
}

function readComposeAttachments(form) {
  var items = form ? form.querySelectorAll("[data-compose-attachment]") : []
  var attachments = []
  for (var i = 0; i < items.length; i++) {
    var existing = items[i].dataset.existingAttachmentId
    attachments.push({
      id: existing || items[i].dataset.attachmentId || "",
      existing: !!existing,
      filename: items[i].dataset.filename || "attachment",
      content_type: items[i].dataset.contentType || "application/octet-stream",
      size: Number(items[i].dataset.size || 0),
      preview_url: items[i].dataset.previewUrl || ""
    })
  }
  return attachments
}

function readComposeInlineImages(form) {
  var imgs = form ? form.querySelectorAll("[data-compose-editor] img[data-compose-inline-image]") : []
  var inlineImages = []
  var seen = {}
  for (var i = 0; i < imgs.length; i++) {
    var cid = imgs[i].dataset.contentId || ""
    var id = imgs[i].dataset.attachmentId || imgs[i].dataset.existingAttachmentId || ""
    var key = (imgs[i].dataset.existingAttachmentId ? "existing:" : "new:") + id + ":" + cid
    if (!id || !cid || seen[key]) continue
    seen[key] = true
    inlineImages.push({
      id: id,
      existing: !!imgs[i].dataset.existingAttachmentId,
      content_id: cid,
      filename: imgs[i].dataset.filename || imgs[i].alt || "image",
      content_type: imgs[i].dataset.contentType || "image/png",
      size: Number(imgs[i].dataset.size || 0),
      preview_url: imgs[i].dataset.previewUrl || imgs[i].src || ""
    })
  }
  return inlineImages
}

function syncComposeInlineImageInputs(form) {
  if (!form) return
  var old = form.querySelectorAll("[data-compose-inline-hidden]")
  for (var i = 0; i < old.length; i++) old[i].remove()
  var inlineImages = readComposeInlineImages(form)
  for (var j = 0; j < inlineImages.length; j++) {
    var att = inlineImages[j]
    if (att.existing) {
      form.appendChild(_composeHiddenInput("existing_inline_attachment_id", att.id, true))
      form.appendChild(_composeHiddenInput("existing_inline_attachment_cid", att.content_id, true))
    } else {
      form.appendChild(_composeHiddenInput("inline_attachment_id", att.id, true))
      form.appendChild(_composeHiddenInput("inline_attachment_cid", att.content_id, true))
      form.appendChild(_composeHiddenInput("inline_attachment_filename", att.filename, true))
      form.appendChild(_composeHiddenInput("inline_attachment_content_type", att.content_type, true))
      form.appendChild(_composeHiddenInput("inline_attachment_size", String(att.size || 0), true))
    }
  }
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
    compose_mode: draft.compose_mode || "new",
    in_reply_to: draft.in_reply_to || "",
    references: draft.references || "",
    attachments: draft.attachments || [],
    inline_images: draft.inline_images || [],
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
  renderComposeAttachments(form, vals.attachments || [])
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

function chooseComposeDialogCloseAction(form) {
  if (!form || !_composeHasDraftContent(form)) return Promise.resolve("discard")
  var root = document.getElementById("compose-close-choice-dialog")
  if (!root || !window.tui || !window.tui.dialog) return chooseComposeCloseAction(form, null, null)
  return new Promise(function (resolve) {
    var settled = false
    var content = root.querySelector("[data-tui-dialog-content]")
    function finish(action) {
      if (settled) return
      settled = true
      root.removeEventListener("click", onClick)
      if (content) content.removeEventListener("close", onClose)
      window.tui.dialog.close(root.id)
      resolve(action)
    }
    function onClick(event) {
      var btn = event.target && event.target.closest ? event.target.closest("[data-compose-close-action]") : null
      if (btn) finish(btn.dataset.composeCloseAction)
    }
    function onClose() {
      finish("cancel")
    }
    root.addEventListener("click", onClick)
    if (content) content.addEventListener("close", onClose)
    window.tui.dialog.open(root.id)
  })
}

function chooseComposeCloseAction(form, anchor, popoverId) {
  if (!form || !_composeHasDraftContent(form)) return Promise.resolve("discard")
  var root = popoverId ? document.getElementById(popoverId) : null
  if (anchor && root && !root.contains(anchor)) root = null
  if (root && root.id && window.tui && window.tui.popover) {
    return new Promise(function (resolve) {
      var settled = false
      var content = root.querySelector("[data-tui-popover-content]")
      function finish(action) {
        if (settled) return
        settled = true
        root.removeEventListener("click", onClick)
        if (content) content.removeEventListener("toggle", onToggle)
        window.tui.popover.close(root.id)
        resolve(action)
      }
      function onClick(event) {
        var btn = event.target && event.target.closest ? event.target.closest("[data-compose-close-action]") : null
        if (btn) finish(btn.dataset.composeCloseAction)
      }
      function onToggle(event) {
        if (event.newState === "closed") finish("cancel")
      }
      root.addEventListener("click", onClick)
      if (content) content.addEventListener("toggle", onToggle)
      window.tui.popover.open(root.id)
    })
  }
  return new Promise(function (resolve) {
    var panel = document.createElement("div")
    panel.className = "compose-close-choice compose-close-choice-floating"
    panel.setAttribute("popover", "auto")
    panel.innerHTML = '<h2>Close compose?</h2><p>Keep this message as a draft, discard it permanently, or continue editing.</p>'
    var settled = false
    function finish(action) {
      if (settled) return
      settled = true
      panel.removeEventListener("toggle", onToggle)
      if (panel.matches && panel.matches(":popover-open")) panel.hidePopover()
      panel.remove()
      resolve(action)
    }
    function onToggle(event) {
      if (event.newState === "closed") finish("cancel")
    }
    function button(label, action, primary) {
      var btn = document.createElement("button")
      btn.type = "button"
      btn.textContent = label
      btn.dataset.composeCloseAction = action
      if (primary) btn.className = "compose-close-choice-primary"
      return btn
    }
    var actions = document.createElement("div")
    actions.className = "compose-close-choice-actions"
    actions.appendChild(button("Exit and keep draft", "keep", true))
    actions.appendChild(button("Exit and discard", "discard", false))
    actions.appendChild(button("Cancel", "cancel", false))
    panel.appendChild(actions)
    panel.addEventListener("click", function (event) {
      var btn = event.target && event.target.closest ? event.target.closest("[data-compose-close-action]") : null
      if (!btn) return
      finish(btn.dataset.composeCloseAction)
    })
    panel.addEventListener("toggle", onToggle)
    document.body.appendChild(panel)
    if (panel.showPopover) panel.showPopover()
  })
}

function discardComposeDialog() {
  var form = document.getElementById("compose-form")
  chooseComposeDialogCloseAction(form).then(function (action) {
    if (action === "cancel") return
    if (action === "keep") {
      saveComposeDraft(false, false).then(function (saved) {
        if (!saved) return
        resetComposeForm(false, true)
        if (window.tui && window.tui.dialog) window.tui.dialog.close("compose-dialog")
        _updateComposeBtn(false)
      })
      return
    }
    cleanupComposeStagedUploads(form)
    _deleteComposeDraft(form)
    resetComposeForm(false, true)
    if (window.tui && window.tui.dialog) window.tui.dialog.close("compose-dialog")
    _updateComposeBtn(false)
  })
}

document.addEventListener("input", function (event) {
  var form = event.target && event.target.closest ? event.target.closest("#compose-form, #compose-pane-form") : null
  if (!form || event.target.matches("[data-compose-editor]")) return
  _markComposeDirty(form)
})

window.addEventListener("beforeunload", function (event) {
  var forms = [document.getElementById("compose-form"), document.getElementById("compose-pane-form")]
  for (var i = 0; i < forms.length; i++) {
    if (forms[i] && ((forms[i].dataset.composeDirty === "true" && _composeHasDraftContent(forms[i])) || _composePendingUploads(forms[i]) > 0 || forms[i].dataset.composeSending === "true")) {
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
  if (_composePendingUploads(form) > 0) {
    showSendStatus("failed", "Wait for uploads to finish before sending")
    updateComposeSendState(form)
    return
  }
  if (form.dataset.composeSending === "true") return
  if (!finalizeComposeRecipients(form)) {
    showSendStatus("failed", "Fix invalid recipient addresses before sending")
    return
  }
  _syncComposeFormEditor(form)
  if (!validateComposeMessageSize(form)) return

  var toField = form.querySelector('input[name="to"]')
  if (!toField || !toField.value.trim()) {
    showSendStatus("failed", "Please enter at least one recipient.")
    return
  }

  var params = new URLSearchParams()
  var inputs = form.querySelectorAll("input, textarea")
  for (var i = 0; inputs && i < inputs.length; i++) {
    if (inputs[i].name) params.append(inputs[i].name, inputs[i].value)
  }

  showSendStatus("sending", "Sending...")
  _setComposeSending(form, true)
  _composeSendState = { formId: form.id, fromPane: !!fromPane }

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
    return r.json().catch(function () { return {} })
  }).catch(function (err) {
    _setComposeSending(form, false)
    _composeSendState = null
    form.dataset.composeDirty = "true"
    showSendStatus("failed", err && err.message ? err.message : "Failed to connect to server")
  })
}

function composeAddress(name, email) {
  email = String(email || "").trim()
  name = String(name || "").trim()
  if (!email) return ""
  return name ? name + " <" + email + ">" : email
}

function composeNormalizeMessageID(messageId) {
  messageId = String(messageId || "").trim()
  if (!messageId) return ""
  return messageId.charAt(0) === "<" ? messageId : "<" + messageId + ">"
}

function composeSourceURL(bar) {
  var params = new URLSearchParams()
  params.set("account_id", bar.dataset.accountId || "")
  params.set("message_id", bar.dataset.messageId || "")
  return "/api/compose/source?" + params.toString()
}

function composeDedupeAddresses(values, excludeEmails) {
  var seen = {}
  var out = []
  excludeEmails = excludeEmails || {}
  for (var i = 0; i < values.length; i++) {
    var parts = _splitComposeRecipients(values[i])
    for (var p = 0; p < parts.length; p++) {
      var email = _composeRecipientEmail(parts[p])
      if (!email || seen[email] || excludeEmails[email]) continue
      seen[email] = true
      out.push(parts[p])
    }
  }
  return out.join(", ")
}

function composeAccountEmail(accountId) {
  var options = document.querySelectorAll("[data-account-id]")
  for (var i = 0; i < options.length; i++) {
    if (options[i].dataset.accountId === accountId && options[i].dataset.accountEmail) {
      return String(options[i].dataset.accountEmail).toLowerCase()
    }
  }
  return ""
}

function setComposeAccount(form, accountId) {
  if (!form || !accountId) return
  var pane = form.id === "compose-pane-form"
  var prefix = pane ? "compose-pane-" : "compose-"
  var idField = document.getElementById(prefix + "account-id")
  if (idField) idField.value = accountId
  var options = document.querySelectorAll("[data-account-id]")
  for (var i = 0; i < options.length; i++) {
    if (options[i].dataset.accountId !== accountId) continue
    var display = document.getElementById(prefix + "from-display")
    if (display && options[i].dataset.accountEmail) {
      var name = options[i].dataset.accountName || ""
      var email = options[i].dataset.accountEmail
      display.innerHTML = (name ? name + " &lt;" : "") + email + (name ? "&gt;" : "")
    }
    return
  }
}

function composeReplyPlain(source) {
  var fromLine = composeAddress(source.from_name, source.from_email)
  var header = source.date ? "On " + source.date + ", " + fromLine + " wrote:" : fromLine + " wrote:"
  var quotedBody = String(source.body || "").split("\n").map(function (line) { return "> " + line }).join("\n")
  return "\n\n" + header + "\n" + quotedBody
}

function composeSourceBodyHTML(source) {
  var html = source.html_body ? _sanitizeComposeHTML(source.html_body) : ""
  if (html && html.trim()) return html
  return _composePlainToHTML(source.body || "")
}

function composeReplyHTML(source) {
  var fromLine = composeAddress(source.from_name, source.from_email)
  var header = source.date ? "On " + source.date + ", " + fromLine + " wrote:" : fromLine + " wrote:"
  return "<p><br></p><p>" + _escapeComposeHTML(header) + "</p><blockquote>" + composeSourceBodyHTML(source) + "</blockquote>"
}

function composeForwardPlain(source) {
  var fromLine = composeAddress(source.from_name, source.from_email)
  var header = "\n\n---------- Forwarded message ----------"
  if (fromLine) header += "\nFrom: " + fromLine
  if (source.date) header += "\nDate: " + source.date
  if (source.subject) header += "\nSubject: " + source.subject
  if (source.to) header += "\nTo: " + source.to
  if (source.cc) header += "\nCc: " + source.cc
  return header + "\n\n" + (source.body || "")
}

function composeForwardHTML(source) {
  var lines = ["---------- Forwarded message ----------"]
  var fromLine = composeAddress(source.from_name, source.from_email)
  if (fromLine) lines.push("From: " + fromLine)
  if (source.date) lines.push("Date: " + source.date)
  if (source.subject) lines.push("Subject: " + source.subject)
  if (source.to) lines.push("To: " + source.to)
  if (source.cc) lines.push("Cc: " + source.cc)
  return "<p><br></p><div>" + lines.map(_escapeComposeHTML).join("<br>") + "</div><br><div>" + composeSourceBodyHTML(source) + "</div>"
}

function composeReferencesForReply(source) {
  var parentMessageId = composeNormalizeMessageID(source.message_id)
  if (!parentMessageId) return ""
  return source.references ? source.references + " " + parentMessageId : parentMessageId
}

function composeValuesFromSource(source, mode) {
  var fromLine = composeAddress(source.from_name, source.from_email)
  var ownEmail = composeAccountEmail(source.account_id)
  var exclude = {}
  if (ownEmail) exclude[ownEmail] = true
  var vals = {
    account_id: source.account_id || "",
    draft_id: "",
    to: "",
    cc: "",
    bcc: "",
    subject: "",
    body: "",
    html_body: "",
    compose_mode: mode === "forward" ? "forward" : "reply",
    in_reply_to: "",
    references: "",
    attachments: [],
    inline_images: [],
    _ccVisible: false,
    _bccVisible: false,
    _composeDirty: "true"
  }
  if (mode === "reply" || mode === "reply-all") {
    vals.to = mode === "reply-all" ? composeDedupeAddresses([fromLine, source.to || ""], exclude) : composeDedupeAddresses([fromLine], exclude)
    vals.cc = mode === "reply-all" ? composeDedupeAddresses([source.cc || ""], exclude) : ""
    vals.subject = /^Re:/i.test(source.subject || "") ? source.subject : "Re: " + (source.subject || "")
    vals.body = composeReplyPlain(source)
    vals.html_body = composeReplyHTML(source)
    vals.in_reply_to = composeNormalizeMessageID(source.message_id)
    vals.references = composeReferencesForReply(source)
    vals._ccVisible = !!vals.cc
  } else {
    vals.subject = /^Fwd:/i.test(source.subject || "") ? source.subject : "Fwd: " + (source.subject || "")
    vals.body = composeForwardPlain(source)
    vals.html_body = composeForwardHTML(source)
    vals.attachments = source.attachments || []
  }
  return vals
}

function focusComposePrefill(form, mode) {
  if (!form) return
  if (mode === "forward") {
    var toInput = form.querySelector('[data-recipient-name="to"] [data-compose-recipient-input]')
    if (toInput) {
      toInput.focus()
      return
    }
  }
  var editor = form.querySelector("[data-compose-editor]")
  if (!editor) return
  editor.focus()
  var range = document.createRange()
  range.setStart(editor, 0)
  range.collapse(true)
  var selection = window.getSelection()
  if (selection) {
    selection.removeAllRanges()
    selection.addRange(range)
  }
}

function writeComposePrefill(form, vals, prefix, mode) {
  _writeComposeFormValues(form, vals, prefix)
  setComposeMode(form, mode === "forward" ? "forward" : "reply")
  _showComposeOptionalFields(form, vals)
  setComposeAccount(form, vals.account_id)
  applyDefaultComposeSignature(form, true)
  focusComposePrefill(form, mode)
}

function openComposePrefill(vals, mode) {
  if (!_activeComposeCanBeReplaced()) return
  var activePane = document.querySelector("[data-compose-pane]")
  if (activePane) {
    writeComposePrefill(document.getElementById("compose-pane-form"), vals, "compose-pane-", mode)
    return
  }
  var view = window.GoferSettings ? GoferSettings.get("default_compose_view") : null
  if ((view === "pane" || view === "full") && document.getElementById("mail-list") && document.getElementById("mail-view")) {
    fetch("/compose/pane").then(function (r) { return r.text() }).then(function (html) {
      writeComposePane(html, vals, view === "full", view === "full")
      writeComposePrefill(document.getElementById("compose-pane-form"), vals, "compose-pane-", mode)
    }).catch(function () {})
    return
  }
  var form = document.getElementById("compose-form")
  writeComposePrefill(form, vals, "compose-", mode)
  if (window.tui && window.tui.dialog) window.tui.dialog.open("compose-dialog")
}

function handleReply(el, mode) {
  var bar = el && el.closest ? el.closest("[data-thread-reply-data]") : null
  if (!bar) bar = document.getElementById("reply-bar")
  if (!bar) return
  fetch(composeSourceURL(bar))
    .then(function (r) {
      if (!r.ok) throw new Error("Failed to load message")
      return r.json()
    })
    .then(function (source) {
      openComposePrefill(composeValuesFromSource(source, mode), mode)
    })
    .catch(function (err) {
      showSendStatus("failed", err && err.message ? err.message : "Failed to start reply")
    })
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
  applyDefaultComposeSignatureWhenReady(document.getElementById("compose-form"), true)
}

function composeOpeningHTML() {
  return '<div class="flex flex-1 w-full min-w-0 flex-col items-center justify-center h-full text-center p-8">' +
    '<div class="size-5 border-2 border-muted-foreground/30 border-t-muted-foreground rounded-full animate-spin mb-3"></div>' +
    '<p class="text-sm text-muted-foreground">Opening compose...</p>' +
    '</div>'
}

function savedMailListWidth() {
  var value = window.GoferSettings ? GoferSettings.get("mail_list_width") : null
  if (!value) {
    try {
      var settings = JSON.parse(localStorage.getItem("gofer:ui_settings") || "{}") || {}
      value = settings.mail_list_width
    } catch (_) {}
  }
  var width = parseInt(value || "384", 10)
  if (isNaN(width) || width <= 0) width = 384
  var max = Math.min(1200, (window.innerWidth || 1024) * 0.55)
  width = Math.max(300, Math.min(max, width))
  return Math.round(width) + "px"
}

function composeOpeningShellHTML() {
  var safeWidth = _escapeComposeHTML(savedMailListWidth())
  return '<div id="mail-list" class="shrink-0 lg:flex flex-col border-r border-border bg-card h-full overflow-hidden" style="width:' + safeWidth + ';flex:0 0 ' + safeWidth + ';max-width:' + safeWidth + '">' +
    '<div class="px-4 py-4 space-y-3">' +
      '<div class="flex items-center justify-between">' +
        '<div class="flex items-center gap-2">' +
          '<h2 class="text-lg font-bold tracking-tight" style="font-family: var(--font-serif)">Inbox</h2>' +
          '<span class="h-5 w-10 rounded-full bg-muted animate-pulse"></span>' +
        '</div>' +
        '<div class="h-8 w-8 rounded-md bg-muted/50"></div>' +
      '</div>' +
      '<div class="flex items-center gap-2">' +
        '<div class="h-9 flex-1 rounded-lg bg-background border border-border/50 opacity-60"></div>' +
        '<div class="h-9 w-28 rounded-lg border border-border bg-card opacity-60"></div>' +
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
        '<span>Loading messages...</span>' +
      '</div>' +
    '</div>' +
  '</div>' +
  '<div class="resize-handle" data-panel="maillist" draggable="false"></div>' +
  '<div id="mail-view" class="hidden lg:flex flex-1 flex-col min-w-0 bg-background surface-desk">' + composeOpeningHTML() + '</div>'
}

function composeOpeningFullShellHTML() {
  return '<div id="mail-list" class="w-full lg:flex flex-col border-r border-border bg-card h-full overflow-hidden" style="display:none;width:0px;opacity:0;overflow:hidden;border-width:0"></div>' +
    '<div class="resize-handle" data-panel="maillist" draggable="false" style="display:none;opacity:0"></div>' +
    '<div id="mail-view" class="hidden lg:flex flex-1 flex-col min-w-0 bg-background surface-desk">' + composeOpeningHTML() + '</div>'
}

function mergeFolderShellBehindCompose(folderID, fullWidth) {
  fetch("/folder/" + encodeURIComponent(folderID || "inbox") + "/full")
    .then(function (r) { return r.text() })
    .then(function (html) {
      var tmp = document.createElement("div")
      tmp.innerHTML = html
      var nextMailList = tmp.querySelector("#mail-list")
      var nextHandle = tmp.querySelector('[data-panel="maillist"]')
      var currentMailList = document.querySelector("#main-content > #mail-list")
      var currentHandle = document.querySelector('#main-content > [data-panel="maillist"]')
      var scroll = nextMailList && nextMailList.querySelector("#mail-list-scroll")
      if (scroll) scroll.removeAttribute("data-autoload-first-email")
      if (nextMailList && currentMailList) currentMailList.replaceWith(nextMailList)
      if (nextHandle && currentHandle) currentHandle.replaceWith(nextHandle)
      if (typeof initResizeHandles === "function") initResizeHandles()
      if (typeof window.applyMailTableColumnSettings === "function") window.applyMailTableColumnSettings(document.getElementById("mail-list-scroll"))
      if (fullWidth) applyComposeFullWidthInstant()
    })
    .catch(function () {})
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

  function showComposeOpeningContent() {
    var mainContent = document.getElementById("main-content")
    if (!mainContent) return
    mainContent.className = "flex flex-1 min-w-0"
    mainContent.innerHTML = fullWidth ? composeOpeningFullShellHTML() : composeOpeningShellHTML()
    mainReady = true
  }

  function openWhenReady() {
    if (!mainReady || paneHTML === null) return
    writeComposePane(paneHTML, vals, fullWidth, instantFullWidth)
  }

  function beforeMainContentSwap(evt) {
    if (!evt.target || evt.target.id !== "main-content") return
    var paneForm = document.getElementById("compose-pane-form")
    if (paneForm) {
      vals = _readComposeFormValues(paneForm)
      vals._skipDefaultSignature = !!existingComposeSignature(paneForm.querySelector("[data-compose-editor]"))
    }
  }

  function afterMainContentSwap(evt) {
    if (!evt.target || evt.target.id !== "main-content") return
    document.body.removeEventListener("htmx:beforeSwap", beforeMainContentSwap)
    document.body.removeEventListener("htmx:afterSwap", afterMainContentSwap)
    if (paneHTML === null) {
      var mailView = document.getElementById("mail-view")
      if (mailView) mailView.innerHTML = composeOpeningHTML()
    }
    mainReady = true
    openWhenReady()
  }

  function afterMainContentSettle(evt) {
    if (!evt.target || evt.target.id !== "main-content") return
    document.body.removeEventListener("htmx:afterSettle", afterMainContentSettle)
    suppressNextAutoloadFirstEmail = false
  }

  if (!fullWidth) {
    document.body.addEventListener("htmx:beforeSwap", beforeMainContentSwap)
    document.body.addEventListener("htmx:afterSwap", afterMainContentSwap)
    document.body.addEventListener("htmx:afterSettle", afterMainContentSettle)
  }
  showComposeOpeningContent()
  fetch("/compose/pane").then(function (r) { return r.text() }).then(function (html) {
    paneHTML = html
    openWhenReady()
  }).catch(function () {
    document.body.removeEventListener("htmx:beforeSwap", beforeMainContentSwap)
    document.body.removeEventListener("htmx:afterSwap", afterMainContentSwap)
    document.body.removeEventListener("htmx:afterSettle", afterMainContentSettle)
    suppressNextAutoloadFirstEmail = false
  })
  if (fullWidth) {
    mergeFolderShellBehindCompose("inbox", true)
  } else {
    suppressNextAutoloadFirstEmail = true
    htmx.ajax("GET", "/folder/inbox/full", { target: "#main-content", swap: "outerHTML" })
  }
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
  vals._skipDefaultSignature = !!existingComposeSignature(form.querySelector("[data-compose-editor]"))
  var ccVisible = !!form.querySelector('[id^="pane-cc-field"]') && !document.getElementById("pane-cc-field").classList.contains("hidden")
  var bccVisible = !!form.querySelector('[id^="pane-bcc-field"]') && !document.getElementById("pane-bcc-field").classList.contains("hidden")
  if (form.id === "compose-form") {
    ccVisible = !document.getElementById("cc-field").classList.contains("hidden")
    bccVisible = !document.getElementById("bcc-field").classList.contains("hidden")
  }
  vals._ccVisible = ccVisible
  vals._bccVisible = bccVisible
  vals._composeDirty = form.dataset.composeDirty || "false"
  vals.attachments = readComposeAttachments(form)
  vals.inline_images = readComposeInlineImages(form)
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
  } else if (vals.account_id) {
    setComposeAccount(form, vals.account_id)
  }
  _setComposeEditorValue(form, vals.body || "", vals.html_body || "", vals.inline_images || [])
  renderComposeAttachments(form, vals.attachments || [])
  form.dataset.composeUploadsPending = "0"
  form.dataset.composeSending = "false"
  delete form.dataset.composeUploadFailed
  form.dataset.composeDirty = vals._composeDirty || "false"
  updateComposeSendState(form)
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

  var mailView = document.getElementById("mail-view")
  if (mailView) mailView.innerHTML = composeOpeningHTML()

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
  if (!vals || !vals._skipDefaultSignature) applyDefaultComposeSignatureWhenReady(paneForm, false)

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

function discardComposePane(anchor) {
  var paneForm = document.getElementById("compose-pane-form")
  chooseComposeCloseAction(paneForm, anchor, "compose-pane-close-choice-popover").then(function (action) {
    if (action === "cancel") return
    if (action === "keep") {
      saveComposeDraft(true, false).then(function (saved) {
        if (!saved) return
        collapseComposeFullWidth()
        var mailView = document.getElementById("mail-view")
        if (mailView) setMailViewEmpty()
        _updateComposeBtn(false)
      })
      return
    }
    cleanupComposeStagedUploads(paneForm)
    _deleteComposeDraft(paneForm)
    collapseComposeFullWidth()
    var mailView = document.getElementById("mail-view")
    if (mailView) setMailViewEmpty()
    _updateComposeBtn(false)
  })
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
