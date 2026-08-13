package imapserver

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kiliant/go-imap"
)

const maxSASLSteps = 16

type writerCore[T any] struct {
	mu     sync.RWMutex
	write  func(context.Context, T) error
	active bool
}

func newWriterCore[T any](write func(context.Context, T) error) *writerCore[T] {
	return &writerCore[T]{write: write, active: write != nil}
}

func (w *writerCore[T]) writeValue(ctx context.Context, value T) error {
	if w == nil {
		return ErrWriterClosed
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	if !w.active || w.write == nil {
		return ErrWriterClosed
	}
	return w.write(ctx, value)
}

func (w *writerCore[T]) close() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.active = false
	w.write = nil
	w.mu.Unlock()
}

//lint:ignore U1000 T22 command handlers construct and close framework writers around backend calls.
func newListWriter(write func(context.Context, *imap.ListData) error) *ListWriter {
	return &ListWriter{core: newWriterCore(write)}
}

//lint:ignore U1000 T22 command handlers construct and close framework writers around backend calls.
func newFetchWriter(write func(context.Context, *imap.FetchMessageData) error) *FetchWriter {
	return &FetchWriter{core: newWriterCore(write)}
}

//lint:ignore U1000 T22 command handlers construct and close framework writers around backend calls.
func newExpungeWriter(write func(context.Context, imap.UID) error) *ExpungeWriter {
	return &ExpungeWriter{core: newWriterCore(write)}
}

type saslServer struct {
	mechanism string
	step      int
	username  string
}

func newSASLServer(mechanism string) (*saslServer, error) {
	mechanism = strings.ToUpper(mechanism)
	switch mechanism {
	case "PLAIN", "LOGIN", "XOAUTH2", "OAUTHBEARER":
		return &saslServer{mechanism: mechanism}, nil
	default:
		return nil, fmt.Errorf("imapserver: unsupported SASL mechanism %q", mechanism)
	}
}

func (s *saslServer) initialChallenge() []byte {
	if s != nil && s.mechanism == "LOGIN" {
		return []byte("Username:")
	}
	return nil
}

func (s *saslServer) next(response []byte) (*Credentials, []byte, error) {
	if s == nil || s.step >= maxSASLSteps {
		return nil, nil, fmt.Errorf("imapserver: invalid SASL exchange state")
	}
	s.step++
	switch s.mechanism {
	case "PLAIN":
		if s.step != 1 {
			return nil, nil, fmt.Errorf("imapserver: unexpected PLAIN response")
		}
		parts := strings.Split(string(response), "\x00")
		if len(parts) != 3 || parts[1] == "" || !validCredentialField(parts[0], false) ||
			!validCredentialField(parts[1], false) || !validCredentialField(parts[2], false) {
			return nil, nil, fmt.Errorf("imapserver: malformed PLAIN response")
		}
		return &Credentials{Mechanism: s.mechanism, AuthzID: parts[0], Username: parts[1], Password: parts[2]}, nil, nil
	case "LOGIN":
		switch s.step {
		case 1:
			username := string(response)
			if username == "" || !validCredentialField(username, false) {
				return nil, nil, fmt.Errorf("imapserver: malformed LOGIN username")
			}
			s.username = username
			return nil, []byte("Password:"), nil
		case 2:
			password := string(response)
			if !validCredentialField(password, false) {
				return nil, nil, fmt.Errorf("imapserver: malformed LOGIN password")
			}
			return &Credentials{Mechanism: s.mechanism, Username: s.username, Password: password}, nil, nil
		default:
			return nil, nil, fmt.Errorf("imapserver: unexpected LOGIN response")
		}
	case "XOAUTH2":
		if s.step != 1 {
			return nil, nil, fmt.Errorf("imapserver: unexpected XOAUTH2 response")
		}
		return parseXOAUTH2(response)
	case "OAUTHBEARER":
		if s.step != 1 {
			return nil, nil, fmt.Errorf("imapserver: unexpected OAUTHBEARER response")
		}
		return parseOAUTHBEARER(response)
	default:
		return nil, nil, fmt.Errorf("imapserver: unsupported SASL mechanism")
	}
}

