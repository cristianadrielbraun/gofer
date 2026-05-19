self.addEventListener("push", function (event) {
  var data = {}
  if (event.data) {
    try { data = event.data.json() || {} } catch (_) { data = { body: event.data.text() } }
  }

  var title = data.title || "Gofer"
  var options = {
    body: data.body || "New notification",
    icon: "/assets/logo.png",
    badge: "/assets/logo.png",
    tag: data.tag || "gofer-notification",
    data: {
      url: data.url || "/",
      folderID: data.folder_id || "",
    },
  }

  event.waitUntil(self.registration.showNotification(title, options))
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
