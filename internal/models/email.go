package models

import (
	"fmt"
	"html/template"
	"sync"
	"time"
)

type Account struct {
	ID       string
	Name     string
	Email    string
	Color    string
	Initials string
	IsActive bool
	Folders  []Folder
}

type Folder struct {
	ID       string
	Name     string
	Icon     string
	Unread   int
	IsSystem bool
	Children []Folder
}

type Email struct {
	ID            string
	AccountID     string
	FolderID      string
	From          Contact
	To            []Contact
	CC            []Contact
	Subject       string
	Preview       string
	Body          template.HTML
	Date          string
	IsRead        bool
	IsStarred     bool
	HasAttachment bool
	Labels        []Label
	IsSelected    bool
	ThreadCount   int
}

type Contact struct {
	Name     string
	Email    string
	Initials string
}

type Label struct {
	Name  string
	Color string
}

type EmailPage struct {
	Emails      []Email
	TotalCount  int
	WindowStart int
	WindowEnd   int
	NextCursor  string
	HasMore     bool
}

var (
	folderEmails map[string][]Email
	folderOnce   sync.Once
)

func ensureFolderEmails() {
	folderOnce.Do(func() {
		folderEmails = generateAllEmails()
	})
}

func generateAllEmails() map[string][]Email {
	result := make(map[string][]Email)
	result["inbox"] = generateFolderEmails("inbox", "acc-1", 500)
	result["sent"] = generateFolderEmails("sent", "acc-1", 200)
	result["drafts"] = generateFolderEmails("drafts", "acc-1", 15)
	result["spam"] = generateFolderEmails("spam", "acc-1", 50)
	result["archive"] = generateFolderEmails("archive", "acc-1", 1000)
	result["trash"] = generateFolderEmails("trash", "acc-1", 30)
	result["work-clients"] = generateFolderEmails("work-clients", "acc-1", 75)
	result["work-projects"] = generateFolderEmails("work-projects", "acc-1", 120)
	result["personal"] = generateFolderEmails("personal", "acc-1", 60)
	result["inbox-2"] = generateFolderEmails("inbox-2", "acc-2", 300)
	result["spam-2"] = generateFolderEmails("spam-2", "acc-2", 40)
	return result
}

var senderPool = []Contact{
	{Name: "Sarah Chen", Email: "sarah@example.com", Initials: "SC"},
	{Name: "GitHub", Email: "noreply@github.com", Initials: "GH"},
	{Name: "Alex Rivera", Email: "alex@company.com", Initials: "AR"},
	{Name: "Linear", Email: "notifications@linear.app", Initials: "LN"},
	{Name: "Emma Wilson", Email: "emma@design.co", Initials: "EW"},
	{Name: "Stripe", Email: "receipts@stripe.com", Initials: "ST"},
	{Name: "Marcus Johnson", Email: "marcus@dev.io", Initials: "MJ"},
	{Name: "Vercel", Email: "noreply@vercel.com", Initials: "VC"},
	{Name: "David Park", Email: "david@startup.io", Initials: "DP"},
	{Name: "Lisa Thompson", Email: "lisa@agency.com", Initials: "LT"},
	{Name: "Notion", Email: "updates@notion.so", Initials: "NO"},
	{Name: "Figma", Email: "team@figma.com", Initials: "FG"},
	{Name: "James Wu", Email: "james@corp.com", Initials: "JW"},
	{Name: "Rachel Green", Email: "rachel@studio.co", Initials: "RG"},
	{Name: "Slack", Email: "notifications@slack.com", Initials: "SL"},
}

var subjectPool = []string{
	"Q4 Planning Meeting - Action Items",
	"Pull Request #47: Add multi-account support",
	"Re: Architecture Decision: Event sourcing vs CRUD",
	"GOFER-142: Implement real-time notifications via WebSocket",
	"Design System Updates - New Components Ready",
	"Payment receipt for Gofer Email Pro - October 2025",
	"Re: Performance benchmarks for Go HTTP frameworks",
	"Deployment successful — gofer-email-web",
	"Team standup notes - Monday",
	"Re: Database migration strategy",
	"New feature request: Dark mode support",
	"Weekly engineering digest",
	"Security audit results - Q4 2025",
	"Interview feedback: Senior Backend Engineer",
	"Office supplies order confirmation",
	"Re: API rate limiting discussion",
	"Sprint retrospective action items",
	"Updated project timeline for Q1",
	"New team member onboarding checklist",
	"Client presentation feedback - Round 2",
	"Infrastructure cost optimization report",
	"Re: OpenAPI spec review for v2 endpoints",
	"CI/CD pipeline migration to GitHub Actions",
	"Quarterly OKR review meeting invite",
	"Bug report: Email sync failing on large folders",
}

