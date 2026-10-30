// Settings pages and account dialogs. This code used to sit in the pages as
// inline scripts; Gofer's pages may only run scripts from its own files.
// Setup that ran when a part of the page arrived now runs through
// goferOnContent (actions.js) when that part appears.

// Compose and display tab
(function() {
  function composeAutosaveEnabled() {
    var toggle = document.querySelector('[data-compose-autosave-setting="enabled"]');
    return !toggle || toggle.checked;
  }
  function selectedComposeAutosaveConditions() {
    var input = document.querySelector('[data-compose-autosave-conditions-value]');
    return (input && input.value ? input.value : '').split(',').filter(Boolean);
  }
  function saveComposeAutosaveConditions(values) {
    var input = document.querySelector('[data-compose-autosave-conditions-value]');
    if (input) input.value = values.join(',');
    if (typeof GoferSettings !== 'undefined') GoferSettings.set('compose_autosave_conditions', values.join(','));
  }
  function composeAutosaveConditionLabel(values) {
    if (!values.length) return 'No conditions';
    return values.length === 1 ? '1 condition' : values.length + ' conditions';
  }
  function syncComposeAutosaveControls() {
    var enabled = composeAutosaveEnabled();
    var selected = selectedComposeAutosaveConditions();
    document.querySelectorAll('[data-compose-autosave-dependent] .select-trigger, [data-compose-autosave-conditions-button], [data-compose-autosave-min-chars]').forEach(function(el) {
      el.disabled = !enabled;
      el.classList.toggle('opacity-50', !enabled);
    });
    document.querySelectorAll('[data-compose-autosave-condition-check]').forEach(function(check) {
      check.classList.toggle('opacity-0', selected.indexOf(check.dataset.composeAutosaveConditionCheck) === -1);
    });
    var label = document.querySelector('[data-compose-autosave-conditions-label]');
    if (label) label.textContent = composeAutosaveConditionLabel(selected);
    var minChars = document.querySelector('[data-compose-autosave-min-chars]');
    if (minChars) minChars.disabled = !enabled || selected.indexOf('chars') === -1;
  }
  document.addEventListener('click', function(e) {
    var item = e.target.closest && e.target.closest('[data-compose-autosave-condition-item]');
    if (!item || e.target.closest('[data-compose-autosave-min-chars]')) return;
    var selected = selectedComposeAutosaveConditions();
    var value = item.dataset.composeAutosaveConditionItem;
    var index = selected.indexOf(value);
    if (index === -1) selected.push(value);
    else selected.splice(index, 1);
    saveComposeAutosaveConditions(selected);
    syncComposeAutosaveControls();
  });
  document.addEventListener('change', function(e) {
    var defaultComposeView = e.target.closest('input[name="default_new_compose_view"], input[name="default_reply_compose_view"]');
    if (defaultComposeView && typeof GoferSettings !== 'undefined') {
      GoferSettings.set(defaultComposeView.name, defaultComposeView.value);
    }
    var paneLayout = e.target.closest('input[name="mail_pane_layout"], input[name="contacts_pane_layout"], input[name="calendar_pane_layout"]');
    if (paneLayout && typeof GoferSettings !== 'undefined') {
      GoferSettings.set(paneLayout.name, paneLayout.value);
    }
    var autosaveToggle = e.target.closest('[data-compose-autosave-setting="enabled"]');
    if (autosaveToggle && typeof GoferSettings !== 'undefined') {
      GoferSettings.set('compose_autosave_enabled', autosaveToggle.checked ? 'true' : 'false');
      syncComposeAutosaveControls();
    }
    var minChars = e.target.closest('[data-compose-autosave-min-chars]');
    if (minChars && typeof GoferSettings !== 'undefined') {
      var n = Math.max(1, Math.min(1000, parseInt(minChars.value.replace(/\D/g, '') || '30', 10) || 30));
      minChars.value = n;
      GoferSettings.set('compose_autosave_min_chars', String(n));
    }
    var autosaveDebounce = e.target.closest('input[name="compose_autosave_debounce"]');
    if (autosaveDebounce && typeof GoferSettings !== 'undefined') {
      GoferSettings.set('compose_autosave_debounce', autosaveDebounce.value);
    }
    var senderDisplay = e.target.closest('input[name="sender_display"]');
    if (senderDisplay && typeof GoferSettings !== 'undefined') {
      GoferSettings.set('sender_display', senderDisplay.value);
    }
    var mailListNavigation = e.target.closest('input[name="mail_list_navigation"]');
    if (mailListNavigation && typeof GoferSettings !== 'undefined') {
      GoferSettings.set('mail_list_navigation', mailListNavigation.value);
    }
    var emailBodyStyle = e.target.closest('input[name="email_body_style"]');
    if (emailBodyStyle && typeof GoferSettings !== 'undefined') {
      GoferSettings.set('email_body_style', emailBodyStyle.value);
    }
    var groupConversations = e.target.closest('[data-group-conversations-setting]');
    if (groupConversations && typeof GoferSettings !== 'undefined') {
      GoferSettings.set('group_conversations', groupConversations.checked ? 'true' : 'false');
    }
    var threadOrder = e.target.closest('input[name="thread_order"]');
    if (threadOrder && typeof GoferSettings !== 'undefined') {
      GoferSettings.set('thread_order', threadOrder.value);
    }
    var autoMarkRead = e.target.closest('input[name="auto_mark_read_after"]');
    if (autoMarkRead && typeof GoferSettings !== 'undefined') {
      GoferSettings.set('auto_mark_read_after', autoMarkRead.value);
    }
    var translationEnabled = e.target.closest('[data-translation-setting="enabled"]');
    if (translationEnabled && typeof GoferSettings !== 'undefined') {
      GoferSettings.set('translation_button_enabled', translationEnabled.checked ? 'true' : 'false');
    }
    var translationProvider = e.target.closest('input[name="translation_provider"]');
    if (translationProvider && typeof GoferSettings !== 'undefined') {
      GoferSettings.set('translation_provider', translationProvider.value);
    }
    var translationLanguage = e.target.closest('input[name="translation_target_language"]');
    if (translationLanguage && typeof GoferSettings !== 'undefined') {
      GoferSettings.set('translation_target_language', translationLanguage.value);
    }
  });
  document.addEventListener('input', function(e) {
    var minChars = e.target.closest && e.target.closest('[data-compose-autosave-min-chars]');
    if (!minChars) return;
    minChars.value = minChars.value.replace(/\D/g, '').slice(0, 4);
  });
  goferOnContent('[data-compose-autosave-conditions-value]', function() { syncComposeAutosaveControls(); });
})();

// Settings that only matter while a switch is on are marked
// data-setting-depends-on="<switch id>". They are turned off and faded with it.
(function() {
  function syncSettingDependents() {
    document.querySelectorAll('[data-setting-depends-on]').forEach(function(section) {
      var master = document.getElementById(section.getAttribute('data-setting-depends-on'));
      if (!master) return;
      // A switch that can't be used right now (notifications blocked for the
      // chosen method) leaves its settings open, so they can be changed.
      var enabled = master.checked || master.disabled;
      section.classList.toggle('opacity-50', !enabled);
      section.querySelectorAll('button, input:not([type="hidden"]), select, textarea').forEach(function(control) {
        control.disabled = !enabled;
      });
    });
  }
  window.goferSyncSettingDependents = syncSettingDependents;
  goferOnContent('[data-setting-depends-on]', function() { syncSettingDependents(); });
  document.addEventListener('change', function(e) {
    if (e.target && e.target.matches && e.target.matches('input[role="switch"]')) syncSettingDependents();
  });
})();

// Appearance tab
(function() {
  document.addEventListener('click', function(e) {
    var mode = e.target.closest('[data-mode]');
    if (mode) {
      if (typeof GoferSettings !== 'undefined') {
        GoferSettings.set('theme', mode.dataset.mode);
      }
      var isDark = mode.dataset.mode === 'dark';
      document.querySelectorAll('[data-theme-preview]').forEach(function(el) {
        // Express is always light, and the Custom theme keeps its own mode.
        if (el.getAttribute('data-theme') === 'custom') return;
        el.classList.toggle('dark', isDark && el.getAttribute('data-theme') !== 'express');
      });
      return;
    }
  });
  document.addEventListener('change', function(e) {
    var themeStyle = e.target.closest('input[name="theme_style"]');
    // A Custom theme's card also picks which theme; the builder code does that.
    if (themeStyle && !themeStyle.hasAttribute('data-custom-theme-id') && typeof GoferSettings !== 'undefined') {
      GoferSettings.set('theme_style', themeStyle.value);
    }
  });
})();

// Style colors. A style's changed colors are saved as theme_colors_<style>,
// and every edit is previewed on the page with CSS from the server. Closing
// the editor without saving puts the page back as it was.
(function() {
  var EDITOR_ID = 'theme-colors-dialog';
  var DISCARD_ID = 'theme-colors-discard-dialog';
  var style = null;
  var editing = null;
  var mode = 'light';
  var before = null;
  var saved = false;
  var previewSeq = 0;
  var previewTimer = null;
  var discardStyle = null;
  var defaultsCache = {};

  function hexColor(value) {
    value = (value || '').trim().toLowerCase();
    return /^#[0-9a-f]{6}$/.test(value) ? value : '';
  }

  function settingKey(name) {
    return 'theme_colors_' + name;
  }

  function styleLabel(name) {
    if (name === 'custom') return 'Custom';
    return name === 'minimal' ? 'Minimal' : 'Classic';
  }

  // Each Custom theme keeps its own changed colors, and the editor edits the
  // one the page is drawn with.
  function savedOverrides(name) {
    try {
      var parsed = name === 'custom'
        ? (GoferSettings.activeCustomTheme() || {}).colors || {}
        : JSON.parse(GoferSettings.get(settingKey(name)) || '{}') || {};
      return { light: Object.assign({}, parsed.light), dark: Object.assign({}, parsed.dark) };
    } catch (_) {
      return { light: {}, dark: {} };
    }
  }

  function saveCustomColors(value) {
    var active = GoferSettings.activeCustomTheme();
    if (!active) return;
    GoferSettings.setCustomThemes(GoferSettings.customThemes().map(function(theme) {
      if (theme.id !== active.id) return theme;
      var changed = Object.assign({}, theme);
      if (value) changed.colors = JSON.parse(value);
      else delete changed.colors;
      return changed;
    }));
    // The theme's card is drawn with its colors; the builder code redraws it.
    document.dispatchEvent(new CustomEvent('gofer:custom-themes-changed'));
  }

  function changedCount(overrides) {
    return Object.keys(overrides.light).length + Object.keys(overrides.dark).length;
  }

  function encode(overrides) {
    if (!changedCount(overrides)) return '';
    var out = {};
    if (Object.keys(overrides.light).length) out.light = overrides.light;
    if (Object.keys(overrides.dark).length) out.dark = overrides.dark;
    return JSON.stringify(out);
  }

  function editorRoot() {
    return document.getElementById(EDITOR_ID);
  }

  function variableNames() {
    var root = editorRoot();
    if (!root) return [];
    return Array.prototype.map.call(root.querySelectorAll('[data-theme-colors-row]'), function(row) {
      return row.getAttribute('data-theme-colors-row');
    });
  }

  // The colors a style ships with. The probe has none of the attributes the
  // saved changes apply to, so it reads the stylesheet alone. The Custom
  // theme's colors change when it is rebuilt, so they are not cached.
  function shippedColors(name, colorMode) {
    var cacheKey = name + ':' + colorMode;
    if (name !== 'custom' && defaultsCache[cacheKey]) return defaultsCache[cacheKey];
    var probe = document.createElement('div');
    probe.setAttribute('data-theme', name);
    if (colorMode === 'dark') probe.className = 'dark';
    probe.style.display = 'none';
    document.body.appendChild(probe);
    var styles = getComputedStyle(probe);
    var colors = {};
    variableNames().forEach(function(variable) {
      colors[variable] = hexColor(styles.getPropertyValue('--' + variable));
    });
    probe.remove();
    defaultsCache[cacheKey] = colors;
    return colors;
  }

  function styleBlock(name) {
    var block = document.querySelector('style[data-theme-colors="' + name + '"]');
    if (!block) {
      block = document.createElement('style');
      block.setAttribute('data-theme-colors', name);
      document.head.appendChild(block);
    }
    return block;
  }

  function render() {
    var root = editorRoot();
    if (!root || !editing) return;
    var shipped = shippedColors(style, mode);
    var changes = editing[mode];
    root.querySelectorAll('[data-theme-colors-row]').forEach(function(row) {
      var name = row.getAttribute('data-theme-colors-row');
      var value = changes[name] || shipped[name] || '#000000';
      var input = row.querySelector('[data-theme-colors-input]');
      if (input) input.value = value;
      var label = row.querySelector('[data-theme-colors-value]');
      if (label) label.textContent = value;
      var changed = !!changes[name];
      row.setAttribute('data-changed', changed ? 'true' : 'false');
      var reset = row.querySelector('[data-theme-colors-reset]');
      if (reset) reset.classList.toggle('invisible', !changed);
    });
    root.querySelectorAll('[data-theme-colors-mode]').forEach(function(button) {
      button.setAttribute('data-active', button.getAttribute('data-theme-colors-mode') === mode ? 'true' : 'false');
    });
    var count = root.querySelector('[data-theme-colors-count]');
    if (count) {
      var n = changedCount(editing);
      count.textContent = n === 0 ? 'No colors changed' : n === 1 ? '1 color changed' : n + ' colors changed';
    }
  }

  function preview() {
    if (previewTimer) clearTimeout(previewTimer);
    previewTimer = setTimeout(function() {
      previewTimer = null;
      var seq = ++previewSeq;
      var name = style;
      fetch('/api/settings/theme-colors.css', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ style: name, overrides: JSON.parse(encode(editing) || '{}') }),
      })
        .then(function(res) { return res.ok ? res.text() : Promise.reject(res.status); })
        .then(function(css) {
          if (seq === previewSeq) styleBlock(name).textContent = css;
        })
        .catch(function() {});
    }, 60);
  }

  // The page shows the mode being edited, and goes back to the saved mode
  // when the editor closes.
  function showMode(colorMode) {
    mode = colorMode;
    document.documentElement.classList.toggle('dark', mode === 'dark');
    render();
  }

  function finish() {
    if (previewTimer) clearTimeout(previewTimer);
    previewTimer = null;
    previewSeq++;
    if (before) {
      if (!saved) styleBlock(before.style).textContent = before.css;
      document.documentElement.classList.toggle('dark', before.dark);
    }
    before = null;
    editing = null;
    style = null;
  }

  function watchClose(root) {
    var content = root.querySelector('[data-tui-dialog-content]');
    if (!content || content.dataset.themeColorsWatch) return;
    content.dataset.themeColorsWatch = 'true';
    content.addEventListener('close', finish);
  }

  function open(name) {
    var root = editorRoot();
    if (!root || typeof GoferSettings === 'undefined' || !window.tui || !window.tui.dialog) return;
    var html = document.documentElement;
    style = name;
    editing = savedOverrides(name);
    saved = false;
    before = { style: name, css: styleBlock(name).textContent, dark: html.classList.contains('dark') };
    mode = before.dark ? 'dark' : 'light';
    // The Custom theme has only its own mode, which the page is already in.
    // The hidden attribute, since the switch's inline-flex outranks the class.
    root.querySelectorAll('[data-theme-colors-modes], [data-theme-colors-modes-note]').forEach(function(el) {
      el.hidden = name === 'custom';
    });
    var title = root.querySelector('[data-theme-colors-title]');
    if (title) title.textContent = 'Edit ' + styleLabel(name) + ' colors';
    watchClose(root);
    render();
    window.tui.dialog.open(EDITOR_ID);
  }

  function showDiscard(name, changed) {
    var wrap = document.querySelector('[data-theme-colors-discard-wrap="' + name + '"]');
    if (wrap) wrap.classList.toggle('hidden', !changed);
  }

  function save() {
    if (!editing) return;
    var name = style;
    var value = encode(editing);
    saved = true;
    if (name === 'custom') saveCustomColors(value);
    else GoferSettings.set(settingKey(name), value);
    showDiscard(name, !!value);
    window.tui.dialog.close(EDITOR_ID);
  }

  function setColor(name, value) {
    if (!editing) return;
    if (value && value !== shippedColors(style, mode)[name]) {
      editing[mode][name] = value;
    } else {
      delete editing[mode][name];
    }
    render();
    preview();
  }

  document.addEventListener('click', function(e) {
    var edit = e.target.closest('[data-theme-colors-edit]');
    if (edit) {
      open(edit.getAttribute('data-theme-colors-edit'));
      return;
    }
    var modeButton = e.target.closest('[data-theme-colors-mode]');
    if (modeButton && editing) {
      showMode(modeButton.getAttribute('data-theme-colors-mode'));
      return;
    }
    var reset = e.target.closest('[data-theme-colors-reset]');
    if (reset) {
      setColor(reset.getAttribute('data-theme-colors-reset'), '');
      return;
    }
    if (e.target.closest('[data-theme-colors-reset-all]') && editing) {
      editing = { light: {}, dark: {} };
      render();
      preview();
      return;
    }
    if (e.target.closest('[data-theme-colors-save]')) {
      save();
      return;
    }
    var discard = e.target.closest('[data-theme-colors-discard]');
    if (discard && window.tui && window.tui.dialog) {
      discardStyle = discard.getAttribute('data-theme-colors-discard');
      var text = document.querySelector('[data-theme-colors-discard-text]');
      if (text) text.textContent = styleLabel(discardStyle) + ' goes back to the colors it ships with, in light and dark mode.';
      window.tui.dialog.open(DISCARD_ID);
      return;
    }
    if (e.target.closest('[data-theme-colors-discard-confirm]') && discardStyle) {
      GoferSettings.set(settingKey(discardStyle), '');
      styleBlock(discardStyle).textContent = '';
      showDiscard(discardStyle, false);
      discardStyle = null;
      window.tui.dialog.close(DISCARD_ID);
    }
  });

  document.addEventListener('input', function(e) {
    if (!e.target.matches || !e.target.matches('[data-theme-colors-input]')) return;
    var value = hexColor(e.target.value);
    if (value) setColor(e.target.getAttribute('data-theme-colors-input'), value);
  });
})();

