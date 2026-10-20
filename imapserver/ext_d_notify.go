package imapserver

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// NOTIFY (RFC 5465).
//
// # Why this needed new framework surface
//
// NOTIFY extends IDLE's push model to mailboxes that are *not* selected. That is
// a different lifetime from [Updater], which is attached to one selection and
// discarded with it, and SERVER-DESIGN.md §3 names widening Updater's scope as
// the trap to avoid: doing so would give every selection-scoped push a way to
// outlive its selection, which is the invariant the whole update accounting
// rests on.
//
// So NOTIFY gets its own channel, [SessionUpdater], scoped to the session. The
// two coexist without interacting: the selected mailbox keeps reporting through
// Updater with sequence numbers and revision chaining, and NOTIFY reports other
// mailboxes by name with no sequence numbers at all — because the client has not
// selected them and therefore has no sequence-number view of them to maintain.

// SessionUpdater publishes events about mailboxes the connection has not
// selected. A backend receives one from [NotifySession.Notify] and may push to
// it until Notify is called again or the session ends.
//
// Push never calls into the backend and never blocks on the connection's event
// loop, on the same terms as [Updater].
// Construct with keyed fields only; fields may be added in a future release.
type SessionUpdater struct {
	// PushFunc receives events when the updater is constructed by an adapter
	// such as backendtest. Ordinary backends call Push and leave this unset.
	PushFunc func(*SessionUpdate) error
	core     *sessionUpdaterCore
	_        struct{}
}

// Push publishes one event. It returns ErrUpdaterClosed once the session's
// NOTIFY registration has ended, including replacement, overflow or session end.
//
// The update must name a mailbox and carry a Status, which is the untagged
// STATUS form RFC 5465 section 6 reports these in. Anything else is refused
// rather than accepted and dropped later: a Push that reports success and never
// reaches the client is indistinguishable, from the backend, from one that did.
//
// A successful Push means the update is queued, not that it reached the client.
// The bounded queue coalesces updates for the same mailbox, retaining the most
// recent state. If updates for distinct mailboxes exceed the queue limit, the
// framework reports NOTIFICATIONOVERFLOW to the client and ends the registration
// as if NOTIFY NONE had been received. Once closed, this updater returns
// ErrUpdaterClosed; the client must register again to resume notifications.
func (u *SessionUpdater) Push(update *SessionUpdate) error {
	if u == nil {
		return ErrUpdaterClosed
	}
	// Validate above the branch, not inside the framework-backed one. The
	// PushFunc branch is what adapters and backendtest construct, so validating
	// only in the core left the conformance harness unable to exercise the rule
	// the framework actually enforces — and any caller able to build a
	// SessionUpdater able to bypass it.
	if err := validateSessionUpdate(update); err != nil {
		return err
	}
	if u.core != nil {
		return u.core.push(update)
	}
	if u.PushFunc != nil {
		return u.PushFunc(update)
	}
	return ErrUpdaterClosed
}

// SessionUpdate is one event about a named mailbox.
//
// It carries a mailbox name rather than a sequence number because the client has
// not selected the mailbox: it holds no sequence-number view of it, so there is
// nothing for a number to index into. RFC 5465 section 6 reports these as
// STATUS responses for exactly that reason.
// Construct with keyed fields only; fields may be added in a future release.
type SessionUpdate struct {
	// Mailbox is the mailbox the event concerns.
	Mailbox string
	// Status carries the mailbox's new state. The framework projects Values
	// onto the STATUS items available in the negotiated revision and capabilities.
	// Include UIDNEXT and MESSAGES for MessageNew/MessageExpunge, UIDVALIDITY
	// for identity changes, and HIGHESTMODSEQ for CONDSTORE/QRESYNC sessions
	// (RFC 5465 section 5). A backend without the negotiated state may always
	// include HIGHESTMODSEQ: the framework removes items the session has not
	// negotiated, including HIGHESTMODSEQ until CONDSTORE is enabled.
	Status *imap.StatusData
	_      struct{}
}

