package maildir

import (
	"slices"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

type recentClaim struct {
	owner *selected
	added []imap.UID
}

// Called under the account gate; the pending claim is persisted by the same
// transaction as delivery. Per-selection state changes only after commit.
func (a *account) prepareRecent(next *accountState, changes map[uint32][]imapserver.Update) map[uint32]recentClaim {
	claims := make(map[uint32]recentClaim)
	for id, updates := range changes {
		claim := recentClaim{}
		for selection := range a.watchers[id] {
			if !selection.closed && !selection.readOnly {
				claim.owner = selection
				break
			}
		}
		for _, change := range updates {
			if add, ok := change.(*imapserver.UpdateAdd); ok {
				claim.added = append(claim.added, add.UIDs...)
			}
		}
		if claim.owner != nil {
			added := make(map[imap.UID]bool, len(claim.added))
			for _, uid := range claim.added {
				added[uid] = true
			}
			if m := byID(next, id); m != nil {
				for _, msg := range m.Messages {
					if added[msg.UID] {
						msg.Recent = false
					}
				}
			}
		}
		claims[id] = claim
	}
	return claims
}
func (s *selected) recentFlags(uid imap.UID, flags []imap.Flag) []imap.Flag {
	out := slices.Clone(flags)
	if s.recent[uid] {
		out = append(out, imap.FlagRecent)
	}
	return out
}
func (s *selected) decorateRecent(data *imap.FetchMessageData, uid imap.UID) {
	if values := data.Items["FLAGS"]; len(values) > 0 {
		if flags, ok := values[0].(imap.FetchDataFlags); ok {
			data.Items["FLAGS"] = []imap.FetchData{imap.FetchDataFlags(s.recentFlags(uid, flags))}
		}
	}
}
func (a *account) publishRecent(id uint32, batch *imapserver.UpdateBatch, claim recentClaim) {
	for selection, updater := range a.watchers[id] {
		if selection.recent == nil {
			selection.recent = make(map[imap.UID]bool)
		}
		before := len(selection.recent)
		if claim.owner == nil || claim.owner == selection {
			for _, uid := range claim.added {
				selection.recent[uid] = true
			}
		}
		updates := make([]imapserver.Update, 0, len(batch.Changes)+1)
		for _, change := range batch.Changes {
			switch value := change.(type) {
			case *imapserver.UpdateExpunge:
				delete(selection.recent, value.UID)
			case *imapserver.UpdateFlags:
				change = &imapserver.UpdateFlags{UID: value.UID, Flags: selection.recentFlags(value.UID, value.Flags), ModSeq: value.ModSeq}
			case *imapserver.UpdateMailboxFlags:
				change = &imapserver.UpdateMailboxFlags{Flags: append(slices.Clone(value.Flags), imap.FlagRecent)}
			}
			updates = append(updates, change)
		}
		if before != len(selection.recent) {
			updates = append(updates, &imapserver.UpdateRecent{NumRecent: uint32(len(selection.recent))})
		}
		_ = updater.Push(&imapserver.UpdateBatch{Before: batch.Before, After: batch.After, Origin: batch.Origin, Changes: updates})
	}
}
