package maildir

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/kiliant/go-imap"
)

const stateVersion = 2
const stateFile = ".imap-state.json"
const journalFile = ".imap-journal.json"
const identityFile = ".imap-identity.json"

type extraFields map[string]json.RawMessage

type accountState struct {
	Version       int                                      `json:"version"`
	Mailboxes     map[string]*mailboxState                 `json:"mailboxes"`
	Subscriptions map[string]bool                          `json:"subscriptions"`
	Metadata      map[imap.MetadataEntryName]metadataValue `json:"metadata"`
	Extra         extraFields                              `json:"-"`
}
type mailboxState struct {
	UIDValidity   uint32                                   `json:"uidvalidity"`
	UIDNext       imap.UID                                 `json:"uidnext"`
	Revision      uint64                                   `json:"revision"`
	HighestModSeq uint64                                   `json:"highestmodseq"`
	Flags         []imap.Flag                              `json:"flags"`
	SpecialUse    []imap.MailboxAttr                       `json:"specialuse"`
	Messages      []*messageRecord                         `json:"messages"`
	Tombstones    []tombstone                              `json:"tombstones"`
	Metadata      map[imap.MetadataEntryName]metadataValue `json:"metadata"`
	Extra         extraFields                              `json:"-"`
}
type messageRecord struct {
	UID    imap.UID    `json:"uid"`
	File   string      `json:"file"`
	Size   int64       `json:"size"`
	Date   time.Time   `json:"date"`
	Flags  []imap.Flag `json:"flags"`
	ModSeq uint64      `json:"modseq"`
	Extra  extraFields `json:"-"`
}
type tombstone struct {
	UID    imap.UID    `json:"uid"`
	ModSeq uint64      `json:"modseq"`
	Extra  extraFields `json:"-"`
}
type identityState struct {
	Version int         `json:"version"`
	Next    uint64      `json:"next"`
	Extra   extraFields `json:"-"`
}

// Unknown fields survive rewriting at every persistent object boundary.
func marshalExtra(value any, extra extraFields) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	for key, value := range extra {
		if _, known := object[key]; !known {
			object[key] = value
		}
	}
	return json.Marshal(object)
}
func unmarshalExtra(raw []byte, value any) (extraFields, error) {
	if err := json.Unmarshal(raw, value); err != nil {
		return nil, err
	}
	var object extraFields
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	typ := reflect.TypeOf(value).Elem()
	for i := 0; i < typ.NumField(); i++ {
		key := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if key != "-" {
			delete(object, key)
		}
	}
	return object, nil
}
func (s accountState) MarshalJSON() ([]byte, error) {
	type plain accountState
	return marshalExtra(plain(s), s.Extra)
}
func (s *accountState) UnmarshalJSON(b []byte) error {
	type plain accountState
	e, err := unmarshalExtra(b, (*plain)(s))
	s.Extra = e
	return err
}
func (s mailboxState) MarshalJSON() ([]byte, error) {
	type plain mailboxState
	return marshalExtra(plain(s), s.Extra)
}
func (s *mailboxState) UnmarshalJSON(b []byte) error {
	type plain mailboxState
	e, err := unmarshalExtra(b, (*plain)(s))
	s.Extra = e
	return err
}
func (s messageRecord) MarshalJSON() ([]byte, error) {
	type plain messageRecord
	return marshalExtra(plain(s), s.Extra)
}
func (s *messageRecord) UnmarshalJSON(b []byte) error {
	type plain messageRecord
	e, err := unmarshalExtra(b, (*plain)(s))
	s.Extra = e
	return err
}
func (s tombstone) MarshalJSON() ([]byte, error) {
	type plain tombstone
	return marshalExtra(plain(s), s.Extra)
}
func (s *tombstone) UnmarshalJSON(b []byte) error {
	type plain tombstone
	e, err := unmarshalExtra(b, (*plain)(s))
	s.Extra = e
	return err
}
func (s identityState) MarshalJSON() ([]byte, error) {
	type plain identityState
	return marshalExtra(plain(s), s.Extra)
}
func (s *identityState) UnmarshalJSON(b []byte) error {
	type plain identityState
	e, err := unmarshalExtra(b, (*plain)(s))
	s.Extra = e
	return err
}