func validCredentialField(value string, rejectSOH bool) bool {
	return utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n") &&
		(!rejectSOH || !strings.ContainsRune(value, '\x01'))
}

func parseXOAUTH2(response []byte) (*Credentials, []byte, error) {
	parts := strings.Split(string(response), "\x01")
	if len(parts) != 4 || parts[2] != "" || parts[3] != "" || !strings.HasPrefix(parts[0], "user=") ||
		!strings.HasPrefix(parts[1], "auth=Bearer ") {
		return nil, nil, fmt.Errorf("imapserver: malformed XOAUTH2 response")
	}
	username := strings.TrimPrefix(parts[0], "user=")
	token := strings.TrimPrefix(parts[1], "auth=Bearer ")
	if username == "" || token == "" || !validCredentialField(username, true) || !validCredentialField(token, true) {
		return nil, nil, fmt.Errorf("imapserver: malformed XOAUTH2 response")
	}
	return &Credentials{Mechanism: "XOAUTH2", Username: username, Token: token}, nil, nil
}

func parseOAUTHBEARER(response []byte) (*Credentials, []byte, error) {
	fields := strings.Split(string(response), "\x01")
	if len(fields) < 4 || fields[len(fields)-1] != "" || fields[len(fields)-2] != "" {
		return nil, nil, fmt.Errorf("imapserver: malformed OAUTHBEARER response")
	}
	header := fields[0]
	if !strings.HasPrefix(header, "n,") || !strings.HasSuffix(header, ",") {
		return nil, nil, fmt.Errorf("imapserver: malformed OAUTHBEARER GS2 header")
	}
	authzID := ""
	middle := strings.TrimSuffix(strings.TrimPrefix(header, "n,"), ",")
	if middle != "" {
		if !strings.HasPrefix(middle, "a=") {
			return nil, nil, fmt.Errorf("imapserver: malformed OAUTHBEARER authorization identity")
		}
		var ok bool
		authzID, ok = unescapeSASLName(strings.TrimPrefix(middle, "a="))
		if !ok || authzID == "" || !validCredentialField(authzID, true) {
			return nil, nil, fmt.Errorf("imapserver: malformed OAUTHBEARER authorization identity")
		}
	}
	token := ""
	for _, field := range fields[1 : len(fields)-2] {
		switch {
		case strings.HasPrefix(field, "auth=Bearer ") && token == "":
			token = strings.TrimPrefix(field, "auth=Bearer ")
		case strings.HasPrefix(field, "host=") || strings.HasPrefix(field, "port="):
		default:
			return nil, nil, fmt.Errorf("imapserver: malformed OAUTHBEARER field")
		}
	}
	if token == "" || !validCredentialField(token, true) {
		return nil, nil, fmt.Errorf("imapserver: malformed OAUTHBEARER bearer token")
	}
	return &Credentials{Mechanism: "OAUTHBEARER", AuthzID: authzID, Username: authzID, Token: token}, nil, nil
}

func unescapeSASLName(value string) (string, bool) {
	var result strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '=' {
			result.WriteByte(value[i])
			continue
		}
		if i+2 >= len(value) {
			return "", false
		}
		switch strings.ToUpper(value[i+1 : i+3]) {
		case "2C":
			result.WriteByte(',')
		case "3D":
			result.WriteByte('=')
		default:
			return "", false
		}
		i += 2
	}
	return result.String(), true
}

//lint:ignore U1000 T22's AUTHENTICATE handler uses this server-side SASL wire encoding.
func encodeSASL(value []byte) string {
	if len(value) == 0 {
		return "="
	}
	return base64.StdEncoding.EncodeToString(value)
}

func decodeSASL(value string) ([]byte, bool, error) {
	if value == "*" {
		return nil, true, nil
	}
	if value == "" || value == "=" {
		return []byte{}, false, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, false, fmt.Errorf("imapserver: malformed base64 SASL response")
	}
	return decoded, false, nil
}

