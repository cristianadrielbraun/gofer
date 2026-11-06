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
    ;["scope", "scope_account_id", "scope_folder_id", "include_descendants", "exclude_spam_trash", "include_local"].forEach(function (name) { params.delete(name) })
    if (selected) {
      params.set("scope", selected.kind)
      if (selected.account_id) params.set("scope_account_id", selected.account_id)
      ;(selected.folder_ids || []).forEach(function (id) { params.append("scope_folder_id", id) })
      if (selected.include_descendants) params.set("include_descendants", "1")
      if (selected.exclude_spam_trash) params.set("exclude_spam_trash", "1")
      if (selected.include_local && selected.kind === "all") params.set("include_local", "1")
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
    if (typeof syncSidebarTagLinks === "function") syncSidebarTagLinks()
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
    var loading = document.querySelector('#mail-view .mail-view-frame[aria-busy="true"]')
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
    // With a search or filters on both sides, this is a change within a
    // search: the rows move rather than the list emptying and refilling.
    // The list still has the filters it was loaded with, before this change.
    var before = !!controller.searchScope || (typeof controller.filterCount === "function" && controller.filterCount() > 0)
    var within = before && !!(window.mailFiltersActive && window.mailFiltersActive())
    // The scope menu stays open while choosing, so it opens again over the
    // new list, which replaces it.
    var menuOpen = !!document.querySelector("#mail-search-scope-menu [data-tui-popover-content]:popover-open")
    if (selected ? enter(controller, url, { move: within }) : leave(controller, url, { move: within })) return Promise.resolve()
    history.pushState({ folder: selected ? "search" : origin, email: null }, "", url)
    return replaceList(controller, request, { move: within }).then(function () {
      if (menuOpen && window.tui && window.tui.popover) window.tui.popover.open("mail-search-scope-menu")
    }).catch(function () {
      controller.cancelTransitionClear()
      if (window.location.pathname + window.location.search === url) history.replaceState(previousState, "", previous)
      sync()
      if (window.showGoferToast) showGoferToast({ title: goferT("js.mail.search.scope_failed"), variant: "error", duration: 5000 })
    })
  }

  // replaceList moves between a folder and search results. Starting or ending
  // a search, the old list fans out and the new one fans in rather than one
  // morphing into the other; the new list loads while the old one leaves and
  // is swapped in once both are done. options.move, for a change within a
  // search, keeps the old rows and hands them to the new list, which moves
  // them (VirtualMailList.moveFrom). Call it after the address changes: a
  // list for an address that has since moved on is dropped.
  function replaceList(controller, request, options) {
    var address = window.location.pathname + window.location.search
    var moving = !!(options && options.move && controller)
    var leaving = !controller || moving ? Promise.resolve() : controller.clearForTransition()
    var loading = fetch(request, { credentials: "same-origin", headers: { "HX-Request": "true", "HX-Target": "main-content", "HX-Current-URL": window.location.href } })
      .then(function (response) {
        if (!response.ok) throw new Error("list unavailable")
        return response.text()
      })
    // The pills of the new list are already dropping in (renderActivePills);
    // the new list keeps them as they end, so it waits for them to end.
    var strip = document.querySelector("[data-mail-active-filter-pills]")
    var settling = strip && strip.getAnimations ? Promise.all(strip.getAnimations({ subtree: true }).map(function (animation) {
      return animation.finished.catch(function () {})
    })) : Promise.resolve()
    return Promise.all([loading, leaving, settling]).then(function (done) {
      if (window.location.pathname + window.location.search !== address) return
      if (moving && window.VirtualMailList) window.VirtualMailList.moveFrom = controller.captureListTransition()
      // The new rows arrive hidden and fan in once the list is set up.
      else if (window.VirtualMailList && window.VirtualMailList.fanInNext) document.documentElement.classList.add("mail-list-entering")
      htmx.swap("#main-content", done[0], { swapStyle: "outerHTML" })
    })
  }

  // scopeFromAddress reads a search scope from an address's query, as
  // scopeURL writes it.
  function scopeFromAddress(address) {
    var params = new URL(address, window.location.origin).searchParams
    var kind = params.get("scope")
    if (!kind) return null
    var selected = { kind: kind }
    if (params.get("scope_account_id")) selected.account_id = params.get("scope_account_id")
    var folders = params.getAll("scope_folder_id")
    if (folders.length) selected.folder_ids = folders
    if (params.get("include_descendants") === "1") selected.include_descendants = true
    if (params.get("exclude_spam_trash") === "1") selected.exclude_spam_trash = true
    if (params.get("include_local") === "1") selected.include_local = true
    return selected
  }

  // enter turns the open list into the search results of an address, in
  // place: the list, header and controls stay, and only the rows are fetched,
  // as a typed search does, rather than the whole mail pane being replaced.
  // The page moves to the address once the rows are in, as a typed search's
  // does (applyFilters with pushUrl). options.move moves the rows instead of
  // fanning them out and in. It answers false when there's no list to switch,
  // for the caller to load the page instead.
  function enter(controller, address, options) {
    var root = document.getElementById("mail-list")
    var url = new URL(address, window.location.origin)
    var selected = scopeFromAddress(url.href)
    if (!controller || !selected || !root || url.pathname !== "/mail/search" ||
      !controller.container || !document.body.contains(controller.container) || typeof controller.applyFilters !== "function") return false
    controller.searchFrom = url.searchParams.get("search_from") || (controller.searchScope ? controller.searchFrom : controller.folderID)
    controller.searchScope = selected
    controller.folderID = "search"
    controller.container.dataset.folderId = "search"
    controller.sidebarTag = controller.readSidebarTagFromURL(url.search)
    root.dataset.mailSearchScope = JSON.stringify(selected)
    root.setAttribute("data-mail-search-results", "")
    var menu = controls()
    if (menu) {
      delete menu.dataset.pendingScope
      if (controller.searchFrom) menu.dataset.currentFolder = controller.searchFrom
    }
    showScope(selected)
    var heading = document.getElementById("mail-folder-name")
    if (heading) heading.textContent = goferT("js.mail.search_results")
    var link = document.querySelector("[data-mail-scope-navigation]")
    if (link) link.setAttribute("href", url.pathname + url.search)
    var filters = controller.readFiltersFromURL(url.search)
    controller.filters = filters
    controller.syncFilterInputs()
    if (typeof syncMailArchiveActionState === "function") syncMailArchiveActionState()
    var before = window.location.pathname + window.location.search
    controller.applyFilters(filters, { replace: !(options && options.move), pushUrl: true }).catch(function () {
      // The rows couldn't load: load the whole page, as before.
      if (window.location.pathname + window.location.search !== before) return
      history.pushState({ folder: "search", email: null }, "", url.pathname + url.search)
      htmx.ajax("GET", url.pathname + url.search, { target: "#main-content", swap: "outerHTML" })
    })
    return true
  }

  // markFolder marks a folder in the sidebar as the open one, as the server
  // draws it, and unmarks the rest.
  function markFolder(folderID) {
    var aside = document.querySelector("aside")
    if (!aside) return
    var on = ["bg-sidebar-accent", "text-sidebar-primary", "font-medium"]
    var off = ["text-sidebar-foreground", "hover:bg-sidebar-accent/60", "hover:text-sidebar-accent-foreground"]
    var target = aside.querySelector('a[hx-get="/folder/' + folderID + '"]')
    aside.querySelectorAll("a[hx-get^='/folder/'], [data-sidebar-folder-row]").forEach(function (link) {
      var active = link === target || (target && link.contains(target)) || (target && target.closest("[data-sidebar-folder-row]") === link)
      on.forEach(function (name) { link.classList.toggle(name, !!active) })
      off.forEach(function (name) { link.classList.toggle(name, !active) })
      var badge = link.querySelector("[data-folder-unread]")
      if (badge) {
        badge.classList.toggle("bg-sidebar-primary/20", !!active)
        badge.classList.toggle("text-sidebar-primary", !!active)
        badge.classList.toggle("bg-sidebar-accent", !active)
        badge.classList.toggle("text-sidebar-foreground", !active)
      }
    })
  }

  // leave turns search results back into the folder list of an address
  // (/folder/<id>), in place, as enter does the other way: only the rows are
  // fetched, and the page moves to the address once they are in.
  function leave(controller, address, options) {
    var root = document.getElementById("mail-list")
    var url = new URL(address, window.location.origin)
    var match = /^\/folder\/([^/]+)$/.exec(url.pathname)
    if (!controller || !match || !root || !controller.searchScope ||
      !controller.container || !document.body.contains(controller.container) || typeof controller.applyFilters !== "function") return false
    var folderID = decodeURIComponent(match[1])
    controller.searchScope = null
    controller.searchFrom = null
    controller.folderID = folderID
    controller.container.dataset.folderId = folderID
    controller.sidebarTag = controller.readSidebarTagFromURL(url.search)
    root.dataset.mailSearchScope = ""
    root.removeAttribute("data-mail-search-results")
    var menu = controls()
    if (menu) {
      delete menu.dataset.pendingScope
      menu.dataset.currentFolder = folderID
    }
    showScope(null)
    var link = document.querySelector("[data-mail-scope-navigation]")
    if (link) link.setAttribute("href", url.pathname + url.search)
    var folderLink = document.querySelector('aside a[hx-get="/folder/' + folderID + '"] span.truncate')
    var heading = document.getElementById("mail-folder-name")
    if (heading && folderLink) {
      heading.textContent = folderLink.textContent.trim()
      heading.setAttribute("data-mail-folder-default-name", folderLink.textContent.trim())
    }
    var filters = controller.readFiltersFromURL(url.search)
    controller.filters = filters
    controller.syncFilterInputs()
    if (typeof syncMailArchiveActionState === "function") syncMailArchiveActionState()
    if (typeof syncSidebarTagLinks === "function") syncSidebarTagLinks(url.pathname + url.search)
    markFolder(folderID)
    var before = window.location.pathname + window.location.search
    controller.applyFilters(filters, { replace: !(options && options.move), pushUrl: true }).catch(function () {
      if (window.location.pathname + window.location.search !== before) return
      history.pushState({ folder: folderID, email: null }, "", url.pathname + url.search)
      htmx.ajax("GET", url.pathname.replace(/^(\/folder\/[^/]+)$/, "$1/full") + url.search, { target: "#main-content", swap: "outerHTML" })
    })
    return true
  }

  function dialog() { return document.getElementById("mail-search-folders-dialog") }

  function updatePicker() {
    var root = dialog()
    if (!root) return
    var count = root.querySelectorAll("[data-mail-scope-folder]:checked").length
    root.querySelector("[data-mail-scope-apply]").disabled = count === 0 || count > 200
    root.querySelector("[data-mail-scope-folder-status]").textContent = count > 200 ? goferT("js.mail.search.too_many") : count ? goferTn("js.mail.search.selected", count) : goferT("js.mail.search.folders_status")
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

  function controls() { return document.querySelector("[data-mail-search-controls]") }

  // pending is a scope chosen while there was nothing to search. It waits on
  // the menu until a search or filter needs it, rather than listing all of
  // the scope's mail.
  function pending() {
    var root = controls()
    try { return JSON.parse(root && root.dataset.pendingScope || "null") } catch (_) { return null }
  }

  // chosen is the scope the menu shows: the open search's, or a waiting one.
  function chosen() { return scope() || pending() }

  // choose applies a scope from the menu or the folder picker. Search results
  // and filtered lists move to it at once; otherwise it waits for a search.
  function choose(selected) {
    var filtering = window.mailFiltersActive && window.mailFiltersActive()
    if (scope() || (selected && filtering)) return navigate(selected)
    var root = controls()
    if (!root) return
    if (selected) root.dataset.pendingScope = JSON.stringify(selected)
    else delete root.dataset.pendingScope
    showScope(selected)
  }

  function folderSelectionLabel(selected) {
    var count = (selected.folder_ids || []).length
    var label = goferTn("js.mail.search.folders", count)
    return selected.include_descendants ? goferT("js.mail.search.with_subfolders", { folders: label }) : label
  }

  // showScope draws a scope on the menu as the server would.
  function showScope(selected) {
    var root = controls()
    if (!root) return
    var kind = selected ? selected.kind : "current"
    var value = !selected ? "current" : selected.kind === "account" ? "account:" + selected.account_id : selected.kind
    var option = root.querySelector('[data-mail-scope-option="' + value + '"]')
    var label = !selected ? goferT("js.mail.search.scope_current") : kind === "all" ? goferT("js.mail.search.scope_all") : kind === "account" ? (option ? option.textContent.trim() : goferT("js.mail.search.scope_one_account"))
      : goferTn("js.mail.search.folders", (selected.folder_ids || []).length)
    var trigger = root.querySelector("[data-mail-scope-trigger]")
    if (trigger) {
      trigger.setAttribute("aria-label", goferT("js.mail.search.scope_label", { scope: label }))
      trigger.querySelector(".mail-search-scope-label").textContent = label
      trigger.querySelectorAll("[data-mail-scope-icon]").forEach(function (icon) { icon.classList.toggle("hidden", icon.dataset.mailScopeIcon !== kind) })
    }
    root.querySelectorAll("[data-mail-scope-option]").forEach(function (item) {
      var checked = item.dataset.mailScopeOption === value && kind !== "folders"
      if (item.dataset.mailScopeOption !== "folders") item.setAttribute("aria-checked", checked ? "true" : "false")
      var check = item.querySelector("[data-mail-scope-check]")
      if (check) check.classList.toggle("hidden", !checked)
    })
    var folders = root.querySelector("[data-mail-scope-folders-label]")
    if (folders) folders.textContent = kind === "folders" ? goferT("js.mail.search.edit_folders", { folders: folderSelectionLabel(selected) }) : goferT("js.mail.search.choose_folders")
    var exclude = root.querySelector("[data-mail-scope-exclude]")
    if (exclude) {
      var excluding = !!(selected && selected.exclude_spam_trash)
      exclude.setAttribute("aria-disabled", selected ? "false" : "true")
      exclude.setAttribute("aria-checked", excluding ? "true" : "false")
      exclude.querySelector("[data-mail-scope-check]").classList.toggle("hidden", !excluding)
    }
    var local = root.querySelector("[data-mail-scope-include-local]")
    if (local) {
      var including = !!(selected && selected.kind === "all" && selected.include_local)
      var row = local.closest("[data-mail-scope-include-local-row]")
      var shown = !!(selected && selected.kind === "all")
      row.dataset.shown = shown ? "true" : "false"
      row.inert = !shown
      local.setAttribute("aria-checked", including ? "true" : "false")
      local.querySelector("[data-mail-scope-check]").classList.toggle("hidden", !including)
    }
  }

  function openPicker() {
    var root = dialog()
    if (!root) return
    var selected = chosen()
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
      var current = chosen()
      var selected = value === "current" ? null : value === "all" ? { kind: "all" } : { kind: "account", account_id: value.slice(8) }
      if (selected && current && current.exclude_spam_trash) selected.exclude_spam_trash = true
      if (selected && current && current.include_local) selected.include_local = true
      // Choosing the scope already in use changes nothing.
      if (!selected ? !current : current && current.kind === selected.kind && (current.account_id || "") === (selected.account_id || "")) return
      return choose(selected)
    }
    if (event.target.closest("[data-mail-scope-include-local]")) {
      var including = chosen()
      if (including && including.kind === "all") choose(Object.assign({}, including, { include_local: !including.include_local }))
      return
    }
    if (event.target.closest("[data-mail-scope-exclude]")) {
      var excluding = chosen()
      if (excluding) choose(Object.assign({}, excluding, { exclude_spam_trash: !excluding.exclude_spam_trash }))
      return
    }
    if (event.target.closest("[data-mail-scope-cancel]")) window.tui.dialog.close("mail-search-folders-dialog")
    if (event.target.closest("[data-mail-scope-apply]")) {
      var root = dialog()
      var ids = Array.from(root.querySelectorAll("[data-mail-scope-folder]:checked"), function (input) { return input.value })
      if (!ids.length || ids.length > 200) return
      var selected = { kind: "folders", folder_ids: ids, include_descendants: root.querySelector("[data-mail-scope-descendants]").checked }
      if (chosen() && chosen().exclude_spam_trash) selected.exclude_spam_trash = true
      window.tui.dialog.close("mail-search-folders-dialog")
      choose(selected)
    }
  })
  document.addEventListener("htmx:afterSettle", function () { requestAnimationFrame(restoreReader) })
  document.addEventListener("DOMContentLoaded", function () { requestAnimationFrame(restoreReader) })

  // touches reports whether a sync event's folder is one the open search
  // reads, so new or changed mail there refreshes its results. The folder
  // picker knows every folder's account and parent.
  function touches(event) {
    var selected = scope()
    if (!selected || !event || !event.folder_id) return false
    var role = event.folder_role || ""
    if (selected.exclude_spam_trash && (role === "spam" || role === "junk" || role === "trash")) return false
    if (selected.kind === "all") return true
    if (selected.kind === "account") return event.account_id === selected.account_id
    var chosen = new Set(selected.folder_ids || [])
    var parents = new Map()
    var root = dialog()
    if (root) root.querySelectorAll("[data-mail-scope-folder]").forEach(function (input) { parents.set(input.value, input.dataset.parentId || "") })
    for (var id = event.folder_id, depth = 0; id && depth < 64; id = parents.get(id), depth++) {
      if (chosen.has(id)) return true
      if (!selected.include_descendants) return false
    }
    return false
  }

  window.GoferMailSearch = { scope: scope, scopeURL: scopeURL, payload: payload, folderFor: folderFor, rowData: rowData, sync: sync, touches: touches,
    remember: function (ids) { rememberedFolders.clear(); ids.forEach(function (id) { if (id.folder_id) rememberedFolders.set(String(id.id), id.folder_id) }) },
    navigate: navigate, replaceList: replaceList, enter: enter, pending: pending }
})()
