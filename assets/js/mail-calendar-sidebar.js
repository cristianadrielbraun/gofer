
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
      toggle.textContent = open ? "‹" : "›";
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
      aside.setAttribute("aria-hidden", String(!open));
      aside.inert = !open;
    }
  }

  function initialize() {
    const shell = document.querySelector(
      "#main-content [data-mail-calendar-shell]"
    );

    if (!shell) return;

    if (shell.dataset.controlsReady !== "yes") {
      shell.dataset.controlsReady = "yes";
      apply(shell, preferredOpen(), width());
    }

    // Initialize HTMX for calendar nodes inserted by SPA navigation.
    const pending = shell.querySelector(
      '#mail-calendar-sidebar-content[hx-trigger="load"]'
    );

    if (pending && window.htmx && pending.dataset.htmxReady !== "yes") {
      pending.dataset.htmxReady = "yes";
      window.htmx.process(pending);
    }
  }

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

    write(stateKey(), open);
    apply(shell, open, width());
  });

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