//lint:ignore U1000 T22's AUTHENTICATE handler drives this reader-owned continuation exchange.
func (c *conn) continueSASL(ctx context.Context, challenge []byte) ([]byte, bool, error) {
	line, err := c.continueLine(ctx, encodeSASL(challenge))
	if err != nil {
		return nil, false, err
	}
	return decodeSASL(line)
}

type updaterCore struct {
	mu     sync.RWMutex
	queue  *updateQueue
	active bool
}

func newUpdater(queue *updateQueue) *Updater {
	return &Updater{core: &updaterCore{queue: queue, active: true}}
}

func (u *updaterCore) push(batch *UpdateBatch) error {
	if batch == nil {
		return fmt.Errorf("imapserver: nil update batch")
	}
	u.mu.RLock()
	if !u.active || u.queue == nil {
		u.mu.RUnlock()
		return ErrUpdaterClosed
	}
	queue := u.queue
	err := queue.push(batch)
	u.mu.RUnlock()
	return err
}

func (u *updaterCore) close() {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.active = false
	u.queue = nil
	u.mu.Unlock()
}

func cloneUpdateBatch(batch *UpdateBatch) *UpdateBatch {
	copyBatch := &UpdateBatch{
		Before:  batch.Before,
		After:   batch.After,
		Origin:  batch.Origin,
		Changes: make([]Update, 0, len(batch.Changes)),
	}
	for _, change := range batch.Changes {
		switch change := change.(type) {
		case *UpdateAdd:
			if change == nil {
				copyBatch.Changes = append(copyBatch.Changes, change)
			} else {
				copyBatch.Changes = append(copyBatch.Changes, &UpdateAdd{UIDs: slices.Clone(change.UIDs)})
			}
		case *UpdateFlags:
			if change == nil {
				copyBatch.Changes = append(copyBatch.Changes, change)
			} else {
				copyBatch.Changes = append(copyBatch.Changes, &UpdateFlags{UID: change.UID, Flags: slices.Clone(change.Flags), ModSeq: change.ModSeq})
			}
		case *UpdateExpunge:
			if change == nil {
				copyBatch.Changes = append(copyBatch.Changes, change)
			} else {
				copyBatch.Changes = append(copyBatch.Changes, &UpdateExpunge{UID: change.UID})
			}
		case *UpdateVanished:
			if change == nil {
				copyBatch.Changes = append(copyBatch.Changes, change)
			} else {
				copyBatch.Changes = append(copyBatch.Changes, &UpdateVanished{UIDs: slices.Clone(change.UIDs), Earlier: change.Earlier})
			}
		default:
			// Unknown values are retained so a future package version can decide
			// how to account for them. External packages cannot implement Update.
			copyBatch.Changes = append(copyBatch.Changes, change)
		}
	}
	return copyBatch
}

type updateQueue struct {
	mu       sync.Mutex
	items    []*UpdateBatch
	bytes    int64
	maxItems int
	maxBytes int64
	signal   chan struct{}
	overflow func()
	closed   bool
}

func newUpdateQueue(maxItems int, maxBytes int64, overflow func()) *updateQueue {
	return &updateQueue{
		maxItems: maxItems,
		maxBytes: maxBytes,
		signal:   make(chan struct{}, 1),
		overflow: overflow,
	}
}

func (q *updateQueue) push(batch *UpdateBatch) error {
	size := updateBatchSize(batch)
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return ErrUpdaterClosed
	}
	if size < 0 || (q.maxItems >= 0 && len(q.items) >= q.maxItems) ||
		(q.maxBytes >= 0 && size > q.maxBytes-q.bytes) {
		overflow := q.overflow
		q.closed = true
		q.mu.Unlock()
		if overflow != nil {
			overflow()
		}
		return ErrUpdateOverflow
	}
	// Bound the original value before cloning it: otherwise a backend could
	// force an allocation larger than the queue's byte limit on the Push path.
	q.items = append(q.items, cloneUpdateBatch(batch))
	q.bytes += size
	q.mu.Unlock()
	select {
	case q.signal <- struct{}{}:
	default:
	}
	return nil
}

func (q *updateQueue) popAll() []*UpdateBatch {
	q.mu.Lock()
	items := q.items
	q.items = nil
	q.bytes = 0
	q.mu.Unlock()
	return items
}

