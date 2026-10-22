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

// queuedExpunges packs consecutive removals inside one atomic batch. It is
// private to the queue: application still removes each UID in its original
// order and emits the same individually ordered EXPUNGE responses.
type queuedExpunges struct{ uids []imap.UID }

func (*queuedExpunges) update() {}

// updateRun returns the next run that can share storage without moving an
// update across another kind. Empty or typed-nil updates remain barriers so
// compaction cannot hide malformed backend input.
func updateRun(changes []Update) (count, uids int) {
	switch changes[0].(type) {
	case *UpdateAdd:
		for _, change := range changes {
			add, ok := change.(*UpdateAdd)
			if !ok || add == nil || len(add.UIDs) == 0 {
				break
			}
			count++
			uids += len(add.UIDs)
		}
	case *UpdateExpunge:
		for _, change := range changes {
			expunge, ok := change.(*UpdateExpunge)
			if !ok || expunge == nil {
				break
			}
			count++
			uids++
		}
	}
	if count == 0 {
		count = 1
	}
	return count, uids
}

func cloneUpdateBatch(batch *UpdateBatch) *UpdateBatch {
	// Size the interface slice for the compact result, not the backend's
	// original per-message updates; retaining its capacity defeats compaction.
	count := 0
	for remaining := batch.Changes; len(remaining) != 0; count++ {
		n, _ := updateRun(remaining)
		remaining = remaining[n:]
	}
	copyBatch := &UpdateBatch{
		Before:  batch.Before,
		After:   batch.After,
		Origin:  batch.Origin,
		Changes: make([]Update, 0, count),
	}
	for remaining := batch.Changes; len(remaining) != 0; {
		n, uidCount := updateRun(remaining)
		run := remaining[:n]
		remaining = remaining[n:]
		switch change := run[0].(type) {
		case *UpdateAdd:
			if change == nil {
				copyBatch.Changes = append(copyBatch.Changes, change)
			} else {
				uids := make([]imap.UID, uidCount)
				at := 0
				for _, item := range run {
					at += copy(uids[at:], item.(*UpdateAdd).UIDs)
				}
				copyBatch.Changes = append(copyBatch.Changes, &UpdateAdd{UIDs: uids})
			}
		case *UpdateRecent:
			if change == nil {
				copyBatch.Changes = append(copyBatch.Changes, change)
			} else {
				copyBatch.Changes = append(copyBatch.Changes, &UpdateRecent{NumRecent: change.NumRecent})
			}
		case *UpdateFlags:
			if change == nil {
				copyBatch.Changes = append(copyBatch.Changes, change)
			} else {
				copyBatch.Changes = append(copyBatch.Changes, &UpdateFlags{UID: change.UID, Flags: slices.Clone(change.Flags), ModSeq: change.ModSeq})
			}
		case *UpdateMailboxFlags:
			if change == nil {
				copyBatch.Changes = append(copyBatch.Changes, change)
			} else {
				copyBatch.Changes = append(copyBatch.Changes, &UpdateMailboxFlags{Flags: slices.Clone(change.Flags)})
			}
		case *UpdateExpunge:
			if change == nil {
				copyBatch.Changes = append(copyBatch.Changes, change)
			} else if n > 1 {
				uids := make([]imap.UID, n)
				for i, item := range run {
					uids[i] = item.(*UpdateExpunge).UID
				}
				copyBatch.Changes = append(copyBatch.Changes, &queuedExpunges{uids: uids})
			} else {
				copyBatch.Changes = append(copyBatch.Changes, &UpdateExpunge{UID: change.UID})
			}
		case *queuedExpunges:
			if change == nil {
				copyBatch.Changes = append(copyBatch.Changes, change)
			} else {
				copyBatch.Changes = append(copyBatch.Changes, &queuedExpunges{uids: slices.Clone(change.uids)})
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
		(q.maxBytes >= 0 && q.bytes > q.maxBytes) {
		overflow := q.overflow
		q.closed = true
		q.mu.Unlock()
		if overflow != nil {
			overflow()
		}
		return ErrUpdateOverflow
	}
	// Bound compact retained storage before allocating it. The backend's
	// original per-message interface slice need not be duplicated wholesale.
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

// popWhile removes and returns the longest *prefix* of queued batches that
// keep accepts, leaving the rest in order.
//
// A prefix rather than a filter, because batches chain: applyBatch rejects one
// whose Before does not match the current revision, and a batch skipped now
// would strand every later one permanently. So the first batch that must wait
// blocks the queue behind it, which is the conservative direction — a delayed
// update is a client that finds out later, a reordered one is a client whose
// sequence numbers are wrong.
func (q *updateQueue) popWhile(keep func(*UpdateBatch) bool) []*UpdateBatch {
	q.mu.Lock()
	defer q.mu.Unlock()
	taken := 0
	for taken < len(q.items) && keep(q.items[taken]) {
		taken++
	}
	if taken == 0 {
		return nil
	}
	items := q.items[:taken]
	q.items = append([]*UpdateBatch(nil), q.items[taken:]...)
	for _, batch := range items {
		q.bytes -= updateBatchSize(batch)
	}
	return items
}

// popThroughOrigin removes the prefix ending at the last batch carrying
// origin. A removal command uses this after its backend call: older revisions
// and the command's own removals must be applied as one ordered unit before the
// command can report sequence numbers. Batches published later stay queued.
func (q *updateQueue) popThroughOrigin(origin ChangeToken) []*UpdateBatch {
	if origin == 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	last := -1
	for i, batch := range q.items {
		if batch != nil && batch.Origin == origin {
			last = i
		}
	}
	if last < 0 {
		return nil
	}
	items := q.items[:last+1]
	q.items = append([]*UpdateBatch(nil), q.items[last+1:]...)
	for _, batch := range items {
		q.bytes -= updateBatchSize(batch)
	}
	return items
}

// origins returns the non-zero command origins still waiting in the queue.
// drainUpdates uses this to retain response accounting across a deferred
// prefix without keeping one map entry for every command forever.
func (q *updateQueue) origins() map[ChangeToken]struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	origins := make(map[ChangeToken]struct{})
	for _, batch := range q.items {
		if batch != nil && batch.Origin != 0 {
			origins[batch.Origin] = struct{}{}
		}
	}
	return origins
}

// mailboxFlagsForOrigin returns the last complete mailbox FLAGS value carried
// by origin without removing or applying its batch. STORE uses this to announce
// a newly created keyword before the FETCH response that first mentions it.
func (q *updateQueue) mailboxFlagsForOrigin(origin ChangeToken) ([]imap.Flag, bool) {
	if origin == 0 {
		return nil, false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	var flags []imap.Flag
	found := false
	for _, batch := range q.items {
		if batch == nil || batch.Origin != origin {
			continue
		}
		for _, change := range batch.Changes {
			if update, ok := change.(*UpdateMailboxFlags); ok && update != nil {
				flags = slices.Clone(update.Flags)
				found = true
			}
		}
	}
	return flags, found
}

// batchRemovesMessages reports whether a batch renumbers the mailbox by taking
// messages out of it, which is the only thing RFC 3501 §7.4.1 restricts.
func batchRemovesMessages(batch *UpdateBatch) bool {
	if batch == nil {
		return false
	}
	for _, change := range batch.Changes {
		switch change.(type) {
		case *UpdateExpunge, *queuedExpunges, *UpdateVanished:
			return true
		}
	}
	return false
}

func (q *updateQueue) hasRemovals() bool {
	if q == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, batch := range q.items {
		if batchRemovesMessages(batch) {
			return true
		}
	}
	return false
}

func (q *updateQueue) hasFlags() bool {
	if q == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, batch := range q.items {
		for _, change := range batch.Changes {
			if _, ok := change.(*UpdateFlags); ok {
				return true
			}
		}
	}
	return false
}

// pendingMessageChanges reports which UIDs a queued, not yet applied batch
// changes or removes. A context refresh searches live backend state, which can
// already include such a change; its ADDTO or REMOVEFROM waits for the drain
// that announces the change itself.
func (q *updateQueue) pendingMessageChanges() func(imap.UID) bool {
	uids := make(map[imap.UID]struct{})
	var sets []imap.UIDSet
	if q != nil {
		q.mu.Lock()
		for _, batch := range q.items {
			if batch == nil {
				continue
			}
			for _, change := range batch.Changes {
				switch change := change.(type) {
				case *UpdateFlags:
					if change != nil {
						uids[change.UID] = struct{}{}
					}
				case *UpdateExpunge:
					if change != nil {
						uids[change.UID] = struct{}{}
					}
				case *queuedExpunges:
					if change != nil {
						for _, uid := range change.uids {
							uids[uid] = struct{}{}
						}
					}
				case *UpdateVanished:
					if change != nil {
						sets = append(sets, change.UIDs)
					}
				}
			}
		}
		q.mu.Unlock()
	}
	return func(uid imap.UID) bool {
		if _, ok := uids[uid]; ok {
			return true
		}
		for _, set := range sets {
			if set.Contains(uid) {
				return true
			}
		}
		return false
	}
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

// updateBatchSize accounts for the compact storage cloneUpdateBatch retains.
func updateBatchSize(batch *UpdateBatch) int64 {
	const batchOverhead = 64
	if batch == nil {
		return batchOverhead
	}
	size := int64(batchOverhead + len(batch.Before) + len(batch.After))
	for remaining := batch.Changes; len(remaining) != 0; {
		n, uidCount := updateRun(remaining)
		change := remaining[0]
		remaining = remaining[n:]
		switch change := change.(type) {
		case *UpdateAdd:
			if change != nil {
				size += 32 + int64(uidCount)*4
			} else {
				size += 16
			}
		case *UpdateFlags:
			if change != nil {
				size += 48
				for _, flag := range change.Flags {
					size += int64(len(flag))
				}
			} else {
				size += 16
			}
		case *UpdateMailboxFlags:
			if change != nil {
				size += 32
				for _, flag := range change.Flags {
					size += int64(len(flag))
				}
			} else {
				size += 16
			}
		case *UpdateExpunge:
			if n > 1 {
				size += 32 + int64(uidCount)*4
			} else {
				size += 24
			}
		case *queuedExpunges:
			if change != nil {
				size += 32 + int64(len(change.uids))*4
			} else {
				size += 16
			}
		case *UpdateVanished:
			if change != nil {
				size += 32 + int64(len(change.UIDs))*8
			} else {
				size += 16
			}
		default:
			size += 64
		}
	}
	return size
}

type selectedState struct {
	mailbox SelectedMailbox
	// name is the selected mailbox's name, which MULTISEARCH needs to name the
	// default search source. See ext_c_multisearch.go.
	name string
	// uidValidity qualifies UIDs in MULTISEARCH context updates.
	uidValidity uint32
	uids        []imap.UID
	flags       []imap.Flag
	recent      uint32
	revision    MailboxRevision
	readOnly    bool
	noModSeq    bool
	maxUIDs     int
	queue       *updateQueue
	updater     *Updater
	// uids is the client's view. While removals are withheld, backendUIDs
	// follows the fully applied revision without renumbering that view.
	backendUIDs []imap.UID
	withheld    []imap.UID
	// suppressed, pendingAdds and pendingRecent are what NOTIFY told the
	// framework not to announce: removed messages that stay in the client's
	// view, added ones kept out of it, and the RECENT count that goes with
	// them. Each UID appears at most once, so they are bounded by the
	// selection's size rather than by the update queue.
	suppressed       []Update
	pendingFlags     map[imap.UID]*UpdateFlags
	pendingAdds      []imap.UID
	pendingRecent    uint32
	hasPendingRecent bool
	// The selection snapshot is a known-safe resynchronisation checkpoint.
	// A later FETCH cannot establish a newer checkpoint while removals or
	// flag changes remain unreported.
	snapshotModSeq uint64
	// savedSearch is the SEARCHRES result referenced by "$". It is framework
	// state, not backend state, per the contract on SelectedMailbox: it is
	// scoped to this selection and discarded with it. RFC 5182.
	savedSearch imap.UIDSet
	// contexts are the CONTEXT=SEARCH registrations, also scoped to this
	// selection. See ext_e_context.go.
	contexts []*searchContext
	// uidBatches records the last successful UIDBATCHES request for this selection.
	uidBatches *uidBatchesRequest
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
	selected.name = mailbox
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
	_ = mailbox.Unselect(ctx, nil)
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
		mailbox:        result.Mailbox,
		name:           result.Snapshot.Status.Mailbox,
		uidValidity:    result.Snapshot.Status.UIDValidity,
		uids:           slices.Clone(result.Snapshot.UIDs),
		flags:          slices.Clone(result.Snapshot.Status.Flags),
		recent:         result.Snapshot.Status.NumRecent,
		revision:       result.Snapshot.Revision,
		readOnly:       result.Snapshot.Status.ReadOnly,
		noModSeq:       result.Snapshot.Status.NoModSeq,
		maxUIDs:        limits.MaxSelectedMessages,
		queue:          queue,
		updater:        updater,
		snapshotModSeq: result.Snapshot.Status.HighestModSeq,
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
	if uint64(len(snapshot.UIDs)) != uint64(snapshot.Status.NumMessages) {
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
	if snapshot.Status.NumRecent > snapshot.Status.NumMessages {
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
	// covered narrows effectStore to the messages whose new state the STORE
	// response itself reported. Nil covers every message of the origin, which
	// is how an unconditional .SILENT store is accounted: RFC 3501 section
	// 6.4.6 lets it omit the new values entirely. A changed message outside
	// it keeps its unsolicited FETCH, so a backend that reported less than it
	// changed cannot hide a change from the client.
	covered map[imap.UID]struct{}
	// silent leaves an older withheld flag change unreported even if the
	// command reports a newer MODSEQ for that same message.
	silent bool
}

// suppressesFlags reports whether uid's flag update from this accounting's
// origin was already answered by the command response.
func (a updateAccounting) suppressesFlags(uid imap.UID) bool {
	if a.effect != effectStore {
		return false
	}
	if a.covered == nil {
		return true
	}
	_, ok := a.covered[uid]
	return ok
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
	updateMailboxFlags
	updateRecent
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
		case previous.kind == updateMailboxFlags && update.kind == updateMailboxFlags:
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
	return s.applyBatchWith(batch, accounting, batchDelivery{})
}

func (s *selectedState) applyBatchDeferringRemovals(batch *UpdateBatch, accounting updateAccounting, deferRemovals bool) ([]deliveredUpdate, error) {
	return s.applyBatchWith(batch, accounting, batchDelivery{deferRemovals: deferRemovals})
}

// batchDelivery is what one batch may announce to the client.
//
// The revision always advances and every change always reaches the backend
// view; these only decide which changes reach the client's view, and when.
type batchDelivery struct {
	// deferRemovals withholds removals for RFC 3501 section 7.4.1 timing.
	// They are delivered at the next drain that permits an EXPUNGE.
	deferRemovals bool
	// suppressMessages withholds additions and removals the client asked not
	// to hear about through NOTIFY (RFC 5465 section 5). Removed messages
	// stay in the client's view and additions stay out of it, so no sequence
	// number the client holds changes meaning without it being told.
	suppressMessages bool
	// suppressFlags retains the latest flags per UID for the next NOTIFY SET.
	// The resynchronisation fence covers them until they are reported.
	suppressFlags bool
}

func (s *selectedState) applyBatchWith(batch *UpdateBatch, accounting updateAccounting, delivery batchDelivery) ([]deliveredUpdate, error) {
	if s == nil || batch == nil {
		return nil, ErrUpdaterClosed
	}
	if batch.Before != s.revision {
		return nil, fmt.Errorf("%w: got %q, want %q", ErrRevisionMismatch, batch.Before, s.revision)
	}
	var delivered []deliveredUpdate
	suppress := batch.Origin != 0 && batch.Origin == accounting.origin
	for _, change := range batch.Changes {
		switch change := change.(type) {
		case *UpdateAdd:
			if change == nil || len(change.UIDs) == 0 {
				return nil, fmt.Errorf("%w: empty ADD update", ErrRevisionMismatch)
			}
			if delivery.suppressMessages {
				if err := s.holdAdditions(change.UIDs); err != nil {
					return nil, err
				}
				continue
			}
			// Withheld additions precede these UIDs and enter the view first.
			delivered = append(delivered, s.releaseAdditions()...)
			if err := s.addUIDs(change.UIDs); err != nil {
				return nil, err
			}
			// Additions are never blanket-suppressed: APPEND/COPY/MOVE results
			// do not tell the selected client the new sequence position.
			delivered = append(delivered, deliveredUpdate{kind: updateExists, exists: uint32(len(s.uids))})
		case *UpdateRecent:
			if change == nil || uint64(change.NumRecent) > uint64(s.backendCount()) {
				return nil, fmt.Errorf("%w: invalid RECENT count", ErrRevisionMismatch)
			}
			// RECENT counts messages the client may not have been told exist
			// yet, so it waits with them.
			if delivery.suppressMessages || len(s.pendingAdds) != 0 {
				s.pendingRecent, s.hasPendingRecent = change.NumRecent, true
				continue
			}
			s.hasPendingRecent = false
			if s.recent != change.NumRecent {
				s.recent = change.NumRecent
				delivered = append(delivered, deliveredUpdate{kind: updateRecent, exists: change.NumRecent})
			}
		case *UpdateFlags:
			if change == nil {
				return nil, fmt.Errorf("%w: nil FLAGS update", ErrRevisionMismatch)
			}
			if !s.backendHas(change.UID) {
				return nil, fmt.Errorf("%w: flag update for unknown UID %d", ErrRevisionMismatch, change.UID)
			}
			seq, visible := s.sequence(change.UID)
			accounted := suppress && accounting.suppressesFlags(change.UID)
			retainSilent := accounted && accounting.silent && s.pendingFlags[change.UID] != nil
			if accounted && !retainSilent {
				// A FLAGS response supersedes older retained flags. A silent
				// response cannot acknowledge an earlier withheld change.
				delete(s.pendingFlags, change.UID)
			} else if delivery.suppressFlags || !visible || retainSilent {
				if s.pendingFlags == nil {
					s.pendingFlags = make(map[imap.UID]*UpdateFlags)
				}
				s.pendingFlags[change.UID] = &UpdateFlags{UID: change.UID, Flags: slices.Clone(change.Flags), ModSeq: change.ModSeq}
			} else {
				delete(s.pendingFlags, change.UID)
				delivered = append(delivered, deliveredUpdate{kind: updateMessageFlags, seqNum: seq, uid: change.UID, flags: slices.Clone(change.Flags), modSeq: change.ModSeq})
			}
		case *UpdateMailboxFlags:
			if change == nil {
				return nil, fmt.Errorf("%w: nil mailbox FLAGS update", ErrRevisionMismatch)
			}
			// flags is what this client has been sent. Leaving it unchanged
			// while suppressed lets ensureFetchFlagsAdvertised announce a new
			// keyword before the first FETCH that carries it.
			if !delivery.suppressFlags && !slices.Equal(s.flags, change.Flags) {
				s.flags = slices.Clone(change.Flags)
				delivered = append(delivered, deliveredUpdate{kind: updateMailboxFlags, flags: slices.Clone(change.Flags)})
			}
		case *UpdateExpunge, *queuedExpunges, *UpdateVanished:
			var uids []imap.UID
			switch change := change.(type) {
			case *UpdateExpunge:
				if change != nil {
					uids = []imap.UID{change.UID}
				}
			case *queuedExpunges:
				if change != nil {
					uids = change.uids
				}
			case *UpdateVanished:
				if change == nil || change.UIDs.IsEmpty() || change.UIDs.Dynamic() {
					return nil, fmt.Errorf("%w: invalid VANISHED update", ErrRevisionMismatch)
				}
				// Live updates always remove from the current view. EARLIER
				// belongs only to SELECT/FETCH resynchronisation reports.
				// Intersect with the backend view without expanding UID ranges;
				// unknown UIDs and unannounced additions never reach the wire.
				current := s.uids
				if s.backendUIDs != nil {
					current = s.backendUIDs
				}
				for _, uid := range current {
					if change.UIDs.Contains(uid) {
						uids = append(uids, uid)
					}
				}
				if len(uids) == 0 {
					continue
				}
			}
			if len(uids) == 0 {
				return nil, fmt.Errorf("%w: empty EXPUNGE update", ErrRevisionMismatch)
			}
			for _, uid := range uids {
				delete(s.pendingFlags, uid)
				// A message removed before the client heard of it needs no
				// announcement at all.
				if s.cancelPendingAdd(uid) {
					continue
				}
				if delivery.deferRemovals || delivery.suppressMessages {
					if !s.removeBackendUID(uid) {
						return nil, fmt.Errorf("%w: expunge for unknown UID %d", ErrRevisionMismatch, uid)
					}
					if delivery.suppressMessages {
						s.suppressed = append(s.suppressed, &UpdateExpunge{UID: uid})
					} else {
						s.withheld = append(s.withheld, uid)
					}
					continue
				}
				if s.backendUIDs != nil && !s.removeBackendUID(uid) {
					return nil, fmt.Errorf("%w: expunge for unknown UID %d", ErrRevisionMismatch, uid)
				}
				seq, ok := s.removeUID(uid)
				if !ok {
					return nil, fmt.Errorf("%w: expunge for unknown UID %d", ErrRevisionMismatch, uid)
				}
				if !(suppress && (accounting.effect == effectExpunge || accounting.effect == effectMoveOut)) {
					delivered = append(delivered, deliveredUpdate{kind: updateMessageExpunge, seqNum: seq, uid: uid})
				}
			}
		default:
			return nil, fmt.Errorf("imapserver: unsupported update type %T", change)
		}
	}
	s.revision = batch.After
	s.reconcileViews()
	return delivered, nil
}

// deliverWithheldRemovals announces the removals deferred for RFC 3501
// section 7.4.1, in the order they happened.
func (s *selectedState) deliverWithheldRemovals() ([]deliveredUpdate, error) {
	var updates []deliveredUpdate
	for _, uid := range s.withheld {
		seq, ok := s.removeUID(uid)
		if !ok {
			return nil, fmt.Errorf("%w: withheld expunge for unknown UID %d", ErrRevisionMismatch, uid)
		}
		updates = append(updates, deliveredUpdate{kind: updateMessageExpunge, seqNum: seq, uid: uid})
	}
	s.withheld = nil
	s.reconcileViews()
	return updates, nil
}

// suppressWithheld moves protocol-deferred removals to NOTIFY suppression.
// Both are bounded by the client UID view, independently of queued batches.
func (s *selectedState) suppressWithheld() {
	for _, uid := range s.withheld {
		s.suppressed = append(s.suppressed, &UpdateExpunge{UID: uid})
	}
	s.withheld = nil
}

// releaseSuppressed announces what NOTIFY suppressed: additions always, and
// removals when an EXPUNGE may be sent now.
func (s *selectedState) releaseSuppressed(removals bool) []deliveredUpdate {
	var updates []deliveredUpdate
	if removals {
		for _, change := range s.suppressed {
			switch change := change.(type) {
			case *UpdateExpunge:
				if seq, ok := s.removeUID(change.UID); ok {
					updates = append(updates, deliveredUpdate{kind: updateMessageExpunge, seqNum: seq, uid: change.UID})
				}
			case *UpdateVanished:
				s.removeUIDSet(change.UIDs)
				updates = append(updates, deliveredUpdate{kind: updateMessageVanished, uids: change.UIDs, earlier: change.Earlier})
			}
		}
		s.suppressed = nil
	}
	updates = append(updates, s.releaseAdditions()...)
	if s.hasPendingRecent {
		s.hasPendingRecent = false
		if s.recent != s.pendingRecent {
			s.recent = s.pendingRecent
			updates = append(updates, deliveredUpdate{kind: updateRecent, exists: s.recent})
		}
	}
	s.reconcileViews()
	return updates
}

// releaseFlags reports retained flags only after the message has a client
// sequence number. Iterate the selection rather than the map for stable order.
func (s *selectedState) releaseFlags() []deliveredUpdate {
	var updates []deliveredUpdate
	for i, uid := range s.uids {
		if flags := s.pendingFlags[uid]; flags != nil {
			updates = append(updates, deliveredUpdate{kind: updateMessageFlags, seqNum: imap.SeqNum(i + 1), uid: uid, flags: flags.Flags, modSeq: flags.ModSeq})
			delete(s.pendingFlags, uid)
		}
	}
	return updates
}

// holdAdditions records additions outside the client's view.
func (s *selectedState) holdAdditions(uids []imap.UID) error {
	if s.maxUIDs > 0 && len(uids) > s.maxUIDs-s.backendCount() {
		return fmt.Errorf("%w: selected message limit of %d exceeded", ErrRevisionMismatch, s.maxUIDs)
	}
	s.ensureBackendView()
	last := imap.UID(0)
	if len(s.backendUIDs) > 0 {
		last = s.backendUIDs[len(s.backendUIDs)-1]
	}
	for _, uid := range uids {
		if uid == 0 || uid <= last {
			return fmt.Errorf("%w: added UIDs are not strictly ascending", ErrRevisionMismatch)
		}
		last = uid
	}
	s.pendingAdds = append(s.pendingAdds, uids...)
	s.backendUIDs = append(s.backendUIDs, uids...)
	return nil
}

// releaseAdditions moves withheld additions into the client's view. They are
// already in the backend view, and follow every UID the client holds.
func (s *selectedState) releaseAdditions() []deliveredUpdate {
	if len(s.pendingAdds) == 0 {
		return nil
	}
	s.uids = append(s.uids, s.pendingAdds...)
	s.pendingAdds = nil
	return []deliveredUpdate{{kind: updateExists, exists: uint32(len(s.uids))}}
}

func (s *selectedState) cancelPendingAdd(uid imap.UID) bool {
	at, ok := slices.BinarySearch(s.pendingAdds, uid)
	if !ok {
		return false
	}
	s.pendingAdds = slices.Delete(s.pendingAdds, at, at+1)
	s.removeBackendUID(uid)
	return true
}

// ensureBackendView starts tracking the backend view separately from the
// client's, which is needed while anything is withheld from the client.
func (s *selectedState) ensureBackendView() {
	if s.backendUIDs == nil {
		s.backendUIDs = slices.Clone(s.uids)
	}
}

func (s *selectedState) removeBackendUID(uid imap.UID) bool {
	s.ensureBackendView()
	at, ok := slices.BinarySearch(s.backendUIDs, uid)
	if !ok {
		return false
	}
	s.backendUIDs = slices.Delete(s.backendUIDs, at, at+1)
	return true
}

func (s *selectedState) backendHas(uid imap.UID) bool {
	if s.backendUIDs != nil {
		_, ok := slices.BinarySearch(s.backendUIDs, uid)
		return ok
	}
	_, ok := s.sequence(uid)
	return ok
}

func (s *selectedState) backendCount() int {
	if s.backendUIDs != nil {
		return len(s.backendUIDs)
	}
	return len(s.uids)
}

// withholdsRemovals reports whether a removal the backend has applied is still
// unannounced, for any reason.
func (s *selectedState) withholdsRemovals() bool {
	return len(s.withheld) != 0 || len(s.suppressed) != 0
}

// reconcileViews stops tracking the backend view once the client's agrees.
func (s *selectedState) reconcileViews() {
	if len(s.withheld) == 0 && len(s.suppressed) == 0 && len(s.pendingAdds) == 0 {
		s.backendUIDs = nil
	}
}

func (s *selectedState) sequence(uid imap.UID) (imap.SeqNum, bool) {
	at, ok := slices.BinarySearch(s.uids, uid)
	if !ok {
		return 0, false
	}
	return imap.SeqNum(at + 1), true
}

func (s *selectedState) addUIDs(uids []imap.UID) error {
	if s.maxUIDs > 0 && len(uids) > s.maxUIDs-max(len(s.uids), s.backendCount()) {
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
	if s.backendUIDs != nil {
		s.backendUIDs = append(s.backendUIDs, uids...)
	}
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
	s.uids = removeUIDsInSet(s.uids, set)
}

func removeUIDsInSet(uids []imap.UID, set imap.UIDSet) []imap.UID {
	out := uids[:0]
	for _, uid := range uids {
		if !set.Contains(uid) {
			out = append(out, uid)
		}
	}
	return out
}