// Theme builder. Custom themes are saved as a list in custom_themes, each with
// an ID, a name, its builder choices and its hand-edited colors, and the
// server makes each palette from the choices. Changes show only in the
// dialog's preview frame; the page takes the theme when it is saved.
(function() {
  var BUILDER_ID = 'custom-theme-dialog';
  var DELETE_ID = 'custom-theme-delete-dialog';
  var FIELDS = ['mode', 'tint', 'strength', 'contrast', 'sidebar', 'depth', 'corners'];
  var MAX_THEMES = 12;
  var editing = null;
  // The ID of the theme being edited, or null for a new one.
  var editingId = null;
  var deletingId = null;
  // The hand edits of the saved theme, shown in the preview until the palette
  // changes.
  var handEdits = '';
  var palette = { key: '', css: '' };
  var paletteRequest = null;
  var previewTimer = null;
  var frameReady = false;
  var frameSeq = 0;

  function hexColor(value) {
    value = (value || '').trim().toLowerCase();
    return /^#[0-9a-f]{6}$/.test(value) ? value : '';
  }

  function findTheme(id) {
    var themes = GoferSettings.customThemes();
    for (var i = 0; i < themes.length; i++) {
      if (themes[i].id === id) return themes[i];
    }
    return null;
  }

  // The theme being edited as it was saved, or null for a new one.
  function savedTheme() {
    return editingId ? findTheme(editingId) : null;
  }

  function handEditCount() {
    var edits = (savedTheme() || {}).colors || {};
    return Object.keys(edits.light || {}).length + Object.keys(edits.dark || {}).length;
  }

  // "My theme", or "My theme 2" and on when that name is taken.
  function defaultName() {
    var names = GoferSettings.customThemes().map(function(theme) { return theme.name; });
    var name = 'My theme';
    for (var n = 2; names.indexOf(name) >= 0; n++) name = 'My theme ' + n;
    return name;
  }

  function newID() {
    var id = '';
    while (!id || findTheme(id)) id = 't' + Math.random().toString(36).slice(2, 10);
    return id;
  }

  // The name typed in, trimmed to what the server keeps, or the default.
  function typedName() {
    var root = builder();
    var input = root && root.querySelector('[data-custom-theme-name]');
    var name = (input ? input.value : '').replace(/[\u0000-\u001f\u007f]/g, '').trim();
    return Array.from(name).slice(0, 40).join('') || defaultName();
  }

  // The saved theme, or a new one that starts with the selected style's look.
  function startingTheme() {
    var theme = savedTheme();
    if (theme) {
      return {
        mode: theme.mode === 'dark' ? 'dark' : 'light',
        seed: theme.seed,
        secondary: theme.secondary || '',
        tint: theme.tint || 'primary',
        strength: theme.strength || 'subtle',
        contrast: theme.contrast || 'normal',
        // Sidebars saved before they were relative to the panes.
        sidebar: theme.sidebar === 'light' || theme.sidebar === 'lighter' ? 'lighter' : 'darker',
        depth: theme.depth,
        corners: theme.corners,
      };
    }
    var minimal = document.documentElement.getAttribute('data-theme') === 'minimal';
    return {
      mode: document.documentElement.classList.contains('dark') ? 'dark' : 'light',
      seed: '#2563eb',
      secondary: '',
      tint: 'primary',
      strength: 'subtle',
      contrast: 'normal',
      sidebar: minimal ? 'lighter' : 'darker',
      depth: minimal ? 'flat' : 'raised',
      corners: minimal ? 'square' : 'medium',
    };
  }

  // The base, colors, tint, contrast and sidebar decide the palette, so changing any of
  // them replaces it and clears hand-edited colors.
  function colorsChanged() {
    var theme = savedTheme();
    if (!theme || !editing) return false;
    var mode = theme.mode === 'dark' ? 'dark' : 'light';
    return mode !== editing.mode ||
      theme.seed !== editing.seed ||
      (theme.secondary || '') !== editing.secondary ||
      (theme.tint || 'primary') !== editing.tint ||
      (theme.strength || 'subtle') !== editing.strength ||
      (theme.contrast || 'normal') !== editing.contrast ||
      startingTheme().sidebar !== editing.sidebar;
  }

  function block(selector, attribute) {
    var el = document.querySelector(selector);
    if (!el) {
      el = document.createElement('style');
      el.setAttribute(attribute.name, attribute.value);
      document.head.appendChild(el);
    }
    return el;
  }

  function paletteBlock() {
    return block('style[data-custom-theme]', { name: 'data-custom-theme', value: '' });
  }

  function handEditsBlock() {
    return block('style[data-theme-colors="custom"]', { name: 'data-theme-colors', value: 'custom' });
  }

  function builder() {
    return document.getElementById(BUILDER_ID);
  }

  function frame() {
    var root = builder();
    return root && root.querySelector('[data-custom-theme-preview-frame]');
  }

  // OKLCH, as themebuilder works in: lightness 0-1, chroma, hue in radians.
  function toOklch(hex) {
    var lin = [1, 3, 5].map(function(i) {
      var v = parseInt(hex.substr(i, 2), 16) / 255;
      return v <= 0.04045 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
    });
    var l = Math.cbrt(0.4122214708 * lin[0] + 0.5363325363 * lin[1] + 0.0514459929 * lin[2]);
    var m = Math.cbrt(0.2119034982 * lin[0] + 0.6806995451 * lin[1] + 0.1073969566 * lin[2]);
    var s = Math.cbrt(0.0883024619 * lin[0] + 0.2817188376 * lin[1] + 0.6299787005 * lin[2]);
    var a = 1.9779984951 * l - 2.4285922050 * m + 0.4505937099 * s;
    var b = 0.0259040371 * l + 0.7827717662 * m - 0.8086757660 * s;
    return { l: 0.2104542553 * l + 0.7936177850 * m - 0.0040720468 * s, c: Math.hypot(a, b), h: Math.atan2(b, a) };
  }

  function oklabToRGB(L, a, b) {
    var l = Math.pow(L + 0.3963377774 * a + 0.2158037573 * b, 3);
    var m = Math.pow(L - 0.1055613458 * a - 0.0638541728 * b, 3);
    var s = Math.pow(L - 0.0894841775 * a - 1.2914855480 * b, 3);
    return [
      4.0767416621 * l - 3.3077115913 * m + 0.2309699292 * s,
      -1.2684380046 * l + 2.6097574011 * m - 0.3413193965 * s,
      -0.0041960863 * l - 0.7034186168 * m + 1.7076147010 * s,
    ].map(function(v) {
      return v <= 0.0031308 ? v * 12.92 : 1.055 * Math.pow(v, 1 / 2.4) - 0.055;
    });
  }

  // The OKLCH color as #rrggbb, with chroma lowered until it fits in sRGB.
  function fromOklch(l, c, h) {
    var fits = function(rgb) { return rgb.every(function(v) { return v >= -1e-4 && v <= 1 + 1e-4; }); };
    var rgb = oklabToRGB(l, c * Math.cos(h), c * Math.sin(h));
    if (!fits(rgb)) {
      var lo = 0, hi = 1;
      rgb = oklabToRGB(l, 0, 0);
      for (var i = 0; i < 24; i++) {
        var mid = (lo + hi) / 2;
        var cand = oklabToRGB(l, c * mid * Math.cos(h), c * mid * Math.sin(h));
        if (fits(cand)) { rgb = cand; lo = mid; } else { hi = mid; }
      }
    }
    return '#' + rgb.map(function(v) {
      return ('0' + Math.round(Math.max(0, Math.min(1, v)) * 255).toString(16)).slice(-2);
    }).join('');
  }

  // The secondary suggestions are the primary's hue turned, at its lightness
  // and chroma; a pale or grey primary still gets colors with some body.
  function suggestSecondaries(root, seed) {
    var primary = toOklch(seed);
    var l = Math.max(0.5, Math.min(0.75, primary.l));
    var chroma = {
      match: Math.max(primary.c, 0.1),
      muted: Math.max(primary.c * 0.35, 0.03),
      neutral: 0.015,
    };
    root.querySelectorAll('[data-custom-theme-suggestion-chroma]').forEach(function(swatch) {
      var kind = swatch.getAttribute('data-custom-theme-suggestion-chroma');
      var turn = parseFloat(swatch.getAttribute('data-custom-theme-suggestion-turn')) * Math.PI / 180;
      var color = kind === 'same' ? seed : fromOklch(l, chroma[kind], primary.h + turn);
      // The first swatch follows the primary, so its value stays empty.
      if (kind !== 'same') swatch.setAttribute('data-custom-theme-secondary-option', color);
      swatch.style.backgroundColor = color;
    });
  }

  function renderSwatches(root, optionAttribute, pickerAttribute, color) {
    var offered = false;
    root.querySelectorAll('[' + optionAttribute + ']').forEach(function(swatch) {
      var active = swatch.getAttribute(optionAttribute) === color;
      if (active) offered = true;
      swatch.setAttribute('data-account-color-active', active ? 'true' : 'false');
      swatch.setAttribute('aria-pressed', active ? 'true' : 'false');
    });
    var picker = root.querySelector('[' + pickerAttribute + ']');
    if (!picker) return;
    var tile = picker.closest('[data-custom-theme-picker-tile]');
    var picked = !!color && !offered;
    if (color) picker.value = color;
    if (!tile) return;
    tile.toggleAttribute('data-custom-theme-picked', picked);
    tile.setAttribute('data-account-color-active', picked ? 'true' : 'false');
    tile.style.backgroundColor = picked ? color : '';
  }

  function render() {
    var root = builder();
    if (!root || !editing) return;
    renderSwatches(root, 'data-custom-theme-seed-option', 'data-custom-theme-seed', editing.seed);
    var value = root.querySelector('[data-custom-theme-seed-value]');
    if (value) value.textContent = editing.seed;
    // An empty secondary follows the primary color.
    suggestSecondaries(root, editing.seed);
    renderSwatches(root, 'data-custom-theme-secondary-option', 'data-custom-theme-secondary', editing.secondary);
    if (!editing.secondary) {
      var secondaryPicker = root.querySelector('[data-custom-theme-secondary]');
      if (secondaryPicker) secondaryPicker.value = editing.seed;
    }
    var secondaryValue = root.querySelector('[data-custom-theme-secondary-value]');
    if (secondaryValue) secondaryValue.textContent = editing.secondary || 'Same as primary';
    root.querySelectorAll('[data-custom-theme-field]').forEach(function(input) {
      input.checked = editing[input.getAttribute('data-custom-theme-field')] === input.value;
    });
    // No tint has no strength to choose, so the choice is greyed out.
    var strength = root.querySelector('[data-custom-theme-choice="strength"]');
    if (strength) {
      strength.disabled = editing.tint === 'none';
      strength.classList.toggle('opacity-50', strength.disabled);
    }
    var warning = root.querySelector('[data-custom-theme-reset-warning]');
    if (warning) {
      var count = colorsChanged() ? handEditCount() : 0;
      warning.classList.toggle('hidden', count === 0);
      warning.textContent = count === 1
        ? 'A new base, colors, tint, contrast or sidebar make a new palette, so saving clears the 1 color you edited by hand.'
        : 'A new base, colors, tint, contrast or sidebar make a new palette, so saving clears the ' + count + ' colors you edited by hand.';
    }
  }

  // The palette CSS for a theme, from the server; the last one is kept.
  function paletteFor(theme) {
    var key = JSON.stringify(theme);
    if (palette.key === key) return Promise.resolve(palette.css);
    if (paletteRequest && paletteRequest.key === key) return paletteRequest.promise;
    var promise = fetch('/api/settings/custom-theme.css', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: key,
    })
      .then(function(res) { return res.ok ? res.text() : Promise.reject(res.status); })
      .then(function(css) {
        palette = { key: key, css: css };
        return css;
      });
    paletteRequest = { key: key, promise: promise };
    return promise;
  }

  function attribute(value) {
    return String(value).replace(/&/g, '&amp;').replace(/"/g, '&quot;');
  }

  // The frame gets the page's stylesheets, fonts and styles, but not its
  // theme's colors, so the preview is drawn with the app's own CSS. It is
  // built once the palette is known, with the palette in it, and stays hidden
  // until its fonts are in, so it never shows other colors or reflowing text.
  function loadFrame() {
    var el = frame();
    var template = builder().querySelector('[data-custom-theme-preview-template]');
    if (!el || !template || !editing) return;
    var seq = ++frameSeq;
    frameReady = false;
    el.style.visibility = 'hidden';
    paletteFor(Object.assign({}, editing)).then(function() {
      if (seq !== frameSeq || !editing) return;
      var head = ['<meta charset="utf-8">'];
      document.querySelectorAll('head link[rel="stylesheet"], head style').forEach(function(node) {
        if (node.matches('style[data-custom-theme], style[data-theme-colors]')) return;
        if (node.tagName === 'LINK') head.push('<link rel="stylesheet" href="' + attribute(node.href) + '">');
        else head.push('<style>' + node.textContent + '</style>');
      });
      // The first drawing shows at once; once the frame is visible, option
      // changes fade into each other.
      head.push('<style data-custom-theme-motion>*,*::before,*::after{transition:none!important}</style>');
      // Raised surfaces are gradients with no color under them, so a fade to
      // flat passes through see-through; the theme background, not the
      // frame's white, shows through.
      head.push('<style>html{background-color:var(--background)}</style>');
      head.push('<style data-custom-theme></style><style data-theme-colors="custom"></style>');
      el.onload = function() {
        if (seq !== frameSeq) return;
        frameReady = true;
        drawFrame();
        var doc = el.contentDocument;
        var fonts = doc && doc.fonts ? doc.fonts.ready : Promise.resolve();
        fonts.then(function() {
          if (seq !== frameSeq) return;
          el.style.visibility = '';
          requestAnimationFrame(function() {
            var motion = seq === frameSeq && doc.querySelector('style[data-custom-theme-motion]');
            if (motion) motion.textContent = FRAME_MOTION;
          });
        });
      };
      el.srcdoc = '<!doctype html><html data-theme="custom"><head>' + head.join('') +
        '</head><body class="bg-background text-foreground antialiased surface-desk">' + template.innerHTML + '</body></html>';
    }).catch(function() {});
    fitFrame();
  }

  var FRAME_MOTION = '*,*::before,*::after{transition:color .3s ease,background-color .3s ease,' +
    'border-color .3s ease,outline-color .3s ease,box-shadow .3s ease,border-radius .3s ease,' +
    'fill .3s ease,stroke .3s ease,text-decoration-color .3s ease!important}';

  // The frame is laid out at a desktop size and scaled down to the stage,
  // which is no wider than the frame, so it never grows past the real size.
  function fitFrame() {
    var el = frame();
    var stage = el && el.parentElement;
    if (!stage || !stage.clientWidth) return;
    var scale = stage.clientWidth / el.offsetWidth;
    el.style.transform = 'scale(' + scale + ')';
    stage.style.height = Math.round(el.offsetHeight * scale) + 'px';
  }

  function drawFrame() {
    var el = frame();
    if (!frameReady || !el || !el.contentDocument || !editing) return;
    var doc = el.contentDocument;
    var html = doc.documentElement;
    html.classList.toggle('dark', editing.mode === 'dark');
    if (editing.depth === 'flat') html.setAttribute('data-depth', 'flat');
    else html.removeAttribute('data-depth');
    if (editing.corners === 'square') html.setAttribute('data-corners', 'square');
    else html.removeAttribute('data-corners');
    var paletteStyle = doc.querySelector('style[data-custom-theme]');
    if (paletteStyle) paletteStyle.textContent = palette.css;
    // Hand edits were made for the saved palette; a new one shows without them.
    var editsStyle = doc.querySelector('style[data-theme-colors="custom"]');
    if (editsStyle) editsStyle.textContent = colorsChanged() ? '' : handEdits;
  }

  function preview() {
    if (previewTimer) clearTimeout(previewTimer);
    previewTimer = setTimeout(function() {
      previewTimer = null;
      if (!editing) return;
      var key = JSON.stringify(editing);
      paletteFor(Object.assign({}, editing))
        .then(function() {
          if (editing && JSON.stringify(editing) === key) drawFrame();
        })
        .catch(function() {});
    }, 60);
  }

  function watchClose(root) {
    var content = root.querySelector('[data-tui-dialog-content]');
    if (!content || content.dataset.customThemeWatch) return;
    content.dataset.customThemeWatch = 'true';
    content.addEventListener('close', function() {
      editing = null;
    });
  }

  function open(id) {
    var root = builder();
    if (!root || typeof GoferSettings === 'undefined' || !window.tui || !window.tui.dialog) return;
    editingId = id && findTheme(id) ? id : null;
    if (!editingId && GoferSettings.customThemes().length >= MAX_THEMES) return;
    editing = startingTheme();
    // The page shows the active theme's hand edits, and only it can be edited.
    var active = GoferSettings.activeCustomTheme();
    handEdits = editingId && active && active.id === editingId ? handEditsBlock().textContent : '';
    var name = root.querySelector('[data-custom-theme-name]');
    if (name) name.value = editingId ? savedTheme().name : defaultName();
    watchClose(root);
    render();
    loadFrame();
    window.tui.dialog.open(BUILDER_ID);
    // Opening focuses the name field, which scrolls the choices past their
    // first label; they start at the top.
    var controls = root.querySelector('[data-custom-theme-controls]');
    if (controls) {
      controls.scrollTop = 0;
      requestAnimationFrame(function() { controls.scrollTop = 0; });
    }
    watchStage();
  }

  function selectStyle(style) {
    document.querySelectorAll('input[name="theme_style"]').forEach(function(radio) {
      radio.checked = radio.value === style;
    });
  }

  // Settings lists the themes as server-drawn cards, each in its own colors,
  // so the Appearance tab is drawn again once the list is saved.
  function redrawAppearance() {
    GoferSettings.saveNow().then(function() {
      if (!window.htmx || !document.getElementById('settings-content')) return;
      window.htmx.ajax('GET', '/settings/appearance', {
        target: '#settings-content',
        select: '#settings-content',
        swap: 'outerHTML',
      });
    });
  }

  function save() {
    if (!editing) return;
    var choices = Object.assign({}, editing);
    var id = editingId || newID();
    var before = savedTheme();
    var clearHandEdits = colorsChanged() && handEditCount() > 0;
    var themes = GoferSettings.customThemes();
    if (!before && themes.length >= MAX_THEMES) return;
    var theme = Object.assign({ id: id, name: typedName() }, choices);
    if (before && before.colors && !clearHandEdits) theme.colors = before.colors;
    paletteFor(choices).then(function(css) {
      paletteBlock().textContent = css;
      // Only the active theme's hand edits are on the page, and a theme saved
      // here becomes the active one: keep them only for that theme, unchanged.
      var active = GoferSettings.activeCustomTheme();
      if (!theme.colors || !active || active.id !== id) handEditsBlock().textContent = '';
      var saved = before
        ? themes.map(function(t) { return t.id === id ? theme : t; })
        : themes.concat([theme]);
      GoferSettings.setCustomThemes(saved);
      GoferSettings.set('custom_theme_active', id);
      GoferSettings.set('theme_style', 'custom');
      window.tui.dialog.close(BUILDER_ID);
      redrawAppearance();
    }).catch(function() {});
  }

  function confirmDelete(button) {
    deletingId = button.getAttribute('data-custom-theme-delete');
    var title = document.querySelector('[data-custom-theme-delete-title]');
    if (title) title.textContent = 'Delete \u201c' + (button.getAttribute('data-custom-theme-delete-name') || 'this theme') + '\u201d?';
    window.tui.dialog.open(DELETE_ID);
  }

  function deleteTheme() {
    var id = deletingId;
    deletingId = null;
    if (!id) return;
    var active = GoferSettings.activeCustomTheme();
    var wasActive = !!active && active.id === id;
    GoferSettings.setCustomThemes(GoferSettings.customThemes().filter(function(theme) { return theme.id !== id; }));
    if (wasActive) {
      GoferSettings.set('custom_theme_active', '');
      paletteBlock().textContent = '';
      handEditsBlock().textContent = '';
      if (GoferSettings.get('theme_style') === 'custom') {
        GoferSettings.set('theme_style', 'classic');
        selectStyle('classic');
      }
    }
    window.tui.dialog.close(DELETE_ID);
    redrawAppearance();
  }

  // "Night copy", or "Night copy 2" and on when that is taken, cut to the
  // longest name the server keeps.
  function copyName(name) {
    var names = GoferSettings.customThemes().map(function(theme) { return theme.name; });
    var fit = function(suffix) { return Array.from(name).slice(0, 40 - suffix.length).join('').trim() + suffix; };
    var copy = fit(' copy');
    for (var n = 2; names.indexOf(copy) >= 0; n++) copy = fit(' copy ' + n);
    return copy;
  }

  // A copy of a theme, hand-edited colors and all, goes right after it and
  // is chosen. It looks the same as the theme it copies, which is the one the
  // page is drawn with, so the page stays as it is.
  function duplicate(id) {
    var themes = GoferSettings.customThemes();
    var theme = findTheme(id);
    if (!theme || themes.length >= MAX_THEMES) return;
    var copy = Object.assign({}, theme, { id: newID(), name: copyName(theme.name) });
    themes.splice(themes.findIndex(function(t) { return t.id === id; }) + 1, 0, copy);
    GoferSettings.setCustomThemes(themes);
    GoferSettings.set('custom_theme_active', copy.id);
    redrawAppearance();
  }

  // Choosing a theme's card draws the page with it: its palette and hand
  // edits come from the server first, so the page never shows the wrong ones.
  function choose(id) {
    var theme = findTheme(id);
    if (!theme) return;
    var choices = {};
    FIELDS.concat(['seed', 'secondary']).forEach(function(key) { choices[key] = theme[key]; });
    var edits = fetch('/api/settings/theme-colors.css', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ style: 'custom', overrides: theme.colors || {} }),
    }).then(function(res) { return res.ok ? res.text() : Promise.reject(res.status); });
    Promise.all([paletteFor(choices), edits]).then(function(css) {
      paletteBlock().textContent = css[0];
      handEditsBlock().textContent = css[1];
      GoferSettings.set('custom_theme_active', id);
      GoferSettings.set('theme_style', 'custom');
    }).catch(function() {});
  }

  function update(change) {
    if (!editing) return;
    Object.keys(change).forEach(function(key) { editing[key] = change[key]; });
    render();
    preview();
  }

  // The stage has no width until the dialog shows, and changes with the window.
  function watchStage() {
    var el = frame();
    var stage = el && el.parentElement;
    if (!stage || stage.dataset.customThemeObserved || typeof ResizeObserver === 'undefined') {
      requestAnimationFrame(fitFrame);
      return;
    }
    stage.dataset.customThemeObserved = 'true';
    new ResizeObserver(fitFrame).observe(stage);
  }

  document.addEventListener('click', function(e) {
    if (e.target.closest('[data-custom-theme-build]')) {
      open(null);
      return;
    }
    var edit = e.target.closest('[data-custom-theme-edit]');
    if (edit) {
      open(edit.getAttribute('data-custom-theme-edit'));
      return;
    }
    var copy = e.target.closest('[data-custom-theme-duplicate]');
    if (copy) {
      duplicate(copy.getAttribute('data-custom-theme-duplicate'));
      return;
    }
    if (e.target.closest('[data-custom-theme-save]')) {
      save();
      return;
    }
    var trash = e.target.closest('[data-custom-theme-delete]');
    if (trash && window.tui && window.tui.dialog) {
      confirmDelete(trash);
      return;
    }
    if (e.target.closest('[data-custom-theme-delete-confirm]')) {
      deleteTheme();
      return;
    }
    var swatch = e.target.closest('[data-custom-theme-seed-option]');
    if (swatch) {
      update({ seed: swatch.getAttribute('data-custom-theme-seed-option') });
      return;
    }
    var secondarySwatch = e.target.closest('[data-custom-theme-secondary-option]');
    if (secondarySwatch) update({ secondary: secondarySwatch.getAttribute('data-custom-theme-secondary-option') });
  });

  document.addEventListener('input', function(e) {
    if (!e.target.matches) return;
    var color = hexColor(e.target.value);
    if (!color) return;
    if (e.target.matches('[data-custom-theme-seed]')) update({ seed: color });
    else if (e.target.matches('[data-custom-theme-secondary]')) update({ secondary: color });
  });

  document.addEventListener('gofer:custom-themes-changed', redrawAppearance);

  document.addEventListener('change', function(e) {
    if (!e.target.matches) return;
    if (e.target.matches('input[name="theme_style"][data-custom-theme-id]')) {
      choose(e.target.getAttribute('data-custom-theme-id'));
      return;
    }
    if (!e.target.matches('[data-custom-theme-field]')) return;
    var field = e.target.getAttribute('data-custom-theme-field');
    if (FIELDS.indexOf(field) < 0) return;
    var change = {};
    change[field] = e.target.value;
    update(change);
  });
})();

