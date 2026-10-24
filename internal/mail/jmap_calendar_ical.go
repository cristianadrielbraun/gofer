package mail

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/emersion/go-ical"
)

// jscalendarLocal is JSCalendar's LocalDateTime: a wall-clock time whose
// zone is the event's timeZone.
const jscalendarLocal = "2006-01-02T15:04:05"

// JMAPEventICS writes a JMAP event as an iCalendar file, the form Gofer's
// CalDAV code reads and edits.
func JMAPEventICS(event jmap.CalendarEvent) ([]byte, error) {
	cal, err := JMAPEventCalendar(event)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := ical.NewEncoder(&out).Encode(cal); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// JMAPEventCalendar turns a JMAP event (JSCalendar) into an iCalendar
// calendar: one VEVENT for the event, and one more for each occurrence of a
// series that was changed. Both the RFC 8984 names and the newer ones
// servers such as Stalwart use are read. iCalendar properties the server
// kept for the event come back as they were.
func JMAPEventCalendar(event jmap.CalendarEvent) (*ical.Calendar, error) {
	if kind, _ := event["@type"].(string); kind != "" && kind != "Event" {
		return nil, fmt.Errorf("JMAP calendar entry is a %s, not an event", kind)
	}
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, "-//Gofer//JMAP Calendars//EN")
	cal.Props.SetText(ical.PropVersion, "2.0")
	master, err := jscalendarVEvent(event, "", "")
	if err != nil {
		return nil, err
	}
	cal.Children = append(cal.Children, master)

	overrides, _ := event["recurrenceOverrides"].(map[string]any)
	if len(overrides) == 0 {
		return cal, nil
	}
	if !jscalendarRecurs(event) {
		return nil, errors.New("JMAP event changes occurrences of a series it doesn't have")
	}
	zone := jscalendarZone(event)
	allDay := jscalendarBool(event, "showWithoutTime")
	location := jscalendarLocation(zone)
	set, err := master.RecurrenceSet(location)
	if err != nil {
		return nil, fmt.Errorf("JMAP event repeat rule: %w", err)
	}
	for _, key := range slices.Sorted(maps.Keys(overrides)) {
		at, err := time.ParseInLocation(jscalendarLocal, key, location)
		if err != nil {
			return nil, fmt.Errorf("JMAP event occurrence %q: %w", key, err)
		}
		patch, _ := overrides[key].(map[string]any)
		if jscalendarBool(patch, "excluded") {
			master.Props.Add(jscalendarDateProp("EXDATE", key, zone, allDay))
			continue
		}
		// An occurrence the rule doesn't make is an extra date.
		if set == nil || len(set.Between(at, at, true)) == 0 {
			master.Props.Add(jscalendarDateProp("RDATE", key, zone, allDay))
		}
		occurrence, err := jscalendarApplyPatch(event, patch)
		if err != nil {
			return nil, fmt.Errorf("JMAP event occurrence %q: %w", key, err)
		}
		if _, moved := occurrence["start"]; !moved || patch["start"] == nil {
			occurrence["start"] = key
		}
		child, err := jscalendarVEvent(occurrence, key, zone)
		if err != nil {
			return nil, fmt.Errorf("JMAP event occurrence %q: %w", key, err)
		}
		cal.Children = append(cal.Children, child)
	}
	return cal, nil
}

