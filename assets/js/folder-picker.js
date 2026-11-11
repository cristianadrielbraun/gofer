// The folder picker the dialogs that choose folders share
// (folder_picker.templ). Typing in its search field shows the folders whose
// account, name or path has the text, so a parent's name finds the folders
// inside it too. An account with no folder left is hidden, unless it has a
// row that stays (data-folder-picker-keep), such as its New folder row,
// which is then named for the search.
;(function () {
  // filter applies the search field of the picker inside root and returns
  // the text typed, trimmed. A new folder's name field being typed in goes.
  function filter(root) {
    if (!root) return ""
    var search = root.querySelector("[data-folder-picker-search]")
    var typed = search ? search.value.trim() : ""
    var query = typed.toLowerCase()
    var visible = 0
    root.querySelectorAll("[data-folder-picker-create-form]").forEach(function (form) { form.remove() })
    root.querySelectorAll("[data-folder-picker-new-label]").forEach(function (label) {
      label.textContent = typed ? goferT("js.mail.move.new_folder_named", { name: typed }) : goferT("js.mail.move.new_folder")
    })
    root.querySelectorAll("[data-folder-picker-account]").forEach(function (account) {
      var matches = 0
      account.querySelectorAll("[data-folder-picker-row]").forEach(function (row) {
        row.hidden = query !== "" && (row.dataset.searchText || "").indexOf(query) === -1
        if (!row.hidden) matches++
      })
      account.hidden = matches === 0 && !account.querySelector("[data-folder-picker-keep]")
      visible += matches
    })
    var empty = root.querySelector("[data-folder-picker-empty]")
    if (empty) empty.classList.toggle("hidden", visible !== 0)
    return typed
  }

  // A row's tooltip names the whole path, but only when the row cuts its
  // name or path short: otherwise its trigger is switched to "manual". This
  // runs before the tooltip's own hover handling, which opens only "hover"
  // triggers.
  document.addEventListener("pointerover", function (event) {
    var trigger = event.target.closest && event.target.closest("[data-folder-picker-tip]")
    if (!trigger) return
    var cut = Array.prototype.some.call(trigger.querySelectorAll(".truncate"), function (text) { return text.scrollWidth > text.clientWidth })
    trigger.setAttribute("data-tui-popover-type", cut ? "hover" : "manual")
  }, true)

  // startCreate names a new folder in the list, where it will be: at the
  // top of an account (data-folder-picker-create-parent "") or inside the
  // folder whose button was pressed. Once the server made it, made(folder)
  // decides what the picker does with it; an error shows under the name.
  function startCreate(button, made) {
    var root = button && button.closest("dialog")
    if (!root) return
    root.querySelectorAll("[data-folder-picker-create-form]").forEach(function (form) { form.remove() })
    var account = button.getAttribute("data-folder-picker-create-account") || ""
    var parent = button.getAttribute("data-folder-picker-create-parent") || ""
    var depth = Number(button.getAttribute("data-folder-picker-create-depth") || 0)
    if (!account) return
    var search = root.querySelector("[data-folder-picker-search]")

    var form = document.createElement("form")
    form.setAttribute("data-folder-picker-create-form", "")
    form.className = "py-1 pr-6"
    form.style.paddingLeft = (24 + Math.min(depth, 8) * 16) + "px"
    var line = document.createElement("div")
    line.className = "flex items-center gap-1.5"
    var input = document.createElement("input")
    input.type = "text"
    input.autocomplete = "off"
    input.placeholder = goferT("js.folders.name")
    input.setAttribute("aria-label", goferT("js.mail.move.new_folder_name"))
    input.className = "h-7 min-w-0 flex-1 rounded-md border border-input bg-background px-2 text-sm placeholder:text-muted-foreground focus:outline-none focus:ring-1 focus:ring-ring"
    input.value = search ? search.value.trim() : ""
    var create = document.createElement("button")
    create.type = "submit"
    create.className = "h-7 shrink-0 rounded-md bg-primary px-2.5 text-xs font-medium text-primary-foreground disabled:opacity-50"
    create.textContent = goferT("js.mail.move.create")
    var problem = document.createElement("p")
    problem.className = "mt-1 hidden text-xs text-destructive"
    line.appendChild(input)
    line.appendChild(create)
    form.appendChild(line)
    form.appendChild(problem)

    var row = button.closest("[data-folder-picker-row]") || button
    row.insertAdjacentElement("afterend", form)
    input.focus()
    input.select()

    input.addEventListener("keydown", function (event) {
      // Escape closes only the new folder's field, not the dialog.
      if (event.key !== "Escape") return
      event.preventDefault()
      event.stopPropagation()
      form.remove()
    })
    form.addEventListener("submit", function (event) {
      event.preventDefault()
      // The picker can sit inside another form, such as a rule's.
      event.stopPropagation()
      var name = input.value.trim()
      if (!name || create.disabled) return
      create.disabled = true
      input.disabled = true
      problem.classList.add("hidden")
      var body = new URLSearchParams()
      body.set("name", name)
      body.set("parent", parent)
      fetch("/api/accounts/" + encodeURIComponent(account) + "/folders", { method: "POST", headers: { Accept: "application/json" }, body: body })
        .then(function (response) {
          if (response.ok) return response.json()
          return response.text().then(function (text) {
            throw new Error(response.status >= 500 && response.status !== 502 ? goferT("js.mail.move.create_failed") : String(text || "").trim())
          })
        })
        .then(function (folder) {
          if (!folder || !folder.id) throw new Error(goferT("js.mail.move.create_failed"))
          if (window.goferRefreshSidebarAccount) window.goferRefreshSidebarAccount(account)
          return made(folder)
        })
        .catch(function (err) {
          create.disabled = false
          input.disabled = false
          problem.textContent = err && err.message ? err.message : goferT("js.mail.move.create_failed")
          problem.classList.remove("hidden")
          input.focus()
        })
    })
  }

  // A picker can sit inside a form, such as a rule's: typing in its own
  // fields neither submits that form nor counts as changing it.
  document.addEventListener("change", function (event) {
    if (event.target.closest && event.target.closest("[data-folder-picker-search], [data-folder-picker-create-form]")) event.stopPropagation()
  }, true)
  document.addEventListener("keydown", function (event) {
    if (event.key === "Enter" && event.target.matches && event.target.matches("[data-folder-picker-search]")) event.preventDefault()
  })

  window.goferFolderPicker = { filter: filter, startCreate: startCreate }
})()
