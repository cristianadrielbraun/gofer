document.addEventListener("DOMContentLoaded", function () {
  var virtualMailList = null

  initVirtualScroll()
  setupFolderClickInterception()
  setupEmailSelectionTracking()
  setupTestButtonAnimation()

  function initVirtualScroll() {
    var container = document.getElementById("mail-list-scroll")
    if (!container) return

    var folderID = container.dataset.folderId || "inbox"
    virtualMailList = new VirtualMailList(container, { folderID: folderID })
    virtualMailList.hydrateFromDOM()
  }

  function setupFolderClickInterception() {
    var sidebar = document.querySelector("aside")
    if (!sidebar) return

    sidebar.addEventListener("click", function (e) {
      var link = e.target.closest('a[hx-get^="/folder/"]')
      if (!link) return
      if (!virtualMailList) return

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

      virtualMailList.switchFolder(folderID)
    })
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

  function setupTestButtonAnimation() {
    document.body.addEventListener("htmx:beforeRequest", function (e) {
      var btn = e.detail.elt
      if (!btn || !btn.dataset || !btn.dataset.testBtn) return

      window["_testBtn_" + btn.id] = btn.outerHTML

      var w = btn.offsetWidth
      btn.style.width = w + "px"
      btn.style.whiteSpace = "nowrap"
      btn.style.pointerEvents = "none"
      btn.disabled = true

      btn.innerHTML =
        '<div class="size-3.5 shrink-0 border-2 border-muted-foreground/30 border-t-muted-foreground rounded-full animate-spin"></div>' +
        '<span class="test-anim-text" style="opacity:0;transition:opacity 0.2s ease 0.15s">Testing...</span>'

      btn.style.width = "auto"
      var newW = btn.offsetWidth
      btn.style.width = w + "px"

      void btn.offsetWidth
      btn.style.transition = "width 0.3s ease"
      btn.style.width = newW + "px"

      requestAnimationFrame(function () {
        var text = btn.querySelector(".test-anim-text")
        if (text) text.style.opacity = "1"
      })

      setTimeout(function () {
        btn.style.width = ""
        btn.style.transition = ""
        btn.style.whiteSpace = ""
      }, 400)
    })

    document.body.addEventListener("htmx:afterSettle", function () {
      var el = document.querySelector("[data-test-restore]")
      if (!el) return
      var id = el.id
      setTimeout(function () {
        restoreTestButton(id)
      }, 3000)
    })
  }
})

window.restoreTestButton = function (id) {
  var original = window["_testBtn_" + id]
  if (!original) return

  var el = document.getElementById(id)
  if (!el) return

  el.style.transition = "opacity 0.3s ease, transform 0.3s ease"
  el.style.opacity = "0"
  el.style.transform = "scale(0.95)"

  setTimeout(function () {
    el.outerHTML = original
    var restored = document.getElementById(id)
    if (restored) {
      restored.style.opacity = "0"
      restored.style.transform = "scale(0.95)"
      requestAnimationFrame(function () {
        restored.style.transition = "opacity 0.3s ease, transform 0.3s ease"
        restored.style.opacity = "1"
        restored.style.transform = "scale(1)"
        setTimeout(function () {
          restored.style.transition = ""
          restored.style.transform = ""
          restored.style.opacity = ""
        }, 300)
      })
    }
  }, 300)
}

window.toggleTestError = function (id) {
  var el = document.getElementById(id)
  if (!el) return
  el.classList.toggle("hidden")
}