// jscalendarVEvent writes one VEVENT. recurrenceID is set for a changed
// occurrence of a series, which then carries no repeat rule; it is a time
// in the series' zone, recurrenceZone, whatever zone the occurrence moved to.
func jscalendarVEvent(event map[string]any, recurrenceID, recurrenceZone string) (*ical.Component, error) {
	vevent := ical.NewComponent(ical.CompEvent)
	props := vevent.Props
	uid := jscalendarString(event, "uid")
	if uid == "" {
		return nil, errors.New("JMAP event has no uid")
	}
	props.SetText(ical.PropUID, uid)
	stamp := time.Now().UTC()
	if updated, err := time.Parse(time.RFC3339, jscalendarString(event, "updated")); err == nil {
		stamp = updated.UTC()
		props.SetDateTime(ical.PropLastModified, stamp)
	}
	props.SetDateTime(ical.PropDateTimeStamp, stamp)
	if created, err := time.Parse(time.RFC3339, jscalendarString(event, "created")); err == nil {
		props.SetDateTime(ical.PropCreated, created.UTC())
	}
	if sequence, ok := event["sequence"].(float64); ok && sequence > 0 {
		props.SetText(ical.PropSequence, strconv.Itoa(int(sequence)))
	}

	zone := jscalendarZone(event)
	allDay := jscalendarBool(event, "showWithoutTime")
	start := jscalendarString(event, "start")
	if start == "" {
		return nil, errors.New("JMAP event has no start")
	}
	location := jscalendarLocation(zone)
	startAt, err := time.ParseInLocation(jscalendarLocal, start, location)
	if err != nil {
		return nil, fmt.Errorf("JMAP event start %q: %w", start, err)
	}
	if recurrenceID != "" {
		props.Set(jscalendarDateProp(ical.PropRecurrenceID, recurrenceID, recurrenceZone, allDay))
	}
	props.Set(jscalendarDateProp(ical.PropDateTimeStart, start, zone, allDay))
	days, elapsed, err := jscalendarDuration(jscalendarString(event, "duration"))
	if err != nil {
		return nil, fmt.Errorf("JMAP event duration: %w", err)
	}
	if allDay && days == 0 && elapsed == 0 {
		days = 1
	}
	if days != 0 || elapsed != 0 {
		end := startAt.AddDate(0, 0, days).Add(elapsed)
		props.Set(jscalendarDateProp(ical.PropDateTimeEnd, end.Format(jscalendarLocal), zone, allDay))
	}

	if title := jscalendarString(event, "title"); title != "" {
		props.SetText(ical.PropSummary, title)
	}
	if description := jscalendarString(event, "description"); description != "" {
		if strings.EqualFold(jscalendarString(event, "descriptionContentType"), "text/html") {
			alternate := ical.NewProp("X-ALT-DESC")
			alternate.Params.Set("FMTTYPE", "text/html")
			alternate.SetText(description)
			props.Set(alternate)
		}
		props.SetText(ical.PropDescription, description)
	}
	if place := jscalendarLocationName(event); place != "" {
		props.SetText(ical.PropLocation, place)
	}
	switch status := strings.ToUpper(jscalendarString(event, "status")); status {
	case "CONFIRMED", "TENTATIVE", "CANCELLED":
		props.SetText(ical.PropStatus, status)
	}
	switch jscalendarString(event, "freeBusyStatus") {
	case "free":
		props.SetText(ical.PropTransparency, "TRANSPARENT")
	case "busy":
		props.SetText(ical.PropTransparency, "OPAQUE")
	}
	switch jscalendarString(event, "privacy") {
	case "private":
		props.SetText(ical.PropClass, "PRIVATE")
	case "secret":
		props.SetText(ical.PropClass, "CONFIDENTIAL")
	}
	if priority, ok := event["priority"].(float64); ok && priority > 0 {
		props.SetText(ical.PropPriority, strconv.Itoa(int(priority)))
	}
	if keywords, _ := event["keywords"].(map[string]any); len(keywords) > 0 {
		categories := ical.NewProp(ical.PropCategories)
		categories.SetTextList(slices.Sorted(maps.Keys(keywords)))
		props.Set(categories)
	}
	if color := jscalendarString(event, "color"); color != "" {
		props.SetText(ical.PropColor, color)
	}
	jscalendarParticipants(props, event)

	if recurrenceID == "" {
		for _, rule := range jscalendarRules(event, "recurrenceRule", "recurrenceRules") {
			value, err := jscalendarRRule(rule, zone, allDay)
			if err != nil {
				return nil, err
			}
			prop := ical.NewProp(ical.PropRecurrenceRule)
			prop.Value = value
			props.Add(prop)
		}
		for _, rule := range jscalendarRules(event, "excludedRecurrenceRule", "excludedRecurrenceRules") {
			value, err := jscalendarRRule(rule, zone, allDay)
			if err != nil {
				return nil, err
			}
			prop := ical.NewProp("EXRULE")
			prop.Value = value
			props.Add(prop)
		}
	}
	alarms, err := jscalendarAlarms(event)
	if err != nil {
		return nil, err
	}
	vevent.Children = append(vevent.Children, alarms...)
	jscalendarKeptProperties(props, event)
	return vevent, nil
}

