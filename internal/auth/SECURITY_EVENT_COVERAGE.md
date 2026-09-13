# Authentication event coverage

Audited against `48b1fa5` (schema v93) and the accompanying uncommitted coverage
changes on 2026-09-08. This is a source and automated-test inventory, not browser
or real-provider acceptance. No schema migration is needed.

## Event and attribution rules

- Enrolled TOTP or a complete same-RP passkey requires verification after every
  password or federated primary sign-in, independently of mandatory enrollment.
  These sign-ins remain `primary_verified` until MFA succeeds. Google, Microsoft,
  and OIDC identities do not themselves satisfy Gofer's MFA requirement.
- Successful passkey enrollment strengthens the current session, advances the
  authentication version, and revokes other sessions in the same transaction as
  its existing `credential_changed` event. Completed first-time TOTP enrollment
  also revokes prior sessions. Failures roll back all these changes; no duplicate
  session-revocation event is added for the credential transition.
- Completed authentication records the authenticated user as actor and subject,
  with the new session ID. A verified primary factor awaiting MFA records
  `primary_verified`, the subject, no actor/session, and `policy_required`; it
  does not claim that application access was granted.
- Failed password and identifier-first passkey attempts are anonymous, including
  known, inactive, unknown, and wrong-surface usernames. A submitted identifier
  is not an authenticated actor. Valid MFA challenges establish a subject;
  verified existing sessions establish the actor for step-up/factor operations.
- Failed provider verification knows only the challenge-bound subject and
  initiating session, if present. It does not claim an authenticated actor.
  Unknown/consumed callback bearers produce anonymous failures. A browser ending
  an unconsumed authorization records `user_action`; an already-consumed flow
  does not emit a second ending event.
- Administrator actions use the validated administrator and exact acting session,
  with the affected user as subject. Instance policy events have no subject.
  CLI actions have no application actor/session and use `local_operator`.
- Event IDs and referenced database row IDs are not bearer tokens. Existing
  metadata uses fixed provider/factor/action/result values, booleans, counts,
  policy values, timestamps, and internal row IDs. Deletion deliberately retains
  only the target account's stable ID and username after its user row is removed.
  The administrator projection explicitly allowlists that deletion identity.
- New writers have closed metadata fields (`method`, `stage`,
  `revocation_reason`) with values supplied by internal typed/static call sites.
  They omit usernames, claims, raw source addresses, user-agent headers, bearer
  values/hashes, and arbitrary errors. Existing writers bound user-agent text to
  1024 bytes, and the views project it to a bounded client description. Existing
  source hashes are keyed, purpose-separated throttle hashes. Existing raw
  user-agent storage is not a guarantee that arbitrary header text is private;
  broader source-metadata policy remains a separate design decision.
- SQL insertion errors propagate. State-changing events use the transaction
  changing that state; failure injection must leave neither partial state nor
  a success event. Routine rollback is not followed by a fabricated success.

## Coverage by transition

“Existing” means implemented before this audit; “added” identifies actual gaps.
Companion `*_test.go` files contain existing lifecycle and rollback coverage.
`security_event_coverage_test.go` covers the additions.