// Regional tab
(function() {
  function timezoneLabel(timezone) {
    if (!timezone || timezone === 'local') return 'Browser timezone';
    try {
      var now = new Date();
      var parts = new Intl.DateTimeFormat('en-US', { timeZone: timezone, timeZoneName: 'shortOffset' }).formatToParts(now);
      var offset = '';
      for (var i = 0; i < parts.length; i++) {
        if (parts[i].type === 'timeZoneName') offset = parts[i].value.replace('GMT', 'UTC');
      }
      return timezone.replace(/_/g, ' ') + (offset ? ' (' + offset + ')' : '');
    } catch (_) {
      return timezone;
    }
  }
  function browserTimezone() {
    try { return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'; } catch (_) { return 'UTC'; }
  }
  function timezoneOptions() {
    if (Intl.supportedValuesOf) {
      try { return Intl.supportedValuesOf('timeZone'); } catch (_) {}
    }
    return ['UTC', 'America/New_York', 'America/Chicago', 'America/Denver', 'America/Los_Angeles', 'Europe/London', 'Europe/Paris', 'Europe/Berlin', 'Europe/Bucharest', 'Asia/Dubai', 'Asia/Kolkata', 'Asia/Tokyo', 'Australia/Sydney'];
  }
  function populateTimezonePicker() {
    var root = document.querySelector('[data-timezone-picker]');
    if (!root) return;
    var content = root.querySelector('[data-timezone-options]');
    var group = content && content.querySelector('[role="group"]');
    var input = root.querySelector('[data-timezone-input]');
    if (!content || !group || !input) return;
    var current = root.getAttribute('data-current-timezone') || '';
    if (typeof GoferSettings !== 'undefined') current = GoferSettings.get('timezone') || current;
    if (!current || current === 'local') current = browserTimezone();
    var options = timezoneOptions();
    group.innerHTML = '';
    if (options.indexOf(current) === -1) options = [current].concat(options);
    for (var i = 0; i < options.length; i++) {
      var item = document.createElement('div');
      item.setAttribute('role', 'option');
      item.setAttribute('tabindex', '0');
      item.setAttribute('data-tui-selectbox-value', options[i]);
      item.setAttribute('data-tui-selectbox-selected', options[i] === current ? 'true' : 'false');
      item.setAttribute('data-tui-selectbox-disabled', 'false');
      item.className = 'select-item group relative flex w-full cursor-default select-none items-center rounded-sm py-1.5 px-2 text-sm font-light outline-none hover:bg-accent hover:text-accent-foreground focus-visible:bg-accent focus-visible:text-accent-foreground data-[tui-selectbox-selected=true]:bg-accent data-[tui-selectbox-selected=true]:text-accent-foreground';
      var text = document.createElement('span');
      text.className = 'truncate select-item-text';
      text.textContent = timezoneLabel(options[i]);
      item.appendChild(text);
      group.appendChild(item);
    }
    input.value = current;
    input.dispatchEvent(new Event('input', { bubbles: true }));
  }
  goferOnContent('[data-timezone-picker]', function() { populateTimezonePicker(); });
  document.addEventListener('change', function(e) {
    var timezone = e.target.closest('[data-timezone-input]');
    if (timezone && typeof GoferSettings !== 'undefined') {
      var valueEl = timezone.closest('.select-trigger') && timezone.closest('.select-trigger').querySelector('.select-value');
      if (valueEl) valueEl.textContent = timezoneLabel(timezone.value);
      GoferSettings.set('timezone', timezone.value);
    }
  });
})();

// Advanced tab: switches saved as they change
(function() {
  document.addEventListener('change', function(e) {
    var toggle = e.target.closest('[data-ui-setting-switch]');
    if (!toggle || typeof GoferSettings === 'undefined') return;
    GoferSettings.set(toggle.getAttribute('data-ui-setting-switch'), toggle.checked ? 'true' : 'false');
  });
})();

// Contacts tab
(function() {
  function syncContactsAutoCreateDependents() {
    var master = document.querySelector('[data-contact-autocreate-switch]');
    var enabled = !master || master.checked;
    document.querySelectorAll('[data-contact-autocreate-dependent]').forEach(function(section) {
      section.disabled = !enabled;
      section.classList.toggle('opacity-45', !enabled);
      section.classList.toggle('pointer-events-none', !enabled);
      section.classList.toggle('saturate-50', !enabled);
    });
  }
  goferOnContent('[data-contact-autocreate-switch]', function() { syncContactsAutoCreateDependents(); });
  document.addEventListener('change', function(e) {
    // The switch itself is saved by the advanced settings listener.
    var toggle = e.target.closest('[data-contact-autocreate-switch]');
    if (toggle) syncContactsAutoCreateDependents();
    var source = e.target.closest('[data-contact-source-switch]');
    if (!source || typeof GoferSettings === 'undefined') return;
    var selected = [];
    document.querySelectorAll('[data-contact-source-switch]').forEach(function(el) {
      if (el.checked) selected.push(el.getAttribute('data-contact-source-switch'));
    });
    GoferSettings.set('contacts_observed_sources', selected.join(','));
  });
})();

// Sync tab
(function() {
  var syncAutoSaveTimer = null;
  var syncSaveInFlight = false;
  var syncSaveQueued = false;

  function setPillEffectiveMode(pill, isIDLE) {
    var label = pill.querySelector('.sync-label');
    var toggle = pill.querySelector('[data-sync-pill-toggle]');
    pill.dataset.effectiveIdle = isIDLE ? 'true' : 'false';
    if (isIDLE) {
      pill.className = pill.className.replace(/bg-muted\/50 border-border text-muted-foreground/g, 'bg-amber-800/10 border-amber-700/20 text-amber-800 dark:bg-amber-300/10 dark:border-amber-300/20 dark:text-amber-200');
      if (!label && toggle) {
        label = document.createElement('span');
        label.className = 'sync-label text-[10px] opacity-70';
        toggle.appendChild(label);
      }
      if (label) label.textContent = 'real-time';
      return;
    }
    pill.className = pill.className.replace(/bg-amber-800\/10 border-amber-700\/20 text-amber-800 dark:bg-amber-300\/10 dark:border-amber-300\/20 dark:text-amber-200/g, 'bg-muted/50 border-border text-muted-foreground');
    if (label) label.remove();
  }

  function fallbackTooltipText(pill) {
    var reason = (pill.dataset.idleFallbackReason || '').trim().replace(/\.$/, '');
    if (!reason) return '';
    var name = pill.dataset.folderName || 'This folder';
    var message = name + ' was moved to polling because ' + reason + '.';
    var retryAt = Date.parse(pill.dataset.idleRetryAt || '');
    if (!isNaN(retryAt) && retryAt > Date.now()) {
      var remaining = retryAt - Date.now();
      var minutes = Math.max(1, Math.ceil(remaining / 60000));
      return message + ' Gofer will try again in ' + minutes + (minutes === 1 ? ' minute.' : ' minutes.');
    }
    return message;
  }

  function setPillFallback(pill, reason, retryAt) {
    reason = (reason || '').trim();
    pill.dataset.idleFallbackReason = reason;
    pill.dataset.idleRetryAt = retryAt || '';
    var warning = pill.querySelector('[data-idle-fallback-warning]');
    if (warning) {
      warning.classList.toggle('hidden', !reason);
      warning.classList.toggle('inline-flex', !!reason);
    }
    var text = pill.querySelector('[data-idle-fallback-tooltip-text]');
    if (text) text.textContent = fallbackTooltipText(pill);
  }

  function movePill(pill, toIdle, animate, afterMove) {
    var container = pill.closest('[data-sync-account-card]');
    if (!container) return;
    var target = container.querySelector(toIdle ? '[data-idle-zone]' : '[data-poll-zone]');
    if (!target) return;
    var moved = pill.parentElement !== target;
    var complete = function() {
      setPillEffectiveMode(pill, toIdle);
      if (moved) target.appendChild(pill);
      if (animate && moved) {
        pill.classList.add('sync-pill-enter');
        pill.addEventListener('animationend', function onEnter() {
          pill.removeEventListener('animationend', onEnter);
          pill.classList.remove('sync-pill-enter');
        }, { once: true });
      }
      updateEmptyStates(container);
      if (afterMove) afterMove();
    };
    if (!animate || !moved) {
      complete();
      return;
    }
    pill.classList.add('sync-pill-exit');
    pill.addEventListener('animationend', function onExit() {
      pill.removeEventListener('animationend', onExit);
      pill.classList.remove('sync-pill-exit');
      complete();
    }, { once: true });
  }

  function togglePill(pill) {
    if (pill.classList.contains('sync-pill-exit')) return;
    var toIdle = pill.dataset.configuredIdle !== 'true';
    pill.dataset.configuredIdle = toIdle ? 'true' : 'false';
    setPillFallback(pill, '', '');
    movePill(pill, toIdle, true, function() {
      updateHiddenInputs();
      scheduleAutoSave(80);
    });
  }

  function applyIDLEFolderStatus(data) {
    if (!data || !data.account_id || !data.folder_id) return;
    var pill = Array.prototype.find.call(document.querySelectorAll('[data-sync-pill]'), function(candidate) {
      return candidate.dataset.accountId === data.account_id && candidate.dataset.folderId === data.folder_id;
    });
    if (!pill || pill.dataset.configuredIdle !== 'true') return;
    var effectiveIDLE = data.effective_mode === 'idle';
    setPillFallback(pill, effectiveIDLE ? '' : (data.reason || ''), effectiveIDLE ? '' : (data.retry_at || ''));
    movePill(pill, effectiveIDLE, true);
  }

  function updateEmptyStates(container) {
    var zones = container
      ? container.querySelectorAll('[data-idle-zone],[data-poll-zone]')
      : document.querySelectorAll('[data-idle-zone],[data-poll-zone]');
    zones.forEach(updateZoneEmpty);
  }

  function updateZoneEmpty(zone) {
    var pills = zone.querySelectorAll('[data-sync-pill]');
    var existing = zone.querySelector('.sync-empty-state');
    if (pills.length === 0 && !existing) {
      var el = document.createElement('span');
      el.className = 'sync-empty-state inline-flex items-center px-2.5 rounded-md border border-dashed border-border text-[11px] text-muted-foreground';
      el.style.height = '26px';
      el.textContent = 'None selected';
      zone.appendChild(el);
    } else if (pills.length > 0 && existing) {
      existing.remove();
    }
  }

  function updateHiddenInputs() {
    var container = document.getElementById('idle-folders-inputs');
    if (!container) return;
    container.innerHTML = '';
    var seenAccounts = {};
    var seenIdle = {};
    document.querySelectorAll('[data-sync-pill][data-configured-idle="true"]').forEach(function(pill) {
      var accountID = pill.dataset.accountId;
      var folderID = pill.dataset.folderId;
      var key = accountID + ':' + folderID;
      if (seenIdle[key]) return;
      seenIdle[key] = true;
      var input = document.createElement('input');
      input.type = 'hidden';
      input.name = 'idle_folders';
      input.value = accountID + ':' + folderID;
      container.appendChild(input);
      seenAccounts[accountID] = true;
    });
    document.querySelectorAll('[data-poll-zone]').forEach(function(zone) {
      var accountID = zone.getAttribute('data-poll-zone');
      if (!seenAccounts[accountID]) {
        seenAccounts[accountID] = true;
      }
    });
    for (var aid in seenAccounts) {
      var input = document.createElement('input');
      input.type = 'hidden';
      input.name = 'account_ids';
      input.value = aid;
      container.appendChild(input);
    }
  }

  document.addEventListener('click', function(e) {
    var toggle = e.target.closest('[data-sync-pill-toggle]');
    var pill = toggle && toggle.closest('[data-sync-pill]');
    if (pill && toggle) {
      e.preventDefault();
      togglePill(pill);
      return;
    }
  });

  document.addEventListener('gofer:idle-folder-status', function(e) {
    applyIDLEFolderStatus(e.detail || {});
  });
  if (window.goferIDLEFallbackTimer) window.clearInterval(window.goferIDLEFallbackTimer);
  window.goferIDLEFallbackTimer = window.setInterval(function() {
    document.querySelectorAll('[data-sync-pill][data-idle-fallback-reason]:not([data-idle-fallback-reason=""])').forEach(function(pill) {
      var text = pill.querySelector('[data-idle-fallback-tooltip-text]');
      if (text) text.textContent = fallbackTooltipText(pill);
    });
  }, 30000);

  function syncPollingIntervalDisplay() {
    var input = document.querySelector('#sync-settings-form input[name="sync_interval_minutes"]');
    if (!input || !input.value) return;
    var interval = input.value;
    document.querySelectorAll('[data-sync-polling-title]').forEach(function(title) {
      title.textContent = 'Polling (' + interval + ' min)';
    });
    document.querySelectorAll('[data-sync-folders]').forEach(function(container) {
      container.dataset.syncFolders = interval;
    });
    document.querySelectorAll('[data-sync-pill]').forEach(function(pill) {
      pill.dataset.interval = interval;
    });
  }

  function setSaveStatus(message) {
    var status = document.getElementById('sync-save-status');
    if (status) status.textContent = message || '';
  }

  function showSaveError() {
    if (typeof showGoferToast !== 'function') return;
    showGoferToast({
      id: 'sync-settings-toast',
      title: 'Could not save email sync settings',
      description: 'Try again in a moment.',
      variant: 'error',
      icon: 'error',
      position: 'bottom-right',
      duration: 5000,
      dismissible: true,
    });
  }

  function scheduleAutoSave(delay) {
    if (syncAutoSaveTimer) window.clearTimeout(syncAutoSaveTimer);
    syncAutoSaveTimer = window.setTimeout(saveSyncSettings, typeof delay === 'number' ? delay : 350);
  }

  function saveSyncSettings() {
    var form = document.getElementById('sync-settings-form');
    if (!form) return;
    if (syncSaveInFlight) {
      syncSaveQueued = true;
      return;
    }

    syncSaveInFlight = true;
    syncAutoSaveTimer = null;
    setSaveStatus('Saving');

    var formData = new FormData(form);
    var params = new URLSearchParams();
    formData.forEach(function(value, key) {
      params.append(key, value);
    });
    fetch('/api/settings/sync', {
      method: 'POST',
      body: params,
      headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'Accept': 'application/json' }
    }).then(function(res) {
      if (!res.ok) throw new Error(res.statusText);
      return res.json();
    }).then(function() {
      setSaveStatus('Saved');
    }).catch(function() {
      setSaveStatus('Save failed');
      showSaveError();
    }).finally(function() {
      syncSaveInFlight = false;
      if (syncSaveQueued) {
        syncSaveQueued = false;
        scheduleAutoSave(0);
      }
    });
  }

  function syncUnifiedFolderControls() {
    var master = document.querySelector('[data-unified-folders-setting="enabled"]');
    var enabled = !master || master.checked;
    document.querySelectorAll('[data-unified-folder-switch]').forEach(function(input) {
      input.disabled = !enabled;
    });
    document.querySelectorAll('[data-unified-folder-account-switch]').forEach(function(input) {
      input.disabled = !enabled;
    });
    document.querySelectorAll('[data-unified-folder-account-button]').forEach(function(button) {
      button.disabled = !enabled;
    });
    document.querySelectorAll('[data-unified-folder-dependent]').forEach(function(row) {
      row.classList.toggle('opacity-50', !enabled);
    });
    document.querySelectorAll('[data-unified-folder-account-dependent]').forEach(function(row) {
      row.classList.toggle('opacity-50', !enabled);
    });
  }

  document.addEventListener('change', function(e) {
    var unifiedFolders = e.target.closest && e.target.closest('[data-unified-folders-setting="enabled"]');
    if (unifiedFolders && typeof GoferSettings !== 'undefined') {
      GoferSettings.set('unified_folders_enabled', unifiedFolders.checked ? 'true' : 'false');
      syncUnifiedFolderControls();
      return;
    }
    var unifiedFolder = e.target.closest && e.target.closest('[data-unified-folder-switch]');
    if (unifiedFolder && typeof GoferSettings !== 'undefined') {
      GoferSettings.set(unifiedFolder.name, unifiedFolder.checked ? 'true' : 'false');
      syncUnifiedFolderControls();
      return;
    }
    var unifiedFolderAccount = e.target.closest && e.target.closest('[data-unified-folder-account-switch]');
    if (unifiedFolderAccount && typeof GoferSettings !== 'undefined') {
      GoferSettings.set(unifiedFolderAccount.name, unifiedFolderAccount.checked ? 'true' : 'false');
    }
  });

  // The form's own listeners are bound each time the sync tab appears.
  goferOnContent('#sync-settings-form', function(form) {
    form.addEventListener('submit', function(e) {
      e.preventDefault();
      saveSyncSettings();
    });
    form.addEventListener('change', function(e) {
      // Controls that save themselves, such as Outlook change notifications.
      if (e.target && e.target.closest && e.target.closest('[data-sync-autosave-ignore]')) return;
      syncPollingIntervalDisplay();
      scheduleAutoSave();
    });
    form.addEventListener('input', function(e) {
      if (e.target && e.target.matches && e.target.matches('input[name="sync_interval_minutes"]')) {
        syncPollingIntervalDisplay();
        scheduleAutoSave();
      }
    });
    form.querySelector('[data-tui-selectbox-content]')?.addEventListener('click', function() {
      setTimeout(function() {
        syncPollingIntervalDisplay();
        scheduleAutoSave();
      }, 50);
    });

    updateHiddenInputs();
    updateEmptyStates();
    syncUnifiedFolderControls();
  });
})();

