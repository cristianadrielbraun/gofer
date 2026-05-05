document.addEventListener("DOMContentLoaded", function () {
  setupTestButtonAnimation()
  setupSettingsHistory()
  setupModePickers()

  function setupTestButtonAnimation() {
    document.body.addEventListener("htmx:beforeRequest", function (e) {
      var btn = (e.detail && e.detail.elt) || e.target
      if (!btn || !btn.hasAttribute("data-test-btn")) return
      transitionTestButton(btn, "testing")
    })

    document.body.addEventListener("htmx:afterRequest", function (e) {
      var btn = (e.detail && e.detail.elt) || e.target
      if (!btn || !btn.hasAttribute("data-test-btn")) return

      var xhr = e.detail.xhr
      if (!xhr || xhr.status !== 200) {
        transitionTestButton(btn, "idle")
        return
      }

      try {
        var data = JSON.parse(xhr.responseText)
        var allSuccess = data.results.every(function (r) { return r.success })
        if (allSuccess) {
          transitionTestButton(btn, "success")
          setTimeout(function () { transitionTestButton(btn, "idle") }, 3000)
        } else {
          transitionTestButton(btn, "error", data.results)
        }
      } catch (err) {
        transitionTestButton(btn, "idle")
      }
    })
  }
})

function transitionTestButton(btn, state, results) {
  var content = btn.querySelector("[data-test-btn-content]")
  if (!content) return

  var currentWidth = btn.offsetWidth

  content.style.transition = "opacity 0.15s ease"
  content.style.opacity = "0"

  setTimeout(function () {
    btn.style.width = currentWidth + "px"
    btn.style.whiteSpace = "nowrap"

    btn.className = testBtnClasses(state)
    if (state === "error") btn.style.position = "relative"
    else btn.style.position = ""

    content.innerHTML = testBtnContent(state, btn.dataset.accountId, results)

    var targetWidth = btn.scrollWidth
    btn.style.width = currentWidth + "px"
    void btn.offsetWidth
    btn.style.transition = "width 0.3s ease"
    btn.style.width = targetWidth + "px"

    requestAnimationFrame(function () {
      requestAnimationFrame(function () {
        content.style.transition = "opacity 0.2s ease"
        content.style.opacity = "1"
      })
    })

    setTimeout(function () {
      btn.style.width = ""
      btn.style.whiteSpace = ""
      btn.style.transition = ""
      content.style.transition = ""
      content.style.opacity = ""
    }, 350)
  }, 160)
}

function testBtnClasses(state) {
  var base = "inline-flex items-center gap-1.5 text-xs transition-all duration-300 px-2.5 py-1.5 rounded-md border"
  switch (state) {
    case "idle":
      return base + " text-muted-foreground hover:text-foreground border-border hover:bg-accent cursor-pointer"
    case "testing":
      return base + " text-muted-foreground border-border cursor-wait"
    case "success":
      return base + " text-emerald-600 border-emerald-500/20 bg-emerald-500/10"
    case "error":
      return base + " text-destructive border-destructive/20 bg-destructive/10 hover:bg-destructive/15 cursor-pointer"
  }
  return base + " text-muted-foreground border-border"
}

function svg(cls, paths) {
  return '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="' + cls + '">' + paths + "</svg>"
}

var ICON_WIFI = '<path d="M12 20h.01"/><path d="M2 8.82a15 15 0 0 1 20 0"/><path d="M5 12.859a10 10 0 0 1 14 0"/><path d="M8.5 16.429a5 5 0 0 1 7 0"/>'
var ICON_CHECK = '<circle cx="12" cy="12" r="10"/><path d="m9 12 2 2 4-4"/>'
var ICON_X = '<circle cx="12" cy="12" r="10"/><path d="m15 9-6 6"/><path d="m9 9 6 6"/>'
var ICON_INFO = '<circle cx="12" cy="12" r="10"/><path d="M12 16v-4"/><path d="M12 8h.01"/>'

function testBtnContent(state, accountId, results) {
  switch (state) {
    case "idle":
      return svg("size-3.5", ICON_WIFI) + "Test"
    case "testing":
      return '<div class="size-3.5 shrink-0 border-2 border-muted-foreground/30 border-t-muted-foreground rounded-full animate-spin"></div>Testing...'
    case "success":
      return svg("size-3.5", ICON_CHECK) + "Connected"
    case "error":
      var html = svg("size-3.5", ICON_X) + "Failed"
      html += '<span class="inline-flex items-center justify-center size-4 rounded hover:bg-destructive/20 transition-colors ml-0.5 -mr-1" onclick="event.stopPropagation();document.getElementById(\'test-err-' + accountId + '\').classList.toggle(\'hidden\')">'
      html += svg("size-3", ICON_INFO)
      html += "</span>"
      html += '<div id="test-err-' + accountId + '" class="hidden absolute top-full right-0 mt-1 p-3 rounded-lg bg-popover border shadow-lg text-xs max-w-xs z-50 text-popover-foreground">'
      html += '<div class="space-y-2">'
      if (results) {
        for (var i = 0; i < results.length; i++) {
          var r = results[i]
          html += "<div>"
          html += '<p class="font-semibold text-muted-foreground uppercase tracking-wider">' + escapeHtml(r.service) + "</p>"
          if (r.success) {
            html += '<p class="text-emerald-600 mt-0.5">' + escapeHtml(r.message) + "</p>"
          } else {
            html += '<p class="text-destructive mt-0.5">' + escapeHtml(r.error) + "</p>"
            html += '<p class="text-muted-foreground mt-0.5">' + escapeHtml(r.message) + "</p>"
          }
          html += "</div>"
        }
      }
      html += "</div></div>"
      return html
  }
  return "Test"
}

