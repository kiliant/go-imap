package imapserver

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// LIST-EXTENDED (RFC 5258), LIST-STATUS (RFC 5819), CHILDREN (RFC 3348) and
// SPECIAL-USE (RFC 6154).
//
// LIST-EXTENDED's own capability descriptor already exists in capability.go,
// gated on the frameworkListExtend component, which this file's handling is what
// finally turns on. The rest register here.

// LIST selection options. RFC 5258 section 3, RFC 6154 section 5.1.
const (
	listSelectSubscribed     = "SUBSCRIBED"
	listSelectRemote         = "REMOTE"
	listSelectRecursiveMatch = "RECURSIVEMATCH"
	listSelectSpecialUse     = "SPECIAL-USE"
)

// LIST return options. RFC 5258 section 3, RFC 5819 section 2, RFC 6154
// section 5.2.
const (
	listReturnSubscribed = "SUBSCRIBED"
	listReturnChildren   = "CHILDREN"
	listReturnStatus     = "STATUS"
	listReturnSpecialUse = "SPECIAL-USE"
)

func init() {
	registerCapabilities(
		// LIST-STATUS is answered by the framework through Session.Status, so it
		// needs no backend witness — but it is meaningless without the extended
		// LIST syntax that carries the RETURN clause.
		capabilityDescriptor{
			Name:    "LIST-STATUS",
			States:  stateMaskAuthenticated | stateMaskSelected,
			Depends: []string{"LIST-EXTENDED"},
		},
		// The remaining three need the backend to report data only it has: child
		// existence, use attributes, and acceptance of USE on CREATE.
		capabilityDescriptor{
			Name:            "CHILDREN",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: backendSupportsCapability("CHILDREN"),
		},
		capabilityDescriptor{
			Name:            "SPECIAL-USE",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: backendSupportsCapability("SPECIAL-USE"),
		},
		capabilityDescriptor{
			Name:            "CREATE-SPECIAL-USE",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: backendSupportsCapability("CREATE-SPECIAL-USE"),
			Depends:         []string{"SPECIAL-USE"},
		},
		// WITHIN adds the OLDER and YOUNGER search keys. They reach the backend
		// through the open SearchCriteria tree with no framework translation, so
		// advertising it is precisely a claim about the backend.
		capabilityDescriptor{
			Name:            "WITHIN",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: backendSupportsCapability("WITHIN"),
		},
	)
}

// parseListReturnOptions reads the optional "RETURN (...)" clause that follows
// the LIST patterns. Return options may carry arguments — STATUS carries the
// item list it wants for each mailbox.
func parseListReturnOptions(decoder *imapwire.Decoder, args *listArgs) error {
	if !decoder.SP() {
		return nil
	}
	var keyword string
	if !decoder.ExpectAtom(&keyword) || !strings.EqualFold(keyword, "RETURN") {
		return fmt.Errorf("LIST expects RETURN after the mailbox patterns")
	}
	if !decoder.ExpectSP() {
		return decoder.Err()
	}
	return decoder.ExpectList(func() error {
		var option string
		if !decoder.ExpectAtom(&option) {
			return decoder.Err()
		}
		option = strings.ToUpper(option)
		args.returnOptions = append(args.returnOptions, option)
		if option == listReturnMetadata {
			return parseListMetadataEntries(decoder, args)
		}
		if option != listReturnStatus {
			return nil
		}
		if !decoder.ExpectSP() {
			return decoder.Err()
		}
		items, err := imapcodec.ReadStatusItems(decoder)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			return fmt.Errorf("LIST RETURN STATUS requires at least one item")
		}
		for _, item := range items {
			args.statusItems = append(args.statusItems, item)
		}
		return nil
	})
}

