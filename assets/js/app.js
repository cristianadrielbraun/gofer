document.addEventListener("DOMContentLoaded", function () {
  var virtualMailList = null

  initVirtualScroll()
  setupFolderClickInterception()
  setupEmailSelectionTracking()

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
})