// Gmail account dialog
// The Gmail form signs in with the address as its username.
document.addEventListener('submit', function(e) {
  var form = e.target;
  if (!form || form.id !== 'gmail-form') return;
  var email = form.querySelector('input[name="email_address"]');
  var username = form.querySelector('input[name="username"]');
  if (email && username) username.value = email.value;
}, true);

// Add account dialog
var _wizStep = 0;
var _wizardFooterTimers = {};
function setWizardFooter(root, step, immediate) {
  if (!root) return;
  var footer = root.querySelector('.account-wizard-card-footer');
  if (!footer) return;
  root.querySelectorAll('[data-wizard-footer-step]').forEach(function(item) {
    if (item.parentElement !== footer) item.remove();
  });
  var key = root.id || 'wizard-footer';
  if (_wizardFooterTimers[key]) window.clearTimeout(_wizardFooterTimers[key]);
  footer.dataset.switching = immediate ? 'false' : 'true';
  function applyFooterStep() {
    var seen = {};
    Array.prototype.slice.call(footer.children).reverse().forEach(function(item) {
      if (!item || !item.hasAttribute || !item.hasAttribute('data-wizard-footer-step')) return;
      var itemStep = item.dataset.wizardFooterStep;
      if (seen[itemStep]) {
        item.remove();
        return;
      }
      seen[itemStep] = true;
      var emailOff = root.id === 'edit-account-dialog' && step < 4 && !editWizardEmailEnabled(root);
      var active = emailOff ? itemStep === 'email-off' : itemStep === String(step);
      item.dataset.active = String(active);
      item.classList.toggle('hidden', !active);
    });
    footer.dataset.switching = 'false';
  }
  if (immediate) {
    applyFooterStep();
    return;
  }
  _wizardFooterTimers[key] = window.setTimeout(applyFooterStep, 180);
}
// Accounts that send with their mail sync connection (Gmail, Outlook, iCloud
// and JMAP) have no sending step (2): moving onto it goes on past it, in the
// direction the wizard was moving.
function wizardSkipSending(step, from, skip) {
  if (step !== 2 || !skip) return step;
  return from > 2 ? 1 : 3;
}
function wizardGo(step, immediate) {
  var track = document.getElementById('wizard-track');
  var root = document.getElementById('add-account-dialog');
  if (!track || !root) return;
  if (step < 0) step = 0;
  if (step > 5) step = 5;
  step = wizardSkipSending(step, _wizStep, root.dataset.mailService === 'icloud' || root.dataset.mailService === 'jmap');
  syncAddContactStep();
  // The Email steps end by creating the account. Going back to them would
  // create it again, so after that its settings belong to Edit account.
  if (step <= 3 && addAccountCreated()) return;
  if (step === 3) {
    var form = document.getElementById('account-form');
    if (!form || !form.reportValidity()) return;
  }
  if (step >= 4 && !addAccountCreated()) return;
  _wizStep = step;
  track.style.transform = 'translateX(-' + (step * (100 / 6)) + '%)';
  root.querySelectorAll('[data-account-wizard-step]').forEach(function(item) {
    item.dataset.active = String(+item.dataset.accountWizardStep === step);
  });
  root.querySelectorAll('[data-wizard-section-steps]').forEach(function(section) {
    var steps = (section.dataset.wizardSectionSteps || '').split(',');
    section.dataset.active = String(steps.indexOf(String(step)) !== -1);
  });
  setWizardFooter(root, step, !!immediate);
  if (step === 3) addAccountTest.run(false);
}
function addStepContentReady(id) {
  var content = document.getElementById(id);
  return !!(content && content.children.length);
}
function addAccountFormReady() {
  var form = document.getElementById('account-form');
  return !!(form && form.checkValidity());
}
function addAccountCreated() {
  return !!document.querySelector('#add-contacts-content [data-add-account-created]');
}
function syncAddAccountSubmitButtons() {
  var disabled = !addAccountFormReady();
  document.querySelectorAll('#add-account-dialog [data-add-account-submit-button]').forEach(function(button) {
    button.disabled = disabled;
  });
}
// Both account wizards end their mail steps with step 1.d, which tests the
// typed settings before the account is created or saved. Its footer follows
// the test: while it runs, the account can be saved without waiting; once it
// passes, saved; once it fails, the settings can be fixed (Back), tested
// again, or saved anyway. Coming back with the same settings keeps the
// earlier result instead of testing again.
function accountWizardTest(options) {
  var test = { state: 'idle', key: '' };
  function results() { return document.getElementById(options.results); }
  function spinner(message) {
    var row = document.createElement('div');
    row.className = 'flex items-center gap-2 py-1';
    row.innerHTML = '<div class="size-4 rounded-full border-2 border-muted-foreground/30 border-t-muted-foreground animate-spin"></div><span class="text-xs text-muted-foreground"></span>';
    row.querySelector('span').textContent = message;
    return row;
  }
  function setState(state) {
    test.state = state;
    var footer = document.querySelector(options.footer);
    if (!footer) return;
    // The hidden attribute, not a class: these buttons are inline-flex,
    // which would win over a "hidden" utility class.
    // Once created or saved, the person can close the window here or go on
    // to contacts and calendar.
    var done = state === 'done';
    var anyway = footer.querySelector('[data-account-test-anyway]');
    var retest = footer.querySelector('[data-account-test-retest]');
    var save = footer.querySelector('[data-account-test-save]');
    var back = footer.querySelector('[data-account-test-back]');
    if (anyway) {
      var label = state === 'failed' ? options.anywayFailedLabel : options.anywayLabel;
      anyway.textContent = typeof label === 'function' ? label() : label;
      anyway.hidden = state === 'passed' || done;
    }
    if (retest) retest.hidden = state !== 'failed';
    if (save) save.hidden = state !== 'passed';
    if (back && options.lockWhenDone) back.hidden = done;
    footer.querySelectorAll('[data-account-test-close], [data-account-test-continue]').forEach(function(button) {
      button.hidden = !done;
    });
    footer.querySelectorAll('[data-account-test-action]').forEach(function(button) {
      button.disabled = state === 'saving';
    });
    if (options.onState) options.onState(state);
  }
  function showError(message) {
    var box = results();
    if (!box) return;
    var alert = document.createElement('div');
    alert.setAttribute('role', 'alert');
    alert.className = 'rounded-md border border-destructive/20 bg-destructive/10 px-3 py-2 text-xs text-destructive';
    alert.textContent = message;
    box.textContent = '';
    box.appendChild(alert);
  }
  function clearSaving() {
    var box = results();
    var row = box && box.querySelector('[data-account-test-saving]');
    if (row) row.remove();
  }
  return {
    state: function() { return test.state; },
    run: function(force) {
      var form = document.getElementById(options.form);
      if (!form) return;
      var key = new URLSearchParams(new FormData(form)).toString();
      var current = test.state === 'passed' || test.state === 'failed' || test.state === 'testing';
      if (!force && key === test.key && current) return;
      test.key = key;
      setState('testing');
      var box = results();
      if (box) {
        box.textContent = '';
        box.appendChild(spinner('Testing connection...'));
      }
      var request = options.request(key);
      fetch(request.url, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'Accept': 'text/html' },
        body: request.body
      }).then(function(response) {
        return response.text().then(function(body) {
          if (test.key !== key || test.state !== 'testing') return;
          if (!response.ok) throw new Error((body || '').trim() || 'Could not test the connection.');
          var box = results();
          if (box) box.innerHTML = body;
          if (options.onResponse) options.onResponse(response);
          setState(response.headers.get('X-Gofer-Connection-Test') === 'ok' ? 'passed' : 'failed');
        });
      }).catch(function(error) {
        if (test.key !== key || test.state !== 'testing') return;
        showError(error && error.message ? error.message : 'Could not test the connection.');
        setState('failed');
      });
    },
    saving: function(message) {
      setState('saving');
      var box = results();
      if (!box) return;
      clearSaving();
      var row = spinner(message);
      row.setAttribute('data-account-test-saving', '');
      box.appendChild(row);
    },
    saved: function(message) {
      clearSaving();
      var box = results();
      if (box && message) {
        var note = document.createElement('div');
        note.setAttribute('role', 'status');
        note.className = 'rounded-md border border-emerald-500/25 bg-emerald-500/10 px-3 py-2 text-xs leading-relaxed text-emerald-800 dark:text-emerald-200';
        note.textContent = message;
        box.appendChild(note);
      }
      setState('done');
    },
    saveFailed: function(message) {
      clearSaving();
      if (message) showError(message);
      setState('failed');
    },
    // Changing a setting makes an earlier result stale.
    invalidate: function() {
      if (test.state === 'saving' || test.state === 'done') return;
      test.key = '';
      test.state = 'idle';
    }
  };
}
function newAddAccountTest() {
  return accountWizardTest({
    form: 'account-form',
    results: 'add-account-test-results',
    footer: '#add-wizard-footer [data-wizard-footer-step="3"]',
    anywayLabel: function() { return addingMailToAccount() ? 'Skip test and add email' : 'Skip test and create'; },
    anywayFailedLabel: function() { return addingMailToAccount() ? 'Add anyway' : 'Create anyway'; },
    // Going back would create the account a second time.
    lockWhenDone: true,
    request: function(key) { return { url: '/api/accounts/test', body: key }; },
    onResponse: function(response) {
      setAddAccountJMAPSessionURL(response.headers.get('X-Gofer-JMAP-Session-URL') || '');
    }
  });
}
// A JMAP account is created with the session URL its connection test found.
// Any change to the form drops it, so creating without testing again finds
// the server anew instead of using a URL found for other settings.
function setAddAccountJMAPSessionURL(value) {
  var form = document.getElementById('account-form');
  var field = form && form.querySelector('input[name="jmap_session_url"]');
  if (field) field.value = value;
}
var addAccountTest = newAddAccountTest();
// A newly rendered dialog starts at its first step with a new test.
goferOnContent('#add-account-dialog', function() {
  _wizStep = 0;
  _wizardFooterTimers = {};
  addAccountTest = newAddAccountTest();
});
document.body.addEventListener('htmx:beforeRequest', function(event) {
  if (!event.target || event.target.id !== 'account-form') return;
  addAccountTest.saving(addingMailToAccount() ? 'Adding email...' : 'Creating account...');
});
// Creating failed: say why on the test step, where the person is. Managed
// servers answer with an error status; others send an error card in place
// of the next steps.
document.body.addEventListener('htmx:afterRequest', function(event) {
  if (!event.target || event.target.id !== 'account-form') return;
  if (event.detail && event.detail.successful) {
    if (!addAccountCreated() && addAccountTest.state() === 'saving') addAccountTest.saveFailed('');
    return;
  }
  var xhr = event.detail && event.detail.xhr;
  addAccountTest.saveFailed(xhr && xhr.responseText ? xhr.responseText.trim() : 'Could not create the account.');
});
document.body.addEventListener('htmx:afterSwap', function(event) {
  var content = event.target;
  if (!content || content.id !== 'add-contacts-content' || addAccountCreated()) return;
  var errorCard = content.querySelector('[data-account-form-error]');
  if (!errorCard) return;
  var message = errorCard.querySelector('p');
  content.textContent = '';
  addAccountTest.saveFailed(message ? message.textContent : 'Could not create the account.');
});
document.addEventListener('input', function(event) {
  if (!event.target || !event.target.closest || !event.target.closest('#account-form') || addAccountCreated()) return;
  addAccountTest.invalidate();
  setAddAccountJMAPSessionURL('');
});
function setWizardStepDisabled(step, disabled) {
  if (!step) return;
  step.disabled = !!disabled;
  var section = step.closest ? step.closest('.account-wizard-section') : null;
  if (section) section.dataset.disabled = String(!!disabled);
}
function syncAddContactStep() {
  var root = document.getElementById('add-account-dialog');
  if (!root) return;
  var contactStep = root.querySelector('[data-account-wizard-step="4"]');
  var calendarStep = root.querySelector('[data-account-wizard-step="5"]');
  var created = addAccountCreated();
  root.querySelectorAll('[data-wizard-section-steps="0,1,2,3"] [data-account-wizard-step], [data-wizard-section-steps="0,1,2,3"] .account-wizard-section-title').forEach(function(button) {
    button.disabled = created;
  });
  setWizardStepDisabled(contactStep, !created);
  setWizardStepDisabled(calendarStep, !created);
  syncAddAccountSubmitButtons();
}
// Runs after the contacts step of the add dialog has been loaded.
function syncAddContactStepAfterSwap() {
  syncAddContactStep();
  maybeGoAddContacts();
}

