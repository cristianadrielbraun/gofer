// Translations for scripts. The server's catalog script (/assets/i18n/<lang>.js)
// loads first and sets window.GOFER_I18N to the page language's "js." messages,
// with English filled in for any the language lacks.
//
//   goferT("js.mail.moved", { folder: "Archive" })
//   goferTn("js.mail.selected", 3)   // picks js.mail.selected.one or .other
(function () {
  var catalog = window.GOFER_I18N || { locale: "en", messages: {} }

  function format(message, params) {
    if (!params) return message
    return message.replace(/\{([a-zA-Z_]+)\}/g, function (match, name) {
      return Object.prototype.hasOwnProperty.call(params, name) ? String(params[name]) : match
    })
  }

  // English and Spanish only tell one from other for whole numbers.
  function pluralForm(count) {
    return count === 1 ? "one" : "other"
  }

  window.goferLocale = catalog.locale || "en"

  window.goferT = function (key, params) {
    var message = catalog.messages[key]
    // Showing the key makes a missing message easy to spot and to find.
    return format(message == null ? key : message, params)
  }

  window.goferTn = function (key, count, params) {
    var shown = count
    try { shown = new Intl.NumberFormat(window.goferLocale).format(count) } catch (_) {}
    var values = Object.assign({ count: shown }, params || {})
    return window.goferT(key + "." + pluralForm(count), values)
  }
})()
