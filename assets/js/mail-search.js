// Search scope stays separate from content filters and always travels with
// list requests and actions. Folder choices are committed only on Apply.
;(function () {
  var rememberedFolders = new Map()

  function list() {
    var container = document.getElementById("mail-list-scroll")
    return container && container._virtualMailList
  }

  function scope() {
    var controller = list()
    if (controller) return controller.searchScope || null
    var root = document.getElementById("mail-list")
    try { return JSON.parse(root && root.dataset.mailSearchScope || "null") } catch (_) { return null }
  }

  function scopeURL(url, selected) {
    var parts = url.split("?")
    var params = new URLSearchParams(parts[1] || "")
    ;["scope", "scope_account_id", "scope_folder_id", "include_descendants", "exclude_spam_trash"].forEach(function (name) { params.delete(name) })
    if (selected) {
      params.set("scope", selected.kind)
      if (selected.account_id) params.set("scope_account_id", selected.account_id)
      ;(selected.folder_ids || []).forEach(function (id) { params.append("scope_folder_id", id) })
      if (selected.include_descendants) params.set("include_descendants", "1")
      if (selected.exclude_spam_trash) params.set("exclude_spam_trash", "1")
    }
    var query = params.toString()
    return parts[0] + (query ? "?" + query : "")
  }

  function rowData(id) {
    id = String(id)
    var controller = list()
    var position = controller && controller.indexById.get(id)
    var item = position !== undefined && controller.cache.get(position)
    if (item && item.html) {
      var template = document.createElement("template")
      template.innerHTML = item.html
      var row = template.content.firstElementChild
      if (row) return row.dataset
    }
    return null
  }

  function folderFor(id) {
    var row = rowData(id)
    return row && row.folderId || rememberedFolders.get(String(id)) || ""
  }

  function payload(body) {
    var selected = Object.prototype.hasOwnProperty.call(body, "search_scope") ? body.search_scope : scope()
    if (!selected || !body.targets) return body
    var result = Object.assign({}, body, { search_scope: selected })
    // A move's folder_id is its destination; preserve real destinations.
    if (result.folder_id === "search") delete result.folder_id
    if (result.source_folder_id === "search") delete result.source_folder_id
    result.targets = body.targets.map(function (target) {
      var folder = target.folder_id || folderFor(target.id)
      return folder ? Object.assign({}, target, { folder_id: folder }) : target
    })
    return result
  }

  function sync() {
    var controls = document.querySelector("[data-mail-search-controls]")
    if (!controls) return
    var selected = scope()
    var controller = list()
    if (controller) {
      var origin = new URLSearchParams(window.location.search).get("search_from")
      if (!selected) origin = controller.folderID
      if (origin) controls.dataset.currentFolder = origin
    }
  }

  function restoreReader() {
    sync()
    var selected = new URLSearchParams(window.location.search).get("selected")
    var loading = document.querySelector('#mail-view [aria-label="Loading message"]')
    if (scope() && selected && loading && !loading.dataset.searchRequested) {
      loading.dataset.searchRequested = "true"
      htmx.ajax("GET", window.mailViewRequestURL(selected), {target: "#mail-view", swap: "innerHTML"})
    }
  }

  function navigate(selected) {
    var controller = list()
    if (!controller) return
    var params = new URLSearchParams(window.location.search)
    var origin = controller.searchScope ? params.get("search_from") || "inbox" : controller.folderID
    var serializer = Object.create(controller)
    if (window.readMailFilters) serializer.filters = window.readMailFilters()
    var query = new URLSearchParams(serializer.filterQueryString())
    query.delete("selected")
    if (selected) query.set("search_from", origin)
    else query.delete("search_from")
    var path = selected ? "/mail/search" : "/folder/" + encodeURIComponent(origin)
    var url = scopeURL(path + "?" + query.toString(), selected)
    var request = selected ? url : url.replace(path, path + "/full")
    var previous = window.location.pathname + window.location.search
    var previousState = history.state
    var link = document.querySelector("[data-mail-scope-navigation]")
    link.setAttribute("href", url)
    document.dispatchEvent(new CustomEvent("gofer:mail-folder-clicked", {detail: {folderID: selected ? "search" : origin}}))
    history.pushState({ folder: selected ? "search" : origin, email: null }, "", url)
    return htmx.ajax("GET", request, { source: link, target: "#main-content", swap: "outerHTML" }).catch(function () {
      if (window.location.pathname + window.location.search === url) history.replaceState(previousState, "", previous)
      sync()
      if (window.showGoferToast) showGoferToast({ title: "Couldn't change the search scope", variant: "error", duration: 5000 })
    })
  }

  function dialog() { return document.getElementById("mail-search-folders-dialog") }

  function updatePicker() {
    var root = dialog()
    if (!root) return
    var count = root.querySelectorAll("[data-mail-scope-folder]:checked").length
    root.querySelector("[data-mail-scope-apply]").disabled = count === 0 || count > 200
    root.querySelector("[data-mail-scope-folder-status]").textContent = count > 200 ? "Choose up to 200 folders." : count ? count + " selected · Only locally synced mail is searched." : "Select up to 200 folders. Only locally synced mail is searched."
    var query = root.querySelector("[data-mail-scope-folder-search]").value.trim().toLowerCase()
    var visible = 0
    root.querySelectorAll("[data-mail-scope-account]").forEach(function (account) {
      var matches = 0
      account.querySelectorAll("[data-mail-scope-folder-row]").forEach(function (row) {
        row.hidden = query !== "" && row.dataset.searchText.indexOf(query) === -1
        if (!row.hidden) matches++
      })
      account.hidden = matches === 0
      visible += matches
    })
    root.querySelector("[data-mail-scope-folder-empty]").classList.toggle("hidden", visible !== 0)
  }

  function openPicker() {
    var root = dialog()
    if (!root) return
    var selected = scope()
    var ids = new Set(selected && selected.folder_ids || [])
    root.querySelectorAll("[data-mail-scope-folder]").forEach(function (input) { input.checked = ids.has(input.value) })
    root.querySelector("[data-mail-scope-descendants]").checked = !selected || selected.kind !== "folders" || !!selected.include_descendants
    root.querySelector("[data-mail-scope-folder-search]").value = ""
    updatePicker()
    sync()
    window.tui.dialog.open("mail-search-folders-dialog")
  }

  document.addEventListener("change", function (event) {
    if (event.target.matches("[data-mail-scope-folder]")) updatePicker()
  })
  document.addEventListener("input", function (event) {
    if (event.target.matches("[data-mail-scope-folder-search]")) updatePicker()
  })
  document.addEventListener("click", function (event) {
    var option = event.target.closest("[data-mail-scope-option]")
    if (option) {
      var value = option.dataset.mailScopeOption
      if (value === "folders") return openPicker()
      var current = scope()
      var selected = value === "current" ? null : value === "all" ? { kind: "all" } : { kind: "account", account_id: value.slice(8) }
      if (selected && current && current.exclude_spam_trash) selected.exclude_spam_trash = true
      // Choosing the scope already in use changes nothing.
      if (!selected ? !current : current && current.kind === selected.kind && (current.account_id || "") === (selected.account_id || "")) return
      return navigate(selected)
    }
    if (event.target.closest("[data-mail-scope-exclude]")) {
      var excluding = scope()
      if (excluding) navigate(Object.assign({}, excluding, { exclude_spam_trash: !excluding.exclude_spam_trash }))
      return
    }
    if (event.target.closest("[data-mail-scope-cancel]")) window.tui.dialog.close("mail-search-folders-dialog")
    if (event.target.closest("[data-mail-scope-apply]")) {
      var root = dialog()
      var ids = Array.from(root.querySelectorAll("[data-mail-scope-folder]:checked"), function (input) { return input.value })
      if (!ids.length || ids.length > 200) return
      var selected = { kind: "folders", folder_ids: ids, include_descendants: root.querySelector("[data-mail-scope-descendants]").checked }
      if (scope() && scope().exclude_spam_trash) selected.exclude_spam_trash = true
      window.tui.dialog.close("mail-search-folders-dialog")
      navigate(selected)
    }
  })
  document.addEventListener("htmx:afterSettle", function () { requestAnimationFrame(restoreReader) })
  document.addEventListener("DOMContentLoaded", function () { requestAnimationFrame(restoreReader) })

  window.GoferMailSearch = { scope: scope, scopeURL: scopeURL, payload: payload, folderFor: folderFor, rowData: rowData, sync: sync,
    remember: function (ids) { rememberedFolders.clear(); ids.forEach(function (id) { if (id.folder_id) rememberedFolders.set(String(id.id), id.folder_id) }) },
    navigate: navigate }
})()