function maybeGoAddContacts() {
  if (!addAccountCreated()) return;
  syncAddContactStep();
  addAccountTest.saved(addingMailToAccount()
    ? 'Email added. Mail is syncing now. You can close this window, or continue to this account\'s contacts and calendar.'
    : 'Account created. Mail is syncing now. You can close this window, or continue to set up this account\'s contacts and calendar.');
}
function syncWizardServiceContent(root) {
  root = root || document;
  syncEditWizardEmailSteps(root);
  root.querySelectorAll('[data-wizard-service-content]').forEach(function(content) {
    var service = content.dataset.wizardServiceContent;
    var toggle = root.querySelector('[data-wizard-service-switch="' + service + '"]');
    var alwaysActive = content.dataset.wizardServiceAlwaysActive === 'true';
    var enabled = alwaysActive || !toggle || toggle.checked;
    if (wizardServiceTurnsOnWhenUsed(service)) {
      content.classList.toggle('opacity-50', !enabled);
      content.classList.remove('pointer-events-none', 'select-none');
      content.removeAttribute('aria-disabled');
      return;
    }
    content.classList.toggle('opacity-50', !enabled);
    content.classList.toggle('pointer-events-none', !enabled);
    content.classList.toggle('select-none', !enabled);
    content.setAttribute('aria-disabled', String(!enabled));
    content.querySelectorAll('input, select, textarea, button').forEach(function(el) {
      if (!el.hasAttribute('data-wizard-original-tabindex')) el.setAttribute('data-wizard-original-tabindex', el.getAttribute('tabindex') || '');
      if (enabled) {
        var original = el.getAttribute('data-wizard-original-tabindex');
        if (original === '') el.removeAttribute('tabindex'); else el.setAttribute('tabindex', original);
      } else {
        el.setAttribute('tabindex', '-1');
      }
    });
  });
}
function syncEditWizardEmailSteps(root) {
  if (root.id !== 'edit-account-dialog') return;
  var section = root.querySelector('[data-wizard-section-steps="0,1,2,3"]');
  if (!section) return;
  var enabled = editWizardEmailEnabled(root);
  section.querySelectorAll('[data-account-wizard-step], .account-wizard-section-title').forEach(function(button) {
    button.disabled = !enabled;
    button.classList.toggle('opacity-50', !enabled);
  });
}
// Contacts and calendar stay usable while switched off. Starting to fill one
// in is taken as wanting it, so its switch turns on.
function wizardServiceTurnsOnWhenUsed(service) {
  return service === 'contacts' || service === 'calendar';
}
function wizardServiceTurnOnFromContent(event) {
  var target = event.target;
  if (!target || !target.closest) return;
  if (event.type === 'click' && !target.closest('button:not([disabled]), [role="button"]')) return;
  var content = target.closest('[data-wizard-service-content]');
  if (!content || content.dataset.wizardServiceAlwaysActive === 'true') return;
  var service = content.dataset.wizardServiceContent;
  if (!wizardServiceTurnsOnWhenUsed(service)) return;
  var root = content.closest('#add-account-dialog,#edit-account-dialog');
  var toggle = root && root.querySelector('[data-wizard-service-switch="' + service + '"]');
  if (!toggle || toggle.checked || toggle.disabled) return;
  toggle.checked = true;
  toggle.dispatchEvent(new Event('change', { bubbles: true }));
}
['input', 'change', 'click'].forEach(function(type) {
  document.addEventListener(type, wizardServiceTurnOnFromContent, true);
});
function wizardServiceStatus(root, service, message) {
  var status = root.querySelector('[data-wizard-service-status="' + service + '"]');
  if (!status) return;
  status.textContent = '';
  var messageBox = document.createElement('div');
  messageBox.className = 'rounded-md border border-destructive/20 bg-destructive/10 px-3 py-2 text-xs text-destructive';
  messageBox.textContent = message;
  status.appendChild(messageBox);
}
// Email saves immediately, independently of the mail settings form. Always
// send a boolean: unchecked checkboxes are omitted from normal form posts.
function saveWizardEmailService(toggle) {
  var form = toggle && toggle.closest('[data-wizard-service-form]');
  var root = toggle && toggle.closest('#edit-account-dialog');
  if (!form || !root || toggle.disabled) return;
  var enabled = toggle.checked;
  var initialEnabled = toggle.dataset.wizardServiceInitialEnabled === 'true';
  var footer = root.querySelector('[data-wizard-footer-step="email-off"]');
  toggle.disabled = true;
  if (footer) footer.querySelectorAll('button').forEach(function(button) { button.disabled = true; });
  if (!enabled) {
    editAccountTest.invalidate();
    editWizardGo(0, true);
  }
  var body = new URLSearchParams({ service: 'email', enabled: String(enabled) });
  return fetch('/api/accounts/' + encodeURIComponent(form.dataset.accountId) + '/services', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'Accept': 'text/html' },
    body: body.toString()
  }).then(function(response) {
    if (!response.ok) {
      return response.text().then(function(message) {
        throw new Error((message || '').trim() || 'Could not save email settings.');
      });
    }
    toggle.dataset.wizardServiceInitialEnabled = String(enabled);
  }).catch(function(error) {
    toggle.checked = initialEnabled;
    if (typeof showGoferToast === 'function') showGoferToast({
      title: 'Could not save email settings',
      description: error.message || 'Could not save email settings.',
      variant: 'error'
    });
  }).finally(function() {
    toggle.disabled = false;
    if (footer) footer.querySelectorAll('button').forEach(function(button) { button.disabled = false; });
    syncWizardServiceContent(root);
    setWizardFooter(root, _editWizStep, true);
  });
}
// Calendar is the add wizard's last step: moving on from it finishes.
function wizardServiceAdvance(root, step) {
  if (root.id === 'add-account-dialog') {
    if (step === 'done' || (step >= 5 && _wizStep === 5)) {
      window.location.href = '/settings/accounts?account_added=1';
      return;
    }
    wizardGo(step);
  } else if (typeof editWizardGo === 'function') {
    if (step === 'done' || (step >= 5 && _editWizStep === 5)) {
      window.location.href = '/settings/accounts';
      return;
    }
    editWizardGo(step, true);
  }
}
function wizardServiceNext(button, service, step) {
  var root = button && button.closest ? button.closest('#add-account-dialog,#edit-account-dialog') : null;
  if (!root) return;
  var toggle = root.querySelector('[data-wizard-service-switch="' + service + '"]');
  var enabled = !toggle || toggle.checked;
  var contactForm = service === 'contacts' ? root.querySelector('[data-wizard-contact-form]') : null;
  var calendarForm = service === 'calendar' && enabled ? root.querySelector('[data-calendar-source-form]') : null;
  if (enabled && contactForm) {
    if (contactForm.requestSubmit) contactForm.requestSubmit(); else contactForm.submit();
    return;
  }
  if (enabled && calendarForm) {
    if (calendarForm.requestSubmit) calendarForm.requestSubmit(); else calendarForm.submit();
    return;
  }
  // Calendar can be toggled on before discovery so users can inspect the
  // setup. With no discovered sources there is nothing valid to persist;
  // let the wizard continue and leave that transient toggle unsaved.
  if (service === 'calendar' && !root.querySelector('[data-calendar-source-checkbox]')) {
    wizardServiceAdvance(root, step);
    return;
  }
  var serviceForm = toggle && toggle.closest ? toggle.closest('[data-wizard-service-form]') : null;
  var accountID = serviceForm ? serviceForm.dataset.accountId : '';
  var initialEnabled = toggle ? toggle.dataset.wizardServiceInitialEnabled : '';
  if (!accountID || !toggle || String(enabled) === initialEnabled || toggle.disabled) {
    wizardServiceAdvance(root, step);
    return;
  }
  var wasDisabled = toggle.disabled;
  toggle.disabled = true;
  wizardServiceStatus(root, service, 'Saving ' + service + ' settings...');
  var body = new URLSearchParams();
  body.append('service', service);
  body.append('enabled', enabled ? 'true' : 'false');
  fetch('/api/accounts/' + encodeURIComponent(accountID) + '/services', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'Accept': 'text/html' },
    body: body.toString()
  }).then(function(response) {
  if (!response.ok) {
    return response.text().then(function(message) {
      throw new Error((message || '').trim() || 'Could not save ' + service + ' settings.');
    });
  }
    toggle.dataset.wizardServiceInitialEnabled = String(enabled);
    wizardServiceAdvance(root, step);
  }).catch(function(error) {
    toggle.checked = initialEnabled === 'true';
    wizardServiceStatus(root, service, error.message || 'Could not save ' + service + ' settings.');
  }).finally(function() {
    toggle.disabled = wasDisabled;
  });
}
document.addEventListener('click', function(event) {
  var button = event.target && event.target.closest ? event.target.closest('[data-account-wizard-action]') : null;
  if (!button) return;
  if (button.hasAttribute('onclick')) return;
  var step = Number(button.dataset.accountWizardTargetStep || 0);
  if (button.dataset.accountWizardAction === 'wizardGo') wizardGo(step);
  if (button.dataset.accountWizardAction === 'editWizardGo' && typeof editWizardGo === 'function') editWizardGo(step);
});
document.addEventListener('change', function(event) {
  if (event.target && event.target.matches && event.target.matches('[data-add-contact-sync-switch]')) syncAddContactStep();
  if (event.target && event.target.closest && event.target.closest('#account-form')) syncAddContactStep();
  if (event.target && event.target.matches && event.target.matches('[data-wizard-service-switch]')) {
    var root = event.target.closest('#add-account-dialog,#edit-account-dialog') || document;
    syncWizardServiceContent(root);
    if (root.id === 'edit-account-dialog') setWizardFooter(root, _editWizStep, true);
  }
});
function toggleSmtpAuth(checked) {
  var fields = document.getElementById('smtp-auth-fields');
  var inputs = fields.querySelectorAll('input');
  fields.classList.toggle('opacity-50', checked);
  inputs.forEach(function(inp) { inp.disabled = checked; });
  if (checked) {
    var u = document.querySelector('input[name="username"]');
    var p = document.querySelector('input[name="password"]');
    var su = document.querySelector('input[name="smtp_username"]');
    var sp = document.querySelector('input[name="smtp_password"]');
    if (su && u) su.value = u.value;
    if (sp && p) sp.value = p.value;
  }
}
function mailDiscoveryStatus() {
  return document.getElementById('mail-discovery-status');
}
function mailDiscoveryEscape(value) {
  return String(value || '').replace(/[&<>"]/g, function(ch) {
    return ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[ch]);
  });
}
function mailDiscoverySourceLabel(source) {
  if (source === 'provider_xml') return 'Provider XML';
  if (source === 'thunderbird_xml') return 'Thunderbird XML';
  if (source === 'mx_provider_xml') return 'Provider via MX';
  if (source === 'dns_srv') return 'DNS SRV';
  if (source === 'heuristic') return 'Common hostnames';
  return source || 'Discovery';
}
function mailDiscoveryProviderLabel(provider) {
  if (provider === 'gmail') return 'Google';
  if (provider === 'outlook') return 'Microsoft';
  return 'provider';
}
function mailDiscoverySetField(form, name, value) {
  var input = form.querySelector('[name="' + name + '"]');
  if (!input) return;
  input.value = value || '';
  input.dispatchEvent(new Event('input', { bubbles: true }));
  input.dispatchEvent(new Event('change', { bubbles: true }));
}
function setupMailDiscoveryOAuthSubmit(form) {
  if (!form || form.dataset.mailDiscoveryOAuthBound === 'true') return;
  form.dataset.mailDiscoveryOAuthBound = 'true';
  form.addEventListener('submit', function(event) {
    var provider = form.dataset.discoveryOauthProvider || '';
    if (!provider) return;
    event.preventDefault();
    event.stopImmediatePropagation();
    var oauthForm = document.createElement('form');
    oauthForm.method = 'POST';
    oauthForm.action = '/api/accounts/oauth2/authorize';
    oauthForm.style.display = 'none';
    var data = new FormData(form);
  data.set('provider', provider);
  data.set('flow_action', 'add');
    data.set('auth_method', 'oauth2');
    data.set('password', '_oauth2_');
    data.forEach(function(value, key) {
      var input = document.createElement('input');
      input.type = 'hidden';
      input.name = key;
      input.value = value;
      oauthForm.appendChild(input);
    });
    document.body.appendChild(oauthForm);
    oauthForm.submit();
  }, true);
}
function mailDiscoverySetOAuthMode(form, candidate) {
  var provider = candidate && candidate.auth_method === 'oauth2' ? (candidate.provider || '') : '';
  var password = form ? form.querySelector('[name="password"]') : null;
  if (provider) {
    form.dataset.discoveryOauthProvider = provider;
    if (password) {
      password.required = false;
      password.value = '_oauth2_';
    }
    setupMailDiscoveryOAuthSubmit(form);
    return;
  }
  if (form) delete form.dataset.discoveryOauthProvider;
  if (password) {
    password.required = true;
    if (password.value === '_oauth2_') password.value = '';
  }
}
function applyMailDiscoveryCandidate(index) {
  var candidates = window._mailDiscoveryCandidates || [];
  var candidate = candidates[index];
  var form = document.getElementById('account-form');
  var status = mailDiscoveryStatus();
  if (!candidate || !form) return;
  mailDiscoverySetField(form, 'imap_host', candidate.imap_host || '');
  mailDiscoverySetField(form, 'imap_port', candidate.imap_port || '');
  mailDiscoverySetField(form, 'imap_tls_mode', candidate.imap_tls_mode || 'tls');
  mailDiscoverySetField(form, 'smtp_host', candidate.smtp_host || '');
  mailDiscoverySetField(form, 'smtp_port', candidate.smtp_port || '');
  mailDiscoverySetField(form, 'smtp_tls_mode', candidate.smtp_tls_mode || 'tls');
  mailDiscoverySetField(form, 'username', candidate.username || '');
  mailDiscoverySetField(form, 'auth_method', candidate.auth_method || 'plain');
  mailDiscoverySetOAuthMode(form, candidate);
  var sameAuth = document.getElementById('smtp-same-auth');
  var separateSmtpUser = candidate.smtp_username && candidate.smtp_username !== candidate.username;
  if (sameAuth) {
    sameAuth.checked = !separateSmtpUser;
    toggleSmtpAuth(sameAuth.checked);
  }
  if (separateSmtpUser) mailDiscoverySetField(form, 'smtp_username', candidate.smtp_username || '');
  if (status) {
    var label = mailDiscoverySourceLabel(candidate.source);
    var summary = mailDiscoveryEscape(candidate.imap_host) + ':' + mailDiscoveryEscape(candidate.imap_port) + ' / ' + mailDiscoveryEscape(candidate.smtp_host) + ':' + mailDiscoveryEscape(candidate.smtp_port);
    status.querySelectorAll('[data-mail-discovery-candidate]').forEach(function(item, idx) {
      item.classList.toggle('border-primary', idx === index);
      item.classList.toggle('bg-primary/10', idx === index);
    });
    var applied = status.querySelector('[data-mail-discovery-applied]');
    if (applied) {
      var oauthHint = candidate.auth_method === 'oauth2' && candidate.provider ? ' Continue with ' + mailDiscoveryProviderLabel(candidate.provider) + ' sign-in to finish.' : '';
      applied.textContent = 'Applied ' + label + ' settings: ' + summary + oauthHint;
    }
  }
}
function renderMailDiscoveryCandidates(candidates) {
  var status = mailDiscoveryStatus();
  if (!status) return;
  if (!candidates.length) {
    status.innerHTML = '<div class="rounded-md border border-border bg-background/60 px-3 py-2 text-xs text-muted-foreground">No automatic settings were found. Continue with manual setup.</div>';
    return;
  }
  var html = '<div class="rounded-lg border border-border bg-background/60 p-3 text-xs"><div class="flex items-start justify-between gap-3"><div><div class="font-semibold text-foreground" data-mail-discovery-applied>Choose a configuration to apply</div><div class="mt-0.5 text-muted-foreground">' + candidates.length + ' candidate' + (candidates.length === 1 ? '' : 's') + ' found</div></div></div>';
  html += '<div class="mt-3 grid gap-2">';
  candidates.forEach(function(candidate, index) {
    var title = mailDiscoverySourceLabel(candidate.source);
    var summary = mailDiscoveryEscape(candidate.imap_host) + ':' + mailDiscoveryEscape(candidate.imap_port) + ' ' + mailDiscoveryEscape(candidate.imap_tls_mode) + ' / ' + mailDiscoveryEscape(candidate.smtp_host) + ':' + mailDiscoveryEscape(candidate.smtp_port) + ' ' + mailDiscoveryEscape(candidate.smtp_tls_mode);
    var notes = Array.isArray(candidate.notes) ? candidate.notes.filter(Boolean).slice(0, 2) : [];
    var card = '<button type="button" data-mail-discovery-candidate class="w-full rounded-md border border-border bg-card px-3 py-2 text-left transition-colors hover:bg-accent" data-on-click=\'["applyMailDiscoveryCandidate",' + index + ']\'><span class="flex items-center justify-between gap-3"><span class="font-semibold text-foreground">' + mailDiscoveryEscape(title) + '</span><span class="text-[11px] text-muted-foreground">' + mailDiscoveryEscape(candidate.confidence) + '%</span></span><span class="mt-1 block truncate text-muted-foreground">' + summary + '</span>';
    if (notes.length) {
      card += '<span class="mt-1 block text-[11px] leading-relaxed text-muted-foreground">' + mailDiscoveryEscape(notes.join(' ')) + '</span>';
    }
    html += card + '</button>';
  });
  html += '</div>';
  html += '</div>';
  status.innerHTML = html;
}
// The add wizard also adds iCloud accounts. They are IMAP accounts on Apple's
// servers, so the wizard fills those in and hides them, leaving the login.
// The server fills them in again from mail_service, whatever is sent.
var addAccountICloudServers = {
  imap_host: 'imap.mail.me.com', imap_port: '993', imap_tls_mode: 'tls',
  smtp_host: 'smtp.mail.me.com', smtp_port: '587', smtp_tls_mode: 'starttls',
  auth_method: 'plain'
};
var addAccountDefaultServers = {
  imap_host: '', imap_port: '993', imap_tls_mode: 'tls',
  smtp_host: '', smtp_port: '465', smtp_tls_mode: 'tls',
  auth_method: 'plain'
};
function setAddAccountMailService(service) {
  var root = document.getElementById('add-account-dialog');
  var form = document.getElementById('account-form');
  if (!root || !form) return;
  var icloud = service === 'icloud';
  var jmap = service === 'jmap';
  var wasICloud = root.dataset.mailService === 'icloud';
  if (icloud || jmap) root.dataset.mailService = service; else delete root.dataset.mailService;
  var marker = form.querySelector('input[name="mail_service"]');
  if (marker) marker.value = icloud || jmap ? service : '';
  var username = form.querySelector('input[name="username"]');
  if (username) {
    username.required = !icloud && !jmap;
    username.placeholder = icloud || jmap ? 'Same as the email address' : 'you@example.com';
  }
  // JMAP has no IMAP or SMTP servers; their hidden fields must not block the form.
  ['imap_host', 'smtp_host'].forEach(function(name) {
    var field = form.querySelector('input[name="' + name + '"]');
    if (field) field.required = !jmap;
  });
  setAddAccountJMAPSessionURL('');
  if (icloud || wasICloud) {
    var servers = icloud ? addAccountICloudServers : addAccountDefaultServers;
    Object.keys(servers).forEach(function(name) { mailDiscoverySetField(form, name, servers[name]); });
    mailDiscoverySetOAuthMode(form, null);
    var sameAuth = document.getElementById('smtp-same-auth');
    if (sameAuth && icloud) {
      sameAuth.checked = true;
      toggleSmtpAuth(true);
    }
  }
  syncAddAccountSubmitButtons();
}
function openAddAccountWizard(service) {
  setAddAccountMailService(service);
  var root = document.getElementById('add-account-dialog');
  if (root) delete root.dataset.addMail;
  goferSwitchDialog('choose-account-type-dialog', 'add-account-dialog');
}
// An account with only calendars or contacts gets its mail through the add
// wizard, which then saves the mailbox to that account instead of creating
// one, and goes on to its contacts and calendar as usual.
var _addMailAccount = null;
function openAddMailChooser(accountID, name) {
  _addMailAccount = { id: accountID, name: name || '' };
  var chooser = document.getElementById('choose-mail-type-dialog');
  if (!chooser || !window.tui || !window.tui.dialog) return;
  chooser.querySelectorAll('[data-add-mail-account-name]').forEach(function(label) { label.textContent = name || 'this account'; });
  if (window.tui.dialog.isOpen('edit-dav-account-dialog')) {
    goferSwitchDialog('edit-dav-account-dialog', 'choose-mail-type-dialog');
  } else {
    window.tui.dialog.open('choose-mail-type-dialog');
  }
}
function startAddMail(service) {
  var root = document.getElementById('add-account-dialog');
  var form = document.getElementById('account-form');
  if (!_addMailAccount || !root || !form) return;
  setAddAccountMailService(service);
  root.dataset.addMail = _addMailAccount.id;
  var name = form.querySelector('input[name="display_name"]');
  if (name && !name.value) name.value = _addMailAccount.name;
  goferSwitchDialog('choose-mail-type-dialog', 'add-account-dialog');
}
document.body.addEventListener('htmx:configRequest', function(event) {
  var root = document.getElementById('add-account-dialog');
  if (!event.target || event.target.id !== 'account-form' || !root || !root.dataset.addMail) return;
  event.detail.path = '/api/accounts/' + encodeURIComponent(root.dataset.addMail) + '/mail';
});
function addingMailToAccount() {
  var root = document.getElementById('add-account-dialog');
  return !!(root && root.dataset.addMail);
}

