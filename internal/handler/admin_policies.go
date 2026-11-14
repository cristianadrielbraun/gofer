package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/admin"
	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/authz"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/policy"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// adminPolicyTabs are the Default policy's tabs, by the category of the
// settings each holds. A tab with no setting yet isn't shown.
var adminPolicyTabs = []struct {
	tab      string
	category policy.Category
	label    string
}{
	{views.AdminPolicyTabSignIn, policy.CategorySignIn, "admin.policies.category.sign_in"},
	{views.AdminPolicyTabMail, policy.CategoryMail, "admin.policies.category.mail"},
	{views.AdminPolicyTabActivity, policy.CategoryActivity, "admin.policies.category.activity"},
}

// adminPolicySettings are the settings the Policies page changes, by key,
// and the dialog each has. A declared setting without a dialog isn't
// shown until it has one.
var adminPolicySettings = map[string]string{
	policy.MFARequired.Key:           views.AdminPolicyDialogTwoFactor,
	policy.OwnProxies.Key:            views.AdminPolicyDialogOwnProxies,
	policy.SecurityRetentionDays.Key: views.AdminPolicyDialogRetention,
}

func (h *Handler) registerAdminPolicyRoutes(adminRoute func(string, http.HandlerFunc)) {
	adminRoute("GET "+views.AdminPoliciesPath, redirectAdminPage(views.AdminDefaultPolicyPath))
	adminRoute("GET "+views.AdminPoliciesPath+"/{$}", redirectAdminPage(views.AdminDefaultPolicyPath))
	adminRoute("GET "+views.AdminDefaultPolicyPath, h.handleDefaultPolicy(views.AdminPolicyTabSignIn))
	for _, tab := range []string{views.AdminPolicyTabMail, views.AdminPolicyTabActivity} {
		adminRoute("GET "+views.AdminDefaultPolicyTabPath(tab), h.handleDefaultPolicy(tab))
	}
	adminRoute("GET "+views.AdminDefaultPolicyPath+"/settings/{key}", h.handleDefaultPolicySettingDialog)
	adminRoute("POST "+views.AdminDefaultPolicyPath+"/settings/{key}", h.handleSetDefaultPolicySetting)
	// The Sign-in page's control moved here.
	adminRoute("GET /admin/security/sign-in", redirectAdminPage(views.AdminDefaultPolicyPath))
	adminRoute("GET /admin/security/sign-in/two-factor", redirectAdminPage(views.AdminPolicySettingPath(policy.MFARequired.Key)))
}

// adminPolicyTabOf is the tab a setting is on.
func adminPolicyTabOf(setting *policy.Setting) string {
	for _, tab := range adminPolicyTabs {
		if tab.category == setting.Category {
			return tab.tab
		}
	}
	return views.AdminPolicyTabSignIn
}

// adminPolicySettingFor is the shown setting with key.
func adminPolicySettingFor(key string) (*policy.Setting, bool) {
	if _, shown := adminPolicySettings[key]; !shown {
		return nil, false
	}
	return policy.Lookup(key)
}

// adminDefaultPolicyExtras are what the page shows over a tab: a setting's
// dialog, and a change that was refused.
type adminDefaultPolicyExtras struct {
	status    int
	dialog    *views.AdminPolicySettingDialog
	pageError string
	// verify opens verification, leading back to the dialog.
	verify bool
}

func (h *Handler) handleDefaultPolicy(tab string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.renderDefaultPolicy(w, r, tab, adminDefaultPolicyExtras{})
	}
}

