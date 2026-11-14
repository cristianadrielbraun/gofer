package handler

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/admin"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/policy"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// The allowed mail lists on the Policies pages: the Default policy's and a
// named policy's Mail tab show them, and each list's dialog adds or removes
// one entry at a time, sets a named policy's list or leaves it to the
// policies after it. Every change comes back to the dialog.

// adminPolicyLists are the list settings, by key, and the list each is.
var adminPolicyLists = map[string]string{
	policy.MailAllowedServers.Key:   storage.MailAllowlistServer,
	policy.MailAllowedDomains.Key:   storage.MailAllowlistDomain,
	policy.MailAllowedProviders.Key: storage.MailAllowlistProvider,
}

// adminPolicyListText is a list's value in words: what it holds, or that it
// limits nothing.
func adminPolicyListText(ctx context.Context, setting *policy.Setting, value any) string {
	entries, _ := value.([]string)
	if setting == policy.MailAllowedProviders {
		gmail, outlook := slices.Contains(entries, providers.ProviderGmail), slices.Contains(entries, providers.ProviderOutlook)
		switch {
		case gmail && outlook:
			return i18n.T(ctx, "admin.policies.providers.both")
		case gmail:
			return "Gmail"
		case outlook:
			return "Outlook"
		}
		return i18n.T(ctx, "admin.policies.providers.none")
	}
	switch {
	case len(entries) == 0:
		return i18n.T(ctx, "admin.policies.list.no_limit")
	case len(entries) > 3:
		return i18n.T(ctx, "admin.policies.list.more", "entries", strings.Join(entries[:3], ", "), "count", i18n.Number(ctx, len(entries)-3))
	}
	return strings.Join(entries, ", ")
}

// adminPolicyValueText is a setting's value in words, whatever its kind.
func adminPolicyValueText(ctx context.Context, setting *policy.Setting, value any) string {
	if _, list := adminPolicyLists[setting.Key]; list {
		return adminPolicyListText(ctx, setting, value)
	}
	return adminPolicyValueLabel(ctx, setting, value)
}

// adminPolicyStoredList is the list a policy stores for setting, and
// whether it stores one.
func adminPolicyStoredList(setting *policy.Setting, values []policy.Value) ([]string, bool) {
	for _, stored := range values {
		if stored.Key != setting.Key {
			continue
		}
		decoded, err := setting.Decode(stored.JSON)
		if err != nil {
			return nil, true
		}
		return decoded.([]string), true
	}
	return nil, false
}

// adminPolicyListDialogFor is a list's dialog on the policy with policyID:
// what the policy stores, and the form every change posts to.
func adminPolicyListDialogFor(ctx context.Context, found policy.Policy, setting *policy.Setting, values []policy.Value, typed, refusal string) *views.AdminPolicyListDialog {
	action := views.AdminNamedPolicySettingPath(found.ID, setting.Key)
	if found.Default {
		action = views.AdminPolicySettingPath(setting.Key)
	}
	entries, set := adminPolicyStoredList(setting, values)
	d := &views.AdminPolicyListDialog{
		Key: setting.Key, Title: i18n.T(ctx, setting.Label), Help: i18n.T(ctx, setting.Help),
		Action: action, CSRFToken: auth.CSRFToken(ctx, http.MethodPost, action),
		Named: !found.Default, Set: set || found.Default, Entries: entries,
		Typed: typed, Error: refusal,
	}
	switch setting {
	case policy.MailAllowedServers:
		d.Empty = i18n.T(ctx, "admin.allowlist.servers_empty")
		d.Placeholder = i18n.T(ctx, "admin.allowlist.or", "a", "imap.example.org", "b", "*.example.org")
		d.AddLabel = i18n.T(ctx, "admin.allowlist.add_server")
	case policy.MailAllowedDomains:
		d.Empty = i18n.T(ctx, "admin.allowlist.domains_empty")
		d.Placeholder = i18n.T(ctx, "admin.allowlist.or", "a", "example.org", "b", "*.example.org")
		d.AddLabel = i18n.T(ctx, "admin.allowlist.add_domain")
	case policy.MailAllowedProviders:
		d.Providers = true
		d.Gmail, d.Outlook = slices.Contains(entries, providers.ProviderGmail), slices.Contains(entries, providers.ProviderOutlook)
		servers, _ := adminPolicyStoredList(policy.MailAllowedServers, values)
		d.ServersLimited = len(servers) > 0 || d.Gmail || d.Outlook
	}
	return d
}

