// Waits for the user to finish Microsoft device sign-in, then follows the
// server's redirect back to account settings.
(function () {
  const root = document.getElementById("microsoft-device-signin");
  if (!root) return;
  const status = root.querySelector("[data-status-text]");
  const interval = Math.max(Number(root.dataset.intervalMs) || 5000, 1000);
  const expiresAt = Number(root.dataset.expiresAtMs) || 0;
  const code = document.getElementById("microsoft-device-code");
  const opener = root.querySelector("[data-device-signin-open]");
  let failures = 0;
  let popup = null;

  // One click copies the code and opens Microsoft in a popup beside this
  // page. If the popup is blocked, the link opens in a new tab as usual.
  if (opener) {
    opener.addEventListener("click", function (event) {
      if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
      if (code && navigator.clipboard?.writeText) {
        navigator.clipboard.writeText(code.textContent.trim()).catch(function () {});
      }
      const width = 520;
      const height = 720;
      const left = Math.max(0, window.screenX + (window.outerWidth - width) / 2);
      const top = Math.max(0, window.screenY + (window.outerHeight - height) / 2);
      popup = window.open(opener.href, "gofer-microsoft-signin", "popup,width=" + width + ",height=" + height + ",left=" + left + ",top=" + top);
      if (!popup) return;
      event.preventDefault();
      popup.focus();
      if (status) status.textContent = "Code copied. Paste it in the Microsoft window.";
    });
  }

  function closePopup() {
    try {
      if (popup && !popup.closed) popup.close();
    } catch (error) {
      // Microsoft may cut the link to its window; the user closes it then.
    }
  }

  async function poll() {
    if (expiresAt && Date.now() >= expiresAt) {
      window.location.assign("/settings/accounts?error=oauth_expired_state");
      return;
    }
    try {
      const response = await fetch(root.dataset.pollPath, {
        method: "POST",
        credentials: "same-origin",
        headers: {
          "Content-Type": "application/x-www-form-urlencoded;charset=UTF-8",
          "X-CSRF-Token": root.dataset.pollCsrf,
        },
        body: new URLSearchParams({ state: root.dataset.state }),
      });
      if (!response.ok) throw new Error("poll failed: " + response.status);
      const result = await response.json();
      if (result.status === "done") {
        closePopup();
        if (status) status.textContent = "Finishing…";
        window.location.assign(result.redirect || "/settings/accounts");
        return;
      }
      if (failures > 0 && status) status.textContent = "Waiting for you to finish signing in…";
      failures = 0;
    } catch (error) {
      failures += 1;
      if (status) status.textContent = "Can't reach Gofer. Retrying…";
    }
    window.setTimeout(poll, interval * Math.min(failures + 1, 4));
  }

  window.setTimeout(poll, interval);
})();
