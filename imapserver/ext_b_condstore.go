package imapserver

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// CONDSTORE (RFC 7162 section 3.1).
//
// CONDSTORE is the one extension in this group that the framework cannot answer
// alone: per-message modification sequences are durable backend state, and a
// conditional STORE's partial failure is information only the backend has. Both
// reach the framework through CondStoreMailbox rather than through a method on
// SelectedMailbox.

// CondStoreResult is the outcome of a conditional STORE.
//
// A conditional STORE that rejects some messages is a *successful* command with
// a MODIFIED response code, not an error — RFC 7162 section 3.1.3. Reporting it
// as an error would lose the messages that did change.
// Construct with keyed fields only; fields may be added in a future release.
type CondStoreResult struct {
	// Modified lists the messages left unmodified because their modification
	// sequence exceeded StoreOptions.UnchangedSince. An empty set means every
	// requested message was stored.
	Modified imap.UIDSet
	// HighestModSeq is the mailbox's modification sequence after the store, or
	// zero when the backend does not report it.
	HighestModSeq uint64
	_             struct{}
}

// CondStoreMailbox is the optional conditional-store operation of CONDSTORE.
// A selected mailbox implements it when the backend witnesses CONDSTORE.
//
// The framework calls StoreCondStore in place of Store only when the client
// supplied UNCHANGEDSINCE; an unconditional STORE still goes to Store, so a
// backend does not implement the same logic twice.
type CondStoreMailbox interface {
	StoreCondStore(ctx context.Context, writer *FetchWriter, uids imap.UIDSet, flags *StoreFlags, options *StoreOptions) (*CondStoreResult, error)
}

func init() {
	registerCapabilities(
		capabilityDescriptor{
			Name:            "CONDSTORE",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: backendSupportsCapability("CONDSTORE"),
			Enable:          func(state *sessionState) bool { return state.enable("CONDSTORE") },
		},
	)
}

// condStoreEnabled reports whether MODSEQ data must accompany FETCH responses.
//
// RFC 7162 section 3.1.8 gives CONDSTORE two routes to being enabled: an
// explicit ENABLE, or the mere use of any CONDSTORE parameter, which enables it
// for the rest of the session as a side effect. Both end up in the same session
// map, so the rest of the code asks one question.
func condStoreEnabled(c *conn) bool {
	return c.state.enabledCapability("CONDSTORE")
}

// enableCondStoreSideEffect implements the second route above. It is deliberately
// not routed through the capability descriptor's Enable hook: that hook answers
// the ENABLE command, whereas this fires when a command *parameter* is used,
// which RFC 7162 treats as equivalent.
func enableCondStoreSideEffect(c *conn) {
	c.state.enable("CONDSTORE")
}

// condStoreAvailable reports whether the session may use CONDSTORE parameters at
// all, which is a question about advertisement rather than about enablement.
func condStoreAvailable(c *conn) bool {
	advertised := advertisedCapabilities(c)
	return advertised["CONDSTORE"] || advertised["QRESYNC"]
}

// parseModSeqModifier reads a parenthesised "(NAME value ...)" command modifier
// such as "(CHANGEDSINCE 12345)" or "(UNCHANGEDSINCE 12345)".
//
// names maps each accepted modifier to whether it carries a number. Unknown
// modifiers are refused here rather than ignored: silently dropping a
// conditional qualifier would turn a conditional command into an unconditional
// one, which is the one failure mode CONDSTORE exists to prevent.
func parseModSeqModifiers(decoder *imapwire.Decoder, accept map[string]bool, into map[string]uint64) error {
	return decoder.ExpectList(func() error {
		var name string
		if !decoder.ExpectAtom(&name) {
			return decoder.Err()
		}
		name = strings.ToUpper(name)
		carriesValue, known := accept[name]
		if !known {
			return fmt.Errorf("unsupported command modifier %q", name)
		}
		if !carriesValue {
			into[name] = 1
			return nil
		}
		// Modification sequences are unsigned 63-bit values on the wire
		// (RFC 7162 section 3.1), so a negative decode is a malformed value
		// rather than a representable one.
		var value int64
		if !decoder.ExpectSP() || !decoder.ExpectNumber64(&value) {
			return decoder.Err()
		}
		if value < 0 {
			return fmt.Errorf("modification sequence %d is negative", value)
		}
		into[name] = uint64(value)
		return nil
	})
}

