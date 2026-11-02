// Native notification icons need their own circular PNG; page CSS is not used.
(function (scope) {
  var cache = new Map()
  var fallback = "/assets/logo.png"

  async function render(url) {
    if (!url || url === fallback) return fallback
    if (cache.has(url)) return cache.get(url)
    var result = circularPNG(url).catch(function () {
      cache.delete(url)
      return fallback
    })
    if (cache.size >= 64) cache.delete(cache.keys().next().value)
    cache.set(url, result)
    return result
  }

  async function circularPNG(url) {
    var response = await fetch(url, { credentials: "same-origin", signal: AbortSignal.timeout(4000) })
    if (!response.ok) throw new Error("Avatar unavailable")
    var bitmap = await createImageBitmap(await response.blob())
    try {
      var canvas = new OffscreenCanvas(128, 128)
      var ctx = canvas.getContext("2d")
      var size = 112
      var left = 8
      var top = 8
      ctx.beginPath()
      ctx.arc(64, 64, size / 2, 0, Math.PI * 2)
      ctx.fillStyle = "white"
      ctx.shadowColor = "rgba(0,0,0,0.2)"
      ctx.shadowBlur = 5
      ctx.shadowOffsetY = 2
      ctx.fill()
      ctx.shadowColor = "transparent"
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
      ctx.beginPath()
      ctx.arc(64, 64, size / 2 - 0.5, 0, Math.PI * 2)
      ctx.strokeStyle = "rgba(0,0,0,0.15)"
      ctx.lineWidth = 1
      ctx.stroke()
      var blob = await canvas.convertToBlob({ type: "image/png" })
      var bytes = new Uint8Array(await blob.arrayBuffer())
      var binary = ""
      for (var i = 0; i < bytes.length; i += 8192) {
        binary += String.fromCharCode.apply(null, bytes.subarray(i, i + 8192))
      }
      return "data:image/png;base64," + btoa(binary)
    } finally {
      bitmap.close()
    }
  }

  scope.GoferNotificationAvatar = { render: render }
})(self)
