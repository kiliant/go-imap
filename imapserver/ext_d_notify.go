package imapserver

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	// negotiated, including HIGHESTMODSEQ until CONDSTORE is enabled. The
	// STATUS responses pushed during a NOTIFY SET STATUS keep HIGHESTMODSEQ
	// whenever FlagChange or AnnotationChange is registered and the server
	// advertises CONDSTORE or QRESYNC, as RFC 5465 section 3 requires.
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
// Queue overflow, or MessageNew data that cannot be delivered, also closes the
// updater and calls Notify with a nil updater and configuration, just like
// NOTIFY NONE.
//
// Before any of that, the framework applies RFC 5465's rules itself: an
// invalid registration is BAD, and one naming an event the framework cannot
// deliver is NO [BADEVENT], in both cases without calling Notify or touching
// the current registration. A backend therefore sees only legal
// configurations of MessageNew, MessageExpunge and FlagChange, but must still
// refuse what it cannot serve, since a later release may pass more. A
// BADEVENT refusal from Notify reaches the client with the server's complete
// event list.
//
// Events for the selected mailbox are the framework's to filter. It reports
// what the selection's Updater publishes according to the SELECTED or
// SELECTED-DELAYED group, keeping the client's sequence numbers stable while
// it withholds them, so a backend publishes every change there regardless of
// the registration. The framework also delivers that group's MessageNew fetch
// attributes through [SelectedMailbox.Fetch]; see
// [NotifyWatch.MessageNewFetchItems]. A successful SET reports accumulated selected changes,
// including flags, before its tagged OK. If Notify refuses a replacement,
// the previous selected policy remains in force, although its session updater
// has been closed as described above.
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
	// MessageNewFetchItems is the fetch-att list the client attached to
	// MessageNew (RFC 5465 section 5.2). Only a SELECTED or SELECTED-DELAYED
	// group can carry one, and it is empty otherwise. The framework delivers
	// it, so a backend need not act on this field. When a drain announces new
	// messages in the selected mailbox, it asks [SelectedMailbox.Fetch] for
	// their data in at most one call and sends unsolicited FETCH responses. The
	// request carries these items adjusted as for a FETCH command: UID is
	// added, MODSEQ is added while CONDSTORE is in effect for the selection
	// and removed otherwise. Additions from a batch carrying the origin of
	// one of this connection's commands ([MutationOptions.Origin]) are not
	// fetched.
	MessageNewFetchItems []imap.FetchItem `imapfeature:"notify"`
	_                    struct{}
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
	for _, known := range append(imap.NotifyEventNames(), notifyEventAnnotationChange) {
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
	config *NotifyConfig
}

// parseNotify reads "NOTIFY SET [STATUS] (group events)..." or "NOTIFY NONE".
// RFC 5465 section 8:
//
//	notify-set = "SET" [status-indicator] SP event-groups
//
// Only the grammar is checked here; the rules that relate groups and events
// to each other are validateNotifySet's.
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
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	// The STATUS indicator comes once, before the first group.
	if decoder.PeekAtomEqual("STATUS") {
		var keyword string
		if !decoder.ExpectAtom(&keyword) || !decoder.ExpectSP() {
			return nil, 0, decoder.Err()
		}
		config.StatusOnSet = true
	}
	for {
		group, err := parseNotifyGroup(decoder)
		if err != nil {
			return nil, 0, err
		}
		config.Watches = append(config.Watches, *group)
		if !decoder.SP() {
			break
		}
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return args, int64(len(config.Watches) * 64), nil
}

// parseNotifyGroup reads one event-group, with MessageNew's fetch-att list.
func parseNotifyGroup(decoder *imapwire.Decoder) (*NotifyWatch, error) {
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
		// one-or-more-mailbox: "()" names nothing and is not a list.
		if len(group.Names) == 0 {
			return nil, fmt.Errorf("NOTIFY %s requires at least one mailbox", group.Specifier)
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
			group.MessageNewFetchItems = items
		}
		return nil
	}); err != nil {
		return nil, err
	}
	// events = "(" event *(SP event) ")" / "NONE": an empty list is neither.
	if len(group.Events) == 0 {
		return nil, fmt.Errorf("NOTIFY event list is empty")
	}
	if !decoder.ExpectSpecial(')') {
		return nil, decoder.Err()
	}
	return group, nil
}