// applyListOptions validates the selection and return options and maps them onto
// the backend's ListOptions. Each option is refused unless the feature that
// activates it is active for this session, which is what keeps a field from
// being set on a backend that never agreed to honour it.
func applyListOptions(c *conn, args *listArgs, options *ListOptions) error {
	features := activeFeaturesContext(c.ctx, &c.state, c.server)
	for _, option := range args.selection {
		switch option {
		case listSelectSubscribed:
			if !args.legacy && !features[featureListSubscribe] {
				return fmt.Errorf("LIST selection option SUBSCRIBED is not enabled")
			}
			options.Subscribed = true
		case listSelectRemote:
			if !features[featureListExtended] {
				return fmt.Errorf("LIST selection option REMOTE is not enabled")
			}
			options.SelectRemote = true
		case listSelectRecursiveMatch:
			if !features[featureListExtended] {
				return fmt.Errorf("LIST selection option RECURSIVEMATCH is not enabled")
			}
			options.SelectRecursiveMatch = true
		case listSelectSpecialUse:
			if !features[featureListSpecialUse] {
				return fmt.Errorf("LIST selection option SPECIAL-USE is not enabled")
			}
			options.SelectSpecialUse = true
		default:
			return fmt.Errorf("unsupported LIST selection option %q", option)
		}
	}
	// RFC 5258 section 6: RECURSIVEMATCH modifies a base selection option and
	// needs one present; section 3 has the server answer BAD otherwise. An
	// independent option does not count: REMOTE is one, and so is SPECIAL-USE
	// (RFC 6154's formal syntax extends list-select-independent-opt), so
	// (REMOTE RECURSIVEMATCH) and (SPECIAL-USE RECURSIVEMATCH) are refused.
	// SUBSCRIBED is the only base option this framework knows.
	if options.SelectRecursiveMatch && !listSelectionHasBaseOption(args.selection) {
		return fmt.Errorf("LIST selection option RECURSIVEMATCH requires a base selection option such as SUBSCRIBED")
	}
	for _, option := range args.returnOptions {
		switch option {
		case listReturnSubscribed:
			if !features[featureListExtended] {
				return fmt.Errorf("LIST return option SUBSCRIBED is not enabled")
			}
			options.ReturnSubscribed = true
		case listReturnChildren:
			if !features[featureListChildren] {
				return fmt.Errorf("LIST return option CHILDREN is not enabled")
			}
			options.ReturnChildren = true
		case listReturnSpecialUse:
			if !features[featureListSpecialUse] {
				return fmt.Errorf("LIST return option SPECIAL-USE is not enabled")
			}
			options.ReturnSpecialUse = true
		case listReturnStatus:
			if !advertisedCapabilities(c)["LIST-STATUS"] {
				return fmt.Errorf("LIST return option STATUS requires LIST-STATUS")
			}
		default:
			// The remaining options belong to other extension groups.
			if err := validateListExtensionReturnOptions(c, option); err != nil {
				return err
			}
		}
	}
	return nil
}

// listSelectBaseOptions are the RFC 5258 base selection options this
// framework implements: the options RECURSIVEMATCH can modify, and so the only
// values a CHILDINFO extended data item can carry. RFC 5258 section 6
// (list-select-base-opt).
var listSelectBaseOptions = []string{listSelectSubscribed}

func listSelectionHasBaseOption(selection []string) bool {
	return slices.ContainsFunc(selection, func(option string) bool {
		return slices.Contains(listSelectBaseOptions, option)
	})
}

// listResultChildInfo returns the CHILDINFO values to write for one LIST
// result: those of the backend's ChildInfo that are base selection options
// this command requested, upper-cased and without repeats.
//
// What the session negotiated, not the backend, decides whether the item
// appears at all. RFC 5258 section 3.5 forbids CHILDINFO unless the command
// used RECURSIVEMATCH, which also rules out LSUB and the LIST answering a
// SELECT, and has it describe "the selection criteria that has caused it to be
// returned", which can only be options the client asked for.
func listResultChildInfo(args *listArgs, options *ListOptions, childInfo []string) []string {
	if args.legacy || !options.SelectRecursiveMatch {
		return nil
	}
	var values []string
	for _, value := range childInfo {
		value = strings.ToUpper(value)
		if slices.Contains(listSelectBaseOptions, value) && slices.Contains(args.selection, value) && !slices.Contains(values, value) {
			values = append(values, value)
		}
	}
	return values
}