// jscalendarParticipants writes the organizer and attendees. A participant
// is an attendee unless its roles leave that out, as the organizer's may.
func jscalendarParticipants(props ical.Props, event map[string]any) {
	participants, _ := event["participants"].(map[string]any)
	organizer := jscalendarString(event, "organizerCalendarAddress")
	if organizer == "" {
		if replyTo, _ := event["replyTo"].(map[string]any); replyTo != nil {
			organizer = jscalendarString(replyTo, "imip")
		}
	}
	var organizerName string
	for _, id := range slices.Sorted(maps.Keys(participants)) {
		participant, _ := participants[id].(map[string]any)
		address := jscalendarParticipantAddress(participant)
		roles, _ := participant["roles"].(map[string]any)
		if address != "" && (strings.EqualFold(address, organizer) || (organizer == "" && jscalendarBool(roles, "owner"))) {
			organizer = address
			organizerName = jscalendarString(participant, "name")
		}
	}
	if organizer != "" {
		prop := ical.NewProp(ical.PropOrganizer)
		prop.Value = organizer
		if organizerName != "" {
			prop.Params.Set(ical.ParamCommonName, organizerName)
		}
		props.Set(prop)
	}
	for _, id := range slices.Sorted(maps.Keys(participants)) {
		participant, _ := participants[id].(map[string]any)
		address := jscalendarParticipantAddress(participant)
		roles, _ := participant["roles"].(map[string]any)
		attends := len(roles) == 0
		for _, role := range []string{"attendee", "required", "optional", "informational", "chair"} {
			attends = attends || jscalendarBool(roles, role)
		}
		if address == "" || !attends {
			continue
		}
		prop := ical.NewProp(ical.PropAttendee)
		prop.Value = address
		if name := jscalendarString(participant, "name"); name != "" {
			prop.Params.Set(ical.ParamCommonName, name)
		}
		if status := jscalendarString(participant, "participationStatus"); status != "" {
			prop.Params.Set(ical.ParamParticipationStatus, strings.ToUpper(status))
		}
		switch {
		case jscalendarBool(roles, "chair"):
			prop.Params.Set(ical.ParamRole, "CHAIR")
		case jscalendarBool(roles, "optional"):
			prop.Params.Set(ical.ParamRole, "OPT-PARTICIPANT")
		case jscalendarBool(roles, "informational"):
			prop.Params.Set(ical.ParamRole, "NON-PARTICIPANT")
		default:
			prop.Params.Set(ical.ParamRole, "REQ-PARTICIPANT")
		}
		switch kind := strings.ToUpper(jscalendarString(participant, "kind")); kind {
		case "GROUP", "RESOURCE", "ROOM":
			prop.Params.Set(ical.ParamCalendarUserType, kind)
		case "LOCATION":
			prop.Params.Set(ical.ParamCalendarUserType, "ROOM")
		}
		if jscalendarBool(participant, "expectReply") {
			prop.Params.Set(ical.ParamRSVP, "TRUE")
		}
		props.Add(prop)
	}
}

// jscalendarParticipantAddress is where a participant gets scheduling
// messages, as a calendar user address such as mailto:.
func jscalendarParticipantAddress(participant map[string]any) string {
	if address := jscalendarString(participant, "calendarAddress"); address != "" {
		return address
	}
	if sendTo, _ := participant["sendTo"].(map[string]any); sendTo != nil {
		if address := jscalendarString(sendTo, "imip"); address != "" {
			return address
		}
	}
	if email := jscalendarString(participant, "email"); email != "" {
		return "mailto:" + email
	}
	return ""
}

