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
    previous = null
    window.GoferAppNavigation.restored()
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
      document.title = "Gofer - Settings"
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
    if (typeof showGoferToast === "function") showGoferToast({ title: "Could not load page", description: "Please try again.", variant: "error", icon: "error" })
  }

  function navigate(address, options) {
    var destination = route(address)
    if (!destination || !window.htmx || !window.GoferAppNavigation || !template("app-navigation-settings-sidebar")) return false
    var pane = document.getElementById("compose-pane-form")
    if (pane && (_composePendingUploads(pane) > 0 || pane.dataset.composeSending === "true")) {
      if (options && options.history) history.pushState(committed.state, "", committed.address)
      if (typeof showGoferToast === "function") showGoferToast({ title: "Message in progress", description: "Wait for sending or attachment uploads to finish before switching pages." })
      return true
    }
    // A swap waiting for the panels to finish sliding can't be called off, so
    // a page chosen meanwhile opens right after it lands.
    if (held) {
      queued = { address: address, options: options }
      return true
    }
    var id = ++serial
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
    if (!previous) previous = snapshot()
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
    if (navigate(window.location.href, { history: true })) event.stopImmediatePropagation()
  }, true)

  document.addEventListener("htmx:afterSettle", function () {
    if (!pending) committed = { address: window.location.pathname + window.location.search, state: history.state }
  })

  // The new page waits for the panels to finish sliding. Its first render takes
  // long enough to stall a slide it lands in the middle of, and the slide's
  // fastest frames are the first ones.
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

  window.GoferNavigation = { navigate: navigate, route: route }
})()
