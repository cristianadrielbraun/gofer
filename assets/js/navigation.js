// Screen structure is delivered once in inert templates. HTMX still owns the
// authenticated requests and refreshes each screen's live panes; this router
// owns mounting, URLs and recovery, including moves into and out of Settings.
(function () {
  if (window.GoferNavigation) return

  var tabs = ["accounts", "sync", "rules", "operations", "contacts", "appearance", "regional", "compose-display", "security", "advanced", "export-import", "about"]
  var source = null
  var pending = null
  var previous = null
  var serial = 0
  var held = null
  var queued = null
  var committed = { address: window.location.pathname + window.location.search, state: history.state }
  // Mail, Contacts and Calendar as last left, kept whole: their panes, with
  // their lists' state, scroll and the open item. Going back to one shows it
  // again with no request; what changed meanwhile comes in place afterwards.
  var kept = {}
  // What changed in a kept section while it was away: its sidebar, its
  // content, or both. Live updates report it (markStale). Opening Settings
  // lets them all go.
  var stale = {}

  function route(address) {
    var url = new URL(address, window.location.href)
    if (url.origin !== window.location.origin || url.hash) return null
    var settings = /^\/settings(?:\/([^/]+))?\/?$/.exec(url.pathname)
    if (settings) {
      var tab = settings[1] || "accounts"
      if (tabs.indexOf(tab) === -1) return null
      if (!settings[1]) url.pathname = "/settings/accounts"
      return { mode: "settings", tab: tab, address: url.pathname + url.search }
    }
    var mode = url.pathname === "/contacts" ? "contacts" : url.pathname === "/calendar" ? "calendar" :
      /^\/(?:folder\/[^/]+(?:\/[^/]+)?)?$/.test(url.pathname) ? "mail" : ""
    return mode ? { mode: mode, address: url.pathname + url.search } : null
  }

  function opensItem(destination) {
    var url = new URL(destination.address, window.location.href)
    if (destination.mode === "contacts") return !!url.searchParams.get("contact") || url.searchParams.get("new") === "1"
    return /^\/folder\/[^/]+\/[^/]+$/.test(url.pathname) || !!url.searchParams.get("email")
  }

  function template(id) {
    var node = document.getElementById(id)
    return node && node.content
  }

  function view(mode) {
    if (mode === "calendar") return window.GoferSettings && GoferSettings.get("calendar_view") === "week" ? "week" : "month"
    return window.matchMedia("(min-width: 1024px)").matches && window.GoferSettings && GoferSettings.get(mode === "contacts" ? "contacts_list_view" : "mail_list_view") === "table" ? "table" : "cards"
  }

  function snapshot() {
    return {
      address: committed.address,
      state: committed.state,
      title: document.title,
      panes: [document.querySelector("#app-shell > aside"), document.getElementById("main-content"), document.getElementById("app-pane-dialogs")].filter(Boolean).map(function (root) {
        return { id: root.id, sidebar: root.tagName === "ASIDE", attributes: root.cloneNode(false), children: Array.from(root.childNodes),
          scroll: [root].concat(Array.from(root.querySelectorAll("*"))).filter(function (node) { return node.scrollTop || node.scrollLeft }).map(function (node) { return { node: node, top: node.scrollTop, left: node.scrollLeft } }) }
      }),
    }
  }

  function restore() {
    if (!previous) return
    previous.panes.forEach(function (saved) {
      var root = saved.sidebar ? document.querySelector("#app-shell > aside") : document.getElementById(saved.id)
      if (!root) return
      replaceAppPaneContents(root, saved.attributes, false)
      root.replaceChildren.apply(root, saved.children)
      saved.scroll.forEach(function (scroll) { scroll.node.scrollTop = scroll.top; scroll.node.scrollLeft = scroll.left })
    })
    history.replaceState(previous.state, "", previous.address)
    document.title = previous.title
    // A section kept as the switch began comes back whole: its panes may sit
    // deeper than the saved roots, as the sync controls do in the header.
    var back = route(previous.address)
    if (back && kept[back.mode]) {
      kept[back.mode].panes.forEach(function (pane) {
        var current = document.getElementById(pane.id)
        if (current && current !== pane) current.replaceWith(pane)
      })
      revivePageTriggers(kept[back.mode])
      delete kept[back.mode]
    }
    previous = null
    window.GoferAppNavigation.restored()
  }

  function liveSection() {
    var body = document.getElementById("sidebar-app-body")
    var main = document.getElementById("main-content")
    var mode = body && body.dataset.sidebarAppBody
    if (pending || held || !main || main.hasAttribute("data-navigation-pending") || document.documentElement.hasAttribute("data-app-switching")) return null
    if (mode !== "mail" && mode !== "contacts" && mode !== "calendar") return null
    var here = route(window.location.href)
    return here && here.mode === mode ? mode : null
  }

  // A section is kept only once it has settled. One still loading a part,
  // or polling for one (which stops for good off the page), loads again.
  function keepSection(busy) {
    var mode = liveSection()
    if (!mode) return
    var main = document.getElementById("main-content")
    var dialogs = document.getElementById("app-pane-dialogs")
    var panes = [document.getElementById("sidebar-app-body"), document.getElementById("sidebar-sync-controls")].filter(Boolean)
    var roots = panes.concat([main], dialogs ? [dialogs] : [])
    if (roots.some(function (root) { return root.hasAttribute("inert") || root.querySelector('[aria-busy="true"], [hx-trigger*="every"]') })) {
      dropKept(mode)
      return
    }
    var children = Array.from(main.childNodes)
    var entry = {
      address: window.location.pathname + window.location.search,
      state: history.state,
      title: document.title,
      day: new Date().toDateString(),
      panes: panes,
      main: { attributes: main.cloneNode(false), children: children },
      dialogs: dialogs ? Array.from(dialogs.childNodes) : [],
      scroll: roots.reduce(function (all, root) {
        return all.concat([root].concat(Array.from(root.querySelectorAll("*"))).filter(function (node) { return node.scrollTop || node.scrollLeft }).map(function (node) { return { node: node, top: node.scrollTop, left: node.scrollLeft } }))
      }, []),
    }
    watchPageTriggers(entry, roots)
    // The next section fills these same panes in place, so the kept ones
    // leave the page and empty copies of them take their place.
    panes.forEach(function (pane) { pane.replaceWith(pane.cloneNode(false)) })
    main.replaceChildren.apply(main, children.map(function (child) { return child.cloneNode(false) }))
    if (dialogs) dialogs.replaceChildren()
    kept[mode] = entry
    // Requests the section had going were just called off.
    stale[mode] = busy ? { content: true } : {}
  }

  // An element that loads again on an event of the whole page (hx-trigger
  // "… from:body") stops listening for good if the event comes while it is
  // off the page: HTMX drops a listener whose element it can't find. So a
  // kept section notes when one of those events comes, and its elements
  // start listening again when it is back (revivePageTriggers), loading once.
  function watchPageTriggers(entry, roots) {
    var nodes = []
    var events = {}
    roots.forEach(function (root) {
      root.querySelectorAll('[hx-trigger*="from:"]').forEach(function (node) {
        var found = false
        node.getAttribute("hx-trigger").split(",").forEach(function (part) {
          var words = part.trim().split(/\s+/)
          var from = words.filter(function (word) { return word.indexOf("from:") === 0 })[0]
          var target = from === "from:body" ? document.body : from === "from:document" ? document : from === "from:window" ? window : null
          if (!target || !words[0]) return
          found = true
          events[from + " " + words[0]] = { target: target, name: words[0] }
        })
        if (found) nodes.push(node)
      })
    })
    entry.triggers = { nodes: nodes, fired: false, listeners: [] }
    Object.keys(events).forEach(function (key) {
      var event = events[key]
      var listener = function () { entry.triggers.fired = true }
      event.target.addEventListener(event.name, listener, true)
      entry.triggers.listeners.push({ target: event.target, name: event.name, listener: listener })
    })
  }

  // Lets a kept section go without showing it again.
  function dropKept(mode) {
    var entry = kept[mode]
    if (entry && entry.triggers) entry.triggers.listeners.forEach(function (watch) { watch.target.removeEventListener(watch.name, watch.listener, true) })
    delete kept[mode]
  }

  function revivePageTriggers(entry) {
    var triggers = entry.triggers
    if (!triggers) return
    triggers.listeners.forEach(function (watch) { watch.target.removeEventListener(watch.name, watch.listener, true) })
    if (!triggers.fired || !window.htmx) return
    // A changed attribute makes HTMX set the element up afresh.
    triggers.nodes.forEach(function (node) {
      node.setAttribute("data-kept-revived", String(Date.now()))
      htmx.process(node)
    })
  }

  function markStale(mode, part) {
    if (!kept[mode]) return
    var marks = stale[mode] || (stale[mode] = {})
    if (part === "sidebar" || part === "all") marks.sidebar = true
    if (part !== "sidebar") marks.content = true
  }

  // Puts a kept section back as it was left, sliding the panels to its sizes.
  function reattach(destination, entry, options) {
    var main = document.getElementById("main-content")
    var sidebar = document.querySelector("#app-shell > aside")
    var dialogs = document.getElementById("app-pane-dialogs")
    if (!main || !sidebar || !sidebar.hasAttribute("data-app-sidebar")) return false
    var panels = typeof measureAppPanels === "function" ? measureAppPanels() : null
    entry.panes.forEach(function (pane) {
      var current = document.getElementById(pane.id)
      if (current) current.replaceWith(pane)
    })
    replaceAppPaneContents(main, entry.main.attributes, false)
    main.replaceChildren.apply(main, entry.main.children)
    if (dialogs) dialogs.replaceChildren.apply(dialogs, entry.dialogs)
    entry.scroll.forEach(function (scroll) { scroll.node.scrollTop = scroll.top; scroll.node.scrollLeft = scroll.left })
    revivePageTriggers(entry)
    var marks = stale[destination.mode] || {}
    delete stale[destination.mode]
    // A new day moves the calendar's today.
    if (destination.mode === "calendar" && entry.day !== new Date().toDateString()) marks.content = true
    var state = entry.state || { goferClientPage: true }
    if (!options || !options.history) history.pushState(state, "", destination.address)
    else history.replaceState(state, "", destination.address)
    committed = { address: destination.address, state: history.state }
    window.GoferAppNavigation.reattached(destination.mode, panels, marks)
    document.title = entry.title
    return true
  }

  function mount(destination) {
    var main = document.getElementById("main-content")
    var sidebar = document.querySelector("#app-shell > aside")
    var content = template("app-navigation-" + destination.mode + "-" + (destination.mode === "settings" ? destination.tab : view(destination.mode)))
    if (!main || !sidebar || !content) return false
    if (destination.mode === "settings") {
      replaceAppPaneContents(sidebar, template("app-navigation-settings-sidebar").querySelector("aside"), false)
      main.className = "flex flex-1 min-h-0 min-w-0 overflow-hidden"
      main.removeAttribute("data-mail-pane-layout")
      main.replaceChildren(content.cloneNode(true))
      if (typeof setSettingsSidebarActive === "function") setSettingsSidebarActive(destination.tab)
      document.title = goferT("js.page.title", { page: goferT("js.app.settings") })
    } else if (!main.querySelector("#mail-list") || !sidebar.hasAttribute("data-app-sidebar")) {
      replaceAppPaneContents(sidebar, template("app-navigation-sidebar").querySelector("aside"), false)
      replaceAppPaneContents(main, content.querySelector("#main-content"), false)
    }
    main.setAttribute("data-navigation-pending", "")
    main.setAttribute("aria-busy", "true")
    main.setAttribute("inert", "")
    var dialogs = document.getElementById("app-pane-dialogs")
    if (dialogs) dialogs.replaceChildren()
    window.GoferAppNavigation.prepare(destination.mode)
    window.GoferAppNavigation.bindSidebar()
    if (destination.mode === "settings" && typeof animateSectionContent === "function") animateSectionContent(document.getElementById("settings-content"))
    if (typeof initResizeHandles === "function") initResizeHandles()
    return true
  }

  function requestSource() {
    if (source && source.isConnected) return source
    source = document.createElement("div")
    source.hidden = true
    source.setAttribute("data-client-page-request", "")
    source.setAttribute("hx-sync", "this:replace")
    source.setAttribute("hx-push-url", "false")
    document.body.appendChild(source)
    source.addEventListener("htmx:beforeRequest", function (event) {
      if (pending) event.detail.xhr.goferClientNavigation = pending.serial
    })
    source.addEventListener("htmx:afterRequest", function (event) {
      if (!pending || event.detail.xhr.goferClientNavigation !== pending.serial) return
      if (!event.detail.successful) failed(pending.serial)
    })
    return source
  }

  function failed(id) {
    if (!pending || pending.serial !== id) return
    pending = null
    restore()
    if (typeof showGoferToast === "function") showGoferToast({ title: goferT("js.app.page_failed"), description: goferT("js.common.please_try_again"), variant: "error", icon: "error" })
  }

  function navigate(address, options) {
    var destination = route(address)
    if (!destination || !window.htmx || !window.GoferAppNavigation || !template("app-navigation-settings-sidebar")) return false
    var pane = document.getElementById("compose-pane-form")
    if (pane && (_composePendingUploads(pane) > 0 || pane.dataset.composeSending === "true")) {
      if (options && options.history) history.pushState(committed.state, "", committed.address)
      if (typeof showGoferToast === "function") showGoferToast({ title: goferT("js.app.message_in_progress"), description: goferT("js.app.message_in_progress_help") })
      return true
    }
    // A swap waiting for the panels to finish sliding can't be called off, so
    // a page chosen meanwhile opens right after it lands.
    if (held) {
      queued = { address: address, options: options }
      return true
    }
    var id = ++serial
    var busy = window.GoferAppNavigation.busy()
    // Abort first, before installing the new skeleton. The previous request's
    // completion handlers must not roll back a newer navigation.
    pending = null
    var trigger = requestSource()
    htmx.trigger(trigger, "htmx:abort")
    window.GoferAppNavigation.cancel()
    if (pane && typeof collapseToDialog === "function" && _composeHasDraftContent(pane)) {
      cancelComposeAutosave(pane)
      collapseToDialog()
      scheduleComposeAutosave(document.getElementById("compose-form"))
    }
    window.GoferAppNavigation.remember()
    var entry = kept[destination.mode]
    var reusable = entry && entry.address === destination.address
    // Taken before the section leaves the page, so a failed switch puts
    // back the section itself.
    if (!reusable && !previous) previous = snapshot()
    // Settings can change accounts, folders and anything else a section
    // shows, so the sections load again after it.
    if (destination.mode === "settings") {
      Object.keys(kept).forEach(dropKept)
      stale = {}
    } else if (liveSection() !== destination.mode) keepSection(busy)
    if (reusable) {
      delete kept[destination.mode]
      // Anything the cancelled switch held for a rollback is on screen again.
      previous = null
      if (reattach(destination, entry, options)) return true
      // Without the panes to put it in, it loads as any other section.
      previous = snapshot()
    }
    if (!mount(destination)) { previous = null; return false }
    if (!options || !options.history) history.pushState({ goferClientPage: true }, "", destination.address)
    pending = { serial: id, destination: destination }
    var settings = destination.mode === "settings"
    trigger.setAttribute("hx-select", settings ? "#settings-content" : "#mail-list")
    if (settings) trigger.removeAttribute("hx-select-oob")
    else {
      // With nothing to open, the reader already shows its empty state from the
      // template; swapping in the same content would replay its fade-in.
      var keepReader = destination.mode !== "calendar" && !opensItem(destination) && document.querySelector("#mail-view [data-mail-view-empty]")
      trigger.setAttribute("hx-select-oob", "#sidebar-app-body:outerHTML,#sidebar-sync-controls:outerHTML," + (keepReader ? "" : "#mail-view:outerHTML,") + "#app-pane-dialogs:outerHTML,#compose-account-data:outerHTML")
    }
    htmx.ajax("GET", settings ? destination.address : window.GoferAppNavigation.requestURL(destination.mode, destination.address), {
      source: trigger, target: settings ? "#settings-content" : "#mail-list", swap: "outerHTML",
    }).catch(function () { failed(id) })
    return true
  }

  document.addEventListener("click", function (event) {
    if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return
    var link = event.target.closest && event.target.closest("a[href]")
    if (!link || link.hasAttribute("download") || (link.target && link.target !== "_self")) return
    var destination = route(link.href)
    if (!destination) return
    var settings = destination.mode === "settings"
    var fromSettings = !!document.querySelector("#main-content [data-settings-page]")
    if (!settings && !fromSettings && !link.hasAttribute("data-sidebar-app-button")) return
    if (!pending && destination.address === window.location.pathname + window.location.search) {
      event.preventDefault()
      event.stopImmediatePropagation()
      return
    }
    if (!navigate(destination.address)) return
    event.preventDefault()
    event.stopImmediatePropagation()
  }, true)

  window.addEventListener("popstate", function (event) {
    if (!route(window.location.href)) return
    // Back to the open list, or to one of its messages, is the list's to do
    // in place (mailListPopState in virtual-scroll.js), with no page to load.
    if (typeof mailListPopState === "function" && mailListPopState(window.location.href)) return
    if (navigate(window.location.href, { history: true })) event.stopImmediatePropagation()
    // The address has changed already and nothing else handles these pages.
    else window.location.reload()
  }, true)

  document.addEventListener("htmx:afterSettle", function () {
    if (!pending) committed = { address: window.location.pathname + window.location.search, state: history.state }
  })

  // The new page waits for the panels to finish sliding. Its first render takes
  // long enough to stall a slide it lands in the middle of, and the slide's
  // fastest frames are the first ones. Its sidebar goes in at once.
  function release() {
    held = null
    var next = queued
    queued = null
    if (next) setTimeout(function () { navigate(next.address, next.options) }, 0)
  }

  document.addEventListener("htmx:beforeSwap", function (event) {
    var xhr = event.detail && event.detail.xhr
    if (!pending || !xhr || xhr.goferClientNavigation !== pending.serial || !event.detail.shouldSwap) return
    var sliding = typeof appPanelsSlideRemaining === "function" ? appPanelsSlideRemaining() : 0
    if (sliding <= 16) return
    held = xhr
    if (pending.destination.mode !== "settings") window.GoferAppNavigation.placeSidebarEarly(event.detail.serverResponse, requestSource())
    event.detail.swapOverride = (event.detail.swapOverride || "outerHTML") + " swap:" + Math.ceil(sliding) + "ms"
    setTimeout(function () { if (held === xhr) release() }, sliding + 1000)
  })
  document.addEventListener("htmx:swapError", function (event) {
    if (held && event.detail && event.detail.xhr === held) release()
  })

  // The source survives pane replacement. Only the current request may finish
  // the navigation; the early page-generation guard also rejects late OOB data.
  document.addEventListener("htmx:afterSwap", function (event) {
    var xhr = event.detail && event.detail.xhr
    if (held && xhr === held) release()
    if (!pending || !xhr || xhr.goferClientNavigation !== pending.serial) return
    if (!event.target || event.target.id !== (pending.destination.mode === "settings" ? "settings-content" : "mail-list")) return
    var destination = pending.destination
    pending = null
    previous = null
    var main = document.getElementById("main-content")
    main.removeAttribute("data-navigation-pending")
    main.removeAttribute("aria-busy")
    main.removeAttribute("inert")
    if (typeof refreshComposeAccountData === "function") refreshComposeAccountData()
    window.GoferAppNavigation.loaded(destination.mode, destination.address)
  })

  // A setting changed (other than a panel's size) may change how a section
  // is drawn, so the ones kept away load again.
  document.body.addEventListener("gofer:settings-changed", function (event) {
    var key = event.detail && event.detail.key
    if (/_(width|height)$/.test(key || "")) return
    var here = liveSection()
    Object.keys(kept).forEach(function (mode) { if (mode !== here) dropKept(mode) })
  })

  // A kept section's panes, off the page, for live updates to reach.
  function keptPanes(mode) {
    var entry = kept[mode]
    if (!entry) return []
    return entry.panes.concat(entry.main.children.filter(function (child) { return child.nodeType === 1 }))
  }

  window.GoferNavigation = { navigate: navigate, route: route, markStale: markStale, keptPanes: keptPanes }
})()
