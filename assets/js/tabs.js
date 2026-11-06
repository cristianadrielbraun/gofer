(function () {
  "use strict";

  function getTabsContainer(tabsId) {
    return document.querySelector(
      `[data-tui-tabs][data-tui-tabs-id="${tabsId}"]`,
    );
  }

  function getTabList(container, tabsId) {
    return container.querySelector(
      `[data-tui-tabs-list][data-tui-tabs-id="${tabsId}"]`,
    );
  }

  function getActiveTrigger(container, tabsId, value) {
    return container.querySelector(
      `[data-tui-tabs-trigger][data-tui-tabs-id="${tabsId}"][data-tui-tabs-value="${value}"]`,
    );
  }

  // A list's indicator is placed again whenever the list changes size: when
  // it is first shown after starting out hidden, its font loads, or the page
  // narrows. Nothing else needs to measure it.
  const listResizes =
    typeof ResizeObserver === "function"
      ? new ResizeObserver((entries) => {
          entries.forEach((entry) => {
            const list = entry.target;
            const tabsId = list.getAttribute("data-tui-tabs-id");
            const container = list.closest("[data-tui-tabs]");
            if (!tabsId || !container || !list.isConnected) return;
            const active = container.querySelector(
              `[data-tui-tabs-trigger][data-tui-tabs-id="${tabsId}"][data-tui-tabs-state="active"]`,
            );
            if (active) {
              syncSlidingIndicator(container, tabsId, active.getAttribute("data-tui-tabs-value"), false);
            }
          });
        })
      : null;

  // Whether lists without an indicator yet are statically positioned, read
  // for all of them before prepareSlidingIndicator writes to any.
  function staticLists(lists) {
    return lists.map(
      (list) => !list.querySelector("[data-tui-tabs-indicator]") && getComputedStyle(list).position === "static",
    );
  }

  function prepareSlidingIndicator(list, isStatic) {
    let indicator = list.querySelector("[data-tui-tabs-indicator]");
    if (indicator) return indicator;
    if (listResizes) listResizes.observe(list);

    if (isStatic === undefined ? getComputedStyle(list).position === "static" : isStatic) {
      list.style.position = "relative";
    }

    indicator = document.createElement("div");
    indicator.setAttribute("data-tui-tabs-indicator", "true");
    indicator.style.position = "absolute";
    indicator.style.left = "0";
    indicator.style.top = "3px";
    indicator.style.height = "calc(100% - 6px)";
    indicator.style.borderRadius = "calc(var(--radius) - 3px)";
    indicator.style.background = "var(--background)";
    indicator.style.border = "1px solid var(--border)";
    indicator.style.boxShadow = "var(--shadow-card)";
    indicator.style.transition = "transform 220ms ease, width 220ms ease";
    indicator.style.willChange = "transform, width";
    indicator.style.pointerEvents = "none";
    indicator.style.zIndex = "0";

    list.prepend(indicator);
    return indicator;
  }

  // Where a list's indicator goes to show value, read from the page.
  function measureIndicator(container, tabsId, value) {
    const list = getTabList(container, tabsId);
    if (!list) return null;
    const indicator = list.querySelector("[data-tui-tabs-indicator]");
    const activeTrigger = getActiveTrigger(container, tabsId, value);
    if (!indicator || !activeTrigger) return null;
    const listRect = list.getBoundingClientRect();
    const triggerRect = activeTrigger.getBoundingClientRect();
    return {
      indicator,
      value,
      left: triggerRect.left - listRect.left,
      width: triggerRect.width,
    };
  }

  // Moves indicators to their measured spots. Without animate they jump
  // there, all with one layout of the page. A list measured while hidden is
  // 0 wide and isn't marked placed, so showing it places it again.
  function placeIndicators(spots, animate) {
    if (!spots.length) return;
    const transitions = spots.map((spot) => spot.indicator.style.transition);
    spots.forEach((spot) => {
      spot.indicator.dataset.tuiTabsPlaced = spot.width > 0 ? spot.value : "";
      if (!animate) spot.indicator.style.transition = "none";
      spot.indicator.style.width = `${spot.width}px`;
      spot.indicator.style.transform = `translateX(${spot.left}px)`;
    });
    if (animate) return;
    void spots[0].indicator.offsetHeight;
    spots.forEach((spot, i) => {
      spot.indicator.style.transition = transitions[i];
    });
  }

  function syncSlidingIndicator(container, tabsId, value, animate) {
    const list = getTabList(container, tabsId);
    if (!list) return false;
    prepareSlidingIndicator(list);
    const spot = measureIndicator(container, tabsId, value);
    if (!spot) return false;
    placeIndicators([spot], animate);
    return true;
  }

  function resetTriggerChrome(trigger, isActive) {
    trigger.style.position = "relative";
    trigger.style.zIndex = "1";
    trigger.style.background = isActive ? "transparent" : "";
    trigger.style.boxShadow = "none";
    trigger.style.borderColor = "transparent";
  }

  function shouldAnimateContent(container, animate) {
    if (!animate || !container.hasAttribute("data-tui-tabs-animate-content")) {
      return false;
    }
    return !(
      window.matchMedia &&
      window.matchMedia("(prefers-reduced-motion: reduce)").matches
    );
  }

  function updateContentState(container, tabsId, value, animate) {
    const contents = Array.from(
      container.querySelectorAll(
        `[data-tui-tabs-content][data-tui-tabs-id="${tabsId}"]`,
      ),
    );
    const target = contents.find(
      (content) => content.getAttribute("data-tui-tabs-value") === value,
    );
    if (!target) return;

    const changed = target.classList.contains("hidden");

    contents.forEach((content) => {
      const isActive = content === target;
      content.setAttribute(
        "data-tui-tabs-state",
        isActive ? "active" : "inactive",
      );
      content.classList.toggle("hidden", !isActive);
    });

    if (animate && changed && typeof animateSectionContent === "function") {
      animateSectionContent(target);
    }
  }

  // Update tab state
  function setActiveTab(tabsId, value, animate = true) {
    const container = getTabsContainer(tabsId);
    if (!container) return;
    applyTabState(container, tabsId, value, animate);
    // An indicator already on value has nowhere to go, so nothing is
    // measured; its list's ResizeObserver follows any change of size.
    const list = getTabList(container, tabsId);
    const indicator = list && list.querySelector("[data-tui-tabs-indicator]");
    if (indicator && indicator.dataset.tuiTabsPlaced === value) return;
    queueIndicator(container, tabsId, value, animate);
  }

  // Indicators move in the next frame, all measured together and then
  // placed, so code that sets many tabs in a row, such as each message of a
  // conversation, never makes the page lay out in between.
  const queuedIndicators = new Map();
  let indicatorFrame = 0;

  function queueIndicator(container, tabsId, value, animate) {
    queuedIndicators.set(container, { tabsId, value, animate });
    if (indicatorFrame) return;
    indicatorFrame = requestAnimationFrame(() => {
      indicatorFrame = 0;
      const jobs = Array.from(queuedIndicators);
      queuedIndicators.clear();
      const lists = jobs
        .map(([container, job]) => container.isConnected && getTabList(container, job.tabsId))
        .filter(Boolean);
      const statics = staticLists(lists);
      lists.forEach((list, i) => prepareSlidingIndicator(list, statics[i]));
      const jumps = [];
      const slides = [];
      jobs.forEach(([container, job]) => {
        if (!container.isConnected) return;
        const spot = measureIndicator(container, job.tabsId, job.value);
        if (spot) (job.animate ? slides : jumps).push(spot);
      });
      placeIndicators(jumps, false);
      placeIndicators(slides, true);
    });
  }

  // Marks value's trigger and content active, without measuring anything.
  function applyTabState(container, tabsId, value, animate) {
    const animateContent = shouldAnimateContent(container, animate);

    // Update all triggers with this tabs-id
    container
      .querySelectorAll(`[data-tui-tabs-trigger][data-tui-tabs-id="${tabsId}"]`)
      .forEach((trigger) => {
        const isActive = trigger.getAttribute("data-tui-tabs-value") === value;
        trigger.setAttribute(
          "data-tui-tabs-state",
          isActive ? "active" : "inactive",
        );
        resetTriggerChrome(trigger, isActive);
      });

    updateContentState(container, tabsId, value, animateContent);
  }

  // Click handler
  document.addEventListener("click", (e) => {
    const trigger = e.target.closest("[data-tui-tabs-trigger]");
    if (!trigger) return;

    const tabsId = trigger.getAttribute("data-tui-tabs-id");
    const value = trigger.getAttribute("data-tui-tabs-value");
    if (tabsId && value) {
      const container = getTabsContainer(tabsId);
      const isLocalTabs = container && container.hasAttribute("data-tui-tabs-local");
      setActiveTab(tabsId, value, true);

      if (!isLocalTabs && window.location.pathname.startsWith("/settings")) {
        var url = "/settings/" + value;
        if (window.location.pathname !== url) {
          history.pushState({ settingsTab: value }, "", url);
        }
      } else if (!isLocalTabs && window.location.pathname.startsWith("/admin/avatars")) {
        var adminPath = value === "overview" ? "/admin/avatars/" : "/admin/avatars/" + value;
        if (window.location.pathname !== adminPath) {
          history.pushState({ adminAvatarTab: value }, "", adminPath + window.location.search);
        }
      }
    }
  });

  // Initialize active states. Every tabs is set up first, then all are
  // measured, then their indicators placed, so a conversation whose messages
  // each have tabs lays out the page once, not once per message.
  function setupInitialStates(force = false) {
    const fresh = [];
    document.querySelectorAll("[data-tui-tabs]").forEach((container) => {
      const tabsId = container.getAttribute("data-tui-tabs-id");
      if (!tabsId) return;
      if (!force && container.getAttribute("data-tui-tabs-initialized") === "true") return;

      // Find active trigger or use first
      const activeTrigger =
        container.querySelector(
          `[data-tui-tabs-trigger][data-tui-tabs-state="active"]`,
        ) || container.querySelector(`[data-tui-tabs-trigger]`);

      if (activeTrigger) {
        const value = activeTrigger.getAttribute("data-tui-tabs-value");
        applyTabState(container, tabsId, value, false);
        if (getTabList(container, tabsId)) fresh.push([container, tabsId, value]);
        container.setAttribute("data-tui-tabs-initialized", "true");
      }
    });
    const lists = fresh.map(([container, tabsId]) => getTabList(container, tabsId));
    const statics = staticLists(lists);
    lists.forEach((list, i) => prepareSlidingIndicator(list, statics[i]));
    const spots = fresh
      .map(([container, tabsId, value]) => measureIndicator(container, tabsId, value))
      .filter(Boolean);
    placeIndicators(spots, false);
  }

  // Setup on load and mutations. The observer sets up only tabs that are
  // new: handed straight to MutationObserver, its list of changes came in as
  // `force`, so every change anywhere on the page measured every tabs again,
  // and opening a conversation, whose messages each have tabs, took seconds
  // on a phone.
  document.addEventListener("DOMContentLoaded", () => setupInitialStates(true));
  // Loaded fonts change the triggers' widths, so every indicator is placed
  // again once.
  document.fonts.ready.then(() => setupInitialStates(true));
  new MutationObserver(() => setupInitialStates(false)).observe(document.body, {
    childList: true,
    subtree: true,
  });

  // Expose public API
  window.tui = window.tui || {};
  window.tui.tabs = {
    setActive: setActiveTab,
  };

  window.addEventListener("popstate", (e) => {
    if (window.location.pathname.startsWith("/admin/avatars")) {
      var adminTab = e.state && e.state.adminAvatarTab;
      if (!adminTab) {
        var suffix = window.location.pathname.replace(/^\/admin\/avatars\/?/, "").replace(/\/$/, "");
        adminTab = suffix || "overview";
      }
      var adminContainer = document.querySelector('[data-tui-tabs][data-tui-tabs-id="avatar-admin-tabs"]');
      var adminTabsId = adminContainer && adminContainer.getAttribute("data-tui-tabs-id");
      if (adminTabsId && adminTab) {
        setActiveTab(adminTabsId, adminTab, true);
      }
      return;
    }

    if (!window.location.pathname.startsWith("/settings")) return;
    if (!e.state || !e.state.settingsTab) return;
    var tab = e.state.settingsTab;
    var container = document.querySelector("[data-tui-tabs]:not([data-tui-tabs-local])");
    if (!container) {
      if (typeof htmx !== "undefined") {
        htmx.ajax("GET", "/settings/" + tab, {target: "#main-content", swap: "outerHTML"});
      }
      return;
    }
    var tabsId = container.getAttribute("data-tui-tabs-id");
    if (tabsId && tab) {
      setActiveTab(tabsId, tab, true);
    }
  });
})();
