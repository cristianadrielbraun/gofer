// List rows are sized in rem so they grow with the root font size on small screens; this
// converts those sizes to the pixels the virtual lists position rows with.
function listRemPx(rem) {
  var root = parseFloat(window.getComputedStyle(document.documentElement).fontSize) || 16
  return Math.round(rem * root)
}

// Search results show where they are in a band (data-search-group): along
// the top of every card, 1.5rem, and above a table row's columns, 1.25rem,
// once per run of rows from one place (input.css). getHeight adds it to
// those rows.
function mailSearchBandRem(viewMode) {
  return viewMode === "table" ? 1.25 : 1.5
}

// Mail card rows are shorter on small screens, where they use a fixed phone layout
// (see the small-screen mail list styles).
function mailCardRowRem() {
  return window.matchMedia("(max-width: 1023.98px)").matches ? 5.25 : 6.5
}

// Contact rows use one 4rem phone layout on small screens, in either view.
function contactRowRem(viewMode) {
  if (window.matchMedia("(max-width: 1023.98px)").matches) return 4
  // Cards are two lines beside the avatar (.contact-card in input.css).
  return viewMode === "table" ? 2.75 : 3.875
}

// Slides entering rows into place from JavaScript, one whole pixel at a time.
// A CSS transition, a fade or a scale leaves the row on its own layer at
// fractional positions, where Chrome draws its text blurry until it settles.
// Whole-pixel offsets set frame by frame keep the text sharp all the way.
//
// MAX_ROW_SLIDE caps how far a row slides; at 0 rows appear in place, as any
// slide's last whole-pixel step read as a small jump once it landed.
var MAX_ROW_SLIDE = 0

function slideRowsInWholePixels(nodes, duration, offsetY, delay, stagger) {
  offsetY = Math.max(-MAX_ROW_SLIDE, Math.min(MAX_ROW_SLIDE, offsetY))
  if (!nodes.length || Math.round(offsetY) === 0) return
  var rows = []
  for (var i = 0; i < nodes.length; i++) {
    var node = nodes[i]
    var base = node.style.transform || ""
    node.style.transition = "none"
    node.style.transform = base + " translateY(" + Math.round(offsetY) + "px)"
    // Reads back the browser's spelling, to notice when the list moves the row.
    rows.push({ node: node, base: base, last: node.style.transform, delay: delay + Math.min(120, i * stagger), done: false })
  }
  var start = null
  function frame(now) {
    if (start === null) start = now
    var pending = false
    for (var j = 0; j < rows.length; j++) {
      var row = rows[j]
      if (row.done) continue
      // The list positioned the row again (a scroll or re-render); leave it there.
      if (row.node.style.transform !== row.last) {
        row.done = true
        row.node.style.transition = ""
        continue
      }
      var t = Math.min(1, Math.max(0, (now - start - row.delay) / duration))
      // Eases out but still moves at the end, about 0.4 of its starting
      // speed. An ease that stops gently held the row a pixel short for a few
      // frames, and its last whole-pixel step then read as a small jump.
      var p = (2 * t - 0.6 * t * t) / 1.4
      var y = Math.round(offsetY * (1 - p))
      row.node.style.transform = y === 0 ? row.base : row.base + " translateY(" + y + "px)"
      row.last = row.node.style.transform
      if (t >= 1) {
        row.done = true
        row.node.style.transition = ""
      } else {
        pending = true
      }
    }
    if (pending) requestAnimationFrame(frame)
  }
  requestAnimationFrame(frame)
}

class VirtualMailList {
  constructor(container, options) {
    this.container = container
    this.root = container.closest("#mail-list") || container.parentElement || container
    this.folderID = options.folderID || "inbox"
    try { this.searchScope = JSON.parse(this.root.dataset.mailSearchScope || "null") } catch (_) { this.searchScope = null }
    this.viewMode = options.viewMode || container.dataset.viewMode || "cards"
    this.navigationMode = (options.navigationMode || container.dataset.navigationMode) === "pagination" ? "pagination" : "infinite"
    this.itemHeight = listRemPx(this.viewMode === "table" ? 2.75 : mailCardRowRem())
    this.subItemHeight = listRemPx(this.viewMode === "table" ? 2 : 3)
    this.expandedThreadGap = 26
    this.overscan = 10

    this.cache = new Map()
    this.indexById = new Map()
    // Rows of the other layout, by "<view mode>:<email id>", fetched while
    // idle so switching between cards and table needs no request. Dropped
    // with the folder or filters; otherViewGeneration tells a late fetch so.
    this.otherViewRows = new Map()
    this.otherViewGeneration = 0
    this.otherViewPending = []
    this.otherViewTimer = null
    this.loadedRanges = []
    this.totalCount = 0
    this.displayTotalCount = 0
    this.effectiveCount = 0
    this.windowStart = 0
    this.selectedEmailId = this.searchScope ? new URLSearchParams(window.location.search).get("selected") : null
    this.nextCursor = null
    this.hasMore = true
    this.isLoading = false
    this.activeFetches = new Set()
    this.newEmailCount = 0
    this.syncState = { active: false, current: 0, total: 0 }
    this.syncBannerDelay = 3000
    this.syncBannerMinVisible = 800
    this.syncBannerLargeRemaining = 1000
    this.syncBannerShowTimer = null
    this.syncBannerHideTimer = null
    this.syncBannerShownAt = 0
    this.filters = this.readFiltersFromURL()
    this.sidebarTag = this.readSidebarTagFromURL()
    this.refreshInFlight = null
    this.refreshQueued = false
    this.refreshQueuedOptions = null
    this.windowedMode = false
    this.windowThreshold = 20000
    this.chunkSize = 100
    this.chunkBuffer = 2
    this.pageSize = parseInt(container.dataset.pageSize) || 50
    this.pageStart = parseInt(container.dataset.windowStart) || 0
    this.loadingSkeletonMinDuration = 180
    this.loadingDirection = null
    this.pendingLoadStart = null
    this.pendingLoadEnd = null
    this.loadError = null
    this.frontierDown = -1
    this.frontierUp = 0
    this.activeChunkFetches = new Set()

    this.prevFirst = null
    this.prevLast = null

    this.spacerTop = null
    this.spacerBottom = null
    this.itemsContainer = null
    this.bannerEl = null
    this.loaderEl = null
    this.transitionOverlay = null
    this.edgeSkeletonEl = null

    this.rowPool = []
    this.visibleRows = new Map()
    this.rowByIndex = new Map()
    this.poolSlack = 6

    this.expandedThreads = new Map()
    this._offsetCache = null
    this._offsetCacheLen = -1
    this._offsetCacheStart = -1

    this.container.style.overflowAnchor = "none"
    this.applyPaneLayoutDensity()
    this.bindEvents()
  }

  isStackedPaneLayout() {
    var main = this.root && this.root.closest ? this.root.closest("#main-content") : null
    if (!main) main = document.getElementById("main-content")
    return !!(main && main.dataset.mailPaneLayout === "stacked")
  }

  mailListFetchLimit() {
    return this.isStackedPaneLayout() ? 32 : 50
  }

  selectedFetchLimit() {
    return this.isStackedPaneLayout() ? 52 : 80
  }

  applyPaneLayoutDensity() {
    var stacked = this.isStackedPaneLayout()
    var overscan = stacked ? 6 : 10
    var chunkSize = stacked ? 60 : 100
    var poolSlack = stacked ? 4 : 6
    var changed = this.overscan !== overscan || this.chunkSize !== chunkSize || this.poolSlack !== poolSlack
    this.overscan = overscan
    this.chunkSize = chunkSize
    this.poolSlack = poolSlack
    if (changed) {
      this.prevFirst = null
      this.prevLast = null
      this.trimRowPool()
    }
  }

  trimRowPool() {
    if (!this.itemsContainer || this.rowPool.length === 0) return
    var needed = Math.ceil(this.container.clientHeight / this.itemHeight) + this.overscan * 2 + this.poolSlack
    for (var i = this.rowPool.length - 1; i >= 0 && this.rowPool.length > needed; i--) {
      var row = this.rowPool[i]
      if (this.visibleRows.has(row)) continue
      row.remove()
      this.rowPool.splice(i, 1)
    }
  }

  rowHTML(item, viewMode) {
    if (!item) return ""
    return item.html || ""
  }