// notifyEventAnnotationChange is RFC 5465's message event for RFC 5257
// annotations. The root vocabulary has no constant for it because this
// library implements no ANNOTATE, but the pairing rules still name it.
const notifyEventAnnotationChange imap.NotifyEventName = "AnnotationChange"

// notifyDeliverableEvents are the events the framework can put on the wire:
// for the selected mailbox through the selection's update stream, and for
// other mailboxes as the untagged STATUS a SessionUpdate carries. Every other
// event needs a response form the framework does not have (LIST, METADATA,
// ANNOTATION), so no backend can serve it through this framework. The
// framework itself serves all three for the selected mailbox, which makes this
// the server's complete list for BADEVENT.
var notifyDeliverableEvents = []imap.NotifyEventName{
	imap.NotifyEventMessageNew,
	imap.NotifyEventMessageExpunge,
	imap.NotifyEventFlagChange,
}

func notifyBadEventArgs() string {
	names := make([]string, len(notifyDeliverableEvents))
	for i, event := range notifyDeliverableEvents {
		names[i] = string(event)
	}
	return "(" + strings.Join(names, " ") + ")"
}

func isNotifyMessageEvent(event imap.NotifyEventName) bool {
	switch event {
	case imap.NotifyEventMessageNew, imap.NotifyEventMessageExpunge, imap.NotifyEventFlagChange, notifyEventAnnotationChange:
		return true
	}
	return false
}

func isNotifySelectedSpecifier(specifier imap.NotifyMailboxSpecifier) bool {
	return specifier == imap.NotifySelected || specifier == imap.NotifySelectedDelayed
}

// validateNotifySet applies RFC 5465's rules to a parsed NOTIFY SET before
// anything changes: no backend call, no registration closed. Every rule that
// makes the command invalid is checked before any refusal of a valid but
// unsupported request, so a malformed command is BAD whatever else it names.
func validateNotifySet(args *notifyArgs) *imap.Error {
	bad := func(text string) *imap.Error { return &imap.Error{Type: imap.ErrorTypeBad, Text: text} }
	selectedGroups := 0
	for _, watch := range args.config.Watches {
		// filter-mailboxes is a closed production; a specifier outside it is
		// unparseable rather than unsupported.
		if !slices.Contains(imap.NotifyMailboxSpecifiers(), watch.Specifier) {
			return bad("unknown NOTIFY mailbox specifier " + string(watch.Specifier))
		}
		selected := isNotifySelectedSpecifier(watch.Specifier)
		if selected {
			// Section 6.1: only one specifier affecting the selected mailbox.
			if selectedGroups++; selectedGroups > 1 {
				return bad("NOTIFY accepts one SELECTED or SELECTED-DELAYED group")
			}
		}
		// Section 8: a fetch-att list only for the selected mailbox.
		if len(watch.MessageNewFetchItems) != 0 && !selected {
			return bad("MessageNew fetch attributes apply only to SELECTED and SELECTED-DELAYED")
		}
		has := func(event imap.NotifyEventName) bool { return slices.Contains(watch.Events, event) }
		// Section 5: MessageNew and MessageExpunge come together, and the
		// per-message change events require both.
		if has(imap.NotifyEventMessageNew) != has(imap.NotifyEventMessageExpunge) {
			return bad("NOTIFY MessageNew and MessageExpunge must be requested together")
		}
		if (has(imap.NotifyEventFlagChange) || has(notifyEventAnnotationChange)) && !has(imap.NotifyEventMessageNew) {
			return bad("NOTIFY FlagChange and AnnotationChange require MessageNew and MessageExpunge")
		}
		// Section 6.1: only message events for the selected mailbox. A name
		// this library has never heard of may be a newer message event, so
		// it is left to the BADEVENT refusal below rather than called invalid.
		if selected {
			for _, event := range watch.Events {
				if !isNotifyMessageEvent(event) && slices.Contains(imap.NotifyEventNames(), event) {
					return bad("NOTIFY " + string(event) + " does not apply to the selected mailbox")
				}
			}
		}
	}
	// Section 3.1: an unsupported event is NO, not BAD, and BADEVENT lists
	// every event the server supports.
	for _, watch := range args.config.Watches {
		for _, event := range watch.Events {
			if !slices.Contains(notifyDeliverableEvents, event) {
				return &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeBadEvent, CodeArgs: notifyBadEventArgs(), Text: "unsupported NOTIFY event " + string(event)}
			}
		}
	}
	// Section 5.2: a client SHOULD NOT ask for attributes that set \Seen. The
	// framework fetches them unsolicited for every new message, so it refuses
	// them rather than mark messages read that the client never opened.
	for _, watch := range args.config.Watches {
		if fetchItemsSetSeen(watch.MessageNewFetchItems) {
			return &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeCannot, Text: "MessageNew fetch attributes must not set \\Seen; use BODY.PEEK or BINARY.PEEK"}
		}
	}
	return nil
}

