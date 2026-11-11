// The folder picker the dialogs that choose folders share
// (folder_picker.templ). Typing in its search field shows the folders whose
// account, name or path has the text, so a parent's name finds the folders
// inside it too. An account with no folder left is hidden, unless it has a
// row that stays (data-folder-picker-keep), such as its New folder row.
;(function () {
  // filter applies the search field of the picker inside root and returns
  // the text typed, trimmed.
  function filter(root) {
    if (!root) return ""
    var search = root.querySelector("[data-folder-picker-search]")
    var typed = search ? search.value.trim() : ""
    var query = typed.toLowerCase()
    var visible = 0
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

  window.goferFolderPicker = { filter: filter }
})()
