// Applies the saved panel sizes before the page is drawn, so it doesn't jump.

// goferMailCalendarFits says whether the calendar sidebar fits beside Mail's
// panes in a Mail area main pixels wide. The reading pane has to keep at
// least 600px, the width most emails are laid out for; with less, the
// calendar goes over the panes. With no reading pane, the list has to keep
// its usual 300px. layout is Mail's pane layout ("side", "stacked" or
// "list"), list the list's saved width ("50%" or pixels) and calendar the
// calendar's width. Only the side by side layout has a list beside the
// reading pane.
function goferMailCalendarFits(main, layout, list, calendar) {
  var readerMin = 600;
  var listMin = 300;
  var divider = 5;
  var room = main - calendar;
  if (layout === "list") return room >= listMin;
  if (layout === "stacked") return room >= readerMin;
  var raw = String(list || "50%").trim();
  var listWidth = raw.slice(-1) === "%"
    ? Math.min(Math.max(300, main * parseFloat(raw) / 100), main - 300)
    : Math.max(300, parseInt(raw, 10) || 300);
  return room - listWidth - divider >= readerMin;
}

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

  // The calendar sidebar's place, so the page is drawn with it beside the
  // panes or over them from the start (mail-calendar-sidebar.js keeps it up
  // to date). The Mail area is the window less the sidebar and its divider;
  // small screens always show the calendar over the panes.
  var sidebarWidth = px(settings.sidebar_width, 180, Math.min(400, vw * 0.25)) || 256;
  var calendarWidth = px(settings.mail_calendar_width, 260, 450) || 300;
  var calendarOver = vw < 1024 || !goferMailCalendarFits(vw - sidebarWidth - 5, settings.mail_pane_layout, settings.mail_list_width, calendarWidth);
  if (calendarOver) document.documentElement.setAttribute("data-mail-calendar-over", "");

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