func (q *updateQueue) close() {
	if q == nil {
		return
	}
	q.mu.Lock()
	q.closed = true
	q.items = nil
	q.bytes = 0
	q.mu.Unlock()
}

func updateBatchSize(batch *UpdateBatch) int64 {
	const batchOverhead = 64
	if batch == nil {
		return batchOverhead
	}
	size := int64(batchOverhead + len(batch.Before) + len(batch.After))
	for _, change := range batch.Changes {
		switch change := change.(type) {
		case *UpdateAdd:
			if change != nil {
				size += 32 + int64(len(change.UIDs))*4
			}
		case *UpdateFlags:
			if change != nil {
				size += 48
				for _, flag := range change.Flags {
					size += int64(len(flag))
				}
			}
		case *UpdateExpunge:
			size += 24
		case *UpdateVanished:
			if change != nil {
				size += 32 + int64(len(change.UIDs))*8
			}
		default:
			size += 64
		}
	}
	return size
}

type selectedState struct {
	mailbox  SelectedMailbox
	uids     []imap.UID
	revision MailboxRevision
	readOnly bool
	maxUIDs  int
	queue    *updateQueue
	updater  *Updater
	// savedSearch is the SEARCHRES result referenced by "$". It is framework
	// state, not backend state, per the contract on SelectedMailbox: it is
	// scoped to this selection and discarded with it. RFC 5182.
	savedSearch imap.UIDSet
}

//lint:ignore U1000 T22's SELECT handler is the first production caller; T19 owns the atomic attachment primitive.
func newSelectionUpdater(limits Limits, overflow func()) (*Updater, *updateQueue) {
	queue := newUpdateQueue(limits.MaxQueuedUpdates, limits.MaxQueuedUpdateBytes, overflow)
	return newUpdater(queue), queue
}

//lint:ignore U1000 T22's SELECT handler owns the protocol response; T19 owns this atomic backend boundary.
func openSelectedState(ctx context.Context, state *sessionState, mailbox string, options *SelectOptions, limits Limits, overflow func()) (selected *selectedState, err error) {
	if state == nil || state.session == nil {
		return nil, fmt.Errorf("imapserver: SELECT requires an authenticated session")
	}
	updater, queue := newSelectionUpdater(limits, overflow)
	succeeded := false
	defer func() {
		if !succeeded {
			updater.core.close()
			queue.close()
		}
		if recovered := recover(); recovered != nil {
			panic(recovered)
		}
	}()
	result, err := state.session.Select(ctx, mailbox, updater, options)
	if err != nil {
		return nil, err
	}
	selected, err = attachSelectedState(result, updater, queue, limits)
	if err != nil {
		if result != nil && result.Mailbox != nil {
			closeRejectedMailbox(result.Mailbox, limits.CommandTimeout)
		}
		return nil, err
	}
	if !state.selectMailbox(selected) {
		selected.close()
		closeRejectedMailbox(selected.mailbox, limits.CommandTimeout)
		return nil, fmt.Errorf("imapserver: selected mailbox does not satisfy the enabled protocol revision")
	}
	succeeded = true
	return selected, nil
}

func closeRejectedMailbox(mailbox SelectedMailbox, timeout time.Duration) {
	if mailbox == nil {
		return
	}
	ctx := context.Background()
	cancel := func() {}
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	_ = mailbox.Unselect(ctx)
	cancel()
}

//lint:ignore U1000 T22's SELECT handler is the first production caller; keeping attachment here preserves T19's update contract.
func attachSelectedState(result *SelectResult, updater *Updater, queue *updateQueue, limits Limits) (*selectedState, error) {
	if result == nil || result.Mailbox == nil {
		if updater != nil && updater.core != nil {
			updater.core.close()
		}
		if queue != nil {
			queue.close()
		}
		return nil, fmt.Errorf("imapserver: SELECT returned no mailbox")
	}
	if err := validateSelectSnapshot(&result.Snapshot, limits.MaxSelectedMessages); err != nil {
		if updater != nil && updater.core != nil {
			updater.core.close()
		}
		if queue != nil {
			queue.close()
		}
		return nil, err
	}
	selected := &selectedState{
		mailbox:  result.Mailbox,
		uids:     slices.Clone(result.Snapshot.UIDs),
		revision: result.Snapshot.Revision,
		readOnly: result.Snapshot.ReadOnly || result.Snapshot.Status.ReadOnly,
		maxUIDs:  limits.MaxSelectedMessages,
		queue:    queue,
		updater:  updater,
	}
	return selected, nil
}