func decodeState(raw []byte) (*accountState, error) {
	var s accountState
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	if s.Version < 1 || s.Version > stateVersion {
		return nil, fmt.Errorf("maildir: unsupported state version %d", s.Version)
	}
	if s.Mailboxes == nil {
		s.Mailboxes = make(map[string]*mailboxState)
	}
	if s.Subscriptions == nil {
		s.Subscriptions = make(map[string]bool)
	}
	if s.Metadata == nil {
		s.Metadata = make(map[imap.MetadataEntryName]metadataValue)
	}
	seen := make(map[uint32]bool)
	for name, m := range s.Mailboxes {
		if validMailbox(name) != nil || mailboxKey(name) != name || m == nil || m.UIDValidity == 0 || m.UIDNext == 0 || seen[m.UIDValidity] {
			return nil, fmt.Errorf("maildir: invalid mailbox state")
		}
		seen[m.UIDValidity] = true
		if s.Version == 1 {
			if m.HighestModSeq == 0 {
				m.HighestModSeq = 1
			}
			if m.Revision == 0 {
				m.Revision = 1
			}
		}
		if m.Revision == 0 || m.HighestModSeq == 0 || m.HighestModSeq > math.MaxInt64 {
			return nil, fmt.Errorf("maildir: invalid mailbox revision")
		}
		if m.Metadata == nil {
			m.Metadata = make(map[imap.MetadataEntryName]metadataValue)
		}
		previous := imap.UID(0)
		files := make(map[string]bool)
		for _, msg := range m.Messages {
			if msg == nil || msg.UID <= previous || msg.UID >= m.UIDNext || filepath.Base(msg.File) != msg.File || !localPath(msg.File) || msg.Size < 0 || files[msg.File] {
				return nil, fmt.Errorf("maildir: invalid message state")
			}
			if s.Version == 1 && msg.ModSeq == 0 {
				msg.ModSeq = 1
			}
			if msg.ModSeq == 0 || msg.ModSeq > m.HighestModSeq {
				return nil, fmt.Errorf("maildir: invalid message modification sequence")
			}
			files[msg.File] = true
			previous = msg.UID
		}
		for _, dead := range m.Tombstones {
			if dead.UID == 0 || dead.UID >= m.UIDNext || dead.ModSeq == 0 || dead.ModSeq > m.HighestModSeq {
				return nil, fmt.Errorf("maildir: invalid tombstone")
			}
		}
	}
	s.Version = stateVersion
	return &s, nil
}

func cloneState(s *accountState) (*accountState, error) {
	b, e := json.Marshal(s)
	if e != nil {
		return nil, e
	}
	return decodeState(b)
}

type fileAction struct{ Kind, From, To, Stage string }
type diskJournal struct {
	Version int
	State   *accountState
	Actions []fileAction
}

func (a *account) reserveIdentity() (uint32, error) {
	if a.identity.Next == 0 || a.identity.Next > math.MaxUint32 {
		return 0, fmt.Errorf("maildir: UIDVALIDITY space exhausted")
	}
	id := uint32(a.identity.Next)
	next := a.identity
	next.Next++
	raw, err := json.Marshal(next)
	if err != nil {
		return 0, err
	}
	if err := atomicFile(a.root, identityFile, raw); err != nil {
		return 0, err
	}
	a.identity = next
	return id, nil
}

func (a *account) newMailbox() (*mailboxState, error) {
	id, err := a.reserveIdentity()
	if err != nil {
		return nil, err
	}
	return &mailboxState{UIDValidity: id, UIDNext: 1, Revision: 1, HighestModSeq: 1, Flags: defaultFlags(), Metadata: make(map[imap.MetadataEntryName]metadataValue)}, nil
}

