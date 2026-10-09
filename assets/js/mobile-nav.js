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

  window.GoferMobileNav = { open: function () { setOpen(true) }, close: function () { setOpen(false) } }
})()
