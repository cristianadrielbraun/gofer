package i18n

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"
)

// Dates are written in named styles (Date, ShortDate, Time, …) rather than
// layouts, so one choice of date format and clock changes every date shown.
//
// Two settings shape them, each "auto" by default:
//   - date_format: one of DateFormats(), such as "dd-mm-yyyy". Automatic uses
//     the language's own text style ("Nov 2, 2026", "2 nov 2026",
//     "2026年11月2日").
//   - time_format: "12h" or "24h". Automatic uses the language's clock.
//
// Long dates ("Monday, November 2, 2026") and month titles always follow the
// language, whatever the format: they are headings, not compact dates.
const (
	DateFormatSettingKey = "date_format"
	TimeFormatSettingKey = "time_format"
)

// DateFormat is a way of writing a date with its year.
type DateFormat struct {
	// ID is the saved setting value.
	ID string
	// Order of day, month and year: "dmy", "mdy" or "ymd". Empty means the
	// language's order.
	Order string
	// Numeric writes the month as a number, joined by Separator.
	Numeric   bool
	Separator string
}

var dateFormats = []DateFormat{
	{ID: "auto"},
	{ID: "d-mmm-yyyy", Order: "dmy"},
	{ID: "mmm-d-yyyy", Order: "mdy"},
	{ID: "dd/mm/yyyy", Order: "dmy", Numeric: true, Separator: "/"},
	{ID: "mm/dd/yyyy", Order: "mdy", Numeric: true, Separator: "/"},
	{ID: "dd-mm-yyyy", Order: "dmy", Numeric: true, Separator: "-"},
	{ID: "mm-dd-yyyy", Order: "mdy", Numeric: true, Separator: "-"},
	{ID: "dd.mm.yyyy", Order: "dmy", Numeric: true, Separator: "."},
	{ID: "yyyy-mm-dd", Order: "ymd", Numeric: true, Separator: "-"},
}

// DateFormats lists the date formats a user can choose, Automatic first.
func DateFormats() []DateFormat {
	return append([]DateFormat(nil), dateFormats...)
}

// TimeFormats lists the clock choices a user can make, Automatic first.
func TimeFormats() []string {
	return []string{"auto", "12h", "24h"}
}

func parseDateFormat(id string) DateFormat {
	for _, format := range dateFormats {
		if format.ID == id {
			return format
		}
	}
	return dateFormats[0]
}

func parseClock(id string) string {
	if id == "12h" || id == "24h" {
		return id
	}
	return "auto"
}

type dateContextKey struct{}

type datePrefs struct{ format, clock string }

// WithDatePrefs returns a context whose dates use the given date_format and
// time_format setting values.
func WithDatePrefs(ctx context.Context, dateFormat, timeFormat string) context.Context {
	return context.WithValue(ctx, dateContextKey{}, datePrefs{parseDateFormat(dateFormat).ID, parseClock(timeFormat)})
}

// Dates returns the date formatter for the context's language and settings.
func Dates(ctx context.Context) DateFormatter {
	prefs, _ := ctx.Value(dateContextKey{}).(datePrefs)
	return NewDateFormatter(FromContext(ctx), prefs.format, prefs.clock)
}

// DatePrefs returns the context's date_format and time_format values, for
// pages that pass them on to scripts.
func DatePrefs(ctx context.Context) (dateFormat, timeFormat string) {
	prefs, _ := ctx.Value(dateContextKey{}).(datePrefs)
	return parseDateFormat(prefs.format).ID, parseClock(prefs.clock)
}

// DateFormatter writes dates and times in one language and format. Callers
// convert times to the user's time zone first.
type DateFormatter struct {
	locale Locale
	format DateFormat
	order  string
	clock  string
}

// NewDateFormatter returns a formatter for work outside a request, from
// setting values ("auto" or empty for the language's defaults).
func NewDateFormatter(locale Locale, dateFormat, timeFormat string) DateFormatter {
	f := DateFormatter{locale: locale, format: parseDateFormat(dateFormat), clock: parseClock(timeFormat)}
	f.order = f.format.Order
	if f.order == "" {
		f.order = f.message("date.default_order")
	}
	if f.clock == "auto" {
		f.clock = f.message("date.default_clock")
	}
	return f
}

func (f DateFormatter) message(key string, args ...any) string {
	return Translate(f.locale, key, args...)
}

// MonthName is a month's full name, such as "November".
func (f DateFormatter) MonthName(month time.Month) string {
	return f.message("date.month." + strconv.Itoa(int(month)))
}

// MonthShortName is a month's short name, such as "Nov".
func (f DateFormatter) MonthShortName(month time.Month) string {
	return f.message("date.month_short." + strconv.Itoa(int(month)))
}

// WeekdayName is a weekday's full name, such as "Monday".
func (f DateFormatter) WeekdayName(day time.Weekday) string {
	return f.message("date.weekday." + strconv.Itoa(int(day)))
}

// WeekdayShortName is a weekday's short name, such as "Mon".
func (f DateFormatter) WeekdayShortName(day time.Weekday) string {
	return f.message("date.weekday_short." + strconv.Itoa(int(day)))
}

