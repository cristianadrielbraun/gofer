
(function () {
  if (window.__goferMailCalendarControlsReady) return;
  window.__goferMailCalendarControlsReady = true;

  const WIDTH_KEY = "gofer.mailCalendar.width.v2";
  const WIDE_KEY = "gofer.mailCalendar.open.wide.v2";
  const NARROW_KEY = "gofer.mailCalendar.open.narrow.v2";
  const MIN = 260;
  const MAX = 450;

  function read(key) {
    try { return localStorage.getItem(key); }
    catch (_) { return null; }
  }

  function write(key, value) {
    try { localStorage.setItem(key, String(value)); }
    catch (_) {}
  }

  function wide() {
    return matchMedia("(min-width: 1536px)").matches;
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
    const saved = Number(read(WIDTH_KEY));
    return saved >= MIN && saved <= MAX ? saved : 300;
  }

  function stateKey() {
    return wide() ? WIDE_KEY : NARROW_KEY;
  }

  function preferredOpen() {
    const saved = read(stateKey());
    return saved === null ? wide() : saved === "true";
  }

  function apply(shell, open, size) {
    if (!shell) return;

    shell.dataset.open = String(open);

    if (wide()) {
      shell.style.setProperty("--mail-calendar-width", size + "px");
    } else {
      shell.style.removeProperty("--mail-calendar-width");
    }

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

    if (shell.dataset.controlsReady !== "yes") {
      shell.dataset.controlsReady = "yes";
      apply(shell, preferredOpen(), width());
    }

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

  window.addEventListener("popstate", function () {
    setDrawer(false);
  });

  matchMedia(SMALL).addEventListener("change", function () {
    setDrawer(false);
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
    write(stateKey(), open);
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

    if (!grip || !wide() || event.button !== 0) return;

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

      write(WIDTH_KEY, clamp(last));
      document.body.style.userSelect = "";
    }

    document.body.style.userSelect = "none";

    document.addEventListener("pointermove", move);
    document.addEventListener("pointerup", end);
    document.addEventListener("pointercancel", end);
    window.addEventListener("blur", end);
  });

  document.addEventListener("keydown", function (event) {
    const grip = event.target.closest &&
      event.target.closest("[data-mail-calendar-grip]");

    if (!grip || !wide()) return;

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

    write(WIDTH_KEY, next);
    apply(shell, true, next);
  });

  matchMedia("(min-width: 1536px)")
    .addEventListener("change", function () {
      document.querySelectorAll(
        "#main-content [data-mail-calendar-shell]"
      ).forEach(function (shell) {
        apply(shell, preferredOpen(), width());
      });
    });

  const observer = new MutationObserver(initialize);
  observer.observe(document.body, {
    childList: true,
    subtree: true
  });

  initialize();
})();
