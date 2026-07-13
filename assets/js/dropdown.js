(function () {
  'use strict';
  
  document.addEventListener('click', (e) => {
    const item = e.target.closest('[data-tui-dropdown-item]');
    if (!item || 
        item.hasAttribute('data-tui-dropdown-submenu-trigger') ||
        item.getAttribute('data-tui-dropdown-prevent-close') === 'true') return;

    const popoverRoot = item.closest('[data-tui-popover-root]');
    const popoverContent = popoverRoot?.querySelector(':scope > [data-tui-popover-content]');
    if (!popoverContent?.matches(':popover-open')) return;

    if (window.tui?.popover?.closeElement) {
      window.tui.popover.closeElement(popoverContent);
      return;
    }

    try {
      popoverContent.setAttribute('data-tui-popover-open', 'false');
      popoverRoot.querySelectorAll(':scope > [data-tui-popover-trigger]').forEach(trigger => {
        trigger.setAttribute('data-tui-popover-open', 'false');
      });
      popoverContent.hidePopover();
    } catch {
      // ignore
    }
  });
})();
