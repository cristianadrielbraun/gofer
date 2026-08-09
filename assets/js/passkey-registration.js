(() => {
  const decodeBase64URL = (value) => {
    const normalized = value.replace(/-/g, "+").replace(/_/g, "/");
    const padded = normalized + "=".repeat((4 - (normalized.length % 4)) % 4);
    const binary = window.atob(padded);
    return Uint8Array.from(binary, (character) => character.charCodeAt(0));
  };

  const encodeBase64URL = (value) => {
    const bytes = new Uint8Array(value);
    let binary = "";
    for (const byte of bytes) {
      binary += String.fromCharCode(byte);
    }
    return window.btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
  };

  const parseCreationOptions = (options) => {
    if (window.PublicKeyCredential?.parseCreationOptionsFromJSON) {
      return {
        ...options,
        publicKey: window.PublicKeyCredential.parseCreationOptionsFromJSON(options.publicKey),
      };
    }
    const parsed = { ...options, publicKey: { ...options.publicKey } };
    parsed.publicKey.challenge = decodeBase64URL(parsed.publicKey.challenge);
    parsed.publicKey.user = {
      ...parsed.publicKey.user,
      id: decodeBase64URL(parsed.publicKey.user.id),
    };
    if (parsed.publicKey.excludeCredentials) {
      parsed.publicKey.excludeCredentials = parsed.publicKey.excludeCredentials.map((credential) => ({
        ...credential,
        id: decodeBase64URL(credential.id),
      }));
    }
    return parsed;
  };

  const serializeCredential = (credential) => {
    if (typeof credential.toJSON === "function") {
      return credential.toJSON();
    }
    const response = {
      clientDataJSON: encodeBase64URL(credential.response.clientDataJSON),
      attestationObject: encodeBase64URL(credential.response.attestationObject),
    };
    if (typeof credential.response.getTransports === "function") {
      response.transports = credential.response.getTransports();
    }
    if (typeof credential.response.getAuthenticatorData === "function") {
      response.authenticatorData = encodeBase64URL(credential.response.getAuthenticatorData());
    }
    if (typeof credential.response.getPublicKey === "function") {
      const publicKey = credential.response.getPublicKey();
      if (publicKey) {
        response.publicKey = encodeBase64URL(publicKey);
      }
    }
    if (typeof credential.response.getPublicKeyAlgorithm === "function") {
      response.publicKeyAlgorithm = credential.response.getPublicKeyAlgorithm();
    }
    return {
      id: credential.id,
      rawId: encodeBase64URL(credential.rawId),
      type: credential.type,
      authenticatorAttachment: credential.authenticatorAttachment || undefined,
      clientExtensionResults: credential.getClientExtensionResults(),
      response,
    };
  };

  const readError = async (response, fallback) => {
    try {
      const payload = await response.json();
      return payload.error || fallback;
    } catch (_) {
      return fallback;
    }
  };

  const bindRegistrationForm = (form) => {
    if (form.dataset.passkeyBound === "true") return;
    form.dataset.passkeyBound = "true";
    const status = form.querySelector("[data-passkey-status]");
    const submit = form.querySelector("button[type='submit']");

    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      if (!window.PublicKeyCredential || !navigator.credentials?.create) {
        status.textContent = "This browser does not support passkey registration.";
        return;
      }
      submit.disabled = true;
      status.textContent = "Waiting for your device or security key…";
      try {
        const body = new URLSearchParams(new FormData(form));
        const start = await fetch(form.dataset.startPath, {
          method: "POST",
          credentials: "same-origin",
          headers: {
            "Content-Type": "application/x-www-form-urlencoded;charset=UTF-8",
            "X-CSRF-Token": form.dataset.startCsrf,
          },
          body,
        });
        if (!start.ok) {
          throw new Error(await readError(start, "Unable to start passkey registration."));
        }
        const options = parseCreationOptions(await start.json());
        const credential = await navigator.credentials.create(options);
        if (!credential) {
          throw new Error("No passkey was created.");
        }
        status.textContent = "Saving the new passkey…";
        const finish = await fetch(form.dataset.finishPath, {
          method: "POST",
          credentials: "same-origin",
          headers: {
            "Content-Type": "application/json",
            "X-CSRF-Token": form.dataset.finishCsrf,
          },
          body: JSON.stringify(serializeCredential(credential)),
        });
        if (!finish.ok) {
          throw new Error(await readError(finish, "Unable to save this passkey."));
        }
        const result = await finish.json();
        window.location.assign(result.redirect || "/settings/security?passkey_added=1");
      } catch (error) {
        if (error?.name === "NotAllowedError") {
          status.textContent = "Passkey creation was cancelled or timed out. You can try again.";
        } else {
          status.textContent = error?.message || "Unable to add a passkey.";
        }
        submit.disabled = false;
      }
    });
  };

  const bind = (root = document) => {
    root.querySelectorAll("[data-passkey-registration]").forEach(bindRegistrationForm);
  };

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", () => bind());
  } else {
    bind();
  }
  document.addEventListener("htmx:afterSwap", (event) => bind(event.target));
})();
