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

  function prepareSlidingIndicator(list) {
    let indicator = list.querySelector("[data-tui-tabs-indicator]");
    if (indicator) return indicator;

    if (getComputedStyle(list).position === "static") {
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

  function syncSlidingIndicator(container, tabsId, value, animate) {
    const list = getTabList(container, tabsId);
    if (!list) return false;

    const indicator = prepareSlidingIndicator(list);
    const activeTrigger = getActiveTrigger(container, tabsId, value);
    if (!activeTrigger) return false;

    const listRect = list.getBoundingClientRect();
    const triggerRect = activeTrigger.getBoundingClientRect();
    const left = triggerRect.left - listRect.left;
    const width = triggerRect.width;

    if (!animate) {
      const previousTransition = indicator.style.transition;
      indicator.style.transition = "none";
      indicator.style.width = `${width}px`;
      indicator.style.transform = `translateX(${left}px)`;
      void indicator.offsetHeight;
      indicator.style.transition = previousTransition;
      return true;
    }

    indicator.style.width = `${width}px`;
    indicator.style.transform = `translateX(${left}px)`;
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

  function clearContentAnimation(container) {
    if (container._tuiTabsContentTimer) {
      window.clearTimeout(container._tuiTabsContentTimer);
      container._tuiTabsContentTimer = null;
    }
    container
      .querySelectorAll("[data-tui-tabs-content]")
      .forEach((content) => {
        content.classList.remove(
          "tui-tabs-content-enter",
          "tui-tabs-content-exit",
        );
      });
  }

  function revealContent(content) {
    content.classList.remove("hidden");
    content.classList.add("tui-tabs-content-enter");
    void content.offsetHeight;
    requestAnimationFrame(() => {
      content.classList.remove("tui-tabs-content-enter");
    });
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

    const activeContents = contents.filter(
      (content) =>
        !content.classList.contains("hidden") && content !== target,
    );

    clearContentAnimation(container);

    contents.forEach((content) => {
      const isActive = content === target;
      content.setAttribute(
        "data-tui-tabs-state",
        isActive ? "active" : "inactive",
      );
      if (!isActive && activeContents.indexOf(content) === -1) {
        content.classList.add("hidden");
      }
    });

    if (!animate || activeContents.length === 0) {
      contents.forEach((content) => {
        content.classList.toggle("hidden", content !== target);
      });
      return;
    }

    activeContents.forEach((content) => {
      content.classList.add("tui-tabs-content-exit");
    });

    container._tuiTabsContentTimer = window.setTimeout(() => {
      activeContents.forEach((content) => {
        content.classList.add("hidden");
        content.classList.remove("tui-tabs-content-exit");
      });
      revealContent(target);
      container._tuiTabsContentTimer = null;
    }, 45);
  }

  // Update tab state
  function setActiveTab(tabsId, value, animate = true) {
    const container = getTabsContainer(tabsId);
    if (!container) return;
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

    syncSlidingIndicator(container, tabsId, value, animate);

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
        var adminURL = value === "overview" ? "/admin/avatars/" : "/admin/avatars/" + value;
        if (window.location.pathname !== adminURL) {
          history.pushState({ adminAvatarTab: value }, "", adminURL);
        }
      }
    }
  });

  // Initialize active states
  function setupInitialStates(force = false) {
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
        setActiveTab(tabsId, activeTrigger.getAttribute("data-tui-tabs-value"), false);
        container.setAttribute("data-tui-tabs-initialized", "true");
      }
    });
  }

  // Setup on load and mutations
  document.addEventListener("DOMContentLoaded", () => setupInitialStates(true));
  document.fonts.ready.then(() => setupInitialStates(false));
  new MutationObserver(setupInitialStates).observe(document.body, {
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