// NotifySession is the optional NOTIFY support of RFC 5465.
//
// Notify hands the backend a session-scoped updater and the events the client
// asked for. A nil configuration means NOTIFY NONE: the backend must stop
// pushing and release anything it holds for the previous registration.
//
// The framework calls Notify with a fresh updater each time, and closes the
// previous one first, so a backend that kept the old updater finds it closed
// rather than delivering into a registration the client has replaced.
// Queue overflow also closes the updater and calls Notify with a nil updater
// and configuration, just like NOTIFY NONE.
type NotifySession interface {
	Notify(ctx context.Context, updater *SessionUpdater, config *NotifyConfig, options *NotifyOptions) error
}

// NotifyOptions configures a NOTIFY registration. A nil pointer selects the
// defaults.
// Construct with keyed fields only; fields may be added in a future release.
type NotifyOptions struct{ _ struct{} }

// NotifyConfig is what the client asked to be told about.
// Construct with keyed fields only; fields may be added in a future release.
type NotifyConfig struct {
	// Watches are the watch groups, in the order the client listed them.
	Watches []NotifyWatch `imapfeature:"notify"`
	// StatusOnSet asks for an immediate STATUS of every watched mailbox, from
	// the STATUS parameter of RFC 5465 section 6. Push them before Notify
	// returns: the framework writes updates queued by then ahead of NOTIFY's
	// tagged OK, as RFC 5465 section 3 requires.
	StatusOnSet bool `imapfeature:"notify"`
	_           struct{}
}

// NotifyWatch is one watch group: a set of mailboxes and the events wanted for
// them.
//
// The specifier and event vocabularies live in the root package as
// [imap.NotifyMailboxSpecifier] and [imap.NotifyEventName], shared with the
// client so the two cannot disagree about a spelling. The framework folds what
// it reads from the wire to the canonical form, so a backend may compare against
// the constants directly.
// Construct with keyed fields only; fields may be added in a future release.
type NotifyWatch struct {
	// Specifier names the group: SELECTED, SELECTED-DELAYED, INBOXES, PERSONAL,
	// SUBSCRIBED, SUBTREE or MAILBOXES. It is an open string because RFC 5465
	// registers further specifiers and a closed set would break on the next one.
	//
	// A specifier the backend does not recognise must be refused, not ignored.
	Specifier imap.NotifyMailboxSpecifier `imapfeature:"notify"`
	// Names lists the mailboxes for SUBTREE and MAILBOXES, and is empty
	// otherwise.
	Names []string `imapfeature:"notify"`
	// Events are the event names wanted for this group. An empty list means
	// the client asked for none, which RFC 5465 section 6 allows as a way to
	// silence a group without removing it.
	//
	// An event the backend cannot serve must be refused, not ignored.
	Events []imap.NotifyEventName `imapfeature:"notify"`
	_      struct{}
}

// canonicalNotifyEvent folds a wire event name to the spelling RFC 5465
// section 5 registers, which is what [imap.NotifyEventName]'s constants carry.
//
// Event names are atoms and therefore case-insensitive on the wire, so a client
// writing "MESSAGENEW" and one writing "MessageNew" mean the same thing and a
// backend must not have to test for both. An unregistered name is passed through
// unchanged rather than dropped: the backend decides whether it can serve it, and
// silently discarding it would leave the client watching for an event that will
// never arrive.
func canonicalNotifyEvent(name string) imap.NotifyEventName {
	for _, known := range imap.NotifyEventNames() {
		if strings.EqualFold(name, string(known)) {
			return known
		}
	}
	return imap.NotifyEventName(name)
}

type sessionUpdaterCore struct {
	mu     sync.RWMutex
	queue  *sessionUpdateQueue
	active bool
}