func validateSelectSnapshot(snapshot *SelectSnapshot, maxMessages int) error {
	if snapshot == nil {
		return fmt.Errorf("imapserver: nil SELECT snapshot")
	}
	if maxMessages >= 0 && len(snapshot.UIDs) > maxMessages {
		return fmt.Errorf("imapserver: selected mailbox has %d messages, limit is %d", len(snapshot.UIDs), maxMessages)
	}
	if uint32(len(snapshot.UIDs)) != snapshot.Status.NumMessages {
		return fmt.Errorf("imapserver: SELECT snapshot has %d UIDs but reports %d messages", len(snapshot.UIDs), snapshot.Status.NumMessages)
	}
	if snapshot.Status.UIDValidity == 0 {
		return fmt.Errorf("imapserver: SELECT snapshot has zero UIDVALIDITY")
	}
	var previous imap.UID
	for _, uid := range snapshot.UIDs {
		if uid == 0 || uid <= previous {
			return fmt.Errorf("imapserver: SELECT snapshot UIDs are not strictly ascending")
		}
		previous = uid
	}
	if snapshot.Status.UIDNext == 0 || snapshot.Status.UIDNext <= previous {
		return fmt.Errorf("imapserver: UIDNEXT does not exceed the selected UIDs")
	}
	if snapshot.NumRecent > uint32(len(snapshot.UIDs)) {
		return fmt.Errorf("imapserver: RECENT exceeds the message count")
	}
	return nil
}

func (s *selectedState) close() {
	if s == nil {
		return
	}
	if s.updater != nil && s.updater.core != nil {
		s.updater.core.close()
	}
	if s.queue != nil {
		s.queue.close()
	}
}

type commandEffect uint8

const (
	effectNone commandEffect = iota
	effectStore
	effectExpunge
	effectMoveOut
)

type updateAccounting struct {
	origin ChangeToken
	effect commandEffect
}

type deliveredUpdate struct {
	kind    updateKind
	seqNum  imap.SeqNum
	uid     imap.UID
	uids    imap.UIDSet
	flags   []imap.Flag
	modSeq  uint64
	earlier bool
	exists  uint32
}

type updateKind uint8

const (
	updateExists updateKind = iota + 1
	updateMessageFlags
	updateMessageExpunge
	updateMessageVanished
)

func coalesceWireUpdates(updates []deliveredUpdate) []deliveredUpdate {
	if len(updates) < 2 {
		return updates
	}
	coalesced := make([]deliveredUpdate, 0, len(updates))
	for _, update := range updates {
		if len(coalesced) == 0 {
			coalesced = append(coalesced, update)
			continue
		}
		previous := &coalesced[len(coalesced)-1]
		switch {
		case previous.kind == updateExists && update.kind == updateExists:
			*previous = update
		case previous.kind == updateMessageFlags && update.kind == updateMessageFlags && previous.uid == update.uid:
			*previous = update
		case previous.kind == updateMessageVanished && update.kind == updateMessageVanished && previous.earlier == update.earlier:
			previous.uids.AddSet(update.uids)
		default:
			coalesced = append(coalesced, update)
		}
	}
	return coalesced
}

