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
    else el.style.width = size + "px";
  }

  function clamp(v, min, max) {
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
      var vh = window.innerHeight || 800;
      return { min: 180, max: Math.min(760, vh * 0.65) };
    }

    if (panelName === "sidebar") {
      return { min: 180, max: Math.min(400, vw * 0.25) };
    }

    return {
      min: 300,
      max: Math.min(1200, vw * 0.55),
    };
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
      setSize(panel, finalSize, axis);
      if (typeof GoferSettings !== "undefined") {
        GoferSettings.set(settingKey(panelName), String(finalSize));
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
          setSize(getPanel(h), clamp(parseInt(saved, 10), b.min, b.max), isStackedMailList(name) ? "y" : "x");
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