// validateSessionUpdate rejects an update the framework has no way to deliver.
//
// Queueing one and dropping it in drainNotify would tell the backend its event
// was published and then silently discard it, which is the hardest kind of
// delivery bug to find from either end. NOTIFY reports these as untagged STATUS,
// so an update without one has no wire form at all.
//
// RFC 5465 section 5 does report MailboxName as untagged LIST and the metadata
// events as untagged METADATA. Those need a field on SessionUpdate, which is
// additive when it lands; until then an error is the truthful answer.
func validateSessionUpdate(update *SessionUpdate) error {
	if update == nil || update.Mailbox == "" {
		return fmt.Errorf("imapserver: NOTIFY update requires a mailbox")
	}
	if update.Status == nil {
		return fmt.Errorf("imapserver: NOTIFY update for %q has no Status, which is the only form the framework can deliver", update.Mailbox)
	}
	return nil
}

func (u *sessionUpdaterCore) push(update *SessionUpdate) error {
	if err := validateSessionUpdate(update); err != nil {
		return err
	}
	u.mu.RLock()
	defer u.mu.RUnlock()
	if !u.active || u.queue == nil {
		return ErrUpdaterClosed
	}
	return u.queue.push(update)
}

func (u *sessionUpdaterCore) close() {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.active = false
	u.mu.Unlock()
}

// sessionUpdateQueue is the NOTIFY analogue of updateQueue.
//
// It coalesces state by mailbox and records overflow when distinct mailbox
// updates exceed its bound. The event loop then discards the pending events,
// closes the registration and reports NOTIFICATIONOVERFLOW, as RFC 5465 section
// 5.8 requires. Unlike a lost selection update, this does not invalidate the
// client's sequence-number view, so the connection itself can remain open.
type sessionUpdateQueue struct {
	mu       sync.Mutex
	items    []*SessionUpdate
	maxItems int
	signal   chan struct{}
	closed   bool
	overflow bool
}

func newSessionUpdateQueue(maxItems int) *sessionUpdateQueue {
	if maxItems <= 0 {
		maxItems = 256
	}
	return &sessionUpdateQueue{maxItems: maxItems, signal: make(chan struct{}, 1)}
}

func (q *sessionUpdateQueue) push(update *SessionUpdate) error {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return ErrUpdaterClosed
	}
	// Coalesce by mailbox: NOTIFY reports state, not a history, so a newer
	// event about a mailbox supersedes an older one and queueing both would
	// tell the client the same thing twice.
	for i, existing := range q.items {
		if existing.Mailbox == update.Mailbox {
			q.items[i] = update
			q.mu.Unlock()
			q.wake()
			return nil
		}
	}
	if len(q.items) >= q.maxItems {
		q.items = q.items[1:]
		q.overflow = true
	}
	q.items = append(q.items, update)
	q.mu.Unlock()
	q.wake()
	return nil
}