// jscalendarAlarms writes the event's alerts as VALARMs.
func jscalendarAlarms(event map[string]any) ([]*ical.Component, error) {
	alerts, _ := event["alerts"].(map[string]any)
	var alarms []*ical.Component
	for _, id := range slices.Sorted(maps.Keys(alerts)) {
		alert, _ := alerts[id].(map[string]any)
		trigger, _ := alert["trigger"].(map[string]any)
		prop := ical.NewProp(ical.PropTrigger)
		switch jscalendarString(trigger, "@type") {
		case "OffsetTrigger", "":
			offset := jscalendarString(trigger, "offset")
			if offset == "" {
				continue
			}
			prop.Value = offset
			if strings.EqualFold(jscalendarString(trigger, "relativeTo"), "end") {
				prop.Params.Set(ical.ParamRelated, "END")
			}
		case "AbsoluteTrigger":
			at, err := time.Parse(time.RFC3339, jscalendarString(trigger, "when"))
			if err != nil {
				return nil, fmt.Errorf("JMAP event alert time: %w", err)
			}
			prop.SetDateTime(at.UTC())
		default:
			continue
		}
		alarm := ical.NewComponent(ical.CompAlarm)
		action := "DISPLAY"
		if strings.EqualFold(jscalendarString(alert, "action"), "email") {
			action = "EMAIL"
		}
		alarm.Props.SetText(ical.PropAction, action)
		alarm.Props.SetText(ical.PropDescription, "Reminder")
		alarm.Props.Set(prop)
		alarms = append(alarms, alarm)
	}
	return alarms, nil
}

// jscalendarKeptProperties writes back the iCalendar properties a server
// kept for the event because they have no JSCalendar form.
func jscalendarKeptProperties(props ical.Props, event map[string]any) {
	kept, _ := event["iCalendar"].(map[string]any)
	list, _ := kept["properties"].([]any)
	for _, item := range list {
		entry, _ := item.([]any)
		if len(entry) < 4 {
			continue
		}
		name, _ := entry[0].(string)
		value, ok := entry[3].(string)
		name = strings.ToUpper(strings.TrimSpace(name))
		if name == "" || !ok || props.Get(name) != nil && !strings.HasPrefix(name, "X-") {
			continue
		}
		prop := ical.NewProp(name)
		prop.Value = value
		if params, _ := entry[1].(map[string]any); params != nil {
			for key, raw := range params {
				switch value := raw.(type) {
				case string:
					prop.Params.Set(strings.ToUpper(key), value)
				case []any:
					for _, one := range value {
						if text, ok := one.(string); ok {
							prop.Params.Add(strings.ToUpper(key), text)
						}
					}
				}
			}
		}
		props.Add(prop)
	}
}

