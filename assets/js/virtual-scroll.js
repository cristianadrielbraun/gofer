class VirtualMailList {
  constructor(container, options) {
    this.container = container
    this.folderID = options.folderID || "inbox"
    this.viewMode = options.viewMode || container.dataset.viewMode || "cards"
    this.itemHeight = this.viewMode === "table" ? 44 : 94
    this.subItemHeight = this.viewMode === "table" ? 38 : 48
    this.expandedThreadGap = 14
    this.overscan = 10

    this.cache = new Map()
    this.indexById = new Map()
    this.loadedRanges = []
    this.totalCount = 0
    this.effectiveCount = 0
    this.selectedEmailId = null
    this.nextCursor = null
    this.hasMore = true
    this.isLoading = false
    this.activeFetches = new Set()
    this.newEmailCount = 0
    this.syncState = { active: false, current: 0, total: 0 }
    this.filters = this.emptyFilters()
    this.refreshInFlight = null
    this.refreshQueued = false
    this.windowedMode = false
    this.windowThreshold = 20000
    this.chunkSize = 100
    this.chunkBuffer = 2
    this.loadingDirection = null
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

    this.rowPool = []
    this.visibleRows = new Map()
    this.rowByIndex = new Map()
    this.poolSlack = 6

    this.expandedThreads = new Map()
    this._offsetCache = null
    this._offsetCacheLen = -1

    this.bindEvents()
  }

  _rebuildOffsets() {
    if (this._offsetCacheLen === this.effectiveCount) return
    this._offsetCache = new Array(this.effectiveCount + 1)
    this._offsetCache[0] = 0
    for (var i = 0; i < this.effectiveCount; i++) {
      this._offsetCache[i + 1] = this._offsetCache[i] + this.getHeight(i)
    }
    this._offsetCacheLen = this.effectiveCount
  }

  totalHeight() {
    if (this.effectiveCount === 0) return 0
    this._rebuildOffsets()
    return this._offsetCache[this.effectiveCount]
  }

  offsetAtPosition(pos) {
    if (pos <= 0) return 0
    this._rebuildOffsets()
    if (pos >= this.effectiveCount) return this._offsetCache[this.effectiveCount]
    return this._offsetCache[pos]
  }

  positionAtOffset(targetOffset) {
    if (targetOffset <= 0 || this.effectiveCount === 0) return 0
    this._rebuildOffsets()
    var lo = 0, hi = this.effectiveCount
    while (lo < hi) {
      var mid = (lo + hi) >> 1
      if (this._offsetCache[mid + 1] <= targetOffset) lo = mid + 1
      else hi = mid
    }
    return Math.min(lo, this.effectiveCount - 1)
  }

  getHeight(pos) {
    var item = this.cache.get(pos)
    if (!item) return this.itemHeight
    var expanded = this.expandedThreads.get(item.id)
    if (expanded) {
      var gap = this.viewMode === "table" ? 0 : this.expandedThreadGap
      return this.itemHeight + expanded.subCount * this.subItemHeight + gap
    }
    return this.itemHeight
  }

  invalidateOffsets() {
    this._offsetCacheLen = -1
    this._offsetCache = null
  }

  setupDOM() {
    this.spacerTop = document.createElement("div")
    this.spacerBottom = document.createElement("div")
    this.itemsContainer = document.createElement("div")

    this.container.innerHTML = ""
    this.container.appendChild(this.spacerTop)
    this.container.appendChild(this.itemsContainer)
    this.container.appendChild(this.spacerBottom)
    this.itemsContainer.style.position = "relative"
    this.itemsContainer.style.minWidth = "0"
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
    if (this.effectiveCount === 0) {
      var stale = this.captureListTransition()
      this.spacerTop.style.height = "0px"
      this.spacerBottom.style.height = "0px"
      this.rowPool = []
      this.visibleRows.clear()
      this.rowByIndex.clear()
      var syncing = this.syncState && this.syncState.active
      var subtitle = syncing
        ? (this.syncState.total > 0
          ? ("Syncing emails " + this.syncState.current + " / " + this.syncState.total)
          : "Syncing emails...")
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
      this.animateExitingRows(stale ? stale.rows : null, new Set(), 180, "cubic-bezier(0.2, 0, 0, 1)", 14, 18)
      return
    }

    this._rebuildOffsets()

    var first = this.positionAtOffset(Math.max(0, scrollTop - this.overscan * this.itemHeight))
    var last = this.positionAtOffset(Math.min(this.totalHeight(), scrollTop + clientHeight + this.overscan * this.itemHeight))
    last = Math.min(last, this.effectiveCount - 1)

    if (first === this.prevFirst && last === this.prevLast) {
      this.maybeLoadAtEdges(first, last)
      return
    }
    this.prevFirst = first
    this.prevLast = last

    this.ensureRangeLoaded(first, last)
    this.maybeLoadAtEdges(first, last)
    // Keep chunks resident for now; eviction can be re-enabled after stability pass.

    this.spacerTop.style.height = "0px"
    this.spacerBottom.style.height = "0px"
    this.itemsContainer.style.height = this.totalHeight() + "px"

    this.renderPooled(first, last)
    this.syncSelectionClasses(this.itemsContainer)
    this.evictFarChunks(first, last)

    if (typeof htmx !== "undefined") {
      htmx.process(this.itemsContainer)
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
      shell.style.willChange = "transform"
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
      if (this.rowByIndex.has(pos)) continue
      var row = this.acquireRow(pos)
      if (!row) continue
      this.stampRow(row, pos)
    }
    var vis = Array.from(this.visibleRows.entries())
    for (var j = 0; j < vis.length; j++) this.stampRow(vis[j][0], vis[j][1])
  }

  stampRow(shell, index) {
    var item = this.cache.get(index)
    shell.hidden = false
    shell.style.transform = "translateY(" + this.offsetAtPosition(index) + "px)"
    shell.style.height = this.getHeight(index) + "px"
    shell.className = ""
    shell.removeAttribute("data-thread-entering")
    shell.removeAttribute("data-thread-collapsing")
    if (!item) {
      shell.innerHTML = this.createSkeleton().outerHTML
      return
    }
    shell.innerHTML = item.html
    var row = shell.querySelector(".mail-list-item") || shell.firstElementChild
    if (!row) return
    var anchor = row.querySelector("a")
    if (!anchor) return
    var expanded = this.expandedThreads.get(item.id)
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
      var mainRow = document.createElement("div")
      mainRow.className = "mail-list-thread-main"
      anchor.style.height = this.viewMode === "table" ? "" : "88px"
      if (toggle) toggle.setAttribute("data-expanded", "")
      mainRow.appendChild(row)

      var subContainer = document.createElement("div")
      subContainer.className = "thread-sub-items"
      subContainer.innerHTML = expanded.html

      shell.replaceChildren(mainRow, subContainer)
      this.syncSelectionClasses(shell)
      return
    }
    if (toggle) toggle.removeAttribute("data-expanded")
    anchor.style.height = ""
  }

  setSyncState(active, current, total) {
    this.syncState = {
      active: !!active,
      current: current || 0,
      total: total || 0,
    }
    if (this.totalCount === 0) {
      this.prevFirst = null
      this.prevLast = null
      this.render()
    }
    this.updateSyncHeader()
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
      rows.set(row.dataset.emailId, {
        rect: node.getBoundingClientRect(),
        html: row.outerHTML,
      })
    }
    return { rows: rows }
  }

  prefersReducedMotion() {
    return window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches
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
      this.animateExitingRows(before, afterIds, duration, ease, options.exitTo || 12, options.exitStagger || 0)
      return
    }

    for (var i = 0; i < this.itemsContainer.children.length; i++) {
      var node = this.itemsContainer.children[i]
      var row = node.classList.contains("mail-list-item") ? node : node.querySelector(".mail-list-item")
      if (!row || !row.dataset.emailId) continue
      var id = row.dataset.emailId
      afterIds.add(id)
      var old = before.get(id)
      if (old) {
        var next = node.getBoundingClientRect()
        var dx = old.rect.left - next.left
        var dy = old.rect.top - next.top
        if (Math.abs(dx) > 0.5 || Math.abs(dy) > 0.5) {
          var base = node.style.transform || ""
          node.style.transition = "none"
          node.style.transform = base + " translate(" + dx + "px," + dy + "px)"
          node.offsetHeight
          node.style.transition = "transform " + duration + "ms " + ease
          node.style.transform = base
          this.cleanupTransition(node, duration)
        }
      } else {
        entering.push(node)
      }
    }

    var exitCount = 0
    before.forEach(function (_, id) { if (!afterIds.has(id)) exitCount++ })
    var exitStagger = options.exitStagger !== undefined ? options.exitStagger : (exitCount > 3 ? 16 : 0)
    var enterStagger = options.enterStagger !== undefined ? options.enterStagger : (entering.length > 3 ? 12 : 0)
    var enterDelay = options.enterDelay !== undefined ? options.enterDelay : (exitCount > 3 && entering.length > 0 ? Math.min(140, exitCount * 12) : 0)

    this.animateEnteringRows(entering, duration, ease, options.enterFrom || -10, enterDelay, enterStagger)
    this.animateExitingRows(before, afterIds, duration, ease, options.exitTo || 12, exitStagger)
  }

  animateEnteringRows(nodes, duration, ease, offsetY, delay, stagger) {
    delay = delay || 0
    stagger = stagger || 0
    for (var i = 0; i < nodes.length; i++) {
      var node = nodes[i]
      var base = node.style.transform || ""
      var itemDelay = delay + Math.min(120, i * stagger)
      node.style.transition = "none"
      node.style.opacity = "0"
      node.style.transform = base + " translateY(" + offsetY + "px) scale(0.985)"
      node.offsetHeight
      node.style.transition = "transform " + duration + "ms " + ease + " " + itemDelay + "ms, opacity " + duration + "ms ease-out " + itemDelay + "ms"
      node.style.opacity = "1"
      node.style.transform = base
      this.cleanupTransition(node, duration + itemDelay)
    }
  }

  animateExitingRows(before, afterIds, duration, ease, offsetY, stagger) {
    if (!before || before.size === 0) return
    stagger = stagger || 0
    var overlay = null
    var containerRect = this.container.getBoundingClientRect()
    var exitIndex = 0
    before.forEach(function (item, id) {
      if (afterIds.has(id)) return
      if (!overlay) overlay = this.ensureTransitionOverlay()
      var delay = Math.min(140, exitIndex * stagger)
      exitIndex++
      var clone = document.createElement("div")
      clone.innerHTML = item.html
      clone.style.position = "absolute"
      clone.style.left = (item.rect.left - containerRect.left + this.container.scrollLeft) + "px"
      clone.style.top = (item.rect.top - containerRect.top + this.container.scrollTop) + "px"
      clone.style.width = item.rect.width + "px"
      clone.style.height = item.rect.height + "px"
      clone.style.transition = "transform " + duration + "ms " + ease + " " + delay + "ms, opacity " + duration + "ms ease-out " + delay + "ms"
      clone.style.willChange = "transform, opacity"
      overlay.appendChild(clone)
      clone.offsetHeight
      clone.style.opacity = "0"
      clone.style.transform = "translateY(" + offsetY + "px) scale(0.985)"
      setTimeout(function () { clone.remove() }, duration + delay + 40)
    }, this)
    if (overlay) {
      setTimeout(function () { if (overlay.parentNode) overlay.remove() }, duration + Math.min(140, Math.max(0, exitIndex - 1) * stagger) + 80)
    }
  }

  cleanupTransition(node, duration) {
    setTimeout(function () {
      node.style.transition = ""
      node.style.opacity = ""
      node.style.willChange = ""
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
      node.style.transition = "transform 180ms ease-out"
      node.style.transform = baseTransform
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
    row.className = "mail-list-skeleton" + (this.viewMode === "table" ? " mail-list-table-skeleton" : "")
    if (this.viewMode === "table") {
      row.innerHTML =
        '<div class="mail-list-table-grid grid items-center gap-3 w-full px-3 py-1.5">' +
        '<div data-mail-table-cell="thread"></div>' +
        '<div class="flex items-center min-w-0" data-mail-table-cell="from">' +
        '<div class="h-3 w-24 rounded bg-muted animate-pulse"></div>' +
        "</div>" +
        '<div class="flex items-center gap-2 min-w-0" data-mail-table-cell="subject">' +
        '<div class="h-3 w-40 rounded bg-muted animate-pulse"></div>' +
        '<div class="hidden xl:block h-3 w-28 rounded bg-muted animate-pulse"></div>' +
        "</div>" +
        '<div class="flex items-center justify-center shrink-0" data-mail-table-cell="starred">' +
        '<div class="h-3 w-3 rounded bg-muted animate-pulse"></div>' +
        "</div>" +
        '<div class="flex items-center justify-center shrink-0" data-mail-table-cell="attachment">' +
        '<div class="h-3 w-3 rounded bg-muted animate-pulse"></div>' +
        "</div>" +
        '<div class="flex items-center justify-end shrink-0" data-mail-table-cell="date">' +
        '<div class="h-3 w-12 rounded bg-muted animate-pulse"></div>' +
        "</div>" +
        "</div>"
      return row
    }
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
    var chunkKey = "chunk-" + chunkIndex
    if (this.activeChunkFetches.has(chunkKey)) return
    if (this.findGaps(start, end).length === 0) return
    this.activeChunkFetches.add(chunkKey)
    this.loadingDirection = direction
    this.pendingLoadEnd = direction === "down" ? start : null
    this.loadError = null
    var revealPendingDownRow = direction === "down"
    this.updateEffectiveCount()
    if (revealPendingDownRow) {
      this.container.scrollTop = this.container.scrollTop + this.itemHeight
    }
    this.prevFirst = null
    this.prevLast = null
    this.render()
    try {
      if (window.__debugWindowedMail) {
        console.debug("[mail-chunk] load", direction, chunkIndex, start, end)
      }
      await this.fetchRange(start, end)
      this.frontierDown = Math.max(this.frontierDown, end)
      this.frontierUp = Math.min(this.frontierUp, start)
    } catch (_) {
      this.loadError = "Failed to load emails. Scroll again to retry."
    } finally {
      this.loadingDirection = null
      this.pendingLoadEnd = null
      this.activeChunkFetches.delete(chunkKey)
      this.updateEffectiveCount()
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

  evictFarChunks(first, last) {
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
    if (evicted) this.invalidateLoadedRanges()
  }

  async fetchRange(start, end) {
    var key = "range-" + start + "-" + end
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
      this.addLoadedRange(start, end)
      this.prevFirst = null
      this.prevLast = null
      this.render()
    } catch (_) {
    } finally {
      this.activeFetches.delete(key)
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

    var tc = parseInt(wrapper.dataset.totalCount)
    if (!isNaN(tc)) this.totalCount = tc

    if (wrapper.dataset.nextCursor) {
      this.nextCursor = wrapper.dataset.nextCursor
    }
    if (wrapper.dataset.folderId) {
      this.folderID = wrapper.dataset.folderId
    }
    if (wrapper.dataset.hasMore !== undefined) {
      this.hasMore = wrapper.dataset.hasMore === "true"
    }

    var items = wrapper.querySelectorAll(".mail-list-item[data-email-id]")
    for (var i = 0; i < items.length; i++) {
      var el = items[i]
      var pos = parseInt(el.dataset.position)
      var id = el.dataset.emailId
      if (isNaN(pos) || !id) continue
      this.cache.set(pos, { id: id, html: el.outerHTML })
      this.indexById.set(id, pos)
    }

    var start = parseInt(wrapper.dataset.windowStart)
    var end = parseInt(wrapper.dataset.windowEnd)
    if (!isNaN(start) && !isNaN(end)) {
      this.addLoadedRange(start, end)
    }
    this.updateEffectiveCount()
  }

  updateEffectiveCount() {
    var maxLoaded = this.getLoadedMax()
    if (maxLoaded < 0) {
      this.effectiveCount = 0
      this.invalidateOffsets()
      return
    }
    var next = Math.min(this.totalCount, maxLoaded + 1)
    if (this.loadingDirection === "down" && this.pendingLoadEnd !== null) {
      next = Math.min(this.totalCount, Math.max(next, this.pendingLoadEnd + 1))
    }
    if (next !== this.effectiveCount) {
      this.effectiveCount = next
      this.invalidateOffsets()
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

  invalidateLoadedRanges() {
    this.loadedRanges = []
    var entries = Array.from(this.cache.entries())
    for (var i = 0; i < entries.length; i++) {
      this.loadedRanges.push({ start: entries[i][0], end: entries[i][0] })
    }
    this.mergeRanges()
    this.updateEffectiveCount()
  }

  hydrateFromDOM() {
    var scrollEl =
      document.getElementById("mail-list-scroll") || this.container
    this.setViewMode(scrollEl.dataset.viewMode || this.viewMode, true)
    var totalCount = parseInt(scrollEl.dataset.totalCount)
    if (!isNaN(totalCount)) this.totalCount = totalCount
    if (scrollEl.dataset.folderId) {
      this.folderID = scrollEl.dataset.folderId
    }

    var items = scrollEl.querySelectorAll(".mail-list-item[data-email-id]")
    for (var i = 0; i < items.length; i++) {
      var el = items[i]
      var pos = parseInt(el.dataset.position)
      var id = el.dataset.emailId
      if (isNaN(pos) || !id) continue
      this.cache.set(pos, { id: id, html: el.outerHTML })
      this.indexById.set(id, pos)
    }

    if (this.cache.size > 0) {
      var positions = Array.from(this.cache.keys())
      this.frontierUp = Math.min.apply(null, positions)
      this.frontierDown = Math.max.apply(null, positions)
      this.addLoadedRange(
        this.frontierUp,
        this.frontierDown
      )
    }
    this.updateEffectiveCount()

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

    this.container.innerHTML = ""
    this.spacerTop = document.createElement("div")
    this.spacerBottom = document.createElement("div")
    this.itemsContainer = document.createElement("div")
    this.itemsContainer.style.position = "relative"
    this.itemsContainer.style.minWidth = "0"
    this.container.appendChild(this.spacerTop)
    this.container.appendChild(this.itemsContainer)
    this.container.appendChild(this.spacerBottom)

    this.renderTableHeader()

    this.render()
  }

  renderTableHeader() {
    var existing = this.container.querySelector(".mail-list-table-header")
    if (existing) existing.remove()
    if (this.viewMode !== "table") return

    var header = document.createElement("div")
    header.className = "mail-list-table-header mail-list-table-grid grid items-center gap-3 px-3 py-1.5 text-[10px] font-semibold uppercase tracking-wider text-muted-foreground bg-card/95 border-b border-border/70 sticky top-0 z-20 backdrop-blur-sm"
    header.innerHTML = this.tableHeaderHTML()
    this.container.insertBefore(header, this.spacerTop)
  }

  tableHeaderHTML() {
    return '<div class="mail-list-table-heading flex items-center justify-start" data-mail-table-column="0" data-mail-table-column-id="thread" data-mail-table-cell="thread" title="Thread"><svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="size-3"><path d="M14 9a2 2 0 0 1-2 2H6l-4 4V4a2 2 0 0 1 2-2h8a2 2 0 0 1 2 2z"></path><path d="M18 9h2a2 2 0 0 1 2 2v11l-4-4h-6a2 2 0 0 1-2-2v-1"></path></svg><span class="mail-list-column-resize" data-mail-table-resize="0"></span></div>' +
      '<div class="mail-list-table-heading" data-mail-table-column="1" data-mail-table-column-id="from" data-mail-table-cell="from">From<span class="mail-list-column-resize" data-mail-table-resize="1"></span></div>' +
      '<div class="mail-list-table-heading" data-mail-table-column="2" data-mail-table-column-id="subject" data-mail-table-cell="subject">Subject<span class="mail-list-column-resize" data-mail-table-resize="2"></span></div>' +
      '<div class="mail-list-table-heading text-center" data-mail-table-column="3" data-mail-table-column-id="starred" data-mail-table-cell="starred" title="Starred"><svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="size-3 mx-auto"><path d="M11.525 2.295a.53.53 0 0 1 .95 0l2.31 4.679a2.12 2.12 0 0 0 1.595 1.16l5.166.751a.53.53 0 0 1 .294.904l-3.736 3.643a2.12 2.12 0 0 0-.611 1.878l.882 5.14a.53.53 0 0 1-.771.56l-4.618-2.428a2.12 2.12 0 0 0-1.973 0L6.396 21.01a.53.53 0 0 1-.77-.56l.881-5.139a2.12 2.12 0 0 0-.611-1.879L2.16 9.795a.53.53 0 0 1 .294-.906l5.165-.75a2.12 2.12 0 0 0 1.596-1.16z"/></svg><span class="mail-list-column-separator"></span></div>' +
      '<div class="mail-list-table-heading text-center" data-mail-table-column="4" data-mail-table-column-id="attachment" data-mail-table-cell="attachment" title="Attachment"><svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="size-3 mx-auto"><path d="m16 6-8.414 8.586a2 2 0 0 0 2.829 2.829l8.414-8.586a4 4 0 1 0-5.657-5.657l-8.379 8.551a6 6 0 1 0 8.485 8.485l8.379-8.551"/></svg><span class="mail-list-column-separator"></span></div>' +
      '<div class="mail-list-table-heading min-w-12 text-right" data-mail-table-column="5" data-mail-table-column-id="date" data-mail-table-cell="date">Date</div>'
  }

  async switchFolder(folderID, pushState) {
    if (pushState === undefined) pushState = true
    var previousSelected = this.selectedEmailId
    var params = "limit=50"
    if (previousSelected) {
      params += "&selected=" + encodeURIComponent(previousSelected)
    }
    var url = "/mail/folder/" + folderID + "/items?" + params
    url += "&view=" + encodeURIComponent(this.viewMode)
    url = this.withFilterParams(url)
    var html = await this.fetchHTML(url)

    this.reset()
    this.folderID = folderID
    this.ingestHTML(html)

    if (this.cache.size > 0) {
      var firstItem = this.cache.get(0)
      if (firstItem) {
        this.selectedEmailId = firstItem.id
        if (typeof htmx !== "undefined") {
          if (typeof showMailViewLoading === "function") showMailViewLoading()
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
    this.updateSyncHeader()
    if (pushState) this.pushUrl()
  }

  async refreshCurrentFolder() {
    if (this.refreshInFlight) {
      this.refreshQueued = true
      return this.refreshInFlight
    }

    var self = this
    this.refreshInFlight = (async function () {
      var transition = self.captureListTransition()
      var params = "limit=50"
      if (self.selectedEmailId) {
        params += "&selected=" + encodeURIComponent(self.selectedEmailId)
      }
      var url = "/mail/folder/" + self.folderID + "/items?" + params + "&view=" + encodeURIComponent(self.viewMode)
      url = self.withFilterParams(url)
      var html = await self.fetchHTML(url)
      var selected = self.selectedEmailId
      var syncState = self.syncState
      if (self.filterCount() > 0) {
        self.reset()
        self.selectedEmailId = selected
        self.syncState = syncState
      }
      self.ingestHTML(html)
      self.prevFirst = null
      self.prevLast = null
      self.render()
      self.animateListTransition(transition, { enterFrom: -12, exitTo: 12 })
      self.updateHeader()
      self.updateSyncHeader()
    })()

    try {
      await this.refreshInFlight
    } finally {
      this.refreshInFlight = null
      if (this.refreshQueued) {
        this.refreshQueued = false
        this.refreshCurrentFolder()
      }
    }
  }

  reset() {
    this.cache.clear()
    this.indexById.clear()
    this.loadedRanges = []
    this.totalCount = 0
    this.effectiveCount = 0
    this.selectedEmailId = null
    this.nextCursor = null
    this.hasMore = true
    this.isLoading = false
    this.newEmailCount = 0
    this.syncState = { active: false, current: 0, total: 0 }
    this.activeFetches.clear()
    this.activeChunkFetches.clear()
    this.pendingLoadEnd = null
    this.loadingDirection = null
    this.windowedMode = false
    this.anchorAbsoluteIndex = null
    this.suppressWindowShift = false
    this.prevFirst = null
    this.prevLast = null
    this.expandedThreads.clear()
    this.rowPool = []
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

  setViewMode(viewMode, keepRows) {
    this.viewMode = viewMode === "table" ? "table" : "cards"
    this.itemHeight = this.viewMode === "table" ? 44 : 94
    this.subItemHeight = this.viewMode === "table" ? 38 : 48
    this.container.dataset.viewMode = this.viewMode
    var mailList = document.getElementById("mail-list")
    if (mailList) mailList.dataset.mailListView = this.viewMode
    this.renderTableHeader()
    this.invalidateOffsets()
    if (!keepRows) {
      this.cache.clear()
      this.indexById.clear()
      this.loadedRanges = []
      this.frontierDown = -1
      this.frontierUp = 0
      this.effectiveCount = 0
      this.prevFirst = null
      this.prevLast = null
    }
  }

  async switchViewMode(viewMode) {
    viewMode = viewMode === "table" ? "table" : "cards"
    if (viewMode === this.viewMode) return
    var selected = this.selectedEmailId
    var params = "limit=50&view=" + encodeURIComponent(viewMode)
    if (selected) params += "&selected=" + encodeURIComponent(selected)
    var html = await this.fetchHTML(this.withFilterParams("/mail/folder/" + this.folderID + "/items?" + params))
    this.setViewMode(viewMode, false)
    this.ingestHTML(html)
    this.selectedEmailId = selected
    this.render()
  }

  withFilterParams(url) {
    var sep = url.indexOf("?") === -1 ? "?" : "&"
    var filters = this.filters || this.emptyFilters()
    var pairs = [
      ["unread", filters.unread ? "1" : ""],
      ["starred", filters.starred ? "1" : ""],
      ["attachments", filters.attachments ? "1" : ""],
      ["read", filters.read ? "1" : ""],
      ["no_attachments", filters.noAttachments ? "1" : ""],
      ["has_labels", filters.hasLabels ? "1" : ""],
      ["threads_only", filters.threadsOnly ? "1" : ""],
      ["from", filters.from || ""],
      ["to", filters.to || ""],
      ["subject", filters.subject || ""],
      ["body", filters.body || ""],
      ["from_domain", filters.fromDomain || ""],
      ["attachment", filters.attachment || ""],
      ["label", filters.label || ""],
      ["account_id", filters.accountId || ""],
      ["q", filters.query || ""],
      ["after_date", filters.afterDate || ""],
      ["before_date", filters.beforeDate || ""],
    ]
    for (var i = 0; i < pairs.length; i++) {
      if (!pairs[i][1]) continue
      url += sep + encodeURIComponent(pairs[i][0]) + "=" + encodeURIComponent(pairs[i][1])
      sep = "&"
    }
    return url
  }

  emptyFilters() {
    return {
      unread: false,
      starred: false,
      attachments: false,
      read: false,
      noAttachments: false,
      hasLabels: false,
      threadsOnly: false,
      from: "",
      to: "",
      subject: "",
      body: "",
      fromDomain: "",
      attachment: "",
      label: "",
      accountId: "",
      query: "",
      afterDate: "",
      beforeDate: "",
    }
  }

  filterCount() {
    var filters = this.filters || this.emptyFilters()
    return (filters.unread ? 1 : 0) + (filters.starred ? 1 : 0) + (filters.attachments ? 1 : 0) +
      (filters.read ? 1 : 0) + (filters.noAttachments ? 1 : 0) + (filters.hasLabels ? 1 : 0) +
      (filters.threadsOnly ? 1 : 0) + (filters.from ? 1 : 0) + (filters.to ? 1 : 0) +
      (filters.subject ? 1 : 0) + (filters.body ? 1 : 0) + (filters.fromDomain ? 1 : 0) +
      (filters.attachment ? 1 : 0) + (filters.label ? 1 : 0) + (filters.accountId ? 1 : 0) +
      (filters.query ? 1 : 0) + (filters.afterDate ? 1 : 0) + (filters.beforeDate ? 1 : 0)
  }

  async applyFilters(filters) {
    var next = this.emptyFilters()
    filters = filters || {}
    for (var key in next) {
      if (typeof next[key] === "boolean") next[key] = !!filters[key]
      else next[key] = (filters[key] || "").trim()
    }
    this.filters = next
    var transition = this.captureListTransition()
    var previousSelected = this.selectedEmailId
    var params = "limit=50&view=" + encodeURIComponent(this.viewMode)
    if (previousSelected) params += "&selected=" + encodeURIComponent(previousSelected)
    var html = await this.fetchHTML(this.withFilterParams("/mail/folder/" + this.folderID + "/items?" + params))
    var syncState = this.syncState
    this.reset()
    this.syncState = syncState
    this.ingestHTML(html)
    if (previousSelected && this.indexById.has(previousSelected)) {
      this.selectedEmailId = previousSelected
    } else if (this.cache.size > 0) {
      var firstItem = this.cache.get(0)
      if (firstItem) this.selectedEmailId = firstItem.id
    }
    this.render()
    this.animateListTransition(transition, { enterFrom: -8, exitTo: 14 })
    this.updateHeader()
    this.updateFilteredSelection(previousSelected)
  }

  updateFilteredSelection(previousSelected) {
    if (this.selectedEmailId && this.selectedEmailId !== previousSelected && typeof htmx !== "undefined") {
      if (typeof showMailViewLoading === "function") showMailViewLoading()
      htmx.ajax("GET", "/email/" + this.selectedEmailId, "#mail-view")
      return
    }
    if (!this.selectedEmailId) {
      var mailView = document.getElementById("mail-view")
      if (mailView) mailView.innerHTML = ""
    }
  }

  onEmailSelected(emailId) {
    this.selectedEmailId = emailId
    this.syncSelectionClasses(this.itemsContainer)
    this.pushUrl()
  }

  ensureSelectionWindowed() {}

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

  updateSyncHeader() {
    var list = document.getElementById("mail-list")
    if (!list) return
    var row = document.getElementById("mail-sync-status")
    if (!row) {
      row = document.createElement("div")
      row.id = "mail-sync-status"
      row.className = "px-4 pb-2 hidden"
      row.innerHTML =
        '<div class="rounded-[var(--radius)] border border-border bg-muted/40 px-2.5 py-2">' +
          '<div class="flex items-center justify-between text-[11px] text-muted-foreground mb-1">' +
            '<span id="mail-sync-text">Syncing folder: fetching messages</span>' +
            '<span id="mail-sync-count"></span>' +
          '</div>' +
          '<div class="h-1.5 w-full rounded-full bg-muted overflow-hidden">' +
            '<div id="mail-sync-progress" class="h-full bg-amber-500 transition-all duration-300 ease-out" style="width: 8%"></div>' +
          '</div>' +
        '</div>'
      var scroll = document.getElementById("mail-list-scroll")
      if (scroll && scroll.parentElement === list) list.insertBefore(row, scroll)
      else list.appendChild(row)
    }

    if (!this.syncState || !this.syncState.active) {
      row.classList.add("hidden")
      return
    }
    row.classList.remove("hidden")
    var cur = this.syncState.current || 0
    var total = this.syncState.total || 0
    var text = document.getElementById("mail-sync-text")
    var count = document.getElementById("mail-sync-count")
    var bar = document.getElementById("mail-sync-progress")
    if (text) {
      text.textContent = total > 0
        ? "Syncing folder: fetching messages"
        : "Syncing folder: fetching messages (total unknown)"
    }
    if (count) {
      count.textContent = total > 0
        ? (cur + " / " + total + " fetched")
        : (cur > 0 ? (cur + " fetched") : "")
    }
    if (bar) {
      if (total > 0) {
        var pct = Math.max(4, Math.min(100, (cur / total) * 100))
        bar.style.width = pct + "%"
        bar.style.animation = "none"
      } else {
        bar.style.width = "35%"
        bar.style.animation = "mailSyncIndeterminate 1.2s ease-in-out infinite"
      }
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

  async toggleThreadExpand(emailId) {
    var pos = this.indexById.get(emailId)
    if (pos === undefined) return
    this.onEmailSelected(emailId)
    var previousLayout = this.captureRenderedLayout()

    if (this.expandedThreads.has(emailId)) {
      var row = this.container.querySelector('[data-email-id="' + emailId + '"]')
      var slot = row ? row.closest(".mail-list-thread-slot") : null
      if (slot && !this.prefersReducedMotion()) {
        slot.setAttribute("data-thread-collapsing", "")
        await new Promise(function (resolve) { setTimeout(resolve, 170) })
      }
      this.expandedThreads.delete(emailId)
      this.invalidateOffsets()
      this.prevFirst = null
      this.prevLast = null
      this.render()
      return
    }

    var item = this.cache.get(pos)
    if (!item) return

    try {
      var threadId = this.getThreadDataAttr(emailId)
      if (!threadId) return
      var html = await this.fetchHTML("/mail/thread/" + encodeURIComponent(threadId) + "/subitems")
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
      this.animateLayoutShift(previousLayout)
      var self = this
      requestAnimationFrame(function () { self.finishThreadEnter(emailId) })
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
    tmp.innerHTML = item.html
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
      "&limit=80&selected=" +
      encodeURIComponent(selectedId) +
      "&view=" +
      encodeURIComponent(this.viewMode)
    var html = await this.fetchHTML(url)
    this.ingestHTML(html)

    var anchorPos = this.indexById.get(selectedId)
    if (anchorPos !== undefined) {
      this.container.scrollTop = this.offsetAtPosition(anchorPos)
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
        var badge = sidebarLinks[i].querySelector("[data-folder-unread]")
        if (badge) {
          badge.classList.remove("bg-sidebar-primary/20", "text-sidebar-primary")
          badge.classList.add("bg-sidebar-accent", "text-sidebar-foreground/80")
        }
      }
      var activeLink = sidebar.querySelector('a[hx-get="/folder/' + folderID + '"]')
      if (activeLink) {
        activeLink.classList.add("bg-sidebar-accent", "text-sidebar-primary", "font-medium")
        activeLink.classList.remove("text-sidebar-foreground")
        var activeBadge = activeLink.querySelector("[data-folder-unread]")
        if (activeBadge) {
          activeBadge.classList.remove("bg-sidebar-accent", "text-sidebar-foreground/80")
          activeBadge.classList.add("bg-sidebar-primary/20", "text-sidebar-primary")
        }
      }
    }
  } else if (e.state.email && e.state.email !== vml.selectedEmailId) {
    vml.selectedEmailId = e.state.email
    vml.prevFirst = null
    vml.prevLast = null
    vml.render()
    if (typeof htmx !== "undefined") {
      if (typeof showMailViewLoading === "function") showMailViewLoading()
      htmx.ajax("GET", "/email/" + e.state.email, "#mail-view")
    }
  }
})