var previewPool = []string{
	"Hey Cristian, following up on our planning session. Here are the key action items we discussed...",
	"Opened a pull request with significant changes across multiple files. Ready for review...",
	"I've been thinking about this more, and I think the approach we discussed would be the right call...",
	"You were assigned to a high priority task for the current sprint. Deadline is next Friday...",
	"The new component library is ready for review. We've added sidebar, dialog, and data table...",
	"Your payment was successfully processed. Thank you for your continued subscription...",
	"Ran the benchmarks you asked for. Here are the results with detailed analysis and flame graphs...",
	"Your deployment to production was successful. Build time: 34s. All checks passed...",
	"Here are the notes from today's standup meeting with action items and blockers...",
	"After reviewing the options, I think we should go with the incremental migration approach...",
	"Several users have requested this feature. Here's my proposed implementation plan...",
	"Here's a summary of this week's engineering updates, milestones, and upcoming deadlines...",
	"The security audit is complete. Here are the findings, severity ratings, and recommendations...",
	"Overall, the candidate showed strong technical skills and good communication. Recommend proceeding...",
	"Your order has been confirmed and will be delivered by next week. Tracking number attached...",
	"After our discussion, I looked into several approaches for rate limiting. Here's my analysis...",
	"Here are the key takeaways from our retrospective session and improvement proposals...",
	"I've updated the project timeline based on our latest estimates and resource allocation...",
	"Welcome aboard! Here's everything you need to get started with the team and tools...",
	"Thanks for the presentation. Here's the feedback from the client along with next steps...",
	"I've compiled the infrastructure cost report. We can save 30% by migrating to ARM instances...",
	"Reviewed the OpenAPI spec. A few suggestions for the v2 endpoints regarding pagination...",
	"The pipeline migration is complete. Build times improved by 40% and flaky tests reduced...",
	"Please review the updated OKRs for Q1. We need alignment before the all-hands next week...",
	"Users are reporting sync failures on folders with more than 10k emails. Root cause analysis...",
}

var labelPool = []Label{
	{Name: "Important", Color: "bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400"},
	{Name: "Work", Color: "bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400"},
	{Name: "GitHub", Color: "bg-purple-100 text-purple-700 dark:bg-purple-900/30 dark:text-purple-400"},
	{Name: "Personal", Color: "bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-400"},
	{Name: "Finance", Color: "bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-400"},
}

func generateFolderEmails(folderID, accountID string, count int) []Email {
	now := time.Now()
	emails := make([]Email, count)

	for i := 0; i < count; i++ {
		sender := senderPool[i%len(senderPool)]
		subject := subjectPool[i%len(subjectPool)]
		preview := previewPool[i%len(previewPool)]

		minutesAgo := i*13 + (i*7)%11
		emailTime := now.Add(-time.Duration(minutesAgo) * time.Minute)

		isRead := i >= 5
		isStarred := i%13 == 0
		hasAttachment := i%8 == 0
		threadCount := 0
		if i%10 == 0 {
			threadCount = 2 + i%6
		}

		var labels []Label
		if i%7 == 0 {
			labels = []Label{labelPool[i%len(labelPool)]}
		}

		var cc []Contact
		if i%4 == 0 {
			cc = []Contact{{Name: "Team", Email: "team@company.com"}}
		}

		emails[i] = Email{
			ID:            fmt.Sprintf("e-%s-%d", folderID, i),
			AccountID:     accountID,
			FolderID:      folderID,
			From:          sender,
			To:            []Contact{{Name: "Cristian", Email: "cristian@gofer.email"}},
			CC:            cc,
			Subject:       subject,
			Preview:       preview,
			Body:          template.HTML(fmt.Sprintf("<p>%s</p>", preview)),
			Date:          formatRelativeDate(emailTime, now),
			IsRead:        isRead,
			IsStarred:     isStarred,
			HasAttachment: hasAttachment,
			Labels:        labels,
			ThreadCount:   threadCount,
		}
	}

	return emails
}

func formatRelativeDate(t, now time.Time) string {
	tDay := t.Format("2006-01-02")
	nowDay := now.Format("2006-01-02")
	yesterdayDay := now.AddDate(0, 0, -1).Format("2006-01-02")

	if tDay == nowDay {
		return t.Format("3:04 PM")
	}
	if tDay == yesterdayDay {
		return "Yesterday"
	}
	if t.Year() == now.Year() {
		return t.Format("Jan 2")
	}
	return t.Format("Jan 2, 2006")
}

