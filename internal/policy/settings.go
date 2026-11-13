package policy

// The settings policies decide. Each is declared once, here; the console,
// the resolver and the audit log all read these declarations.

// MFARequired requires two-factor sign-in. Administrators need it
// whatever this says.
var MFARequired = Bool(Spec{
	Key: "auth.mfa_required", Category: CategorySignIn, Kind: KindSecurity,
	Label: "admin.policy.setting.mfa_required", Help: "admin.policy.setting.mfa_required.help",
}, false)

// SecurityRetentionDays is how long sign-in and security events are kept.
var SecurityRetentionDays = Int(Spec{
	Key: "audit.security_retention_days", Category: CategoryActivity, Kind: KindSecurity, InstanceOnly: true,
	Label: "admin.policy.setting.security_retention_days", Help: "admin.policy.setting.security_retention_days.help",
}, 180, 1, 365)

// OwnProxies lets people add proxies of their own for their mailboxes.
var OwnProxies = Bool(Spec{
	Key: "mail.own_proxies", Category: CategoryMail, Kind: KindSecurity,
	Label: "admin.policy.setting.own_proxies", Help: "admin.policy.setting.own_proxies.help",
}, false)
