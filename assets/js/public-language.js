// The language picker on the pages shown before sign-in. Picking a language
// keeps it in the language cookie and marks it as picked, so the server saves
// it as the user's language once they sign in. Then the page reloads in it.
(function () {
  document.addEventListener("change", function (event) {
    var target = event.target
    if (!target || !target.closest || !target.closest("[data-public-language-input]")) return
    var value = String(target.value || "").trim()
    if (!value) return
    document.cookie = "gofer_lang=" + encodeURIComponent(value) + "; path=/; max-age=31536000; samesite=lax"
    // A day is enough to finish signing in; an unused pick shouldn't wait for
    // the next person to sign in on this browser.
    document.cookie = "gofer_lang_pick=1; path=/; max-age=86400; samesite=lax"
    window.location.reload()
  })
})()