// jscalendarRRule writes a JSCalendar RecurrenceRule as an RRULE value.
func jscalendarRRule(rule map[string]any, zone string, allDay bool) (string, error) {
	frequency := strings.ToUpper(jscalendarString(rule, "frequency"))
	if frequency == "" {
		return "", errors.New("JMAP event repeat rule has no frequency")
	}
	parts := []string{"FREQ=" + frequency}
	if interval, ok := rule["interval"].(float64); ok && interval > 1 {
		parts = append(parts, "INTERVAL="+strconv.Itoa(int(interval)))
	}
	if skip := strings.ToUpper(jscalendarString(rule, "skip")); skip != "" && skip != "OMIT" {
		parts = append(parts, "SKIP="+skip)
	}
	if rscale := strings.ToUpper(jscalendarString(rule, "rscale")); rscale != "" && rscale != "GREGORIAN" {
		parts = append(parts, "RSCALE="+rscale)
	}
	list := func(name, key string) {
		values, _ := rule[key].([]any)
		var out []string
		for _, value := range values {
			switch value := value.(type) {
			case float64:
				out = append(out, strconv.Itoa(int(value)))
			case string:
				out = append(out, strings.ToUpper(value))
			}
		}
		if len(out) > 0 {
			parts = append(parts, name+"="+strings.Join(out, ","))
		}
	}
	if days, _ := rule["byDay"].([]any); len(days) > 0 {
		var out []string
		for _, item := range days {
			day, _ := item.(map[string]any)
			value := strings.ToUpper(jscalendarString(day, "day"))
			if value == "" {
				continue
			}
			if nth, ok := day["nthOfPeriod"].(float64); ok && nth != 0 {
				value = strconv.Itoa(int(nth)) + value
			}
			out = append(out, value)
		}
		if len(out) > 0 {
			parts = append(parts, "BYDAY="+strings.Join(out, ","))
		}
	}
	list("BYMONTHDAY", "byMonthDay")
	list("BYMONTH", "byMonth")
	list("BYYEARDAY", "byYearDay")
	list("BYWEEKNO", "byWeekNo")
	list("BYHOUR", "byHour")
	list("BYMINUTE", "byMinute")
	list("BYSECOND", "bySecond")
	list("BYSETPOS", "bySetPosition")
	if first := strings.ToUpper(jscalendarString(rule, "firstDayOfWeek")); first != "" && first != "MO" {
		parts = append(parts, "WKST="+first)
	}
	if count, ok := rule["count"].(float64); ok && count > 0 {
		parts = append(parts, "COUNT="+strconv.Itoa(int(count)))
	}
	if until := jscalendarString(rule, "until"); until != "" {
		at, err := time.ParseInLocation(jscalendarLocal, until, jscalendarLocation(zone))
		if err != nil {
			return "", fmt.Errorf("JMAP event repeat rule end %q: %w", until, err)
		}
		// UNTIL is in UTC when the start has a zone, and floating otherwise.
		switch {
		case allDay:
			parts = append(parts, "UNTIL="+at.Format("20060102"))
		case zone == "":
			parts = append(parts, "UNTIL="+at.Format("20060102T150405"))
		default:
			parts = append(parts, "UNTIL="+at.UTC().Format("20060102T150405Z"))
		}
	}
	return strings.Join(parts, ";"), nil
}

// jscalendarDateProp writes a LocalDateTime as an iCalendar date property:
// a DATE for an all-day event, UTC for a UTC zone, and else with its TZID,
// or floating without a zone.
func jscalendarDateProp(name, local, zone string, allDay bool) *ical.Prop {
	prop := ical.NewProp(name)
	at, err := time.Parse(jscalendarLocal, local)
	if err != nil {
		prop.Value = local
		return prop
	}
	switch {
	case allDay:
		prop.Params.Set(ical.ParamValue, string(ical.ValueDate))
		prop.Value = at.Format("20060102")
	case jscalendarUTC(zone):
		prop.Value = at.Format("20060102T150405Z")
	default:
		if zone != "" {
			prop.Params.Set(ical.ParamTimezoneID, zone)
		}
		prop.Value = at.Format("20060102T150405")
	}
	return prop
}

// jscalendarDuration splits a JSCalendar Duration into calendar days and
// elapsed time, which differ across a daylight-saving change.
func jscalendarDuration(value string) (days int, elapsed time.Duration, err error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, 0, nil
	}
	rest, ok := strings.CutPrefix(value, "P")
	if !ok || rest == "" {
		return 0, 0, fmt.Errorf("invalid duration %q", value)
	}
	date, clock, _ := strings.Cut(rest, "T")
	number := func(text string, unit byte) (int, string, error) {
		i := strings.IndexByte(text, unit)
		if i < 0 {
			return 0, text, nil
		}
		n, err := strconv.Atoi(text[:i])
		if err != nil || n < 0 {
			return 0, text, fmt.Errorf("invalid duration %q", value)
		}
		return n, text[i+1:], nil
	}
	weeks, date, err := number(date, 'W')
	if err != nil {
		return 0, 0, err
	}
	d, date, err := number(date, 'D')
	if err != nil || date != "" {
		return 0, 0, fmt.Errorf("invalid duration %q", value)
	}
	hours, clock, err := number(clock, 'H')
	if err != nil {
		return 0, 0, err
	}
	minutes, clock, err := number(clock, 'M')
	if err != nil {
		return 0, 0, err
	}
	seconds := 0.0
	if clock != "" {
		text, ok := strings.CutSuffix(clock, "S")
		if seconds, err = strconv.ParseFloat(text, 64); !ok || err != nil {
			return 0, 0, fmt.Errorf("invalid duration %q", value)
		}
	}
	elapsed = time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute + time.Duration(seconds*float64(time.Second))
	return weeks*7 + d, elapsed, nil
}

