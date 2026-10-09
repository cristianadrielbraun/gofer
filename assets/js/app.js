function setupPageRequestOwnership() {
  if (window._goferPageRequestOwnershipReady) return
  window._goferPageRequestOwnershipReady = true
  var generation = 0

  // Register immediately: HTMX processes load triggers in its own
  // DOMContentLoaded listener, before the rest of our app is initialized.
  document.addEventListener("htmx:beforeRequest", function (event) {
    var detail = event.detail
    if (!detail || !detail.xhr) return
    var trigger = detail.elt
    var pageTarget = detail.target && /^(main-content|settings-content|app-shell)$/.test(detail.target.id)
    var navigation = trigger && trigger.hasAttribute && (
      trigger.hasAttribute("data-sidebar-app-button") ||
      (pageTarget && detail.requestConfig && detail.requestConfig.verb === "get" &&
        (trigger.hasAttribute("href") || detail.boosted)))
    if (navigation) generation++
    detail.xhr.goferPageGeneration = generation
  }, true)

  function rejectPreviousPageResponse(event) {
    var xhr = event.detail && event.detail.xhr
    if (!xhr || typeof xhr.goferPageGeneration !== "number" || xhr.goferPageGeneration === generation) return
    event.detail.shouldSwap = false
    // Cancelling the event also blocks response headers, history/title updates,
    // and out-of-band swaps, rather than just hiding the primary swap.
    event.preventDefault()
  }
  document.addEventListener("htmx:beforeOnLoad", rejectPreviousPageResponse, true)
  // The original trigger may be detached, so also check the surviving target.
  document.addEventListener("htmx:beforeSwap", rejectPreviousPageResponse, true)
  window.addEventListener("popstate", function () { generation++ }, true)
}

setupPageRequestOwnership()

function animateSectionContent(section) {
  if (!section || typeof section.animate !== "function" ||
      (window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches)) return
  section.animate([
    { opacity: 0, transform: "translateY(5px)" },
    { opacity: 1, transform: "translateY(0)" },
  ], { duration: 180, easing: "ease-out" })
}

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
  var syncRefreshLastAt = 0
  var syncRefreshPendingVml = null
  var syncRefreshPendingOptions = null
  var syncRefreshMinInterval = 5000
  var processingStatusHandler = null
  var syncStatesByFolder = Object.create(null)
  var appEventSource = null
  var prefetchedBodies = Object.create(null)
  var avatarWarmupTimer = null
  var avatarWarmupSent = Object.create(null)
  var autoMarkReadTimer = null
  var autoMarkReadEmailId = null
  var suppressEmailUrlPushFor = null
  var preserveMailListSelectionFor = null
  var selectedMailIds = new Set()
  var lastSelectedMailId = null
  var mailSelectionBusy = false
  var accountDeletionPolls = Object.create(null)

  function cssEscape(value) {
    if (window.CSS && typeof window.CSS.escape === "function") return window.CSS.escape(value)
    return String(value).replace(/[^a-zA-Z0-9_-]/g, "\\$&")
  }

  // Register app/request ownership before Mail can start its initial fetch.
  setupSidebarAppNavToggle()
  initVirtualScroll()
  setupFolderClickInterception()
  setupEmailSelectionTracking()
  setupMailListViewToggle()
  setupContactsList()
  setupMailFilters()
  setupMailTableColumnResize()
  setupSSE()
  setupOutgoingSendStatus()
  setupMailOperationActions()
  setupContactAvatarImages()
  setupAvatarWarmup()
  setupMailListActions()
  setupAccountDeletionTracking()
  setupAccountResultFeedback()
  setupSidebarAccountCollapse()
  setupProcessingStatus()
  setupBodyPrefetch()
  setupEmailBodyModeTabs()
  setupEmailTranslation()
  setupMailtoIntent()
  setupMailSyncSidebarControls()
  setupMailSyncCancelControls()
  setupDesktopNotifications()
  setupSidebarSyncErrorTimes()
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
      var sortForm = document.querySelector("[data-contact-sort-form]")
      return {
        query: search ? search.value || "" : "",
        source: form && form.querySelector('[name="source"]') ? form.querySelector('[name="source"]').value || "" : "",
        saveTarget: form && form.querySelector('[name="save_target"]') ? form.querySelector('[name="save_target"]').value || "" : "",
        activity: form && form.querySelector('[name="activity"]') ? form.querySelector('[name="activity"]').value || "" : "",
        sortBy: sortForm && sortForm.querySelector('[name="sort_by"]') ? sortForm.querySelector('[name="sort_by"]').value || "updated" : "updated",
        sortOrder: sortForm && sortForm.querySelector('[name="sort_order"]') ? sortForm.querySelector('[name="sort_order"]').value || "desc" : "desc",
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
          var inputs = form.querySelectorAll('input[name="source"], input[name="save_target"], input[name="activity"]')
          for (var ci = 0; ci < inputs.length; ci++) inputs[ci].value = ""
          var selectedItems = form.querySelectorAll("[data-tui-selectbox-selected='true']")
          for (var si = 0; si < selectedItems.length; si++) selectedItems[si].setAttribute("data-tui-selectbox-selected", "false")
          var placeholders = form.querySelectorAll("[data-tui-selectbox-placeholder]")
          for (var pi = 0; pi < placeholders.length; pi++) placeholders[pi].textContent = placeholders[pi].getAttribute("data-tui-selectbox-placeholder") || ""
        }
        applyFilters()
        return
      }

      var addContactValue = e.target.closest && e.target.closest("[data-contact-add-value]")
      if (addContactValue) {
        e.preventDefault()
        var valueName = addContactValue.getAttribute("data-contact-add-value") || ""
        var form = addContactValue.closest("[data-contact-editor-form]")
        var list = form && form.querySelector('[data-contact-value-list="' + valueName + '"]')
        var template = form && form.querySelector('template[data-contact-value-template="' + valueName + '"]')
        if (list && template && template.content) {
          var node = template.content.firstElementChild.cloneNode(true)
          list.appendChild(node)
          var input = node.querySelector("input")
          if (input) input.focus()
        }
        return
      }

      var removeContactValue = e.target.closest && e.target.closest("[data-contact-remove-value]")
      if (removeContactValue) {
        e.preventDefault()
        var valueRow = removeContactValue.closest("[data-contact-value-row]")
        if (valueRow) valueRow.remove()
        return
      }

      var chooseContactAvatar = e.target.closest && e.target.closest("[data-contact-avatar-choose]")
      if (chooseContactAvatar) {
        e.preventDefault()
        var avatarEditor = chooseContactAvatar.closest("[data-contact-avatar-editor]")
        var avatarInput = avatarEditor && avatarEditor.querySelector("[data-contact-avatar-input]")
        if (avatarInput) avatarInput.click()
        return
      }

      var removeContactAvatar = e.target.closest && e.target.closest("[data-contact-avatar-remove]")
      if (removeContactAvatar) {
        e.preventDefault()
        setContactAvatarEditorValue(removeContactAvatar.closest("[data-contact-avatar-editor]"), "", "remove")
        return
      }

      var syncContactNowButton = e.target.closest && e.target.closest("[data-contact-sync-now]")
      if (syncContactNowButton) {
        e.preventDefault()
        syncContactNow(syncContactNowButton)
        return
      }

      var editContact = e.target.closest && e.target.closest("[data-contact-edit-trigger]")
      if (editContact) {
        var editDialogID = editContact.getAttribute("data-contact-edit-dialog") || ""
        window.setTimeout(function () {
          var editDialog = editDialogID ? document.getElementById(editDialogID) : null
          var editForm = editDialog && editDialog.querySelector("[data-contact-edit-form]")
          if (editForm) editForm.dataset.initialState = contactEditorState(editForm)
        }, 0)
        return
      }

      var cancelContactEdit = e.target.closest && e.target.closest("[data-contact-editor-cancel]")
      if (cancelContactEdit) {
        e.preventDefault()
        var cancelForm = cancelContactEdit.closest("[data-contact-edit-form]")
        if (!cancelForm) return
        var initialState = cancelForm.dataset.initialState || contactEditorState(cancelForm)
        var hasChanges = contactEditorState(cancelForm) !== initialState
        if (hasChanges && !window.confirm("Discard unsaved contact changes?")) return
        var cancelDialogID = cancelForm.getAttribute("data-contact-edit-dialog") || ""
        if (cancelDialogID && window.tui && window.tui.dialog) window.tui.dialog.close(cancelDialogID)
        if (hasChanges && cancelForm.dataset.contactId) {
          window.setTimeout(function () {
            refreshContactsDetail(cancelForm.dataset.contactId, null, false)
          }, 220)
        }
        return
      }

      var cancelSyncSetup = e.target.closest && e.target.closest("[data-contact-sync-setup-cancel]")
      if (cancelSyncSetup) {
        e.preventDefault()
        if (window.tui && window.tui.dialog) window.tui.dialog.close("contact-sync-setup-dialog")
        window.setTimeout(function () {
          var host = document.getElementById("contact-sync-setup-host")
          if (host) host.remove()
        }, 220)
        return
      }

      var toggleSyncLocationSearch = e.target.closest && e.target.closest("[data-contact-sync-location-search-toggle]")
      if (toggleSyncLocationSearch) {
        e.preventDefault()
        var toggleLocation = toggleSyncLocationSearch.closest("[data-contact-sync-setup-location]")
        var togglePanel = toggleLocation && toggleLocation.querySelector("[data-contact-sync-location-search-panel]")
        if (!togglePanel) return
        togglePanel.classList.toggle("hidden")
        if (!togglePanel.classList.contains("hidden")) {
          var toggleInput = togglePanel.querySelector("[data-contact-sync-location-search-query]")
          if (toggleInput) window.requestAnimationFrame(function () { toggleInput.focus() })
        }
        return
      }

      var submitSyncLocationSearch = e.target.closest && e.target.closest("[data-contact-sync-location-search-submit]")
      if (submitSyncLocationSearch) {
        e.preventDefault()
        searchContactSyncLocation(submitSyncLocationSearch)
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
      var primaryActivation = e.button == null || e.button === 0
      if (!e.metaKey && !e.ctrlKey && !e.shiftKey && !e.altKey && primaryActivation) {
        showContactsDetailLoading(contactItem)
      }
    })

    document.addEventListener("submit", function (e) {
      if (e.target && e.target.matches("[data-contact-sync-setup-form]")) {
        e.preventDefault()
        submitContactSyncSetup(e.target, false)
        return
      }
      if (e.target && e.target.matches("[data-contact-sync-setup-confirm]")) {
        e.preventDefault()
        submitContactSyncSetup(e.target, true)
        return
      }
      if (e.target && e.target.matches("[data-contact-editor-form]")) {
        e.preventDefault()
        saveContactEditor(e.target, e.submitter || null)
        return
      }
      if (!e.target || (!e.target.matches("[data-contact-filter-form]") && !e.target.matches("[data-contact-search-form]") && !e.target.matches("[data-contact-sort-form]"))) return
      e.preventDefault()
      if (e.target.matches("[data-contact-sort-form]") && window.GoferSettings) {
        var contactSortBy = e.target.querySelector('[name="sort_by"]')
        var contactSortOrder = e.target.querySelector('[name="sort_order"]')
        GoferSettings.set("contacts_list_sort_by", contactSortBy ? contactSortBy.value || "updated" : "updated")
        GoferSettings.set("contacts_list_sort_order", contactSortOrder ? contactSortOrder.value || "desc" : "desc")
      }
      applyFilters()
    }, true)

    function contactDetailURL(contactId, syncQueued) {
      var url = "/contacts?partial=detail&contact=" + encodeURIComponent(contactId)
      if (syncQueued) url += "&sync=queued"
      return url
    }

    function refreshContactsDetail(contactId, trigger, syncQueued) {
      if (!contactId || typeof htmx === "undefined") return
      showContactsDetailLoading(trigger)
      htmx.ajax("GET", contactDetailURL(contactId, syncQueued), { target: "#contacts-detail", swap: "outerHTML" })
    }

    function syncContactNow(button) {
      if (!button || button.disabled) return
      var contactID = button.getAttribute("data-contact-id") || ""
      if (!contactID) return
      setupSSE()
      setContactSyncButtonsBusy(contactID, true)
      fetch("/api/contacts/" + encodeURIComponent(contactID) + "/sync-now", {
        method: "POST",
        headers: { "Accept": "application/json" },
      }).then(function (res) {
        if (!res.ok) return res.text().then(function (text) { throw new Error((text || "Could not start Gofer Sync").trim()) })
        return res.json()
      }).then(function (data) {
        updateContactSyncLiveState({ contact_id: contactID, status: (data && data.status) || "pending" })
        showGoferToast({
          id: "contact-sync-toast",
          title: "Gofer Sync queued",
          description: "The selected locations will be synchronized now.",
          variant: "info",
          icon: "spinner",
          position: "bottom-right",
          duration: 0,
          dismissible: false,
        })
      }).catch(function (err) {
        setContactSyncButtonsBusy(contactID, false)
        showGoferToast({
          id: "contact-sync-toast",
          title: "Could not start Gofer Sync",
          description: err && err.message ? err.message : "The contact could not be queued for synchronization.",
          variant: "error",
          icon: "error",
          position: "bottom-right",
          duration: 8000,
          dismissible: true,
        })
      })
    }

    function contactEditorState(form) {
      return new URLSearchParams(new FormData(form)).toString()
    }

    function contactSyncSetupHost() {
      var host = document.getElementById("contact-sync-setup-host")
      if (host) return host
      host = document.createElement("div")
      host.id = "contact-sync-setup-host"
      document.body.appendChild(host)
      return host
    }

    function renderContactSyncSetup(html, replaceDialogID) {
      var host = contactSyncSetupHost()
      if (window.tui && window.tui.dialog && window.tui.dialog.isOpen("contact-sync-setup-dialog")) {
        window.tui.dialog.close("contact-sync-setup-dialog")
      }
      host.innerHTML = html
      window.requestAnimationFrame(function () {
        if (window.tui && window.tui.dialog) {
          if (replaceDialogID) window.tui.dialog.close(replaceDialogID)
          window.tui.dialog.open("contact-sync-setup-dialog")
        }
        startContactSyncSetupSearch(host)
      })
    }

    function startContactSyncSetupSearch(root) {
      var pending = root && root.querySelector("[data-contact-sync-setup-auto-search]")
      if (!pending || pending.dataset.searchStarted === "true") return
      pending.dataset.searchStarted = "true"
      loadContactSyncSetupBody(pending.getAttribute("data-contact-sync-setup-auto-search") || "")
    }

    function renderContactSyncSetupBody(html) {
      var body = document.querySelector("#contact-sync-setup-dialog [data-contact-sync-setup-body]")
      if (!body) {
        renderContactSyncSetup(html)
        return
      }
      body.innerHTML = html
      startContactSyncSetupSearch(body)
    }

    function loadContactSyncSetupBody(url) {
      if (!url) return
      fetch(url, { headers: { "Accept": "text/html" } }).then(function (res) {
        if (!res.ok) return res.text().then(function (text) { throw new Error(text || "Could not search sync locations") })
        return res.text()
      }).then(renderContactSyncSetupBody).catch(function (err) {
        showGoferToast({ id: "contact-sync-setup-error", title: "Sync setup failed", description: err.message || "Could not search sync locations.", variant: "error", icon: "error", position: "bottom-right", duration: 8000, dismissible: true })
      })
    }

    function searchContactSyncLocation(button) {
      var location = button && button.closest("[data-contact-sync-setup-location]")
      var panel = location && location.querySelector("[data-contact-sync-location-search-panel]")
      var input = panel && panel.querySelector("[data-contact-sync-location-search-query]")
      var query = input ? input.value.trim() : ""
      if (!location || !panel || !query) {
        if (input) input.focus()
        return
      }
      var searchURL = new URL(panel.getAttribute("data-contact-sync-location-search-url") || "", window.location.origin)
      searchURL.searchParams.set("query", query)
      button.disabled = true
      fetch(searchURL.pathname + searchURL.search, { headers: { "Accept": "text/html" } }).then(function (res) {
        if (!res.ok) return res.text().then(function (text) { throw new Error(text || "Could not search this account") })
        return res.text()
      }).then(function (html) {
        location.outerHTML = html
      }).catch(function (err) {
        showGoferToast({ id: "contact-sync-location-search-error", title: "Account search failed", description: err.message || "Could not search this account.", variant: "error", icon: "error", position: "bottom-right", duration: 8000, dismissible: true })
      }).finally(function () {
        if (button.isConnected) button.disabled = false
      })
    }

    document.addEventListener("keydown", function (e) {
      if (e.key !== "Enter" || !e.target || !e.target.matches("[data-contact-sync-location-search-query]")) return
      e.preventDefault()
      var panel = e.target.closest("[data-contact-sync-location-search-panel]")
      var button = panel && panel.querySelector("[data-contact-sync-location-search-submit]")
      if (button) searchContactSyncLocation(button)
    })

    function loadContactSyncSetup(url, replaceDialogID) {
      fetch(url, { headers: { "Accept": "text/html" } }).then(function (res) {
        if (!res.ok) return res.text().then(function (text) { throw new Error(text || "Could not load sync setup") })
        return res.text()
      }).then(function (html) {
        renderContactSyncSetup(html, replaceDialogID || "")
      }).catch(function (err) {
        showGoferToast({ id: "contact-sync-setup-error", title: "Sync setup failed", description: err.message || "Could not load sync setup.", variant: "error", icon: "error", position: "bottom-right", duration: 8000, dismissible: true })
      })
    }

    function submitContactSyncSetup(form, confirming) {
      var submit = form.querySelector('button[type="submit"]')
      if (submit) submit.disabled = true
      fetch(form.action, {
        method: "POST",
        body: new URLSearchParams(new FormData(form)),
        headers: { "Accept": confirming ? "application/json" : "text/html", "Content-Type": "application/x-www-form-urlencoded" },
      }).then(function (res) {
        if (!res.ok) return res.text().then(function (text) { throw new Error(text || "Sync setup failed") })
        return confirming ? res.json() : res.text()
      }).then(function (data) {
        if (!confirming) {
          renderContactSyncSetupBody(data)
          return
        }
        if (window.tui && window.tui.dialog) window.tui.dialog.close("contact-sync-setup-dialog")
        var host = document.getElementById("contact-sync-setup-host")
        if (host) window.setTimeout(function () { host.remove() }, 220)
        var syncQueued = !!data.contact_sync_queued
        if (data.contact_id) refreshContactsDetail(data.contact_id, null, syncQueued)
        if (syncQueued) setupSSE()
        showGoferToast({ id: "contact-sync-toast", title: "Gofer Sync enabled", description: syncQueued ? "The resolved contact is being synchronized across all selected locations." : "Sync is enabled for the selected locations.", variant: "success", icon: "success", position: "bottom-right", duration: 5000, dismissible: true })
      }).catch(function (err) {
        showGoferToast({ id: "contact-sync-setup-error", title: "Sync setup failed", description: err.message || "Could not finish sync setup.", variant: "error", icon: "error", position: "bottom-right", duration: 8000, dismissible: true })
      }).finally(function () {
        if (submit) submit.disabled = false
      })
    }

    function setContactAvatarEditorValue(editor, dataURL, action) {
      if (!editor) return
      var actionInput = editor.querySelector("[data-contact-avatar-action]")
      var dataInput = editor.querySelector("[data-contact-avatar-data]")
      var preview = editor.querySelector("[data-contact-avatar-edit-preview]")
      var remove = editor.querySelector("[data-contact-avatar-remove]")
      var status = editor.querySelector("[data-contact-avatar-status]")
      var chooseLabel = editor.querySelector("[data-contact-avatar-choose-label]")
      if (actionInput) actionInput.value = action || "preserve"
      if (dataInput) dataInput.value = dataURL || ""
      if (!preview) return
      var image = preview.querySelector("[data-avatar-image]")
      if (dataURL) {
        if (!image) {
          image = document.createElement("img")
          image.setAttribute("data-avatar-image", "")
          image.alt = "Contact profile picture"
          image.className = "absolute inset-0 size-full object-cover"
          preview.appendChild(image)
        }
        image.src = dataURL
        image.classList.remove("hidden")
        hideContactAvatarFallback(preview)
        if (remove) {
          remove.classList.remove("hidden")
          remove.classList.add("inline-flex")
        }
        if (status) {
          status.textContent = "New picture ready"
          status.classList.remove("hidden", "text-ink/45")
          status.classList.add("text-emerald-700")
        }
        if (chooseLabel) chooseLabel.textContent = "Replace"
      } else {
        if (image) image.remove()
        showContactAvatarFallback(preview)
        if (remove) {
          remove.classList.add("hidden")
          remove.classList.remove("inline-flex")
        }
        if (status) {
          status.textContent = "Picture will be removed"
          status.classList.remove("hidden")
          status.classList.remove("text-emerald-700")
          status.classList.add("text-ink/45")
        }
        if (chooseLabel) chooseLabel.textContent = "Choose image"
      }
    }

    function prepareContactAvatar(file, editor) {
      if (!file || !editor) return
      if (["image/jpeg", "image/png", "image/webp"].indexOf(file.type) === -1) {
        showGoferToast({ id: "contact-avatar-error", title: "Unsupported picture", description: "Choose a JPEG, PNG, or WebP image.", variant: "error", icon: "error", position: "bottom-right", duration: 5000, dismissible: true })
        return
      }
      if (file.size > 8 * 1024 * 1024) {
        showGoferToast({ id: "contact-avatar-error", title: "Picture is too large", description: "Choose an image smaller than 8 MB.", variant: "error", icon: "error", position: "bottom-right", duration: 5000, dismissible: true })
        return
      }
      var objectURL = URL.createObjectURL(file)
      var image = new Image()
      image.onload = function () {
        URL.revokeObjectURL(objectURL)
        var side = Math.min(image.naturalWidth, image.naturalHeight)
        if (!side) return
        var canvas = document.createElement("canvas")
        canvas.width = 512
        canvas.height = 512
        var context = canvas.getContext("2d")
        if (!context) return
        var sourceX = Math.max(0, (image.naturalWidth - side) / 2)
        var sourceY = Math.max(0, (image.naturalHeight - side) / 2)
        context.drawImage(image, sourceX, sourceY, side, side, 0, 0, 512, 512)
        var dataURL = canvas.toDataURL("image/webp", 0.9)
        if (dataURL.indexOf("data:image/webp") !== 0) dataURL = canvas.toDataURL("image/png")
        setContactAvatarEditorValue(editor, dataURL, "replace")
      }
      image.onerror = function () {
        URL.revokeObjectURL(objectURL)
        showGoferToast({ id: "contact-avatar-error", title: "Could not read picture", description: "The selected image appears to be invalid.", variant: "error", icon: "error", position: "bottom-right", duration: 5000, dismissible: true })
      }
      image.src = objectURL
    }

    function updateContactListPreview(contactId, form) {
      if (!contactId || !form) return
      var rows = document.querySelectorAll("[data-contact-id]")
      var row = null
      for (var ri = 0; ri < rows.length; ri++) {
        if (rows[ri].dataset.contactId === contactId) {
          row = rows[ri]
          break
        }
      }
      if (!row) return
      var nameInput = form.querySelector('[name="name"]')
      var emailInput = form.querySelector('[name="email"]')
      var names = row.querySelectorAll("[data-contact-name]")
      var emails = row.querySelectorAll("[data-contact-email]")
      for (var i = 0; i < names.length; i++) names[i].textContent = nameInput ? nameInput.value : ""
      for (var j = 0; j < emails.length; j++) emails[j].textContent = emailInput ? emailInput.value : ""
    }

    function saveContactEditor(form, submitter) {
      if (!form || form.dataset.saving === "true") return
      form.dataset.saving = "true"
      var submit = submitter && submitter.matches && submitter.matches('button[type="submit"]') ? submitter : form.querySelector('button[type="submit"]')
      if (submit) submit.disabled = true
      var formData = new FormData(form)
      var hasActionOverride = submitter && submitter.hasAttribute && submitter.hasAttribute("formaction")
      var hasMethodOverride = submitter && submitter.hasAttribute && submitter.hasAttribute("formmethod")
      var action = hasActionOverride ? submitter.formAction : form.action
      var method = hasMethodOverride ? submitter.formMethod : form.method
      fetch(action, {
        method: (method || "POST").toUpperCase(),
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
        var syncQueued = !!(data && (data.contact_sync_queued || data.gmail_sync_queued))
        var syncSetupURL = data && data.contact_sync_setup_url ? data.contact_sync_setup_url : ""
        if (data && data.avatar_hash) updateVisibleAvatars(data.avatar_hash, data.avatar_url || "")
        if (data && data.contact_id) updateContactListPreview(data.contact_id, form)
        if (data && data.refresh_detail && data.contact_id) {
          refreshContactsDetail(data.contact_id, form, syncQueued)
        }
        if (syncSetupURL) {
          var editDialogID = form.getAttribute("data-contact-edit-dialog") || ""
          loadContactSyncSetup(syncSetupURL, editDialogID)
        } else if (syncQueued) {
          setupSSE()
          showGoferToast({
            id: "contact-sync-toast",
            title: "Contact saved",
            description: "Gofer Sync is updating the selected locations...",
            variant: "info",
            icon: "spinner",
            position: "bottom-right",
            duration: 0,
            dismissible: false,
          })
        } else {
          showGoferToast({
            id: "contact-save-toast",
            title: "Contact saved",
            description: "The aggregate profile has been updated.",
            variant: "success",
            icon: "success",
            position: "bottom-right",
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
          position: "bottom-right",
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
      if (e.target && e.target.matches("[data-contact-avatar-input]")) {
        var file = e.target.files && e.target.files[0]
        prepareContactAvatar(file, e.target.closest("[data-contact-avatar-editor]"))
        e.target.value = ""
        return
      }
      if (e.target && e.target.matches("[data-contact-sync-enabled]")) {
        var form = e.target.closest("form")
        var targets = form && form.querySelector("[data-contact-sync-targets]")
        var label = form && form.querySelector("[data-contact-sync-state-label]")
        var enabled = e.target.checked
        if (targets) {
          targets.classList.toggle("pointer-events-none", !enabled)
          targets.classList.toggle("select-none", !enabled)
          targets.classList.toggle("opacity-45", !enabled)
          targets.setAttribute("aria-disabled", enabled ? "false" : "true")
        }
        if (label) {
          label.textContent = enabled ? "Enabled" : "Disabled"
          label.classList.toggle("border-emerald-500/20", enabled)
          label.classList.toggle("bg-emerald-500/[0.08]", enabled)
          label.classList.toggle("text-emerald-700", enabled)
          label.classList.toggle("dark:text-emerald-300", enabled)
          label.classList.toggle("border-ink/10", !enabled)
          label.classList.toggle("bg-ink/[0.03]", !enabled)
          label.classList.toggle("text-ink/35", !enabled)
        }
        return
      }
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

  function setupEmailTranslation() {
    function setting(key, fallback) {
      if (window.GoferSettings && GoferSettings.get(key)) return GoferSettings.get(key)
      return fallback
    }

    function provider() {
      return setting("translation_provider", "google_web_basic") || "google_web_basic"
    }

    function targetLanguage() {
      return setting("translation_target_language", "en") || "en"
    }

    var translationLanguages = [
      { code: "en", label: "English" },
      { code: "cs", label: "Czech" },
      { code: "de", label: "German" },
      { code: "es", label: "Spanish" },
      { code: "fr", label: "French" },
      { code: "it", label: "Italian" },
      { code: "nl", label: "Dutch" },
      { code: "pl", label: "Polish" },
      { code: "pt", label: "Portuguese" },
      { code: "uk", label: "Ukrainian" },
      { code: "zh-cn", label: "Chinese" },
      { code: "ja", label: "Japanese" },
      { code: "ko", label: "Korean" }
    ]

    function normalizeTranslationLanguageCode(code) {
      var normalized = String(code || "").toLowerCase()
      if (normalized === "zh") normalized = "zh-cn"
      return normalized
    }

    function languageLabel(code) {
      var normalized = normalizeTranslationLanguageCode(code)
      for (var i = 0; i < translationLanguages.length; i++) {
        if (translationLanguages[i].code === normalized) return translationLanguages[i].label
      }
      return code || "selected language"
    }

    function translationEnabled() {
      return setting("translation_button_enabled", "true") !== "false"
    }

    function emailSelector(attr, emailId) {
      return "[" + attr + '="' + String(emailId).replace(/"/g, '\\"') + '"]'
    }

    function frameForEmail(emailId) {
      return document.querySelector('[data-email-body-frame][data-email-id="' + String(emailId).replace(/"/g, '\\"') + '"]')
    }

    function buttonForEmail(emailId) {
      return document.querySelector(emailSelector("data-translate-email", emailId))
    }

    function idleButtonLabel() {
      var target = targetLanguage()
      return target ? "Translate to " + languageLabel(target) : "Translate"
    }

    function activeTargetLanguage(emailId) {
      var frame = frameForEmail(emailId)
      return frame && frame.dataset.translationTargetLanguage ? frame.dataset.translationTargetLanguage : targetLanguage()
    }

    function syncTranslationLanguageItems(emailId) {
      if (!emailId) return
      var active = normalizeTranslationLanguageCode(activeTargetLanguage(emailId))
      var items = document.querySelectorAll(emailSelector("data-translate-email-language", emailId))
      for (var i = 0; i < items.length; i++) {
        items[i].dataset.translationLanguageSelected = normalizeTranslationLanguageCode(items[i].dataset.translationLanguage) === active ? "true" : "false"
      }
    }

    function setButtonState(emailId, state, data) {
      var button = buttonForEmail(emailId)
      if (!button) return
      var label = button.querySelector("[data-translate-email-label]")
      var shell = button.closest("[data-email-translation-button]")
      var text = idleButtonLabel()
      var translated = state === "translated"
      if (state === "loading") text = "Translating..."
      if (state === "error") text = "Translation failed"
      if (translated) text = "Show original"
      if (label && label.textContent !== text) label.textContent = text
      button.disabled = state === "loading"
      button.dataset.translationState = state
      button.dataset.translated = translated ? "true" : "false"
      if (shell) shell.dataset.translated = translated ? "true" : "false"
      button.setAttribute("aria-pressed", translated ? "true" : "false")
      button.setAttribute("aria-label", translated ? "Show original email" : idleButtonLabel())
      button.classList.toggle("opacity-60", state === "loading")
      syncTranslationIconButtons(emailId, state)
      syncTranslationLanguageItems(emailId)
      if (translated && data) {
        button.title = "Translated to " + languageLabel(activeTargetLanguage(emailId))
      } else {
        button.removeAttribute("title")
      }
    }

    function syncTranslationIconButtons(emailId, state) {
      var translated = state === "translated"
      var buttons = document.querySelectorAll(emailSelector("data-translate-email-icon", emailId))
      for (var i = 0; i < buttons.length; i++) {
        buttons[i].disabled = state === "loading"
        buttons[i].dataset.translated = translated ? "true" : "false"
        buttons[i].setAttribute("aria-pressed", translated ? "true" : "false")
        buttons[i].setAttribute("aria-label", translated ? "Show original email" : idleButtonLabel())
        buttons[i].classList.toggle("opacity-60", state === "loading")
        var shell = buttons[i].closest("[data-translate-email-icon-shell]")
        if (shell) shell.dataset.translated = translated ? "true" : "false"
      }
    }

    function syncTranslationButton(button) {
      if (!button || !button.dataset) return
      var frame = frameForEmail(button.dataset.translateEmail)
      if (frame && frame.dataset.translationActive === "true") {
        setButtonState(button.dataset.translateEmail, button.dataset.translationState === "loading" ? "loading" : "translated", true)
        return
      }
      if (!button.disabled) setButtonState(button.dataset.translateEmail, "idle")
    }

    function syncTranslationControls(root) {
      var enabled = translationEnabled()
      var scope = root || document
      if (!scope.querySelectorAll) scope = document
      if (scope.matches && scope.matches("[data-email-translation-shell]")) {
        scope.classList.toggle("hidden", !enabled)
      }
      var shells = scope.querySelectorAll("[data-email-translation-shell]")
      for (var i = 0; i < shells.length; i++) shells[i].classList.toggle("hidden", !enabled)
      var icons = scope.querySelectorAll("[data-translate-email-icon-shell]")
      for (var k = 0; k < icons.length; k++) icons[k].classList.toggle("hidden", !enabled)

      if (scope.matches && scope.matches("[data-translate-email]")) {
        syncTranslationButton(scope)
      }
      var buttons = scope.querySelectorAll("[data-translate-email]")
      for (var j = 0; j < buttons.length; j++) syncTranslationButton(buttons[j])
    }

    function showOriginal(emailId) {
      var frame = frameForEmail(emailId)
      if (frame) {
        delete frame.dataset.translationActive
        delete frame.dataset.translationProvider
        delete frame.dataset.translationTargetLanguage
        delete frame.dataset.translationCacheKey
        if (typeof applyEmailBodyTheme === "function") applyEmailBodyTheme(frame)
      }
      setButtonState(emailId, "idle")
    }

    function setTranslationLoading(emailId) {
      setButtonState(emailId, "loading")
    }

    function setTranslationError(emailId, message) {
      setButtonState(emailId, "error")
      window.setTimeout(function () {
        var button = buttonForEmail(emailId)
        if (button && button.dataset.translationState === "error") syncTranslationButton(button)
      }, message ? 3000 : 1800)
    }

    function translateEmail(emailId, button, targetOverride) {
      var frame = frameForEmail(emailId)
      if (!frame) {
        setTranslationError(emailId)
        return
      }
      if (frame && frame.dataset.translationActive === "true" && !targetOverride) {
        showOriginal(emailId)
        return
      }

      var currentProvider = provider()
      var target = targetOverride || targetLanguage()
      var cacheKey = currentProvider + "|" + target
      frame.dataset.translationActive = "true"
      frame.dataset.translationProvider = currentProvider
      frame.dataset.translationTargetLanguage = target
      frame.dataset.translationCacheKey = cacheKey
      setTranslationLoading(emailId)
      if (button) button.classList.add("opacity-60")
      if (typeof applyEmailBodyTheme === "function") applyEmailBodyTheme(frame)
    }

    window.goferEmailTranslationFrameLoaded = function (emailId) {
      var frame = frameForEmail(emailId)
      if (frame && frame.dataset.translationActive === "true") setButtonState(emailId, "translated", true)
    }

    syncTranslationControls(document)

    document.body.addEventListener("htmx:afterSwap", function (event) {
      syncTranslationControls(event.target || document)
    })
    document.body.addEventListener("htmx:oobAfterSwap", function (event) {
      syncTranslationControls(event.target || document)
    })
    document.body.addEventListener("gofer:settings-changed", function () {
      syncTranslationControls(document)
    })

    document.addEventListener("click", function (e) {
      var language = e.target.closest("[data-translate-email-language]")
      if (language) {
        e.preventDefault()
        if (!translationEnabled()) return
        translateEmail(language.dataset.translateEmailLanguage, buttonForEmail(language.dataset.translateEmailLanguage), language.dataset.translationLanguage)
        return
      }

      var translateIcon = e.target.closest("[data-translate-email-icon]")
      if (translateIcon) {
        e.preventDefault()
        if (!translationEnabled()) return
        translateEmail(translateIcon.dataset.translateEmailIcon, buttonForEmail(translateIcon.dataset.translateEmailIcon))
        return
      }

      var translate = e.target.closest("[data-translate-email]")
      if (translate) {
        e.preventDefault()
        if (!translationEnabled()) return
        translateEmail(translate.dataset.translateEmail, translate)
        return
      }
    })
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

    function readTagState() {
      var raw = window.GoferSettings ? GoferSettings.get("sidebar_tag_group_collapsed") : null
      try {
        return JSON.parse(raw || "{}") || {}
      } catch (_) {
        return {}
      }
    }

    function writeTagState(state) {
      if (window.GoferSettings) GoferSettings.set("sidebar_tag_group_collapsed", JSON.stringify(state))
    }

    function readFolderState() {
      var raw = window.GoferSettings ? GoferSettings.get("sidebar_folder_collapsed") : null
      try {
        return JSON.parse(raw || "{}") || {}
      } catch (_) {
        return {}
      }
    }

    function writeFolderState(state) {
      if (window.GoferSettings) GoferSettings.set("sidebar_folder_collapsed", JSON.stringify(state))
    }

    function setCollapsed(section, collapsed) {
      var toggle = section.querySelector("[data-sidebar-account-toggle]")
      section.setAttribute("data-sidebar-account-collapsed", collapsed ? "true" : "false")
      if (toggle) toggle.setAttribute("aria-expanded", collapsed ? "false" : "true")
    }

    function setTagCollapsed(group, collapsed) {
      var toggle = group.querySelector("[data-sidebar-tag-toggle]")
      group.setAttribute("data-sidebar-tag-collapsed", collapsed ? "true" : "false")
      if (toggle) toggle.setAttribute("aria-expanded", collapsed ? "false" : "true")
    }

    function setFolderCollapsed(group, collapsed) {
      var toggle = group.querySelector("[data-sidebar-folder-toggle]")
      group.setAttribute("data-sidebar-folder-collapsed", collapsed ? "true" : "false")
      if (toggle) toggle.setAttribute("aria-expanded", collapsed ? "false" : "true")
    }

    function sectionHasActiveFolder(section) {
      return section.hasAttribute("data-sidebar-account-active") || !!section.querySelector('a[hx-get^="/folder/"].bg-sidebar-accent')
    }

    function tagGroupHasActiveTag(group) {
      return group.hasAttribute("data-sidebar-tag-active") || !!group.querySelector('a[data-sidebar-tag-filter].bg-sidebar-accent')
    }

    function folderGroupHasActiveDescendant(group) {
      var childContainer = null
      for (var i = 0; i < group.children.length; i++) {
        if (group.children[i].classList && group.children[i].classList.contains("sidebar-folder-children")) {
          childContainer = group.children[i]
          break
        }
      }
      return !!(childContainer && childContainer.querySelector('[data-sidebar-folder-active], a[hx-get^="/folder/"].bg-sidebar-accent'))
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
      var tagState = readTagState()
      var tagGroups = (root || document).querySelectorAll("[data-sidebar-tag-group]")
      for (var j = 0; j < tagGroups.length; j++) {
        var group = tagGroups[j]
        var groupId = group.getAttribute("data-sidebar-tag-group")
        var tagCollapsed = tagState[groupId] === true && !tagGroupHasActiveTag(group)
        setTagCollapsed(group, tagCollapsed)
      }
      var folderState = readFolderState()
      var folderGroups = (root || document).querySelectorAll("[data-sidebar-folder]")
      for (var k = 0; k < folderGroups.length; k++) {
        var folderGroup = folderGroups[k]
        var folderGroupId = folderGroup.getAttribute("data-sidebar-folder")
        var folderCollapsed = folderState[folderGroupId] === true && !folderGroupHasActiveDescendant(folderGroup)
        setFolderCollapsed(folderGroup, folderCollapsed)
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

    document.addEventListener("click", function (e) {
      var toggle = e.target.closest("[data-sidebar-tag-toggle]")
      if (!toggle) return

      e.preventDefault()
      e.stopPropagation()

      var group = toggle.closest("[data-sidebar-tag-group]")
      if (!group) return
      var groupId = group.getAttribute("data-sidebar-tag-group")
      var collapsed = group.getAttribute("data-sidebar-tag-collapsed") !== "true"
      var state = readTagState()
      state[groupId] = collapsed
      writeTagState(state)
      setTagCollapsed(group, collapsed)
    })

    document.addEventListener("click", function (e) {
      var toggle = e.target.closest("[data-sidebar-folder-toggle]")
      if (!toggle) return
      if (toggle.matches && toggle.matches('a[hx-get^="/folder/"]')) return

      e.preventDefault()
      e.stopPropagation()
      e.stopImmediatePropagation()

      var group = toggle.closest("[data-sidebar-folder]")
      if (!group) return
      var groupId = group.getAttribute("data-sidebar-folder")
      var collapsed = group.getAttribute("data-sidebar-folder-collapsed") !== "true"
      var state = readFolderState()
      state[groupId] = collapsed
      writeFolderState(state)
      setFolderCollapsed(group, collapsed)
    })

    document.body.addEventListener("htmx:afterSettle", function (evt) {
      if (evt.target && evt.target.querySelector && (evt.target.querySelector("[data-sidebar-account]") || evt.target.querySelector("[data-sidebar-tag-group]") || evt.target.querySelector("[data-sidebar-folder]"))) {
        hydrate(evt.target)
      }
    })

    hydrate(document)
  }

  function setupMailListActions() {
    window.syncMailSelectionControls = syncMailSelectionControls
    window.clearMailSelection = clearMailSelection
    window.applyOptimisticMailRemove = applyOptimisticRemove

    var suppressMailRowClickUntil = 0
    var mobileMailList = window.matchMedia("(max-width: 1023.98px)")
    var mailSwipeActive = false
    setupMailTouchSelection()
    setupMailPullToRefresh()
    setupMailSwipeActions()

    document.addEventListener("click", function (e) {
      var rowLink = e.target.closest && e.target.closest(".mail-list-item[data-email-id] > a")
      if (rowLink && Date.now() < suppressMailRowClickUntil) {
        // The click that ends a long press must not also open the message.
        e.preventDefault()
        e.stopPropagation()
        return
      }
      var threadToggle = e.target.closest && e.target.closest("#mail-list-scroll [data-thread-toggle]")
      if (threadToggle && mobileMailList.matches && !mailSelectionModeActive()) {
        // On a phone, opening the message would replace the list, so the toggle only
        // expands the thread. Stopping here also keeps htmx on the row from firing.
        e.preventDefault()
        e.stopPropagation()
        var toggleScroll = document.getElementById("mail-list-scroll")
        var toggleList = (toggleScroll && toggleScroll._virtualMailList) || virtualMailList
        if (toggleList) toggleList.toggleThreadExpand(threadToggle.dataset.threadToggle, { select: false })
        return
      }
      var rowClickIgnored = e.target.closest && (e.target.closest(".star-btn") || e.target.closest("[data-thread-toggle]"))
      if (rowLink && !rowClickIgnored) {
        var linkRow = rowLink.closest(".mail-list-item[data-email-id]")
        if (linkRow && linkRow.dataset.emailId) {
          if (e.shiftKey || e.metaKey || e.ctrlKey || mailSelectionModeActive()) {
            e.preventDefault()
            e.stopPropagation()
            var linkNext = e.shiftKey && lastSelectedMailId ? true : !selectedMailIds.has(linkRow.dataset.emailId)
            if (e.shiftKey && lastSelectedMailId) selectMailRange(lastSelectedMailId, linkRow.dataset.emailId, linkNext)
            else setMailSelected(linkRow.dataset.emailId, linkNext)
          } else {
            selectedMailIds.clear()
            setMailSelected(linkRow.dataset.emailId, true)
          }
          lastSelectedMailId = linkRow.dataset.emailId
          syncMailSelectionControls()
          setTimeout(syncMailSelectionControls, 0)
          return
        }
      }

      var mobileAction = e.target.closest && e.target.closest("[data-mobile-mail-action]")
      if (mobileAction) {
        // The phone top bar carries search, sort and filters. Sort and filters keep
        // their triggers in the list header, hidden on small screens; their menus open
        // as bottom sheets, so they need no visible anchor. Stop this tap here, or the
        // menus' click-outside handler closes them again.
        e.preventDefault()
        e.stopPropagation()
        var mobileActionName = mobileAction.getAttribute("data-mobile-mail-action")
        if (mobileActionName === "search") {
          setMailSearchMode(true)
        } else {
          // The filter sheet always opens on its filters, not on the applied list.
          var filterSheetBody = mobileActionName === "filter" && document.querySelector(".mail-filters-sheet-body")
          if (filterSheetBody) filterSheetBody.removeAttribute("data-mail-filters-showing-applied")
          var proxied = document.querySelector(mobileActionName === "sort"
            ? "[data-mail-sort-trigger], [data-contact-sort-trigger]"
            : "[data-mail-filter-button], [data-contact-filter-trigger]")
          if (proxied) proxied.click()
        }
        return
      }

      if (e.target.closest && e.target.closest("[data-mail-search-close]")) {
        e.preventDefault()
        setMailSearchMode(false)
        return
      }

      var clearSelection = e.target.closest && e.target.closest("[data-mail-selection-clear]")
      if (clearSelection) {
        e.preventDefault()
        clearMailSelection()
        return
      }

      var selectionAction = e.target.closest && e.target.closest("[data-mail-selection-action]")
      if (selectionAction) {
        e.preventDefault()
        if (!selectionAction.disabled) performMailSelectionAction(selectionAction.getAttribute("data-mail-selection-action"))
        return
      }

      var starBtn = e.target.closest(".star-btn")
      if (starBtn) {
        e.preventDefault()
        e.stopPropagation()
        e.stopImmediatePropagation()
        var emailId = starBtn.dataset.emailId
        var starRow = starBtn.closest(".mail-list-item")
        // A collapsed thread row stands for the whole thread; expanded messages star alone.
        var threadRow = starRow && starRow.dataset.emailId === emailId && starRow.dataset.hasThread === "true"
        if (emailId && threadRow) toggleThreadStar(emailId, starRow.dataset.threadId)
        else if (emailId) toggleStar(emailId)
      }

      var repairBtn = e.target.closest("[data-repair-account-action]")
      if (repairBtn) {
        var repairAccountId = repairBtn.getAttribute("data-account-id")
        if (repairAccountId && window.tui && window.tui.dialog) {
          window.tui.dialog.close("repair-account-" + repairAccountId)
        }
      }
    }, true)

    document.body.addEventListener("htmx:beforeRequest", function (evt) {
      var path = evt.detail.pathInfo && evt.detail.pathInfo.requestPath
      if (path && path.match(/^\/folder\//)) clearMailSelection()
    })

    // Closing the reader drops the selection that opening the email seeded.
    document.addEventListener("gofer:mail-view-closed", function (evt) {
      var emailId = evt.detail && evt.detail.emailId
      if (selectedMailIds.size === 1 && selectedMailIds.has(emailId)) clearMailSelection()
    })

    document.body.addEventListener("htmx:afterSettle", function () {
      seedMailSelectionFromActive()
      syncMailDeleteActionState()
      syncMailSelectionControls()
    })

    seedMailSelectionFromActive()
    syncMailDeleteActionState()
    syncMailSelectionControls()
    setupMailKeyboardShortcuts()

    function renderedMailRows() {
      return Array.prototype.slice.call(document.querySelectorAll("#mail-list-scroll .mail-list-item[data-email-id]"))
    }

    function setMailSelected(emailId, selected) {
      if (!emailId) return
      if (selected) selectedMailIds.add(emailId)
      else selectedMailIds.delete(emailId)
    }

    function seedMailSelectionFromActive() {
      if (selectedMailIds.size > 0) return
      var active = document.querySelector("#mail-list-scroll .mail-list-item[data-email-id] > a.envelope-active")
      var row = active && active.closest(".mail-list-item[data-email-id]")
      if (!row || !row.dataset.emailId) return
      setMailSelected(row.dataset.emailId, true)
      lastSelectedMailId = row.dataset.emailId
    }

    function selectMailRange(fromId, toId, selected) {
      var rows = renderedMailRows().filter(function (row) { return row.dataset.emailId })
      var fromIndex = -1
      var toIndex = -1
      for (var i = 0; i < rows.length; i++) {
        if (rows[i].dataset.emailId === fromId) fromIndex = i
        if (rows[i].dataset.emailId === toId) toIndex = i
      }
      if (fromIndex === -1 || toIndex === -1) {
        setMailSelected(toId, selected)
        return
      }
      var start = Math.min(fromIndex, toIndex)
      var end = Math.max(fromIndex, toIndex)
      for (var j = start; j <= end; j++) setMailSelected(rows[j].dataset.emailId, selected)
    }

    // On touch screens a long press on a row starts selecting; while selecting,
    // a tap toggles a row instead of opening it.
    // On a phone the search field opens over the top bar from its search button.
    // Contacts share the list shell, so the same search mode serves both.
    function setMailSearchMode(active) {
      var list = document.getElementById("mail-list")
      if (!list) return
      list.toggleAttribute("data-mail-searching", active)
      var input = list.querySelector("[data-mail-search-input], [data-contact-search-input]")
      if (!input) return
      if (active) input.focus()
      else input.blur()
    }

    document.addEventListener("keydown", function (e) {
      if (!e.target || !e.target.matches || !e.target.matches("#mail-list[data-mail-searching] :is([data-mail-search-input], [data-contact-search-input])")) return
      // Enter runs the search (handled elsewhere) and its chip shows above the list.
      if (e.key === "Enter" || e.key === "Escape") setTimeout(function () { setMailSearchMode(false) }, 0)
    })

    function mailSelectionModeActive() {
      var list = document.getElementById("mail-list")
      return !!(list && list.hasAttribute("data-mail-selecting"))
    }

    function setMailSelectionMode(active) {
      var list = document.getElementById("mail-list")
      if (list) list.toggleAttribute("data-mail-selecting", active)
    }

    // Pulling the list down from the top runs the same sync as the drawer's refresh
    // button, so progress and errors show the way they do for that button.
    function setupMailPullToRefresh() {
      var mobile = mobileMailList
      var startX = 0
      var startY = 0
      var tracking = false
      var pulling = false
      var distance = 0
      var refreshing = false

      function remPx(rem) {
        return rem * (parseFloat(getComputedStyle(document.documentElement).fontSize) || 16)
      }

      function indicator() {
        var body = document.querySelector("#mail-list [data-mail-list-body]")
        if (!body) return null
        var el = body.querySelector(":scope > .mail-pull-indicator")
        if (!el) {
          el = document.createElement("div")
          el.className = "mail-pull-indicator"
          el.setAttribute("aria-hidden", "true")
          el.innerHTML = '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M8 16H3v5"/></svg>'
          body.appendChild(el)
        }
        return el
      }

      function show(el, offset, spin) {
        el.style.transform = "translate(-50%, " + offset + "px)"
        el.firstChild.style.transform = spin ? "rotate(" + spin + "deg)" : ""
      }

      function settle(el) {
        el.removeAttribute("data-pulling")
        el.removeAttribute("data-armed")
        el.removeAttribute("data-refreshing")
        el.style.transform = ""
        el.firstChild.style.transform = ""
      }

      function refresh(el) {
        var button = document.querySelector("[data-mail-sidebar-sync-button]")
        refreshing = true
        el.setAttribute("data-refreshing", "")
        show(el, remPx(1), 0)
        var started = Date.now()
        var finished = false
        function done() {
          if (finished) return
          finished = true
          setTimeout(function () {
            refreshing = false
            settle(el)
          }, Math.max(0, 700 - (Date.now() - started)))
        }
        if (!button || button.dataset.syncing === "true") {
          // A sync is already running; its own progress toast reports on it.
          done()
          return
        }
        button.addEventListener("htmx:afterRequest", done, { once: true })
        setTimeout(done, 15000)
        button.click()
      }

      document.addEventListener("touchstart", function (e) {
        tracking = false
        if (!mobile.matches || refreshing || e.touches.length !== 1 || mailSelectionModeActive()) return
        var scroller = e.target.closest && e.target.closest("#mail-list-scroll")
        if (!scroller || scroller.scrollTop > 0) return
        tracking = true
        pulling = false
        distance = 0
        startX = e.touches[0].clientX
        startY = e.touches[0].clientY
      }, { passive: true })

      document.addEventListener("touchmove", function (e) {
        if (!tracking) return
        if (mailSwipeActive) {
          tracking = false
          return
        }
        var scroller = document.getElementById("mail-list-scroll")
        var dx = e.touches[0].clientX - startX
        var dy = e.touches[0].clientY - startY
        if (!pulling) {
          // Start only on a clearly downward drag, leaving sideways ones to row swipes.
          if (dy < 0 || Math.abs(dx) > Math.max(dy, 10) || !scroller || scroller.scrollTop > 0) {
            tracking = false
            return
          }
          if (dy < 10) return
        }
        var el = indicator()
        if (!el) return
        pulling = true
        distance = Math.min(dy * 0.5, remPx(6))
        el.setAttribute("data-pulling", "")
        el.toggleAttribute("data-armed", distance >= remPx(3.5))
        show(el, distance - remPx(2.5), distance * 3)
      }, { passive: true })

      function end() {
        if (!tracking) return
        tracking = false
        if (!pulling) return
        pulling = false
        var el = indicator()
        if (!el) return
        if (distance >= remPx(3.5)) refresh(el)
        else settle(el)
      }
      document.addEventListener("touchend", end, { passive: true })
      document.addEventListener("touchcancel", end, { passive: true })
    }

    // Swiping a row right archives it and swiping left deletes it. A collapsed thread
    // row stands for the whole thread, as it does for the selection bar. Delete is
    // left out in Trash, where it would be permanent.
    function setupMailSwipeActions() {
      var mobile = mobileMailList
      var start = null
      var row = null
      var anchor = null
      var dx = 0
      var axis = ""

      var icons = {
        archive: '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect width="20" height="5" x="2" y="3" rx="1"/><path d="M4 8v11a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8"/><path d="M10 12h4"/></svg>',
        delete: '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 6h18"/><path d="M19 6v14c0 1-1 2-2 2H7c-1 0-2-1-2-2V6"/><path d="M8 6V4c0-1 1-2 2-2h4c1 0 2 1 2 2v2"/><line x1="10" x2="10" y1="11" y2="17"/><line x1="14" x2="14" y1="11" y2="17"/></svg>',
      }

      function deleteIsPermanent() {
        var button = document.querySelector('[data-mail-selection-action="delete"]')
        return !!(button && button.dataset.mailDeletePermanent === "true")
      }

      function actionFor(offset) {
        if (offset > 0) return "archive"
        if (offset < 0 && !deleteIsPermanent()) return "delete"
        return ""
      }

      function backdrop(action) {
        var el = row.querySelector(":scope > .mail-swipe-action")
        if (!el) {
          el = document.createElement("div")
          el.className = "mail-swipe-action"
          el.setAttribute("aria-hidden", "true")
          row.insertBefore(el, row.firstChild)
        }
        if (el.dataset.action !== action) {
          el.dataset.action = action
          el.innerHTML = action ? icons[action] + "<span>" + (action === "archive" ? "Archive" : "Delete") + "</span>" : ""
        }
        return el
      }

      function reset(target, link) {
        if (!target) return
        target.removeAttribute("data-swipe-armed")
        if (!link) return
        link.style.transition = "transform 0.2s ease"
        link.style.transform = ""
        // Keep the row lifted over the action strip until it has slid all the way back.
        setTimeout(function () {
          if (row === target && start) return
          link.style.transition = ""
          target.removeAttribute("data-swiping")
          var el = target.querySelector(":scope > .mail-swipe-action")
          if (el) el.remove()
        }, 220)
      }

      function commit(target, link, action) {
        var emailId = target.dataset.emailId
        var targets = selectedMailTargets([emailId])
        link.style.transition = "transform 0.18s ease-in"
        link.style.transform = "translateX(" + (action === "archive" ? "" : "-") + "100%)"
        var current = virtualMailList && virtualMailList.selectedEmailId
        if (current === emailId) setMailViewEmpty()
        selectedMailIds.delete(emailId)
        var path = action === "archive" ? "/api/messages/archive" : "/api/messages/delete"
        var extra = action === "delete" ? { folder_id: currentMailListFolderID() } : null
        sendBulkMessageAction(path, targets, extra).then(function () {
          if (virtualMailList && typeof virtualMailList.refreshCurrentFolder === "function") {
            virtualMailList.refreshCurrentFolder({ noAnimation: true }).catch(function () {})
          }
          refreshSidebarUnread()
        })
      }

      document.addEventListener("touchstart", function (e) {
        start = null
        if (!mobile.matches || e.touches.length !== 1 || mailSelectionModeActive()) return
        var link = e.target.closest && e.target.closest("#mail-list-scroll .mail-list-item[data-email-id] > a")
        if (!link || e.target.closest(".star-btn, [data-thread-toggle]")) return
        start = { x: e.touches[0].clientX, y: e.touches[0].clientY }
        row = link.parentElement
        anchor = link
        dx = 0
        axis = ""
      }, { passive: true })

      document.addEventListener("touchmove", function (e) {
        if (!start) return
        var t = e.touches[0]
        var mx = t.clientX - start.x
        var my = t.clientY - start.y
        if (!axis) {
          if (Math.abs(mx) < 10 && Math.abs(my) < 10) return
          axis = Math.abs(mx) > Math.abs(my) ? "x" : "y"
          if (axis === "y") {
            start = null
            return
          }
          mailSwipeActive = true
          row.setAttribute("data-swiping", "")
        }
        if (!row.isConnected) return
        var action = actionFor(mx)
        // Without an action that way the row only gives a little.
        dx = action ? mx : mx / 6
        anchor.style.transform = "translateX(" + dx + "px)"
        var el = backdrop(action)
        el.dataset.side = mx > 0 ? "left" : "right"
        row.toggleAttribute("data-swipe-armed", !!action && Math.abs(dx) > row.offsetWidth * 0.35)
      }, { passive: true })

      function end() {
        if (!start) return
        var target = row
        var link = anchor
        start = null
        if (axis !== "x") return
        mailSwipeActive = false
        suppressMailRowClickUntil = Date.now() + 400
        var action = actionFor(dx)
        if (action && target.isConnected && Math.abs(dx) > target.offsetWidth * 0.35) {
          if (navigator.vibrate) navigator.vibrate(12)
          commit(target, link, action)
        } else {
          reset(target, link)
        }
      }
      document.addEventListener("touchend", end, { passive: true })
      document.addEventListener("touchcancel", end, { passive: true })
    }

    function setupMailTouchSelection() {
      var mobile = mobileMailList
      var timer = 0
      var start = null

      function cancel() {
        clearTimeout(timer)
        timer = 0
        start = null
      }

      document.addEventListener("touchstart", function (e) {
        cancel()
        if (!mobile.matches || e.touches.length !== 1) return
        var link = e.target.closest && e.target.closest("#mail-list-scroll .mail-list-item[data-email-id] > a")
        if (!link || e.target.closest(".star-btn, [data-thread-toggle]")) return
        start = { x: e.touches[0].clientX, y: e.touches[0].clientY }
        timer = setTimeout(function () {
          timer = 0
          var row = link.closest(".mail-list-item[data-email-id]")
          if (!row || !row.isConnected) return
          if (!mailSelectionModeActive()) {
            selectedMailIds.clear()
            setMailSelectionMode(true)
          }
          setMailSelected(row.dataset.emailId, !selectedMailIds.has(row.dataset.emailId))
          lastSelectedMailId = row.dataset.emailId
          suppressMailRowClickUntil = Date.now() + 700
          if (navigator.vibrate) navigator.vibrate(12)
          syncMailSelectionControls()
        }, 450)
      }, { passive: true })

      document.addEventListener("touchmove", function (e) {
        if (!start) return
        var t = e.touches[0]
        if (Math.abs(t.clientX - start.x) > 10 || Math.abs(t.clientY - start.y) > 10) cancel()
      }, { passive: true })
      document.addEventListener("touchend", cancel, { passive: true })
      document.addEventListener("touchcancel", cancel, { passive: true })

      // A long press on a link would otherwise open the browser's link menu.
      document.addEventListener("contextmenu", function (e) {
        if (mobile.matches && e.target.closest && e.target.closest("#mail-list-scroll .mail-list-item[data-email-id] > a")) e.preventDefault()
      })
    }

    function clearMailSelection() {
      selectedMailIds.clear()
      lastSelectedMailId = null
      syncMailSelectionControls()
    }

    function syncMailSelectionControls() {
      var rows = renderedMailRows()
      var visibleSelected = 0
      for (var i = 0; i < rows.length; i++) {
        var selected = selectedMailIds.has(rows[i].dataset.emailId)
        if (selected) visibleSelected++
        rows[i].toggleAttribute("data-mail-selected", selected)
        var anchor = rows[i].querySelector(":scope > a")
        if (anchor) anchor.toggleAttribute("data-mail-selected", selected)
      }

      var count = selectedMailIds.size
      if (count === 0) setMailSelectionMode(false)
      var summary = document.querySelector("[data-mail-selection-summary]")
      if (summary) summary.textContent = count === 1 ? "1 selected" : count + " selected"

      var clear = document.querySelector("[data-mail-selection-clear]")
      if (clear) {
        clear.classList.toggle("hidden", count === 0)
        clear.classList.toggle("inline-flex", count > 0)
      }

      var actions = document.querySelectorAll("[data-mail-selection-action]")
      for (var a = 0; a < actions.length; a++) actions[a].disabled = count === 0 || mailSelectionBusy
    }

    function updateCachedRenderedRow(row) {
      if (!row || !virtualMailList || !virtualMailList.cache) return
      var pos = parseInt(row.dataset.position, 10)
      if (isNaN(pos)) return
      var cached = virtualMailList.cache.get(pos)
      if (cached) cached.html = row.outerHTML
    }

    function applyOptimisticRead(emailId) {
      var row = document.querySelector('#mail-list-scroll .mail-list-item[data-email-id="' + cssEscape(emailId) + '"]')
      if (!row) return
      row.setAttribute("data-mail-read-optimistic", "")
      row.removeAttribute("data-mail-selected")
      var anchor = row.querySelector(":scope > a")
      if (anchor) {
        anchor.setAttribute("data-mail-read-optimistic", "")
        anchor.removeAttribute("data-mail-selected")
        var unreadFields = anchor.querySelectorAll('[data-mail-card-field="unread"]')
        for (var u = 0; u < unreadFields.length; u++) {
          var placeholder = document.createElement("span")
          placeholder.dataset.mailCardField = "unread"
          placeholder.className = "mail-list-card-empty-icon-slot"
          placeholder.setAttribute("aria-hidden", "true")
          unreadFields[u].replaceWith(placeholder)
        }
        var unreadDots = anchor.querySelectorAll(".bg-primary")
        for (var i = 0; i < unreadDots.length; i++) {
          if (unreadDots[i].className.indexOf("rounded-full") !== -1) unreadDots[i].remove()
        }
      }
      updateCachedRenderedRow(row)
    }

    function selectedMailTargets(ids) {
      return ids.map(function (emailId) {
        var row = document.querySelector('#mail-list-scroll .mail-list-item[data-email-id="' + cssEscape(emailId) + '"]')
        return {
          id: emailId,
          thread: !!(row && row.dataset.hasThread === "true"),
        }
      })
    }

    function currentMailListFolderID() {
      return mailActionCurrentFolderID()
    }

    function markSelectedReadInBackground(ids) {
      var targets = selectedMailTargets(ids)
      for (var i = 0; i < ids.length; i++) applyOptimisticRead(ids[i])
      clearMailSelection()

      sendBulkMessageAction("/api/messages/read", targets).then(function () {
        if (virtualMailList && typeof virtualMailList.refreshCurrentFolder === "function") {
          virtualMailList.refreshCurrentFolder({ noAnimation: true }).catch(function () {})
        }
        refreshSidebarUnread()
      })
    }

    function sendBulkMessageAction(path, targets, extra) {
      var body = { targets: targets }
      if (extra) {
        for (var key in extra) body[key] = extra[key]
      }
      return fetch(path, {
        method: "POST",
        keepalive: true,
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      })
    }

    function applyOptimisticRemove(ids) {
      for (var i = 0; i < ids.length; i++) {
        var row = document.querySelector('#mail-list-scroll .mail-list-item[data-email-id="' + cssEscape(ids[i]) + '"]')
        if (!row) continue
        row.style.opacity = "0.45"
        row.style.pointerEvents = "none"
      }
    }

    function performMailSelectionAction(action) {
      if (mailSelectionBusy || selectedMailIds.size === 0) return
      var ids = Array.from(selectedMailIds)
      if (action === "label") {
        var labelName = promptMailLabelName()
        if (!labelName) return
        var labelTargets = selectedMailTargets(ids)
        clearMailSelection()
        sendBulkMessageAction("/api/messages/label", labelTargets, { label: labelName, folder_id: currentMailListFolderID() }).then(function () {
          if (virtualMailList && typeof virtualMailList.refreshCurrentFolder === "function") {
            virtualMailList.refreshCurrentFolder({ noAnimation: true }).catch(function () {})
          }
          var currentEmail = virtualMailList && virtualMailList.selectedEmailId
          if (currentEmail && ids.indexOf(currentEmail) !== -1 && window.htmx) {
            htmx.ajax("GET", mailViewRequestURL(currentEmail), { target: "#mail-view", swap: "innerHTML" })
          }
        })
        return
      }
      if (action === "unlabel") {
        var removeLabelName = promptMailLabelName()
        if (!removeLabelName) return
        var unlabelTargets = selectedMailTargets(ids)
        clearMailSelection()
        sendBulkMessageAction("/api/messages/unlabel", unlabelTargets, { label: removeLabelName, folder_id: currentMailListFolderID() }).then(function () {
          if (virtualMailList && typeof virtualMailList.refreshCurrentFolder === "function") {
            virtualMailList.refreshCurrentFolder({ noAnimation: true }).catch(function () {})
          }
          var currentEmail = virtualMailList && virtualMailList.selectedEmailId
          if (currentEmail && ids.indexOf(currentEmail) !== -1 && window.htmx) {
            htmx.ajax("GET", mailViewRequestURL(currentEmail), { target: "#mail-view", swap: "innerHTML" })
          }
        })
        return
      }
      if (action === "read") {
        markSelectedReadInBackground(ids)
        return
      }

      if (action === "archive" || action === "delete" || action === "star" || action === "spam" || action === "not-spam") {
        var targets = selectedMailTargets(ids)
        var current = virtualMailList && virtualMailList.selectedEmailId
        var removesFromFolder = action === "archive" || action === "delete" || action === "spam" || action === "not-spam"
        if (removesFromFolder && current && ids.indexOf(current) !== -1) setMailViewEmpty()
        if (removesFromFolder) applyOptimisticRemove(ids)
        clearMailSelection()
        var path = action === "archive" ? "/api/messages/archive" : (action === "delete" ? "/api/messages/delete" : (action === "spam" ? "/api/messages/spam" : (action === "not-spam" ? "/api/messages/not-spam" : "/api/messages/star")))
        var extra = action === "star" ? { state: "starred" } : null
        if (action === "delete") extra = { folder_id: currentMailListFolderID() }
        if (action === "spam" || action === "not-spam") extra = { folder_id: currentMailListFolderID() }
        sendBulkMessageAction(path, targets, extra).then(function () {
          if (virtualMailList && typeof virtualMailList.refreshCurrentFolder === "function") {
            virtualMailList.refreshCurrentFolder({ noAnimation: action === "star" }).catch(function () {})
          }
          refreshSidebarUnread()
        })
        return
      }

      mailSelectionBusy = true
      syncMailSelectionControls()

      Promise.allSettled(ids.map(function (emailId) {
        if (action === "archive") return fetch("/api/messages/" + encodeURIComponent(emailId) + "/thread/archive", { method: "POST" })
        if (action === "delete") return fetch("/api/messages/" + encodeURIComponent(emailId) + mailDeleteFolderQuery(), { method: "DELETE" })
        return Promise.resolve()
      })).then(function () {
        var mailView = document.getElementById("mail-view")
        var current = virtualMailList && virtualMailList.selectedEmailId
        if ((action === "archive" || action === "delete") && current && ids.indexOf(current) !== -1 && mailView) setMailViewEmpty()
        clearMailSelection()
        for (var i = 0; i < ids.length; i++) invalidateMailListItem(ids[i])
        if (virtualMailList && typeof virtualMailList.refreshCurrentFolder === "function") {
          virtualMailList.refreshCurrentFolder({ noAnimation: action === "read" }).catch(function () {})
        }
        refreshSidebarUnread()
      }).finally(function () {
        mailSelectionBusy = false
        syncMailSelectionControls()
      })
    }

    function setupMailKeyboardShortcuts() {
      if (document.body && document.body._goferMailKeyboardShortcutsBound) return
      if (document.body) document.body._goferMailKeyboardShortcutsBound = true

      document.addEventListener("keydown", function (e) {
        if (document.getElementById("calendar-main") || isShortcutIgnored(e)) return

        if (e.key === "?" || (e.key === "/" && e.shiftKey)) {
          e.preventDefault()
          toggleShortcutHelp()
          return
        }

        if (e.key === "Escape" && closeShortcutHelp()) {
          e.preventDefault()
          return
        }

        if (e.key === "Escape" && (selectedMailIds.size > 0 || selectedMailIdForKeyboard())) {
          e.preventDefault()
          clearKeyboardMailSelection()
          return
        }

        var key = String(e.key || "").toLowerCase()
        if (key === "j" || key === "arrowdown") {
          e.preventDefault()
          moveKeyboardMailSelection(1)
          return
        }
        if (key === "k" || key === "arrowup") {
          e.preventDefault()
          moveKeyboardMailSelection(-1)
          return
        }
        if (key === "enter" || key === "o") {
          e.preventDefault()
          openKeyboardSelectedMail()
          return
        }
        if (key === "/") {
          e.preventDefault()
          focusMailSearch()
          return
        }
        if (key === "c") {
          e.preventDefault()
          if (typeof openNewCompose === "function") openNewCompose()
          return
        }
        if (key === "r") {
          e.preventDefault()
          if (typeof handleReply === "function") handleReply(null, "reply")
          return
        }
        if (key === "a") {
          e.preventDefault()
          if (typeof handleReply === "function") handleReply(null, "reply-all")
          return
        }
        if (key === "f") {
          e.preventDefault()
          if (typeof handleReply === "function") handleReply(null, "forward")
          return
        }
        if (key === "e") {
          e.preventDefault()
          ensureKeyboardMailSelection()
          performMailSelectionAction("archive")
          return
        }
        if (key === "delete" || key === "#") {
          e.preventDefault()
          ensureKeyboardMailSelection()
          performMailSelectionAction("delete")
          return
        }
        if (key === "s") {
          e.preventDefault()
          ensureKeyboardMailSelection()
          performMailSelectionAction("star")
          return
        }
        if (key === "u") {
          e.preventDefault()
          toggleKeyboardSelectedRead()
        }
      })
    }

    function isShortcutIgnored(e) {
      if (!e || e.defaultPrevented) return true
      if (e.metaKey || e.ctrlKey || e.altKey) return true
      var target = e.target
      if (!target || !target.closest) return false

      var help = document.getElementById("mail-shortcut-help")
      if (help && help.contains(target)) return false
      if (target.closest("input, textarea, select, [contenteditable='true'], [data-compose-editor], [data-compose-pane]")) return true

      var openDialog = document.querySelector("dialog[open]")
      if (openDialog && (!help || !help.contains(openDialog))) return true
      return false
    }

    function currentMailListController() {
      var scroll = document.getElementById("mail-list-scroll")
      return (scroll && scroll._virtualMailList) || virtualMailList
    }

    function clearActiveMailSelection() {
      var vml = currentMailListController()
      var activeRows = document.querySelectorAll("#mail-list-scroll .envelope-active")
      var hadActive = !!(vml && vml.selectedEmailId) || activeRows.length > 0
      if (vml) {
        vml.selectedEmailId = null
        if (typeof vml.syncSelectionClasses === "function") vml.syncSelectionClasses(vml.itemsContainer || vml.container)
        if (typeof vml.replaceUrl === "function") vml.replaceUrl()
      } else {
        for (var i = 0; i < activeRows.length; i++) {
          activeRows[i].classList.remove("envelope-active")
          if (activeRows[i].closest(".mail-list-item")) activeRows[i].classList.add("envelope")
        }
      }
      if (hadActive && typeof setMailViewEmpty === "function") setMailViewEmpty()
      return hadActive
    }

    function sortedRenderedMailRows() {
      return renderedMailRows().sort(function (a, b) {
        return (parseInt(a.dataset.position, 10) || 0) - (parseInt(b.dataset.position, 10) || 0)
      })
    }

    function selectedMailIdForKeyboard() {
      if (lastSelectedMailId && selectedMailIds.has(lastSelectedMailId)) return lastSelectedMailId
      if (selectedMailIds.size === 1) return Array.from(selectedMailIds)[0]
      var vml = currentMailListController()
      if (vml && vml.selectedEmailId) return vml.selectedEmailId
      var active = document.querySelector("#mail-list-scroll .mail-list-item[data-email-id] > a.envelope-active")
      var row = active && active.closest(".mail-list-item[data-email-id]")
      return row && row.dataset.emailId ? row.dataset.emailId : null
    }

    function mailRowById(emailId) {
      if (!emailId) return null
      return document.querySelector('#mail-list-scroll .mail-list-item[data-email-id="' + cssEscape(emailId) + '"]')
    }

    function selectKeyboardMailRow(row) {
      if (!row || !row.dataset.emailId) return false
      selectedMailIds.clear()
      setMailSelected(row.dataset.emailId, true)
      lastSelectedMailId = row.dataset.emailId
      syncMailSelectionControls()
      row.scrollIntoView({ block: "nearest" })
      var anchor = row.querySelector(":scope > a")
      if (anchor && typeof anchor.focus === "function") anchor.focus({ preventScroll: true })
      return true
    }

    function ensureKeyboardMailSelection() {
      if (selectedMailIds.size > 0) return true
      var row = mailRowById(selectedMailIdForKeyboard()) || sortedRenderedMailRows()[0]
      return selectKeyboardMailRow(row)
    }

    function clearKeyboardMailSelection() {
      var hadSelection = selectedMailIds.size > 0
      clearMailSelection()
      var hadActive = clearActiveMailSelection()
      var active = document.activeElement
      if (active && active.closest && active.closest("#mail-list-scroll")) active.blur()
      return hadSelection || hadActive
    }

    function moveKeyboardMailSelection(delta) {
      var rows = sortedRenderedMailRows()
      if (!rows.length) return

      var currentId = selectedMailIdForKeyboard()
      var currentIndex = -1
      for (var i = 0; i < rows.length; i++) {
        if (rows[i].dataset.emailId === currentId) {
          currentIndex = i
          break
        }
      }

      if (currentIndex === -1) {
        selectKeyboardMailRow(delta < 0 ? rows[rows.length - 1] : rows[0])
        return
      }

      var targetIndex = currentIndex + delta
      if (targetIndex >= 0 && targetIndex < rows.length) {
        selectKeyboardMailRow(rows[targetIndex])
        return
      }

      moveKeyboardMailSelectionPastRenderedEdge(rows[currentIndex], delta)
    }

    function moveKeyboardMailSelectionPastRenderedEdge(currentRow, delta) {
      var vml = currentMailListController()
      var currentPos = currentRow ? parseInt(currentRow.dataset.position, 10) : NaN
      if (!vml || isNaN(currentPos)) return

      if (vml.navigationMode === "pagination") {
        var nextStart = vml.pageStart + (delta > 0 ? vml.pageSize : -vml.pageSize)
        if (typeof vml.loadPage !== "function" || nextStart < 0 || (delta > 0 && vml.pageStart + vml.pageSize >= vml.totalCount)) return
        vml.loadPage(nextStart, { preserveSelection: false, loadSelected: false }).then(function () {
          var rows = sortedRenderedMailRows()
          selectKeyboardMailRow(delta > 0 ? rows[0] : rows[rows.length - 1])
        }).catch(function () {})
        return
      }

      vml.container.scrollTop += delta * vml.itemHeight
      requestAnimationFrame(function () {
        requestAnimationFrame(function () {
          var rows = sortedRenderedMailRows()
          for (var i = 0; i < rows.length; i++) {
            var pos = parseInt(rows[i].dataset.position, 10)
            if ((delta > 0 && pos > currentPos) || (delta < 0 && pos < currentPos)) {
              selectKeyboardMailRow(rows[i])
              return
            }
          }
        })
      })
    }

    function openKeyboardSelectedMail() {
      ensureKeyboardMailSelection()
      var row = mailRowById(selectedMailIdForKeyboard())
      var anchor = row && row.querySelector(":scope > a")
      if (anchor) anchor.click()
    }

    function focusMailSearch() {
      var input = document.querySelector("[data-mail-search-input]")
      if (!input) return
      input.focus()
      if (typeof input.select === "function") input.select()
    }

    function toggleKeyboardSelectedRead() {
      ensureKeyboardMailSelection()
      var id = selectedMailIdForKeyboard()
      if (!id) return
      var row = mailRowById(id)
      if (row && row.dataset.hasThread === "true" && typeof toggleThreadRead === "function") toggleThreadRead(id, row.dataset.threadId)
      else if (typeof toggleRead === "function") toggleRead(id)
    }

    function shortcutHelpRow(keys, label) {
      return '<div class="flex items-center justify-between gap-5 rounded-md border border-border/60 bg-background/45 px-3 py-2">' +
        '<span class="text-sm text-foreground">' + label + '</span>' +
        '<span class="flex shrink-0 gap-1">' + keys.map(function (key) {
          return '<kbd class="min-w-6 rounded border border-border bg-card px-1.5 py-0.5 text-center text-[11px] font-semibold text-muted-foreground shadow-sm">' + key + '</kbd>'
        }).join('') + '</span>' +
      '</div>'
    }

    function shortcutHelpHTML() {
      return '<div id="mail-shortcut-help" class="fixed inset-0 z-[1000] flex items-center justify-center bg-background/70 px-4 backdrop-blur-sm" role="dialog" aria-modal="true" aria-label="Keyboard shortcuts">' +
        '<div class="w-full max-w-lg rounded-2xl border border-border bg-card p-5 shadow-raised animate-fade-in">' +
          '<div class="mb-4 flex items-start justify-between gap-3">' +
            '<div><h2 class="text-lg font-bold tracking-tight" style="font-family: var(--font-serif)">Keyboard shortcuts</h2><p class="mt-1 text-xs text-muted-foreground">Shortcuts are disabled while typing or composing.</p></div>' +
            '<button type="button" class="rounded-md border border-border px-2 py-1 text-xs font-semibold text-muted-foreground hover:bg-accent hover:text-foreground" data-mail-shortcut-help-close>Esc</button>' +
          '</div>' +
          '<div class="grid gap-2 sm:grid-cols-2">' +
            shortcutHelpRow(['j', 'Down'], 'Next email') +
            shortcutHelpRow(['k', 'Up'], 'Previous email') +
            shortcutHelpRow(['Enter', 'o'], 'Open email') +
            shortcutHelpRow(['/'], 'Focus search') +
            shortcutHelpRow(['c'], 'Compose') +
            shortcutHelpRow(['r'], 'Reply') +
            shortcutHelpRow(['a'], 'Reply all') +
            shortcutHelpRow(['f'], 'Forward') +
            shortcutHelpRow(['e'], 'Archive selected') +
            shortcutHelpRow(['Del', '#'], 'Delete selected') +
            shortcutHelpRow(['s'], 'Star selected') +
            shortcutHelpRow(['u'], 'Toggle read') +
            shortcutHelpRow(['Esc'], 'Clear selection') +
          '</div>' +
        '</div>' +
      '</div>'
    }

    function toggleShortcutHelp() {
      if (closeShortcutHelp()) return
      document.body.insertAdjacentHTML("beforeend", shortcutHelpHTML())
      var overlay = document.getElementById("mail-shortcut-help")
      if (!overlay) return
      overlay.addEventListener("click", function (e) {
        if (e.target === overlay || (e.target.closest && e.target.closest("[data-mail-shortcut-help-close]"))) closeShortcutHelp()
      })
    }

    function closeShortcutHelp() {
      var help = document.getElementById("mail-shortcut-help")
      if (!help) return false
      help.remove()
      return true
    }
  }

  function setupMailFilters() {
    var searchTimer = null
    var committedQuery = ""
    var committedParticipant = ""
    var contactFilterSearchTimer = null
    var contactFilterSearchSequence = 0
    var activePillOverflowFrame = null
    var filterResultsTimer = null
    var activePillAnimationDelay = 190

    function emptyFilters() {
      return {
        unread: false,
        starred: false,
        attachments: false,
        read: false,
        noAttachments: false,
        hasTags: false,
        noTags: false,
        threadsOnly: false,
        noThreads: false,
        participant: "",
        from: "",
        to: "",
        recipientType: "",
        recipientDomain: "",
        subject: "",
        body: "",
        fromDomain: "",
        attachment: "",
        attachmentType: "",
        attachmentExtension: "",
        minSizeMB: "",
        maxSizeMB: "",
        tag: "",
        accountId: "",
        query: "",
        afterDate: "",
        beforeDate: "",
        sortBy: "date",
        sortOrder: "desc",
      }
    }

    function promptMailLabelName() {
      var value = window.prompt("Label name")
      if (value == null) return ""
      return String(value).trim()
    }

    function readFilters() {
      var filters = emptyFilters()
      var form = document.querySelector("[data-mail-advanced-filter-form]") || document.querySelector("[data-mail-filter-form]")
      if (form) {
        var status = form.querySelector('[data-mail-tristate="status"]')
        var attachments = form.querySelector('[data-mail-tristate="attachments"]')
        var tags = form.querySelector('[data-mail-tristate="tags"]')
        var threads = form.querySelector('[data-mail-tristate="threads"]')
        var statusValue = status ? status.getAttribute("data-mail-tristate-value") : ""
        var attachmentValue = attachments ? attachments.getAttribute("data-mail-tristate-value") : ""
        var tagValue = tags ? tags.getAttribute("data-mail-tristate-value") : ""
        var threadValue = threads ? threads.getAttribute("data-mail-tristate-value") : ""
        filters.unread = statusValue === "unread"
        filters.read = statusValue === "read"
        filters.attachments = attachmentValue === "yes"
        filters.noAttachments = attachmentValue === "no"
        filters.starred = !!form.querySelector('input[name="starred"]:checked')
        filters.hasTags = tagValue === "yes"
        filters.noTags = tagValue === "no"
        filters.threadsOnly = threadValue === "yes"
        filters.noThreads = threadValue === "no"
      }
      var advanced = document.querySelector("[data-mail-advanced-filter-form]")
      if (advanced) {
        filters.participant = ((advanced.querySelector('input[name="participant"]') || {}).value || "").trim()
        filters.from = (advanced.querySelector('input[name="from"]') || {}).value || ""
        filters.to = (advanced.querySelector('input[name="to"]') || {}).value || ""
        filters.recipientType = (advanced.querySelector('input[name="recipient_type"]') || {}).value || ""
        filters.recipientDomain = (advanced.querySelector('input[name="recipient_domain"]') || {}).value || ""
        filters.subject = (advanced.querySelector('input[name="subject"]') || {}).value || ""
        filters.body = (advanced.querySelector('input[name="body"]') || {}).value || ""
        filters.fromDomain = (advanced.querySelector('input[name="from_domain"]') || {}).value || ""
        filters.attachment = (advanced.querySelector('input[name="attachment"]') || {}).value || ""
        filters.attachmentType = (advanced.querySelector('input[name="attachment_type"]') || {}).value || ""
        filters.attachmentExtension = filters.attachmentType === "custom" ? (((advanced.querySelector('input[name="attachment_extension"]') || {}).value || "").trim().replace(/^\./, "").toLowerCase()) : ""
        filters.minSizeMB = (advanced.querySelector('input[name="min_size_mb"]') || {}).value || ""
        filters.maxSizeMB = (advanced.querySelector('input[name="max_size_mb"]') || {}).value || ""
        var minSize = parseFloat(filters.minSizeMB)
        var maxSize = parseFloat(filters.maxSizeMB)
        if (!isFinite(minSize) || minSize <= 0) filters.minSizeMB = ""
        if (!isFinite(maxSize) || maxSize <= 0) filters.maxSizeMB = ""
        if (filters.minSizeMB && filters.maxSizeMB && minSize > maxSize) {
          var originalMin = filters.minSizeMB
          filters.minSizeMB = filters.maxSizeMB
          filters.maxSizeMB = originalMin
        }
        filters.tag = (advanced.querySelector('input[name="tag"]') || {}).value || ""
        filters.accountId = (advanced.querySelector('input[name="account_id"]') || {}).value || ""
        filters.afterDate = (advanced.querySelector('input[name="after_date"]') || {}).value || ""
        filters.beforeDate = (advanced.querySelector('input[name="before_date"]') || {}).value || ""
      }
      var sortForm = document.querySelector("[data-mail-sort-form]")
      if (sortForm) {
        filters.sortBy = (sortForm.querySelector('[name="sort_by"]') || {}).value || "date"
        filters.sortOrder = (sortForm.querySelector('[name="sort_order"]') || {}).value || "desc"
      }
      var search = document.querySelector("[data-mail-search-input]")
      var pendingQuery = search ? (search.value || "").trim() : ""
      filters.query = [committedQuery, pendingQuery].filter(Boolean).join(" ")
      if (!advanced) filters.participant = committedParticipant
      return filters
    }

    function escapeHTML(value) {
      return String(value || "").replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;")
    }

    function syncFilterButton(filters) {
      var count = (filters.unread ? 1 : 0) + (filters.starred ? 1 : 0) + (filters.attachments ? 1 : 0) +
        (filters.read ? 1 : 0) + (filters.noAttachments ? 1 : 0) + (filters.hasTags ? 1 : 0) + (filters.noTags ? 1 : 0) +
        (filters.threadsOnly ? 1 : 0) + (filters.noThreads ? 1 : 0) + (filters.participant ? 1 : 0) + (filters.from ? 1 : 0) + (filters.to ? 1 : 0) +
        (filters.recipientType ? 1 : 0) + (filters.recipientDomain ? 1 : 0) + (filters.subject ? 1 : 0) + (filters.body ? 1 : 0) + (filters.fromDomain ? 1 : 0) +
        (filters.attachment ? 1 : 0) + (filters.attachmentType ? 1 : 0) + (filters.minSizeMB ? 1 : 0) + (filters.maxSizeMB ? 1 : 0) + (filters.tag ? 1 : 0) + (filters.accountId ? 1 : 0) +
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
      var mirror = document.querySelector("[data-mail-filter-count-mirror]")
      if (mirror) {
        mirror.textContent = String(count)
        mirror.classList.toggle("hidden", count === 0)
      }
    }

    function advancedFilterDefs() {
      return [
        { key: "participant", name: "participant", label: "Contact" },
        { key: "accountId", name: "account_id", label: "Account" },
        { key: "afterDate", name: "after_date", label: "After" },
        { key: "beforeDate", name: "before_date", label: "Before" },
        { key: "from", name: "from", label: "From" },
        { key: "fromDomain", name: "from_domain", label: "From domain" },
        { key: "to", name: "to", label: "Recipients" },
        { key: "recipientType", name: "recipient_type", label: "Recipient type" },
        { key: "recipientDomain", name: "recipient_domain", label: "Recipient domain" },
        { key: "subject", name: "subject", label: "Subject" },
        { key: "body", name: "body", label: "Body" },
        { key: "attachment", name: "attachment", label: "Attachment" },
        { key: "attachmentType", name: "attachment_type", label: "Attachment type" },
        { key: "minSizeMB", name: "min_size_mb", label: "Minimum size" },
        { key: "maxSizeMB", name: "max_size_mb", label: "Maximum size" },
        { key: "tag", name: "tag", label: "Tag" },
        { key: "unread", name: "unread", label: "Unread" },
        { key: "read", name: "read", label: "Read" },
        { key: "attachments", name: "attachments", label: "Has attachments" },
        { key: "noAttachments", name: "no_attachments", label: "No attachments" },
        { key: "hasTags", name: "has_tags", label: "Has tags" },
        { key: "noTags", name: "no_tags", label: "No tags" },
        { key: "threadsOnly", name: "threads_only", label: "Threads only" },
        { key: "noThreads", name: "no_threads", label: "Single messages" },
      ]
    }

    function displayValueForAdvanced(name, value) {
      if (name === "account_id") {
        var item = document.querySelector('[data-tui-selectbox-value="' + value + '"]')
        return item ? item.textContent.trim() : value
      }
      return value
    }

    function syncAttachmentExtensionField() {
      var form = document.querySelector("[data-mail-advanced-filter-form]")
      if (!form) return
      var typeInput = form.querySelector('input[name="attachment_type"]')
      var extensionInput = form.querySelector('input[name="attachment_extension"]')
      var field = form.querySelector("[data-mail-attachment-extension-field]")
      var custom = !!typeInput && typeInput.value === "custom"
      if (field) field.classList.toggle("hidden", !custom)
      if (extensionInput) {
        extensionInput.required = custom
        if (!custom) extensionInput.value = ""
      }
    }

    function syncAdvancedFilterCount() {
      var counter = document.querySelector("[data-mail-advanced-filter-count]")
      if (!counter) return
      var filters = readFilters()
      var defs = advancedFilterDefs()
      var count = 0
      for (var i = 0; i < defs.length; i++) {
        if (filters[defs[i].key]) count++
      }
      counter.textContent = String(count)
      counter.classList.toggle("hidden", count === 0)
    }

    function switchAdvancedFilterPanel(panelButton) {
      var panel = panelButton.getAttribute("data-mail-filter-panel-button")
      var sections = Array.prototype.slice.call(document.querySelectorAll("[data-mail-filter-panel]"))
      var nextSection = null
      for (var i = 0; i < sections.length; i++) {
        if (sections[i].getAttribute("data-mail-filter-panel") === panel) nextSection = sections[i]
      }
      if (!nextSection || !nextSection.classList.contains("hidden")) return

      document.querySelectorAll("[data-mail-filter-panel-button]").forEach(function (btn) {
        var active = btn === panelButton
        btn.setAttribute("data-active", active ? "true" : "false")
        btn.setAttribute("aria-selected", active ? "true" : "false")
      })

      var box = nextSection.closest("[data-mail-filter-panel-box]")
      var oldHeight = box ? box.getBoundingClientRect().height : 0
      for (var j = 0; j < sections.length; j++) sections[j].classList.toggle("hidden", sections[j] !== nextSection)
      if (!box || window.matchMedia("(prefers-reduced-motion: reduce)").matches) return

      if (box._mailFilterPanelTimer) window.clearTimeout(box._mailFilterPanelTimer)
      box.style.height = ""
      box.style.overflow = ""
      var nextHeight = box.getBoundingClientRect().height
      box.style.height = oldHeight + "px"
      box.style.overflow = "hidden"
      box.offsetHeight
      box.style.height = nextHeight + "px"
      animateSectionContent(nextSection)
      box._mailFilterPanelTimer = window.setTimeout(function () {
        box._mailFilterPanelTimer = null
        box.style.height = ""
        box.style.overflow = ""
      }, 220)
    }

    function scheduleActivePillOverflow(bar) {
      if (activePillOverflowFrame) cancelAnimationFrame(activePillOverflowFrame)
      activePillOverflowFrame = requestAnimationFrame(function () {
        activePillOverflowFrame = requestAnimationFrame(function () {
          activePillOverflowFrame = null
          syncActivePillOverflow(bar)
        })
      })
    }

    function retryActivePillOverflow(bar) {
      if (!bar || bar._mailPillOverflowRetry) return
      bar._mailPillOverflowRetry = window.setTimeout(function () {
        bar._mailPillOverflowRetry = null
        syncActivePillOverflow(bar)
      }, 80)
    }

    function setupActivePillResizeObserver(box, bar) {
      if (!box || box._mailPillResizeBound) return
      box._mailPillResizeBound = true
      if (typeof ResizeObserver === "function") {
        box._mailPillResizeObserver = new ResizeObserver(function () {
          scheduleActivePillOverflow(bar)
        })
        box._mailPillResizeObserver.observe(box)
      } else {
        window.addEventListener("resize", function () {
          scheduleActivePillOverflow(bar)
        })
      }
    }

    function ensureActivePillOverflowPanel(box) {
      if (!box) return null
      var existing = box.querySelector("[data-mail-active-filter-overflow-panel]")
      if (existing) return existing
      var panel = document.createElement("div")
      panel.setAttribute("data-mail-active-filter-overflow-panel", "")
      panel.className = "mail-active-filter-overflow-panel hidden"
      box.appendChild(panel)
      return panel
    }

    function ensureActivePillBar() {
      var bar = document.querySelector("[data-mail-active-filter-pills]")
      if (!bar) return null
      ensureActivePillOverflowPanel(bar)
      setupActivePillResizeObserver(bar, bar)
      return bar
    }

    function activePillDefs(filters) {
      filters = filters || emptyFilters()
      var pills = []
      var query = (filters.query || committedQuery || "").trim()
      if (query) pills.push({ name: "q", label: "Search", value: query })
      if (filters.unread) pills.push({ name: "unread", label: "Status", value: "Unread" })
      if (filters.read) pills.push({ name: "read", label: "Status", value: "Read" })
      if (filters.starred) pills.push({ name: "starred", label: "Starred" })
      if (filters.attachments) pills.push({ name: "attachments", label: "Attachments", value: "Yes" })
      if (filters.noAttachments) pills.push({ name: "no_attachments", label: "Attachments", value: "No" })
      if (filters.hasTags) pills.push({ name: "has_tags", label: "Tags", value: "Yes" })
      if (filters.noTags) pills.push({ name: "no_tags", label: "Tags", value: "No" })
      if (filters.threadsOnly) pills.push({ name: "threads_only", label: "Threads", value: "Yes" })
      if (filters.noThreads) pills.push({ name: "no_threads", label: "Threads", value: "No" })
      if (filters.participant) pills.push({ name: "participant", label: "Contact", value: filters.participant })
      if (filters.accountId) pills.push({ name: "account_id", label: "Account", value: displayValueForAdvanced("account_id", filters.accountId) })
      if (filters.afterDate) pills.push({ name: "after_date", label: "After", value: filters.afterDate })
      if (filters.beforeDate) pills.push({ name: "before_date", label: "Before", value: filters.beforeDate })
      if (filters.from) pills.push({ name: "from", label: "From", value: filters.from })
      if (filters.fromDomain) pills.push({ name: "from_domain", label: "From domain", value: filters.fromDomain })
      if (filters.to) pills.push({ name: "to", label: "Recipients", value: filters.to })
      if (filters.recipientType) pills.push({ name: "recipient_type", label: "Recipient type", value: filters.recipientType.toUpperCase() })
      if (filters.recipientDomain) pills.push({ name: "recipient_domain", label: "Recipient domain", value: filters.recipientDomain })
      if (filters.subject) pills.push({ name: "subject", label: "Subject", value: filters.subject })
      if (filters.body) pills.push({ name: "body", label: "Body", value: filters.body })
      if (filters.attachment) pills.push({ name: "attachment", label: "Attachment", value: filters.attachment })
      if (filters.attachmentType) pills.push({ name: "attachment_type", label: "Attachment type", value: attachmentTypeLabel(filters.attachmentType, filters.attachmentExtension) })
      if (filters.minSizeMB) pills.push({ name: "min_size_mb", label: "Minimum size", value: filters.minSizeMB + " MB" })
      if (filters.maxSizeMB) pills.push({ name: "max_size_mb", label: "Maximum size", value: filters.maxSizeMB + " MB" })
      if (filters.tag) pills.push({ name: "tag", label: "Tag", value: filters.tag })
      return pills
    }

    function attachmentTypeLabel(type, extension) {
      if (type === "custom") return "." + extension
      if (type === "pdf") return "PDF"
      return type.charAt(0).toUpperCase() + type.slice(1)
    }

    function activePillText(pill) {
      return pill.value ? (pill.label + ": " + pill.value) : pill.label
    }

    function activePillKey(pill) {
      return JSON.stringify([pill.name || "", pill.value || ""])
    }

    function activePillButtonHTML(pill, attrs, className) {
      return '<button type="button" ' + (attrs || "") + ' data-mail-active-filter-remove="' + escapeHTML(pill.name) + '" class="' + (className || "mail-active-filter-pill") + '">' +
        '<span class="mail-active-filter-pill-label">' + escapeHTML(activePillText(pill)) + '</span><span class="mail-active-filter-pill-remove">x</span></button>'
    }

    function closeActivePillOverflow() {
      document.querySelectorAll("[data-mail-active-filter-overflow]").forEach(function (button) {
        button.setAttribute("aria-expanded", "false")
      })
      document.querySelectorAll("[data-mail-active-filter-overflow-panel]").forEach(function (panel) {
        panel.classList.add("hidden")
      })
    }

    function renderActivePillOverflowPanel(panel, hiddenPills) {
      if (!panel) return
      var html = ""
      for (var i = 0; i < hiddenPills.length; i++) {
        html += activePillButtonHTML(hiddenPills[i], "", "mail-active-filter-panel-pill")
      }
      panel.innerHTML = html
      if (!hiddenPills.length) panel.classList.add("hidden")
    }

    function hideActivePillBar(bar, panel) {
      if (!bar) return
      if (bar._mailPillClearTimer) clearTimeout(bar._mailPillClearTimer)
      closeActivePillOverflow()
      renderActivePillOverflowPanel(panel, [])
      bar.classList.add("hidden")
      bar._mailPillClearTimer = window.setTimeout(function () {
        bar._mailPillClearTimer = null
        if (bar.classList.contains("hidden")) bar.innerHTML = ""
      }, activePillAnimationDelay)
    }

    function syncActivePillOverflow(bar) {
      if (!bar || bar.classList.contains("hidden")) return
      var panel = ensureActivePillOverflowPanel(bar)
      var visible = bar.querySelector("[data-mail-active-filter-visible]")
      var pillButtons = Array.prototype.slice.call(bar.querySelectorAll("[data-mail-active-filter-pill]"))
      var overflow = bar.querySelector("[data-mail-active-filter-overflow]")
      if (!visible || !overflow || !pillButtons.length) return

      for (var i = 0; i < pillButtons.length; i++) pillButtons[i].classList.remove("hidden")
      overflow.classList.add("hidden")
      overflow.setAttribute("aria-expanded", "false")
      if (panel) panel.classList.add("hidden")

      var barStyle = window.getComputedStyle(bar)
      var visibleStyle = window.getComputedStyle(visible)
      var barGap = parseFloat(barStyle.columnGap || barStyle.gap) || 0
      var pillGap = parseFloat(visibleStyle.columnGap || visibleStyle.gap) || 0
      var barWidth = Math.max(0, Math.floor(bar.getBoundingClientRect().width || bar.clientWidth || 0))
      var available = Math.max(0, Math.floor(visible.getBoundingClientRect().width || visible.clientWidth || 0))
      var overflowOnly = visibleStyle.display === "none"
      var widths = pillButtons.map(function (button) { return button.offsetWidth })
      if (barWidth <= 1 || (!overflowOnly && widths.some(function (width) { return width <= 1 }))) {
        retryActivePillOverflow(bar)
        renderActivePillOverflowPanel(panel, [])
        return
      }
      if (overflowOnly) available = 0
      var total = 0
      for (var t = 0; t < widths.length; t++) total += widths[t] + (t > 0 ? pillGap : 0)
      if (total <= available) {
        renderActivePillOverflowPanel(panel, [])
        return
      }

      overflow.textContent = "+" + pillButtons.length
      overflow.classList.remove("hidden")
      overflow.style.visibility = "hidden"
      var overflowWidth = Math.max(0, Math.ceil(overflow.getBoundingClientRect().width || overflow.offsetWidth || 0))
      overflow.style.visibility = ""
      available = Math.max(0, barWidth - overflowWidth - barGap)
      var visibleCount = 0
      var used = 0
      for (var p = 0; p < widths.length; p++) {
        var nextUsed = used + (visibleCount > 0 ? pillGap : 0) + widths[p]
        if (nextUsed <= available) {
          used = nextUsed
          visibleCount++
        } else {
          break
        }
      }

      var hiddenPills = []
      for (var h = 0; h < pillButtons.length; h++) {
        var hidden = h >= visibleCount
        pillButtons[h].classList.toggle("hidden", hidden)
        if (hidden) {
          var pillData = pillButtons[h].getAttribute("data-mail-active-filter-pill")
          if (pillData) {
            try { hiddenPills.push(JSON.parse(pillData)) } catch (_) {}
          }
        }
      }
      overflow.textContent = "+" + hiddenPills.length
      overflow.classList.toggle("hidden", hiddenPills.length === 0)
      renderActivePillOverflowPanel(panel, hiddenPills)
    }

    // Small screens summarise the applied filters inside the filter sheet ("3 filters
    // applied") instead of in a strip above the list. The summary opens a list of them,
    // one full-width row each, where they can be removed. The top bar's search button
    // is marked while a search is on.
    function renderAppliedFilterSheet(pills) {
      var summary = document.querySelector("[data-mail-filters-applied]")
      if (summary) {
        var countLabel = summary.querySelector("[data-mail-filters-applied-count]")
        if (countLabel) countLabel.textContent = pills.length === 1 ? "1 filter applied" : pills.length + " filters applied"
        summary.classList.toggle("hidden", pills.length === 0)
      }
      var list = document.querySelector("[data-mail-filters-applied-list]")
      if (list) {
        var html = ""
        for (var i = 0; i < pills.length; i++) {
          var pill = pills[i]
          html += '<div class="mail-filters-applied-row">' +
            '<div class="mail-filters-applied-row-text"><span class="mail-filters-applied-row-label">' + escapeHTML(pill.label) + '</span>' +
            (pill.value ? '<span class="mail-filters-applied-row-value">' + escapeHTML(String(pill.value)) + '</span>' : "") + '</div>' +
            '<button type="button" class="mail-filters-applied-row-remove" data-mail-active-filter-remove="' + escapeHTML(pill.name) + '" aria-label="Remove ' + escapeHTML(activePillText(pill)) + '">' +
            '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg>' +
            '</button></div>'
        }
        list.innerHTML = html
      }
      if (!pills.length) showAppliedFilterList(false)
      var searchButton = document.querySelector('[data-mobile-mail-action="search"]')
      if (searchButton) {
        searchButton.toggleAttribute("data-active", pills.some(function (pill) { return pill.name === "q" }))
      }
    }

    function showAppliedFilterList(show) {
      var body = document.querySelector(".mail-filters-sheet-body")
      if (body) body.toggleAttribute("data-mail-filters-showing-applied", show)
    }

    document.addEventListener("click", function (e) {
      if (!e.target || !e.target.closest) return
      if (e.target.closest("[data-mail-filters-applied-open]")) showAppliedFilterList(true)
      else if (e.target.closest("[data-mail-filters-applied-close]")) showAppliedFilterList(false)
    })

    function renderActivePills(filters) {
      filters = filters || readFilters()
      renderAppliedFilterSheet(activePillDefs(filters))
      var bar = ensureActivePillBar()
      if (!bar) return
      var pills = activePillDefs(filters)
      var panel = ensureActivePillOverflowPanel(bar)
      var existingKeys = Object.create(null)
      var hadOverflow = !!bar.querySelector("[data-mail-active-filter-overflow]")
      bar.querySelectorAll("[data-mail-active-filter-key]").forEach(function (pill) {
        existingKeys[pill.getAttribute("data-mail-active-filter-key") || ""] = true
      })
      if (!pills.length) {
        hideActivePillBar(bar, panel)
        return
      }
      if (bar._mailPillClearTimer) {
        clearTimeout(bar._mailPillClearTimer)
        bar._mailPillClearTimer = null
      }
      var html = '<div data-mail-active-filter-visible class="mail-active-filter-visible">'
      for (var i = 0; i < pills.length; i++) {
        var pill = pills[i]
        var key = activePillKey(pill)
        var pillClass = existingKeys[key] ? "mail-active-filter-pill mail-active-filter-pill-stable" : "mail-active-filter-pill"
        html += activePillButtonHTML(pill, 'data-mail-active-filter-pill="' + escapeHTML(JSON.stringify(pill)) + '" data-mail-active-filter-key="' + escapeHTML(key) + '"', pillClass)
      }
      html += '</div><button type="button" data-mail-active-filter-overflow class="mail-active-filter-overflow ' + (hadOverflow ? "mail-active-filter-overflow-stable " : "") + 'hidden" aria-expanded="false" aria-label="Show hidden filters">+0</button>'
      bar.innerHTML = html
      bar.classList.remove("hidden")
      ensureActivePillOverflowPanel(bar)
      scheduleActivePillOverflow(bar)
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
      for (var t = 0; t < tristates.length; t++) setFilterTriState(tristates[t].getAttribute("data-mail-tristate"), "")
      syncAttachmentExtensionField()
      syncAdvancedFilterCount()
      renderActivePills()
    }

    function setBooleanFilterControl(name, value) {
      var inputs = document.querySelectorAll('[data-mail-filter-form] input[name="' + name + '"], [data-mail-advanced-filter-form] input[name="' + name + '"]')
      for (var i = 0; i < inputs.length; i++) inputs[i].checked = !!value
    }

    function setFilterTriState(name, value) {
      var controls = document.querySelectorAll('[data-mail-tristate="' + name + '"]')
      for (var i = 0; i < controls.length; i++) setTriState(controls[i], value)
    }

    function syncFilterControls(filters) {
      filters = filters || emptyFilters()
      committedQuery = (filters.query || "").trim()
      committedParticipant = (filters.participant || "").trim()
      var search = document.querySelector("[data-mail-search-input]")
      if (search) {
        search.dataset.mailCommittedQuery = committedQuery
        search.value = ""
      }
      setFilterTriState("status", filters.unread ? "unread" : (filters.read ? "read" : ""))
      setFilterTriState("attachments", filters.attachments ? "yes" : (filters.noAttachments ? "no" : ""))
      setFilterTriState("tags", filters.hasTags ? "yes" : (filters.noTags ? "no" : ""))
      setFilterTriState("threads", filters.threadsOnly ? "yes" : (filters.noThreads ? "no" : ""))
      setBooleanFilterControl("starred", !!filters.starred)
      setContactFilterValue(committedParticipant)
      setInputValue("from", filters.from || "")
      setInputValue("from_domain", filters.fromDomain || "")
      setInputValue("to", filters.to || "")
      setInputValue("recipient_type", filters.recipientType || "")
      setInputValue("recipient_domain", filters.recipientDomain || "")
      setInputValue("subject", filters.subject || "")
      setInputValue("body", filters.body || "")
      setInputValue("attachment", filters.attachment || "")
      setInputValue("attachment_type", filters.attachmentType || "")
      setInputValue("attachment_extension", filters.attachmentExtension || "")
      setInputValue("min_size_mb", filters.minSizeMB || "")
      setInputValue("max_size_mb", filters.maxSizeMB || "")
      setInputValue("tag", filters.tag || "")
      setInputValue("account_id", filters.accountId || "")
      setInputValue("after_date", filters.afterDate || "")
      setInputValue("before_date", filters.beforeDate || "")
      syncAttachmentExtensionField()
      syncMailFolderHeading(filters)
      renderActivePills(filters)
      syncFilterButton(filters)
    }

    function syncMailFolderHeading(filters) {
      var heading = document.getElementById("mail-folder-name")
      if (!heading) return
      var defaultName = heading.getAttribute("data-mail-folder-default-name") || "Inbox"
      heading.textContent = filters && filters.participant ? "All mail" : defaultName
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
      var selectRoot = input && input.closest(".select-container")
      if (selectRoot) {
        var valueEl = selectRoot.querySelector("[data-tui-selectbox-placeholder]")
        if (valueEl) valueEl.textContent = valueEl.getAttribute("data-tui-selectbox-placeholder") || ""
        var selectedItems = selectRoot.querySelectorAll("[data-tui-selectbox-selected='true']")
        for (var i = 0; i < selectedItems.length; i++) selectedItems[i].setAttribute("data-tui-selectbox-selected", "false")
      }
      if (name === "attachment_type") {
        var extension = form.querySelector('input[name="attachment_extension"]')
        if (extension) extension.value = ""
        syncAttachmentExtensionField()
      }
      syncAdvancedFilterCount()
      renderActivePills()
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

    function applyFiltersToResults(filters, delay) {
      if (filterResultsTimer) {
        clearTimeout(filterResultsTimer)
        filterResultsTimer = null
      }
      var run = function () {
        filterResultsTimer = null
        if (virtualMailList) virtualMailList.applyFilters(filters).catch(function () {})
      }
      if (delay > 0) filterResultsTimer = window.setTimeout(run, delay)
      else run()
    }

    function applyCurrentFilters(options) {
      options = options || {}
      var filters = readFilters()
      committedParticipant = filters.participant || ""
      syncMailFolderHeading(filters)
      syncFilterButton(filters)
      renderActivePills()
      applyFiltersToResults(filters, options.delayResults === false ? 0 : activePillAnimationDelay)
    }

    function splitSearchTokens(text) {
      var tokens = []
      var buf = ""
      var quote = ""
      for (var i = 0; i < text.length; i++) {
        var ch = text.charAt(i)
        if (quote) {
          if (ch === quote) quote = ""
          else buf += ch
          continue
        }
        if (ch === '"' || ch === "'") {
          quote = ch
          continue
        }
        if (/\s/.test(ch)) {
          if (buf) {
            tokens.push(buf)
            buf = ""
          }
          continue
        }
        buf += ch
      }
      if (buf) tokens.push(buf)
      return tokens
    }

    function contactFilterSelect() {
      return document.querySelector("[data-mail-contact-filter-select]")
    }

    function contactFilterOption(email, label, selected) {
      var item = document.createElement("div")
      item.className = "select-item group relative flex w-full cursor-default select-none items-center rounded-sm px-2 py-1.5 text-sm font-light outline-none hover:bg-accent hover:text-accent-foreground focus-visible:bg-accent focus-visible:text-accent-foreground data-[tui-selectbox-selected=true]:bg-accent data-[tui-selectbox-selected=true]:text-accent-foreground"
      item.setAttribute("role", "option")
      item.setAttribute("tabindex", "0")
      item.setAttribute("data-tui-selectbox-value", email)
      item.setAttribute("data-tui-selectbox-selected", selected ? "true" : "false")
      item.setAttribute("data-tui-selectbox-disabled", "false")

      var text = document.createElement("span")
      text.className = "select-item-text min-w-0 truncate pr-6"
      text.textContent = label || email
      item.appendChild(text)

      var check = document.createElement("span")
      check.className = "select-check absolute right-2 flex h-3.5 w-3.5 items-center justify-center text-xs opacity-0 group-data-[tui-selectbox-selected=true]:opacity-100"
      check.textContent = "✓"
      item.appendChild(check)
      return item
    }

    function contactFilterStatus(options, text) {
      var status = document.createElement("p")
      status.className = "px-2 py-3 text-center text-xs text-muted-foreground"
      status.setAttribute("data-mail-contact-filter-status", "")
      status.textContent = text
      options.appendChild(status)
    }

    function contactFilterEmail(value) {
      value = String(value || "").trim()
      return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(value) ? value : ""
    }

    function renderContactFilterOptions(results, query) {
      var root = contactFilterSelect()
      var options = root && root.querySelector("[data-mail-contact-filter-options]")
      var hidden = root && root.querySelector('input[name="participant"]')
      if (!options || !hidden) return
      options.innerHTML = ""

      var selected = (hidden.value || "").trim()
      var seen = Object.create(null)
      var count = 0
      ;(results || []).forEach(function (contact) {
        var email = String(contact && contact.email || "").trim()
        var key = email.toLowerCase()
        if (!email || seen[key]) return
        seen[key] = true
        var name = String(contact.name || "").trim()
        var label = name && name.toLowerCase() !== key ? name + " — " + email : email
        options.appendChild(contactFilterOption(email, label, selected.toLowerCase() === key))
        count++
      })

      var customEmail = contactFilterEmail(query)
      if (customEmail && !seen[customEmail.toLowerCase()]) {
        options.appendChild(contactFilterOption(customEmail, customEmail + " — use this address", selected.toLowerCase() === customEmail.toLowerCase()))
        count++
      }
      if (!count && selected && (!query || selected.toLowerCase().indexOf(String(query).toLowerCase()) >= 0)) {
        options.appendChild(contactFilterOption(selected, selected, true))
        count++
      }
      if (!count) contactFilterStatus(options, query ? "No matching contacts." : "Type at least 2 characters to search contacts.")
    }

    function setContactFilterValue(value) {
      var root = contactFilterSelect()
      var input = root && root.querySelector('input[name="participant"]')
      var options = root && root.querySelector("[data-mail-contact-filter-options]")
      if (!input || !options) return false
      value = String(value || "").trim()
      contactFilterSearchSequence++
      options.innerHTML = ""
      if (value) options.appendChild(contactFilterOption(value, value, true))
      else contactFilterStatus(options, "Type at least 2 characters to search contacts.")
      input.value = value
      input.dispatchEvent(new Event("input", { bubbles: true }))
      return true
    }

    function scheduleContactFilterSearch(searchInput) {
      var query = String(searchInput && searchInput.value || "").trim()
      var root = searchInput && searchInput.closest("[data-mail-contact-filter-select]")
      var options = root && root.querySelector("[data-mail-contact-filter-options]")
      if (!options) return
      if (contactFilterSearchTimer) window.clearTimeout(contactFilterSearchTimer)
      var sequence = ++contactFilterSearchSequence
      if (query.length < 2) {
        renderContactFilterOptions([], "")
        return
      }
      options.innerHTML = ""
      contactFilterStatus(options, "Searching contacts…")
      contactFilterSearchTimer = window.setTimeout(function () {
        contactFilterSearchTimer = null
        fetch("/api/contacts/search?q=" + encodeURIComponent(query), { headers: { "Accept": "application/json" } })
          .then(function (response) { return response.ok ? response.json() : { results: [] } })
          .then(function (data) {
            if (sequence !== contactFilterSearchSequence) return
            renderContactFilterOptions(data.results || [], query)
          })
          .catch(function () {
            if (sequence !== contactFilterSearchSequence) return
            options.innerHTML = ""
            contactFilterStatus(options, "Contacts could not be loaded.")
          })
      }, 160)
    }

    function setInputValue(name, value) {
      var form = document.querySelector("[data-mail-advanced-filter-form]")
      var input = form && form.querySelector('[name="' + name + '"]')
      if (!input) return false
      if (input.type === "checkbox") input.checked = value === true || value === "1" || value === "true"
      else input.value = value
      var dateDisplay = form.querySelector('[data-mail-date-display="' + name + '"]')
      if (dateDisplay) dateDisplay.textContent = value || "Any date"
      return true
    }

    function setQuickBoolean(name, value) {
      if (name === "has_tags") {
        setFilterTriState("tags", value ? "yes" : "")
        return
      }
      if (name === "threads_only") {
        setFilterTriState("threads", value ? "yes" : "")
        return
      }
      setBooleanFilterControl(name, value)
    }

    function applyKeywordToken(key, value) {
      key = (key || "").toLowerCase().replace(/_/g, "-")
      value = (value || "").trim()
      if (!key) return false
      if (key === "q" || key === "query" || key === "text") {
        if (value) committedQuery = committedQuery ? (committedQuery + " " + value) : value
        return true
      }
      if (key === "contact" || key === "participant") return setContactFilterValue(value)
      if (key === "from") return setInputValue("from", value)
      if (key === "to" || key === "cc" || key === "bcc" || key === "recipient" || key === "recipients") return setInputValue("to", value)
      if (key === "subject" || key === "subj") return setInputValue("subject", value)
      if (key === "body") return setInputValue("body", value)
      if (key === "attachment" || key === "attach" || key === "filename") return setInputValue("attachment", value)
      if (key === "tag") return setInputValue("tag", value)
      if (key === "account" || key === "account-id") return setInputValue("account_id", value)
      if (key === "after") return setInputValue("after_date", value)
      if (key === "before") return setInputValue("before_date", value)
      if (key === "from-domain" || key === "fromdomain" || key === "domain") return setInputValue("from_domain", value)
      if (key === "is") {
        if (value === "unread") setFilterTriState("status", "unread")
        else if (value === "read") setFilterTriState("status", "read")
        else if (value === "starred") setQuickBoolean("starred", true)
        else return false
        return true
      }
      if (key === "has") {
        if (value === "attachment" || value === "attachments") setFilterTriState("attachments", "yes")
        else if (value === "tag" || value === "tags") setQuickBoolean("has_tags", true)
        else if (value === "thread" || value === "threads") setQuickBoolean("threads_only", true)
        else return false
        return true
      }
      return false
    }

    function commitSearchInput(input) {
      var raw = (input && input.value ? input.value : "").trim()
      if (!raw) return false
      if (searchTimer) {
        clearTimeout(searchTimer)
        searchTimer = null
      }
      var tokens = splitSearchTokens(raw)
      var plain = []
      for (var i = 0; i < tokens.length; i++) {
        var token = tokens[i]
        var idx = token.indexOf(":")
        if (idx > 0) {
          var key = token.slice(0, idx)
          var value = token.slice(idx + 1)
          if (value && applyKeywordToken(key, value)) continue
        } else {
          var lower = token.toLowerCase()
          if (lower === "unread") { setFilterTriState("status", "unread"); continue }
          if (lower === "read") { setFilterTriState("status", "read"); continue }
          if (lower === "starred") { setQuickBoolean("starred", true); continue }
          if (lower === "attachments") { setFilterTriState("attachments", "yes"); continue }
          if (lower === "threads") { setQuickBoolean("threads_only", true); continue }
        }
        plain.push(token)
      }
      if (plain.length) committedQuery = committedQuery ? (committedQuery + " " + plain.join(" ")) : plain.join(" ")
      input.value = ""
      syncAdvancedFilterCount()
      applyCurrentFilters()
      return true
    }

    function clearActiveFilter(name, options) {
      options = options || {}
      if (name === "q") {
        committedQuery = ""
        var search = document.querySelector("[data-mail-search-input]")
        if (search) {
          search.value = ""
          search.dataset.mailCommittedQuery = ""
        }
      } else if (name === "participant") {
        committedParticipant = ""
        setContactFilterValue("")
      } else if (name === "unread" || name === "read") {
        setFilterTriState("status", "")
      } else if (name === "attachments" || name === "no_attachments") {
        setFilterTriState("attachments", "")
      }
      else if (name === "starred") setQuickBoolean("starred", false)
      else if (name === "has_tags" || name === "no_tags") setFilterTriState("tags", "")
      else if (name === "threads_only" || name === "no_threads") setFilterTriState("threads", "")
      else clearAdvancedFilter(name)
      syncAdvancedFilterCount()
      applyCurrentFilters(options)
    }

    function removeActiveFilterWithAnimation(remove) {
      if (!remove) return
      var name = remove.getAttribute("data-mail-active-filter-remove")
      var pill = remove.closest(".mail-active-filter-pill")
      var canAnimate = pill && !window.matchMedia("(prefers-reduced-motion: reduce)").matches
      if (!canAnimate) {
        clearActiveFilter(name)
        return
      }
      pill.classList.add("mail-active-filter-pill-exiting")
      window.setTimeout(function () {
        clearActiveFilter(name, { delayResults: false })
      }, 145)
    }

    function initSearchStateFromInput() {
      if (virtualMailList && virtualMailList.filters) syncFilterControls(virtualMailList.filters)
      var search = document.querySelector("[data-mail-search-input]")
      committedQuery = search ? (search.dataset.mailCommittedQuery || search.value || "").trim() : ""
      committedParticipant = virtualMailList && virtualMailList.filters ? (virtualMailList.filters.participant || "").trim() : committedParticipant
      if (search) {
        search.dataset.mailCommittedQuery = committedQuery
        search.value = ""
        // Small screens swap in a shorter placeholder (see mobile-nav.js).
        search.placeholder = search.hasAttribute("data-desktop-placeholder")
          ? search.getAttribute("data-mobile-placeholder")
          : "Search, or use from: subject: body: then Enter"
      }
      renderActivePills()
      syncFilterButton(readFilters())
    }

    window.syncMailFilterControls = syncFilterControls

    document.addEventListener("submit", function (e) {
      var form = e.target && e.target.closest && e.target.closest("[data-mail-filter-form]")
      if (!form) return
      e.preventDefault()
    })

    document.addEventListener("submit", function (e) {
      var form = e.target && e.target.closest && e.target.closest("[data-mail-sort-form]")
      if (!form) return
      e.preventDefault()
      if (window.GoferSettings) {
        var mailSortBy = form.querySelector('[name="sort_by"]')
        var mailSortOrder = form.querySelector('[name="sort_order"]')
        GoferSettings.set("mail_list_sort_by", mailSortBy ? mailSortBy.value || "date" : "date")
        GoferSettings.set("mail_list_sort_order", mailSortOrder ? mailSortOrder.value || "desc" : "desc")
      }
      applyCurrentFilters({ delayResults: false })
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
        switchAdvancedFilterPanel(panelButton)
        return
      }

      var tristateOption = e.target && e.target.closest && e.target.closest("[data-mail-tristate-option]")
      if (tristateOption) {
        e.preventDefault()
        var control = tristateOption.closest("[data-mail-tristate]")
        setFilterTriState(control ? control.getAttribute("data-mail-tristate") : "", tristateOption.getAttribute("data-mail-tristate-option") || "")
        if (tristateOption.closest("[data-mail-advanced-filter-form]")) {
          syncAdvancedFilterCount()
          return
        }
        applyCurrentFilters()
        return
      }

      var filtersOpen = e.target && e.target.closest && e.target.closest("[data-mail-filter-button]")
      if (filtersOpen) {
        syncAttachmentExtensionField()
        syncAdvancedFilterCount()
        return
      }

      var filtersClose = e.target && e.target.closest && e.target.closest("[data-mail-filters-close]")
      if (filtersClose) {
        e.preventDefault()
        if (window.tui && window.tui.popover) window.tui.popover.close("mail-filters-popover")
        return
      }

      var clear = e.target && e.target.closest && e.target.closest("[data-mail-filter-clear]")
      if (!clear) return
      e.preventDefault()
      clearInputs("[data-mail-filter-form]")
      clearInputs("[data-mail-advanced-filter-form]")
      committedQuery = ""
      committedParticipant = ""
      var search = document.querySelector("[data-mail-search-input]")
      if (search) {
        search.value = ""
        search.dataset.mailCommittedQuery = ""
      }
      applyCurrentFilters()
    })

    document.addEventListener("click", function (e) {
      var overflow = e.target && e.target.closest && e.target.closest("[data-mail-active-filter-overflow]")
      if (overflow) {
        e.preventDefault()
        var summary = overflow.closest("[data-mail-active-filter-pills]")
        var panel = summary && summary.querySelector("[data-mail-active-filter-overflow-panel]")
        var open = panel && panel.classList.contains("hidden")
        closeActivePillOverflow()
        if (panel && open) {
          panel.classList.remove("hidden")
          overflow.setAttribute("aria-expanded", "true")
        }
        return
      }
      if (e.target && e.target.closest && e.target.closest("[data-mail-active-filter-overflow-panel]")) return
      closeActivePillOverflow()
    })

    document.addEventListener("click", function (e) {
      var remove = e.target && e.target.closest && e.target.closest("[data-mail-active-filter-remove]")
      if (!remove) return
      e.preventDefault()
      removeActiveFilterWithAnimation(remove)
    })

    document.addEventListener("submit", function (e) {
      var form = e.target && e.target.closest && e.target.closest("[data-mail-advanced-filter-form]")
      if (!form) return
      e.preventDefault()
      applyCurrentFilters()
      if (window.tui && window.tui.popover) window.tui.popover.close("mail-filters-popover")
    })

    document.addEventListener("click", function (e) {
      var clear = e.target && e.target.closest && e.target.closest("[data-mail-advanced-filter-clear]")
      if (!clear) return
      e.preventDefault()
      clearInputs("[data-mail-advanced-filter-form]")
      syncAdvancedFilterCount()
      renderActivePills()
    })

    document.addEventListener("keydown", function (e) {
      if (e.key === "Escape") closeActivePillOverflow()
      if (!e.target || !e.target.matches || !e.target.matches("[data-mail-search-input]")) return
      if (e.key !== "Enter") return
      e.preventDefault()
      commitSearchInput(e.target)
    })

    document.addEventListener("input", function (e) {
      if (e.target && e.target.matches && e.target.matches("[data-mail-search-input]")) {
        if (searchTimer) clearTimeout(searchTimer)
        searchTimer = setTimeout(function () {
          searchTimer = null
          applyCurrentFilters()
        }, 250)
        return
      }
      if (e.target && e.target.matches && e.target.matches("[data-tui-selectbox-search]") && e.target.closest("[data-mail-contact-filter-select]")) {
        scheduleContactFilterSearch(e.target)
      }
      if (!e.target || !e.target.closest || !e.target.closest("[data-mail-advanced-filter-form]")) return
      syncAdvancedFilterCount()
    })

    document.addEventListener("search", function (e) {
      if (!e.target || !e.target.matches || !e.target.matches("[data-mail-search-input]")) return
      if (!e.target.value) clearActiveFilter("q")
    })

    document.addEventListener("change", function (e) {
      if (!e.target || !e.target.closest || !e.target.closest("[data-mail-advanced-filter-form]")) return
      syncAttachmentExtensionField()
      syncAdvancedFilterCount()
    })

    document.addEventListener("calendar-date-selected", function (e) {
      var container = e.target && e.target.closest && e.target.closest("[data-tui-calendar-container]")
      if (!container) return
      var hidden = container.closest("[data-tui-calendar-wrapper]") && container.closest("[data-tui-calendar-wrapper]").querySelector("[data-tui-calendar-hidden-input]")
      if (!hidden || !hidden.name) return
      var display = document.querySelector('[data-mail-date-display="' + hidden.name + '"]')
      if (display) display.textContent = hidden.value || "Any date"
      syncAdvancedFilterCount()
    })

    document.body.addEventListener("htmx:afterSettle", function (evt) {
      if (!evt.target || !evt.target.querySelector) return
      if (evt.target.id === "mail-list" || evt.target.querySelector("#mail-list")) initSearchStateFromInput()
    })

    initSearchStateFromInput()
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
    card.dataset.accountDeleting = "true"
    var actions = card.querySelector("[data-account-actions]")
    if (!actions) return
    actions.innerHTML = '<button type="button" data-account-deleting-status role="status" aria-live="polite" disabled class="inline-flex items-center gap-1.5 text-xs text-amber-700 dark:text-amber-400 px-2.5 py-1.5 rounded-md border border-amber-300/40 dark:border-amber-500/30 bg-amber-100/50 dark:bg-amber-500/10 cursor-default"><svg class="size-3.5 animate-spin" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12a9 9 0 1 1-2.64-6.36"/><path d="M21 3v6h-6"/></svg>Deleting…</button>'
  }

  function setupAccountDeletionTracking() {
    function deletionToastId(accountId) {
      return "account-deletion-toast-" + accountId
    }

    function accountEmail(accountId) {
      var card = document.getElementById("account-card-" + accountId)
      return card && card.dataset.accountEmail ? card.dataset.accountEmail : "this account"
    }

    function showDeletionStarted(accountId) {
      if (typeof showGoferToast !== "function") return
      showGoferToast({
        id: deletionToastId(accountId),
        title: "Deleting account…",
        description: "Removing " + accountEmail(accountId) + ". You can leave this page; cleanup will continue in the background.",
        variant: "default",
        icon: "info",
        position: "bottom-right",
        duration: 0,
        dismissible: false,
      })
    }

    function finishAccountDeletion(accountId) {
      if (accountDeletionPolls[accountId] && accountDeletionPolls[accountId].timer) {
        clearTimeout(accountDeletionPolls[accountId].timer)
      }
      delete accountDeletionPolls[accountId]
      var email = accountEmail(accountId)
      var card = document.getElementById("account-card-" + accountId)
      if (card) {
        card.style.opacity = "0"
        card.style.transform = "translateY(-0.35rem)"
      }
      if (typeof showGoferToast === "function") {
        showGoferToast({
          id: deletionToastId(accountId),
          title: "Account deleted",
          description: email + " and its local data were removed from Gofer.",
          variant: "success",
          icon: "success",
          position: "bottom-right",
          duration: 5000,
          dismissible: true,
        })
      }
      refreshMailSidebarBody()
      setTimeout(function () {
        if (window.location.pathname === "/settings/accounts" && window.htmx && document.getElementById("main-content")) {
          htmx.ajax("GET", "/settings/accounts", { target: "#main-content", swap: "outerHTML" })
        } else if (card) {
          card.remove()
        }
      }, 220)
    }

    function pollAccountDeletion(accountId) {
      var tracker = accountDeletionPolls[accountId]
      if (!tracker) return
      fetch("/api/accounts/" + encodeURIComponent(accountId) + "/deletion-status", {
        credentials: "same-origin",
        cache: "no-store",
        headers: { Accept: "application/json" },
      }).then(function (resp) {
        if (!resp.ok) throw new Error("status request failed")
        return resp.json()
      }).then(function (result) {
        if (!accountDeletionPolls[accountId]) return
        if (result && result.status === "deleted") {
          finishAccountDeletion(accountId)
          return
        }
        tracker.failures = 0
        tracker.timer = setTimeout(function () { pollAccountDeletion(accountId) }, 1200)
      }).catch(function () {
        if (!accountDeletionPolls[accountId]) return
        tracker.failures += 1
        var delay = tracker.failures > 3 ? 5000 : 1800
        tracker.timer = setTimeout(function () { pollAccountDeletion(accountId) }, delay)
      })
    }

    function trackAccountDeletion(accountId) {
      if (!accountId || accountDeletionPolls[accountId]) return
      accountDeletionPolls[accountId] = { timer: null, failures: 0 }
      markAccountDeleting(accountId)
      showDeletionStarted(accountId)
      pollAccountDeletion(accountId)
    }

    function initialize(root) {
      var scope = root && root.querySelectorAll ? root : document
      var cards = []
      if (scope.matches && scope.matches('[data-settings-account-card][data-account-deleting="true"]')) cards.push(scope)
      scope.querySelectorAll('[data-settings-account-card][data-account-deleting="true"]').forEach(function (card) { cards.push(card) })
      cards.forEach(function (card) { trackAccountDeletion(card.dataset.accountId) })
    }

    document.body.addEventListener("htmx:beforeRequest", function (evt) {
      var path = evt.detail.pathInfo && evt.detail.pathInfo.requestPath
      if (!path || !path.match(/^\/api\/accounts\/[^/]+$/)) return
      var button = evt.detail.elt && evt.detail.elt.closest ? evt.detail.elt.closest("[data-account-delete-submit]") : null
      if (!button) return
      button.dataset.originalContent = button.innerHTML
      button.disabled = true
      button.innerHTML = '<svg class="size-4 animate-spin" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12a9 9 0 1 1-2.64-6.36"/><path d="M21 3v6h-6"/></svg>Starting…'
    })

    document.body.addEventListener("htmx:afterRequest", function (evt) {
      var path = evt.detail.pathInfo && evt.detail.pathInfo.requestPath
      var match = path && path.match(/^\/api\/accounts\/([^/]+)$/)
      if (!match || !evt.detail.xhr) return
      var accountId = decodeURIComponent(match[1])
      var button = evt.detail.elt && evt.detail.elt.closest ? evt.detail.elt.closest("[data-account-delete-submit]") : null
      if (evt.detail.xhr.status === 202) {
        if (window.tui && window.tui.dialog) window.tui.dialog.close("delete-account-" + accountId)
        trackAccountDeletion(accountId)
        refreshMailSidebarBody()
        return
      }
      if (button) {
        button.disabled = false
        if (button.dataset.originalContent) button.innerHTML = button.dataset.originalContent
      }
      if (typeof showGoferToast === "function") {
        showGoferToast({
          id: deletionToastId(accountId),
          title: "Could not delete account",
          description: "Gofer could not start account cleanup. Please try again.",
          variant: "error",
          icon: "error",
          position: "bottom-right",
          duration: 7000,
          dismissible: true,
        })
      }
    })

    document.body.addEventListener("htmx:afterSettle", function (evt) { initialize(evt.target) })
    initialize(document)
  }

  function setupAccountResultFeedback() {
    var params = new URLSearchParams(window.location.search)
    var added = params.get("account_added") === "1"
    var reconnected = params.get("account_reconnected") === "1"
    var errors = {
      oauth_invalid_state: "This authorization link is no longer valid. Start connecting the account again.",
      oauth_expired_state: "Account authorization expired. Start connecting the account again.",
      oauth_session_mismatch: "Finish authorization in the same browser session that started it.",
      oauth_no_code: "The provider did not complete authorization. Please try again.",
      oauth_exchange_failed: "Gofer could not complete authorization with the provider. Please try again.",
      oauth_userinfo_failed: "Gofer could not identify the authorized mailbox. Please try again.",
      oauth_email_mismatch: "The authorized email address differs from the mailbox you chose. Choose the matching account and try again.",
      oauth_identity_mismatch: "The authorized provider account differs from this mailbox. Reconnect using its original provider account.",
      oauth_store_failed: "Gofer could not save the mailbox authorization. Reconnect the account to try again.",
      oauth_metadata_failed: "Authorization was saved, but account details could not be updated. Reconnect the account to try again.",
      oauth_sync_failed: "Authorization was saved, but synchronization could not start. Try syncing the account again.",
      create_failed: "Gofer could not save this account. Review your existing accounts and try again.",
    }
    var errorCode = params.get("error")
    var errorMessage = Object.prototype.hasOwnProperty.call(errors, errorCode) ? errors[errorCode] : ""
    if (!added && !reconnected && !errorMessage) return

    if (typeof showGoferToast === "function") {
      showGoferToast({
        id: "account-connection-toast",
        title: errorMessage ? "Could not connect account" : (added ? "Account added" : "Account reconnected"),
        description: errorMessage || (added
          ? "The account was added successfully. Initial synchronization has started."
          : "The account was reconnected successfully. Synchronization has resumed."),
        variant: errorMessage ? "error" : "success",
        icon: errorMessage ? "error" : "success",
        position: "bottom-right",
        duration: errorMessage ? 9000 : 5000,
        dismissible: true,
      })
    }

    params.delete("account_added")
    params.delete("account_reconnected")
    if (errorMessage) params.delete("error")
    var query = params.toString()
    var cleanURL = window.location.pathname + (query ? "?" + query : "") + window.location.hash
    window.history.replaceState(window.history.state, "", cleanURL)
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

  function mailFolderSyncStateFromEvent(data, active) {
    data = data || {}
    return {
      active: !!active,
      current: data.current || 0,
      total: data.total || 0,
      folderRole: data.folder_role || "",
      folderName: data.current_folder || data.folder_name || "",
      accountName: data.account_name || data.name || "",
      accountEmail: data.account_email || data.email || "",
      provider: data.provider || "",
      mode: data.mode || "",
      refreshOnly: !!data.refresh_only,
      totalEstimated: !!data.total_estimated,
      updatedAt: Date.now(),
    }
  }

  function setupSSE() {
    if (appEventSource) return

    var source = new EventSource("/api/events")
    appEventSource = source

    source.addEventListener("calendar-sync", function (event) {
      var data
      try { data = JSON.parse(event.data) } catch (_) { return }
      handleCalendarSyncEvent(data)
    })

    source.addEventListener("calendar-changed", function () {
      scheduleCalendarCacheRefresh()
    })

    source.addEventListener("new-mail", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { return }
      if (!data || !data.folder_id) return

      refreshSidebarUnread()
      showBrowserTabNewMailNotification(data)
      withMailListForFolder(data.folder_id, data.folder_role || "", function (vml) { vml.onNewEmail() })
    })

    source.addEventListener("send-result", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { return }
      if (!data) return

      if (data.send_id) {
        fetchOutgoingSendStatus(data.send_id)
        startOutgoingSendPolling(data.send_id)
      }

      if (data.status === "sent") {
        showSendStatus("sent", "Message sent")
        handleComposeSendResult("sent", data)
      } else if (data.status === "retrying") {
        var retrySeconds = Math.max(1, Number(data.retry_in_seconds) || 60)
        var retryMinutes = Math.max(1, Math.ceil(retrySeconds / 60))
        var retryText = data.error || "The provider could not send the message yet."
        retryText += " Gofer will try again in " + retryMinutes + (retryMinutes === 1 ? " minute." : " minutes.")
        showSendStatus("retrying", retryText)
        handleComposeSendResult("retrying", data)
      } else if (data.status === "ambiguous") {
        showSendStatus("ambiguous", data.error || "Send status unknown")
        handleComposeSendResult("ambiguous", data)
      } else {
        showSendStatus("failed", data.error || "Failed to send")
        handleComposeSendResult("failed", data)
      }
    })

    source.addEventListener("mutation", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { data = null }
      if (data && data.folder_id === "scheduled") {
        refreshMailSidebarBody()
        return
      }
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

    source.addEventListener("scheduled-sync-started", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { return }
      handleMailScheduledSyncEvent("started", data)
    })

    source.addEventListener("scheduled-sync-progress", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { return }
      handleMailScheduledSyncEvent("progress", data)
    })

    source.addEventListener("scheduled-sync-complete", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { return }
      handleMailScheduledSyncEvent("complete", data)
    })

    source.addEventListener("account-sync-status", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { return }
      handleAccountSyncStatus(data)
    })

    source.addEventListener("idle-folder-status", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { return }
      if (!data || !data.account_id || !data.folder_id) return
      document.dispatchEvent(new CustomEvent("gofer:idle-folder-status", { detail: data }))
    })

    source.addEventListener("sync-started", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { return }
      if (!data || !data.folder_id) return
      updateMailSyncFolderProgress("started", data)
      syncStatesByFolder[data.folder_id] = mailFolderSyncStateFromEvent(data, true)
      withMailListForFolder(data.folder_id, data.folder_role, function (vml) {
        vml.setSyncState(true, data.current || 0, data.total || 0, data)
      }, false)
    })

    source.addEventListener("sync-progress", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { return }
      if (!data || !data.folder_id) return
      // Progress events are frequent; content changes refresh through new-mail, mutation, or completion events.
      if (data.refresh_only) {
        updateMailSyncFolderProgress("progress", data)
        syncStatesByFolder[data.folder_id] = mailFolderSyncStateFromEvent(data, true)
        withMailListForFolder(data.folder_id, data.folder_role, function (vml) {
          vml.setSyncState(true, data.current || 0, data.total || 0, data)
        }, false)
        return
      }
      updateMailSyncFolderProgress("progress", data)
      syncStatesByFolder[data.folder_id] = mailFolderSyncStateFromEvent(data, true)
      withMailListForFolder(data.folder_id, data.folder_role, function (vml) {
        var current = data.current || 0
        vml.setSyncState(current > 0, current, data.total || 0, data)
      }, false)
    })

    source.addEventListener("sync-complete", function (e) {
      var data
      try { data = JSON.parse(e.data) } catch (_) { return }
      if (!data || !data.folder_id) return
      if (data.refresh_only) {
        syncStatesByFolder[data.folder_id] = mailFolderSyncStateFromEvent(data, false)
        refreshSidebarUnread()
        withMailListForFolder(data.folder_id, data.folder_role, function (vml) {
          vml.setSyncState(false, 0, 0, data)
          scheduleSyncRefresh(vml, { noAnimation: true, rebase: mailListNearTop(vml), immediate: true })
        }, false)
        return
      }
      updateMailSyncFolderProgress("complete", data)
      syncStatesByFolder[data.folder_id] = mailFolderSyncStateFromEvent(data, false)
      refreshSidebarUnread()
      withMailListForFolder(data.folder_id, data.folder_role, function (vml) {
        vml.setSyncState(false, 0, 0, data)
        scheduleSyncRefresh(vml, { noAnimation: true, rebase: mailListNearTop(vml), immediate: true })
      }, false)
    })

    source.onerror = function () {
      source.close()
      if (appEventSource === source) appEventSource = null
      for (var folderID in syncStatesByFolder) {
        if (!Object.prototype.hasOwnProperty.call(syncStatesByFolder, folderID)) continue
        if (syncStatesByFolder[folderID]) syncStatesByFolder[folderID].active = false
      }
      applyActiveFolderSyncState()
      setTimeout(setupSSE, 5000)
    }
  }

  function currentSidebarActiveFolder() {
    if (virtualMailList && virtualMailList.folderID) return virtualMailList.folderID
    var active = document.querySelector('aside a[hx-get^="/folder/"].bg-sidebar-accent')
    if (!active) return ""
    var raw = active.getAttribute("hx-get") || ""
    try {
      var parsed = new URL(raw, window.location.origin)
      return decodeURIComponent(parsed.pathname.replace(/^\/folder\//, ""))
    } catch (_) {
      return raw.replace("/folder/", "").split("?")[0]
    }
  }

  function currentSidebarNavigationParams() {
    var tag = virtualMailList && virtualMailList.sidebarTag ? virtualMailList.sidebarTag : null
    var params = new URLSearchParams()
    if (tag && tag.label) params.set("tag", tag.label)
    if (tag && tag.label && tag.accountId) params.set("tag_account_id", tag.accountId)
    if (tag && tag.label && tag.providerId) params.set("tag_provider_id", tag.providerId)
    if (tag && tag.label && tag.providerId && tag.providerType) params.set("tag_provider_type", tag.providerType)
    return params
  }

  function sidebarRefreshURL(path) {
    var params = currentSidebarNavigationParams()
    params.set("active_folder", currentSidebarActiveFolder())
    return path + "?" + params.toString()
  }

  function refreshSidebarAccount(accountID) {
    if (!accountID) return
    var target = document.getElementById("sidebar-account-" + accountID)
    if (!target) return
    var url = sidebarRefreshURL("/api/sidebar/accounts/" + encodeURIComponent(accountID))
    if (window.htmx && typeof window.htmx.ajax === "function") {
      window.htmx.ajax("GET", url, { target: "#" + cssEscape(target.id), swap: "outerHTML" })
      return
    }
    fetch(url).then(function (resp) {
      if (!resp.ok) throw new Error("sidebar account refresh failed")
      return resp.text()
    }).then(function (html) {
      target.outerHTML = html
      updateMailSyncErrorIndicator()
    }).catch(function () {})
  }
  window.goferRefreshSidebarAccount = refreshSidebarAccount

  function refreshMailSidebarBody() {
    var target = document.getElementById("sidebar-app-body")
    if (!target || target.dataset.sidebarAppBody !== "mail") {
      refreshSidebarUnread()
      return
    }
    var url = sidebarRefreshURL("/api/sidebar/mail")
    if (window.htmx && typeof window.htmx.ajax === "function") {
      window.htmx.ajax("GET", url, { target: "#sidebar-app-body", swap: "outerHTML" })
      return
    }
    fetch(url).then(function (resp) {
      if (!resp.ok) throw new Error("mail sidebar refresh failed")
      return resp.text()
    }).then(function (html) {
      target.outerHTML = html
      refreshSidebarUnread()
      updateMailSyncErrorIndicator()
    }).catch(function () {
      refreshSidebarUnread()
    })
  }

  function setupSidebarSyncErrorTimes() {
    function hydrate(root) {
      var scope = root && root.querySelectorAll ? root : document
      var nodes = []
      if (scope.matches && scope.matches("[data-account-sync-error-at]")) nodes.push(scope)
      var descendants = scope.querySelectorAll("[data-account-sync-error-at]")
      for (var n = 0; n < descendants.length; n++) nodes.push(descendants[n])
      for (var i = 0; i < nodes.length; i++) {
        var date = parseMailSyncUTCInstant(nodes[i].getAttribute("data-account-sync-error-at"))
        if (!date) continue
        nodes[i].textContent = formatGoferDateTime(date, {
          year: "numeric",
          month: "short",
          day: "numeric",
          hour: "numeric",
          minute: "2-digit",
          timeZoneName: "short",
        })
      }
    }

    hydrate(document)
    document.addEventListener("htmx:afterSwap", function (event) { hydrate(event.target) })
    new MutationObserver(function (mutations) {
      for (var i = 0; i < mutations.length; i++) {
        for (var j = 0; j < mutations[i].addedNodes.length; j++) {
          var node = mutations[i].addedNodes[j]
          if (node && node.nodeType === 1) hydrate(node)
        }
      }
    }).observe(document.body, { childList: true, subtree: true })
  }

  function updateDesktopNotificationControls() {
    var supported = webPushSupported()
    var permission = "Notification" in window ? Notification.permission : "unsupported"
    var enabled = notificationsEnabled()
    var mode = notificationMode()
    var toggles = document.querySelectorAll("[data-desktop-notifications-switch]")
    for (var i = 0; i < toggles.length; i++) {
      toggles[i].disabled = mode === "web_push" && (!supported || permission === "denied")
    }
    var labels = document.querySelectorAll("[data-desktop-notifications-status]")
    for (var j = 0; j < labels.length; j++) {
      if (!enabled || mode === "off") labels[j].textContent = "Notifications are off."
      else if (permission === "denied") labels[j].textContent = "Notifications are blocked in this browser."
      else if (supported) labels[j].textContent = "Web Push is available in this browser."
      else if (browserNotificationsSupported()) labels[j].textContent = "Browser-tab notifications are available while Gofer is open."
      else labels[j].textContent = "No notification method is available for this browser/origin."
    }
  }

  function setupDesktopNotifications() {
    updateDesktopNotificationControls()
    if (notificationsEnabled()) {
      configureNotificationMethod({ prompt: false }).catch(function () { setNotificationActiveMethod("none") })
    } else {
      setNotificationActiveMethod("off")
    }
    document.addEventListener("change", function (e) {
      var toggle = e.target && e.target.closest ? e.target.closest("[data-desktop-notifications-switch]") : null
      var modeInput = e.target && e.target.closest ? e.target.closest('input[name="notification_mode"]') : null
      if (!toggle && !modeInput) return

      if (modeInput && window.GoferSettings) GoferSettings.set("notification_mode", modeInput.value)
      if (modeInput && modeInput.value === "off") {
        var switches = document.querySelectorAll("[data-desktop-notifications-switch]")
        for (var i = 0; i < switches.length; i++) switches[i].checked = false
        if (window.GoferSettings) GoferSettings.set("desktop_notifications", "false")
      }
      if (toggle && window.GoferSettings) GoferSettings.set("desktop_notifications", toggle.checked ? "true" : "false")

      if (!notificationsEnabled() || notificationMode() === "off") {
        disableClientNotifications().finally(function () {
          setNotificationActiveMethod("off")
          updateDesktopNotificationControls()
        })
        return
      }

      configureNotificationMethod({ prompt: true }).then(function () {
        updateDesktopNotificationControls()
      }).catch(function (err) {
        var switches = document.querySelectorAll("[data-desktop-notifications-switch]")
        for (var i = 0; i < switches.length; i++) switches[i].checked = false
        if (window.GoferSettings) GoferSettings.set("desktop_notifications", "false")
        disableClientNotifications().catch(function () {})
        setNotificationActiveMethod("none")
        showGoferToast({
          id: "desktop-notifications-toast",
          title: "Notifications unavailable",
          description: err && err.message ? err.message : "Could not enable Web Push notifications.",
          variant: "warning",
          icon: "warning",
          position: "bottom-right",
          duration: 7000,
          dismissible: true,
        })
        updateDesktopNotificationControls()
      })
    })
    document.addEventListener("htmx:afterSwap", function () {
      updateDesktopNotificationControls()
      setNotificationActiveMethod(_notificationActiveMethod)
    })
  }

  var _notificationActiveMethod = "off"

  function notificationsEnabled() {
    return window.GoferSettings && GoferSettings.get("desktop_notifications") === "true"
  }

  function notificationMode() {
    var mode = window.GoferSettings ? GoferSettings.get("notification_mode") : "auto"
    if (mode === "web_push" || mode === "browser_tab" || mode === "off") return mode
    return "auto"
  }

  function isLoopbackHost() {
    return location.hostname === "localhost" || location.hostname === "127.0.0.1" || location.hostname === "[::1]"
  }

  function browserNotificationsSupported() {
    return "Notification" in window && (window.isSecureContext || isLoopbackHost())
  }

  function setNotificationActiveMethod(method) {
    _notificationActiveMethod = method || "none"
    var label = "None"
    if (_notificationActiveMethod === "off") label = "Off"
    else if (_notificationActiveMethod === "web_push") label = "Web Push"
    else if (_notificationActiveMethod === "browser_tab") label = "Browser tab"
    var nodes = document.querySelectorAll("[data-notification-active-method]")
    for (var i = 0; i < nodes.length; i++) nodes[i].textContent = "Active method: " + label
  }

  function requestNotificationPermission(prompt) {
    if (!browserNotificationsSupported()) return Promise.resolve("unsupported")
    if (Notification.permission === "granted" || Notification.permission === "denied" || !prompt) return Promise.resolve(Notification.permission)
    try {
      if (Notification.requestPermission.length > 0) {
        return new Promise(function (resolve) { Notification.requestPermission(resolve) })
      }
      var result = Notification.requestPermission()
      if (result && typeof result.then === "function") return result
    } catch (_) {
      return Promise.resolve("denied")
    }
    return Promise.resolve(Notification.permission)
  }

  function configureNotificationMethod(opts) {
    opts = opts || {}
    var mode = notificationMode()
    if (!notificationsEnabled() || mode === "off") {
      return disableClientNotifications().then(function () { setNotificationActiveMethod("off") })
    }
    if (mode === "web_push") return activateWebPushNotifications(opts.prompt)
    if (mode === "browser_tab") return activateBrowserTabNotifications(opts.prompt)

    return activateWebPushNotifications(opts.prompt).catch(function () {
      return activateBrowserTabNotifications(opts.prompt)
    })
  }

  function activateWebPushNotifications(prompt) {
    return ensureWebPushSubscription(prompt).then(function () {
      setNotificationActiveMethod("web_push")
    })
  }

  function activateBrowserTabNotifications(prompt) {
    return requestNotificationPermission(prompt).then(function (permission) {
      if (permission !== "granted") throw new Error("Notification permission was not granted.")
      return deleteWebPushSubscription().catch(function () {})
    }).then(function () {
      setNotificationActiveMethod("browser_tab")
    })
  }

  function disableClientNotifications() {
    return deleteWebPushSubscription()
  }

  function webPushSupported() {
    return window.isSecureContext && "serviceWorker" in navigator && "PushManager" in window && "Notification" in window
  }

  function base64URLToUint8Array(value) {
    var padding = "=".repeat((4 - value.length % 4) % 4)
    var base64 = (value + padding).replace(/-/g, "+").replace(/_/g, "/")
    var raw = window.atob(base64)
    var output = new Uint8Array(raw.length)
    for (var i = 0; i < raw.length; i++) output[i] = raw.charCodeAt(i)
    return output
  }

  function ensureWebPushSubscription(prompt) {
    if (!webPushSupported()) return Promise.reject(new Error("Web Push requires HTTPS or localhost in a supported browser."))
    return requestNotificationPermission(prompt).then(function (permission) {
      if (permission !== "granted") throw new Error("Notification permission was not granted.")
      return fetch("/api/push/vapid-public-key")
    }).then(function (res) {
      if (!res.ok) throw new Error("Could not load push configuration.")
      return res.json()
    }).then(function (data) {
      if (!data.public_key) throw new Error("Web Push is not configured on this server.")
      return navigator.serviceWorker.register("/sw.js").then(function (registration) {
        return registration.pushManager.getSubscription().then(function (existing) {
          if (existing) return existing
          return registration.pushManager.subscribe({
            userVisibleOnly: true,
            applicationServerKey: base64URLToUint8Array(data.public_key),
          })
        })
      })
    }).then(function (subscription) {
      return fetch("/api/push/subscription", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(subscription),
      }).then(function (res) {
        if (!res.ok) throw new Error("Could not save push subscription.")
        return subscription
      })
    })
  }

  function deleteWebPushSubscription() {
    if (!("serviceWorker" in navigator)) return Promise.resolve()
    return navigator.serviceWorker.getRegistration("/sw.js").then(function (registration) {
      if (!registration) return null
      return registration.pushManager.getSubscription()
    }).then(function (subscription) {
      if (!subscription) return null
      var endpoint = subscription.endpoint
      return subscription.unsubscribe().catch(function () {}).then(function () {
        return fetch("/api/push/subscription", {
          method: "DELETE",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ endpoint: endpoint }),
        }).catch(function () {})
      })
    })
  }

  function showBrowserTabNewMailNotification(data) {
    if (!notificationsEnabled()) return
    if (_notificationActiveMethod !== "browser_tab") return
    if (!browserNotificationsSupported() || Notification.permission !== "granted") return
    if (!document.hidden && document.hasFocus && document.hasFocus()) return

    var role = String(data.folder_role || "")
    if (role === "sent" || role === "drafts" || role === "trash" || role === "junk" || role === "archive") return

    var unreadCount = Number(data.unread_count || 0)
    if (unreadCount <= 0) return

    var sender = String(data.from_name || data.from_email || "New mail")
    var subject = String(data.subject || "(no subject)")
    var folderName = String(data.folder_name || "Inbox")
    var title = unreadCount === 1 ? sender : unreadCount + " new messages"
    var body = unreadCount === 1 ? subject : sender + ": " + subject
    var icon = data.avatar_url || data.icon || "/assets/logo.png"

    var notification = new Notification(title, {
      body: body,
      tag: "gofer-new-mail-" + (data.account_id || "") + "-" + (data.folder_id || folderName),
      icon: icon,
      badge: "/assets/logo.png",
      data: { folderID: data.folder_id || "" },
    })
    notification.onclick = function () {
      window.focus()
      if (notification.data && notification.data.folderID) {
        window.location.href = "/folder/" + encodeURIComponent(notification.data.folderID)
      }
      notification.close()
    }
    setTimeout(function () { notification.close() }, 12000)
  }

  function openContactDetail(contactID) {
    var detail = document.getElementById("contacts-detail")
    if (!detail || !contactID || detail.getAttribute("data-contact-detail-id") !== String(contactID)) return null
    return detail
  }

  function setContactSyncButtonsBusy(contactID, busy) {
    var detail = openContactDetail(contactID)
    if (!detail) return
    var buttons = detail.querySelectorAll("[data-contact-sync-now]")
    for (var i = 0; i < buttons.length; i++) {
      buttons[i].disabled = !!busy
      var icon = buttons[i].querySelector("svg")
      if (icon) icon.classList.toggle("animate-spin", !!busy)
    }
  }

  function updateContactSyncLiveState(data) {
    var detail = openContactDetail(data && data.contact_id)
    if (!detail) return false
    var status = String((data && data.status) || "")
    var statusLabel = ""
    if (status === "pending") statusLabel = "Sync pending"
    else if (status === "running") statusLabel = "Syncing"
    else if (status === "done") statusLabel = "Synced"
    else if (status === "error") statusLabel = "Sync failed"
    if (statusLabel) {
      var statusNodes = detail.querySelectorAll("[data-contact-sync-operation-status]")
      for (var i = 0; i < statusNodes.length; i++) statusNodes[i].textContent = statusLabel
    }
    setContactSyncButtonsBusy(data.contact_id, status === "pending" || status === "running")
    return true
  }

  function refreshOpenContactDetail(contactID) {
    var detail = openContactDetail(contactID)
    if (!detail || typeof htmx === "undefined") return
    var editForm = detail.querySelector("[data-contact-edit-form]")
    var editDialogID = editForm && editForm.getAttribute("data-contact-edit-dialog")
    if (editDialogID && window.tui && window.tui.dialog && window.tui.dialog.isOpen(editDialogID)) return
    htmx.ajax("GET", "/contacts?partial=detail&contact=" + encodeURIComponent(contactID), { target: "#contacts-detail", swap: "outerHTML" })
  }

  function handleContactActivityEvent(data) {
    var eventType = String((data && data.event_type) || "")
    if (["contact_sync_queued", "contact_sync_started", "contact_synced", "contact_sync_failed"].indexOf(eventType) === -1) return
    if (!updateContactSyncLiveState(data)) return
    var retryScheduled = eventType === "contact_sync_failed" && data.status === "pending"
    var title = "Gofer Sync queued"
    var description = data.message || "The contact is waiting to be synchronized."
    var variant = "info"
    var icon = "spinner"
    var duration = 0
    var dismissible = false
    if (eventType === "contact_sync_started") {
      title = "Gofer Sync in progress"
      description = data.message || "The selected locations are being synchronized."
    } else if (eventType === "contact_synced") {
      title = "Gofer Sync complete"
      description = data.message || "The contact is synchronized across all selected locations."
      variant = "success"
      icon = "success"
      duration = 3500
      dismissible = true
    } else if (retryScheduled) {
      title = "Gofer Sync will retry"
      description = data.error || data.message || "A location could not be updated yet. Gofer will retry automatically."
      variant = "warning"
      icon = "spinner"
      duration = 7000
      dismissible = true
    } else if (eventType === "contact_sync_failed") {
      title = "Gofer Sync failed"
      description = data.error || data.message || "The selected locations could not be synchronized."
      variant = "error"
      icon = "error"
      duration = 8000
      dismissible = true
    }
    showGoferToast({
      id: "contact-sync-toast",
      title: title,
      description: description,
      variant: variant,
      icon: icon,
      position: "bottom-right",
      duration: duration,
      dismissible: dismissible,
    })
    if (eventType === "contact_synced" || (eventType === "contact_sync_failed" && !retryScheduled)) {
      window.setTimeout(function () { refreshOpenContactDetail(String(data.contact_id || "")) }, 150)
    }
  }

  function updateVisibleAvatars(hash, avatarURL) {
    patchVirtualMailListAvatarCache(hash, avatarURL)
    var nodes = document.querySelectorAll("[data-contact-avatar][data-avatar-hash]")
    for (var i = 0; i < nodes.length; i++) {
      var node = nodes[i]
      if (node.getAttribute("data-avatar-hash") !== hash) continue
      if (avatarURL) applyAvatarURLToNode(node, avatarURL)
      else clearAvatarURLFromNode(node)
    }
  }

  function clearAvatarURLFromNode(node) {
    var img = node.querySelector("img[data-avatar-image]")
    if (img) img.remove()
    showContactAvatarFallback(node)
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
      if (avatarURL) applyAvatarURLToNode(node, avatarURL)
      else clearAvatarURLFromNode(node)
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
    syncRefreshPendingVml = vml
    syncRefreshPendingOptions = mergeSyncRefreshOptions(syncRefreshPendingOptions, options || {})
    if (syncRefreshTimer) {
      if (!syncRefreshPendingOptions.immediate) return
      clearTimeout(syncRefreshTimer)
      syncRefreshTimer = null
    }
    var now = Date.now()
    var immediate = !!(syncRefreshPendingOptions && syncRefreshPendingOptions.immediate)
    var elapsed = syncRefreshLastAt ? now - syncRefreshLastAt : syncRefreshMinInterval
    var delay = immediate ? 0 : Math.max(700, syncRefreshMinInterval - elapsed)
    syncRefreshTimer = setTimeout(function () {
      syncRefreshTimer = null
      var runVml = syncRefreshPendingVml
      var runOptions = syncRefreshPendingOptions || {}
      syncRefreshPendingVml = null
      syncRefreshPendingOptions = null
      syncRefreshLastAt = Date.now()
      runVml.refreshCurrentFolder({
        noAnimation: !!runOptions.noAnimation,
        rebase: !!runOptions.rebase,
      }).catch(function () {})
    }, delay)
  }

  function mergeSyncRefreshOptions(base, next) {
    base = base || {}
    next = next || {}
    return {
      noAnimation: !!(base.noAnimation || next.noAnimation),
      rebase: !!(base.rebase || next.rebase),
      immediate: !!(base.immediate || next.immediate),
    }
  }

  function mailListNearTop(vml) {
    if (!vml || !vml.container) return true
    return vml.container.scrollTop < (vml.itemHeight || 100) * 2
  }

  function refreshActiveMailListAfterAccountSync(data) {
    if (!data || data.status !== "ok") return
    if (!virtualMailList || typeof virtualMailList.refreshCurrentFolder !== "function") return
    if (!mailListCanChangeForAccount(virtualMailList, data.account_id || "")) return
    scheduleSyncRefresh(virtualMailList, { noAnimation: true, rebase: mailListNearTop(virtualMailList) })
  }
  window.goferRefreshActiveMailListAfterAccountSync = refreshActiveMailListAfterAccountSync

  function mailListCanChangeForAccount(vml, accountID) {
    if (!vml || !accountID) return true
    var filters = vml.filters || {}
    if (filters.accountId && filters.accountId !== accountID) return false
    var tag = vml.sidebarTag || {}
    if (tag.accountId && tag.accountId !== accountID) return false
    var folderID = String(vml.folderID || "").trim()
    if (!folderID || isRoleFolderID(folderID)) return true
    if (folderID === accountID || folderID.indexOf(accountID + "_") === 0) return true
    return folderID.indexOf("acc_") !== 0
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
    return isRoleFolderID(activeFolderId) && activeFolderId === normalizedRoleFolderID(eventFolderRole)
  }

  function isRoleFolderID(folderId) {
    return folderId === "inbox" || folderId === "sent" || folderId === "drafts" || folderId === "trash" || folderId === "archive" || folderId === "spam"
  }

  function normalizedRoleFolderID(role) {
    return role === "junk" ? "spam" : role
  }

  function activeSyncStateForFolder(folderID) {
    var state = syncStatesByFolder[folderID]
    if (state && state.active) return state
    if (!isRoleFolderID(folderID)) return null
    var newest = null
    var newestAt = 0
    for (var id in syncStatesByFolder) {
      if (!Object.prototype.hasOwnProperty.call(syncStatesByFolder, id)) continue
      state = syncStatesByFolder[id]
      if (!state || !state.active) continue
      if (normalizedRoleFolderID(state.folderRole || "") !== folderID) continue
      var updatedAt = state.updatedAt || 0
      if (!newest || updatedAt >= newestAt) {
        newest = state
        newestAt = updatedAt
      }
    }
    return newest
  }

  function applyActiveFolderSyncState() {
    if (!virtualMailList) return
    var state = activeSyncStateForFolder(virtualMailList.folderID)
    if (state && state.active) {
      var current = state.current || 0
      virtualMailList.setSyncState(true, current, state.total || 0, state)
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

    virtualMailList = createMailListController(container, folderID)
    virtualMailList.hydrateFromDOM({ animate: true })
    container._virtualMailList = virtualMailList
    flushPendingSyncEvents()
    applyActiveFolderSyncState()
    bindThreadToggle(container)

    virtualMailList.replaceUrl()
  }

  function createMailListController(container, folderID) {
    var options = { folderID: folderID, viewMode: container.dataset.viewMode || "cards", navigationMode: container.dataset.navigationMode || "infinite" }
    return new VirtualMailList(container, options)
  }

  function loadInitialFolderContent(container, folderID) {
    if (!container || !container.hasAttribute("data-load-folder")) return false
    container.removeAttribute("data-load-folder")
    var initialEmailId = document.body ? (document.body.getAttribute("data-initial-email-id") || "") : ""
    if (document.body) document.body.removeAttribute("data-initial-email-id")
    var path = "/folder/" + folderID + (initialEmailId ? "/" + initialEmailId : "")
    if (window.location.pathname !== path) {
      history.replaceState({ folder: folderID, email: initialEmailId || null }, "", path + window.location.search)
    }
    if (typeof htmx !== "undefined") {
      if (initialEmailId) {
        var loadEmailAfterShell = function (evt) {
          if (!evt.target || evt.target.id !== "main-content") return
          document.body.removeEventListener("htmx:afterSettle", loadEmailAfterShell)
          preserveMailListSelectionFor = initialEmailId
          htmx.ajax("GET", mailViewRequestURL(initialEmailId), "#mail-view")
        }
        document.body.addEventListener("htmx:afterSettle", loadEmailAfterShell)
      }
      var params = new URLSearchParams(window.location.search)
      if (initialEmailId) params.set("selected", initialEmailId)
      var query = params.toString()
      var url = "/folder/" + folderID + "/full" + (query ? "?" + query : "")
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

  function setupFolderClickInterception() {
    var sidebar = document.querySelector("aside")
    if (!sidebar) return

    function clearFolderActiveState() {
      var sidebarLinks = sidebar.querySelectorAll("a[hx-get^='/folder/']")
      for (var i = 0; i < sidebarLinks.length; i++) {
        sidebarLinks[i].classList.remove("bg-sidebar-accent", "text-sidebar-primary", "font-medium")
        sidebarLinks[i].classList.add("text-sidebar-foreground", "hover:bg-sidebar-accent/60", "hover:text-sidebar-accent-foreground")
        var badge = sidebarLinks[i].querySelector("[data-folder-unread]")
        if (badge) {
          badge.classList.remove("bg-sidebar-primary/20", "text-sidebar-primary")
          badge.classList.add("bg-sidebar-accent", "text-sidebar-foreground/80")
        }
      }
      var folderRows = sidebar.querySelectorAll("[data-sidebar-folder-row]")
      for (var r = 0; r < folderRows.length; r++) {
        folderRows[r].classList.remove("bg-sidebar-accent", "text-sidebar-primary", "font-medium")
        folderRows[r].classList.add("text-sidebar-foreground", "hover:bg-sidebar-accent/60", "hover:text-sidebar-accent-foreground")
        var rowBadge = folderRows[r].querySelector("[data-folder-unread]")
        if (rowBadge) {
          rowBadge.classList.remove("bg-sidebar-primary/20", "text-sidebar-primary")
          rowBadge.classList.add("bg-sidebar-accent", "text-sidebar-foreground/80")
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

    function setActiveSidebarTagGroup(link, sidebarTag) {
      var groups = sidebar.querySelectorAll("[data-sidebar-tag-group]")
      for (var i = 0; i < groups.length; i++) {
        groups[i].removeAttribute("data-sidebar-tag-active")
      }
      if (!sidebarTag || !sidebarTag.label || !link || !link.hasAttribute("data-sidebar-tag-filter")) return
      var group = link.closest("[data-sidebar-tag-group]")
      if (!group) return
      group.setAttribute("data-sidebar-tag-active", "")
      group.setAttribute("data-sidebar-tag-collapsed", "false")
      var toggle = group.querySelector("[data-sidebar-tag-toggle]")
      if (toggle) toggle.setAttribute("aria-expanded", "true")
    }

    function readSidebarFolderCollapseState() {
      var raw = window.GoferSettings ? GoferSettings.get("sidebar_folder_collapsed") : null
      try {
        return JSON.parse(raw || "{}") || {}
      } catch (_) {
        return {}
      }
    }

    function writeSidebarFolderCollapseState(state) {
      if (window.GoferSettings) GoferSettings.set("sidebar_folder_collapsed", JSON.stringify(state))
    }

    function toggleSidebarFolderBranch(link) {
      if (!link || !link.hasAttribute("data-sidebar-folder-toggle")) return null
      var group = link.closest("[data-sidebar-folder]")
      if (!group) return null
      var groupId = group.getAttribute("data-sidebar-folder")
      if (!groupId) return group
      var collapsed = group.getAttribute("data-sidebar-folder-collapsed") !== "true"
      var state = readSidebarFolderCollapseState()
      state[groupId] = collapsed
      writeSidebarFolderCollapseState(state)
      group.setAttribute("data-sidebar-folder-collapsed", collapsed ? "true" : "false")
      link.setAttribute("aria-expanded", collapsed ? "false" : "true")
      return group
    }

    function setActiveSidebarFolderGroups(link, sidebarTag, preserveCollapsedGroup) {
      var groups = sidebar.querySelectorAll("[data-sidebar-folder]")
      for (var i = 0; i < groups.length; i++) {
        groups[i].removeAttribute("data-sidebar-folder-active")
      }
      if (sidebarTag && sidebarTag.label) return
      var group = link && link.closest ? link.closest("[data-sidebar-folder]") : null
      while (group) {
        group.setAttribute("data-sidebar-folder-active", "")
        var toggle = group.querySelector("[data-sidebar-folder-toggle]")
        if (group !== preserveCollapsedGroup) {
          group.setAttribute("data-sidebar-folder-collapsed", "false")
          if (toggle) toggle.setAttribute("aria-expanded", "true")
        }
        group = group.parentElement && group.parentElement.closest ? group.parentElement.closest("[data-sidebar-folder]") : null
      }
    }

    function sidebarFolderLinkTarget(link) {
      var raw = (link && link.getAttribute("hx-get")) || "/folder/inbox"
      try {
        var parsed = new URL(raw, window.location.origin)
        return {
          folderID: decodeURIComponent(parsed.pathname.replace(/^\/folder\//, "")) || "inbox",
          search: parsed.search || "",
        }
      } catch (_) {
        var parts = raw.split("?")
        return {
          folderID: (parts[0] || "/folder/inbox").replace("/folder/", "") || "inbox",
          search: parts.length > 1 ? "?" + parts.slice(1).join("?") : "",
        }
      }
    }

    function sidebarTagForLink(link) {
      if (!link || !link.hasAttribute("data-sidebar-tag-filter")) {
        return { label: "", accountId: "" }
      }
      var target = sidebarFolderLinkTarget(link)
      var params = new URLSearchParams(target.search || "")
      return {
        label: (link.dataset.sidebarTagLabel || params.get("tag") || "").trim(),
        accountId: (link.dataset.sidebarTagAccount || params.get("tag_account_id") || "").trim(),
        providerId: (params.get("tag_provider_id") || "").trim(),
        providerType: (params.get("tag_provider_type") || "").trim(),
      }
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

      var target = sidebarFolderLinkTarget(link)
      var folderID = target.folderID
      var sidebarTag = sidebarTagForLink(link)
      var toggledFolderGroup = toggleSidebarFolderBranch(link)
      if (virtualMailList && typeof virtualMailList.setSidebarTag === "function") {
        virtualMailList.setSidebarTag(sidebarTag)
      }
      setActiveSidebarTagGroup(link, sidebarTag)
      setActiveSidebarFolderGroups(link, sidebarTag, toggledFolderGroup)

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
      link.classList.remove("text-sidebar-foreground", "hover:bg-sidebar-accent/60", "hover:text-sidebar-accent-foreground")
      var activeRow = link.closest("[data-sidebar-folder-row]")
      if (activeRow) {
        activeRow.classList.add("bg-sidebar-accent")
        activeRow.classList.remove("hover:bg-sidebar-accent/60")
      }
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
          history.pushState({ folder: folderID, email: null }, "", "/folder/" + folderID + target.search)
          if (mainContent) showMailContentLoading(mainContent, link)
          htmx.ajax("GET", "/folder/" + folderID + "/full" + target.search, {target: "#main-content", swap: "outerHTML"})
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
      '<div class="mail-list-header px-4 py-4 space-y-3">' +
      '<div class="mail-list-title-row flex items-center justify-between">' +
      '<div class="mail-list-title flex items-center gap-2 min-w-0">' +
      '<h2 id="mail-folder-name" class="text-lg font-bold tracking-tight" style="font-family: var(--font-serif)">' + escapeHTML(folderName) + '</h2>' +
      (count ? '<span id="mail-folder-count" class="inline-flex h-5 min-w-10 items-center justify-center rounded-full bg-muted px-2 text-xs font-medium text-muted-foreground shadow-[0_1px_2px_rgba(0,0,0,0.06)]">' + escapeHTML(count) + '</span>' : '') +
      '</div>' +
      '<div class="h-8 w-8 rounded-md bg-muted/50"></div>' +
      '</div>' +
      '<div class="mail-list-search-section">' +
      '<div class="mail-list-search-primary">' +
      '<div class="mail-list-search-input-wrap relative groove rounded-lg min-w-0">' +
      '<input type="text" placeholder="Search, or use from: subject: body: then Enter" disabled class="h-9 w-full pl-3 pr-3 rounded-lg text-sm bg-background border border-border/50 opacity-60" />' +
      '</div>' +
      '<div class="mail-list-search-actions">' +
      '<button type="button" disabled class="mail-list-advanced-filter-button inline-flex h-9 shrink-0 items-center gap-1.5 rounded-lg border border-border bg-card px-2.5 text-xs font-semibold text-muted-foreground opacity-60"><span>Filters</span>' + pendingIcon("size-3.5") + '</button>' +
      '</div>' +
      '</div>' +
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
      '<div id="mail-view" class="hidden lg:flex flex-1 flex-col min-w-0 bg-background surface-desk" data-mail-reader>' +
      '<div class="flex flex-col items-center justify-center h-full text-center p-8" data-mail-view-empty>' +
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
      if (mode === "cards" && scroll && typeof window.applyMailCardFieldSettings === "function") {
        window.applyMailCardFieldSettings(scroll)
      }

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

  function setSidebarAppNavMode(mode) {
    var group = document.querySelector("[data-sidebar-app-nav]")
    if (group) {
      var buttons = group.querySelectorAll("[data-sidebar-app-button]")
      for (var i = 0; i < buttons.length; i++) {
        var isActive = buttons[i].dataset.sidebarAppButton === mode
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
    if (mode === "contacts") document.title = "Gofer - Contacts"
    else if (mode === "calendar") document.title = "Gofer - Calendar"
    else if (mode === "mail") document.title = "Gofer - Email"
  }

  function setupSidebarAppNavToggle() {
    var request = null
    var previous = null
    function restore() {
      if (!previous) return
      previous.panes.forEach(function (saved) {
        var root = document.getElementById(saved.id)
        if (root) {
          replaceAppPaneContents(root, saved, false)
          // Reattach the actual old nodes, including their virtual-list and
          // HTMX state, rather than recreating an inert copy of the last app.
          root.replaceChildren.apply(root, saved.liveChildren)
          saved.scrollPositions.forEach(function (scroll) { scroll.node.scrollTop = scroll.top; scroll.node.scrollLeft = scroll.left })
        }
      })
      setMainContentAppMode(previous.mode)
      setSidebarAppNavMode(previous.mode)
      document.getElementById("app-shell").removeAttribute("data-hx-history")
      previous = null
      request = null
      var mailScroll = document.getElementById("mail-list-scroll")
      var contactsScroll = document.getElementById("contacts-list-scroll")
      virtualMailList = mailScroll && mailScroll._virtualMailList
      virtualContactsList = contactsScroll && contactsScroll._virtualContactsList
      if (mailScroll && !virtualMailList) initVirtualScroll()
      initializeCalendarDaySelection()
    }
    // Capture runs before HTMX's click handler: the indicator never waits for
    // a request, a skeleton render, or a response to start moving.
    document.body.addEventListener("click", function (e) {
      var btn = e.target.closest && e.target.closest("[data-sidebar-app-button]")
      if (!btn || btn.disabled || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return
      var href = btn.getAttribute("href")
      var mode = btn.dataset.sidebarAppButton
      var sidebar = document.getElementById("sidebar-app-body")
      var samePath = href && new URL(href, window.location.href).pathname === window.location.pathname
      if (sidebar && sidebar.dataset.sidebarAppBody === mode && (request || samePath)) {
        e.preventDefault()
        e.stopPropagation()
        return
      }
      setSidebarAppNavMode(mode)

      if (!href) return
      if (btn.hasAttribute("hx-get") && typeof htmx !== "undefined") return
      e.preventDefault()
      showAppSwitchPending(mode)
      window.location.href = href
    }, true)
    document.body.addEventListener("htmx:beforeRequest", function (event) {
      var detail = event.detail
      var trigger = detail && detail.elt
      if (!trigger || !trigger.hasAttribute("data-sidebar-app-button")) {
        var origin = document.getElementById("sidebar-app-body")
        if (detail && detail.xhr && detail.target && origin &&
            /^(mail-list|mail-view|main-content|sidebar-app-body)$/.test(detail.target.id)) {
          detail.xhr.goferAppPaneMode = origin.dataset.sidebarAppBody
        }
        return
      }
      if (_calendarContentRequest) _calendarContentRequest.abort()
      if (_calendarCacheRequest) {
        var cache = _calendarCacheRequest
        _calendarCacheRequest = null
        cache.abort()
      }
      if (_calendarSyncRequest) {
        var sync = _calendarSyncRequest
        handleCalendarSyncAbort({ detail: { xhr: sync } })
        sync.abort()
      }
      _calendarNavigationRequest = null
      finishCalendarNavigationTransition()
      if (!previous) {
        var sidebar = document.getElementById("sidebar-app-body")
        previous = { mode: sidebar ? sidebar.dataset.sidebarAppBody : "mail", panes: [] }
        ;["sidebar-app-body", "sidebar-sync-controls", "mail-list", "mail-view"].forEach(function (id) {
          var root = document.getElementById(id)
          if (root) {
            var saved = root.cloneNode(false)
            saved.liveChildren = Array.from(root.childNodes)
            saved.scrollPositions = Array.from(root.querySelectorAll("nav, [id$='-scroll'], [data-calendar-week-scroll], #calendar-agenda-list")).map(function (node) {
              return { node: node, top: node.scrollTop, left: node.scrollLeft }
            })
            previous.panes.push(saved)
          }
        })
      }
      request = detail.xhr
      request.goferAppSwitchMode = trigger.dataset.sidebarAppButton
      setSidebarAppNavMode(request.goferAppSwitchMode)
      document.getElementById("app-shell").setAttribute("data-hx-history", "false")
      showAppSwitchPending(request.goferAppSwitchMode)
    })
    document.body.addEventListener("htmx:beforeSwap", function (event) {
      var xhr = event.detail && event.detail.xhr
      if (xhr && xhr.goferAppSwitchMode && xhr !== request) event.detail.shouldSwap = false
      var sidebar = document.getElementById("sidebar-app-body")
      if (xhr && xhr.goferAppPaneMode && sidebar && xhr.goferAppPaneMode !== sidebar.dataset.sidebarAppBody) event.detail.shouldSwap = false
      if (request && xhr !== request && event.detail.target &&
          /^(mail-list|mail-view|main-content|sidebar-app-body)$/.test(event.detail.target.id)) event.detail.shouldSwap = false
    })
    document.body.addEventListener("htmx:afterSwap", function (event) {
      if (!event.detail || event.detail.xhr !== request) return
      previous = null
      request = null
      document.getElementById("app-shell").removeAttribute("data-hx-history")
    })
    function completed(event) {
      if (!event.detail || event.detail.xhr !== request || event.detail.successful) return
      var mode = request.goferAppSwitchMode
      restore()
      if (event.type !== "htmx:sendAbort") showGoferToast({ title: "Could not load " + mode, description: "Please try again.", variant: "error", icon: "error" })
    }
    document.body.addEventListener("htmx:afterRequest", completed)
    document.body.addEventListener("htmx:sendAbort", completed)
  }

  function mailMainContentClass() {
    var layout = window.GoferSettings ? GoferSettings.get("mail_pane_layout") : ""
    return layout === "stacked" ? "flex flex-1 min-w-0 flex-col" : "flex flex-1 min-w-0"
  }

  function setMainContentAppMode(mode) {
    var main = document.getElementById("main-content")
    if (!main) return
    if (mode === "contacts" || mode === "calendar") {
      main.className = "flex flex-1 min-w-0 bg-background"
      main.removeAttribute("data-mail-pane-layout")
      return
    }
    main.className = mailMainContentClass()
    main.setAttribute("data-mail-pane-layout", window.GoferSettings && GoferSettings.get("mail_pane_layout") === "stacked" ? "stacked" : "side")
  }

  function sidebarPendingHTML(mode) {
    var rows = mode === "contacts" ? 5 : 7
    var html = '<div class="px-4 pb-4">'
    if (mode === "contacts") {
      html += '<div class="inline-flex w-full items-stretch rounded-lg shadow-sm">'
      html += '<div class="btn-skeuo flex h-10 min-w-0 flex-1 items-center justify-center gap-2 rounded-l-lg rounded-r-none text-sm font-semibold text-sidebar-primary-foreground opacity-75">'
      html += pendingSidebarIcon("user-plus", "size-4") + '<span>New contact</span></div>'
      html += '<div class="btn-skeuo inline-flex h-10 w-10 shrink-0 items-center justify-center rounded-l-none rounded-r-lg border-l border-sidebar-border/70 text-sidebar-primary-foreground opacity-75">'
      html += pendingSidebarIcon("ellipsis-vertical", "size-4") + '</div></div>'
    } else {
      html += '<div class="btn-skeuo flex h-10 w-full items-center justify-center gap-2 rounded-lg text-sm font-semibold text-sidebar-primary-foreground opacity-75">'
      html += pendingSidebarIcon("pen", "size-4") + '<span>Compose</span></div>'
    }
    html += '</div>'
    html += '<hr class="divider-etched mx-4"><nav class="flex-1 overflow-y-auto px-3 pt-2 pb-3">'
    for (var i = 0; i < rows; i++) {
      html += '<div class="mb-1 flex items-center gap-2.5 rounded-md px-2.5 py-1.5"><span class="size-5 rounded bg-sidebar-accent"></span><span class="h-3 flex-1 rounded bg-sidebar-accent"></span></div>'
    }
    html += '</nav>'
    return html
  }

  function pendingSidebarIcon(name, className) {
    var paths = {
      "ellipsis-vertical": '<circle cx="12" cy="12" r="1"/><circle cx="12" cy="5" r="1"/><circle cx="12" cy="19" r="1"/>',
      pen: '<path d="M21.174 6.812a1 1 0 0 0-3.986-3.987L3.842 16.174a2 2 0 0 0-.5.83l-1.321 4.352a.5.5 0 0 0 .623.622l4.353-1.32a2 2 0 0 0 .83-.497z"/>',
      "user-plus": '<path d="M14 19a6 6 0 0 0-12 0"/><circle cx="8" cy="9" r="4"/><path d="M19 8v6"/><path d="M22 11h-6"/>',
    }
    return '<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="' + (className || "size-4") + '" aria-hidden="true">' + (paths[name] || "") + '</svg>'
  }

  function normalizedListViewMode(value) {
    return value === "table" ? "table" : "cards"
  }

  function appSwitchListViewMode(mode) {
    var key = mode === "contacts" ? "contacts_list_view" : "mail_list_view"
    var currentScroll = mode === "contacts" ? document.getElementById("contacts-list-scroll") : document.getElementById("mail-list-scroll")
    var currentShell = mode === "contacts" ? document.querySelector("[data-contact-list-shell]") : document.querySelector("[data-mail-list-view]")
    var saved = window.GoferSettings ? GoferSettings.get(key) : ""
    return normalizedListViewMode(
      saved ||
      (currentScroll && currentScroll.dataset.viewMode) ||
      (currentShell && (currentShell.dataset.viewMode || currentShell.dataset.mailListView)) ||
      "cards"
    )
  }

  function pendingRowCount(list, mode, viewMode) {
    var height = list && list.getBoundingClientRect ? list.getBoundingClientRect().height : 0
    var reserved = mode === "contacts" ? 148 : 160
    var itemHeight = viewMode === "table" ? 44 : 100
    var count = Math.ceil(Math.max(0, height - reserved) / itemHeight) + 2
    if (!isFinite(count) || count <= 0) count = viewMode === "table" ? 14 : 8
    return Math.max(viewMode === "table" ? 10 : 6, Math.min(viewMode === "table" ? 28 : 12, count))
  }

  function pendingBar(width, className) {
    return '<span class="' + (className || "block h-3") + ' rounded bg-muted animate-pulse" style="width:' + width + '"></span>'
  }

  function pendingIcon(className) {
    return '<span class="inline-block ' + (className || "size-3.5") + ' rounded-sm bg-current opacity-30"></span>'
  }

  function pendingFilterButton(label, className) {
    return '<button type="button" disabled aria-label="' + label + '" class="' + (className || "relative inline-flex h-7 w-7 items-center justify-center rounded-md text-muted-foreground opacity-75") + '">' + pendingIcon("size-3.5") + '</button>'
  }

  function pendingViewToggleHTML(viewMode) {
    var cardsClass = viewMode === "cards" ? "text-foreground" : "text-muted-foreground"
    var tableClass = viewMode === "table" ? "text-foreground" : "text-muted-foreground"
    var indicator = viewMode === "table" ? "transform:translateX(100%)" : "transform:translateX(0)"
    return '<div class="relative inline-flex h-7 shrink-0 rounded-lg border border-border bg-background p-0.5 gap-0.5" aria-hidden="true">' +
      '<div class="absolute top-0.5 bottom-0.5 left-0.5 w-[calc(50%-2px)] rounded-md border border-border bg-card shadow-sm transition-transform duration-200 ease-out" style="' + indicator + '"></div>' +
      '<button type="button" disabled class="relative z-10 inline-flex h-6 items-center gap-1 px-2 rounded-md text-xs font-medium ' + cardsClass + '">' + pendingIcon("size-3.5") + 'Cards</button>' +
      '<button type="button" disabled class="relative z-10 inline-flex h-6 items-center gap-1 px-2 rounded-md text-xs font-medium ' + tableClass + '">' + pendingIcon("size-3.5") + 'Table</button>' +
    '</div>'
  }

  function mailPendingHeaderHTML() {
    return '<div class="mail-list-header px-4 py-4 space-y-3">' +
      '<div class="mail-list-title-row flex items-center justify-between"><div class="mail-list-title flex items-center gap-2 min-w-0"><h2 id="mail-folder-name" class="text-lg font-bold tracking-tight" style="font-family: var(--font-serif)">Inbox</h2><span id="mail-folder-count" class="inline-flex h-5 min-w-10 items-center justify-center rounded-full bg-muted px-2 text-xs font-medium text-muted-foreground shadow-[0_1px_2px_rgba(0,0,0,0.06)] animate-pulse"></span></div></div>' +
      '<div class="mail-list-search-section"><div class="mail-list-search-primary"><div class="mail-list-search-input-wrap relative groove rounded-lg min-w-0">' +
          '<span class="absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 rounded-sm bg-muted-foreground/30"></span>' +
          '<input type="text" disabled placeholder="Search, or use from: subject: body: then Enter" class="h-9 w-full pl-8 pr-3 rounded-lg text-sm bg-background border border-border/50 outline-none opacity-70"/>' +
        '</div><div class="mail-list-search-actions">' +
          '<button type="button" disabled class="mail-list-advanced-filter-button inline-flex h-9 shrink-0 items-center gap-1.5 rounded-lg border border-border bg-card px-2.5 text-xs font-semibold text-foreground opacity-70"><span>Filters</span>' + pendingIcon("size-3.5") + '</button>' +
        '</div></div></div>' +
    '</div>'
  }

  function contactsPendingHeaderHTML() {
    return '<div class="px-4 py-4 space-y-3">' +
      '<div class="flex items-center justify-between"><div class="flex items-center gap-2"><h2 class="text-lg font-bold tracking-tight" style="font-family: var(--font-serif)">Contacts</h2><span id="contacts-count" class="inline-flex h-5 min-w-10 items-center justify-center rounded-md bg-muted px-2 text-xs font-medium text-muted-foreground shadow-[0_1px_2px_rgba(0,0,0,0.06)] animate-pulse"></span></div></div>' +
      '<div class="flex items-center gap-2"><div class="relative groove rounded-lg flex-1 min-w-0">' +
        '<span class="absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 rounded-sm bg-muted-foreground/30"></span>' +
        '<input type="search" disabled placeholder="Search contacts" class="h-9 w-full rounded-lg border border-border/50 bg-background pl-8 pr-3 text-sm text-foreground placeholder:text-muted-foreground opacity-70"/>' +
      '</div></div>' +
    '</div>'
  }

  function mailPendingToolbarHTML(viewMode) {
    var sortBy = window.GoferSettings ? GoferSettings.get("mail_list_sort_by") : "date"
    var sortLabel = sortBy === "sender" ? "Sender" : (sortBy === "subject" ? "Subject" : "Date")
    return '<div class="mail-list-toolbar flex items-center gap-1 px-4 py-1.5">' +
      pendingViewToggleHTML(viewMode) +
      '<div class="mail-list-toolbar-spacer flex-1"></div>' +
      '<button type="button" disabled class="inline-flex h-7 items-center gap-1.5 rounded-md px-2 text-xs font-medium text-muted-foreground opacity-60">' + pendingIcon("size-3.5") + '<span class="hidden sm:inline">' + sortLabel + '</span></button>' +
      '<button type="button" disabled class="h-7 w-7 rounded-md text-muted-foreground opacity-50">' + pendingIcon("mx-auto size-3.5") + '</button>' +
      '<button type="button" disabled class="h-7 w-7 rounded-md text-muted-foreground opacity-50">' + pendingIcon("mx-auto size-3.5") + '</button>' +
    '</div>'
  }

  function contactsPendingToolbarHTML(viewMode) {
    var sortBy = window.GoferSettings ? GoferSettings.get("contacts_list_sort_by") : "updated"
    var sortLabel = sortBy === "name" ? "Name" : (sortBy === "last_interaction" ? "Last interaction" : "Recently updated")
    return '<div class="flex items-center gap-1 px-4 py-1.5">' +
      pendingFilterButton("Filter contacts") +
      '<div class="flex-1"></div>' +
      '<button type="button" disabled class="inline-flex h-7 items-center gap-1.5 rounded-md px-2 text-xs font-medium text-muted-foreground opacity-60">' + pendingIcon("size-3.5") + '<span class="hidden sm:inline">' + sortLabel + '</span></button>' +
      pendingViewToggleHTML(viewMode) +
    '</div>'
  }

  function mailPendingTableHeaderHTML() {
    return '<div class="mail-list-table-header mail-list-table-grid grid items-center gap-3 px-3 py-1.5 text-[10px] font-semibold uppercase tracking-wider text-muted-foreground bg-card/95 border-b border-border/70 sticky top-0 z-20 backdrop-blur-sm">' +
      '<div class="mail-list-table-heading flex items-center justify-center" data-mail-table-column="0" data-mail-table-column-id="accountMarker" data-mail-table-cell="accountMarker" title="Account Marker"><span class="account-color-marker size-2.5 bg-muted"></span><span class="mail-list-column-separator"></span></div>' +
      '<div class="mail-list-table-heading text-center" data-mail-table-column="1" data-mail-table-column-id="starred" data-mail-table-cell="starred" title="Starred">' + pendingIcon("mx-auto size-3") + '<span class="mail-list-column-separator"></span></div>' +
      '<div class="mail-list-table-heading text-center" data-mail-table-column="2" data-mail-table-column-id="attachment" data-mail-table-cell="attachment" title="Attachment">' + pendingIcon("mx-auto size-3") + '<span class="mail-list-column-separator"></span></div>' +
      '<div class="mail-list-table-heading flex items-center justify-center" data-mail-table-column="3" data-mail-table-column-id="thread" data-mail-table-cell="thread" title="Thread">' + pendingIcon("size-3") + '<span class="mail-list-column-separator"></span></div>' +
      '<div class="mail-list-table-heading pl-1" data-mail-table-column="4" data-mail-table-column-id="from" data-mail-table-cell="from">From<span class="mail-list-column-resize" data-mail-table-resize="4"></span></div>' +
      '<div class="mail-list-table-heading" data-mail-table-column="5" data-mail-table-column-id="to" data-mail-table-cell="to">To<span class="mail-list-column-resize" data-mail-table-resize="5"></span></div>' +
      '<div class="mail-list-table-heading" data-mail-table-column="6" data-mail-table-column-id="subject" data-mail-table-cell="subject">Subject<span class="mail-list-column-resize" data-mail-table-resize="6"></span></div>' +
      '<div class="mail-list-table-heading min-w-12 text-right" data-mail-table-column="7" data-mail-table-column-id="date" data-mail-table-cell="date">Date</div>' +
    '</div>'
  }

  function contactsPendingTableHeaderHTML() {
    return '<div class="mail-list-table-header mail-list-table-grid grid items-center gap-3 px-3 py-1.5 text-[10px] font-semibold uppercase tracking-wider text-muted-foreground bg-card/95 border-b border-border/70 sticky top-0 z-20 backdrop-blur-sm" style="--mail-list-table-columns:minmax(10rem,1.4fr) minmax(8rem,0.9fr) minmax(3.5rem,auto)">' +
      '<div class="mail-list-table-heading">Name</div><div class="mail-list-table-heading">Origin</div><div class="mail-list-table-heading min-w-12 text-right">Msgs</div>' +
    '</div>'
  }

  function mailCardPendingRow(i) {
    var senderWidths = ["62%", "48%", "70%", "54%"]
    var subjectWidths = ["84%", "66%", "76%", "58%"]
    var previewWidths = ["92%", "80%", "68%", "86%"]
    return '<div class="mail-list-item" aria-hidden="true"><div class="mail-list-card h-full px-3.5 py-2.5 rounded-lg envelope" data-mail-card-layout-scope>' +
      '<div class="mail-list-card-zone mail-list-card-zone-rail-top" data-mail-card-zone="railTop"><span data-mail-card-field="avatar" class="size-6 rounded-full bg-muted animate-pulse"></span></div>' +
      '<div class="mail-list-card-zone mail-list-card-zone-rail-middle" data-mail-card-zone="railMiddle"><span data-mail-card-field="accountMarker" class="account-color-marker size-2.5 shrink-0 bg-muted animate-pulse"></span></div>' +
      '<div class="mail-list-card-zone mail-list-card-zone-rail-bottom" data-mail-card-zone="railBottom">' + (i % 4 === 0 ? '<span data-mail-card-field="thread">' + pendingBar("60%", "block h-3") + '</span>' : '<span data-mail-card-field="thread" class="mail-list-card-empty-icon-slot"></span>') + '</div>' +
      '<div class="mail-list-card-zone mail-list-card-zone-header" data-mail-card-zone="header"><span data-mail-card-field="from">' + pendingBar(senderWidths[i % senderWidths.length], "block h-3.5") + '</span><span data-mail-card-field="date">' + pendingBar("3.5rem", "block h-3") + '</span><span data-mail-card-field="account" class="h-4 w-20 rounded-full border border-border bg-background animate-pulse"></span></div>' +
      '<div class="mail-list-card-zone mail-list-card-zone-meta" data-mail-card-zone="meta">' + (i % 3 === 0 ? '<span data-mail-card-field="attachment">' + pendingIcon("size-3") + '</span><span data-mail-card-field="unread" class="inline-flex size-4 shrink-0 items-center justify-center"><span class="size-2 rounded-full bg-primary/35"></span></span>' : "") + '</div>' +
      '<div class="mail-list-card-zone mail-list-card-zone-body" data-mail-card-zone="body"><span data-mail-card-field="subject">' + pendingBar(subjectWidths[i % subjectWidths.length], "block h-3.5") + '</span><span data-mail-card-field="to">' + pendingBar("46%", "block h-3") + '</span></div>' +
      '<div class="mail-list-card-zone mail-list-card-zone-footer" data-mail-card-zone="footer"><span data-mail-card-field="preview" class="min-w-0 flex-1">' + pendingBar(previewWidths[i % previewWidths.length], "block h-3 w-full") + '</span>' + (i % 5 === 0 ? '<span data-mail-card-field="labels" class="h-4 w-10 rounded bg-muted animate-pulse"></span>' : "") + '</div>' +
      '<div class="mail-list-card-zone mail-list-card-zone-status" data-mail-card-zone="status"></div><div class="mail-list-card-zone mail-list-card-zone-corner" data-mail-card-zone="corner"><span data-mail-card-field="starred" class="h-3 w-3 rounded bg-muted animate-pulse"></span></div><div class="hidden" data-mail-card-zone="hidden"></div></div></div>'
  }

  function contactCardPendingRow(i) {
    var nameWidths = ["58%", "72%", "46%", "66%"]
    var emailWidths = ["78%", "64%", "86%", "54%"]
    var chipWidths = ["5rem", "7rem", "4rem", "6rem"]
    return '<div class="mail-list-item" aria-hidden="true"><div class="contact-list-item mail-list-card h-full px-3.5 py-2.5 rounded-lg envelope">' +
      '<div class="mail-list-card-zone mail-list-card-zone-rail-top" data-mail-card-zone="railTop"><span data-mail-card-field="avatar" class="size-6 rounded-full bg-muted animate-pulse"></span></div>' +
      '<div class="mail-list-card-zone mail-list-card-zone-header" data-mail-card-zone="header"><span data-mail-card-field="from">' + pendingBar(nameWidths[i % nameWidths.length], "block h-3.5") + '</span>' + (i % 3 === 0 ? '<span data-mail-card-field="date">' + pendingBar("3rem", "block h-3") + '</span>' : "") + '</div>' +
      '<div class="mail-list-card-zone mail-list-card-zone-body" data-mail-card-zone="body"><span data-mail-card-field="subject">' + pendingBar(emailWidths[i % emailWidths.length], "block h-3.5") + '</span></div>' +
      '<div class="mail-list-card-zone mail-list-card-zone-footer" data-mail-card-zone="footer"><span data-mail-card-field="preview" class="flex min-w-0 flex-nowrap items-center gap-1 overflow-hidden"><span class="h-4 rounded-full bg-muted animate-pulse" style="width:' + chipWidths[i % chipWidths.length] + '"></span>' + (i % 4 === 0 ? '<span class="h-4 w-16 rounded-full bg-muted animate-pulse"></span>' : "") + '</span></div>' +
      '</div></div>'
  }

  function mailTablePendingRow(i) {
    var fromWidths = ["70%", "55%", "82%", "64%"]
    var toWidths = ["60%", "76%", "48%", "68%"]
    var subjectWidths = ["88%", "72%", "95%", "58%"]
    return '<div class="mail-list-item mail-list-table-row" aria-hidden="true"><a tabindex="-1" class="mail-list-table-grid grid items-center gap-3 px-3 py-1.5 rounded-md cursor-default transition-all duration-150 group text-xs envelope">' +
      '<div class="flex items-center justify-center" data-mail-table-cell="accountMarker"><span class="account-color-marker size-2.5 shrink-0 bg-muted"></span></div>' +
      '<div class="flex items-center justify-center text-muted-foreground" data-mail-table-cell="starred">' + (i % 4 === 0 ? pendingIcon("size-3") : "") + '</div>' +
      '<div class="flex items-center justify-center text-muted-foreground" data-mail-table-cell="attachment">' + (i % 5 === 0 ? pendingIcon("size-3") : "") + '</div>' +
      '<div class="flex items-center justify-start min-w-0" data-mail-table-cell="thread">' + (i % 3 === 0 ? pendingBar("1.75rem", "block h-3") : "") + '</div>' +
      '<div class="flex items-center min-w-0" data-mail-table-cell="from">' + pendingBar(fromWidths[i % fromWidths.length], "block h-3") + '</div>' +
      '<div class="flex items-center min-w-0" data-mail-table-cell="to">' + pendingBar(toWidths[i % toWidths.length], "block h-3") + '</div>' +
      '<div class="flex items-center gap-2 min-w-0" data-mail-table-cell="subject">' + pendingBar(subjectWidths[i % subjectWidths.length], "block h-3") + (i % 6 === 0 ? '<span class="hidden xl:inline h-4 w-10 rounded bg-muted animate-pulse"></span>' : "") + '</div>' +
      '<div class="flex items-center justify-end shrink-0 text-muted-foreground tabular-nums" data-mail-table-cell="date">' + pendingBar("3rem", "block h-3") + '</div>' +
    '</a></div>'
  }

  function contactTablePendingRow(i) {
    var nameWidths = ["62%", "48%", "74%", "56%"]
    var emailWidths = ["86%", "68%", "76%", "58%"]
    var originWidths = ["6rem", "8rem", "5rem", "7rem"]
    return '<div class="mail-list-item mail-list-table-row" aria-hidden="true"><a tabindex="-1" class="contact-list-item mail-list-table-grid grid items-center gap-3 px-3.5 py-1.5 rounded-md cursor-default transition-all duration-150 group text-xs envelope" style="--mail-list-table-columns:minmax(10rem,1.4fr) minmax(8rem,0.9fr) minmax(3.5rem,auto)">' +
      '<div class="flex min-w-0 items-center gap-3"><div class="w-7 flex shrink-0 items-center justify-center"><span class="size-6 rounded-full bg-muted animate-pulse"></span></div><div class="min-w-0 flex-1">' + pendingBar(nameWidths[i % nameWidths.length], "block h-3") + '<div class="mt-1.5">' + pendingBar(emailWidths[i % emailWidths.length], "block h-3") + '</div></div></div>' +
      '<div class="truncate text-xs text-muted-foreground">' + pendingBar(originWidths[i % originWidths.length], "block h-3") + '</div>' +
      '<div class="flex justify-end text-right text-xs tabular-nums text-muted-foreground">' + pendingBar(i % 2 === 0 ? "2rem" : "1.25rem", "block h-3") + '</div>' +
    '</a></div>'
  }

  function listRowsPendingHTML(mode, viewMode, rowCount) {
    var html = ""
    if (viewMode === "table") html += mode === "contacts" ? contactsPendingTableHeaderHTML() : mailPendingTableHeaderHTML()
    for (var i = 0; i < rowCount; i++) {
      if (mode === "contacts") html += viewMode === "table" ? contactTablePendingRow(i) : contactCardPendingRow(i)
      else html += viewMode === "table" ? mailTablePendingRow(i) : mailCardPendingRow(i)
    }
    return html
  }

  function listPendingHTML(mode, viewMode, rowCount) {
    var scrollID = mode === "contacts" ? "contacts-list-scroll" : "mail-list-scroll"
    return (mode === "contacts" ? contactsPendingHeaderHTML() + contactsPendingToolbarHTML(viewMode) : mailPendingHeaderHTML() + mailPendingToolbarHTML(viewMode)) +
      '<hr class="divider-etched">' +
      '<div id="' + scrollID + '" class="flex-1 overflow-y-auto px-2 py-2" data-view-mode="' + viewMode + '" aria-busy="true">' +
        listRowsPendingHTML(mode, viewMode, rowCount) +
      '</div>'
  }

  function readPanePendingHTML(mode) {
    var label = mode === "contacts" ? "Loading contacts..." : "Loading message..."
    return '<div class="flex flex-col h-full p-2"><div class="surface-paper rounded-md flex flex-col h-full overflow-hidden"><div class="flex items-center justify-between px-6 py-2.5"><div class="flex items-center gap-1"><div class="size-8 rounded-md bg-ink/[0.03] border border-ink/6"></div><div class="size-8 rounded-md bg-ink/[0.03] border border-ink/6"></div></div><div class="h-4 w-20 rounded bg-ink/5 animate-pulse"></div></div><div class="h-px bg-gradient-to-r from-transparent via-amber-900/10 to-transparent"></div><div class="flex-1 overflow-y-auto"><div class="mx-auto px-8 py-6"><div class="flex items-center gap-2 text-sm text-ink/45"><div class="size-4 border-2 border-ink/15 border-t-ink/45 rounded-full animate-spin"></div><span>' + label + '</span></div><div class="space-y-3 mt-5"><div class="h-4 w-full rounded bg-ink/5 animate-pulse"></div><div class="h-4 w-11/12 rounded bg-ink/5 animate-pulse"></div><div class="h-4 w-4/5 rounded bg-ink/5 animate-pulse"></div></div></div></div></div></div>'
  }

  function showAppSwitchPending(mode) {
    if (mode !== "contacts" && mode !== "mail" && mode !== "calendar") return
    setMainContentAppMode(mode)
    virtualMailList = null
    virtualContactsList = null
    if (mode === "calendar") {
      var template = document.getElementById("calendar-loading-month")
      if (!template) return
      var content = template.content.cloneNode(true)
      ;["sidebar-app-body", "sidebar-sync-controls", "mail-list", "mail-view"].forEach(function (id) {
        var root = document.getElementById(id)
        var loading = content.querySelector("#" + id)
        if (root && loading) replaceAppPaneContents(root, loading, id === "mail-list")
      })
      initializeCalendarViewport()
      return
    }
    var viewMode = appSwitchListViewMode(mode)
    var sidebarBody = document.getElementById("sidebar-app-body")
    if (sidebarBody) {
      sidebarBody.removeAttribute("inert")
      sidebarBody.removeAttribute("aria-busy")
      sidebarBody.dataset.sidebarAppBody = mode
      sidebarBody.innerHTML = sidebarPendingHTML(mode)
    }
    var list = document.getElementById("mail-list")
    if (list) {
      list.removeAttribute("inert")
      list.removeAttribute("aria-busy")
      var rows = pendingRowCount(list, mode, viewMode)
      list.className = "w-full lg:flex flex-col border-r border-border bg-card h-full overflow-hidden"
      list.dataset.viewMode = viewMode
      if (mode === "contacts") {
        list.setAttribute("data-contact-list-shell", "")
        list.removeAttribute("data-mail-list-view")
        list.removeAttribute("data-mail-navigation-mode")
      } else {
        list.removeAttribute("data-contact-list-shell")
        list.dataset.mailListView = viewMode
        list.dataset.mailNavigationMode = window.GoferSettings ? (GoferSettings.get("mail_list_navigation") || "infinite") : "infinite"
      }
      list.innerHTML = listPendingHTML(mode, viewMode, rows)
      if (mode === "mail" && viewMode === "table" && typeof window.applyMailTableColumnSettings === "function") {
        window.applyMailTableColumnSettings(list.querySelector("#mail-list-scroll"))
      }
      if (mode === "mail" && viewMode === "cards" && typeof window.applyMailCardFieldSettings === "function") {
        window.applyMailCardFieldSettings(list.querySelector("#mail-list-scroll"))
      }
    }
    var pane = document.getElementById("mail-view")
    if (pane) {
      pane.removeAttribute("inert")
      pane.removeAttribute("aria-busy")
      pane.removeAttribute("data-calendar-agenda")
      pane.className = mode === "contacts" ? "hidden flex-1 min-w-0 bg-background surface-desk xl:flex xl:flex-col" : "hidden lg:flex flex-1 flex-col min-w-0 bg-background surface-desk"
      pane.innerHTML = readPanePendingHTML(mode)
    }
  }

  document.body.addEventListener("htmx:afterSwap", function () {
    var shell = document.getElementById("app-shell")
    if (!shell) return
    if (!shell.querySelector("#mail-list-scroll")) virtualMailList = null
    if (!shell.querySelector("#contacts-list-scroll")) virtualContactsList = null
  })

  function setupMailTableColumnResize() {
    var columnIds = ["accountMarker", "starred", "attachment", "thread", "from", "to", "subject", "date"]
    var minWidths = [24, 32, 32, 28, 90, 90, 140, 64]
    var fixedWidths = { accountMarker: 24, starred: 24, attachment: 24, thread: 28 }
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
      var button = e.target.closest("[data-mail-list-display-menu-button]")
      if (button) {
        var root = button.closest("[data-tui-popover-root]")
        var menu = root && root.querySelector("[data-mail-list-display-menu]")
        if (menu) syncDisplayMenu(menu)
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
        var parentMenu = item.closest("[data-mail-list-display-menu]")
        if (parentMenu) syncDisplayMenu(parentMenu)
        else syncColumnMenu(menuPanel)
      }
    })

    function syncDisplayMenu(menu) {
      var tableMenu = menu.querySelector("[data-mail-table-column-menu]")
      if (tableMenu) syncColumnMenu(tableMenu)
    }

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
        var readButtons = document.querySelectorAll('[data-read-email="' + emailId + '"]')
        for (var i = 0; i < readButtons.length; i++) setReadButtonIcon(readButtons[i], true)
        syncThreadReadButtons()
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

  function getContactRowPreview(trigger) {
    var row = trigger && trigger.closest && trigger.closest(".mail-list-item")
    if (!row) return null
    var fallback = row.querySelector("[data-avatar-fallback]")
    var avatar = row.querySelector("[data-avatar-image]")
    var name = textFrom(row, "[data-contact-name]")
    var email = textFrom(row, "[data-contact-email]")
    return {
      initials: fallback ? fallback.textContent.trim() : "",
      avatar: avatar && !avatar.classList.contains("hidden") ? (avatar.currentSrc || avatar.getAttribute("src") || "") : "",
      name: name || email || "Loading contact",
      email: email,
    }
  }

  function contactDetailSkeletonField(label, widthClass, className) {
    return '<div class="min-w-0 border-b border-ink/10 py-3 last:border-b-0 lg:[&:nth-last-child(-n+2)]:border-b-0 xl:last:border-b-0 ' + (className || "") + '">' +
      '<div class="text-[10px] font-semibold uppercase tracking-wider text-ink/35">' + label + '</div>' +
      '<div class="mt-1 flex min-h-5 items-center">' +
        '<div class="h-3 ' + widthClass + ' max-w-full rounded bg-ink/5 animate-pulse"></div>' +
      '</div>' +
    '</div>'
  }

  function contactDetailSkeletonSectionHeader(label) {
    return '<div class="flex items-center gap-2 border-b border-ink/10 px-4 py-3 text-ink/40">' +
      pendingIcon("size-3.5") +
      '<h2 class="text-xs font-semibold uppercase tracking-wider text-ink/45">' + label + '</h2>' +
    '</div>'
  }

  function contactDetailRecentActivitySkeleton() {
    var rows = ""
    for (var i = 0; i < 4; i++) {
      rows += '<div class="border-b border-ink/10 px-3 py-2.5 last:border-b-0">' +
        '<div class="flex items-start justify-between gap-3">' +
          '<div class="min-w-0 flex flex-1 items-center gap-2">' +
            '<div class="h-5 w-9 shrink-0 rounded border border-ink/10 bg-ink/[0.04] animate-pulse"></div>' +
            '<div class="h-3 min-w-0 flex-1 rounded bg-ink/5 animate-pulse"></div>' +
          '</div>' +
          '<div class="h-3 w-16 shrink-0 rounded bg-ink/5 animate-pulse"></div>' +
        '</div>' +
        '<div class="mt-2 h-3 w-4/5 rounded bg-ink/5 animate-pulse"></div>' +
      '</div>'
    }
    return '<div class="w-full rounded-lg border border-ink/10 bg-ink/[0.025] p-4">' +
      '<div class="mb-3 flex items-center justify-between gap-3">' +
        '<div>' +
          '<h2 class="text-xs font-semibold uppercase tracking-wider text-ink/45">Recent activity</h2>' +
          '<p class="mt-1 text-xs text-ink/35">Latest emails involving this contact.</p>' +
        '</div>' +
        '<div class="flex shrink-0 items-center gap-2">' +
          '<span class="rounded bg-ink/[0.05] px-2 py-1 text-[10px] font-semibold uppercase tracking-wider text-ink/40">Top 10</span>' +
          '<div class="h-7 w-16 rounded-md border border-ink/10 bg-paper/45"></div>' +
        '</div>' +
      '</div>' +
      '<div class="overflow-hidden rounded-md border border-ink/10 bg-paper/35">' + rows + '</div>' +
    '</div>'
  }

  function showContactsDetailLoading(trigger) {
    var detail = document.getElementById("contacts-detail")
    if (!detail) return
    var preview = getContactRowPreview(trigger) || {}
    var initials = escapeHTML(preview.initials || "")
    var avatar = escapeHTML(preview.avatar || "")
    var name = escapeHTML(preview.name || "Loading contact")
    var avatarHTML = avatar
      ? '<div class="size-14 shrink-0 overflow-hidden rounded-full bg-ink/5 shadow-[0_8px_22px_rgba(0,0,0,0.16)]"><img src="' + avatar + '" alt="" class="block size-full object-cover"/></div>'
      : '<div class="flex size-14 shrink-0 items-center justify-center rounded-full bg-gradient-to-b from-amber-700/70 to-amber-900/70 text-base font-bold text-amber-100 shadow-[0_8px_22px_rgba(0,0,0,0.16)]">' + initials + '</div>'
    detail.setAttribute("aria-busy", "true")
    detail.innerHTML =
      '<div class="surface-paper rounded-md flex flex-col h-full overflow-hidden" data-contact-detail-loading>' +
        '<span class="sr-only" role="status">Loading contact details</span>' +
        '<div class="shrink-0 border-b border-ink/10 bg-paper/70 px-5 py-4 sm:px-7">' +
          '<div class="flex flex-col gap-4 xl:flex-row xl:items-center xl:justify-between">' +
            '<div class="flex min-w-0 items-start gap-4">' +
              avatarHTML +
              '<div class="min-w-0 pt-0.5">' +
                '<h1 class="min-w-0 truncate text-2xl font-bold leading-tight tracking-tight text-ink" style="font-family: var(--font-serif)">' + name + '</h1>' +
                '<div class="mt-1 text-sm text-ink/45">Contact entry</div>' +
              '</div>' +
            '</div>' +
            '<div class="flex flex-wrap items-center gap-2 xl:justify-end" aria-hidden="true">' +
              '<div class="h-9 w-24 rounded-md border border-ink/10 bg-ink/[0.025] animate-pulse"></div>' +
              '<div class="size-9 rounded-md bg-ink/[0.03] animate-pulse"></div>' +
              '<div class="size-9 rounded-md bg-ink/[0.03] animate-pulse"></div>' +
              '<div class="size-9 rounded-md bg-ink/[0.03] animate-pulse"></div>' +
            '</div>' +
          '</div>' +
        '</div>' +
        '<div class="flex-1 overflow-y-auto">' +
          '<div class="w-full space-y-5 px-5 py-5 sm:px-7" aria-hidden="true">' +
            '<section class="rounded-lg border border-ink/10 bg-ink/[0.02]">' +
              contactDetailSkeletonSectionHeader("Contact") +
              '<div class="grid gap-x-6 px-4 lg:grid-cols-2">' +
                contactDetailSkeletonField("Name", "w-40") +
                contactDetailSkeletonField("Email", "w-52") +
                contactDetailSkeletonField("Additional emails", "w-24") +
                contactDetailSkeletonField("Phone", "w-28") +
                contactDetailSkeletonField("Additional phones", "w-24") +
                contactDetailSkeletonField("Title", "w-32") +
                contactDetailSkeletonField("Organization", "w-36") +
                contactDetailSkeletonField("Notes", "w-48") +
              '</div>' +
            '</section>' +
            '<section class="rounded-lg border border-ink/10 bg-ink/[0.02]">' +
              '<div class="flex flex-wrap items-center justify-between gap-3 border-b border-ink/10 px-4 py-3">' +
                '<div class="flex flex-wrap items-center gap-2 text-ink/40">' +
                  pendingIcon("size-3.5") +
                  '<h2 class="text-xs font-semibold uppercase tracking-wider text-ink/55">Gofer Sync</h2>' +
                  '<div class="h-6 w-16 rounded-md border border-ink/10 bg-ink/[0.03] animate-pulse"></div>' +
                '</div>' +
                '<div class="h-7 w-24 rounded-md border border-ink/10 bg-ink/[0.025] animate-pulse"></div>' +
              '</div>' +
              '<div class="grid gap-x-6 px-4 lg:grid-cols-2">' +
                contactDetailSkeletonField("Sync locations", "w-16", "lg:col-span-2") +
                contactDetailSkeletonField("Origin", "w-24") +
                contactDetailSkeletonField("Sync status", "w-20") +
                contactDetailSkeletonField("Sync updated", "w-32") +
                contactDetailSkeletonField("Sync error", "w-24") +
              '</div>' +
            '</section>' +
            '<section class="rounded-lg border border-ink/10 bg-ink/[0.02]">' +
              contactDetailSkeletonSectionHeader("Activity") +
              '<div class="grid gap-x-6 px-4 sm:grid-cols-2 xl:grid-cols-4">' +
                contactDetailSkeletonField("Messages", "w-10") +
                contactDetailSkeletonField("Last seen", "w-28") +
                contactDetailSkeletonField("Added to Gofer", "w-24") +
                contactDetailSkeletonField("Contact updated", "w-24") +
              '</div>' +
            '</section>' +
            contactDetailRecentActivitySkeleton() +
          '</div>' +
        '</div>' +
      '</div>'
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
    if (loadInitialFolderContent(scroll, folderID)) return
    virtualMailList = createMailListController(scroll, folderID)
    virtualMailList.hydrateFromDOM({ animate: true })
    scroll._virtualMailList = virtualMailList
    flushPendingSyncEvents()
    applyActiveFolderSyncState()
    bindThreadToggle(scroll)

    virtualMailList.replaceUrl()
    if (typeof initResizeHandles === "function") initResizeHandles()
  })

  document.body.addEventListener("htmx:afterSwap", function (evt) {
    if (!evt.target || !evt.target.querySelector) return

    var scroll = evt.target.id === "mail-list-scroll"
      ? evt.target
      : evt.target.querySelector("#mail-list-scroll")
    if (scroll && typeof window.applyMailTableColumnSettings === "function") window.applyMailTableColumnSettings(scroll)
    if (scroll && typeof window.applyMailCardFieldSettings === "function") window.applyMailCardFieldSettings(scroll)
  })

  document.body.addEventListener("htmx:afterSettle", function () {
    if (typeof initResizeHandles === "function") initResizeHandles()
  })
})

var sendStatusTimer = null
var _sendStatusToast = null
var _mailSyncIssuesByAccount = Object.create(null)
var _mailSyncIssueOrder = []
var _outgoingSendStatusByID = Object.create(null)
var _outgoingSendStatusByMessage = Object.create(null)
var _outgoingSendPollers = Object.create(null)
var _outgoingStatusLoadTimer = null
var _outgoingStatusLoading = false
var _outgoingStatusReloadQueued = false
var _outgoingStatusSetupDone = false
var _mailOperationActionsSetupDone = false

function outgoingStatusEscape(value) {
  return String(value == null ? "" : value).replace(/[&<>'"]/g, function (ch) {
    return { "&": "&amp;", "<": "&lt;", ">": "&gt;", "'": "&#39;", '"': "&quot;" }[ch]
  })
}

function outgoingSendStatusLabel(summary) {
  if (!summary) return ""
  if (summary.status === "pending") {
    if (Number(summary.attempt_count) > 0) return "Retrying"
    if (summary.is_scheduled) return "Scheduled"
    return "Queued"
  }
  if (summary.status === "sending") return "Sending"
  if (summary.status === "failed") return "Failed"
  if (summary.status === "ambiguous") return "Needs review"
  if (summary.status === "canceled") return "Canceled"
  if (summary.status === "sent") {
    if (summary.sent_copy_status === "pending" || summary.sent_copy_status === "copying") return "Sent copy pending"
    if (summary.sent_copy_status === "failed") return "Sent copy failed"
    if (summary.sent_copy_status === "ambiguous") return "Sent copy needs review"
  }
  return ""
}

function outgoingSendStatusClasses(summary) {
  if (!summary) return ""
  if (summary.status === "failed") return "border-destructive/30 bg-destructive/10 text-destructive"
  if (summary.status === "ambiguous") return "border-orange-500/30 bg-orange-500/10 text-orange-700 dark:text-orange-300"
  if (summary.status === "sent" && (summary.sent_copy_status === "failed" || summary.sent_copy_status === "ambiguous")) {
    return "border-orange-500/30 bg-orange-500/10 text-orange-700 dark:text-orange-300"
  }
  if (summary.status === "sending") return "border-primary/30 bg-primary/10 text-primary"
  if (summary.status === "pending" && Number(summary.attempt_count) > 0) return "border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-300"
  return "border-blue-500/30 bg-blue-500/10 text-blue-700 dark:text-blue-300"
}

function outgoingSendStatusError(summary) {
  if (!summary) return ""
  return summary.last_error || summary.sent_copy_last_error || ""
}

function outgoingSendStatusMarkup(summary) {
  var label = outgoingSendStatusLabel(summary)
  if (!label) return ""
  var error = outgoingSendStatusError(summary)
  var title = error ? " title=\"" + outgoingStatusEscape(error) + "\"" : ""
  var html = '<span data-outgoing-status="" class="inline-flex items-center gap-1 rounded-full border px-1.5 py-0.5 text-[10px] font-medium ' + outgoingSendStatusClasses(summary) + '"' + title + '>' + outgoingStatusEscape(label) + '</span>'
  var actions = []
  if (summary.can_retry) actions.push({ action: "retry", label: "Retry" })
  if (summary.can_retry_now && (Number(summary.attempt_count) > 0 || summary.is_scheduled)) actions.push({ action: "retry-now", label: summary.is_scheduled ? "Send now" : "Try now" })
  if (summary.can_cancel) actions.push({ action: "cancel", label: "Cancel" })
  for (var i = 0; i < actions.length; i++) {
    html += '<button type="button" data-outgoing-action="' + actions[i].action + '" data-outgoing-id="' + outgoingStatusEscape(summary.id) + '" class="text-[10px] font-medium underline decoration-dotted underline-offset-2 hover:no-underline" aria-label="' + outgoingStatusEscape(actions[i].label) + ' outgoing message">' + outgoingStatusEscape(actions[i].label) + '</button>'
  }
  return '<span class="inline-flex items-center gap-1.5" data-outgoing-status-group="">' + html + '</span>'
}

function outgoingSendSummaryIsTerminal(summary) {
  if (!summary) return true
  if (summary.status === "failed" || summary.status === "ambiguous" || summary.status === "canceled") return true
  return summary.status === "sent" && (summary.sent_copy_status === "not_required" || summary.sent_copy_status === "complete")
}

function outgoingSendSummaryShouldHide(summary) {
  if (!summary) return true
  return summary.status === "canceled" || (summary.status === "sent" && (summary.sent_copy_status === "not_required" || summary.sent_copy_status === "complete"))
}

function clearOutgoingSendMessageStatus(messageID) {
  if (!messageID) return
  var rows = document.querySelectorAll('.mail-list-item[data-email-id]')
  for (var i = 0; i < rows.length; i++) {
    if (String(rows[i].getAttribute("data-email-id")) !== String(messageID)) continue
    var cardZone = rows[i].querySelector('[data-mail-card-zone="status"]')
    if (cardZone) cardZone.innerHTML = ""
    var tableStatuses = rows[i].querySelectorAll('[data-outgoing-status-group]')
    for (var j = 0; j < tableStatuses.length; j++) tableStatuses[j].remove()
  }
}

function applyOutgoingSendSummary(summary) {
  if (!summary || !summary.id) return
  var previous = _outgoingSendStatusByID[summary.id]
  if (outgoingSendSummaryShouldHide(summary)) {
    delete _outgoingSendStatusByID[summary.id]
    if (summary.message_id) delete _outgoingSendStatusByMessage[String(summary.message_id)]
    if (previous && previous.message_id) clearOutgoingSendMessageStatus(previous.message_id)
    if (summary.message_id) clearOutgoingSendMessageStatus(summary.message_id)
    stopOutgoingSendPolling(summary.id)
    return
  }
  _outgoingSendStatusByID[summary.id] = summary
  if (summary.message_id) _outgoingSendStatusByMessage[String(summary.message_id)] = summary
  if (!summary.message_id) return
  var markup = outgoingSendStatusMarkup(summary)
  if (!markup) return
  var rows = document.querySelectorAll('.mail-list-item[data-email-id]')
  for (var i = 0; i < rows.length; i++) {
    if (String(rows[i].getAttribute("data-email-id")) !== String(summary.message_id)) continue
    var cardZone = rows[i].querySelector('[data-mail-card-zone="status"]')
    if (cardZone) cardZone.innerHTML = markup
    var subject = rows[i].querySelector('[data-mail-table-cell="subject"]')
    if (subject) {
      var oldStatuses = subject.querySelectorAll('[data-outgoing-status-group]')
      for (var j = 0; j < oldStatuses.length; j++) oldStatuses[j].remove()
      subject.insertAdjacentHTML("beforeend", markup)
    }
  }
}

function renderOutgoingSendStatuses() {
  for (var id in _outgoingSendStatusByID) {
    if (Object.prototype.hasOwnProperty.call(_outgoingSendStatusByID, id)) applyOutgoingSendSummary(_outgoingSendStatusByID[id])
  }
}

function recoverComposeOutgoingSend(summary) {
  if (!summary || !summary.draft_id) return
  var forms = document.querySelectorAll("#compose-form, #compose-pane-form")
  for (var i = 0; i < forms.length; i++) {
    var draftField = forms[i].querySelector('input[name="draft_id"]')
    if (!draftField || String(draftField.value || "").trim() !== String(summary.draft_id).trim()) continue
    var fromPane = forms[i].id === "compose-pane-form"
    if (summary.status === "pending" || summary.status === "sending") {
      _composeSendState = { formId: forms[i].id, fromPane: fromPane, sendID: summary.id }
      forms[i].dataset.composeOutgoingStatus = outgoingSendStatusLabel(summary)
      _setComposeSending(forms[i], true)
      if (summary.status === "pending" && (Number(summary.attempt_count) > 0 || summary.is_scheduled)) {
        _setComposeSending(forms[i], false)
      }
      forms[i].dataset.composeDirty = "false"
    } else if (_composeSendState && _composeSendState.sendID === summary.id) {
      _setComposeSending(forms[i], false)
      forms[i].dataset.composeOutgoingStatus = outgoingSendStatusLabel(summary)
      forms[i].dataset.composeDirty = "true"
      _composeSendState = null
    }
  }
}

function fetchOutgoingSendStatus(id) {
  if (!id) return Promise.resolve(null)
  return fetch("/api/outgoing-sends/" + encodeURIComponent(id), { headers: { "Accept": "application/json" } })
    .then(function (response) {
      if (response.status === 404) {
        stopOutgoingSendPolling(id)
        return null
      }
      if (!response.ok) throw new Error("failed to load outgoing send status")
      return response.json()
    })
    .then(function (summary) {
      if (!summary) return null
      applyOutgoingSendSummary(summary)
      recoverComposeOutgoingSend(summary)
      if (outgoingSendSummaryIsTerminal(summary)) stopOutgoingSendPolling(summary.id)
      return summary
    })
}

function startOutgoingSendPolling(id) {
  if (!id || _outgoingSendPollers[id]) return
  var poller = { stopped: false, timer: null }
  _outgoingSendPollers[id] = poller
  function schedule() {
    if (poller.stopped) return
    poller.timer = setTimeout(tick, 4000)
  }
  function tick() {
    if (poller.stopped) return
    fetchOutgoingSendStatus(id).catch(function () {}).then(function (summary) {
      if (poller.stopped) return
      if (summary && outgoingSendSummaryIsTerminal(summary)) {
        stopOutgoingSendPolling(id)
        return
      }
      schedule()
    })
  }
  tick()
}

function stopOutgoingSendPolling(id) {
  var poller = id && _outgoingSendPollers[id]
  if (!poller) return
  poller.stopped = true
  if (poller.timer) clearTimeout(poller.timer)
  delete _outgoingSendPollers[id]
}

function scheduleOutgoingSendStatusLoad() {
  if (_outgoingStatusLoadTimer) clearTimeout(_outgoingStatusLoadTimer)
  _outgoingStatusLoadTimer = setTimeout(function () {
    _outgoingStatusLoadTimer = null
    loadActiveOutgoingSends()
  }, 120)
}

function loadActiveOutgoingSends() {
  if (_outgoingStatusLoading) {
    _outgoingStatusReloadQueued = true
    return
  }
  _outgoingStatusLoading = true
  fetch("/api/outgoing-sends/active", { headers: { "Accept": "application/json" } })
    .then(function (response) {
      if (!response.ok) throw new Error("failed to load active outgoing sends")
      return response.json()
    })
    .then(function (data) {
      var summaries = data && Array.isArray(data.sends) ? data.sends : []
      var activeIDs = Object.create(null)
      document.querySelectorAll("[data-outgoing-status-group]").forEach(function (node) { node.remove() })
      _outgoingSendStatusByID = Object.create(null)
      _outgoingSendStatusByMessage = Object.create(null)
      for (var i = 0; i < summaries.length; i++) {
        var summary = summaries[i]
        activeIDs[summary.id] = true
        applyOutgoingSendSummary(summary)
        recoverComposeOutgoingSend(summary)
        if (!outgoingSendSummaryIsTerminal(summary)) startOutgoingSendPolling(summary.id)
      }
      for (var id in _outgoingSendPollers) {
        if (Object.prototype.hasOwnProperty.call(_outgoingSendPollers, id) && !activeIDs[id]) stopOutgoingSendPolling(id)
      }
      renderOutgoingSendStatuses()
    })
    .catch(function () {})
    .then(function () {
      _outgoingStatusLoading = false
      if (_outgoingStatusReloadQueued) {
        _outgoingStatusReloadQueued = false
        scheduleOutgoingSendStatusLoad()
      }
    })
}

function outgoingSendAction(action, id, button) {
  if (!action || !id) return
  var summary = _outgoingSendStatusByID[id]
  var confirm = false
  if (action === "retry" && summary && summary.ambiguous_warning_required) {
    confirm = window.confirm("Gofer lost the connection after sending this message, so it may already have been delivered. Check Sent before retrying. Retrying can send a duplicate. Continue?")
    if (!confirm) return
  }
  if (button) button.disabled = true
  var endpoint = "/api/outgoing-sends/" + encodeURIComponent(id) + "/" + action
  var options = { method: "POST", headers: { "Accept": "application/json" } }
  if (action === "retry") {
    options.headers["Content-Type"] = "application/json"
    options.body = JSON.stringify({ confirm: confirm })
  }
  fetch(endpoint, options)
    .then(function (response) {
      return response.json().catch(function () { return {} }).then(function (data) {
        if (!response.ok) throw new Error(data.error || "Could not update outgoing send")
        return data
      })
    })
    .then(function (next) {
      applyOutgoingSendSummary(next)
      recoverComposeOutgoingSend(next)
      if (next.status === "canceled") showSendStatus("canceled", "The message remains available as a draft.")
      else showSendStatus("sending", next.status === "pending" ? "Message queued" : "Updating message status...")
      if (!outgoingSendSummaryIsTerminal(next)) startOutgoingSendPolling(next.id)
      scheduleOutgoingSendStatusLoad()
    })
    .catch(function (error) {
      showSendStatus("failed", error && error.message ? error.message : "Could not update outgoing send")
    })
    .finally(function () {
      if (button) button.disabled = false
    })
}

function setupOutgoingSendStatus() {
  if (_outgoingStatusSetupDone) return
  _outgoingStatusSetupDone = true
  loadActiveOutgoingSends()
  document.addEventListener("click", function (event) {
    var button = event.target && event.target.closest ? event.target.closest("[data-outgoing-action]") : null
    if (!button) return
    event.preventDefault()
    event.stopImmediatePropagation()
    outgoingSendAction(button.getAttribute("data-outgoing-action"), button.getAttribute("data-outgoing-id"), button)
  })
  document.addEventListener("htmx:afterSwap", scheduleOutgoingSendStatusLoad)
  document.addEventListener("htmx:afterSettle", scheduleOutgoingSendStatusLoad)
  new MutationObserver(function (mutations) {
    for (var i = 0; i < mutations.length; i++) {
      for (var j = 0; j < mutations[i].addedNodes.length; j++) {
        var node = mutations[i].addedNodes[j]
        if (node && node.nodeType === 1 && (node.matches(".mail-list-item[data-email-id]") || node.querySelector(".mail-list-item[data-email-id]"))) {
          renderOutgoingSendStatuses()
          return
        }
      }
    }
  }).observe(document.body, { childList: true, subtree: true })
}

function setupMailOperationActions() {
  if (_mailOperationActionsSetupDone) return
  _mailOperationActionsSetupDone = true
  document.addEventListener("click", function (event) {
    var button = event.target && event.target.closest ? event.target.closest("[data-mail-operation-retry]") : null
    if (!button) return
    event.preventDefault()
    event.stopImmediatePropagation()
    var operationID = button.getAttribute("data-mail-operation-id") || ""
    if (!operationID) return
    button.disabled = true
    fetch("/api/mail-operations/" + encodeURIComponent(operationID) + "/retry", {
      method: "POST",
      headers: { "Accept": "application/json" },
    }).then(function (response) {
      return response.json().catch(function () { return {} }).then(function (data) {
        if (!response.ok) throw new Error(data.error || "Could not retry mail operation")
        return data
      })
    }).then(function () {
      if (window.htmx && typeof window.htmx.ajax === "function") {
        window.htmx.ajax("GET", "/settings/operations/content", { target: "#mail-operations-content", swap: "outerHTML" })
      } else {
        window.location.reload()
      }
      if (typeof showGoferToast === "function") {
        showGoferToast({ id: "mail-operation-toast", title: "Mail operation queued", description: "Gofer will try the provider operation again.", variant: "success", icon: "success", position: "bottom-right", duration: 4500, dismissible: true })
      }
    }).catch(function (error) {
      if (typeof showGoferToast === "function") {
        showGoferToast({ id: "mail-operation-toast", title: "Could not retry operation", description: error && error.message ? error.message : "The operation was not changed.", variant: "error", icon: "error", position: "bottom-right", duration: 7000, dismissible: true })
      }
      button.disabled = false
    })
  })
}

// On a phone the open reader slides away before it is cleared (see mobile-nav.js).
function setMailViewEmpty() {
  if (window.GoferMobileNav && window.GoferMobileNav.animatePaneClose) {
    window.GoferMobileNav.animatePaneClose(setMailViewEmptyNow)
    return
  }
  setMailViewEmptyNow()
}

function setMailViewEmptyNow() {
  var mailView = document.getElementById("mail-view")
  if (!mailView) return
  mailView.innerHTML =
    '<div class="flex flex-col items-center justify-center h-full text-center" data-mail-view-empty>' +
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

// Returns from the full-screen reader to the list on small screens.
function closeMobileMailView() {
  if (history.state && history.state.email && history.state.fromList) {
    history.back()
    return
  }
  var container = document.getElementById("mail-list-scroll")
  var vml = container && container._virtualMailList
  if (vml) {
    var emailId = vml.selectedEmailId
    vml.selectedEmailId = null
    vml.syncSelectionClasses(vml.itemsContainer)
    vml.replaceUrl()
    document.dispatchEvent(new CustomEvent("gofer:mail-view-closed", { detail: { emailId: emailId } }))
  }
  setMailViewEmpty()
}

// Returns from the full-screen contact to the list on small screens. The list keeps the
// selected contact in its history entries, so this clears the selection in place rather
// than going back.
function closeMobileContactView() {
  var scroll = document.getElementById("contacts-list-scroll")
  var list = scroll && scroll._virtualContactsList
  if (list) {
    list.selectedContactId = null
    list.render()
    list.updateURLForState()
  }
  function clearDetail() {
    var detail = document.getElementById("contacts-detail")
    if (!detail) return
    detail.innerHTML = ""
    detail.removeAttribute("data-contact-detail-id")
    detail.setAttribute("data-mail-view-empty", "")
  }
  if (window.GoferMobileNav && window.GoferMobileNav.animatePaneClose) window.GoferMobileNav.animatePaneClose(clearDetail)
  else clearDetail()
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
    position: "bottom-right",
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
  if (status === "canceled") return { title: "Send canceled", variant: "info", icon: "info" }
  if (status === "scheduled") return { title: "Message scheduled", variant: "success", icon: "success" }
  if (status === "sending") return { title: "Working...", variant: "info", icon: "spinner" }
  if (status === "retrying") return { title: "Send delayed", variant: "warning", icon: "warning" }
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
  if (toast._goferToastDismissing) return
  toast._goferToastDismissing = true
  if (toast._goferToastTimer) clearTimeout(toast._goferToastTimer)
  toast.style.transition = "opacity 300ms, transform 300ms"
  toast.style.opacity = "0"
  toast.style.transform = "translateY(1rem)"
  setTimeout(function () {
    if (!toast.isConnected) return
    if (toast.matches && toast.matches(":popover-open") && toast.hidePopover) {
      try { toast.hidePopover() } catch (_) {}
    }
    toast.remove()
  }, 300)
}

function removeGoferToastNow(toast) {
  if (!toast || !toast.isConnected) return
  if (toast._goferToastTimer) clearTimeout(toast._goferToastTimer)
  if (toast.matches && toast.matches(":popover-open") && toast.hidePopover) {
    try { toast.hidePopover() } catch (_) {}
  }
  toast.remove()
}

function showGoferToast(opts) {
  opts = opts || {}
  // Small screens show mail sync status in the top bar rather than as a toast.
  if (opts.id === "mail-sync-toast" && window.GoferMobileNav && window.GoferMobileNav.showSyncStatus(opts)) {
    var shown = document.getElementById(opts.id)
    if (shown) removeGoferToastNow(shown)
    return null
  }
  var id = opts.id || "gofer-toast-" + Date.now()
  var existing = document.getElementById(id)
  while (existing) {
    removeGoferToastNow(existing)
    existing = document.getElementById(id)
  }
  var duration = Number(opts.duration || 0)
  var actionHTML = ""
  if (opts.actionLabel || opts.secondaryActionLabel) {
    actionHTML = '<div class="mt-2 flex items-center gap-3 text-xs font-semibold">'
    if (opts.actionLabel) actionHTML += '<span class="underline underline-offset-2">' + _escapeComposeHTML(opts.actionLabel) + '</span>'
    if (opts.secondaryActionLabel) actionHTML += '<button type="button" class="text-xs font-semibold underline underline-offset-2 opacity-80 hover:opacity-100 disabled:opacity-50" data-gofer-toast-secondary' + (opts.secondaryActionDisabled ? ' disabled aria-disabled="true"' : '') + '>' + _escapeComposeHTML(opts.secondaryActionLabel) + '</button>'
    actionHTML += '</div>'
  }
  var toast = document.createElement("div")
  toast.id = id
  toast.setAttribute("popover", "manual")
  toast.dataset.tuiToast = ""
  toast.dataset.tuiToastDuration = String(duration)
  toast.dataset.position = opts.position || "bottom-right"
  toast.dataset.variant = opts.variant || "default"
  toast.className = "z-50 fixed m-0 border-0 bg-transparent overflow-visible pointer-events-auto p-4 w-fit max-w-[calc(100vw-2rem)] md:max-w-[680px] animate-in fade-in slide-in-from-bottom-4 duration-300 data-[position=top-right]:top-0 data-[position=top-right]:right-0 data-[position=top-left]:top-0 data-[position=top-left]:left-0 data-[position=top-center]:top-0 data-[position=top-center]:left-1/2 data-[position=top-center]:-translate-x-1/2 data-[position=bottom-right]:bottom-0 data-[position=bottom-right]:right-0 data-[position=bottom-left]:bottom-0 data-[position=bottom-left]:left-0 data-[position=bottom-center]:bottom-0 data-[position=bottom-center]:left-1/2 data-[position=bottom-center]:-translate-x-1/2 data-[position*=top]:slide-in-from-top-4 data-[position*=bottom]:slide-in-from-bottom-4"
  if (opts.width) toast.style.width = opts.width
  positionGoferToast(toast, toast.dataset.position)
  toast.innerHTML =
    '<div class="gofer-toast-card" data-variant="' + _escapeComposeHTML(opts.variant || "default") + '">' +
      (duration > 0 ? '<div class="gofer-toast-progress-wrap"><div class="toast-progress gofer-toast-progress" data-variant="' + _escapeComposeHTML(opts.variant || "default") + '"></div></div>' : '') +
      _goferToastIcon(opts.icon || "info") +
      '<span class="flex-1 min-w-0">' +
        (opts.title ? '<p class="text-sm font-semibold truncate">' + _escapeComposeHTML(opts.title) + '</p>' : '') +
        (opts.description ? '<p class="text-sm opacity-90 mt-1">' + _escapeComposeHTML(opts.description) + '</p>' : '') +
        actionHTML +
      '</span>' +
      (opts.dismissible ? '<button type="button" class="gofer-toast-dismiss" aria-label="Close" data-tui-toast-dismiss>x</button>' : '') +
    '</div>'
  var dismiss = toast.querySelector("[data-tui-toast-dismiss]")
  if (dismiss) dismiss.addEventListener("click", function () { dismissGoferToast(toast) })
  var secondary = toast.querySelector("[data-gofer-toast-secondary]")
  if (secondary && !opts.secondaryActionDisabled && typeof opts.onSecondaryAction === "function") {
    secondary.addEventListener("click", function (e) {
      e.preventDefault()
      e.stopPropagation()
      opts.onSecondaryAction(e)
    })
  }
  if (typeof opts.onClick === "function") {
    var card = toast.querySelector(".gofer-toast-card")
    if (card) {
      card.setAttribute("role", "button")
      card.setAttribute("tabindex", "0")
      card.style.cursor = "pointer"
      card.addEventListener("click", function (e) {
        if (e.target && e.target.closest && e.target.closest("[data-tui-toast-dismiss]")) return
        if (e.target && e.target.closest && e.target.closest("[data-gofer-toast-secondary]")) return
        opts.onClick(e)
      })
      card.addEventListener("keydown", function (e) {
        if (e.key !== "Enter" && e.key !== " ") return
        if (e.target && e.target.closest && e.target.closest("[data-gofer-toast-secondary]")) return
        e.preventDefault()
        opts.onClick(e)
      })
    }
  }
  document.body.appendChild(toast)
  if (toast.showPopover) {
    try { toast.showPopover() } catch (_) {}
  }
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

function positionGoferToast(toast, position) {
  toast.style.position = "fixed"
  toast.style.inset = "auto"
  toast.style.top = "auto"
  toast.style.right = "auto"
  toast.style.bottom = "auto"
  toast.style.left = "auto"
  toast.style.transform = ""

  if (position === "top-left") {
    toast.style.top = "0"
    toast.style.left = "0"
  } else if (position === "top-right") {
    toast.style.top = "0"
    toast.style.right = "0"
  } else if (position === "bottom-left") {
    toast.style.bottom = "0"
    toast.style.left = "0"
  } else if (position === "bottom-right") {
    toast.style.bottom = "0"
    toast.style.right = "0"
  } else if (position === "bottom-center") {
    toast.style.bottom = "0"
    toast.style.left = "50%"
    toast.style.transform = "translateX(-50%)"
  } else {
    toast.style.top = "0"
    toast.style.left = "50%"
    toast.style.transform = "translateX(-50%)"
  }
}

var _mailSyncRunID = ""
var _mailSyncActive = false
var _mailSyncCancelRequested = false
var _mailSyncState = createMailSyncProgressState("")
var _mailSyncForceTooltipText = "Force sync, including IDLE folders."
var _mailSyncScheduledTooltipText = "Scheduled sync running. IDLE folders are not included."
var _mailSyncForceAriaLabel = "Force sync all mail, including IDLE folders"
var _mailSyncScheduledAriaLabel = "Scheduled sync running, IDLE folders not included"

function createMailSyncProgressState(runID) {
  return {
    runID: runID || "",
    kind: "",
    active: false,
    status: "idle",
    mode: "",
    startedAt: 0,
    completedAt: 0,
    total: 0,
    done: 0,
    parallelism: 0,
    failures: 0,
    skipped: 0,
    cancelled: 0,
    notDone: 0,
    accounts: Object.create(null),
    accountOrder: [],
    runs: Object.create(null),
    runOrder: [],
  }
}

function resetMailSyncProgressState(runID, kind) {
  _mailSyncCancelRequested = false
  _mailSyncState = createMailSyncProgressState(runID || "")
  _mailSyncState.kind = kind || ""
  _mailSyncState.active = true
  _mailSyncState.status = "syncing"
  _mailSyncState.startedAt = Date.now()
  renderMailSyncProgressDialog()
}

function stopMailSyncProgressState(status) {
  _mailSyncState.active = false
  _mailSyncState.status = status || _mailSyncState.status || "idle"
  _mailSyncState.completedAt = Date.now()
  renderMailSyncProgressDialog()
}

function _mailSyncModeFromData(data) {
  var hasMode = !!(data && Object.prototype.hasOwnProperty.call(data, "mode"))
  var mode = String(hasMode ? data.mode : (_mailSyncState.mode || "")).trim().toLowerCase()
  return mode === "repair" ? "repair" : "sync"
}

function _mailSyncRunMode(run) {
  return _mailSyncModeFromData(run || {})
}

function _mailSyncRunKey(data, kind) {
  var runID = String((data && data.run_id) || "").trim()
  if (runID) return runID
  return (kind || "manual") + ":" + _mailSyncModeFromData(data)
}

function _mailSyncAccountKey(accountID, runID) {
  accountID = accountID || "__unknown__"
  runID = String(runID || "").trim()
  return runID ? runID + "::" + accountID : accountID
}

function ensureMailSyncRun(data, kind) {
  data = data || {}
  var key = _mailSyncRunKey(data, kind)
  var run = _mailSyncState.runs[key]
  if (!run) {
    run = {
      key: key,
      runID: String(data.run_id || "").trim(),
      kind: kind || data.kind || "manual",
      mode: _mailSyncModeFromData(data),
      active: true,
      status: "syncing",
      total: 0,
      done: 0,
      parallelism: 0,
      failures: 0,
      skipped: 0,
      cancelled: 0,
      notDone: 0,
      startedAt: Date.now(),
      completedAt: 0,
    }
    _mailSyncState.runs[key] = run
    _mailSyncState.runOrder.push(key)
  }
  if (data.run_id) run.runID = String(data.run_id).trim()
  run.kind = kind || data.kind || run.kind || "manual"
  run.mode = _mailSyncModeFromData(data)
  return run
}

function _mailSyncHasActiveRunMode(mode) {
  mode = mode === "repair" ? "repair" : "sync"
  var keys = _mailSyncState.runOrder || []
  for (var i = 0; i < keys.length; i++) {
    var run = _mailSyncState.runs[keys[i]]
    if (run && run.active && _mailSyncRunMode(run) === mode) return true
  }
  return false
}

function _mailSyncHasActiveManualRun() {
  var keys = _mailSyncState.runOrder || []
  for (var i = 0; i < keys.length; i++) {
    var run = _mailSyncState.runs[keys[i]]
    if (run && run.active && run.kind === "manual") return true
  }
  return false
}

function updateMailSyncAggregateFromRuns() {
  var keys = _mailSyncState.runOrder || []
  var active = false
  var anyManual = false
  var anyScheduled = false
  var anyError = false
  var anyPartial = false
  var anyCancelled = false
  var total = 0
  var done = 0
  var failures = 0
  var skipped = 0
  var cancelled = 0
  var notDone = 0
  var parallelism = 0
  var activeRunID = ""
  for (var i = 0; i < keys.length; i++) {
    var run = _mailSyncState.runs[keys[i]]
    if (!run) continue
    var runTotal = _mailSyncCount(run.total)
    var runDone = _mailSyncCount(run.done)
    total += runTotal
    done += runDone
    failures += _mailSyncCount(run.failures)
    skipped += _mailSyncCount(run.skipped)
    cancelled += _mailSyncCount(run.cancelled)
    notDone += _mailSyncCount(run.notDone)
    parallelism = Math.max(parallelism, _mailSyncCount(run.parallelism))
    if (run.kind === "manual") anyManual = true
    if (run.kind === "scheduled") anyScheduled = true
    if (run.active) {
      active = true
      if (!activeRunID && run.runID) activeRunID = run.runID
    }
    if (run.status === "error") anyError = true
    if (run.status === "partial") anyPartial = true
    if (run.status === "cancelled") anyCancelled = true
  }
  _mailSyncState.active = active
  _mailSyncState.kind = anyManual ? "manual" : (anyScheduled ? "scheduled" : _mailSyncState.kind)
  _mailSyncState.runID = activeRunID
  _mailSyncState.status = active ? "syncing" : (anyError ? "error" : (anyPartial ? "partial" : (anyCancelled ? "cancelled" : "ok")))
  _mailSyncState.total = total
  _mailSyncState.done = done
  _mailSyncState.failures = failures
  _mailSyncState.skipped = skipped
  _mailSyncState.cancelled = cancelled
  _mailSyncState.notDone = notDone
  _mailSyncState.parallelism = parallelism
  _mailSyncActive = active
  _mailSyncRunID = activeRunID
}

function _mailSyncRunningTitle(data) {
  if (_mailSyncHasActiveRunMode("repair") && _mailSyncHasActiveRunMode("sync")) return "Repair and sync running"
  return _mailSyncModeFromData(data) === "repair" ? "Repairing Gmail" : "Syncing mail"
}

function _mailSyncCompleteTitle(status, data) {
  if (_mailSyncModeFromData(data) !== "repair") {
    return status === "cancelled" ? "Mail sync cancelled" : (status === "error" ? "Mail sync failed" : (status === "partial" ? "Mail sync partly finished" : "Mail synced"))
  }
  return status === "cancelled" ? "Gmail repair cancelled" : (status === "error" ? "Gmail repair failed" : (status === "partial" ? "Gmail repair partly finished" : "Gmail repair complete"))
}

function setupMailSyncSidebarControls() {
  document.addEventListener("click", function (e) {
    var button = e.target && e.target.closest ? e.target.closest("[data-mail-sidebar-sync-button], [data-mail-account-sync-button]") : null
    if (!button || button.dataset.syncing !== "true") return
    if (_mailSyncCanSubmitBusyButton(button)) return
    e.preventDefault()
    e.stopImmediatePropagation()
    openMailSyncProgressDialog()
  }, true)
  syncMailSyncIssuesFromDOM(document, true)
  document.addEventListener("htmx:afterSwap", function (event) {
    updateMailSyncErrorIndicator(event.target || document)
  })
  updateMailSyncErrorIndicator()
  updateMailSyncSidebarTooltip()
}

function _mailSyncCanSubmitBusyButton(button) {
  if (!button) return false
  if (button.hasAttribute("data-repair-account-action")) return false
  if (_mailSyncHasActiveRunMode("sync")) return false
  return _mailSyncHasActiveRunMode("repair")
}

function setupMailSyncCancelControls() {
  document.addEventListener("click", function (e) {
    var button = e.target && e.target.closest ? e.target.closest("[data-mail-sync-cancel]") : null
    if (!button) return
    e.preventDefault()
    cancelMailSync()
  })
}

function cancelMailSync() {
  if (_mailSyncCancelRequested || _mailSyncState.kind !== "manual" || !(_mailSyncRunID || _mailSyncState.runID)) return
  _mailSyncCancelRequested = true
  renderMailSyncProgressDialog()
  showMailSyncToast({
    id: "mail-sync-toast",
    title: "Cancelling mail sync",
    description: "Stopping the foreground sync...",
    variant: "info",
    icon: "spinner",
    position: "bottom-right",
    duration: 0,
    dismissible: false,
  })
  fetch("/api/mail/sync/cancel", { method: "POST" }).catch(function () {
    _mailSyncCancelRequested = false
    renderMailSyncProgressDialog()
    showGoferToast({
      id: "mail-sync-toast",
      title: "Could not cancel sync",
      description: "Try again in a moment.",
      variant: "error",
      icon: "error",
      position: "bottom-right",
      duration: 6000,
      dismissible: true,
    })
  })
}

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

function _mailSyncHasField(data, key) {
  return !!data && Object.prototype.hasOwnProperty.call(data, key)
}

function _mailSyncIdleExclusionLabel(count) {
  count = _mailSyncCount(count)
  if (!count) return ""
  return "excluding " + count + " IDLE " + (count === 1 ? "folder" : "folders")
}

function _mailSyncAppendIdleExclusion(text, count) {
  var exclusion = _mailSyncIdleExclusionLabel(count)
  if (!exclusion) return text
  if (!text) return exclusion.charAt(0).toUpperCase() + exclusion.slice(1)
  return text + ", " + exclusion
}

function _mailSyncTotalExcludedIdleFolders() {
  var total = 0
  var ids = _mailSyncState.accountOrder || []
  for (var i = 0; i < ids.length; i++) {
    var account = _mailSyncState.accounts[ids[i]]
    total += _mailSyncCount(account && account.excludedIdleFolders)
  }
  return total
}

function _mailSyncAccountsLabel(count) {
  return count === 1 ? "1 account" : count + " accounts"
}

function _mailSyncIsScheduledKind(kind) {
  return kind === "scheduled"
}

function _mailSyncCanAdoptAccountKind() {
  return !_mailSyncRunID && _mailSyncState.kind !== "manual" && _mailSyncState.kind !== "scheduled"
}

function _mailSyncIsScheduledEvent(data) {
  return !!data && data.kind === "scheduled"
}

function populateMailSyncRunAccounts(data) {
  if (!data || !Array.isArray(data.account_ids)) return
  var runID = String(data.run_id || "").trim()
  var mode = _mailSyncModeFromData(data)
  for (var i = 0; i < data.account_ids.length; i++) {
    if (data.account_ids[i]) ensureMailSyncAccount(data.account_ids[i], i + 1, runID, mode)
  }
}

function _mailSyncSingleAccountIDFromData(data) {
  if (!data || !Array.isArray(data.account_ids) || data.account_ids.length !== 1) return ""
  return String(data.account_ids[0] || "").trim()
}

function ensureMailScheduledRunFromEvent(data) {
  if (!_mailSyncIsScheduledEvent(data)) return false
  if (_mailSyncHasActiveManualRun()) return false
  _mailSyncScopedBusyAccountID = ""
  var runID = data.run_id || ""
  if (_mailSyncState.kind !== "scheduled" || (runID && _mailSyncState.runID && _mailSyncState.runID !== runID)) {
    resetMailSyncProgressState(runID, "scheduled")
  } else if (runID && !_mailSyncState.runID) {
    _mailSyncState.runID = runID
  }
  _mailSyncActive = true
  _mailSyncState.kind = "scheduled"
  _mailSyncState.active = true
  _mailSyncState.status = "syncing"
  _mailSyncState.total = _mailSyncCount(data.accounts_total) || _mailSyncState.total
  if (_mailSyncHasField(data, "accounts_done")) _mailSyncState.done = _mailSyncCount(data.accounts_done)
  _mailSyncState.parallelism = _mailSyncCount(data.parallelism) || _mailSyncState.parallelism
  _mailSyncState.failures = _mailSyncCount(data.failures)
  _mailSyncState.skipped = _mailSyncCount(data.skipped)
  _mailSyncState.cancelled = _mailSyncCount(data.cancelled)
  populateMailSyncRunAccounts(data)
  _setMailSyncButtonBusy(true)
  return true
}

function updateMailSyncSidebarTooltip() {
  var scheduled = _mailSyncState.active && _mailSyncIsScheduledKind(_mailSyncState.kind)
  var text = scheduled ? _mailSyncScheduledTooltipText : _mailSyncForceTooltipText
  var tooltip = document.querySelector("[data-mail-sidebar-sync-tooltip]")
  if (tooltip) tooltip.textContent = text
  var button = document.querySelector("[data-mail-sidebar-sync-button]")
  if (button) button.setAttribute("aria-label", scheduled ? _mailSyncScheduledAriaLabel : _mailSyncForceAriaLabel)
}

function showMailSyncToast(opts) {
  opts = opts || {}
  opts.width = opts.width || "min(24rem, calc(100vw - 2rem))"
  opts.onClick = openMailSyncProgressDialog
  opts.actionLabel = opts.actionLabel || "View progress"
  if (opts.cancelable && !_mailSyncCancelRequested) {
    opts.secondaryActionLabel = opts.secondaryActionLabel || "Cancel"
    opts.onSecondaryAction = opts.onSecondaryAction || cancelMailSync
    opts.secondaryActionDisabled = !(_mailSyncRunID || _mailSyncState.runID)
  }
  return showGoferToast(opts)
}

function _mailSyncStatusLabel(status) {
  if (status === "synced" || status === "complete") return "Done"
  if (status === "cancelled") return "Cancelled"
  if (status === "error") return "Failed"
  if (status === "skipped") return "Already running"
  if (status === "queued") return "Queued"
  if (status === "syncing") return "Syncing"
  return "Waiting"
}

function _mailSyncStatusClass(status) {
  if (status === "synced" || status === "complete") return "text-emerald-600 dark:text-emerald-400"
  if (status === "error") return "text-destructive"
  if (status === "skipped" || status === "cancelled") return "text-amber-600 dark:text-amber-400"
  return "text-muted-foreground"
}

function _mailSyncAccountLabel(accountID) {
  var sections = document.querySelectorAll("[data-sidebar-account]")
  for (var i = 0; i < sections.length; i++) {
    if (sections[i].getAttribute("data-sidebar-account") !== accountID) continue
    var label = sections[i].querySelector("[data-sidebar-account-toggle] .flex-1")
    if (label && label.textContent.trim()) return label.textContent.trim()
  }
  return accountID || "Account"
}

function _mailSyncAccountEmail(accountID) {
  var buttons = document.querySelectorAll("[data-mail-account-sync-button]")
  for (var i = 0; i < buttons.length; i++) {
    if (buttons[i].getAttribute("data-mail-account-sync-button") !== accountID) continue
    var rows = buttons[i].querySelectorAll(".min-w-0 span")
    if (rows.length > 1 && rows[1].textContent.trim()) return rows[1].textContent.trim()
  }
  return ""
}

function _mailSyncFolderLabel(folderID, role) {
  var links = document.querySelectorAll('a[hx-get^="/folder/"]')
  for (var i = 0; i < links.length; i++) {
    if (links[i].getAttribute("hx-get") !== "/folder/" + folderID) continue
    var label = links[i].querySelector(".flex-1")
    if (label && label.textContent.trim()) return label.textContent.trim()
  }
  if (role) return role.charAt(0).toUpperCase() + role.slice(1)
  return folderID || "Folder"
}

function _mailSyncAccountFolderTotal(accountID) {
  if (!accountID) return 0
  var sections = document.querySelectorAll("[data-sidebar-account]")
  for (var i = 0; i < sections.length; i++) {
    if (sections[i].getAttribute("data-sidebar-account") !== accountID) continue
    return sections[i].querySelectorAll('a[hx-get^="/folder/"]').length
  }
  return 0
}

function _mailSyncCompletedFolderCount(account) {
  var count = 0
  var ids = account && account.folderOrder ? account.folderOrder : []
  for (var i = 0; i < ids.length; i++) {
    var folder = account.folders[ids[i]]
    if (folder && folder.status === "complete") count++
  }
  return count
}

function _mailSyncAccountProgressPercent(account) {
  if (!account) return 0
  if (account.status === "synced" || account.status === "complete" || account.status === "skipped" || account.status === "error" || account.status === "cancelled") return 100
  var total = _mailSyncCount(account.totalFolders) || account.folderOrder.length
  var done = Math.min(_mailSyncCount(account.syncedFolders), total)
  if (total > 0) return Math.max(account.status === "syncing" ? 8 : 0, Math.min(100, Math.round((done / total) * 100)))
  return account.status === "syncing" ? 28 : 0
}

function _mailSyncAccountFolderMeta(account) {
  var total = _mailSyncCount(account.totalFolders) || account.folderOrder.length
  var done = Math.min(_mailSyncCount(account.syncedFolders), total)
  var idleExcluded = _mailSyncCount(account.excludedIdleFolders)
  if (account.status === "queued") return "Waiting for account"
  if (account.status === "skipped" && /repair/i.test(account.error || "")) return "Skipped because repair is running"
  if (account.status === "skipped") return "Already running"
  if (!total && idleExcluded) return _mailSyncAppendIdleExclusion(account.status === "syncing" ? "Checking folders..." : "No polled folders", idleExcluded)
  if (account.status === "cancelled") return total ? _mailSyncAppendIdleExclusion("Cancelled after " + done + " of " + total + " folders", idleExcluded) : "Cancelled"
  if (account.status === "error") return total ? _mailSyncAppendIdleExclusion(done + " of " + total + " folders before error", idleExcluded) : _mailSyncAppendIdleExclusion("Sync failed", idleExcluded)
  if (!total) return account.status === "syncing" ? "Checking folders..." : _mailSyncStatusLabel(account.status)
  var label = account.status === "syncing" ? "Refreshing " : "Refreshed "
  return _mailSyncAppendIdleExclusion(label + done + " of " + total + " folders", idleExcluded)
}

function mailSyncHasActiveAccounts() {
  var ids = _mailSyncState.accountOrder || []
  for (var i = 0; i < ids.length; i++) {
    var account = _mailSyncState.accounts[ids[i]]
    if (account && (account.status === "syncing" || account.status === "queued")) return true
  }
  return false
}

function parseMailSyncUTCInstant(raw) {
  raw = String(raw || "").trim()
  if (!raw) return null
  if (/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}(:\d{2}(\.\d+)?)?$/.test(raw)) {
    raw = raw.replace(" ", "T") + "Z"
  }
  var date = new Date(raw)
  return isNaN(date.getTime()) ? null : date
}

function mailSyncIssueList() {
  var issues = []
  var seen = Object.create(null)
  for (var i = 0; i < _mailSyncIssueOrder.length; i++) {
    var id = _mailSyncIssueOrder[i]
    var issue = _mailSyncIssuesByAccount[id]
    if (!issue || seen[id]) continue
    seen[id] = true
    issues.push(issue)
  }
  var keys = Object.keys(_mailSyncIssuesByAccount)
  for (var j = 0; j < keys.length; j++) {
    if (seen[keys[j]]) continue
    issues.push(_mailSyncIssuesByAccount[keys[j]])
  }
  return issues
}

function upsertMailSyncIssue(issue) {
  if (!issue || !issue.id || !String(issue.message || "").trim()) return
  var existing = _mailSyncIssuesByAccount[issue.id] || {}
  var name = String(issue.name || "").trim()
  var email = String(issue.email || "").trim()
  _mailSyncIssuesByAccount[issue.id] = {
    id: issue.id,
    name: name || existing.name || _mailSyncAccountLabel(issue.id) || "Account",
    email: email || existing.email || _mailSyncAccountEmail(issue.id) || "",
    message: String(issue.message || "").trim(),
    failedAt: String(issue.failedAt || existing.failedAt || "").trim(),
  }
  if (_mailSyncIssueOrder.indexOf(issue.id) === -1) _mailSyncIssueOrder.push(issue.id)
}

function removeMailSyncIssue(accountID) {
  if (!accountID || !_mailSyncIssuesByAccount[accountID]) return
  delete _mailSyncIssuesByAccount[accountID]
  _mailSyncIssueOrder = _mailSyncIssueOrder.filter(function (id) { return id !== accountID })
}

function mailSyncIssueFromNode(node) {
  if (!node || !node.getAttribute) return null
  var message = String(node.getAttribute("data-account-sync-error-message") || "").trim()
  if (!message) return null
  var id = String(node.getAttribute("data-account-sync-error") || "").trim()
  if (!id) return null
  return {
    id: id,
    name: node.getAttribute("data-account-sync-error-name") || "",
    email: node.getAttribute("data-account-sync-error-email") || "",
    message: message,
    failedAt: node.getAttribute("data-account-sync-error-failed-at") || "",
  }
}

function syncMailSyncIssuesFromDOM(root, reset) {
  var scope = root && root.querySelectorAll ? root : document
  if (reset) {
    _mailSyncIssuesByAccount = Object.create(null)
    _mailSyncIssueOrder = []
  }

  var accountSections = []
  if (scope.matches && scope.matches("[data-sidebar-account]")) accountSections.push(scope)
  var sectionNodes = scope.querySelectorAll ? scope.querySelectorAll("[data-sidebar-account]") : []
  for (var i = 0; i < sectionNodes.length; i++) accountSections.push(sectionNodes[i])

  if (accountSections.length) {
    for (var j = 0; j < accountSections.length; j++) {
      var accountID = accountSections[j].getAttribute("data-sidebar-account") || ""
      var errorNode = accountSections[j].querySelector("[data-account-sync-error]")
      var issue = mailSyncIssueFromNode(errorNode)
      if (issue) upsertMailSyncIssue(issue)
      else if (accountID && accountID !== "__unified__") removeMailSyncIssue(accountID)
    }
    return
  }

  var nodes = []
  if (scope.matches && scope.matches("[data-account-sync-error]")) nodes.push(scope)
  var descendants = scope.querySelectorAll ? scope.querySelectorAll("[data-account-sync-error]") : []
  for (var k = 0; k < descendants.length; k++) nodes.push(descendants[k])
  for (var n = 0; n < nodes.length; n++) {
    var nodeIssue = mailSyncIssueFromNode(nodes[n])
    if (nodeIssue) upsertMailSyncIssue(nodeIssue)
  }
}

function applyMailSyncIssueStatus(data) {
  if (!data || !data.account_id) return
  var status = data.status || ""
  updateSettingsAccountSyncError(data)
  if (status === "error") {
    upsertMailSyncIssue({
      id: data.account_id,
      name: data.account_name || data.name || "",
      email: data.account_email || data.email || "",
      message: data.error || data.message || "Sync failed",
      failedAt: data.failed_at || data.email_sync_error_at || "",
    })
    updateMailSyncErrorIndicator()
    return
  }
  if (status === "ok" || status === "synced" || status === "complete") {
    removeMailSyncIssue(data.account_id)
    updateMailSyncErrorIndicator()
  }
}

function updateSettingsAccountSyncError(data) {
  var accountID = String((data && data.account_id) || "")
  if (!accountID) return
  var status = String(data.status || "")
  if (status !== "error" && status !== "ok" && status !== "synced" && status !== "complete") return

  var nodes = document.querySelectorAll("[data-settings-account-sync-error]")
  for (var i = 0; i < nodes.length; i++) {
    var node = nodes[i]
    if (node.getAttribute("data-settings-account-sync-error") !== accountID) continue
    if (status !== "error") {
      var popoverRoot = node.querySelector("[data-tui-popover-root]")
      if (popoverRoot && popoverRoot.id && window.tui && window.tui.popover) {
        window.tui.popover.close(popoverRoot.id)
      }
      node.hidden = true
      continue
    }

    var message = String(data.error || data.message || "Sync failed").trim()
    var failedAt = String(data.failed_at || data.email_sync_error_at || "").trim()
    var messageNode = node.querySelector("[data-settings-account-sync-error-message]")
    if (messageNode) messageNode.textContent = message

    var failedAtWrapper = node.querySelector("[data-settings-account-sync-error-at-wrapper]")
    var failedAtNode = node.querySelector("[data-account-sync-error-at]")
    if (failedAtWrapper) failedAtWrapper.hidden = !failedAt
    if (failedAtNode) {
      failedAtNode.setAttribute("data-account-sync-error-at", failedAt)
      var date = parseMailSyncUTCInstant(failedAt)
      failedAtNode.textContent = date ? formatGoferDateTime(date, {
        year: "numeric",
        month: "short",
        day: "numeric",
        hour: "numeric",
        minute: "2-digit",
        timeZoneName: "short",
      }) : failedAt
    }
    node.hidden = false
  }
}

function mailSyncErrorSummaryHTML(issues) {
  if (!issues.length) {
    return '<div class="text-xs text-sidebar-foreground/70">No current mail sync issues.</div>'
  }
  var html = '<div class="font-semibold">Mail sync issues</div><div class="space-y-2">'
  for (var i = 0; i < issues.length; i++) {
    var issue = issues[i]
    var name = issue.name || "Account"
    var email = issue.email || ""
    var message = issue.message || "Sync failed"
    var failedAt = issue.failedAt || ""
    var date = parseMailSyncUTCInstant(failedAt)
    var formattedAt = date ? formatGoferDateTime(date, {
      year: "numeric",
      month: "short",
      day: "numeric",
      hour: "numeric",
      minute: "2-digit",
      timeZoneName: "short",
    }) : ""
    html += '<div class="rounded-md border border-sidebar-border/70 bg-sidebar-accent/20 px-2 py-1.5">' +
      '<div class="truncate font-semibold">' + _escapeComposeHTML(name) + '</div>' +
      (email ? '<div class="truncate text-[11px] opacity-70">' + _escapeComposeHTML(email) + '</div>' : '') +
      (formattedAt ? '<div class="mt-1 opacity-85">Last failure: ' + _escapeComposeHTML(formattedAt) + '</div>' : '') +
      '<div class="mt-1 break-words opacity-90">' + _escapeComposeHTML(message) + '</div>' +
    '</div>'
  }
  return html + '</div>'
}

function updateMailSyncErrorIndicator(root) {
  if (root && root.querySelectorAll) syncMailSyncIssuesFromDOM(root, false)
  var indicator = document.querySelector("[data-mail-sync-error-indicator]")
  if (!indicator) return
  var issues = mailSyncIssueList()
  var count = issues.length
  indicator.hidden = count === 0
  indicator.setAttribute("aria-label", count + " mail sync issue" + (count === 1 ? "" : "s"))
  indicator.removeAttribute("title")
  var summary = document.querySelector("[data-mail-sync-error-summary]")
  if (summary) summary.innerHTML = mailSyncErrorSummaryHTML(issues)
}

function ensureMailSyncAccount(accountID, index, runID, mode) {
  accountID = accountID || "__unknown__"
  runID = String(runID || "").trim()
  mode = mode === "repair" ? "repair" : "sync"
  var accountKey = _mailSyncAccountKey(accountID, runID)
  var account = _mailSyncState.accounts[accountKey]
  if (!account) {
    account = {
      key: accountKey,
      id: accountID,
      runID: runID,
      runMode: mode,
      index: index || _mailSyncState.accountOrder.length + 1,
      label: _mailSyncAccountLabel(accountID),
      status: "queued",
      totalFolders: _mailSyncAccountFolderTotal(accountID),
      syncedFolders: 0,
      currentFolderLabel: "",
      folders: Object.create(null),
      folderOrder: [],
      error: "",
      excludedIdleFolders: 0,
    }
    _mailSyncState.accounts[accountKey] = account
    _mailSyncState.accountOrder.push(accountKey)
  } else if (index && (!account.index || index < account.index)) {
    account.index = index
  }
  if (runID && !account.runID) account.runID = runID
  account.runMode = mode
  return account
}

function applyMailSyncAccountPayload(account, data) {
  if (!account || !data) return
  if (_mailSyncHasField(data, "account_folders_total")) {
    account.totalFolders = _mailSyncCount(data.account_folders_total)
  }
  if (_mailSyncHasField(data, "account_folders_done")) {
    account.syncedFolders = _mailSyncCount(data.account_folders_done)
  }
  if (_mailSyncHasField(data, "idle_folders_excluded")) {
    account.excludedIdleFolders = _mailSyncCount(data.idle_folders_excluded)
  }
}

function updateMailSyncStateFromRun(phase, data, kind) {
  var runID = data && data.run_id ? data.run_id : ""
  var manualMerge = kind === "manual" && _mailSyncHasActiveManualRun()
  var shouldReset = !manualMerge && (phase === "started" || (kind === "scheduled" && _mailSyncState.kind !== "scheduled") || (runID && _mailSyncState.runID && _mailSyncState.runID !== runID))
  if (shouldReset) {
    resetMailSyncProgressState(runID, kind)
  } else if (runID && !_mailSyncState.runID) {
    _mailSyncState.runID = runID
  }
  var run = ensureMailSyncRun(data || {}, kind)
  run.active = phase !== "complete"
  run.status = phase === "complete" ? (data.status || "ok") : "syncing"
  run.total = _mailSyncCount(data.accounts_total) || run.total
  run.done = _mailSyncCount(data.accounts_done)
  run.parallelism = _mailSyncCount(data.parallelism) || run.parallelism
  run.failures = _mailSyncCount(data.failures)
  run.skipped = _mailSyncCount(data.skipped)
  run.cancelled = _mailSyncCount(data.cancelled)
  run.notDone = _mailSyncCount(data.not_done)
  if (phase === "complete") run.completedAt = Date.now()
  if (!_mailSyncState.startedAt) _mailSyncState.startedAt = Date.now()
  populateMailSyncRunAccounts(data)

  if (data.account_id) {
    var account = ensureMailSyncAccount(data.account_id, _mailSyncCount(data.account_index), runID, run.mode)
    account.status = data.status || account.status
    account.error = data.error || account.error || ""
    applyMailSyncAccountPayload(account, data)
    if (!account.totalFolders) account.totalFolders = _mailSyncAccountFolderTotal(data.account_id)
    if (account.status === "synced" && account.totalFolders) account.syncedFolders = account.totalFolders
  }
  updateMailSyncAggregateFromRuns()
  if (!_mailSyncState.active) _mailSyncState.completedAt = Date.now()
  renderMailSyncProgressDialog()
}

function updateMailSyncStateFromManual(phase, data) {
  updateMailSyncStateFromRun(phase, data, "manual")
}

function updateMailSyncStateFromScheduled(phase, data) {
  updateMailSyncStateFromRun(phase, data, "scheduled")
}

function updateMailSyncFolderProgress(phase, data) {
  if (!data || !data.account_id || !data.folder_id) return
  if (_mailSyncIsScheduledEvent(data) && _mailSyncHasActiveManualRun()) return
  if (!_mailSyncIsScheduledEvent(data) && !data.run_id && !_mailSyncHasActiveManualRun()) return
  var scheduledEvent = ensureMailScheduledRunFromEvent(data)
  if (!_mailSyncState.active) {
    _mailSyncActive = true
    resetMailSyncProgressState("", data.kind || "background")
    _setMailSyncButtonBusy(true)
  }
  if (data.kind && _mailSyncCanAdoptAccountKind()) _mailSyncState.kind = data.kind
  var account = ensureMailSyncAccount(data.account_id, _mailSyncCount(data.account_index), data.run_id || "", _mailSyncModeFromData(data))
  if (account.status === "queued") account.status = "syncing"
  if (data.account_name || data.account_email) account.label = data.account_name || data.account_email || account.label
  applyMailSyncAccountPayload(account, data)
  if (!_mailSyncHasField(data, "account_folders_total") && !account.totalFolders) account.totalFolders = _mailSyncAccountFolderTotal(data.account_id)
  var folder = account.folders[data.folder_id]
  if (!folder) {
    folder = {
      id: data.folder_id,
      label: data.current_folder || _mailSyncFolderLabel(data.folder_id, data.folder_role || ""),
      role: data.folder_role || "",
      status: "syncing",
      current: 0,
      total: 0,
      refreshOnly: !!data.refresh_only,
      totalEstimated: !!data.total_estimated,
      updatedAt: Date.now(),
    }
    account.folders[data.folder_id] = folder
    account.folderOrder.push(data.folder_id)
  }
  if (data.current_folder) folder.label = data.current_folder
  folder.status = phase === "complete" ? "complete" : "syncing"
  folder.current = _mailSyncCount(data.current)
  if (_mailSyncHasField(data, "total") && (phase !== "complete" || _mailSyncCount(data.total) > 0)) {
    folder.total = _mailSyncCount(data.total)
  }
  folder.refreshOnly = !!data.refresh_only
  folder.totalEstimated = !!data.total_estimated
  folder.updatedAt = Date.now()
  account.currentFolderLabel = folder.label
  if (!_mailSyncHasField(data, "account_folders_done")) account.syncedFolders = _mailSyncCompletedFolderCount(account)
  if (phase === "complete" && account.totalFolders > 0 && account.syncedFolders >= account.totalFolders) {
    account.status = "synced"
    account.currentFolderLabel = ""
  }
  if (phase === "complete" && !scheduledEvent && !_mailSyncRunID && !mailSyncHasActiveAccounts()) {
    _mailSyncActive = false
    _mailSyncState.active = false
    _mailSyncState.status = "ok"
    _mailSyncState.done = _mailSyncState.accountOrder.length
    _mailSyncState.total = _mailSyncState.accountOrder.length
    _mailSyncState.completedAt = Date.now()
    _setMailSyncButtonBusy(_mailSyncState.active)
  }
  renderMailSyncProgressDialog()
}

function handleAccountSyncStatus(data) {
  if (!data || !data.account_id) return
  applyMailSyncIssueStatus(data)
  if (_mailSyncIsScheduledEvent(data) && _mailSyncHasActiveManualRun()) return
  var status = data.status || ""
  if (!_mailSyncIsScheduledEvent(data) && !data.run_id && !_mailSyncHasActiveManualRun()) {
    if (status !== "syncing") {
      refreshSidebarAccountForSync(data.account_id)
      refreshActiveMailListAfterAccountSyncForSync(data)
      setTimeout(updateMailSyncErrorIndicator, 100)
    }
    return
  }
  var scheduledEvent = ensureMailScheduledRunFromEvent(data)
  if (status === "syncing") {
    if (!_mailSyncState.active) {
      _mailSyncActive = true
      resetMailSyncProgressState("", data.kind || "background")
    } else if (data.kind && _mailSyncCanAdoptAccountKind()) {
      _mailSyncState.kind = data.kind
    }
    var syncingAccount = ensureMailSyncAccount(data.account_id, _mailSyncCount(data.account_index), data.run_id || "", _mailSyncModeFromData(data))
    syncingAccount.status = "syncing"
    syncingAccount.error = ""
    applyMailSyncAccountPayload(syncingAccount, data)
    _setMailSyncButtonBusy(true)
    renderMailSyncProgressDialog()
    return
  }

  refreshSidebarAccountForSync(data.account_id)
  refreshActiveMailListAfterAccountSyncForSync(data)
  if (!_mailSyncState.active && !_mailSyncState.accounts[data.account_id]) {
    setTimeout(updateMailSyncErrorIndicator, 100)
    return
  }

  var account = ensureMailSyncAccount(data.account_id, _mailSyncCount(data.account_index), data.run_id || "", _mailSyncModeFromData(data))
  applyMailSyncAccountPayload(account, data)
  if (status === "error") {
    account.status = "error"
    account.error = data.error || account.error || "Sync failed"
    _mailSyncState.failures = Math.max(_mailSyncState.failures || 0, 1)
    var indicator = document.querySelector("[data-mail-sync-error-indicator]")
    if (indicator) indicator.hidden = false
  } else if (status === "ok") {
    account.status = "synced"
    account.error = ""
    if (account.totalFolders) account.syncedFolders = account.totalFolders
    account.currentFolderLabel = ""
  }

  if (_mailSyncRunID) {
    renderMailSyncProgressDialog()
    setTimeout(updateMailSyncErrorIndicator, 100)
    return
  }

  if (!scheduledEvent && !mailSyncHasActiveAccounts()) {
    _mailSyncActive = false
    _mailSyncState.active = false
    _mailSyncState.status = _mailSyncState.failures > 0 ? "partial" : "ok"
    _mailSyncState.done = _mailSyncState.accountOrder.length
    _mailSyncState.total = _mailSyncState.accountOrder.length
    _mailSyncState.completedAt = Date.now()
    if (!_mailSyncRunID) _setMailSyncButtonBusy(false)
  }
  renderMailSyncProgressDialog()
  setTimeout(updateMailSyncErrorIndicator, 100)
}

function refreshSidebarAccountForSync(accountID) {
  if (typeof window.goferRefreshSidebarAccount === "function") {
    window.goferRefreshSidebarAccount(accountID)
  }
}

function refreshActiveMailListAfterAccountSyncForSync(data) {
  if (typeof window.goferRefreshActiveMailListAfterAccountSync === "function") {
    window.goferRefreshActiveMailListAfterAccountSync(data)
  }
}

function openMailSyncProgressDialog() {
  var dialog = document.getElementById("mail-sync-progress-dialog")
  if (!dialog) return
  renderMailSyncProgressDialog()
  if (window.tui && window.tui.dialog) {
    window.tui.dialog.open("mail-sync-progress-dialog")
    return
  }
  var content = dialog.querySelector("[data-tui-dialog-content]")
  if (!content || content.open) return
  try { content.showModal() } catch (_) {}
}

function closeMailSyncProgressDialog() {
  var dialog = document.getElementById("mail-sync-progress-dialog")
  if (!dialog) return
  if (window.tui && window.tui.dialog) {
    window.tui.dialog.close("mail-sync-progress-dialog")
    return
  }
  var content = dialog.querySelector("[data-tui-dialog-content]")
  if (content && content.open) content.close()
}

function _mailSyncModeLabel(mode) {
  return mode === "repair" ? "Repairing Gmail" : "Regular sync"
}

function _mailSyncModeDescription(mode, total, done, active) {
  if (mode === "repair") {
    if (active) return total ? "Repairing " + done + " of " + total + " accounts" : "Repairing Gmail account"
    return total ? "Repaired " + done + " of " + total + " accounts" : "Repair complete"
  }
  if (active) return total ? "Syncing " + done + " of " + total + " accounts" : "Syncing available accounts"
  return total ? "Synced " + done + " of " + total + " accounts" : "Sync complete"
}

function _mailSyncRunTotalsForMode(mode) {
  var out = { total: 0, done: 0, active: false, failures: 0, skipped: 0, cancelled: 0 }
  var keys = _mailSyncState.runOrder || []
  for (var i = 0; i < keys.length; i++) {
    var run = _mailSyncState.runs[keys[i]]
    if (!run || _mailSyncRunMode(run) !== mode) continue
    out.total += _mailSyncCount(run.total)
    out.done += _mailSyncCount(run.done)
    out.failures += _mailSyncCount(run.failures)
    out.skipped += _mailSyncCount(run.skipped)
    out.cancelled += _mailSyncCount(run.cancelled)
    if (run.active) out.active = true
  }
  return out
}

function _mailSyncAccountCardHTML(account) {
  var accountPct = _mailSyncAccountProgressPercent(account)
  var folderMeta = _mailSyncAccountFolderMeta(account)
  return '<div class="rounded-lg border border-border bg-background/45 p-3">' +
    '<div class="flex items-center justify-between gap-3">' +
      '<div class="min-w-0"><div class="truncate text-sm font-semibold">' + _escapeComposeHTML(account.label) + '</div>' +
      '<div class="mt-1 truncate text-xs text-muted-foreground">' + _escapeComposeHTML(folderMeta) + '</div>' +
      (account.currentFolderLabel && account.status === "syncing" ? '<div class="mt-1 truncate text-xs text-muted-foreground">Current: ' + _escapeComposeHTML(account.currentFolderLabel) + '</div>' : '') +
      (account.error ? '<div class="mt-1 truncate text-xs text-destructive">' + _escapeComposeHTML(account.error) + '</div>' : '') + '</div>' +
      '<div class="shrink-0 text-xs font-medium ' + _mailSyncStatusClass(account.status) + '">' + _mailSyncStatusLabel(account.status) + '</div>' +
    '</div>' +
    '<div class="mt-2 h-1.5 overflow-hidden rounded-full bg-muted"><div class="h-full rounded-full bg-primary transition-all" style="width:' + accountPct + '%"></div></div>' +
  '</div>'
}

function renderMailSyncProgressDialog() {
  updateMailSyncSidebarTooltip()
  var dialog = document.getElementById("mail-sync-progress-dialog")
  if (!dialog) return
  var total = _mailSyncState.total || _mailSyncState.accountOrder.length
  var done = _mailSyncState.done || 0
  var pct = total > 0 ? Math.min(100, Math.round((done / total) * 100)) : (_mailSyncState.active ? 5 : 100)
  var subtitle = "Sync finished with issues"
  if (_mailSyncState.active) subtitle = "Sync is running"
  else if (_mailSyncState.status === "ok") subtitle = "Sync finished"
  else if (_mailSyncState.status === "cancelled") subtitle = "Sync cancelled"
  else if (_mailSyncState.status === "already-running") subtitle = "Mail sync is already running"
  else if (_mailSyncState.status === "error" && total === 0) subtitle = "Sync could not start"
  if (_mailSyncState.parallelism > 1 && _mailSyncState.active) subtitle += ", up to " + _mailSyncState.parallelism + " accounts at a time"
  var subtitleEl = dialog.querySelector("[data-mail-sync-dialog-subtitle]")
  var summaryEl = dialog.querySelector("[data-mail-sync-dialog-summary]")
  var percentEl = dialog.querySelector("[data-mail-sync-dialog-percent]")
  var barEl = dialog.querySelector("[data-mail-sync-dialog-bar]")
  var cancelButtons = dialog.querySelectorAll("[data-mail-sync-cancel]")
  if (subtitleEl) subtitleEl.textContent = subtitle
  if (summaryEl) {
    var idleExcluded = _mailSyncTotalExcludedIdleFolders()
    var summaryText = ""
    if (total) summaryText = done + " of " + total + " accounts checked"
    else if (_mailSyncState.active) summaryText = "Preparing account sync..."
    else if (_mailSyncState.status === "already-running") summaryText = "Another sync is already running."
    else if (_mailSyncState.status === "error") summaryText = "Could not start mail sync."
    else summaryText = "No account progress available."
    summaryEl.textContent = _mailSyncAppendIdleExclusion(summaryText, idleExcluded)
  }
  if (percentEl) percentEl.textContent = pct + "%"
  if (barEl) barEl.style.width = pct + "%"
  var canCancel = _mailSyncState.active && _mailSyncState.kind === "manual" && !!(_mailSyncRunID || _mailSyncState.runID)
  for (var cancelIndex = 0; cancelIndex < cancelButtons.length; cancelIndex++) {
    cancelButtons[cancelIndex].classList.toggle("hidden", !canCancel)
    cancelButtons[cancelIndex].disabled = _mailSyncCancelRequested
    cancelButtons[cancelIndex].textContent = _mailSyncCancelRequested ? "Cancelling..." : "Cancel"
  }

  var accountsEl = dialog.querySelector("[data-mail-sync-dialog-accounts]")
  if (!accountsEl) return
  var ids = _mailSyncState.accountOrder.slice().sort(function (a, b) {
    return (_mailSyncState.accounts[a].index || 0) - (_mailSyncState.accounts[b].index || 0)
  })
  if (!ids.length) {
    accountsEl.innerHTML = '<div class="rounded-lg border border-dashed border-border px-4 py-6 text-center text-sm text-muted-foreground">Waiting for account progress...</div>'
    return
  }
  var html = ""
  var modes = ["repair", "sync"]
  for (var modeIndex = 0; modeIndex < modes.length; modeIndex++) {
    var mode = modes[modeIndex]
    var sectionIDs = ids.filter(function (id) {
      var account = _mailSyncState.accounts[id]
      return account && (account.runMode || "sync") === mode
    })
    if (!sectionIDs.length) continue
    var totals = _mailSyncRunTotalsForMode(mode)
    var sectionPct = totals.total > 0 ? Math.min(100, Math.round((totals.done / totals.total) * 100)) : (totals.active ? 8 : 100)
    html += '<section class="space-y-2 rounded-lg border border-border/80 bg-background/25 p-3">' +
      '<div class="flex items-center justify-between gap-3">' +
        '<div class="min-w-0">' +
          '<div class="truncate text-xs font-semibold uppercase tracking-wide text-muted-foreground">' + _escapeComposeHTML(_mailSyncModeLabel(mode)) + '</div>' +
          '<div class="mt-0.5 truncate text-xs text-muted-foreground">' + _escapeComposeHTML(_mailSyncModeDescription(mode, totals.total, totals.done, totals.active)) + '</div>' +
        '</div>' +
        '<div class="shrink-0 text-xs font-medium text-muted-foreground">' + sectionPct + '%</div>' +
      '</div>' +
      '<div class="h-1.5 overflow-hidden rounded-full bg-muted"><div class="h-full rounded-full bg-primary transition-all" style="width:' + sectionPct + '%"></div></div>' +
      '<div class="space-y-2">'
    for (var i = 0; i < sectionIDs.length; i++) {
      html += _mailSyncAccountCardHTML(_mailSyncState.accounts[sectionIDs[i]])
    }
    html += '</div></section>'
  }
  accountsEl.innerHTML = html
}

var _mailSyncScopedBusyAccountID = ""

function _mailSyncButtonFromContext(context) {
  var node = context && context.target ? context.target : context
  if (!node || !node.closest) return null
  return node.closest("[data-mail-sidebar-sync-button], [data-mail-account-sync-button]")
}

function _mailSyncAccountIDFromButton(button) {
  if (!button || !button.hasAttribute("data-mail-account-sync-button")) return ""
  return String(button.getAttribute("data-mail-account-sync-button") || "").trim()
}

function _setMailSyncButtonNodeBusy(button, busy) {
  if (!button) return
  button.dataset.syncing = busy ? "true" : "false"
  var icon = button.querySelector("svg")
  if (icon) icon.classList.toggle("animate-spin", !!busy)
}

function _clearMailSyncButtons() {
  var buttons = document.querySelectorAll("[data-mail-sidebar-sync-button], [data-mail-account-sync-button]")
  for (var i = 0; i < buttons.length; i++) {
    _setMailSyncButtonNodeBusy(buttons[i], false)
  }
}

function _setMailSyncButtonBusy(busy, accountID) {
  var scopedAccountID = typeof accountID === "string" ? accountID : _mailSyncScopedBusyAccountID
  var buttons = document.querySelectorAll("[data-mail-sidebar-sync-button], [data-mail-account-sync-button]")
  for (var i = 0; i < buttons.length; i++) {
    var buttonAccountID = _mailSyncAccountIDFromButton(buttons[i])
    if (scopedAccountID && buttonAccountID !== scopedAccountID) {
      _setMailSyncButtonNodeBusy(buttons[i], false)
      continue
    }
    _setMailSyncButtonNodeBusy(buttons[i], busy)
  }
  updateMailSyncSidebarTooltip()
}

function handleMailSidebarSyncStart(context, mode) {
  if (context === "repair") {
    mode = "repair"
    context = null
  }
  mode = mode === "repair" ? "repair" : "sync"
  var button = _mailSyncButtonFromContext(context)
  _mailSyncScopedBusyAccountID = _mailSyncAccountIDFromButton(button)
  _mailSyncActive = true
  if (!_mailSyncState.active && !_mailSyncState.runOrder.length) {
    resetMailSyncProgressState("", "manual")
  }
  _clearMailSyncButtons()
  _setMailSyncButtonBusy(true, _mailSyncScopedBusyAccountID)
  showMailSyncToast({
    id: "mail-sync-toast",
    title: _mailSyncRunningTitle({ mode: mode }),
    description: mode === "repair" ? "Preparing Gmail repair..." : "Checking connected mailboxes...",
    variant: "info",
    icon: "spinner",
    position: "bottom-right",
    duration: 0,
    dismissible: false,
    cancelable: true,
  })
}

function handleMailSidebarSyncResult(event) {
  var xhr = event && event.detail ? event.detail.xhr : null
  var ok = !!(event && event.detail && event.detail.successful) && (!xhr || xhr.getResponseHeader("X-Gofer-Status") !== "error")
  var message = _goferResponseText(xhr, "Mail sync started.")
  var mode = xhr ? (xhr.getResponseHeader("X-Gofer-Mail-Sync-Mode") || "") : ""
  if (mode) _mailSyncState.mode = _mailSyncModeFromData({ mode: mode })
  if (!ok) {
    if (!_mailSyncState.active) {
      _mailSyncRunID = ""
      stopMailSyncProgressState("error")
    }
    _setMailSyncButtonBusy(_mailSyncState.active)
    if (!_mailSyncState.active) _mailSyncScopedBusyAccountID = ""
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
    if (!_mailSyncState.active) {
      _mailSyncRunID = ""
      stopMailSyncProgressState("already-running")
    }
    _setMailSyncButtonBusy(_mailSyncState.active)
    if (!_mailSyncState.active) _mailSyncScopedBusyAccountID = ""
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
  if (_mailSyncRunID) _mailSyncState.runID = _mailSyncRunID
  ensureMailSyncRun({ run_id: _mailSyncRunID, mode: mode }, "manual")
  updateMailSyncAggregateFromRuns()
  showMailSyncToast({
    id: "mail-sync-toast",
    title: _mailSyncRunningTitle({ mode: mode }),
    description: message,
    variant: "info",
    icon: "spinner",
    position: "bottom-right",
    duration: 0,
    dismissible: false,
    cancelable: true,
  })
}

function handleMailManualSyncEvent(phase, data) {
  if (!data) return
  var runID = data.run_id || ""
  if (!_mailSyncActive && phase !== "started" && !runID) return
  _mailSyncActive = true
  if (phase === "started" && _mailSyncCount(data.accounts_total) > 1) {
    _mailSyncScopedBusyAccountID = ""
  } else if (!_mailSyncScopedBusyAccountID) {
    _mailSyncScopedBusyAccountID = _mailSyncSingleAccountIDFromData(data)
  }
  _setMailSyncButtonBusy(true)
  updateMailSyncStateFromManual(phase, data)

  var total = _mailSyncCount(data.accounts_total)
  var done = _mailSyncCount(data.accounts_done)
  var index = _mailSyncCount(data.account_index)
  var parallelism = _mailSyncCount(data.parallelism)
  var failures = _mailSyncCount(data.failures)
  var skipped = _mailSyncCount(data.skipped)
  var cancelled = _mailSyncCount(data.cancelled)
  var notDone = _mailSyncCount(data.not_done)

  if (phase === "complete") {
    var status = data.status || "ok"
    var title = _mailSyncCompleteTitle(status, data)
    var variant = status === "error" ? "error" : (status === "partial" || status === "cancelled" ? "warning" : "success")
    var icon = status === "error" ? "error" : (status === "partial" || status === "cancelled" ? "warning" : "success")
    var repairMode = _mailSyncModeFromData(data) === "repair"
    var description = (repairMode ? "Repaired " : "Checked ") + _mailSyncAccountsLabel(total || done) + "."
    if (status === "cancelled" && total) description = "Stopped after " + (repairMode ? "repairing " : "checking ") + done + " of " + total + " accounts."
    else if (status === "cancelled") description = repairMode ? "Gmail repair was stopped." : "Mail sync was stopped."
    if (failures > 0 && skipped > 0) description += " " + failures + " failed, " + skipped + " already running."
    else if (failures > 0) description += " " + failures + " failed."
    else if (skipped > 0) description += " " + skipped + " already running."
    if (cancelled > 0) description += " " + cancelled + " cancelled."
    if (notDone > 0) description += " " + notDone + " did not finish."
    _mailSyncActive = false
    _mailSyncCancelRequested = false
    updateMailSyncAggregateFromRuns()
    _setMailSyncButtonBusy(_mailSyncState.active)
    if (!_mailSyncState.active) _mailSyncScopedBusyAccountID = ""
    if (_mailSyncState.active) {
      showMailSyncToast({
        id: "mail-sync-toast",
        title: _mailSyncRunningTitle(data),
        description: "Another mail activity is still running.",
        variant: "info",
        icon: "spinner",
        position: "bottom-right",
        duration: 0,
        dismissible: false,
        cancelable: true,
      })
      return
    }
    showMailSyncToast({
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

  var repairMode = _mailSyncModeFromData(data) === "repair"
  var message = (repairMode ? "Repairing " : "Checking ") + _mailSyncAccountsLabel(total || 1) + "..."
  if (phase === "started" && parallelism > 1 && total > 1) {
    message = (repairMode ? "Repairing " : "Checking ") + _mailSyncAccountsLabel(total) + ", up to " + parallelism + " at a time..."
  }
  if (phase === "progress") {
    if (data.status === "syncing" && index && total) message = (repairMode ? "Repairing account " : "Checking account ") + index + " of " + total + "..."
    else if (data.status === "skipped") message = "Account already syncing (" + done + " / " + total + ")."
    else if (data.status === "error") message = "Account failed (" + done + " / " + total + ")."
    else if (done && total) message = "Checked " + done + " of " + total + " accounts."
  }
  showMailSyncToast({
    id: "mail-sync-toast",
    title: _mailSyncCancelRequested ? (repairMode ? "Cancelling Gmail repair" : "Cancelling mail sync") : _mailSyncRunningTitle(data),
    description: _mailSyncCancelRequested ? "Stopping the foreground sync..." : message,
    variant: "info",
    icon: "spinner",
    position: "bottom-right",
    duration: 0,
    dismissible: false,
    cancelable: !_mailSyncCancelRequested,
  })
}

function handleMailScheduledSyncEvent(phase, data) {
  if (!data) return
  var runID = data.run_id || ""
  if (_mailSyncHasActiveManualRun()) return
  if (_mailSyncState.kind === "scheduled" && _mailSyncState.active && _mailSyncState.runID && runID && _mailSyncState.runID !== runID && phase !== "started") return

  _mailSyncScopedBusyAccountID = ""
  _mailSyncActive = phase !== "complete"
  _setMailSyncButtonBusy(phase !== "complete")
  updateMailSyncStateFromScheduled(phase, data)

  if (phase === "complete") {
    _mailSyncActive = false
    _mailSyncCancelRequested = false
    _setMailSyncButtonBusy(false)
    setTimeout(updateMailSyncErrorIndicator, 100)
  }
}

var _contactSidebarSyncActiveButton = null

function _contactSidebarSyncButtonFromContext(context) {
  var node = context && context.target ? context.target : context
  if (!node || !node.closest) return null
  return node.closest("[data-contact-sidebar-sync-button], [data-contact-account-sync-button]")
}

function _setContactSidebarSyncButtonBusy(button, busy) {
  if (!button) return
  button.dataset.syncing = busy ? "true" : "false"
  var icon = button.querySelector("svg")
  if (icon) icon.classList.toggle("animate-spin", !!busy)
}

function _clearContactSidebarSyncButtons() {
  var buttons = document.querySelectorAll("[data-contact-sidebar-sync-button], [data-contact-account-sync-button]")
  for (var i = 0; i < buttons.length; i++) {
    _setContactSidebarSyncButtonBusy(buttons[i], false)
  }
}

function _contactSidebarSyncButtonLabel(button) {
  if (!button || !button.hasAttribute("data-contact-account-sync-button")) return ""
  var label = button.querySelector(".font-medium")
  return label && label.textContent ? label.textContent.trim() : ""
}

// Keep HTMX's target nodes alive while swapping only their presentation.
function replaceAppPaneContents(root, source, preserveWidth) {
  var width = preserveWidth && root.style.width
  Array.from(root.attributes).forEach(function (attribute) { root.removeAttribute(attribute.name) })
  Array.from(source.attributes).forEach(function (attribute) { root.setAttribute(attribute.name, attribute.value) })
  root.replaceChildren.apply(root, Array.from(source.childNodes).map(function (child) { return child.cloneNode(true) }))
  if (width) root.style.width = width
}

var _calendarContentRequest = null
var _calendarContentPrevious = null

function calendarLoadingWeekCount(month) {
  var parts = /^(\d{4})-(\d{2})$/.exec(month || "")
  if (!parts) return null
  var year = Number(parts[1]), index = Number(parts[2]) - 1
  if (index < 0 || index > 11) return null
  var leading = (new Date(Date.UTC(year, index, 1)).getUTCDay() + 6) % 7
  return Math.ceil((leading + new Date(Date.UTC(year, index + 1, 0)).getUTCDate()) / 7)
}

function calendarLoadingPeriodLabel(view, date) {
  if (!/^\d{4}-\d{2}(?:-\d{2})?$/.test(date || "")) return ""
  var at = new Date(date.length === 7 ? date + "-01T12:00:00Z" : date + "T12:00:00Z")
  if (isNaN(at.getTime())) return ""
  function label(day, year) {
    return day.toLocaleDateString("en-US", { timeZone: "UTC", month: "short", day: "numeric", year: year ? "numeric" : undefined })
  }
  if (view !== "week") return at.toLocaleDateString("en-US", { timeZone: "UTC", month: "long", year: "numeric" })
  at.setUTCDate(at.getUTCDate() - (at.getUTCDay() + 6) % 7)
  var end = new Date(at.getTime())
  end.setUTCDate(end.getUTCDate() + 6)
  if (at.getUTCFullYear() !== end.getUTCFullYear()) return label(at, true) + " – " + label(end, true)
  if (at.getUTCMonth() !== end.getUTCMonth()) return label(at, false) + " – " + label(end, true)
  return label(at, false) + "–" + end.getUTCDate() + ", " + end.getUTCFullYear()
}

function showCalendarContentPending(event) {
  var detail = event.detail
  var trigger = detail && detail.elt
  var calendar = document.getElementById("calendar-main")
  if (!calendar || !trigger || !detail.target || detail.target.id !== "main-content" ||
      trigger.hasAttribute("data-calendar-auto-sync") || trigger.hasAttribute("data-calendar-sync-button") || trigger.hasAttribute("data-calendar-cache-refresh")) return
  // Keep the grid and agenda visible until the other view is ready. Briefly
  // clearing them makes even a fast view switch flash.
  if (trigger.hasAttribute("data-calendar-view-switch")) return
  var href = trigger.getAttribute("hx-get")
  if (!href || !href.startsWith("/calendar")) return
  var url = new URL(href, window.location.href)
  var view = url.searchParams.get("view") === "week" ? "week" : "month"
  var template = document.getElementById("calendar-loading-" + view)
  if (!template) return
  var content = template.content.cloneNode(true)
  var loading = content.querySelector("#calendar-main")
  var agenda = document.getElementById("mail-view")
  if (!_calendarContentPrevious) {
    _calendarContentPrevious = { calendar: calendar.cloneNode(true), agenda: agenda.cloneNode(true) }
    // Restoring a failed navigation must not retrigger an already-run load sync.
    _calendarContentPrevious.calendar.querySelectorAll("[data-calendar-auto-sync]").forEach(function (node) { node.remove() })
  }
  _calendarContentRequest = detail.xhr
  detail.xhr.goferCalendarContentPending = true
  // Capture the real outgoing grid before installing the empty loading grid.
  if (typeof detail.xhr.goferCalendarNavigationDirection === "number") {
    prepareCalendarNavigation({ detail: { xhr: detail.xhr, target: detail.target, shouldSwap: true } })
  }
  var monthGrid = loading.querySelector("[data-calendar-month-grid]")
  if (monthGrid) {
    var month = url.searchParams.get("month") || (url.searchParams.get("date") || calendar.dataset.calendarTodayDate).slice(0, 7)
    var count = calendarLoadingWeekCount(month)
    if (count) {
      var days = monthGrid.lastElementChild
      var cell = days.firstElementChild.cloneNode(true)
      days.replaceChildren.apply(days, Array.from({ length: count * 7 }, function () { return cell.cloneNode(true) }))
      days.style.gridTemplateRows = "repeat(" + count + ",minmax(0,1fr))"
    }
  }
  // Dates belong to the incoming response. Only known weekday labels stay
  // readable here; the loading grid must not show stale dates or placeholders.
  loading.querySelectorAll("[data-calendar-day]").forEach(function (day) {
    day.setAttribute("aria-hidden", "true")
    var number = day.querySelector("span.rounded-full")
    if (number) {
      number.className = "inline-flex size-6 items-center justify-center sm:size-7"
      number.replaceChildren()
    }
    day.querySelectorAll("span").forEach(function (span) { if (span.textContent === "Today") span.remove() })
  })
  var timezone = loading.querySelector("[data-calendar-week-timezone]")
  if (timezone) timezone.textContent = ""
  // Keep the actual header node: the Month/Week indicator continues its slide
  // even when the empty grid is installed synchronously in beforeRequest.
  var header = calendar.querySelector("header")
  var label = calendarLoadingPeriodLabel(view, url.searchParams.get("month") || url.searchParams.get("date") || calendar.dataset.calendarTodayDate)
  if (label) header.querySelector("h1").textContent = label
  loading.querySelector("header").remove()
  // Even reinserting the same header through replaceChildren cancels its CSS
  // transition. Leave it connected while replacing only the other children.
  Array.from(calendar.childNodes).forEach(function (child) { if (child !== header) child.remove() })
  calendar.append.apply(calendar, Array.from(loading.childNodes))
  calendar.setAttribute("data-calendar-loading", "")
  calendar.setAttribute("aria-busy", "true")
  Array.from(calendar.children).forEach(function (child) { if (child !== header && child.tagName !== "FOOTER") child.inert = true })
  replaceAppPaneContents(agenda, content.querySelector("#mail-view"), false)
  if (label) agenda.querySelector("[data-calendar-agenda-context]").textContent = label
  document.getElementById("app-shell").setAttribute("data-hx-history", "false")
  initializeCalendarViewport()
}

function handleCalendarContentResult(event) {
  var detail = event.detail
  if (!detail || detail.xhr !== _calendarContentRequest) return
  if (!detail.successful && _calendarContentPrevious) {
    var calendar = document.getElementById("calendar-main")
    if (calendar && calendar.hasAttribute("data-calendar-loading")) {
      replaceAppPaneContents(calendar, _calendarContentPrevious.calendar, false)
      replaceAppPaneContents(document.getElementById("mail-view"), _calendarContentPrevious.agenda, false)
      if (window.htmx) window.htmx.process(calendar)
      initializeCalendarDaySelection()
      if (event.type !== "htmx:sendAbort") showGoferToast({ title: "Could not load calendar", description: "Please try again.", variant: "error", icon: "error" })
    }
  }
  _calendarContentRequest = null
  _calendarContentPrevious = null
  document.getElementById("app-shell").removeAttribute("data-hx-history")
}

var _calendarSelectedDay = null
var _calendarViewSwitchRequest = null
var _calendarWeekScrollState = null
var _calendarWeekZoom = 0
var _calendarViewportObserver = null
var _calendarViewportPane = null
var _calendarLayoutFrame = null
var _calendarNavigationRequest = null
var _calendarNavigationTransition = null

function setCalendarViewSwitch(view) {
  var group = document.querySelector("[data-calendar-view-nav]")
  if (!group) return
  group.querySelectorAll("[data-calendar-view-switch]").forEach(function (link) {
    var active = link.dataset.calendarViewSwitch === view
    link.classList.toggle("text-foreground", active)
    link.classList.toggle("text-muted-foreground", !active)
    link.classList.toggle("hover:text-foreground", !active)
    link.setAttribute("aria-current", active ? "page" : "false")
  })
  var indicator = group.querySelector("[data-calendar-view-indicator]")
  if (indicator) indicator.style.transform = view === "week" ? "translateX(calc(100% + 2px))" : "translateX(0)"
}

document.body.addEventListener("htmx:beforeRequest", function (event) {
  var trigger = event.detail && event.detail.elt
  if (!trigger || !trigger.hasAttribute("data-calendar-view-switch")) return
  _calendarViewSwitchRequest = event.detail.xhr
  _calendarViewSwitchRequest.goferCalendarViewSwitch = trigger.dataset.calendarViewSwitch
  setCalendarViewSwitch(trigger.dataset.calendarViewSwitch)
})

function handleCalendarViewSwitchResult(event) {
  if (!event.detail || event.detail.xhr !== _calendarViewSwitchRequest) return
  if (event.detail.successful) return
  _calendarViewSwitchRequest = null
  var calendar = document.getElementById("calendar-main")
  if (calendar) setCalendarViewSwitch(calendar.dataset.calendarView)
}

document.body.addEventListener("htmx:afterRequest", handleCalendarViewSwitchResult)
document.body.addEventListener("htmx:sendAbort", handleCalendarViewSwitchResult)
document.body.addEventListener("htmx:beforeSwap", function (event) {
  var xhr = event.detail && event.detail.xhr
  if (!xhr || typeof xhr.goferCalendarViewSwitch !== "string") return
  if (xhr !== _calendarViewSwitchRequest) {
    event.detail.shouldSwap = false
    return
  }
  var indicator = document.querySelector("[data-calendar-view-indicator]")
  if (indicator) xhr.goferCalendarViewTransform = window.getComputedStyle(indicator).transform
})
document.body.addEventListener("htmx:afterSwap", function (event) {
  var xhr = event.detail && event.detail.xhr
  if (!xhr || xhr !== _calendarViewSwitchRequest) return
  // Continue the slide on the new tabs even when the response is immediate.
  var indicator = document.querySelector("[data-calendar-view-indicator]")
  if (indicator && xhr.goferCalendarViewTransform) {
    indicator.style.transition = "none"
    indicator.style.transform = xhr.goferCalendarViewTransform
    void indicator.offsetWidth
    indicator.style.transition = ""
  }
  _calendarViewSwitchRequest = null
  setCalendarViewSwitch(xhr.goferCalendarViewSwitch)
})

function _calendarAgendaEventMatchesDay(event, day) {
  if (event.calendarEventAllDay === "true") {
    return !!event.calendarEventStartDate && !!event.calendarEventEndDate &&
      event.calendarEventStartDate <= day.calendarDay && event.calendarEventEndDate > day.calendarDay
  }
  var start = Date.parse(event.calendarEventStart)
  var end = Date.parse(event.calendarEventEnd)
  var dayStart = Date.parse(day.calendarDayStart)
  var dayEnd = Date.parse(day.calendarDayEnd)
  return start < dayEnd && (end > dayStart || (start === end && start >= dayStart))
}

function _calendarAgendaEventIsUpcoming(event, today) {
  if (event.calendarEventAllDay === "true") {
    return !event.calendarEventEndDate || event.calendarEventEndDate > today
  }
  var end = Date.parse(event.calendarEventEnd)
  return isNaN(end) || end >= Date.now()
}

// Session overrides also protect optimistic choices from an older HTMX cache
// response. Persistence is per source and independent of provider sync.
var _calendarVisibility = new Map()

function _calendarSourceIsVisible(node) {
  var state = _calendarVisibility.get(node.dataset.calendarSourceId)
  return state ? state.visible : node.dataset.calendarSourceHidden !== "true"
}

function initializeCalendarVisibility() {
  document.querySelectorAll("[data-calendar-visibility]").forEach(function (input) {
    var state = _calendarVisibility.get(input.dataset.calendarVisibility)
    input.checked = _calendarSourceIsVisible(input)
    input.setAttribute("aria-busy", state && state.saving ? "true" : "false")
    var row = input.closest("[data-calendar-source-row]")
    var label = row && row.querySelector("[data-calendar-source-name]")
    if (label) label.classList.toggle("opacity-50", !input.checked)
    var status = row && row.querySelector("[data-calendar-visibility-status]")
    if (status) status.style.opacity = state && state.saving ? "1" : "0"
    var sync = row && row.querySelector("[data-calendar-source-sync]")
    if (sync) sync.style.visibility = state && state.saving ? "hidden" : ""
  })
  document.querySelectorAll("#calendar-main [data-calendar-week-all-day], #calendar-main [data-calendar-week-event]").forEach(function (node) {
    if (node.dataset.calendarPillExit) return
    node.hidden = !_calendarSourceIsVisible(node)
  })
}

function saveCalendarVisibility(sourceID, state, force) {
  if (state.saving || (!force && state.visible === state.confirmed)) return
  var visible = state.visible
  var revision = state.revision
  state.saving = true
  initializeCalendarVisibility()
  fetch("/api/calendar/sources/" + encodeURIComponent(sourceID) + "/visibility", {
    method: "POST",
    headers: { "Content-Type": "application/json", "Accept": "application/json" },
    body: JSON.stringify({ visible: visible }),
  }).then(function (response) {
    if (!response.ok) throw new Error("Could not save calendar visibility")
    state.confirmed = visible
    state.saving = false
    initializeCalendarVisibility()
    saveCalendarVisibility(sourceID, state)
  }).catch(function () {
    state.saving = false
    // A newer choice supersedes a failed earlier write. Send that choice even
    // if it matches the old confirmed value (the lost response may have saved).
    if (state.revision !== revision) {
      saveCalendarVisibility(sourceID, state, true)
      return
    }
    state.visible = state.confirmed
    initializeCalendarDaySelection()
    showGoferToast({ title: "Could not save calendar visibility", description: "Your previous choice has been restored. Please try again.", variant: "error", icon: "error", position: "bottom-right", duration: 6000, dismissible: true })
  })
}

document.addEventListener("change", function (event) {
  var input = event.target
  if (!input.matches || !input.matches("[data-calendar-visibility]") || input.disabled) return
  var sourceID = input.dataset.calendarVisibility
  var state = _calendarVisibility.get(sourceID)
  if (!state) {
    state = { visible: input.dataset.calendarSourceHidden !== "true", confirmed: input.dataset.calendarSourceHidden !== "true", saving: false, revision: 0 }
    _calendarVisibility.set(sourceID, state)
  }
  state.visible = input.checked
  state.revision++
  finishCalendarNavigationTransition()
  initializeCalendarDaySelection()
  saveCalendarVisibility(sourceID, state)
})

function initializeCalendarDaySelection() {
  rememberCalendarPillGeometry(document.getElementById("calendar-main"))
  initializeCalendarVisibility()
  var calendar = document.getElementById("calendar-main")
  var agenda = document.querySelector("[data-calendar-agenda]")
  if (!calendar || !agenda) {
    _calendarSelectedDay = null
    _calendarViewSwitchRequest = null
    return
  }
  if (calendar.hasAttribute("data-calendar-loading")) return
  setCalendarViewSwitch(_calendarViewSwitchRequest ? _calendarViewSwitchRequest.goferCalendarViewSwitch : calendar.dataset.calendarView)
  if (_calendarSelectedDay && _calendarSelectedDay.period !== calendar.dataset.calendarPeriod) _calendarSelectedDay = null
  var selected = _calendarSelectedDay && calendar.querySelector('[data-calendar-day="' + _calendarSelectedDay.date + '"]')
  if (!selected) _calendarSelectedDay = null
  calendar.querySelectorAll("[data-calendar-day]").forEach(function (day) {
    day.setAttribute("data-calendar-day-selected", day === selected ? "true" : "false")
    day.querySelectorAll("[data-calendar-select-day]").forEach(function (button) {
      button.setAttribute("aria-pressed", day === selected ? "true" : "false")
    })
  })
  var visible = 0
  agenda.querySelectorAll("[data-calendar-agenda-event]").forEach(function (event) {
    var show = _calendarSourceIsVisible(event) && (selected ? _calendarAgendaEventMatchesDay(event.dataset, selected.dataset) :
      _calendarAgendaEventIsUpcoming(event.dataset, calendar.dataset.calendarTodayDate))
    event.hidden = !show
    if (show) visible++
  })
  agenda.querySelector("#calendar-agenda-heading").textContent = selected ? selected.dataset.calendarDayTitle : "Upcoming"
  agenda.querySelector("[data-calendar-agenda-context]").textContent = selected ?
    visible + (visible === 1 ? " event" : " events") : calendar.dataset.calendarMonthLabel
  agenda.querySelector("[data-calendar-agenda-clear]").hidden = !selected
  agenda.querySelector("#calendar-agenda-list").hidden = visible === 0
  agenda.querySelector("[data-calendar-agenda-empty]").hidden = visible !== 0
  var sources = Array.from(document.querySelectorAll("[data-calendar-visibility]"))
  var allHidden = sources.length > 0 && sources.every(function (input) { return !_calendarSourceIsVisible(input) })
  agenda.querySelector("[data-calendar-agenda-empty-title]").textContent = allHidden ? "All calendars are hidden" : selected ? "No events for this day" : "No upcoming events"
  agenda.querySelector("[data-calendar-agenda-empty-detail]").textContent = allHidden ?
    "Show a calendar in the sidebar to see its events. Hidden calendars continue syncing." : selected ?
    "Choose another date to browse your cached calendar events." :
    "Events from Google, Microsoft, or CalDAV will appear here after your selected calendars are synchronized."
  agenda.querySelector("[data-calendar-agenda-providers]").hidden = !!selected || allHidden
  // Switch views around the selected date rather than an unrelated week.
  var focusDate = selected ? selected.dataset.calendarDay : calendar.dataset.calendarDate
  calendar.querySelectorAll("[data-calendar-view-switch]").forEach(function (link) {
    var url = link.dataset.calendarViewSwitch === "week" ?
      "/calendar?date=" + encodeURIComponent(focusDate) + "&view=week" : "/calendar?month=" + focusDate.slice(0, 7)
    if (link.getAttribute("hx-get") === url) return
    link.setAttribute("href", url)
    link.setAttribute("hx-get", url)
    link.setAttribute("hx-push-url", url)
    if (window.htmx) window.htmx.process(link)
  })
  // Desktop keeps the two panes; small screens use the agenda as the day view.
  var mainPane = calendar.closest("#mail-list")
  mainPane.classList.toggle("hidden", !!selected)
  mainPane.classList.toggle("flex", !selected)
  agenda.classList.toggle("hidden", !selected)
  agenda.classList.toggle("flex", !!selected)
  initializeCalendarViewport()
}

document.addEventListener("click", function (event) {
  var trigger = event.target.closest && event.target.closest("[data-calendar-select-day]")
  var calendar = document.getElementById("calendar-main")
  if (trigger && calendar) {
    var scroller = calendar.querySelector("[data-calendar-week-scroll]")
    if (scroller && scroller.clientHeight) _calendarWeekScrollState = { period: calendar.dataset.calendarPeriod, zoom: _calendarWeekZoom, top: scroller.scrollTop, left: scroller.scrollLeft }
    _calendarSelectedDay = { period: calendar.dataset.calendarPeriod, date: trigger.dataset.calendarSelectDay }
    initializeCalendarDaySelection()
    document.getElementById("calendar-agenda-list").scrollTop = 0
    if (window.matchMedia("(max-width: 1023px)").matches) document.getElementById("calendar-agenda-heading").focus({ preventScroll: true })
    return
  }
  if (calendar && event.target.closest && event.target.closest("[data-calendar-agenda-clear]")) {
    var previous = _calendarSelectedDay && calendar.querySelector('[data-calendar-select-day="' + _calendarSelectedDay.date + '"]')
    _calendarSelectedDay = null
    initializeCalendarDaySelection()
    if (previous) previous.focus({ preventScroll: true })
  }
})

document.body.addEventListener("htmx:afterSwap", initializeCalendarDaySelection)
if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", initializeCalendarDaySelection)
else initializeCalendarDaySelection()

// All seven days share this axis: empty hours compress together, so events
// at the same time still line up across columns. Zoom returns to uniform hours.
function _calendarWeekAxis(availableHeight, periods, zoom) {
  var busy = Array(24).fill(false)
  periods.forEach(function (period) {
    for (var hour = 0; hour < 24; hour++) {
      if (period.start < (hour + 1) * 60 && (period.end > hour * 60 ||
          (period.start === period.end && period.start >= hour * 60))) busy[hour] = true
    }
  })
  var hasEvents = busy.some(function (value) { return value })
  var expanded = busy.map(function (value, hour) { return value || (!hasEvents && hour >= 8 && hour < 18) })
  var height = Math.max(1, availableHeight)
  var heights
  if (zoom > 0) {
    height = Math.max(height * [1, 1.4, 2, 2.6][zoom], [0, 960, 1536, 2304][zoom])
    heights = Array(24).fill(height / 24)
  } else {
    var minimum = Math.min(4, height / 48)
    var weights = expanded.map(function (value) { return value ? 4 : 0.5 })
    var total = weights.reduce(function (sum, weight) { return sum + weight }, 0)
    heights = weights.map(function (weight) { return minimum + (height - minimum * 24) * weight / total })
  }
  var offsets = [0]
  heights.forEach(function (value) { offsets.push(offsets[offsets.length - 1] + value) })
  return { height: height, heights: heights, offsets: offsets, expanded: expanded, zoom: zoom }
}

function _calendarWeekMinutePosition(minute, axis) {
  minute = Math.max(0, Math.min(1440, minute))
  if (minute === 1440) return axis.height
  var hour = Math.floor(minute / 60)
  return axis.offsets[hour] + axis.heights[hour] * (minute % 60) / 60
}

function _calendarWeekMinuteAtPosition(position, axis) {
  position = Math.max(0, Math.min(axis.height, position))
  if (position === axis.height) return 1440
  for (var hour = 0; hour < 24; hour++) {
    if (position < axis.offsets[hour + 1]) return hour * 60 + (position - axis.offsets[hour]) * 60 / axis.heights[hour]
  }
  return 1440
}

function _calendarWeekBlockLayout(periods, axis) {
  var minimum = Math.min(22, axis.height)
  var blocks = periods.map(function (period, index) {
    var top = Math.min(_calendarWeekMinutePosition(period.start, axis), axis.height - minimum)
    var end = Math.min(axis.height, Math.max(_calendarWeekMinutePosition(period.end, axis), top + minimum))
    return { index: index, top: top, height: end - top, end: end, column: 0, columns: 1 }
  }).sort(function (left, right) { return left.top - right.top || right.end - left.end || left.index - right.index })
  for (var first = 0; first < blocks.length;) {
    var last = first, groupEnd = -1, columns = []
    while (last < blocks.length && (last === first || blocks[last].top < groupEnd - 0.01)) {
      var block = blocks[last]
      var column = 0
      while (column < columns.length && columns[column] > block.top + 0.01) column++
      columns[column] = block.end
      block.column = column
      groupEnd = Math.max(groupEnd, block.end)
      last++
    }
    for (var index = first; index < last; index++) blocks[index].columns = columns.length
    first = last
  }
  return blocks
}

function _calendarMonthVisibleCount(total, available, itemHeight, overflowHeight, gap) {
  var visible = Math.min(total, 3, Math.max(0, Math.floor((available + gap) / (itemHeight + gap))))
  if (total > visible) visible = Math.min(visible, Math.max(0, Math.floor((available - overflowHeight) / (itemHeight + gap))))
  return visible
}

function _calendarAllDayEventRows(days) {
  var spans = new Map(), rows = new Map(), ends = []
  days.forEach(function (day, index) {
    day.buttons.forEach(function (button) {
      var event = button.dataset
      var id = event.calendarMonthEvent || event.calendarWeekAllDay
      if (event.calendarEventAllDay !== "true" || !id ||
          !(Date.parse(event.calendarEventEndDate) - Date.parse(event.calendarEventStartDate) > 86400000)) return
      var span = spans.get(id)
      if (!span) { span = { first: index, last: index, buttons: [] }; spans.set(id, span) }
      span.last = index
      span.buttons.push(button)
    })
  })
  Array.from(spans.values()).sort(function (a, b) { return a.first - b.first || b.last - a.last }).forEach(function (span) {
    var row = 0
    while (row < ends.length && ends[row] >= span.first) row++
    ends[row] = span.last
    span.buttons.forEach(function (button) { rows.set(button, row) })
  })
  days.forEach(function (day) {
    var occupied = new Set(day.buttons.filter(function (button) { return rows.has(button) }).map(function (button) { return rows.get(button) }))
    day.buttons.forEach(function (button) {
      if (rows.has(button)) return
      var row = 0
      while (occupied.has(row)) row++
      occupied.add(row)
      rows.set(button, row)
    })
  })
  return rows
}

function layoutCalendarMonth(calendar) {
  var grid = calendar.querySelector("[data-calendar-month-grid]")
  if (!grid || grid.clientHeight === 0) return
  var weeks = new Map()
  grid.querySelectorAll("[data-calendar-month-events]").forEach(function (container) {
    var allButtons = Array.from(container.querySelectorAll("[data-calendar-month-event]"))
    var buttons = allButtons.filter(_calendarSourceIsVisible)
    var overflow = container.querySelector("[data-calendar-day-overflow]")
    if (!overflow) return
    var total = buttons.length
    var day = container.closest("[data-calendar-day]")
    day.dataset.calendarDayEventCount = String(total)
    allButtons.forEach(function (button) {
      button.hidden = true
      button.style.display = "none"
    })
    container.hidden = total === 0
    if (!total) { overflow.hidden = true; return }
    var key = day.dataset.calendarWeekStart || ""
    if (!weeks.has(key)) weeks.set(key, [])
    weeks.get(key).push({ container: container, buttons: buttons, overflow: overflow })
  })
  weeks.forEach(function (days) {
    var rows = _calendarAllDayEventRows(days), spanLimits = new Map()
    days.forEach(function (day) {
      var buttons = day.buttons, overflow = day.overflow
      var totalRows = Math.max.apply(null, buttons.map(function (button) { return rows.get(button) })) + 1
      // Measuring a visible candidate avoids stale zero heights after a resize.
      buttons[0].hidden = false
      buttons[0].style.display = ""
      buttons[0].style.gridRow = "1"
      overflow.hidden = false
      day.limit = _calendarMonthVisibleCount(totalRows, day.container.clientHeight, buttons[0].offsetHeight, overflow.offsetHeight, 4)
      buttons.forEach(function (button) {
        var event = button.dataset
        if (event.calendarEventAllDay !== "true" || !event.calendarMonthEvent) return
        var previous = spanLimits.get(event.calendarMonthEvent)
        spanLimits.set(event.calendarMonthEvent, previous === undefined ? day.limit : Math.min(previous, day.limit))
      })
    })
    days.forEach(function (day) {
      var visible = 0
      day.buttons.forEach(function (button) {
        var row = rows.get(button), limit = spanLimits.get(button.dataset.calendarMonthEvent)
        var show = row < day.limit && (limit === undefined || row < limit)
        button.hidden = !show
        button.style.display = show ? "" : "none"
        button.style.gridRow = String(row + 1)
        if (show) visible++
      })
      day.overflow.hidden = day.buttons.length <= visible
      day.overflow.textContent = "+" + (day.buttons.length - visible) + " more"
    })
  })
}

function layoutCalendarWeekAllDay(grid) {
  var days = Array.from(grid.querySelectorAll("[data-calendar-all-day-column]")).map(function (column) {
    var buttons = Array.from(column.querySelectorAll("[data-calendar-week-all-day]"))
    buttons.forEach(function (button) {
      button.hidden = !_calendarSourceIsVisible(button)
      button.style.display = button.hidden ? "none" : ""
    })
    return { buttons: buttons.filter(_calendarSourceIsVisible) }
  })
  var rows = _calendarAllDayEventRows(days)
  rows.forEach(function (row, button) { button.style.gridRow = String(row + 1) })
}

function initializeCalendarWeekScroll() {
  var calendar = document.getElementById("calendar-main")
  var scroller = calendar && calendar.querySelector("[data-calendar-week-scroll]")
  if (!scroller || scroller.clientHeight === 0) return
  // Keep day widths stable as zoom adds or removes the vertical scrollbar.
  scroller.style.scrollbarGutter = "stable"
  scroller.style.overflowAnchor = "none"
  var grid = scroller.querySelector("[data-calendar-week-grid]")
  var header = grid && grid.querySelector("[data-calendar-week-header]")
  var timeline = grid && grid.querySelector("[data-calendar-week-timeline]")
  if (!header || !timeline) return
  layoutCalendarWeekAllDay(grid)
  var nodes = Array.from(grid.querySelectorAll("[data-calendar-week-event]"))
  var visibility = nodes.map(function (node) { return _calendarSourceIsVisible(node) ? "1" : "0" }).join("")
  var key = [scroller.clientHeight, scroller.clientWidth, header.offsetHeight, _calendarWeekZoom, visibility].join(":")
  if (scroller.dataset.calendarLayoutKey === key) return
  cancelCalendarWeekZoom(scroller)
  var previousAxis = grid._calendarWeekAxis
  var previousTop = scroller.scrollTop
  var wasInitialized = !!scroller.dataset.calendarLayoutKey
  nodes = nodes.filter(_calendarSourceIsVisible)
  var periods = nodes.map(function (node) { return { start: Number(node.dataset.calendarWeekStart), end: Number(node.dataset.calendarWeekEnd) } })
  var axis = _calendarWeekAxis(Math.max(1, scroller.clientHeight - header.offsetHeight - 2), periods, _calendarWeekZoom)
  grid.style.height = (axis.height + header.offsetHeight + 2) + "px"
  grid._calendarWeekAxis = axis
  timeline.style.height = axis.height + "px"
  timeline.style.flex = "0 0 auto"
  grid.querySelectorAll("[data-calendar-week-hour]").forEach(function (row) {
    var hour = Number(row.dataset.calendarWeekHour)
    var compact = _calendarWeekZoom === 0 && !axis.expanded[hour]
    var first = hour === 0 || axis.expanded[hour - 1]
    row.style.height = axis.heights[hour] + "px"
    row.style.borderTopStyle = compact && !first ? "none" : "solid"
    row.querySelector("[data-calendar-half-hour]").hidden = compact || axis.heights[hour] < 28
  })
  grid.querySelectorAll("[data-calendar-week-hour-label]").forEach(function (row) {
    var hour = Number(row.dataset.calendarWeekHourLabel)
    var text = row.querySelector("[data-calendar-week-hour-text]")
    row.style.height = axis.heights[hour] + "px"
    text.hidden = false
    text.textContent = String(hour).padStart(2, "0") + ":00"
    text.title = ""
    if (_calendarWeekZoom === 0 && !axis.expanded[hour]) {
      var end = hour + 1
      while (end < 24 && !axis.expanded[end]) end++
      text.hidden = (hour > 0 && !axis.expanded[hour - 1]) || axis.offsets[end] - axis.offsets[hour] < 15
      text.textContent = String(hour).padStart(2, "0") + "–" + String(end).padStart(2, "0")
      text.title = "Empty hours condensed. Zoom in to expand."
    } else if (axis.heights[hour] < 15) text.hidden = hour % 2 !== 0
  })
  grid.querySelectorAll("[data-calendar-timed-column]").forEach(function (column) {
    var buttons = Array.from(column.querySelectorAll("[data-calendar-week-event]")).filter(_calendarSourceIsVisible)
    var columnPeriods = buttons.map(function (node) { return { start: Number(node.dataset.calendarWeekStart), end: Number(node.dataset.calendarWeekEnd) } })
    _calendarWeekBlockLayout(columnPeriods, axis).forEach(function (block) {
      var button = buttons[block.index]
      button.style.top = block.top + "px"
      button.style.height = block.height + "px"
      button.style.left = "calc(" + (block.column * 100 / block.columns) + "% + 2px)"
      button.style.width = "calc(" + (100 / block.columns) + "% - 4px)"
      var time = button.querySelector("[data-calendar-week-event-time]")
      if (time) time.hidden = block.height < 42
    })
  })
  scroller.dataset.calendarLayoutKey = key
  var saved = _calendarWeekScrollState && _calendarWeekScrollState.period === calendar.dataset.calendarPeriod ? _calendarWeekScrollState : null
  if (_calendarWeekZoom === 0) scroller.scrollTop = 0
  else if (!wasInitialized) scroller.scrollTop = saved && saved.zoom === _calendarWeekZoom ? saved.top : _calendarWeekMinutePosition(7 * 60, axis)
  else if (previousAxis && previousAxis.zoom !== _calendarWeekZoom) {
    var center = _calendarWeekMinuteAtPosition(previousTop + (scroller.clientHeight - header.offsetHeight - 2) / 2, previousAxis)
    scroller.scrollTop = Math.max(0, _calendarWeekMinutePosition(center, axis) - (scroller.clientHeight - header.offsetHeight - 2) / 2)
  } else if (previousAxis) scroller.scrollTop = previousTop * axis.height / previousAxis.height
  if (!wasInitialized) scroller.scrollLeft = saved ? saved.left : 0
  var status = calendar.querySelector("[data-calendar-week-scale-status]")
  if (status) status.textContent = _calendarWeekZoom === 0 ? "Fit · Empty hours condensed" : "Zoom · " + Math.round(axis.height / 24) + " px/hour"
  calendar.querySelectorAll("[data-calendar-week-zoom]").forEach(function (button) {
    var action = button.dataset.calendarWeekZoom
    button.disabled = action === "out" ? _calendarWeekZoom === 0 : action === "in" && _calendarWeekZoom === 3
    if (action === "fit") {
      button.setAttribute("aria-pressed", _calendarWeekZoom === 0 ? "true" : "false")
      button.classList.toggle("bg-accent", _calendarWeekZoom === 0)
    }
  })
}

function cancelCalendarWeekZoom(scroller) {
  var animation = scroller && scroller._calendarZoomAnimation
  if (!animation) return
  cancelAnimationFrame(animation.frame)
  scroller._calendarZoomAnimation = null
}

function setCalendarWeekZoom(zoom) {
  if (zoom === _calendarWeekZoom) return
  var calendar = document.getElementById("calendar-main")
  var scroller = calendar && calendar.querySelector("[data-calendar-week-scroll]")
  var grid = scroller && scroller.querySelector("[data-calendar-week-grid]")
  cancelCalendarWeekZoom(scroller)
  var fromAxis = grid && grid._calendarWeekAxis
  var animate = fromAxis && scroller.clientHeight > 0 &&
    !window.matchMedia("(prefers-reduced-motion: reduce)").matches
  var fromTop = scroller ? scroller.scrollTop : 0
  var frames = []
  if (animate) {
    grid.querySelectorAll("[data-calendar-week-timeline], [data-calendar-week-hour], [data-calendar-week-hour-label], [data-calendar-week-event]").forEach(function (element) {
      if (element.hasAttribute("data-calendar-week-event") && !_calendarSourceIsVisible(element)) return
      var properties = element.hasAttribute("data-calendar-week-event") ? ["top", "height", "left", "width"] : ["height"]
      frames.push({ element: element, properties: properties })
    })
    frames.push({ element: grid, properties: ["height"] })
    frames.forEach(function (frame) {
      var computed = window.getComputedStyle(frame.element)
      frame.from = frame.properties.map(function (property) { return parseFloat(computed[property]) })
    })
  }
  _calendarWeekZoom = zoom
  initializeCalendarViewport()
  if (!animate) return
  var toAxis = grid._calendarWeekAxis
  var toTop = scroller.scrollTop
  frames.forEach(function (frame) {
    var computed = window.getComputedStyle(frame.element)
    frame.to = frame.properties.map(function (property) { return parseFloat(computed[property]) })
    frame.styles = frame.properties.map(function (property) { return frame.element.style[property] })
  })
  var animation = { frame: null, started: null }
  scroller._calendarZoomAnimation = animation
  function render(progress) {
    function mix(from, to) { return from + (to - from) * progress }
    frames.forEach(function (frame) {
      frame.properties.forEach(function (property, index) {
        frame.element.style[property] = progress === 1 ? frame.styles[index] : mix(frame.from[index], frame.to[index]) + "px"
      })
    })
    // A second zoom starts from the visible axis, not the previous destination.
    grid._calendarWeekAxis = progress === 1 ? toAxis : {
      height: mix(fromAxis.height, toAxis.height),
      heights: fromAxis.heights.map(function (height, hour) { return mix(height, toAxis.heights[hour]) }),
      offsets: fromAxis.offsets.map(function (offset, hour) { return mix(offset, toAxis.offsets[hour]) }),
      expanded: toAxis.expanded,
      zoom: toAxis.zoom,
    }
    scroller.scrollTop = mix(fromTop, toTop)
  }
  function step(now) {
    if (scroller._calendarZoomAnimation !== animation) return
    if (!scroller.isConnected) { cancelCalendarWeekZoom(scroller); return }
    if (animation.started === null) animation.started = now
    var progress = Math.min(1, (now - animation.started) / 220)
    render(1 - Math.pow(1 - progress, 3))
    if (progress === 1) scroller._calendarZoomAnimation = null
    else animation.frame = requestAnimationFrame(step)
  }
  // Restore the starting geometry before the browser paints the new layout.
  render(0)
  animation.frame = requestAnimationFrame(step)
}

var _calendarPillSnapshot = null
var _calendarPillFades = new Map()

function captureCalendarPills(calendar) {
  var pills = new Map(), counts = new Map()
  var bounds = calendar.getBoundingClientRect()
  calendar.querySelectorAll("[data-calendar-month-event], [data-calendar-week-all-day], [data-calendar-week-event]").forEach(function (node) {
    if (node.dataset.calendarPillExit) return
    var kind = node.dataset.calendarMonthEvent !== undefined ? "month" : node.dataset.calendarWeekAllDay !== undefined ? "all-day" : "timed"
    var id = node.dataset.calendarMonthEvent || node.dataset.calendarWeekAllDay || node.dataset.calendarWeekEvent
    var base = JSON.stringify([kind, node.dataset.calendarSourceId, id])
    var index = counts.get(base) || 0
    counts.set(base, index + 1) // Count hidden segments too, keeping multi-day keys stable.
    if (node.hidden || node.style.display === "none" || !node.getClientRects().length) return
    var rect = node.getBoundingClientRect()
    var style = window.getComputedStyle(node)
    var scroll = node.closest("[data-calendar-week-scroll], [data-calendar-all-day-row]")
    var clip = scroll && scroll.getBoundingClientRect()
    var inset = clip ? [Math.max(0, clip.top - rect.top), Math.max(0, rect.right - clip.right),
      Math.max(0, rect.bottom - clip.bottom), Math.max(0, clip.left - rect.left)] : [0, 0, 0, 0]
    pills.set(base + ":" + index, { node: node, top: rect.top - bounds.top, left: rect.left - bounds.left,
      width: rect.width, height: rect.height, opacity: style.opacity, dayPadding: style.getPropertyValue("--calendar-day-padding"),
      clipPath: inset.some(function (value) { return value > 0 }) ? "inset(" + inset.map(function (value) { return value + "px" }).join(" ") + ")" : "none" })
  })
  return { calendar: calendar, period: calendar.dataset.calendarPeriod, view: calendar.dataset.calendarView, pills: pills }
}

function rememberCalendarPillGeometry(calendar) {
  if (!calendar || !_calendarPillSnapshot || _calendarPillSnapshot.calendar !== calendar) return
  var current = captureCalendarPills(calendar)
  // Preserve entries already hidden by another layout pass until reconciliation.
  current.pills.forEach(function (pill, key) {
    if (_calendarPillSnapshot.pills.has(key)) _calendarPillSnapshot.pills.set(key, pill)
  })
}

function finishCalendarPillFade(key) {
  var fade = _calendarPillFades.get(key)
  if (!fade) return
  _calendarPillFades.delete(key)
  fade.animation.cancel()
  if (fade.ghost) fade.node.remove()
}

function clearCalendarPillFades() {
  Array.from(_calendarPillFades.keys()).forEach(finishCalendarPillFade)
}

function fadeCalendarPill(key, node, from, to, ghost) {
  var fade = { node: node, ghost: ghost, animation: node.animate([{ opacity: from }, { opacity: to }], {
    duration: 300, easing: "ease", fill: "both", // Same easing as tabs, with a gentler pill fade.
  }) }
  _calendarPillFades.set(key, fade)
  fade.animation.onfinish = function () {
    if (_calendarPillFades.get(key) === fade) finishCalendarPillFade(key)
  }
}

function animateCalendarPills(calendar) {
  if (!calendar || calendar.hasAttribute("data-calendar-loading")) {
    clearCalendarPillFades()
    _calendarPillSnapshot = null
    return
  }
  var current = captureCalendarPills(calendar), previous = _calendarPillSnapshot
  var samePeriod = previous && previous.period === current.period && previous.view === current.view
  _calendarPillSnapshot = current
  if (!samePeriod) clearCalendarPillFades()
  if ((window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches) || !calendar.animate) {
    clearCalendarPillFades()
    return
  }
  // A cache refresh can replace the calendar during an exit fade. Keep its
  // inert copy on the current root until the same animation finishes.
  if (samePeriod) _calendarPillFades.forEach(function (fade) {
    if (fade.ghost && fade.node.parentElement !== calendar) calendar.appendChild(fade.node)
  })
  if (samePeriod) previous.pills.forEach(function (pill, key) {
    if (current.pills.has(key)) return
    var fade = _calendarPillFades.get(key)
    var opacity = fade ? window.getComputedStyle(fade.node).opacity || pill.opacity : pill.opacity
    finishCalendarPillFade(key)
    var ghost = pill.node.cloneNode(true)
    // Keep visual data attributes for theme/RSVP styles, but no IDs or HTMX
    // actions. The exit copy never participates in layout or user interaction.
    ;[ghost].concat(Array.from(ghost.querySelectorAll("*"))).forEach(function (node) {
      Array.from(node.attributes).forEach(function (attr) {
        if (attr.name === "id" || /^(?:data-)?hx-/.test(attr.name) || /^on/i.test(attr.name)) node.removeAttribute(attr.name)
      })
    })
    ghost.removeAttribute("data-calendar-event-hover")
    ghost.dataset.calendarPillExit = "true"
    ghost.hidden = false
    ghost.inert = true
    ghost.setAttribute("aria-hidden", "true")
    Object.assign(ghost.style, { position: "absolute", display: "flex", pointerEvents: "none", margin: "0",
      top: pill.top + "px", left: pill.left + "px", width: pill.width + "px", height: pill.height + "px", zIndex: "30", clipPath: pill.clipPath })
    ghost.style.setProperty("--calendar-day-padding", pill.dayPadding)
    calendar.appendChild(ghost)
    fadeCalendarPill(key, ghost, opacity, 0, true)
  })
  current.pills.forEach(function (pill, key) {
    var before = samePeriod && previous.pills.get(key)
    var fade = _calendarPillFades.get(key)
    if (before && (!fade || fade.node === pill.node)) return
    var opacity = fade ? window.getComputedStyle(fade.node).opacity || (before ? before.opacity : 0) : before ? before.opacity : 0
    finishCalendarPillFade(key)
    fadeCalendarPill(key, pill.node, opacity, pill.opacity, false)
  })
}

document.body.addEventListener("htmx:beforeSwap", function (event) {
  if (event.defaultPrevented || !event.detail || event.detail.shouldSwap === false) return
  rememberCalendarPillGeometry(document.getElementById("calendar-main"))
})

function initializeCalendarViewport() {
  var calendar = document.getElementById("calendar-main")
  rememberCalendarPillGeometry(calendar)
  var pane = calendar && calendar.closest("#mail-list")
  if (_calendarViewportPane !== pane) {
    if (_calendarViewportObserver) _calendarViewportObserver.disconnect()
    _calendarViewportPane = pane
    if (pane && window.ResizeObserver) {
      _calendarViewportObserver = new ResizeObserver(function () {
        if (_calendarLayoutFrame) cancelAnimationFrame(_calendarLayoutFrame)
        _calendarLayoutFrame = requestAnimationFrame(function () {
          _calendarLayoutFrame = null
          initializeCalendarViewport()
        })
      })
      _calendarViewportObserver.observe(pane)
      var scroller = pane.querySelector("[data-calendar-week-scroll]")
      if (scroller) _calendarViewportObserver.observe(scroller)
    }
  }
  if (!calendar) { animateCalendarPills(null); return }
  layoutCalendarMonth(calendar)
  initializeCalendarWeekScroll()
  animateCalendarPills(calendar)
}

document.addEventListener("scroll", function (event) {
  var scroller = event.target
  if (!scroller.hasAttribute || !scroller.hasAttribute("data-calendar-week-scroll") || scroller.clientHeight === 0) return
  var calendar = document.getElementById("calendar-main")
  if (calendar) _calendarWeekScrollState = { period: calendar.dataset.calendarPeriod, zoom: _calendarWeekZoom, top: scroller.scrollTop, left: scroller.scrollLeft }
}, true)
document.addEventListener("click", function (event) {
  var button = event.target.closest && event.target.closest("[data-calendar-week-zoom]")
  if (!button || button.disabled) return
  finishCalendarNavigationTransition()
  var action = button.dataset.calendarWeekZoom
  setCalendarWeekZoom(action === "fit" ? 0 : Math.max(0, Math.min(3, _calendarWeekZoom + (action === "in" ? 1 : -1))))
})
window.addEventListener("resize", initializeCalendarViewport)
document.body.addEventListener("htmx:afterSwap", initializeCalendarViewport)
if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", initializeCalendarViewport)
else initializeCalendarViewport()

function finishCalendarNavigationTransition() {
  var transition = _calendarNavigationTransition
  if (!transition) return
  _calendarNavigationTransition = null
  transition.incoming.cancel()
  transition.outgoing.cancel()
  transition.snapshot.remove()
}

function prepareCalendarNavigation(event) {
  var detail = event.detail
  var xhr = detail && detail.xhr
  if (!xhr) return
  if (typeof xhr.goferCalendarCachePeriod === "string" && !calendarCacheResponseCurrent(xhr)) { detail.shouldSwap = false; return }
  var navigation = typeof xhr.goferCalendarNavigationDirection === "number"
  if (navigation && xhr !== _calendarNavigationRequest) {
    detail.shouldSwap = false
    return
  }
  if (detail.shouldSwap === false || !detail.target || detail.target.id !== "main-content") return
  var active = _calendarNavigationTransition
  if (!navigation && active) {
    // A quick background refresh must not cut the period transition short.
    var incoming = window.getComputedStyle(active.surface)
    var outgoing = window.getComputedStyle(active.snapshot)
    xhr.goferCalendarNavigationSnapshot = {
      element: active.snapshot, top: active.snapshot.scrollTop, left: active.snapshot.scrollLeft,
      incoming: { opacity: incoming.opacity, transform: incoming.transform },
      outgoing: { opacity: outgoing.opacity, transform: outgoing.transform },
      crossover: active.crossover, bounds: active.bounds,
      distance: active.distance, duration: Math.max(0, active.duration - (active.incoming.currentTime || 0)),
    }
  }
  finishCalendarNavigationTransition()
  if (navigation && xhr.goferCalendarNavigationSnapshot) return
  if (!navigation || window.matchMedia("(prefers-reduced-motion: reduce)").matches) return
  var surface = document.querySelector("#calendar-main [data-calendar-surface]")
  if (!surface || !surface.clientHeight || !surface.animate) return
  var snapshot = surface.cloneNode(true)
  // The outgoing grid is only a visual layer, never a second set of live controls.
  var elements = [snapshot].concat(Array.from(snapshot.querySelectorAll("*")))
  elements.forEach(function (element) {
    Array.from(element.attributes).forEach(function (attribute) {
      if (attribute.name === "id" || /^(data-calendar-|(?:data-)?hx-)/.test(attribute.name)) element.removeAttribute(attribute.name)
    })
  })
  snapshot.inert = true
  snapshot.setAttribute("aria-hidden", "true")
  snapshot.setAttribute("data-calendar-navigation-overlay", "")
  xhr.goferCalendarNavigationSnapshot = {
    element: snapshot, top: surface.scrollTop, left: surface.scrollLeft,
    bounds: xhr.goferCalendarNavigationCrossover ? surface.getBoundingClientRect() : null,
  }
}

function animateCalendarNavigation(event) {
  var xhr = event.detail && event.detail.xhr
  if (!xhr || (typeof xhr.goferCalendarNavigationDirection === "number" && xhr !== _calendarNavigationRequest)) return
  if (xhr === _calendarNavigationRequest) _calendarNavigationRequest = null
  var saved = xhr.goferCalendarNavigationSnapshot
  if (!saved) return
  delete xhr.goferCalendarNavigationSnapshot
  var calendar = document.getElementById("calendar-main")
  var surface = calendar && calendar.querySelector("[data-calendar-surface]")
  if (!saved || !surface || !surface.animate || window.matchMedia("(prefers-reduced-motion: reduce)").matches) return
  var crossover = saved.crossover || xhr.goferCalendarNavigationCrossover
  var bounds = saved.bounds || surface.getBoundingClientRect()
  var parent = calendar.getBoundingClientRect()
  var snapshot = saved.element
  Object.assign(snapshot.style, {
    position: "absolute", top: (bounds.top - parent.top) + "px", left: (bounds.left - parent.left) + "px",
    width: bounds.width + "px", height: bounds.height + "px", margin: "0", overflow: "hidden",
    pointerEvents: "none", zIndex: "30",
  })
  // Dissolve the old view over a fully opaque new one, never through the page
  // background. Keep the old geometry so switching layouts cannot stretch it.
  if (crossover) snapshot.style.backgroundColor = "var(--color-card)"
  calendar.appendChild(snapshot)
  snapshot.scrollTop = saved.top
  snapshot.scrollLeft = saved.left
  var distance = crossover ? 0 : saved.distance || xhr.goferCalendarNavigationDirection * 12
  var timing = { duration: saved.duration === undefined ? (crossover ? 140 : 220) : saved.duration, easing: crossover ? "ease-out" : "cubic-bezier(0.22, 1, 0.36, 1)" }
  var incoming = crossover ? [{ opacity: 1 }, { opacity: 1 }] : [saved.incoming || { opacity: 0, transform: "translateX(" + distance + "px) scale(0.98)" }, { opacity: 1, transform: "translateX(0) scale(1)" }]
  var outgoing = crossover ? [{ opacity: saved.outgoing ? saved.outgoing.opacity : 1 }, { opacity: 0 }] : [saved.outgoing || { opacity: 1, transform: "translateX(0) scale(1)" }, { opacity: 0, transform: "translateX(" + -distance + "px) scale(0.98)" }]
  var transition = {
    snapshot: snapshot, surface: surface, distance: distance, duration: timing.duration,
    crossover: crossover, bounds: saved.bounds,
    incoming: surface.animate(incoming, timing),
    outgoing: snapshot.animate(outgoing, timing),
  }
  _calendarNavigationTransition = transition
  transition.incoming.onfinish = function () {
    if (_calendarNavigationTransition === transition) finishCalendarNavigationTransition()
  }
}

function configureCalendarNavigationRequest(event) {
  var detail = event.detail
  var trigger = detail && detail.elt
  var arrow = trigger && trigger.hasAttribute("data-calendar-navigate")
  var viewSwitch = trigger && trigger.hasAttribute("data-calendar-view-switch")
  var calendar = viewSwitch && document.getElementById("calendar-main")
  var unchanged = calendar && calendar.dataset.calendarView === trigger.dataset.calendarViewSwitch
  if ((!arrow && !viewSwitch) || unchanged) {
    if (detail && detail.target && detail.target.id === "main-content" && (!trigger || !trigger.hasAttribute("data-calendar-auto-sync"))) _calendarNavigationRequest = null
    return
  }
  _calendarNavigationRequest = detail.xhr
  detail.xhr.goferCalendarNavigationCrossover = viewSwitch
  if (viewSwitch) finishCalendarNavigationTransition()
  detail.xhr.goferCalendarNavigationDirection = arrow ? Number(trigger.dataset.calendarNavigate) : trigger.dataset.calendarViewSwitch === "week" ? 1 : -1
}
document.body.addEventListener("htmx:beforeRequest", configureCalendarNavigationRequest)
document.body.addEventListener("htmx:beforeRequest", showCalendarContentPending)
document.body.addEventListener("htmx:beforeSwap", function (event) {
  var xhr = event.detail && event.detail.xhr
  if (xhr && xhr.goferCalendarContentPending && xhr !== _calendarContentRequest) event.detail.shouldSwap = false
})
document.body.addEventListener("htmx:beforeSwap", prepareCalendarNavigation)
// Layout/scroll restoration listeners above run before the incoming grid fades in.
document.body.addEventListener("htmx:afterSwap", animateCalendarNavigation)
function handleCalendarNavigationResult(event) {
  if (!event.detail || event.detail.xhr !== _calendarNavigationRequest) return
  delete event.detail.xhr.goferCalendarNavigationSnapshot
  _calendarNavigationRequest = null
}
document.body.addEventListener("htmx:afterRequest", handleCalendarNavigationResult)
document.body.addEventListener("htmx:sendAbort", handleCalendarNavigationResult)
document.body.addEventListener("htmx:afterRequest", handleCalendarContentResult)
document.body.addEventListener("htmx:sendAbort", handleCalendarContentResult)
document.body.addEventListener("htmx:beforeHistorySave", finishCalendarNavigationTransition)
window.addEventListener("resize", finishCalendarNavigationTransition)

function updateCalendarEventHover(event) {
  if (event.pointerType === "touch") return
  var selector = "[data-calendar-month-event], [data-calendar-week-all-day], [data-calendar-week-event]"
  var segment = event.target && event.target.closest && event.target.closest(selector)
  var calendar = segment && segment.closest("#calendar-main")
  if (!calendar) return
  var related = event.relatedTarget && event.relatedTarget.closest && event.relatedTarget.closest(selector)
  if (segment === related) return // Moving between a pill's label and color dot.
  var active = event.type === "pointerover" ? segment : related
  if (active && (active.hidden || active.closest("#calendar-main") !== calendar)) active = null
  var id = active && (active.dataset.calendarMonthEvent || active.dataset.calendarWeekAllDay || active.dataset.calendarWeekEvent)
  var source = active && active.dataset.calendarSourceId
  // Compare data values directly: event IDs must never become CSS selectors.
  // Keep every segment, including those wrapping into another row, in sync.
  calendar.querySelectorAll(selector).forEach(function (pill) {
    var sameEvent = !!id && !pill.hidden && pill.dataset.calendarSourceId === source &&
      (pill.dataset.calendarMonthEvent || pill.dataset.calendarWeekAllDay || pill.dataset.calendarWeekEvent) === id
    pill.toggleAttribute("data-calendar-event-hover", sameEvent)
  })
}

document.addEventListener("pointerover", updateCalendarEventHover)
document.addEventListener("pointerout", updateCalendarEventHover)
document.addEventListener("pointercancel", updateCalendarEventHover)
window.addEventListener("blur", function () {
  document.querySelectorAll("#calendar-main [data-calendar-event-hover]").forEach(function (pill) {
    pill.removeAttribute("data-calendar-event-hover")
  })
})

var _calendarEventRequest = null

function calendarEventRequestCurrent(xhr) {
  var calendar = document.getElementById("calendar-main")
  var dialog = xhr.goferCalendarEventDialog
  return xhr === _calendarEventRequest && calendar && !calendar.hasAttribute("data-calendar-loading") &&
    calendar.dataset.calendarPeriod === xhr.goferCalendarEventPeriod &&
    _calendarSourceIsVisible({dataset: {calendarSourceId: xhr.goferCalendarEventSource}}) &&
    (!dialog || (dialog.isConnected && dialog.open && !dialog.hasAttribute("data-tui-dialog-closing")))
}

function clearCalendarEventLoading(xhr, removeDialog) {
  if (!xhr) return
  clearTimeout(xhr.goferCalendarEventTimer)
  delete xhr.goferCalendarEventTimer
  var dialog = xhr.goferCalendarEventDialog
  if (removeDialog && dialog && dialog.isConnected && !xhr.goferCalendarEventSwapped) {
    if (dialog.open) dialog.close()
    xhr.goferCalendarEventRoot.remove()
  }
}

function showCalendarEventPending(xhr, trigger) {
  if (!calendarEventRequestCurrent(xhr) || !window.tui || !window.tui.dialog) return
  var template = document.getElementById("calendar-event-loading")
  var target = document.getElementById("app-pane-dialogs")
  if (!template || !target) return
  target.replaceChildren(template.content.cloneNode(true))
  var root = target.querySelector("#calendar-event-details-dialog")
  var dialog = root && root.querySelector("[data-tui-dialog-content]")
  if (!dialog) return
  var summary = trigger.querySelector("[data-calendar-event-text]") || trigger.querySelector("[data-calendar-event-summary]")
  if (summary) dialog.querySelector("#calendar-event-details-title").textContent = summary.textContent
  xhr.goferCalendarEventRoot = root
  xhr.goferCalendarEventDialog = dialog
  function cancelPending() {
    if (xhr !== _calendarEventRequest || xhr.goferCalendarEventSwapped) return
    _calendarEventRequest = null
    clearCalendarEventLoading(xhr, false)
    if (window.htmx) window.htmx.trigger(trigger, "htmx:abort")
  }
  dialog.addEventListener("cancel", cancelPending)
  dialog.addEventListener("close", cancelPending)
  dialog.addEventListener("click", function (event) {
    if (event.target === dialog || event.target.closest("[data-tui-dialog-close]")) cancelPending()
  })
  window.tui.dialog.open("calendar-event-details-dialog")
}

document.body.addEventListener("htmx:beforeRequest", function (event) {
  var trigger = event.detail && event.detail.elt
  if (!trigger || !trigger.hasAttribute("data-calendar-event-trigger")) return
  clearCalendarEventLoading(_calendarEventRequest, true)
  var calendar = document.getElementById("calendar-main")
  var xhr = _calendarEventRequest = event.detail.xhr
  xhr.goferCalendarEventPeriod = calendar ? calendar.dataset.calendarPeriod : ""
  xhr.goferCalendarEventSource = trigger.dataset.calendarSourceId
  // Fast requests open the complete dialog with no loading flash.
  xhr.goferCalendarEventTimer = setTimeout(function () { showCalendarEventPending(xhr, trigger) }, 250)
  trigger.setAttribute("aria-busy", "true")
})

document.body.addEventListener("htmx:beforeSwap", function (event) {
  var xhr = event.detail && event.detail.xhr
  if (!xhr || typeof xhr.goferCalendarEventPeriod !== "string") return
  clearCalendarEventLoading(xhr, false)
  if (!calendarEventRequestCurrent(xhr)) {
    event.detail.shouldSwap = false
    clearCalendarEventLoading(xhr, true)
    return
  }
  if (!event.detail.shouldSwap || event.detail.isError || !xhr.goferCalendarEventDialog) return
  var incoming = new DOMParser().parseFromString(event.detail.serverResponse, "text/html").querySelector("#calendar-event-details-dialog [data-tui-dialog-content] > [data-tui-dialog-panel]")
  if (!incoming) { event.detail.shouldSwap = false; return }
  // Keep the open modal in the top layer while its loading panel is filled.
  event.detail.target = xhr.goferCalendarEventDialog
  event.detail.selectOverride = "#calendar-event-details-dialog [data-tui-dialog-content] > [data-tui-dialog-panel]"
  event.detail.swapOverride = "innerHTML"
})

document.body.addEventListener("htmx:afterSwap", function (event) {
  var xhr = event.detail && event.detail.xhr
  if (!xhr || typeof xhr.goferCalendarEventPeriod !== "string" || !calendarEventRequestCurrent(xhr)) return
  xhr.goferCalendarEventSwapped = true
  if (xhr.goferCalendarEventDialog) xhr.goferCalendarEventDialog.removeAttribute("aria-busy")
})

document.body.addEventListener("htmx:sendAbort", function (event) {
  var xhr = event.detail && event.detail.xhr
  if (!xhr || typeof xhr.goferCalendarEventPeriod !== "string") return
  if (event.detail.elt) event.detail.elt.removeAttribute("aria-busy")
  clearCalendarEventLoading(xhr, true)
  if (xhr === _calendarEventRequest) _calendarEventRequest = null
})

document.body.addEventListener("htmx:afterRequest", function (event) {
  var xhr = event.detail && event.detail.xhr
  if (!xhr || typeof xhr.goferCalendarEventPeriod !== "string") return
  var trigger = event.detail.elt
  if (trigger) trigger.removeAttribute("aria-busy")
  clearCalendarEventLoading(xhr, false)
  if (xhr !== _calendarEventRequest) return
  var current = calendarEventRequestCurrent(xhr)
  _calendarEventRequest = null
  if (!current) { clearCalendarEventLoading(xhr, true); return }
  if (event.detail.successful && xhr.goferCalendarEventSwapped) {
    if (!xhr.goferCalendarEventDialog && window.tui && window.tui.dialog) window.tui.dialog.open("calendar-event-details-dialog")
    return
  }
  clearCalendarEventLoading(xhr, true)
  showGoferToast({
    title: "Could not open event",
    description: xhr.status === 404 ? "This event is no longer available. Refresh calendars and try again." : "Event details could not be loaded. Please try again.",
    variant: "error", icon: "error", position: "bottom-right", duration: 6000, dismissible: true,
  })
})

var _calendarCreateDialogRequest = null

function configureCalendarCreateDialog(event) {
  if (event.detail.elt && event.detail.elt.hasAttribute("data-calendar-edit-trigger")) return
  var calendar = document.getElementById("calendar-main")
  if (!calendar) return
  var selected = _calendarSelectedDay && _calendarSelectedDay.period === calendar.dataset.calendarPeriod ? _calendarSelectedDay.date : ""
  event.detail.parameters.date = selected || (calendar.dataset.calendarView === "week" ? calendar.dataset.calendarDate : calendar.dataset.calendarTodayDate.slice(0, 7) === calendar.dataset.calendarMonth ? calendar.dataset.calendarTodayDate : calendar.dataset.calendarMonth + "-01")
}

document.body.addEventListener("htmx:configRequest", function (event) {
  if (event.detail && event.detail.elt && event.detail.elt.hasAttribute("data-calendar-create-trigger")) configureCalendarCreateDialog(event)
})

document.body.addEventListener("htmx:beforeRequest", function (event) {
  var trigger = event.detail && event.detail.elt
  if (!trigger || !trigger.hasAttribute("data-calendar-create-trigger")) return
  _calendarCreateDialogRequest = event.detail.xhr
  var calendar = document.getElementById("calendar-main")
  _calendarCreateDialogRequest.goferCalendarCreatePeriod = calendar ? calendar.dataset.calendarPeriod : ""
  _calendarCreateDialogRequest.goferCalendarEdit = trigger.hasAttribute("data-calendar-edit-trigger")
  if (_calendarCreateDialogRequest.goferCalendarEdit && window.tui && window.tui.popover) window.tui.popover.closeElement(trigger)
  var details = _calendarCreateDialogRequest.goferCalendarEdit && document.getElementById("calendar-event-details-dialog")
  _calendarCreateDialogRequest.goferCalendarEditDialog = details && details.querySelector("[data-tui-dialog-content]")
  trigger.setAttribute("aria-busy", "true")
})

function calendarCreateDialogResponseCurrent(xhr) {
  var calendar = document.getElementById("calendar-main")
  var details = xhr.goferCalendarEditDialog
  if (details && (!details.isConnected || !details.open || details.hasAttribute("data-tui-dialog-closing"))) return false
  return xhr === _calendarCreateDialogRequest && calendar && !calendar.hasAttribute("data-calendar-loading") && calendar.dataset.calendarPeriod === xhr.goferCalendarCreatePeriod
}

function transitionCalendarEditDialog(details, editor, from) {
  // Lay out the editor at its final size once. Disable templUI's transition-all
  // while measuring so neither its width nor its entrance scale is in flight.
  var transition = details.style.transitionProperty
  details.style.transitionProperty = "none"
  details.closest("[data-tui-dialog]").id = "calendar-create-dialog"
  details.className = editor.className
  details.removeAttribute("id")
  details.setAttribute("aria-labelledby", editor.getAttribute("aria-labelledby"))
  initializeCalendarCreateForm()
  var to = details.getBoundingClientRect()
  details.style.transitionProperty = transition
  if (!from || !details.animate || window.matchMedia("(prefers-reduced-motion: reduce)").matches) return
  if (from.width <= 0 || from.height <= 0 || to.width <= 0 || to.height <= 0) return
  if (Math.abs(from.width - to.width) < 0.5 && Math.abs(from.height - to.height) < 0.5) return
  var panel = details.querySelector("[data-tui-dialog-panel]")
  var opacity = panel && panel.style.opacity
  var inert = panel && panel.inert
  if (panel) {
    // Hide the entire content layer, including footer buttons. Visibility is
    // inherited and templUI's transition-all buttons can delay that change;
    // parent opacity cannot be overridden or delayed by a descendant.
    panel.style.opacity = "0"
    panel.inert = true
  }
  // FLIP the empty frame from its old visual bounds to the already-laid-out
  // editor. Scale stays on the compositor; width/height would reflow the entire
  // form on every frame even with visibility:hidden. Individual scale also
  // preserves templUI's separate translate used to center the native dialog.
  var willChange = details.style.willChange
  details.style.willChange = "scale"
  var resize = details.animate([
    { scale: (from.width / to.width) + " " + (from.height / to.height) },
    { scale: "1 1" },
  ], { duration: 220, easing: "ease" }) // Match the templUI tabs indicator cadence.
  function focusSummary() {
    if (!details.isConnected || !details.open || details.hasAttribute("data-tui-dialog-closing") || (panel && (!panel.isConnected || panel.inert))) return
    var input = details.querySelector("#calendar-create-summary")
    if (input) input.focus({preventScroll: true})
  }
  function revealContent(fade) {
    details.style.willChange = willChange
    if (!panel) { focusSummary(); return }
    panel.style.opacity = opacity
    panel.inert = inert
    if (!panel.isConnected || !details.isConnected || !details.open || details.hasAttribute("data-tui-dialog-closing")) return
    if (fade && panel.animate) {
      var reveal = panel.animate([{ opacity: 0 }, { opacity: opacity || "1" }], { duration: 160, easing: "ease" })
      reveal.onfinish = focusSummary
    } else focusSummary()
  }
  resize.onfinish = function () { revealContent(true) }
  resize.oncancel = function () { revealContent(false) }
  return resize
}

document.body.addEventListener("htmx:beforeSwap", function (event) {
  var xhr = event.detail && event.detail.xhr
  if (!xhr || typeof xhr.goferCalendarCreatePeriod !== "string") return
  if (!calendarCreateDialogResponseCurrent(xhr)) { event.detail.shouldSwap = false; return }
  if (!event.detail.shouldSwap || event.detail.isError) return
  var details = xhr.goferCalendarEditDialog
  if (details) {
    var editor = new DOMParser().parseFromString(event.detail.serverResponse, "text/html").querySelector("#calendar-create-dialog [data-tui-dialog-content]")
    if (!editor || !editor.querySelector("[data-tui-dialog-panel]")) { event.detail.shouldSwap = false; return }
    // Swap only the panel: keep the native modal in the top layer so its backdrop
    // never closes or replays its entrance transition during the handoff.
    xhr.goferCalendarEditContent = editor
    xhr.goferCalendarEditBounds = details.getBoundingClientRect()
    event.detail.target = details
    event.detail.selectOverride = "#calendar-create-dialog [data-tui-dialog-content] > [data-tui-dialog-panel]"
    event.detail.swapOverride = "innerHTML"
  }
})

document.body.addEventListener("htmx:afterSwap", function (event) {
  var xhr = event.detail && event.detail.xhr
  if (!xhr || typeof xhr.goferCalendarCreatePeriod !== "string" || !calendarCreateDialogResponseCurrent(xhr)) return
  var details = xhr.goferCalendarEditDialog
  var editor = xhr.goferCalendarEditContent
  if (details && editor) xhr.goferCalendarEditAnimation = transitionCalendarEditDialog(details, editor, xhr.goferCalendarEditBounds)
  xhr.goferCalendarCreateSwapped = true
})

document.body.addEventListener("htmx:sendAbort", function (event) {
  var xhr = event.detail && event.detail.xhr
  if (!xhr || typeof xhr.goferCalendarCreatePeriod !== "string") return
  if (event.detail.elt) event.detail.elt.removeAttribute("aria-busy")
  if (xhr === _calendarCreateDialogRequest) _calendarCreateDialogRequest = null
})

document.body.addEventListener("htmx:afterRequest", function (event) {
  var xhr = event.detail && event.detail.xhr
  if (!xhr || typeof xhr.goferCalendarCreatePeriod !== "string") return
  if (event.detail.elt) event.detail.elt.removeAttribute("aria-busy")
  if (xhr !== _calendarCreateDialogRequest) return
  var current = calendarCreateDialogResponseCurrent(xhr)
  _calendarCreateDialogRequest = null
  if (!current) return
  if (!event.detail.successful) {
    showGoferToast({title: xhr.goferCalendarEdit ? "Could not open Edit event" : "Could not open New event", description: "Please try again.", variant: "error", icon: "error", duration: 5000})
    return
  }
  if (!xhr.goferCalendarCreateSwapped) return
  if (!xhr.goferCalendarEditDialog) {
    initializeCalendarCreateForm()
    if (window.tui && window.tui.dialog) window.tui.dialog.open("calendar-create-dialog")
  }
  var input = document.getElementById("calendar-create-summary")
  if (input && !xhr.goferCalendarEditAnimation) setTimeout(function () { if (input.isConnected) input.focus() }, 80)
})

function adjustCalendarCreateAllDayRange(form) {
  if (!form || form._calendarCreateBusy || form._calendarCreateUncertain || form._calendarCreateConflict || form._calendarCreateAdjustingDates) return
  var allDay = form.querySelector('[name="all_day"]')
  var start = form.querySelector('[name="start_date"]')
  var end = form.querySelector('[name="end_date"]')
  if (!allDay.checked) { form._calendarCreateDateAdjusted = ""; return }
  if (!start || !end || !/^\d{4}-\d{2}-\d{2}$/.test(start.value) || !/^\d{4}-\d{2}-\d{2}$/.test(end.value)) return
  if (end.value >= start.value) {
    if (start.value !== form._calendarCreateDateAdjusted || end.value !== start.value) form._calendarCreateDateAdjusted = ""
    return
  }
  form._calendarCreateAdjustingDates = true
  form._calendarCreateDateAdjusted = start.value
  try {
    end.value = start.value
    // Keep the popup's highlighted day and viewed month in sync with the
    // authoritative hidden input, not just the date displayed on its trigger.
    var root = end.closest("[data-tui-datepicker-root]")
    var calendar = root && root.querySelector("[data-tui-calendar-container]")
    if (calendar) {
      calendar.setAttribute("data-tui-calendar-selected-date", start.value)
      calendar.dataset.tuiCalendarCurrentYear = String(Number(start.value.slice(0, 4)))
      calendar.dataset.tuiCalendarCurrentMonth = String(Number(start.value.slice(5, 7)) - 1)
      var year = calendar.querySelector("[data-tui-calendar-year-select]")
      var month = calendar.querySelector("[data-tui-calendar-month-select]")
      if (year) year.value = calendar.dataset.tuiCalendarCurrentYear
      if (month) {
        month.value = calendar.dataset.tuiCalendarCurrentMonth
        month.dispatchEvent(new Event("change", {bubbles: true}))
      }
    }
  } finally { form._calendarCreateAdjustingDates = false }
}

function updateCalendarRecurrenceForm(form, locked) {
  var frequency = form.querySelector('[name="repeat_frequency"]')
  if (!frequency) return
  var options = form.querySelector("[data-calendar-repeat-options]")
  var repeating = frequency.value !== "none"
  var row = form.querySelector("[data-calendar-repeat-row]")
  if (row) row.dataset.repeating = String(repeating)
  options.hidden = !repeating
  options.disabled = locked || !repeating
  var intervalGroup = form.querySelector("[data-calendar-repeat-interval-group]")
  if (intervalGroup) {
    intervalGroup.hidden = !repeating
    intervalGroup.disabled = locked || !repeating
  }
  var ending = form.querySelector('[name="repeat_end"]').value
  ;["until", "count"].forEach(function (kind) {
    var group = form.querySelector("[data-calendar-repeat-" + kind + "]")
    group.hidden = ending !== kind
    group.disabled = locked || !repeating || ending !== kind
  })
  if (!repeating) return
  var interval = form.querySelector('[name="repeat_interval"]').value
  var unit = {daily: "day", weekly: "week", monthly: "month", yearly: "year"}[frequency.value]
  form.querySelector("[data-calendar-repeat-unit]").textContent = unit + (interval === "1" ? "" : "s")
  var summary = "Every " + (interval || "…") + " " + unit + (interval === "1" ? "" : "s")
  var start = form.querySelector('[name="start_date"]').value
  var date = /^\d{4}-\d{2}-\d{2}$/.test(start) ? new Date(start + "T12:00:00Z") : null
  if (date && !isNaN(date.getTime())) {
    if (frequency.value === "weekly") summary += " on " + date.toLocaleDateString(undefined, {weekday: "long", timeZone: "UTC"})
    if (frequency.value === "monthly") summary += " on day " + date.getUTCDate()
    if (frequency.value === "yearly") summary += " on " + date.toLocaleDateString(undefined, {month: "long", day: "numeric", timeZone: "UTC"})
  }
  if (ending === "count") {
    var count = form.querySelector('[name="repeat_count"]').value
    summary += ", " + (count || "…") + (count === "1" ? " occurrence" : " occurrences including the first")
  }
  if (ending === "until") summary += ", through " + (form.querySelector('[name="repeat_until"]').value || "the selected date")
  if (ending === "never") summary += ", with no end date"
  summary += "."
  if (date && (frequency.value === "monthly" || frequency.value === "yearly") && date.getUTCDate() > 28) summary += " Uses the last day in shorter months."
  if (!form.querySelector('[name="all_day"]').checked) summary += " Times follow " + form.querySelector('[name="timezone"]').value + "."
  form.querySelector("[data-calendar-repeat-summary]").textContent = summary
}

function toggleCalendarDescription(form) {
  if (!form || form._calendarCreateBusy || form._calendarCreateUncertain || form._calendarCreateConflict) return
  return setCalendarDescriptionExpanded(form, !form._calendarDescriptionExpanded)
}

// Calendar uses the composer's contenteditable, formatting and selection
// mechanics, without its email attachments, signatures or draft autosave.
function syncCalendarDescription(editor) {
  var form = editor && editor.closest("[data-calendar-create-form]")
  if (!form) return
  var html = _sanitizeComposeHTML(editor.innerHTML || "")
  // No remote images or message-only metadata in calendar notes.
  var fragment = document.createElement("template")
  fragment.innerHTML = html
  fragment.content.querySelectorAll("img").forEach(function (image) { image.remove() })
  form.querySelector('[name="description_html"]').value = fragment.innerHTML.trim()
  form.querySelector('[name="description"]').value = _composeEditorText(editor)
  calendarDescriptionSelection(editor)
}

function calendarDescriptionSelection(editor) {
  _saveComposeSelection(editor)
  var form = editor && editor.closest("[data-calendar-create-form]")
  if (!form) return
  form.querySelectorAll("[data-calendar-description-command]").forEach(function (button) {
    var active = false
    try { active = document.queryCommandState(button.dataset.calendarDescriptionCommand) } catch (e) {}
    button.classList.toggle("bg-accent", active)
    button.classList.toggle("text-foreground", active)
    button.setAttribute("aria-pressed", String(active))
  })
}

function calendarDescriptionExec(el, command, value) {
  // templUI dropdown items may be portaled outside the form.
  var form = el.closest("[data-calendar-create-form]") || document.getElementById("calendar-create-form")
  var editor = form && form.querySelector("[data-calendar-rich-editor]")
  if (!editor || editor.getAttribute("contenteditable") !== "true") return
  if (command === "createLink") {
    value = window.prompt("Paste a URL or email address")
    if (!value) return
    value = value.trim()
    if (value.indexOf("@") > 0 && !/^[a-z][a-z0-9+.-]*:/i.test(value)) value = "mailto:" + value
    if (!/^[a-z][a-z0-9+.-]*:/i.test(value)) value = "https://" + value
    if (!/^(https?:|mailto:)/i.test(value)) return
  }
  editor.focus()
  _restoreComposeSelection(editor)
  document.execCommand(command, false, value || null)
  syncCalendarDescription(editor)
}

function pasteCalendarDescription(event) {
  var clipboard = event.clipboardData || event.dataTransfer
  if (!clipboard) return
  event.preventDefault()
  var fragment = document.createElement("template")
  fragment.innerHTML = _sanitizeComposeHTML(clipboard.getData("text/html") || _composePlainToHTML(clipboard.getData("text/plain")))
  fragment.content.querySelectorAll("img").forEach(function (image) { image.remove() })
  document.execCommand("insertHTML", false, fragment.innerHTML)
  syncCalendarDescription(event.currentTarget)
}

document.addEventListener("keydown", function (event) {
  var editor = event.target && event.target.closest && event.target.closest("[data-calendar-rich-editor]")
  if (!editor || (!event.ctrlKey && !event.metaKey)) return
  var command = {b: "bold", i: "italic", u: "underline", k: "createLink"}[event.key.toLowerCase()]
  if (command) { event.preventDefault(); calendarDescriptionExec(editor, command) }
})

document.addEventListener("selectionchange", function () {
  var editor = document.activeElement && document.activeElement.closest && document.activeElement.closest("[data-calendar-rich-editor]")
  if (editor) calendarDescriptionSelection(editor)
})

function setCalendarDescriptionExpanded(form, expanded) {
  var editor = form.querySelector("[data-calendar-description-editor]")
  var slot = form.querySelector("[data-calendar-description-slot]")
  var viewport = form.querySelector("[data-calendar-create-viewport]")
  var body = form.querySelector("[data-calendar-create-body]")
  var content = editor && editor.querySelector("[data-calendar-description-content]")
  if (!editor || !slot || !viewport || !body || !content) return Promise.resolve()
  var richInput = editor.querySelector("[data-calendar-rich-editor]")
  _saveComposeSelection(richInput)
  var richRange = richInput._composeRange && richInput._composeRange.cloneRange()
  var state = form._calendarDescriptionState
  if (!state && !expanded) return Promise.resolve()
  if (editor._calendarDescriptionReveal) editor._calendarDescriptionReveal.cancel()
  var from = editor.getBoundingClientRect()
  var opacity = getComputedStyle(body).opacity
  if (!state) {
    state = form._calendarDescriptionState = {
      editorStyle: editor.style.cssText, slotHeight: slot.style.minHeight,
      bodyOpacity: body.style.opacity, bodyPointerEvents: body.style.pointerEvents, bodyInert: body.inert,
      contentOpacity: content.style.opacity,
      token: 0,
    }
    slot.style.minHeight = from.height + "px"
    // Move the original editor, not a copy: value, selection, and undo history
    // stay live. Its placeholder keeps the dialog's layout and height stable.
    viewport.appendChild(editor)
    editor.dataset.expanded = "true"
    editor.style.position = "absolute"
    editor.style.zIndex = "10"
  }
  var token = ++state.token
  if (state.resize) state.resize.cancel()
  if (state.fade) state.fade.cancel()
  if (state.reveal) state.reveal.cancel()
  var bounds = viewport.getBoundingClientRect()
  var padding = getComputedStyle(body)
  var left = parseFloat(padding.paddingLeft) || 0
  var top = parseFloat(padding.paddingTop) || 0
  var right = parseFloat(padding.paddingRight) || 0
  var bottom = parseFloat(padding.paddingBottom) || 0
  var destination = expanded ? {
    left: left, top: top,
  } : (function () {
    var target = slot.getBoundingClientRect()
    return {left: target.left - bounds.left, top: target.top - bounds.top, width: target.width, height: target.height}
  })()
  editor.style.left = destination.left + "px"
  editor.style.top = destination.top + "px"
  // The expanded resting size follows the viewport on window resizing.
  editor.style.width = expanded ? "calc(100% - " + (left + right) + "px)" : destination.width + "px"
  editor.style.height = expanded ? "calc(100% - " + (top + bottom) + "px)" : destination.height + "px"
  editor.style.transformOrigin = "top left"
  editor.style.willChange = "translate, scale"
  body.inert = true
  body.style.pointerEvents = "none"
  body.style.opacity = expanded ? "0" : state.bodyOpacity || "1"
  form._calendarDescriptionExpanded = expanded
  var button = editor.querySelector("[data-calendar-description-toggle]")
  var label = expanded ? "Collapse description" : "Expand description"
  button.setAttribute("aria-expanded", String(expanded))
  button.setAttribute("aria-label", label)
  button.title = label
  editor.querySelector("[data-calendar-description-expand]").hidden = expanded
  editor.querySelector("[data-calendar-description-collapse]").hidden = !expanded
  // Rich-text wrapping and scrollbar changes are discrete. Keep them
  // out of the motion: lay out the real input once, and scale an empty frame.
  content.style.opacity = "0"
  richInput.focus({preventScroll: true})
  if (richRange) { richInput._composeRange = richRange; _restoreComposeSelection(richInput) }
  editor.dataset.resizing = "true"
  function finish() {
    if (state.token !== token) return
    var resize = state.resize, fade = state.fade
    state.resize = state.fade = null
    editor.style.willChange = ""
    delete editor.dataset.resizing
    if (!expanded) {
      body.inert = state.bodyInert
      body.style.opacity = state.bodyOpacity
      body.style.pointerEvents = state.bodyPointerEvents
      slot.appendChild(editor)
      slot.style.minHeight = state.slotHeight
      editor.style.cssText = state.editorStyle
      delete editor.dataset.expanded
      form._calendarDescriptionState = null
    }
    // Release the held final frame only after the normal layout is restored.
    if (resize) resize.cancel()
    if (fade) fade.cancel()
    // Moving a focused input back into its slot may clear focus. Restore it
    // in the same frame, before the text and controls fade back in.
    var dialog = form.closest("dialog")
    if (form.isConnected && dialog && dialog.open) {
      var range = richInput._composeRange
      richInput.focus({preventScroll: true})
      if (range) { richInput._composeRange = range; _restoreComposeSelection(richInput) }
    }
    content.style.opacity = state.contentOpacity
    if (!reduced && content.animate) {
      state.reveal = content.animate([{opacity: 0}, {opacity: state.contentOpacity || "1"}], {duration: 160, easing: "ease"})
      editor._calendarDescriptionReveal = state.reveal
      return state.reveal.finished.catch(function () {})
    }
  }
  var reduced = window.matchMedia("(prefers-reduced-motion: reduce)").matches
  if (!editor.animate || reduced) { finish(); return Promise.resolve() }
  var to = editor.getBoundingClientRect()
  // FLIP only the empty surface, not the text or buttons. No width/height
  // interpolation, percentage endpoint handoff, or last-frame text reflow.
  state.resize = editor.animate([
    {translate: (from.left - to.left) + "px " + (from.top - to.top) + "px", scale: (from.width / to.width) + " " + (from.height / to.height)},
    {translate: "0px 0px", scale: "1 1"},
  ], {duration: 220, easing: "ease", fill: "forwards"})
  state.fade = body.animate([{opacity: opacity}, {opacity: body.style.opacity}], {duration: expanded ? 160 : 220, easing: "ease", fill: "forwards"})
  return state.resize.finished.then(finish, function () {})
}

function prepareCalendarDescriptionSubmit(event, button) {
  var form = button.closest("[data-calendar-create-form]")
  if (!form || !form._calendarDescriptionState) return
  // Reveal required fields before the browser tries to focus an invalid one.
  event.preventDefault()
  setCalendarDescriptionExpanded(form, false).then(function () {
    if (form.isConnected && !form._calendarDescriptionState) submitCalendarCreate(form)
  })
}

function updateCalendarTeamsForm(form, selected, locked) {
  var teamsSource = form.querySelector('[name="source_id"]')
  if (form._calendarTeamsPreview && (!teamsSource || teamsSource.value !== form._calendarTeamsPreview.source)) abandonCalendarTeamsPreview(form)
  var currentSource = form.querySelector('[name="source_id"]')
  if (form._calendarMeetPreview && (!currentSource || currentSource.value !== form._calendarMeetPreview.source)) form._calendarMeetPreview = null
  var root = form.querySelector("[data-calendar-teams-options]")
  if (!root) return
  var source = form.querySelector('[name="source_id"]')
  var provider = selected && selected.dataset.calendarSourceProvider
  var supported = provider === "outlook" || provider === "gmail"
  root.hidden = !supported
  if (locked) {
    if (root._calendarTeamsPending) {
      root._calendarTeamsSource = ""
      root.dataset.calendarTeamsState = ""
      root._calendarTeamsPending = false
    }
    if (window.htmx) window.htmx.trigger(root, "htmx:abort")
    root.querySelectorAll("input, button").forEach(function (input) { input.disabled = true })
    return
  }
  if (!supported) {
    if (window.htmx) window.htmx.trigger(root, "htmx:abort")
    root.replaceChildren()
    root._calendarTeamsSource = ""
    root._calendarTeamsPending = false
    return
  }
  if (root._calendarTeamsSource === undefined && root.dataset.calendarTeamsSource === source.value && root.dataset.calendarTeamsState) {
    root._calendarTeamsSource = source.value
  }
  if (root._calendarTeamsSource !== source.value && window.htmx) {
    window.htmx.trigger(root, "htmx:abort")
    root._calendarTeamsSource = source.value
    root.replaceChildren(form.querySelector(provider === "gmail" ? "[data-calendar-meet-checking]" : "[data-calendar-teams-checking]").content.cloneNode(true))
    root._calendarTeamsPending = true
    // A freshly swapped dialog can initialize before HTMX's settle phase
    // binds this custom trigger. Process it before sending the first check.
    window.htmx.process(root)
    window.htmx.trigger(root, "calendar-teams-source-changed")
  }
  root.querySelectorAll("[data-calendar-teams-toggle]").forEach(function (toggle) {
    toggle.disabled = toggle.dataset.calendarTeamsAvailable !== "true"
  })
  root.querySelectorAll("[data-calendar-teams-retry]").forEach(function (button) { button.disabled = false })
  updateCalendarMeetPreview(form, locked)
  updateCalendarTeamsPreview(form, locked)
}

// The conference is prepared independently of the event draft. Changes to
// dates, guests, or title must not allocate another meeting or lose its link.
function updateCalendarMeetPreview(form, locked) {
  var toggle = form.querySelector('[name="google_meet_meeting"]')
  var panel = form.querySelector("[data-calendar-meet-preview]")
  if (!toggle || !panel) return
  var input = panel.querySelector('[name="google_meet_draft_id"]')
  var source = form.querySelector('[name="source_id"]').value
  if (!toggle.checked) {
    panel.hidden = true
    input.value = ""
    form._calendarMeetPreview = null
    return
  }
  panel.hidden = false
  var state = form._calendarMeetPreview
  if (!state || state.source !== source) {
    if (locked) return
    state = {source: source, id: window.crypto.randomUUID(), status: "loading", url: ""}
    form._calendarMeetPreview = state
    prepareCalendarMeetPreview(form, state)
  }
  input.value = state.status === "ready" ? state.id : ""
  panel.querySelector("[data-calendar-meet-status]").textContent = state.status === "loading" ? "Generating Google Meet link…" : state.status === "error" ? state.error : ""
  var link = panel.querySelector("[data-calendar-meet-link]")
  link.hidden = state.status !== "ready"
  var anchor = panel.querySelector("[data-calendar-meet-url]")
  anchor.textContent = state.url
  if (state.url) anchor.href = state.url
  else anchor.removeAttribute("href")
  var retry = panel.querySelector("[data-calendar-meet-prepare-retry]")
  retry.hidden = state.status !== "error"
  retry.disabled = locked
  panel.querySelector("[data-calendar-meet-copy]").disabled = locked
}

function abandonCalendarTeamsPreview(form) {
  var state = form && form._calendarTeamsPreview
  if (!state || state.saved || form._calendarCreateBusy || form._calendarCreateUncertain) return
  form._calendarTeamsPreview = null
  // The server serializes discard behind preparation and retains a tombstone
  // if discard arrives first. A disconnected browser still has expiry cleanup.
  fetch("/api/calendar/teams/drafts/discard", {method: "POST", headers: {"Content-Type": "application/x-www-form-urlencoded"}, body: new URLSearchParams({source_id: state.source, draft_id: state.id}).toString(), keepalive: true}).catch(function () {})
}

document.body.addEventListener("htmx:beforeCleanupElement", function (event) {
  var element = event.detail && event.detail.elt
  if (!element || !element.querySelector) return
  var form = element.matches("[data-calendar-create-form]") ? element : element.querySelector("[data-calendar-create-form]")
  if (form) { abandonCalendarTeamsPreview(form); stopCalendarDialogResize(form) }
})

window.addEventListener("pagehide", function () {
  var form = document.querySelector("[data-calendar-create-form]")
  if (form) abandonCalendarTeamsPreview(form)
})

function updateCalendarTeamsPreview(form, locked) {
  var panel = form.querySelector('[data-calendar-meeting-provider="teams_meeting"]')
  var toggle = form.querySelector('[name="teams_meeting"]')
  if (!panel || !toggle || form.dataset.calendarEventId) return
  var input = panel.querySelector('[name="teams_draft_id"]')
  if (!toggle.checked) {
    panel.hidden = true
    input.value = ""
    abandonCalendarTeamsPreview(form)
    return
  }
  panel.hidden = false
  var source = form.querySelector('[name="source_id"]').value
  var state = form._calendarTeamsPreview
  if (!state || state.source !== source) {
    if (locked) return
    state = {provider: "teams", source: source, id: window.crypto.randomUUID(), status: "loading", url: ""}
    form._calendarTeamsPreview = state
    prepareCalendarMeetPreview(form, state)
  }
  input.value = state.status === "ready" ? state.id : ""
  panel.querySelector("[data-calendar-meet-status]").textContent = state.status === "loading" ? "Generating Teams link…" : state.status === "error" ? state.error : ""
  panel.querySelector("[data-calendar-meet-link]").hidden = state.status !== "ready"
  var anchor = panel.querySelector("[data-calendar-meet-url]")
  anchor.textContent = state.url
  if (state.url) anchor.href = state.url
  else anchor.removeAttribute("href")
  var retry = panel.querySelector("[data-calendar-meet-prepare-retry]")
  retry.hidden = state.status !== "error"
  retry.disabled = locked
  panel.querySelector("[data-calendar-meet-copy]").disabled = locked
}

function retryCalendarTeamsPreview(form) {
  var state = form && form._calendarTeamsPreview
  if (!state || state.status !== "error" || form._calendarCreateBusy || form._calendarCreateUncertain || form._calendarCreateConflict) return
  state.status = "loading"
  state.attempts = 0
  state.deadline = 0
  updateCalendarCreateForm(form)
  prepareCalendarMeetPreview(form, state)
}

function prepareCalendarMeetPreview(form, state) {
  var teams = state.provider === "teams"
  var key = teams ? "_calendarTeamsPreview" : "_calendarMeetPreview"
  var label = teams ? "Teams" : "Google Meet"
  var payload = new URLSearchParams({source_id: state.source, draft_id: state.id})
  if (form.dataset.calendarEventId) payload.set("event_id", form.dataset.calendarEventId)
  state.attempts = (state.attempts || 0) + 1
  if (!state.deadline) state.deadline = Date.now() + 75000
  var controller = new AbortController()
  var timeout
  var deadline = new Promise(function (_, reject) {
    timeout = setTimeout(function () {
      reject(new Error(label + " link generation timed out. Retry to check the same link again."))
      controller.abort()
    }, Math.max(0, Math.min(35000, state.deadline - Date.now())))
  })
  var request = fetch(teams ? "/api/calendar/teams/drafts" : "/api/calendar/google-meet/drafts", {method: "POST", headers: {"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json"}, body: payload.toString(), signal: controller.signal})
    .then(function (response) { return response.json().then(function (data) { return {ok: response.ok, data: data} }) })
  return Promise.race([request, deadline])
    .finally(function () { clearTimeout(timeout) })
    .then(function (result) {
      if (!form.isConnected || form[key] !== state) return
      if (!result.ok) throw new Error(result.data.error || "Could not generate the " + label + " link. Retry.")
      if (result.data.source_id !== state.source || result.data.draft_id !== state.id) throw new Error("The meeting request changed. Retry.")
      if (result.data.pending && state.attempts < 5) {
        setTimeout(function () { if (form.isConnected && form[key] === state) prepareCalendarMeetPreview(form, state) }, 750)
        return
      }
      var validURL = teams ? /^https:\/\/(?:teams\.microsoft\.com|teams\.live\.com|teams\.cloud\.microsoft)\/(?:meet|l\/meetup-join)\/[^\s<>"']+$/i : /^https:\/\/meet\.google\.com\/[a-z]{3}-[a-z]{4}-[a-z]{3}(?:\?[^\s]*)?$/
      if (!validURL.test(result.data.join_url || "")) throw new Error(label + " is still preparing the link. Retry to check it again.")
      state.status = "ready"
      state.url = result.data.join_url
      updateCalendarCreateForm(form)
    })
    .catch(function (error) {
      if (!form.isConnected || form[key] !== state) return
      state.status = "error"
      state.error = error.message || "Could not generate the " + label + " link. Retry."
      updateCalendarCreateForm(form)
    })
}

function retryCalendarMeetPreview(form) {
  var state = form && form._calendarMeetPreview
  if (!state || state.status !== "error" || form._calendarCreateBusy || form._calendarCreateUncertain || form._calendarCreateConflict) return
  state.status = "loading"
  state.attempts = 0
  state.deadline = 0
  updateCalendarCreateForm(form)
  prepareCalendarMeetPreview(form, state)
}

function copyCalendarMeetLink(button) {
  var link = button.closest("[data-calendar-meet-preview]").querySelector("[data-calendar-meet-url]")
  if (!link || !link.href || !navigator.clipboard) return
  navigator.clipboard.writeText(link.href).then(function () {
    button.textContent = "Copied"
    setTimeout(function () { if (button.isConnected) button.textContent = "Copy link" }, 1500)
  }).catch(function () {})
}

function retryCalendarTeams(form) {
  if (!form || form._calendarCreateBusy || form._calendarCreateUncertain || form._calendarCreateConflict) return
  var root = form.querySelector("[data-calendar-teams-options]")
  if (!root) return
  root._calendarTeamsSource = ""
  root.dataset.calendarTeamsState = ""
  updateCalendarCreateForm(form)
}

function calendarTeamsSwapAllowed(detail) {
  var root = detail.target
  var form = root.closest("[data-calendar-create-form]")
  var source = form && form.querySelector('[name="source_id"]')
  return !!(form && form.isConnected && !form._calendarCreateBusy && !form._calendarCreateUncertain && !form._calendarCreateConflict && source && detail.xhr.getResponseHeader("X-Gofer-Calendar-Source") === source.value)
}

document.body.addEventListener("htmx:beforeSwap", function (event) {
  var detail = event.detail
  if (detail && detail.target && detail.target.id === "calendar-teams-options" && !calendarTeamsSwapAllowed(detail)) detail.shouldSwap = false
})

document.body.addEventListener("htmx:afterSwap", function (event) {
  var target = event.detail && event.detail.target
  if (target && target.id === "calendar-teams-options") {
    target._calendarTeamsPending = false
    updateCalendarCreateForm(target.closest("[data-calendar-create-form]"))
  }
})

document.body.addEventListener("htmx:afterRequest", function (event) {
  var detail = event.detail
  var root = detail && detail.elt
  if (!root || root.id !== "calendar-teams-options" || !detail.failed || !root.isConnected) return
  var form = root.closest("[data-calendar-create-form]")
  if (!form || form._calendarCreateBusy || form._calendarCreateUncertain || form._calendarCreateConflict) return
  var retry = root.querySelector("[data-calendar-teams-retry]")
  if (retry) retry.hidden = false
})

function updateCalendarCreateForm(form) {
  if (!form) return
  adjustCalendarCreateAllDayRange(form)
  var editing = !!form.dataset.calendarEventId
  var source = form.querySelector('[name="source_id"]')
  var sourceSelect = form.querySelector("[data-calendar-create-source-select]")
  var choices = sourceSelect ? Array.from(sourceSelect.querySelectorAll("[data-tui-selectbox-value]")) : []
  var selected = source && choices.find(function (choice) { return choice.dataset.tuiSelectboxValue === source.value })
  var writable = selected && selected.dataset.tuiSelectboxDisabled !== "true" && selected.dataset.calendarSourceWritable === "true"
  var allowed = writable && selected.dataset.calendarSourceAuthorized === "true"
  var busy = !!form._calendarCreateBusy
  var uncertain = !!form._calendarCreateUncertain
  var locked = busy || uncertain || !!form._calendarCreateConflict
  var richEditor = form.querySelector("[data-calendar-rich-editor]")
  if (richEditor) {
    richEditor.setAttribute("contenteditable", String(!locked))
    richEditor.setAttribute("aria-disabled", String(locked))
  }
  var toolbar = form.querySelector("[data-calendar-description-toolbar]")
  if (toolbar) {
    toolbar.inert = locked
    toolbar.querySelectorAll("button").forEach(function (button) { button.disabled = locked })
  }
  var descriptionToggle = form.querySelector("[data-calendar-description-toggle]")
  if (descriptionToggle) descriptionToggle.disabled = locked
  var allDay = form.querySelector('[name="all_day"]').checked
  form.querySelectorAll("input, select, textarea").forEach(function (input) { input.disabled = locked })
  updateCalendarTeamsForm(form, selected, locked)
  var sourceTrigger = sourceSelect && sourceSelect.querySelector(".select-trigger")
  form.querySelectorAll(".select-trigger, [data-tui-datepicker], [data-tui-timepicker]").forEach(function (trigger) {
    trigger.disabled = locked
  })
  // The disabled templUI button still owns an enabled hidden source_id input.
  if (sourceTrigger) sourceTrigger.disabled = editing || locked || choices.length === 0
  form.querySelectorAll("[data-calendar-create-time]").forEach(function (node) {
    node.hidden = allDay
    var input = node.querySelector("input")
    input.disabled = locked || allDay
    var trigger = node.querySelector("[data-tui-timepicker]")
    if (trigger) trigger.disabled = locked || allDay
  })
  if (locked || allDay) form.querySelectorAll("[data-tui-popover-content]").forEach(function (content) {
    if (!locked && !content.closest("[data-calendar-create-time], [data-calendar-create-timezone]")) return
    if (content.matches(":popover-open") && window.tui && window.tui.popover) window.tui.popover.closeElement(content)
  })
  form.querySelector("[data-calendar-create-timezone]").hidden = allDay
  updateCalendarRecurrenceForm(form, locked)
  var dateHelp = form.querySelector("[data-calendar-create-date-help]")
  dateHelp.hidden = !allDay
  dateHelp.textContent = form._calendarCreateDateAdjusted ?
    "End date adjusted to the start date: it can’t end earlier." :
    "The end date is included in an all-day event."
  var access = form.querySelector("[data-calendar-create-access]")
  access.hidden = !!allowed
  access.textContent = editing ? "This calendar is no longer writable. Refresh calendars and reopen the event to check access." : !writable ?
    "No writable calendar is configured. Choose calendars in Accounts first." :
    "This account has read-only Calendar access. Reconnect it from Accounts to grant event creation permission."
  var submit = form.querySelector("[data-calendar-create-submit]")
  var meetToggle = form.querySelector('[name="google_meet_meeting"]')
  var meetDraft = form.querySelector('[name="google_meet_draft_id"]')
  var teamsToggle = form.querySelector('[name="teams_meeting"]')
  var teamsDraft = form.querySelector('[name="teams_draft_id"]')
  var teamsNotReady = !!(!editing && teamsToggle && teamsToggle.checked && (!teamsDraft || !teamsDraft.value))
  var meetNotReady = !!(meetToggle && meetToggle.checked && (!meetDraft || !meetDraft.value))
  submit.disabled = (editing ? locked || !allowed : busy || (!uncertain && !allowed)) || (!uncertain && (meetNotReady || teamsNotReady))
  submit.setAttribute("aria-busy", busy ? "true" : "false")
  form.querySelector("[data-calendar-create-spinner]").hidden = !busy
  form.querySelector("[data-calendar-create-submit-label]").textContent = editing ? (busy ? "Saving..." : form.dataset.calendarEditSeries === "true" ? "Save series" : "Save changes") : busy ? "Creating…" : uncertain ? "Retry safely" : "Create event"
  form.setAttribute("aria-busy", busy ? "true" : "false")
}

function stopCalendarDialogResize(form) {
  var state = form && form._calendarDialogResize
  if (!state) return
  form._calendarDialogResize = null
  state.mutations.disconnect()
  if (state.sizes) state.sizes.disconnect()
  if (state.frame) cancelAnimationFrame(state.frame)
  if (state.animation) state.animation.cancel()
  state.contentAnimations.forEach(function (animation) { animation.cancel() })
  state.dialog.style.height = state.heightStyle
  if (state.panel) state.panel.style.height = state.panelHeightStyle
  if (state.body) state.body.style.scrollbarGutter = state.gutter
  if (state.transition !== null) state.dialog.style.transitionProperty = state.transition
  state.dialog.removeEventListener("close", state.close)
  form.removeEventListener("input", state.interact, true)
  form.removeEventListener("change", state.interact, true)
  form.removeEventListener("click", state.interact, true)
  window.removeEventListener("resize", state.resize)
}

function initializeCalendarDialogResize(form) {
  if (!form || form._calendarDialogResize || !form.closest || typeof MutationObserver === "undefined") return
  var dialog = form.closest("dialog")
  if (!dialog || !dialog.animate) return
  var body = form.querySelector("[data-calendar-create-body]")
  var panel = dialog.querySelector && dialog.querySelector("[data-tui-dialog-panel]")
  var gutter = body && body.style.scrollbarGutter
  // Keep field widths stable as the animated viewport gains/loses scrolling.
  if (body) body.style.scrollbarGutter = "stable"
  function height() { return parseFloat(getComputedStyle(dialog).height) || 0 }
  var state = {dialog: dialog, body: body, panel: panel, panelHeightStyle: panel && panel.style.height, gutter: gutter, endsAt: 0, heightStyle: dialog.style.height, height: dialog.open ? height() : 0, transition: null, animation: null, frame: null, contentAnimations: new Map(), layout: new Map(), interacted: false}
  form._calendarDialogResize = state
  function layout() {
    var positions = new Map()
    if (!body) return positions
    function top(node) {
      var value = 0
      while (node && node !== dialog) { value += node.offsetTop; node = node.offsetParent }
      return value
    }
    var origin = top(body)
    // Only whole field rows participate. Layout offsets deliberately exclude
    // the dialog's entrance transform, scrolling, and individual input geometry.
    Array.from(body.children).forEach(function (node) {
      if (!node.animate || !node.offsetHeight) return
      positions.set(node, {y: top(node) - origin})
    })
    return positions
  }
  state.layout = layout()
  function moveContent(duration, animate) {
    // Keep rows below an expanded/collapsed section at their previous position,
    // then slide them to their new position with the dialog's height animation.
    var previous = state.layout
    state.contentAnimations.forEach(function (animation, node) {
      var position = previous.get(node)
      var translate = getComputedStyle(node).translate.split(/\s+/)
      if (position) {
        position.y += parseFloat(translate[1]) || 0
      }
      animation.cancel()
    })
    state.contentAnimations.clear()
    var next = layout()
    state.layout = next
    if (!animate) return
    next.forEach(function (position, node) {
      var old = previous.get(node)
      var dy = old ? old.y - position.y : 0
      if (Math.abs(dy) < 0.5) return
      var keyframes = [{translate: "0px " + dy + "px"}, {translate: "0px 0px"}]
      var animation = node.animate(keyframes, {duration: duration, easing: "ease"})
      state.contentAnimations.set(node, animation)
      animation.onfinish = function () {
        if (state.contentAnimations.get(node) === animation) state.contentAnimations.delete(node)
      }
    })
  }
  function restore() {
    dialog.style.height = state.heightStyle
    if (panel) panel.style.height = state.panelHeightStyle
    if (state.transition !== null) dialog.style.transitionProperty = state.transition
    state.transition = null
  }
  function measure() {
    state.frame = null
    if (form._calendarDialogResize !== state) return
    if (!form.isConnected || !dialog.isConnected) { stopCalendarDialogResize(form); return }
    if (!dialog.open || dialog.hasAttribute("data-tui-dialog-closing")) {
      if (state.animation) state.animation.cancel()
      state.animation = null
      restore()
      state.height = 0
      moveContent(0, false)
      return
    }
    // Read the current animated height when retargeting a rapid second change.
    // Measuring CSS height avoids templUI's separate entrance/exit scale.
    var active = !!state.animation
    var previousTarget = state.height
    var from = active ? height() : state.height
    if (state.animation) state.animation.cancel()
    state.animation = null
    // Exclude height from templUI's transition-all while measuring natural
    // layout, keeping its opacity and transform transitions intact.
    if (state.transition === null) state.transition = dialog.style.transitionProperty
    dialog.style.transitionProperty = "opacity, transform, scale, translate"
    dialog.style.height = state.heightStyle
    if (panel) panel.style.height = state.panelHeightStyle
    var to = height()
    state.height = to
    var reduced = window.matchMedia("(prefers-reduced-motion: reduce)").matches
    var now = performance.now()
    var duration = active && Math.abs(previousTarget - to) < 0.5 ? Math.max(1, state.endsAt - now) : 220
    moveContent(duration, state.interacted && !!from && !!to && !reduced)
    if (!from || !to || Math.abs(from - to) < 0.5 || reduced) {
      restore()
      return
    }
    // Animate height rather than scaling text and controls. The existing flex
    // viewport keeps the footer visible, and max-height still caps the dialog.
    dialog.style.height = to + "px"
    // The panel otherwise keeps its new intrinsic height while only the outer
    // dialog animates. Make its flex viewport follow each animated frame so
    // the footer stays attached to the moving bottom edge.
    if (panel) panel.style.height = "100%"
    // Mutations that do not change the target must not keep restarting a resize
    // (for example toolbar highlights while typing in the description).
    state.endsAt = now + duration
    var animation = dialog.animate([{height: from + "px"}, {height: to + "px"}], {duration: duration, easing: "ease"})
    state.animation = animation
    animation.onfinish = function () {
      if (form._calendarDialogResize !== state || state.animation !== animation) return
      state.animation = null
      restore()
      if (Math.abs(height() - state.height) >= 0.5) measure()
    }
  }
  function schedule() {
    if (!state.frame) state.frame = requestAnimationFrame(measure)
  }
  state.mutations = new MutationObserver(schedule)
  state.mutations.observe(form, {subtree: true, childList: true, characterData: true, attributes: true, attributeFilter: ["hidden", "class", "style"]})
  // Observe the intrinsic field rows, not the flex viewport whose height is
  // changing during this animation. This also catches text wrapping/fonts.
  if (typeof ResizeObserver !== "undefined") {
    state.sizes = new ResizeObserver(function () { if (!state.animation) schedule() })
    if (body) Array.from(body.children).forEach(function (row) { state.sizes.observe(row) })
  }
  state.close = function () { stopCalendarDialogResize(form) }
  state.interact = function () { state.interacted = true }
  state.resize = schedule
  dialog.addEventListener("close", state.close)
  form.addEventListener("input", state.interact, true)
  form.addEventListener("change", state.interact, true)
  form.addEventListener("click", state.interact, true)
  window.addEventListener("resize", state.resize)
}

function initializeCalendarCreateForm() {
  var form = document.querySelector("[data-calendar-create-form]")
  if (!form) return
  var list = form.querySelector("#calendar-create-timezones")
  var template = form.querySelector("[data-calendar-create-timezone-option]")
  if (list && template && !list.dataset.calendarTimezonesInitialized) {
    list.dataset.calendarTimezonesInitialized = "true"
    var existing = Array.from(list.querySelectorAll("[data-tui-selectbox-value]")).map(function (item) { return item.dataset.tuiSelectboxValue })
    var zones = ["UTC"].concat(typeof Intl.supportedValuesOf === "function" ? Intl.supportedValuesOf("timeZone") : [])
    var fragment = document.createDocumentFragment()
    zones.forEach(function (zone) {
      if (existing.indexOf(zone) !== -1) return
      var option = template.content.firstElementChild.cloneNode(true)
      option.dataset.tuiSelectboxValue = zone
      option.querySelector(".select-item-text").textContent = zone
      fragment.appendChild(option)
    })
    list.appendChild(fragment)
  }
  if (!form._calendarTeamsCloseBound) {
    var dialog = form.closest("dialog")
    if (dialog) {
      form._calendarTeamsCloseBound = true
      dialog.addEventListener("close", function () { abandonCalendarTeamsPreview(form) })
    }
  }
  updateCalendarCreateForm(form)
  initializeCalendarDialogResize(form)
}

function updateCalendarGuestDropdown(form) {
  if (!form || !window.tui || !window.tui.popover) return
  var input = form.querySelector('[name="guests"]')
  var root = form.querySelector("#calendar-guests-dropdown")
  var list = form.querySelector("#calendar-guest-suggestions")
  if (!input || !root || !list) return
  var content = root.querySelector("[data-calendar-guests-dropdown-content]")
  if (input.disabled || !list.querySelector("[data-calendar-guest-value]") || (document.activeElement !== input && !content.contains(document.activeElement))) {
    window.tui.popover.close(root.id)
    input.setAttribute("aria-expanded", "false")
    input.removeAttribute("aria-activedescendant")
    return
  }
  content.style.width = input.getBoundingClientRect().width + "px"
  window.tui.popover.open(root.id)
  input.setAttribute("aria-expanded", "true")
}

function syncCalendarGuestDropdownState(content, event) {
  var form = content.closest("[data-calendar-create-form]")
  var input = form && form.querySelector('[name="guests"]')
  if (!input) return
  var open = event.newState === "open"
  input.setAttribute("aria-expanded", String(open))
  if (!open) input.removeAttribute("aria-activedescendant")
}

function handleCalendarGuestKeydown(event, input) {
  var form = input.closest("[data-calendar-create-form]")
  var api = window.tui && window.tui.popover
  if (!form || !api || input.disabled) return
  var list = form.querySelector("#calendar-guest-suggestions")
  if (event.key === "Escape" || event.key === "Tab") {
    if (event.key === "Escape" && api.isOpen("calendar-guests-dropdown")) event.preventDefault()
    api.close("calendar-guests-dropdown")
    input.setAttribute("aria-expanded", "false")
    input.removeAttribute("aria-activedescendant")
    return
  }
  if (!list) return
  var options = Array.from(list.querySelectorAll("[data-calendar-guest-value]"))
  var active = options.findIndex(function (option) { return option.dataset.calendarGuestActive === "true" })
  if (event.key === "Enter" && api.isOpen("calendar-guests-dropdown") && active !== -1) {
    event.preventDefault()
    selectCalendarGuest(options[active])
  } else if ((event.key === "ArrowDown" || event.key === "ArrowUp") && options.length) {
    event.preventDefault()
    updateCalendarGuestDropdown(form)
    active = active === -1 ? (event.key === "ArrowDown" ? 0 : options.length - 1) : (active + (event.key === "ArrowDown" ? 1 : -1) + options.length) % options.length
    options.forEach(function (option, index) {
      option.dataset.calendarGuestActive = String(index === active)
      option.setAttribute("aria-selected", String(index === active))
    })
    input.setAttribute("aria-activedescendant", options[active].id)
    options[active].scrollIntoView({block: "nearest"})
  }
}

document.body.addEventListener("htmx:afterSwap", function (event) {
  var target = event.detail && event.detail.target
  if (!target || target.id !== "calendar-guest-suggestions") return
  var form = target.closest("[data-calendar-create-form]")
  var input = form && form.querySelector('[name="guests"]')
  if (input) input.removeAttribute("aria-activedescendant")
  updateCalendarGuestDropdown(form)
})

function selectCalendarGuest(button) {
  var form = button.closest("[data-calendar-create-form]")
  var input = form && form.querySelector('[name="guests"]')
  if (!input || input.disabled) return
  var separator = -1, quoted = false, escaped = false
  for (var i = 0; i < input.value.length; i++) {
    var char = input.value[i]
    if (escaped) { escaped = false; continue }
    if (char === "\\" && quoted) { escaped = true; continue }
    if (char === '"') quoted = !quoted
    if (char === "," && !quoted) separator = i
  }
  var prefix = input.value.slice(0, separator + 1).trim()
  input.value = (prefix ? prefix + " " : "") + button.dataset.calendarGuestValue + ", "
  if (window.htmx) window.htmx.trigger(input, "htmx:abort")
  form.querySelector("#calendar-guest-suggestions").replaceChildren()
  updateCalendarGuestDropdown(form)
  input.dispatchEvent(new Event("change", {bubbles: true}))
  input.focus()
}

function validateCalendarCreatePickers(form) {
  adjustCalendarCreateAllDayRange(form)
  var fields = ["start_date", "end_date", "timezone"]
  if (!form.querySelector('[name="all_day"]').checked) fields = fields.concat(["start_time", "end_time"])
  var repeat = form.querySelector('[name="repeat_frequency"]')
  var teams = form.querySelector('[name="teams_meeting"]')
  if (teams && teams.checked && !teams.disabled && repeat && repeat.value !== "none") {
    setCalendarCreateError(form, "Teams meetings currently support events that do not repeat. Choose Does not repeat or turn Teams off.")
    teams.focus()
    return false
  }
  var meet = form.querySelector('[name="google_meet_meeting"]')
  if (meet && meet.checked && !meet.disabled && repeat && repeat.value !== "none") {
    setCalendarCreateError(form, "Google Meet meetings currently support events that do not repeat. Choose Does not repeat or turn Google Meet off.")
    meet.focus()
    return false
  }
  var guests = form.querySelector('[name="guests"]')
  if (guests && guests.value.trim() && repeat && repeat.value !== "none") {
    setCalendarCreateError(form, "Guest invitations currently support events that do not repeat. Remove the guests or choose Does not repeat.")
    guests.focus()
    return false
  }
  var meetDraft = form.querySelector('[name="google_meet_draft_id"]')
  if (meet && meet.checked && !meet.disabled && (!meetDraft || !meetDraft.value)) {
    setCalendarCreateError(form, "Wait for the Google Meet link before saving, or turn Google Meet off.")
    return false
  }
  var teamsDraft = form.querySelector('[name="teams_draft_id"]')
  if (!(form.dataset && form.dataset.calendarEventId) && teams && teams.checked && !teams.disabled && (!teamsDraft || !teamsDraft.value)) {
    setCalendarCreateError(form, "Wait for the Teams link before saving, or turn Teams off.")
    return false
  }
  var until = repeat && repeat.value !== "none" && form.querySelector('[name="repeat_end"]').value === "until"
  if (until) fields.push("repeat_until")
  for (var i = 0; i < fields.length; i++) {
    var input = form.querySelector('[name="' + fields[i] + '"]')
    if (input && input.value) continue
    setCalendarCreateError(form, fields[i] === "repeat_until" ? "Choose the last date the series can start." : "Choose a " + fields[i].replace("_", " ").replace("timezone", "time zone") + ".")
    var trigger = form.querySelector("#calendar-create-" + fields[i].replace(/_/g, "-"))
    if (trigger) trigger.focus()
    return false
  }
  if (until && form.querySelector('[name="repeat_until"]').value < form.querySelector('[name="start_date"]').value) {
    setCalendarCreateError(form, "The repeat end date must be on or after the event’s start date.")
    form.querySelector("#calendar-create-repeat-until").focus()
    return false
  }
  return true
}

function setCalendarCreateError(form, message) {
  var error = form.querySelector("[data-calendar-create-error]")
  error.textContent = message || ""
  error.hidden = !message
  if (message && form.isConnected && form.dataset.calendarEventId && (form._calendarCreateUncertain || form._calendarCreateConflict)) error.focus()
}

function submitCalendarCreate(form) {
  if (!form) return Promise.resolve()
  if (form._calendarDescriptionState && !form._calendarCreateBusy && !form._calendarCreateUncertain && !form._calendarCreateConflict) {
    return setCalendarDescriptionExpanded(form, false).then(function () {
      if (form.isConnected && !form._calendarDescriptionState) return submitCalendarCreate(form)
    })
  }
  var editing = !!form.dataset.calendarEventId
  if (form._calendarCreateBusy || (editing && (form._calendarCreateUncertain || form._calendarCreateConflict)) || (!form._calendarCreateUncertain && (!form.reportValidity() || !validateCalendarCreatePickers(form)))) return Promise.resolve()
  if (!form._calendarCreateUncertain) syncCalendarDescription(form.querySelector("[data-calendar-rich-editor]"))
  if (editing) ["source_id", "version", "request_id", "edit_scope"].forEach(function (name) {
    var input = form.querySelector('[name="' + name + '"]')
    if (input) input.disabled = false
  })
  var payload = form._calendarCreateUncertain ? form._calendarCreatePayload : new URLSearchParams(new FormData(form)).toString()
  form._calendarCreatePayload = payload
  form._calendarCreateBusy = true
  setCalendarCreateError(form, "")
  updateCalendarCreateForm(form)
  return fetch(editing ? form.action : "/api/calendar/events", {method: editing ? "PATCH" : "POST", headers: {"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json"}, body: payload})
    .then(function (response) {
      return response.json().then(function (data) { return {ok: response.ok, data: data} })
    })
    .then(function (result) {
      if (!result.ok) {
        form._calendarCreateUncertain = !!result.data.uncertain
        form._calendarCreateConflict = editing && !!result.data.conflict
        if (!editing && !form._calendarCreateUncertain) {
          if (result.data.request_id) form.querySelector('[name="request_id"]').value = result.data.request_id
          else if (window.crypto && typeof window.crypto.randomUUID === "function") form.querySelector('[name="request_id"]').value = window.crypto.randomUUID()
        }
        var message = result.data.error || (editing ? "Could not update this event." : "Could not create this event. Try again.")
        if (editing && (form._calendarCreateUncertain || form._calendarCreateConflict) && !result.data.error) message += " Refresh calendars and reopen this event to verify the saved details before editing again."
        setCalendarCreateError(form, message)
        return
      }
      if (!(result.data.event_id || (!editing && result.data.series_id)) || (editing && result.data.saved !== true)) throw new Error("Event save could not be confirmed")
      if (editing && (result.data.event_id !== form.dataset.calendarEventId || (form.dataset.calendarEditOccurrence === "true" && result.data.scope !== "occurrence"))) throw new Error("Event scope could not be confirmed")
      if (form._calendarTeamsPreview) form._calendarTeamsPreview.saved = true
      // Close only this form; a delayed save must not close a newer dialog.
      if (form.isConnected && window.tui && window.tui.dialog) window.tui.dialog.close("calendar-create-dialog")
      var warning = result.data.teams_unconfirmed || result.data.google_meet_unconfirmed || result.data.refresh_pending
      showGoferToast({title: editing ? (form.dataset.calendarEditSeries === "true" ? "Series updated" : "Event updated") : result.data.series_id ? "Recurring event created" : "Event created", description: result.data.teams_unconfirmed ? "Event saved, but Microsoft did not confirm a Teams link. Check the event in Outlook; do not create it again." : result.data.google_meet_unconfirmed ? "Event saved, but its Google Meet link is not ready. Refresh the calendar or check Google Calendar; do not create it again." : result.data.refresh_pending ? "Series saved. Its occurrences could not refresh yet; refresh the calendar to load them." : result.data.notify_guests ? "Meeting saved. Guest notifications are being delivered." : result.data.hidden ? "Saved to a hidden calendar. Enable its visibility to see it." : "Saved to your calendar.", variant: warning ? "warning" : "success", icon: warning ? "warning" : "success", duration: warning ? 10000 : 4500})
      scheduleCalendarCacheRefresh()
    })
    .catch(function () {
      form._calendarCreateUncertain = true
      setCalendarCreateError(form, editing ? "The save result could not be confirmed. Refresh calendars and reopen this event to verify the saved details before editing again." : "The result could not be confirmed. Retry this same event to check without creating a duplicate.")
    })
    .finally(function () { form._calendarCreateBusy = false; updateCalendarCreateForm(form) })
}

document.addEventListener("submit", function (event) {
  if (!event.target || !event.target.matches("[data-calendar-create-form]")) return
  event.preventDefault()
  submitCalendarCreate(event.target)
})

document.addEventListener("change", function (event) {
  var form = event.target && event.target.closest && event.target.closest("[data-calendar-create-form]")
  if (!form || form._calendarCreateBusy || form._calendarCreateUncertain || form._calendarCreateConflict) return
  setCalendarCreateError(form, "")
  updateCalendarCreateForm(form)
})

document.addEventListener("input", function (event) {
  if (!event.target || !event.target.matches("[data-tui-datepicker-hidden-input], [data-tui-timepicker-hidden-input], [data-tui-selectbox-hidden-input], [data-calendar-repeat-number]")) return
  var form = event.target.closest("[data-calendar-create-form]")
  if (!form || form._calendarCreateBusy || form._calendarCreateUncertain || form._calendarCreateConflict) return
  setCalendarCreateError(form, "")
  updateCalendarCreateForm(form)
})

document.addEventListener("click", function (event) {
  var toggle = event.target.closest && event.target.closest("[data-mail-calendar-response-edit], [data-mail-calendar-response-cancel]")
  if (!toggle || toggle.disabled) return
  var form = toggle.closest("[data-calendar-response-form]")
  if (!form || form._calendarResponseBusy || form._calendarResponseBlocked || form._calendarResponseLoader || form.dataset.calendarResponseReady !== "true") return
  var summary = form.querySelector("[data-mail-calendar-response-summary]")
  var editor = form.querySelector("[data-mail-calendar-response-editor]")
  if (!summary || !editor) return
  var editing = toggle.hasAttribute("data-mail-calendar-response-edit")
  summary.hidden = editing
  editor.hidden = !editing
  form.querySelector("[data-calendar-response-progress]").hidden = !editing
  if (!editing) {
    setCalendarResponseError(form, "")
    summary.querySelector("[data-mail-calendar-response-edit]").focus()
    return
  }
  var choice = editor.querySelector('[data-calendar-response-choice][aria-pressed="true"]') || editor.querySelector("[data-calendar-response-choice]")
  if (choice) choice.focus()
})

function updateCalendarResponseForm(form) {
  var busy = !!form._calendarResponseBusy
  var locked = busy || !!form._calendarResponseBlocked
  form.setAttribute("aria-busy", busy || !!form._calendarResponseLoader ? "true" : "false")
  form.querySelectorAll("[data-calendar-response-choice], [data-calendar-response-trigger]").forEach(function (button) {
    button.disabled = locked || form.dataset.calendarResponseReady !== "true"
  })
  form.querySelectorAll("[data-calendar-response-load]").forEach(function (button) { button.disabled = locked })
  form.querySelectorAll("[data-mail-calendar-response-edit], [data-mail-calendar-response-cancel]").forEach(function (button) {
    button.disabled = locked || !!form._calendarResponseLoader || form.dataset.calendarResponseReady !== "true"
  })
  var progress = form.querySelector("[data-calendar-response-progress]")
  if (!form._calendarResponseHelp) form._calendarResponseHelp = progress.textContent
  progress.textContent = busy ? "Sending your response…" : form._calendarResponseLoader ? "Checking invitation…" : form._calendarResponseHelp
}

function setCalendarResponseError(form, message) {
  var error = form.querySelector("[data-calendar-response-error]")
  error.textContent = message || ""
  error.hidden = !message
  if (message && form.isConnected) error.focus()
}

document.body.addEventListener("htmx:beforeRequest", function (event) {
  var trigger = event.detail && event.detail.elt
  if (!trigger) return
  if (trigger.matches("[data-calendar-response-load]")) {
    var loading = trigger.closest("[data-calendar-response-form]")
    if (!loading || loading._calendarResponseBusy || loading._calendarResponseBlocked) { event.preventDefault(); return }
    event.detail.xhr.goferCalendarResponseLoader = loading
    loading._calendarResponseLoader = event.detail.xhr
    loading.dataset.calendarResponseReady = "false"
    setCalendarResponseError(loading, "")
    updateCalendarResponseForm(loading)
    return
  }
  if (!trigger.matches("[data-calendar-response-form]")) return
  var response = event.detail.requestConfig && event.detail.requestConfig.parameters.response
  if (trigger._calendarResponseBusy || trigger._calendarResponseBlocked || trigger.dataset.calendarResponseReady !== "true" || ["accepted", "tentative", "declined"].indexOf(response) < 0) { event.preventDefault(); return }
  event.detail.xhr.goferCalendarResponse = {form: trigger, response: response, scope: trigger.dataset.calendarResponseScope}
  trigger._calendarResponseBusy = true
  setCalendarResponseError(trigger, "")
  updateCalendarResponseForm(trigger)
})

document.body.addEventListener("htmx:beforeSwap", function (event) {
  var form = event.detail && event.detail.xhr && event.detail.xhr.goferCalendarResponseLoader
  if (!form) return
  var dialog = form.closest("[data-tui-dialog-content]")
  if (!form.isConnected || form._calendarResponseLoader !== event.detail.xhr || (dialog && (!dialog.open || dialog.hasAttribute("data-tui-dialog-closing")))) event.detail.shouldSwap = false
})

document.body.addEventListener("htmx:afterRequest", function (event) {
  var xhr = event.detail && event.detail.xhr
  var loading = xhr && xhr.goferCalendarResponseLoader
  if (loading) {
    delete xhr.goferCalendarResponseLoader
    if (loading._calendarResponseLoader !== xhr) return
    delete loading._calendarResponseLoader
    if (!event.detail.successful && loading.isConnected) {
      var message = xhr.status >= 400 && xhr.status < 500 && xhr.responseText && xhr.responseText.length < 600 ? xhr.responseText.trim() : "Could not check your response. Reopen the invitation or try the scope again."
      setCalendarResponseError(loading, message)
      updateCalendarResponseForm(loading)
    }
    return
  }
  var request = xhr && xhr.goferCalendarResponse
  if (!request) return
  delete xhr.goferCalendarResponse
  var form = request.form
  form._calendarResponseBusy = false
  try {
    var result = JSON.parse(xhr.responseText)
    if (!event.detail.successful) {
      form._calendarResponseBlocked = !!result.uncertain || !!result.conflict
      setCalendarResponseError(form, result.error || "Could not send your response. Try again.")
    } else {
      if ((result.responded !== true && result.pending !== true) || (result.responded === true && result.pending === true) || result.event_id !== form.dataset.calendarEventId || result.scope !== request.scope || result.response !== request.response) throw new Error("Unconfirmed response")
      form._calendarResponseBlocked = true
      if (result.delivery === "email") {
        if (result.pending !== true || !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(result.delivery_id || "")) throw new Error("Unconfirmed email reply")
        updateCalendarResponseForm(form)
        var deliveryRoot = form.closest("[data-calendar-response-container]")
        var deliveryDialog = form.closest("[data-tui-dialog]")
        if (form.isConnected && deliveryRoot && (!deliveryDialog || (deliveryDialog.open && !deliveryDialog.hasAttribute("data-tui-dialog-closing")))) {
          window.htmx.ajax("GET", "/api/calendar/replies/" + result.delivery_id, { target: deliveryRoot, swap: "innerHTML" }).catch(function () {
            setCalendarResponseError(form, "Reply queued. Reopen this invitation to check delivery; do not send it again.")
          })
        }
        showGoferToast({title: "Reply queued", description: "The email reply will send in the background. Reopen the invitation to check delivery.", variant: "info", duration: 5000})
        return
      }
      var responseDialog = form.closest("[data-tui-dialog]")
      if (form.isConnected && responseDialog && window.tui && window.tui.dialog) window.tui.dialog.close(responseDialog)
      var mailFooter = form.closest("[data-mail-calendar-footer]")
      var pending = result.pending || result.refresh_pending
      if (mailFooter && form.isConnected) {
        if (result.responded === true) {
          var summary = form.querySelector("[data-mail-calendar-response-summary]")
          var editor = form.querySelector("[data-mail-calendar-response-editor]")
          var chosen = form.querySelector('[data-calendar-response-choice="' + request.response + '"]')
          if (summary && editor && chosen) {
            summary.querySelector("[data-mail-calendar-response-message]").textContent = chosen.getAttribute("data-mail-calendar-response-message")
            summary.hidden = false
            editor.hidden = true
            form.querySelector("[data-calendar-response-progress]").hidden = true
          }
        }
        if (pending) {
          form._calendarResponseHelp = "Response submitted. Refresh to check its status before responding again."
          form.querySelector("[data-calendar-response-progress]").hidden = false
        } else {
          window.htmx.ajax("GET", mailFooter.getAttribute("hx-get"), {target: mailFooter, swap: "innerHTML"}).catch(function () {
            form._calendarResponseHelp = "Response saved. Refresh to update the event information."
            form.querySelector("[data-calendar-response-progress]").hidden = false
            updateCalendarResponseForm(form)
          })
        }
      }
      showGoferToast({title: result.pending ? "Response submitted" : "Response saved", description: result.pending ? "The provider is still processing your reply. Refresh the calendar to check its status; don't send it again." : result.refresh_pending ? "Your response was confirmed, but the calendar could not refresh yet." : request.scope === "series" ? "Your response to the series was confirmed." : "Your response was confirmed.", variant: pending ? "warning" : "success", icon: pending ? "warning" : "success", duration: pending ? 8000 : 4500})
      scheduleCalendarCacheRefresh()
    }
  } catch (_) {
    form._calendarResponseBlocked = true
    setCalendarResponseError(form, "The response could not be confirmed. Refresh the calendar and reopen the invitation before trying again.")
  }
  updateCalendarResponseForm(form)
})

function updateCalendarDeleteForm(form) {
  var busy = !!form._calendarDeleteBusy
  var locked = busy || !!form._calendarDeleteBlocked
  form.setAttribute("aria-busy", busy ? "true" : "false")
  form.querySelector("[data-calendar-delete-submit]").disabled = locked || form.dataset.calendarDeleteReady === "false"
  form.querySelector("[data-calendar-delete-cancel]").disabled = busy
  form.querySelector("[data-calendar-delete-spinner]").hidden = !busy
  form.querySelectorAll("[data-calendar-delete-load]").forEach(function (button) { button.disabled = locked })
  form.querySelector("[data-calendar-delete-label]").textContent = busy ? "Deleting…" : form.dataset.calendarDeleteSeries === "true" ? "Delete series" : "Delete event"
  var root = form.closest("[data-tui-dialog]")
  if (root) root.querySelectorAll("[data-calendar-edit-trigger], [data-calendar-delete-trigger]").forEach(function (button) { button.disabled = locked })
}

function setCalendarDeleteError(form, message) {
  var error = form.querySelector("[data-calendar-delete-error]")
  error.textContent = message || ""
  error.hidden = !message
  if (message && form.isConnected) error.focus()
}

document.addEventListener("click", function (event) {
  var cancel = event.target.closest && event.target.closest("[data-calendar-delete-cancel]")
  if (!cancel || !window.tui || !window.tui.popover) return
  var root = cancel.closest("[data-tui-popover-root]")
  window.tui.popover.closeElement(root)
  var trigger = root && root.querySelector("[data-calendar-delete-trigger]")
  if (trigger) trigger.focus()
})

document.body.addEventListener("htmx:beforeRequest", function (event) {
  var form = event.detail && event.detail.elt
  if (form && form.matches("[data-calendar-delete-load]")) {
    form = form.closest("[data-tui-popover-root]").querySelector("[data-calendar-delete-form]")
    if (!form || form._calendarDeleteBusy || form._calendarDeleteBlocked) { event.preventDefault(); return }
    event.detail.xhr.goferCalendarDeleteLoader = form
    form._calendarDeleteLoader = event.detail.xhr
    form.dataset.calendarDeleteReady = "false"
    form.querySelector("[data-calendar-delete-check]").hidden = false
    setCalendarDeleteError(form, "")
    updateCalendarDeleteForm(form)
    return
  }
  if (!form || !form.matches("[data-calendar-delete-form]")) return
  if (form._calendarDeleteBusy || form._calendarDeleteBlocked || form.dataset.calendarDeleteReady === "false") { event.preventDefault(); return }
  event.detail.xhr.goferCalendarDeleteForm = form
  form._calendarDeleteBusy = true
  setCalendarDeleteError(form, "")
  updateCalendarDeleteForm(form)
})

document.body.addEventListener("htmx:beforeSwap", function (event) {
  var form = event.detail && event.detail.xhr && event.detail.xhr.goferCalendarDeleteLoader
  if (form && (!form.isConnected || form._calendarDeleteLoader !== event.detail.xhr)) event.detail.shouldSwap = false
})

document.body.addEventListener("htmx:afterRequest", function (event) {
  var xhr = event.detail && event.detail.xhr
  var loading = xhr && xhr.goferCalendarDeleteLoader
  if (loading) {
    delete xhr.goferCalendarDeleteLoader
    if (loading._calendarDeleteLoader !== xhr) return
    delete loading._calendarDeleteLoader
    if (!event.detail.successful && loading.isConnected) {
      loading.querySelector("[data-calendar-delete-check]").hidden = true
      var message = xhr.status >= 400 && xhr.status < 500 && xhr.responseText && xhr.responseText.length < 600 ? xhr.responseText.trim() : "Could not check the selection. Choose the scope again or reopen this confirmation to retry."
      setCalendarDeleteError(loading, message)
      updateCalendarDeleteForm(loading)
    }
    return
  }
  var form = xhr && xhr.goferCalendarDeleteForm
  if (!form) return
  // Detached triggers can bubble afterRequest through their surviving ancestor.
  // Handle a response once, and never close a newer event dialog.
  delete xhr.goferCalendarDeleteForm
  form._calendarDeleteBusy = false
  try {
    var result = JSON.parse(xhr.responseText)
    if (!event.detail.successful) {
      form._calendarDeleteBlocked = !!result.uncertain || !!result.conflict
      setCalendarDeleteError(form, result.error || (form._calendarDeleteBlocked ? "Refresh the calendar and reopen the event before trying again." : "Could not delete the event. Try again."))
    } else {
      if (result.deleted !== true || result.event_id !== form.dataset.calendarEventId) throw new Error("Unconfirmed deletion")
      if (form.dataset.calendarDeleteSeries === "true" && (!result.series_id || result.series_id !== form.dataset.calendarDeleteSeriesId)) throw new Error("Unconfirmed series deletion")
      if (form.dataset.calendarDeleteOccurrence === "true" && result.scope !== "occurrence") throw new Error("Unconfirmed occurrence deletion")
      form._calendarDeleteBlocked = true
      if (form.isConnected && window.tui) {
        if (window.tui.popover) window.tui.popover.closeElement(form)
        if (window.tui.dialog) window.tui.dialog.close(form.closest("[data-tui-dialog]"))
      }
      showGoferToast({title: form.dataset.calendarDeleteSeries === "true" ? "Series deleted" : "Event deleted", description: form.dataset.calendarDeleteSeries === "true" ? "All occurrences were removed from your calendar." : form.dataset.calendarDeleteOccurrence === "true" ? "Only this occurrence was removed. The rest of the series is unchanged." : "Removed from your calendar.", variant: "success", icon: "success", duration: 4500})
      scheduleCalendarCacheRefresh()
    }
  } catch (_) {
    form._calendarDeleteBlocked = true
    setCalendarDeleteError(form, "The deletion could not be confirmed. Refresh the calendar and reopen the event before trying again.")
  }
  updateCalendarDeleteForm(form)
})

var _calendarSyncRequest = null
var _calendarSyncStates = new Map()
var _calendarCacheRequest = null
var _calendarCacheRefreshPending = false
var _calendarCacheRefreshTimer = null

function mergeCalendarSyncState(data) {
  var previous = _calendarSyncStates.get(data.source_id)
  data.attempt = Number(data.attempt) || 0
  if (previous && (previous.attempt > data.attempt || (previous.attempt === data.attempt &&
      (previous.state === "ok" || previous.state === "failed") && (data.state === "syncing" || data.state === "pending")))) return previous
  _calendarSyncStates.set(data.source_id, data)
  return data
}

function formatCalendarSyncTime(value, full) {
  var at = new Date(value)
  if (!value || isNaN(at.getTime())) return ""
  var calendar = document.getElementById("calendar-main")
  var zone = calendar && calendar.dataset.calendarTimezone
  var options = { hour: "2-digit", minute: "2-digit", hour12: false }
  if (full) { options.year = "numeric"; options.month = "short"; options.day = "numeric" }
  if (zone && zone !== "Local" && zone !== "local") options.timeZone = zone
  try { return at.toLocaleString("en-GB", options) } catch (_) { delete options.timeZone; return at.toLocaleString("en-GB", options) }
}

function updateCalendarSyncPresentation() {
  var calendar = document.getElementById("calendar-main")
  if (calendar && !calendar.hasAttribute("data-calendar-loading") && !calendar._calendarSyncMetadataLoaded) {
    calendar._calendarSyncMetadataLoaded = true
    var metadata
    try { metadata = JSON.parse(calendar.dataset.calendarSyncSources || "[]") } catch (_) { metadata = [] }
    if (Array.isArray(metadata)) {
      metadata.forEach(mergeCalendarSyncState)
      calendar._calendarSyncMetadataIDs = metadata.map(function (state) { return state.source_id })
    }
  }
  var sources = []
  document.querySelectorAll("[data-calendar-source-sync]").forEach(function (node) {
    var state = mergeCalendarSyncState({ source_id: node.dataset.calendarSourceSync,
      state: node.dataset.calendarSyncState || "pending", attempt: node.dataset.calendarSyncAttempt,
      error: node.dataset.calendarSourceError || "", last_synced_at: node.dataset.calendarLastSynced || "", next_attempt_at: node.dataset.calendarNextAttempt || "" })
    // A live event may be newer than the page that just arrived.
    node.dataset.calendarSyncState = state.state
    node.dataset.calendarSyncAttempt = String(state.attempt)
    node.dataset.calendarSourceError = state.error || ""
    node.dataset.calendarLastSynced = state.last_synced_at || ""
    node.dataset.calendarNextAttempt = state.next_attempt_at || ""
    var iconState = state.state === "ok" && !state.last_synced_at ? "pending" : state.state
    node.querySelectorAll("[data-calendar-source-sync-icon]").forEach(function (icon) { icon.hidden = icon.dataset.calendarSourceSyncIcon !== iconState })
    var title = state.state === "syncing" ? "Refreshing calendar…" : state.state === "failed" ?
      "Refresh failed: " + (state.error || "Could not connect.") + " Cached events remain available." : state.last_synced_at ?
      "Last synced " + formatCalendarSyncTime(state.last_synced_at, true) : "Waiting for first refresh"
    if (state.state === "failed" && state.next_attempt_at) title += " Retrying at " + formatCalendarSyncTime(state.next_attempt_at, false) + "."
    node.title = title
    node.setAttribute("aria-label", title)
    sources.push(state)
  })
  if (calendar && Array.isArray(calendar._calendarSyncMetadataIDs)) {
    sources = calendar._calendarSyncMetadataIDs.map(function (id) { return _calendarSyncStates.get(id) }).filter(Boolean)
  }
  if (!calendar || calendar.hasAttribute("data-calendar-loading") || !sources.length) return
  var busy = !!_calendarSyncRequest || sources.some(function (state) { return state.state === "syncing" })
  var failed = sources.filter(function (state) { return state.state === "failed" }).length
  var pending = sources.some(function (state) { return !state.last_synced_at })
  var last = sources.reduce(function (oldest, state) {
    var stamp = Date.parse(state.last_synced_at)
    return isNaN(stamp) ? oldest : Math.min(oldest, stamp)
  }, Infinity)
  var synced = !pending && isFinite(last)
  _setCalendarSyncBusy(busy)
  var message = busy ? "Refreshing calendars…" : failed ? failed + " calendar(s) could not refresh. Retrying automatically." :
    synced ? "Last synced " + formatCalendarSyncTime(last, false) : "Waiting for first refresh"
  document.querySelectorAll("[data-calendar-sync-status]").forEach(function (node) {
    node.textContent = message
    node.parentElement.className = "inline-flex items-center gap-1.5 " + (busy ? "text-muted-foreground" : failed ? "text-destructive" : synced ? "text-emerald-700 dark:text-emerald-300" : "text-muted-foreground")
  })
  document.querySelectorAll("[data-calendar-sync-label]").forEach(function (node) {
    node.textContent = busy ? "Refreshing calendars" : failed ? "Calendar sync needs attention" : synced ? "Calendar synchronized" : "Calendar sync pending"
    node.parentElement.className = "flex items-center gap-2 text-xs font-semibold " + (!busy && failed ? "text-destructive" : "text-foreground")
  })
  document.querySelectorAll("[data-calendar-sync-dot]").forEach(function (node) {
    node.className = "size-2 rounded-full " + (busy ? "bg-primary animate-pulse" : failed ? "bg-destructive" : synced ? "bg-emerald-500" : "bg-muted-foreground/50")
  })
  document.querySelectorAll("[data-calendar-sync-detail]").forEach(function (node) {
    node.textContent = busy ? "Fetching events from your selected calendars." : failed ? "Previously cached events remain available. " + message :
      synced ? message + ". Background refresh is enabled." : "Waiting for the first successful refresh. Cached events remain available."
  })
}

function handleCalendarSyncEvent(data) {
  if (!data || typeof data.source_id !== "string" || !["pending", "syncing", "ok", "failed"].includes(data.state)) return
  var previous = _calendarSyncStates.get(data.source_id)
  var current = mergeCalendarSyncState(data)
  updateCalendarSyncPresentation()
  if (current !== data || data.state !== "ok") return
  if (data.changed || (data.snapshot && previous && Date.parse(data.last_synced_at) > (Date.parse(previous.last_synced_at) || 0))) scheduleCalendarCacheRefresh()
}

function scheduleCalendarCacheRefresh() {
  _calendarCacheRefreshPending = true
  if (_calendarCacheRefreshTimer) return
  _calendarCacheRefreshTimer = setTimeout(function () {
    _calendarCacheRefreshTimer = null
    var calendar = document.getElementById("calendar-main")
    if (!calendar) { _calendarCacheRefreshPending = false; return }
    if (calendar.hasAttribute("data-calendar-loading") || _calendarContentRequest || _calendarSyncRequest || _calendarCacheRequest) return
    var trigger = calendar.querySelector("[data-calendar-cache-refresh]")
    if (!trigger || !window.htmx) return
    _calendarCacheRefreshPending = false
    window.htmx.trigger(trigger, "calendar-cache-refresh")
  }, 180)
}

function configureCalendarCacheRequest(event) {
  var calendar = document.getElementById("calendar-main")
  if (!calendar) return
  _calendarCacheRequest = event.detail.xhr
  _calendarCacheRequest.goferCalendarCachePeriod = calendar.dataset.calendarPeriod
}

function calendarCacheResponseCurrent(xhr) {
  var calendar = document.getElementById("calendar-main")
  return xhr === _calendarCacheRequest && calendar && !calendar.hasAttribute("data-calendar-loading") && calendar.dataset.calendarPeriod === xhr.goferCalendarCachePeriod
}

document.body.addEventListener("htmx:beforeSwap", function (event) {
  var xhr = event.detail && event.detail.xhr
  if (!xhr || typeof xhr.goferCalendarCachePeriod !== "string") return
  if (!calendarCacheResponseCurrent(xhr)) { event.detail.shouldSwap = false; return }
  var calendar = document.getElementById("calendar-main")
  var agenda = document.getElementById("calendar-agenda-list")
  xhr.goferCalendarAgendaScroll = agenda ? agenda.scrollTop : 0
  var scroller = calendar.querySelector("[data-calendar-week-scroll]")
  if (scroller) _calendarWeekScrollState = { period: calendar.dataset.calendarPeriod, zoom: _calendarWeekZoom, top: scroller.scrollTop, left: scroller.scrollLeft }
})

document.body.addEventListener("htmx:afterSwap", function (event) {
  var xhr = event.detail && event.detail.xhr
  if (xhr && typeof xhr.goferCalendarCachePeriod === "string" && calendarCacheResponseCurrent(xhr)) {
    var agenda = document.getElementById("calendar-agenda-list")
    if (agenda) agenda.scrollTop = xhr.goferCalendarAgendaScroll || 0
  }
  updateCalendarSyncPresentation()
  if (_calendarCacheRefreshPending) scheduleCalendarCacheRefresh()
})

function finishCalendarCacheRequest(event) {
  var xhr = event.detail && event.detail.xhr
  if (!xhr || xhr !== _calendarCacheRequest) return
  _calendarCacheRequest = null
  if (_calendarCacheRefreshPending) scheduleCalendarCacheRefresh()
}
document.body.addEventListener("htmx:afterRequest", finishCalendarCacheRequest)
document.body.addEventListener("htmx:sendAbort", finishCalendarCacheRequest)
if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", updateCalendarSyncPresentation)
else updateCalendarSyncPresentation()

function configureCalendarSyncRequest(event) {
  var calendar = document.getElementById("calendar-main")
  if (calendar && event.detail) {
    event.detail.parameters.month = calendar.dataset.calendarMonth
    event.detail.parameters.view = calendar.dataset.calendarView
    event.detail.parameters.date = calendar.dataset.calendarDate
  }
}

function _setCalendarSyncBusy(busy) {
  var accountID = _calendarSyncRequest && _calendarSyncRequest.goferCalendarSyncAccount
  document.querySelectorAll("[data-calendar-sync-button]").forEach(function (button) {
    var buttonAccountID = button.dataset && button.dataset.calendarAccountSyncButton
    var active = busy && (!accountID || !buttonAccountID || buttonAccountID === accountID)
    button.disabled = busy
    button.setAttribute("aria-busy", active ? "true" : "false")
    var icon = button.querySelector("svg")
    if (icon) icon.classList.toggle("animate-spin", active)
  })
  if (!busy) return
  document.querySelectorAll("[data-calendar-sync-status]").forEach(function (status) {
    status.textContent = "Refreshing calendars…"
    status.parentElement.className = "inline-flex items-center gap-1.5 text-muted-foreground"
  })
  document.querySelectorAll("[data-calendar-sync-label]").forEach(function (label) {
    label.textContent = "Refreshing calendars"
    label.parentElement.className = "flex items-center gap-2 text-xs font-semibold text-foreground"
  })
  document.querySelectorAll("[data-calendar-sync-dot]").forEach(function (dot) {
    dot.className = "size-2 rounded-full bg-primary animate-pulse"
  })
  document.querySelectorAll("[data-calendar-sync-detail]").forEach(function (detail) {
    detail.textContent = "Fetching events from your selected calendars."
  })
}

function handleCalendarSyncStart(event) {
  _calendarSyncRequest = event.detail.xhr
  var button = event.detail.elt
  _calendarSyncRequest.goferCalendarSyncAccount = button && button.getAttribute("data-calendar-account-sync-button") || ""
  var menu = button && button.closest("[data-tui-popover-content]")
  if (menu && window.tui && window.tui.popover) window.tui.popover.closeElement(menu)
  var calendar = document.getElementById("calendar-main")
  _calendarSyncRequest.goferCalendarSyncPeriod = calendar ? calendar.dataset.calendarPeriod : ""
  _setCalendarSyncBusy(true)
}

document.body.addEventListener("htmx:beforeSwap", function (event) {
  var xhr = event.detail && event.detail.xhr
  if (!xhr || typeof xhr.goferCalendarSyncPeriod !== "string") return
  var calendar = document.getElementById("calendar-main")
  if (xhr !== _calendarSyncRequest || !calendar || calendar.dataset.calendarPeriod !== xhr.goferCalendarSyncPeriod) {
    event.detail.shouldSwap = false
  }
})

function handleCalendarSyncAbort(event) {
  if (_calendarSyncRequest !== event.detail.xhr) return
  _calendarSyncRequest = null
  _setCalendarSyncBusy(false)
  updateCalendarSyncPresentation()
  if (_calendarCacheRefreshPending) scheduleCalendarCacheRefresh()
}

function handleCalendarSyncResult(event) {
  if (_calendarSyncRequest !== event.detail.xhr) return
  _calendarSyncRequest = null
  _setCalendarSyncBusy(false)
  updateCalendarSyncPresentation()
  if (_calendarCacheRefreshPending) scheduleCalendarCacheRefresh()
  if (event.detail.successful) return
  var message = "Calendar refresh failed. Cached events are still available."
  document.querySelectorAll("[data-calendar-sync-status]").forEach(function (status) {
    status.textContent = message
    status.parentElement.className = "inline-flex items-center gap-1.5 text-destructive"
  })
  document.querySelectorAll("[data-calendar-sync-label]").forEach(function (label) {
    label.textContent = "Calendar refresh failed"
    label.parentElement.className = "flex items-center gap-2 text-xs font-semibold text-destructive"
  })
  document.querySelectorAll("[data-calendar-sync-dot]").forEach(function (dot) {
    dot.className = "size-2 rounded-full bg-destructive"
  })
  document.querySelectorAll("[data-calendar-sync-detail]").forEach(function (detail) {
    detail.textContent = message
  })
}

function handleContactSidebarSyncStart(event) {
  var button = _contactSidebarSyncButtonFromContext(event)
  _clearContactSidebarSyncButtons()
  if (button) {
    _contactSidebarSyncActiveButton = button
    _setContactSidebarSyncButtonBusy(button, true)
  }
  var label = _contactSidebarSyncButtonLabel(button)
  showGoferToast({
    id: "contact-sync-toast",
    title: "Syncing contacts",
    description: label ? "Checking " + label + " address books..." : "Checking connected address books...",
    variant: "info",
    icon: "spinner",
    position: "bottom-right",
    duration: 0,
    dismissible: false,
  })
}

function handleContactSidebarSyncResult(event) {
  var xhr = event && event.detail ? event.detail.xhr : null
  var button = _contactSidebarSyncButtonFromContext(event) || _contactSidebarSyncActiveButton
  _setContactSidebarSyncButtonBusy(button, false)
  _contactSidebarSyncActiveButton = null
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
  syncComposeAccountItems(fromPane ? "pane" : "dialog", accountId)
  _markComposeDirty(document.getElementById(prefix + "form"))
  applyDefaultComposeSignature(document.getElementById(prefix + "form"), true)
}

function syncComposeAccountItems(scope, accountId) {
  if (!scope || !accountId) return
  var items = document.querySelectorAll('[data-compose-account-scope="' + scope + '"][data-compose-account-item]')
  for (var i = 0; i < items.length; i++) {
    items[i].dataset.composeAccountSelected = items[i].dataset.accountId === accountId ? "true" : "false"
  }
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
  setComposeKind(form, "new")
  var editor = form.querySelector("[data-compose-editor]")
  if (editor) editor.innerHTML = ""
  syncComposeInlineImageInputs(form)
  var recipientFields = form.querySelectorAll("[data-compose-recipient-field]")
  for (var i = 0; i < recipientFields.length; i++) renderComposeRecipientField(recipientFields[i], "")
  renderComposeAttachments(form, [])
  form.dataset.composeUploadsPending = "0"
  form.dataset.composeSending = "false"
  delete form.dataset.composeOutgoingStatus
  delete form.dataset.composeUploadFailed
  form.dataset.composeDirty = "false"
  updateComposeSendState(form)
  _setComposeDraftButtonState(form, "default")
}

function composeModeForForm(form) {
  var field = form && form.querySelector('input[name="compose_mode"]')
  return (field && field.value) || "new"
}

// The compose title names the kind of message being written. Reply-all is stored as
// compose mode "reply", so the form also keeps the kind it was opened as.
var COMPOSE_TITLES = { "new": "New message", reply: "Reply", "reply-all": "Reply all", forward: "Forward" }

function setComposeKind(form, kind) {
  if (!form) return
  form.dataset.composeKind = COMPOSE_TITLES[kind] ? kind : "new"
  var root = _composeRootForForm(form)
  if (!root) return
  var titles = root.querySelectorAll("[data-compose-title]")
  for (var i = 0; i < titles.length; i++) titles[i].textContent = COMPOSE_TITLES[form.dataset.composeKind]
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

function _composeRootForForm(form) {
  if (!form) return
  return form.id === "compose-pane-form" ? form.closest("[data-compose-pane]") : document.getElementById("compose-dialog")
}

function _composeDraftButton(form) {
  var root = _composeRootForForm(form)
  return root ? root.querySelector("[data-compose-draft-button]") : null
}

var COMPOSE_MOBILE_DRAFT_STATUS = { saving: "Saving draft…", saved: "Draft saved", failed: "Draft not saved", empty: "Nothing to save" }

function _setComposeDraftButtonState(form, state) {
  // The phone compose bar has no draft button; it shows the same states under its title.
  var root = _composeRootForForm(form)
  var mobileStatus = root && root.querySelector("[data-compose-mobile-status]")
  if (mobileStatus) {
    mobileStatus.textContent = COMPOSE_MOBILE_DRAFT_STATUS[state] || ""
    mobileStatus.dataset.state = state || "default"
  }
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
  var pending = _composePendingUploads(form)
  var sending = form.dataset.composeSending === "true"
  var disabled = pending > 0 || sending
  var root = _composeRootForForm(form)
  var mobileSend = root && root.querySelector("[data-compose-mobile-send]")
  if (mobileSend) {
    mobileSend.disabled = disabled
    mobileSend.setAttribute("aria-busy", disabled ? "true" : "false")
  }
  var button = _composeSendButton(form)
  if (!button) return
  button.disabled = disabled
  button.setAttribute("aria-busy", sending || pending > 0 ? "true" : "false")
  button.classList.toggle("opacity-60", disabled)
  button.classList.toggle("cursor-not-allowed", disabled)
  if (sending) {
    button.title = "Sending..."
  } else if (pending > 0) {
    button.title = "Waiting for uploads to finish"
  } else if (form.dataset.composeOutgoingStatus) {
    button.title = form.dataset.composeOutgoingStatus
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
  if (state.sendID) stopOutgoingSendPolling(state.sendID)
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

function handleComposeSendResult(status, data) {
  if (!_composeSendState) return
  var state = _composeSendState
  if (data && data.send_id) {
    if (state.sendID && state.sendID !== data.send_id) return
    state.sendID = data.send_id
  }
  var form = document.getElementById(state.formId)
  if (status === "sent") {
    finishComposeSendSuccess(state)
    return
  }
  if (status === "retrying") {
    if (form) {
      _setComposeSending(form, false)
      form.dataset.composeOutgoingStatus = "Retrying"
      form.dataset.composeDirty = "true"
    }
    return
  }
  if (form) {
    _setComposeSending(form, false)
    form.dataset.composeOutgoingStatus = status === "ambiguous" ? "Needs review" : "Failed"
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

function composeViewPreference(kind) {
  // The pane and full-width views need the list beside them; small screens use the dialog.
  if (window.matchMedia && window.matchMedia("(max-width: 1023.98px)").matches) return "dialog"
  var key = kind === "reply" ? "default_reply_compose_view" : "default_new_compose_view"
  var view = window.GoferSettings ? GoferSettings.get(key) : null
  if (!view && window.GoferSettings) view = GoferSettings.get("default_compose_view")
  return view === "pane" || view === "full" ? view : "dialog"
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
	  var view = composeViewPreference(vals.compose_mode === "reply" || vals.compose_mode === "forward" ? "reply" : "new")
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

// The phone compose bar's "Send later" opens the footer's schedule picker, which stays
// in the page (hidden) on small screens and opens as a bottom sheet. It runs after the
// menu's own click has closed the menu.
document.addEventListener("click", function (e) {
  if (!e.target || !e.target.closest || !e.target.closest("[data-compose-mobile-send-later]")) return
  setTimeout(function () {
    var trigger = document.querySelector('#compose-dialog .compose-dialog-footer [title="Schedule send"]')
    if (trigger) trigger.click()
  }, 0)
})

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
  delete form.dataset.composeOutgoingStatus
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
  }).then(function (data) {
    if (data && data.send_id && _composeSendState) {
      _composeSendState.sendID = data.send_id
      startOutgoingSendPolling(data.send_id)
      fetchOutgoingSendStatus(data.send_id).catch(function () {})
    }
    return data
  }).catch(function (err) {
    if (_composeSendState && _composeSendState.sendID) stopOutgoingSendPolling(_composeSendState.sendID)
    _setComposeSending(form, false)
    _composeSendState = null
    form.dataset.composeDirty = "true"
    showSendStatus("failed", err && err.message ? err.message : "Failed to connect to server")
  })
}

function prepareComposeSchedule(button, fromPane) {
  var form = document.getElementById(fromPane ? "compose-pane-form" : "compose-form")
  if (!form) return false
  var root = button && button.closest ? button.closest("[data-tui-popover-root]") : null
  var panel = root && root.querySelector("[data-compose-schedule-panel]")
  if (!panel) return true
  var dateInput = panel.querySelector("[data-tui-calendar-hidden-input]")
  var timeInputs = getComposeScheduleTimeInputs(panel)
  var timezone = getGoferTimezone()
  var date = new Date(Date.now() + 60 * 60 * 1000)
  date = new Date(Math.ceil(date.getTime() / (5 * 60 * 1000)) * 5 * 60 * 1000)
  var zoned = datePartsInTimezone(date, timezone)
  if (timeInputs.hour && !timeInputs.hour.value) timeInputs.hour.value = pad2(zoned.hour)
  if (timeInputs.minute && !timeInputs.minute.value) timeInputs.minute.value = pad2(zoned.minute)
  updateComposeScheduleTimezone(panel)
  if (dateInput && !dateInput.value) {
    var desiredDate = formatDateFromParts(zoned)
    selectComposeScheduleDate(panel, desiredDate)
  }
  disableComposeSchedulePastDates(panel)
  return true
}

document.addEventListener("click", function (event) {
  var button = event.target && event.target.closest ? event.target.closest("[data-tui-calendar-prev], [data-tui-calendar-next], [data-tui-calendar-day]") : null
  if (!button) return
  var panel = button.closest("[data-compose-schedule-panel]")
  if (!panel) return
  requestAnimationFrame(function () { disableComposeSchedulePastDates(panel) })
})

function closeComposeSchedulePopover(el) {
  var content = el && el.closest && el.closest("[data-tui-popover-content]")
  if (content && content.hidePopover) content.hidePopover()
  if (content) content.setAttribute("data-tui-popover-open", "false")
}

function pad2(n) {
  return String(n).padStart(2, "0")
}

function getGoferTimezone() {
  var timezone = window.GoferSettings ? GoferSettings.get("timezone") : null
  if (!timezone || timezone === "local") {
    try { timezone = Intl.DateTimeFormat().resolvedOptions().timeZone } catch (_) {}
  }
  return timezone || "UTC"
}

function datePartsInTimezone(date, timezone) {
  var parts = {}
  try {
    var formatter = new Intl.DateTimeFormat("en-US", {
      timeZone: timezone,
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
      hourCycle: "h23"
    })
    formatter.formatToParts(date).forEach(function (part) {
      if (part.type !== "literal") parts[part.type] = parseInt(part.value, 10)
    })
  } catch (_) {}
  if (!parts.year) {
    parts = {
      year: date.getFullYear(),
      month: date.getMonth() + 1,
      day: date.getDate(),
      hour: date.getHours(),
      minute: date.getMinutes(),
      second: date.getSeconds()
    }
  }
  return parts
}

function formatDateFromParts(parts) {
  return parts.year + "-" + pad2(parts.month) + "-" + pad2(parts.day)
}

function selectComposeScheduleDate(panel, dateValue) {
  var dateInput = panel && panel.querySelector("[data-tui-calendar-hidden-input]")
  if (dateInput) dateInput.value = dateValue

  var calendar = panel && panel.querySelector("[data-tui-calendar-container]")
  if (!calendar) return

  var parts = String(dateValue || "").split("-")
  if (parts.length !== 3) return
  var year = parseInt(parts[0], 10)
  var month = parseInt(parts[1], 10) - 1
  var day = parseInt(parts[2], 10)
  if ([year, month, day].some(function (value) { return isNaN(value) })) return

  var monthSelect = calendar.querySelector("[data-tui-calendar-month-select]")
  var yearSelect = calendar.querySelector("[data-tui-calendar-year-select]")
  if (monthSelect && monthSelect.value !== String(month)) {
    monthSelect.value = String(month)
    monthSelect.dispatchEvent(new Event("change", { bubbles: true }))
  }
  if (yearSelect && yearSelect.value !== String(year)) {
    yearSelect.value = String(year)
    yearSelect.dispatchEvent(new Event("change", { bubbles: true }))
  }

  var dayButton = calendar.querySelector('[data-tui-calendar-day="' + day + '"]')
  if (dayButton && !dayButton.disabled) {
    dayButton.click()
    return
  }

  calendar.setAttribute("data-tui-calendar-selected-date", dateValue)
}

function timezoneOffsetMillis(date, timezone) {
  var parts = datePartsInTimezone(date, timezone)
  var asUTC = Date.UTC(parts.year, parts.month - 1, parts.day, parts.hour || 0, parts.minute || 0, parts.second || 0)
  return asUTC - date.getTime()
}

function zonedDateTimeToDate(dateValue, hourValue, minuteValue, timezone) {
  var dateParts = String(dateValue || "").split("-")
  if (dateParts.length !== 3) return new Date(NaN)
  var year = parseInt(dateParts[0], 10)
  var month = parseInt(dateParts[1], 10)
  var day = parseInt(dateParts[2], 10)
  var hour = parseInt(hourValue, 10)
  var minute = parseInt(minuteValue, 10)
  if ([year, month, day, hour, minute].some(function (value) { return isNaN(value) })) return new Date(NaN)
  var wallUTC = Date.UTC(year, month - 1, day, hour, minute, 0)
  var offset = timezoneOffsetMillis(new Date(wallUTC), timezone)
  var scheduled = new Date(wallUTC - offset)
  var corrected = timezoneOffsetMillis(scheduled, timezone)
  if (corrected !== offset) scheduled = new Date(wallUTC - corrected)
  return scheduled
}

function timezoneOffsetLabel(timezone, date) {
  var offset = Math.round(timezoneOffsetMillis(date || new Date(), timezone) / 60000)
  var sign = offset >= 0 ? "+" : "-"
  var absolute = Math.abs(offset)
  var hours = Math.floor(absolute / 60)
  var minutes = absolute % 60
  return "UTC" + sign + hours + (minutes ? ":" + pad2(minutes) : "")
}

function formatGoferDateTime(date, options) {
  options = options || {}
  try {
    return new Intl.DateTimeFormat(undefined, Object.assign({
      timeZone: getGoferTimezone(),
      month: "short",
      day: "numeric",
      hour: "numeric",
      minute: "2-digit"
    }, options)).format(date)
  } catch (_) {
    return date.toLocaleString()
  }
}

function disableComposeSchedulePastDates(panel) {
  var calendar = panel && panel.querySelector("[data-tui-calendar-container]")
  if (!calendar) return
  var month = parseInt(calendar.dataset.tuiCalendarCurrentMonth, 10)
  var year = parseInt(calendar.dataset.tuiCalendarCurrentYear, 10)
  if (isNaN(month) || isNaN(year)) return

  var today = datePartsInTimezone(new Date(), getGoferTimezone())
  var todayKey = today.year * 10000 + today.month * 100 + today.day
  var days = calendar.querySelectorAll("[data-tui-calendar-day]")
  for (var i = 0; i < days.length; i++) {
    var day = parseInt(days[i].dataset.tuiCalendarDay, 10)
    if (isNaN(day)) continue
    var dayKey = year * 10000 + (month + 1) * 100 + day
    var disabled = dayKey < todayKey
    days[i].disabled = disabled
    days[i].setAttribute("aria-disabled", disabled ? "true" : "false")
    days[i].classList.toggle("pointer-events-none", disabled)
    days[i].classList.toggle("text-muted-foreground/35", disabled)
    if (disabled) {
      days[i].classList.remove("hover:bg-accent", "hover:text-accent-foreground")
    }
  }
}

function getComposeScheduleTimeInputs(panel) {
  return {
    hour: panel && panel.querySelector('[data-compose-schedule-hour] input[type="hidden"]'),
    minute: panel && panel.querySelector('[data-compose-schedule-minute] input[type="hidden"]')
  }
}

function updateComposeScheduleTimezone(panel) {
  var timezoneEl = panel && panel.querySelector("[data-compose-schedule-timezone]")
  if (!timezoneEl) return
  var label = timezoneOffsetLabel(getGoferTimezone(), new Date())
  timezoneEl.textContent = label
  timezoneEl.parentElement.title = "Timezone: " + getGoferTimezone() + " (" + label + ")"
}

function scheduleCompose(fromPane, trigger) {
  var formId = fromPane ? "compose-pane-form" : "compose-form"
  var form = document.getElementById(formId)
  if (!form) return
  if (_composePendingUploads(form) > 0) {
    showSendStatus("failed", "Wait for uploads to finish before scheduling")
    return
  }
  if (form.dataset.composeSending === "true") return
  if (!finalizeComposeRecipients(form)) {
    showSendStatus("failed", "Fix invalid recipient addresses before scheduling")
    return
  }
  _syncComposeFormEditor(form)
  if (!validateComposeMessageSize(form)) return

  var toField = form.querySelector('input[name="to"]')
  if (!toField || !toField.value.trim()) {
    showSendStatus("failed", "Please enter at least one recipient.")
    return
  }

  var panel = trigger && trigger.closest ? trigger.closest("[data-compose-schedule-panel]") : null
  var dateInput = panel && panel.querySelector("[data-tui-calendar-hidden-input]")
  var timeInputs = getComposeScheduleTimeInputs(panel)
  var dateValue = dateInput && dateInput.value
  var hourValue = timeInputs.hour && timeInputs.hour.value
  var minuteValue = timeInputs.minute && timeInputs.minute.value
  var timeValue = hourValue && minuteValue ? hourValue + ":" + minuteValue : ""
  if (!dateValue) {
    showSendStatus("failed", "Choose a send date")
    return
  }
  if (!timeValue) {
    showSendStatus("failed", "Choose a send time")
    return
  }
  var scheduledAt = zonedDateTimeToDate(dateValue, hourValue, minuteValue, getGoferTimezone())
  if (isNaN(scheduledAt.getTime())) {
    showSendStatus("failed", "Choose a valid schedule time")
    return
  }
  if (scheduledAt.getTime() <= Date.now() + 30000) {
    showSendStatus("failed", "Choose a time at least 1 minute in the future.")
    return
  }
  var params = new URLSearchParams()
  var inputs = form.querySelectorAll("input, textarea")
  for (var i = 0; inputs && i < inputs.length; i++) {
    if (inputs[i].name) params.append(inputs[i].name, inputs[i].value)
  }
  params.set("schedule_date", dateValue)
  params.set("schedule_hour", hourValue)
  params.set("schedule_minute", minuteValue)
  params.set("schedule_timezone", getGoferTimezone())

  closeComposeSchedulePopover(trigger)
  showSendStatus("sending", "Scheduling...")
  _setComposeSending(form, true)

  fetch("/compose/schedule", {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: params.toString()
  }).then(function (r) {
    if (!r.ok) {
      return r.json().catch(function () { return {} }).then(function (data) {
        throw new Error(data.error || "Failed to schedule message")
      })
    }
    return r.json().catch(function () { return {} })
  }).then(function (data) {
    _setComposeSending(form, false)
    form.dataset.composeDirty = "false"
    var labelDate = data && data.scheduled_for ? new Date(data.scheduled_for) : scheduledAt
    showSendStatus("scheduled", "Will send " + formatGoferDateTime(labelDate))
    setTimeout(function () {
      if (fromPane) {
        setMailViewEmpty()
        _updateComposeBtn(false)
      } else {
        resetComposeForm(false)
        if (window.tui && window.tui.dialog) window.tui.dialog.close("compose-dialog")
        _updateComposeBtn(false)
      }
    }, 250)
  }).catch(function (err) {
    _setComposeSending(form, false)
    form.dataset.composeDirty = "true"
    showSendStatus("failed", err && err.message ? err.message : "Failed to schedule message")
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
  syncComposeAccountItems(pane ? "pane" : "dialog", accountId)
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
  if (mode === "new") {
    var newToInput = form.querySelector('[data-recipient-name="to"] [data-compose-recipient-input]')
    var subjectInput = form.querySelector('input[name="subject"]')
    var toValue = form.querySelector('input[name="to"]')
    if ((!toValue || !String(toValue.value || "").trim()) && newToInput) {
      newToInput.focus()
      return
    }
    if (subjectInput && !String(subjectInput.value || "").trim()) {
      subjectInput.focus()
      return
    }
  }
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
  setComposeMode(form, mode === "forward" ? "forward" : (mode === "new" ? "new" : "reply"))
  setComposeKind(form, mode)
  _showComposeOptionalFields(form, vals)
  setComposeAccount(form, vals.account_id)
  applyDefaultComposeSignature(form, true)
  focusComposePrefill(form, mode)
}

function openComposePrefill(vals, mode) {
  if (!_activeComposeCanBeReplaced()) return false
  var activePane = document.querySelector("[data-compose-pane]")
  if (activePane) {
    writeComposePrefill(document.getElementById("compose-pane-form"), vals, "compose-pane-", mode)
    return true
  }
  var view = composeViewPreference(mode === "new" ? "new" : "reply")
  if ((view === "pane" || view === "full") && document.getElementById("mail-list") && document.getElementById("mail-view")) {
    fetch("/compose/pane").then(function (r) { return r.text() }).then(function (html) {
      writeComposePane(html, vals, view === "full", view === "full")
      writeComposePrefill(document.getElementById("compose-pane-form"), vals, "compose-pane-", mode)
    }).catch(function () {})
    return true
  }
  var form = document.getElementById("compose-form")
  writeComposePrefill(form, vals, "compose-", mode)
  if (window.tui && window.tui.dialog) window.tui.dialog.open("compose-dialog")
  return true
}

function mailtoDecodedPath(pathname) {
  try {
    return decodeURIComponent(pathname || "")
  } catch (_) {
    return pathname || ""
  }
}

function parseMailtoIntent(raw) {
  raw = String(raw || "").trim()
  if (!raw || raw.length > 65536 || !/^mailto:/i.test(raw)) return null

  var parsed
  try {
    parsed = new URL(raw)
  } catch (_) {
    return null
  }
  if (String(parsed.protocol || "").toLowerCase() !== "mailto:") return null

  var headers = Object.create(null)
  parsed.searchParams.forEach(function (value, key) {
    key = String(key || "").toLowerCase()
    if (!headers[key]) headers[key] = []
    headers[key].push(value)
  })

  function joinedHeader(name) {
    return (headers[name] || []).filter(function (value) { return String(value || "").trim() }).join(", ")
  }

  var pathRecipients = mailtoDecodedPath(parsed.pathname)
  var to = [pathRecipients, joinedHeader("to")].filter(Boolean).join(", ")
  var cc = joinedHeader("cc")
  var bcc = joinedHeader("bcc")
  return {
    draft_id: "",
    to: to,
    cc: cc,
    bcc: bcc,
    subject: (headers.subject && headers.subject[0]) || "",
    body: (headers.body && headers.body[0]) || "",
    html_body: "",
    compose_mode: "new",
    in_reply_to: "",
    references: "",
    attachments: [],
    inline_images: [],
    _handlerTestToken: (headers["x-gofer-handler-test"] && headers["x-gofer-handler-test"][0]) || "",
    _ccVisible: !!cc,
    _bccVisible: !!bcc,
    _composeDirty: "true"
  }
}

function clearMailtoIntentFromURL() {
  try {
    var clean = new URL(window.location.href)
    clean.searchParams.delete("mailto")
    window.history.replaceState(window.history.state, "", clean.pathname + clean.search + clean.hash)
  } catch (_) {}
}

function setupMailtoIntent() {
  var params
  try {
    params = new URL(window.location.href).searchParams
  } catch (_) {
    return
  }
  if (!params.has("mailto")) return

  var values = parseMailtoIntent(params.get("mailto"))
  if (!values) {
    clearMailtoIntentFromURL()
    if (typeof showGoferToast === "function") {
      showGoferToast({
        id: "mailto-intent-error",
        title: "Could not open email link",
        description: "The mail link is invalid or too large.",
        variant: "error",
        icon: "error",
        position: "bottom-right",
        duration: 6000,
        dismissible: true,
      })
    }
    return
  }

  var handlerTest = false
  try {
    var expectedTestToken = window.localStorage.getItem("gofer_mailto_handler_test_token") || ""
    handlerTest = !!values._handlerTestToken && values._handlerTestToken === expectedTestToken
    if (handlerTest) window.localStorage.removeItem("gofer_mailto_handler_test_token")
    window.localStorage.setItem("gofer_mailto_handler_state", JSON.stringify({ status: "confirmed", updated_at: Date.now() }))
  } catch (_) {}

  if (handlerTest) {
    clearMailtoIntentFromURL()
    if (typeof showGoferToast === "function") {
      showGoferToast({
        id: "mailto-handler-confirmed",
        title: "Gofer opened the email link",
        description: "Email-link handling was confirmed on this browser.",
        variant: "success",
        icon: "success",
        position: "bottom-right",
        duration: 6000,
        dismissible: true,
      })
    }
    return
  }

  if (openComposePrefill(values, "new")) clearMailtoIntentFromURL()
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
  var view = composeViewPreference("new")
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
  var raw = String(value || "50%").trim()
  if (raw.charAt(raw.length - 1) === "%") {
    var percent = parseFloat(raw)
    if (!isNaN(percent) && percent > 0) return "clamp(300px," + percent + "%,calc(100% - 300px))"
  }
  var width = parseFloat(raw)
  if (isNaN(width) || width <= 0) return "clamp(300px,50%,calc(100% - 300px))"
  return Math.max(300, width) + "px"
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
  '<div id="mail-view" class="hidden lg:flex flex-1 flex-col min-w-0 bg-background surface-desk" data-mail-reader>' + composeOpeningHTML() + '</div>'
}

function composeOpeningFullShellHTML() {
  return '<div id="mail-list" class="w-full lg:flex flex-col border-r border-border bg-card h-full overflow-hidden" style="display:none;width:0px;opacity:0;overflow:hidden;border-width:0"></div>' +
    '<div class="resize-handle" data-panel="maillist" draggable="false" style="display:none;opacity:0"></div>' +
    '<div id="mail-view" class="hidden lg:flex flex-1 flex-col min-w-0 bg-background surface-desk" data-mail-reader>' + composeOpeningHTML() + '</div>'
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

  if (!fullWidth) {
    document.body.addEventListener("htmx:beforeSwap", beforeMainContentSwap)
    document.body.addEventListener("htmx:afterSwap", afterMainContentSwap)
  }
  showComposeOpeningContent()
  fetch("/compose/pane").then(function (r) { return r.text() }).then(function (html) {
    paneHTML = html
    openWhenReady()
  }).catch(function () {
    document.body.removeEventListener("htmx:beforeSwap", beforeMainContentSwap)
    document.body.removeEventListener("htmx:afterSwap", afterMainContentSwap)
  })
  if (fullWidth) {
    mergeFolderShellBehindCompose("inbox", true)
  } else {
    htmx.ajax("GET", "/folder/inbox/full", { target: "#main-content", swap: "outerHTML" })
  }
}

var MAIL_READ_ICON = '<path d="m22 7-8.991 5.727a2 2 0 0 1-2.009 0L2 7"/>\n  <rect x="2" y="4" width="20" height="16" rx="2"/>'
var MAIL_UNREAD_ICON = '<path d="M21.2 8.4c.5.38.8.97.8 1.6v10a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V10a2 2 0 0 1 .8-1.6l8-6a2 2 0 0 1 2.4 0l8 6Z"/>\n  <path d="m22 10-8.97 5.7a1.94 1.94 0 0 1-2.06 0L2 10"/>'

function setReadButtonIcon(button, isRead) {
  if (!button) return
  button.dataset.isRead = isRead ? "true" : "false"
  var svg = button.querySelector('svg')
  if (svg) svg.innerHTML = isRead ? MAIL_READ_ICON : MAIL_UNREAD_ICON
}

// The reader's thread read toggle shows the thread as read only when every message is read.
function syncThreadReadButtons() {
  var threadButtons = document.querySelectorAll('#mail-view [data-read-thread]')
  if (!threadButtons.length) return
  var allRead = !document.querySelector('#mail-view [data-read-email][data-is-read="false"]')
  for (var i = 0; i < threadButtons.length; i++) setReadButtonIcon(threadButtons[i], allRead)
}

function toggleRead(emailId) {
  fetch("/api/messages/" + emailId + "/read", { method: "POST" })
    .then(function (r) { return r.json() })
    .then(function (data) {
      var buttons = document.querySelectorAll('[data-read-email="' + emailId + '"]')
      for (var i = 0; i < buttons.length; i++) setReadButtonIcon(buttons[i], data.is_read)
      syncThreadReadButtons()
      invalidateMailListItem(emailId)
      refreshSidebarUnread()
    })
    .catch(function () {})
}

function toggleThreadRead(emailId, threadId) {
  fetch("/api/messages/" + emailId + "/thread/read", { method: "POST" })
    .then(function (r) { return r.json() })
    .then(function (data) {
      // Every message in the open thread now has the same read state.
      var readerThread = document.querySelector('#mail-view [data-read-thread]')
      var readerShowsThread = readerThread && (readerThread.getAttribute('data-read-thread') === String(emailId) ||
        (threadId && readerThread.getAttribute('data-read-thread-id') === String(threadId)))
      if (readerShowsThread) {
        var buttons = document.querySelectorAll('#mail-view [data-read-email], #mail-view [data-read-thread]')
        for (var i = 0; i < buttons.length; i++) setReadButtonIcon(buttons[i], data.is_read)
      } else {
        var button = document.querySelector('[data-read-email="' + emailId + '"]')
        if (button) setReadButtonIcon(button, data.is_read)
      }
      invalidateMailListItem(emailId)
      refreshSidebarUnread()
    })
    .catch(function () {})
}

function setStarButtonIcon(button, starred) {
  var svg = button && button.querySelector('svg')
  if (!svg) return
  var size = (svg.getAttribute('class') || '').match(/\bsize-[\w.]+/)
  size = size ? size[0] : 'size-4'
  if (starred) {
    svg.setAttribute('class', size + ' text-amber-500 fill-amber-500 drop-shadow-[0_1px_1px_rgba(180,120,0,0.3)]')
  } else {
    svg.setAttribute('class', size + ' text-ink/30')
  }
}

// The reader's thread star is filled while any message in the open thread is starred.
function syncThreadStarButtons() {
  var threadStars = document.querySelectorAll('#mail-view [data-star-thread]')
  if (!threadStars.length) return
  var starred = !!document.querySelector('#mail-view [data-star-email] svg.fill-amber-500')
  for (var i = 0; i < threadStars.length; i++) setStarButtonIcon(threadStars[i], starred)
}

function toggleStar(emailId) {
  fetch("/api/messages/" + emailId + "/star", { method: "POST" })
    .then(function (r) { return r.json() })
    .then(function (data) {
      var starBtns = document.querySelectorAll('[data-star-email="' + emailId + '"]')
      for (var i = 0; i < starBtns.length; i++) setStarButtonIcon(starBtns[i], data.is_starred)
      syncThreadStarButtons()
      invalidateMailListItem(emailId)
    })
    .catch(function () {})
}

// Stars every message in the thread, or unstars them all when any is starred.
function toggleThreadStar(emailId, threadId) {
  fetch("/api/messages/" + encodeURIComponent(emailId) + "/thread/star", { method: "POST" })
    .then(function (r) {
      if (!r.ok) throw new Error("Failed to star thread")
      return r.json()
    })
    .then(function (data) {
      var readerThread = document.querySelector('#mail-view [data-star-thread]')
      var readerShowsThread = readerThread && (readerThread.getAttribute('data-star-thread') === String(emailId) ||
        (threadId && readerThread.getAttribute('data-star-thread-id') === String(threadId)))
      if (readerShowsThread) {
        var stars = document.querySelectorAll('#mail-view [data-star-email], #mail-view [data-star-thread]')
        for (var i = 0; i < stars.length; i++) setStarButtonIcon(stars[i], data.is_starred)
      }
      invalidateMailListItem(emailId)
    })
    .catch(function () {})
}

function mailDeleteFolderQuery() {
  var folderID = mailActionCurrentFolderID()
  return folderID ? "?folder_id=" + encodeURIComponent(folderID) : ""
}

function beginOptimisticMailRemoval(emailId) {
  if (typeof window.applyOptimisticMailRemove === "function") {
    window.applyOptimisticMailRemove([String(emailId)])
  }
  if (typeof setMailViewEmpty === "function") setMailViewEmpty()
  var container = document.getElementById("mail-list-scroll")
  var vml = container && container._virtualMailList
  if (vml && String(vml.selectedEmailId || "") === String(emailId)) {
    vml.selectedEmailId = null
    if (typeof vml.syncSelectionClasses === "function") vml.syncSelectionClasses(vml.itemsContainer || vml.container)
    if (typeof vml.replaceUrl === "function") vml.replaceUrl()
  }
  return vml
}

function finishOptimisticMailRemoval(vml) {
  if (vml && typeof vml.refreshCurrentFolder === "function") {
    vml.refreshCurrentFolder({ noAnimation: true }).catch(function () {})
  }
  refreshSidebarUnread()
}

function restoreOptimisticMailRemoval(emailId, vml) {
  var row = document.querySelector('.mail-list-item[data-email-id="' + String(emailId).replace(/"/g, '\\"') + '"]')
  if (row) {
    row.style.opacity = ""
    row.style.pointerEvents = ""
  }
  if (vml) {
    vml.selectedEmailId = String(emailId)
    if (typeof vml.syncSelectionClasses === "function") vml.syncSelectionClasses(vml.itemsContainer || vml.container)
    if (typeof vml.replaceUrl === "function") vml.replaceUrl()
  }
  if (window.htmx) htmx.ajax("GET", mailViewRequestURL(emailId), { target: "#mail-view", swap: "innerHTML" })
}

function deleteMessage(emailId) {
  var vml = beginOptimisticMailRemoval(emailId)
  fetch("/api/messages/" + encodeURIComponent(emailId) + mailDeleteFolderQuery(), { method: "DELETE" })
    .then(function (response) {
      if (!response.ok) throw new Error("delete failed")
      finishOptimisticMailRemoval(vml)
    })
    .catch(function () { restoreOptimisticMailRemoval(emailId, vml) })
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
  var vml = beginOptimisticMailRemoval(emailId)
  fetch("/api/messages/" + encodeURIComponent(emailId) + "/thread" + mailDeleteFolderQuery(), { method: "DELETE" })
    .then(function (response) {
      if (!response.ok) throw new Error("delete thread failed")
      finishOptimisticMailRemoval(vml)
    })
    .catch(function () { restoreOptimisticMailRemoval(emailId, vml) })
}

function markSpam(emailId) {
  markSpamState(emailId, false, false)
}

function markThreadSpam(emailId) {
  markSpamState(emailId, false, true)
}

function markNotSpam(emailId) {
  markSpamState(emailId, true, false)
}

function markThreadNotSpam(emailId) {
  markSpamState(emailId, true, true)
}

function mailActionCurrentFolderID() {
  var container = document.getElementById("mail-list-scroll")
  if (container && container._virtualMailList && container._virtualMailList.folderID) return container._virtualMailList.folderID
  if (container && container.dataset.folderId) return container.dataset.folderId
  var active = document.querySelector('aside a[hx-get^="/folder/"].bg-sidebar-accent')
  return active ? (active.getAttribute("hx-get") || "").replace("/folder/", "") : ""
}

var mailPermanentDeleteClasses = [
  "border",
  "border-red-500/35",
  "bg-red-500/12",
  "text-red-700",
  "shadow-[0_1px_2px_rgba(0,0,0,0.04)]",
  "hover:bg-red-500/20",
  "hover:text-red-800",
  "dark:border-red-300/20",
  "dark:bg-red-300/10",
  "dark:text-red-300",
  "dark:hover:text-red-200"
]

function mailFolderIsTrash(folderID) {
  folderID = String(folderID || "").trim()
  if (!folderID) return false
  if (folderID.toLowerCase() === "trash") return true
  var links = document.querySelectorAll('aside a[hx-get^="/folder/"]')
  for (var i = 0; i < links.length; i++) {
    if ((links[i].getAttribute("hx-get") || "") !== "/folder/" + folderID) continue
    return String(links[i].dataset.folderRole || "").toLowerCase() === "trash"
  }
  return false
}

function syncMailDeleteActionState() {
  var button = document.querySelector('[data-mail-selection-action="delete"]')
  if (!button) return
  var permanent = mailFolderIsTrash(mailActionCurrentFolderID())
  for (var i = 0; i < mailPermanentDeleteClasses.length; i++) {
    button.classList.toggle(mailPermanentDeleteClasses[i], permanent)
  }
  button.dataset.mailDeletePermanent = permanent ? "true" : "false"
  button.setAttribute("aria-label", permanent ? "Permanently delete selected messages" : "Delete selected messages")
  var root = button.closest("[data-tui-popover-root]")
  var tooltip = root && root.querySelector("[data-mail-delete-tooltip]")
  if (tooltip) tooltip.textContent = permanent ? "Permanently delete" : "Delete"
}

function mailViewRequestURL(emailID, single) {
  var params = new URLSearchParams()
  var folderID = mailActionCurrentFolderID()
  if (folderID) params.set("folder_id", folderID)
  if (single) params.set("single", "1")
  var query = params.toString()
  return "/email/" + encodeURIComponent(emailID) + (query ? "?" + query : "")
}

function markSpamState(emailId, notSpam, thread) {
  var path = notSpam ? "/api/messages/not-spam" : "/api/messages/spam"
  fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ targets: [{ id: String(emailId), thread: !!thread }], folder_id: mailActionCurrentFolderID() })
  })
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

function promptLabelMessage(emailId, thread) {
  var labelName = window.prompt("Label name")
  if (labelName == null) return
  labelName = String(labelName).trim()
  if (!labelName) return
  fetch("/api/messages/" + encodeURIComponent(emailId) + "/label", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ label: labelName, thread: !!thread, folder_id: mailActionCurrentFolderID() })
  })
    .then(function () { refreshAfterLabelMutation(emailId) })
    .catch(function () {})
}

function removeLabelMessage(emailId, labelName, thread) {
  labelName = String(labelName || "").trim()
  if (!labelName) return
  fetch("/api/messages/" + encodeURIComponent(emailId) + "/unlabel", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ label: labelName, thread: !!thread, folder_id: mailActionCurrentFolderID() })
  })
    .then(function () { refreshAfterLabelMutation(emailId) })
    .catch(function () {})
}

function refreshAfterLabelMutation(emailId) {
  invalidateMailListItem(emailId)
  var container = document.getElementById("mail-list-scroll")
  if (container && container._virtualMailList && typeof container._virtualMailList.refreshCurrentFolder === "function") {
    container._virtualMailList.refreshCurrentFolder({ noAnimation: true }).catch(function () {})
  }
  if (window.htmx) htmx.ajax("GET", mailViewRequestURL(emailId), { target: "#mail-view", swap: "innerHTML" })
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
      if (iframe.dataset.translationActive === "true" && typeof window.goferEmailTranslationFrameLoaded === "function") {
        window.goferEmailTranslationFrameLoaded(e.data.emailId)
      }
    }
  }
  if (e.data.type === "remoteContentBlocked" && e.data.emailId) {
    var banner = document.querySelector('[data-remote-content-banner="' + e.data.emailId + '"]')
    if (banner) banner.classList.remove("hidden")
  }
})

// Small screens use larger text, including in message bodies that leave the size to Gofer.
function emailBodyWantsLargeText() {
  return !!(window.matchMedia && window.matchMedia("(max-width: 1023.98px)").matches)
}

function translatedEmailBodyURL(iframe, theme, bg, fg, link, original) {
  var params = new URLSearchParams()
  params.set("theme", theme)
  if (original) params.set("mode", "original")
  if (!original && bg) params.set("bg", bg)
  if (!original && fg) params.set("fg", fg)
  if (!original && link) params.set("link", link)
  if (iframe.dataset.remoteLoaded === "true") params.set("remote", "true")
  if (emailBodyWantsLargeText()) params.set("text", "large")
  params.set("provider", iframe.dataset.translationProvider || "google_web_basic")
  params.set("target_language", iframe.dataset.translationTargetLanguage || "en")
  return "/email/" + iframe.dataset.emailId + "/body/translated?" + params.toString()
}

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
  if (emailBodyWantsLargeText()) params.set("text", "large")
  setEmailBodyFrameSrc(iframe, iframe.dataset.translationActive === "true" ?
    translatedEmailBodyURL(iframe, theme, bg, fg, link, original) :
    "/email/" + iframe.dataset.emailId + "/body?" + params.toString())
  updateEmailBodySchemeButton(iframe, baseTheme, theme, bodyMode)
}

// Reloading the body by assigning a new src adds an entry to the page's history, so
// Back would step through every restyle, translation or remote-content load before
// leaving the message. After the first load the frame's location is replaced instead,
// and the current address is kept in data-body-src, since src then goes stale.
function setEmailBodyFrameSrc(iframe, url) {
  var absolute = new URL(url, window.location.origin).toString()
  if (iframe.getAttribute("src") && iframe.contentWindow) {
    try {
      iframe.contentWindow.location.replace(absolute)
      iframe.dataset.bodySrc = absolute
      return
    } catch (e) {}
  }
  iframe.dataset.bodySrc = absolute
  iframe.src = absolute
}

function emailBodyFrameSrc(iframe) {
  return iframe.dataset.bodySrc || iframe.src
}

function loadRemoteContent(emailId) {
  var iframe = document.querySelector('[data-email-body-frame][data-email-id="' + emailId + '"]')
  if (!iframe) return
  var src = emailBodyFrameSrc(iframe)
  if (!src) return
  var url = new URL(src, window.location.origin)
  url.searchParams.set("remote", "true")
  setEmailBodyFrameSrc(iframe, url.toString())
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
      if (iframe && emailBodyFrameSrc(iframe)) {
        var url = new URL(emailBodyFrameSrc(iframe), window.location.origin)
        url.searchParams.set("remote", "true")
        setEmailBodyFrameSrc(iframe, url.toString())
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
  var mode = bodyMode || iframe.dataset.bodyMode || (iframe.dataset.forceScheme === "opposite" ? oppositeEmailBodyTheme(baseTheme) : baseTheme)
  if (mode !== "dark" && mode !== "light" && mode !== "original") mode = baseTheme
  if (emailId) {
    var cycleButtons = document.querySelectorAll('[data-email-body-mode-cycle="' + emailId + '"]')
    for (var c = 0; c < cycleButtons.length; c++) cycleButtons[c].setAttribute("data-email-body-mode", mode)
  }
  var btn = emailId ? document.querySelector('[data-force-email-scheme="' + emailId + '"]') : document.querySelector("[data-force-email-scheme]")
  if (!btn) return
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
        htmx.ajax("GET", mailViewRequestURL(emailId), { target: "#mail-view", swap: "innerHTML" })
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
  vals._kind = form.dataset.composeKind || ""
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
  if (vals.account_id) syncComposeAccountItems(form.id === "compose-pane-form" ? "pane" : "dialog", vals.account_id)
  _setComposeEditorValue(form, vals.body || "", vals.html_body || "", vals.inline_images || [])
  renderComposeAttachments(form, vals.attachments || [])
  form.dataset.composeUploadsPending = "0"
  form.dataset.composeSending = "false"
  delete form.dataset.composeUploadFailed
  form.dataset.composeDirty = vals._composeDirty || "false"
  setComposeKind(form, vals._kind || vals.compose_mode)
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

  var axis = isStackedComposeLayout() ? "height" : "width"
  mailList._composeFullWidthAxis = axis
  mailList._savedWidth = axis === "height" ? mailList.style.height : mailList.style.width
  if (axis === "height") mailList._savedMinHeight = mailList.style.minHeight
  mailList.style.display = "none"
  mailList.style[axis] = "0px"
  if (axis === "height") mailList.style.minHeight = "0px"
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

function isStackedComposeLayout() {
  var main = document.getElementById("main-content")
  return !!(main && main.dataset.mailPaneLayout === "stacked")
}

function expandComposeFullWidth() {
  var mailList = document.querySelector("#main-content > #mail-list")
  var resizeHandles = document.querySelectorAll('[data-panel="maillist"]')
  if (!mailList || mailList._animating) return

  var axis = isStackedComposeLayout() ? "height" : "width"
  mailList._animating = true
  mailList._composeFullWidthAxis = axis
  mailList._savedWidth = axis === "height" ? mailList.style.height : mailList.style.width
  if (axis === "height") {
    mailList._savedMinHeight = mailList.style.minHeight
    mailList.style.minHeight = "0px"
  }

  for (var i = 0; i < resizeHandles.length; i++) {
    resizeHandles[i]._savedDisplay = resizeHandles[i].style.display
    resizeHandles[i].style.transition = "opacity 0.25s ease"
    resizeHandles[i].style.opacity = "0"
  }

  mailList.style.transition = axis + " 0.3s cubic-bezier(0.4,0,0.2,1), opacity 0.25s ease, border-width 0.3s ease"
  mailList.style.overflow = "hidden"
  mailList.style.borderWidth = "0"

  requestAnimationFrame(function () {
    requestAnimationFrame(function () {
      mailList.style[axis] = "0px"
      mailList.style.opacity = "0"
    })
  })

  var composePane = document.querySelector("[data-compose-pane]")
  if (composePane) {
    composePane.style.animation = (axis === "height" ? "pane-slide-up-in" : "pane-slide-in") + " 0.3s ease-out"
  }

  function onEnd(ev) {
    if (ev.target !== mailList || ev.propertyName !== axis) return
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

  var axis = mailList._composeFullWidthAxis || (isStackedComposeLayout() ? "height" : "width")
  mailList.style.display = ""
  mailList.style[axis] = "0px"
  if (axis === "height") mailList.style.minHeight = "0px"
  mailList.style.opacity = "0"
  mailList.style.overflow = "hidden"
  mailList.style.transition = axis + " 0.3s cubic-bezier(0.4,0,0.2,1), opacity 0.25s ease, border-width 0.3s ease"

  for (var i = 0; i < resizeHandles.length; i++) {
    resizeHandles[i].style.display = resizeHandles[i]._savedDisplay || ""
    delete resizeHandles[i]._savedDisplay
    resizeHandles[i].style.opacity = "0"
    resizeHandles[i].style.transition = "opacity 0.25s ease 0.1s"
  }

  void mailList.offsetHeight

  requestAnimationFrame(function () {
    mailList.style[axis] = mailList._savedWidth
    mailList.style.opacity = "1"
    for (var i = 0; i < resizeHandles.length; i++) {
      resizeHandles[i].style.opacity = "1"
    }
  })

  function onEnd(ev) {
    if (ev.target !== mailList || ev.propertyName !== axis) return
    mailList.removeEventListener("transitionend", onEnd)
    mailList.style.transition = ""
    mailList.style.opacity = ""
    mailList.style.overflow = ""
    mailList.style.borderWidth = ""
    if (axis === "height") {
      mailList.style.minHeight = mailList._savedMinHeight || ""
      delete mailList._savedMinHeight
    }
    delete mailList._savedWidth
    delete mailList._composeFullWidthAxis
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

;(function () {
  document.addEventListener('error', function (event) {
    var image = event.target
    if (!image || !image.matches || !image.matches('[data-avatar-image]')) return

    image.classList.add('hidden')
    var avatar = image.closest('[data-contact-avatar]')
    var fallback = avatar ? avatar.querySelector('[data-avatar-fallback]') : null
    if (!fallback) return

    fallback.classList.remove('hidden')
    fallback.classList.add('flex')
  }, true)

  document.addEventListener('click', function (event) {
    var trigger = event.target && event.target.closest ? event.target.closest('[data-contact-avatar-preview-trigger]') : null
    if (!trigger) return

    var dialogID = trigger.getAttribute('data-contact-avatar-preview-dialog') || ''
    var root = dialogID ? document.getElementById(dialogID) : null
    if (!root) return

    var source = trigger.querySelector('[data-avatar-image]')
    var preview = root.querySelector('[data-contact-avatar-preview-image]')
    if (!source || !preview) return

    var originalURL = source.currentSrc || source.getAttribute('src') || ''
    var sourceURL = trigger.getAttribute('data-contact-avatar-preview-src') || originalURL
    if (!sourceURL) return

    preview.src = sourceURL
    preview.alt = trigger.getAttribute('data-contact-avatar-preview-alt') || source.alt || 'Profile picture'
    if (window.console && window.console.info) {
      window.console.info('[Gofer] contact avatar preview', {
        originalURL: originalURL,
        previewURL: sourceURL,
        upgraded: !!originalURL && sourceURL !== originalURL,
      })
    }
  })

  document.addEventListener('close', function (event) {
    var content = event.target
    if (!content || !content.matches || !content.matches('[data-tui-dialog-content]')) return

    var root = content.closest('[data-tui-dialog]')
    if (!root) return

    var preview = root.querySelector('[data-contact-avatar-preview-image]')
    if (!preview) return

    preview.removeAttribute('src')
  }, true)
})()