func (h *Handler) handleDefaultPolicySettingDialog(w http.ResponseWriter, r *http.Request) {
	setting, ok := adminPolicySettingFor(r.PathValue("key"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	actor, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	dialog, err := h.adminPolicyDialog(r.Context(), actor, setting)
	if err != nil {
		h.adminPolicyLoadFailed(w, r, err)
		return
	}
	h.renderDefaultPolicy(w, r, adminPolicyTabOf(setting), adminDefaultPolicyExtras{dialog: dialog})
}

func (h *Handler) adminPolicyLoadFailed(w http.ResponseWriter, r *http.Request, err error) {
	ctx := r.Context()
	if admin.CodeOf(err) == admin.CodeForbidden {
		http.Error(w, i18n.T(ctx, "server.error.admin_access_required"), http.StatusForbidden)
		return
	}
	log.Printf("load the Default policy: %v", err)
	http.Error(w, i18n.T(ctx, "server.error.failed_to_load_policies"), http.StatusInternalServerError)
}

// renderDefaultPolicy draws one tab of the Default policy: each setting,
// its value, and where the value comes from.
func (h *Handler) renderDefaultPolicy(w http.ResponseWriter, r *http.Request, tab string, extras adminDefaultPolicyExtras) {
	ctx := r.Context()
	actor, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	effective, err := h.auth.DefaultPolicy(ctx, actor.UserID)
	if err != nil {
		h.adminPolicyLoadFailed(w, r, err)
		return
	}
	data := views.AdminDefaultPolicyData{Tab: tab, Dialog: extras.dialog}
	for _, candidate := range adminPolicyTabs {
		shown := false
		for _, setting := range policy.Settings() {
			if _, ok := adminPolicySettings[setting.Key]; ok && setting.Category == candidate.category {
				shown = true
			}
		}
		if shown {
			data.Tabs = append(data.Tabs, views.AdminPolicyTab{Tab: candidate.tab, Label: i18n.T(ctx, candidate.label)})
		}
	}
	for _, setting := range policy.Settings() {
		if _, ok := adminPolicySettings[setting.Key]; !ok || adminPolicyTabOf(setting) != tab {
			continue
		}
		row, err := h.adminPolicyRow(ctx, actor, setting, effective.Get(setting))
		if err != nil {
			h.adminPolicyLoadFailed(w, r, err)
			return
		}
		data.Settings = append(data.Settings, row)
	}

	returnTo := views.AdminDefaultPolicyTabPath(tab)
	if extras.dialog != nil {
		returnTo = views.AdminPolicySettingPath(extras.dialog.Key)
	}
	verification, stepUp, ok := h.adminStepUpVerification(w, r, returnTo, extras.verify)
	if !ok {
		return
	}
	data.StepUpRequired = stepUp
	h.renderAdminPage(w, r, adminStatus(extras.status), views.AdminShellPage{
		Section: views.AdminSectionPolicies,
		Content: views.AdminDefaultPolicyPage(data, verification),
		Toasts:  adminErrorToasts(extras.pageError),
	})
}

// adminPolicyRow is a setting's row: its value in words, where it comes
// from, and what to know about it now.
func (h *Handler) adminPolicyRow(ctx context.Context, actor admin.Actor, setting *policy.Setting, resolved policy.Resolved) (views.AdminPolicySetting, error) {
	row := views.AdminPolicySetting{
		Key: setting.Key, Label: i18n.T(ctx, setting.Label), Help: i18n.T(ctx, setting.Help),
		Enforced: resolved.Source.Enforced, EditPath: views.AdminPolicySettingPath(setting.Key),
		Source: i18n.T(ctx, "admin.policies.source."+string(resolved.Source.Kind)),
	}
	switch setting {
	case policy.MFARequired:
		preview, err := h.auth.PreviewInstanceMFAPolicy(ctx, actor.UserID)
		if err != nil {
			return row, err
		}
		row.Value = i18n.T(ctx, "admin.sign_in.two_factor.administrators")
		if resolved.Value == true {
			row.Value = i18n.T(ctx, "admin.sign_in.two_factor.everyone")
		}
		if preview.WithoutAppTotal > 0 {
			row.Note = i18n.N(ctx, "admin.sign_in.two_factor.people", preview.WithoutAppTotal, "count", i18n.Number(ctx, preview.WithoutAppTotal))
			row.NoteHref = "/admin/users?attention=mfa_setup"
		}
		if updated := preview.Policy.UpdatedAt; updated != nil {
			row.Changed = adminPolicyChanged(ctx, adminTimeText(ctx, *updated), preview.UpdatedByUsername)
		}
	case policy.OwnProxies:
		row.Value = i18n.T(ctx, "admin.policies.own_proxies.off")
		if resolved.Value == true {
			row.Value = i18n.T(ctx, "admin.policies.own_proxies.on")
		}
	case policy.SecurityRetentionDays:
		days, _ := resolved.Value.(int)
		row.Value = i18n.N(ctx, "admin.policies.days", days, "count", i18n.Number(ctx, days))
		current, err := h.auth.AuthenticationEventRetention(ctx)
		if err != nil {
			return row, err
		}
		if current.UpdatedAt != nil {
			row.Changed = adminPolicyChanged(ctx, adminTimeText(ctx, *current.UpdatedAt), h.adminUsername(ctx, current.UpdatedBy))
		}
	}
	return row, nil
}

// adminTimeText is a time as the console writes it, in the reader's
// language.
func adminTimeText(ctx context.Context, at time.Time) string {
	return i18n.Dates(ctx).ShortDateTime(at.Local())
}

func adminPolicyChanged(ctx context.Context, when, by string) string {
	if by == "" {
		return i18n.T(ctx, "admin.policies.changed", "when", when)
	}
	return i18n.T(ctx, "admin.policies.changed_by", "when", when, "user", by)
}

// adminPolicyDialog is a setting's dialog, saying what changing it does.
func (h *Handler) adminPolicyDialog(ctx context.Context, actor admin.Actor, setting *policy.Setting) (*views.AdminPolicySettingDialog, error) {
	action := views.AdminPolicySettingPath(setting.Key)
	dialog := &views.AdminPolicySettingDialog{
		Kind: adminPolicySettings[setting.Key], Key: setting.Key, Action: action,
		Title: i18n.T(ctx, setting.Label), Help: i18n.T(ctx, setting.Help),
		CSRFToken: auth.CSRFToken(ctx, http.MethodPost, action),
	}
	switch setting {
	case policy.MFARequired:
		preview, err := h.auth.PreviewInstanceMFAPolicy(ctx, actor.UserID)
		if err != nil {
			return nil, err
		}
		dialog.TwoFactor = views.AdminSignInData{
			Everyone: preview.Policy.MFA.RequiresAllUsers(), WithoutAppTotal: preview.WithoutAppTotal, SessionsToEnd: preview.SessionsToEnd,
		}
		for _, person := range preview.WithoutApp {
			dialog.TwoFactor.WithoutApp = append(dialog.TwoFactor.WithoutApp, views.AdminSignInPerson{ID: person.ID, Username: person.Username})
		}
	case policy.OwnProxies:
		effective, err := h.auth.DefaultPolicy(ctx, actor.UserID)
		if err != nil {
			return nil, err
		}
		dialog.OwnProxiesAllowed = effective.Bool(policy.OwnProxies)
		if dialog.OwnProxyPeople, dialog.OwnProxies, err = h.db.CountOwnOutboundProxies(ctx); err != nil {
			return nil, err
		}
	case policy.SecurityRetentionDays:
		current, err := h.auth.AuthenticationEventRetention(ctx)
		if err != nil {
			return nil, err
		}
		dialog.Days, dialog.MinDays, dialog.MaxDays = current.Days, auth.MinimumAuthenticationEventRetentionDays, auth.MaximumAuthenticationEventRetentionDays
	}
	return dialog, nil
}

// handleSetDefaultPolicySetting changes one of the Default policy's
// settings, through the code that owns it where it lives.
func (h *Handler) handleSetDefaultPolicySetting(w http.ResponseWriter, r *http.Request) {
	setting, ok := adminPolicySettingFor(r.PathValue("key"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	actor, ok := h.adminActor(w, r)
	if !ok {
		return
	}
	switch setting {
	case policy.MFARequired:
		h.setDefaultPolicyTwoFactor(w, r, actor)
	case policy.OwnProxies:
		h.setDefaultPolicyOwnProxies(w, r, actor)
	case policy.SecurityRetentionDays:
		h.setDefaultPolicyRetention(w, r, actor)
	}
}

// refuseDefaultPolicyChange shows the setting's dialog again with why the
// change didn't go through, or verification when that's why.
func (h *Handler) refuseDefaultPolicyChange(w http.ResponseWriter, r *http.Request, actor admin.Actor, setting *policy.Setting, status int, message string, verify bool, edit func(*views.AdminPolicySettingDialog)) {
	dialog, err := h.adminPolicyDialog(r.Context(), actor, setting)
	if err != nil {
		h.adminPolicyLoadFailed(w, r, err)
		return
	}
	if edit != nil {
		edit(dialog)
	}
	h.renderDefaultPolicy(w, r, adminPolicyTabOf(setting), adminDefaultPolicyExtras{status: status, dialog: dialog, pageError: message, verify: verify})
}

func (h *Handler) setDefaultPolicyTwoFactor(w http.ResponseWriter, r *http.Request, actor admin.Actor) {
	ctx := r.Context()
	var mfa auth.InstanceMFAPolicy
	switch r.PostFormValue("value") {
	case "true":
		mfa = auth.InstanceMFAPolicyAllUsers
	case "false":
		mfa = auth.InstanceMFAPolicyAdministrators
	default:
		h.refuseDefaultPolicyChange(w, r, actor, policy.MFARequired, http.StatusBadRequest, i18n.T(ctx, "auth.admin.sign_in.request_invalid"), false, nil)
		return
	}
	result, err := h.auth.SetInstanceMFAPolicy(ctx, auth.SetInstanceMFAPolicyOptions{
		ActorUserID: actor.UserID, ActorSessionID: actor.SessionID, Policy: mfa,
	})
	switch {
	case err == nil:
	case admin.CodeOf(err) == admin.CodeForbidden:
		http.Error(w, i18n.T(ctx, "server.error.admin_access_required"), http.StatusForbidden)
		return
	case admin.CodeOf(err) == admin.CodeVerificationRequired:
		h.refuseDefaultPolicyChange(w, r, actor, policy.MFARequired, http.StatusForbidden, i18n.T(ctx, "auth.admin.verify.instance_policy"), true, nil)
		return
	default:
		log.Printf("change the instance two-factor policy: %v", err)
		h.refuseDefaultPolicyChange(w, r, actor, policy.MFARequired, http.StatusInternalServerError, i18n.T(ctx, "auth.admin.sign_in.change_failed"), false, nil)
		return
	}
	switch {
	case !result.Changed:
		h.setAdminFlashSuccess(w, i18n.T(ctx, "auth.admin.sign_in.unchanged"))
	case mfa.RequiresAllUsers():
		h.setAdminFlashSuccess(w, i18n.N(ctx, "auth.admin.sign_in.required_everyone", int(result.RevokedSessions), "count", i18n.Number(ctx, int(result.RevokedSessions))))
	default:
		h.setAdminFlashSuccess(w, i18n.T(ctx, "auth.admin.sign_in.required_administrators"))
	}
	redirectAdmin(w, r, views.AdminDefaultPolicyTabPath(adminPolicyTabOf(policy.MFARequired)))
}

func (h *Handler) setDefaultPolicyRetention(w http.ResponseWriter, r *http.Request, actor admin.Actor) {
	ctx := r.Context()
	setting := policy.SecurityRetentionDays
	days, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("days")))
	invalid := i18n.T(ctx, "auth.admin.retention.invalid", "min", i18n.Number(ctx, auth.MinimumAuthenticationEventRetentionDays), "max", i18n.Number(ctx, auth.MaximumAuthenticationEventRetentionDays))
	if err != nil || days < auth.MinimumAuthenticationEventRetentionDays || days > auth.MaximumAuthenticationEventRetentionDays {
		h.refuseDefaultPolicyChange(w, r, actor, setting, http.StatusBadRequest, "", false, func(dialog *views.AdminPolicySettingDialog) {
			dialog.DaysError = invalid
		})
		return
	}
	current, err := h.auth.AuthenticationEventRetention(ctx)
	if err != nil {
		h.adminPolicyLoadFailed(w, r, err)
		return
	}
	// Keeping events for less time removes the older ones: say how many
	// first, and change it once that's confirmed.
	if days < current.Days && r.PostFormValue("confirm") != "1" {
		removes, err := h.auth.CountAuthenticationEventsOlderThan(ctx, actor.UserID, days)
		if err != nil {
			h.adminPolicyLoadFailed(w, r, err)
			return
		}
		if removes > 0 {
			h.refuseDefaultPolicyChange(w, r, actor, setting, http.StatusOK, "", false, func(dialog *views.AdminPolicySettingDialog) {
				dialog.ConfirmDays, dialog.Removes = days, removes
			})
			return
		}
	}
	result, err := h.auth.SetAuthenticationEventRetention(ctx, auth.SetAuthenticationEventRetentionOptions{
		ActorUserID: actor.UserID, ActorSessionID: actor.SessionID, Days: days,
	})
	switch {
	case err == nil:
	case errors.Is(err, auth.ErrRecentStepUpRequired):
		h.refuseDefaultPolicyChange(w, r, actor, setting, http.StatusForbidden, i18n.T(ctx, "admin.policies.verify"), true, nil)
		return
	case errors.Is(err, auth.ErrAdministratorRequired):
		http.Error(w, i18n.T(ctx, "server.error.admin_access_required"), http.StatusForbidden)
		return
	case errors.Is(err, auth.ErrAuthenticationEventRetentionInvalid):
		h.refuseDefaultPolicyChange(w, r, actor, setting, http.StatusBadRequest, "", false, func(dialog *views.AdminPolicySettingDialog) {
			dialog.DaysError = invalid
		})
		return
	default:
		log.Printf("set authentication event retention: %v", err)
		http.Error(w, i18n.T(ctx, "server.error.failed_to_update_security_activity_retention"), http.StatusInternalServerError)
		return
	}
	if result.Changed {
		h.wakeAuthenticationEventRetention()
		h.setAdminFlashSuccess(w, i18n.N(ctx, "auth.admin.retention.changed", days, "count", i18n.Number(ctx, days)))
	} else {
		h.setAdminFlashSuccess(w, i18n.N(ctx, "auth.admin.retention.unchanged", days, "count", i18n.Number(ctx, days)))
	}
	redirectAdmin(w, r, views.AdminDefaultPolicyTabPath(adminPolicyTabOf(setting)))
}

func (h *Handler) setDefaultPolicyOwnProxies(w http.ResponseWriter, r *http.Request, actor admin.Actor) {
	ctx := r.Context()
	if h.ownedMailbox != nil {
		var cancel context.CancelFunc
		ctx, cancel = h.ownedAdminDiagnosticContext(ctx)
		defer cancel()
		r = r.WithContext(ctx)
	}
	setting := policy.OwnProxies
	var allowed bool
	switch r.PostFormValue("value") {
	case "true":
		allowed = true
	case "false":
	default:
		h.refuseDefaultPolicyChange(w, r, actor, setting, http.StatusBadRequest, i18n.T(ctx, "auth.admin.form_invalid"), false, nil)
		return
	}
	if err := h.auth.RequireAdministratorPermission(ctx, actor.UserID, actor.SessionID, authz.SecurityChange); err != nil {
		if admin.CodeOf(err) == admin.CodeVerificationRequired {
			h.refuseDefaultPolicyChange(w, r, actor, setting, http.StatusForbidden, i18n.T(ctx, "admin.policies.verify"), true, nil)
			return
		}
		http.Error(w, i18n.T(ctx, "server.error.admin_access_required"), http.StatusForbidden)
		return
	}
	if err := h.validateMailSecurityAdministrator(ctx); err != nil {
		http.Error(w, i18n.T(ctx, "auth.admin.access_lost"), http.StatusForbidden)
		return
	}
	effective, err := h.auth.DefaultPolicy(ctx, actor.UserID)
	if err != nil {
		h.adminPolicyLoadFailed(w, r, err)
		return
	}
	if effective.Bool(setting) == allowed {
		h.setAdminFlashSuccess(w, i18n.T(ctx, "admin.policies.unchanged"))
		redirectAdmin(w, r, views.AdminDefaultPolicyTabPath(adminPolicyTabOf(setting)))
		return
	}
	recorded := h.withAdminAudit(ctx, audit.Event{
		Action:   "security.proxy_policy_changed",
		Target:   audit.Target{Kind: auditTargetInstance},
		Metadata: map[string]any{"people_may_add_proxies": allowed},
	})
	if err := h.db.SetPeopleMayAddOutboundProxies(recorded, allowed); err != nil {
		log.Printf("policies: set own proxies: %v", err)
		h.refuseDefaultPolicyChange(w, r, actor, setting, http.StatusInternalServerError, i18n.T(ctx, "admin.proxies.policy_failed"), false, nil)
		return
	}
	if !allowed {
		// Accounts that used their owner's proxies go back to the default.
		h.reconnectAllAccounts(ctx)
	}
	notice := "admin.proxies.policy_off"
	if allowed {
		notice = "admin.proxies.policy_on"
	}
	h.setAdminFlashSuccess(w, i18n.T(ctx, notice))
	redirectAdmin(w, r, views.AdminDefaultPolicyTabPath(adminPolicyTabOf(setting)))
}

// adminPolicyAddress is a Policies page verification can lead back to: a
// tab of the Default policy, or a setting's dialog.
func adminPolicyAddress(path string) bool {
	if path == views.AdminDefaultPolicyPath {
		return true
	}
	rest, ok := strings.CutPrefix(path, views.AdminDefaultPolicyPath+"/")
	if !ok {
		return false
	}
	if rest == views.AdminPolicyTabMail || rest == views.AdminPolicyTabActivity {
		return true
	}
	key, ok := strings.CutPrefix(rest, "settings/")
	if !ok {
		return false
	}
	_, shown := adminPolicySettingFor(key)
	return shown
}
