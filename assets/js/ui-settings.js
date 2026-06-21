var GoferSettings;

(function () {
  var LS_KEY = "gofer:ui_settings";
  var _cache = {};
  var _saveTimer = null;
  var _migratedPanelSize = false;

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
  var MAIL_CARD_FIELDS = [
    { id: "avatar" },
    { id: "thread" },
    { id: "from" },
    { id: "account" },
    { id: "accountMarker" },
    { id: "to" },
    { id: "date" },
    { id: "subject" },
    { id: "preview" },
    { id: "labels" },
    { id: "attachment" },
    { id: "starred" },
    { id: "unread" },
  ];
  var DEFAULT_MAIL_CARD_FIELDS = "avatar,thread,from,attachment,date,unread,subject,preview,labels,starred";
  var MAIL_CARD_LAYOUT_ZONES = ["railTop", "header", "meta", "railMiddle", "body", "status", "railBottom", "footer", "corner", "hidden"];
  var MAIL_CARD_VISIBLE_LAYOUT_ZONES = ["railTop", "header", "meta", "railMiddle", "body", "status", "railBottom", "footer", "corner"];
  var DEFAULT_MAIL_CARD_LAYOUT = "railTop:avatar|header:from,date|meta:attachment,unread|railMiddle:|body:subject|status:|railBottom:thread|footer:preview,labels|corner:starred|hidden:account,accountMarker,to";
  var LEGACY_DEFAULT_MAIL_CARD_LAYOUTS = [
    "rail:avatar,thread|header:from,account|meta:attachment,date|body:subject,to,preview|footer:labels,starred|status:unread|hidden:",
    "rail:avatar,thread|header:from|meta:attachment,date|body:subject,preview|footer:labels,starred|status:unread|hidden:account,to",
    "rail:avatar,thread|header:from|meta:attachment,date|body:subject|footer:preview,labels,starred|status:unread|hidden:account,to",
    "rail:avatar,thread|header:from|meta:attachment,date,unread|body:subject|footer:preview,labels|status:|corner:starred|hidden:account,accountMarker,to",
    "rail:avatar,thread|header:from,date|meta:attachment,unread|body:subject|footer:preview,labels|status:|corner:starred|hidden:account,accountMarker,to",
  ];
  var MAIL_CARD_ICON_FIELD_MAP = {
    avatar: true,
    thread: true,
    accountMarker: true,
    attachment: true,
    starred: true,
    unread: true,
  };
  var MAIL_CARD_SIDE_ZONES = ["railTop", "railMiddle", "railBottom", "meta", "status", "corner"];
  var MAIL_CARD_CENTER_ZONES = ["header", "body", "footer"];
  var MAIL_CARD_SIDE_ZONE_MAX = {
    railTop: 1,
    railMiddle: 1,
    railBottom: 1,
    meta: 3,
    status: 3,
    corner: 3,
  };
  var MAIL_CARD_DEFAULT_ZONE_BY_ID = {
    avatar: "railTop",
    accountMarker: "railMiddle",
    thread: "railBottom",
    from: "header",
    account: "header",
    attachment: "meta",
    date: "header",
    unread: "meta",
    subject: "body",
    to: "body",
    preview: "footer",
    labels: "footer",
    starred: "corner",
  };

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

  function normalizeMailCardFieldIds(value) {
    var allowed = {};
    var ids = [];
    for (var i = 0; i < MAIL_CARD_FIELDS.length; i++) allowed[MAIL_CARD_FIELDS[i].id] = true;

    var raw = value ? String(value) : DEFAULT_MAIL_CARD_FIELDS;
    var parts = raw.split(",");
    for (var j = 0; j < parts.length; j++) {
      var id = parts[j].trim();
      if (allowed[id] && ids.indexOf(id) === -1) ids.push(id);
    }
    if (ids.length === 0) ids.push("subject");
    return ids;
  }

  function mailCardAllowedFieldMap() {
    var allowed = {};
    for (var i = 0; i < MAIL_CARD_FIELDS.length; i++) allowed[MAIL_CARD_FIELDS[i].id] = true;
    return allowed;
  }

  function emptyMailCardLayout() {
    var layout = {};
    for (var i = 0; i < MAIL_CARD_LAYOUT_ZONES.length; i++) layout[MAIL_CARD_LAYOUT_ZONES[i]] = [];
    return layout;
  }

  function mailCardZoneAllowed(zone) {
    return MAIL_CARD_LAYOUT_ZONES.indexOf(zone) !== -1;
  }

  function defaultMailCardZone(id) {
    return MAIL_CARD_DEFAULT_ZONE_BY_ID[id] || "body";
  }

  function mailCardFieldType(id) {
    return MAIL_CARD_ICON_FIELD_MAP[id] ? "icon" : "text";
  }

  function mailCardZoneAcceptsField(zone, id) {
    if (zone === "hidden") return true;
    if (mailCardFieldType(id) === "icon") return MAIL_CARD_SIDE_ZONES.indexOf(zone) !== -1;
    return MAIL_CARD_CENTER_ZONES.indexOf(zone) !== -1;
  }

  function legacyRailZone(index, count) {
    if (count <= 1) return "railTop";
    if (count === 2) return index === 0 ? "railTop" : "railBottom";
    if (index === 0) return "railTop";
    if (index === 1) return "railMiddle";
    return "railBottom";
  }

  function mailCardZoneHasRoom(layout, zone) {
    var max = MAIL_CARD_SIDE_ZONE_MAX[zone];
    return !max || ((layout[zone] || []).length < max);
  }

  function compatibleMailCardZone(layout, id, preferredZone) {
    var candidates = mailCardFieldType(id) === "icon" ? MAIL_CARD_SIDE_ZONES : MAIL_CARD_CENTER_ZONES;
    var fallbackZone = defaultMailCardZone(id);

    if (mailCardZoneAcceptsField(preferredZone, id) && mailCardZoneHasRoom(layout, preferredZone)) return preferredZone;
    if (mailCardZoneAcceptsField(fallbackZone, id) && mailCardZoneHasRoom(layout, fallbackZone)) return fallbackZone;

    for (var i = 0; i < candidates.length; i++) {
      if (mailCardZoneHasRoom(layout, candidates[i])) return candidates[i];
    }
    return candidates[0] || "body";
  }

  function parseMailCardLayout(value) {
    var allowed = mailCardAllowedFieldMap();
    var layout = emptyMailCardLayout();
    var seen = {};
    var raw = value ? String(value) : DEFAULT_MAIL_CARD_LAYOUT;
    if (LEGACY_DEFAULT_MAIL_CARD_LAYOUTS.indexOf(raw) !== -1) raw = DEFAULT_MAIL_CARD_LAYOUT;
    var groups = raw.split("|");

    for (var i = 0; i < groups.length; i++) {
      var group = groups[i];
      var sep = group.indexOf(":");
      if (sep === -1) continue;
      var zone = group.slice(0, sep).trim();
      var ids = group.slice(sep + 1).split(",");
      if (zone === "rail") {
        var railIds = [];
        for (var r = 0; r < ids.length; r++) {
          var railID = ids[r].trim();
          if (allowed[railID] && !seen[railID]) railIds.push(railID);
        }
        for (var n = 0; n < railIds.length; n++) {
          var railZone = legacyRailZone(n, railIds.length);
          layout[railZone].push(railIds[n]);
          seen[railIds[n]] = true;
        }
        continue;
      }
      if (!mailCardZoneAllowed(zone)) continue;
      for (var j = 0; j < ids.length; j++) {
        var id = ids[j].trim();
        if (!allowed[id] || seen[id]) continue;
        layout[zone].push(id);
        seen[id] = true;
      }
    }

    for (var k = 0; k < MAIL_CARD_FIELDS.length; k++) {
      var fieldID = MAIL_CARD_FIELDS[k].id;
      if (!seen[fieldID]) layout[defaultMailCardZone(fieldID)].push(fieldID);
    }

    return layout;
  }

  function normalizeMailCardLayout(value, fieldsValue) {
    var parsed = parseMailCardLayout(value);
    var visibleIds = normalizeMailCardFieldIds(fieldsValue);
    var visible = {};
    var layout = emptyMailCardLayout();

    for (var i = 0; i < visibleIds.length; i++) visible[visibleIds[i]] = true;

    for (var z = 0; z < MAIL_CARD_LAYOUT_ZONES.length; z++) {
      var zone = MAIL_CARD_LAYOUT_ZONES[z];
      var ids = parsed[zone] || [];
      for (var j = 0; j < ids.length; j++) {
        var id = ids[j];
        if (visible[id]) {
          var targetZone = zone === "hidden" ? defaultMailCardZone(id) : zone;
          layout[compatibleMailCardZone(layout, id, targetZone)].push(id);
        } else {
          layout.hidden.push(id);
        }
      }
    }

    return layout;
  }

  function mailCardVisibleIdsFromLayout(layout) {
    var ids = [];
    for (var i = 0; i < MAIL_CARD_VISIBLE_LAYOUT_ZONES.length; i++) {
      var zoneIds = layout[MAIL_CARD_VISIBLE_LAYOUT_ZONES[i]] || [];
      for (var j = 0; j < zoneIds.length; j++) {
        if (ids.indexOf(zoneIds[j]) === -1) ids.push(zoneIds[j]);
      }
    }
    if (ids.length === 0) ids.push("subject");
    return ids;
  }

  function serializeMailCardLayout(layout) {
    var normalized = layout || emptyMailCardLayout();
    var parts = [];
    for (var i = 0; i < MAIL_CARD_LAYOUT_ZONES.length; i++) {
      var zone = MAIL_CARD_LAYOUT_ZONES[i];
      parts.push(zone + ":" + ((normalized[zone] || []).join(",")));
    }
    return parts.join("|");
  }

  function mailCardEmptyRightRows(layout) {
    var rows = [];
    if (!((layout.meta || []).length)) rows.push("meta");
    if (!((layout.status || []).length)) rows.push("status");
    if (!((layout.corner || []).length)) rows.push("corner");
    return rows.join(" ");
  }

  function cssEscape(value) {
    if (window.CSS && CSS.escape) return CSS.escape(value);
    return String(value).replace(/["\\]/g, "\\$&");
  }

  function applyMailCardLayoutToScopes(root, layout) {
    var scope = root || document;
    var scopes = [];
    if (scope.matches && scope.matches("[data-mail-card-layout-scope]")) scopes.push(scope);
    if (scope.querySelectorAll) {
      var found = scope.querySelectorAll("[data-mail-card-layout-scope]");
      for (var i = 0; i < found.length; i++) scopes.push(found[i]);
    }

    for (var s = 0; s < scopes.length; s++) {
      var card = scopes[s];
      var emptyRightRows = mailCardEmptyRightRows(layout);
      if (emptyRightRows) card.dataset.mailCardEmptyRightRows = emptyRightRows;
      else delete card.dataset.mailCardEmptyRightRows;
      for (var z = 0; z < MAIL_CARD_LAYOUT_ZONES.length; z++) {
        var zone = MAIL_CARD_LAYOUT_ZONES[z];
        var target = card.querySelector('[data-mail-card-zone="' + zone + '"]');
        if (!target) continue;
        var ids = layout[zone] || [];
        for (var j = 0; j < ids.length; j++) {
          var fields = card.querySelectorAll('[data-mail-card-field="' + cssEscape(ids[j]) + '"]');
          for (var k = 0; k < fields.length; k++) target.appendChild(fields[k]);
        }
      }
    }
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

  function applyMailCardSettings(root) {
    var layout = normalizeMailCardLayout(_cache.mail_card_layout, _cache.mail_card_fields);
    var visibleIds = mailCardVisibleIdsFromLayout(layout);
    var hidden = MAIL_CARD_FIELDS.map(function (field) { return field.id; }).filter(function (id) {
      return visibleIds.indexOf(id) === -1;
    }).join(" ");

    var scope = root || document;
    var scroll = scope.id === "mail-list-scroll" ? scope : scope.querySelector && scope.querySelector("#mail-list-scroll");
    if (scroll) {
      scroll.dataset.mailCardHidden = hidden;
      scroll.dataset.mailCardFields = visibleIds.join(",");
      scroll.dataset.mailCardLayout = serializeMailCardLayout(layout);
    }
    applyMailCardLayoutToScopes(scope, layout);
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
        list.style.width = mailListWidthCSS(_cache.mail_list_width, list);
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

  function trimPercent(value) {
    return value.toFixed(4).replace(/\.0+$/, "").replace(/(\.\d*?)0+$/, "$1") + "%";
  }

  function mailListPercentFromPixels(panel, pixels) {
    var parent = panel && panel.parentElement;
    var parentWidth = parent ? parent.getBoundingClientRect().width : 0;
    if (!parentWidth) return null;
    return trimPercent(Math.max(1, Math.min(99, (pixels / parentWidth) * 100)));
  }

  function mailListWidthCSS(value, panel) {
    var raw = String(value || "").trim();
    if (raw.charAt(raw.length - 1) === "%") {
      var percent = parseFloat(raw);
      if (!isNaN(percent) && percent > 0) return "clamp(300px," + percent + "%,calc(100% - 300px))";
    }

    var px = parseFloat(raw);
    if (!isNaN(px) && px > 0) {
      var converted = mailListPercentFromPixels(panel, Math.max(300, px));
      if (converted) {
        _cache.mail_list_width = converted;
        _migratedPanelSize = true;
        return mailListWidthCSS(converted, panel);
      }
      return Math.max(300, px) + "px";
    }

    return "clamp(300px,50%,calc(100% - 300px))";
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

  window.applyMailCardFields = function (value, root) {
    _cache.mail_card_fields = normalizeMailCardFieldIds(value).join(",");
    _cache.mail_card_layout = serializeMailCardLayout(normalizeMailCardLayout(_cache.mail_card_layout, _cache.mail_card_fields));
    applyMailCardSettings(root);
  };

  window.applyMailCardLayout = function (value, fieldsValue, root) {
    var layout = normalizeMailCardLayout(value, fieldsValue || _cache.mail_card_fields);
    _cache.mail_card_layout = serializeMailCardLayout(layout);
    _cache.mail_card_fields = mailCardVisibleIdsFromLayout(layout).join(",");
    applyMailCardSettings(root);
  };

  window.applyMailCardFieldSettings = applyMailCardSettings;
  window.applyMailCardLayoutSettings = applyMailCardSettings;

  window.getMailCardFields = function () {
    return normalizeMailCardFieldIds(_cache.mail_card_fields);
  };

  window.getMailCardLayout = function () {
    return normalizeMailCardLayout(_cache.mail_card_layout, _cache.mail_card_fields);
  };

  window.getDefaultMailCardLayout = function () {
    return normalizeMailCardLayout(DEFAULT_MAIL_CARD_LAYOUT, DEFAULT_MAIL_CARD_FIELDS);
  };

  window.getDefaultMailCardFields = function () {
    return normalizeMailCardFieldIds(DEFAULT_MAIL_CARD_FIELDS);
  };

  window.getMailCardLayoutZones = function () {
    return MAIL_CARD_LAYOUT_ZONES.slice();
  };

  window.getMailCardVisibleFieldsFromLayout = mailCardVisibleIdsFromLayout;
  window.serializeMailCardLayout = serializeMailCardLayout;

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
        panel.style.width = mailListWidthCSS(value, panel);
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
    if (key === "mail_card_fields") {
      _cache.mail_card_fields = normalizeMailCardFieldIds(value).join(",");
      _cache.mail_card_layout = serializeMailCardLayout(normalizeMailCardLayout(_cache.mail_card_layout, _cache.mail_card_fields));
      applyMailCardSettings();
    }
    if (key === "mail_card_layout") {
      _cache.mail_card_layout = serializeMailCardLayout(normalizeMailCardLayout(value, _cache.mail_card_fields));
      applyMailCardSettings();
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
      if (_migratedPanelSize) persistSettingsNow();
    },

    get: function (key) {
      return _cache[key] || null;
    },

    set: function (key, value) {
      var oldValue = _cache[key] || null;
      _cache[key] = value;
      writeCache();
      applySetting(key, value);
      document.body.dispatchEvent(new CustomEvent("gofer:settings-changed", {
        detail: { key: key, value: value, oldValue: oldValue },
      }));

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