| Area | Event coverage and attribution | Atomic boundary / evidence |
| --- | --- | --- |
| Setup access | Existing `setup_token_issued`, `setup_token_rotated`, `setup_token_verified`, and `setup_token_verification_failed`. Anonymous/system attribution until final enrollment. Invalid token attempt counters and failure metadata are bounded. | `setup_state.go`, `setup_access.go`; issue/rotation hash, access challenge, attempt counter and event share the write transaction. Their tests inject audit failures and check token/hash exclusion. |
| Setup completion | Existing `setup_completed`, owner actor/subject, issued session; system-initialization reason. No duplicate login or credential events are needed for this composite transition. | `setup_completion.go`; owner, factors, recovery hashes, auth version, revocations, session, initialization and event commit together. Setup draft screens do not enroll an account. |
| Password login | Added `login_succeeded` for a completed single-factor login, `primary_verified` for MFA continuation, and anonymous `login_failed` for invalid/inactive/unknown/wrong-surface credentials. | `password_login.go`; hash upgrade, last-login timestamp, throttle reset, session or MFA challenge and event share the transaction. Rejected credentials and their three throttle counters share a separate failure transaction. |
| Federated login | Added completed/primary-pending events for Google, Microsoft and generic OIDC. Added callback verification, unknown-identity, and authorization-ending failure events. Existing closed registration and separate mailbox authorization remain intact. | `primary_authentication.go`, `federated_event.go`, provider callback and identity files. Successful callback consumption now shares the identity-use/session/event transaction. Failed verification consumes the challenge with its event. Unknown identities remain anonymous. |
| Passkey login | Existing success, invalid assertion and counter-regression events; added anonymous unavailable-identifier and early-throttle events. | `passkey_authentication.go`; assertion consumption, counter/clone state, throttle, session and existing event are transactional. No extra success event is added. |
| TOTP login | Existing `login_succeeded` / `login_failed`, subject-only failures; added early-throttle event. | `totp_login.go`; accepted time step, attempts, challenge consumption, throttle and session/event commit together. |
| Throttling | Added `throttled` failure events at password, passkey login/start-step-up, TOTP login/step-up/management and recovery entry rejection. Existing in-transaction invalid-factor events remain the single event for that rejected attempt, including attempts that trigger a delay. | `login_throttle.go`, factor entry points. Low-level bucket bookkeeping alone is not an additional authentication attempt and does not emit duplicate events. Early rejection does not change counters/factors/challenges. |
| Logout | Added `session_revoked`, `user_action`, `revocation_reason=logout`, exact bearer session and owner; unknown/already-revoked bearers are no-ops. | `session_repository.go`; revocation and event share a transaction. Concurrent requests have one winner/event. `handleLogout` returns a generic retryable error and retains the cookie when persistence fails; handler test covers it. |
| Session actions | Existing own-session revocation events include the acting current session, fixed scope and revoked count. Added events for low-level single/all-session revocation (including logout). | `security_session_management.go` and `session_repository.go`; existing targeted/all-other UI actions perform direct transactional SQL and do not call the new writer, avoiding duplicates. Existing ownership, replay, concurrency and rollback tests apply. |
| Password change | Existing `credential_changed` with rotated session; replaced its empty reason with `challenge_verified`. Added an authenticated failed `credential_changed` for incorrect current-password verification. | `password_change.go`; password, auth version, old-session revocation, replacement session and success event share the transaction. Invalid new-password policy is form validation, not a credential change. |
| Factor changes | Existing `credential_changed` for passkey add/remove (including duplicate/invalid registration), TOTP enrollment/replacement/disable (including invalid code), recovery-code replacement/revocation; added early TOTP-management throttle failures. | `passkey_registration.go`, `mfa_management.go`; factor changes, recovery invalidation, challenges and session rotations share their event transaction. Fixed metadata contains no seed/code/assertion/credential material. |
| Step-up | Existing TOTP/passkey `step_up_succeeded` / `step_up_failed`, exact actor/subject/current session. Added early throttle failures. | `mfa_management.go`, `passkey_authentication.go`; replay state and session freshness commit with event. Clone warnings use `policy_required`. |
| Required MFA enrollment | Existing paired `credential_changed` and `login_succeeded` describe two distinct outcomes in one transaction. | `mfa_enrollment.go`; permanent credential, recovery batch, activation/session and both events commit together. Pending seed drafting is not permanent factor enrollment. |
| Recovery | Existing invalid recovery-code `login_failed`, subject-only `recovery_used` before repair, then paired `credential_changed` / `login_succeeded` on completed repair. Added early-throttle failure. | `recovery_login.go`; consumption and restricted challenge are atomic with recovery-use event. Final repair atomically replaces factors, invalidates sessions and records both completed outcomes. No full session exists just because a recovery code was accepted. |
| Reset assistance | Existing `credential_reset_requested` has anonymous actor, eligible webmail subject, and no session; one event when pending status is first created. | `password_reset_request.go`; request state, throttles and event are transactional. Unknown/management/pending/ineligible/repeated/throttled requests preserve the same public response and do not invent subject events. |
| Invitations and password-reset redemption | Existing `enrollment_token_issued`, `enrollment_token_revoked`, `enrollment_completed`, `credential_reset_completed`. Issuance has admin actor/session and target subject; redemption has subject only. Existing Google invitation conflict is a failed completion event. | `admin_user_invitation.go`, `enrollment_token.go`, `enrollment_redemption.go`, `federated_enrollment.go`; token lifecycle, user/credential changes and event share the transaction. Replacements use the issued event with replacement count, not an extra duplicate revoke event. |
| Application sign-in identity link/unlink | Existing `identity_linked` (including cross-user conflict and an occupied Google/Microsoft provider slot) and `identity_unlinked` for all three providers. Added earlier provider-verification/authorization-ending failures. Unlink records the replacement session. | `federated_identity.go`, `microsoft_identity.go`, `generic_oidc_identity.go`; link challenge/identity/event and unlink identity/auth-version/revocation/rotation/event share transactions. Mailbox OAuth grants are separate and are not touched. |
| User status/deletion | Existing `user_disabled`, `user_enabled`, `user_deletion_started`, `user_deleted`, with admin actor/session and target subject. Idempotent status/restart actions do not invent changes. | `admin_user_status.go`, `admin_user_deletion.go`; disable includes session invalidation. Deletion is deliberately resumable: each database transition/event is atomic, external local-blob cleanup is between those transitions. Completion reuses the persisted initiating administrator; target FK becomes NULL on deletion, with allowlisted retained identity metadata. |
| Security policy | Existing `security_policy_changed` for instance MFA, per-user MFA and event retention; administrator attribution, per-user subject only. | `instance_security_policy.go`, `admin_user_mfa_policy.go`, `authentication_event_retention.go`; settings and any associated weak-session revocation share the event transaction. No-op changes produce no event. Pruning is bounded housekeeping, not recursively audited deletion. |
| CLI recovery | Existing `local_recovery_started`, `session_revoked` / `local_operator`, and setup-token rotation events. No invented web actor/session. | `local_recovery.go`, `local_session_revocation.go`, `setup_state.go`, plus `internal/authoperator` tests; exact-target, stopped-server lock, hash-only storage, rollback and output redaction are already covered. Management recovery remains CLI-only. |

