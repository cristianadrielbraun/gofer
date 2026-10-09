// Small-screen navigation drawer. The sidebar is a drawer below the lg breakpoint;
// this opens and closes it. Loaded from the top bar, which htmx may swap in again.
(function () {
  if (window.GoferMobileNav) return

  var root = document.documentElement
  var desktop = window.matchMedia("(min-width: 1024px)")

  function isOpen() {
    return root.hasAttribute("data-mobile-nav-open")
  }

  function setOpen(open) {
    if (open === isOpen()) return
    root.toggleAttribute("data-mobile-nav-open", open)
    document.querySelectorAll("[data-mobile-nav-toggle]").forEach(function (button) {
      button.setAttribute("aria-expanded", String(open))
    })
    var drawer = document.querySelector(".app-drawer")
    if (open && drawer) {
      var current = drawer.querySelector("[aria-current], .bg-sidebar-accent") || drawer
      if (current.scrollIntoView) current.scrollIntoView({ block: "nearest" })
    }
  }

  document.addEventListener("click", function (event) {
    var target = event.target && event.target.closest ? event.target : null
    if (!target) return
    if (target.closest("[data-mobile-nav-toggle]")) {
      setOpen(!isOpen())
      return
    }
    if (target.closest("[data-mobile-nav-close]")) {
      setOpen(false)
      return
    }
  })

  // Following a link in the drawer navigates away, so the drawer has done its job. This
  // runs in the capture phase because the folder links stop propagation.
  document.addEventListener("click", function (event) {
    var target = event.target && event.target.closest ? event.target : null
    if (target && isOpen() && target.closest(".app-drawer a[href], .app-drawer #sidebar-compose-btn")) setOpen(false)
  }, true)

  document.addEventListener("keydown", function (event) {
    if (event.key === "Escape" && isOpen()) setOpen(false)
  })

  window.addEventListener("popstate", function () { setOpen(false) })
  desktop.addEventListener("change", function (event) { if (event.matches) setOpen(false) })

  // The top bar names the open mail folder. The list rewrites its own heading from
  // several places, so the bar mirrors that heading instead of being told directly.
  var titleFrame = 0
  function syncTitle() {
    titleFrame = 0
    syncPlaceholders(true)
    var bar = document.querySelector("[data-mobile-topbar]")
    if (!bar) return
    var heading = document.querySelector("#main-content #mail-folder-name, #main-content [data-contacts-title]")
    var name = heading ? heading.textContent.trim() : ""
    var slot = bar.querySelector("[data-mobile-topbar-title]")
    if (!slot) return
    if (slot.querySelector("[data-mobile-topbar-title-text]").textContent !== name) {
      slot.querySelector("[data-mobile-topbar-title-text]").textContent = name
    }
    var countEl = document.querySelector("#main-content #mail-folder-count, #main-content #contacts-count")
    var count = countEl ? countEl.textContent.trim() : ""
    var countSlot = slot.querySelector("[data-mobile-topbar-title-count]")
    if (countSlot.textContent !== count) countSlot.textContent = count
    bar.toggleAttribute("data-mobile-topbar-titled", name !== "")
    syncTopbarActionState(bar)
  }

  // The top bar's filter badge and search highlight follow whichever list is open.
  function syncTopbarActionState(bar) {
    var mirror = bar.querySelector("[data-mail-filter-count-mirror]")
    var searchButton = bar.querySelector('[data-mobile-mail-action="search"]')
    var badge = document.querySelector("#main-content [data-mail-filter-count], #main-content [data-contact-filter-count]")
    var count = badge && !badge.classList.contains("hidden") ? badge.textContent.trim() : ""
    if (count === "0") count = ""
    if (mirror) {
      if (mirror.textContent !== count) mirror.textContent = count
      mirror.classList.toggle("hidden", count === "")
    }
    var mailSearch = document.querySelector("#main-content [data-mail-search-input]")
    var contactSearch = document.querySelector("#main-content [data-contact-search-input]")
    var searching = mailSearch ? !!(mailSearch.dataset.mailCommittedQuery || "").trim() : !!(contactSearch && contactSearch.value.trim())
    if (searchButton) searchButton.toggleAttribute("data-active", searching)
  }
  // Inputs may carry a shorter placeholder for small screens.
  function syncPlaceholders(small) {
    document.querySelectorAll("[data-mobile-placeholder]").forEach(function (input) {
      if (small && !input.hasAttribute("data-desktop-placeholder")) {
        input.setAttribute("data-desktop-placeholder", input.placeholder)
        input.placeholder = input.getAttribute("data-mobile-placeholder")
      } else if (!small && input.hasAttribute("data-desktop-placeholder")) {
        input.placeholder = input.getAttribute("data-desktop-placeholder")
        input.removeAttribute("data-desktop-placeholder")
      }
    })
  }
  function scheduleTitleSync() {
    if (!titleFrame) titleFrame = requestAnimationFrame(syncTitle)
  }
  // Opening a message or contact on a phone slides it in over the list, and going back
  // slides the list in again. This watches the reader pane go from empty to showing
  // something and back, so every way in and out (taps, back buttons, history) gets the
  // same motion. The observer callback runs before the next paint, so the animation
  // starts on the frame the pane first appears.
  var paneOpen = null
  var paneTimer = 0
  var paneClosing = false
  function checkPaneState() {
    var open = !!document.querySelector("#main-content > [data-mail-reader] > :not([data-mail-view-empty])")
    if (paneOpen === null || !document.querySelector("#main-content > [data-mail-reader]")) {
      paneOpen = open
      return
    }
    if (open === paneOpen) return
    paneOpen = open
    if (paneClosing) {
      // animatePaneClose already slid the reader out and the list in.
      paneClosing = false
      root.removeAttribute("data-pane-anim")
      return
    }
    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) return
    clearTimeout(paneTimer)
    root.setAttribute("data-pane-anim", open ? "open" : "close")
    paneTimer = setTimeout(function () { root.removeAttribute("data-pane-anim") }, 320)
  }

  // Closing mirrors opening: the reader slides out to the right over the list, and
  // only then is it cleared. clear() empties the reader; it is skipped if something
  // else has replaced the reader's content in the meantime.
  function animatePaneClose(clear) {
    var reader = document.querySelector("#main-content > [data-mail-reader]")
    var content = reader && reader.firstElementChild
    var open = !!(content && !content.hasAttribute("data-mail-view-empty"))
    if (desktop.matches || !open || window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
      clear()
      return
    }
    if (root.getAttribute("data-pane-anim") === "closing") return
    clearTimeout(paneTimer)
    root.setAttribute("data-pane-anim", "closing")
    paneTimer = setTimeout(function () {
      if (reader.isConnected && reader.firstElementChild === content) {
        paneClosing = true
        clear()
      }
      // If nothing changed (the reader was replaced, or clear() left it open), finish here.
      setTimeout(function () {
        if (root.getAttribute("data-pane-anim") === "closing") {
          paneClosing = false
          root.removeAttribute("data-pane-anim")
        }
      }, 0)
    }, 240)
  }

  var titleObserver = new MutationObserver(function () {
    checkPaneState()
    scheduleTitleSync()
  })
  function watchTitle() {
    if (desktop.matches) {
      titleObserver.disconnect()
      syncPlaceholders(false)
      return
    }
    titleObserver.observe(document.body, { subtree: true, childList: true, characterData: true })
    scheduleTitleSync()
  }
  desktop.addEventListener("change", watchTitle)
  if (document.body) watchTitle()
  else document.addEventListener("DOMContentLoaded", watchTitle)

  // Mail sync status: while a sync runs the top bar shows "Syncing…" in place of the
  // folder count and a thin line along its bottom edge; a failure or partial result
  // shows briefly in the same place and opens the progress dialog when tapped.
  var syncTimer = 0
  var syncClick = null
  function showSyncStatus(opts) {
    if (desktop.matches) return false
    var bar = document.querySelector("[data-mobile-topbar]")
    var status = bar && bar.querySelector("[data-mobile-topbar-sync]")
    if (!status || !bar.hasAttribute("data-mobile-topbar-titled")) return false
    clearTimeout(syncTimer)
    syncClick = opts.onClick || null
    var running = opts.icon === "spinner"
    var problem = opts.variant === "error" || opts.variant === "warning"
    var state = running ? "running" : (problem ? opts.variant : "")
    if (state) bar.setAttribute("data-mobile-sync", state)
    else bar.removeAttribute("data-mobile-sync")
    status.textContent = running ? "Syncing…" : (problem ? (opts.title || "Sync problem") : "")
    status.setAttribute("aria-label", status.textContent ? status.textContent + ", show sync progress" : "")
    if (!running && problem) {
      syncTimer = setTimeout(function () {
        bar.removeAttribute("data-mobile-sync")
        status.textContent = ""
      }, Number(opts.duration) || 8000)
    }
    return true
  }

  document.addEventListener("click", function (event) {
    if (event.target && event.target.closest && event.target.closest("[data-mobile-topbar-sync]") && syncClick) syncClick()
  })

  // A tap outside an open bottom sheet only closes the sheet. These listeners run
  // first (window, capture phase) and keep the tap from reaching whatever sits under
  // it, so it cannot open a message, start a long press or press a button. Taps
  // inside any open popover (the sheet, or a picker opened from it) pass through.
  function openSheet() {
    return document.querySelector('.mobile-sheet[data-tui-popover-open="true"]')
  }
  function guardSheetTap(event) {
    if (desktop.matches) return
    var sheet = openSheet()
    if (!sheet) return
    var target = event.target
    if (target && target.closest && target.closest(":popover-open")) return
    event.stopPropagation()
    if (event.type === "click") {
      event.preventDefault()
      if (window.tui && window.tui.popover) window.tui.popover.closeElement(sheet)
    }
  }
  ;["pointerdown", "mousedown", "touchstart", "touchend", "click"].forEach(function (type) {
    window.addEventListener(type, guardSheetTap, { capture: true })
  })

  window.GoferMobileNav = {
    showSyncStatus: showSyncStatus,
    animatePaneClose: animatePaneClose, open: function () { setOpen(true) }, close: function () { setOpen(false) } }
})()