// validateMessageNewFetchItems gates the selected group's MessageNew fetch
// attributes as FETCH gates its items, and treats MODSEQ among them as a
// CONDSTORE enabling use, as FETCH MODSEQ is (RFC 7162 section 3.1). The
// registration outlives the selection, so a NOMODSEQ selection does not
// refuse it; MODSEQ is then left out of what that selection is sent.
func validateMessageNewFetchItems(ctx context.Context, c *conn, config *NotifyConfig) error {
	for _, watch := range config.Watches {
		if len(watch.MessageNewFetchItems) == 0 {
			continue
		}
		if err := requireFetchItemCapabilities(c, watch.MessageNewFetchItems); err != nil {
			return err
		}
		if fetchItemsHaveModSeq(watch.MessageNewFetchItems) {
			if err := enableCondStoreForCommand(ctx, c, true, "NOTIFY MessageNew MODSEQ"); err != nil {
				return err
			}
		}
	}
	return nil
}

func fetchItemsHaveModSeq(items []imap.FetchItem) bool {
	for _, item := range items {
		if keyword, ok := item.(imap.FetchItemKeyword); ok && strings.EqualFold(string(keyword), string(imap.FetchItemModSeq)) {
			return true
		}
	}
	return false
}

// completeBadEvent replaces a backend's BADEVENT list with the server's.
// A backend lists what it serves for other mailboxes; RFC 5465 section 3.1
// requires every event the server supports, including those the framework
// serves for the selected mailbox.
func completeBadEvent(err error) error {
	var protocol *imap.Error
	if !errors.As(err, &protocol) || !strings.EqualFold(string(protocol.Code), string(imap.CodeBadEvent)) {
		return err
	}
	complete := *protocol
	complete.CodeArgs = notifyBadEventArgs()
	return &complete
}

