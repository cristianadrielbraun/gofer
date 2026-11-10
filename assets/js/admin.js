(function () {
  "use strict"

  function bindTOTPVerification(form) {
    if (form.dataset.adminSecurityTotpBound === "true") return
    form.dataset.adminSecurityTotpBound = "true"

    var status = form.querySelector("[data-admin-security-totp-status]")
    var submit = form.querySelector("button[type='submit']")
    form.addEventListener("submit", function (event) {
      event.preventDefault()
      if (submit) submit.disabled = true
      if (status) {
        status.classList.remove("text-destructive")
        status.textContent = goferT("js.admin.verify.verifying")
      }

      fetch(form.action, {
        method: "POST",
        credentials: "same-origin",
        headers: { "Accept": "application/json" },
        body: new URLSearchParams(new FormData(form)),
      }).then(function (response) {
        return response.json().catch(function () { return {} }).then(function (result) {
          if (!response.ok) throw new Error(result.error || goferT("js.admin.verify.failed"))
          // A change waiting for this verification is sent instead.
          if (!form.dispatchEvent(new CustomEvent("gofer:verified", { bubbles: true, cancelable: true }))) return
          window.location.assign(result.redirect || form.dataset.successRedirect || window.location.href)
        })
      }).catch(function (error) {
        if (status) {
          status.classList.add("text-destructive")
          status.textContent = error && error.message ? error.message : goferT("js.admin.verify.failed")
        }
        if (submit) submit.disabled = false
      })
    })
  }

  // Non-modal verification lets password-manager menus outside the dialog receive clicks.
  // Native non-modal dialogs do not handle Escape, so retain the expected dismissal here.
  document.addEventListener("keydown", function (event) {
    if (event.key !== "Escape" || event.defaultPrevented) return
    var root = document.getElementById("admin-security-verification-dialog")
    var panel = root && root.querySelector("dialog[open]")
    if (!panel) return
    window.tui.dialog.close(root)
    var trigger = document.querySelector('[data-tui-dialog-target="admin-security-verification-dialog"] button')
    if (trigger) trigger.focus()
  })

  function bind(root) {
    ;(root || document).querySelectorAll("[data-admin-security-totp]").forEach(bindTOTPVerification)
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", function () { bind(document) })
  } else {
    bind(document)
  }
  document.addEventListener("htmx:afterSwap", function (event) { bind(event.target) })
})();

(function () {
  "use strict"

  // Changes that need a recently verified session carry data-step-up. When
  // the page has the verification dialog, the session must verify again:
  // sending such a change opens verification first, and the change goes out
  // once it succeeds, with nothing typed into it lost. The server still
  // checks, so a page that missed the session going stale is refused there.
  var waiting = null

  function verificationDialog() {
    return document.getElementById("admin-security-verification-dialog")
  }

  document.addEventListener("submit", function (event) {
    var form = event.target
    if (!form || !form.matches || !form.matches("form[data-step-up]")) return
    var verification = verificationDialog()
    if (!verification || !window.tui || !window.tui.dialog) return
    event.preventDefault()
    var holder = form.closest("[data-tui-dialog]")
    if (holder === verification) holder = null
    waiting = { form: form, submitter: event.submitter && event.submitter.form === form ? event.submitter : null, holder: holder }
    // Verification can't be used under another modal dialog.
    if (holder) window.tui.dialog.close(holder)
    window.tui.dialog.open(verification)
  })

  // The verification forms announce success before leaving the page. The
  // change is sent as it was, without its submit handlers running again: a
  // form that asked a question first (data-confirm) doesn't ask twice.
  document.addEventListener("gofer:verified", function (event) {
    if (!waiting) return
    event.preventDefault()
    var change = waiting
    waiting = null
    if (change.submitter && change.submitter.name) {
      var pressed = document.createElement("input")
      pressed.type = "hidden"
      pressed.name = change.submitter.name
      pressed.value = change.submitter.value
      change.form.appendChild(pressed)
    }
    HTMLFormElement.prototype.submit.call(change.form)
  })

  // Closing verification without verifying returns to the change.
  document.addEventListener("close", function (event) {
    var verification = verificationDialog()
    if (!waiting || !verification || !verification.contains(event.target)) return
    var change = waiting
    waiting = null
    if (change.holder && window.tui && window.tui.dialog) window.tui.dialog.open(change.holder)
  }, true)

  // A person's row actions fetch their dialog with htmx. It arrives rendered
  // open, but templui's dialog script opens such dialogs only at page load.
  document.addEventListener("htmx:afterSwap", function (event) {
    var target = (event.detail && event.detail.target) || event.target
    if (!target || !target.querySelectorAll || !window.tui || !window.tui.dialog) return
    target.querySelectorAll("[data-tui-dialog]").forEach(function (root) {
      var content = root.querySelector("[data-tui-dialog-content]")
      if (content && content.getAttribute("data-tui-dialog-initial-open") === "true") window.tui.dialog.open(root)
    })
  })
})();

