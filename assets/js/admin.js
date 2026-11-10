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