// parseFetchModifiers reads the optional modifier list that follows FETCH's
// data items: "(CHANGEDSINCE 12345)" from CONDSTORE and "VANISHED" from
// QRESYNC, which may appear together.
func parseFetchModifiers(decoder *imapwire.Decoder, args *fetchArgs) error {
	if !decoder.SPListAhead() {
		return nil
	}
	if !decoder.ExpectSP() {
		return decoder.Err()
	}
	values := make(map[string]uint64)
	if err := parseModSeqModifiers(decoder, map[string]bool{
		"CHANGEDSINCE": true,
		"VANISHED":     false,
	}, values); err != nil {
		return err
	}
	args.changedSince = values["CHANGEDSINCE"]
	_, args.vanished = values["VANISHED"]
	// RFC 7162 section 3.2.6: VANISHED is only meaningful alongside
	// CHANGEDSINCE, since it reports what was removed since that point.
	if args.vanished && args.changedSince == 0 {
		return fmt.Errorf("FETCH VANISHED requires CHANGEDSINCE")
	}
	return nil
}

// parseStoreModifiers reads the optional modifier list that precedes STORE's
// operation: "(UNCHANGEDSINCE 12345)" from CONDSTORE.
func parseStoreModifiers(decoder *imapwire.Decoder, args *storeArgs) error {
	if !decoder.PeekSpecial('(') {
		return nil
	}
	values := make(map[string]uint64)
	if err := parseModSeqModifiers(decoder, map[string]bool{"UNCHANGEDSINCE": true}, values); err != nil {
		return err
	}
	args.unchangedSince = values["UNCHANGEDSINCE"]
	if !decoder.ExpectSP() {
		return decoder.Err()
	}
	return nil
}

// selectArgs carries SELECT's mailbox name and its optional parameter list.
// SELECT took a bare mailbox name until CONDSTORE and QRESYNC added parameters
// to it.
type selectArgs struct {
	mailbox   string
	condStore bool
	qresync   *QResyncSelect
}

