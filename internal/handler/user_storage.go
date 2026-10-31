package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// RegisterUserStorageRoutes mounts owned mailbox routes and their lifecycle.
// Managed startup also registers the central authentication and administration
// routes, then wraps the complete mux in authentication and HTTP middleware.
func (h *Handler) RegisterUserStorageRoutes(ctx context.Context, mux *http.ServeMux, routing *storage.AccountRouting, options ...UserStorageOptions) error {
	if routing == nil || routing.System() != h.db {
		return errors.New("user routing must use the handler's central database")
	}
	if h.auth == nil || !h.auth.IsEnabled() || h.auth.Config().AuthenticationMode() != auth.ModeManaged {
		return errors.New("user storage routes require managed authentication")
	}
	if len(options) > 1 {
		return errors.New("only one user storage options value is supported")
	}
	var option UserStorageOptions
	if len(options) == 1 {
		option = options[0]
	}
	if option.Credentials != nil && (option.IMAP == nil || option.Credentials.Routing() != routing) {
		return errors.New("mailbox credentials require an IMAP lifecycle service with the same routing coordinator")
	}
	if option.ContactSync != nil && option.IMAP == nil {
		return errors.New("owned contact scheduling requires an IMAP lifecycle service")
	}
	if option.CalendarSync != nil && option.IMAP == nil {
		return errors.New("owned calendar scheduling requires an IMAP lifecycle service")
	}

	if option.IMAP != nil {
		if option.IMAP.Routing() != routing || option.Accounts != option.IMAP.Accounts() || h.syncer == nil || option.IMAP.Events() != h.syncer.Events() {
			return errors.New("IMAP service must use the same account repository, router and event bus")
		}
		if option.Hooks.Created != nil || option.Hooks.Updated != nil || option.Hooks.Cleanup != nil {
			return errors.New("IMAP lifecycle hooks cannot be overridden")
		}
		option.Hooks = UserAccountHooks{Created: option.IMAP.QueueAccount, Updated: option.IMAP.RestartAccount, Cleanup: option.IMAP.Cleanup}
	}
	if option.Accounts != nil {
		if option.Accounts.Routing() != routing {
			return errors.New("account repository must use the same routing coordinator")
		}
		if option.Hooks.Created == nil || option.Hooks.Updated == nil || option.Hooks.Cleanup == nil {
			return errors.New("routed account management requires create, update and cleanup lifecycle hooks")
		}
	} else if option.Hooks.Created != nil || option.Hooks.Updated != nil || option.Hooks.Cleanup != nil {
		return errors.New("account lifecycle hooks require a scoped account repository")
	}
	// Share immutable services, not Handler mutexes or mutable worker state.
	routed := &Handler{db: h.db, auth: h.auth, syncer: h.syncer, userStorage: routing,
		blobStore: h.blobStore,
		userIMAP:  option.IMAP, userCredentials: option.Credentials, userAccounts: option.Accounts, userAccountHooks: option.Hooks, userStorageContext: ctx, userDeletions: make(map[string]*userAccountDeletionJob),
		vapidPublicKey: h.vapidPublicKey, testPush: h.testPush, userBackfillQueue: make(chan userContactBackfillJob, 32),
		googleTranslator:         h.googleTranslator,
		remoteResourceDownloader: h.remoteResourceDownloader,
		messageFileKey:           h.messageFileKey,
		providerAvatarHTTPClient: h.providerAvatarHTTPClient,
		avatarWarmupOwner:        h,
		userBackfills:            make(map[string]struct{}), contactSyncRunning: make(map[string]struct{}),
		mailRuleRuns:             newMailRuleRuns()}
	if option.IMAP != nil {
		if h.blobStore != nil && h.blobStore != option.IMAP.Blobs() {
			return errors.New("routed compose must use the IMAP blob store")
		}
		routed.blobStore = option.IMAP.Blobs()
		if option.Credentials != nil {
			if err := option.IMAP.SetCredentials(option.Credentials); err != nil {
				return err
			}
		}
		if err := option.IMAP.SetMailQueue(&userMailDelivery{h: routed}); err != nil {
			return err
		}
		created, updated := routed.userAccountHooks.Created, routed.userAccountHooks.Updated
		routed.userAccountHooks.Created = func(ctx context.Context, id string) error {
			return errors.Join(created(ctx, id), routed.wakeUserContactAccountID(ctx, id), routed.wakeUserCalendarAccountID(ctx, id))
		}
		routed.userAccountHooks.Updated = func(ctx context.Context, id string) error {
			return errors.Join(updated(ctx, id), routed.wakeUserContactAccountID(ctx, id), routed.wakeUserCalendarAccountID(ctx, id))
		}
		if option.ContactSync != nil {
			if err := routed.StartUserContactSync(ctx, *option.ContactSync); err != nil {
				return err
			}
		}
		if option.CalendarSync != nil {
			if err := routed.StartUserCalendarSync(ctx, *option.CalendarSync); err != nil {
				return err
			}
		}
	}
	// Avatar bytes/attempts and worker bounds are installation-wide; the routed
	// handler authorizes visibility through local stores before using that cache.
	h.avatarRouting = routing
	if h.blobStore == nil {
		h.blobStore = routed.blobStore
	}
	h.startAvatarWarmupWorkers(ctx)
	if option.IMAP != nil {
		if err := option.IMAP.StartBackgroundService(ctx, func(ctx context.Context) { <-ctx.Done(); h.WaitAvatarWorkers() }); err != nil {
			return err
		}
	}
	privateHandler := func(pattern string, handler http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if err := routing.ValidateUser(r.Context(), routed.userID(r.Context())); err != nil {
				http.Error(w, "user storage unavailable", http.StatusForbidden)
				return
			}
			// Protect copied file paths for local reads and compose publication.
			// SSE holds no file paths and can stay open indefinitely.
			needsFiles := !strings.HasPrefix(r.URL.Path, "/settings/security") && pattern != "GET /api/events" &&
				pattern != "GET /api/avatars/{hash}" && pattern != "GET /api/provider-avatar" && pattern != "POST /api/avatars/warmup" &&
				pattern != "POST /api/accounts/discover" &&
				pattern != "GET /api/accounts/server-check" &&
				pattern != "POST /api/accounts/test" &&
				pattern != "POST /api/accounts/dav/find" &&
				pattern != "POST /api/accounts/{id}/dav/find" &&
				pattern != "POST /api/accounts/{id}/test-changes" &&
				pattern != "POST /api/accounts/{id}/test" &&
				pattern != "POST /api/accounts/{id}/identities/refresh" &&
				pattern != "POST /api/accounts/{id}/identities/smtp-test" &&
				pattern != "POST /api/mail/sync/accounts/{id}/repair" &&
				pattern != "GET /api/mail-operations" &&
				pattern != "GET /settings/operations/content" &&
				pattern != "POST /api/mail-operations/{id}/retry" &&
				pattern != "GET /api/calendar/events/new" &&
				pattern != "POST /api/calendar/events" &&
				pattern != "PATCH /api/calendar/events/{id}" &&
				pattern != "POST /api/calendar/google-meet/drafts" &&
				pattern != "POST /api/calendar/teams/drafts" &&
				pattern != "POST /api/calendar/teams/drafts/discard" &&
				pattern != "GET /api/calendar/meeting-options" &&
				pattern != "GET /api/calendar/teams-options" &&
				pattern != "GET /api/calendar/events/{id}" &&
				pattern != "GET /api/calendar/events/{id}/actions" &&
				pattern != "DELETE /api/calendar/events/{id}" &&
				pattern != "GET /api/calendar/events/{id}/delivery" &&
				pattern != "POST /api/calendar/events/{id}/delivery/{sendID}/retry" &&
				pattern != "GET /api/calendar/events/{id}/edit" &&
				pattern != "GET /api/calendar/events/{id}/delete-series-confirmation" &&
				pattern != "GET /api/calendar/events/{id}/delete-occurrence-confirmation" &&
				pattern != "POST /api/calendar/sync" &&
				pattern != "POST /api/accounts/{id}/calendar/discover" &&
				pattern != "POST /api/accounts/{id}/contacts/sync/test" &&
				pattern != "POST /api/accounts/{id}/contacts/sync/discover" &&
				pattern != "POST /api/settings/contacts/accounts/sync" &&
				pattern != "POST /api/settings/contacts/providers/gmail/sync"
				// A standalone RSVP preflight reads metadata/provider state only.
			// Email-backed preflights retain the owner file pin for raw MIME.
			if pattern == "GET /api/calendar/events/{id}/response" && r.URL.Query().Get("mail_id") == "" {
				needsFiles = false
			}
			if routed.blobStore != nil && needsFiles {
				release, err := routed.blobStore.PinUserFiles(r.Context(), routed.userID(r.Context()))
				if err != nil {
					http.Error(w, "user files unavailable", 503)
					return
				}
				defer func() {
					release()
					if routed.userIMAP != nil && (strings.HasPrefix(r.URL.Path, "/compose") || strings.HasPrefix(r.URL.Path, "/api/drafts/")) {
						routed.userIMAP.MaybeCleanupUserFiles(r.Context(), routed.userID(r.Context()))
					}
				}()
				// A retention/deletion pass may have held file admission while
				// this request waited. Recheck before file-only writes as well as
				// database operations; the earlier check is no longer sufficient.
				if err := routing.ValidateUser(r.Context(), routed.userID(r.Context())); err != nil {
					http.Error(w, "user storage unavailable", http.StatusForbidden)
					return
				}
			}
			handler(w, r)
		}
	}
	private := func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, privateHandler(pattern, handler))
	}
	private("GET /api/settings/ui", routed.handleGetUISettings)
	private("GET /api/avatars/{hash}", routed.handleAvatarImage)
	private("GET /api/provider-avatar", routed.handleProviderAvatarImage)
	private("POST /api/avatars/warmup", routed.handleAvatarWarmup)
	private("PATCH /api/settings/ui", routed.handleSaveUISettings)
	private("POST /api/settings/theme-colors.css", routed.handleThemeColorsCSS)
	private("POST /api/settings/custom-theme.css", routed.handleCustomThemeCSS)
	private("GET /api/push/vapid-public-key", routed.handlePushVAPIDPublicKey)
	// Browser endpoints need one owner across the entire installation. Their
	// registration stays central; notification preferences are stored per user.
	private("POST /api/push/subscription", routed.handleSavePushSubscription)
	private("DELETE /api/push/subscription", routed.handleDeletePushSubscription)
	private("POST /api/push/test", routed.handleTestPush)
	private("GET /api/contacts/search", routed.handleContactSearch)
	private("GET /api/contacts/export", routed.handleExportContacts)
	private("POST /api/contacts/export", routed.handleExportSelectedContacts)
	private("POST /api/contacts/bulk-delete", routed.handleTrashContacts)
	private("POST /api/contacts/trash/restore", routed.handleRestoreContacts)
	private("POST /api/contacts/trash/delete", routed.handleDeleteContactsForever)
	private("GET /api/contacts/ids", routed.handleContactIDs)
	private("GET /api/contacts/{id}/export", routed.handleExportContact)
	private("GET /calendar", routed.handleCalendar)
	private("GET /api/calendar/guest-suggestions", routed.handleCalendarGuestSuggestions)
	private("GET /contacts", routed.handleContacts)
	private("GET /contacts/items", routed.handleContactItems)
	private("GET /contacts/trash-count", routed.handleContactTrashCount)
	private("GET /{$}", routed.handleIndex)
	private("GET /folder/{id}", routed.handleFolderPartial)
	private("GET /folder/{id}/full", routed.handleFolderFull)
	private("GET /folder/{id}/{email}", routed.handleFolderWithEmail)
	private("GET /mail/folder/{id}/items", routed.handleMailItems)
	private("GET /mail/folder/{id}/ids", routed.handleMailItemIDs)
	private("GET /mail/thread/{threadId}/subitems", routed.handleThreadSubItems)
	private("GET /mail/search", routed.handleMailSearch)
	private("GET /mail/search/items", routed.handleMailSearchItems)
	private("GET /mail/search/ids", routed.handleMailSearchIDs)
	private("GET /mail/search/thread/{id}/subitems", routed.handleMailSearchThreadSubItems)
	private("GET /email/{id}", routed.handleEmailPartial)
	private("GET /email/{id}/move", routed.handleMoveDialog)
	private("POST /mail/labels-dialog", routed.handleLabelsDialog)
	private("GET /email/{id}/body", routed.handleUserEmailBody)
	private("GET /api/inline-content/{messageID}/{contentID}", routed.handleUserInlineContent)
	// Signed message-file links carry their owner instead of a session. The
	// owner gets the same user and file checks as a private route; anything
	// that fails them looks like a missing file.
	mux.HandleFunc("GET "+messageFileRoute+"{kind}/{messageID}/{name}", func(w http.ResponseWriter, r *http.Request) {
		ownerRequest, kind, ok := routed.verifiedMessageFileRequest(r)
		if !ok || (kind == messageFileRemote && routed.userIMAP == nil) {
			http.NotFound(w, r)
			return
		}
		owner := routed.userID(ownerRequest.Context())
		if err := routing.ValidateUser(r.Context(), owner); err != nil {
			http.NotFound(w, r)
			return
		}
		if routed.blobStore != nil {
			release, err := routed.blobStore.PinUserFiles(r.Context(), owner)
			if err != nil {
				http.Error(w, "user files unavailable", http.StatusServiceUnavailable)
				return
			}
			defer release()
			if err := routing.ValidateUser(r.Context(), owner); err != nil {
				http.NotFound(w, r)
				return
			}
		}
		allowMessageFileEmbedding(w)
		switch kind {
		case messageFileWeb:
			routed.relayRemoteContent(w, ownerRequest)
		case messageFileRemote:
			routed.handleUserRemoteAsset(w, ownerRequest)
		default:
			routed.handleUserInlineContent(w, ownerRequest)
		}
	})
	private("GET /api/attachments/{id}/download", routed.handleUserAttachmentDownload)
	private("GET /api/attachments/{id}/preview", routed.handleUserAttachmentPreview)
	private("GET /search", routed.handleSearch)
	private("GET /api/folders/unread", routed.handleFolderUnreadCounts)
	private("GET /api/sidebar/mail", routed.handleMailSidebar)
	private("GET /api/sidebar/accounts/{id}", routed.handleSidebarAccount)
	private("GET /api/accounts", routed.handleUserAccounts)
	private("POST /api/accounts/order", routed.handleAccountOrder)
	private("POST /api/accounts/discover", routed.handleDiscoverAccount)
	private("GET /api/accounts/server-check", routed.handleAccountServerCheck)
	private("POST /api/accounts/test", routed.handleTestNewAccount)
	private("POST /api/accounts/{id}/test-changes", routed.handleTestAccountChanges)
	private("GET /api/mail-operations", routed.handleMailOperations)
	private("GET /settings/operations/content", routed.handleSettingsMailOperationsContent)
	private("GET /settings", routed.handleSettings)
	private("GET /settings/{tab}", routed.handleSettingsTab)
	// The About page's update check and license texts read nothing of the
	// user's, so they need no storage.
	mux.HandleFunc("POST /api/settings/about/update-check", routed.handleAboutUpdateCheck)
	mux.HandleFunc("GET /settings/about/licenses", routed.handleAboutLicenses)
	// Administrators verify and manage their own sign-in on the same security
	// routes, and have no user store, so the central handler serves them.
	management := map[string]http.HandlerFunc{}
	h.registerSecuritySettingsRoutes(func(pattern string, handler http.HandlerFunc) { management[pattern] = handler })
	routed.registerSecuritySettingsRoutes(func(pattern string, handler http.HandlerFunc) {
		central, webmail := management[pattern], privateHandler(pattern, handler)
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if user := auth.GetCurrentUser(r.Context()); user != nil && user.IsManagement() && central != nil {
				central(w, r)
				return
			}
			webmail(w, r)
		})
	})
	private("GET /settings/accounts", routed.handleUserAccountSettings)
	private("GET /settings/sync", func(w http.ResponseWriter, r *http.Request) { routed.handleUserSyncSettingsView(w, r, "sync") })
	private("GET /settings/contacts", func(w http.ResponseWriter, r *http.Request) { routed.handleUserSyncSettingsView(w, r, "contacts") })
	private("GET /api/accounts/{id}/deletion-status", routed.handleUserAccountDeletionStatus)
	private("GET /api/accounts/{id}/mailbox-size", routed.handleMailboxSize)
	if option.Accounts != nil {
		routed.registerUserSignatures(private)
		routed.registerProfileTransferRoutes(private)
		if option.IMAP != nil {
			routed.StartMailExports(ctx)
			routed.registerMailExportRoutes(private)
			routed.StartMailImports(ctx)
			routed.registerMailImportRoutes(private)
			routed.StartMailTransfers(ctx)
			routed.registerMailTransferRoutes(private)
		}
		private("POST /api/accounts", routed.handleUserCreateAccount)
		private("GET /api/settings/contacts/suppressed", routed.handleUserSuppressedContactsSettings)
		private("GET /api/accounts/{id}/edit", routed.handleUserEditAccount)
		private("POST /api/accounts/{id}/edit", routed.handleUserUpdateAccount)
		private("POST /api/accounts/{id}/color", routed.handleUserAccountColor)
		private("DELETE /api/accounts/{id}", routed.handleUserDeleteAccount)
		if option.Credentials != nil {
			private("POST /api/accounts/oauth2/authorize", routed.handleUserAccountOAuthAuthorize)
			private("GET /auth/google/mailbox/callback", func(w http.ResponseWriter, r *http.Request) { routed.handleUserAccountOAuthCallback(w, r, "gmail") })
			private("GET /auth/microsoft/mailbox/callback", func(w http.ResponseWriter, r *http.Request) { routed.handleUserAccountOAuthCallback(w, r, "outlook") })
			private("GET "+microsoftDeviceSignInPath, routed.handleUserMicrosoftDeviceSignInPage)
			private("POST "+microsoftDeviceSignInPath, routed.handleUserMicrosoftDeviceSignInPoll)
		}
	}
	if option.IMAP != nil {
		private("POST /api/messages/{id}/prefetch-body", routed.handleUserPrefetchBody)
		private("POST /api/messages/{id}/download-full", routed.handleUserDownloadFullBody)
		private("GET /api/messages/{id}/source", routed.handleUserMessageSource)
		private("GET /email/{id}/source", routed.handleUserMessageSourceDialog)
		private("POST /api/messages/{id}/translate", routed.handleUserTranslateMessage)
		private("GET /email/{id}/body/translated", routed.handleUserTranslatedEmailBody)
		private("POST /api/remote-content/{id}/allow", routed.handleUserAllowRemoteContent)
		private("GET /api/remote-assets/{messageID}/{filename}", routed.handleUserRemoteAsset)
		private("POST /api/messages/{id}/refetch", routed.handleUserRefetchBody)
		private("GET /api/mail/{id}/calendar", routed.handleUserMailCalendarFooter)
		private("POST /api/mail/{id}/calendar/invitation", routed.handleUserMailCalendarInvitation)
		private("GET /api/calendar/events/new", routed.handleUserNewCalendarEvent)
		private("POST /api/calendar/events", routed.handleUserCreateCalendarEvent)
		private("PATCH /api/calendar/events/{id}", routed.handleUserUpdateCalendarEvent)
		private("POST /api/calendar/google-meet/drafts", routed.handleUserCalendarGoogleMeetDraft)
		private("POST /api/calendar/teams/drafts", routed.handleUserCalendarTeamsDraft)
		private("POST /api/calendar/teams/drafts/discard", routed.handleUserCalendarTeamsDraftDiscard)
		private("GET /api/calendar/meeting-options", routed.handleUserCalendarMeetingOptions)
		private("GET /api/calendar/teams-options", routed.handleUserCalendarMeetingOptions)
		private("GET /api/calendar/events/{id}", routed.handleUserCalendarEvent)
		private("GET /api/calendar/events/{id}/actions", routed.handleUserCalendarEventActions)
		private("DELETE /api/calendar/events/{id}", routed.handleUserDeleteCalendarEvent)
		private("GET /api/calendar/events/{id}/delivery", routed.handleUserCalendarDeliveryStatus)
		private("POST /api/calendar/events/{id}/delivery/{sendID}/retry", routed.handleUserCalendarDeliveryRetry)
		private("GET /api/calendar/events/{id}/response", routed.handleUserCalendarResponseForm)
		private("POST /api/calendar/events/{id}/response", routed.handleUserCalendarResponse)
		private("POST /api/calendar/events/{id}/forward", routed.handleUserCalendarForward)
		private("GET /api/calendar/replies/{id}", routed.handleUserCalendarReplyStatus)
		private("POST /api/calendar/replies/{id}", routed.handleUserCalendarReplyAction)
		private("GET /api/calendar/events/{id}/edit", routed.handleUserEditCalendarEvent)
		private("GET /api/calendar/events/{id}/delete-series-confirmation", routed.handleUserCalendarSeriesDeleteConfirmation)
		private("GET /api/calendar/events/{id}/delete-occurrence-confirmation", routed.handleUserCalendarOccurrenceDeleteConfirmation)
		private("POST /api/calendar/sync", routed.handleUserCalendarSync)
		private("POST /api/calendar/sources/{id}/visibility", routed.handleCalendarVisibility)
		private("POST /api/calendar/sources/{id}/sync", routed.handleCalendarSourceSync)
		private("DELETE /api/calendar/sources/{id}/sync", routed.handleCalendarSourceSync)
		private("PATCH /api/calendar/sources/{id}", routed.handleCalendarSourceManage)
		private("DELETE /api/calendar/sources/{id}", routed.handleCalendarSourceManage)
		private("POST /api/accounts/{id}/calendar/sources", routed.handleUserSaveCalendarSources)
		private("POST /api/accounts/{id}/calendar/discover", routed.handleUserDiscoverCalendars)
		private("POST /api/accounts/{id}/contacts/sync", routed.handleUserSaveAccountContactSync)
		private("POST /api/accounts/{id}/contacts/sync/test", routed.handleUserTestAccountContactSync)
		private("POST /api/accounts/{id}/contacts/sync/discover", routed.handleUserDiscoverAccountContactSync)
		if option.Accounts != nil {
			private("POST /api/accounts/dav/find", routed.handleUserFindDAVItems)
			private("POST /api/accounts/dav", routed.handleUserCreateDAVAccount)
			private("POST /api/accounts/{id}/dav/find", routed.handleUserFindDAVItems)
			private("POST /api/accounts/{id}/dav", routed.handleUserUpdateDAVAccount)
			private("POST /api/accounts/{id}/mail", routed.handleUserAddAccountMail)
		}
		private("POST /api/settings/contacts/accounts/sync", routed.handleUserSyncAccountContacts)
		private("POST /api/settings/contacts/providers/gmail/sync", routed.handleUserSyncAccountContacts)
		private("POST /api/contacts/{id}/sync-now", routed.handleUserSyncContactNow)
		private("POST /api/contacts", routed.handleUserSaveContact)
		private("POST /api/contacts/import", routed.handleUserImportContacts)
		private("POST /api/contacts/{id}/unify", routed.handleUserUnifyContact)
		private("POST /api/contacts/{id}/delete", routed.handleDeleteContact)
		private("POST /api/contacts/{id}/delete-forever", routed.handleDeleteContactForever)
		private("POST /api/settings/contacts/suppressed/clear", routed.handleUserClearSuppressedContacts)
		private("POST /api/settings/contacts/suppressed/{id}/clear", routed.handleUserClearSuppressedContact)
		private("POST /api/settings/contacts/delete-observed", routed.handleUserDeleteObservedContacts)
		private("POST /api/contacts/{id}/sync-setup/confirm", routed.handleUserConfirmContactSyncSetup)
		private("GET /api/contacts/{id}/sync-setup", routed.handleUserContactSyncSetup)
		private("GET /api/contacts/{id}/sync-setup/findings", routed.handleUserContactSyncSetupFindings)
		private("POST /api/contacts/{id}/sync-setup/preview", routed.handleUserPreviewContactSyncSetup)
		private("POST /api/mail-operations/{id}/retry", routed.handleUserRetryMailOperation)
		private("POST /api/settings/sync", routed.handleUserSaveSyncSettings)
		private("POST /api/settings/accounts/{id}/outlook-push", routed.handleOutlookPushSetting)
		private("POST /api/settings/accounts/{id}/outlook-push/check", routed.handleOutlookPushCheck)
		private("POST /api/accounts/{id}/services", routed.handleUserEmailService)
		// Retain the earlier opt-in URL while the application uses /services.
		private("POST /api/accounts/{id}/service", routed.handleUserEmailService)
		private("POST /api/accounts/{id}/test", routed.handleUserTestAccount)
		private("POST /api/accounts/{id}/identities/refresh", routed.handleUserRefreshAccountIdentities)
		private("POST /api/accounts/{id}/identities/smtp-test", routed.handleUserTestIdentitySMTP)
		private("POST /api/accounts/{id}/folder-roles/create", routed.handleCreateFolderRoleFolder)
		private("POST /api/accounts/{id}/folders", routed.handleCreateFolder)
		private("GET /api/accounts/{id}/folders/manage", routed.handleManageFolders)
		private("GET /api/folders/{id}/manage", routed.handleFolderInfo)
		private("POST /api/folders/{id}/rename", routed.handleRenameFolder)
		private("POST /api/folders/{id}/delete", routed.handleDeleteFolder)
		private("POST /api/folders/{id}/restore", routed.handleRestoreFolder)
		private("POST /api/folders/{id}/trash-folders/delete", routed.handleDeleteTrashFolders)
		private("POST /api/accounts/{id}/labels", routed.handleCreateLabel)
		private("GET /api/labels/{id}/manage", routed.handleLabelInfo)
		private("POST /api/labels/{id}/rename", routed.handleRenameLabel)
		private("POST /api/labels/{id}/delete", routed.handleDeleteLabel)
		private("POST /api/mail/sync/accounts/{id}/repair", routed.handleUserRepairMailAccount)
		private("POST /api/mail/sync", routed.handleUserManualSync)
		private("POST /api/mail/sync/accounts/{id}", routed.handleUserManualSync)
		private("POST /api/mail/sync/cancel", routed.handleUserCancelSync)
		private("GET /api/events", routed.handleUserSSE)
		routed.registerUserMessageMutations(private)
		routed.registerUserCompose(private)
	}
	if err := routed.startUserContactBackfills(ctx); err != nil {
		return err
	}
	if option.IMAP != nil {
		if err := routed.StartUserDeletionCleanup(ctx); err != nil {
			return err
		}
	}
	h.ownedMailbox = routed
	return nil
}

