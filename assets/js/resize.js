var initResizeHandles;

(function () {
  function getPanel(el) {
    return el.previousElementSibling;
  }

  function getSize(el) {
    return el.getBoundingClientRect().width;
  }

  function setSize(el, w) {
    el.style.width = w + "px";
  }

  function clamp(v, min, max) {
    return Math.max(min, Math.min(max, v));
  }

  function settingKey(panelName) {
    if (panelName === "sidebar") return "sidebar_width";
    if (panelName === "maillist") return "mail_list_width";
    return panelName + "_width";
  }

  function getBounds(panelName) {
    var vw = window.innerWidth;

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
    var startX = e.clientX || (e.touches && e.touches[0].clientX) || 0;
    var startW = getSize(panel);

    document.body.style.cursor = "col-resize";
    document.body.style.userSelect = "none";
    handle.classList.add("active");

    function onMove(ev) {
      var cx = ev.clientX || (ev.touches && ev.touches[0].clientX) || 0;
      var delta = cx - startX;
      var b = getBounds(panelName);
      setSize(panel, clamp(startW + delta, b.min, b.max));
    }

    function onUp() {
      document.body.style.cursor = "";
      document.body.style.userSelect = "";
      handle.classList.remove("active");
      var b = getBounds(panelName);
      var finalW = Math.round(clamp(getSize(panel), b.min, b.max));
      setSize(panel, finalW);
      if (typeof GoferSettings !== "undefined") {
        GoferSettings.set(settingKey(panelName), String(finalW));
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
          setSize(getPanel(h), clamp(parseInt(saved, 10), b.min, b.max));
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
