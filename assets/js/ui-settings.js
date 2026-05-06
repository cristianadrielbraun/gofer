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
      if (panel && value) {
        var w = parseInt(value, 10);
        if (!isNaN(w) && w > 0) panel.style.width = w + "px";
      }
    }
  }

  GoferSettings = {
    init: function () {
      var bodySettings = document.body ? document.body.dataset.uiSettings : null;
      if (bodySettings) {
        try {
          _cache = JSON.parse(bodySettings);
        } catch (_) {}
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
            writeCache();
            for (var k in _cache) {
              applySetting(k, _cache[k]);
            }
          })
          .catch(function () {});
      }
      writeCache();
    },

    get: function (key) {
      return _cache[key] || null;
    },

    set: function (key, value) {
      _cache[key] = value;
      writeCache();
      applySetting(key, value);

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
