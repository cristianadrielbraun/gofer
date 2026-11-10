// The diagnostics pages' live part: avatar checks and contacts follow
// Gofer's work as it happens. The console's pages and an owner's Settings →
// Diagnostics share it; each page says where its data is
// (data-diagnostics-api) and which events it follows
// (data-diagnostics-events: an event stream's address, or "app" for the
// webmail's, which app.js passes on as gofer:diagnostics-event). Settings
// opens pages in place, so the part starts with each page and stops when
// its page goes.
(function () {
  "use strict"

  var mounted = null

  function mountDiagnostics() {
    var avatarRoot = document.querySelector("[data-avatar-admin]")
    var contactRoot = document.querySelector("[data-contact-admin]")
    var root = avatarRoot || contactRoot
    if (mounted && mounted.root === root) return
    if (mounted) {
      mounted.stop()
      mounted = null
    }
    if (root) mounted = startDiagnostics(avatarRoot, contactRoot)
  }

  function startDiagnostics(avatarRoot, contactRoot) {
	var root = avatarRoot || contactRoot
	var api = root.getAttribute("data-diagnostics-api") || "/api/admin"
	var events = root.getAttribute("data-diagnostics-events") || "/api/admin/events"
	var stopped = false
	var cleanups = []
	var handle = {
		root: root,
		stop: function () {
			stopped = true
			clearTimeout(statusTimer)
			clearTimeout(tableTimer)
			cleanups.forEach(function (cleanup) { cleanup() })
		},
	}

	var statusTimer = null
	var tableTimer = null
	var tableLiveUpdates = true
	var lastBackfill = null

  function qs(selector, base) {
    return (base || document).querySelector(selector)
  }

  function adminDataURL(path) {
    var destination = new URL(path, window.location.href)
    var userID = new URL(window.location.href).searchParams.get("user_id")
    if (userID) destination.searchParams.set("user_id", userID)
    return destination.pathname + destination.search
  }

  function percent(part, total) {
    if (!total || total <= 0) return 0
    return Math.min(100, Math.floor((part || 0) * 100 / total))
  }

  function fmtNumber(value) {
    return goferNumber(value || 0)
  }

  function filterValue(node, fallback) {
    if (!node) return fallback
    return node.value || fallback
  }

  function filterBoolean(node) {
    if (!node) return false
    if (node.type === "checkbox") return node.checked
    return node.value === "true"
  }

  function fmtTime(value) {
    if (!value || value === "0001-01-01T00:00:00Z") return goferT("js.admin.never")
    var date = new Date(value)
    if (isNaN(date.getTime())) return goferT("js.admin.never")
    return goferDates.shortDateTime(date)
  }

  function latestAvatarError(status) {
    var errors = status && status.recent_errors
    if (errors && errors.length) {
      for (var i = 0; i < errors.length; i++) {
        if (errors[i] && errors[i].message) return errors[i].message
      }
    }
    return (status && status.backfill && status.backfill.last_error) || goferT("js.admin.none")
  }

  function escapeHTML(value) {
    return String(value == null ? "" : value)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#39;")
  }

  function statusClass(status) {
    if (status === "found") return "bg-emerald-500/12 text-emerald-700 dark:text-emerald-300"
    if (status === "missing") return "bg-slate-500/12 text-slate-700 dark:text-slate-300"
    if (status === "skipped") return "bg-blue-500/12 text-blue-700 dark:text-blue-300"
    if (status === "error") return "bg-red-500/12 text-red-700 dark:text-red-300"
    return "bg-muted text-muted-foreground"
  }

  // avatarStatusLabel names an avatar check status; one the catalog doesn't
  // know is shown as the server sent it.
  var AVATAR_STATUSES = ["found", "missing", "skipped", "error", "unchecked", "pending", "unknown"]
  function avatarStatusLabel(status) {
    return AVATAR_STATUSES.indexOf(status) >= 0 ? goferT("js.admin.avatars.status." + status) : status
  }

  function plainStatusClass(status) {
    if (status === "found") return "text-emerald-700 dark:text-emerald-300"
    if (status === "missing") return "text-slate-700 dark:text-slate-300"
    if (status === "skipped") return "text-blue-700 dark:text-blue-300"
    if (status === "error") return "text-red-600 dark:text-red-300"
    return "text-muted-foreground"
  }

  function setMetric(key, value) {
    var next = fmtNumber(value)
    root.querySelectorAll('[data-admin-metric="' + key + '"] [data-admin-metric-value]').forEach(function (node) {
      if (node.textContent === next) return
      node.textContent = next
      var card = node.closest("[data-admin-metric]")
      if (card) {
        card.classList.add("bg-accent")
        setTimeout(function () { card.classList.remove("bg-accent") }, 450)
      }
    })
  }

  function providerMetricKey(provider, metric) {
    provider = String(provider || "unknown").toLowerCase().trim().replace(/[\s-]+/g, "_")
    if (!provider) provider = "unknown"
    return provider + "_" + metric
  }

  function setDetail(key, value) {
    root.querySelectorAll('[data-admin-detail="' + key + '"] [data-admin-detail-value]').forEach(function (node) {
      node.textContent = value
    })
  }

  function contactRunDescription(backfill) {
    if (backfill && backfill.in_progress) return goferT("js.admin.contacts.run.running")
    if (backfill && backfill.last_error) return goferT("js.admin.contacts.run.failed")
    return goferT("js.admin.contacts.run.idle")
  }

  function contactRunLabel(backfill) {
    if (backfill && backfill.in_progress) return goferT("js.admin.contacts.state.running")
    if (backfill && backfill.last_error) return goferT("js.admin.contacts.state.error")
    return goferT("js.admin.state.idle")
  }

  function contactEventLabel(eventType) {
    var labels = {
      backfill_forced: goferT("js.admin.contacts.event.backfill_forced"),
      backfill_started: goferT("js.admin.contacts.event.backfill_started"),
      backfill_completed: goferT("js.admin.contacts.event.backfill_completed"),
      manual_contact_added: goferT("js.admin.contacts.event.manual_contact_added"),
      observed_contact_added: goferT("js.admin.contacts.event.observed_contact_added"),
      contact_deleted: goferT("js.admin.contacts.event.contact_deleted"),
      observed_contacts_deleted: goferT("js.admin.contacts.event.observed_contacts_deleted"),
    }
    return labels[eventType] || String(eventType || "event").replace(/_/g, " ").replace(/\b\w/g, function (ch) { return ch.toUpperCase() })
  }

  function contactEventClass(eventType) {
    if (eventType === "backfill_forced" || eventType === "backfill_started") return "bg-amber-500/12 text-amber-700 dark:text-amber-300"
    if (eventType === "backfill_completed" || eventType === "manual_contact_added" || eventType === "observed_contact_added") return "bg-emerald-500/12 text-emerald-700 dark:text-emerald-300"
    if (eventType === "contact_deleted" || eventType === "observed_contacts_deleted") return "bg-red-500/12 text-red-700 dark:text-red-300"
    return "bg-muted text-muted-foreground"
  }

  function renderContactEventRow(event) {
    event = event || {}
    return '<div class="grid grid-cols-[10rem_minmax(15rem,1fr)_12rem_8rem_minmax(18rem,1.3fr)] gap-3 border-b border-border/70 bg-background/55 px-4 py-3 text-sm odd:bg-background/70 hover:bg-accent/35">' +
      '<div class="text-xs font-medium text-muted-foreground">' + escapeHTML(fmtTime(event.created_at)) + '</div>' +
      '<div class="min-w-0"><div class="truncate font-medium text-foreground">' + escapeHTML(event.username || goferT("js.admin.unknown_user")) + '</div><div class="truncate text-xs text-muted-foreground">' + escapeHTML(event.email || goferT("js.admin.system_event")) + '</div></div>' +
      '<div><span class="inline-flex rounded-full px-2 py-0.5 text-[11px] font-semibold ' + contactEventClass(event.type) + '">' + escapeHTML(contactEventLabel(event.type)) + '</span></div>' +
      '<div class="text-sm font-semibold tabular-nums text-foreground">' + escapeHTML(fmtNumber(event.count)) + '</div>' +
      '<div class="min-w-0 truncate text-sm text-muted-foreground">' + escapeHTML(event.message || "") + '</div>' +
      '</div>'
  }

  function renderContactBackfill(backfill) {
    backfill = backfill || {}
    var pct = percent(backfill.processed, backfill.total)
    var progressText = qs("[data-contact-progress-text]", root)
    var progressPct = qs("[data-contact-progress-percent]", root)
    var progressBar = qs("[data-contact-progress-bar]", root)
    if (progressText) progressText.textContent = goferT("js.admin.progress", { done: fmtNumber(backfill.processed), total: fmtNumber(backfill.total) })
    if (progressPct) progressPct.textContent = pct + "%"
    if (progressBar) progressBar.style.width = pct + "%"
    var state = qs("[data-contact-state]", root)
    if (state) {
      state.textContent = contactRunLabel(backfill)
      state.className = "inline-flex w-fit shrink-0 items-center rounded-md px-2.5 py-1 text-[11px] font-semibold " + (backfill.in_progress ? "bg-amber-500/12 text-amber-700 dark:text-amber-300" : (backfill.last_error ? "bg-red-500/12 text-red-700 dark:text-red-300" : "bg-emerald-500/12 text-emerald-700 dark:text-emerald-300"))
    }
    var description = qs("[data-contact-run-description]", root)
    if (description) description.textContent = contactRunDescription(backfill)
    var button = document.querySelector('form[action="/admin/contacts/backfill"] button')
    if (button) button.disabled = !!backfill.in_progress
    setDetail("contacts_started", fmtTime(backfill.started_at))
    setDetail("contacts_finished", fmtTime(backfill.finished_at))
    setDetail("contacts_last_error", backfill.last_error || goferT("js.admin.none"))
  }

  function renderContactStatus(status) {
    if (!status) return
    renderContactBackfill(status.backfill || {})
    setMetric("contacts_total", status.total)
    setMetric("contacts_observed", status.observed)
    setMetric("contacts_manual", status.manual)
    setMetric("contacts_suppressed", status.suppressed)
    setMetric("contacts_added_today", status.added_today)
    setMetric("contacts_deleted_today", status.deleted_today)
    setDetail("contacts_last_backfill", fmtTime(status.last_backfill))
    var events = status.recent_events || []
    var count = qs("[data-contact-event-count]", root)
    if (count) count.textContent = goferTn("js.admin.contacts.recent_events", events.length, { count: fmtNumber(events.length) })
    var viewport = qs("[data-contact-event-viewport]", root)
    if (viewport) {
      viewport.innerHTML = events.length ? events.map(renderContactEventRow).join("") : '<div class="px-4 py-8 text-center text-sm text-muted-foreground">' + escapeHTML(goferT("js.admin.contacts.no_activity")) + '</div>'
    }
  }

  function refreshContactStatus() {
    return fetch(adminDataURL(api + "/contacts/status"), { headers: { "Accept": "application/json" } })
      .then(function (res) { if (!res.ok) throw new Error("status " + res.status); return res.json() })
      .then(renderContactStatus)
      .catch(function () {})
  }

  function bindForceContactBackfill() {
    var form = document.querySelector("[data-contact-backfill-form]")
    if (!form) return
    form.addEventListener("submit", function (event) {
      event.preventDefault()
      var button = qs("button", form)
      if (button) button.disabled = true
      fetch(form.action, { method: "POST", headers: { "Accept": "application/json" } })
        .then(function () { refreshContactStatus() })
        .catch(function () { if (button) button.disabled = false })
    })
  }

  // A page that is left closes its event stream at once, instead of keeping
  // one of the browser's six connections to Gofer while the browser holds the
  // page for going back. Going back opens a new one.
  function closeOnPageHide(source, reopen) {
    function hide() {
      source.close()
      window.removeEventListener("pagehide", hide)
      window.addEventListener("pageshow", function show(event) {
        window.removeEventListener("pageshow", show)
        if (event.persisted && !stopped) reopen()
      })
    }
    window.addEventListener("pagehide", hide)
    cleanups.push(function () { window.removeEventListener("pagehide", hide) })
  }

  // subscribe calls handlers[type] with each event of that type, from the
  // page's own stream or the webmail's, until the page goes.
  function subscribe(handlers) {
    if (events === "app") {
      var listener = function (event) {
        var detail = event.detail || {}
        var handler = handlers[detail.type]
        if (handler && !stopped) handler({ data: detail.data })
      }
      document.addEventListener("gofer:diagnostics-event", listener)
      cleanups.push(function () { document.removeEventListener("gofer:diagnostics-event", listener) })
      return
    }
    if (!window.EventSource) return
    var source = null
    var retry = null
    function open() {
      if (stopped) return
      source = new EventSource(events)
      closeOnPageHide(source, open)
      Object.keys(handlers).forEach(function (type) {
        source.addEventListener(type, function (event) {
          if (!stopped) handlers[type](event)
        })
      })
      source.onerror = function () {
        source.close()
        retry = setTimeout(open, 5000)
      }
    }
    open()
    cleanups.push(function () {
      clearTimeout(retry)
      if (source) source.close()
    })
  }

  function setupContactSSE() {
    subscribe({
      "contact-backfill": function (event) {
        var data = parseEventData(event)
        if (data.backfill) renderContactBackfill(data.backfill)
        refreshContactStatus()
      },
      "contact-activity": function () {
        refreshContactStatus()
      },
    })
  }

  function setupContactAdmin() {
    bindForceContactBackfill()
    setupContactSSE()
    refreshContactStatus()
  }

  if (contactRoot && !avatarRoot) {
    setupContactAdmin()
    return handle
  }

  function setState(backfill) {
    var state = qs("[data-avatar-state]", root)
    if (!state) return
    var running = !!backfill.in_progress
    var canceling = running && !!backfill.cancel_requested
    var canceled = !running && !!backfill.canceled
    state.textContent = canceling ? goferT("js.admin.avatars.state.canceling") : (running ? (backfill.mode === "manual" ? goferT("js.admin.avatars.state.full") : goferT("js.admin.avatars.state.due")) : (canceled ? goferT("js.admin.avatars.state.canceled") : goferT("js.admin.state.idle")))
    state.className = "inline-flex w-fit shrink-0 items-center rounded-md px-2.5 py-1 text-[11px] font-semibold " + (canceling || canceled ? "bg-red-500/12 text-red-700 dark:text-red-300" : (running ? "bg-amber-500/12 text-amber-700 dark:text-amber-300" : "bg-emerald-500/12 text-emerald-700 dark:text-emerald-300"))
    var description = qs("[data-avatar-run-description]", root)
    if (description) {
      description.textContent = canceling ? goferT("js.admin.avatars.run.canceling") : (running ? (backfill.mode === "manual" ? goferT("js.admin.avatars.run.full") : goferT("js.admin.avatars.run.due")) : (canceled ? goferT("js.admin.avatars.run.canceled") : goferT("js.admin.avatars.run.idle")))
    }
    var button = document.querySelector("[data-avatar-recheck-form] button")
    if (button) button.disabled = running
    var cancelButton = document.querySelector("[data-avatar-cancel-form] button")
    if (cancelButton) cancelButton.disabled = !running || canceling
  }

  function renderBackfill(backfill) {
    if (!backfill) return
    lastBackfill = Object.assign({}, lastBackfill || {}, backfill)
    backfill = lastBackfill
    var pct = percent(backfill.processed, backfill.total)
    var progressText = qs("[data-avatar-progress-text]", root)
    var progressPct = qs("[data-avatar-progress-percent]", root)
    var progressBar = qs("[data-avatar-progress-bar]", root)
    if (progressText) progressText.textContent = goferT("js.admin.progress", { done: fmtNumber(backfill.processed), total: fmtNumber(backfill.total) })
    if (progressPct) progressPct.textContent = pct + "%"
    if (progressBar) progressBar.style.width = pct + "%"
    setState(backfill)
    ;(backfill.provider_stats || []).forEach(function (provider) {
      setMetric("run_" + providerMetricKey(provider.provider, "checked"), provider.checked)
      setMetric("run_" + providerMetricKey(provider.provider, "found"), provider.found)
      setMetric("run_" + providerMetricKey(provider.provider, "missing"), provider.missing)
      setMetric("run_" + providerMetricKey(provider.provider, "skipped"), provider.skipped)
      setMetric("run_" + providerMetricKey(provider.provider, "error"), provider.error)
    })
    setDetail("started", fmtTime(backfill.started_at))
    setDetail("finished", fmtTime(backfill.finished_at))
  }

  function renderStatus(status) {
    if (!status || !status.cache || !status.backfill) return
    var cache = status.cache
    var backfill = status.backfill
    renderBackfill(backfill)

    setMetric("total", cache.total)
    setMetric("pending", cache.pending)
    setMetric("found", cache.found)
    setMetric("missing", cache.missing)
    setMetric("error", cache.error)
    setMetric("due", cache.due)
    setMetric("checked", Math.max(0, (cache.total || 0) - (cache.pending || 0)))
    var providerAssignments = 0
    ;(cache.provider_stats || []).forEach(function (provider) { providerAssignments += provider.in_use || 0 })
    setMetric("providers_total", (cache.provider_stats || []).length)
    setMetric("provider_assignments", providerAssignments)
    ;(cache.provider_stats || []).forEach(function (provider) {
      setMetric(providerMetricKey(provider.provider, "in_use"), provider.in_use)
      setMetric(providerMetricKey(provider.provider, "checked"), provider.checked)
      setMetric(providerMetricKey(provider.provider, "found"), provider.found)
      setMetric(providerMetricKey(provider.provider, "missing"), provider.missing)
      setMetric(providerMetricKey(provider.provider, "skipped"), provider.skipped)
      setMetric(providerMetricKey(provider.provider, "error"), provider.error)
    })
    setDetail("last_error", latestAvatarError(status))
  }

  function parseEventData(event) {
    try {
      return JSON.parse(event.data || "{}")
    } catch (_) {
      return {}
    }
  }

  function renderBackfillEvent(event) {
    var data = parseEventData(event)
    var wasRunning = !!(lastBackfill && lastBackfill.in_progress)
    if (data.backfill) {
      renderBackfill(data.backfill)
      return { wasRunning: wasRunning, running: !!data.backfill.in_progress, status: data.status || "" }
    }
    var backfill = Object.assign({}, lastBackfill || {})
    if (data.status === "manual" || data.status === "scheduled") {
      backfill.in_progress = true
      backfill.cancel_requested = false
      backfill.canceled = false
      backfill.mode = data.status
    } else if (data.status === "canceling") {
      backfill.in_progress = true
      backfill.cancel_requested = true
      backfill.mode = backfill.mode || "manual"
    } else if (data.status === "idle") {
      backfill.in_progress = false
      backfill.cancel_requested = false
      backfill.canceled = false
    }
    if (data.current != null) backfill.processed = data.current
    if (data.total != null) backfill.total = data.total
    if (data.error != null) backfill.last_error = data.error
    renderBackfill(backfill)
    return { wasRunning: wasRunning, running: !!backfill.in_progress, status: data.status || "" }
  }

	function refreshStatus() {
		return fetch(adminDataURL(api + "/avatars/status"), { headers: { "Accept": "application/json" } })
			.then(function (res) { if (!res.ok) throw new Error("status " + res.status); return res.json() })
			.then(renderStatus)
			.catch(function () {})
	}

	function activeAvatarTab() {
		var activeTrigger = qs('[data-tui-tabs-trigger][data-tui-tabs-state="active"]', root)
		if (activeTrigger) return activeTrigger.getAttribute("data-tui-tabs-value") || "overview"
		var suffix = window.location.pathname.replace(/^\/admin\/avatars\/?/, "").replace(/\/$/, "")
		return suffix || "overview"
	}

	function activeTableList() {
		var tab = activeAvatarTab()
		if (tab === "senders") return attemptList
		if (tab === "events") return eventList
		return null
	}

	function refreshActiveTable(resetScroll) {
		var list = activeTableList()
		if (list) list.refresh(!!resetScroll)
	}

	function scheduleActiveTableRefresh(delay) {
		if (!tableLiveUpdates || tableTimer || !activeTableList()) return
		tableTimer = setTimeout(function () {
			tableTimer = null
			if (stopped) return
			refreshActiveTable(false)
		}, delay == null ? 1500 : delay)
	}

	function updateTableControls() {
		root.querySelectorAll("[data-avatar-table-live]").forEach(function (node) {
			node.checked = tableLiveUpdates
		})
		root.querySelectorAll("[data-avatar-table-refresh]").forEach(function (node) {
			node.disabled = tableLiveUpdates || !activeTableList()
		})
	}

	function scheduleStatusRefresh(delay, force) {
		if (force && statusTimer) {
			clearTimeout(statusTimer)
			statusTimer = null
		}
		if (statusTimer) return
		statusTimer = setTimeout(function () {
			statusTimer = null
			if (stopped) return
			refreshStatus()
		}, delay == null ? 1500 : delay)
	}

	function bindTabRerender() {
		function renderTab(value) {
			if (value === "senders" && attemptList) attemptList.render()
			if (value === "events" && eventList) eventList.render()
			updateTableControls()
		}

    root.addEventListener("click", function (event) {
      var trigger = event.target.closest("[data-tui-tabs-trigger]")
      if (!trigger || !root.contains(trigger)) return
      setTimeout(function () {
        renderTab(trigger.getAttribute("data-tui-tabs-value"))
      }, 260)
    })

		function rerenderAfterHistory() {
			setTimeout(function () { if (!stopped) renderTab(activeAvatarTab()) }, 260)
		}
		window.addEventListener("popstate", rerenderAfterHistory)
		cleanups.push(function () { window.removeEventListener("popstate", rerenderAfterHistory) })
	}

  class VirtualAvatarSenderList {
    constructor(container) {
      this.container = container
      this.itemHeight = 76
      this.pageSize = 80
      this.items = []
      this.nextOffset = 0
      this.hasMore = true
      this.isLoading = false
      this.totalCount = 0
      this.filters = { query: "", provider: "all", status: "all", errors: false }
      this.providerColumns = []
      this.itemsContainer = document.createElement("div")
      this.loader = document.createElement("div")
      this.itemsContainer.style.minWidth = "0"
      this.loader.className = "px-4 py-4 text-center text-sm text-muted-foreground"
      this.container.innerHTML = ""
      this.container.appendChild(this.itemsContainer)
      this.container.appendChild(this.loader)
      this.bind()
      this.refresh(true)
    }

    bind() {
      var self = this
      var raf = null
      this.container.addEventListener("scroll", function () {
        if (raf) return
        raf = requestAnimationFrame(function () {
          self.maybeLoadMore()
          raf = null
        })
      })
    }

    setFilters(filters) {
      this.filters = filters
      this.refresh(true)
    }

    refresh(resetScroll) {
      this.items = []
      this.nextOffset = 0
      this.hasMore = true
      this.isLoading = false
      this.totalCount = 0
      this.itemsContainer.innerHTML = ""
      this.setLoader(goferT("js.admin.avatars.loading_senders"))
      if (resetScroll) this.container.scrollTop = 0
      this.loadNext(true)
    }

    rowHTML(item) {
      var columns = this.gridTemplateColumns()
      if (!item) {
        var skeletonProviders = this.providerColumns.map(function () { return '<div class="h-5 w-20 rounded bg-muted"></div>' }).join("")
        return '<div class="grid h-[76px] min-w-[48rem] items-center gap-2 border-b border-border/70 bg-background/35 px-4 py-2" style="grid-template-columns:' + columns + '">' +
          '<div class="space-y-2"><div class="h-4 w-52 rounded bg-muted"></div><div class="h-3 w-72 rounded bg-muted"></div></div><div class="h-5 w-24 rounded bg-muted"></div>' + skeletonProviders + '</div>'
      }
      var inUse = item.in_use || { status: item.status, source: item.source, avatar_url: item.avatar_url, error: item.error }
      var avatarURL = inUse.avatar_url || inUse.avatar_data_url
      var avatarImageClass = "h-full w-full object-cover" + ((inUse.source === "bimi" || inUse.source === "domain_icon") ? " scale-110" : "")
      var avatar = avatarURL
        ? '<div class="size-9 shrink-0 overflow-hidden rounded-full bg-muted/40"><img src="' + escapeHTML(avatarURL) + '" alt="" class="' + avatarImageClass + '"/></div>'
        : '<div class="size-9 shrink-0 rounded-full bg-muted text-xs font-semibold text-muted-foreground flex items-center justify-center">' + escapeHTML((item.email || "?").slice(0, 1).toUpperCase()) + '</div>'
      var providerByName = new Map()
      ;(item.providers || []).forEach(function (provider) { providerByName.set(provider.provider, provider) })
		var providerCells = this.providerColumns.map(function (name) {
			var provider = providerByName.get(name)
			if (!provider) return '<div class="text-xs text-muted-foreground">' + escapeHTML(goferT("js.admin.avatars.not_attempted")) + '</div>'
			return '<div class="min-w-0 text-sm font-medium ' + plainStatusClass(provider.status) + '">' + escapeHTML(avatarStatusLabel(provider.status || "unchecked")) + '</div>'
		}, this).join('')
      return '<div class="grid h-[76px] min-w-[48rem] items-center gap-1.5 border-b border-border/70 bg-background/55 px-4 py-2 text-sm odd:bg-background/70 hover:bg-accent/35" style="grid-template-columns:' + columns + '">' +
        '<div class="flex min-w-0 items-center gap-3">' + avatar + '<div class="min-w-0"><div class="truncate font-medium text-foreground">' + escapeHTML(item.email || goferT("js.admin.unknown_sender")) + '</div><div class="mt-0.5 truncate text-xs text-muted-foreground">' + escapeHTML(item.email_hash || "") + '</div></div></div>' +
        '<div class="truncate text-sm font-medium text-foreground">' + escapeHTML(inUse.source || goferT("js.admin.avatars.source_none")) + '</div>' +
        providerCells +
      '</div>'
    }

    gridTemplateColumns() {
      return 'minmax(22rem,1.4fr) 7rem ' + this.providerColumns.map(function () { return 'minmax(6.5rem,8rem)' }).join(' ')
    }

    providerLabel(provider) {
      if (!provider) return goferT("js.admin.avatars.provider")
      return provider.replace(/_/g, " ").replace(/\b\w/g, function (ch) { return ch.toUpperCase() })
    }

    updateProviderColumns(providers) {
      var next = []
      this.providerColumns.forEach(function (provider) {
        if (next.indexOf(provider) < 0) next.push(provider)
      })
      ;(providers || []).forEach(function (provider) {
        if (next.indexOf(provider) < 0) next.push(provider)
      })
      var changed = next.join("|") !== this.providerColumns.join("|")
      this.providerColumns = next
      this.renderHeader()
      this.updateProviderFilterOptions()
      if (changed && this.items.length > 0) this.renderItems()
    }

    renderHeader() {
      var header = qs("[data-avatar-sender-header]", root)
      if (!header) return
      header.style.gridTemplateColumns = this.gridTemplateColumns()
      header.innerHTML = '<div>' + escapeHTML(goferT("js.admin.avatars.column.sender")) + '</div><div>' + escapeHTML(goferT("js.admin.avatars.column.in_use")) + '</div>' + this.providerColumns.map(function (provider) {
        return '<div>' + escapeHTML(this.providerLabel(provider)) + '</div>'
      }, this).join('')
    }

    updateProviderFilterOptions() {
      var select = qs('[data-avatar-filter="provider"]', root)
      if (!select) return
      var current = select.value || "all"
      this.updateProviderSelect(select, current)
      var eventSelect = qs('[data-avatar-event-filter="provider"]', root)
      if (eventSelect) {
        this.updateProviderSelect(eventSelect, eventSelect.value || "all")
      }
    }

    selectboxItemHTML(value, label, selected) {
      return '<div class="select-item group relative flex w-full cursor-default select-none items-center rounded-sm py-1.5 px-2 text-sm font-light outline-none hover:bg-accent hover:text-accent-foreground focus-visible:bg-accent focus-visible:text-accent-foreground data-[tui-selectbox-selected=true]:bg-accent data-[tui-selectbox-selected=true]:text-accent-foreground" role="option" data-tui-selectbox-value="' + escapeHTML(value) + '" data-tui-selectbox-selected="' + (selected ? 'true' : 'false') + '" data-tui-selectbox-disabled="false" tabindex="0"><span class="truncate select-item-text">' + escapeHTML(label) + '</span><span class="select-check absolute right-2 flex h-3.5 w-3.5 items-center justify-center opacity-0 group-data-[tui-selectbox-selected=true]:opacity-100">✓</span></div>'
    }

    updateProviderSelect(input, current) {
      var selected = this.providerColumns.indexOf(current) >= 0 ? current : "all"
      if (input.tagName === "SELECT") {
        input.innerHTML = '<option value="all">' + escapeHTML(goferT("js.admin.avatars.all_providers")) + '</option>' + this.providerColumns.map(function (provider) {
          return '<option value="' + escapeHTML(provider) + '">' + escapeHTML(this.providerLabel(provider)) + '</option>'
        }, this).join('')
        input.value = selected
        return
      }
      input.value = selected
      var trigger = input.closest(".select-trigger")
      var container = input.closest(".select-container")
      var valueNode = trigger && trigger.querySelector(".select-value")
      if (valueNode) {
        valueNode.textContent = selected === "all" ? goferT("js.admin.avatars.all_providers") : this.providerLabel(selected)
        valueNode.classList.remove("text-muted-foreground")
      }
      var content = container && container.querySelector("[data-tui-selectbox-content]")
      var list = content && content.querySelector(".select-item") && content.querySelector(".select-item").parentElement
      if (!list) return
      list.innerHTML = this.selectboxItemHTML("all", goferT("js.admin.avatars.all_providers"), selected === "all") + this.providerColumns.map(function (provider) {
        return this.selectboxItemHTML(provider, this.providerLabel(provider), selected === provider)
      }, this).join("")
    }

    render() {
      this.maybeLoadMore()
    }

    renderItems() {
      this.itemsContainer.innerHTML = this.items.map(function (item) {
        return this.rowHTML(item)
      }, this).join("")
    }

    appendItems(items) {
      if (!items.length) return
      var html = items.map(function (item) { return this.rowHTML(item) }, this).join("")
      this.itemsContainer.insertAdjacentHTML("beforeend", html)
    }

    setLoader(message) {
      this.loader.textContent = message || ""
      this.loader.hidden = !message
    }

    isVisible() {
      return this.container.clientHeight > 0 && this.container.offsetParent !== null
    }

    maybeLoadMore() {
      if (!this.isVisible()) return
      if (!this.hasMore || this.isLoading) return
      var distance = this.container.scrollHeight - (this.container.scrollTop + this.container.clientHeight)
      if (distance < this.itemHeight * 6) this.loadNext(false)
    }

    loadNext(initial) {
      if (this.isLoading || !this.hasMore) return
      var offset = this.nextOffset
      var limit = this.pageSize
      this.isLoading = true
      this.setLoader(this.items.length === 0 ? goferT("js.admin.avatars.loading_senders") : goferT("js.admin.avatars.loading_more_senders"))
      var params = new URLSearchParams()
      params.set("offset", offset)
      params.set("limit", limit)
      if (this.filters.query) params.set("q", this.filters.query)
      if (this.filters.provider && this.filters.provider !== "all") params.set("provider", this.filters.provider)
      if (this.filters.status && this.filters.status !== "all") params.set("status", this.filters.status)
      if (this.filters.errors) params.set("errors", "true")
      var self = this
      fetch(adminDataURL(api + "/avatars/senders?" + params.toString()), { headers: { "Accept": "application/json" } })
        .then(function (res) { if (!res.ok) throw new Error("status " + res.status); return res.json() })
        .then(function (data) {
          self.totalCount = data.total_count || 0
          var items = data.items || []
          self.updateProviderColumns(self.providersFromResponse(data.providers || [], items))
          self.items = self.items.concat(items)
          self.nextOffset = data.next_offset != null ? data.next_offset : self.items.length
          self.hasMore = !!data.has_more
          self.updateCount()
          if (offset === 0) self.renderItems()
          else self.appendItems(items)
          if (self.items.length === 0) self.setLoader(goferT("js.admin.avatars.no_senders"))
          else if (self.hasMore) self.setLoader(goferT("js.admin.avatars.more_senders"))
          else self.setLoader("")
          if (self.hasMore && self.isVisible()) setTimeout(function () { self.maybeLoadMore() }, 0)
        })
        .catch(function () {
          if (initial) self.itemsContainer.innerHTML = ""
          self.setLoader(goferT("js.admin.avatars.senders_failed"))
        })
        .finally(function () { self.isLoading = false })
    }

    updateCount() {
      var node = qs("[data-avatar-log-count]", root)
      if (!node) return
      node.textContent = goferTn("js.admin.avatars.matching_senders", this.totalCount, { count: fmtNumber(this.totalCount) })
    }

    providersFromResponse(providers, items) {
      var next = providers.slice()
      ;(items || []).forEach(function (item) {
        ;(item.providers || []).forEach(function (provider) {
          if (provider.provider && next.indexOf(provider.provider) < 0) next.push(provider.provider)
        })
      })
      return next
    }
  }

  var attemptList = null
  var eventList = null

  class VirtualAvatarEventList {
    constructor(container) {
      this.container = container
      this.itemHeight = 76
      this.pageSize = 80
      this.items = []
      this.nextOffset = 0
      this.hasMore = true
      this.isLoading = false
      this.totalCount = 0
      this.filters = { query: "", provider: "all", status: "all", errors: false }
      this.itemsContainer = document.createElement("div")
      this.loader = document.createElement("div")
      this.itemsContainer.style.minWidth = "0"
      this.loader.className = "px-4 py-4 text-center text-sm text-muted-foreground"
      this.container.innerHTML = ""
      this.container.appendChild(this.itemsContainer)
      this.container.appendChild(this.loader)
      this.bind()
      this.refresh(true)
    }

    bind() {
      var self = this
      var raf = null
      this.container.addEventListener("scroll", function () {
        if (raf) return
        raf = requestAnimationFrame(function () {
          self.maybeLoadMore()
          raf = null
        })
      })
    }

    setFilters(filters) {
      this.filters = filters
      this.refresh(true)
    }

    refresh(resetScroll) {
      this.items = []
      this.nextOffset = 0
      this.hasMore = true
      this.isLoading = false
      this.totalCount = 0
      this.itemsContainer.innerHTML = ""
      this.setLoader(goferT("js.admin.avatars.loading_events"))
      if (resetScroll) this.container.scrollTop = 0
      this.loadNext(true)
    }

    rowHTML(item) {
      if (!item) {
        return '<div class="grid min-h-[64px] min-w-[58rem] grid-cols-[10rem_minmax(15rem,1fr)_8rem_8rem_minmax(18rem,1.3fr)] items-center gap-3 border-b border-border/70 bg-background/35 px-4 py-2"><div class="h-3 w-24 rounded bg-muted"></div><div class="h-4 w-52 rounded bg-muted"></div><div class="h-4 w-16 rounded bg-muted"></div><div class="h-5 w-16 rounded bg-muted"></div><div class="h-3 w-72 rounded bg-muted"></div></div>'
      }
      var detailClass = item.status === "error" ? "break-words text-xs text-red-600 dark:text-red-300" : "break-words text-xs text-muted-foreground"
      var details = item.message ? escapeHTML(item.message) : ""
      return '<div class="grid min-h-[64px] min-w-[58rem] grid-cols-[10rem_minmax(15rem,1fr)_8rem_8rem_minmax(18rem,1.3fr)] items-center gap-3 border-b border-border/70 bg-background/55 px-4 py-2 text-sm odd:bg-background/70 hover:bg-accent/35">' +
        '<div class="text-xs font-medium text-muted-foreground">' + escapeHTML(fmtTime(item.created_at)) + '</div>' +
        '<div class="min-w-0 truncate font-medium text-foreground">' + escapeHTML(item.email || goferT("js.admin.unknown_sender")) + '</div>' +
        '<div class="truncate text-xs font-semibold uppercase tracking-wider text-muted-foreground">' + escapeHTML(item.provider || goferT("js.admin.avatars.status.unknown")) + '</div>' +
        '<div class="text-sm text-foreground">' + escapeHTML(avatarStatusLabel(item.status || "unknown")) + '</div>' +
        '<div class="min-w-0 ' + detailClass + '">' + details + '</div>' +
      '</div>'
    }

    render() {
      this.maybeLoadMore()
    }

    renderItems() {
      this.itemsContainer.innerHTML = this.items.map(function (item) { return this.rowHTML(item) }, this).join("")
    }

    appendItems(items) {
      if (!items.length) return
      this.itemsContainer.insertAdjacentHTML("beforeend", items.map(function (item) { return this.rowHTML(item) }, this).join(""))
    }

    setLoader(message) {
      this.loader.textContent = message || ""
      this.loader.hidden = !message
    }

    isVisible() {
      return this.container.clientHeight > 0 && this.container.offsetParent !== null
    }

    maybeLoadMore() {
      if (!this.isVisible()) return
      if (!this.hasMore || this.isLoading) return
      var distance = this.container.scrollHeight - (this.container.scrollTop + this.container.clientHeight)
      if (distance < this.itemHeight * 6) this.loadNext(false)
    }

    loadNext(initial) {
      if (this.isLoading || !this.hasMore) return
      var offset = this.nextOffset
      var limit = this.pageSize
      this.isLoading = true
      this.setLoader(this.items.length === 0 ? goferT("js.admin.avatars.loading_events") : goferT("js.admin.avatars.loading_more_events"))
      var params = new URLSearchParams()
      params.set("offset", offset)
      params.set("limit", limit)
      if (this.filters.query) params.set("q", this.filters.query)
      if (this.filters.provider && this.filters.provider !== "all") params.set("provider", this.filters.provider)
      if (this.filters.status && this.filters.status !== "all") params.set("status", this.filters.status)
      if (this.filters.errors) params.set("kind", "errors")
      var self = this
      fetch(adminDataURL(api + "/avatars/attempts?" + params.toString()), { headers: { "Accept": "application/json" } })
        .then(function (res) { if (!res.ok) throw new Error("status " + res.status); return res.json() })
        .then(function (data) {
          self.totalCount = data.total_count || 0
          var items = data.items || []
          self.items = self.items.concat(items)
          self.nextOffset = data.next_offset != null ? data.next_offset : self.items.length
          self.hasMore = !!data.has_more
          self.updateCount()
          if (offset === 0) self.renderItems()
          else self.appendItems(items)
          if (self.items.length === 0) self.setLoader(goferT("js.admin.avatars.no_events"))
          else if (self.hasMore) self.setLoader(goferT("js.admin.avatars.more_events"))
          else self.setLoader("")
          if (self.hasMore && self.isVisible()) setTimeout(function () { self.maybeLoadMore() }, 0)
        })
        .catch(function () {
          if (initial) self.itemsContainer.innerHTML = ""
          self.setLoader(goferT("js.admin.avatars.events_failed"))
        })
        .finally(function () { self.isLoading = false })
    }

    updateCount() {
      var node = qs("[data-avatar-event-count]", root)
      if (!node) return
      node.textContent = goferTn("js.admin.avatars.matching_events", this.totalCount, { count: fmtNumber(this.totalCount) })
    }
  }

  function bindFilters() {
    var query = qs('[data-avatar-filter="query"]', root)
    var provider = qs('[data-avatar-filter="provider"]', root)
    var status = qs('[data-avatar-filter="status"]', root)
    var errors = qs('[data-avatar-filter="errors"]', root)
    var timer = null
    function apply() {
      if (!attemptList) return
      attemptList.setFilters({
        query: filterValue(query, "").trim(),
        provider: filterValue(provider, "all"),
        status: filterValue(status, "all"),
        errors: filterBoolean(errors),
      })
    }
    function schedule() {
      clearTimeout(timer)
      timer = setTimeout(apply, 250)
    }
    if (query) query.addEventListener("input", schedule)
    if (provider) provider.addEventListener("change", apply)
    if (status) status.addEventListener("change", apply)
    if (errors) errors.addEventListener("change", apply)
  }

  function bindEventFilters() {
    var query = qs('[data-avatar-event-filter="query"]', root)
    var provider = qs('[data-avatar-event-filter="provider"]', root)
    var status = qs('[data-avatar-event-filter="status"]', root)
    var errors = qs('[data-avatar-event-filter="errors"]', root)
    var timer = null
    function apply() {
      if (!eventList) return
      eventList.setFilters({
        query: filterValue(query, "").trim(),
        provider: filterValue(provider, "all"),
        status: filterValue(status, "all"),
        errors: filterBoolean(errors),
      })
    }
    function schedule() {
      clearTimeout(timer)
      timer = setTimeout(apply, 250)
    }
    if (query) query.addEventListener("input", schedule)
    if (provider) provider.addEventListener("change", apply)
    if (status) status.addEventListener("change", apply)
    if (errors) errors.addEventListener("change", apply)
  }

  function bindForceRecheck() {
    var form = document.querySelector("[data-avatar-recheck-form]")
    if (!form) return
    form.addEventListener("submit", function (event) {
      event.preventDefault()
      fetch(form.action, { method: "POST", headers: { "Accept": "application/json" } })
        .then(function () { scheduleStatusRefresh() })
        .catch(function () {})
    })
  }

  function bindCancelBackfill() {
    var form = document.querySelector("[data-avatar-cancel-form]")
    if (!form) return
    form.addEventListener("submit", function (event) {
      event.preventDefault()
      var button = qs("button", form)
      if (button) button.disabled = true
      fetch(form.action, { method: "POST", headers: { "Accept": "application/json" } })
        .then(function () { scheduleStatusRefresh() })
        .catch(function () { if (button) button.disabled = false })
    })
  }

  function bindTableUpdateControls() {
    var liveSwitches = root.querySelectorAll("[data-avatar-table-live]")
    if (liveSwitches.length) tableLiveUpdates = liveSwitches[0].checked
    liveSwitches.forEach(function (liveSwitch) {
      liveSwitch.addEventListener("change", function () {
        tableLiveUpdates = liveSwitch.checked
        updateTableControls()
        if (tableLiveUpdates) scheduleActiveTableRefresh(0)
      })
    })
    root.querySelectorAll("[data-avatar-table-refresh]").forEach(function (refreshButton) {
      refreshButton.addEventListener("click", function () {
        if (tableLiveUpdates) return
        refreshActiveTable(true)
      })
    })
    updateTableControls()
  }

  function setupSSE() {
    subscribe({
      "avatar-backfill": function (event) {
        var transition = renderBackfillEvent(event) || {}
        if (transition.wasRunning !== transition.running || transition.status === "canceling") {
          scheduleStatusRefresh(0, true)
        } else {
          scheduleStatusRefresh(10000)
        }
        scheduleActiveTableRefresh(2500)
      },
      "avatar-updated": function () {
        scheduleStatusRefresh(2500)
        scheduleActiveTableRefresh(2500)
      },
    })
  }

  bindForceRecheck()
  bindCancelBackfill()
  bindTabRerender()
  bindFilters()
  bindEventFilters()
  var viewport = qs("[data-avatar-log-viewport]", root)
  if (viewport) attemptList = new VirtualAvatarSenderList(viewport)
  var eventViewport = qs("[data-avatar-event-viewport]", root)
  if (eventViewport) eventList = new VirtualAvatarEventList(eventViewport)
  bindTableUpdateControls()
  setupSSE()
  refreshStatus()
  return handle
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", mountDiagnostics)
  else mountDiagnostics()
  document.addEventListener("htmx:afterSettle", mountDiagnostics)
})()
