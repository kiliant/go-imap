package maildir

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/kiliant/go-imap"
)

const changePrefix = ".imap-change-"
const maxChanges = 64
const headFile = ".imap-head.json"

// The durable head detects a missing tail change, preventing UID reuse after
// metadata loss. A prepared journal explains a head one transaction ahead.
type changeHead struct {
	Version    int    `json:"version"`
	Generation uint32 `json:"generation"`
	Sequence   uint64 `json:"sequence"`
}

func (a *account) writeHead(state *accountState) error {
	raw, err := json.Marshal(changeHead{stateVersion, state.Generation, state.Sequence})
	if err != nil {
		return err
	}
	if err := atomicFile(a.root, headFile, raw); err != nil {
		return err
	}
	return a.backend.fault("head")
}

func (a *account) validateHead(state *accountState, pending *diskJournal) error {
	raw, err := readFile(a.root, headFile)
	if err != nil {
		return err
	}
	var head changeHead
	if err := json.Unmarshal(raw, &head); err != nil {
		return err
	}
	if head.Version != stateVersion || head.Generation != state.Generation {
		return fmt.Errorf("maildir: unsupported or mismatched state head")
	}
	if head.Sequence == state.Sequence {
		return nil
	}
	if pending != nil {
		if pending.Change != nil && head.Sequence == pending.Change.Header.Sequence {
			return nil
		}
		if pending.State != nil && head.Sequence == pending.State.Sequence {
			return nil
		}
	}
	return fmt.Errorf("maildir: missing incremental state at sequence %d", head.Sequence)
}

// Message records are immutable between transactions. Mutators replace a
// changed record; copying pointer slices keeps rollback independent without
// serializing, parsing or copying every message in the account. Only named
// mailboxes are copied; no names copies account-level maps alone.
func mutationState(s *accountState, names ...string) *accountState {
	next := *s
	next.Mailboxes = maps.Clone(s.Mailboxes)
	next.Metadata = maps.Clone(s.Metadata)
	next.Subscriptions = maps.Clone(s.Subscriptions)
	for _, name := range names {
		m := s.Mailboxes[name]
		if m == nil {
			continue
		}
		copy := *m
		copy.Messages = slices.Clone(m.Messages)
		copy.Flags = slices.Clone(m.Flags)
		copy.SpecialUse = slices.Clone(m.SpecialUse)
		copy.Tombstones = slices.Clone(m.Tombstones)
		copy.Metadata = maps.Clone(m.Metadata)
		next.Mailboxes[name] = &copy
	}
	return &next
}

type stateChange struct {
	Header    accountState              `json:"header"`
	Mailboxes map[string]*mailboxChange `json:"mailboxes"`
}

type mailboxChange struct {
	Header mailboxState     `json:"header"`
	Upsert []*messageRecord `json:"upsert,omitempty"`
	Remove []imap.UID       `json:"remove,omitempty"`
}

func makeChange(before, after *accountState) *stateChange {
	d := &stateChange{Header: *after, Mailboxes: make(map[string]*mailboxChange)}
	d.Header.Mailboxes = nil
	for name := range before.Mailboxes {
		if after.Mailboxes[name] == nil {
			d.Mailboxes[name] = nil
		}
	}
	for name, m := range after.Mailboxes {
		if before.Mailboxes[name] == m {
			continue
		}
		change := &mailboxChange{Header: *m}
		change.Header.Messages = nil
		old := before.Mailboxes[name]
		if old == nil || old.UIDValidity != m.UIDValidity {
			change.Upsert = m.Messages
			d.Mailboxes[name] = change
			continue
		}
		i, j := 0, 0
		for i < len(old.Messages) || j < len(m.Messages) {
			switch {
			case j == len(m.Messages) || i < len(old.Messages) && old.Messages[i].UID < m.Messages[j].UID:
				change.Remove = append(change.Remove, old.Messages[i].UID)
				i++
			case i == len(old.Messages) || m.Messages[j].UID < old.Messages[i].UID:
				change.Upsert = append(change.Upsert, m.Messages[j])
				j++
			default:
				if old.Messages[i] != m.Messages[j] && !reflect.DeepEqual(old.Messages[i], m.Messages[j]) {
					change.Upsert = append(change.Upsert, m.Messages[j])
				}
				i++
				j++
			}
		}
		header := *old
		header.Messages = nil
		if len(change.Upsert) != 0 || len(change.Remove) != 0 || !reflect.DeepEqual(header, change.Header) {
			d.Mailboxes[name] = change
		}
	}
	return d
}

