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
// NOTIFY registration has been replaced or the session has ended.
func (u *SessionUpdater) Push(update *SessionUpdate) error {
	if u == nil {
		return ErrUpdaterClosed
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
	// Status carries the mailbox's new state. The framework reports the items
	// the client asked NOTIFY to include.
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
	// Mailboxes are the watch groups, in the order the client listed them.
	Mailboxes []NotifyMailboxes
	// StatusOnSet asks for an immediate STATUS of every watched mailbox, from
	// the STATUS parameter of RFC 5465 section 6.
	StatusOnSet bool
	_           struct{}
}

// NotifyMailboxes is one watch group: a set of mailboxes and the events wanted
// for them.
// Construct with keyed fields only; fields may be added in a future release.
type NotifyMailboxes struct {
	// Specifier names the group: SELECTED, INBOXES, PERSONAL, SUBSCRIBED,
	// SUBTREE or MAILBOXES. It is an open string because RFC 5465 registers
	// further specifiers and a closed set would break on the next one.
	Specifier NotifyMailboxSpecifier
	// Names lists the mailboxes for SUBTREE and MAILBOXES, and is empty
	// otherwise.
	Names []string
	// Events are the event names wanted for this group. An empty list means
	// the client asked for none, which RFC 5465 section 6 allows as a way to
	// silence a group without removing it.
	Events []NotifyEvent
	_      struct{}
}

// NotifyMailboxSpecifier names a NOTIFY watch group. Open by design.
type NotifyMailboxSpecifier string

// Watch groups from RFC 5465 section 6.
const (
	NotifySelected   NotifyMailboxSpecifier = "SELECTED"
	NotifyInboxes    NotifyMailboxSpecifier = "INBOXES"
	NotifyPersonal   NotifyMailboxSpecifier = "PERSONAL"
	NotifySubscribed NotifyMailboxSpecifier = "SUBSCRIBED"
	NotifySubtree    NotifyMailboxSpecifier = "SUBTREE"
	NotifyMailboxSet NotifyMailboxSpecifier = "MAILBOXES"
)

// NotifyEvent names an event a client asked to be told about. Open by design:
// RFC 5465 registers further events and IMAP extensions add more.
type NotifyEvent string

// Events from RFC 5465 section 5.
const (
	NotifyMessageNew     NotifyEvent = "MESSAGENEW"
	NotifyMessageExpunge NotifyEvent = "MESSAGEEXPUNGE"
	NotifyFlagChange     NotifyEvent = "FLAGCHANGE"
	NotifyMailboxName    NotifyEvent = "MAILBOXNAME"
	NotifySubscription   NotifyEvent = "SUBSCRIPTIONCHANGE"
)

type sessionUpdaterCore struct {
	mu     sync.RWMutex
	queue  *sessionUpdateQueue
	active bool
}

func (u *sessionUpdaterCore) push(update *SessionUpdate) error {
	if update == nil || update.Mailbox == "" {
		return fmt.Errorf("imapserver: NOTIFY update requires a mailbox")
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
// It drops the oldest event on overflow rather than terminating the connection,
// which is the opposite of the selection queue's rule and deliberately so: a
// dropped selection update would desynchronise a sequence-number view the client
// is actively using, while a dropped NOTIFY event costs the client only a
// refresh it can make itself. RFC 5465 section 6 anticipates this by allowing a
// server to stop notifying and say so.
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

func init() {
	registerCapabilities(
		capabilityDescriptor{
			Name:            "NOTIFY",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: sessionImplements[NotifySession](),
		},
	)
	registerCommand("NOTIFY", stateMaskAuthenticated|stateMaskSelected, false, parseNotify, handleNotify)
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
		return (*NotifyConfig)(nil), 0, nil
	case "SET":
	default:
		return nil, 0, fmt.Errorf("unsupported NOTIFY verb %q", verb)
	}
	config := &NotifyConfig{}
	for decoder.SP() {
		if decoder.PeekAtomEqual("STATUS") {
			var keyword string
			if !decoder.ExpectAtom(&keyword) {
				return nil, 0, decoder.Err()
			}
			config.StatusOnSet = true
			continue
		}
		group, err := parseNotifyGroup(decoder)
		if err != nil {
			return nil, 0, err
		}
		config.Mailboxes = append(config.Mailboxes, *group)
	}
	if len(config.Mailboxes) == 0 {
		return nil, 0, fmt.Errorf("NOTIFY SET requires at least one mailbox group")
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return config, int64(len(config.Mailboxes) * 64), nil
}

func parseNotifyGroup(decoder *imapwire.Decoder) (*NotifyMailboxes, error) {
	group := &NotifyMailboxes{}
	if !decoder.ExpectSpecial('(') {
		return nil, decoder.Err()
	}
	var specifier string
	if !decoder.ExpectAtom(&specifier) {
		return nil, decoder.Err()
	}
	group.Specifier = NotifyMailboxSpecifier(strings.ToUpper(specifier))
	switch group.Specifier {
	case NotifySubtree, NotifyMailboxSet:
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
		group.Events = append(group.Events, NotifyEvent(strings.ToUpper(event)))
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
	config, _ := command.args.(*NotifyConfig)
	if err := requireCapability(c, "NOTIFY"); err != nil {
		return c.writeBad(command.tag, err.Error())
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
	for _, update := range updates {
		if update == nil || update.Status == nil {
			continue
		}
		if err := imapcodec.WriteStatusResponse(c.encoder, update.Status); err != nil {
			return err
		}
		if err := c.encoder.Flush(); err != nil {
			return err
		}
	}
	if dropped {
		// The client is told rather than left believing it saw everything. A
		// silently truncated notification stream is worse than none, because
		// the client stops polling on the strength of it.
		writeUntaggedOK(c, imap.ResponseCode("NOTIFICATIONOVERFLOW"), "", "some notifications were dropped")
		if err := c.encoder.Flush(); err != nil {
			return err
		}
	}
	return nil
}