// Calendar-only and contacts-only accounts: a server, its login, and the
// calendars or address books to sync. Find signs in and lists them; the
// account is then created, or saved, with the ones ticked. Changing the
// server or login drops the list until it is found again.
function openDAVAccountDialog(service) {
  var root = document.getElementById('add-dav-account-dialog');
  var form = root && root.querySelector('[data-dav-account-form]');
  if (!form || !window.tui || !window.tui.dialog) return;
  form.reset();
  root.dataset.davService = service === 'contacts' ? 'contacts' : 'calendar';
  form.querySelector('input[name="service"]').value = root.dataset.davService;
  davAccountItems(root, '');
  davAccountStatus(root, '', '');
  davAccountBusy(root, false);
  window.tui.dialog.open('add-dav-account-dialog');
}
function davAccountRoot(element) {
  return element && element.closest ? element.closest('[data-dav-account-dialog]') : null;
}
function davAccountStatus(root, kind, message) {
  var status = root.querySelector('[data-dav-account-status]');
  if (!status) return;
  status.textContent = '';
  if (!message) return;
  var box = document.createElement('div');
  box.setAttribute('role', kind === 'error' ? 'alert' : 'status');
  if (kind === 'busy') {
    box.className = 'flex items-center gap-2 py-1 text-xs text-muted-foreground';
    var spinner = document.createElement('div');
    spinner.className = 'size-4 rounded-full border-2 border-muted-foreground/30 border-t-muted-foreground animate-spin';
    box.appendChild(spinner);
    box.appendChild(document.createElement('span')).textContent = message;
  } else {
    box.className = 'rounded-md border border-destructive/20 bg-destructive/10 px-3 py-2 text-xs text-destructive';
    box.textContent = message;
  }
  status.appendChild(box);
}
function davAccountItems(root, html) {
  var list = root.querySelector('[data-dav-items]');
  if (list) list.innerHTML = html;
  var save = root.querySelector('[data-dav-account-save]');
  if (save) save.disabled = !root.querySelector('[data-dav-items-found]');
}
function davAccountBusy(root, busy) {
  root.querySelectorAll('[data-dav-account-action]').forEach(function(button) {
    button.disabled = busy || (button.hasAttribute('data-dav-account-save') && !root.querySelector('[data-dav-items-found]'));
  });
}
function davAccountURL(root, path) {
  var id = root.dataset.accountId;
  return id ? '/api/accounts/' + encodeURIComponent(id) + '/dav' + path : '/api/accounts/dav' + path;
}
function davAccountRequest(root, path, busyMessage) {
  var form = root.querySelector('[data-dav-account-form]');
  var missing = Array.prototype.find.call(form.querySelectorAll('input[required]'), function(input) {
    return !input.value.trim();
  });
  if (missing) {
    missing.reportValidity();
    return Promise.reject(new Error(''));
  }
  davAccountBusy(root, true);
  davAccountStatus(root, 'busy', busyMessage);
  return fetch(davAccountURL(root, path), {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'Accept': 'text/html' },
    body: new URLSearchParams(new FormData(form)).toString()
  }).then(function(response) {
    return response.text().then(function(body) {
      if (!response.ok) throw new Error((body || '').trim() || 'Could not reach the server.');
      davAccountStatus(root, '', '');
      return body;
    });
  }).catch(function(error) {
    davAccountBusy(root, false);
    if (error && error.message) davAccountStatus(root, 'error', error.message);
    throw new Error('');
  });
}
function findDAVItems(button) {
  var root = davAccountRoot(button);
  if (!root) return;
  davAccountRequest(root, '/find', 'Signing in...').then(function(html) {
    davAccountItems(root, html);
    davAccountBusy(root, false);
  }).catch(function() {});
}
function saveDAVAccount(root) {
  var editing = !!root.dataset.accountId;
  davAccountRequest(root, '', editing ? 'Signing in and saving...' : 'Signing in and creating the account...').then(function() {
    window.location.href = editing ? '/settings/accounts' : '/settings/accounts?account_added=1';
  }).catch(function() {});
}
document.addEventListener('submit', function(event) {
  var form = event.target;
  if (!form || !form.hasAttribute || !form.hasAttribute('data-dav-account-form')) return;
  event.preventDefault();
  var root = davAccountRoot(form);
  if (root) saveDAVAccount(root);
});
document.addEventListener('input', function(event) {
  var input = event.target;
  if (!input || !input.hasAttribute || !input.hasAttribute('data-dav-login')) return;
  var root = davAccountRoot(input);
  if (root && root.querySelector('[data-dav-items-found]')) davAccountItems(root, '');
});
// Edit on a calendar-only or contacts-only account loads its own dialog.
document.addEventListener('htmx:afterSettle', function(event) {
  if (event.target && event.target.querySelector && event.target.querySelector('#edit-dav-account-dialog') && window.tui && window.tui.dialog) {
    setTimeout(function() { window.tui.dialog.open('edit-dav-account-dialog'); }, 20);
  }
});
function discoverMailSettings(button) {
  var form = document.getElementById('account-form');
  var status = mailDiscoveryStatus();
  if (!form || !status) return;
  var email = form.querySelector('input[name="email_address"]');
  if (!email || !email.value.trim()) {
    status.innerHTML = '<div class="rounded-md border border-destructive/20 bg-destructive/10 px-3 py-2 text-xs text-destructive">Enter an email address first.</div>';
    return;
  }
  var label = button ? button.querySelector('[data-mail-discovery-label]') : null;
  var originalLabel = label ? label.textContent : '';
  if (button) button.disabled = true;
  if (label) label.textContent = 'Discovering...';
  status.innerHTML = '<div class="rounded-lg border border-border bg-background/60 p-3 text-xs text-muted-foreground"><div class="flex items-center gap-2"><span class="size-3.5 rounded-full border-2 border-muted-foreground/30 border-t-muted-foreground animate-spin"></span><span>Checking provider XML, MX provider records, DNS SRV, and verified common hostnames...</span></div></div>';
  var body = new URLSearchParams();
  body.append('email_address', email.value.trim());
  fetch('/api/accounts/discover', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'Accept': 'application/json' },
    body: body.toString()
  }).then(function(resp) {
    return resp.json().then(function(data) {
      if (!resp.ok) throw new Error(data && data.error ? data.error : 'Could not discover settings.');
      return data;
    });
  }).then(function(data) {
    window._mailDiscoveryCandidates = data.candidates || [];
    renderMailDiscoveryCandidates(window._mailDiscoveryCandidates);
  }).catch(function(err) {
    status.innerHTML = '<div class="rounded-md border border-destructive/20 bg-destructive/10 px-3 py-2 text-xs text-destructive">' + mailDiscoveryEscape(err && err.message ? err.message : 'Could not discover settings.') + '</div>';
  }).finally(function() {
    if (button) button.disabled = false;
    if (label) label.textContent = originalLabel || 'Discover settings';
  });
}
function contactSyncSaveButtons(accountID) {
  return document.querySelectorAll('[data-contact-sync-save-button][data-account-id="' + accountID + '"]');
}
function contactSyncSetSaveButton(accountID, label, disabled, continueMode) {
  contactSyncSaveButtons(accountID).forEach(function(button) {
    var buttonLabel = button.querySelector('[data-account-edit-submit-label]');
    if (buttonLabel) buttonLabel.textContent = label; else button.textContent = label;
    button.disabled = !!disabled;
    button.setAttribute('aria-busy', label === 'Saving...' ? 'true' : 'false');
    var buttonSpinner = button.querySelector('[data-account-edit-submit-spinner]');
    if (buttonSpinner) buttonSpinner.classList.toggle('hidden', label !== 'Saving...');
    if (continueMode) {
      button.type = 'button';
      button.removeAttribute('form');
      button.onclick = function() { document.getElementById('edit-account-form').requestSubmit(); };
    } else {
      button.type = 'submit';
      button.setAttribute('form', 'account-contact-sync-form-' + accountID);
      button.onclick = null;
    }
  });
}
function contactSyncSaving(accountID) {
  contactSyncSetSaveButton(accountID, 'Saving...', true, false);
}
function contactSyncSaved(accountID, ok) {
  if (!ok) {
    contactSyncSetSaveButton(accountID, 'Save', false, false);
    return;
  }
  contactSyncSetSaveButton(accountID, 'Success', true, false);
  window.setTimeout(function() {
    contactSyncSetSaveButton(accountID, 'Continue', false, true);
  }, 2500);
}
function contactSyncResetSave(accountID) {
  contactSyncSetSaveButton(accountID, 'Save', false, false);
}
function toggleCardDAVAuth(checkbox) {
  var form = checkbox && checkbox.closest ? checkbox.closest('form') : null;
  if (!form) return;
  var fields = form.querySelector('[data-carddav-credential-fields]');
  if (!fields) return;
  fields.classList.toggle('opacity-50', checkbox.checked);
  fields.querySelectorAll('input[name="username"], input[name="password"]').forEach(function(input) {
    input.disabled = checkbox.checked;
  });
}
// chooseContactProtocol switches a JMAP account's contacts form between JMAP
// and CardDAV. The other protocol's fields are hidden and disabled so they
// are neither sent nor required; fields that were already disabled, such as
// the CardDAV login while it follows the mail login, are left as they were.
function chooseContactProtocol(radio) {
  var form = radio && radio.closest ? radio.closest('form') : null;
  if (form && radio.checked) applyContactProtocol(form, radio.value);
}
// applyProtocolChoice shows the form's sections for the chosen protocol,
// marked data-<kind>-protocol, and disables the inputs of the others so they
// aren't sent; kind is "contact" or "calendar".
function applyProtocolChoice(form, kind, protocol) {
  var field = form.querySelector('[data-' + kind + '-protocol-field]');
  if (field) field.value = protocol;
  form.querySelectorAll('[data-' + kind + '-protocol]').forEach(function(section) {
    var show = section.getAttribute('data-' + kind + '-protocol') === protocol;
    section.classList.toggle('hidden', !show);
    if (section.tagName === 'SPAN') section.classList.toggle('contents', show);
    section.querySelectorAll('input, select, textarea, button').forEach(function(input) {
      if (!show && !input.disabled) {
        input.disabled = true;
        input.dataset.protocolDisabled = '1';
      } else if (show && input.dataset.protocolDisabled) {
        input.disabled = false;
        delete input.dataset.protocolDisabled;
      }
    });
  });
}
function applyContactProtocol(form, protocol) {
  applyProtocolChoice(form, 'contact', protocol);
  var status = form.querySelector('[id$="-contact-sync-status-' + (form.dataset.accountId || '') + '"]');
  if (status && form.dataset.contactProtocolShown && form.dataset.contactProtocolShown !== protocol) status.textContent = '';
  form.dataset.contactProtocolShown = protocol;
}
function syncContactProtocols(root) {
  (root || document).querySelectorAll('[data-contact-protocol-field]').forEach(function(field) {
    var form = field.closest('form');
    if (form) applyContactProtocol(form, field.value);
  });
}
// chooseCalendarProtocol switches a JMAP account's calendar form between
// JMAP and CalDAV.
function chooseCalendarProtocol(radio) {
  var form = radio && radio.closest ? radio.closest('form') : null;
  if (form && radio.checked) applyProtocolChoice(form, 'calendar', radio.value);
}
function syncProtocolChoices(root) {
  syncContactProtocols(root);
  (root || document).querySelectorAll('[data-calendar-protocol-field]').forEach(function(field) {
    var form = field.closest('form');
    if (form) applyProtocolChoice(form, 'calendar', field.value);
  });
}
document.addEventListener('DOMContentLoaded', function() { syncProtocolChoices(document); });
document.body.addEventListener('htmx:afterSwap', function(event) { syncProtocolChoices(event.target || document); });
function discoverCardDAV(accountID, fieldsID, statusID, autodiscover) {
  var fields = document.getElementById(fieldsID);
  var status = document.getElementById(statusID);
  if (!fields || !status) return;
  var controller = window.AbortController ? new AbortController() : null;
  var timeout = controller ? window.setTimeout(function() { controller.abort(); }, 28000) : null;
  var body = new URLSearchParams();
  fields.querySelectorAll('input, select, textarea').forEach(function(input) {
    if (!input.name || input.disabled) return;
    body.append(input.name, input.value || '');
  });
  if (autodiscover) body.append('autodiscover', '1');
  var overJMAP = body.get('provider') === 'jmap';
  function renderDiscoveryProgress(completed, total, endpoint) {
    var percent = total > 0 ? Math.max(0, Math.min(100, Math.round((completed / total) * 100))) : 0;
    var label = total > 0 ? 'Testing ' + Math.min(completed + 1, total) + ' of ' + total + ' CardDAV checks' : (overJMAP ? 'Looking up JMAP address books...' : 'Preparing CardDAV discovery...');
    var safeEndpoint = endpoint ? endpoint.replace(/[&<>"]/g, function(ch) { return ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[ch]); }) : '';
    status.innerHTML = '<div class="rounded-lg border border-border bg-background/60 p-3 text-xs text-muted-foreground"><div class="flex items-center justify-between gap-3"><span class="font-medium text-foreground">' + label + '</span><span class="tabular-nums">' + completed + '/' + (total || '?') + '</span></div><div class="mt-2 h-1.5 overflow-hidden rounded-md bg-muted"><div class="h-full rounded-md bg-primary transition-all" style="width:' + percent + '%"></div></div>' + (safeEndpoint ? '<div class="mt-2 truncate">' + safeEndpoint + '</div>' : '') + '</div>';
  }
  renderDiscoveryProgress(0, 0, '');
  fetch('/api/accounts/' + encodeURIComponent(accountID) + '/contacts/sync/discover', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'Accept': 'application/x-ndjson' },
    body: body.toString(),
    signal: controller ? controller.signal : undefined
  }).then(function(resp) {
    if (!resp.body || !resp.body.getReader) throw new Error('stream unavailable');
    var reader = resp.body.getReader();
    var decoder = new TextDecoder();
    var buffer = '';
    function handleEvent(event) {
      if (!event || !event.type) return;
      if (event.type === 'progress') {
        renderDiscoveryProgress(event.completed || 0, event.total || 0, event.endpoint || '');
        return;
      }
      if (event.type === 'error') {
        if (timeout) window.clearTimeout(timeout);
        var err = document.createElement('div');
        err.className = 'rounded-md border border-destructive/20 bg-destructive/10 px-3 py-2 text-xs text-destructive';
        err.textContent = event.error || 'Could not discover CardDAV address books.';
        status.textContent = '';
        status.appendChild(err);
        return;
      }
      if (event.type === 'done') {
        if (timeout) window.clearTimeout(timeout);
        renderDiscoveryResults(event.address_books || []);
      }
    }
    function read() {
      return reader.read().then(function(result) {
        if (result.done) return;
        buffer += decoder.decode(result.value, { stream: true });
        var lines = buffer.split('\n');
        buffer = lines.pop() || '';
        lines.forEach(function(line) {
          line = line.trim();
          if (!line) return;
          try { handleEvent(JSON.parse(line)); } catch (_) {}
        });
        return read();
      });
    }
    return read();
  }).catch(function(err) {
    if (timeout) window.clearTimeout(timeout);
    var message = err && err.name === 'AbortError' ? 'CardDAV discovery timed out. Check the app logs for the attempted DNS and URL checks, or enter the provider\'s exact CardDAV base URL and try again.' : 'Could not discover CardDAV address books. Check the app logs for details.';
    status.innerHTML = '<div class="rounded-md border border-destructive/20 bg-destructive/10 px-3 py-2 text-xs text-destructive">' + message + '</div>';
  });
  function renderDiscoveryResults(books) {
    status.textContent = '';
    if (!books.length) {
      var empty = document.createElement('div');
      empty.className = 'rounded-md border border-border bg-background/60 px-3 py-2 text-xs text-muted-foreground';
      empty.textContent = overJMAP ? 'The JMAP server has no address books for this login.' : 'No CardDAV address books were found.';
      status.appendChild(empty);
      return;
    }
    var pickerSection = fields.querySelector('[data-carddav-addressbook-section]');
    if (pickerSection) pickerSection.classList.remove('hidden');
    var existingInputs = fields.querySelectorAll('input[name="addressbook_url"]');
    var selected = {};
    existingInputs.forEach(function(input) { if (input.value) selected[input.value] = true; });
    var wrap = document.createElement('div');
    wrap.className = pickerSection ? '' : 'rounded-lg border border-border bg-background/60 p-3';
    var title = document.createElement('div');
    title.className = 'text-xs font-semibold text-foreground';
    title.textContent = 'Choose address books';
    wrap.appendChild(title);
    var hint = document.createElement('p');
    hint.className = 'mt-1 text-[11px] leading-relaxed text-muted-foreground';
    hint.textContent = 'Select one or more address books to sync. The default is used for new contacts.';
    wrap.appendChild(hint);
    var list = document.createElement('div');
    list.className = 'mt-3 space-y-2';
    books.forEach(function(book) {
      var row = document.createElement('label');
      row.className = 'flex w-full items-start gap-3 rounded-md border border-border bg-card px-3 py-2 text-xs transition-colors hover:bg-accent';
      var checkWrap = document.createElement('div');
      checkWrap.className = 'relative mt-0.5 inline-flex items-center';
      var check = document.createElement('input');
      check.type = 'checkbox';
      check.name = 'addressbook_url';
      check.value = book.url || '';
      check.checked = !!selected[book.url] || Object.keys(selected).length === 0;
      check.className = 'peer size-4 shrink-0 rounded-sm border border-input shadow-xs focus-visible:outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:border-ring disabled:cursor-not-allowed disabled:opacity-50 checked:bg-primary checked:text-primary-foreground checked:border-primary indeterminate:bg-primary indeterminate:text-primary-foreground indeterminate:border-primary appearance-none cursor-pointer transition-shadow relative';
      var checkIcon = document.createElement('div');
      checkIcon.className = 'absolute inset-0 pointer-events-none flex items-center justify-center text-primary-foreground opacity-0 peer-checked:opacity-100';
      checkIcon.innerHTML = '<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="size-3.5" data-lucide="icon"><path d="M20 6 9 17l-5-5"></path></svg>';
      checkWrap.appendChild(check);
      checkWrap.appendChild(checkIcon);
      var hiddenName = document.createElement('input');
      hiddenName.type = 'hidden';
      hiddenName.name = 'addressbook_name';
      hiddenName.value = book.name || '';
      hiddenName.disabled = !check.checked;
      var hiddenID = document.createElement('input');
      hiddenID.type = 'hidden';
      hiddenID.name = 'addressbook_id';
      hiddenID.value = book.id || '';
      hiddenID.disabled = !check.checked;
      var content = document.createElement('div');
      content.className = 'min-w-0 flex-1';
      var name = document.createElement('div');
      name.className = 'font-semibold text-foreground';
      name.textContent = book.name || 'Address book';
      var url = document.createElement('div');
      url.className = 'mt-0.5 truncate text-muted-foreground';
      url.textContent = overJMAP ? '' : (book.url || '');
      var defaultWrap = document.createElement('label');
      defaultWrap.className = 'mt-2 inline-flex items-center gap-1.5 text-[11px] text-muted-foreground';
      var radio = document.createElement('input');
      radio.type = 'radio';
      radio.name = 'default_addressbook_url';
      radio.value = book.url || '';
      radio.disabled = !check.checked;
      radio.className = 'relative h-4 w-4 before:absolute before:left-1/2 before:top-1/2 before:h-1.5 before:w-1.5 before:-translate-x-1/2 before:-translate-y-1/2 appearance-none rounded-full border-2 border-primary before:content-[\'\'] before:rounded-full before:bg-background checked:border-primary checked:bg-primary checked:before:visible focus-visible:outline-hidden focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background disabled:cursor-not-allowed';
      defaultWrap.appendChild(radio);
      defaultWrap.appendChild(document.createTextNode('Default for new contacts'));
      content.appendChild(name);
      content.appendChild(url);
      content.appendChild(defaultWrap);
      row.appendChild(checkWrap);
      row.appendChild(hiddenID);
      row.appendChild(hiddenName);
      row.appendChild(content);
      check.addEventListener('change', function() {
        radio.disabled = !check.checked;
        hiddenName.disabled = !check.checked;
        hiddenID.disabled = !check.checked;
        row.classList.toggle('border-primary', check.checked);
        row.classList.toggle('bg-primary/10', check.checked);
        if (check.checked && !list.querySelector('input[name="default_addressbook_url"]:checked')) radio.checked = true;
        if (!check.checked && radio.checked) {
          radio.checked = false;
          var next = list.querySelector('input[name="addressbook_url"]:checked');
          if (next) next.closest('label').querySelector('input[name="default_addressbook_url"]').checked = true;
        }
      });
      if (check.checked) row.classList.add('border-primary', 'bg-primary/10');
      list.appendChild(row);
    });
    var firstChecked = list.querySelector('input[name="addressbook_url"]:checked');
    if (firstChecked) firstChecked.closest('label').querySelector('input[name="default_addressbook_url"]').checked = true;
    wrap.appendChild(list);
    if (pickerSection) {
      pickerSection.textContent = '';
      pickerSection.appendChild(wrap);
    } else {
      status.appendChild(wrap);
    }
  }
}
document.addEventListener('click', function(event) {
  var button = event.target && event.target.closest ? event.target.closest('[data-carddav-discover]') : null;
  if (!button) return;
  event.preventDefault();
  discoverCardDAV(button.dataset.accountId || '', button.dataset.fieldsId || '', button.dataset.statusId || '', button.dataset.carddavAutodiscover === 'true');
});
document.addEventListener('click', function(event) {
  var button = event.target && event.target.closest ? event.target.closest('[data-carddav-manual]') : null;
  if (!button) return;
  event.preventDefault();
  var fields = document.getElementById(button.dataset.fieldsId || '');
  if (!fields) return;
  var manual = fields.querySelector('[data-carddav-manual-addressbook]');
  if (!manual) return;
  manual.classList.remove('hidden');
  var input = manual.querySelector('input[name="addressbook_url"]');
  if (input) input.focus();
});
document.addEventListener('input', function(event) {
  if (event.target && event.target.closest && event.target.closest('#account-form')) syncAddContactStep();
  var form = event.target && event.target.closest ? event.target.closest('[data-contact-sync-form]') : null;
  if (form) contactSyncResetSave(form.dataset.accountId || '');
});
document.addEventListener('change', function(event) {
  var form = event.target && event.target.closest ? event.target.closest('[data-contact-sync-form]') : null;
  if (form) contactSyncResetSave(form.dataset.accountId || '');
});
document.body.addEventListener('htmx:beforeRequest', function(event) {
  var form = event.target && event.target.closest ? event.target.closest('[data-contact-sync-form]') : null;
  if (form) contactSyncSaving(form.dataset.accountId || '');
});
document.body.addEventListener('htmx:afterRequest', function(event) {
  var form = event.target && event.target.closest ? event.target.closest('[data-contact-sync-form]') : null;
  if (!form) return;
  var xhr = event.detail && event.detail.xhr ? event.detail.xhr : null;
  var ok = !!(event.detail && event.detail.successful) && (!xhr || xhr.getResponseHeader('X-Gofer-Status') !== 'error');
  contactSyncSaved(form.dataset.accountId || '', ok);
});
  document.body.addEventListener('htmx:afterRequest', function(event) {
    var target = event.detail && event.detail.elt ? event.detail.elt : event.target;
    var form = target && target.closest ? target.closest('[data-wizard-contact-form]') : null;
  if (!form) return;
    var xhr = event.detail && event.detail.xhr ? event.detail.xhr : null;
    var ok = !!(event.detail && event.detail.successful) && (!xhr || xhr.getResponseHeader('X-Gofer-Status') !== 'error');
    if (!ok) return;
    var root = form.closest('#add-account-dialog,#edit-account-dialog');
    var toggle = root && root.querySelector('[data-wizard-service-switch="contacts"]');
    var savedEnabled = xhr ? xhr.getResponseHeader('X-Gofer-Contact-Sync-Enabled') : '';
    if (toggle && (savedEnabled === 'true' || savedEnabled === 'false')) {
      toggle.checked = savedEnabled === 'true';
      toggle.dataset.wizardServiceInitialEnabled = savedEnabled;
      syncWizardServiceContent(root);
      if (root.id === 'add-account-dialog') syncAddContactStep();
    }
    if (root) wizardServiceAdvance(root, Number(form.dataset.wizardNextStep || 0));
  });
