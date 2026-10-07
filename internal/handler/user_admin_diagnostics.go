package handler

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// eachUserDiagnostic releases each store before the next owner, runtime lookup
// or browser write. Rebuild scope from central identities rather than local
// profile stubs, whose usernames and lifecycle state can be stale.
func (h *Handler) eachUserDiagnostic(parent context.Context, selected string, kind storage.UserDiagnosticsKind, visit func(context.Context, models.AdminWebmailUserOption, storage.UserDiagnostics, storage.DiagnosticsActor) error) (models.AdminWebmailScope, error) {
	ctx, cancel := h.ownedAdminDiagnosticContext(parent)
	defer cancel()
	user := auth.GetCurrentUser(ctx)
	if user == nil {
		return models.AdminWebmailScope{}, storage.ErrUserDiagnosticsAccess
	}
	actor := storage.DiagnosticsActor{ID: user.ID, AuthVersion: user.AuthVersion}
	if err := h.ownedMailbox.userStorage.ValidateDiagnosticsAdministrator(ctx, actor); err != nil {
		return models.AdminWebmailScope{}, err
	}
	scope, err := h.adminWebmailScope(ctx, selected)
	if err != nil {
		return scope, err
	}
	for _, owner := range scope.Users {
		if scope.SelectedUserID != "" && scope.SelectedUserID != owner.ID {
			continue
		}
		data, err := h.ownedMailbox.userStorage.ReadUserDiagnostics(ctx, actor, owner.ID, kind)
		if err != nil {
			return scope, err
		}
		if err := visit(ctx, owner, data, actor); err != nil {
			return scope, err
		}
	}
	return scope, ctx.Err()
}

func (h *Handler) ownedAdminDiagnosticContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	if root := h.ownedMailbox.userStorageContext; root != nil {
		stop := context.AfterFunc(root, cancel)
		if root.Err() != nil {
			cancel()
		}
		return ctx, func() { stop(); cancel() }
	}
	return ctx, cancel
}

func (h *Handler) ownedContactAdminStatus(ctx context.Context, scope models.AdminWebmailScope) (models.ContactAdminStatus, error) {
	var result models.ContactAdminStatus
	current, err := h.eachUserDiagnostic(ctx, scope.SelectedUserID, storage.UserDiagnosticsContacts, func(_ context.Context, owner models.AdminWebmailUserOption, data storage.UserDiagnostics, _ storage.DiagnosticsActor) error {
		s := data.Contacts
		result.Total += s.Total
		result.Manual += s.Manual
		result.Observed += s.Observed
		result.Synced += s.Synced
		result.Suppressed += s.Suppressed
		result.AddedToday += s.AddedToday
		result.DeletedToday += s.DeletedToday
		if s.LastBackfill.After(result.LastBackfill) {
			result.LastBackfill = s.LastBackfill
		}
		for _, event := range s.RecentEvents {
			event.Username = owner.Username
			result.RecentEvents = append(result.RecentEvents, event)
		}
		sort.SliceStable(result.RecentEvents, func(i, j int) bool { return result.RecentEvents[i].CreatedAt.After(result.RecentEvents[j].CreatedAt) })
		if len(result.RecentEvents) > 50 {
			result.RecentEvents = result.RecentEvents[:50]
		}
		for _, account := range s.AccountSync {
			account.OwnerUsername = owner.Username
			result.AccountSync = append(result.AccountSync, account)
		}
		return nil
	})
	if err != nil {
		return models.ContactAdminStatus{}, err
	}
	result.Scope = current
	result.Backfill = h.ownedMailbox.getContactBackfillState()
	running := h.ownedMailbox.contactSyncRunningAccounts()
	for i := range result.AccountSync {
		result.AccountSync[i].Running = running[result.AccountSync[i].AccountID]
	}
	sort.SliceStable(result.AccountSync, func(i, j int) bool {
		a, b := result.AccountSync[i], result.AccountSync[j]
		if a.OwnerUsername != b.OwnerUsername {
			return strings.ToLower(a.OwnerUsername) < strings.ToLower(b.OwnerUsername)
		}
		return strings.ToLower(a.AccountEmail) < strings.ToLower(b.AccountEmail)
	})
	return result, nil
}