// UserAccountHooks bridge persistence to converted provider workers and external
// cleanup. They run without a database lease. Cleanup must stop/join account
// workers and remove blobs and central OAuth credentials, and be idempotent.
type UserAccountHooks struct {
	Created func(context.Context, string) error
	Updated func(context.Context, string) error
	Cleanup func(context.Context, string) error
}

type UserStorageOptions struct {
	Accounts     *config.UserAccountStore
	Hooks        UserAccountHooks
	IMAP         *mail.UserIMAP
	Credentials  *mailauth.UserCredentials
	ContactSync  *UserContactSyncOptions
	CalendarSync *UserCalendarSyncOptions
}

func (h *Handler) withUserDB(ctx context.Context, userID string, fn func(*storage.DB) error) error {
	if h.userStorage == nil {
		return fn(h.db)
	}
	return h.userStorage.WithUser(ctx, userID, fn)
}

type userContactBackfillJob struct {
	userID, sourceKey string
	owners            []string
	actor             *storage.DiagnosticsActor
}

func (h *Handler) ensureUserContactsBackfilled(ctx context.Context) {
	if h.userBackfillContext == nil || h.userBackfillContext.Err() != nil {
		return
	}
	userID := h.userID(ctx)
	var job userContactBackfillJob
	err := h.withUserDB(ctx, userID, func(db *storage.DB) error {
		settings := db.GetContactSettings(ctx, userID)
		if !settings.AutoCreateObserved || (!settings.ObserveSenders && !settings.ObserveRecipients) {
			return nil
		}
		key := storage.ContactBackfillSourceKey(settings)
		done, err := db.GetSetting(ctx, userID, "contacts_observed_backfilled_v1")
		if err != nil {
			return err
		}
		if done != key {
			job = userContactBackfillJob{userID: userID, sourceKey: key}
		}
		return nil
	})
	if err != nil {
		log.Printf("contacts: load user backfill settings: %v", err)
		return
	}
	if job.userID == "" {
		return
	}
	h.contactBackfillMu.Lock()
	defer h.contactBackfillMu.Unlock()
	if h.userBackfillContext == nil || h.userBackfillContext.Err() != nil {
		return
	}
	if _, exists := h.userBackfills[userID]; exists {
		return
	}
	select {
	case h.userBackfillQueue <- job:
		h.userBackfills[userID] = struct{}{}
	default: // A later search retries; pending users never open/pin database files.
	}
}

