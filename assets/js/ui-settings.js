var GoferSettings;

(function () {
  var LS_KEY = "gofer:ui_settings";
  var _cache = {};
  var _saveTimer = null;

  function readCache() {
    try {
      var raw = localStorage.getItem(LS_KEY);
      if (raw) {
        _cache = JSON.parse(raw);
      }
    } catch (_) {}
  }

  function writeCache() {
    try {
      localStorage.setItem(LS_KEY, JSON.stringify(_cache));
    } catch (_) {}
  }

  function applyTheme(theme) {
    var html = document.documentElement;
    if (theme === "dark") {
      html.classList.add("dark");
    } else {
      html.classList.remove("dark");
    }
  }

  function applyThemeStyle(style) {
    var html = document.documentElement;
    html.setAttribute("data-theme", style || "classic");
  }

  function browserTimezone() {
    try {
      return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
    } catch (_) {
      return "UTC";
    }
  }

  function persistSettingsNow() {
    return fetch("/api/settings/ui", {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(_cache),
    }).catch(function () {});
  }

  var MAIL_TABLE_COLUMNS = [
    { id: "accountMarker", fixed: 24 },
    { id: "starred", fixed: 24 },
    { id: "attachment", fixed: 24 },
    { id: "thread", fixed: 28 },
    { id: "from", min: 90 },
    { id: "to", min: 90 },
    { id: "subject", min: 140 },
    { id: "date", min: 64 },
  ];

  function normalizeMailTableColumnIds(value) {
    var allowed = {};
    var ids = [];
    for (var i = 0; i < MAIL_TABLE_COLUMNS.length; i++) allowed[MAIL_TABLE_COLUMNS[i].id] = true;

    var parts = value ? String(value).split(",") : MAIL_TABLE_COLUMNS.map(function (column) { return column.id; });
    for (var j = 0; j < parts.length; j++) {
      var id = parts[j].trim();
      if (allowed[id] && ids.indexOf(id) === -1) ids.push(id);
    }
    if (ids.length === 0) ids.push("subject");
    return ids;
  }

  function normalizeMailTableColumnWidths(value, visibleIds) {
    var parts = String(value).split(",");
    if (!value || parts.length !== MAIL_TABLE_COLUMNS.length) parts = ["0.8", "0.8", "0.8", "1", "3", "3", "5", "2"];
    var values = [];
    for (var i = 0; i < parts.length; i++) {
      var n = parseFloat(parts[i]);
      if (isNaN(n)) n = 1;
      values.push(Math.max(0.01, n));
    }

    var visible = {};
    var total = 0;
    for (var j = 0; j < visibleIds.length; j++) visible[visibleIds[j]] = true;
    for (var k = 0; k < MAIL_TABLE_COLUMNS.length; k++) {
      if (visible[MAIL_TABLE_COLUMNS[k].id] && !MAIL_TABLE_COLUMNS[k].fixed) total += values[k];
    }
    if (total <= 0) return null;

    var columns = [];
    for (var l = 0; l < MAIL_TABLE_COLUMNS.length; l++) {
      if (!visible[MAIL_TABLE_COLUMNS[l].id]) continue;
      if (MAIL_TABLE_COLUMNS[l].fixed) {
        columns.push(MAIL_TABLE_COLUMNS[l].fixed + "px");
        continue;
      }
      columns.push("minmax(" + MAIL_TABLE_COLUMNS[l].min + "px, " + (values[l] / total).toFixed(5) + "fr)");
    }
    return columns.join(" ");
  }

  function applyMailTableColumnSettings(root) {
    var visibleIds = normalizeMailTableColumnIds(_cache.mail_table_columns);
    var columns = normalizeMailTableColumnWidths(_cache.mail_table_column_widths, visibleIds);
    if (!columns) return;

    var hidden = MAIL_TABLE_COLUMNS.map(function (column) { return column.id; }).filter(function (id) {
      return visibleIds.indexOf(id) === -1;
    }).join(" ");

    document.documentElement.style.setProperty("--mail-list-table-columns", columns);
    var scope = root || document;
    var scroll = scope.id === "mail-list-scroll" ? scope : scope.querySelector && scope.querySelector("#mail-list-scroll");
    if (scroll) {
      scroll.style.setProperty("--mail-list-table-columns", columns);
      scroll.dataset.mailTableHidden = hidden;
      scroll.dataset.mailTableColumns = visibleIds.join(",");
    }
  }

  function mailPaneLayout(value) {
    return value === "stacked" ? "stacked" : "side";
  }

  function applyMailPaneLayout(value) {
    var layout = mailPaneLayout(value);
    var main = document.getElementById("main-content");
    if (!main || !main.querySelector("#mail-list") || !main.querySelector("#mail-view")) return;

    main.dataset.mailPaneLayout = layout;
    main.classList.toggle("flex-col", layout === "stacked");

    var list = document.getElementById("mail-list");
    if (list) {
      if (layout === "stacked") {
        list.style.width = "";
        if (_cache.mail_list_height) list.style.height = parseInt(_cache.mail_list_height, 10) + "px";
      } else {
        list.style.height = "";
        if (_cache.mail_list_width) list.style.width = parseInt(_cache.mail_list_width, 10) + "px";
      }
    }

    if (typeof initResizeHandles === "function") initResizeHandles();
    refreshVirtualMailListLayout();
  }

  function refreshVirtualMailListLayout() {
    var scroll = document.getElementById("mail-list-scroll");
    var list = scroll && scroll._virtualMailList;
    if (!list) return;
    if (typeof list.applyPaneLayoutDensity === "function") list.applyPaneLayoutDensity();
    if (typeof list.render === "function") list.render();
  }

  window.applyMailTableColumnWidths = function (value, root) {
    _cache.mail_table_column_widths = value;
    applyMailTableColumnSettings(root);
  };

  window.applyMailTableColumnSettings = applyMailTableColumnSettings;

  window.applyMailTableColumns = function (value, root) {
    _cache.mail_table_columns = normalizeMailTableColumnIds(value).join(",");
    applyMailTableColumnSettings(root);
  };

  window.getMailTableColumns = function () {
    return normalizeMailTableColumnIds(_cache.mail_table_columns);
  };

  function applySetting(key, value) {
    if (key === "theme") {
      applyTheme(value);
      if (typeof applyEmailBodyTheme === "function") applyEmailBodyTheme();
    }
    if (key === "theme_style") {
      applyThemeStyle(value);
      if (typeof applyEmailBodyTheme === "function") applyEmailBodyTheme();
    }
    if (key === "sidebar_width") {
      var panel = document.querySelector("aside");
      if (panel && value) {
        var w = parseInt(value, 10);
        if (!isNaN(w) && w > 0) panel.style.width = w + "px";
      }
    }
    if (key === "mail_list_width") {
      var panel = document.getElementById("mail-list");
      if (panel && value && mailPaneLayout(_cache.mail_pane_layout) !== "stacked") {
        var w = parseInt(value, 10);
        if (!isNaN(w) && w > 0) panel.style.width = w + "px";
      }
    }
    if (key === "mail_list_height") {
      var listPanel = document.getElementById("mail-list");
      if (listPanel && value && mailPaneLayout(_cache.mail_pane_layout) === "stacked") {
        var h = parseInt(value, 10);
        if (!isNaN(h) && h > 0) listPanel.style.height = h + "px";
        refreshVirtualMailListLayout();
      }
    }
    if (key === "mail_pane_layout") {
      _cache.mail_pane_layout = mailPaneLayout(value);
      applyMailPaneLayout(_cache.mail_pane_layout);
    }
    if (key === "mail_table_column_widths") {
      _cache.mail_table_column_widths = value;
      applyMailTableColumnSettings();
    }
    if (key === "mail_table_columns") {
      _cache.mail_table_columns = normalizeMailTableColumnIds(value).join(",");
      applyMailTableColumnSettings();
    }
    if (key === "timezone") {
      document.documentElement.setAttribute("data-timezone", value || browserTimezone());
    }
  }

  GoferSettings = {
    init: function () {
      var bodySettings = document.body ? document.body.dataset.uiSettings : null;
      if (bodySettings) {
        var serverSettings = {};
        try {
          serverSettings = JSON.parse(bodySettings);
        } catch (_) {}
        readCache();
        _cache = Object.assign({}, _cache, serverSettings);
        if (!_cache.timezone || _cache.timezone === "local") {
          _cache.timezone = browserTimezone();
          persistSettingsNow();
        }
        for (var k in _cache) {
          applySetting(k, _cache[k]);
        }
      } else {
        readCache();
        for (var k in _cache) {
          applySetting(k, _cache[k]);
        }
        fetch("/api/settings/ui")
          .then(function (r) {
            return r.json();
          })
          .then(function (serverSettings) {
            _cache = serverSettings;
            if (!_cache.timezone || _cache.timezone === "local") {
              _cache.timezone = browserTimezone();
              persistSettingsNow();
            }
            writeCache();
            for (var k in _cache) {
              applySetting(k, _cache[k]);
            }
          })
          .catch(function () {});
      }
      var initialSizeStyle = document.querySelector("[data-saved-panel-size-style]");
      if (initialSizeStyle) initialSizeStyle.remove();
      writeCache();
    },

    get: function (key) {
      return _cache[key] || null;
    },

    set: function (key, value) {
      var oldValue = _cache[key] || null;
      _cache[key] = value;
      writeCache();
      applySetting(key, value);

      if (key === "timezone" && oldValue !== value) {
        if (_saveTimer) clearTimeout(_saveTimer);
        persistSettingsNow().finally(function () {
          window.location.reload();
        });
        return;
      }

      if (_saveTimer) clearTimeout(_saveTimer);
      _saveTimer = setTimeout(function () {
        fetch("/api/settings/ui", {
          method: "PATCH",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(_cache),
        }).catch(function () {});
        _saveTimer = null;
      }, 300);
    },
  };

  GoferSettings.init();
})();
