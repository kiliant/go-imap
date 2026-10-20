package maildir

import (
	"iter"
	"slices"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

// TombstoneFloor is the largest forgotten expunge mod-sequence. A request at
// or above it needs only retained history; an older request must receive every
// currently missing UID in its scope (RFC 7162 section 3.2.6).
func (a *account) pruneHistory(state *accountState) {
	limit := a.backend.maxTombstones
	if limit <= 0 {
		return
	}
	for name, m := range state.Mailboxes {
		if len(m.Tombstones) <= limit {
			continue
		}
		copy := *m
		m = &copy
		state.Mailboxes[name] = m
		cut := len(m.Tombstones) - limit
		for _, dead := range m.Tombstones[:cut] {
			m.TombstoneFloor = max(m.TombstoneFloor, dead.ModSeq)
		}
		m.Tombstones = slices.Clone(m.Tombstones[cut:])
	}
}

// A matching sequence/UID sample proves that the client already knows about
// removals through that UID. Iterate at most the live mailbox length, even if
// the supplied sets cover the entire 32-bit number space.
func knownMatchFloor(m *mailboxState, params *imapserver.QResyncSelect) imap.UID {
	nextUID, stop := iter.Pull(params.SeqMatchUIDs.All())
	defer stop()
	var floor imap.UID
	for seq := range params.SeqMatchSeqNums.All() {
		if uint64(seq) > uint64(len(m.Messages)) {
			break
		}
		uid, ok := nextUID()
		if !ok {
			break
		}
		if seq != 0 && m.Messages[seq-1].UID == uid {
			floor = max(floor, uid)
		}
	}
	return floor
}

// missingUIDs constructs gaps without expanding the UID number space. A
// mailbox with UIDNEXT near 2^32 still costs only its live-message count.
func missingUIDs(m *mailboxState) imap.UIDSet {
	var missing imap.UIDSet
	next := imap.UID(1)
	for _, msg := range m.Messages {
		if next < msg.UID {
			missing = append(missing, imap.UIDRange{Start: next, Stop: msg.UID - 1})
		}
		next = msg.UID + 1
	}
	if next < m.UIDNext {
		missing = append(missing, imap.UIDRange{Start: next, Stop: m.UIDNext - 1})
	}
	return missing
}

func intersectUIDs(a, b imap.UIDSet) imap.UIDSet {
	a, b = a.Normalized(), b.Normalized()
	var out imap.UIDSet
	for i, j := 0, 0; i < len(a) && j < len(b); {
		start, stop := max(a[i].Start, b[j].Start), min(a[i].Stop, b[j].Stop)
		if start <= stop {
			out = append(out, imap.UIDRange{Start: start, Stop: stop})
		}
		if a[i].Stop < b[j].Stop {
			i++
		} else {
			j++
		}
	}
	return out
}