func (h *Handler) startUserContactBackfills(ctx context.Context) error {
	if ctx == nil || h.userStorage == nil || h.userBackfillQueue == nil {
		return errors.New("contact backfill requires owned routing, queue and lifecycle")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	h.contactBackfillMu.Lock()
	defer h.contactBackfillMu.Unlock()
	if h.userBackfillDone != nil {
		return errors.New("contact backfill dispatcher already started")
	}
	workCtx, cancel := context.WithCancel(ctx)
	h.userBackfillContext, h.userBackfillCancel, h.userBackfillDone = workCtx, cancel, make(chan struct{})
	if h.userIMAP != nil {
		if err := h.userIMAP.StartBackgroundService(workCtx, h.runUserContactBackfills); err != nil {
			cancel()
			close(h.userBackfillDone)
			return err
		}
	} else {
		go h.runUserContactBackfills(workCtx)
	}
	return nil
}

func (h *Handler) runUserContactBackfills(ctx context.Context) {
	defer close(h.userBackfillDone)
	defer h.userBackfillCancel()
	defer func() {
		h.contactBackfillMu.Lock()
		defer h.contactBackfillMu.Unlock()
		clear(h.userBackfills)
		if h.contactBackfillState.InProgress {
			h.contactBackfillState.InProgress = false
			h.contactBackfillState.FinishedAt = time.Now().UTC()
			h.contactBackfillState.LastError = context.Canceled.Error()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-h.userBackfillQueue:
			if ctx.Err() != nil {
				return
			}
			workCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			if job.actor != nil {
				h.runAdminContactBackfill(workCtx, job)
				cancel()
				continue
			}
			state := models.ContactBackfillState{InProgress: true, StartedAt: time.Now().UTC()}
			h.publishContactBackfill(job.userID, state)
			err := h.userStorage.BackfillUserContacts(workCtx, job.userID, nil, job.sourceKey, func(processed int) { state.Processed = processed; h.publishContactBackfill(job.userID, state) })
			cancel()
			state.InProgress, state.FinishedAt = false, time.Now().UTC()
			if err != nil {
				state.LastError = err.Error()
				log.Printf("contacts: user backfill failed: %v", err)
			}
			h.publishContactBackfill(job.userID, state)
			h.contactBackfillMu.Lock()
			delete(h.userBackfills, job.userID)
			h.contactBackfillMu.Unlock()
		}
	}
}

// WaitUserContactBackfills stops admission and joins all automatic and manual
// contact maintenance. It is also joined by the owned IMAP runtime when present.
func (h *Handler) WaitUserContactBackfills() {
	if h.ownedMailbox != nil {
		h.ownedMailbox.WaitUserContactBackfills()
		return
	}
	if h.userBackfillCancel != nil {
		h.userBackfillCancel()
		<-h.userBackfillDone
	}
}
