// Translations for scripts. The server's catalog script (/assets/i18n/<lang>.js)
// loads first and sets window.GOFER_I18N to the page language's "js." messages
// and date words, with English filled in for any the language lacks.
//
//   goferT("js.mail.moved", { folder: "Archive" })
//   goferTn("js.mail.selected", 3)   // picks js.mail.selected.one or .other
//   goferDates.date(new Date())      // "Nov 2, 2026", "2 nov 2026", "2026年11月2日", "02-11-2026"
//
// goferDates writes dates exactly as the server's i18n.DateFormatter does,
// using the user's date_format and time_format from this script's tag.
(function () {
  var script = document.currentScript
  var catalog = window.GOFER_I18N || { locale: "en", messages: {} }

  function fill(message, params) {
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
    return fill(message == null ? key : message, params)
  }

  window.goferTn = function (key, count, params) {
    var shown = count
    try { shown = new Intl.NumberFormat(window.goferLocale).format(count) } catch (_) {}
    var values = Object.assign({ count: shown }, params || {})
    return window.goferT(key + "." + pluralForm(count), values)
  }

  // Mirrors internal/i18n/dates.go; keep the two in step.
  var formats = {
    "auto": {},
    "d-mmm-yyyy": { order: "dmy" },
    "mmm-d-yyyy": { order: "mdy" },
    "dd/mm/yyyy": { order: "dmy", numeric: true, separator: "/" },
    "mm/dd/yyyy": { order: "mdy", numeric: true, separator: "/" },
    "dd-mm-yyyy": { order: "dmy", numeric: true, separator: "-" },
    "mm-dd-yyyy": { order: "mdy", numeric: true, separator: "-" },
    "dd.mm.yyyy": { order: "dmy", numeric: true, separator: "." },
    "yyyy-mm-dd": { order: "ymd", numeric: true, separator: "-" },
  }
  var format = formats[(script && script.dataset.dateFormat) || "auto"] || formats.auto
  var order = format.order || window.goferT("date.default_order")
  var clock = (script && script.dataset.timeFormat) || "auto"
  if (clock !== "12h" && clock !== "24h") clock = window.goferT("date.default_clock")

  var weekdayIndex = { Sun: 0, Mon: 1, Tue: 2, Wed: 3, Thu: 4, Fri: 5, Sat: 6 }

  // The calendar parts of a moment, in timeZone when given, else the
  // browser's own zone.
  function parts(date, timeZone) {
    if (!(date instanceof Date)) date = new Date(date)
    if (timeZone) {
      try {
        var values = {}
        new Intl.DateTimeFormat("en-US", {
          timeZone: timeZone, year: "numeric", month: "numeric", day: "numeric",
          hour: "numeric", minute: "numeric", weekday: "short", hourCycle: "h23",
        }).formatToParts(date).forEach(function (part) { values[part.type] = part.value })
        return {
          year: Number(values.year), month: Number(values.month), day: Number(values.day),
          hour: Number(values.hour) % 24, minute: Number(values.minute), weekday: weekdayIndex[values.weekday],
        }
      } catch (_) {}
    }
    return {
      year: date.getFullYear(), month: date.getMonth() + 1, day: date.getDate(),
      hour: date.getHours(), minute: date.getMinutes(), weekday: date.getDay(),
    }
  }

  function two(n) { return n < 10 ? "0" + n : String(n) }
  function t(key, params) { return window.goferT(key, params) }
  function monthName(p) { return t("date.month." + p.month) }
  function monthShort(p) { return t("date.month_short." + p.month) }
  function weekdayName(p) { return t("date.weekday." + p.weekday) }
  function weekdayShort(p) { return t("date.weekday_short." + p.weekday) }

  function compact(p, withYear) {
    if (format.numeric) {
      var d = two(p.day), m = two(p.month), sep = format.separator
      if (order === "mdy") return withYear ? m + sep + d + sep + p.year : m + sep + d
      if (order === "ymd") return withYear ? p.year + sep + m + sep + d : m + sep + d
      return withYear ? d + sep + m + sep + p.year : d + sep + m
    }
    if (order === "mdy") return monthShort(p) + " " + p.day + (withYear ? ", " + p.year : "")
    if (order === "ymd") {
      return withYear
        ? t("date.pattern.year_month_day", { year: p.year, month: monthShort(p), day: p.day })
        : t("date.pattern.month_day", { month: monthShort(p), day: p.day })
    }
    return p.day + " " + monthShort(p) + (withYear ? " " + p.year : "")
  }

  function withWeekday(p, text) {
    return t("date.pattern.weekday_date", { weekday: weekdayShort(p), date: text })
  }

  function marker(hour) { return t(hour % 24 >= 12 ? "date.pm" : "date.am") }

  function clockTime(hour, minute) {
    if (clock !== "12h") return two(hour) + ":" + two(minute)
    return t("date.pattern.time_12h", { time: (hour % 12 || 12) + ":" + two(minute), marker: marker(hour) })
  }

  function hourLabel(hour) {
    if (clock !== "12h") return two(hour) + ":00"
    return t("date.pattern.hour_12h", { hour: hour % 12 || 12, marker: marker(hour) })
  }

  function time(p) { return clockTime(p.hour, p.minute) }

  function joinDateTime(text, p) {
    return t("date.pattern.date_time", { date: text, time: time(p) })
  }

  function sameDay(a, b) { return a.year === b.year && a.month === b.month && a.day === b.day }

  // Each takes a Date (or anything new Date accepts) and an optional IANA
  // time zone.
  window.goferDates = {
    uses12HourClock: function () { return clock === "12h" },
    // A wall-clock time from its hour (0–24) and minute: "3:04 PM", "15:04".
    clockTime: clockTime,
    // A whole hour for a time axis: "3 PM", "15:00".
    hourLabel: hourLabel,
    monthName: function (month) { return t("date.month." + month) },
    monthShortName: function (month) { return t("date.month_short." + month) },
    weekdayName: function (weekday) { return t("date.weekday." + weekday) },
    weekdayShortName: function (weekday) { return t("date.weekday_short." + weekday) },
    date: function (date, zone) { var p = parts(date, zone); return compact(p, true) },
    shortDate: function (date, zone) { var p = parts(date, zone); return compact(p, false) },
    weekdayDate: function (date, zone) { var p = parts(date, zone); return withWeekday(p, compact(p, true)) },
    weekdayShortDate: function (date, zone) { var p = parts(date, zone); return withWeekday(p, compact(p, false)) },
    longDate: function (date, zone) {
      var p = parts(date, zone)
      return t("date.pattern.long", { weekday: weekdayName(p), month: monthName(p), day: p.day, year: p.year })
    },
    longDateNoYear: function (date, zone) {
      var p = parts(date, zone)
      return t("date.pattern.long_no_year", { weekday: weekdayName(p), month: monthName(p), day: p.day })
    },
    longMonthDate: function (date, zone) {
      var p = parts(date, zone)
      return t("date.pattern.long_month_date", { month: monthName(p), day: p.day, year: p.year })
    },
    monthYear: function (date, zone) {
      var p = parts(date, zone)
      return t("date.pattern.month_year", { month: monthName(p), year: p.year })
    },
    time: function (date, zone) { return time(parts(date, zone)) },
    timeRange: function (start, end, zone) {
      return t("date.pattern.time_range", { start: time(parts(start, zone)), end: time(parts(end, zone)) })
    },
    dateTime: function (date, zone) { var p = parts(date, zone); return joinDateTime(window.goferDates.date(date, zone), p) },
    shortDateTime: function (date, zone) { var p = parts(date, zone); return joinDateTime(window.goferDates.shortDate(date, zone), p) },
    weekdayDateTime: function (date, zone) { var p = parts(date, zone); return joinDateTime(withWeekday(p, compact(p, true)), p) },
    range: function (start, end) { return t("date.pattern.range", { start: start, end: end }) },
    // The days from start to end, leaving out what they share, as
    // DateFormatter.DateRange does: "Nov 2–9, 2026", "2–9 nov 2026".
    dateRange: function (start, end, zone) {
      var a = parts(start, zone), b = parts(end, zone), d = window.goferDates
      if (a.year !== b.year) return d.range(d.date(start, zone), d.date(end, zone))
      if (a.month !== b.month || format.numeric) {
        if (order === "ymd") return d.range(d.date(start, zone), d.shortDate(end, zone))
        return d.range(d.shortDate(start, zone), d.date(end, zone))
      }
      var days = t("date.pattern.day_range", { start: a.day, end: b.day })
      if (order === "mdy") return monthShort(a) + " " + days + ", " + a.year
      if (order === "ymd") return t("date.pattern.year_month_day", { year: a.year, month: monthShort(a), day: days })
      return days + " " + monthShort(a) + " " + a.year
    },
    // A run of whole hours on a condensed time axis: "03–07", "3–7 AM", "上午3–7点".
    hourRange: function (start, end) {
      if (clock !== "12h") return t("date.pattern.day_range", { start: two(start), end: two(end) })
      if (marker(start) === marker(end)) {
        var hours = t("date.pattern.day_range", { start: start % 12 || 12, end: end % 12 || 12 })
        return t("date.pattern.hour_12h", { hour: hours, marker: marker(end) })
      }
      return t("date.pattern.time_range", { start: hourLabel(start), end: hourLabel(end) })
    },
    relative: function (date, now, zone) {
      var p = parts(date, zone), today = parts(now || new Date(), zone)
      var yesterday = parts(new Date((now ? new Date(now) : new Date()).getTime() - 864e5), zone)
      if (sameDay(p, today)) return time(p)
      if (sameDay(p, yesterday)) return t("date.yesterday")
      if (p.year === today.year) return window.goferDates.shortDate(date, zone)
      return window.goferDates.date(date, zone)
    },
  }

  // Whole numbers with the language's digit grouping: "12,345", "12.345".
  window.goferNumber = function (n) {
    try { return new Intl.NumberFormat(window.goferLocale).format(n) } catch (_) { return String(n) }
  }
})()