function refreshCardDAVBaseURLActions(root) {
  (root || document).querySelectorAll('[data-carddav-base-url-action]').forEach(function(group) {
    var fields = document.getElementById(group.dataset.fieldsId || '');
    var base = fields && fields.querySelector ? fields.querySelector('input[name="base_url"]') : null;
    var enabled = !!(base && base.value && base.value.trim());
    var enabledAction = group.querySelector('[data-carddav-enabled-action]');
    var disabledAction = group.querySelector('[data-carddav-disabled-action]');
    if (enabledAction) enabledAction.classList.toggle('!hidden', !enabled);
    if (disabledAction) disabledAction.classList.toggle('!hidden', enabled);
  });
}
document.addEventListener('input', function(event) {
  if (!event.target || !event.target.matches || !event.target.matches('input[name="base_url"]')) return;
  refreshCardDAVBaseURLActions(document);
});
document.addEventListener('DOMContentLoaded', function() { refreshCardDAVBaseURLActions(document); });
document.body.addEventListener('htmx:afterSwap', function(event) {
  refreshCardDAVBaseURLActions(event.target || document);
  var addRoot = document.getElementById('add-account-dialog');
  if (addRoot) syncAddContactStep();
  if (addRoot && typeof _wizStep !== 'undefined') setWizardFooter(addRoot, _wizStep, true);
  if (addRoot) syncWizardServiceContent(addRoot);
  var editRoot = document.getElementById('edit-account-dialog');
  if (editRoot && typeof _editWizStep !== 'undefined') setWizardFooter(editRoot, _editWizStep, true);
  if (editRoot) syncWizardServiceContent(editRoot);
});
document.body.addEventListener('htmx:oobAfterSwap', function() {
  var addRoot = document.getElementById('add-account-dialog');
  if (addRoot) syncAddContactStep();
  if (addRoot && typeof _wizStep !== 'undefined') setWizardFooter(addRoot, _wizStep, true);
  if (addRoot) syncWizardServiceContent(addRoot);
  var editRoot = document.getElementById('edit-account-dialog');
  if (editRoot && typeof _editWizStep !== 'undefined') setWizardFooter(editRoot, _editWizStep, true);
  if (editRoot) syncWizardServiceContent(editRoot);
});
document.body.addEventListener('htmx:afterSettle', function() {
  var addRoot = document.getElementById('add-account-dialog');
  if (addRoot) syncAddContactStep();
  if (addRoot && typeof _wizStep !== 'undefined') setWizardFooter(addRoot, _wizStep, true);
  if (addRoot) syncWizardServiceContent(addRoot);
  var editRoot = document.getElementById('edit-account-dialog');
  if (editRoot && typeof _editWizStep !== 'undefined') setWizardFooter(editRoot, _editWizStep, true);
  if (editRoot) syncWizardServiceContent(editRoot);
});
document.addEventListener('DOMContentLoaded', function() {
  var dlg = document.querySelector('#add-account-dialog dialog[data-tui-dialog-content]');
  if (!dlg) return;
  new MutationObserver(function() {
    var track = document.getElementById('wizard-track');
    if (!track) return;
    if (dlg.open) {
      wizardGo(0, true);
      syncAddContactStep();
      syncWizardServiceContent(document.getElementById('add-account-dialog'));
    }
  }).observe(dlg, { attributes: true, attributeFilter: ['open'] });
});