func twoDigits(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// compact writes the date in the chosen format, with or without its year,
// leaving out the weekday.
func (f DateFormatter) compact(t time.Time, year bool) string {
	day, month, y := t.Day(), t.Month(), strconv.Itoa(t.Year())
	if f.format.Numeric {
		d, m, sep := twoDigits(day), twoDigits(int(month)), f.format.Separator
		switch f.order {
		case "mdy":
			if year {
				return m + sep + d + sep + y
			}
			return m + sep + d
		case "ymd":
			if year {
				return y + sep + m + sep + d
			}
			return m + sep + d
		default:
			if year {
				return d + sep + m + sep + y
			}
			return d + sep + m
		}
	}
	name := f.MonthShortName(month)
	switch f.order {
	case "mdy":
		if year {
			return name + " " + strconv.Itoa(day) + ", " + y
		}
		return name + " " + strconv.Itoa(day)
	case "ymd":
		// Languages that write the year first mark each part their own
		// way, as in 2026年11月2日, so their catalogs give the layout.
		if year {
			return f.message("date.pattern.year_month_day", "year", y, "month", name, "day", day)
		}
		return f.message("date.pattern.month_day", "month", name, "day", day)
	}
	if year {
		return strconv.Itoa(day) + " " + name + " " + y
	}
	return strconv.Itoa(day) + " " + name
}

func (f DateFormatter) withWeekday(t time.Time, date string) string {
	return f.message("date.pattern.weekday_date", "weekday", f.WeekdayShortName(t.Weekday()), "date", date)
}

// Date writes a date with its year: "Nov 2, 2026", "2 nov 2026",
// "2026年11月2日", "02-11-2026".
func (f DateFormatter) Date(t time.Time) string {
	return f.compact(t, true)
}

// ShortDate writes a date without its year: "Nov 2", "2 nov", "11月2日",
// "02-11".
func (f DateFormatter) ShortDate(t time.Time) string {
	return f.compact(t, false)
}

// WeekdayDate writes a date with its weekday and year: "Mon, Nov 2, 2026".
func (f DateFormatter) WeekdayDate(t time.Time) string {
	return f.withWeekday(t, f.compact(t, true))
}

// WeekdayShortDate writes a date with its weekday and no year: "Mon, Nov 2".
func (f DateFormatter) WeekdayShortDate(t time.Time) string {
	return f.withWeekday(t, f.compact(t, false))
}

// LongDate writes a date in full words: "Monday, November 2, 2026".
func (f DateFormatter) LongDate(t time.Time) string {
	return f.message("date.pattern.long", "weekday", f.WeekdayName(t.Weekday()), "month", f.MonthName(t.Month()), "day", t.Day(), "year", t.Year())
}

// LongDateNoYear writes a date in full words without its year: "Monday, November 2".
func (f DateFormatter) LongDateNoYear(t time.Time) string {
	return f.message("date.pattern.long_no_year", "weekday", f.WeekdayName(t.Weekday()), "month", f.MonthName(t.Month()), "day", t.Day())
}

// MonthYear writes a month and its year: "November 2026".
func (f DateFormatter) MonthYear(t time.Time) string {
	return f.message("date.pattern.month_year", "month", f.MonthName(t.Month()), "year", t.Year())
}

// LongMonthDate writes a date with the month's full name and year, without
// the weekday: "November 2, 2026".
func (f DateFormatter) LongMonthDate(t time.Time) string {
	return f.message("date.pattern.long_month_date", "month", f.MonthName(t.Month()), "day", t.Day(), "year", t.Year())
}

// Uses12HourClock reports whether times show AM and PM.
func (f DateFormatter) Uses12HourClock() bool {
	return f.clock == "12h"
}

// Time writes a time of day on the chosen clock: "3:04 PM" or "15:04".
func (f DateFormatter) Time(t time.Time) string {
	return f.ClockTime(t.Hour(), t.Minute())
}

// ClockTime writes a wall-clock time from its hour and minute. Hour 24, the
// end of a day, reads "24:00" or "12:00 AM". The language puts AM or PM
// before or after the time: "3:04 PM", "下午3:04".
func (f DateFormatter) ClockTime(hour, minute int) string {
	if !f.Uses12HourClock() {
		return twoDigits(hour) + ":" + twoDigits(minute)
	}
	return f.message("date.pattern.time_12h", "time", f.twelveHour(hour)+":"+twoDigits(minute), "marker", f.marker(hour))
}

// HourLabel writes a whole hour for a time axis: "3 PM", "下午3点" or
// "15:00".
func (f DateFormatter) HourLabel(hour int) string {
	if !f.Uses12HourClock() {
		return twoDigits(hour) + ":00"
	}
	return f.message("date.pattern.hour_12h", "hour", f.twelveHour(hour), "marker", f.marker(hour))
}

func (f DateFormatter) twelveHour(hour int) string {
	if h := hour % 12; h != 0 {
		return strconv.Itoa(h)
	}
	return "12"
}

func (f DateFormatter) marker(hour int) string {
	if hour%24 >= 12 {
		return f.message("date.pm")
	}
	return f.message("date.am")
}

// TimeRange writes two times of the same day: "15:00–16:30".
func (f DateFormatter) TimeRange(start, end time.Time) string {
	return f.message("date.pattern.time_range", "start", f.Time(start), "end", f.Time(end))
}

// DateTime joins a date and a time: "Nov 2, 2026, 3:04 PM".
func (f DateFormatter) DateTime(t time.Time) string {
	return f.JoinDateTime(f.Date(t), t)
}

// ShortDateTime joins a date without its year and a time: "Nov 2, 3:04 PM".
func (f DateFormatter) ShortDateTime(t time.Time) string {
	return f.JoinDateTime(f.ShortDate(t), t)
}

// WeekdayDateTime writes a date with weekday and year, and its time.
func (f DateFormatter) WeekdayDateTime(t time.Time) string {
	return f.JoinDateTime(f.WeekdayDate(t), t)
}

// JoinDateTime puts an already written date and t's time together.
func (f DateFormatter) JoinDateTime(date string, t time.Time) string {
	return f.message("date.pattern.date_time", "date", date, "time", f.Time(t))
}

// Range joins two already written dates or times: "Nov 2 – Nov 9, 2026".
func (f DateFormatter) Range(start, end string) string {
	return f.message("date.pattern.range", "start", start, "end", end)
}

// DateRange writes the days from start to end, leaving out what they share:
// "Nov 2 – 9, 2026" style text depends on the format and language.
func (f DateFormatter) DateRange(start, end time.Time) string {
	if start.Year() != end.Year() {
		return f.Range(f.Date(start), f.Date(end))
	}
	if start.Month() != end.Month() || f.format.Numeric {
		// Year-first formats write the shared year on the first date.
		if f.order == "ymd" {
			return f.Range(f.Date(start), f.ShortDate(end))
		}
		return f.Range(f.ShortDate(start), f.Date(end))
	}
	month, year := f.MonthShortName(start.Month()), strconv.Itoa(start.Year())
	days := f.message("date.pattern.day_range", "start", start.Day(), "end", end.Day())
	switch f.order {
	case "mdy":
		return month + " " + days + ", " + year
	case "ymd":
		return f.message("date.pattern.year_month_day", "year", year, "month", month, "day", days)
	}
	return days + " " + month + " " + year
}

// Relative writes a list date: the time today, "Yesterday", the date
// without its year this year, and the date with its year before that.
func (f DateFormatter) Relative(t, now time.Time) string {
	switch {
	case sameDay(t, now):
		return f.Time(t)
	case sameDay(t, now.AddDate(0, 0, -1)):
		return f.message("date.yesterday")
	case t.Year() == now.Year():
		return f.ShortDate(t)
	default:
		return f.Date(t)
	}
}

// RelativeWithTime is Relative with the time added to days before today.
func (f DateFormatter) RelativeWithTime(t, now time.Time) string {
	date := f.Relative(t, now)
	if sameDay(t, now) {
		return date
	}
	return f.JoinDateTime(date, t)
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// Number writes a whole number with the language's digit grouping:
// "12,345" or "12.345".
func Number(ctx context.Context, n int) string {
	return FormatNumber(FromContext(ctx), int64(n))
}

// FormatNumber is Number for a known locale.
func FormatNumber(locale Locale, n int64) string {
	digits := strconv.FormatInt(n, 10)
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	minimum, _ := strconv.Atoi(Translate(locale, "number.min_grouping_digits"))
	if len(digits) < 4 || len(digits) < 3+max(minimum, 1) {
		return sign + digits
	}
	group := Translate(locale, "number.group")
	var out strings.Builder
	for i, digit := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out.WriteString(group)
		}
		out.WriteRune(digit)
	}
	return sign + out.String()
}

