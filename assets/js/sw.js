importScripts("/assets/js/notification-avatar.js")

// This worker only handles notifications, so updates can activate while a
// Gofer tab remains open without changing its page or network requests.
self.addEventListener("install", function (event) {
  event.waitUntil(self.skipWaiting())
})

self.addEventListener("push", function (event) {
  var data = {}
  if (event.data) {
    try { data = event.data.json() || {} } catch (_) { data = { body: event.data.text() } }
  }

  var title = data.title || "Gofer"
  var icon = data.icon || data.avatar_url || "/assets/logo.png"
  var options = {
    body: typeof data.body === "string" ? data.body : "New notification",
    icon: icon,
    badge: "/assets/logo.png",
    tag: data.tag || "gofer-notification",
    data: {
      url: data.url || "/",
      folderID: data.folder_id || "",
    },
  }

  event.waitUntil(self.GoferNotificationAvatar.render(icon).then(function (circularIcon) {
    options.icon = circularIcon
    return self.registration.showNotification(title, options)
  }))
})

self.addEventListener("notificationclick", function (event) {
  event.notification.close()
  var targetURL = new URL((event.notification.data && event.notification.data.url) || "/", self.location.origin).href

  event.waitUntil(clients.matchAll({ type: "window", includeUncontrolled: true }).then(function (clientList) {
    for (var i = 0; i < clientList.length; i++) {
      var client = clientList[i]
      if (client.url && new URL(client.url).origin === self.location.origin) {
        client.navigate(targetURL)
        return client.focus()
      }
    }
    if (clients.openWindow) return clients.openWindow(targetURL)
  }))
})