// selectedNotifyPolicyFor is the selected-mailbox half of a registration. A
// SET without a SELECTED or SELECTED-DELAYED group means SELECTED NONE (RFC
// 5465 section 3.1); a nil configuration is NOTIFY NONE.
func selectedNotifyPolicyFor(config *NotifyConfig) selectedNotifyPolicy {
	policy := selectedNotifyPolicy{registered: true}
	if config == nil {
		return policy
	}
	for _, watch := range config.Watches {
		if !isNotifySelectedSpecifier(watch.Specifier) {
			continue
		}
		policy.immediate = watch.Specifier == imap.NotifySelected
		policy.messages = slices.Contains(watch.Events, imap.NotifyEventMessageNew)
		policy.flags = slices.Contains(watch.Events, imap.NotifyEventFlagChange)
		if policy.messages {
			policy.fetchItems = slices.Clone(watch.MessageNewFetchItems)
		}
	}
	return policy
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
	// Every refusal the framework can decide is answered here, before the
	// previous registration is closed or the backend is asked.
	if config != nil {
		if refusal := validateNotifySet(args); refusal != nil {
			return writeTaggedCondition(c, command.tag, strings.ToUpper(string(refusal.Type)), refusal.Code, refusal.CodeArgs, refusal.Text)
		}
		if err := validateMessageNewFetchItems(ctx, c, config); err != nil {
			return writeCondStoreUseError(c, command.tag, command.name, err)
		}
	}
	session, ok := c.state.session.(NotifySession)
	if !ok {
		return c.writeBad(command.tag, "NOTIFY is not available")
	}
	// The previous registration is closed first, so a backend that kept its
	// old updater finds it closed rather than delivering into a registration
	// the client has already replaced. A refusal by the backend therefore
	// leaves no registration for other mailboxes; the selected mailbox keeps
	// its previous policy, which only a successful NOTIFY changes.
	c.closeNotify()
	if config == nil {
		if err := session.Notify(ctx, nil, nil, nil); err != nil {
			return writeBackendError(c, command.tag, command.name, err)
		}
		// NOTIFY NONE has no implicit NOOP: what is pending stays pending.
		c.notifySelected = selectedNotifyPolicyFor(nil)
		return c.writeTagged(command.tag, "OK", command.name+" completed")
	}
	queue := newSessionUpdateQueue(c.server.options.Limits.MaxQueuedUpdates)
	updater := &SessionUpdater{core: &sessionUpdaterCore{queue: queue, active: true}}
	c.notifyQueue, c.notifyUpdater = queue, updater
	if err := session.Notify(ctx, updater, config, nil); err != nil {
		c.closeNotify()
		return writeBackendError(c, command.tag, command.name, completeBadEvent(err))
	}
	// RFC 5465 section 3: STATUS responses requested by the STATUS indicator
	// precede the tagged OK. The backend pushed them during Notify.
	if err := c.drainNotifyStatus(initialStatusModSeq(c, config)); err != nil {
		return err
	}
	// A successful SET is an implicit NOOP: it returns every change to the
	// selected mailbox accumulated so far, including those the previous
	// registration withheld, before the new policy filters what follows.
	c.notifySelected = selectedNotifyPolicyFor(config)
	c.releasingNotify = true
	err := c.drainUpdates(updateAccounting{})
	c.releasingNotify = false
	if err != nil {
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
	return c.drainNotifyStatus(false)
}

// initialStatusModSeq reports whether the STATUS responses a NOTIFY SET STATUS
// writes before its tagged OK carry HIGHESTMODSEQ although CONDSTORE is not
// enabled. RFC 5465 section 3 requires it there when FlagChange or
// AnnotationChange is registered and the server supports CONDSTORE or QRESYNC;
// later event STATUS responses carry it only once the client has enabled
// CONDSTORE (section 5.2).
func initialStatusModSeq(c *conn, config *NotifyConfig) bool {
	if config == nil || !config.StatusOnSet {
		return false
	}
	advertised := advertisedCapabilities(c)
	if !advertised["CONDSTORE"] && !advertised["QRESYNC"] {
		return false
	}
	for _, watch := range config.Watches {
		for _, event := range watch.Events {
			if event == imap.NotifyEventFlagChange || event == notifyEventAnnotationChange {
				return true
			}
		}
	}
	return false
}

// drainNotifyStatus is drainNotify, with HIGHESTMODSEQ written regardless of
// CONDSTORE enabling when modSeq is set.
func (c *conn) drainNotifyStatus(modSeq bool) error {
	if c.notifyQueue == nil {
		return nil
	}
	updates, dropped := c.notifyQueue.popAll()
	if dropped {
		return c.notifyOverflow("notifications disabled after queue overflow")
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
			// RFC 5465 section 5 includes HIGHESTMODSEQ in event STATUS
			// responses once the client has enabled CONDSTORE or QRESYNC, not
			// merely when it is advertised.
			if item == imap.StatusItemHighestModSeq {
				if modSeq || condStoreEnabled(c) {
					items = append(items, item)
				}
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

// notifyOverflow ends the registration as RFC 5465 section 5.8 requires when
// notifications cannot be delivered: an untagged OK [NOTIFICATIONOVERFLOW],
// then behaviour as if NOTIFY NONE had been received, which silences the
// selected mailbox too.
func (c *conn) notifyOverflow(text string) error {
	// Close before calling the backend, so even a producer racing with
	// teardown cannot publish into the ended registration.
	c.closeNotify()
	c.notifySelected = selectedNotifyPolicyFor(nil)
	ctx, cancel := context.WithTimeout(c.ctx, c.server.options.Limits.CommandTimeout)
	err := c.state.session.(NotifySession).Notify(ctx, nil, nil, nil)
	cancel()
	writeUntaggedOK(c, imap.CodeNotificationOverflow, "", text)
	if flushErr := c.encoder.Flush(); flushErr != nil {
		return flushErr
	}
	// If teardown failed, end the connection rather than silently keeping
	// backend resources for a registration the client believes has ended.
	return err
}

// writeMessageNewFetches sends the FETCH data that a SELECTED or
// SELECTED-DELAYED MessageNew registration asks for, for the messages a drain
// has just announced with EXISTS (RFC 5465 section 5.2). Additions from a
// batch carrying the origin of this connection's own command are left out,
// as that section recommends.
//
// At most one SelectedMailbox.Fetch call supplies the data, bounded by
// Limits.CommandTimeout, and each response is numbered in the client's view
// as it stands after the drain's other responses, under FETCH's response size
// bound. A message no longer in the view is skipped. If Fetch returns an
// error or the bound is exceeded, the registration ends with
// NOTIFICATIONOVERFLOW (section 5.8) and responses already sent stand. A
// response that cannot be mapped or measured counts as a Fetch error; only a
// failure while writing a response ends the connection.
func (c *conn) writeMessageNewFetches(updates []deliveredUpdate) error {
	selected := c.state.selected
	requested := c.notifySelected.fetchItems
	if selected == nil || len(requested) == 0 {
		return nil
	}
	var uids []imap.UID
	for _, update := range updates {
		if update.kind != updateExists {
			continue
		}
		for _, uid := range update.added {
			if _, visible := selected.sequence(uid); visible {
				uids = append(uids, uid)
			}
		}
	}
	if len(uids) == 0 {
		return nil
	}
	// Once CONDSTORE is in effect every FETCH carries MODSEQ (RFC 7162
	// section 3.1.4.1); otherwise none can, which leaves out a requested
	// MODSEQ in a NOMODSEQ selection. A list left empty sends nothing: a
	// FETCH response needs at least one item.
	usesModSeq := condStoreMailboxEnabled(c)
	if !usesModSeq {
		requested = slices.DeleteFunc(slices.Clone(requested), func(item imap.FetchItem) bool {
			return fetchItemsHaveModSeq([]imap.FetchItem{item})
		})
		if len(requested) == 0 {
			return nil
		}
	}
	items, requestedUID := withFetchUID(requested)
	items = applyCondStoreFetchItems(c, items)
	// UIDONLY needs the UID as the UIDFETCH response's subject.
	includeUID := requestedUID || uidOnlyEnabled(c) || condStoreRequiresUID(c) && usesModSeq
	ctx, cancel := context.WithTimeout(c.ctx, c.server.options.Limits.CommandTimeout)
	defer cancel()
	var responseBytes int64
	var writeErr error
	writer := newFetchWriter(func(_ context.Context, data *imap.FetchMessageData) error {
		mapped, err := mapFetchResponse(selected, data, includeUID)
		if err != nil {
			return err
		}
		if len(mapped.Items) == 0 {
			return nil
		}
		_, cleanup, err := prepareFetchResponseLiterals(mapped, maxCommandFetchBytes-responseBytes)
		if err != nil {
			return err
		}
		defer cleanup()
		wireBytes, err := fetchResponseWireSize(mapped)
		if err != nil {
			return err
		}
		if wireBytes > maxCommandFetchBytes-responseBytes {
			return commandLimitError("MessageNew FETCH byte limit exceeded")
		}
		responseBytes += wireBytes
		if err := writeFetchLikeResponse(c, mapped); err != nil {
			writeErr = err
			return err
		}
		if err := c.encoder.Flush(); err != nil {
			writeErr = err
			return err
		}
		return nil
	})
	err := selected.mailbox.Fetch(ctx, writer, imap.UIDSetNum(uids...), &FetchOptions{Items: items})
	writer.core.close()
	if writeErr != nil {
		return writeErr
	}
	if err != nil {
		c.reportBackendError("FETCH", err)
		return c.notifyOverflow("notifications disabled: MessageNew data could not be delivered")
	}
	return nil
}

// selectedNotifyPolicy is what the client asked to hear about the selected
// mailbox. Backends publish every change through the selection's Updater
// regardless; the framework decides what reaches the wire, because only it
// holds the client's sequence-number view that suppression must preserve.
type selectedNotifyPolicy struct {
	// registered is false until the first successful NOTIFY. Before it, the
	// selected mailbox reports MessageNew, MessageExpunge and FlagChange as
	// RFC 3501 always has (RFC 5465 section 3.1).
	registered bool
	// messages is MessageNew with MessageExpunge, which RFC 5465 section 5
	// only accepts together.
	messages bool
	// flags is FlagChange.
	flags bool
	// immediate is SELECTED, as opposed to SELECTED-DELAYED: removals are
	// delivered as they happen rather than at the next command that allows
	// them (RFC 5465 section 6.1).
	immediate bool
	// fetchItems is MessageNew's fetch-att list, fetched for each new
	// message after its EXISTS. See writeMessageNewFetches.
	fetchItems []imap.FetchItem
}

func (p selectedNotifyPolicy) suppressesMessages() bool { return p.registered && !p.messages }
func (p selectedNotifyPolicy) suppressesFlags() bool    { return p.registered && !p.flags }
func (p selectedNotifyPolicy) flagChangeRegistered() bool {
	return p.registered && p.flags
}
func (p selectedNotifyPolicy) immediateRemovals() bool {
	return p.registered && p.messages && p.immediate
}
