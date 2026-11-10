package views

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/a-h/templ"
	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
)

// AdminAuditActivityPath is the Activity page's administration tab.
const AdminAuditActivityPath = "/admin/activity/administration"

// AdminAuditKinds are the administration tab's filters: each keeps the
// actions that start with it and a dot.
var AdminAuditKinds = []string{"user", "invitation", "security", "instance", "audit"}

// AdminAuditActivityData is the administration tab of the Activity page.
type AdminAuditActivityData struct {
	ManagedMode    bool
	StepUpRequired bool
	// Kind is the filter in use, one of AdminAuditKinds or "".
	Kind    string
	Records []audit.Record
	// Before is where this page started (0 for the newest), and Next where
	// the following one does, or 0.
	Before int64
	Next   int64
}

func adminAuditActivityPagePath(kind string, before int64) templ.SafeURL {
	values := url.Values{}
	if kind != "" {
		values.Set("kind", kind)
	}
	if before > 0 {
		values.Set("before", strconv.FormatInt(before, 10))
	}
	if len(values) == 0 {
		return templ.URL(AdminAuditActivityPath)
	}
	return templ.URL(AdminAuditActivityPath + "?" + values.Encode())
}

// adminAuditSentence says what a record's change did, in the reader's
// language.
func adminAuditSentence(ctx context.Context, record audit.Record) string {
	target := record.Target.Label
	if target == "" {
		target = record.Target.ID
	}
	key := "admin.audit.action." + record.Action
	args := []any{"target", target}
	switch record.Action {
	case "instance.security_retention_changed":
		change := record.Changes["security_event_retention_days"]
		args = append(args, "from", adminAuditValue(ctx, change.From), "to", adminAuditValue(ctx, change.To))
	case "instance.mfa_policy_changed":
		change := record.Changes["mfa_policy"]
		args = append(args, "from", adminAuditMFAPolicy(ctx, change.From), "to", adminAuditMFAPolicy(ctx, change.To))
	case "audit.pruned":
		args = append(args, "count", adminAuditValue(ctx, record.Metadata["deleted"]), "date", adminAuditDate(ctx, record.Metadata["older_than"]))
	case "security.exception_added", "security.exception_removed":
		args = append(args, "kind", i18n.T(ctx, "admin.audit.exception."+fmt.Sprint(record.Metadata["kind"])))
	case "security.allowlist_provider_changed":
		key = "admin.audit.action.security.allowlist_provider_blocked"
		if record.Metadata["allowed"] == true {
			key = "admin.audit.action.security.allowlist_provider_allowed"
		}
		args = append(args, "provider", adminAuditProvider(record.Metadata["provider"]))
	case "security.allowlist_exemption_changed":
		key = "admin.audit.action.security.allowlist_unexempted"
		if record.Metadata["exempt"] == true {
			key = "admin.audit.action.security.allowlist_exempted"
		}
	case "security.proxy_everyone_changed":
		key = "admin.audit.action.security.proxy_chosen_people"
		if record.Metadata["everyone"] == true {
			key = "admin.audit.action.security.proxy_everyone"
		}
	case "security.proxy_person_changed":
		key = "admin.audit.action.security.proxy_person_removed"
		if record.Metadata["chosen"] == true {
			key = "admin.audit.action.security.proxy_person_added"
		}
	case "security.proxy_policy_changed":
		key = "admin.audit.action.security.proxy_policy_stopped"
		if record.Metadata["people_may_add_proxies"] == true {
			key = "admin.audit.action.security.proxy_policy_allowed"
		}
	}
	if !i18n.Has(key) {
		return record.Action
	}
	return i18n.T(ctx, key, args...)
}

// adminAuditActor names who made a change: an administrator by the name
// they had then, or Gofer itself.
func adminAuditActor(ctx context.Context, record audit.Record) string {
	if record.Actor.Kind == audit.ActorSystem {
		return i18n.T(ctx, "admin.audit.system")
	}
	if record.Actor.Label != "" {
		return record.Actor.Label
	}
	return record.Actor.ID
}

func adminAuditValue(ctx context.Context, value any) string {
	if number, ok := value.(float64); ok && number == float64(int(number)) {
		return i18n.Number(ctx, int(number))
	}
	return fmt.Sprint(value)
}

func adminAuditMFAPolicy(ctx context.Context, value any) string {
	key := "admin.audit.mfa_policy." + fmt.Sprint(value)
	if i18n.Has(key) {
		return i18n.T(ctx, key)
	}
	return fmt.Sprint(value)
}

func adminAuditProvider(value any) string {
	switch value {
	case "gmail":
		return "Gmail"
	case "outlook":
		return "Outlook"
	}
	return fmt.Sprint(value)
}

func adminAuditDate(ctx context.Context, value any) string {
	text, _ := value.(string)
	at, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return text
	}
	return i18n.Dates(ctx).ShortDateTime(at.Local())
}

func adminAuditTime(ctx context.Context, at time.Time) string {
	return i18n.Dates(ctx).ShortDateTime(at.Local())
}