func (a *account) commit(next *accountState, actions []fileAction) error {
	if a.state == nil || !sameMailboxNames(a.state, next) {
		if err := a.checkMailboxLayout(next); err != nil {
			return noError(imap.CodeCannot, err.Error())
		}
	}
	// Replay fails closed on a rename whose source and destination are both
	// missing, so a message file removed outside the backend after it was read
	// would make this journal unreplayable on every Open, and Open fails if
	// any account fails to load. Refusing before the journal exists keeps the
	// account usable; the next poll reconciles the removal as an expunge.
	for _, action := range actions {
		if action.Kind == "merge" || action.Kind == "relocate" {
			if err := checkMergeTree(a.root, action.From, action.To); err != nil {
				return noError(imap.CodeCannot, err.Error())
			}
		}
		if action.Kind != "rename" || strings.HasPrefix(action.From, ".staging") {
			continue
		}
		if _, err := a.root.Lstat(action.From); os.IsNotExist(err) {
			return noError(imap.CodeExpungeIssued, "message was removed outside the server")
		} else if err != nil {
			return err
		}
	}
	j := diskJournal{Version: stateVersion, State: next, Actions: actions}
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	// Once the journal is durable, recovery must roll forward. Any later error
	// poisons this account until reopened, so no reader sees half a transaction.
	if err = atomicFile(a.root, journalFile, raw); err != nil {
		a.failure = err
		return err
	}
	if err = a.backend.fault("journal"); err == nil {
		err = a.replay(&j)
	}
	if err != nil {
		a.failure = err
		return err
	}
	a.state = next
	return nil
}

func sameMailboxNames(a, b *accountState) bool {
	if len(a.Mailboxes) != len(b.Mailboxes) {
		return false
	}
	for name := range a.Mailboxes {
		if b.Mailboxes[name] == nil {
			return false
		}
	}
	return true
}