func (h *Handler) ownedLabelAdminStatus(ctx context.Context, scope models.AdminWebmailScope) (models.LabelAdminStatus, error) {
	var result models.LabelAdminStatus
	current, err := h.eachUserDiagnostic(ctx, scope.SelectedUserID, storage.UserDiagnosticsLabels, func(_ context.Context, owner models.AdminWebmailUserOption, data storage.UserDiagnostics, _ storage.DiagnosticsActor) error {
		s := data.Labels.Totals
		t := &result.Totals
		t.Accounts += s.Accounts
		t.TotalMessages += s.TotalMessages
		t.MessagesWithLabels += s.MessagesWithLabels
		t.MessagesWithoutLabels += s.MessagesWithoutLabels
		t.ProviderBackedMessages += s.ProviderBackedMessages
		t.LocalOnlyMessages += s.LocalOnlyMessages
		t.MissingProviderMessages += s.MissingProviderMessages
		t.MissingIdentityMessages += s.MissingIdentityMessages
		t.KnownLabels += s.KnownLabels
		t.PendingMutations += s.PendingMutations
		t.MutationErrors += s.MutationErrors
		t.LastRunMissingProvider += s.LastRunMissingProvider
		t.LastRunSkipped += s.LastRunSkipped
		t.LastRunFailed += s.LastRunFailed
		for _, account := range data.Labels.Accounts {
			account.OwnerUsername = owner.Username
			result.Accounts = append(result.Accounts, account)
		}
		return nil
	})
	if err != nil {
		return models.LabelAdminStatus{}, err
	}
	result.Scope = current
	sort.SliceStable(result.Accounts, func(i, j int) bool {
		a, b := result.Accounts[i], result.Accounts[j]
		if a.OwnerUsername != b.OwnerUsername {
			return strings.ToLower(a.OwnerUsername) < strings.ToLower(b.OwnerUsername)
		}
		return a.AccountID < b.AccountID
	})
	return result, nil
}

func (h *Handler) ownedMailOperationsAdminStatus(ctx context.Context, scope models.AdminWebmailScope) (models.MailOperationsAdminStatus, error) {
	var result models.MailOperationsAdminStatus
	typeKey := func(s models.MailOperationAdminTypeCount) string { return s.Type + "\x00" + s.Provider }
	types := make(map[string]models.MailOperationAdminTypeCount)
	current, err := h.eachUserDiagnostic(ctx, scope.SelectedUserID, storage.UserDiagnosticsMail, func(operation context.Context, owner models.AdminWebmailUserOption, data storage.UserDiagnostics, actor storage.DiagnosticsActor) error {
		s := data.Mail
		result.Total += s.Total
		result.ActionRequired += s.ActionRequired
		for _, item := range s.ByType {
			key := typeKey(item)
			entry := types[key]
			entry.Type, entry.Provider = item.Type, item.Provider
			entry.Total += item.Total
			entry.ActionRequired += item.ActionRequired
			types[key] = entry
		}
		result.ByAccount = append(result.ByAccount, s.ByAccount...)
		mergeUserMailAdminHealth(&result.Health, s.Health)
		runtime := make(map[[2]string]bool)
		known := make(map[[2]string]bool)
		if h.ownedMailbox.userIMAP != nil {
			states, err := h.ownedMailbox.userIMAP.IdleStatusesForDiagnostics(operation, actor, owner.ID)
			if err != nil {
				return err
			}
			for _, state := range states {
				key := [2]string{state.AccountID, state.FolderID}
				known[key] = state.Healthy || state.Reason != ""
				runtime[key] = state.Healthy
			}
		}
		for _, folder := range data.Idle {
			result.Health.IDLE.Configured++
			key := [2]string{folder.AccountID, folder.FolderID}
			if !known[key] {
				result.Health.IDLE.Pending++
			} else if runtime[key] {
				result.Health.IDLE.Healthy++
			} else {
				result.Health.IDLE.Fallback++
			}
		}
		return nil
	})
	if err != nil {
		return models.MailOperationsAdminStatus{}, err
	}
	result.Scope = current
	for _, item := range types {
		result.ByType = append(result.ByType, item)
	}
	sort.Slice(result.ByType, func(i, j int) bool {
		a, b := result.ByType[i], result.ByType[j]
		if a.ActionRequired != b.ActionRequired {
			return a.ActionRequired > b.ActionRequired
		}
		return typeKey(a) < typeKey(b)
	})
	sort.SliceStable(result.ByAccount, func(i, j int) bool {
		a, b := result.ByAccount[i], result.ByAccount[j]
		if a.ActionRequired != b.ActionRequired {
			return a.ActionRequired > b.ActionRequired
		}
		return a.AccountLabel < b.AccountLabel
	})
	if current.SelectedUserID == "" {
		result.Health.SMTPProfile = h.ownedMailbox.smtpDeliveryProfile()
		result.Retention = h.ownedMailbox.mailRetentionDiagnostics()
	}
	return result, nil
}