// jscalendarApplyPatch gives the event as one occurrence sees it: the
// override's properties, by name or JSON pointer path, over the event's,
// without the series' own repeat properties.
func jscalendarApplyPatch(event map[string]any, patch map[string]any) (map[string]any, error) {
	var occurrence map[string]any
	data, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &occurrence); err != nil {
		return nil, err
	}
	for _, key := range []string{"recurrenceRule", "recurrenceRules", "excludedRecurrenceRule", "excludedRecurrenceRules", "recurrenceOverrides", "recurrenceId", "recurrenceIdTimeZone"} {
		delete(occurrence, key)
	}
	for _, path := range slices.Sorted(maps.Keys(patch)) {
		if path == "excluded" {
			continue
		}
		segments := strings.Split(path, "/")
		for i, segment := range segments {
			segments[i] = strings.NewReplacer("~1", "/", "~0", "~").Replace(segment)
		}
		target := occurrence
		for _, segment := range segments[:len(segments)-1] {
			next, ok := target[segment].(map[string]any)
			if !ok {
				next = map[string]any{}
				target[segment] = next
			}
			target = next
		}
		last := segments[len(segments)-1]
		if patch[path] == nil {
			delete(target, last)
		} else {
			target[last] = patch[path]
		}
	}
	return occurrence, nil
}

func jscalendarRecurs(event map[string]any) bool {
	return len(jscalendarRules(event, "recurrenceRule", "recurrenceRules")) > 0
}

// jscalendarRules reads a repeat rule given singly (newer JSCalendar) or as
// a list (RFC 8984).
func jscalendarRules(event map[string]any, single, many string) []map[string]any {
	var rules []map[string]any
	if rule, ok := event[single].(map[string]any); ok {
		rules = append(rules, rule)
	}
	list, _ := event[many].([]any)
	for _, item := range list {
		if rule, ok := item.(map[string]any); ok {
			rules = append(rules, rule)
		}
	}
	return rules
}

// jscalendarLocationName is the event's place: its first named location.
func jscalendarLocationName(event map[string]any) string {
	locations, _ := event["locations"].(map[string]any)
	for _, id := range slices.Sorted(maps.Keys(locations)) {
		location, _ := locations[id].(map[string]any)
		if name := strings.TrimSpace(jscalendarString(location, "name")); name != "" {
			return name
		}
	}
	return ""
}

// jscalendarZone is the event's time zone; "" means floating time.
func jscalendarZone(event map[string]any) string {
	return strings.TrimSpace(jscalendarString(event, "timeZone"))
}

func jscalendarUTC(zone string) bool {
	switch zone {
	case "UTC", "Etc/UTC", "Etc/GMT", "GMT":
		return true
	}
	return false
}

// jscalendarLocation loads a zone; floating times and unknown zones are
// read as UTC, which keeps their wall-clock values.
func jscalendarLocation(zone string) *time.Location {
	if zone == "" || jscalendarUTC(zone) {
		return time.UTC
	}
	if location, err := time.LoadLocation(zone); err == nil {
		return location
	}
	return time.UTC
}

func jscalendarString(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return value
}

func jscalendarBool(object map[string]any, key string) bool {
	value, _ := object[key].(bool)
	return value
}
