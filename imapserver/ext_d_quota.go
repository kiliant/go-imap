package imapserver

import (
	"context"
	"fmt"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// QUOTA, QUOTA=RES-* and QUOTASET (RFC 9208).
//
// Resource names are an open registry, so they cross the backend boundary as
// [imap.QuotaResourceName] strings rather than as an enumeration. A server that
// serves a resource this library has never heard of needs no change here — which
// is the same reasoning that keeps FETCH and SEARCH items open.

// QuotaSession is the optional QUOTA support of RFC 9208. A Session implements
// it when the authenticated user has quota information.
//
// GetQuota answers a named quota root. QuotaRoots answers which roots apply to
// a mailbox, which RFC 9208 section 4.1.2 allows to be none.
type QuotaSession interface {
	QuotaRoots(ctx context.Context, mailbox string, options *QuotaOptions) ([]string, error)
	GetQuota(ctx context.Context, root string, options *QuotaOptions) (*imap.QuotaData, error)
}

// QuotaSetSession is the optional QUOTASET support of RFC 9208 section 4.1.3:
// changing a quota root's limits. It is a separate interface from QuotaSession
// because reading quotas is common and administering them is not, and a backend
// that serves quotas read-only should not have to implement a writer that
// always fails.
type QuotaSetSession interface {
	SetQuota(ctx context.Context, root string, limits []imap.QuotaResourceLimit, options *QuotaOptions) error
}

// QuotaOptions configures a quota operation. A nil pointer selects the
// defaults.
// Construct with keyed fields only; fields may be added in a future release.
type QuotaOptions struct{ _ struct{} }

func init() {
	registerCapabilities(
		capabilityDescriptor{
			Name:            "QUOTA",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: sessionImplements[QuotaSession](),
		},
		capabilityDescriptor{
			Name:            "QUOTASET",
			States:          stateMaskAuthenticated | stateMaskSelected,
			Depends:         []string{"QUOTA"},
			RequiresBackend: sessionImplements[QuotaSetSession](),
		},
	)
	registerCommand("GETQUOTA", stateMaskAuthenticated|stateMaskSelected, false, parseSingleAstring, handleGetQuota)
	registerCommand("GETQUOTAROOT", stateMaskAuthenticated|stateMaskSelected, false, parseSingleAstring, handleGetQuotaRoot)
	registerCommand("SETQUOTA", stateMaskAuthenticated|stateMaskSelected, false, parseSetQuota, handleSetQuota)
}

// parseSingleAstring reads a command whose only argument is one astring, which
// covers GETQUOTA's root and GETQUOTAROOT's and the ACL commands' mailbox.
func parseSingleAstring(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	var value string
	if !decoder.ExpectAstring(&value) || !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return value, int64(len(value)), nil
}

type setQuotaArgs struct {
	root   string
	limits []imap.QuotaResourceLimit
}

func parseSetQuota(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &setQuotaArgs{}
	if !decoder.ExpectAstring(&args.root) || !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	// RFC 9208 section 4.1.3 permits an empty limit list, which removes every
	// limit from the root.
	if err := decoder.ExpectList(func() error {
		var name string
		var limit int64
		if !decoder.ExpectAtom(&name) || !decoder.ExpectSP() || !decoder.ExpectNumber64(&limit) {
			return decoder.Err()
		}
		if limit < 0 {
			return fmt.Errorf("quota limit %d is negative", limit)
		}
		args.limits = append(args.limits, imap.QuotaResourceLimit{
			Name:  imap.QuotaResourceName(name),
			Limit: uint64(limit),
		})
		return nil
	}); err != nil {
		return nil, 0, err
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return args, int64(len(args.root) + len(args.limits)*32), nil
}

func handleGetQuota(ctx context.Context, c *conn, command *queuedCommand) error {
	root, _ := command.args.(string)
	if err := requireCapability(c, "QUOTA"); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	session, ok := c.state.session.(QuotaSession)
	if !ok {
		return c.writeBad(command.tag, "QUOTA is not available")
	}
	data, err := session.GetQuota(ctx, root, nil)
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if data != nil {
		if err := writeQuota(c, data); err != nil {
			return err
		}
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}

func handleGetQuotaRoot(ctx context.Context, c *conn, command *queuedCommand) error {
	mailbox, _ := command.args.(string)
	if err := requireCapability(c, "QUOTA"); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	session, ok := c.state.session.(QuotaSession)
	if !ok {
		return c.writeBad(command.tag, "QUOTA is not available")
	}
	roots, err := session.QuotaRoots(ctx, mailbox, nil)
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	// The QUOTAROOT response is written even for a mailbox with no roots:
	// RFC 9208 section 4.1.2 makes "no quota root" a real answer, distinct
	// from a root that happens to have no resources.
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("QUOTAROOT").SP().Mailbox(mailbox)
	for _, root := range roots {
		c.encoder.SP().Astring(root)
	}
	c.encoder.CRLF()
	if err := c.encoder.Flush(); err != nil {
		return err
	}
	for _, root := range roots {
		data, err := session.GetQuota(ctx, root, nil)
		if err != nil {
			return writeBackendError(c, command.tag, command.name, err)
		}
		if data == nil {
			continue
		}
		if err := writeQuota(c, data); err != nil {
			return err
		}
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}

func handleSetQuota(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*setQuotaArgs)
	if args == nil {
		return c.writeBad(command.tag, "invalid SETQUOTA arguments")
	}
	if err := requireCapability(c, "QUOTASET"); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	session, ok := c.state.session.(QuotaSetSession)
	if !ok {
		return c.writeBad(command.tag, "QUOTASET is not available")
	}
	if err := session.SetQuota(ctx, args.root, args.limits, nil); err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	// RFC 9208 section 4.1.3 expects the new state to be reported back, so the
	// client does not have to guess how the server clamped what it asked for.
	if reader, ok := c.state.session.(QuotaSession); ok {
		data, err := reader.GetQuota(ctx, args.root, nil)
		if err == nil && data != nil {
			if err := writeQuota(c, data); err != nil {
				return err
			}
		}
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}

func writeQuota(c *conn, data *imap.QuotaData) error {
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("QUOTA").SP().Astring(data.Root).SP().
		List(len(data.Resources), func(i int) {
			resource := data.Resources[i]
			c.encoder.Atom(string(resource.Name)).SP().Number64(int64(resource.Usage)).SP().Number64(int64(resource.Limit))
		}).CRLF()
	return c.encoder.Flush()
}

// sessionImplements builds a capability witness for an optional interface the
// Session implements.
//
// Unlike CapabilitySupport, which is a claim the backend makes in words, this
// is a claim it makes by having the methods — appropriate where the interface
// itself is the whole of the support. The two coexist: a capability whose
// behaviour is spread across data the backend returns needs the spoken witness,
// while one that is entirely "can you answer this command" needs only this.
// Before authentication there is no session and the question cannot be asked, so
// this abstains rather than answering no — matching selectedImplements and
// supportsAtomicMove, which abstain before a mailbox is selected for the same
// reason. Abstaining is unreachable for the capabilities that use this witness
// directly: all of them are authenticated-or-selected, and deriveCapabilities
// checks the state mask before the witness. It matters only to witnessesRev2,
// which consults these witnesses from a descriptor advertised in every state.
func sessionImplements[T any]() func(context.Context, *sessionState, Backend) bool {
	return func(_ context.Context, state *sessionState, _ Backend) bool {
		if state == nil || state.session == nil {
			return true
		}
		_, ok := state.session.(T)
		return ok
	}
}
