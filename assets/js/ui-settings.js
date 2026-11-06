var GoferSettings;

(function () {
  var LS_KEY = "gofer:ui_settings";
  var _cache = {};
  var _saveTimer = null;
  var _migratedPanelSize = false;

  // Contacts and calendar keep their own panel sizes and pane layout. Mail
  // owns the original keys, and a section that was never resized uses mail's
  // size. The layout does not fall back: each section starts side by side.
  var PANEL_SECTION_KEYS = {
    contacts: { sidebar_width: "contacts_sidebar_width", mail_list_width: "contacts_list_width", mail_list_height: "contacts_list_height", mail_pane_layout: "contacts_pane_layout" },
    calendar: { sidebar_width: "calendar_sidebar_width", mail_list_width: "calendar_list_width", mail_list_height: "calendar_list_height", mail_pane_layout: "calendar_pane_layout" },
  };

  function panelSection() {
    var list = document.getElementById("mail-list");
    return (list && list.dataset.panelSection) || "mail";
  }

  function sectionKey(section, key) {
    var keys = PANEL_SECTION_KEYS[section];
    return (keys && keys[key]) || key;
  }

  function panelKey(key) {
    return sectionKey(panelSection(), key);
  }

  // Only mail can do without its second pane; contacts and calendar keep
  // theirs side by side.
  function paneLayout(section) {
    section = section || panelSection();
    var layout = mailPaneLayout(_cache[sectionKey(section, "mail_pane_layout")]);
    return layout === "list" && PANEL_SECTION_KEYS[section] ? "side" : layout;
  }

  function panelSetting(key, section) {
    return _cache[sectionKey(section || panelSection(), key)] || _cache[key] || null;
  }

  function isSectionKey(base, key) {
    if (key === base) return true;
    for (var section in PANEL_SECTION_KEYS) {
      if (PANEL_SECTION_KEYS[section][base] === key) return true;
    }
    return false;
  }

  function isPanelSizeKey(key) {
    return isSectionKey("sidebar_width", key) || isSectionKey("mail_list_width", key) || isSectionKey("mail_list_height", key);
  }

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

  // Express has no dark mode: it always shows light, while the saved choice is
  // kept for the other styles.
  // Express is always light, and the Custom theme is shown in the mode it was
  // built for.
  function effectiveTheme(theme, style) {
    if (style === "express") return "light";
    if (style === "custom") {
      var custom = customTheme();
      if (custom) return custom.mode === "dark" ? "dark" : "light";
    }
    return theme;
  }

  function applyTheme(theme) {
    var html = document.documentElement;
    if (effectiveTheme(theme, html.getAttribute("data-theme")) === "dark") {
      html.classList.add("dark");
    } else {
      html.classList.remove("dark");
    }
  }

  // The look that goes with each style: flat surfaces and square controls
  // for Minimal, and what the Custom theme was built with. Mirrors
  // themeLookAttributes in internal/views/helpers.go.
  var STYLE_LOOKS = { minimal: { depth: "flat", corners: "square" } };

  // The saved Custom themes. Before themes had names there was one, kept in
  // custom_theme with its hand edits in theme_colors_custom; it reads as the
  // first of the list. Mirrors themebuilder.ListFromSettings.
  function customThemes() {
    var list = (_cache.custom_themes || "").trim();
    if (list) {
      try {
        var themes = JSON.parse(list);
        return Array.isArray(themes) ? themes : [];
      } catch (_) {
        return [];
      }
    }
    try {
      var theme = JSON.parse(_cache.custom_theme || "null");
      if (!theme || !theme.seed) return [];
      var colors = JSON.parse(_cache.theme_colors_custom || "null");
      return [Object.assign({ id: "theme1", name: goferT("js.settings.theme.default_name") }, theme, colors ? { colors: colors } : {})];
    } catch (_) {
      return [];
    }
  }

  // The Custom theme the page is drawn with when the Custom style is chosen.
  function customTheme() {
    var themes = customThemes();
    for (var i = 0; i < themes.length; i++) {
      if (themes[i].id === _cache.custom_theme_active) return themes[i];
    }
    return themes[0] || null;
  }

  function styleLook(style) {
    if (style !== "custom") return STYLE_LOOKS[style] || {};
    var theme = customTheme() || {};
    return {
      depth: theme.depth === "flat" ? "flat" : "",
      corners: theme.corners === "square" ? "square" : "",
    };
  }

  function applyThemeStyle(style) {
    var html = document.documentElement;
    // Custom falls back to Classic when no theme is saved.
    if (style === "custom" && !customTheme()) style = "classic";
    var look = styleLook(style);
    html.setAttribute("data-theme", style || "classic");
    if (look.depth) html.setAttribute("data-depth", look.depth);
    else html.removeAttribute("data-depth");
    if (look.corners) html.setAttribute("data-corners", look.corners);
    else html.removeAttribute("data-corners");
    applyTheme(_cache.theme || "dark");
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

  // Card layouts and table columns of the mail and contacts lists. Each list
  // describes its fields and zones once (cardLayoutModel, tableColumnsModel);
  // the layout editor (mail-card-layout.js) and the column menu and resizing
  // (app.js) work from the same descriptions, through GoferListLayouts.

  function cardLayoutModel(spec) {
    var fieldIds = spec.fields.slice();
    var zones = spec.zones.slice();
    var visibleZones = zones.filter(function (zone) { return zone !== "hidden"; });
    var allowed = {};
    for (var i = 0; i < fieldIds.length; i++) allowed[fieldIds[i]] = true;

    function emptyLayout() {
      var layout = {};
      for (var i = 0; i < zones.length; i++) layout[zones[i]] = [];
      return layout;
    }

    function zoneAllowed(zone) {
      return zones.indexOf(zone) !== -1;
    }

    function defaultZone(id) {
      return spec.defaultZoneById[id] || spec.fallbackZone;
    }

    function fieldType(id) {
      return spec.iconFields[id] ? "icon" : "text";
    }

    function zoneAcceptsField(zone, id) {
      if (zone === "hidden") return true;
      // A field kept to some zones, as the contact avatar is to its own.
      if (spec.fieldZones && spec.fieldZones[id]) return spec.fieldZones[id].indexOf(zone) !== -1;
      if (fieldType(id) === "icon") return spec.sideZones.indexOf(zone) !== -1;
      return spec.centerZones.indexOf(zone) !== -1;
    }

    function zoneHasRoom(layout, zone) {
      var max = spec.zoneMax[zone];
      return !max || ((layout[zone] || []).length < max);
    }

    function compatibleZone(layout, id, preferredZone) {
      var candidates = fieldType(id) === "icon" ? spec.sideZones : spec.centerZones;
      var fallbackZone = defaultZone(id);

      if (zoneAcceptsField(preferredZone, id) && zoneHasRoom(layout, preferredZone)) return preferredZone;
      if (zoneAcceptsField(fallbackZone, id) && zoneHasRoom(layout, fallbackZone)) return fallbackZone;

      for (var i = 0; i < candidates.length; i++) {
        if (zoneAcceptsField(candidates[i], id) && zoneHasRoom(layout, candidates[i])) return candidates[i];
      }
      return candidates[0] || spec.fallbackZone;
    }

    function normalizeFieldIds(value) {
      var ids = [];
      var parts = (value ? String(value) : spec.defaultFields).split(",");
      for (var j = 0; j < parts.length; j++) {
        var id = parts[j].trim();
        if (allowed[id] && ids.indexOf(id) === -1) ids.push(id);
      }
      if (ids.length === 0) ids.push(spec.fallbackField);
      return ids;
    }

    function parse(value) {
      var layout = emptyLayout();
      var seen = {};
      var raw = value ? String(value) : spec.defaultLayout;
      if (spec.legacyDefaults.indexOf(raw) !== -1) raw = spec.defaultLayout;
      // Mail layouts saved before the middle had two lines stacked their body
      // fields; the first stays on the upper line and the next go to the lower one.
      var splitBody = !!spec.legacySplitBody && raw.indexOf("bodyBottom:") === -1;
      var groups = raw.split("|");

      for (var i = 0; i < groups.length; i++) {
        var group = groups[i];
        var sep = group.indexOf(":");
        if (sep === -1) continue;
        var zone = group.slice(0, sep).trim();
        var ids = group.slice(sep + 1).split(",");
        if (zone === "rail" && spec.legacyRail) {
          var railIds = [];
          for (var r = 0; r < ids.length; r++) {
            var railID = ids[r].trim();
            if (allowed[railID] && !seen[railID]) railIds.push(railID);
          }
          for (var n = 0; n < railIds.length; n++) {
            var railZone = spec.legacyRail(n, railIds.length);
            layout[railZone].push(railIds[n]);
            seen[railIds[n]] = true;
          }
          continue;
        }
        if (!zoneAllowed(zone)) continue;
        for (var j = 0; j < ids.length; j++) {
          var id = ids[j].trim();
          if (!allowed[id] || seen[id]) continue;
          var target = splitBody && zone === "body" && layout.body.length ? "bodyBottom" : zone;
          // A saved layout over a zone's limit moves the extra fields elsewhere.
          layout[zoneHasRoom(layout, target) ? target : compatibleZone(layout, id, target)].push(id);
          seen[id] = true;
        }
      }

      if (spec.upgrade && value) spec.upgrade(layout, raw);

      for (var k = 0; k < fieldIds.length; k++) {
        if (!seen[fieldIds[k]]) layout[defaultZone(fieldIds[k])].push(fieldIds[k]);
      }

      return layout;
    }

    function normalize(value, fieldsValue) {
      // An untouched old default also saved the old default fields, which would
      // keep fields hidden that the current default shows.
      if (value && spec.legacyDefaults.indexOf(String(value)) !== -1) fieldsValue = spec.defaultFields;
      var parsed = parse(value);
      var visibleIds = normalizeFieldIds(fieldsValue);
      var visible = {};
      var layout = emptyLayout();

      for (var i = 0; i < visibleIds.length; i++) visible[visibleIds[i]] = true;

      for (var z = 0; z < zones.length; z++) {
        var zone = zones[z];
        var ids = parsed[zone] || [];
        for (var j = 0; j < ids.length; j++) {
          var id = ids[j];
          if (visible[id]) {
            var targetZone = zone === "hidden" ? defaultZone(id) : zone;
            layout[compatibleZone(layout, id, targetZone)].push(id);
          } else {
            layout.hidden.push(id);
          }
        }
      }

      return layout;
    }

    function visibleIdsFromLayout(layout) {
      var ids = [];
      for (var i = 0; i < visibleZones.length; i++) {
        var zoneIds = layout[visibleZones[i]] || [];
        for (var j = 0; j < zoneIds.length; j++) {
          if (ids.indexOf(zoneIds[j]) === -1) ids.push(zoneIds[j]);
        }
      }
      if (ids.length === 0) ids.push(spec.fallbackField);
      return ids;
    }

    function serialize(layout) {
      var normalized = layout || emptyLayout();
      var parts = [];
      for (var i = 0; i < zones.length; i++) {
        var zone = zones[i];
        parts.push(zone + ":" + ((normalized[zone] || []).join(",")));
      }
      return parts.join("|");
    }

    // Moves each card's fields into the zones the layout gives them.
    function applyToScopes(root, layout) {
      var scope = root || document;
      var scopes = [];
      if (scope.matches && scope.matches(spec.scopeSelector)) scopes.push(scope);
      if (scope.querySelectorAll) {
        var found = scope.querySelectorAll(spec.scopeSelector);
        for (var i = 0; i < found.length; i++) scopes.push(found[i]);
      }

      for (var s = 0; s < scopes.length; s++) {
        var card = scopes[s];
        for (var z = 0; z < zones.length; z++) {
          var zone = zones[z];
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

    function current() {
      return normalize(_cache[spec.layoutKey], _cache[spec.fieldsKey]);
    }

    // Saves a layout, with the fields it shows.
    function save(layout) {
      var visible = visibleIdsFromLayout(layout);
      GoferSettings.set(spec.fieldsKey, visible.join(","));
      GoferSettings.set(spec.layoutKey, serialize(layout));
    }

    return {
      fields: fieldIds,
      zones: zones,
      visibleZones: visibleZones,
      iconFields: spec.iconFields,
      zoneAcceptsField: zoneAcceptsField,
      sideZones: spec.sideZones,
      centerZones: spec.centerZones,
      zoneMax: spec.zoneMax,
      layoutKey: spec.layoutKey,
      fieldsKey: spec.fieldsKey,
      normalizeFieldIds: normalizeFieldIds,
      normalize: normalize,
      visibleIdsFromLayout: visibleIdsFromLayout,
      serialize: serialize,
      applyToScopes: applyToScopes,
      current: current,
      defaultLayout: function () { return normalize(spec.defaultLayout, spec.defaultFields); },
      save: save,
    };
  }

  function tableColumnsModel(spec) {
    var ids = spec.columns.map(function (column) { return column.id; });

    function normalizeIds(value) {
      var allowed = {};
      var result = [];
      for (var i = 0; i < ids.length; i++) allowed[ids[i]] = true;
      var parts = value ? String(value).split(",") : spec.defaultIds;
      for (var j = 0; j < parts.length; j++) {
        var id = parts[j].trim();
        if (allowed[id] && result.indexOf(id) === -1) result.push(id);
      }
      if (result.length === 0) result.push(spec.fallbackId);
      return result;
    }

    function ratios(value) {
      var parts = value ? String(value).split(",") : [];
      if (spec.legacyColumnIds && parts.length === spec.legacyColumnIds.length && parts.length !== spec.columns.length) {
        // Each saved width goes to its column; the new ones get their default.
        var defaults = spec.defaultWidths.split(",");
        parts = ids.map(function (id, i) {
          var old = spec.legacyColumnIds.indexOf(id);
          return old === -1 ? defaults[i] : parts[old];
        });
      }
      if (parts.length !== spec.columns.length) parts = spec.defaultWidths.split(",");
      var values = [];
      for (var i = 0; i < parts.length; i++) {
        var n = parseFloat(parts[i]);
        if (isNaN(n)) n = 1;
        values.push(Math.max(0.01, n));
      }
      return values;
    }

    function widthsCSS(value, visibleIds) {
      var values = ratios(value);
      var visible = {};
      var total = 0;
      for (var j = 0; j < visibleIds.length; j++) visible[visibleIds[j]] = true;
      for (var k = 0; k < spec.columns.length; k++) {
        if (visible[spec.columns[k].id] && !spec.columns[k].fixed) total += values[k];
      }
      if (total <= 0) return null;

      var columns = [];
      for (var l = 0; l < spec.columns.length; l++) {
        if (!visible[spec.columns[l].id]) continue;
        if (spec.columns[l].fixed) {
          columns.push(spec.columns[l].fixed + "px");
          continue;
        }
        columns.push("minmax(" + spec.columns[l].min + "px, " + (values[l] / total).toFixed(5) + "fr)");
      }
      return columns.join(" ");
    }

    function scrollIn(root) {
      var scope = root || document;
      if (scope.id === spec.scrollId) return scope;
      return (scope.querySelector && scope.querySelector("#" + spec.scrollId)) || document.getElementById(spec.scrollId);
    }

    // Shows the chosen columns at their saved widths.
    function apply(root) {
      var visibleIds = normalizeIds(_cache[spec.columnsKey]);
      var columns = widthsCSS(_cache[spec.widthsKey], visibleIds);
      if (!columns) return;
      var hidden = ids.filter(function (id) { return visibleIds.indexOf(id) === -1; }).join(" ");
      if (spec.rootVariable) document.documentElement.style.setProperty("--mail-list-table-columns", columns);
      var scroll = scrollIn(root);
      if (scroll) {
        scroll.style.setProperty("--mail-list-table-columns", columns);
        scroll.dataset.mailTableHidden = hidden;
        scroll.dataset.mailTableColumns = visibleIds.join(",");
      }
    }

    return {
      columns: spec.columns,
      ids: ids,
      scrollId: spec.scrollId,
      columnsKey: spec.columnsKey,
      widthsKey: spec.widthsKey,
      normalizeIds: normalizeIds,
      ratios: ratios,
      current: function () { return normalizeIds(_cache[spec.columnsKey]); },
      apply: apply,
      // Shows widths while a column is being resized, before they are saved.
      applyWidths: function (value, root) {
        _cache[spec.widthsKey] = value;
        apply(root);
      },
    };
  }

  function cssEscape(value) {
    if (window.CSS && CSS.escape) return CSS.escape(value);
    return String(value).replace(/["\\]/g, "\\$&");
  }

  function legacyRailZone(index, count) {
    if (count <= 1) return "railTop";
    if (count === 2) return index === 0 ? "railTop" : "railBottom";
    if (index === 0) return "railTop";
    if (index === 1) return "railMiddle";
    return "railBottom";
  }

  var MAIL_CARD = cardLayoutModel({
    fields: ["avatar", "thread", "from", "account", "accountMarker", "to", "cc", "date", "subject", "preview", "labels", "attachment", "starred", "unread"],
    defaultFields: "avatar,thread,from,account,to,attachment,date,subject,preview,labels,starred",
    zones: ["railTop", "header", "meta", "railMiddle", "body", "bodyBottom", "status", "railBottom", "footer", "corner", "hidden"],
    defaultLayout: "railTop:avatar|header:account,from,date|meta:attachment|railMiddle:|body:to|bodyBottom:subject|status:|railBottom:thread|footer:preview,labels|corner:starred|hidden:accountMarker,unread,cc",
    legacyDefaults: [
      "rail:avatar,thread|header:from,account|meta:attachment,date|body:subject,to,preview|footer:labels,starred|status:unread|hidden:",
      "rail:avatar,thread|header:from|meta:attachment,date|body:subject,preview|footer:labels,starred|status:unread|hidden:account,to",
      "rail:avatar,thread|header:from|meta:attachment,date|body:subject|footer:preview,labels,starred|status:unread|hidden:account,to",
      "rail:avatar,thread|header:from|meta:attachment,date,unread|body:subject|footer:preview,labels|status:|corner:starred|hidden:account,accountMarker,to",
      "rail:avatar,thread|header:from,date|meta:attachment,unread|body:subject|footer:preview,labels|status:|corner:starred|hidden:account,accountMarker,to",
      "railTop:avatar|header:from,date|meta:attachment,unread|railMiddle:|body:subject|status:|railBottom:thread|footer:preview,labels|corner:starred|hidden:account,accountMarker,to",
    ],
    legacySplitBody: true,
    legacyRail: legacyRailZone,
    iconFields: { avatar: true, thread: true, accountMarker: true, attachment: true, starred: true, unread: true },
    sideZones: ["railTop", "railMiddle", "railBottom", "meta", "status", "corner"],
    centerZones: ["header", "body", "bodyBottom", "footer"],
    // How many fields a zone holds. The middle is two lines, body and
    // bodyBottom, of two fields each: more would not fit in the card.
    zoneMax: { body: 2, bodyBottom: 2, railTop: 1, railMiddle: 1, railBottom: 1, meta: 3, status: 3, corner: 3 },
    defaultZoneById: {
      avatar: "railTop",
      accountMarker: "railMiddle",
      thread: "railBottom",
      from: "header",
      account: "header",
      attachment: "meta",
      date: "header",
      unread: "meta",
      subject: "bodyBottom",
      to: "body",
      cc: "body",
      preview: "footer",
      labels: "footer",
      starred: "corner",
    },
    fallbackZone: "body",
    fallbackField: "subject",
    scopeSelector: "[data-mail-card-layout-scope]",
    layoutKey: "mail_card_layout",
    fieldsKey: "mail_card_fields",
  });

  // A contact card is one or two lines beside the avatar. Each line has text
  // at its start (header, body), text at its end (headerEnd, bodyEnd) and a
  // slot for icons after it. By default: the name and organization with the
  // message count and Gofer Sync, then the address, with the main location
  // and the date it was created at the end.
  var CONTACT_CARD = cardLayoutModel({
    // "date" is the Interaction field (last mail); it kept its id when Created
    // and Edited joined it, so saved layouts keep it where it was.
    fields: ["avatar", "name", "organization", "title", "email", "phone", "home", "date", "created", "edited", "notes", "messages", "sync"],
    defaultFields: "avatar,name,organization,messages,sync,email,home,created",
    zones: ["railTop", "header", "headerEnd", "meta", "body", "bodyEnd", "status", "hidden"],
    defaultLayout: "railTop:avatar|header:name,organization|headerEnd:|meta:messages,sync|body:email|bodyEnd:home,created|status:|hidden:title,phone,date,edited,notes",
    // Layouts saved before lines had an end kept the main location and date
    // at the start of the line, where the card showed them at its end: they
    // move to the end, so the card looks the same.
    upgrade: function (layout, raw) {
      if (raw.indexOf("headerEnd:") !== -1 || raw.indexOf("bodyEnd:") !== -1) return;
      [["header", "headerEnd"], ["body", "bodyEnd"]].forEach(function (line) {
        layout[line[0]] = layout[line[0]].filter(function (id) {
          if (id !== "home" && id !== "date") return true;
          layout[line[1]].push(id);
          return false;
        });
      });
    },
    // The default before Created was a field, saved as it came.
    legacyDefaults: ["railTop:avatar|header:name,organization|headerEnd:|meta:messages|body:email|bodyEnd:home,date|status:|hidden:title,phone,notes,sync"],
    iconFields: { avatar: true, messages: true, sync: true },
    // The avatar is as tall as the card, so it only fits its own slot; moved
    // to the other side, it flips the card (contacts_card_mirrored).
    fieldZones: { avatar: ["railTop"] },
    sideZones: ["railTop", "meta", "status"],
    centerZones: ["header", "headerEnd", "body", "bodyEnd"],
    zoneMax: { railTop: 1, header: 3, headerEnd: 3, body: 3, bodyEnd: 3, meta: 2, status: 2 },
    defaultZoneById: {
      avatar: "railTop",
      name: "header",
      organization: "header",
      messages: "meta",
      email: "body",
      home: "bodyEnd",
      date: "bodyEnd",
      created: "bodyEnd",
      edited: "bodyEnd",
      title: "header",
      phone: "body",
      notes: "body",
      sync: "meta",
    },
    fallbackZone: "body",
    fallbackField: "name",
    scopeSelector: "[data-contact-card-layout-scope]",
    layoutKey: "contacts_card_layout",
    fieldsKey: "contacts_card_fields",
  });

  var MAIL_TABLE = tableColumnsModel({
    columns: [
      { id: "accountMarker", fixed: 24 },
      { id: "starred", fixed: 24 },
      { id: "attachment", fixed: 24 },
      { id: "thread", fixed: 28 },
      { id: "from", min: 90 },
      { id: "to", min: 90 },
      { id: "subject", min: 140 },
      { id: "date", min: 64 },
    ],
    defaultIds: ["accountMarker", "starred", "attachment", "thread", "from", "to", "subject", "date"],
    defaultWidths: "0.8,0.8,0.8,1,3,3,5,2",
    fallbackId: "subject",
    scrollId: "mail-list-scroll",
    // Mail's widths also reach rows drawn outside the list (the move preview).
    rootVariable: true,
    columnsKey: "mail_table_columns",
    widthsKey: "mail_table_column_widths",
  });

  var CONTACT_TABLE = tableColumnsModel({
    columns: [
      { id: "name", min: 100 },
      { id: "organization", min: 60 },
      { id: "title", min: 60 },
      { id: "email", min: 90 },
      { id: "phone", min: 64 },
      { id: "home", min: 64 },
      { id: "messages", min: 56 },
      { id: "date", min: 56 },
      { id: "created", min: 56 },
      { id: "edited", min: 56 },
      { id: "notes", min: 80 },
      { id: "sync", min: 72 },
    ],
    defaultIds: ["name", "email", "home", "messages"],
    defaultWidths: "3,2,2,3,2,2,0.8,1.4,1.4,1.4,3,1.4",
    // Widths saved before Created and Edited were columns.
    legacyColumnIds: ["name", "organization", "title", "email", "phone", "home", "messages", "date", "notes", "sync"],
    fallbackId: "name",
    scrollId: "contacts-list-scroll",
    columnsKey: "contacts_table_columns",
    widthsKey: "contacts_table_column_widths",
  });

  // The lines of a contact card the layout uses, each with its center zone
  // and its icon slot, top to bottom.
  var CONTACT_CARD_LINES = [["header", "headerEnd", "meta"], ["body", "bodyEnd", "status"]];

  function contactCardLines(layout) {
    return CONTACT_CARD_LINES.filter(function (line) {
      return line.some(function (zone) { return (layout[zone] || []).length; });
    });
  }

  function applyMailTableColumnSettings(root) {
    MAIL_TABLE.apply(root);
  }

  function applyMailCardSettings(root) {
    var layout = MAIL_CARD.current();
    var visibleIds = MAIL_CARD.visibleIdsFromLayout(layout);
    var hidden = MAIL_CARD.fields.filter(function (id) {
      return visibleIds.indexOf(id) === -1;
    }).join(" ");

    var scope = root || document;
    var scroll = scope.id === "mail-list-scroll" ? scope : scope.querySelector && scope.querySelector("#mail-list-scroll");
    if (scroll) {
      scroll.dataset.mailCardHidden = hidden;
      scroll.dataset.mailCardFields = visibleIds.join(",");
      scroll.dataset.mailCardLayout = MAIL_CARD.serialize(layout);
      scroll.toggleAttribute("data-mail-date-time", _cache.mail_card_date_time === "true");
      // On unless turned off, so settings saved before the option keep it.
      scroll.toggleAttribute("data-mail-account-no-marker", _cache.mail_card_account_marker === "false");
    }
    MAIL_CARD.applyToScopes(scope, layout);
  }

  // Lays the contact cards out: their fields in their zones, and the card as
  // tall as the lines it uses, which the list spaces its rows by.
  function applyContactCardSettings(root) {
    var layout = CONTACT_CARD.current();
    var visibleIds = CONTACT_CARD.visibleIdsFromLayout(layout);
    var hidden = CONTACT_CARD.fields.filter(function (id) {
      return visibleIds.indexOf(id) === -1;
    }).join(" ");
    var lines = contactCardLines(layout);
    if (!lines.length) lines = [CONTACT_CARD_LINES[0]];

    var scope = root || document;
    var mirrored = _cache.contacts_card_mirrored === "true";
    var scroll = scope.id === "contacts-list-scroll" ? scope : (scope.querySelector && scope.querySelector("#contacts-list-scroll")) || document.getElementById("contacts-list-scroll");
    if (scroll) {
      scroll.dataset.contactCardHidden = hidden;
      scroll.dataset.contactCardLines = String(lines.length);
      var list = scroll._virtualContactsList;
      if (list && list.contactCardLines !== lines.length && typeof list.refreshRowMetrics === "function") list.refreshRowMetrics();
    }
    CONTACT_CARD.applyToScopes(scope, layout);
    // A flipped card has the avatar on the right and the icon slots on the
    // left (input.css lays it out right to left).
    var cards = scope.querySelectorAll ? Array.prototype.slice.call(scope.querySelectorAll("[data-contact-card-layout-scope]")) : [];
    if (scope.matches && scope.matches("[data-contact-card-layout-scope]")) cards.push(scope);
    for (var i = 0; i < cards.length; i++) cards[i].toggleAttribute("data-contact-card-mirrored", mirrored);
  }

  window.GoferListLayouts = {
    mail: { card: MAIL_CARD, table: MAIL_TABLE, applyCard: applyMailCardSettings },
    contacts: { card: CONTACT_CARD, table: CONTACT_TABLE, applyCard: applyContactCardSettings },
    // How many lines the contact cards use, one or two.
    contactCardLines: function () {
      return Math.max(1, contactCardLines(CONTACT_CARD.current()).length);
    },
  };

  window.applyContactCardLayoutSettings = applyContactCardSettings;
  window.applyContactTableColumnSettings = CONTACT_TABLE.apply;

  function mailPaneLayout(value) {
    return value === "stacked" || value === "list" ? value : "side";
  }

  function applyPaneLayout() {
    var layout = paneLayout();
    var main = document.getElementById("main-content");
    if (!main || !main.querySelector("#mail-list") || !main.querySelector("#mail-view")) return;

    main.dataset.mailPaneLayout = layout;
    main.classList.toggle("flex-col", layout === "stacked");

    var list = document.getElementById("mail-list");
    if (list) {
      if (layout === "stacked") {
        list.style.width = "";
        var height = parseInt(panelSetting("mail_list_height"), 10);
        list.style.height = !isNaN(height) && height > 0 ? height + "px" : "";
      } else {
        list.style.height = "";
        list.style.width = mailListWidthCSS(panelSetting("mail_list_width"), list);
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
        _cache[panelKey("mail_list_width")] = converted;
        _migratedPanelSize = true;
        return mailListWidthCSS(converted, panel);
      }
      return Math.max(300, px) + "px";
    }

    return "clamp(300px,50%,calc(100% - 300px))";
  }

  window.applyMailTableColumnWidths = function (value, root) {
    MAIL_TABLE.applyWidths(value, root);
  };

  window.applyMailTableColumnSettings = applyMailTableColumnSettings;

  window.applyMailTableColumns = function (value, root) {
    _cache.mail_table_columns = MAIL_TABLE.normalizeIds(value).join(",");
    applyMailTableColumnSettings(root);
  };

  window.getMailTableColumns = function () {
    return MAIL_TABLE.current();
  };

  window.applyMailCardFields = function (value, root) {
    _cache.mail_card_fields = MAIL_CARD.normalizeFieldIds(value).join(",");
    _cache.mail_card_layout = MAIL_CARD.serialize(MAIL_CARD.normalize(_cache.mail_card_layout, _cache.mail_card_fields));
    applyMailCardSettings(root);
  };

  window.applyMailCardLayout = function (value, fieldsValue, root) {
    var layout = MAIL_CARD.normalize(value, fieldsValue || _cache.mail_card_fields);
    _cache.mail_card_layout = MAIL_CARD.serialize(layout);
    _cache.mail_card_fields = MAIL_CARD.visibleIdsFromLayout(layout).join(",");
    applyMailCardSettings(root);
  };

  window.applyMailCardFieldSettings = applyMailCardSettings;
  window.applyMailCardLayoutSettings = applyMailCardSettings;

  window.getMailCardFields = function () {
    return MAIL_CARD.normalizeFieldIds(_cache.mail_card_fields);
  };

  window.getMailCardLayout = function () {
    return MAIL_CARD.current();
  };

  window.getDefaultMailCardLayout = function () {
    return MAIL_CARD.defaultLayout();
  };

  window.getDefaultMailCardFields = function () {
    return MAIL_CARD.normalizeFieldIds("");
  };

  window.getMailCardLayoutZones = function () {
    return MAIL_CARD.zones.slice();
  };

  window.getMailCardVisibleFieldsFromLayout = MAIL_CARD.visibleIdsFromLayout;
  window.serializeMailCardLayout = MAIL_CARD.serialize;

  // The chosen fonts' CSS comes from the server, which loads them through
  // Gofer; the page's head carries it from the start.
  var fontChoiceSeq = 0;
  function applyFontChoice() {
    var style = document.querySelector("style[data-font-choice]");
    if (!style) return;
    var seq = ++fontChoiceSeq;
    var query = new URLSearchParams({ body: _cache.body_font || "", heading: _cache.heading_font || "" });
    fetch("/fonts/google/choice.css?" + query.toString(), { credentials: "same-origin" })
      .then(function (res) { return res.ok ? res.text() : Promise.reject(res.status); })
      .then(function (css) { if (seq === fontChoiceSeq) style.textContent = css; })
      .catch(function () {});
  }

  function applySetting(key, value) {
    if (key === "theme") {
      applyTheme(value);
      if (typeof applyEmailBodyTheme === "function") applyEmailBodyTheme();
    }
    if (key === "theme_style") {
      applyThemeStyle(value);
      if (typeof applyEmailBodyTheme === "function") applyEmailBodyTheme();
    }
    // Another Custom theme, or a changed one, has its own mode and look.
    if ((key === "custom_themes" || key === "custom_theme_active") && _cache.theme_style === "custom") {
      applyThemeStyle("custom");
      if (typeof applyEmailBodyTheme === "function") applyEmailBodyTheme();
    }
    if (key === "body_font" || key === "heading_font") applyFontChoice();
    if (isPanelSizeKey(key)) {
      var sidebarWidth = parseInt(panelSetting("sidebar_width"), 10);
      var sidebar = document.querySelector("aside");
      if (sidebar && !isNaN(sidebarWidth) && sidebarWidth > 0) sidebar.style.width = sidebarWidth + "px";

      var list = document.getElementById("mail-list");
      if (list && paneLayout() === "stacked") {
        var listHeight = parseInt(panelSetting("mail_list_height"), 10);
        if (!isNaN(listHeight) && listHeight > 0) list.style.height = listHeight + "px";
        refreshVirtualMailListLayout();
      } else if (list) {
        var listWidth = panelSetting("mail_list_width");
        if (listWidth) list.style.width = mailListWidthCSS(listWidth, list);
      }
    }
    if (isSectionKey("mail_pane_layout", key)) {
      _cache[key] = mailPaneLayout(value);
      if (key === panelKey("mail_pane_layout")) applyPaneLayout();
    }
    if (key === "mail_table_column_widths") {
      _cache.mail_table_column_widths = value;
      applyMailTableColumnSettings();
    }
    if (key === "mail_table_columns") {
      _cache.mail_table_columns = MAIL_TABLE.normalizeIds(value).join(",");
      applyMailTableColumnSettings();
    }
    if (key === "mail_card_fields") {
      _cache.mail_card_fields = MAIL_CARD.normalizeFieldIds(value).join(",");
      _cache.mail_card_layout = MAIL_CARD.serialize(MAIL_CARD.normalize(_cache.mail_card_layout, _cache.mail_card_fields));
      applyMailCardSettings();
    }
    if (key === "mail_card_date_time" || key === "mail_card_account_marker") {
      applyMailCardSettings();
    }
    if (key === "mail_card_layout") {
      _cache.mail_card_layout = MAIL_CARD.serialize(MAIL_CARD.normalize(value, _cache.mail_card_fields));
      applyMailCardSettings();
    }
    if (key === "contacts_table_column_widths") {
      _cache.contacts_table_column_widths = value;
      CONTACT_TABLE.apply();
    }
    if (key === "contacts_table_columns") {
      _cache.contacts_table_columns = CONTACT_TABLE.normalizeIds(value).join(",");
      CONTACT_TABLE.apply();
    }
    if (key === "contacts_card_fields") {
      _cache.contacts_card_fields = CONTACT_CARD.normalizeFieldIds(value).join(",");
      _cache.contacts_card_layout = CONTACT_CARD.serialize(CONTACT_CARD.normalize(_cache.contacts_card_layout, _cache.contacts_card_fields));
      applyContactCardSettings();
    }
    if (key === "contacts_card_mirrored") {
      applyContactCardSettings();
    }
    if (key === "contacts_card_layout") {
      _cache.contacts_card_layout = CONTACT_CARD.serialize(CONTACT_CARD.normalize(value, _cache.contacts_card_fields));
      applyContactCardSettings();
    }
    if (key === "timezone") {
      document.documentElement.setAttribute("data-timezone", value || browserTimezone());
    }
    if (key === "language") {
      // Pages shown before sign-in read the cookie, so they keep the chosen
      // language after signing out. Automatic leaves them to the browser.
      document.cookie = value && value !== "auto"
        ? "gofer_lang=" + encodeURIComponent(value) + "; path=/; max-age=31536000; samesite=lax"
        : "gofer_lang=; path=/; max-age=0; samesite=lax";
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

    // The key a panel size is saved under in the section on screen.
    panelKey: panelKey,

    // A panel size for a section, or the one on screen, falling back to mail's.
    panelSetting: panelSetting,

    // "side" or "stacked" for a section, or for the one on screen.
    paneLayout: paneLayout,

    // Lays the panes out for the section now on screen.
    applyPaneLayout: applyPaneLayout,

    // The saved Custom themes, and the one the page is drawn with.
    customThemes: customThemes,
    activeCustomTheme: customTheme,

    // Saves the Custom themes. The list replaces the settings that held the
    // one theme before themes had names, as the server does.
    setCustomThemes: function (themes) {
      _cache.custom_theme = "";
      _cache.theme_colors_custom = "";
      GoferSettings.set("custom_themes", JSON.stringify(themes));
    },

    // Saves every setting now, rather than after the usual pause.
    saveNow: function () {
      if (_saveTimer) clearTimeout(_saveTimer);
      _saveTimer = null;
      return persistSettingsNow();
    },

    // The light or dark mode actually shown, which Express forces to light.
    effectiveTheme: function () {
      return effectiveTheme(_cache.theme || "dark", _cache.theme_style || "classic");
    },

    set: function (key, value) {
      var oldValue = _cache[key] || null;
      _cache[key] = value;
      writeCache();
      applySetting(key, value);
      document.body.dispatchEvent(new CustomEvent("gofer:settings-changed", {
        detail: { key: key, value: value, oldValue: oldValue },
      }));

      // The page is drawn in the time zone, language and date formats, so
      // reload it.
      var drawn = key === "language" || key === "date_format" || key === "time_format";
      if ((key === "timezone" && oldValue !== value) || (drawn && (oldValue || "auto") !== (value || "auto"))) {
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
