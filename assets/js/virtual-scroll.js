class VirtualMailList {
  constructor(container, options) {
    this.container = container
    this.folderID = options.folderID || "inbox"
    this.itemHeight = 88
    this.overscan = 10

    this.cache = new Map()
    this.indexById = new Map()
    this.loadedRanges = []
    this.totalCount = 0
    this.selectedEmailId = null
    this.nextCursor = null
    this.hasMore = true
    this.isLoading = false
    this.activeFetches = new Set()
    this.newEmailCount = 0

    this.prevFirst = null
    this.prevLast = null

    this.spacerTop = null
    this.spacerBottom = null
    this.itemsContainer = null
    this.bannerEl = null

    this.bindEvents()
  }

  setupDOM() {
    this.spacerTop = document.createElement("div")
    this.spacerBottom = document.createElement("div")
    this.itemsContainer = document.createElement("div")

    this.container.innerHTML = ""
    this.container.appendChild(this.spacerTop)
    this.container.appendChild(this.itemsContainer)
    this.container.appendChild(this.spacerBottom)
  }

  bindEvents() {
    var self = this
    var rafId = null
    this.container.addEventListener("scroll", function () {
      if (rafId) return
      rafId = requestAnimationFrame(function () {
        self.render()
        rafId = null
      })
    })
  }

  render() {
    var scrollTop = this.container.scrollTop
    var clientHeight = this.container.clientHeight
    if (this.totalCount === 0) {
      this.spacerTop.style.height = "0px"
      this.spacerBottom.style.height = "0px"
      this.itemsContainer.innerHTML =
        '<div class="flex flex-col items-center justify-center py-20 px-4 text-center">' +
          '<div class="size-16 rounded-2xl bg-muted/50 flex items-center justify-center mb-4 raised">' +
            '<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="size-7 text-muted-foreground/40" data-lucide="icon">' +
              '<polyline points="22 12 16 12 14 15 10 15 8 12 2 12"/>' +
              '<path d="M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"/>' +
            '</svg>' +
          '</div>' +
          '<h3 class="font-semibold text-sm mb-1">No emails</h3>' +
          '<p class="text-xs text-muted-foreground">This folder is empty</p>' +
        '</div>'
      return
    }

    var first = Math.max(
      0,
      Math.floor(scrollTop / this.itemHeight) - this.overscan
    )
    var last = Math.min(
      this.totalCount - 1,
      Math.ceil((scrollTop + clientHeight) / this.itemHeight) + this.overscan
    )

    if (first === this.prevFirst && last === this.prevLast) return
    this.prevFirst = first
    this.prevLast = last

    this.ensureRangeLoaded(first, last)
    this.prefetchSequential(last)

    this.spacerTop.style.height = first * this.itemHeight + "px"
    this.spacerBottom.style.height =
      Math.max(0, this.totalCount - last - 1) * this.itemHeight + "px"

    var fragment = document.createDocumentFragment()
    for (var i = first; i <= last; i++) {
      var item = this.cache.get(i)
      if (item) {
        var row = document.createElement("div")
        row.innerHTML = item.html
        var el = row.firstChild
        var anchor = el.querySelector("a")
        if (anchor) {
          if (item.id === this.selectedEmailId) {
            anchor.classList.remove("envelope")
            anchor.classList.add("envelope-active")
          } else {
            anchor.classList.remove("envelope-active")
            anchor.classList.add("envelope")
          }
        }
        fragment.appendChild(el)
      } else {
        fragment.appendChild(this.createSkeleton())
      }
    }

    this.itemsContainer.innerHTML = ""
    this.itemsContainer.appendChild(fragment)

    if (typeof htmx !== "undefined") {
      htmx.process(this.itemsContainer)
    }

    if (this.container.scrollTop !== scrollTop) {
      this.container.scrollTop = scrollTop
    }
  }

  createSkeleton() {
    var row = document.createElement("div")
    row.className = "mail-list-skeleton"
    row.innerHTML =
      '<div class="flex items-center gap-1.5 pt-0.5 shrink-0">' +
      '<div class="h-3.5 w-3.5 rounded bg-muted animate-pulse"></div>' +
      '<div class="h-3.5 w-3.5 rounded bg-muted animate-pulse"></div>' +
      "</div>" +
      '<div class="flex-1 min-w-0 space-y-2">' +
      '<div class="flex items-center justify-between">' +
      '<div class="flex items-center gap-2">' +
      '<div class="size-6 rounded-full bg-muted animate-pulse"></div>' +
      '<div class="h-3 w-24 rounded bg-muted animate-pulse"></div>' +
      "</div>" +
      '<div class="h-3 w-12 rounded bg-muted animate-pulse"></div>' +
      "</div>" +
      '<div class="h-3 w-3/4 rounded bg-muted animate-pulse"></div>' +
      '<div class="h-2.5 w-1/2 rounded bg-muted animate-pulse"></div>' +
      "</div>"
    return row
  }

  async ensureRangeLoaded(first, last) {
    var gaps = this.findGaps(first, last)
    for (var i = 0; i < gaps.length; i++) {
      var gap = gaps[i]
      var key = "range-" + gap.start + "-" + gap.end
      if (this.activeFetches.has(key)) continue
      this.activeFetches.add(key)
      try {
        var url =
          "/mail/folder/" +
          this.folderID +
          "/items?start=" +
          gap.start +
          "&limit=" +
          (gap.end - gap.start + 1)
        if (this.selectedEmailId) {
          url += "&selected=" + encodeURIComponent(this.selectedEmailId)
        }
        var html = await this.fetchHTML(url)
        this.ingestHTML(html)
        this.prevFirst = null
        this.prevLast = null
        this.render()
      } finally {
        this.activeFetches.delete(key)
      }
    }
  }

  async prefetchSequential(last) {
    if (this.cache.size === 0) return
    if (last < this.cache.size - 30) return
    if (!this.hasMore || this.isLoading) return
    this.isLoading = true
    try {
      var params = "limit=50"
      if (this.nextCursor) {
        params += "&after=" + encodeURIComponent(this.nextCursor)
      }
      if (this.selectedEmailId) {
        params += "&selected=" + encodeURIComponent(this.selectedEmailId)
      }
      var url = "/mail/folder/" + this.folderID + "/items?" + params
      var html = await this.fetchHTML(url)
      this.ingestHTML(html)
      this.prevFirst = null
      this.prevLast = null
      this.render()
    } finally {
      this.isLoading = false
    }
  }

  async fetchHTML(url) {
    var res = await fetch(url, {
      headers: { Accept: "text/html" },
    })
    if (!res.ok) throw new Error("Fetch failed: " + res.status)
    return res.text()
  }

  ingestHTML(html) {
    var template = document.createElement("template")
    template.innerHTML = html
    var wrapper = template.content.firstElementChild
    if (!wrapper) return

    var tc = parseInt(wrapper.dataset.totalCount)
    if (!isNaN(tc)) this.totalCount = tc

    if (wrapper.dataset.nextCursor) {
      this.nextCursor = wrapper.dataset.nextCursor
    }
    if (wrapper.dataset.hasMore !== undefined) {
      this.hasMore = wrapper.dataset.hasMore === "true"
    }

    var items = wrapper.querySelectorAll("[data-email-id]")
    for (var i = 0; i < items.length; i++) {
      var el = items[i]
      var pos = parseInt(el.dataset.position)
      var id = el.dataset.emailId
      this.cache.set(pos, { id: id, html: el.outerHTML })
      this.indexById.set(id, pos)
    }

    var start = parseInt(wrapper.dataset.windowStart)
    var end = parseInt(wrapper.dataset.windowEnd)
    if (!isNaN(start) && !isNaN(end)) {
      this.addLoadedRange(start, end)
    }
  }

  findGaps(first, last) {
    var gaps = []
    var pos = first
    var sorted = this.loadedRanges.slice().sort(function (a, b) {
      return a.start - b.start
    })

    for (var i = 0; i < sorted.length; i++) {
      var range = sorted[i]
      if (range.end < pos) continue
      if (range.start > last) break
      if (range.start > pos) {
        gaps.push({ start: pos, end: Math.min(range.start - 1, last) })
      }
      pos = Math.max(pos, range.end + 1)
    }

    if (pos <= last) {
      gaps.push({ start: pos, end: last })
    }

    return gaps
  }

  addLoadedRange(start, end) {
    this.loadedRanges.push({ start: start, end: end })
    this.mergeRanges()
  }

  mergeRanges() {
    this.loadedRanges.sort(function (a, b) {
      return a.start - b.start
    })
    var merged = [this.loadedRanges[0]]
    for (var i = 1; i < this.loadedRanges.length; i++) {
      var last = merged[merged.length - 1]
      var current = this.loadedRanges[i]
      if (current.start <= last.end + 1) {
        last.end = Math.max(last.end, current.end)
      } else {
        merged.push(current)
      }
    }
    this.loadedRanges = merged
  }

  invalidateLoadedRanges() {
    this.loadedRanges = []
    var entries = Array.from(this.cache.entries())
    for (var i = 0; i < entries.length; i++) {
      this.loadedRanges.push({ start: entries[i][0], end: entries[i][0] })
    }
    this.mergeRanges()
  }

  hydrateFromDOM() {
    var scrollEl =
      document.getElementById("mail-list-scroll") || this.container
    var totalCount = parseInt(scrollEl.dataset.totalCount)
    if (!isNaN(totalCount)) this.totalCount = totalCount
    if (scrollEl.dataset.folderId) {
      this.folderID = scrollEl.dataset.folderId
    }

    var items = scrollEl.querySelectorAll("[data-email-id]")
    for (var i = 0; i < items.length; i++) {
      var el = items[i]
      var pos = parseInt(el.dataset.position)
      var id = el.dataset.emailId
      this.cache.set(pos, { id: id, html: el.outerHTML })
      this.indexById.set(id, pos)
    }

    if (this.cache.size > 0) {
      var positions = Array.from(this.cache.keys())
      this.addLoadedRange(
        Math.min.apply(null, positions),
        Math.max.apply(null, positions)
      )
    }

    this.hasMore = this.totalCount > this.cache.size
    if (this.hasMore && this.cache.size > 0) {
      var maxPos = Math.max.apply(null, Array.from(this.cache.keys()))
      var lastItem = this.cache.get(maxPos)
      if (lastItem) this.nextCursor = lastItem.id
    }

    var selectedEl = scrollEl.querySelector(".envelope-active")
    if (selectedEl) {
      var parent = selectedEl.closest("[data-email-id]")
      if (parent) this.selectedEmailId = parent.dataset.emailId
    }

    this.container.innerHTML = ""
    this.spacerTop = document.createElement("div")
    this.spacerBottom = document.createElement("div")
    this.itemsContainer = document.createElement("div")
    this.container.appendChild(this.spacerTop)
    this.container.appendChild(this.itemsContainer)
    this.container.appendChild(this.spacerBottom)

    this.render()
  }

  async switchFolder(folderID, pushState) {
    if (pushState === undefined) pushState = true
    this.reset()
    this.folderID = folderID

    var params = "limit=50"
    if (this.selectedEmailId) {
      params += "&selected=" + encodeURIComponent(this.selectedEmailId)
    }
    var url = "/mail/folder/" + folderID + "/items?" + params
    var html = await this.fetchHTML(url)
    this.ingestHTML(html)

    if (this.cache.size > 0) {
      var firstItem = this.cache.get(0)
      if (firstItem) {
        this.selectedEmailId = firstItem.id
        if (typeof htmx !== "undefined") {
          htmx.ajax("GET", "/email/" + firstItem.id, "#mail-view")
        }
      }
    } else {
      var mailView = document.getElementById("mail-view")
      if (mailView) {
        mailView.innerHTML = ""
      }
    }

    this.render()
    this.updateHeader()
    if (pushState) this.pushUrl()
  }

  reset() {
    this.cache.clear()
    this.indexById.clear()
    this.loadedRanges = []
    this.totalCount = 0
    this.selectedEmailId = null
    this.nextCursor = null
    this.hasMore = true
    this.isLoading = false
    this.newEmailCount = 0
    this.activeFetches.clear()
    this.prevFirst = null
    this.prevLast = null
    this.container.scrollTop = 0
    this.removeBanner()
  }

  onEmailSelected(emailId) {
    this.selectedEmailId = emailId
    this.prevFirst = null
    this.prevLast = null
    this.render()
    this.pushUrl()
  }

  pushUrl() {
    var path = "/folder/" + this.folderID
    if (this.selectedEmailId) {
      path += "/" + this.selectedEmailId
    }
    if (window.location.pathname !== path) {
      history.pushState({ folder: this.folderID, email: this.selectedEmailId }, "", path)
    }
  }

  showNewEmailBanner() {
    var self = this
    if (this.bannerEl) return
    this.bannerEl = document.createElement("div")
    this.bannerEl.className = "new-email-banner"
    this.bannerEl.textContent = this.newEmailCount + " new email" + (this.newEmailCount !== 1 ? "s" : "")
    this.bannerEl.addEventListener("click", function () {
      self.container.scrollTop = 0
      self.switchFolder(self.folderID)
    })
    this.container.insertBefore(this.bannerEl, this.itemsContainer)
  }

  removeBanner() {
    if (this.bannerEl) {
      this.bannerEl.remove()
      this.bannerEl = null
    }
  }

  updateHeader() {
    var nameEl = document.getElementById("mail-folder-name")
    if (nameEl) {
      var link = document.querySelector(
        'aside a[hx-get="/folder/' + this.folderID + '"]'
      )
      if (link) {
        var span = link.querySelector("span.truncate")
        if (span) nameEl.textContent = span.textContent.trim()
      }
    }
    var countEl = document.getElementById("mail-folder-count")
    if (countEl) {
      countEl.textContent = String(this.totalCount)
    }
  }

  onNewEmail() {
    if (this.container.scrollTop < this.itemHeight * 2) {
      this.removeBanner()
      this.switchFolder(this.folderID)
    } else {
      this.newEmailCount++
      this.showNewEmailBanner()
    }
  }

  invalidateItem(emailId) {
    var pos = this.indexById.get(emailId)
    if (pos === undefined) return

    var url = "/mail/folder/" + this.folderID + "/items?start=" + pos + "&limit=1"
    if (this.selectedEmailId) {
      url += "&selected=" + encodeURIComponent(this.selectedEmailId)
    }
    var self = this
    fetch(url, { headers: { Accept: "text/html" } })
      .then(function (r) { return r.text() })
      .then(function (html) {
        self.ingestHTML(html)
        self.prevFirst = null
        self.prevLast = null
        self.render()
      })
      .catch(function () {})
  }

  async restoreFromUrl() {
    var params = new URLSearchParams(window.location.search)
    var selectedId = params.get("selected")

    if (!selectedId) {
      await this.prefetchSequential(0)
      this.render()
      return
    }

    this.selectedEmailId = selectedId
    var url =
      "/mail/folder/" +
      this.folderID +
      "/items?around=" +
      encodeURIComponent(selectedId) +
      "&limit=80&selected=" +
      encodeURIComponent(selectedId)
    var html = await this.fetchHTML(url)
    this.ingestHTML(html)

    var anchorPos = this.indexById.get(selectedId)
    if (anchorPos !== undefined) {
      this.container.scrollTop = anchorPos * this.itemHeight
    }

    this.render()
  }
}