// parseSelect reads SELECT's or EXAMINE's mailbox name and the optional
// parameter list of RFC 7162 sections 3.1.8 and 3.2.5.
func parseSelect(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &selectArgs{}
	if !decoder.ExpectMailbox(&args.mailbox) {
		return nil, 0, decoder.Err()
	}
	if decoder.SPListAhead() {
		if !decoder.ExpectSP() {
			return nil, 0, decoder.Err()
		}
		if err := decoder.ExpectList(func() error {
			var name string
			if !decoder.ExpectAtom(&name) {
				return decoder.Err()
			}
			switch strings.ToUpper(name) {
			case "CONDSTORE":
				args.condStore = true
				return nil
			case "QRESYNC":
				if !decoder.ExpectSP() {
					return decoder.Err()
				}
				qresync, err := parseQResyncParams(decoder)
				if err != nil {
					return err
				}
				args.qresync = qresync
				return nil
			default:
				return fmt.Errorf("unsupported SELECT parameter %q", name)
			}
		}); err != nil {
			return nil, 0, err
		}
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return args, int64(len(args.mailbox)) + 64, nil
}

// applySelectParams validates SELECT's parameters and maps them onto
// SelectOptions.
func applySelectParams(c *conn, args *selectArgs, options *SelectOptions) error {
	if args.qresync != nil {
		if !advertisedCapabilities(c)["QRESYNC"] {
			return fmt.Errorf("SELECT QRESYNC requires QRESYNC")
		}
		// RFC 7162 section 3.2.5 requires QRESYNC to have been enabled before
		// it is used as a selection parameter, because the client must already
		// be prepared to receive VANISHED responses.
		if !c.state.enabledCapability("QRESYNC") {
			return fmt.Errorf("SELECT QRESYNC requires ENABLE QRESYNC first")
		}
		options.QResync = args.qresync
		// QRESYNC implies CONDSTORE. RFC 7162 section 3.2.
		options.CondStore = true
		enableCondStoreSideEffect(c)
		return nil
	}
	if err := validateCondStoreUse(c, args.condStore, "SELECT CONDSTORE"); err != nil {
		return err
	}
	options.CondStore = args.condStore
	return nil
}

// applyCondStoreFetchItems adds MODSEQ to a FETCH when CONDSTORE is enabled.
//
// RFC 7162 section 3.1.4.1 requires the server to include MODSEQ in every FETCH
// response once CONDSTORE is enabled, whether or not the client asked for it —
// the client is expected to track modseqs from that point on, and a response
// without one would leave a hole in its record.
func applyCondStoreFetchItems(c *conn, items []imap.FetchItem) []imap.FetchItem {
	if !condStoreEnabled(c) {
		return items
	}
	for _, item := range items {
		if keyword, ok := item.(imap.FetchItemKeyword); ok && strings.EqualFold(string(keyword), string(imap.FetchItemModSeq)) {
			return items
		}
	}
	return append(slices.Clone(items), imap.FetchItemModSeq)
}

// stripModSeqUnlessEnabled removes MODSEQ from a response when CONDSTORE is not
// enabled for the session.
//
// STORE's untagged FETCH responses must carry MODSEQ once CONDSTORE is enabled
// (RFC 7162 section 3.1.4.2), and must not carry it otherwise — a rev1 client
// that never asked for modseqs should not have to parse them. Only the backend
// knows the value, so it always reports it and the framework removes it here,
// the same way mapFetchResponse removes the UID it requested internally.
func stripModSeqUnlessEnabled(c *conn, data *imap.FetchMessageData) *imap.FetchMessageData {
	if data == nil || condStoreEnabled(c) {
		return data
	}
	for key := range data.Items {
		if strings.EqualFold(string(key), string(imap.FetchItemModSeq)) {
			delete(data.Items, key)
		}
	}
	return data
}

// validateCondStoreUse refuses a CONDSTORE parameter the session may not use,
// and records the enabling side effect when it may.
func validateCondStoreUse(c *conn, used bool, what string) error {
	if !used {
		return nil
	}
	if !condStoreAvailable(c) {
		return fmt.Errorf("%s requires CONDSTORE", what)
	}
	enableCondStoreSideEffect(c)
	return nil
}

// storeCondStore runs a conditional STORE and reports the rejected messages.
//
// A backend that witnesses CONDSTORE but whose selected mailbox does not
// implement CondStoreMailbox is a backend bug, not a client error: the
// capability was advertised on its behalf. It is reported as such rather than
// silently degrading to an unconditional store, which would modify messages the
// client explicitly asked to protect.
func storeCondStore(ctx context.Context, c *conn, writer *FetchWriter, uids imap.UIDSet, flags *StoreFlags, options *StoreOptions) (*CondStoreResult, error) {
	mailbox, ok := c.state.selected.mailbox.(CondStoreMailbox)
	if !ok {
		return nil, fmt.Errorf("imapserver: backend advertises CONDSTORE but the selected mailbox does not implement CondStoreMailbox")
	}
	result, err := mailbox.StoreCondStore(ctx, writer, uids, flags, options)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return &CondStoreResult{}, nil
	}
	return result, nil
}

// condStoreModifiedArgs renders the MODIFIED response code arguments, or reports
// that there is nothing to report.
func condStoreModifiedArgs(result *CondStoreResult) (string, bool) {
	if result == nil || result.Modified.IsEmpty() || result.Modified.Dynamic() {
		return "", false
	}
	return result.Modified.String(), true
}
