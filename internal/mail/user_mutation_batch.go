package mail

import (
	"context"
	"errors"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	goimap "github.com/emersion/go-imap/v2"
)

// mutationBatchLimit is how many changes one replay sends to the provider
// together.
const mutationBatchLimit = 500

// applyMutationBatch sends lead and the account's other due changes of the
// same kind, value and folder to the provider in one request. Read and
// starred go as one IMAP STORE over their UIDs, Email/set calls naming many
// emails on JMAP, messages.batchModify on Gmail and Graph $batch on Outlook;
// permanent deletes as one IMAP expunge, Email/set destroys,
// messages.batchDelete and $batch deletes. A change whose message must be
// looked up on the server first, or that a batch cannot carry, takes the
// one-by-one path, and one whose message is gone is dropped. It gives back
// the changes that failed; err is set when the queue itself could not be
// read or written.
func (s *UserIMAP) applyMutationBatch(ctx context.Context, scope *userIMAPScope, lead storage.MessageMutation) (failed []mutationFailure, err error) {
	batch := []storage.MessageMutation{lead}
	infos := make([]*storage.MessageMutationInfo, 0, mutationBatchLimit)
	deleting := lead.Kind == storage.MessageMutationDelete
	validity := lead.SourceUIDValidity
	if err := scope.call(ctx, func(db *storage.DB) error {
		more, err := db.ClaimMessageMutationBatch(ctx, lead, time.Now(), mutationBatchLimit-1)
		if err != nil {
			return err
		}
		batch = append(batch, more...)
		for _, m := range batch {
			info, err := db.GetMessageMutationInfoForReplay(ctx, m)
			if err != nil {
				return err
			}
			infos = append(infos, info)
		}
		if lead.ProviderType == storage.MessageMutationProviderIMAP && validity == 0 {
			validity, err = db.GetFolderUIDValidity(ctx, lead.FolderID)
		}
		return err
	}); err != nil {
		// The claimed rows stay processing and recover on the next pass.
		return nil, err
	}

	var vanished, single, grouped []storage.MessageMutation
	var groupedInfos []storage.MessageMutationInfo
	for i, m := range batch {
		info := infos[i]
		switch {
		case info == nil:
			vanished = append(vanished, m)
		case m.AccountID != scope.id || info.AccountID != scope.id || info.AccountProvider != m.ProviderType:
			// As applyMutation checks: the account is still the same IMAP or JMAP one.
			failed = append(failed, mutationFailure{m, errors.New("queued mutation account or provider changed")})
		case m.ProviderType == storage.MessageMutationProviderIMAP && info.RemoteUID == 0,
			m.ProviderType != storage.MessageMutationProviderIMAP && info.RemoteMessageID == "":
			single = append(single, m)
		case m.ProviderType == storage.MessageMutationProviderIMAP && info.FolderRemoteID == "":
			failed = append(failed, mutationFailure{m, errors.New("message has no remote IMAP folder identity")})
		default:
			grouped = append(grouped, m)
			groupedInfos = append(groupedInfos, *info)
		}
	}
	if len(vanished) > 0 {
		if err := scope.call(ctx, func(db *storage.DB) error {
			for _, m := range vanished {
				if err := db.DiscardMessageMutation(ctx, m.ID); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return failed, err
		}
	}
	for _, m := range single {
		if err := s.applyMutation(ctx, scope, m); err != nil {
			failed = append(failed, mutationFailure{m, err})
		}
	}
	if len(grouped) == 0 {
		return failed, nil
	}

	var groupFailures []error
	gone := make([]bool, len(grouped))
	switch lead.ProviderType {
	case storage.MessageMutationProviderIMAP:
		groupFailures = s.sendIMAPBatch(ctx, scope, lead, groupedInfos, validity)
	case storage.MessageMutationProviderJMAP:
		emails := make([]JMAPBatchEmail, len(groupedInfos))
		for i, info := range groupedInfos {
			emails[i] = JMAPBatchEmail{EmailID: info.RemoteMessageID, FolderCopyIDs: info.FolderCopyIDs}
			if lead.IsScopedFlagColor() {
				emails[i].FolderCopyIDs = nil
			}
		}
		var sendErr error
		var jmapGone []bool
		groupFailures, jmapGone, sendErr = ApplyJMAPBatch(ctx, scope.config, scope.password, lead, emails)
		if sendErr != nil {
			groupFailures = sameError(len(grouped), sendErr)
		} else if deleting || lead.IsScopedFlagColor() {
			gone = jmapGone
		}
	case storage.MessageMutationProviderGmail, storage.MessageMutationProviderOutlook:
		groupFailures, gone = s.sendProviderBatch(ctx, scope, lead, groupedInfos)
	}
	LogRefusedBatch(scope.id, lead.ProviderType, lead.Kind, groupFailures)
	var applied []int
	for i, m := range grouped {
		switch {
		case errors.Is(groupFailures[i], ErrBatchRefused):
			if err := s.applyMutation(ctx, scope, m); err != nil {
				failed = append(failed, mutationFailure{m, err})
			}
		case groupFailures[i] != nil:
			failed = append(failed, mutationFailure{m, groupFailures[i]})
		default:
			applied = append(applied, i)
		}
	}
	if lead.ProviderType == storage.MessageMutationProviderGmail || lead.ProviderType == storage.MessageMutationProviderOutlook {
		// As applyProviderMutation records each, checking the attempt. A
		// message whose identity changed meanwhile fails alone, as it does
		// there; the others are still recorded.
		for _, i := range applied {
			m, info := grouped[i], groupedInfos[i]
			err := scope.call(ctx, func(db *storage.DB) error {
				return db.CompleteProviderMutation(ctx, m, scope.config.ProviderAccountID, info.RemoteMessageID, info.InternetMessageID, info.RemoteMessageID, "", gone[i])
			})
			if err != nil && !errors.Is(err, storage.ErrMessageMutationSuperseded) {
				failed = append(failed, mutationFailure{m, err})
			}
		}
		return failed, nil
	}
	if err := scope.call(ctx, func(db *storage.DB) error {
		for _, i := range applied {
			m := grouped[i]
			var err error
			switch {
			case gone[i] && m.TargetFlagCopy != "":
				err = db.DiscardMessageMutation(ctx, m.ID)
			case deleting:
				// As applyMutation records a delete, confirming one already gone.
				err = db.CompleteMessageDeleteMutation(ctx, m.ID)
				if err == nil && gone[i] {
					err = db.ConfirmMessageDeleteMutation(ctx, m.ID)
				}
			default:
				err = db.CompleteMessageMutation(ctx, m.ID)
			}
			if err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return failed, err
	}
	return failed, nil
}

// sendProviderBatch sends a Gmail or Outlook batch with the account's token,
// through the same request path single changes use. gone[i] is set for a
// delete whose message the provider no longer had.
func (s *UserIMAP) sendProviderBatch(ctx context.Context, scope *userIMAPScope, lead storage.MessageMutation, infos []storage.MessageMutationInfo) (failures []error, gone []bool) {
	gone = make([]bool, len(infos))
	if scope.config.Provider != lead.ProviderType {
		return sameError(len(infos), errors.New("queued mutation account or provider changed")), gone
	}
	request, err := userProviderMutationRequest(ctx, scope)
	if err != nil {
		return sameError(len(infos), err), gone
	}
	messages := make([]ProviderBatchMessage, len(infos))
	for i, info := range infos {
		messages[i] = ProviderBatchMessage{IDs: append([]string{info.RemoteMessageID}, info.FolderCopyIDs...)}
	}
	switch {
	case lead.ProviderType == storage.MessageMutationProviderGmail && lead.Kind == storage.MessageMutationDelete:
		return SendGmailDeleteBatch(ProviderRequest(request), gmailAPIBaseURL, messages), gone
	case lead.ProviderType == storage.MessageMutationProviderGmail:
		return SendGmailFlagBatch(ProviderRequest(request), gmailAPIBaseURL, lead.Kind, lead.TargetValue, messages), gone
	case lead.Kind == storage.MessageMutationDelete:
		return SendOutlookDeleteBatch(ProviderRequest(request), outlookGraphBaseURL, messages)
	}
	return SendOutlookFlagBatch(ProviderRequest(request), outlookGraphBaseURL, lead.Kind, lead.TargetValue, messages), gone
}

// sendIMAPBatch sets or clears a flag on every message's UID in the batch's
// folder, or deletes them all for good, checking the folder's UIDVALIDITY.
// One command covers them all, so they succeed or fail together.
func (s *UserIMAP) sendIMAPBatch(ctx context.Context, scope *userIMAPScope, lead storage.MessageMutation, infos []storage.MessageMutationInfo, validity uint32) []error {
	uids := make([]uint32, len(infos))
	for i, info := range infos {
		uids[i] = info.RemoteUID
	}
	if validity == 0 {
		return sameError(len(infos), errors.New("IMAP UIDVALIDITY is unavailable; waiting for folder sync"))
	}
	client, err := imap.DefaultPool.Get(ctx, scope.config, scope.password)
	if err == nil {
		defer client.Close()
		if lead.Kind == storage.MessageMutationDelete {
			var changed bool
			changed, err = client.DeleteMessagesIfUIDValidity(ctx, infos[0].FolderRemoteID, uids, validity)
			if err == nil && changed {
				err = imap.ErrMutationUIDValidityChanged
			}
		} else if bits, color := lead.FlagBitsTarget(); color {
			err = client.StoreFlagColorBatchIfUIDValidity(ctx, infos[0].FolderRemoteID, uids, lead.TargetValue, bits, validity)
		} else {
			op, flag := goimap.StoreFlagsDel, goimap.FlagSeen
			if lead.TargetValue {
				op = goimap.StoreFlagsAdd
			}
			if lead.Kind == storage.MessageMutationStarred {
				flag = goimap.FlagFlagged
			}
			err = client.StoreFlagsBatchIfUIDValidity(ctx, infos[0].FolderRemoteID, uids, op, []goimap.Flag{flag}, validity)
		}
	}
	if err != nil {
		return sameError(len(infos), err)
	}
	return make([]error, len(infos))
}

func sameError(n int, err error) []error {
	errs := make([]error, n)
	for i := range errs {
		errs[i] = err
	}
	return errs
}
