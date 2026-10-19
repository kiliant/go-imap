package memory

import (
	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

// All helpers run with the account lock held. Pending messages remain recent
// until a read-write selection claims them; EXAMINE leaves that state intact.
func (s *selected) recentFlags(uid imap.UID, flags []imap.Flag) []imap.Flag {
	out := cloneFlags(flags)
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
func publishLocked(m *mailbox, batch *imapserver.UpdateBatch) {
	var added []imap.UID
	for _, change := range batch.Changes {
		if add, ok := change.(*imapserver.UpdateAdd); ok {
			added = append(added, add.UIDs...)
		}
	}
	var owner *selected
	for selection := range m.watchers {
		if !selection.closed && !selection.readOnly {
			owner = selection
			break
		}
	}
	if owner != nil && len(added) > 0 {
		newUIDs := make(map[imap.UID]bool, len(added))
		for _, uid := range added {
			newUIDs[uid] = true
		}
		for _, msg := range m.messages {
			if newUIDs[msg.uid] {
				msg.recent = false
			}
		}
	}
	for selection, updater := range m.watchers {
		if selection.recent == nil {
			selection.recent = make(map[imap.UID]bool)
		}
		before := len(selection.recent)
		if owner == nil || owner == selection {
			for _, uid := range added {
				selection.recent[uid] = true
			}
		}
		changes := make([]imapserver.Update, 0, len(batch.Changes)+1)
		for _, change := range batch.Changes {
			switch value := change.(type) {
			case *imapserver.UpdateExpunge:
				delete(selection.recent, value.UID)
			case *imapserver.UpdateFlags:
				change = &imapserver.UpdateFlags{UID: value.UID, Flags: selection.recentFlags(value.UID, value.Flags), ModSeq: value.ModSeq}
			case *imapserver.UpdateMailboxFlags:
				change = &imapserver.UpdateMailboxFlags{Flags: append(cloneFlags(value.Flags), imap.FlagRecent)}
			}
			changes = append(changes, change)
		}
		if before != len(selection.recent) {
			changes = append(changes, &imapserver.UpdateRecent{NumRecent: uint32(len(selection.recent))})
		}
		_ = updater.Push(&imapserver.UpdateBatch{Before: batch.Before, After: batch.After, Origin: batch.Origin, Changes: changes})
	}
}