func GetAccounts() []Account {
	return []Account{
		{
			ID:       "acc-1",
			Name:     "Cristian",
			Email:    "cristian@gofer.email",
			Color:    "bg-blue-500",
			Initials: "CR",
			IsActive: true,
			Folders: []Folder{
				{ID: "inbox", Name: "Inbox", Icon: "inbox", Unread: 5, IsSystem: true},
				{ID: "starred", Name: "Starred", Icon: "star", Unread: 0, IsSystem: true},
				{ID: "sent", Name: "Sent", Icon: "send", Unread: 0, IsSystem: true},
				{ID: "drafts", Name: "Drafts", Icon: "file", Unread: 2, IsSystem: true},
				{ID: "archive", Name: "Archive", Icon: "archive", Unread: 0, IsSystem: true},
				{ID: "spam", Name: "Spam", Icon: "alert-circle", Unread: 3, IsSystem: true},
				{ID: "trash", Name: "Trash", Icon: "trash", Unread: 0, IsSystem: true},
				{
					ID:       "work",
					Name:     "Work",
					Icon:     "folder",
					IsSystem: false,
					Children: []Folder{
						{ID: "work-clients", Name: "Clients", Icon: "folder"},
						{ID: "work-projects", Name: "Projects", Icon: "folder", Unread: 4},
					},
				},
				{ID: "personal", Name: "Personal", Icon: "folder", IsSystem: false},
			},
		},
		{
			ID:       "acc-2",
			Name:     "Work",
			Email:    "cristian@company.com",
			Color:    "bg-emerald-500",
			Initials: "WK",
			IsActive: false,
			Folders: []Folder{
				{ID: "inbox-2", Name: "Inbox", Icon: "inbox", Unread: 28, IsSystem: true},
				{ID: "starred-2", Name: "Starred", Icon: "star", Unread: 0, IsSystem: true},
				{ID: "sent-2", Name: "Sent", Icon: "send", Unread: 0, IsSystem: true},
				{ID: "drafts-2", Name: "Drafts", Icon: "file", Unread: 1, IsSystem: true},
				{ID: "spam-2", Name: "Spam", Icon: "alert-circle", Unread: 7, IsSystem: true},
				{ID: "trash-2", Name: "Trash", Icon: "trash", Unread: 0, IsSystem: true},
			},
		},
	}
}

func GetEmails(folderID string) []Email {
	ensureFolderEmails()
	return folderEmails[folderID]
}

func GetFolderEmailCount(folderID string) int {
	ensureFolderEmails()
	return len(folderEmails[folderID])
}

func GetEmailByID(id string) *Email {
	ensureFolderEmails()
	for _, emails := range folderEmails {
		for i := range emails {
			if emails[i].ID == id {
				return &emails[i]
			}
		}
	}
	return nil
}

func GetEmailsRange(folderID string, start, limit int) EmailPage {
	ensureFolderEmails()
	emails := folderEmails[folderID]
	totalCount := len(emails)

	if start >= totalCount {
		return EmailPage{TotalCount: totalCount, WindowStart: start, WindowEnd: start, HasMore: false}
	}

	end := start + limit
	if end > totalCount {
		end = totalCount
	}

	slice := emails[start:end]
	hasMore := end < totalCount
	nextCursor := ""
	if end > 0 && hasMore {
		nextCursor = emails[end-1].ID
	}

	return EmailPage{
		Emails:      slice,
		TotalCount:  totalCount,
		WindowStart: start,
		WindowEnd:   end - 1,
		NextCursor:  nextCursor,
		HasMore:     hasMore,
	}
}

func GetEmailsAfterCursor(folderID, cursor string, limit int) EmailPage {
	ensureFolderEmails()
	emails := folderEmails[folderID]
	totalCount := len(emails)

	start := 0
	if cursor != "" {
		for i, e := range emails {
			if e.ID == cursor {
				start = i + 1
				break
			}
		}
	}

	if start >= totalCount {
		return EmailPage{TotalCount: totalCount, WindowStart: start, WindowEnd: start, HasMore: false}
	}

	end := start + limit
	if end > totalCount {
		end = totalCount
	}

	slice := emails[start:end]
	hasMore := end < totalCount
	nextCursor := ""
	if end > 0 && hasMore {
		nextCursor = emails[end-1].ID
	}

	return EmailPage{
		Emails:      slice,
		TotalCount:  totalCount,
		WindowStart: start,
		WindowEnd:   end - 1,
		NextCursor:  nextCursor,
		HasMore:     hasMore,
	}
}

func GetEmailsAroundEmail(folderID, emailID string, limit int) EmailPage {
	ensureFolderEmails()
	emails := folderEmails[folderID]

	anchorPos := -1
	for i, e := range emails {
		if e.ID == emailID {
			anchorPos = i
			break
		}
	}

	if anchorPos == -1 {
		return GetEmailsRange(folderID, 0, limit)
	}

	half := limit / 2
	start := anchorPos - half
	if start < 0 {
		start = 0
	}

	return GetEmailsRange(folderID, start, limit)
}