function escapeHtml(text) {
  if (!text) return ""
  var el = document.createElement("span")
  el.textContent = text
  return el.innerHTML
}

function getActiveThemeMode() {
  if (typeof GoferSettings !== "undefined" && GoferSettings.get) {
    return GoferSettings.get("theme") || "dark"
  }
  return document.documentElement.classList.contains("dark") ? "dark" : "light"
}

function modeButtonClasses(isActive) {
  var base = "inline-flex items-center gap-1.5 px-3 py-1.5 rounded-md text-xs font-medium transition-all duration-200"
  return isActive ? base + " text-foreground" : base + " text-muted-foreground hover:text-foreground"
}

function setupModePickers() {
  document.querySelectorAll("[data-mode-picker]").forEach(function (picker) {
    if (picker.dataset.modePickerReady === "1") return
    picker.dataset.modePickerReady = "1"

    if (getComputedStyle(picker).position === "static") {
      picker.style.position = "relative"
    }

    var indicator = document.createElement("div")
    indicator.setAttribute("data-mode-picker-indicator", "true")
    indicator.style.position = "absolute"
    indicator.style.left = "0"
    indicator.style.top = "0"
    indicator.style.borderRadius = "0.5rem"
    indicator.style.background = "var(--background)"
    indicator.style.border = "1px solid var(--border)"
    indicator.style.boxShadow = "0 1px 3px 0 rgb(0 0 0 / 0.1), 0 1px 2px -1px rgb(0 0 0 / 0.1)"
    indicator.style.transition = "transform 220ms ease, width 220ms ease, height 220ms ease"
    indicator.style.willChange = "transform, width, height"
    indicator.style.pointerEvents = "none"
    indicator.style.zIndex = "0"
    picker.prepend(indicator)

    var buttons = Array.prototype.slice.call(picker.querySelectorAll("[data-mode]"))
    buttons.forEach(function (btn) {
      btn.style.position = "relative"
      btn.style.zIndex = "1"
      btn.style.background = "transparent"
      btn.style.boxShadow = "none"
      btn.style.borderColor = "transparent"
    })

    function sync(animate) {
      var activeMode = getActiveThemeMode()
      var activeButton = picker.querySelector('[data-mode="' + activeMode + '"]') || buttons[0]
      if (!activeButton) return

      buttons.forEach(function (btn) {
        var isActive = btn === activeButton
        btn.className = modeButtonClasses(isActive)
      })

      var pickerRect = picker.getBoundingClientRect()
      var buttonRect = activeButton.getBoundingClientRect()
      var left = buttonRect.left - pickerRect.left
      var top = buttonRect.top - pickerRect.top

      if (!animate) {
        var previousTransition = indicator.style.transition
        indicator.style.transition = "none"
        indicator.style.width = buttonRect.width + "px"
        indicator.style.height = buttonRect.height + "px"
        indicator.style.transform = "translate(" + left + "px, " + top + "px)"
        void indicator.offsetHeight
        indicator.style.transition = previousTransition
        return
      }

      indicator.style.width = buttonRect.width + "px"
      indicator.style.height = buttonRect.height + "px"
      indicator.style.transform = "translate(" + left + "px, " + top + "px)"
    }

    picker.__syncModePicker = sync
    requestAnimationFrame(function () { sync(false) })
  })
}

function refreshModePickers(animate) {
  document.querySelectorAll("[data-mode-picker]").forEach(function (picker) {
    if (typeof picker.__syncModePicker === "function") {
      picker.__syncModePicker(animate !== false)
    }
  })
}

if (typeof MutationObserver !== "undefined" && document.documentElement) {
  var themeObserver = new MutationObserver(function (mutations) {
    for (var i = 0; i < mutations.length; i++) {
      if (mutations[i].type === "attributes") {
        refreshModePickers(true)
        break
      }
    }
  })

  themeObserver.observe(document.documentElement, {
    attributes: true,
    attributeFilter: ["class", "data-theme"],
  })
}

function setupSettingsHistory() {
  if (!window.location.pathname.startsWith("/settings")) return
  var parts = window.location.pathname.replace(/\/+$/, "").split("/")
  var tab = parts[2] || "accounts"
  if (tab !== "accounts" && tab !== "sync" && tab !== "appearance") tab = "accounts"
  history.replaceState({ settingsTab: tab }, "", window.location.pathname)
}

document.body.addEventListener("htmx:afterSettle", function (e) {
  if (!e.target || !e.target.querySelector) return
  if (!e.target.querySelector("[data-tui-tabs]") && !e.target.querySelector("[data-mode-picker]")) return
  setupSettingsHistory()
  setupModePickers()
  requestAnimationFrame(function () {
    refreshModePickers(false)
  })
})

document.addEventListener("click", function (e) {
  var trigger = e.target.closest('[data-tui-tabs-trigger]')
  if (!trigger) return
  var value = trigger.getAttribute('data-tui-tabs-value')
  if (value === 'appearance') {
    requestAnimationFrame(function () {
      if (typeof refreshModePickers === 'function') refreshModePickers(false)
    })
  }
})
