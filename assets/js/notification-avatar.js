// Native notification icons need their own circular PNG; page CSS is not used.
(function (scope) {
  var cache = new Map()
  var fallback = "/assets/logo.png"
  var size = 112
  var left = 8
  var top = 8

  // render returns the sender's picture in a circle, or their initials when
  // there is no picture, like the avatars in the mail list. Without either it
  // is the Gofer logo.
  async function render(url, initials) {
    initials = String(initials || "").trim()
    var picture = url && url !== fallback ? url : ""
    if (!picture && !initials) return fallback
    var key = picture + "\n" + initials
    if (cache.has(key)) return cache.get(key)
    var result = (picture ? circularPNG(picture) : Promise.reject(new Error("No picture"))).catch(function () {
      return initials ? initialsPNG(initials) : fallback
    }).catch(function () {
      cache.delete(key)
      return fallback
    })
    if (cache.size >= 64) cache.delete(cache.keys().next().value)
    cache.set(key, result)
    return result
  }

  function circle(ctx, fill) {
    ctx.beginPath()
    ctx.arc(64, 64, size / 2, 0, Math.PI * 2)
    ctx.fillStyle = fill
    ctx.shadowColor = "rgba(0,0,0,0.2)"
    ctx.shadowBlur = 5
    ctx.shadowOffsetY = 2
    ctx.fill()
    ctx.shadowColor = "transparent"
  }

  function outline(ctx) {
    ctx.beginPath()
    ctx.arc(64, 64, size / 2 - 0.5, 0, Math.PI * 2)
    ctx.strokeStyle = "rgba(0,0,0,0.15)"
    ctx.lineWidth = 1
    ctx.stroke()
  }

  async function circularPNG(url) {
    var response = await fetch(url, { credentials: "same-origin", signal: AbortSignal.timeout(4000) })
    if (!response.ok) throw new Error("Avatar unavailable")
    var bitmap = await createImageBitmap(await response.blob())
    try {
      var canvas = new OffscreenCanvas(128, 128)
      var ctx = canvas.getContext("2d")
      circle(ctx, "white")
      ctx.save()
      ctx.clip()
      var sample = new URL(url, scope.location.href).pathname === "/assets/notification-avatar.png"
      var zoom = sample ? 0.85 : 1
      var scale = Math.max(size / bitmap.width, size / bitmap.height) * zoom
      var width = bitmap.width * scale
      var height = bitmap.height * scale
      // Match the sample's zoom and lowered position in the settings circle.
      var y = sample ? top + size * (1 - zoom) / 2 + size * 0.1 : top + (size - height) / 2
      ctx.drawImage(bitmap, left + (size - width) / 2, y, width, height)
      ctx.restore()
      outline(ctx)
      return dataURL(canvas)
    } finally {
      bitmap.close()
    }
  }

  // The mail list's fallback avatar: amber gradient with light initials.
  async function initialsPNG(initials) {
    var canvas = new OffscreenCanvas(128, 128)
    var ctx = canvas.getContext("2d")
    var gradient = ctx.createLinearGradient(0, top, 0, top + size)
    gradient.addColorStop(0, "#b45309")
    gradient.addColorStop(1, "#78350f")
    circle(ctx, gradient)
    ctx.fillStyle = "#fef3c7"
    ctx.font = "700 44px system-ui, sans-serif"
    ctx.textAlign = "center"
    ctx.textBaseline = "middle"
    ctx.fillText(initials, 64, 66, size - 24)
    outline(ctx)
    return dataURL(canvas)
  }

  async function dataURL(canvas) {
    var blob = await canvas.convertToBlob({ type: "image/png" })
    var bytes = new Uint8Array(await blob.arrayBuffer())
    var binary = ""
    for (var i = 0; i < bytes.length; i += 8192) {
      binary += String.fromCharCode.apply(null, bytes.subarray(i, i + 8192))
    }
    return "data:image/png;base64," + btoa(binary)
  }

  scope.GoferNotificationAvatar = { render: render }
})(self)