func applyChange(before *accountState, d *stateChange) (*accountState, error) {
	if before == nil || d.Header.Version != stateVersion || d.Header.Generation != before.Generation || before.Sequence == math.MaxUint64 || d.Header.Sequence != before.Sequence+1 || d.Header.Mailboxes != nil {
		return nil, fmt.Errorf("maildir: invalid incremental state sequence")
	}
	next := d.Header
	next.Mailboxes = maps.Clone(before.Mailboxes)
	for name, change := range d.Mailboxes {
		if change == nil {
			delete(next.Mailboxes, name)
			continue
		}
		m := change.Header
		if m.Messages != nil {
			return nil, fmt.Errorf("maildir: messages in incremental header")
		}
		var previous []*messageRecord
		if old := before.Mailboxes[name]; old != nil && old.UIDValidity == m.UIDValidity {
			previous = old.Messages
		}
		for i, msg := range change.Upsert {
			if msg == nil || msg.UID == 0 || i > 0 && change.Upsert[i-1].UID >= msg.UID {
				return nil, fmt.Errorf("maildir: unordered incremental messages")
			}
		}
		for i, uid := range change.Remove {
			if uid == 0 || i > 0 && change.Remove[i-1] >= uid {
				return nil, fmt.Errorf("maildir: unordered incremental removals")
			}
		}
		m.Messages = make([]*messageRecord, 0, len(previous)+len(change.Upsert))
		i, j, k := 0, 0, 0
		for i < len(previous) || j < len(change.Upsert) {
			var msg *messageRecord
			switch {
			case j == len(change.Upsert) || i < len(previous) && previous[i].UID < change.Upsert[j].UID:
				msg = previous[i]
				i++
			case i == len(previous) || change.Upsert[j].UID < previous[i].UID:
				msg = change.Upsert[j]
				j++
			default:
				msg = change.Upsert[j]
				i++
				j++
			}
			for k < len(change.Remove) && change.Remove[k] < msg.UID {
				k++
			}
			if k == len(change.Remove) || change.Remove[k] != msg.UID {
				m.Messages = append(m.Messages, msg)
			}
		}
		if len(m.Messages) == 0 {
			m.Messages = nil
		}
		next.Mailboxes[name] = &m
	}
	return &next, nil
}

func changeFile(sequence uint64) string { return fmt.Sprintf("%s%020d.json", changePrefix, sequence) }

func changeSequence(name string) (uint64, error) {
	value := strings.TrimSuffix(strings.TrimPrefix(name, changePrefix), ".json")
	seq, err := strconv.ParseUint(value, 10, 64)
	if err != nil || changeFile(seq) != name || seq == 0 {
		return 0, fmt.Errorf("maildir: invalid change filename %q", name)
	}
	return seq, nil
}

// Checkpoint publication precedes deleting covered changes. A crash during
// cleanup leaves harmless files whose sequence is at/below the checkpoint.
func (a *account) checkpoint(state *accountState) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := a.writeHead(state); err != nil {
		return err
	}
	if err := atomicFile(a.root, stateFile, raw); err != nil {
		return err
	}
	if err := a.backend.fault("checkpoint"); err != nil {
		return err
	}
	entries, err := readDir(a.root, ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), changePrefix) {
			continue
		}
		if _, err := changeSequence(entry.Name()); err != nil {
			return err
		}
		// Every published change is covered by this checkpoint. Files from a
		// previous incarnation (missing-state reconstruction) are retired too.
		if err := a.root.Remove(entry.Name()); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := syncDir(a.root, "."); err != nil {
		return err
	}
	a.checkpointBytes, a.changeBytes, a.changeCount = int64(len(raw)), 0, 0
	return nil
}