func (s *selectedState) applyBatch(batch *UpdateBatch, accounting updateAccounting) ([]deliveredUpdate, error) {
	if s == nil || batch == nil {
		return nil, ErrUpdaterClosed
	}
	if batch.Before != s.revision {
		return nil, fmt.Errorf("%w: got %q, want %q", ErrRevisionMismatch, batch.Before, s.revision)
	}
	var delivered []deliveredUpdate
	for _, change := range batch.Changes {
		suppress := batch.Origin != 0 && batch.Origin == accounting.origin
		switch change := change.(type) {
		case *UpdateAdd:
			if change == nil || len(change.UIDs) == 0 {
				return nil, fmt.Errorf("%w: empty ADD update", ErrRevisionMismatch)
			}
			if err := s.addUIDs(change.UIDs); err != nil {
				return nil, err
			}
			// Additions are never blanket-suppressed: APPEND/COPY/MOVE results
			// do not tell the selected client the new sequence position.
			delivered = append(delivered, deliveredUpdate{kind: updateExists, exists: uint32(len(s.uids))})
		case *UpdateFlags:
			if change == nil {
				return nil, fmt.Errorf("%w: nil FLAGS update", ErrRevisionMismatch)
			}
			seq, ok := s.sequence(change.UID)
			if !ok {
				return nil, fmt.Errorf("%w: flag update for unknown UID %d", ErrRevisionMismatch, change.UID)
			}
			if !(suppress && accounting.effect == effectStore) {
				delivered = append(delivered, deliveredUpdate{kind: updateMessageFlags, seqNum: seq, uid: change.UID, flags: slices.Clone(change.Flags), modSeq: change.ModSeq})
			}
		case *UpdateExpunge:
			if change == nil {
				return nil, fmt.Errorf("%w: nil EXPUNGE update", ErrRevisionMismatch)
			}
			seq, ok := s.removeUID(change.UID)
			if !ok {
				return nil, fmt.Errorf("%w: expunge for unknown UID %d", ErrRevisionMismatch, change.UID)
			}
			if !(suppress && (accounting.effect == effectExpunge || accounting.effect == effectMoveOut)) {
				delivered = append(delivered, deliveredUpdate{kind: updateMessageExpunge, seqNum: seq, uid: change.UID})
			}
		case *UpdateVanished:
			if change == nil || change.UIDs.IsEmpty() {
				return nil, fmt.Errorf("%w: empty VANISHED update", ErrRevisionMismatch)
			}
			if change.UIDs.Dynamic() {
				return nil, fmt.Errorf("%w: VANISHED update contains a dynamic UID set", ErrRevisionMismatch)
			}
			s.removeUIDSet(change.UIDs)
			if !(suppress && (accounting.effect == effectExpunge || accounting.effect == effectMoveOut)) {
				delivered = append(delivered, deliveredUpdate{kind: updateMessageVanished, uids: slices.Clone(change.UIDs), earlier: change.Earlier})
			}
		default:
			return nil, fmt.Errorf("imapserver: unsupported update type %T", change)
		}
	}
	s.revision = batch.After
	return delivered, nil
}

func (s *selectedState) sequence(uid imap.UID) (imap.SeqNum, bool) {
	at, ok := slices.BinarySearch(s.uids, uid)
	if !ok {
		return 0, false
	}
	return imap.SeqNum(at + 1), true
}

func (s *selectedState) addUIDs(uids []imap.UID) error {
	if s.maxUIDs > 0 && len(uids) > s.maxUIDs-len(s.uids) {
		return fmt.Errorf("%w: selected message limit of %d exceeded", ErrRevisionMismatch, s.maxUIDs)
	}
	last := imap.UID(0)
	if len(s.uids) > 0 {
		last = s.uids[len(s.uids)-1]
	}
	for _, uid := range uids {
		if uid == 0 || uid <= last {
			return fmt.Errorf("%w: added UIDs are not strictly ascending", ErrRevisionMismatch)
		}
		last = uid
	}
	s.uids = append(s.uids, uids...)
	return nil
}

func (s *selectedState) removeUID(uid imap.UID) (imap.SeqNum, bool) {
	at, ok := slices.BinarySearch(s.uids, uid)
	if !ok {
		return 0, false
	}
	seq := imap.SeqNum(at + 1)
	s.uids = slices.Delete(s.uids, at, at+1)
	return seq, true
}

func (s *selectedState) removeUIDSet(set imap.UIDSet) {
	out := s.uids[:0]
	for _, uid := range s.uids {
		if !set.Contains(uid) {
			out = append(out, uid)
		}
	}
	s.uids = out
}
