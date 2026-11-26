
(function () {
  if (window.__goferMailCalendarControlsReady) return;
  window.__goferMailCalendarControlsReady = true;

  // The width is saved with the other pane sizes, so the page is drawn at it.
  const WIDTH_SETTING = "mail_calendar_width";
  const MIN = 260;
  const MAX = 450;

  // Whether the calendar sits beside the panes rather than over them (see
  // place below and data-mail-calendar-over in input.css).
  function beside() {
    return !document.documentElement.hasAttribute("data-mail-calendar-over");
  }

  // Small screens show the calendar as a drawer, opened from the top bar.
  const SMALL = "(max-width: 1023.98px)";

  function small() {
    return matchMedia(SMALL).matches;
  }

  function drawerOpen() {
    return document.documentElement.hasAttribute("data-mail-calendar-drawer-open");
  }

  function clamp(value) {
    return Math.max(MIN, Math.min(MAX, Math.round(value)));
  }

  function width() {
    const saved = Number(typeof GoferSettings !== "undefined" ? GoferSettings.get(WIDTH_SETTING) : null);
    return saved >= MIN && saved <= MAX ? saved : 300;
  }

  function saveWidth(value) {
    if (typeof GoferSettings !== "undefined") GoferSettings.set(WIDTH_SETTING, String(value));
  }

  // Whether it's open is saved with the other pane settings. It's one choice
  // wherever the calendar sits, so moving between beside the panes and over
  // them never opens or closes it. It's open unless it was closed.
  const OPEN_SETTING = "mail_calendar_open";

  function preferredOpen() {
    const saved = typeof GoferSettings !== "undefined" ? GoferSettings.get(OPEN_SETTING) : null;
    return saved !== "false";
  }

  function apply(shell, open, size) {
    if (!shell) return;

    shell.dataset.open = String(open);

    shell.style.setProperty("--mail-calendar-width", size + "px");

    const toggle = shell.querySelector(
      "[data-mail-calendar-toggle]"
    );
    const grip = shell.querySelector(
      "[data-mail-calendar-grip]"
    );
    const aside = shell.querySelector(
      "[data-mail-calendar-sidebar]"
    );

    if (toggle) {
      toggle.setAttribute("aria-expanded", String(open));
      toggle.setAttribute(
        "aria-label",
        open ? shell.dataset.mailCalendarHideLabel : shell.dataset.mailCalendarShowLabel
      );
    }

    if (grip) {
      grip.setAttribute("aria-valuenow", String(size));
    }

    if (aside) {
      const shown = small() ? drawerOpen() : open;
      aside.setAttribute("aria-hidden", String(!shown));
      aside.inert = !shown;
    }
  }

  // A day's events, or the note that it has none.
  const AGENDA_ITEMS = "[data-calendar-event-trigger], [data-mail-calendar-agenda-empty]";

  function reducedMotion() {
    return matchMedia("(prefers-reduced-motion: reduce)").matches;
  }

  // A day's events arrive like a new mail list's rows (fanInElements in
  // virtual-scroll.js), when the calendar first loads and whenever another
  // day or month comes in. This runs before the new events are first drawn.
  function fanInEvents(shell) {
    const content = shell.querySelector("#mail-calendar-sidebar-content");
    if (!content || content.getAttribute("aria-busy") === "true" || content.dataset.fannedIn) return;

    content.dataset.fannedIn = "true";
    if (typeof fanInElements !== "function" || reducedMotion()) return;

    fanInElements(Array.from(content.querySelectorAll(AGENDA_ITEMS)), 0);
  }

  // Moving to another day or month works like switching mail folders: the
  // day's events fan out (fanOutElement in virtual-scroll.js) while the next
  // one loads, it goes in once they're gone, and its events fan in. A new
  // month's days also fade in from the side it came from, as on the Calendar
  // page (fadeInCalendarDays in app.js).
  let leaving = null;

  document.addEventListener("htmx:beforeRequest", function (event) {
    const detail = event.detail || {};
    const content = detail.target;
    if (!content || content.id !== "mail-calendar-sidebar-content" || content.getAttribute("aria-busy") === "true") return;

    // A newer pick takes over the fan-out already running.
    if (leaving) {
      leaving.xhr = detail.xhr;
      return;
    }

    const items = reducedMotion() || typeof fanOutElement !== "function"
      ? []
      : Array.from(content.querySelectorAll(AGENDA_ITEMS)).filter(function (item) { return typeof item.animate === "function"; });
    const animations = items.map(fanOutElement);
    leaving = {
      xhr: detail.xhr,
      month: content.dataset.mailCalendarMonth,
      animations: animations,
      done: Promise.all(animations.map(function (animation) { return animation.finished.catch(function () {}); })),
    };
  });

  // A pick that didn't load puts the day's events back.
  function cancelLeaving(xhr) {
    if (!leaving || (xhr && leaving.xhr !== xhr)) return;
    leaving.animations.forEach(function (animation) { animation.cancel(); });
    leaving = null;
  }

  ["htmx:sendError", "htmx:sendAbort", "htmx:timeout"].forEach(function (type) {
    document.addEventListener(type, function (event) {
      cancelLeaving(event.detail && event.detail.xhr);
    });
  });

  document.addEventListener("htmx:beforeSwap", function (event) {
    const detail = event.detail || {};
    if (!leaving || detail.xhr !== leaving.xhr) return;

    if (!detail.shouldSwap) {
      cancelLeaving();
      return;
    }

    const move = leaving;
    const response = detail.serverResponse;
    detail.shouldSwap = false;
    move.done.then(function () {
      if (leaving !== move) return;
      leaving = null;

      const content = document.getElementById("mail-calendar-sidebar-content");
      if (!content || typeof htmx === "undefined") return;
      htmx.swap(content, response, { swapStyle: "outerHTML" });

      const arrived = document.getElementById("mail-calendar-sidebar-content");
      const month = arrived && arrived.dataset.mailCalendarMonth;
      if (!month || !move.month || month === move.month || reducedMotion() || typeof fadeInCalendarDays !== "function") return;
      fadeInCalendarDays(arrived.querySelectorAll("[data-mail-calendar-day]"), month > move.month ? 1 : -1);
    });
  });

  function currentShell() {
    return document.querySelector("#main-content [data-mail-calendar-shell]");
  }

  // Opens or closes the drawer on small screens. The open state of wider
  // screens stays as it was.
  function setDrawer(open) {
    if (open === drawerOpen()) return;

    document.documentElement.toggleAttribute("data-mail-calendar-drawer-open", open);
    document.querySelectorAll("[data-mobile-mail-calendar]").forEach(function (button) {
      button.setAttribute("aria-expanded", String(open));
    });

    const shell = currentShell();
    if (shell) apply(shell, shell.dataset.open === "true", width());
  }

  function initialize() {
    const shell = currentShell();
    const root = document.documentElement;

    // Marks the page while Mail shows the calendar, so CSS shows the top
    // bar's calendar button there and nowhere else.
    if (root.hasAttribute("data-mail-calendar-in-page") !== !!shell) {
      root.toggleAttribute("data-mail-calendar-in-page", !!shell);
    }

    // Leaving Mail takes the calendar away, so its drawer can't stay open.
    if (!shell) {
      setDrawer(false);
      return;
    }

    observeMain();

    if (shell.dataset.controlsReady !== "yes") {
      shell.dataset.controlsReady = "yes";
      apply(shell, preferredOpen(), width());
    }

    fanInEvents(shell);

    // Initialize HTMX for calendar nodes inserted by SPA navigation.
    const pending = shell.querySelector(
      '#mail-calendar-sidebar-content[hx-trigger]'
    );

    if (pending && window.htmx && pending.dataset.htmxReady !== "yes") {
      pending.dataset.htmxReady = "yes";
      window.htmx.process(pending);
    }
  }

  // The top bar's calendar button opens and closes the drawer, and a tap on
  // the backdrop beside it closes it, as with the sidebar's drawer.
  document.addEventListener("click", function (event) {
    const target = event.target && event.target.closest ? event.target : null;
    if (!target) return;

    if (target.closest("[data-mobile-mail-calendar]")) {
      setDrawer(!drawerOpen());
    } else if (drawerOpen() && target.closest("[data-mobile-nav-close]")) {
      setDrawer(false);
    }
  });

  document.addEventListener("keydown", function (event) {
    if (event.key === "Escape" && drawerOpen()) setDrawer(false);
  });

  // The calendar keeps itself (hx-preserve) when Mail is drawn again, so it
  // doesn't load a second time. The empty placeholder of a Mail drawn with the
  // calendar off has the same id, though, and htmx would keep that instead of
  // the calendar coming in to replace it.
  document.addEventListener("htmx:oobBeforeSwap", function (event) {
    const target = event.detail && event.detail.target;
    if (!target || target.id !== "mail-calendar-sidebar-slot" || target.hasAttribute("data-mail-calendar-shell")) return;

    event.detail.fragment.querySelectorAll("[data-mail-calendar-shell][hx-preserve]").forEach(function (shell) {
      shell.removeAttribute("hx-preserve");
    });
  });

  window.addEventListener("popstate", function () {
    setDrawer(false);
  });

  matchMedia(SMALL).addEventListener("change", function () {
    setDrawer(false);
    place();
    const shell = currentShell();
    if (shell) apply(shell, shell.dataset.open === "true", width());
  });

  document.addEventListener("click", function (event) {
    const button = event.target.closest(
      "[data-mail-calendar-toggle]"
    );

    if (!button) return;

    const shell = button.closest(
      "[data-mail-calendar-shell]"
    );

    if (!shell) return;

    const open = shell.dataset.open !== "true";
    const before = shell.getBoundingClientRect().width;

    if (shell._mailCalendarSlide) shell._mailCalendarSlide.cancel();
    if (typeof GoferSettings !== "undefined") GoferSettings.set(OPEN_SETTING, String(open));
    apply(shell, open, width());
    slide(shell, before);
  });

  // Slides the calendar in or out like the panes do when switching sections.
  // While it slides away the panel stays drawn, then CSS hides it.
  function slide(shell, before) {
    if (typeof slideAppPanelWidth !== "function") return;

    const animation = slideAppPanelWidth(
      shell,
      before,
      shell.getBoundingClientRect().width
    );

    if (!animation) return;

    shell._mailCalendarSlide = animation;
    shell.dataset.mailCalendarSliding = "true";

    function done() {
      if (shell._mailCalendarSlide !== animation) return;
      shell._mailCalendarSlide = null;
      delete shell.dataset.mailCalendarSliding;
    }

    animation.addEventListener("finish", done);
    animation.addEventListener("cancel", done);
  }

  document.addEventListener("pointerdown", function (event) {
    const grip = event.target.closest(
      "[data-mail-calendar-grip]"
    );

    if (!grip || !beside() || event.button !== 0) return;

    const shell = grip.closest(
      "[data-mail-calendar-shell]"
    );

    if (!shell || shell.dataset.open !== "true") return;

    event.preventDefault();

    const origin = event.clientX;
    const initial = shell.getBoundingClientRect().width;
    let last = initial;

    function move(e) {
      last = clamp(initial + origin - e.clientX);
      apply(shell, true, last);
    }

    function end() {
      document.removeEventListener("pointermove", move);
      document.removeEventListener("pointerup", end);
      document.removeEventListener("pointercancel", end);
      window.removeEventListener("blur", end);

      saveWidth(clamp(last));
      grip.removeAttribute("data-dragging");
      document.body.style.userSelect = "";
    }

    // Keeps the grip highlighted while the pointer drifts off it.
    grip.setAttribute("data-dragging", "");
    document.body.style.userSelect = "none";

    document.addEventListener("pointermove", move);
    document.addEventListener("pointerup", end);
    document.addEventListener("pointercancel", end);
    window.addEventListener("blur", end);
  });

  document.addEventListener("keydown", function (event) {
    const grip = event.target.closest &&
      event.target.closest("[data-mail-calendar-grip]");

    if (!grip || !beside()) return;

    if (
      event.key !== "ArrowLeft" &&
      event.key !== "ArrowRight"
    ) return;

    event.preventDefault();

    const shell = grip.closest(
      "[data-mail-calendar-shell]"
    );

    const next = clamp(
      width() + (event.key === "ArrowLeft" ? 10 : -10)
    );

    saveWidth(next);
    apply(shell, true, next);
  });

  // Puts the calendar beside the panes or over them, by the same rule the
  // page was first drawn with (goferMailCalendarFits in saved-panel-size.js),
  // from the Mail area's width now and the saved list and calendar widths.
  // Small screens always have it over the panes.
  function place() {
    const main = document.getElementById("main-content");
    if (!main || !currentShell() || typeof goferMailCalendarFits !== "function") return;

    const list = typeof GoferSettings !== "undefined" ? GoferSettings.get("mail_list_width") : null;
    const over = small() || !goferMailCalendarFits(main.clientWidth, main.dataset.mailPaneLayout || "side", list, width());
    if (over === !beside()) return;

    document.documentElement.toggleAttribute("data-mail-calendar-over", over);
    const shell = currentShell();
    apply(shell, shell.dataset.open === "true", width());
  }

  // The Mail area changes width with the window and the sidebar, and the
  // calendar's room with the list's and its own saved widths.
  const resizeObserver = typeof ResizeObserver === "function" ? new ResizeObserver(place) : null;
  let observedMain = null;

  function observeMain() {
    const main = document.getElementById("main-content");
    if (!resizeObserver || !main || main === observedMain) return;
    if (observedMain) resizeObserver.unobserve(observedMain);
    resizeObserver.observe(main);
    observedMain = main;
  }

  document.body.addEventListener("gofer:settings-changed", function (event) {
    const key = event.detail && event.detail.key;
    if (key === "mail_list_width" || key === "mail_pane_layout" || key === WIDTH_SETTING) place();
  });

  const observer = new MutationObserver(initialize);
  observer.observe(document.body, {
    childList: true,
    subtree: true
  });

  initialize();
})();
