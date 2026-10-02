package views

type CalendarCreateSource struct {
	ID          string
	Name        string
	AccountName string
	Writable    bool
	Authorized  bool
}

type CalendarCreateData struct {
	RequestID string
	SourceID  string
	Sources   []CalendarCreateSource
	Date      string
	StartTime string
	EndDate   string
	EndTime   string
	TimeZone  string
}