// Edit account dialog
var _editWizStep = 0;
// Mail changes are kept in the form until step 1.d tests and saves them.
// The form counts as changed when its values differ from those it opened
// with: its widgets fire change events while loading, so events alone
// can't say whether the person changed anything.
var _editAccountSavedKey = null;
function editAccountFormKey() {
  var form = document.getElementById('edit-account-form');
  return form ? new URLSearchParams(new FormData(form)).toString() : '';
}
function editAccountChanged() {
  return _editAccountSavedKey !== null && editAccountFormKey() !== _editAccountSavedKey;
}
function editAccountMarkSaved() {
  _editAccountSavedKey = editAccountFormKey();
}
function newEditAccountTest() {
  return accountWizardTest({
    form: 'edit-account-form',
    results: 'edit-account-test-results',
    footer: '#edit-wizard-footer [data-wizard-footer-step="3"]',
    anywayLabel: 'Skip test and save',
    anywayFailedLabel: 'Save anyway',
    request: function(key) {
      var results = document.getElementById('edit-account-test-results');
      var typed = results && results.dataset.testTyped === 'true';
      return { url: results ? results.dataset.testUrl : '', body: typed ? key : '' };
    },
    // A JMAP account's test reports the session URL it found; saving then
    // stores that instead of looking for the server again.
    onResponse: function(response) {
      var found = response.headers.get('X-Gofer-JMAP-Session-URL');
      var field = found && document.querySelector('#edit-account-form input[name="jmap_session_url"]');
      if (field) field.value = found;
    },
    onState: function() {
      var label = document.querySelector('#edit-wizard-footer [data-edit-account-save-label]');
      if (label) label.textContent = editAccountChanged() ? 'Save changes' : 'Continue';
    }
  });
}
var editAccountTest = newEditAccountTest();
// Each loaded edit dialog starts at its first step with a new test, and
// remembers its saved settings once its fields have been filled in.
goferOnContent('#edit-account-dialog', function() {
  _editWizStep = 0;
  _editAccountSavedKey = null;
  window.setTimeout(editAccountMarkSaved, 300);
  editAccountTest = newEditAccountTest();
});
// An account imported from a profile marks the steps that still need a
// password with "!". A save button stays unusable until its form's marked
// password fields are filled, and a save that brings a password clears the
// marks for it. The account syncs each service once its password is saved.
function reconnectMissingFields(form) {
  if (!form) return false;
  return Array.prototype.some.call(form.querySelectorAll('[data-reconnect-required]'), function(input) {
    return !input.disabled && !input.value.trim();
  });
}
function updateReconnectGates() {
  var root = document.getElementById('edit-account-dialog');
  if (!root) return;
  root.querySelectorAll('[data-reconnect-gate]').forEach(function(button) {
    var form = document.getElementById(button.getAttribute('data-reconnect-gate'));
    var service = button.getAttribute('data-reconnect-gate-switch');
    var toggle = service ? root.querySelector('[data-wizard-service-switch="' + service + '"]') : null;
    var held = (!toggle || toggle.checked) && reconnectMissingFields(form);
    button.classList.toggle('pointer-events-none', held);
    button.classList.toggle('opacity-50', held);
    if (held) {
      button.setAttribute('aria-disabled', 'true');
      button.title = 'Enter the password marked with ! first';
    } else if (button.getAttribute('aria-disabled') === 'true') {
      button.removeAttribute('aria-disabled');
      button.removeAttribute('title');
    }
  });
}
goferOnContent('#edit-account-dialog', updateReconnectGates);
document.addEventListener('input', function(event) {
  if (event.target.closest && event.target.closest('#edit-account-dialog')) updateReconnectGates();
});
document.addEventListener('change', function(event) {
  if (event.target.closest && event.target.closest('#edit-account-dialog')) updateReconnectGates();
});
document.addEventListener('submit', function(event) {
  var form = event.target;
  if (!form.closest || !form.closest('#edit-account-dialog') || !reconnectMissingFields(form)) return;
  var field = Array.prototype.find.call(form.querySelectorAll('[data-reconnect-required]'), function(input) {
    return !input.disabled && !input.value.trim();
  });
  event.preventDefault();
  event.stopImmediatePropagation();
  if (field && field.reportValidity) field.reportValidity();
}, true);
document.addEventListener('htmx:afterRequest', function(event) {
  var xhr = event.detail && event.detail.xhr;
  var login = xhr && xhr.getResponseHeader('X-Gofer-Reconnect-Resolved');
  var root = document.getElementById('edit-account-dialog');
  if (!login || !root) return;
  root.querySelectorAll('[data-reconnect-mark="' + login + '"]').forEach(function(mark) { mark.remove(); });
  root.querySelectorAll('[data-reconnect-required="' + login + '"]').forEach(function(input) {
    input.removeAttribute('data-reconnect-required');
    input.required = false;
  });
  root.querySelectorAll('[data-reconnect-gate]').forEach(function(button) {
    var form = document.getElementById(button.getAttribute('data-reconnect-gate'));
    if (!form || !form.querySelector('[data-reconnect-required]')) button.removeAttribute('data-reconnect-gate');
  });
  var note = root.querySelector('[data-edit-account-reconnect-pending]');
  var remaining = (xhr.getResponseHeader('X-Gofer-Reconnect-Remaining') || '').split(',').filter(Boolean);
  if (note && remaining.length === 0) {
    note.hidden = true;
  } else if (note) {
    var labels = remaining.map(function(item) { return item === 'mail' ? 'Mail' : item.charAt(0).toUpperCase() + item.slice(1); });
    var text = note.querySelector('[data-reconnect-note]');
    var list = labels.length > 1 ? labels.slice(0, -1).join(', ') + ' and ' + labels[labels.length - 1] : labels[0];
    if (text) text.textContent = 'Saved. ' + list + (labels.length > 1 ? ' still need their passwords' : ' still needs its password') + ', in the steps marked with !.';
  }
  updateReconnectGates();
});
document.addEventListener('htmx:afterSettle', function(event) {
  if (event.target.closest && event.target.closest('#edit-account-dialog')) updateReconnectGates();
});
function editWizardEmailEnabled(root) {
  var toggle = root.querySelector('[data-wizard-service-switch="email"]');
  return !toggle || toggle.checked;
}
function editWizardGo(step, immediate) {
  if (step < 0) step = 0;
  if (step > 5) step = 5;
  var track = document.getElementById('edit-wizard-track');
  var root = document.getElementById('edit-account-dialog');
  if (!track || !root) return;
  var emailEnabled = editWizardEmailEnabled(root);
  if (!emailEnabled && step < 4) step = 0;
  step = wizardSkipSending(step, _editWizStep, !!root.querySelector('[data-wizard-skip-sending]'));
  // Moving on to contacts or calendar with unsaved mail changes would lose
  // them, so they go through the test step to be saved first.
  var unsaved = root.querySelector('[data-edit-account-unsaved]');
  if (step >= 4 && emailEnabled && editAccountChanged()) {
    step = 3;
    if (unsaved) unsaved.hidden = false;
  } else if (unsaved) {
    unsaved.hidden = true;
  }
  _editWizStep = step;
  track.style.transform = 'translateX(-' + (step * (100 / 6)) + '%)';
  root.querySelectorAll('[data-account-wizard-step]').forEach(function(item) {
    item.dataset.active = String(+item.dataset.accountWizardStep === step);
  });
  root.querySelectorAll('[data-wizard-section-steps]').forEach(function(section) {
    var steps = (section.dataset.wizardSectionSteps || '').split(',');
    section.dataset.active = String(steps.indexOf(String(step)) !== -1);
  });
  setWizardFooter(root, step, !!immediate);
  if (step === 3) {
    if (reconnectMissingFields(document.getElementById('edit-account-form'))) {
      editAccountTest.saveFailed('Enter the password marked with ! before testing.');
    } else {
      editAccountTest.run(false);
    }
  }
}
// After a passing test: save the changes, or just continue when there are
// none, so the account isn't saved (and its sync restarted) for nothing.
function editAccountSaveOrContinue() {
  var form = document.getElementById('edit-account-form');
  if (!editAccountChanged() || !form) {
    editWizardGo(4, true);
    return;
  }
  form.requestSubmit();
}
function editAccountFormChanged(event) {
  if (!event.target || !event.target.closest || !event.target.closest('#edit-account-form')) return;
  editAccountTest.invalidate();
}
document.addEventListener('input', editAccountFormChanged);
document.addEventListener('change', editAccountFormChanged);
function editToggleSmtpAuth(checked) {
  var fields = document.getElementById('edit-smtp-auth-fields');
  var inputs = fields.querySelectorAll('input');
  fields.classList.toggle('opacity-50', checked);
  inputs.forEach(function(inp) { inp.disabled = checked; });
  if (checked) {
    var form = document.getElementById('edit-account-form');
    var u = form.querySelector('input[name="username"]');
    var p = form.querySelector('input[name="password"]');
    var su = form.querySelector('input[name="smtp_username"]');
    var sp = form.querySelector('input[name="smtp_password"]');
    if (su && u) su.value = u.value;
    if (sp && p) sp.value = p.value;
  }
}
