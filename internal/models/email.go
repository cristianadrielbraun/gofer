package models

import "html/template"

type Account struct {
	ID        string
	Name      string
	Email     string
	Color     string
	Initials  string
	IsActive  bool
	Folders   []Folder
}

type Folder struct {
	ID         string
	Name       string
	Icon       string
	Unread     int
	IsSystem   bool
	Children   []Folder
}

type Email struct {
	ID           string
	AccountID    string
	FolderID     string
	From         Contact
	To           []Contact
	CC           []Contact
	Subject      string
	Preview      string
	Body         template.HTML
	Date         string
	IsRead       bool
	IsStarred    bool
	HasAttachment bool
	Labels       []Label
	IsSelected   bool
	ThreadCount  int
}

type Contact struct {
	Name    string
	Email   string
	Initials string
}

type Label struct {
	Name  string
	Color string
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
				{ID: "inbox", Name: "Inbox", Icon: "inbox", Unread: 12, IsSystem: true},
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
	emails := []Email{
		{
			ID:        "e1",
			AccountID: "acc-1",
			FolderID:  "inbox",
			From:      Contact{Name: "Sarah Chen", Email: "sarah@example.com", Initials: "SC"},
			To:        []Contact{{Name: "Cristian", Email: "cristian@gofer.email"}},
			Subject:   "Q4 Planning Meeting - Action Items",
			Preview:   "Hey Cristian, following up on our Q4 planning session. Here are the key action items we discussed...",
			Body:      template.HTML(`<p>Hey Cristian,</p><p>Following up on our Q4 planning session. Here are the key action items we discussed:</p><ul><li>Finalize the product roadmap by Oct 15</li><li>Review resource allocation for the new features</li><li>Schedule stakeholder presentations for next week</li><li>Update the sprint backlog with new priorities</li></ul><p>Let me know if you have any questions or if I missed anything.</p><p>Best,<br/>Sarah</p>`),
			Date:      "10:24 AM",
			IsRead:    false,
			IsStarred: true,
			HasAttachment: true,
			Labels:    []Label{{Name: "Important", Color: "bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400"}},
		},
		{
			ID:        "e2",
			AccountID: "acc-1",
			FolderID:  "inbox",
			From:      Contact{Name: "GitHub", Email: "noreply@github.com", Initials: "GH"},
			To:        []Contact{{Name: "Cristian", Email: "cristian@gofer.email"}},
			Subject:   "[gofer.email] Pull Request #47: Add multi-account support",
			Preview:   "alexrivera opened a pull request in gofer.email/gofer · +342 −28 across 12 files...",
			Body:      template.HTML(`<p><strong>alexrivera</strong> opened a pull request in <code>gofer.email/gofer</code></p><p><strong>+342 −28</strong> across 12 files</p><p>Implements multi-account support with account switching, unified inbox view, and per-account folder management.</p><h3>Changes:</h3><ul><li>Added account management service</li><li>Implemented folder tree per account</li><li>Unified inbox with account badges</li><li>Updated settings for account configuration</li></ul>`),
			Date:      "9:45 AM",
			IsRead:    false,
			IsStarred: false,
			HasAttachment: false,
			Labels:    []Label{{Name: "GitHub", Color: "bg-purple-100 text-purple-700 dark:bg-purple-900/30 dark:text-purple-400"}},
		},
		{
			ID:        "e3",
			AccountID: "acc-1",
			FolderID:  "inbox",
			From:      Contact{Name: "Alex Rivera", Email: "alex@company.com", Initials: "AR"},
			To:        []Contact{{Name: "Cristian", Email: "cristian@gofer.email"}},
			CC:        []Contact{{Name: "Team", Email: "team@company.com"}},
			Subject:   "Re: Architecture Decision: Event sourcing vs CRUD",
			Preview:   "I've been thinking about this more, and I think event sourcing would be the right call for the email sync engine...",
			Body:      template.HTML(`<p>I've been thinking about this more, and I think event sourcing would be the right call for the email sync engine. Here's why:</p><ol><li><strong>Auditability</strong> — Every state change is recorded, which is crucial for email metadata</li><li><strong>Sync conflicts</strong> — We can replay events to resolve conflicts between IMAP and local state</li><li><strong>Performance</strong> — Projections can be optimized for read-heavy workloads</li></ol><p>The main concern is complexity, but I think we can mitigate that with good abstractions.</p><p>Thoughts?</p>`),
			Date:      "9:12 AM",
			IsRead:    false,
			IsStarred: false,
			HasAttachment: false,
			ThreadCount: 5,
		},
		{
			ID:        "e4",
			AccountID: "acc-1",
			FolderID:  "inbox",
			From:      Contact{Name: "Linear", Email: "notifications@linear.app", Initials: "LN"},
			To:        []Contact{{Name: "Cristian", Email: "cristian@gofer.email"}},
			Subject:   "GOFER-142: Implement real-time notifications via WebSocket",
			Preview:   "You were assigned to GOFER-142 · Priority: High · Sprint: Alpha Release...",
			Body:      template.HTML(`<p>You were assigned to <strong>GOFER-142</strong></p><p><strong>Implement real-time notifications via WebSocket</strong></p><p>Priority: <span style="color: orange;">High</span> · Sprint: Alpha Release</p><p>Set up WebSocket connections for real-time email notifications. Should support new email alerts, read/unread status changes, and folder updates.</p>`),
			Date:      "Yesterday",
			IsRead:    true,
			IsStarred: false,
			HasAttachment: false,
			Labels:    []Label{{Name: "Work", Color: "bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400"}},
		},
		{
			ID:        "e5",
			AccountID: "acc-1",
			FolderID:  "inbox",
			From:      Contact{Name: "Emma Wilson", Email: "emma@design.co", Initials: "EW"},
			To:        []Contact{{Name: "Cristian", Email: "cristian@gofer.email"}},
			Subject:   "Design System Updates - New Components Ready",
			Preview:   "Hi! The new component library is ready for review. We've added the sidebar, dialog, and data table components...",
			Body:      template.HTML(`<p>Hi!</p><p>The new component library is ready for review. We've added:</p><ul><li><strong>Sidebar</strong> — Collapsible with nested navigation</li><li><strong>Dialog</strong> — Modal system with animations</li><li><strong>Data Table</strong> — Sortable with pagination</li><li><strong>Command Palette</strong> — Searchable with keyboard shortcuts</li></ul><p>Everything is in the staging Figma file. Let me know your thoughts!</p><p>Cheers,<br/>Emma</p>`),
			Date:      "Yesterday",
			IsRead:    true,
			IsStarred: true,
			HasAttachment: true,
		},
		{
			ID:        "e6",
			AccountID: "acc-1",
			FolderID:  "inbox",
			From:      Contact{Name: "Stripe", Email: "receipts@stripe.com", Initials: "ST"},
			To:        []Contact{{Name: "Cristian", Email: "cristian@gofer.email"}},
			Subject:   "Payment receipt for Gofer Email Pro - October 2025",
			Preview:   "Your payment of $29.00 for Gofer Email Pro was successfully processed...",
			Body:      template.HTML(`<p>Your payment was successfully processed.</p><table style="width:100%; border-collapse: collapse;"><tr><td style="padding: 8px; border-bottom: 1px solid #eee;">Gofer Email Pro (Monthly)</td><td style="padding: 8px; border-bottom: 1px solid #eee; text-align: right;">$29.00</td></tr><tr><td style="padding: 8px;"><strong>Total</strong></td><td style="padding: 8px; text-align: right;"><strong>$29.00</strong></td></tr></table>`),
			Date:      "Oct 1",
			IsRead:    true,
			IsStarred: false,
			HasAttachment: false,
		},
		{
			ID:        "e7",
			AccountID: "acc-1",
			FolderID:  "inbox",
			From:      Contact{Name: "Marcus Johnson", Email: "marcus@dev.io", Initials: "MJ"},
			To:        []Contact{{Name: "Cristian", Email: "cristian@gofer.email"}},
			Subject:   "Re: Performance benchmarks for Go HTTP frameworks",
			Preview:   "Ran the benchmarks you asked for. Go 1.23 with net/http handles 120k req/s on the email list endpoint...",
			Body:      template.HTML(`<p>Ran the benchmarks you asked for. Here are the results:</p><ul><li><strong>net/http (stdlib)</strong>: 120k req/s — email list endpoint</li><li><strong>fasthttp</strong>: 185k req/s — same endpoint</li><li><strong>Valyala</strong>: 145k req/s — with middleware</li></ul><p>The stdlib is plenty fast for our use case. The bottleneck will be IMAP connections, not HTTP handling.</p><p>Attached: Full benchmark report with flame graphs.</p>`),
			Date:      "Sep 29",
			IsRead:    true,
			IsStarred: false,
			HasAttachment: true,
			ThreadCount: 3,
		},
		{
			ID:        "e8",
			AccountID: "acc-1",
			FolderID:  "inbox",
			From:      Contact{Name: "Vercel", Email: "noreply@vercel.com", Initials: "VC"},
			To:        []Contact{{Name: "Cristian", Email: "cristian@gofer.email"}},
			Subject:   "Deployment successful — gofer-email-web",
			Preview:   "Your deployment to production was successful. Build time: 34s. Domain: gofer.email...",
			Body:      template.HTML(`<p><strong>Deployment Successful</strong></p><p><strong>Project:</strong> gofer-email-web<br/><strong>Branch:</strong> main<br/><strong>Build Time:</strong> 34s<br/><strong>Domain:</strong> gofer.email</p><p>All checks passed. The deployment is live.</p>`),
			Date:      "Sep 28",
			IsRead:    true,
			IsStarred: false,
			HasAttachment: false,
		},
	}

	if folderID != "" && folderID != "inbox" {
		return []Email{}
	}
	return emails
}

func GetEmailByID(id string) *Email {
	emails := GetEmails("inbox")
	for i := range emails {
		if emails[i].ID == id {
			return &emails[i]
		}
	}
	return nil
}