## Deliberate exclusions and limits

- Read-only inventory/history, session touches/expiry cleanup, login-page reads,
  draft preparation, idempotent actions and authorization/CSRF rejection before
  an owned operation are not account-transition events. Unknown/foreign private
  references retain their existing indistinguishable responses; no new target
  lookup is added for audit enrichment.
- Invalid/expired setup state after its access window and invalid/replayed
  enrollment bearers do not append subject events. Invalid factor submissions
  within a validated live challenge do. Missing/expired MFA or management
  challenges are rejected before the factor transaction, without inventing an
  actor. These are explicit coverage boundaries, not claims that every HTTP
  error is audited.
- `CreateSession`/`CreateAuthenticatedSession`, `RecordSessionStepUp`,
  `RevokeOtherSessions`, and `RotateSession` have no production callers outside
  their repository wrappers as of this audit (test fixtures call them). Runtime
  authentication, step-up and all-other-session UI operations use the audited
  transitions above. Exposing a low-level helper in a new runtime flow requires
  rechecking event coverage; do not add generic success hooks that duplicate
  those existing composite transitions.
- Existing raw user-agent retention and provider/network behavior are unchanged.
  Source metadata privacy design, real-browser/provider acceptance, dual-mode
  hardening, and release gates are not completed by this audit.

## Verification

New tests verify password success/MFA/failure/throttle attribution, password and
throttle rollback, federated single-factor challenge/session/event rollback,
callback failure attribution/rollback for each provider and purpose, early
factor throttle events, and concurrent logout with secret exclusion. Existing
suites cover the other rows' ownership, exact-session checks, fixed metadata,
secret exclusion, concurrency and injected audit-write failures. The handler
suite additionally verifies event titles/category filtering and logout failure
response/cookie behavior. See the local tracker for commands and final results.

## Required password changes (2026-09-09)

Administrators have two separate operations. Ordinary reset-token issuance
retains its previous optional-redemption behavior. Requiring a password change
creates no token and records `password_change_required` with the verified
administrator as actor, target user as subject, exact acting session, and the
bounded `administrator_action` reason. It appears under policy events. The
existing credential's `must_change` flag, authentication-version increment,
session revocations, and event commit together; an audit failure rolls them all
back. Repeated requests are idempotent and do not append duplicate events.

Successful authentication may establish a password-change-restricted session;
`login_succeeded` establishes the authenticated identity, not unrestricted mail
access. Session reads project the persisted requirement across all authentication
methods, and middleware allows only password change and logout until it clears.
Required MFA remains enforced before this session can be used. Password change
records the existing `credential_changed` event and atomically clears the flag,
revokes reset links and other sessions, invalidates pending login continuations,
and rotates the current session. Token redemption retains its existing reset
transaction and also clears the requirement.

Focused required-password-change tests cover restriction, eligibility, MFA,
audit rollback, concurrency, CSRF, and secret non-disclosure. Browser acceptance
of this optional workflow remains separate.