func (a *account) replay(j *diskJournal) error {
	for _, action := range j.Actions {
		if err := a.applyAction(action); err != nil {
			return err
		}
		if err := a.backend.fault("action"); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(j.State)
	if err != nil {
		return err
	}
	if err = atomicFile(a.root, stateFile, raw); err != nil {
		return err
	}
	if err = a.backend.fault("state"); err != nil {
		return err
	}
	if err = a.root.Remove(journalFile); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err = syncDir(a.root, "."); err != nil {
		return err
	}
	for _, action := range j.Actions {
		if action.Kind == "relocate" {
			_ = a.root.Remove(action.Stage + ".moved")
		}
	}
	return a.backend.fault("complete")
}

func (a *account) applyAction(action fileAction) error {
	for _, path := range []string{action.From, action.To, action.Stage} {
		if path != "" {
			if err := checkedPath(a.root, path); err != nil {
				return err
			}
		}
	}
	switch action.Kind {
	case "mkdir":
		return mkdirAll(a.root, action.To)
	case "rename":
		if _, err := a.root.Lstat(action.From); os.IsNotExist(err) {
			_, err = a.root.Lstat(action.To)
			return err
		} else if err != nil {
			return err
		}
		if err := mkdirAll(a.root, filepath.Dir(action.To)); err != nil {
			return err
		}
		if err := renameFile(a.root, action.From, action.To); err != nil {
			return err
		}
		if err := syncDir(a.root, filepath.Dir(action.From)); err != nil {
			return err
		}
		return syncDir(a.root, filepath.Dir(action.To))
	case "remove":
		if err := removeTree(a.root, action.From); err != nil {
			return err
		}
		return syncDir(a.root, filepath.Dir(action.From))
	case "merge":
		return mergeTree(a.root, action.From, action.To)
	case "relocate":
		// A single replayable action owns the intermediate directory. Once
		// consumed by the merge, it must not be mistaken for lost message data.
		marker := action.Stage + ".moved"
		if _, err := a.root.Lstat(marker); os.IsNotExist(err) {
			if _, err := a.root.Lstat(action.Stage); os.IsNotExist(err) {
				if err := renameFile(a.root, action.From, action.Stage); err != nil {
					return err
				}
				if err := syncDir(a.root, filepath.Dir(action.From)); err != nil {
					return err
				}
				if err := syncDir(a.root, filepath.Dir(action.Stage)); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			if err := atomicFile(a.root, marker, []byte("moved\n")); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := a.backend.fault("relocate-stage"); err != nil {
			return err
		}
		if _, err := a.root.Lstat(action.Stage); os.IsNotExist(err) {
			_, err = a.root.Lstat(action.To)
			return err
		} else if err != nil {
			return err
		}
		if err := mergeTree(a.root, action.Stage, action.To); err != nil {
			return err
		}
		return a.backend.fault("relocate-merged")
	default:
		return fmt.Errorf("maildir: invalid journal action")
	}
}

func removeTree(root *os.Root, path string) error {
	info, err := root.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		entries, err := readDir(root, path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := removeTree(root, filepath.Join(path, entry.Name())); err != nil {
				return err
			}
		}
	}
	return root.Remove(path)
}

func mergeTree(root *os.Root, from, to string) error {
	entries, err := readDir(root, from)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := mkdirAll(root, to); err != nil {
		return err
	}
	for _, entry := range entries {
		src, dst := filepath.Join(from, entry.Name()), filepath.Join(to, entry.Name())
		if entry.IsDir() {
			if err := mergeTree(root, src, dst); err != nil {
				return err
			}
			continue
		}
		if _, err := root.Lstat(dst); err == nil {
			return fmt.Errorf("maildir: rename destination collision")
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := renameFile(root, src, dst); err != nil {
			return err
		}
	}
	if err := syncDir(root, to); err != nil {
		return err
	}
	if err := root.Remove(from); err != nil {
		return err
	}
	return syncDir(root, filepath.Dir(from))
}

func (a *account) load() error {
	stateRaw, stateErr := readFile(a.root, stateFile)
	var state *accountState
	var err error
	if stateErr == nil {
		state, err = decodeState(stateRaw)
		if err != nil {
			return err
		}
	} else if !os.IsNotExist(stateErr) {
		return stateErr
	}
	journalRaw, journalErr := readFile(a.root, journalFile)
	var pending *diskJournal
	if journalErr == nil {
		var j diskJournal
		if err := json.Unmarshal(journalRaw, &j); err != nil {
			return err
		}
		if j.Version != stateVersion || j.State == nil {
			return fmt.Errorf("maildir: unsupported journal version")
		}
		raw, err := json.Marshal(j.State)
		if err != nil {
			return err
		}
		j.State, err = decodeState(raw)
		if err != nil {
			return err
		}
		pending = &j
	} else if !os.IsNotExist(journalErr) {
		return journalErr
	}
	idRaw, idErr := readFile(a.root, identityFile)
	if idErr == nil {
		if err := json.Unmarshal(idRaw, &a.identity); err != nil {
			return err
		}
		if a.identity.Version != 1 || a.identity.Next == 0 {
			return fmt.Errorf("maildir: unsupported identity allocator")
		}
	} else if os.IsNotExist(idErr) {
		if a.existing {
			return fmt.Errorf("maildir: registered account identity allocator is missing")
		}
		entries, err := readDir(a.root, ".")
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return fmt.Errorf("maildir: identity allocator missing; refusing possible UIDVALIDITY reuse")
		}
		a.identity = identityState{Version: 1, Next: 1}
		raw, _ := json.Marshal(a.identity)
		if err := atomicFile(a.root, identityFile, raw); err != nil {
			return err
		}
	} else {
		return idErr
	}
	if err := mkdirAll(a.root, ".staging"); err != nil {
		return err
	}
	if pending != nil {
		if err := a.checkMailboxLayout(pending.State); err != nil {
			return err
		}
		if err := a.replay(pending); err != nil {
			return err
		}
		state = pending.State
	}
	if err := mkdirAll(a.root, "mailboxes"); err != nil {
		return err
	}
	if state == nil {
		state = &accountState{Version: stateVersion, Mailboxes: make(map[string]*mailboxState), Subscriptions: make(map[string]bool), Metadata: make(map[imap.MetadataEntryName]metadataValue)}
		var names []string
		if err := a.discover("mailboxes", nil, &names); err != nil {
			return err
		}
		names = append(names, "INBOX")
		sort.Strings(names)
		for _, name := range names {
			if state.Mailboxes[name] != nil {
				continue
			}
			m, err := a.newMailbox()
			if err != nil {
				return err
			}
			state.Mailboxes[name] = m
		}
	}
	if err := a.checkMailboxLayout(state); err != nil {
		return err
	}
	for name, m := range state.Mailboxes {
		if uint64(m.UIDValidity) >= a.identity.Next {
			return fmt.Errorf("maildir: identity allocator is behind mailbox state")
		}
		for _, sub := range []string{"cur", "new", "tmp"} {
			if err := mkdirAll(a.root, filepath.Join(mailboxPath(name), sub)); err != nil {
				return err
			}
		}
	}
	a.state = state
	if err := a.reconcile(); err != nil {
		return err
	}
	staged, err := readDir(a.root, ".staging")
	if err != nil {
		return err
	}
	for _, entry := range staged {
		if err := removeTree(a.root, filepath.Join(".staging", entry.Name())); err != nil {
			return err
		}
	}
	// Also persists version-1 migrations, retaining unrecognised fields.
	raw, err := json.Marshal(a.state)
	if err != nil {
		return err
	}
	return atomicFile(a.root, stateFile, raw)
}

type rootRegistry struct {
	Version  int             `json:"version"`
	Accounts map[string]bool `json:"accounts"`
	Extra    extraFields     `json:"-"`
}

func (s rootRegistry) MarshalJSON() ([]byte, error) {
	type plain rootRegistry
	return marshalExtra(plain(s), s.Extra)
}
func (s *rootRegistry) UnmarshalJSON(raw []byte) error {
	type plain rootRegistry
	extra, err := unmarshalExtra(raw, (*plain)(s))
	s.Extra = extra
	return err
}
func loadRootRegistry(root *os.Root, fresh bool) (*rootRegistry, error) {
	raw, err := readFile(root, ".imap-root.json")
	if err == nil {
		var registry rootRegistry
		if err := json.Unmarshal(raw, &registry); err != nil {
			return nil, err
		}
		if registry.Version != 1 || registry.Accounts == nil {
			return nil, fmt.Errorf("maildir: unsupported root registry")
		}
		return &registry, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	entries, err := readDir(root, ".")
	if err != nil {
		return nil, err
	}
	if !fresh || len(entries) != 1 || entries[0].Name() != ".imap-lock" {
		return nil, fmt.Errorf("maildir: root registry missing; refusing possible identity reuse")
	}
	registry := &rootRegistry{Version: 1, Accounts: make(map[string]bool)}
	if err := saveRootRegistry(root, registry); err != nil {
		return nil, err
	}
	return registry, nil
}
func saveRootRegistry(root *os.Root, registry *rootRegistry) error {
	raw, err := json.Marshal(registry)
	if err != nil {
		return err
	}
	return atomicFile(root, ".imap-root.json", raw)
}

func (a *account) discover(path string, parts []string, names *[]string) error {
	entries, err := readDir(a.root, path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("maildir: symbolic links are not supported")
		}
		if !entry.IsDir() {
			continue
		}
		if entry.Name() == "cur" {
			if len(parts) > 0 {
				*names = append(*names, strings.Join(parts, "/"))
			}
			continue
		}
		if entry.Name() == "new" || entry.Name() == "tmp" {
			continue
		}
		segment, err := unescapeSegment(entry.Name())
		if err != nil {
			return err
		}
		if err := a.discover(filepath.Join(path, entry.Name()), append(append([]string(nil), parts...), segment), names); err != nil {
			return err
		}
	}
	return nil
}

// Check the complete relocation before preparing a roll-forward journal.
func checkMergeTree(root *os.Root, from, to string) error {
	entries, err := readDir(root, from)
	if err != nil {
		return err
	}
	info, err := root.Lstat(to)
	destinationMissing := os.IsNotExist(err)
	if err != nil && !destinationMissing {
		return err
	}
	if !destinationMissing && !info.IsDir() {
		return fmt.Errorf("maildir: rename destination is not a directory")
	}
	for _, entry := range entries {
		src, dst := filepath.Join(from, entry.Name()), filepath.Join(to, entry.Name())
		if entry.IsDir() {
			if err := checkMergeTree(root, src, dst); err != nil {
				return err
			}
		} else if !entry.Type().IsRegular() {
			return fmt.Errorf("maildir: rename source contains a nonregular file")
		} else if _, err := root.Lstat(dst); err == nil {
			return fmt.Errorf("maildir: rename destination collision")
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