// changeAdminPolicyList makes the change a list's dialog sent to the policy
// with policyID, then comes back to the dialog. The mailboxes the change
// refuses stop, and the ones it lets in reconnect, right away.
func (h *Handler) changeAdminPolicyList(w http.ResponseWriter, r *http.Request, actor admin.Actor, policyID string, setting *policy.Setting) {
	ctx := r.Context()
	if h.ownedMailbox != nil {
		var cancel context.CancelFunc
		ctx, cancel = h.ownedAdminDiagnosticContext(ctx)
		defer cancel()
		r = r.WithContext(ctx)
	}
	op := auth.MailListOp(r.PostFormValue("op"))
	list := adminPolicyLists[setting.Key]
	value, err := h.adminService().ChangeMailList(ctx, actor, policyID, list, r.PostFormValue("value"), op)
	if err != nil {
		h.refuseAdminPolicyListChange(w, r, actor, policyID, setting, err)
		return
	}
	if h.ownedMailbox != nil {
		h.checkMailRefusalsIfChanged(ctx)
	}
	h.setAdminFlashSuccess(w, adminPolicyListNotice(ctx, setting, op, value))
	if policyID == policy.DefaultID {
		redirectAdmin(w, r, views.AdminPolicySettingPath(setting.Key))
		return
	}
	redirectAdmin(w, r, views.AdminNamedPolicySettingPath(policyID, setting.Key))
}

// adminPolicyListNotice says what a change to a list did.
func adminPolicyListNotice(ctx context.Context, setting *policy.Setting, op auth.MailListOp, value string) string {
	switch {
	case op == auth.MailListSet || op == auth.MailListClear:
		return i18n.T(ctx, "admin.policies.setting_saved", "setting", i18n.T(ctx, setting.Label))
	case setting == policy.MailAllowedProviders:
		name := "Gmail"
		if value == providers.ProviderOutlook {
			name = "Outlook"
		}
		if op == auth.MailListAdd {
			return i18n.T(ctx, "auth.allowlist.provider_allowed", "provider", name)
		}
		return i18n.T(ctx, "auth.allowlist.provider_blocked", "provider", name)
	case op == auth.MailListRemove:
		return i18n.T(ctx, "auth.allowlist.removed")
	case setting == policy.MailAllowedDomains:
		return i18n.T(ctx, "auth.allowlist.domain_added", "domain", value)
	}
	return i18n.T(ctx, "auth.allowlist.server_added", "server", value)
}

// refuseAdminPolicyListChange shows the dialog again with why the change
// didn't go through: on its field for an entry that isn't one, or as a
// message, with verification when that's why.
func (h *Handler) refuseAdminPolicyListChange(w http.ResponseWriter, r *http.Request, actor admin.Actor, policyID string, setting *policy.Setting, err error) {
	ctx := r.Context()
	status, message := http.StatusBadRequest, ""
	typed, refusal := "", ""
	switch {
	case errors.Is(err, storage.ErrMailAllowlistEntry) && setting == policy.MailAllowedDomains:
		typed, refusal = r.PostFormValue("value"), i18n.T(ctx, "auth.allowlist.domain_invalid")
	case errors.Is(err, storage.ErrMailAllowlistEntry):
		typed, refusal = r.PostFormValue("value"), i18n.T(ctx, "auth.allowlist.server_invalid")
	case errors.Is(err, auth.ErrMailListProvider):
		message = i18n.T(ctx, "auth.allowlist.choose_provider")
	default:
		status, message = adminPolicyRefusal(ctx, err, "changing a list of")
	}
	verify := admin.CodeOf(err) == admin.CodeVerificationRequired
	if policyID != policy.DefaultID {
		h.renderNamedPolicy(w, r, adminNamedPolicyExtras{
			tab: adminPolicyTabOf(setting), setting: setting.Key, status: status, pageError: message, verify: verify,
			listTyped: typed, listError: refusal,
		})
		return
	}
	dialog, loadErr := h.adminDefaultPolicyListDialog(ctx, actor, setting, typed, refusal)
	if loadErr != nil {
		h.adminPolicyLoadFailed(w, r, loadErr)
		return
	}
	h.renderDefaultPolicy(w, r, adminPolicyTabOf(setting), adminDefaultPolicyExtras{status: status, listDialog: dialog, pageError: message, verify: verify})
}

// adminDefaultPolicyListDialog is a list's dialog on the Default policy.
func (h *Handler) adminDefaultPolicyListDialog(ctx context.Context, actor admin.Actor, setting *policy.Setting, typed, refusal string) (*views.AdminPolicyListDialog, error) {
	detail, err := h.adminService().Policy(ctx, actor, policy.DefaultID)
	if err != nil {
		return nil, err
	}
	return adminPolicyListDialogFor(ctx, detail.Policy, setting, detail.Values, typed, refusal), nil
}