// listResultSelected reports whether a backend's LIST result meets the
// selection criteria the framework enforces itself. RFC 6154 section 2: with
// the SPECIAL-USE selection option, LIST "MUST return only those mailboxes
// that have a special-use attribute set". Dropping the rest here keeps a
// backend that ignores SelectSpecialUse correct on the wire.
func listResultSelected(options *ListOptions, data *imap.ListData) bool {
	return !options.SelectSpecialUse || slices.ContainsFunc(data.Attrs, isSpecialUseAttr)
}

// wantsListStatus reports whether this LIST must be followed by STATUS
// responses.
func wantsListStatus(args *listArgs) bool {
	return len(args.statusItems) > 0 && slices.Contains(args.returnOptions, listReturnStatus)
}

// wantsPerMailboxResponses reports whether any return option needs the framework
// to remember which mailboxes LIST returned, so it can query each one after
// Session.List has finished streaming.
func wantsPerMailboxResponses(args *listArgs) bool {
	return wantsListStatus(args) ||
		slices.Contains(args.returnOptions, listReturnMyRights) ||
		slices.Contains(args.returnOptions, listReturnMetadata)
}

// writeListStatus answers LIST-STATUS, issuing one untagged STATUS response per
// mailbox the LIST returned.
//
// RFC 5819 delivers this as separate untagged STATUS responses rather than as
// data on the LIST line, so the framework can answer it through the mandatory
// Session.Status and no backend needs to know LIST-STATUS exists.
//
// The STATUS calls deliberately run after Session.List has returned, not while
// it streams: calling back into the backend mid-stream would re-enter it during
// one of its own methods, which the backend contract does not permit. The cost
// is holding the matched names, which the existing LIST result limit bounds.
//
// A mailbox that cannot be statused is skipped rather than failing the command.
// RFC 5819 section 2 expects LIST to succeed; a mailbox may also have been
// deleted between the LIST and the STATUS, which is a race, not an error.
func writeListStatus(ctx context.Context, c *conn, args *listArgs, mailboxes []string) error {
	if !wantsListStatus(args) {
		return nil
	}
	for _, mailbox := range mailboxes {
		data, err := c.state.session.Status(ctx, mailbox, &StatusOptions{Items: args.statusItems})
		if err != nil || data == nil {
			continue
		}
		if err := imapcodec.WriteStatusResponse(c.encoder, c.statusReply(data, args.statusItems)); err != nil {
			return err
		}
		if err := c.encoder.Flush(); err != nil {
			return err
		}
	}
	return nil
}

// listResultAttrs applies the return options that shape a LIST line's attribute
// list.
//
// A backend reports what it knows; the framework decides what the client is
// allowed to see. \Subscribed and the special-use attributes are suppressed
// unless asked for, because RFC 5258 and RFC 6154 make them opt-in — a client
// that did not ask must not have to guess whether an absent attribute means
// "not subscribed" or "not reported".
func listResultAttrs(args *listArgs, options *ListOptions, attrs []imap.MailboxAttr) []imap.MailboxAttr {
	if args.legacy {
		return attrs
	}
	filtered := make([]imap.MailboxAttr, 0, len(attrs))
	for _, attr := range attrs {
		switch {
		case attr.Equal(imap.MailboxAttrSubscribed):
			// A SUBSCRIBED selection already implies every result is
			// subscribed, so the attribute is meaningful there too.
			if !options.ReturnSubscribed && !options.Subscribed {
				continue
			}
		case attr.Equal(imap.MailboxAttrHasChildren), attr.Equal(imap.MailboxAttrHasNoChildren):
			if !options.ReturnChildren {
				continue
			}
		case isSpecialUseAttr(attr):
			if !options.ReturnSpecialUse && !options.SelectSpecialUse {
				continue
			}
		}
		filtered = append(filtered, attr)
	}
	return filtered
}

// specialUseAttrs are the use attributes of RFC 6154 section 2.
var specialUseAttrs = []imap.MailboxAttr{
	imap.MailboxAttrAll,
	imap.MailboxAttrArchive,
	imap.MailboxAttrDrafts,
	imap.MailboxAttrFlagged,
	imap.MailboxAttrJunk,
	imap.MailboxAttrSent,
	imap.MailboxAttrTrash,
}

func isSpecialUseAttr(attr imap.MailboxAttr) bool {
	return slices.ContainsFunc(specialUseAttrs, attr.Equal)
}