func (a *account) loadChanges(state *accountState, pending *diskJournal) (*accountState, error) {
	entries, err := readDir(a.root, ".")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, changePrefix) {
			continue
		}
		base, _, _ := strings.Cut(name, ".tmp-")
		if _, err := changeSequence(base); err != nil {
			return nil, err
		}
		if base != name {
			if err := cleanupAtomicFiles(a.root, base); err != nil {
				return nil, err
			}
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		seq, _ := changeSequence(name)
		raw, err := readFile(a.root, name)
		if err != nil {
			return nil, err
		}
		var change stateChange
		if err := json.Unmarshal(raw, &change); err != nil {
			return nil, err
		}
		if change.Header.Version != stateVersion {
			return nil, fmt.Errorf("maildir: unsupported change version")
		}
		if change.Header.Sequence != seq {
			return nil, fmt.Errorf("maildir: change filename/sequence mismatch")
		}
		// Missing state gets fresh identities, never a suffix of an old history.
		if state == nil || change.Header.Generation != state.Generation || seq <= state.Sequence {
			continue
		}
		if pending != nil && pending.Change != nil && seq == pending.Change.Header.Sequence {
			continue
		}
		state, err = applyChange(state, &change)
		if err != nil {
			return nil, err
		}
		a.changeBytes += int64(len(raw))
		a.changeCount++
		if a.changeCount > maxChanges || a.checkpointBytes+a.changeBytes > maxStateFileBytes {
			return nil, noError(imap.CodeLimit, "incremental state exceeds storage budget")
		}
	}
	return state, nil
}

func (a *account) commitIncremental(next *accountState, actions []fileAction) error {
	if a.state.Sequence == math.MaxUint64 {
		return noError(imap.CodeLimit, "transaction sequence exhausted")
	}
	// Direct state replacement is used during provisioning and by import tools.
	if next == a.state || a.checkpointBytes == 0 {
		return a.commitWithLimit(next, actions, maxStateFileBytes)
	}
	next.Sequence = a.state.Sequence + 1
	a.pruneHistory(next)
	change := makeChange(a.state, next)
	rawChange, err := json.Marshal(change)
	if err != nil {
		return err
	}
	// The checkpoint plus all changes conservatively bounds the reconstructed
	// state: changes encode every added/replaced value with extra framing.
	if a.changeCount >= maxChanges || a.changeBytes+int64(len(rawChange)) > 8<<20 || a.checkpointBytes+a.changeBytes+int64(len(rawChange)) > maxStateFileBytes {
		if err := a.checkpoint(a.state); err != nil {
			return err
		}
	}
	if a.checkpointBytes+a.changeBytes+int64(len(rawChange)) > maxStateFileBytes {
		// A shrinking transaction can fit even when the conservative bound
		// does not. Use the complete journal path to admit it safely.
		return a.commitWithLimit(next, actions, maxStateFileBytes)
	}
	return a.commitJournal(&diskJournal{Version: stateVersion, State: next, Change: change, Actions: actions}, maxStateFileBytes)
}

func (a *account) persistJournalState(j *diskJournal) error {
	if j.Change == nil {
		return a.checkpoint(j.State)
	}
	// Recovery may have recorded a vanished cur-to-cur source as an expunge.
	// Regenerate from the pre-transaction state so that result is durable too.
	j.Change = makeChange(a.state, j.State)
	raw, err := json.Marshal(j.Change)
	if err != nil {
		return err
	}
	if err := atomicFile(a.root, changeFile(j.State.Sequence), raw); err != nil {
		return err
	}
	a.changeBytes += int64(len(raw))
	a.changeCount++
	return a.writeHead(j.State)
}