  setCachedRow(pos, id, html, viewMode) {
    var existing = this.cache.get(pos)
    if (existing && existing.id && existing.id !== id) this.indexById.delete(existing.id)
    var item = existing && existing.id === id ? existing : { id: id, html: "" }
    item.id = id
    item.html = html
    // Search results: where the row is, and whether its location band shows.
    var opening = html.slice(0, html.indexOf(">") + 1)
    var location = opening.match(/ data-search-location="([^"]*)"/)
    item.searchLocation = location ? location[1] : ""
    item.searchGroup = opening.indexOf(" data-search-group") !== -1
    this.cache.set(pos, item)
    this.indexById.set(id, pos)
  }

  itemsURLForView(viewMode, start, limit, options) {
    options = options || {}
    var selected = options.selected !== undefined ? options.selected : this.selectedEmailId
    var includeKnownTotal = options.knownTotal !== false
    var url = "/mail/folder/" + this.folderID + "/items?start=" + start + "&limit=" + limit + "&view=" + encodeURIComponent(viewMode === "table" ? "table" : "cards")
    if (selected) url += "&selected=" + encodeURIComponent(selected)
    if (includeKnownTotal && this.totalCount >= 0) url += "&known_total=" + encodeURIComponent(this.totalCount)
    return this.withFilterParams(url)
  }

  _rebuildOffsets() {
    if (this._offsetCacheLen === this.effectiveCount && this._offsetCacheStart === this.windowStart) return
    this._offsetCache = new Array(this.effectiveCount + 1)
    this._offsetCache[0] = 0
    for (var i = 0; i < this.effectiveCount; i++) {
      this._offsetCache[i + 1] = this._offsetCache[i] + this.getHeight(this.windowStart + i)
    }
    this._offsetCacheLen = this.effectiveCount
    this._offsetCacheStart = this.windowStart
  }

  totalHeight() {
    if (this.effectiveCount === 0) return 0
    this._rebuildOffsets()
    return this._offsetCache[this.effectiveCount]
  }

  // scrollToOffset scrolls to rows that may not be drawn yet. The browser
  // caps a scroll at the content it has, so the window gets its full height
  // first; the next render sizes the spacers properly.
  scrollToOffset(offset) {
    if (this.spacerBottom) this.spacerBottom.style.height = this.totalHeight() + "px"
    this.container.scrollTop = offset
  }

  offsetAtPosition(pos) {
    if (pos <= this.windowStart) return 0
    this._rebuildOffsets()
    var rel = pos - this.windowStart
    if (rel >= this.effectiveCount) return this._offsetCache[this.effectiveCount]
    return this._offsetCache[rel]
  }

  positionAtOffset(targetOffset) {
    if (this.effectiveCount === 0) return this.windowStart
    if (targetOffset <= 0) return this.windowStart
    this._rebuildOffsets()
    var lo = 0, hi = this.effectiveCount
    while (lo < hi) {
      var mid = (lo + hi) >> 1
      if (this._offsetCache[mid + 1] <= targetOffset) lo = mid + 1
      else hi = mid
    }
    return this.windowStart + Math.min(lo, this.effectiveCount - 1)
  }

  getHeight(pos) {
    var item = this.cache.get(pos)
    if (!item) return this.itemHeight
    var rowHeight = this.itemHeight + (item.searchGroup ? listRemPx(mailSearchBandRem(this.viewMode)) : 0)
    var expanded = this.expandedThreads.get(item.id)
    if (expanded) {
      if (expanded.measuredHeight) return expanded.measuredHeight
      var gap = this.viewMode === "table" ? 0 : this.expandedThreadGap
      return rowHeight + expanded.subCount * this.subItemHeight + gap
    }
    return rowHeight
  }

  invalidateOffsets() {
    this._offsetCacheLen = -1
    this._offsetCacheStart = -1
    this._offsetCache = null
  }

  captureExpandedThreads() {
    if (!this.expandedThreads || this.expandedThreads.size === 0) return null
    var snapshot = new Map()
    this.expandedThreads.forEach(function (thread, emailId) {
      if (!thread || !thread.html || thread.collapsing) return
      snapshot.set(emailId, {
        html: thread.html,
        subCount: thread.subCount || 0,
        threadId: this.getThreadDataAttr(emailId) || "",
        measuredHeight: thread.measuredHeight || 0,
      })
    }, this)
    return snapshot.size > 0 ? snapshot : null
  }

  restoreExpandedThreads(snapshot) {
    if (!snapshot || typeof snapshot.forEach !== "function") return
    var restored = new Map()
    snapshot.forEach(function (thread, emailId) {
      if (!thread || !thread.html) return
      var pos = this.indexById.get(emailId)
      if (pos === undefined) return
      var item = this.cache.get(pos)
      if (!item || !item.html) return
      var tmp = document.createElement("div")
      tmp.innerHTML = this.rowHTML(item, this.viewMode)
      var node = tmp.firstElementChild
      if (!node || node.dataset.hasThread !== "true") return
      var nextThreadId = node.dataset.threadId || ""
      if (thread.threadId && nextThreadId && thread.threadId !== nextThreadId) return
      restored.set(emailId, {
        html: thread.html,
        subCount: thread.subCount || 0,
        entering: false,
        measuredHeight: thread.measuredHeight || 0,
      })
    }, this)
    this.expandedThreads = restored
    this.invalidateOffsets()
  }

  updateExpandedThreadMeasuredHeight(emailId, shell) {
    var expanded = this.expandedThreads.get(emailId)
    if (!expanded || !shell) return
    var measured = Math.ceil(shell.scrollHeight || 0)
    if (measured <= 0) return
    if (Math.abs((expanded.measuredHeight || 0) - measured) <= 1) return
    expanded.measuredHeight = measured
    this.invalidateOffsets()
    this.scheduleMeasuredLayoutRefresh()
  }

  scheduleMeasuredLayoutRefresh() {
    if (this._threadMeasureRaf) return
    var self = this
    var schedule = typeof requestAnimationFrame === "function"
      ? requestAnimationFrame
      : function (fn) { return setTimeout(fn, 16) }
    this._threadMeasureRaf = schedule(function () {
      self._threadMeasureRaf = null
      self.prevFirst = null
      self.prevLast = null
      self.render()
    })
  }

  setupDOM() {
    this.spacerTop = document.createElement("div")
    this.spacerBottom = document.createElement("div")
    this.itemsContainer = document.createElement("div")

    this.container.innerHTML = ""
    this.appendStickySearchGroup()
    this.container.appendChild(this.spacerTop)
    this.container.appendChild(this.itemsContainer)
    this.container.appendChild(this.spacerBottom)
    this.itemsContainer.style.position = "relative"
    this.itemsContainer.style.minWidth = "0"
    this.itemsContainer.style.overflowAnchor = "none"
  }

  showEdgeSkeleton(direction) {
    if (direction !== "up" || !this.itemsContainer) return
    if (!this.edgeSkeletonEl) {
      this.edgeSkeletonEl = document.createElement("div")
      this.edgeSkeletonEl.style.position = "absolute"
      this.edgeSkeletonEl.style.left = "0"
      this.edgeSkeletonEl.style.right = "0"
      this.edgeSkeletonEl.style.top = "0"
      this.edgeSkeletonEl.style.zIndex = "30"
      this.edgeSkeletonEl.style.pointerEvents = "none"
      this.itemsContainer.appendChild(this.edgeSkeletonEl)
    }
    this.edgeSkeletonEl.style.height = this.itemHeight + "px"
    this.edgeSkeletonEl.innerHTML = this.createSkeleton().outerHTML
  }

  hideEdgeSkeleton() {
    if (!this.edgeSkeletonEl) return
    this.edgeSkeletonEl.remove()
    this.edgeSkeletonEl = null
  }

  bindEvents() {
    var self = this
    var rafId = null
    this.container.addEventListener("scroll", function () {
      if (rafId) return
      rafId = requestAnimationFrame(function () {
        self.render({ scrolling: true })
        rafId = null
      })
    })
    if (this.root) {
      this.root.addEventListener("click", function (e) {
        if (self.navigationMode !== "pagination") return
        var prev = e.target.closest && e.target.closest("[data-mail-page-prev]")
        var next = e.target.closest && e.target.closest("[data-mail-page-next]")
        if (!prev && !next) return
        e.preventDefault()
        if (self.isLoading || (prev && prev.disabled) || (next && next.disabled)) return
        var targetStart = prev ? self.pageStart - self.pageSize : self.pageStart + self.pageSize
        self.loadPage(targetStart, { preserveSelection: true }).catch(function () {})
      })
    }
  }

  // While scrolling, only rows coming into view change: they get their selection
  // marks and htmx here, and the toolbar and select-all controls are left alone
  // unless loaded rows were dropped. Syncing those measures the toolbar, which
  // made Chrome lay out the whole page for every row scrolled in.
  render(options) {
    var scrolling = !!(options && options.scrolling)
    this.applyPaneLayoutDensity()
    this.updateStickySearchGroup()
    var scrollTop = this.container.scrollTop
    var clientHeight = this.container.clientHeight
    if (this.effectiveCount === 0) {
      var stale = this.captureListTransition()
      this.hideEdgeSkeleton()
      this.spacerTop.style.height = "0px"
      this.spacerBottom.style.height = "0px"
      this.rowPool = []
      this.visibleRows.clear()
      this.rowByIndex.clear()
      var syncing = this.syncState && this.syncState.active
      var syncCount = syncing ? this.syncCountLabel() : ""
      var subtitle = syncing
        ? (this.syncActionLabel() + (syncCount ? " (" + syncCount + ")" : "..."))
        : "This folder is empty"
      this.itemsContainer.innerHTML =
        '<div class="flex flex-col items-center justify-center py-20 px-4 text-center">' +
          '<div class="empty-icon-box size-16 rounded-2xl bg-muted/50 flex items-center justify-center mb-4 raised">' +
            '<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="size-7 text-muted-foreground/40" data-lucide="icon">' +
              '<polyline points="22 12 16 12 14 15 10 15 8 12 2 12"/>' +
              '<path d="M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"/>' +
            '</svg>' +
          '</div>' +
          '<h3 class="font-semibold text-sm mb-1">' + (syncing ? 'Syncing folder' : 'No emails') + '</h3>' +
          '<p class="text-xs text-muted-foreground">' + subtitle + '</p>' +
        '</div>'
      if (stale) this.fanOutLeavingRows(stale.rows, new Set())
      return
    }

    this._rebuildOffsets()

    var first = this.positionAtOffset(Math.max(0, scrollTop - this.overscan * this.itemHeight))
    var last = this.positionAtOffset(Math.min(this.totalHeight(), scrollTop + clientHeight + this.overscan * this.itemHeight))
    last = Math.min(last, this.windowStart + this.effectiveCount - 1)

    if (first === this.prevFirst && last === this.prevLast) {
      this.maybeLoadAtEdges(first, last)
      return
    }
    this.prevFirst = first
    this.prevLast = last

    this.ensureRangeLoaded(first, last)
    this.maybeLoadAtEdges(first, last)
    if (this.evictFarChunks(first, last)) {
      scrolling = false
      scrollTop = this.container.scrollTop
      this._rebuildOffsets()
      first = this.positionAtOffset(Math.max(0, scrollTop - this.overscan * this.itemHeight))
      last = this.positionAtOffset(Math.min(this.totalHeight(), scrollTop + clientHeight + this.overscan * this.itemHeight))
      last = Math.min(last, this.windowStart + this.effectiveCount - 1)
      this.prevFirst = first
      this.prevLast = last
    }

    this.spacerTop.style.height = "0px"
    this.spacerBottom.style.height = "0px"
    var height = this.totalHeight() + "px"
    if (this.itemsContainer.style.height !== height) this.itemsContainer.style.height = height

    var stamped = this.renderPooled(first, last)
    if (scrolling) {
      for (var i = 0; i < stamped.length; i++) {
        if (typeof window.syncMailRowSelection === "function") window.syncMailRowSelection(stamped[i])
        if (typeof htmx !== "undefined") htmx.process(stamped[i])
      }
    } else {
      this.syncSelectionClasses(this.itemsContainer)
      if (typeof window.syncMailSelectionControls === "function") window.syncMailSelectionControls()
      if (typeof htmx !== "undefined") htmx.process(this.itemsContainer)
    }

    if (this.container.scrollTop !== scrollTop) {
      this.container.scrollTop = scrollTop
    }
  }

  ensureRowPool() {
    var needed = Math.ceil(this.container.clientHeight / this.itemHeight) + this.overscan * 2 + this.poolSlack
    while (this.rowPool.length < needed) {
      var shell = document.createElement("div")
      shell.style.position = "absolute"
      shell.style.left = "0"
      shell.style.right = "0"
      // No will-change: a row on its own compositor layer loses subpixel text
      // smoothing in Chrome, and bold or colored text looked blurry.
      shell.hidden = true
      this.itemsContainer.appendChild(shell)
      this.rowPool.push(shell)
    }
  }

  acquireRow(index) {
    if (this.rowByIndex.has(index)) return this.rowByIndex.get(index)
    for (var i = 0; i < this.rowPool.length; i++) {
      var row = this.rowPool[i]
      if (!this.visibleRows.has(row)) {
        this.visibleRows.set(row, index)
        this.rowByIndex.set(index, row)
        return row
      }
    }
    return null
  }

  releaseRow(row) {
    var idx = this.visibleRows.get(row)
    if (idx !== undefined) this.rowByIndex.delete(idx)
    this.visibleRows.delete(row)
    row.hidden = true
  }

  // Returns the shells that got a mail row's content.
  renderPooled(first, last) {
    this.ensureRowPool()
    var stamped = []
    var entries = Array.from(this.visibleRows.entries())
    for (var i = 0; i < entries.length; i++) {
      var idx = entries[i][1]
      if (idx < first || idx > last) this.releaseRow(entries[i][0])
    }
    for (var pos = first; pos <= last; pos++) {
      if (this.rowByIndex.has(pos)) continue
      var row = this.acquireRow(pos)
      if (!row) continue
      if (this.stampRow(row, pos)) stamped.push(row)
    }
    var vis = Array.from(this.visibleRows.entries())
    for (var j = 0; j < vis.length; j++) {
      if (this.stampRow(vis[j][0], vis[j][1])) stamped.push(vis[j][0])
    }
    return stamped
  }

  // Returns whether the shell got a mail row's content.
  stampRow(shell, index) {
    var item = this.cache.get(index)
    if (shell.hidden) shell.hidden = false
    var transform = "translateY(" + this.offsetAtPosition(index) + "px)"
    if (shell.style.transform !== transform) shell.style.transform = transform
    var height = this.getHeight(index) + "px"
    if (shell.style.height !== height) shell.style.height = height
    if (!item) {
      if (shell._mailRenderSkeleton === true && shell._mailRenderViewMode === this.viewMode) return false
      shell._mailRenderSkeleton = true
      shell._mailRenderId = ""
      shell._mailRenderHTML = ""
      shell._mailRenderExpandedHTML = ""
      shell._mailRenderExpandedSubCount = 0
      shell._mailRenderExpandedEntering = false
      shell._mailRenderExpandedCollapsing = false
      shell._mailRenderViewMode = this.viewMode
      shell.className = ""
      shell.removeAttribute("data-thread-entering")
      shell.removeAttribute("data-thread-collapsing")
      shell.innerHTML = this.createSkeleton().outerHTML
      if (this.viewMode === "cards" && typeof window.applyMailCardLayoutSettings === "function") window.applyMailCardLayoutSettings(shell)
      return false
    }
    var expanded = this.expandedThreads.get(item.id)
    var expandedHTML = expanded && expanded.html ? expanded.html : ""
    var expandedSubCount = expanded ? (expanded.subCount || 0) : 0
    var expandedEntering = !!(expanded && expanded.entering)
    var expandedCollapsing = !!(expanded && expanded.collapsing)
    if (
      shell._mailRenderSkeleton === false &&
      shell._mailRenderId === item.id &&
      shell._mailRenderHTML === item.html &&
      shell._mailRenderExpandedHTML === expandedHTML &&
      shell._mailRenderExpandedSubCount === expandedSubCount &&
      shell._mailRenderExpandedEntering === expandedEntering &&
      shell._mailRenderExpandedCollapsing === expandedCollapsing &&
      shell._mailRenderViewMode === this.viewMode
    ) {
      return false
    }
    shell._mailRenderSkeleton = false
    shell._mailRenderId = item.id
    shell._mailRenderHTML = item.html
    shell._mailRenderExpandedHTML = expandedHTML
    shell._mailRenderExpandedSubCount = expandedSubCount
    shell._mailRenderExpandedEntering = expandedEntering
    shell._mailRenderExpandedCollapsing = expandedCollapsing
    shell._mailRenderViewMode = this.viewMode
    shell.className = ""
    shell.removeAttribute("data-thread-entering")
    shell.removeAttribute("data-thread-collapsing")
    shell.innerHTML = this.rowHTML(item, this.viewMode)
    if (this.viewMode === "cards" && typeof window.applyMailCardLayoutSettings === "function") window.applyMailCardLayoutSettings(shell)
    var row = shell.querySelector(".mail-list-item") || shell.firstElementChild
    if (!row) return true
    var anchor = row.querySelector("a")
    if (!anchor) return true
    if (item.id === this.selectedEmailId) {
      anchor.classList.remove("envelope")
      anchor.classList.add("envelope-active")
    } else {
      anchor.classList.remove("envelope-active")
      anchor.classList.add("envelope")
    }
    var toggle = row.querySelector("[data-thread-toggle]")
    if (expanded && expanded.html) {
      shell.className = "mail-list-thread-slot"
      if (expanded.entering) shell.setAttribute("data-thread-entering", "")
      else shell.removeAttribute("data-thread-entering")
      if (expanded.collapsing) shell.setAttribute("data-thread-collapsing", "")
      else shell.removeAttribute("data-thread-collapsing")
      var mainRow = document.createElement("div")
      mainRow.className = "mail-list-thread-main"
      if (item.searchGroup) mainRow.setAttribute("data-search-group", "")
      anchor.style.height = ""
      if (toggle) toggle.setAttribute("data-expanded", "")
      mainRow.appendChild(row)

      var subContainer = document.createElement("div")
      subContainer.className = "thread-sub-items"
      subContainer.innerHTML = expanded.html

      shell.replaceChildren(mainRow, subContainer)
      this.syncSelectionClasses(shell)
      if (typeof window.syncMailRowSelection === "function") window.syncMailRowSelection(shell)
      this.updateExpandedThreadMeasuredHeight(item.id, shell)
      return true
    }
    if (toggle) toggle.removeAttribute("data-expanded")
    anchor.style.height = ""
    return true
  }

  setSyncState(active, current, total, meta) {
    meta = meta || {}
    this.syncState = {
      active: !!active,
      current: current || 0,
      total: total || 0,
      accountName: meta.accountName || meta.account_name || meta.name || "",
      accountEmail: meta.accountEmail || meta.account_email || meta.email || "",
      folderName: meta.folderName || meta.current_folder || meta.folder_name || "",
      folderRole: meta.folderRole || meta.folder_role || "",
      provider: meta.provider || "",
      mode: meta.mode || "",
      refreshOnly: !!(meta.refreshOnly || meta.refresh_only),
      totalEstimated: !!(meta.totalEstimated || meta.total_estimated),
    }
    if (this.totalCount === 0) {
      this.prevFirst = null
      this.prevLast = null
      this.render()
    }
    this.updateSyncHeader()
  }

  syncUsesIndeterminateProgress() {
    var state = this.syncState || {}
    return !!(state.refreshOnly || state.totalEstimated || !(state.total > 0))
  }

  syncActionLabel() {
    var state = this.syncState || {}
    if (state.refreshOnly && state.provider === "gmail_api" && state.mode === "repair") return "Repairing Gmail"
    if (state.refreshOnly && state.provider === "gmail_api") return "Refreshing Gmail labels"
    if (state.refreshOnly) return "Refreshing mailbox state"
    return "Syncing folder"
  }

  syncDetailLabel() {
    var state = this.syncState || {}
    var parts = []
    var account = state.accountEmail || state.accountName || ""
    if (account) parts.push(account)
    if (state.folderName) parts.push(state.folderName)
    return parts.join(" · ")
  }

  syncNumber(value) {
    value = Math.max(0, Number(value) || 0)
    try { return value.toLocaleString() } catch (_) { return String(value) }
  }

  syncCountLabel() {
    var state = this.syncState || {}
    var current = Math.max(0, Number(state.current) || 0)
    var total = Math.max(0, Number(state.total) || 0)
    var verb = state.refreshOnly ? "refreshed" : "fetched"
    if (!this.syncUsesIndeterminateProgress() && total > 0) {
      return this.syncNumber(current) + " / " + this.syncNumber(total) + " " + verb
    }
    if (current > 0) return this.syncNumber(current) + " " + verb
    return ""
  }

  syncHeaderText() {
    var detail = this.syncDetailLabel()
    return this.syncActionLabel() + (detail ? ": " + detail : "")
  }

  createMailRow(html) {
    var row = document.createElement("div")
    row.innerHTML = html
    return row.firstChild
  }

  collectRenderedRows() {
    var rows = new Map()
    if (!this.itemsContainer) return rows
    for (var i = 0; i < this.itemsContainer.children.length; i++) {
      var node = this.itemsContainer.children[i]
      var row = node.classList.contains("mail-list-item") ? node : node.querySelector(".mail-list-item")
      if (!row || !row.dataset.emailId) continue
      rows.set(row.dataset.emailId, {
        row: row,
        slot: node.classList.contains("mail-list-thread-slot") ? node : null,
      })
    }
    return rows
  }

  captureRenderedLayout() {
    var layout = new Map()
    if (!this.itemsContainer) return layout
    for (var i = 0; i < this.itemsContainer.children.length; i++) {
      var node = this.itemsContainer.children[i]
      var row = node.classList.contains("mail-list-item") ? node : node.querySelector(".mail-list-item")
      if (!row || !row.dataset.emailId) continue
      layout.set(row.dataset.emailId, node.getBoundingClientRect())
    }
    return layout
  }

  captureListTransition() {
    if (this.prefersReducedMotion()) return null
    var rows = new Map()
    if (!this.itemsContainer) return { rows: rows }
    for (var i = 0; i < this.itemsContainer.children.length; i++) {
      var node = this.itemsContainer.children[i]
      var row = node.classList.contains("mail-list-item") ? node : node.querySelector(".mail-list-item")
      if (!row || !row.dataset.emailId) continue
      var anchor = row.querySelector("a")
      var visualRect = anchor ? anchor.getBoundingClientRect() : node.getBoundingClientRect()
      var shellRect = node.getBoundingClientRect()
      rows.set(row.dataset.emailId, {
        rect: shellRect,
        visualTopOffset: visualRect.top - shellRect.top,
        html: row.outerHTML,
      })
    }
    return { rows: rows }
  }

  prefersReducedMotion() {
    return window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches
  }

  // appendStickySearchGroup adds the band that pins a run of search results'
  // location to the top of the list (updateStickySearchGroup).
  appendStickySearchGroup() {
    this.stickySearchGroup = document.createElement("div")
    this.stickySearchGroup.className = "mail-search-sticky"
    this.stickySearchGroup.hidden = true
    this.stickySearchGroup.setAttribute("aria-hidden", "true")
    this.container.appendChild(this.stickySearchGroup)
  }

  // updateStickySearchGroup keeps the location of the topmost table row's run
  // of search results pinned under the column names once that run's own
  // band has scrolled away. The next run's band pushes it up as it arrives.
  updateStickySearchGroup() {
    var sticky = this.stickySearchGroup
    if (!sticky) return
    var hide = function () { if (!sticky.hidden) sticky.hidden = true }
    // Only the table groups rows under one band; every card has its own.
    if (!this.searchScope || this.viewMode !== "table" || this.effectiveCount === 0) return hide()
    var scrollTop = this.container.scrollTop
    var top = this.positionAtOffset(scrollTop)
    var item = this.cache.get(top)
    if (!item || !item.searchLocation) return hide()
    var start = top
    while (start > this.windowStart) {
      var previous = this.cache.get(start - 1)
      if (!previous || previous.searchLocation !== item.searchLocation) break
      start--
    }
    // The run's own band is still in view.
    if (this.offsetAtPosition(start) >= scrollTop) return hide()
    var band = listRemPx(mailSearchBandRem(this.viewMode))
    var shift = 0
    var last = Math.min(this.windowStart + this.effectiveCount - 1, top + 3)
    for (var next = top + 1; next <= last; next++) {
      var nextItem = this.cache.get(next)
      if (!nextItem) break
      if (nextItem.searchGroup) {
        shift = Math.min(0, this.offsetAtPosition(next) - scrollTop - band)
        break
      }
    }
    if (sticky._location !== item.searchLocation) {
      sticky._location = item.searchLocation
      var decode = document.createElement("textarea")
      decode.innerHTML = item.searchLocation
      sticky.innerHTML = '<div class="mail-search-location-strip"><svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="size-3 shrink-0"><path d="M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z"/></svg><span class="truncate"></span></div>'
      sticky.querySelector("span").textContent = decode.value
    }
    if (sticky.hidden) sticky.hidden = false
    // In the table it pins below the column names, which pin at the top. It
    // tucks a pixel under them (they draw above it), so a fractional height
    // can't leave a gap for the rows to show through.
    var columns = this.viewMode === "table" ? this.container.querySelector(".mail-list-table-header") : null
    var pinnedTop = (columns ? Math.max(0, parseFloat(getComputedStyle(columns).height) - 1) : 0) + "px"
    if (sticky.style.top !== pinnedTop) sticky.style.top = pinnedTop
    // The band lines up with the rows' own bands; the next run's pushes it.
    var strip = sticky.firstElementChild
    var anchor = this.itemsContainer && this.itemsContainer.querySelector(".mail-list-item > a")
    if (anchor) {
      var anchorRect = anchor.getBoundingClientRect()
      var left = (anchorRect.left - sticky.getBoundingClientRect().left) + "px"
      var width = anchorRect.width + "px"
      if (strip.style.left !== left) strip.style.left = left
      if (strip.style.width !== width) strip.style.width = width
    }
    var transform = shift ? "translateY(" + shift + "px)" : ""
    if (strip.style.transform !== transform) strip.style.transform = transform
  }

  // onScreenRows are the rendered rows the reader can see, top to bottom.
  onScreenRows() {
    if (!this.itemsContainer) return []
    var view = this.container.getBoundingClientRect()
    var rows = []
    for (var i = 0; i < this.itemsContainer.children.length; i++) {
      var node = this.itemsContainer.children[i]
      var row = node.classList.contains("mail-list-item") ? node : node.querySelector(".mail-list-item")
      if (!row || !row.dataset.emailId) continue
      var rect = node.getBoundingClientRect()
      if (rect.bottom > view.top && rect.top < view.bottom) rows.push({ node: node, top: rect.top })
    }
    rows.sort(function (a, b) { return a.top - b.top })
    return rows.map(function (row) { return row.node })
  }

  // clearForTransition fans the list's rows out, top to bottom, and resolves
  // once the last is gone. Moving between a folder and search results
  // replaces the whole list rather than morphing one into the other, so the
  // old list visibly ends before the new one fans in (fanInRows). Rows keep
  // their positioning transform; the fan uses translate and opacity.
  clearForTransition() {
    var nodes = this.prefersReducedMotion() ? [] : this.onScreenRows()
    var self = this
    var hide = function () { if (self.itemsContainer) self.itemsContainer.style.visibility = "hidden" }
    VirtualMailList.fanInNext = nodes.length > 0
    if (nodes.length === 0 || typeof nodes[0].animate !== "function") {
      hide()
      return Promise.resolve()
    }
    this.transitionAnimations = nodes.map(function (node, i) {
      // Each card drops back and shrinks a little before it fades, so it
      // reads as moving away.
      return node.animate([
        { opacity: 1, translate: "0 0", scale: "1" },
        { opacity: 1, offset: 0.45 },
        { opacity: 0, translate: "0 18px", scale: "0.99" },
      ], { duration: 220, delay: Math.min(150, i * 25), easing: "cubic-bezier(0.4, 0, 1, 1)", fill: "forwards" })
    })
    return Promise.all(this.transitionAnimations.map(function (animation) {
      return animation.finished.catch(function () {})
    })).then(hide)
  }

  // releaseClearedRows drops the finished fan-out from the row elements but
  // keeps the list hidden, so the list can reuse them for new rows and show
  // those with fanInRows.
  releaseClearedRows() {
    VirtualMailList.fanInNext = false
    ;(this.transitionAnimations || []).forEach(function (animation) { animation.cancel() })
    this.transitionAnimations = null
  }

  // cancelTransitionClear shows the rows again when the list they were
  // cleared for couldn't load.
  cancelTransitionClear() {
    VirtualMailList.fanInNext = false
    document.documentElement.classList.remove("mail-list-entering")
    ;(this.transitionAnimations || []).forEach(function (animation) { animation.cancel() })
    this.transitionAnimations = null
    if (this.itemsContainer) this.itemsContainer.style.visibility = ""
  }

  // fanOutLeavingRows fans out the rows a list change removed, from where
  // they were, top to bottom, and returns how many it shows leaving. They
  // are copies by now, drawn under the list so the rows arriving in their
  // place land on top of them.
  fanOutLeavingRows(before, afterIds) {
    var view = this.container.getBoundingClientRect()
    var leaving = []
    before.forEach(function (item, id) {
      if (afterIds.has(id)) return
      if (item.rect.bottom > view.top && item.rect.top < view.bottom) leaving.push(item)
    })
    if (leaving.length === 0 || this.prefersReducedMotion()) return 0
    leaving.sort(function (a, b) { return a.rect.top - b.rect.top })
    var overlay = this.ensureTransitionOverlay()
    overlay.style.zIndex = "0"
    this.itemsContainer.style.zIndex = "1"
    var itemsContainer = this.itemsContainer
    var scrollLeft = this.container.scrollLeft
    var scrollTop = this.container.scrollTop
    var last = null
    for (var i = 0; i < leaving.length; i++) {
      var item = leaving[i]
      var clone = document.createElement("div")
      clone.innerHTML = item.html
      clone.style.position = "absolute"
      clone.style.left = (item.rect.left - view.left + scrollLeft) + "px"
      clone.style.top = (item.rect.top - view.top + scrollTop) + "px"
      clone.style.width = item.rect.width + "px"
      clone.style.height = item.rect.height + "px"
      overlay.appendChild(clone)
      if (typeof clone.animate !== "function") continue
      last = clone.animate([
        { opacity: 1, translate: "0 0", scale: "1" },
        { opacity: 1, offset: 0.45 },
        { opacity: 0, translate: "0 18px", scale: "0.99" },
      ], { duration: 220, delay: Math.min(150, i * 25), easing: "cubic-bezier(0.4, 0, 1, 1)", fill: "forwards" })
    }
    var done = function () {
      if (overlay.parentNode) overlay.remove()
      itemsContainer.style.zIndex = ""
    }
    if (last) last.finished.then(done, done)
    else done()
    return leaving.length
  }

  // fanInRows is clearForTransition run backwards for the new list's rows:
  // the same stagger and distance, each card fading in first and then
  // dropping into place from above, slightly bigger as if dropped. The rows
  // arrive hidden (the mail-list-entering class); they stay hidden until the
  // list has been drawn once, because an animation started during the rest
  // of the set-up would have its first cards done before anyone saw them.
  fanInRows(rows, delay) {
    delay = delay || 0
    var nodes = this.prefersReducedMotion() ? [] : this.onScreenRows()
    if (rows) nodes = nodes.filter(function (node) { return rows.indexOf(node) !== -1 })
    for (var i = 0; i < nodes.length; i++) nodes[i].style.opacity = "0"
    document.documentElement.classList.remove("mail-list-entering")
    if (nodes.length === 0) return
    requestAnimationFrame(function () {
      requestAnimationFrame(function () {
        for (var j = 0; j < nodes.length; j++) {
          nodes[j].style.opacity = ""
          if (typeof nodes[j].animate !== "function") continue
          nodes[j].animate([
            { opacity: 0, translate: "0 -18px", scale: "1.01" },
            { opacity: 1, offset: 0.55 },
            { opacity: 1, translate: "0 0", scale: "1" },
          ], { duration: 220, delay: delay + Math.min(150, j * 25), easing: "cubic-bezier(0, 0, 0.6, 1)", fill: "backwards" })
        }
      })
    })
  }

  ensureTransitionOverlay() {
    var existing = this.container.querySelector("[data-mail-list-transition-overlay]")
    if (existing) existing.remove()
    var overlay = document.createElement("div")
    overlay.setAttribute("data-mail-list-transition-overlay", "")
    overlay.style.position = "absolute"
    overlay.style.left = "0"
    overlay.style.top = "0"
    overlay.style.right = "0"
    overlay.style.pointerEvents = "none"
    overlay.style.zIndex = "30"
    overlay.style.overflow = "visible"
    var containerStyle = window.getComputedStyle(this.container)
    if (containerStyle.position === "static") this.container.style.position = "relative"
    this.container.appendChild(overlay)
    this.transitionOverlay = overlay
    return overlay
  }

  animateListTransition(snapshot, options) {
    if (!snapshot || !snapshot.rows || this.prefersReducedMotion()) return
    if (!this.itemsContainer) return

    options = options || {}
    var duration = options.duration || 190
    var ease = "cubic-bezier(0.2, 0, 0, 1)"
    var before = snapshot.rows
    var afterIds = new Set()
    var entering = []

    if (this.effectiveCount === 0) {
      this.fanOutLeavingRows(before, afterIds)
      return
    }

    // Every row is measured first, then every row moved to where it was,
    // then the page laid out once before they slide back. Measuring and
    // moving each row in turn made Chrome lay out the whole list again for
    // every row, which froze a switch between cards and table for most of a
    // second.
    var moving = []
    for (var i = 0; i < this.itemsContainer.children.length; i++) {
      var node = this.itemsContainer.children[i]
      var row = node.classList.contains("mail-list-item") ? node : node.querySelector(".mail-list-item")
      if (!row || !row.dataset.emailId) continue
      var id = row.dataset.emailId
      afterIds.add(id)
      var old = before.get(id)
      if (!old) {
        entering.push(node)
        continue
      }
      var next = node.getBoundingClientRect()
      var nextAnchor = node.querySelector(".mail-list-item > a")
      var nextVisualRect = nextAnchor ? nextAnchor.getBoundingClientRect() : next
      var nextVisualTopOffset = nextVisualRect.top - next.top
      var dx = old.rect.left - next.left
      var dy = (old.rect.top + (old.visualTopOffset || 0)) - (next.top + nextVisualTopOffset)
      var heightChanged = options.animateHeight && Math.abs(old.rect.height - next.height) > 0.5
      if (Math.abs(dx) > 0.5 || Math.abs(dy) > 0.5 || heightChanged) {
        moving.push({ node: node, dx: dx, dy: dy, oldHeight: old.rect.height, finalHeight: node.style.height || (next.height + "px"), heightChanged: heightChanged, base: node.style.transform || "" })
      } else if (options.enterExisting) {
        entering.push(node)
      }
    }
    for (var m = 0; m < moving.length; m++) {
      var move = moving[m]
      move.node.style.transition = "none"
      move.node.style.transform = move.base + " translate(" + move.dx + "px," + move.dy + "px)"
      if (move.heightChanged) {
        move.node.style.height = move.oldHeight + "px"
        move.node.style.overflow = "hidden"
      }
    }
    if (moving.length) this.itemsContainer.offsetHeight
    for (var n = 0; n < moving.length; n++) {
      var moved = moving[n]
      moved.node.style.transition = "transform " + duration + "ms " + ease + (moved.heightChanged ? ", height " + duration + "ms " + ease : "")
      moved.node.style.transform = moved.base
      if (moved.heightChanged) moved.node.style.height = moved.finalHeight
      this.cleanupTransition(moved.node, duration)
    }

    // The rows staying slide to their places, while the ones leaving fan out
    // and the new ones fan in, together; the leaving ones get a short head
    // start.
    var leaving = this.fanOutLeavingRows(before, afterIds)
    this.fanInRows(entering, leaving > 0 ? 60 : 0)
  }

  animateRenderedRows(options) {
    if (this.prefersReducedMotion() || !this.itemsContainer) return
    options = options || {}
    var nodes = []
    for (var i = 0; i < this.itemsContainer.children.length; i++) {
      var node = this.itemsContainer.children[i]
      var row = node.classList.contains("mail-list-item") ? node : node.querySelector(".mail-list-item")
      if (row && row.dataset.emailId) nodes.push(node)
    }
    if (nodes.length === 0) return
    var duration = options.duration || 190
    var ease = "cubic-bezier(0.2, 0, 0, 1)"
    var stagger = options.enterStagger !== undefined ? options.enterStagger : (nodes.length > 3 ? 12 : 0)
    this.animateEnteringRows(nodes, duration, ease, options.enterFrom || -8, options.enterDelay || 0, stagger)
  }

  animateEnteringRows(nodes, duration, ease, offsetY, delay, stagger) {
    slideRowsInWholePixels(nodes, duration, offsetY, delay || 0, stagger || 0)
  }

  cleanupTransition(node, duration) {
    setTimeout(function () {
      node.style.transition = ""
      node.style.opacity = ""
      node.style.willChange = ""
      node.style.overflow = ""
    }, duration + 40)
  }

  animateLayoutShift(previousLayout) {
    if (!previousLayout || previousLayout.size === 0) return
    for (var i = 0; i < this.itemsContainer.children.length; i++) {
      var node = this.itemsContainer.children[i]
      var row = node.classList.contains("mail-list-item") ? node : node.querySelector(".mail-list-item")
      if (!row || !row.dataset.emailId) continue
      var oldRect = previousLayout.get(row.dataset.emailId)
      if (!oldRect) continue
      var newRect = node.getBoundingClientRect()
      var dy = oldRect.top - newRect.top
      if (Math.abs(dy) < 1) continue
      var baseTransform = node.style.transform || ""
      node.style.transition = "none"
      node.style.transform = baseTransform + " translateY(" + dy + "px)"
      node.offsetHeight
      node.style.transition = "transform 120ms ease-out"
      node.style.transform = baseTransform
      this.cleanupTransition(node, 120)
    }
  }

  finishThreadEnter(emailId) {
    var expanded = this.expandedThreads.get(emailId)
    if (expanded) expanded.entering = false
    var row = this.container.querySelector('[data-email-id="' + emailId + '"]')
    var slot = row ? row.closest(".mail-list-thread-slot") : null
    if (slot) slot.removeAttribute("data-thread-entering")
  }

  syncSelectionClasses(root) {
    if (!root) return
    var active = root.querySelectorAll(".envelope-active")
    for (var i = 0; i < active.length; i++) {
      active[i].classList.remove("envelope-active")
      if (active[i].closest(".mail-list-item")) active[i].classList.add("envelope")
    }

    if (!this.selectedEmailId) return
    var main = root.querySelector('[data-email-id="' + this.selectedEmailId + '"] > a')
    if (main) {
      main.classList.remove("envelope")
      main.classList.add("envelope-active")
      return
    }

    var sub = root.querySelector('[data-sub-email-id="' + this.selectedEmailId + '"] > a')
    if (sub) sub.classList.add("envelope-active")
  }

  createSkeleton() {
    var row = document.createElement("div")
    row.className = "mail-list-skeleton" + (this.viewMode === "table" ? " mail-list-table-skeleton" : " mail-list-card")
    if (this.viewMode === "table") {
      row.innerHTML =
        '<div class="mail-list-table-grid grid items-center gap-3 w-full px-3 py-1.5">' +
        '<div class="flex items-center justify-center shrink-0" data-mail-table-cell="accountMarker">' +
        '<div class="h-2.5 w-2.5 rounded bg-muted animate-pulse"></div>' +
        "</div>" +
        '<div class="flex items-center justify-center shrink-0" data-mail-table-cell="starred">' +
        '<div class="h-3 w-3 rounded bg-muted animate-pulse"></div>' +
        "</div>" +
        '<div class="flex items-center justify-center shrink-0" data-mail-table-cell="attachment">' +
        '<div class="h-3 w-3 rounded bg-muted animate-pulse"></div>' +
        "</div>" +
        '<div data-mail-table-cell="thread"></div>' +
        '<div class="flex items-center min-w-0" data-mail-table-cell="from">' +
        '<div class="h-3 w-24 rounded bg-muted animate-pulse"></div>' +
        "</div>" +
        '<div class="flex items-center min-w-0" data-mail-table-cell="to">' +
        '<div class="h-3 w-24 rounded bg-muted animate-pulse"></div>' +
        "</div>" +
        '<div class="flex items-center gap-2 min-w-0" data-mail-table-cell="subject">' +
        '<div class="h-3 w-40 rounded bg-muted animate-pulse"></div>' +
        '<div class="hidden xl:block h-3 w-28 rounded bg-muted animate-pulse"></div>' +
        "</div>" +
        '<div class="flex items-center justify-end shrink-0" data-mail-table-cell="date">' +
        '<div class="h-3 w-12 rounded bg-muted animate-pulse"></div>' +
        "</div>" +
        "</div>"
      return row
    }
    row.setAttribute("data-mail-card-layout-scope", "")
    row.innerHTML =
      '<div class="mail-list-card-zone mail-list-card-zone-rail-top" data-mail-card-zone="railTop">' +
      '<div data-mail-card-field="avatar" class="size-6 rounded-full bg-muted animate-pulse shadow-[0_1px_3px_rgba(0,0,0,0.12)]"></div>' +
      "</div>" +
      '<div class="mail-list-card-zone mail-list-card-zone-rail-middle" data-mail-card-zone="railMiddle">' +
      '<div data-mail-card-field="accountMarker" class="account-color-marker size-2.5 shrink-0 bg-muted animate-pulse"></div>' +
      "</div>" +
      '<div class="mail-list-card-zone mail-list-card-zone-rail-bottom" data-mail-card-zone="railBottom">' +
      '<div data-mail-card-field="thread" class="mail-list-card-empty-icon-slot"></div>' +
      "</div>" +
      '<div class="mail-list-card-zone mail-list-card-zone-header" data-mail-card-zone="header">' +
      '<div data-mail-card-field="from" class="h-3.5 w-32 max-w-[42%] rounded bg-muted animate-pulse"></div>' +
      '<div data-mail-card-field="date" class="h-3 w-16 rounded bg-muted animate-pulse"></div>' +
      '<div data-mail-card-field="account" class="h-4 w-20 rounded-full border border-border bg-background animate-pulse"></div>' +
      "</div>" +
      '<div class="mail-list-card-zone mail-list-card-zone-meta" data-mail-card-zone="meta">' +
      '<div data-mail-card-field="attachment" class="mail-list-card-empty-icon-slot"></div>' +
      '<div data-mail-card-field="unread" class="inline-flex size-4 shrink-0 items-center justify-center"><span class="size-2 rounded-full bg-muted animate-pulse"></span></div>' +
      "</div>" +
      '<div class="mail-list-card-zone mail-list-card-zone-body" data-mail-card-zone="body">' +
      '<div data-mail-card-field="to" class="h-3 w-36 max-w-[46%] rounded bg-muted animate-pulse"></div>' +
      "</div>" +
      '<div class="mail-list-card-zone mail-list-card-zone-body-bottom" data-mail-card-zone="bodyBottom">' +
      '<div data-mail-card-field="subject" class="h-3.5 w-56 max-w-[58%] rounded bg-muted animate-pulse"></div>' +
      "</div>" +
      '<div class="mail-list-card-zone mail-list-card-zone-footer" data-mail-card-zone="footer">' +
      '<div data-mail-card-field="preview" class="h-3 w-72 max-w-[70%] rounded bg-muted animate-pulse"></div>' +
      '<div data-mail-card-field="labels" class="h-4 w-10 rounded bg-muted animate-pulse"></div>' +
      "</div>" +
      '<div class="mail-list-card-zone mail-list-card-zone-status" data-mail-card-zone="status"></div>' +
      '<div class="mail-list-card-zone mail-list-card-zone-corner" data-mail-card-zone="corner"><div data-mail-card-field="starred" class="h-3 w-3 rounded bg-muted animate-pulse"></div></div>' +
      '<div class="hidden" data-mail-card-zone="hidden"></div>'
    if (typeof window.applyMailCardLayoutSettings === "function") window.applyMailCardLayoutSettings(row)
    return row
  }

  async ensureRangeLoaded(first, last) {
    if (first > last) return
    if (this.activeChunkFetches.size > 0) return
    var gaps = this.findGaps(first, last)
    for (var i = 0; i < gaps.length; i++) {
      var gap = gaps[i]
      if (gap.end - gap.start > 300) {
        for (var splitStart = gap.start; splitStart <= gap.end; splitStart += 300) {
          await this.fetchRange(splitStart, Math.min(splitStart + 299, gap.end))
        }
      } else {
        await this.fetchRange(gap.start, gap.end)
      }
    }
  }

  maybeLoadAtEdges(first, last) {
    if (this.navigationMode === "pagination") return
    if (this.activeChunkFetches.size > 0) return
    if (!this.hasMore) return
    if (this.effectiveCount >= this.totalCount) return

    var viewportBottom = this.container.scrollTop + this.container.clientHeight
    if (this.frontierDown < this.totalCount - 1 && viewportBottom >= this.totalHeight() - 1) {
      this.loadChunk(Math.floor((this.frontierDown + 1) / this.chunkSize), "down")
      return
    }
    if (this.frontierUp > 0 && this.container.scrollTop <= 1) {
      this.loadChunk(Math.floor((this.frontierUp - 1) / this.chunkSize), "up")
    }
  }

  async loadChunk(chunkIndex, direction) {
    var start = direction === "down" ? this.frontierDown + 1 : chunkIndex * this.chunkSize
    if (chunkIndex < 0 || start >= this.totalCount) return
    var end = direction === "up" ? this.frontierUp - 1 : Math.min(this.totalCount - 1, start + this.chunkSize - 1)
    if (end < start) return
    var chunkKey = "chunk-" + chunkIndex
    if (this.activeChunkFetches.has(chunkKey)) return
    if (this.findGaps(start, end).length === 0) return
    this.activeChunkFetches.add(chunkKey)
    this.loadingDirection = direction
    this.pendingLoadStart = direction === "up" ? end : null
    this.pendingLoadEnd = direction === "down" ? start : null
    this.loadError = null
    var revealStartedAt = this.now()
    var revealPendingDownRow = direction === "down"
    this.updateEffectiveCount()
    if (revealPendingDownRow) {
      this.container.scrollTop = this.container.scrollTop + this.itemHeight
    }
    this.prevFirst = null
    this.prevLast = null
    this.render()
    this.showEdgeSkeleton(direction)
    this.pinPendingSkeleton(direction)
    var skeletonRevealed = this.waitForSkeletonReveal(revealStartedAt, direction)
    try {
      if (window.__debugWindowedMail) {
        console.debug("[mail-chunk] load", direction, chunkIndex, start, end)
      }
      var htmlPromise = this.fetchHTML(this.itemsURLForView(this.viewMode, start, end - start + 1))
      if (direction === "up") await skeletonRevealed
      var html = await htmlPromise
      if (direction !== "up") await skeletonRevealed
      this.ingestHTML(html)
      this.prevFirst = null
      this.prevLast = null
      this.render()
      this.frontierDown = Math.max(this.frontierDown, end)
      this.frontierUp = Math.min(this.frontierUp, start)
    } catch (_) {
      await skeletonRevealed
      this.loadError = "Failed to load emails. Scroll again to retry."
    } finally {
      this.loadingDirection = null
      this.pendingLoadStart = null
      this.pendingLoadEnd = null
      this.activeChunkFetches.delete(chunkKey)
      this.hideEdgeSkeleton()
      this.updateEffectiveCount()
    }
  }

  now() {
    return typeof performance !== "undefined" && performance.now ? performance.now() : Date.now()
  }

  waitForViewSwitchPending(startedAt) {
    var elapsed = this.now() - startedAt
    var remaining = 130 - elapsed
    if (remaining <= 0) return Promise.resolve()
    return new Promise(function (resolve) { setTimeout(resolve, remaining) })
  }

  waitForSkeletonReveal(startedAt, direction) {
    var self = this
    return new Promise(function (resolve) {
      var afterPaint = function () {
        self.pinPendingSkeleton(direction)
        var elapsed = self.now() - startedAt
        var remaining = self.loadingSkeletonMinDuration - elapsed
        if (remaining > 0) {
          setTimeout(function () {
            self.pinPendingSkeleton(direction)
            resolve()
          }, remaining)
        } else resolve()
      }
      self.pinPendingSkeleton(direction)
      if (typeof requestAnimationFrame !== "function") {
        setTimeout(afterPaint, 16)
        return
      }
      requestAnimationFrame(function () {
        self.pinPendingSkeleton(direction)
        requestAnimationFrame(afterPaint)
      })
    })
  }

  pinPendingSkeleton(direction) {
    if (!this.container) return
    if (direction === "up") {
      this.container.scrollTop = 0
      return
    }
    if (direction === "down") {
      var maxScroll = Math.max(0, this.container.scrollHeight - this.container.clientHeight)
      if (this.container.scrollTop < maxScroll) this.container.scrollTop = maxScroll
    }
  }

  getLoadedMin() {
    if (this.loadedRanges.length === 0) return 0
    return this.loadedRanges[0].start
  }

  getLoadedMax() {
    if (this.loadedRanges.length === 0) return -1
    return this.loadedRanges[this.loadedRanges.length - 1].end
  }

  updateFrontiers() {
    var range = this.currentLoadedRange()
    if (!range) {
      this.frontierUp = 0
      this.frontierDown = -1
      return
    }
    this.frontierUp = range.start
    this.frontierDown = range.end
  }

  currentLoadedRange() {
    if (this.loadedRanges.length === 0) return null
    var anchor = this.windowStart
    if (this.effectiveCount > 0 && this.container) {
      anchor = this.positionAtOffset(this.container.scrollTop)
    }
    for (var i = 0; i < this.loadedRanges.length; i++) {
      var range = this.loadedRanges[i]
      if (anchor >= range.start && anchor <= range.end) return range
    }
    var windowEnd = this.windowStart + Math.max(0, this.effectiveCount - 1)
    for (var j = 0; j < this.loadedRanges.length; j++) {
      var current = this.loadedRanges[j]
      if ((this.windowStart >= current.start && this.windowStart <= current.end) || (windowEnd >= current.start && windowEnd <= current.end)) return current
    }
    return this.loadedRanges[0]
  }

  evictFarChunks(first, last) {
    if (this.navigationMode === "pagination") return false
    var firstChunk = Math.floor(first / this.chunkSize)
    var lastChunk = Math.floor(last / this.chunkSize)
    var keepMin = Math.max(0, (firstChunk - this.chunkBuffer) * this.chunkSize)
    var keepMax = Math.min(this.totalCount - 1, ((lastChunk + this.chunkBuffer + 1) * this.chunkSize) - 1)
    var keys = Array.from(this.cache.keys())
    var evicted = false
    for (var i = 0; i < keys.length; i++) {
      var pos = keys[i]
      if (pos < keepMin || pos > keepMax) {
        var item = this.cache.get(pos)
        if (item && item.id) this.indexById.delete(item.id)
        this.cache.delete(pos)
        evicted = true
      }
    }
    if (evicted) this.invalidateLoadedRanges({ preserveScroll: true })
    return evicted
  }

  async fetchRange(start, end) {
    var key = "range-" + this.viewMode + "-" + start + "-" + end
    if (this.activeFetches.has(key)) return
    this.activeFetches.add(key)
    try {
      var url =
        "/mail/folder/" +
        this.folderID +
        "/items?start=" +
        start +
        "&limit=" +
        (end - start + 1) +
        "&view=" +
        encodeURIComponent(this.viewMode)
      if (this.selectedEmailId) {
        url += "&selected=" + encodeURIComponent(this.selectedEmailId)
      }
      url = this.withFilterParams(url)
      var html = await this.fetchHTML(url)
      this.ingestHTML(html)
      this.prevFirst = null
      this.prevLast = null
      this.render()
    } catch (_) {
    } finally {
      this.activeFetches.delete(key)
    }
  }

  async prefetchSequential(last) {
    if (this.navigationMode === "pagination") return
    if (this.cache.size === 0) return
    if (last < this.cache.size - 30) return
    if (!this.hasMore || this.isLoading) return
    this.isLoading = true
    try {
      var params = "limit=" + this.mailListFetchLimit()
      if (this.searchScope) {
        params += "&start=" + (this.frontierDown + 1)
      } else if (this.nextCursor) {
        params += "&after=" + encodeURIComponent(this.nextCursor)
      }
      if (this.selectedEmailId) {
        params += "&selected=" + encodeURIComponent(this.selectedEmailId)
      }
      var url = "/mail/folder/" + this.folderID + "/items?" + params + "&view=" + encodeURIComponent(this.viewMode)
      url = this.withFilterParams(url)
      var html = await this.fetchHTML(url)
      this.ingestHTML(html)
      this.prevFirst = null
      this.prevLast = null
      this.render()
    } catch (_) {
    } finally {
      this.isLoading = false
    }
  }

  maybeShiftWindow() {}

  shiftWindowTo() {}

  prefetchWindowNeighbors() {}

  async prefetchWindowRange() {}

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
    var viewMode = wrapper.dataset.viewMode === "table" ? "table" : "cards"

    var tc = parseInt(wrapper.dataset.totalCount)
    if (!isNaN(tc)) this.totalCount = tc
    var displayTotal = parseInt(wrapper.dataset.displayTotalCount)
    if (!isNaN(displayTotal)) this.displayTotalCount = displayTotal
    else if (!isNaN(tc)) this.displayTotalCount = tc

    if (wrapper.dataset.nextCursor) {
      this.nextCursor = wrapper.dataset.nextCursor
    }
    if (wrapper.dataset.folderId) {
      this.folderID = wrapper.dataset.folderId
    }
    if (wrapper.dataset.hasMore !== undefined) {
      this.hasMore = wrapper.dataset.hasMore === "true"
    }

    if (this.navigationMode === "pagination") {
      this.ingestPaginationHTML(wrapper, viewMode)
      return
    }

    var items = wrapper.querySelectorAll(".mail-list-item[data-email-id]")
    for (var i = 0; i < items.length; i++) {
      var el = items[i]
      var pos = parseInt(el.dataset.position)
      var id = el.dataset.emailId
      if (isNaN(pos) || !id) continue
      this.setCachedRow(pos, id, el.outerHTML, viewMode)
    }

    var start = parseInt(wrapper.dataset.windowStart)
    var end = parseInt(wrapper.dataset.windowEnd)
    if (viewMode === this.viewMode && !isNaN(start) && !isNaN(end) && end >= start) {
      this.addLoadedRange(start, end)
      if (!wrapper.hasAttribute("data-local-view-switch")) this.queueOtherViewRows(start, end)
    }
    if (viewMode === this.viewMode) {
      this.updateFrontiers()
      this.updateEffectiveCount({ preserveScroll: this.loadingDirection === "up" })
    }
  }

  otherViewMode() {
    return this.viewMode === "table" ? "cards" : "table"
  }

  dropOtherViewRows() {
    this.otherViewRows.clear()
    this.otherViewGeneration++
    this.otherViewPending = []
    if (this.otherViewTimer) clearTimeout(this.otherViewTimer)
    this.otherViewTimer = null
  }

  // queueOtherViewRows fetches a range just loaded again in the other layout
  // once the page is idle, so a switch finds it ready. Ranges loaded close
  // together go in one quiet batch.
  queueOtherViewRows(start, end) {
    if (this.navigationMode === "pagination") return
    this.otherViewPending.push([start, end])
    if (this.otherViewTimer) return
    var list = this
    this.otherViewTimer = setTimeout(function () {
      list.otherViewTimer = null
      var idle = window.requestIdleCallback || function (fn) { return setTimeout(fn, 0) }
      idle(function () { list.fetchOtherViewRows() })
    }, 400)
  }

  async fetchOtherViewRows() {
    var ranges = this.otherViewPending
    this.otherViewPending = []
    var generation = this.otherViewGeneration
    var mode = this.otherViewMode()
    for (var i = 0; i < ranges.length; i++) {
      var start = ranges[i][0]
      var end = ranges[i][1]
      try {
        var html = await this.fetchHTML(this.itemsURLForView(mode, start, end - start + 1))
        if (generation !== this.otherViewGeneration || mode !== this.otherViewMode()) return
        var template = document.createElement("template")
        template.innerHTML = html
        var rows = template.content.querySelectorAll(".mail-list-item[data-email-id]")
        for (var j = 0; j < rows.length; j++) this.otherViewRows.set(mode + ":" + rows[j].dataset.emailId, rows[j].outerHTML)
      } catch (_) {
        // A switch then asks the server, as before.
      }
    }
  }

  // otherViewReply builds, from the other layout's stored rows, the reply the
  // server would give for the rows from rangeStart to rangeEnd, as one reply
  // for each run of them stored, or null when the rows on screen, first to
  // last, aren't all stored. Rows further away load from the server as they
  // used to, so none shows a state older than the refresh that follows. The
  // current layout's rows are all kept for switching back.
  otherViewReply(viewMode, first, last, rangeStart, rangeEnd) {
    for (var pos = first; pos <= last; pos++) {
      var item = this.cache.get(pos)
      if (!item || !item.id || !this.otherViewRows.has(viewMode + ":" + item.id)) return null
    }
    var positions = Array.from(this.cache.keys()).sort(function (a, b) { return a - b })
    var replies = []
    var run = null
    var template = document.createElement("template")
    for (var i = 0; i < positions.length; i++) {
      var p = positions[i]
      var cached = this.cache.get(p)
      var stored = cached && cached.id ? this.otherViewRows.get(viewMode + ":" + cached.id) : null
      if (cached && cached.id && cached.html) this.otherViewRows.set(this.viewMode + ":" + cached.id, cached.html)
      if (!stored || p < rangeStart || p > rangeEnd) {
        run = null
        continue
      }
      template.innerHTML = stored
      var row = template.content.firstElementChild
      if (!row) continue
      row.dataset.position = String(p)
      if (!run || run.end !== p - 1) {
        run = { start: p, end: p, rows: [] }
        replies.push(run)
      }
      run.end = p
      run.rows.push(row.outerHTML)
    }
    var list = this
    return replies.map(function (r) {
      return '<div data-local-view-switch data-view-mode="' + viewMode + '" data-total-count="' + list.totalCount +
        '" data-display-total-count="' + list.displayTotalCount + '" data-window-start="' + r.start + '" data-window-end="' + r.end + '">' +
        r.rows.join("") + "</div>"
    })
  }

  updateEffectiveCount(options) {
    options = options || {}
    var anchorPos = null
    var anchorOffset = 0
    if (options.preserveScroll && this.effectiveCount > 0 && this.container) {
      anchorPos = this.positionAtOffset(this.container.scrollTop)
      anchorOffset = Math.max(0, this.container.scrollTop - this.offsetAtPosition(anchorPos))
    }
    var activeRange = this.currentLoadedRange()
    if (!activeRange) {
      this.windowStart = 0
      this.effectiveCount = 0
      this.frontierUp = 0
      this.frontierDown = -1
      this.invalidateOffsets()
      return
    }
    this.frontierUp = activeRange.start
    this.frontierDown = activeRange.end
    var nextStart = Math.max(0, activeRange.start)
    var nextEnd = Math.min(this.totalCount - 1, activeRange.end)
    if (this.loadingDirection === "up" && this.pendingLoadStart !== null) {
      nextStart = Math.max(0, Math.min(nextStart, this.pendingLoadStart))
    }
    if (this.loadingDirection === "down" && this.pendingLoadEnd !== null) {
      nextEnd = Math.min(this.totalCount - 1, Math.max(nextEnd, this.pendingLoadEnd))
    }
    var next = nextEnd >= nextStart ? nextEnd - nextStart + 1 : 0
    if (next !== this.effectiveCount || nextStart !== this.windowStart) {
      this.windowStart = nextStart
      this.effectiveCount = next
      this.invalidateOffsets()
      if (anchorPos !== null) {
        this.scrollToOffset(this.offsetAtPosition(anchorPos) + anchorOffset)
      }
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
    if (this.loadedRanges.length === 0) return
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

  invalidateLoadedRanges(options) {
    this.loadedRanges = []
    var entries = Array.from(this.cache.entries())
    for (var i = 0; i < entries.length; i++) {
      this.loadedRanges.push({ start: entries[i][0], end: entries[i][0] })
    }
    this.mergeRanges()
    this.updateFrontiers()
    this.updateEffectiveCount(options)
  }

  ingestPaginationHTML(wrapper, viewMode) {
    if (viewMode !== this.viewMode) {
      this.setViewMode(viewMode, false)
    } else {
      this.cache.clear()
      this.indexById.clear()
      this.loadedRanges = []
      this.expandedThreads.clear()
      this.rowPool = []
      this.visibleRows.clear()
      this.rowByIndex.clear()
      if (this.itemsContainer) this.itemsContainer.innerHTML = ""
      this.invalidateOffsets()
    }

    var start = parseInt(wrapper.dataset.windowStart)
    this.pageStart = isNaN(start) ? 0 : start
    this.windowStart = this.pageStart
    this.container.dataset.windowStart = String(this.pageStart)
    this.container.dataset.totalCount = String(this.totalCount)
    this.container.dataset.displayTotalCount = String(this.displayTotalCount)
    this.container.dataset.viewMode = this.viewMode
    this.container.dataset.navigationMode = "pagination"
    if (this.root) this.root.dataset.mailListView = this.viewMode

    var items = wrapper.querySelectorAll(".mail-list-item[data-email-id]")
    for (var i = 0; i < items.length; i++) {
      var el = items[i]
      var pos = parseInt(el.dataset.position)
      var id = el.dataset.emailId
      if (isNaN(pos) || !id) continue
      this.setCachedRow(pos, id, el.outerHTML, viewMode)
    }

    this.effectiveCount = items.length
    if (items.length > 0) {
      this.frontierUp = this.pageStart
      this.frontierDown = this.pageStart + items.length - 1
      this.addLoadedRange(this.frontierUp, this.frontierDown)
    } else {
      this.frontierUp = this.pageStart
      this.frontierDown = this.pageStart - 1
    }
    this.invalidateOffsets()
    this.prevFirst = null
    this.prevLast = null
    this.updateHeader()
    this.updatePaginationControls()
  }

  clampPageStart(start) {
    start = parseInt(start)
    if (isNaN(start) || start < 0) start = 0
    if (this.totalCount <= 0) return 0
    var maxStart = Math.floor((this.totalCount - 1) / this.pageSize) * this.pageSize
    return Math.max(0, Math.min(start, maxStart))
  }

  firstEmailId() {
    for (var pos = this.windowStart; pos < this.windowStart + this.effectiveCount; pos++) {
      var item = this.cache.get(pos)
      if (item && item.id) return item.id
    }
    return null
  }

  updatePaginationControls() {
    if (this.navigationMode !== "pagination") return
    var pagination = this.root && this.root.querySelector("[data-mail-pagination]")
    if (!pagination) return
    var pageSize = parseInt(pagination.dataset.pageSize)
    if (!isNaN(pageSize) && pageSize > 0) this.pageSize = pageSize
    var itemCount = this.effectiveCount
    var totalPages = Math.max(1, Math.ceil(this.totalCount / this.pageSize))
    var currentPage = this.totalCount > 0 ? Math.floor(this.pageStart / this.pageSize) + 1 : 1
    var summary = pagination.querySelector("[data-mail-pagination-summary]")
    var page = pagination.querySelector("[data-mail-pagination-page]")
    var prev = pagination.querySelector("[data-mail-page-prev]")
    var next = pagination.querySelector("[data-mail-page-next]")
    if (summary) summary.textContent = itemCount > 0 ? ((this.pageStart + 1) + "-" + Math.min(this.totalCount, this.pageStart + itemCount) + " of " + this.totalCount) : "No messages"
    if (page) page.textContent = currentPage + " / " + totalPages
    if (prev) prev.disabled = this.isLoading || this.pageStart <= 0
    if (next) next.disabled = this.isLoading || itemCount === 0 || this.pageStart + itemCount >= this.totalCount
  }

  setPaginationLoading(loading) {
    this.isLoading = !!loading
    this.container.dataset.pageLoading = loading ? "true" : "false"
    this.updatePaginationControls()
  }

  captureScrollAnchor() {
    if (!this.container || !this.itemsContainer) return null
    var containerRect = this.container.getBoundingClientRect()
    var rows = this.itemsContainer.querySelectorAll(".mail-list-item[data-email-id]")
    for (var i = 0; i < rows.length; i++) {
      var rect = rows[i].getBoundingClientRect()
      if (rect.bottom <= containerRect.top) continue
      return {
        id: rows[i].dataset.emailId,
        offset: rect.top - containerRect.top,
        scrollTop: this.container.scrollTop,
      }
    }
    return { id: "", offset: 0, scrollTop: this.container.scrollTop }
  }

  restoreScrollAnchor(anchor) {
    if (!anchor || !this.container || !this.itemsContainer) return
    if (!anchor.id) {
      this.container.scrollTop = anchor.scrollTop || 0
      return
    }
    var row = this.itemsContainer.querySelector('[data-email-id="' + this.cssEscape(anchor.id) + '"]')
    if (!row) {
      this.container.scrollTop = anchor.scrollTop || 0
      return
    }
    var containerRect = this.container.getBoundingClientRect()
    var rect = row.getBoundingClientRect()
    this.container.scrollTop += rect.top - containerRect.top - anchor.offset
  }

  cssEscape(value) {
    if (window.CSS && CSS.escape) return CSS.escape(value)
    return String(value).replace(/"/g, '\\"')
  }

  async loadPage(start, options) {
    if (this.navigationMode !== "pagination") return
    options = options || {}
    start = this.clampPageStart(start)
    var targetViewMode = options.viewMode === "table" ? "table" : (options.viewMode === "cards" ? "cards" : this.viewMode)
    var transition = options.noAnimation ? null : this.captureListTransition()
    var scrollAnchor = options.preserveScroll ? this.captureScrollAnchor() : null
    var expandedSnapshot = options.preserveExpandedThreads ? this.captureExpandedThreads() : null
    var selected = options.preserveSelection ? this.selectedEmailId : null
    this.setPaginationLoading(true)
    try {
      var html = await this.fetchHTML(this.itemsURLForView(targetViewMode, start, this.pageSize, { selected: selected, knownTotal: options.knownTotal !== false }))
      if (options.pendingStartedAt !== undefined) await this.waitForViewSwitchPending(options.pendingStartedAt)
      this.ingestHTML(html)
      if (expandedSnapshot) this.restoreExpandedThreads(expandedSnapshot)
      if (options.preserveScroll) this.restoreScrollAnchor(scrollAnchor)
      else this.container.scrollTop = 0
      if (selected && this.indexById.has(selected)) this.selectedEmailId = selected
      else if (!options.keepSelection) this.selectedEmailId = null
      this.prevFirst = null
      this.prevLast = null
      this.render()
      this.syncSelectionClasses(this.itemsContainer)
      if (typeof htmx !== "undefined") htmx.process(this.itemsContainer)
      if (typeof window.applyMailTableColumnSettings === "function") window.applyMailTableColumnSettings(this.container)
      if (typeof window.applyMailCardFieldSettings === "function") window.applyMailCardFieldSettings(this.container)
      this.setPaginationLoading(false)
      if (options.clearViewSwitchPending) this.setViewSwitchPending(false)
      if (transition) this.animateListTransition(transition, options.animation || {})
      else if (options.animateInitial) this.animateRenderedRows({ enterFrom: -8 })
      if (options.loadSelected !== false && this.selectedEmailId && this.selectedEmailId !== selected && typeof htmx !== "undefined") {
        if (typeof showMailViewLoading === "function") showMailViewLoading()
        htmx.ajax("GET", typeof mailViewRequestURL === "function" ? mailViewRequestURL(this.selectedEmailId) : "/email/" + this.selectedEmailId, "#mail-view")
      }
    } finally {
      this.setPaginationLoading(false)
    }
  }

  hydrateFromDOM(options) {
    options = options || {}
    var scrollEl =
      document.getElementById("mail-list-scroll") || this.container
    this.navigationMode = scrollEl.dataset.navigationMode === "pagination" ? "pagination" : "infinite"
    this.container.dataset.navigationMode = this.navigationMode
    var pageSize = parseInt(scrollEl.dataset.pageSize)
    if (!isNaN(pageSize) && pageSize > 0) this.pageSize = pageSize
    var pageStart = parseInt(scrollEl.dataset.windowStart)
    this.pageStart = isNaN(pageStart) ? 0 : pageStart
    this.setViewMode(scrollEl.dataset.viewMode || this.viewMode, true)
    var totalCount = parseInt(scrollEl.dataset.totalCount)
    if (!isNaN(totalCount)) this.totalCount = totalCount
    var displayTotalCount = parseInt(scrollEl.dataset.displayTotalCount)
    if (!isNaN(displayTotalCount)) this.displayTotalCount = displayTotalCount
    else if (!isNaN(totalCount)) this.displayTotalCount = totalCount
    if (scrollEl.dataset.folderId) {
      this.folderID = scrollEl.dataset.folderId
    }
    this.syncFilterInputs()

    var items = scrollEl.querySelectorAll(".mail-list-item[data-email-id]")
    for (var i = 0; i < items.length; i++) {
      var el = items[i]
      var pos = parseInt(el.dataset.position)
      var id = el.dataset.emailId
      if (isNaN(pos) || !id) continue
      this.setCachedRow(pos, id, el.outerHTML, this.viewMode)
    }

    if (this.cache.size > 0) {
      var positions = Array.from(this.cache.keys())
      this.frontierUp = Math.min.apply(null, positions)
      this.frontierDown = Math.max.apply(null, positions)
      if (this.navigationMode === "pagination") {
        this.pageStart = isNaN(pageStart) ? this.frontierUp : pageStart
        this.windowStart = this.pageStart
        this.effectiveCount = this.cache.size
        this.loadedRanges = [{ start: this.windowStart, end: this.windowStart + this.effectiveCount - 1 }]
        this.frontierUp = this.windowStart
        this.frontierDown = this.windowStart + this.effectiveCount - 1
        this.invalidateOffsets()
      } else {
        this.addLoadedRange(
          this.frontierUp,
          this.frontierDown
        )
        this.queueOtherViewRows(this.frontierUp, this.frontierDown)
      }
    }
    if (this.navigationMode === "pagination") {
      if (this.cache.size === 0) {
        this.windowStart = this.pageStart
        this.effectiveCount = 0
        this.frontierUp = this.pageStart
        this.frontierDown = this.pageStart - 1
        this.invalidateOffsets()
      }
    } else {
      this.updateEffectiveCount()
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

    this.windowedMode = false

    this.container.removeAttribute("data-hydrate-dropin")
    this.container.innerHTML = ""
    this.spacerTop = document.createElement("div")
    this.spacerBottom = document.createElement("div")
    this.itemsContainer = document.createElement("div")
    this.itemsContainer.style.position = "relative"
    this.itemsContainer.style.minWidth = "0"
    this.itemsContainer.style.overflowAnchor = "none"
    this.appendStickySearchGroup()
    this.container.appendChild(this.spacerTop)
    this.container.appendChild(this.itemsContainer)
    this.container.appendChild(this.spacerBottom)

    if (typeof window.applyMailTableColumnSettings === "function") {
      window.applyMailTableColumnSettings(this.container)
    }
    if (typeof window.applyMailCardFieldSettings === "function") {
      window.applyMailCardFieldSettings(this.container)
    }
    this.renderTableHeader()

    // Scroll before the first render: rendering at the top would start
    // loading the rows above the open message.
    if (this.selectedEmailId) {
      var selectedPos = this.indexById.get(this.selectedEmailId)
      if (selectedPos !== undefined) this.scrollToOffset(this.offsetAtPosition(selectedPos))
    }

    this.render()
    this.updatePaginationControls()
    if (VirtualMailList.fanInNext) {
      VirtualMailList.fanInNext = false
      this.fanInRows()
    } else if (VirtualMailList.moveFrom) {
      // The list this one replaced, within a search: move rather than refill.
      var previous = VirtualMailList.moveFrom
      VirtualMailList.moveFrom = null
      document.documentElement.classList.remove("mail-list-entering")
      this.animateListTransition(previous)
    } else if (options.animate !== false) {
      this.fanInRows()
    } else {
      document.documentElement.classList.remove("mail-list-entering")
    }
  }

  renderTableHeader() {
    var existing = this.container.querySelector(".mail-list-table-header")
    if (existing) existing.remove()
    if (this.viewMode !== "table") return

    var header = document.createElement("div")
    header.className = "mail-list-table-header mail-list-table-grid grid items-center gap-3 px-3 py-1.5 text-[10px] font-semibold uppercase tracking-wider text-muted-foreground bg-card/95 border-b border-border/70 sticky top-0 z-20 backdrop-blur-sm"
    header.style.opacity = "0"
    header.style.transform = "translateY(-4px)"
    header.style.transition = "opacity 140ms ease-out, transform 140ms ease-out"
    header.innerHTML = this.tableHeaderHTML()
    this.container.insertBefore(header, this.spacerTop)
    requestAnimationFrame(function () {
      header.style.opacity = "1"
      header.style.transform = "translateY(0)"
    })
  }

  setViewSwitchPending(pending) {
    if (!this.itemsContainer) return
    this.container.dataset.viewSwitchPending = pending ? "true" : "false"
    this.itemsContainer.style.transition = "opacity 220ms ease-out, transform 220ms ease-out, filter 220ms ease-out"
    if (pending) {
      this.itemsContainer.style.opacity = "0.72"
      this.itemsContainer.style.filter = "saturate(0.96)"
      return
    }
    this.itemsContainer.style.opacity = ""
    this.itemsContainer.style.transform = ""
    this.itemsContainer.style.filter = ""
    var container = this.itemsContainer
    setTimeout(function () {
      if (container.style.opacity === "") container.style.transition = ""
    }, 260)
  }

  tableHeaderHTML() {
    var accountMarkerStyle = String(this.container.dataset.accountMarkerStyle || "background-color: #8b5cf6").replace(/[&<>"]/g, function (ch) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[ch]
    })
    return '<div class="mail-list-table-heading flex items-center justify-center" data-mail-table-column="0" data-mail-table-column-id="accountMarker" data-mail-table-cell="accountMarker" title="Account Marker"><span class="account-color-marker size-2.5" style="' + accountMarkerStyle + '"></span><span class="mail-list-column-separator"></span></div>' +
      '<div class="mail-list-table-heading text-center" data-mail-table-column="1" data-mail-table-column-id="starred" data-mail-table-cell="starred" title="Starred"><svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" data-lucide="star" class="size-3 mx-auto"><path d="M11.525 2.295a.53.53 0 0 1 .95 0l2.31 4.679a2.12 2.12 0 0 0 1.595 1.16l5.166.751a.53.53 0 0 1 .294.904l-3.736 3.643a2.12 2.12 0 0 0-.611 1.878l.882 5.14a.53.53 0 0 1-.771.56l-4.618-2.428a2.12 2.12 0 0 0-1.973 0L6.396 21.01a.53.53 0 0 1-.77-.56l.881-5.139a2.12 2.12 0 0 0-.611-1.879L2.16 9.795a.53.53 0 0 1 .294-.906l5.165-.75a2.12 2.12 0 0 0 1.596-1.16z"/></svg><span class="mail-list-column-separator"></span></div>' +
      '<div class="mail-list-table-heading text-center" data-mail-table-column="2" data-mail-table-column-id="attachment" data-mail-table-cell="attachment" title="Attachment"><svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" data-lucide="paperclip" class="size-3 mx-auto"><path d="m16 6-8.414 8.586a2 2 0 0 0 2.829 2.829l8.414-8.586a4 4 0 1 0-5.657-5.657l-8.379 8.551a6 6 0 1 0 8.485 8.485l8.379-8.551"/></svg><span class="mail-list-column-separator"></span></div>' +
      '<div class="mail-list-table-heading flex items-center justify-center" data-mail-table-column="3" data-mail-table-column-id="thread" data-mail-table-cell="thread" title="Thread"><svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" data-lucide="messages-square" class="size-3"><path d="M14 9a2 2 0 0 1-2 2H6l-4 4V4a2 2 0 0 1 2-2h8a2 2 0 0 1 2 2z"></path><path d="M18 9h2a2 2 0 0 1 2 2v11l-4-4h-6a2 2 0 0 1-2-2v-1"></path></svg><span class="mail-list-column-separator"></span></div>' +
      '<div class="mail-list-table-heading pl-1" data-mail-table-column="4" data-mail-table-column-id="from" data-mail-table-cell="from">From<span class="mail-list-column-resize" data-mail-table-resize="4"></span></div>' +
      '<div class="mail-list-table-heading" data-mail-table-column="5" data-mail-table-column-id="to" data-mail-table-cell="to">To<span class="mail-list-column-resize" data-mail-table-resize="5"></span></div>' +
      '<div class="mail-list-table-heading" data-mail-table-column="6" data-mail-table-column-id="subject" data-mail-table-cell="subject">Subject<span class="mail-list-column-resize" data-mail-table-resize="6"></span></div>' +
      '<div class="mail-list-table-heading min-w-12 text-right" data-mail-table-column="7" data-mail-table-column-id="date" data-mail-table-cell="date">Date</div>'
  }

  async switchFolder(folderID, pushState) {
    if (folderID !== "search") this.searchScope = null
    if (window.GoferMailSearch) window.GoferMailSearch.sync()
    if (pushState === undefined) pushState = true
    if (this.navigationMode === "pagination") {
      this.folderID = folderID
      this.selectedEmailId = null
      if (typeof setMailViewEmpty === "function") setMailViewEmpty()
      this.pageStart = 0
      await this.loadPage(0, { loadSelected: false, knownTotal: false, animation: { enterFrom: -8, exitTo: 14 } })
      this.updateHeader()
      this.updateSyncHeader()
      if (pushState) this.pushUrl()
      return
    }
    var transition = this.captureListTransition()
    this.selectedEmailId = null
    if (typeof setMailViewEmpty === "function") setMailViewEmpty()
    var params = "limit=" + this.mailListFetchLimit()
    var url = "/mail/folder/" + folderID + "/items?" + params
    url += "&view=" + encodeURIComponent(this.viewMode)
    url = this.withFilterParams(url)
    var html = await this.fetchHTML(url)

    this.reset()
    this.folderID = folderID
    this.ingestHTML(html)

    this.render()
    if (transition) this.animateListTransition(transition, { enterFrom: -8, exitTo: 14 })
    else this.animateRenderedRows({ enterFrom: -8 })
    this.updateHeader()
    this.updateSyncHeader()
    if (pushState) this.pushUrl()
  }

  async refreshCurrentFolder(options) {
    options = options || {}
    if (this.refreshInFlight) {
      this.refreshQueued = true
      this.refreshQueuedOptions = this.mergeRefreshOptions(this.refreshQueuedOptions, options)
      return this.refreshInFlight
    }

    var self = this
    this.refreshInFlight = (async function () {
      if (self.navigationMode === "pagination") {
        await self.loadPage(self.pageStart, { preserveSelection: true, preserveExpandedThreads: true, loadSelected: false, preserveScroll: true, knownTotal: false, noAnimation: !!options.noAnimation, animation: { enterFrom: -12, exitTo: 12 } })
        self.updateHeader()
        self.updateSyncHeader()
        return
      }
      var transition = options.noAnimation ? null : self.captureListTransition()
      var params = "limit=" + self.mailListFetchLimit()
      if (self.selectedEmailId) {
        params += "&selected=" + encodeURIComponent(self.selectedEmailId)
      }
      var url = "/mail/folder/" + self.folderID + "/items?" + params + "&view=" + encodeURIComponent(self.viewMode)
      url = self.withFilterParams(url)
      var html = await self.fetchHTML(url)
      var selected = self.selectedEmailId
      var syncState = self.syncState
      var expandedSnapshot = self.captureExpandedThreads()
      var rebaseTopWindow = !!options.rebase || self.filterCount() > 0 || self.container.scrollTop < self.itemHeight * 2
      var hardRebaseTopWindow = rebaseTopWindow && (self.windowStart !== 0 || self.frontierUp !== 0)
      if (hardRebaseTopWindow) {
        self.reset()
        self.selectedEmailId = selected
        self.syncState = syncState
      }
      self.ingestHTML(html)
      if (expandedSnapshot) self.restoreExpandedThreads(expandedSnapshot)
      self.prevFirst = null
      self.prevLast = null
      self.render()
      if (transition) self.animateListTransition(transition, { enterFrom: -12, exitTo: 12 })
      self.updateHeader()
      self.updateSyncHeader()
      self.removeBanner()
    })()

    try {
      await this.refreshInFlight
    } finally {
      this.refreshInFlight = null
      if (this.refreshQueued) {
        var queuedOptions = this.mergeRefreshOptions(options, this.refreshQueuedOptions)
        this.refreshQueued = false
        this.refreshQueuedOptions = null
        this.refreshCurrentFolder(queuedOptions)
      }
    }
  }

  mergeRefreshOptions(base, next) {
    base = base || {}
    next = next || {}
    return {
      noAnimation: !!(base.noAnimation || next.noAnimation),
      rebase: !!(base.rebase || next.rebase),
    }
  }

  reset() {
    this.cache.clear()
    this.indexById.clear()
    this.dropOtherViewRows()
    this.loadedRanges = []
    this.totalCount = 0
    this.displayTotalCount = 0
    this.effectiveCount = 0
    this.windowStart = 0
    this.selectedEmailId = null
    this.nextCursor = null
    this.hasMore = true
    this.isLoading = false
    this.newEmailCount = 0
    this.syncState = { active: false, current: 0, total: 0 }
    this.activeFetches.clear()
    this.activeChunkFetches.clear()
    this.pendingLoadStart = null
    this.pendingLoadEnd = null
    this.loadingDirection = null
    this.loadError = null
    this.windowedMode = false
    this.anchorAbsoluteIndex = null
    this.suppressWindowShift = false
    this.prevFirst = null
    this.prevLast = null
    this.expandedThreads.clear()
    this.rowPool = []
    this.hideEdgeSkeleton()
    this.visibleRows.clear()
    this.rowByIndex.clear()
    if (this.itemsContainer) this.itemsContainer.innerHTML = ""
    this.invalidateOffsets()
    this.container.scrollTop = 0
    this.frontierDown = -1
    this.frontierUp = 0
    this.removeBanner()
    this.updateSyncHeader()
  }

  // Row sizes depend on the screen (phone rows are shorter, and the root text size
  // differs), so they are worked out again when the screen crosses the breakpoint.
  refreshRowMetrics() {
    var anchor = this.positionAtOffset(this.container.scrollTop)
    this.itemHeight = listRemPx(this.viewMode === "table" ? 2.75 : mailCardRowRem())
    this.subItemHeight = listRemPx(this.viewMode === "table" ? 2 : 3)
    this.invalidateOffsets()
    this.container.scrollTop = this.offsetAtPosition(anchor)
    // The visible range may be unchanged; the rows still need placing at the new size.
    this.prevFirst = null
    this.prevLast = null
    this.render()
  }

  setViewMode(viewMode, keepRows) {
    this.viewMode = viewMode === "table" ? "table" : "cards"
    this.itemHeight = listRemPx(this.viewMode === "table" ? 2.75 : mailCardRowRem())
    this.subItemHeight = listRemPx(this.viewMode === "table" ? 2 : 3)
    this.container.dataset.viewMode = this.viewMode
    var mailList = document.getElementById("mail-list")
    if (mailList) mailList.dataset.mailListView = this.viewMode
    if (this.viewMode === "table" && typeof window.applyMailTableColumnSettings === "function") {
      window.applyMailTableColumnSettings(this.container)
    }
    if (this.viewMode === "cards" && typeof window.applyMailCardFieldSettings === "function") {
      window.applyMailCardFieldSettings(this.container)
    }
    this.renderTableHeader()
    this.invalidateOffsets()
    if (!keepRows) {
      this.cache.clear()
      this.indexById.clear()
      this.loadedRanges = []
      this.frontierDown = -1
      this.frontierUp = 0
      this.effectiveCount = 0
      this.windowStart = 0
      this.pendingLoadStart = null
      this.pendingLoadEnd = null
      this.loadingDirection = null
      this.loadError = null
      this.prevFirst = null
      this.prevLast = null
      this.rowPool = []
      this.hideEdgeSkeleton()
      this.visibleRows.clear()
      this.rowByIndex.clear()
      if (this.itemsContainer) this.itemsContainer.innerHTML = ""
    }
  }

  // A quiet switch (the window crossed the phone breakpoint, see app.js) hides the old
  // layout at once and fades the new rows in, without the pending state or the row
  // transition that follow the user's own switch.
  async switchViewModeQuietly(viewMode, load) {
    var items = this.itemsContainer
    if (items) {
      items.style.transition = "none"
      items.style.opacity = "0"
    }
    try {
      await load()
    } finally {
      items = this.itemsContainer
      if (items) {
        void items.offsetWidth
        items.style.transition = "opacity 140ms ease-out"
        items.style.opacity = ""
      }
    }
  }

  async switchViewMode(viewMode, options) {
    viewMode = viewMode === "table" ? "table" : "cards"
    if (viewMode === this.viewMode) return
    if (options && options.quiet) {
      var list = this
      return this.switchViewModeQuietly(viewMode, async function () {
        if (list.navigationMode === "pagination") {
          await list.loadPage(list.pageStart, { viewMode: viewMode, preserveSelection: true, loadSelected: false, preserveScroll: true, noAnimation: true })
          return
        }
        var anchorIndex = list.positionAtOffset(list.container.scrollTop)
        var rangeStart = Math.max(0, anchorIndex - list.overscan)
        var rangeEnd = Math.min(list.totalCount - 1, rangeStart + list.chunkSize - 1)
        var selected = list.selectedEmailId
        var html = await list.fetchHTML(list.itemsURLForView(viewMode, rangeStart, Math.max(1, rangeEnd - rangeStart + 1)))
        list.setViewMode(viewMode, false)
        list.ingestHTML(html)
        list.selectedEmailId = selected
        list.container.scrollTop = list.offsetAtPosition(anchorIndex)
        list.render()
      })
    }
    if (this.navigationMode === "pagination") {
      var pendingStartedAt = this.now()
      this.setViewSwitchPending(true)
      try {
        await this.loadPage(this.pageStart, {
          viewMode: viewMode,
          preserveSelection: true,
          loadSelected: false,
          preserveScroll: true,
          pendingStartedAt: pendingStartedAt,
          clearViewSwitchPending: true,
          animation: { enterFrom: -8, exitTo: 14, animateHeight: true, enterExisting: true },
        })
      } finally {
        if (this.container.dataset.viewSwitchPending === "true") this.setViewSwitchPending(false)
      }
      return
    }
    var transition = this.captureListTransition()
    var selected = this.selectedEmailId
    var oldItemHeight = this.itemHeight
    var targetItemHeight = viewMode === "table" ? 44 : 100
    var anchorIndex = this.positionAtOffset(this.container.scrollTop)
    var anchorOffset = Math.max(0, this.container.scrollTop - this.offsetAtPosition(anchorIndex))
    var anchorRatio = oldItemHeight > 0 ? Math.min(1, anchorOffset / oldItemHeight) : 0
    var viewportRows = Math.ceil(this.container.clientHeight / Math.max(1, targetItemHeight))
    var rangeStart = Math.max(0, anchorIndex - this.overscan)
    var rangeEnd = Math.min(this.totalCount - 1, rangeStart + Math.max(this.chunkSize, viewportRows + this.overscan * 2 + this.poolSlack) - 1)
    var rangeLimit = Math.max(1, rangeEnd - rangeStart + 1)
    // With the rows on screen fetched in this layout already, the switch is
    // made here, and the server is asked afterwards only to catch rows that
    // changed meanwhile, such as one just read.
    var firstShown = this.positionAtOffset(this.container.scrollTop)
    var lastShown = Math.min(this.totalCount - 1, this.positionAtOffset(this.container.scrollTop + this.container.clientHeight))
    var local = lastShown >= firstShown ? this.otherViewReply(viewMode, firstShown, lastShown, rangeStart, rangeEnd) : null
    if (local && local.length) {
      this.setViewMode(viewMode, false)
      for (var r = 0; r < local.length; r++) this.ingestHTML(local[r])
      this.selectedEmailId = selected
      this.container.scrollTop = this.offsetAtPosition(anchorIndex) + Math.round(anchorRatio * this.itemHeight)
      this.render()
      this.animateListTransition(transition, { enterFrom: -8, exitTo: 14, animateHeight: true, animateText: true })
      var list = this
      this.fetchHTML(this.itemsURLForView(viewMode, rangeStart, rangeLimit)).then(function (html) {
        if (list.viewMode !== viewMode) return
        list.ingestHTML(html)
        list.prevFirst = null
        list.prevLast = null
        list.render()
      }).catch(function () {})
      return
    }
    var pendingStart = performance.now ? performance.now() : Date.now()
    this.setViewSwitchPending(true)
    try {
      var html = await this.fetchHTML(this.itemsURLForView(viewMode, rangeStart, rangeLimit))
      var elapsed = (performance.now ? performance.now() : Date.now()) - pendingStart
      if (elapsed < 130) {
        await new Promise(function (resolve) { setTimeout(resolve, 130 - elapsed) })
      }
      this.setViewMode(viewMode, false)
      this.ingestHTML(html)
      this.selectedEmailId = selected
      this.container.scrollTop = this.offsetAtPosition(anchorIndex) + Math.round(anchorRatio * this.itemHeight)
      this.render()
      this.setViewSwitchPending(false)
      this.animateListTransition(transition, { enterFrom: -8, exitTo: 14, animateHeight: true, animateText: true })
    } catch (e) {
      this.setViewSwitchPending(false)
      throw e
    }
  }

  withFilterParams(url) {
    if (this.searchScope) url = url.replace(/^\/mail\/folder\/search\/(items|ids)/, "/mail/search/$1")
    var sep = url.indexOf("?") === -1 ? "?" : "&"
    var filters = this.filters || this.emptyFilters()
    var pairs = [
      ["unread", filters.unread ? "1" : ""],
      ["starred", filters.starred ? "1" : ""],
      ["attachments", filters.attachments ? "1" : ""],
      ["read", filters.read ? "1" : ""],
      ["no_attachments", filters.noAttachments ? "1" : ""],
      ["has_tags", filters.hasTags ? "1" : ""],
      ["no_tags", filters.noTags ? "1" : ""],
      ["threads_only", filters.threadsOnly ? "1" : ""],
      ["no_threads", filters.noThreads ? "1" : ""],
      ["participant", filters.participant || ""],
      ["from", filters.from || ""],
      ["to", filters.to || ""],
      ["recipient_type", filters.recipientType || ""],
      ["recipient_domain", filters.recipientDomain || ""],
      ["subject", filters.subject || ""],
      ["body", filters.body || ""],
      ["from_domain", filters.fromDomain || ""],
      ["attachment", filters.attachment || ""],
      ["attachment_type", filters.attachmentType || ""],
      ["attachment_extension", filters.attachmentExtension || ""],
      ["min_size_mb", filters.minSizeMB || ""],
      ["max_size_mb", filters.maxSizeMB || ""],
      ["account_id", filters.accountId || ""],
      ["q", filters.query || ""],
      ["after_date", filters.afterDate || ""],
      ["before_date", filters.beforeDate || ""],
      ["sort_by", filters.sortBy || "date"],
      ["sort_order", filters.sortOrder || "desc"],
    ]
    for (var i = 0; i < pairs.length; i++) {
      if (!pairs[i][1]) continue
      url += sep + encodeURIComponent(pairs[i][0]) + "=" + encodeURIComponent(pairs[i][1])
      sep = "&"
    }
    var tag = this.sidebarTag || this.emptySidebarTag()
    var filterTag = (filters.tag || tag.label || "").trim()
    if (filterTag) {
      url += sep + "tag=" + encodeURIComponent(filterTag)
      sep = "&"
    }
    if (tag.label === filterTag && tag.accountId) {
      url += sep + "tag_account_id=" + encodeURIComponent(tag.accountId)
      sep = "&"
    }
    if (tag.label === filterTag && tag.providerId) {
      url += sep + "tag_provider_id=" + encodeURIComponent(tag.providerId)
      sep = "&"
    }
    if (tag.label === filterTag && tag.providerId && tag.providerType) {
      url += sep + "tag_provider_type=" + encodeURIComponent(tag.providerType)
      sep = "&"
    }
    return this.searchScope && window.GoferMailSearch ? window.GoferMailSearch.scopeURL(url, this.searchScope) : url
  }

  emptyFilters() {
    return {
      unread: false,
      starred: false,
      attachments: false,
      read: false,
      noAttachments: false,
      hasTags: false,
      noTags: false,
      threadsOnly: false,
      noThreads: false,
      participant: "",
      from: "",
      to: "",
      recipientType: "",
      recipientDomain: "",
      subject: "",
      body: "",
      fromDomain: "",
      attachment: "",
      attachmentType: "",
      attachmentExtension: "",
      minSizeMB: "",
      maxSizeMB: "",
      tag: "",
      accountId: "",
      query: "",
      afterDate: "",
      beforeDate: "",
      sortBy: "date",
      sortOrder: "desc",
    }
  }

  readFiltersFromURL() {
    var filters = this.emptyFilters()
    var params = new URLSearchParams(window.location.search)
    filters.unread = params.get("unread") === "1"
    filters.starred = params.get("starred") === "1"
    filters.attachments = params.get("attachments") === "1"
    filters.read = params.get("read") === "1"
    filters.noAttachments = params.get("no_attachments") === "1"
    filters.hasTags = params.get("has_tags") === "1"
    filters.noTags = params.get("no_tags") === "1"
    filters.threadsOnly = params.get("threads_only") === "1"
    filters.noThreads = params.get("no_threads") === "1"
    filters.participant = (params.get("participant") || "").trim()
    filters.from = (params.get("from") || "").trim()
    filters.to = (params.get("to") || "").trim()
    filters.recipientType = (params.get("recipient_type") || "").trim()
    filters.recipientDomain = (params.get("recipient_domain") || "").trim()
    filters.subject = (params.get("subject") || "").trim()
    filters.body = (params.get("body") || "").trim()
    filters.fromDomain = (params.get("from_domain") || "").trim()
    filters.attachment = (params.get("attachment") || "").trim()
    filters.attachmentType = (params.get("attachment_type") || "").trim()
    filters.attachmentExtension = (params.get("attachment_extension") || "").trim()
    filters.minSizeMB = (params.get("min_size_mb") || "").trim()
    filters.maxSizeMB = (params.get("max_size_mb") || "").trim()
    filters.tag = (params.get("tag") || "").trim()
    filters.accountId = (params.get("account_id") || "").trim()
    filters.query = (params.get("q") || "").trim()
    filters.afterDate = (params.get("after_date") || "").trim()
    filters.beforeDate = (params.get("before_date") || "").trim()
    filters.sortBy = (params.get("sort_by") || "date").trim()
    filters.sortOrder = (params.get("sort_order") || "desc").trim()
    if (filters.hasTags) filters.noTags = false
    if (filters.threadsOnly) filters.noThreads = false
    if (filters.attachmentType !== "custom") filters.attachmentExtension = ""
    else filters.attachmentExtension = filters.attachmentExtension.replace(/^\./, "").toLowerCase()
    var minSize = parseFloat(filters.minSizeMB)
    var maxSize = parseFloat(filters.maxSizeMB)
    if (!isFinite(minSize) || minSize <= 0) filters.minSizeMB = ""
    if (!isFinite(maxSize) || maxSize <= 0) filters.maxSizeMB = ""
    if (filters.minSizeMB && filters.maxSizeMB && minSize > maxSize) {
      var originalMin = filters.minSizeMB
      filters.minSizeMB = filters.maxSizeMB
      filters.maxSizeMB = originalMin
    }
    return filters
  }

  emptySidebarTag() {
    return {
      label: "",
      accountId: "",
      providerId: "",
      providerType: "",
    }
  }

  readSidebarTagFromURL() {
    var params = new URLSearchParams(window.location.search)
    var label = (params.get("tag") || "").trim()
    return {
      label: label,
      accountId: label ? (params.get("tag_account_id") || "").trim() : "",
      providerId: label ? (params.get("tag_provider_id") || "").trim() : "",
      providerType: label ? (params.get("tag_provider_type") || "").trim() : "",
    }
  }

  setSidebarTag(tag) {
    tag = tag || {}
    var label = (tag.label || "").trim()
    this.sidebarTag = {
      label: label,
      accountId: label ? (tag.accountId || "").trim() : "",
      providerId: label ? (tag.providerId || "").trim() : "",
      providerType: label ? (tag.providerType || "").trim() : "",
    }
  }

  syncFilterInputs() {
    var search = document.querySelector("[data-mail-search-input]")
    if (search) {
      search.dataset.mailCommittedQuery = this.filters.query || ""
      search.value = this.filters.query || ""
    }
    if (typeof window.syncMailFilterControls === "function") window.syncMailFilterControls(this.filters || this.emptyFilters())
  }

  filterQueryString() {
    var params = new URLSearchParams()
    var filters = this.filters || this.emptyFilters()
    if (filters.unread) params.set("unread", "1")
    if (filters.starred) params.set("starred", "1")
    if (filters.attachments) params.set("attachments", "1")
    if (filters.read) params.set("read", "1")
    if (filters.noAttachments) params.set("no_attachments", "1")
    if (filters.hasTags) params.set("has_tags", "1")
    if (filters.noTags) params.set("no_tags", "1")
    if (filters.threadsOnly) params.set("threads_only", "1")
    if (filters.noThreads) params.set("no_threads", "1")
    if (filters.participant) params.set("participant", filters.participant)
    if (filters.from) params.set("from", filters.from)
    if (filters.to) params.set("to", filters.to)
    if (filters.recipientType) params.set("recipient_type", filters.recipientType)
    if (filters.recipientDomain) params.set("recipient_domain", filters.recipientDomain)
    if (filters.subject) params.set("subject", filters.subject)
    if (filters.body) params.set("body", filters.body)
    if (filters.fromDomain) params.set("from_domain", filters.fromDomain)
    if (filters.attachment) params.set("attachment", filters.attachment)
    if (filters.attachmentType) params.set("attachment_type", filters.attachmentType)
    if (filters.attachmentExtension) params.set("attachment_extension", filters.attachmentExtension)
    if (filters.minSizeMB) params.set("min_size_mb", filters.minSizeMB)
    if (filters.maxSizeMB) params.set("max_size_mb", filters.maxSizeMB)
    var tag = this.sidebarTag || this.emptySidebarTag()
    var filterTag = (filters.tag || tag.label || "").trim()
    if (filterTag) params.set("tag", filterTag)
    if (filters.accountId) params.set("account_id", filters.accountId)
    if (filters.query) params.set("q", filters.query)
    if (filters.afterDate) params.set("after_date", filters.afterDate)
    if (filters.beforeDate) params.set("before_date", filters.beforeDate)
    params.set("sort_by", filters.sortBy || "date")
    params.set("sort_order", filters.sortOrder || "desc")
    if (tag.label === filterTag && tag.accountId) params.set("tag_account_id", tag.accountId)
    if (tag.label === filterTag && tag.providerId) params.set("tag_provider_id", tag.providerId)
    if (tag.label === filterTag && tag.providerId && tag.providerType) params.set("tag_provider_type", tag.providerType)
    if (this.searchScope && window.GoferMailSearch) {
      var origin = new URLSearchParams(window.location.search).get("search_from")
      if (origin) params.set("search_from", origin)
      return window.GoferMailSearch.scopeURL("?" + params.toString(), this.searchScope).slice(1)
    }
    return params.toString()
  }

  filterCount() {
    var filters = this.filters || this.emptyFilters()
    return (filters.unread ? 1 : 0) + (filters.starred ? 1 : 0) + (filters.attachments ? 1 : 0) +
      (filters.read ? 1 : 0) + (filters.noAttachments ? 1 : 0) + (filters.hasTags ? 1 : 0) + (filters.noTags ? 1 : 0) +
      (filters.threadsOnly ? 1 : 0) + (filters.noThreads ? 1 : 0) + (filters.participant ? 1 : 0) + (filters.from ? 1 : 0) + (filters.to ? 1 : 0) +
      (filters.recipientType ? 1 : 0) + (filters.recipientDomain ? 1 : 0) + (filters.subject ? 1 : 0) + (filters.body ? 1 : 0) + (filters.fromDomain ? 1 : 0) +
      (filters.attachment ? 1 : 0) + (filters.attachmentType ? 1 : 0) + (filters.minSizeMB ? 1 : 0) + (filters.maxSizeMB ? 1 : 0) + (filters.tag ? 1 : 0) + (filters.accountId ? 1 : 0) +
      (filters.query ? 1 : 0) + (filters.afterDate ? 1 : 0) + (filters.beforeDate ? 1 : 0)
  }

  // applyFilters reloads the list for new filters. options.replace fans the
  // old rows out and then the new ones in (clearForTransition, fanInRows),
  // as starting or ending a search does; otherwise the rows move in one go
  // (animateListTransition), as a change within a search does.
  async applyFilters(filters, options) {
    var replacing = !!(options && options.replace) && !this.prefersReducedMotion()
    var next = this.emptyFilters()
    filters = filters || {}
    for (var key in next) {
      if (typeof next[key] === "boolean") next[key] = !!filters[key]
      else next[key] = (filters[key] || "").trim()
    }
    this.filters = next
    if (this.navigationMode === "pagination") {
      var previousSelectedPaginated = this.selectedEmailId
      if (replacing) {
        // A page renders as soon as it loads, so it waits for the rows to leave.
        await this.clearForTransition()
        this.releaseClearedRows()
      }
      try {
        await this.loadPage(0, replacing ? { preserveSelection: true, loadSelected: false, knownTotal: false, noAnimation: true }
          : { preserveSelection: true, loadSelected: false, knownTotal: false })
      } finally {
        if (replacing && this.itemsContainer) this.itemsContainer.style.visibility = ""
      }
      if (previousSelectedPaginated && this.indexById.has(previousSelectedPaginated)) this.selectedEmailId = previousSelectedPaginated
      else this.selectedEmailId = null
      this.prevFirst = null
      this.prevLast = null
      this.render()
      this.syncSelectionClasses(this.itemsContainer)
      this.replaceUrl()
      this.updateFilteredSelection(previousSelectedPaginated)
      if (replacing) this.fanInRows()
      return
    }
    var transition = replacing ? null : this.captureListTransition()
    var previousSelected = this.selectedEmailId
    var params = "limit=" + this.mailListFetchLimit() + "&view=" + encodeURIComponent(this.viewMode)
    if (previousSelected) params += "&selected=" + encodeURIComponent(previousSelected)
    // The old rows leave while the new ones load.
    var leaving = replacing ? this.clearForTransition() : null
    var html
    try {
      html = await this.fetchHTML(this.withFilterParams("/mail/folder/" + this.folderID + "/items?" + params))
      if (leaving) await leaving
    } catch (err) {
      if (replacing) this.cancelTransitionClear()
      throw err
    }
    if (replacing) this.releaseClearedRows()
    var syncState = this.syncState
    this.reset()
    this.syncState = syncState
    this.ingestHTML(html)
    if (previousSelected && this.indexById.has(previousSelected)) {
      this.selectedEmailId = previousSelected
    } else {
      this.selectedEmailId = null
    }
    this.render()
    if (replacing) {
      if (this.itemsContainer) this.itemsContainer.style.visibility = ""
      this.fanInRows()
    } else {
      this.animateListTransition(transition)
    }
    this.updateHeader()
    this.replaceUrl()
    this.updateFilteredSelection(previousSelected)
  }

  updateFilteredSelection(previousSelected) {
    if (this.selectedEmailId && this.selectedEmailId !== previousSelected && typeof htmx !== "undefined") {
      if (typeof showMailViewLoading === "function") showMailViewLoading()
      htmx.ajax("GET", typeof mailViewRequestURL === "function" ? mailViewRequestURL(this.selectedEmailId) : "/email/" + this.selectedEmailId, "#mail-view")
      return
    }
    if (!this.selectedEmailId) {
      if (typeof setMailViewEmpty === "function") setMailViewEmpty()
    }
  }

  onEmailSelected(emailId) {
    this.selectedEmailId = emailId
    this.syncSelectionClasses(this.itemsContainer)
    this.pushUrl()
  }

  ensureSelectionWindowed() {}

  pushUrl() {
    var path = this.searchScope ? "/mail/search" : "/folder/" + this.folderID
    if (!this.searchScope && this.selectedEmailId) {
      path += "/" + this.selectedEmailId
    }
    var query = new URLSearchParams(this.filterQueryString())
    if (this.searchScope && this.selectedEmailId) query.set("selected", this.selectedEmailId)
    if (query.toString()) path += "?" + query.toString()
    if (window.location.pathname + window.location.search !== path) {
      // fromList lets the small-screen back button return to the list with history.back().
      var fromList = !!this.selectedEmailId && !(history.state && history.state.email)
      history.pushState({ folder: this.folderID, email: this.selectedEmailId, fromList: fromList }, "", path)
    }
  }

  replaceUrl() {
    var path = this.searchScope ? "/mail/search" : "/folder/" + this.folderID
    if (!this.searchScope && this.selectedEmailId) path += "/" + this.selectedEmailId
    var query = new URLSearchParams(this.filterQueryString())
    if (this.searchScope && this.selectedEmailId) query.set("selected", this.selectedEmailId)
    if (query.toString()) path += "?" + query.toString()
    history.replaceState({ folder: this.folderID, email: this.selectedEmailId || null }, "", path)
  }

  showNewEmailBanner() {
    var self = this
    if (this.bannerEl) return
    this.bannerEl = document.createElement("div")
    this.bannerEl.className = "new-email-banner"
    this.bannerEl.textContent = this.newEmailCount + " new email" + (this.newEmailCount !== 1 ? "s" : "")
    this.bannerEl.addEventListener("click", function () {
      self.container.scrollTop = 0
      self.refreshCurrentFolder({ rebase: true }).catch(function () {})
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
    if (window.GoferMailSearch) window.GoferMailSearch.sync()
    var nameEl = document.getElementById("mail-folder-name")
    if (nameEl) {
      var link = null
      var tag = this.sidebarTag || this.emptySidebarTag()
      if (tag.label) {
        var tagLinks = document.querySelectorAll("aside a[data-sidebar-tag-filter]")
        for (var i = 0; i < tagLinks.length; i++) {
          if ((tagLinks[i].dataset.sidebarTagLabel || "") === tag.label && (tagLinks[i].dataset.sidebarTagAccount || "") === tag.accountId) {
            link = tagLinks[i]
            break
          }
        }
      }
      if (!link) {
        link = document.querySelector(
          'aside a[hx-get="/folder/' + this.folderID + '"]'
        )
      }
      if (link) {
        var span = link.querySelector("span.truncate")
        if (span) nameEl.textContent = span.textContent.trim()
      }
    }
    var countEl = document.getElementById("mail-folder-count")
    if (countEl) {
      countEl.className = "inline-flex h-5 min-w-10 items-center justify-center rounded-full bg-muted px-2 text-xs font-medium text-muted-foreground shadow-[0_1px_2px_rgba(0,0,0,0.06)]"
      countEl.textContent = String(this.displayTotalCount)
    }
    if (typeof syncMailDeleteActionState === "function") syncMailDeleteActionState()
  }

  updateSyncHeader() {
    var list = document.getElementById("mail-list")
    if (!list) return
    var row = document.getElementById("mail-sync-status")
    var rowCreated = false
    if (!row) {
      row = document.createElement("div")
      row.id = "mail-sync-status"
      row.className = "pointer-events-none absolute inset-x-0 bottom-0 z-30 translate-y-6 scale-[0.98] px-3 pb-4 opacity-0 transition-[opacity,transform] duration-300 ease-out"
      row.setAttribute("role", "status")
      row.setAttribute("aria-live", "polite")
      row.setAttribute("aria-hidden", "true")
      row.innerHTML =
        '<div class="relative overflow-hidden rounded-xl border border-border bg-card/98 px-3.5 py-3 shadow-[0_14px_38px_rgba(0,0,0,0.24),0_2px_8px_rgba(0,0,0,0.14)] ring-1 ring-black/5 dark:ring-white/10 backdrop-blur-md">' +
          '<div class="absolute inset-x-0 top-0 h-px bg-gradient-to-r from-transparent via-foreground/15 to-transparent"></div>' +
          '<div class="flex items-center justify-between gap-3">' +
            '<div class="flex min-w-0 items-center gap-2.5">' +
              '<span class="flex size-8 shrink-0 items-center justify-center rounded-lg border border-primary/25 bg-primary/10 text-primary shadow-sm">' +
                '<svg class="size-4 animate-spin" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 12a9 9 0 1 1-2.64-6.36"/><path d="M21 3v6h-6"/></svg>' +
              '</span>' +
              '<div class="min-w-0">' +
                '<div class="text-[10px] font-bold uppercase tracking-[0.12em] text-primary">Mail sync in progress</div>' +
                '<div id="mail-sync-text" class="mt-0.5 truncate text-xs font-medium text-foreground">Syncing folder</div>' +
              '</div>' +
            '</div>' +
            '<span id="mail-sync-count" class="shrink-0 rounded-md border border-primary/20 bg-primary/10 px-2 py-1 text-[11px] font-semibold tabular-nums text-primary"></span>' +
          '</div>' +
          '<div class="mt-2.5 h-2 w-full overflow-hidden rounded-full bg-primary/10 ring-1 ring-inset ring-primary/10">' +
            '<div id="mail-sync-progress" class="h-full rounded-full bg-primary transition-all duration-300 ease-out" style="width: 8%"></div>' +
          '</div>' +
        '</div>'
      var body = list.querySelector("[data-mail-list-body]")
      if (body) body.appendChild(row)
      else list.appendChild(row)
      rowCreated = true
      this.syncBannerShownAt = 0
    }

    var hide = function () {
      this.syncBannerHideTimer = null
      this.syncBannerShownAt = 0
      row.classList.add("pointer-events-none", "translate-y-6", "scale-[0.98]", "opacity-0")
      row.classList.remove("translate-y-0", "scale-100", "opacity-100")
      row.setAttribute("aria-hidden", "true")
    }.bind(this)
    if (!this.syncState || !this.syncState.active) {
      clearTimeout(this.syncBannerShowTimer)
      this.syncBannerShowTimer = null
      if (this.syncBannerHideTimer) return
      // A banner that just appeared stays briefly so it never blinks out.
      var visibleFor = this.syncBannerShownAt ? Date.now() - this.syncBannerShownAt : Infinity
      if (visibleFor < this.syncBannerMinVisible) {
        this.syncBannerHideTimer = setTimeout(hide, this.syncBannerMinVisible - visibleFor)
      } else {
        hide()
      }
      return
    }
    clearTimeout(this.syncBannerHideTimer)
    this.syncBannerHideTimer = null
    var cur = this.syncState.current || 0
    var total = this.syncState.total || 0
    var text = document.getElementById("mail-sync-text")
    var count = document.getElementById("mail-sync-count")
    var bar = document.getElementById("mail-sync-progress")
    if (text) {
      text.textContent = this.syncHeaderText()
    }
    if (count) {
      count.textContent = this.syncCountLabel()
    }
    if (bar) {
      if (!this.syncUsesIndeterminateProgress() && total > 0) {
        var pct = Math.max(4, Math.min(100, (cur / total) * 100))
        bar.style.width = pct + "%"
        bar.style.animation = "none"
        bar.style.transform = "translateX(0)"
      } else {
        bar.style.width = "35%"
        bar.style.animation = "mailSyncIndeterminate 1.2s ease-in-out infinite"
        bar.style.transform = ""
      }
    }
    if (this.syncBannerShownAt) return
    var show = function () {
      this.syncBannerShowTimer = null
      if (!this.syncState || !this.syncState.active || !row.isConnected) return
      this.syncBannerShownAt = Date.now()
      row.classList.remove("pointer-events-none", "translate-y-6", "scale-[0.98]", "opacity-0")
      row.classList.add("translate-y-0", "scale-100", "opacity-100")
      row.setAttribute("aria-hidden", "false")
    }.bind(this)
    if (this.syncIsLargeDownload()) {
      clearTimeout(this.syncBannerShowTimer)
      this.syncBannerShowTimer = null
      if (rowCreated) window.requestAnimationFrame(show)
      else show()
      return
    }
    // Most syncs are quick refreshes that finish before this delay, so they never show the banner.
    if (!this.syncBannerShowTimer) this.syncBannerShowTimer = setTimeout(show, this.syncBannerDelay)
  }

  // A real message download with many messages left is worth showing without waiting.
  syncIsLargeDownload() {
    var state = this.syncState || {}
    if (state.refreshOnly || state.totalEstimated) return false
    var current = Number(state.current) || 0
    var total = Number(state.total) || 0
    return current > 0 && total - current >= this.syncBannerLargeRemaining
  }

  onNewEmail() {
    if (this.navigationMode === "pagination") {
      this.loadPage(0, { preserveSelection: true, preserveExpandedThreads: true, loadSelected: false, knownTotal: false, animation: { enterFrom: -12, exitTo: 12 } }).catch(function () {})
      return
    }
    if (this.container.scrollTop < this.itemHeight * 2) {
      this.removeBanner()
      this.refreshCurrentFolder({ rebase: true }).catch(function () {})
    } else {
      this.newEmailCount++
      this.showNewEmailBanner()
    }
  }

  invalidateItem(emailId) {
    if (this.navigationMode === "pagination") {
      this.loadPage(this.pageStart, { preserveSelection: true, preserveExpandedThreads: true, loadSelected: false, preserveScroll: true, animation: { enterFrom: -8, exitTo: 12 } }).catch(function () {})
      return
    }
    var pos = this.indexById.get(emailId)
    if (pos === undefined) return
    var item = this.cache.get(pos)
    if (item) {
      item.html = ""
    }

    var url = "/mail/folder/" + this.folderID + "/items?start=" + pos + "&limit=1&view=" + encodeURIComponent(this.viewMode)
    if (this.selectedEmailId) {
      url += "&selected=" + encodeURIComponent(this.selectedEmailId)
    }
    url = this.withFilterParams(url)
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

  async toggleThreadExpand(emailId, options) {
    var pos = this.indexById.get(emailId)
    if (pos === undefined) return
    // Small screens expand a thread in place without selecting (and so opening) it.
    if (!options || options.select !== false) this.onEmailSelected(emailId)
    var previousLayout = this.captureRenderedLayout()

    if (this.expandedThreads.has(emailId)) {
      if (this.viewMode === "table") {
        this.expandedThreads.delete(emailId)
        this.invalidateOffsets()
        this.prevFirst = null
        this.prevLast = null
        this.render()
        this.animateLayoutShift(previousLayout)
        return
      }

      var expanded = this.expandedThreads.get(emailId)
      expanded.entering = false
      expanded.collapsing = true
      this.prevFirst = null
      this.prevLast = null
      this.render()
      var self = this
      setTimeout(function () {
        if (!self.expandedThreads.has(emailId)) return
        var collapseLayout = self.captureRenderedLayout()
        self.expandedThreads.delete(emailId)
        self.invalidateOffsets()
        self.prevFirst = null
        self.prevLast = null
        self.render()
        self.animateLayoutShift(collapseLayout)
      }, 110)
      return
    }

    var item = this.cache.get(pos)
    if (!item) return

    try {
      var threadId = this.getThreadDataAttr(emailId)
      if (!threadId) return
      var threadURL = this.searchScope ? this.withFilterParams("/mail/search/thread/" + encodeURIComponent(emailId) + "/subitems") : "/mail/thread/" + encodeURIComponent(threadId) + "/subitems"
      var html = await this.fetchHTML(threadURL)
      var tmp = document.createElement("template")
      tmp.innerHTML = html
      var wrapper = tmp.content.firstElementChild
      if (!wrapper) return

      var subItems = wrapper.querySelectorAll("[data-sub-email-id]")
      var subHtml = ""
      var subCount = 0
      for (var i = 0; i < subItems.length; i++) {
        if (subItems[i].dataset.subEmailId === emailId) continue
        subHtml += subItems[i].outerHTML
        subCount++
      }

      this.expandedThreads.set(emailId, {
        subCount: subCount,
        html: subHtml,
        entering: true
      })
      this.invalidateOffsets()
      this.prevFirst = null
      this.prevLast = null
      this.render()
      var self = this
      if (this.viewMode === "table") {
        this.animateLayoutShift(previousLayout)
        requestAnimationFrame(function () { self.finishThreadEnter(emailId) })
      } else {
        this.animateLayoutShift(previousLayout)
        setTimeout(function () { self.finishThreadEnter(emailId) }, 130)
      }
    } catch (e) {
      console.error("Failed to expand thread:", e)
    }
  }

  getThreadDataAttr(emailId) {
    var el = this.container.querySelector('[data-email-id="' + emailId + '"]')
    if (el) return el.dataset.threadId
    var pos = this.indexById.get(emailId)
    if (pos === undefined) return null
    var item = this.cache.get(pos)
    if (!item) return null
    var tmp = document.createElement("div")
    tmp.innerHTML = this.rowHTML(item, this.viewMode)
    var node = tmp.firstElementChild
    return node ? node.dataset.threadId : null
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
      "&limit=" +
      this.selectedFetchLimit() +
      "&selected=" +
      encodeURIComponent(selectedId) +
      "&view=" +
      encodeURIComponent(this.viewMode)
    var html = await this.fetchHTML(this.withFilterParams(url))
    this.ingestHTML(html)

    var anchorPos = this.indexById.get(selectedId)
    if (anchorPos !== undefined) {
      this.container.scrollTop = this.offsetAtPosition(anchorPos)
    }

    this.render()
  }
}

class VirtualContactsList {
  constructor(container, options) {
    this.container = container
    this.viewMode = options.viewMode || container.dataset.viewMode || "cards"
    this.itemHeight = listRemPx(contactRowRem(this.viewMode))
    this.overscan = 10
    this.chunkSize = 100
    this.loadingSkeletonMinDuration = 180
    this.cache = new Map()
    this.indexById = new Map()
    this.loadedRanges = []
    this.totalCount = 0
    this.effectiveCount = 0
    this.hasMore = true
    this.selectedContactId = options.selectedContactId || null
    this.filters = this.readFiltersFromContainer()
    this.activeFetches = new Set()
    this.activeChunkFetches = new Set()
    this.loadingDirection = null
    this.pendingLoadEnd = null
    this.loadError = null
    this.frontierDown = -1
    this.frontierUp = 0
    this.spacerTop = null
    this.spacerBottom = null
    this.itemsContainer = null
    this.rowPool = []
    this.visibleRows = new Map()
    this.rowByIndex = new Map()
    this.poolSlack = 6
    this.prevFirst = null
    this.prevLast = null
    this.transitionOverlay = null
    this.container.style.overflowAnchor = "none"
    this.bindEvents()
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

  readFiltersFromContainer() {
    return {
      query: this.container.dataset.query || "",
      source: this.container.dataset.source || "",
      saveTarget: this.container.dataset.saveTarget || "",
      activity: this.container.dataset.activity || "",
      trash: this.container.dataset.trash === "1",
      sortBy: this.container.dataset.sortBy || "updated",
      sortOrder: this.container.dataset.sortOrder || "desc",
    }
  }

  hydrateFromDOM(options) {
    options = options || {}
    this.setViewMode(this.container.dataset.viewMode || this.viewMode, true)
    var totalCount = parseInt(this.container.dataset.totalCount)
    if (!isNaN(totalCount)) this.totalCount = totalCount
    this.ingestRows(this.container, true)
    var selected = this.container.querySelector(".envelope-active")
    if (selected) {
      var selectedRow = selected.closest("[data-contact-id]")
      if (selectedRow) this.selectedContactId = selectedRow.dataset.contactId
    }
    this.container.removeAttribute("data-hydrate-dropin")
    this.container.innerHTML = ""
    this.spacerTop = document.createElement("div")
    this.spacerBottom = document.createElement("div")
    this.itemsContainer = document.createElement("div")
    this.itemsContainer.style.position = "relative"
    this.itemsContainer.style.minWidth = "0"
    this.itemsContainer.style.overflowAnchor = "none"
    this.container.appendChild(this.spacerTop)
    this.container.appendChild(this.itemsContainer)
    this.container.appendChild(this.spacerBottom)
    this.renderTableHeader()
    this.render()
    if (options.animate !== false) this.animateRenderedRows({ enterFrom: -8 })
  }

  // Row sizes depend on the screen (phone rows use one shorter layout, and the root
  // text size differs), so they are worked out again when it crosses the breakpoint.
  refreshRowMetrics() {
    var anchor = Math.floor(this.container.scrollTop / Math.max(1, this.itemHeight))
    this.itemHeight = listRemPx(contactRowRem(this.viewMode))
    this.container.scrollTop = anchor * this.itemHeight
    // The visible range may be unchanged; the rows still need placing at the new size.
    this.prevFirst = null
    this.prevLast = null
    this.render()
  }

  setViewMode(viewMode, keepRows) {
    this.viewMode = viewMode === "table" ? "table" : "cards"
    this.itemHeight = listRemPx(contactRowRem(this.viewMode))
    this.container.dataset.viewMode = this.viewMode
    var shell = document.querySelector("[data-contact-list-shell]") || document.getElementById("mail-list")
    if (shell) shell.dataset.viewMode = this.viewMode
    this.renderTableHeader()
    if (!keepRows) this.resetRows()
    this.prevFirst = null
    this.prevLast = null
  }

  resetRows() {
    this.cache.clear()
    this.indexById.clear()
    this.loadedRanges = []
    this.effectiveCount = 0
    this.hasMore = true
    this.activeChunkFetches.clear()
    this.loadingDirection = null
    this.pendingLoadEnd = null
    this.loadError = null
    this.frontierDown = -1
    this.frontierUp = 0
    this.rowPool = []
    this.visibleRows.clear()
    this.rowByIndex.clear()
    this.prevFirst = null
    this.prevLast = null
    if (this.itemsContainer) this.itemsContainer.innerHTML = ""
    this.container.scrollTop = 0
  }

  renderTableHeader() {
    var existing = this.container.querySelector(".mail-list-table-header")
    if (existing) existing.remove()
    if (this.viewMode !== "table") return
    var header = document.createElement("div")
    header.className = "mail-list-table-header mail-list-table-grid grid items-center gap-3 px-3 py-1.5 text-[10px] font-semibold uppercase tracking-wider text-muted-foreground bg-card/95 border-b border-border/70 sticky top-0 z-20 backdrop-blur-sm"
    header.style.cssText = this.tableGridStyle()
    header.style.opacity = "0"
    header.style.transform = "translateY(-4px)"
    header.style.transition = "opacity 140ms ease-out, transform 140ms ease-out"
    header.innerHTML = '<div class="mail-list-table-heading">Name</div><div class="mail-list-table-heading">Origin</div><div class="mail-list-table-heading min-w-12 text-right">Msgs</div>'
    this.container.insertBefore(header, this.spacerTop || this.container.firstChild)
    requestAnimationFrame(function () {
      header.style.opacity = "1"
      header.style.transform = "translateY(0)"
    })
  }

  tableGridStyle() {
    return "--mail-list-table-columns:minmax(10rem,1.4fr) minmax(8rem,0.9fr) minmax(3.5rem,auto)"
  }

  render() {
    if (!this.itemsContainer) return
    if (this.effectiveCount === 0) {
      var stale = this.captureListTransition()
      this.spacerTop.style.height = "0px"
      this.spacerBottom.style.height = "0px"
      this.itemsContainer.style.height = "auto"
      this.itemsContainer.innerHTML = this.emptyHTML()
      this.animateExitingRows(stale ? stale.rows : null, new Set(), 180, "cubic-bezier(0.2, 0, 0, 1)", 14, 18)
      return
    }

    var scrollTop = this.container.scrollTop
    var clientHeight = this.container.clientHeight
    var first = Math.max(0, Math.floor((scrollTop - this.overscan * this.itemHeight) / this.itemHeight))
    var last = Math.min(this.effectiveCount - 1, Math.ceil((scrollTop + clientHeight + this.overscan * this.itemHeight) / this.itemHeight))
    if (first === this.prevFirst && last === this.prevLast) {
      this.maybeLoadAtEdges(first, last)
      return
    }
    this.prevFirst = first
    this.prevLast = last
    this.ensureRangeLoaded(first, last)
    this.maybeLoadAtEdges(first, last)
    this.spacerTop.style.height = "0px"
    this.spacerBottom.style.height = "0px"
    this.itemsContainer.style.height = this.totalHeight() + "px"
    this.renderPooled(first, last)
    this.syncSelectionClasses(this.itemsContainer)
    if (typeof htmx !== "undefined") htmx.process(this.itemsContainer)
  }

  totalHeight() {
    return this.effectiveCount * this.itemHeight
  }

  emptyHTML() {
    return '<div class="flex flex-col items-center justify-center py-20 px-4 text-center">' +
      '<div class="empty-icon-box size-16 rounded-2xl bg-muted/50 flex items-center justify-center mb-4 raised">' +
      '<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="size-7 text-muted-foreground/40"><path d="M15 18a3 3 0 1 0-6 0"></path><circle cx="12" cy="10" r="2"></circle><rect width="18" height="18" x="3" y="4" rx="2"></rect><path d="M8 2v4"></path><path d="M16 2v4"></path></svg>' +
      '</div><h3 class="font-semibold text-sm mb-1">No contacts yet</h3>' +
      '<p class="text-xs text-muted-foreground">Observed contacts will appear as mail is synced, or you can add one manually.</p></div>'
  }

  ensureRowPool() {
    var needed = Math.ceil(this.container.clientHeight / this.itemHeight) + this.overscan * 2 + this.poolSlack
    while (this.rowPool.length < needed) {
      var shell = document.createElement("div")
      shell.style.position = "absolute"
      shell.style.left = "0"
      shell.style.right = "0"
      // No will-change: a row on its own compositor layer loses subpixel text
      // smoothing in Chrome, and bold or colored text looked blurry.
      shell.hidden = true
      this.itemsContainer.appendChild(shell)
      this.rowPool.push(shell)
    }
  }

  acquireRow(index) {
    if (this.rowByIndex.has(index)) return this.rowByIndex.get(index)
    for (var i = 0; i < this.rowPool.length; i++) {
      var row = this.rowPool[i]
      if (!this.visibleRows.has(row)) {
        this.visibleRows.set(row, index)
        this.rowByIndex.set(index, row)
        return row
      }
    }
    return null
  }

  releaseRow(row) {
    var idx = this.visibleRows.get(row)
    if (idx !== undefined) this.rowByIndex.delete(idx)
    this.visibleRows.delete(row)
    row.hidden = true
  }

  renderPooled(first, last) {
    this.ensureRowPool()
    var entries = Array.from(this.visibleRows.entries())
    for (var i = 0; i < entries.length; i++) {
      var idx = entries[i][1]
      if (idx < first || idx > last) this.releaseRow(entries[i][0])
    }
    for (var pos = first; pos <= last; pos++) {
      var row = this.rowByIndex.has(pos) ? this.rowByIndex.get(pos) : this.acquireRow(pos)
      if (!row) continue
      this.stampRow(row, pos)
    }
  }

  stampRow(shell, index) {
    var item = this.cache.get(index)
    shell.hidden = false
    shell.style.transform = "translateY(" + (index * this.itemHeight) + "px)"
    shell.style.height = this.itemHeight + "px"
    if (!item) {
      shell.innerHTML = this.createSkeleton()
      return
    }
    shell.innerHTML = item.html
    if (typeof htmx !== "undefined" && htmx.process) htmx.process(shell)
    // Rows are redrawn from cached HTML; app.js marks the selected ones again.
    if (typeof window.markContactRowsSelected === "function") window.markContactRowsSelected(shell)
    var anchor = shell.querySelector("a")
    if (!anchor) return
    if (item.id === this.selectedContactId) {
      anchor.classList.remove("envelope")
      anchor.classList.add("envelope-active")
      anchor.dataset.active = "true"
    } else {
      anchor.classList.remove("envelope-active")
      anchor.classList.add("envelope")
      anchor.dataset.active = "false"
    }
  }

  createSkeleton() {
    if (this.viewMode === "table") {
      return '<div class="mail-list-skeleton mail-list-table-skeleton"><div class="mail-list-table-grid grid items-center gap-3 w-full px-3 py-1.5" style="' + this.tableGridStyle() + '"><div class="h-3 w-32 rounded bg-muted animate-pulse"></div><div class="h-3 w-24 rounded bg-muted animate-pulse"></div><div class="ml-auto h-3 w-8 rounded bg-muted animate-pulse"></div></div></div>'
    }
    return '<div class="mail-list-skeleton"><div class="flex items-center gap-3 px-3.5 py-1.5"><div class="size-8 rounded-full bg-muted animate-pulse"></div><div class="flex-1 min-w-0 space-y-1.5"><div class="h-3 w-28 rounded bg-muted animate-pulse"></div><div class="h-3 w-40 rounded bg-muted animate-pulse"></div></div></div></div>'
  }

  async ensureRangeLoaded(first, last) {
    if (first > last) return
    if (this.activeChunkFetches.size > 0) return
    var gaps = this.findGaps(first, last)
    for (var i = 0; i < gaps.length; i++) {
      try {
        await this.fetchRange(gaps[i].start, gaps[i].end)
      } catch (_) {}
    }
  }

  maybeLoadAtEdges(first, last) {
    if (this.activeChunkFetches.size > 0) return
    if (this.effectiveCount >= this.totalCount) return

    var viewportBottom = this.container.scrollTop + this.container.clientHeight
    if (this.frontierDown < this.totalCount - 1 && viewportBottom >= this.totalHeight() - 1) {
      this.loadChunk(Math.floor((this.frontierDown + 1) / this.chunkSize), "down")
      return
    }
    if (this.frontierUp > 0 && this.container.scrollTop <= 1) {
      this.loadChunk(Math.floor((this.frontierUp - 1) / this.chunkSize), "up")
    }
  }

  async loadChunk(chunkIndex, direction) {
    var start = direction === "down" ? this.frontierDown + 1 : chunkIndex * this.chunkSize
    if (chunkIndex < 0 || start >= this.totalCount) return
    var end = Math.min(this.totalCount - 1, start + this.chunkSize - 1)
    var chunkKey = "chunk-" + chunkIndex + "-" + this.viewMode
    if (this.activeChunkFetches.has(chunkKey)) return
    if (this.findGaps(start, end).length === 0) return
    this.activeChunkFetches.add(chunkKey)
    this.loadingDirection = direction
    this.pendingLoadEnd = direction === "down" ? start : null
    this.loadError = null
    var revealStartedAt = this.now()
    var revealPendingDownRow = direction === "down"
    this.updateEffectiveCount()
    if (revealPendingDownRow) this.container.scrollTop = this.container.scrollTop + this.itemHeight
    this.prevFirst = null
    this.prevLast = null
    this.render()
    this.pinPendingSkeleton(direction)
    var skeletonRevealed = this.waitForSkeletonReveal(revealStartedAt, direction)
    try {
      var html = await this.fetchHTML(this.itemsURL(start, end - start + 1))
      await skeletonRevealed
      this.ingestHTML(html)
      this.prevFirst = null
      this.prevLast = null
      this.render()
      this.frontierDown = Math.max(this.frontierDown, end)
      this.frontierUp = Math.min(this.frontierUp, start)
    } catch (_) {
      await skeletonRevealed
      this.loadError = "Failed to load contacts. Scroll again to retry."
    } finally {
      this.loadingDirection = null
      this.pendingLoadEnd = null
      this.activeChunkFetches.delete(chunkKey)
      this.updateEffectiveCount()
    }
  }

  now() {
    return typeof performance !== "undefined" && performance.now ? performance.now() : Date.now()
  }

  waitForSkeletonReveal(startedAt, direction) {
    var self = this
    return new Promise(function (resolve) {
      var afterPaint = function () {
        self.pinPendingSkeleton(direction)
        var elapsed = self.now() - startedAt
        var remaining = self.loadingSkeletonMinDuration - elapsed
        if (remaining > 0) {
          setTimeout(function () {
            self.pinPendingSkeleton(direction)
            resolve()
          }, remaining)
        } else resolve()
      }
      self.pinPendingSkeleton(direction)
      if (typeof requestAnimationFrame !== "function") {
        setTimeout(afterPaint, 16)
        return
      }
      requestAnimationFrame(function () {
        self.pinPendingSkeleton(direction)
        requestAnimationFrame(afterPaint)
      })
    })
  }

  pinPendingSkeleton(direction) {
    if (!this.container) return
    if (direction === "up") {
      this.container.scrollTop = 0
      return
    }
    if (direction === "down") {
      var maxScroll = Math.max(0, this.container.scrollHeight - this.container.clientHeight)
      if (this.container.scrollTop < maxScroll) this.container.scrollTop = maxScroll
    }
  }

  findGaps(first, last) {
    var gaps = []
    var pos = first
    var sorted = this.loadedRanges.slice().sort(function (a, b) { return a.start - b.start })
    for (var i = 0; i < sorted.length; i++) {
      var range = sorted[i]
      if (range.end < pos) continue
      if (range.start > last) break
      if (range.start > pos) gaps.push({ start: pos, end: Math.min(range.start - 1, last) })
      pos = Math.max(pos, range.end + 1)
    }
    if (pos <= last) gaps.push({ start: pos, end: last })
    return gaps
  }

  async fetchRange(start, end) {
    var key = start + "-" + end + "-" + this.viewMode
    if (this.activeFetches.has(key)) return
    this.activeFetches.add(key)
    var url = this.itemsURL(start, end - start + 1)
    try {
      var html = await this.fetchHTML(url)
      this.ingestHTML(html)
      this.prevFirst = null
      this.prevLast = null
      this.render()
    } finally {
      this.activeFetches.delete(key)
    }
  }

  fetchHTML(url) {
    return fetch(url, { headers: { Accept: "text/html" } }).then(function (res) {
      if (!res.ok) throw new Error("Fetch failed: " + res.status)
      return res.text()
    })
  }

  itemsURL(start, limit) {
    return this.itemsURLFor(this.viewMode, this.filters, start, limit)
  }

  itemsURLFor(viewMode, filters, start, limit) {
    var params = new URLSearchParams()
    params.set("start", String(Math.max(0, start || 0)))
    params.set("limit", String(Math.max(1, limit || this.chunkSize)))
    params.set("view", viewMode === "table" ? "table" : "cards")
    if (this.selectedContactId) params.set("selected", this.selectedContactId)
    filters = filters || this.filters
    if (filters.query) params.set("q", filters.query)
    if (filters.source) params.set("source", filters.source)
    if (filters.saveTarget) params.set("save_target", filters.saveTarget)
    if (filters.activity) params.set("activity", filters.activity)
    if (filters.trash) params.set("trash", "1")
    params.set("sort_by", filters.sortBy || "updated")
    params.set("sort_order", filters.sortOrder || "desc")
    return "/contacts/items?" + params.toString()
  }

  ingestHTML(html) {
    var template = document.createElement("template")
    template.innerHTML = html
    var wrapper = template.content.firstElementChild
    if (!wrapper) return
    var total = parseInt(wrapper.dataset.totalCount)
    if (!isNaN(total)) this.totalCount = total
    if (wrapper.dataset.hasMore !== undefined) this.hasMore = wrapper.dataset.hasMore === "true"
    this.ingestRows(wrapper, false)
    this.updateHeader()
  }

  ingestRows(root, fromDOM) {
    var items = root.querySelectorAll(".mail-list-item[data-contact-id]")
    for (var i = 0; i < items.length; i++) {
      var el = items[i]
      var pos = parseInt(el.dataset.position)
      var id = el.dataset.contactId
      if (isNaN(pos) || !id) continue
      this.cache.set(pos, { id: id, html: el.outerHTML })
      this.indexById.set(id, pos)
    }
    var start = parseInt(root.dataset.windowStart)
    var end = parseInt(root.dataset.windowEnd)
    if (!isNaN(start) && !isNaN(end) && end >= start) this.addLoadedRange(start, end)
    if (fromDOM && this.cache.size > 0 && (isNaN(start) || isNaN(end))) {
      var positions = Array.from(this.cache.keys())
      this.addLoadedRange(Math.min.apply(null, positions), Math.max.apply(null, positions))
    }
    this.updateFrontiers()
    this.updateEffectiveCount()
  }

  updateFrontiers() {
    if (this.loadedRanges.length === 0) {
      this.frontierUp = 0
      this.frontierDown = -1
      return
    }
    this.frontierUp = this.loadedRanges[0].start
    this.frontierDown = this.loadedRanges[this.loadedRanges.length - 1].end
  }

  getLoadedMax() {
    if (this.loadedRanges.length === 0) return -1
    return this.loadedRanges[this.loadedRanges.length - 1].end
  }

  updateEffectiveCount() {
    var maxLoaded = this.getLoadedMax()
    if (maxLoaded < 0) {
      this.effectiveCount = 0
      return
    }
    var next = Math.min(this.totalCount, maxLoaded + 1)
    if (this.loadingDirection === "down" && this.pendingLoadEnd !== null) {
      next = Math.min(this.totalCount, Math.max(next, this.pendingLoadEnd + 1))
    }
    this.effectiveCount = next
  }

  addLoadedRange(start, end) {
    this.loadedRanges.push({ start: start, end: end })
    this.loadedRanges.sort(function (a, b) { return a.start - b.start })
    var merged = []
    for (var i = 0; i < this.loadedRanges.length; i++) {
      var current = this.loadedRanges[i]
      var last = merged[merged.length - 1]
      if (last && current.start <= last.end + 1) last.end = Math.max(last.end, current.end)
      else merged.push({ start: current.start, end: current.end })
    }
    this.loadedRanges = merged
  }

  syncSelectionClasses(root) {
    if (!root) return
    var active = root.querySelectorAll(".envelope-active")
    for (var i = 0; i < active.length; i++) {
      active[i].classList.remove("envelope-active")
      active[i].classList.add("envelope")
      active[i].dataset.active = "false"
    }
    if (!this.selectedContactId) return
    var main = root.querySelector('[data-contact-id="' + this.cssEscape(this.selectedContactId) + '"] > a')
    if (main) {
      main.classList.remove("envelope")
      main.classList.add("envelope-active")
      main.dataset.active = "true"
    }
  }

  cssEscape(value) {
    if (window.CSS && CSS.escape) return CSS.escape(value)
    return String(value).replace(/"/g, '\\"')
  }

  onContactSelected(contactId) {
    this.selectedContactId = contactId
    this.syncSelectionClasses(this.itemsContainer)
    this.pushUrl()
  }

  async switchViewMode(viewMode, options) {
    viewMode = viewMode === "table" ? "table" : "cards"
    if (viewMode === this.viewMode) return
    var quiet = !!(options && options.quiet)
    var items = this.itemsContainer
    // A quiet switch (the window crossed the phone breakpoint) hides the old layout at
    // once and fades the new rows in, without the row transition.
    if (quiet && items) {
      items.style.transition = "none"
      items.style.opacity = "0"
    }
    var transition = quiet ? null : this.captureListTransition()
    var oldItemHeight = this.itemHeight
    var anchorIndex = Math.max(0, Math.floor(this.container.scrollTop / Math.max(1, oldItemHeight)))
    var anchorOffset = Math.max(0, this.container.scrollTop - anchorIndex * oldItemHeight)
    var anchorRatio = oldItemHeight > 0 ? Math.min(1, anchorOffset / oldItemHeight) : 0
    var rangeStart = Math.max(0, anchorIndex - this.overscan)
    var rangeEnd = Math.min(this.totalCount - 1, rangeStart + this.chunkSize - 1)
    try {
      var html = await this.fetchHTML(this.itemsURLFor(viewMode, this.filters, rangeStart, rangeEnd - rangeStart + 1))
      this.setViewMode(viewMode, false)
      this.ingestHTML(html)
      this.container.scrollTop = anchorIndex * this.itemHeight + Math.round(anchorRatio * this.itemHeight)
      this.render()
      if (!quiet) this.animateListTransition(transition, { enterFrom: -8, exitTo: 14, animateHeight: true })
      this.updateURLForState()
    } finally {
      items = this.itemsContainer
      if (quiet && items) {
        void items.offsetWidth
        items.style.transition = "opacity 140ms ease-out"
        items.style.opacity = ""
      }
    }
  }

  async applyFilters(filters) {
    var nextFilters = {
      query: (filters.query || "").trim(),
      source: (filters.source || "").trim(),
      saveTarget: (filters.saveTarget || "").trim(),
      activity: (filters.activity || "").trim(),
      trash: !!filters.trash,
      sortBy: (filters.sortBy || "updated").trim(),
      sortOrder: (filters.sortOrder || "desc").trim(),
    }
    var transition = this.captureListTransition()
    var previousSelected = this.selectedContactId
    var html = await this.fetchHTML(this.itemsURLFor(this.viewMode, nextFilters, 0, this.chunkSize))
    this.filters = nextFilters
    this.resetRows()
    this.ingestHTML(html)
    if (previousSelected && this.indexById.has(previousSelected)) this.selectedContactId = previousSelected
    else this.selectedContactId = null
    this.render()
    this.animateListTransition(transition, { enterFrom: -8, exitTo: 14 })
    this.updateURLForState()
    this.updateFilteredSelection(previousSelected)
  }

  updateFilteredSelection(previousSelected) {
    if (this.selectedContactId && this.selectedContactId !== previousSelected && typeof htmx !== "undefined") {
      htmx.ajax("GET", "/contacts?partial=detail&contact=" + encodeURIComponent(this.selectedContactId), "#contacts-detail")
      return
    }
    if (!this.selectedContactId) this.showEmptyDetail()
  }

  // The detail pane's "Select a contact" screen, for when none is open.
  showEmptyDetail() {
    var detail = document.getElementById("contacts-detail")
    if (detail) detail.innerHTML = '<div class="flex flex-col items-center justify-center h-full text-center"><div class="space-y-4 animate-fade-in"><div class="size-20 rounded-2xl bg-card flex items-center justify-center mx-auto raised"><svg class="size-9 text-muted-foreground/30" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M15 18a3 3 0 1 0-6 0"></path><circle cx="12" cy="10" r="2"></circle><rect width="18" height="18" x="3" y="4" rx="2"></rect><path d="M8 2v4"></path><path d="M16 2v4"></path></svg></div><div><h3 class="font-semibold mb-1">Select a contact</h3><p class="text-sm text-muted-foreground">Choose a contact from the list to view or edit it</p></div></div></div>'
  }

  updateHeader() {
    var countEl = document.getElementById("contacts-count")
    if (countEl) countEl.textContent = String(this.totalCount)
  }

  updateURLForState() {
    var params = new URLSearchParams()
    if (this.filters.query) params.set("q", this.filters.query)
    if (this.filters.source) params.set("source", this.filters.source)
    if (this.filters.saveTarget) params.set("save_target", this.filters.saveTarget)
    if (this.filters.activity) params.set("activity", this.filters.activity)
    if (this.filters.trash) params.set("trash", "1")
    params.set("sort_by", this.filters.sortBy || "updated")
    params.set("sort_order", this.filters.sortOrder || "desc")
    if (this.viewMode !== "cards") params.set("view", this.viewMode)
    if (this.selectedContactId) params.set("contact", this.selectedContactId)
    var url = "/contacts" + (params.toString() ? "?" + params.toString() : "")
    history.replaceState({ contacts: true, contact: this.selectedContactId || null }, "", url)
  }

  pushUrl() {
    this.updateURLForState()
  }

  captureListTransition() {
    if (this.prefersReducedMotion() || !this.itemsContainer) return null
    var rows = new Map()
    for (var i = 0; i < this.itemsContainer.children.length; i++) {
      var node = this.itemsContainer.children[i]
      var row = node.classList.contains("mail-list-item") ? node : node.querySelector(".mail-list-item")
      if (!row || !row.dataset.contactId) continue
      var anchor = row.querySelector("a")
      var visualRect = anchor ? anchor.getBoundingClientRect() : node.getBoundingClientRect()
      var shellRect = node.getBoundingClientRect()
      rows.set(row.dataset.contactId, {
        rect: shellRect,
        visualTopOffset: visualRect.top - shellRect.top,
        html: row.outerHTML,
        text: this.captureContactTextMetrics(row),
      })
    }
    return { rows: rows }
  }

  captureContactTextMetrics(row) {
    var metrics = {}
    var fields = [
      { key: "name", selector: "[data-contact-name]" },
      { key: "email", selector: "[data-contact-email]" },
    ]
    for (var i = 0; i < fields.length; i++) {
      var el = row.querySelector(fields[i].selector)
      if (!el) continue
      var style = window.getComputedStyle(el)
      metrics[fields[i].key] = {
        fontSize: style.fontSize,
        lineHeight: style.lineHeight,
      }
    }
    return metrics
  }

  animateContactTextResize(node, oldMetrics, duration, ease) {
    if (!oldMetrics) return
    var fields = [
      { key: "name", selector: "[data-contact-name]" },
      { key: "email", selector: "[data-contact-email]" },
    ]
    for (var i = 0; i < fields.length; i++) {
      var old = oldMetrics[fields[i].key]
      if (!old) continue
      var el = node.querySelector(fields[i].selector)
      if (!el) continue
      var next = window.getComputedStyle(el)
      if (old.fontSize === next.fontSize && old.lineHeight === next.lineHeight) continue
      el.style.transition = "none"
      el.style.fontSize = old.fontSize
      el.style.lineHeight = old.lineHeight
      el.offsetHeight
      el.style.transition = "font-size " + duration + "ms " + ease + ", line-height " + duration + "ms " + ease
      el.style.fontSize = next.fontSize
      el.style.lineHeight = next.lineHeight
      this.cleanupTextTransition(el, duration)
    }
  }

  cleanupTextTransition(el, duration) {
    setTimeout(function () {
      el.style.transition = ""
      el.style.fontSize = ""
      el.style.lineHeight = ""
    }, duration + 40)
  }

  prefersReducedMotion() {
    return window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches
  }

  ensureTransitionOverlay() {
    var existing = this.container.querySelector("[data-contact-list-transition-overlay]")
    if (existing) existing.remove()
    var overlay = document.createElement("div")
    overlay.setAttribute("data-contact-list-transition-overlay", "")
    overlay.style.position = "absolute"
    overlay.style.left = "0"
    overlay.style.top = "0"
    overlay.style.right = "0"
    overlay.style.pointerEvents = "none"
    overlay.style.zIndex = "30"
    overlay.style.overflow = "visible"
    if (window.getComputedStyle(this.container).position === "static") this.container.style.position = "relative"
    this.container.appendChild(overlay)
    return overlay
  }

  animateListTransition(snapshot, options) {
    if (!snapshot || !snapshot.rows || this.prefersReducedMotion()) return
    options = options || {}
    var duration = options.duration || 190
    var ease = "cubic-bezier(0.2, 0, 0, 1)"
    var before = snapshot.rows
    var afterIds = new Set()
    var entering = []
    for (var i = 0; i < this.itemsContainer.children.length; i++) {
      var node = this.itemsContainer.children[i]
      var row = node.classList.contains("mail-list-item") ? node : node.querySelector(".mail-list-item")
      if (!row || !row.dataset.contactId) continue
      var id = row.dataset.contactId
      afterIds.add(id)
      var old = before.get(id)
      if (old) {
        var next = node.getBoundingClientRect()
        var nextAnchor = node.querySelector(".mail-list-item > a")
        var nextVisualRect = nextAnchor ? nextAnchor.getBoundingClientRect() : next
        var nextVisualTopOffset = nextVisualRect.top - next.top
        var dx = old.rect.left - next.left
        var dy = (old.rect.top + (old.visualTopOffset || 0)) - (next.top + nextVisualTopOffset)
        var finalHeight = node.style.height || (next.height + "px")
        var heightChanged = options.animateHeight && Math.abs(old.rect.height - next.height) > 0.5
        var animated = false
        if (Math.abs(dx) > 0.5 || Math.abs(dy) > 0.5 || heightChanged) {
          var base = node.style.transform || ""
          node.style.transition = "none"
          node.style.transform = base + " translate(" + dx + "px," + dy + "px)"
          if (heightChanged) {
            node.style.height = old.rect.height + "px"
            node.style.overflow = "hidden"
          }
          node.offsetHeight
          node.style.transition = "transform " + duration + "ms " + ease + (heightChanged ? ", height " + duration + "ms " + ease : "")
          node.style.transform = base
          if (heightChanged) node.style.height = finalHeight
          this.cleanupTransition(node, duration)
          animated = true
        }
        if (options.animateText) this.animateContactTextResize(node, old.text, duration, ease)
        if (!animated && options.enterExisting) entering.push(node)
      } else {
        entering.push(node)
      }
    }
    var exitCount = 0
    before.forEach(function (_, id) { if (!afterIds.has(id)) exitCount++ })
    this.animateEnteringRows(entering, duration, ease, options.enterFrom || -10, exitCount > 3 ? Math.min(140, exitCount * 12) : 0, entering.length > 3 ? 12 : 0)
    this.animateExitingRows(before, afterIds, duration, ease, options.exitTo || 12, exitCount > 3 ? 16 : 0)
  }

  animateRenderedRows(options) {
    if (this.prefersReducedMotion() || !this.itemsContainer) return
    options = options || {}
    var nodes = []
    for (var i = 0; i < this.itemsContainer.children.length; i++) {
      var node = this.itemsContainer.children[i]
      var row = node.classList.contains("mail-list-item") ? node : node.querySelector(".mail-list-item")
      if (row && row.dataset.contactId) nodes.push(node)
    }
    if (nodes.length === 0) return
    var duration = options.duration || 190
    var ease = "cubic-bezier(0.2, 0, 0, 1)"
    var stagger = options.enterStagger !== undefined ? options.enterStagger : (nodes.length > 3 ? 12 : 0)
    this.animateEnteringRows(nodes, duration, ease, options.enterFrom || -8, options.enterDelay || 0, stagger)
  }

  animateEnteringRows(nodes, duration, ease, offsetY, delay, stagger) {
    slideRowsInWholePixels(nodes, duration, offsetY, delay || 0, stagger || 0)
  }

  animateExitingRows(before, afterIds, duration, ease, offsetY, stagger) {
    if (!before || before.size === 0) return
    stagger = stagger || 0
    var overlay = null
    var containerRect = this.container.getBoundingClientRect()
    var exitIndex = 0
    // All the leaving rows' copies are placed before the page is laid out
    // once, rather than once per copy, and then fade out together.
    var clones = []
    var scrollLeft = this.container.scrollLeft
    var scrollTop = this.container.scrollTop
    before.forEach(function (item, id) {
      if (afterIds.has(id)) return
      if (!overlay) overlay = this.ensureTransitionOverlay()
      var delay = Math.min(140, exitIndex * stagger)
      exitIndex++
      var clone = document.createElement("div")
      clone.innerHTML = item.html
      clone.style.position = "absolute"
      clone.style.left = (item.rect.left - containerRect.left + scrollLeft) + "px"
      clone.style.top = (item.rect.top - containerRect.top + scrollTop) + "px"
      clone.style.width = item.rect.width + "px"
      clone.style.height = item.rect.height + "px"
      clone.style.transition = "transform " + duration + "ms " + ease + " " + delay + "ms, opacity " + duration + "ms ease-out " + delay + "ms"
      clone.style.willChange = "transform, opacity"
      overlay.appendChild(clone)
      clones.push(clone)
      setTimeout(function () { clone.remove() }, duration + delay + 40)
    }, this)
    if (overlay) overlay.offsetHeight
    for (var c = 0; c < clones.length; c++) {
      clones[c].style.opacity = "0"
      clones[c].style.transform = "translateY(" + offsetY + "px)"
    }
    if (overlay) setTimeout(function () { if (overlay.parentNode) overlay.remove() }, duration + Math.min(140, Math.max(0, exitIndex - 1) * stagger) + 80)
  }

  cleanupTransition(node, duration) {
    setTimeout(function () {
      node.style.transition = ""
      node.style.opacity = ""
      node.style.willChange = ""
      node.style.overflow = ""
    }, duration + 40)
  }
}

window.VirtualMailList = VirtualMailList
window.VirtualContactsList = VirtualContactsList

window.addEventListener("popstate", function (e) {
  if (!e.state) return

  if (e.state.settingsTab) {
    if (typeof htmx !== "undefined") {
      htmx.ajax("GET", "/settings/" + e.state.settingsTab, {target: "#main-content", swap: "outerHTML"})
    }
    return
  }

  var currentMail = document.getElementById("mail-list-scroll")
  var wasSearch = currentMail && currentMail._virtualMailList && currentMail._virtualMailList.searchScope
  if (window.location.pathname === "/mail/search" || window.location.pathname === "/search" || (wasSearch && /^\/folder\/[^/]+/.test(window.location.pathname))) {
    var target = window.location.pathname
    if (/^\/folder\//.test(target)) target = "/folder/" + target.split("/")[2] + "/full"
    var search = window.location.search
    // Let all history listeners advance request ownership before starting
    // this navigation, so its own response isn't mistaken for a stale page.
    // Moving into or out of search clears the old list first.
    var leaving = currentMail && currentMail._virtualMailList
    setTimeout(function () {
      if (leaving && window.GoferMailSearch) {
        window.GoferMailSearch.replaceList(leaving, target + search).catch(function () {
          leaving.cancelTransitionClear()
          if (typeof htmx !== "undefined") htmx.ajax("GET", target + search, {target: "#main-content", swap: "outerHTML"})
        })
      } else if (typeof htmx !== "undefined") {
        htmx.ajax("GET", target + search, {target: "#main-content", swap: "outerHTML"})
      }
    }, 0)
    return
  }

  if (e.state.contacts || window.location.pathname === "/contacts") {
    if (typeof htmx !== "undefined") {
      htmx.ajax("GET", window.location.pathname + window.location.search, {target: "#main-content", swap: "outerHTML"})
    }
    var contactsLink = document.querySelector("[data-sidebar-contacts-link]")
    var folderLinks = document.querySelectorAll("aside a[hx-get^='/folder/']")
    for (var c = 0; c < folderLinks.length; c++) {
      folderLinks[c].classList.remove("bg-sidebar-accent", "text-sidebar-primary", "font-medium")
      folderLinks[c].classList.add("text-sidebar-foreground", "hover:bg-sidebar-accent/60", "hover:text-sidebar-accent-foreground")
    }
    if (contactsLink) {
      contactsLink.classList.add("bg-sidebar-accent", "text-sidebar-primary", "font-medium")
      contactsLink.classList.remove("text-sidebar-foreground")
    }
    return
  }

  if (!e.state.folder) return

  var container = document.getElementById("mail-list-scroll")
  if (!container || !container._virtualMailList) {
    if (typeof htmx !== "undefined") {
      htmx.ajax("GET", "/folder/" + e.state.folder + "/full" + window.location.search, {target: "#main-content", swap: "outerHTML"})
    }
    return
  }

  var vml = container._virtualMailList
  var folderID = e.state.folder
  var oldNavigationState = JSON.stringify({ filters: vml.filters || {}, tag: vml.sidebarTag || {} })
  vml.filters = vml.readFiltersFromURL()
  vml.syncFilterInputs()
  vml.setSidebarTag(vml.readSidebarTagFromURL())
  var navigationStateChanged = oldNavigationState !== JSON.stringify({ filters: vml.filters || {}, tag: vml.sidebarTag || {} })
  var sidebarTag = vml.sidebarTag || vml.emptySidebarTag()
  var updateSidebarActive = function () {
    var sidebar = document.querySelector("aside")
    if (!sidebar) return
    var sidebarLinks = sidebar.querySelectorAll("a[hx-get^='/folder/']")
    for (var i = 0; i < sidebarLinks.length; i++) {
      sidebarLinks[i].classList.remove(
        "bg-sidebar-accent",
        "text-sidebar-primary",
        "font-medium"
      )
      sidebarLinks[i].classList.add("text-sidebar-foreground", "hover:bg-sidebar-accent/60", "hover:text-sidebar-accent-foreground")
      var badge = sidebarLinks[i].querySelector("[data-folder-unread]")
      if (badge) {
        badge.classList.remove("bg-sidebar-primary/20", "text-sidebar-primary")
        badge.classList.add("bg-sidebar-accent", "text-sidebar-foreground")
      }
    }
    var folderRows = sidebar.querySelectorAll("[data-sidebar-folder-row]")
    for (var r = 0; r < folderRows.length; r++) {
      folderRows[r].classList.remove("bg-sidebar-accent", "text-sidebar-primary", "font-medium")
      folderRows[r].classList.add("text-sidebar-foreground", "hover:bg-sidebar-accent/60", "hover:text-sidebar-accent-foreground")
      var rowBadge = folderRows[r].querySelector("[data-folder-unread]")
      if (rowBadge) {
        rowBadge.classList.remove("bg-sidebar-primary/20", "text-sidebar-primary")
        rowBadge.classList.add("bg-sidebar-accent", "text-sidebar-foreground")
      }
    }
    var activeLink = null
    var tagGroups = sidebar.querySelectorAll("[data-sidebar-tag-group]")
    for (var g = 0; g < tagGroups.length; g++) tagGroups[g].removeAttribute("data-sidebar-tag-active")
    var folderGroups = sidebar.querySelectorAll("[data-sidebar-folder]")
    for (var f = 0; f < folderGroups.length; f++) folderGroups[f].removeAttribute("data-sidebar-folder-active")
    if (sidebarTag.label) {
      var tagLinks = sidebar.querySelectorAll("a[data-sidebar-tag-filter]")
      for (var t = 0; t < tagLinks.length; t++) {
        if ((tagLinks[t].dataset.sidebarTagLabel || "") === sidebarTag.label && (tagLinks[t].dataset.sidebarTagAccount || "") === sidebarTag.accountId) {
          activeLink = tagLinks[t]
          break
        }
      }
    }
    if (!activeLink) activeLink = sidebar.querySelector('a[hx-get="/folder/' + folderID + '"]')
    if (activeLink) {
      activeLink.classList.add("bg-sidebar-accent", "text-sidebar-primary", "font-medium")
      activeLink.classList.remove("text-sidebar-foreground", "hover:bg-sidebar-accent/60", "hover:text-sidebar-accent-foreground")
      var activeRow = activeLink.closest("[data-sidebar-folder-row]")
      if (activeRow) {
        activeRow.classList.add("bg-sidebar-accent")
        activeRow.classList.remove("hover:bg-sidebar-accent/60")
      }
      var activeBadge = activeLink.querySelector("[data-folder-unread]")
      if (activeBadge) {
        activeBadge.classList.remove("bg-sidebar-accent", "text-sidebar-foreground")
        activeBadge.classList.add("bg-sidebar-primary/20", "text-sidebar-primary")
      }
      if (activeLink.hasAttribute("data-sidebar-tag-filter")) {
        var activeGroup = activeLink.closest("[data-sidebar-tag-group]")
        if (activeGroup) {
          activeGroup.setAttribute("data-sidebar-tag-active", "")
          activeGroup.setAttribute("data-sidebar-tag-collapsed", "false")
          var toggle = activeGroup.querySelector("[data-sidebar-tag-toggle]")
          if (toggle) toggle.setAttribute("aria-expanded", "true")
        }
      } else {
        var folderGroup = activeLink.closest("[data-sidebar-folder]")
        while (folderGroup) {
          folderGroup.setAttribute("data-sidebar-folder-active", "")
          folderGroup.setAttribute("data-sidebar-folder-collapsed", "false")
          var folderToggle = folderGroup.querySelector("[data-sidebar-folder-toggle]")
          if (folderToggle) folderToggle.setAttribute("aria-expanded", "true")
          folderGroup = folderGroup.parentElement && folderGroup.parentElement.closest ? folderGroup.parentElement.closest("[data-sidebar-folder]") : null
        }
      }
    }
  }
  if (folderID && folderID !== vml.folderID) {
    vml.switchFolder(folderID, false)
    updateSidebarActive()
  } else if (navigationStateChanged) {
    vml.refreshCurrentFolder({ rebase: true }).catch(function () {})
    updateSidebarActive()
  } else if (!e.state.email && vml.selectedEmailId) {
    var closedEmailId = vml.selectedEmailId
    vml.selectedEmailId = null
    vml.syncSelectionClasses(vml.itemsContainer)
    if (typeof setMailViewEmpty === "function") setMailViewEmpty()
    document.dispatchEvent(new CustomEvent("gofer:mail-view-closed", { detail: { emailId: closedEmailId } }))
  } else if (e.state.email && e.state.email !== vml.selectedEmailId) {
    vml.selectedEmailId = e.state.email
    vml.prevFirst = null
    vml.prevLast = null
    vml.render()
    if (typeof htmx !== "undefined") {
      if (typeof showMailViewLoading === "function") showMailViewLoading()
      htmx.ajax("GET", typeof mailViewRequestURL === "function" ? mailViewRequestURL(e.state.email) : "/email/" + e.state.email, "#mail-view")
    }
  }
})
