;(function () {
  "use strict"

  var count = 0
  var favicon = null
  var originalHref = ""
  var originalType = ""
  var iconImage = null
  var iconLoaded = false

  function findFavicon() {
    if (favicon && favicon.isConnected) return favicon
    favicon = document.querySelector("link[data-brand-favicon]") || document.querySelector('link[rel~="icon"]')
    if (favicon && !originalHref) {
      originalHref = favicon.getAttribute("href") || ""
      originalType = favicon.getAttribute("type") || ""
    }
    return favicon
  }

  function drawFavicon() {
    var link = findFavicon()
    if (!link) return
    if (count <= 0) {
      link.setAttribute("href", originalHref)
      if (originalType) link.setAttribute("type", originalType)
      return
    }
    if (!iconImage) {
      iconImage = new Image()
      iconImage.onload = function () { iconLoaded = true; drawFavicon() }
      iconImage.onerror = function () { iconLoaded = false; drawFavicon() }
      iconImage.src = originalHref
      return
    }
    var size = 64
    var canvas = document.createElement("canvas")
    canvas.width = size
    canvas.height = size
    var context = canvas.getContext && canvas.getContext("2d")
    if (!context) return
    if (iconLoaded) {
      try { context.drawImage(iconImage, 0, 0, size, size) } catch (_) {}
    }
    var label = count > 99 ? "99+" : String(count)
    var radius = label.length > 2 ? 22 : 19
    var x = size - radius
    var y = radius
    context.beginPath()
    context.arc(x, y, radius, 0, 2 * Math.PI)
    context.fillStyle = "#d93025"
    context.fill()
    context.lineWidth = 3
    context.strokeStyle = "#ffffff"
    context.stroke()
    context.fillStyle = "#ffffff"
    context.font = "bold " + (label.length > 2 ? 18 : label.length > 1 ? 24 : 28) + "px sans-serif"
    context.textAlign = "center"
    context.textBaseline = "middle"
    context.fillText(label, x, y + 1)
    try {
      link.setAttribute("type", "image/png")
      link.setAttribute("href", canvas.toDataURL("image/png"))
    } catch (_) {
      // A cross-origin icon taints the canvas; keep the plain favicon.
    }
  }

  function updateAppBadge() {
    var nav = window.navigator
    if (!nav) return
    try {
      if (count > 0 && typeof nav.setAppBadge === "function") nav.setAppBadge(count).catch(function () {})
      else if (count <= 0 && typeof nav.clearAppBadge === "function") nav.clearAppBadge().catch(function () {})
    } catch (_) {}
  }

  window.goferUpdateUnreadBadge = function (n) {
    n = Math.max(0, parseInt(n, 10) || 0)
    if (n === count) return
    count = n
    drawFavicon()
    updateAppBadge()
  }
})()