// Decimal writes a number with one decimal place at most, using the
// language's decimal mark: "1.5" or "1,5".
func Decimal(ctx context.Context, value float64) string {
	text := strconv.FormatFloat(value, 'f', 1, 64)
	text = strings.TrimSuffix(text, ".0")
	return strings.Replace(text, ".", Translate(FromContext(ctx), "number.decimal"), 1)
}

// Bytes writes a size in decimal units, as go-humanize does: "1.2 MB",
// "1,2 MB".
func Bytes(ctx context.Context, size uint64) string {
	if size < 10 {
		return strconv.FormatUint(size, 10) + " B"
	}
	units := []string{"B", "kB", "MB", "GB", "TB", "PB", "EB"}
	value := float64(size)
	unit := 0
	for value >= 1000 && unit < len(units)-1 {
		value /= 1000
		unit++
	}
	// Round to a tenth first, as go-humanize does, so 9.96 kB shows as 10.
	value = math.Floor(value*10+0.5) / 10
	if unit == 0 {
		return strconv.FormatUint(size, 10) + " B"
	}
	text := strconv.FormatFloat(value, 'f', 0, 64)
	if value < 10 {
		text = strconv.FormatFloat(value, 'f', 1, 64)
	}
	return strings.Replace(text, ".", Translate(FromContext(ctx), "number.decimal"), 1) + " " + units[unit]
}
