package i18n

import (
	"context"
	"testing"
	"time"
)

var (
	sample    = time.Date(2026, time.November, 2, 15, 4, 0, 0, time.UTC) // a Monday
	sampleAM  = time.Date(2026, time.November, 2, 0, 5, 0, 0, time.UTC)
	sampleEnd = time.Date(2026, time.November, 9, 16, 30, 0, 0, time.UTC)
)

func TestDatesFollowTheLanguageByDefault(t *testing.T) {
	en := NewDateFormatter(English, "", "")
	es := NewDateFormatter("es", "auto", "auto")
	cases := []struct{ name, got, want string }{
		{"en Date", en.Date(sample), "Nov 2, 2026"},
		{"en ShortDate", en.ShortDate(sample), "Nov 2"},
		{"en WeekdayDate", en.WeekdayDate(sample), "Mon, Nov 2, 2026"},
		{"en WeekdayShortDate", en.WeekdayShortDate(sample), "Mon, Nov 2"},
		{"en LongDate", en.LongDate(sample), "Monday, November 2, 2026"},
		{"en LongDateNoYear", en.LongDateNoYear(sample), "Monday, November 2"},
		{"en LongMonthDate", en.LongMonthDate(sample), "November 2, 2026"},
		{"en MonthYear", en.MonthYear(sample), "November 2026"},
		{"en Time", en.Time(sample), "3:04 PM"},
		{"en Time midnight", en.Time(sampleAM), "12:05 AM"},
		{"en DateTime", en.DateTime(sample), "Nov 2, 2026, 3:04 PM"},
		{"en TimeRange", en.TimeRange(sample, sampleEnd), "3:04 PM–4:30 PM"},
		{"en DateRange", en.DateRange(sample, sampleEnd), "Nov 2–9, 2026"},
		{"es Date", es.Date(sample), "2 nov 2026"},
		{"es ShortDate", es.ShortDate(sample), "2 nov"},
		{"es WeekdayDate", es.WeekdayDate(sample), "lun, 2 nov 2026"},
		{"es LongDate", es.LongDate(sample), "lunes, 2 de noviembre de 2026"},
		{"es LongMonthDate", es.LongMonthDate(sample), "2 de noviembre de 2026"},
		{"es MonthYear", es.MonthYear(sample), "noviembre de 2026"},
		{"es Time", es.Time(sample), "15:04"},
		{"es Time midnight", es.Time(sampleAM), "00:05"},
		{"es DateTime", es.DateTime(sample), "2 nov 2026, 15:04"},
		{"es DateRange", es.DateRange(sample, sampleEnd), "2–9 nov 2026"},
		{"en ClockTime", en.ClockTime(24, 0), "12:00 AM"},
		{"es ClockTime", es.ClockTime(24, 0), "24:00"},
		{"en HourLabel", en.HourLabel(0), "12 AM"},
		{"en HourLabel noon", en.HourLabel(12), "12 PM"},
		{"es HourLabel", es.HourLabel(7), "07:00"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestChineseDatesPutTheYearFirst(t *testing.T) {
	zh := NewDateFormatter("zh-cn", "", "")
	twelve := NewDateFormatter("zh-cn", "", "12h")
	monthEnd := time.Date(2026, time.November, 30, 9, 0, 0, 0, time.UTC)
	nextMonth := time.Date(2026, time.December, 6, 9, 0, 0, 0, time.UTC)
	cases := []struct{ name, got, want string }{
		{"Date", zh.Date(sample), "2026年11月2日"},
		{"ShortDate", zh.ShortDate(sample), "11月2日"},
		{"WeekdayDate", zh.WeekdayDate(sample), "2026年11月2日 周一"},
		{"WeekdayShortDate", zh.WeekdayShortDate(sample), "11月2日 周一"},
		{"LongDate", zh.LongDate(sample), "2026年11月2日星期一"},
		{"LongDateNoYear", zh.LongDateNoYear(sample), "11月2日星期一"},
		{"LongMonthDate", zh.LongMonthDate(sample), "2026年11月2日"},
		{"MonthYear", zh.MonthYear(sample), "2026年11月"},
		{"Time", zh.Time(sample), "15:04"},
		{"DateTime", zh.DateTime(sample), "2026年11月2日 15:04"},
		{"DateRange", zh.DateRange(sample, sampleEnd), "2026年11月2–9日"},
		{"DateRange across months", zh.DateRange(monthEnd, nextMonth), "2026年11月30日 – 12月6日"},
		{"numeric DateRange across months", NewDateFormatter("zh-cn", "yyyy-mm-dd", "").DateRange(monthEnd, nextMonth), "2026-11-30 – 12-06"},
		{"12h Time", twelve.Time(sample), "下午3:04"},
		{"12h Time midnight", twelve.Time(sampleAM), "上午12:05"},
		{"12h ClockTime", twelve.ClockTime(24, 0), "上午12:00"},
		{"12h HourLabel", twelve.HourLabel(15), "下午3点"},
		{"24h HourLabel", zh.HourLabel(7), "07:00"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("zh-cn %s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestDateFormatsOverrideTheLanguage(t *testing.T) {
	cases := []struct {
		locale              Locale
		format, clock       string
		date, short, dtTime string
	}{
		{English, "dd-mm-yyyy", "24h", "02-11-2026", "02-11", "02-11-2026, 15:04"},
		{English, "mm-dd-yyyy", "", "11-02-2026", "11-02", "11-02-2026, 3:04 PM"},
		{English, "dd/mm/yyyy", "", "02/11/2026", "02/11", "02/11/2026, 3:04 PM"},
		{English, "mm/dd/yyyy", "", "11/02/2026", "11/02", "11/02/2026, 3:04 PM"},
		{English, "dd.mm.yyyy", "", "02.11.2026", "02.11", "02.11.2026, 3:04 PM"},
		{English, "yyyy-mm-dd", "", "2026-11-02", "11-02", "2026-11-02, 3:04 PM"},
		{English, "d-mmm-yyyy", "", "2 Nov 2026", "2 Nov", "2 Nov 2026, 3:04 PM"},
		{"es", "mmm-d-yyyy", "12h", "nov 2, 2026", "nov 2", "nov 2, 2026, 3:04 p. m."},
		{"es", "unknown", "nonsense", "2 nov 2026", "2 nov", "2 nov 2026, 15:04"},
	}
	for _, c := range cases {
		f := NewDateFormatter(c.locale, c.format, c.clock)
		if got := f.Date(sample); got != c.date {
			t.Errorf("%s %s Date = %q, want %q", c.locale, c.format, got, c.date)
		}
		if got := f.ShortDate(sample); got != c.short {
			t.Errorf("%s %s ShortDate = %q, want %q", c.locale, c.format, got, c.short)
		}
		if got := f.DateTime(sample); got != c.dtTime {
			t.Errorf("%s %s DateTime = %q, want %q", c.locale, c.format, got, c.dtTime)
		}
		// Headings keep the language's words whatever the format.
		if c.locale == "es" && f.LongDate(sample) != "lunes, 2 de noviembre de 2026" {
			t.Errorf("%s %s LongDate = %q", c.locale, c.format, f.LongDate(sample))
		}
	}
}

func TestRelativeDates(t *testing.T) {
	now := time.Date(2026, time.November, 2, 18, 0, 0, 0, time.UTC)
	en := NewDateFormatter(English, "", "")
	es := NewDateFormatter("es", "", "")
	zh := NewDateFormatter("zh-cn", "", "")
	numeric := NewDateFormatter(English, "dd-mm-yyyy", "24h")
	yesterday := sample.AddDate(0, 0, -1)
	earlier := time.Date(2026, time.March, 5, 9, 0, 0, 0, time.UTC)
	lastYear := time.Date(2025, time.December, 31, 9, 0, 0, 0, time.UTC)
	cases := []struct{ got, want string }{
		{en.Relative(sample, now), "3:04 PM"},
		{en.Relative(yesterday, now), "Yesterday"},
		{en.Relative(earlier, now), "Mar 5"},
		{en.Relative(lastYear, now), "Dec 31, 2025"},
		{en.RelativeWithTime(sample, now), "3:04 PM"},
		{en.RelativeWithTime(yesterday, now), "Yesterday, 3:04 PM"},
		{es.Relative(yesterday, now), "Ayer"},
		{es.RelativeWithTime(earlier, now), "5 mar, 09:00"},
		{zh.Relative(yesterday, now), "昨天"},
		{zh.Relative(earlier, now), "3月5日"},
		{zh.RelativeWithTime(earlier, now), "3月5日 09:00"},
		{zh.Relative(lastYear, now), "2025年12月31日"},
		{numeric.Relative(earlier, now), "05-03"},
		{numeric.Relative(lastYear, now), "31-12-2025"},
	}
	for i, c := range cases {
		if c.got != c.want {
			t.Errorf("case %d = %q, want %q", i, c.got, c.want)
		}
	}
}

func TestDatesReadTheContext(t *testing.T) {
	ctx := WithDatePrefs(WithLocale(context.Background(), "es"), "yyyy-mm-dd", "12h")
	if got := Dates(ctx).DateTime(sample); got != "2026-11-02, 3:04 p. m." {
		t.Fatalf("DateTime = %q", got)
	}
	if format, clock := DatePrefs(ctx); format != "yyyy-mm-dd" || clock != "12h" {
		t.Fatalf("DatePrefs = %q %q", format, clock)
	}
	if format, clock := DatePrefs(context.Background()); format != "auto" || clock != "auto" {
		t.Fatalf("default DatePrefs = %q %q", format, clock)
	}
}

func TestNumbers(t *testing.T) {
	en := WithLocale(context.Background(), English)
	es := WithLocale(context.Background(), "es")
	zh := WithLocale(context.Background(), "zh-cn")
	cases := []struct{ got, want string }{
		{Number(en, 999), "999"},
		{Number(en, 1234), "1,234"},
		{Number(en, -1234567), "-1,234,567"},
		{Number(es, 1234), "1234"},
		{Number(es, 12345), "12.345"},
		{Number(es, 1234567), "1.234.567"},
		{Decimal(en, 1.25), "1.2"},
		{Decimal(es, 1.5), "1,5"},
		{Decimal(es, 2), "2"},
		{Bytes(en, 5), "5 B"},
		{Bytes(en, 999), "999 B"},
		{Bytes(en, 1500), "1.5 kB"},
		{Bytes(es, 1500), "1,5 kB"},
		{Bytes(en, 9960), "10 kB"},
		{Bytes(es, 23_400_000), "23 MB"},
		{Number(zh, 1234), "1,234"},
		{Decimal(zh, 1.5), "1.5"},
		{Bytes(zh, 1500), "1.5 kB"},
	}
	for i, c := range cases {
		if c.got != c.want {
			t.Errorf("case %d = %q, want %q", i, c.got, c.want)
		}
	}
}
