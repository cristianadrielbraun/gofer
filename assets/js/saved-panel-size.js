// Applies the saved panel sizes before the page is drawn, so it doesn't jump.
(function() {
  document.documentElement.classList.add("js");

  var settings = {};
  try {
    settings = JSON.parse(localStorage.getItem("gofer:ui_settings") || "{}") || {};
  } catch (_) {}

  function px(value, min, max) {
    var n = parseInt(value, 10);
    if (isNaN(n) || n <= 0) return null;
    if (max === null || max === undefined) return Math.max(min, n);
    return Math.max(min, Math.min(max, n));
  }

  function mailListWidth(value) {
    var raw = String(value || "").trim();
    if (raw.slice(-1) === "%") {
      var percent = parseFloat(raw);
      if (!isNaN(percent) && percent > 0) return "clamp(300px," + percent + "%,calc(100% - 300px))";
    }
    var n = px(raw, 300);
    return n ? n + "px" : null;
  }

  // Contacts and calendar keep their own sizes, and use mail's until resized.
  // Their layout is their own from the start.
  var section = /^\/(contacts|calendar)(\/|$)/.exec(location.pathname);
  section = section ? section[1] : "";
  function saved(key, sectionKey) {
    return (section && settings[section + "_" + sectionKey]) || settings[key];
  }

  var vw = window.innerWidth || 1024;
  var stacked = settings[section ? section + "_pane_layout" : "mail_pane_layout"] === "stacked";
  var sidebar = px(saved("sidebar_width", "sidebar_width"), 180, Math.min(400, vw * 0.25));
  var mailList = stacked ? px(saved("mail_list_height", "list_height"), 180) : mailListWidth(saved("mail_list_width", "list_width"));
  if (!sidebar && !mailList && !stacked) return;

  var style = document.createElement("style");
  style.setAttribute("data-saved-panel-size-style", "");
  // Saved panel sizes are desktop-only; small screens show one pane at a time.
  // Only the app's sidebar takes its width: Mail's calendar sidebar is an
  // aside too, and has a width of its own.
  style.textContent = "@media (min-width:1024px){" + (sidebar ? "#app-shell>aside{width:" + sidebar + "px!important}" : "") + (stacked ? "#main-content{flex-direction:column}#mail-list{width:100%!important" + (mailList ? ";height:" + mailList + "px!important" : "") + "}" : (mailList ? "#mail-list{width:" + mailList + "!important}" : "")) + "}";
  document.head.appendChild(style);
})();
