var initResizeHandles;
var measureAppPanels;
var slideAppPanels;
var appPanelsSlideRemaining;

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

  function baseSettingKey(panelName) {
    if (panelName === "sidebar") return "sidebar_width";
    if (panelName === "maillist") return isStackedMailList(panelName) ? "mail_list_height" : "mail_list_width";
    return panelName + "_width";
  }

  // Mail, contacts and calendar each save their own sizes.
  function settingKey(panelName) {
    return GoferSettings.panelKey(baseSettingKey(panelName));
  }

  function savedSize(panelName) {
    var saved = GoferSettings.panelSetting(baseSettingKey(panelName));
    // The sidebar stays on screen across sections, so one without a saved
    // width goes back to the default rather than keeping the last section's.
    if (!saved && panelName === "sidebar") return "256";
    return saved;
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

  // The sidebar's bounds depend on the viewport, so a phone would clamp the
  // saved width down to the minimum. Phones show the drawer at its own width.
  var phoneQuery = window.matchMedia("(max-width: 1023.98px)");

  initResizeHandles = function () {
    document.querySelectorAll(".resize-handle").forEach(function (h) {
      var name = h.dataset.panel;
      if (typeof GoferSettings !== "undefined" && !(name === "sidebar" && phoneQuery.matches)) {
        var saved = savedSize(name);
        if (saved) {
          var panel = getPanel(h);
          if (isSideMailList(name)) {
            setSize(panel, mailListWidthCSS(saved), "x");
          } else {
            // Only these need the bounds, which measure the page: measuring
            // them for every handle laid it out each time a list arrived.
            var b = getBounds(name);
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

  phoneQuery.addEventListener("change", function (e) {
    if (!e.matches) initResizeHandles();
  });

  // Switching sections slides the sidebar and the pane divider to the new
  // section's sizes rather than jumping. The divider only slides when both
  // sections use the same layout; otherwise the panes are rearranged anyway.
  var SLIDE_MS = 260;
  var SLIDE_EASING = "cubic-bezier(0.2, 0.8, 0.2, 1)";
  var listSlide = null;
  var listPlaced = null;
  var slides = [];

  // Marks the page while panels slide, so work that follows pane sizes, like
  // the calendar's layout, can wait for the end instead of running each frame.
  function trackSlide(animation) {
    slides.push(animation);
    document.documentElement.setAttribute("data-panels-sliding", "");
    function done() {
      if (slides.indexOf(animation) === -1) return;
      slides = slides.filter(function (other) { return other !== animation; });
      if (slides.length) return;
      document.documentElement.removeAttribute("data-panels-sliding");
      document.dispatchEvent(new CustomEvent("gofer:panels-slid"));
    }
    animation.addEventListener("finish", done);
    animation.addEventListener("cancel", done);
    // A slide never holds the calendar back for long, even if its element
    // leaves the page without the animation reporting an end.
    setTimeout(done, SLIDE_MS * 3);
    return animation;
  }

  function canSlide() {
    return !phoneQuery.matches && typeof GoferSettings !== "undefined" &&
      !window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  }

  function slideKeyframes(prop, from, to) {
    var start = {}, end = {};
    start[prop] = from + "px";
    end[prop] = to + "px";
    return [start, end];
  }

  function startPanelSlide(element, prop, from, to, startTime) {
    var animation = element.animate(slideKeyframes(prop, from, to), { duration: SLIDE_MS, easing: SLIDE_EASING });
    // Resolve the clock before a fast response can replace the loading pane.
    // Otherwise its first animation can still be pending at the handover.
    if (startTime !== null) animation.startTime = startTime;
    return trackSlide(animation);
  }

  // Milliseconds until every running slide has ended.
  appPanelsSlideRemaining = function () {
    return slides.reduce(function (longest, animation) {
      var timing = animation.effect && animation.effect.getComputedTiming();
      if (!timing || animation.playState === "finished") return longest;
      return Math.max(longest, (timing.endTime || 0) - (timing.localTime || 0));
    }, 0);
  };

  measureAppPanels = function () {
    var aside = document.querySelector("aside");
    var list = document.getElementById("mail-list");
    return {
      sidebar: aside ? aside.getBoundingClientRect().width : 0,
      layout: isStackedMailList("maillist") ? "stacked" : "side",
      list: list ? list.getBoundingClientRect() : null,
    };
  };

  slideAppPanels = function (before, section) {
    if (!before || !canSlide()) return;
    var aside = document.querySelector("aside");
    var list = document.getElementById("mail-list");
    var layout = GoferSettings.paneLayout(section);
    var slideList = list && before.list && layout === before.layout;
    var placeList = list && !phoneQuery.matches;
    var prop = layout === "stacked" ? "height" : "width";

    // Settle on the final sizes first, so the list is measured against the
    // sidebar's final width, then animate from where everything was.
    var sidebarTo = 0;
    if (aside && before.sidebar) {
      var b = getBounds("sidebar");
      sidebarTo = clamp(parseInt(GoferSettings.panelSetting("sidebar_width", section), 10) || 256, b.min, b.max);
      aside.style.width = sidebarTo + "px";
    }
    // The list takes the new section's size even when the layout changes, so
    // it doesn't jump a second time when the section finishes loading.
    var listTo = 0;
    if (placeList) {
      if (prop === "height") {
        var height = parseInt(GoferSettings.panelSetting("mail_list_height", section), 10);
        if (height > 0) list.style.height = Math.max(180, height) + "px";
      } else {
        list.style.height = "";
        list.style.width = mailListWidthCSS(GoferSettings.panelSetting("mail_list_width", section) || "50%");
      }
      listTo = list.getBoundingClientRect()[prop];
      listPlaced = { el: list, section: section, width: list.style.width, height: list.style.height };
    }

    var startTime = document.timeline ? document.timeline.currentTime : null;
    if (sidebarTo && Math.abs(sidebarTo - before.sidebar) > 1) {
      startPanelSlide(aside, "width", before.sidebar, sidebarTo, startTime);
    }
    if (slideList && Math.abs(listTo - before.list[prop]) > 1) {
      listSlide = {
        el: list,
        prop: prop,
        from: before.list[prop],
        to: listTo,
        animation: startPanelSlide(list, prop, before.list[prop], listTo, startTime),
      };
    }
  };

  // The loaded section replaces the list element, which would drop its size
  // until the handles settle and cut a slide short when the response is quick.
  // The new element takes the size at once and finishes the slide.
  function continueListSlide() {
    var list = document.getElementById("mail-list");
    if (listPlaced && list && list !== listPlaced.el) {
      if (list.dataset.panelSection === listPlaced.section) {
        list.style.width = listPlaced.width;
        list.style.height = listPlaced.height;
      }
      listPlaced = null;
    }
    if (!listSlide || !list || list === listSlide.el) return;
    var slide = listSlide;
    listSlide = null;
    if (slide.animation.playState !== "running") return;
    // The same animation, picked up at the same moment, so the handover has
    // no change of speed.
    var animation = list.animate(slideKeyframes(slide.prop, slide.from, slide.to), { duration: SLIDE_MS, easing: SLIDE_EASING });
    // Copying currentTime leaves a newly created animation pending: its clock
    // starts on a later frame, adding a pause at every pane replacement. Keep
    // the original timeline anchor so loading and loaded panes finish together.
    if (slide.animation.startTime !== null) animation.startTime = slide.animation.startTime;
    else animation.currentTime = slide.animation.currentTime;
    listSlide = { el: list, prop: slide.prop, from: slide.from, to: slide.to, animation: trackSlide(animation) };
    slide.animation.cancel();
  }

  document.addEventListener("htmx:afterSwap", continueListSlide);
})();
