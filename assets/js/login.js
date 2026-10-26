// The sign-in page asks for the username first. Continue looks up whether the
// username has a passkey, then opens the password, and the passkey when there
// is one, under it. Changing the username closes them again. Without
// JavaScript, Continue posts the username and the page comes back opened.
(() => {
  const flow = document.querySelector("[data-login-flow]");
  if (!flow) return;
  const form = flow.querySelector("[data-login-form]");
  const identifier = form.querySelector("#login-identifier");
  const password = form.querySelector("#login-password");
  const submit = form.querySelector("[data-login-submit]");
  const passwordReveal = flow.querySelector("[data-login-reveal='password']");
  const passkeyReveal = flow.querySelector("[data-login-reveal='passkey']");
  const passkeyStatus = flow.querySelector("[data-passkey-authentication-status]");
  const labels = form.querySelectorAll("[data-login-submit-label]");

  let shown = flow.dataset.methodsShown === "true";
  let identified = flow.dataset.identified || "";
  let looking = false;

  const showLabel = (name) => {
    labels.forEach((label) => {
      const current = label.dataset.loginSubmitLabel === name;
      label.dataset.shown = String(current);
      label.setAttribute("aria-hidden", String(!current));
    });
  };

  const showMethods = (hasPasskey) => {
    shown = true;
    identified = identifier.value.trim();
    password.disabled = false;
    passwordReveal.dataset.open = "true";
    passkeyReveal.dataset.open = String(hasPasskey);
    showLabel("sign-in");
    password.focus({ preventScroll: true });
  };

  const hideMethods = () => {
    shown = false;
    identified = "";
    password.value = "";
    password.disabled = true;
    passwordReveal.dataset.open = "false";
    passkeyReveal.dataset.open = "false";
    if (passkeyStatus) passkeyStatus.textContent = "";
    showLabel("continue");
  };

  form.addEventListener("submit", async (event) => {
    if (shown) return;
    event.preventDefault();
    const value = identifier.value.trim();
    if (!value || looking) return;
    looking = true;
    submit.disabled = true;
    let hasPasskey = false;
    try {
      const response = await fetch(flow.dataset.methodsPath, {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/x-www-form-urlencoded;charset=UTF-8" },
        body: new URLSearchParams({ identifier: value }),
      });
      if (response.ok) hasPasskey = (await response.json()).passkey === true;
    } catch (_) {
      // The password is always offered, so a failed lookup just leaves it alone.
    }
    looking = false;
    submit.disabled = false;
    if (identifier.value.trim() === value) showMethods(hasPasskey);
  });

  identifier.addEventListener("input", () => {
    if (shown && identifier.value.trim() !== identified) hideMethods();
  });
})();