func (q *sessionUpdateQueue) wake() {
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

func (q *sessionUpdateQueue) popAll() ([]*SessionUpdate, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	items, dropped := q.items, q.overflow
	q.items, q.overflow = nil, false
	return items, dropped
}

func (q *sessionUpdateQueue) close() {
	if q == nil {
		return
	}
	q.mu.Lock()
	q.closed = true
	q.items = nil
	q.mu.Unlock()
}

const featureNotify featureID = "notify"

func init() {
	registerFeatures(featureDescriptor{
		ID: featureNotify,
		Active: func(_ *sessionState, advertised map[string]bool) bool {
			return advertised["NOTIFY"]
		},
	})
	registerCapabilities(
		capabilityDescriptor{
			Name:            "NOTIFY",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: sessionImplements[NotifySession](),
		},
	)
	registerCommand("NOTIFY", stateMaskAuthenticated|stateMaskSelected, false, parseNotify, handleNotify)
}

type notifyArgs struct {
	config          *NotifyConfig
	messageNewFetch bool
}

// parseNotify reads "NOTIFY SET [STATUS] (group events)..." or "NOTIFY NONE".
// RFC 5465 section 6.
func parseNotify(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	var verb string
	if !decoder.ExpectAtom(&verb) {
		return nil, 0, decoder.Err()
	}
	switch strings.ToUpper(verb) {
	case "NONE":
		if !decoder.ExpectCRLF() {
			return nil, 0, decoder.Err()
		}
		return &notifyArgs{}, 0, nil
	case "SET":
	default:
		return nil, 0, fmt.Errorf("unsupported NOTIFY verb %q", verb)
	}
	config := &NotifyConfig{}
	args := &notifyArgs{config: config}
	for decoder.SP() {
		if decoder.PeekAtomEqual("STATUS") {
			var keyword string
			if !decoder.ExpectAtom(&keyword) {
				return nil, 0, decoder.Err()
			}
			config.StatusOnSet = true
			continue
		}
		group, err := parseNotifyGroup(decoder, args)
		if err != nil {
			return nil, 0, err
		}
		config.Watches = append(config.Watches, *group)
	}
	if len(config.Watches) == 0 {
		return nil, 0, fmt.Errorf("NOTIFY SET requires at least one mailbox group")
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return args, int64(len(config.Watches) * 64), nil
}

func parseNotifyGroup(decoder *imapwire.Decoder, args *notifyArgs) (*NotifyWatch, error) {
	group := &NotifyWatch{}
	if !decoder.ExpectSpecial('(') {
		return nil, decoder.Err()
	}
	var specifier string
	if !decoder.ExpectAtom(&specifier) {
		return nil, decoder.Err()
	}
	// Specifiers are registered upper-case, so folding to upper is already the
	// canonical form; events are mixed-case and need the table.
	group.Specifier = imap.NotifyMailboxSpecifier(strings.ToUpper(specifier))
	switch group.Specifier {
	case imap.NotifySubtree, imap.NotifyMailboxes:
		if !decoder.ExpectSP() {
			return nil, decoder.Err()
		}
		readName := func() error {
			var name string
			if !decoder.ExpectMailbox(&name) {
				return decoder.Err()
			}
			group.Names = append(group.Names, name)
			return nil
		}
		if decoder.PeekSpecial('(') {
			if err := decoder.ExpectList(readName); err != nil {
				return nil, err
			}
		} else if err := readName(); err != nil {
			return nil, err
		}
	}
	if !decoder.ExpectSP() {
		return nil, decoder.Err()
	}
	// NONE silences a group without removing it. RFC 5465 section 6.
	if decoder.PeekAtomEqual("NONE") {
		var keyword string
		if !decoder.ExpectAtom(&keyword) || !decoder.ExpectSpecial(')') {
			return nil, decoder.Err()
		}
		return group, nil
	}
	if err := decoder.ExpectList(func() error {
		var event string
		if !decoder.ExpectAtom(&event) {
			return decoder.Err()
		}
		name := canonicalNotifyEvent(event)
		group.Events = append(group.Events, name)
		if name == imap.NotifyEventMessageNew && decoder.SPListAhead() {
			if !decoder.ExpectSP() {
				return decoder.Err()
			}
			items, err := imapcodec.ReadFetchItems(decoder)
			if err != nil {
				return err
			}
			if len(items) == 0 {
				return fmt.Errorf("MessageNew requires at least one fetch attribute")
			}
			args.messageNewFetch = true
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if !decoder.ExpectSpecial(')') {
		return nil, decoder.Err()
	}
	return group, nil
}

func handleNotify(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*notifyArgs)
	if args == nil {
		return c.writeBad(command.tag, "invalid NOTIFY arguments")
	}
	config := args.config
	if err := requireCapability(c, "NOTIFY"); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	// Parse the legal syntax completely, then refuse the unsupported behavior
	// before changing an existing registration or calling the backend.
	if args.messageNewFetch {
		return writeBackendError(c, command.tag, command.name, &imap.Error{
			Type: imap.ErrorTypeNo,
			Code: imap.CodeCannot,
			Text: "MessageNew fetch attributes are unsupported",
		})
	}
	session, ok := c.state.session.(NotifySession)
	if !ok {
		return c.writeBad(command.tag, "NOTIFY is not available")
	}
	// The previous registration is closed first, so a backend that kept its
	// old updater finds it closed rather than delivering into a registration
	// the client has already replaced.
	c.closeNotify()
	if config == nil {
		if err := session.Notify(ctx, nil, nil, nil); err != nil {
			return writeBackendError(c, command.tag, command.name, err)
		}
		return c.writeTagged(command.tag, "OK", command.name+" completed")
	}
	queue := newSessionUpdateQueue(c.server.options.Limits.MaxQueuedUpdates)
	updater := &SessionUpdater{core: &sessionUpdaterCore{queue: queue, active: true}}
	c.notifyQueue, c.notifyUpdater = queue, updater
	if err := session.Notify(ctx, updater, config, nil); err != nil {
		c.closeNotify()
		return writeBackendError(c, command.tag, command.name, err)
	}
	// RFC 5465 section 3: STATUS responses requested by the STATUS indicator
	// precede the tagged OK. The backend pushed them during Notify.
	if err := c.drainNotify(); err != nil {
		return err
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}

// closeNotify tears down the current NOTIFY registration.
func (c *conn) closeNotify() {
	c.notifyUpdater.closeCore()
	c.notifyQueue.close()
	c.notifyUpdater, c.notifyQueue = nil, nil
}

func (u *SessionUpdater) closeCore() {
	if u != nil {
		u.core.close()
	}
}

// notifySignal is the NOTIFY analogue of updateSignal.
func (c *conn) notifySignal() <-chan struct{} {
	if c.notifyQueue == nil {
		return nil
	}
	return c.notifyQueue.signal
}

// drainNotify writes the pending NOTIFY events as untagged STATUS responses.
//
// RFC 5465 section 6 reports events about unselected mailboxes as STATUS, which
// is what lets the framework deliver them without the client holding a
// sequence-number view of a mailbox it never selected.
func (c *conn) drainNotify() error {
	if c.notifyQueue == nil {
		return nil
	}
	updates, dropped := c.notifyQueue.popAll()
	if dropped {
		// Close before calling the backend, so even a producer racing with
		// teardown cannot publish into the ended registration.
		c.closeNotify()
		ctx, cancel := context.WithTimeout(c.ctx, c.server.options.Limits.CommandTimeout)
		err := c.state.session.(NotifySession).Notify(ctx, nil, nil, nil)
		cancel()
		writeUntaggedOK(c, imap.CodeNotificationOverflow, "", "notifications disabled after queue overflow")
		if flushErr := c.encoder.Flush(); flushErr != nil {
			return flushErr
		}
		// If teardown failed, end the connection rather than silently keeping
		// backend resources for a registration the client believes has ended.
		return err
	}
	var advertised map[string]bool
	if len(updates) > 0 {
		advertised = advertisedCapabilities(c)
	}
	for _, update := range updates {
		if update == nil || update.Status == nil {
			continue
		}
		var items []imap.StatusItem
		for item := range update.Status.Values {
			// RFC 5465 section 5 includes HIGHESTMODSEQ once the client has
			// enabled CONDSTORE or QRESYNC, not merely when it is advertised.
			if item == imap.StatusItemHighestModSeq && !condStoreEnabled(c) {
				continue
			}
			if gate, ok := statusItemCapability(item); ok && gate(&c.state, advertised) {
				items = append(items, item)
			}
		}
		if err := imapcodec.WriteStatusResponse(c.encoder, c.statusReply(update.Status, items)); err != nil {
			return err
		}
		if err := c.encoder.Flush(); err != nil {
			return err
		}
	}
	return nil
}