window.VirtualMailList = VirtualMailList

window.addEventListener("popstate", function (e) {
  if (!e.state) return

  if (e.state.settingsTab) {
    if (typeof htmx !== "undefined") {
      htmx.ajax("GET", "/settings/" + e.state.settingsTab, {target: "#main-content", swap: "outerHTML"})
    }
    return
  }

  if (!e.state.folder) return

  var container = document.getElementById("mail-list-scroll")
  if (!container || !container._virtualMailList) {
    if (typeof htmx !== "undefined") {
      htmx.ajax("GET", "/folder/" + e.state.folder + "/full", {target: "#main-content", swap: "outerHTML"})
    }
    return
  }

  var vml = container._virtualMailList
  var folderID = e.state.folder
  if (folderID && folderID !== vml.folderID) {
    vml.switchFolder(folderID, false)
    var sidebar = document.querySelector("aside")
    if (sidebar) {
      var sidebarLinks = sidebar.querySelectorAll("a[hx-get^='/folder/']")
      for (var i = 0; i < sidebarLinks.length; i++) {
        sidebarLinks[i].classList.remove(
          "bg-sidebar-accent",
          "text-sidebar-primary",
          "font-medium"
        )
        sidebarLinks[i].classList.add("text-sidebar-foreground")
      }
      var activeLink = sidebar.querySelector('a[hx-get="/folder/' + folderID + '"]')
      if (activeLink) {
        activeLink.classList.add("bg-sidebar-accent", "text-sidebar-primary", "font-medium")
        activeLink.classList.remove("text-sidebar-foreground")
      }
    }
  } else if (e.state.email && e.state.email !== vml.selectedEmailId) {
    vml.selectedEmailId = e.state.email
    vml.prevFirst = null
    vml.prevLast = null
    vml.render()
    if (typeof htmx !== "undefined") {
      htmx.ajax("GET", "/email/" + e.state.email, "#mail-view")
    }
  }
})