(function () {
  "use strict"

  function clearNavigationLoading() {
    var main = document.getElementById("main-content")
    if (main) main.removeAttribute("aria-busy")
    var indicator = document.querySelector("[data-admin-navigation-loading]")
    if (indicator) {
      indicator.classList.add("hidden")
      indicator.classList.remove("flex")
      indicator.setAttribute("aria-hidden", "true")
    }
    document.querySelectorAll("[data-admin-navigation-link][aria-busy='true']").forEach(function (link) {
      link.removeAttribute("aria-busy")
    })
  }

  document.addEventListener("click", function (event) {
    if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return
    var target = event.target
    var link = target && target.closest ? target.closest("[data-admin-navigation-link]") : null
    if (!link || (link.target && link.target !== "_self") || link.hasAttribute("download")) return

    var destination
    try {
      destination = new URL(link.href, window.location.href)
    } catch (_) {
      return
    }
    if (destination.origin !== window.location.origin) return
    var main = document.getElementById("main-content")
    var indicator = document.querySelector("[data-admin-navigation-loading]")
    if (!main || !indicator) return

    var label = indicator.querySelector("[data-admin-navigation-loading-label]")
    if (label) {
      var section = link.getAttribute("data-admin-navigation-label")
      label.textContent = section ? goferT("js.admin.loading_section", { section: section }) : goferT("js.admin.loading")
    }
    main.setAttribute("aria-busy", "true")
    link.setAttribute("aria-busy", "true")
    indicator.classList.remove("hidden")
    indicator.classList.add("flex")
    indicator.setAttribute("aria-hidden", "false")
  })

  window.addEventListener("pageshow", clearNavigationLoading)
})();

(function () {
  "use strict"

  function bindWebmailScope(root) {
    ;(root || document).querySelectorAll("[data-admin-webmail-scope-form]").forEach(function (form) {
      if (form.dataset.adminWebmailScopeBound === "true") return
      form.dataset.adminWebmailScopeBound = "true"
      var input = form.querySelector("[data-admin-webmail-scope]")
      if (!input) return
      input.addEventListener("change", function () {
        var destination = new URL(form.action, window.location.href)
        var userID = String(input.value || "").trim()
        if (userID) destination.searchParams.set("user_id", userID)
        else destination.searchParams.delete("user_id")
        window.location.assign(destination.toString())
      })
    })
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", function () { bindWebmailScope(document) })
  } else {
    bindWebmailScope(document)
  }
  document.addEventListener("htmx:afterSwap", function (event) { bindWebmailScope(event.target) })
})();

(function () {
  "use strict"

  // The Users page's selection: the people checked on the page or, once the
  // whole page is checked and the administrator asks for it, everyone the
  // list matches. The bar above the list says how many and offers the
  // changes; a change's dialog reads the selection from the list's inputs.
  function barOf(root) {
    return root ? root.querySelector("[data-admin-users-bulk]") : null
  }

  function show(element, shown) {
    if (element) element.hidden = !shown
  }

  function update(root) {
    var bar = barOf(root)
    if (!bar) return
    var boxes = Array.prototype.slice.call(root.querySelectorAll("[data-admin-users-select]"))
    var checked = boxes.filter(function (box) { return box.checked }).length
    var whole = boxes.length > 0 && checked === boxes.length
    var all = whole && bar.getAttribute("data-admin-users-bulk-all-selected") === "true"
    if (!all) bar.removeAttribute("data-admin-users-bulk-all-selected")
    bar.hidden = checked === 0
    var count = bar.querySelector("[data-admin-users-bulk-count]")
    if (count) count.textContent = goferTn("js.admin.users.selected", all ? parseInt(bar.getAttribute("data-matching"), 10) || checked : checked)
    show(bar.querySelector("[data-admin-users-bulk-select-matching]"), whole && !all)
    show(bar.querySelector("[data-admin-users-bulk-all-note]"), all)
    show(bar.querySelector("[data-admin-users-bulk-limit-note]"), whole)
    bar.querySelectorAll("[data-admin-users-bulk-all] input").forEach(function (input) { input.disabled = !all })
  }

  document.addEventListener("change", function (event) {
    var box = event.target
    if (!box || !box.matches || !box.matches("[data-admin-users-select], [data-admin-users-select-page]")) return
    var root = box.closest("[data-admin-users-results]")
    // templui's checkbox script checks the rest of the page on this same
    // event; count once it has.
    setTimeout(function () { update(root) }, 0)
  })

  document.addEventListener("click", function (event) {
    var target = event.target && event.target.closest ? event.target : null
    var root = target ? target.closest("[data-admin-users-results]") : null
    if (!root) return
    if (target.closest("[data-admin-users-bulk-select-matching]")) {
      barOf(root).setAttribute("data-admin-users-bulk-all-selected", "true")
      update(root)
    } else if (target.closest("[data-admin-users-bulk-clear]")) {
      root.querySelectorAll("[data-admin-users-select], [data-admin-users-select-page]").forEach(function (box) {
        box.checked = false
        box.indeterminate = false
      })
      barOf(root).removeAttribute("data-admin-users-bulk-all-selected")
      update(root)
    }
  })

  // What a change to many people did shows over the list it was made from.
  // The address goes back to that list, so reloading shows the list rather
  // than sending the change again.
  function showList() {
    var result = document.querySelector("[data-admin-users-bulk-result]")
    var list = result ? result.getAttribute("data-admin-users-list") : ""
    if (list && list.charAt(0) === "/" && window.location.pathname.indexOf("/admin/users/bulk/") === 0) {
      window.history.replaceState(null, "", list)
    }
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", showList)
  } else {
    showList()
  }
})();