func mergeUserMailAdminHealth(to *models.MailOperationAdminHealth, from models.MailOperationAdminHealth) {
	to.Outgoing.Pending += from.Outgoing.Pending
	to.Outgoing.Sending += from.Outgoing.Sending
	to.Outgoing.Failed += from.Outgoing.Failed
	to.Outgoing.Ambiguous += from.Outgoing.Ambiguous
	to.SentCopy.Pending += from.SentCopy.Pending
	to.SentCopy.Copying += from.SentCopy.Copying
	to.SentCopy.Failed += from.SentCopy.Failed
	to.SentCopy.Ambiguous += from.SentCopy.Ambiguous
	to.FolderSync.Complete += from.FolderSync.Complete
	to.FolderSync.Partial += from.FolderSync.Partial
	to.FolderSync.Failed += from.FolderSync.Failed
	if !from.OldestPendingAt.IsZero() && (to.OldestPendingAt.IsZero() || from.OldestPendingAt.Before(to.OldestPendingAt)) {
		to.OldestPendingAt = from.OldestPendingAt
	}
	if !from.NextRetryAt.IsZero() && (to.NextRetryAt.IsZero() || from.NextRetryAt.Before(to.NextRetryAt)) {
		to.NextRetryAt = from.NextRetryAt
	}
	mutations := make(map[[2]string]int)
	for _, item := range append(to.MessageMutations, from.MessageMutations...) {
		mutations[[2]string{item.Kind, item.State}] += item.Count
	}
	to.MessageMutations = nil
	for key, count := range mutations {
		to.MessageMutations = append(to.MessageMutations, models.MailOperationAdminKindStateCount{Kind: key[0], State: key[1], Count: count})
	}
	sort.Slice(to.MessageMutations, func(i, j int) bool {
		a, b := to.MessageMutations[i], to.MessageMutations[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.State < b.State
	})
	mergeStates := func(a, b []models.MailOperationAdminStateCount) []models.MailOperationAdminStateCount {
		counts := make(map[string]int)
		for _, item := range append(a, b...) {
			counts[item.State] += item.Count
		}
		var result []models.MailOperationAdminStateCount
		for state, count := range counts {
			result = append(result, models.MailOperationAdminStateCount{State: state, Count: count})
		}
		sort.Slice(result, func(i, j int) bool { return result[i].State < result[j].State })
		return result
	}
	to.LabelMutations = mergeStates(to.LabelMutations, from.LabelMutations)
	to.IMAPDraftOperations = mergeStates(to.IMAPDraftOperations, from.IMAPDraftOperations)
}
