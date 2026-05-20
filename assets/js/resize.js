var initResizeHandles;

(function () {
  function getPanel(el) {
    return el.previousElementSibling;
  }

  function isStackedMailList(panelName) {
    var main = document.getElementById("main-content");
    return panelName === "maillist" && main && main.dataset.mailPaneLayout === "stacked";
  }

  function getSize(el, axis) {
    var rect = el.getBoundingClientRect();
    return axis === "y" ? rect.height : rect.width;
  }

  function setSize(el, size, axis) {
    if (axis === "y") el.style.height = size + "px";
    else el.style.width = typeof size === "string" ? size : size + "px";
  }

  function isSideMailList(panelName) {
    return panelName === "maillist" && !isStackedMailList(panelName);
  }

  function mailListWidthCSS(value) {
    var raw = String(value || "").trim();
    if (raw.charAt(raw.length - 1) === "%") {
      var percent = parseFloat(raw);
      if (!isNaN(percent) && percent > 0) {
        return "clamp(300px," + percent + "%,calc(100% - 300px))";
      }
    }

    var px = parseFloat(raw);
    if (!isNaN(px) && px > 0) return Math.max(300, px) + "px";
    return "clamp(300px,50%,calc(100% - 300px))";
  }

  function percentSize(panel, size) {
    var parent = panel && panel.parentElement;
    var parentSize = parent ? parent.getBoundingClientRect().width : 0;
    if (!parentSize) return null;
    var percent = (size / parentSize) * 100;
    return Math.max(1, Math.min(99, percent)).toFixed(4).replace(/\.0+$/, "").replace(/(\.\d*?)0+$/, "$1") + "%";
  }

  function clamp(v, min, max) {
    if (max === null || max === undefined) return Math.max(min, v);
    return Math.max(min, Math.min(max, v));
  }

  function settingKey(panelName) {
    if (panelName === "sidebar") return "sidebar_width";
    if (panelName === "maillist") return isStackedMailList(panelName) ? "mail_list_height" : "mail_list_width";
    return panelName + "_width";
  }

  function getBounds(panelName) {
    var vw = window.innerWidth;

    if (isStackedMailList(panelName)) {
      return { min: 180, max: null };
    }

    if (panelName === "sidebar") {
      return { min: 180, max: Math.min(400, vw * 0.25) };
    }

    if (panelName === "maillist") {
      var list = document.getElementById("mail-list");
      var parent = list && list.parentElement;
      var parentWidth = parent ? parent.getBoundingClientRect().width : 0;
      return {
        min: 300,
        max: parentWidth ? Math.max(300, parentWidth - 300) : null,
      };
    }

    return { min: 300, max: null };
  }

  function onStart(e) {
    e.preventDefault();
    var handle = e.currentTarget;
    var panel = getPanel(handle);
    var panelName = handle.dataset.panel;
    var axis = isStackedMailList(panelName) ? "y" : "x";
    var startX = e.clientX || (e.touches && e.touches[0].clientX) || 0;
    var startY = e.clientY || (e.touches && e.touches[0].clientY) || 0;
    var startSize = getSize(panel, axis);

    document.body.style.cursor = axis === "y" ? "row-resize" : "col-resize";
    document.body.style.userSelect = "none";
    handle.classList.add("active");

    function onMove(ev) {
      var cx = ev.clientX || (ev.touches && ev.touches[0].clientX) || 0;
      var cy = ev.clientY || (ev.touches && ev.touches[0].clientY) || 0;
      var delta = axis === "y" ? cy - startY : cx - startX;
      var b = getBounds(panelName);
      setSize(panel, clamp(startSize + delta, b.min, b.max), axis);
    }

    function onUp() {
      document.body.style.cursor = "";
      document.body.style.userSelect = "";
      handle.classList.remove("active");
      var b = getBounds(panelName);
      var finalSize = Math.round(clamp(getSize(panel, axis), b.min, b.max));
      var settingValue = String(finalSize);
      if (axis === "x" && isSideMailList(panelName)) {
        settingValue = percentSize(panel, finalSize) || settingValue;
        setSize(panel, mailListWidthCSS(settingValue), axis);
      } else {
        setSize(panel, finalSize, axis);
      }
      if (typeof GoferSettings !== "undefined") {
        GoferSettings.set(settingKey(panelName), settingValue);
      }
      document.removeEventListener("mousemove", onMove);
      document.removeEventListener("mouseup", onUp);
      document.removeEventListener("touchmove", onMove);
      document.removeEventListener("touchend", onUp);
    }

    document.addEventListener("mousemove", onMove);
    document.addEventListener("mouseup", onUp);
    document.addEventListener("touchmove", onMove, { passive: false });
    document.addEventListener("touchend", onUp);
  }

  initResizeHandles = function () {
    document.querySelectorAll(".resize-handle").forEach(function (h) {
      var name = h.dataset.panel;
      if (typeof GoferSettings !== "undefined") {
        var saved = GoferSettings.get(settingKey(name));
        if (saved) {
          var b = getBounds(name);
          var panel = getPanel(h);
          if (isSideMailList(name)) {
            setSize(panel, mailListWidthCSS(saved), "x");
          } else {
            setSize(panel, clamp(parseInt(saved, 10), b.min, b.max), isStackedMailList(name) ? "y" : "x");
          }
        }
      }
      if (h._resizeBound) return;
      h._resizeBound = true;
      h.addEventListener("mousedown", onStart);
      h.addEventListener("touchstart", onStart, { passive: false });
    });
  };

  initResizeHandles();
})();
