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

  const parseRequestOptions = (options) => {
    if (window.PublicKeyCredential?.parseRequestOptionsFromJSON) {
      return {
        ...options,
        publicKey: window.PublicKeyCredential.parseRequestOptionsFromJSON(options.publicKey),
      };
    }
    const parsed = { ...options, publicKey: { ...options.publicKey } };
    parsed.publicKey.challenge = decodeBase64URL(parsed.publicKey.challenge);
    if (parsed.publicKey.allowCredentials) {
      parsed.publicKey.allowCredentials = parsed.publicKey.allowCredentials.map((credential) => ({
        ...credential,
        id: decodeBase64URL(credential.id),
      }));
    }
    return parsed;
  };

  const serializeAssertion = (credential) => {
    if (typeof credential.toJSON === "function") {
      return credential.toJSON();
    }
    return {
      id: credential.id,
      rawId: encodeBase64URL(credential.rawId),
      type: credential.type,
      authenticatorAttachment: credential.authenticatorAttachment || undefined,
      clientExtensionResults: credential.getClientExtensionResults(),
      response: {
        clientDataJSON: encodeBase64URL(credential.response.clientDataJSON),
        authenticatorData: encodeBase64URL(credential.response.authenticatorData),
        signature: encodeBase64URL(credential.response.signature),
        userHandle: credential.response.userHandle
          ? encodeBase64URL(credential.response.userHandle)
          : null,
      },
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

  const bindAuthenticationForm = (form) => {
    if (form.dataset.passkeyAuthenticationBound === "true") return;
    form.dataset.passkeyAuthenticationBound = "true";
    const status = form.querySelector("[data-passkey-authentication-status]");
    const submit = form.querySelector("button[type='submit']");

    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      if (!window.PublicKeyCredential || !navigator.credentials?.get) {
        status.textContent = `This browser does not support passkeys. ${form.dataset.fallback || ""}`.trim();
        return;
      }
      submit.disabled = true;
      status.textContent = "Waiting for your device or security key…";
      try {
        const body = new URLSearchParams();
        if (form.dataset.identifierSource) {
          const identifier = document.querySelector(form.dataset.identifierSource);
          body.set("identifier", identifier?.value?.trim() || "");
        }
        const startHeaders = {
          "Content-Type": "application/x-www-form-urlencoded;charset=UTF-8",
        };
        if (form.dataset.startCsrf) {
          startHeaders["X-CSRF-Token"] = form.dataset.startCsrf;
        }
        const start = await fetch(form.dataset.startPath, {
          method: "POST",
          credentials: "same-origin",
          headers: startHeaders,
          body,
        });
        if (!start.ok) {
          throw new Error(await readError(start, "Unable to start passkey verification."));
        }
        const credential = await navigator.credentials.get(parseRequestOptions(await start.json()));
        if (!credential) {
          throw new Error("No passkey was selected.");
        }
        status.textContent = "Verifying the passkey…";
        const finishHeaders = { "Content-Type": "application/json" };
        if (form.dataset.finishCsrf) {
          finishHeaders["X-CSRF-Token"] = form.dataset.finishCsrf;
        }
        const finish = await fetch(form.dataset.finishPath, {
          method: "POST",
          credentials: "same-origin",
          headers: finishHeaders,
          body: JSON.stringify(serializeAssertion(credential)),
        });
        if (!finish.ok) {
          throw new Error(await readError(finish, "Unable to verify this passkey."));
        }
        const result = await finish.json();
        window.location.assign(form.dataset.successRedirect || result.redirect || "/");
      } catch (error) {
        if (error?.name === "NotAllowedError") {
          status.textContent = `Passkey verification was cancelled or timed out. ${form.dataset.fallback || ""}`.trim();
        } else {
          status.textContent = error?.message || `Unable to verify this passkey. ${form.dataset.fallback || ""}`.trim();
        }
        submit.disabled = false;
      }
    });
  };

  const bind = (root = document) => {
    root.querySelectorAll("[data-passkey-authentication]").forEach(bindAuthenticationForm);
  };

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", () => bind());
  } else {
    bind();
  }
  document.addEventListener("htmx:afterSwap", (event) => bind(event.target));
})();
