(function () {
  if (window.__goferMailCardLayoutDialogReady) return
  window.__goferMailCardLayoutDialogReady = true

  function setupMailCardLayoutDialog() {
    var fieldIds = ["avatar", "thread", "from", "account", "to", "date", "subject", "preview", "labels", "attachment", "starred", "unread"]
    var zones = ["rail", "header", "meta", "body", "footer", "status", "corner", "hidden"]
    var visibleZones = ["rail", "header", "meta", "body", "footer", "status", "corner"]
    var fieldLabels = {
      avatar: "Avatar",
      thread: "Thread count",
      from: "Sender",
      account: "Account",
      to: "Recipient",
      date: "Date",
      subject: "Subject",
      preview: "Preview",
      labels: "Labels",
      attachment: "Attachment",
      starred: "Star",
      unread: "Unread dot",
    }
    var activeDrag = null
    var layoutTooltip = null
    var layoutTooltipTarget = null
    var layoutTooltipHideTimer = null

    function currentLayout() {
      if (typeof window.getMailCardLayout === "function") return window.getMailCardLayout()
      return {
        rail: ["avatar", "thread"],
        header: ["from"],
        meta: ["attachment", "date", "unread"],
        body: ["subject"],
        footer: ["preview", "labels"],
        status: [],
        corner: ["starred"],
        hidden: ["account", "to"],
      }
    }

    function defaultLayout() {
      if (typeof window.getDefaultMailCardLayout === "function") return window.getDefaultMailCardLayout()
      return {
        rail: ["avatar", "thread"],
        header: ["from"],
        meta: ["attachment", "date", "unread"],
        body: ["subject"],
        footer: ["preview", "labels"],
        status: [],
        corner: ["starred"],
        hidden: ["account", "to"],
      }
    }

    function visibleIdsFromLayout(layout) {
      if (typeof window.getMailCardVisibleFieldsFromLayout === "function") return window.getMailCardVisibleFieldsFromLayout(layout)
      var ids = []
      for (var i = 0; i < visibleZones.length; i++) {
        var zoneIds = layout[visibleZones[i]] || []
        for (var j = 0; j < zoneIds.length; j++) if (ids.indexOf(zoneIds[j]) === -1) ids.push(zoneIds[j])
      }
      return ids.length ? ids : ["subject"]
    }

    function serializeLayout(layout) {
      if (typeof window.serializeMailCardLayout === "function") return window.serializeMailCardLayout(layout)
      return zones.map(function (zone) { return zone + ":" + ((layout[zone] || []).join(",")) }).join("|")
    }

    function dialogRoot(target) {
      return target && target.closest ? target.closest("#mail-card-layout-dialog") : document.getElementById("mail-card-layout-dialog")
    }

    function preview(dialog) {
      return dialog && dialog.querySelector("[data-mail-card-layout-preview]")
    }

    function previewZone(dialog, zone) {
      var card = preview(dialog)
      return card ? card.querySelector('[data-mail-card-zone="' + zone + '"]') : null
    }

    function hiddenTray(dialog) {
      return dialog ? dialog.querySelector('[data-mail-card-layout-zone-items="hidden"]') : null
    }

    function dialogContent(dialog) {
      return dialog ? dialog.querySelector("[data-tui-dialog-content]") || dialog : null
    }

    function tooltipTokenFromTarget(target) {
      if (!target || !target.closest) return null
      var token = target.closest("[data-mail-card-layout-token]")
      var dialog = dialogRoot(token)
      if (!token || !dialog || !dialog.contains(token)) return null
      return token
    }

    function tokenTooltipText(token) {
      if (!token) return ""
      if (token.dataset.mailCardLayoutTooltip) return token.dataset.mailCardLayoutTooltip
      var id = token.dataset.mailCardLayoutToken || token.dataset.mailCardField || ""
      return fieldLabels[id] || id
    }

    function ensureLayoutTooltip(dialog) {
      var host = dialogContent(dialog)
      if (!host) return null
      if (layoutTooltip && layoutTooltip.parentElement !== host) {
        layoutTooltip.remove()
        layoutTooltip = null
      }
      if (layoutTooltip) return layoutTooltip

      layoutTooltip = document.createElement("div")
      layoutTooltip.id = "mail-card-layout-tooltip"
      layoutTooltip.className = "mail-card-layout-tooltip"
      layoutTooltip.setAttribute("role", "tooltip")
      layoutTooltip.setAttribute("data-tui-popover-content", "")
      layoutTooltip.setAttribute("data-tui-popover-open", "false")
      layoutTooltip.setAttribute("data-tui-popover-placement", "top")
      layoutTooltip.setAttribute("data-tui-popover-offset", "8")
      layoutTooltip.setAttribute("data-tui-popover-disable-clickaway", "false")
      layoutTooltip.setAttribute("data-tui-popover-disable-esc", "false")
      layoutTooltip.setAttribute("data-tui-popover-show-arrow", "true")
      layoutTooltip.setAttribute("data-tui-popover-hover-delay", "0")
      layoutTooltip.setAttribute("data-tui-popover-hover-out-delay", "0")
      layoutTooltip.setAttribute("data-tui-popover-exclusive", "false")
      layoutTooltip.style.visibility = "hidden"

      var content = document.createElement("div")
      content.className = "mail-card-layout-tooltip-content"
      layoutTooltip.appendChild(content)

      var arrow = document.createElement("div")
      arrow.setAttribute("data-tui-popover-arrow", "")
      layoutTooltip.appendChild(arrow)

      host.appendChild(layoutTooltip)
      return layoutTooltip
    }

    function positionLayoutTooltip(token) {
      if (!layoutTooltip || !token || !token.isConnected) {
        hideLayoutTooltip(true)
        return
      }

      var host = layoutTooltip.parentElement
      if (!host) {
        hideLayoutTooltip(true)
        return
      }

      var targetRect = token.getBoundingClientRect()
      var hostRect = host.getBoundingClientRect()
      var tooltipRect = layoutTooltip.getBoundingClientRect()
      var gap = 8
      var viewportLeft = targetRect.left + targetRect.width / 2 - tooltipRect.width / 2
      viewportLeft = Math.max(8, Math.min(window.innerWidth - tooltipRect.width - 8, viewportLeft))
      var viewportTop = targetRect.top - tooltipRect.height - gap
      var placement = "top"

      if (viewportTop < 8) {
        viewportTop = targetRect.bottom + gap
        placement = "bottom"
      }

      layoutTooltip.style.left = Math.round(viewportLeft - hostRect.left) + "px"
      layoutTooltip.style.top = Math.round(viewportTop - hostRect.top) + "px"
      layoutTooltip.setAttribute("data-tui-popover-placement", placement)

      var arrow = layoutTooltip.querySelector("[data-tui-popover-arrow]")
      if (arrow) {
        var arrowLeft = targetRect.left + targetRect.width / 2 - viewportLeft - 5
        arrow.style.left = Math.round(Math.max(8, Math.min(tooltipRect.width - 18, arrowLeft))) + "px"
        if (placement === "top") {
          arrow.style.top = ""
          arrow.style.bottom = "-5px"
        } else {
          arrow.style.top = "-5px"
          arrow.style.bottom = ""
        }
      }
    }

    function showLayoutTooltip(token) {
      if (activeDrag || !token) return
      var text = tokenTooltipText(token)
      if (!text) return
      var tooltip = ensureLayoutTooltip(dialogRoot(token))
      if (!tooltip) return

      window.clearTimeout(layoutTooltipHideTimer)
      if (layoutTooltipTarget && layoutTooltipTarget !== token) layoutTooltipTarget.removeAttribute("aria-describedby")
      layoutTooltipTarget = token
      token.setAttribute("aria-describedby", tooltip.id)
      var content = tooltip.querySelector(".mail-card-layout-tooltip-content")
      if (content) content.textContent = text
      tooltip.style.visibility = "hidden"
      tooltip.setAttribute("data-tui-popover-open", "false")
      positionLayoutTooltip(token)
      tooltip.style.visibility = "visible"
      tooltip.setAttribute("data-tui-popover-open", "true")
    }

    function hideLayoutTooltip(immediate) {
      window.clearTimeout(layoutTooltipHideTimer)
      if (layoutTooltipTarget) layoutTooltipTarget.removeAttribute("aria-describedby")
      layoutTooltipTarget = null
      if (!layoutTooltip) return
      layoutTooltip.setAttribute("data-tui-popover-open", "false")
      if (immediate || reducedMotion()) {
        layoutTooltip.style.visibility = "hidden"
        return
      }
      layoutTooltipHideTimer = window.setTimeout(function () {
        if (!layoutTooltipTarget && layoutTooltip) layoutTooltip.style.visibility = "hidden"
      }, 160)
    }

    function dropListFromTarget(target) {
      if (!target || !target.closest) return null
      var list = target.closest("[data-mail-card-layout-zone-items], [data-mail-card-zone]")
      var dialog = dialogRoot(list)
      if (!dialog || !list || !dialog.contains(list)) return null
      return list
    }

    function dropZoneName(list) {
      if (!list) return ""
      if (list.dataset.mailCardZone) return list.dataset.mailCardZone
      if (list.dataset.mailCardLayoutZoneItems) return list.dataset.mailCardLayoutZoneItems
      return ""
    }

    function tokenZone(token) {
      if (!token || !token.closest) return ""
      var previewList = token.closest("[data-mail-card-zone]")
      if (previewList) return previewList.dataset.mailCardZone || ""
      var list = token.closest("[data-mail-card-layout-zone-items]")
      return list ? list.dataset.mailCardLayoutZoneItems || "" : ""
    }

    function createToken(id) {
      var token = document.createElement("button")
      token.type = "button"
      token.draggable = false
      token.className = "mail-card-layout-token"
      token.dataset.mailCardLayoutToken = id
      token.dataset.mailCardLayoutTooltip = fieldLabels[id] || id
      token.setAttribute("aria-label", fieldLabels[id] || id)

      var grip = document.createElement("span")
      grip.className = "mail-card-layout-token-grip"
      grip.setAttribute("aria-hidden", "true")
      token.appendChild(grip)

      var label = document.createElement("span")
      label.className = "truncate"
      label.textContent = fieldLabels[id] || id
      token.appendChild(label)
      return token
    }

    function hiddenPreviewField(dialog, id) {
      var hidden = previewZone(dialog, "hidden")
      return hidden && hidden.querySelector('[data-mail-card-field="' + id + '"]')
    }

    function readEditorLayout(dialog) {
      var layout = {}
      var seen = {}
      for (var i = 0; i < zones.length; i++) {
        var zone = zones[i]
        layout[zone] = []
        var list = previewZone(dialog, zone)
        if (!list) continue
        var tokens = list.querySelectorAll("[data-mail-card-field]")
        for (var j = 0; j < tokens.length; j++) {
          var id = tokens[j].dataset.mailCardField
          if (fieldIds.indexOf(id) === -1 || seen[id]) continue
          layout[zone].push(id)
          seen[id] = true
        }
      }
      for (var k = 0; k < fieldIds.length; k++) {
        if (!seen[fieldIds[k]]) layout.hidden.push(fieldIds[k])
      }
      return layout
    }

    function visibleCount(dialog) {
      var layout = readEditorLayout(dialog)
      var seen = {}
      var count = 0
      for (var i = 0; i < visibleZones.length; i++) {
        var zoneIds = layout[visibleZones[i]] || []
        for (var j = 0; j < zoneIds.length; j++) {
          if (seen[zoneIds[j]]) continue
          seen[zoneIds[j]] = true
          count++
        }
      }
      return count
    }

    function applyLayoutToPreview(dialog, layout) {
      var card = preview(dialog)
      if (!card) return
      for (var z = 0; z < zones.length; z++) {
        var zone = zones[z]
        var target = previewZone(dialog, zone)
        if (!target) continue
        var ids = layout[zone] || []
        for (var i = 0; i < ids.length; i++) {
          var fields = card.querySelectorAll('[data-mail-card-field="' + ids[i] + '"]')
          for (var j = 0; j < fields.length; j++) target.appendChild(fields[j])
        }
      }
    }

    function decoratePreviewFields(dialog) {
      var card = preview(dialog)
      if (!card) return
      var fields = card.querySelectorAll("[data-mail-card-field]")
      for (var i = 0; i < fields.length; i++) {
        var id = fields[i].dataset.mailCardField
        fields[i].draggable = false
        fields[i].dataset.mailCardLayoutToken = id
        fields[i].dataset.mailCardLayoutTooltip = fieldLabels[id] || id
        fields[i].classList.add("mail-card-layout-preview-token")
        fields[i].setAttribute("aria-label", fieldLabels[id] || id)
        fields[i].removeAttribute("title")
      }
    }

    function renderHiddenTray(dialog, layout) {
      var tray = hiddenTray(dialog)
      if (!tray) return
      tray.textContent = ""
      var hidden = layout.hidden || []
      for (var i = 0; i < hidden.length; i++) tray.appendChild(createToken(hidden[i]))
    }

    function syncLimitState(dialog) {
      if (!dialog) return
      var max = typeof window.getMailCardFieldMax === "function" ? window.getMailCardFieldMax() : 10
      var count = visibleCount(dialog)
      var atLimit = count >= max
      var limit = dialog.querySelector("[data-mail-card-layout-limit]")
      if (limit) limit.textContent = count + " / " + max + " fields"

      var hidden = hiddenTray(dialog)
      var hiddenTokens = hidden ? hidden.querySelectorAll("[data-mail-card-layout-token]") : []
      for (var i = 0; i < hiddenTokens.length; i++) {
        hiddenTokens[i].draggable = false
        hiddenTokens[i].setAttribute("aria-disabled", atLimit ? "true" : "false")
        hiddenTokens[i].classList.toggle("mail-card-layout-token-disabled", atLimit)
        hiddenTokens[i].dataset.mailCardLayoutTooltip = atLimit
          ? "Remove another field first"
          : fieldLabels[hiddenTokens[i].dataset.mailCardLayoutToken] || hiddenTokens[i].dataset.mailCardLayoutToken || ""
        hiddenTokens[i].removeAttribute("title")
      }
    }

    function renderEditor(dialog, layout) {
      if (!dialog) return
      hideLayoutTooltip(true)
      layout = layout || currentLayout()
      applyLayoutToPreview(dialog, layout)
      decoratePreviewFields(dialog)
      renderHiddenTray(dialog, layout)
      syncLimitState(dialog)
    }

    function persistEditorLayout(dialog) {
      var layout = readEditorLayout(dialog)
      var visible = visibleIdsFromLayout(layout)
      if (!visible.length) return
      var serialized = serializeLayout(layout)
      if (window.GoferSettings) {
        GoferSettings.set("mail_card_layout", serialized)
        GoferSettings.set("mail_card_fields", visible.join(","))
      } else if (typeof window.applyMailCardLayout === "function") {
        window.applyMailCardLayout(serialized, visible.join(","), document)
      }
      renderEditor(dialog, layout)
    }

    function abortActiveDrag() {
      var drag = activeDrag
      if (!drag) return
      activeDrag = null
      restoreVisualToken(drag)
      if (drag.originParent) drag.originParent.insertBefore(drag.visualToken, drag.originNext || null)
      cleanupDragShell(drag)
    }

    function resetEditorLayout(dialog) {
      if (!dialog) return
      abortActiveDrag()
      renderEditor(dialog, defaultLayout())
      persistEditorLayout(dialog)
    }

    function canDropFromZone(fromZone, targetZone, dialog) {
      if (!fromZone || !targetZone) return false
      if (fromZone === targetZone) return true
      if (targetZone === "hidden") return fromZone === "hidden" || visibleCount(dialog) >= 1
      if (fromZone !== "hidden") return true
      var max = typeof window.getMailCardFieldMax === "function" ? window.getMailCardFieldMax() : 10
      return visibleCount(dialog) < max
    }

    function insertionTarget(list, event) {
      var tokens = Array.prototype.slice.call(list.querySelectorAll("[data-mail-card-layout-token]:not(.mail-card-layout-lifted-token)"))
      for (var i = 0; i < tokens.length; i++) {
        var rect = tokens[i].getBoundingClientRect()
        var beforeRow = event.clientY < rect.top + rect.height / 2
        var beforeColumn = event.clientY <= rect.bottom && event.clientX < rect.left + rect.width / 2
        if (beforeRow || beforeColumn) return tokens[i]
      }
      return null
    }

    function clearDropState(dialog) {
      var zonesEls = dialog ? dialog.querySelectorAll("[data-mail-card-layout-dropzone]") : []
      for (var i = 0; i < zonesEls.length; i++) zonesEls[i].classList.remove("mail-card-layout-dropzone-active", "mail-card-layout-dropzone-denied")
    }

    function reducedMotion() {
      return window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches
    }

    function clampPointerOffset(value, size) {
      if (!isFinite(value) || !isFinite(size) || size <= 0) return 0
      if (size <= 12) return size / 2
      return Math.max(6, Math.min(size - 6, value))
    }

    function createPlaceholder(rect) {
      var placeholder = document.createElement("span")
      placeholder.className = "mail-card-layout-placeholder"
      placeholder.setAttribute("aria-hidden", "true")
      placeholder.style.width = Math.max(8, Math.ceil(rect.width)) + "px"
      placeholder.style.height = Math.max(8, Math.ceil(rect.height)) + "px"
      return placeholder
    }

    function ensureDragLayer(dialog) {
      var content = dialog && dialog.querySelector("[data-tui-dialog-content]")
      if (!content) return null
      var layer = content.querySelector(":scope > .mail-card-layout-drag-layer")
      if (layer) return layer
      layer = document.createElement("div")
      layer.className = "mail-card-layout-drag-layer"
      layer.setAttribute("aria-hidden", "true")
      content.appendChild(layer)
      return layer
    }

    function visualTransform(drag, left, top, scale) {
      var layerRect = drag.layer.getBoundingClientRect()
      var x = left - layerRect.left
      var y = top - layerRect.top
      var rotation = drag.fromTray ? -1.2 : -0.7
      return "translate3d(" + Math.round(x) + "px," + Math.round(y) + "px,0) rotate(" + rotation + "deg) scale(" + scale + ")"
    }

    function positionVisualToken(drag, event) {
      if (!drag || !event) return
      drag.lastClientX = event.clientX
      drag.lastClientY = event.clientY
      var left = event.clientX - drag.offsetX
      var top = event.clientY - drag.offsetY
      drag.visualToken.style.transform = visualTransform(drag, left, top, drag.scale)
    }

    function restoreVisualToken(drag) {
      if (!drag || !drag.visualToken) return
      var token = drag.visualToken
      token.classList.remove("mail-card-layout-lifted-token", "mail-card-layout-lifted-token-raised", "mail-card-layout-lifted-token-dropping")
      if (drag.originalStyle === null) token.removeAttribute("style")
      else token.setAttribute("style", drag.originalStyle)
    }

    function markDropSettle(token) {
      if (!token || reducedMotion()) return
      token.classList.remove("mail-card-layout-drop-settle")
      void token.offsetWidth
      token.classList.add("mail-card-layout-drop-settle")
      window.setTimeout(function () {
        token.classList.remove("mail-card-layout-drop-settle")
      }, 220)
    }

    function listForDropZone(dialog, zone) {
      return zone === "hidden" ? hiddenTray(dialog) : previewZone(dialog, zone)
    }

    function movePlaceholderToList(drag, list, event) {
      if (!drag || !list) return false
      var zone = dropZoneName(list)
      var before = insertionTarget(list, event)
      var currentParent = drag.placeholder.parentElement
      if (before && before !== drag.placeholder && before.parentElement === list) {
        if (currentParent !== list || drag.placeholder.nextSibling !== before) list.insertBefore(drag.placeholder, before)
      } else if (currentParent !== list || drag.placeholder.nextSibling !== null) {
        list.appendChild(drag.placeholder)
      }
      drag.dropZone = zone
      return true
    }

    function updateDropTargetFromPoint(event) {
      if (!activeDrag || !event) return
      positionVisualToken(activeDrag, event)

      var target = document.elementFromPoint(event.clientX, event.clientY)
      var list = dropListFromTarget(target)
      var dialog = activeDrag.dialog
      clearDropState(dialog)
      activeDrag.dropZone = ""
      activeDrag.dropAllowed = false
      if (!list) return

      var zone = dropZoneName(list)
      var allowed = canDropFromZone(activeDrag.fromZone, zone, dialog)
      var dropzone = list.closest("[data-mail-card-layout-dropzone]")
      if (dropzone) {
        dropzone.classList.toggle("mail-card-layout-dropzone-active", allowed)
        dropzone.classList.toggle("mail-card-layout-dropzone-denied", !allowed)
      }
      if (!allowed) return

      activeDrag.dropAllowed = movePlaceholderToList(activeDrag, list, event)
    }

    function endBodyDragState(dialog) {
      document.body.classList.remove("mail-card-layout-drag-active")
      clearDropState(dialog)
    }

    function animateVisualTokenToRect(drag, rect, done) {
      var token = drag.visualToken
      var finish = function () {
        if (finish.done) return
        finish.done = true
        token.removeEventListener("transitionend", finish)
        done()
      }
      token.classList.remove("mail-card-layout-lifted-token-raised")
      token.classList.add("mail-card-layout-lifted-token-dropping")
      token.style.transition = reducedMotion()
        ? "none"
        : "transform 170ms cubic-bezier(0.2, 0, 0, 1), opacity 130ms ease, box-shadow 170ms ease, filter 170ms ease"
      token.style.transform = visualTransform(drag, rect.left, rect.top, 1)
      if (reducedMotion()) finish()
      else {
        token.addEventListener("transitionend", finish)
        window.setTimeout(finish, 230)
      }
    }

    function cleanupDragShell(drag) {
      if (drag.placeholder && drag.placeholder.parentElement) drag.placeholder.remove()
      if (drag.layer && !drag.layer.children.length) drag.layer.remove()
      endBodyDragState(drag.dialog)
    }

    function cancelDrag() {
      var drag = activeDrag
      if (!drag) return
      activeDrag = null
      animateVisualTokenToRect(drag, drag.originRect, function () {
        if (drag.fromTray) {
          restoreVisualToken(drag)
          if (drag.originParent) drag.originParent.insertBefore(drag.visualToken, drag.originNext || null)
        } else {
          restoreVisualToken(drag)
          if (drag.originParent) drag.originParent.insertBefore(drag.visualToken, drag.originNext || null)
        }
        cleanupDragShell(drag)
        renderEditor(drag.dialog, drag.originLayout)
      })
    }

    function commitDrag(zone) {
      var drag = activeDrag
      if (!drag) return
      var targetList = listForDropZone(drag.dialog, zone)
      if (!targetList || !drag.placeholder.parentElement) {
        cancelDrag()
        return
      }

      activeDrag = null
      var finalRect = drag.placeholder.getBoundingClientRect()
      animateVisualTokenToRect(drag, finalRect, function () {
        var settledToken = null

        if (drag.fromTray && zone === "hidden") {
          restoreVisualToken(drag)
          if (drag.placeholder.parentElement) drag.placeholder.parentElement.insertBefore(drag.visualToken, drag.placeholder)
        } else if (drag.fromTray) {
          restoreVisualToken(drag)
          drag.visualToken.remove()
          if (drag.fieldToken && drag.placeholder.parentElement) {
            drag.placeholder.parentElement.insertBefore(drag.fieldToken, drag.placeholder)
            settledToken = drag.fieldToken
          }
        } else if (zone === "hidden") {
          restoreVisualToken(drag)
          var hidden = previewZone(drag.dialog, "hidden")
          if (hidden) hidden.appendChild(drag.visualToken)
        } else {
          restoreVisualToken(drag)
          if (drag.placeholder.parentElement) {
            drag.placeholder.parentElement.insertBefore(drag.visualToken, drag.placeholder)
            settledToken = drag.visualToken
          }
        }

        cleanupDragShell(drag)
        if (drag.fromTray && zone === "hidden") {
          renderEditor(drag.dialog, drag.originLayout)
          return
        }
        persistEditorLayout(drag.dialog)
        markDropSettle(settledToken)
      })
    }

    function beginDrag(token, event) {
      var dialog = dialogRoot(token)
      if (!dialog) return

      var id = token.dataset.mailCardLayoutToken || token.dataset.mailCardField
      var fromTray = !!(token.closest && token.closest('[data-mail-card-layout-zone-items="hidden"]'))
      var fieldToken = fromTray ? hiddenPreviewField(dialog, id) : token
      var fromZone = fromTray ? "hidden" : tokenZone(token)
      if (!id || !fieldToken || !fromZone) return

      if (activeDrag) cancelDrag()
      event.preventDefault()
      hideLayoutTooltip(true)

      var rect = token.getBoundingClientRect()
      var placeholder = createPlaceholder(rect)
      var originParent = token.parentElement
      var originNext = token.nextSibling
      var layer = ensureDragLayer(dialog)
      if (!layer) return
      originParent.insertBefore(placeholder, token)

      activeDrag = {
        dialog: dialog,
        layer: layer,
        visualToken: token,
        fieldToken: fieldToken,
        id: id,
        fromTray: fromTray,
        fromZone: fromZone,
        originLayout: readEditorLayout(dialog),
        originParent: originParent,
        originNext: originNext,
        originRect: rect,
        placeholder: placeholder,
        offsetX: clampPointerOffset(event.clientX - rect.left, rect.width),
        offsetY: clampPointerOffset(event.clientY - rect.top, rect.height),
        originalStyle: token.getAttribute("style"),
        scale: 0.98,
        dropZone: "",
        dropAllowed: false,
        lastClientX: event.clientX,
        lastClientY: event.clientY,
      }

      token.classList.add("mail-card-layout-lifted-token")
      token.style.position = "absolute"
      token.style.left = "0"
      token.style.top = "0"
      token.style.width = Math.ceil(rect.width) + "px"
      token.style.height = Math.ceil(rect.height) + "px"
      token.style.margin = "0"
      token.style.zIndex = "9999"
      token.style.pointerEvents = "none"
      token.style.transition = "none"
      layer.appendChild(token)
      document.body.classList.add("mail-card-layout-drag-active")
      positionVisualToken(activeDrag, event)
      void token.offsetWidth

      window.requestAnimationFrame(function () {
        if (!activeDrag || activeDrag.visualToken !== token) return
        token.style.transition = reducedMotion() ? "none" : "transform 120ms cubic-bezier(0.16, 1, 0.3, 1), opacity 120ms ease, box-shadow 120ms ease, filter 120ms ease"
        activeDrag.scale = 1.035
        token.classList.add("mail-card-layout-lifted-token-raised")
        positionVisualToken(activeDrag, { clientX: activeDrag.lastClientX, clientY: activeDrag.lastClientY })
        window.setTimeout(function () {
          if (!activeDrag || activeDrag.visualToken !== token) return
          token.style.transition = reducedMotion() ? "none" : "opacity 120ms ease, box-shadow 120ms ease, filter 120ms ease"
        }, 135)
      })
    }

    document.body.addEventListener("click", function (e) {
      var open = e.target.closest && e.target.closest("[data-mail-card-layout-dialog-open]")
      if (open) {
        e.preventDefault()
        var dialog = document.getElementById("mail-card-layout-dialog")
        renderEditor(dialog, currentLayout())
        if (window.tui && window.tui.dialog) window.tui.dialog.open("mail-card-layout-dialog")
        return
      }

      var reset = e.target.closest && e.target.closest("[data-mail-card-layout-reset]")
      if (reset) {
        e.preventDefault()
        resetEditorLayout(dialogRoot(reset))
      }
    })

    document.body.addEventListener("pointerover", function (e) {
      var token = tooltipTokenFromTarget(e.target)
      if (!token || token === layoutTooltipTarget) return
      showLayoutTooltip(token)
    })

    document.body.addEventListener("pointerout", function (e) {
      if (!layoutTooltipTarget) return
      if (e.relatedTarget && layoutTooltipTarget.contains(e.relatedTarget)) return
      hideLayoutTooltip()
    })

    document.body.addEventListener("focusin", function (e) {
      var token = tooltipTokenFromTarget(e.target)
      if (token) showLayoutTooltip(token)
    })

    document.body.addEventListener("focusout", function (e) {
      if (layoutTooltipTarget && e.target === layoutTooltipTarget) hideLayoutTooltip()
    })

    document.body.addEventListener("pointerdown", function (e) {
      if (e.button !== undefined && e.button !== 0) return
      var token = e.target.closest && e.target.closest("[data-mail-card-layout-token]")
      if (!token) return
      hideLayoutTooltip(true)
      if (token.getAttribute("aria-disabled") === "true") {
        e.preventDefault()
        return
      }
      beginDrag(token, e)
    })

    document.addEventListener("pointermove", function (e) {
      if (!activeDrag) return
      e.preventDefault()
      updateDropTargetFromPoint(e)
    })

    document.addEventListener("pointerup", function (e) {
      if (!activeDrag) return
      e.preventDefault()
      updateDropTargetFromPoint(e)
      if (!activeDrag || !activeDrag.dropAllowed || !activeDrag.dropZone) {
        cancelDrag()
        return
      }
      commitDrag(activeDrag.dropZone)
    })

    document.addEventListener("pointercancel", function () {
      cancelDrag()
    })

    document.addEventListener("keydown", function (e) {
      if (e.key === "Escape") hideLayoutTooltip(true)
      if (e.key === "Escape" && activeDrag) {
        e.preventDefault()
        cancelDrag()
      }
    })

    document.addEventListener("scroll", function () {
      if (layoutTooltipTarget) positionLayoutTooltip(layoutTooltipTarget)
    }, true)

    window.addEventListener("resize", function () {
      if (layoutTooltipTarget) positionLayoutTooltip(layoutTooltipTarget)
    })
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", setupMailCardLayoutDialog)
  } else {
    setupMailCardLayoutDialog()
  }
})()
