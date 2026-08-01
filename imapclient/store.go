package imapclient

import (
	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// StoreFlagsOp selects whether STORE replaces, adds, or removes flags.
type StoreFlagsOp string

const (
	StoreFlagsSet    StoreFlagsOp = "FLAGS"
	StoreFlagsAdd    StoreFlagsOp = "+FLAGS"
	StoreFlagsRemove StoreFlagsOp = "-FLAGS"
)

// StoreOptions configures STORE. Construct with keyed fields only.
type StoreOptions struct {
	// Op is FLAGS (the default), +FLAGS, or -FLAGS.
	Op StoreFlagsOp
	// Silent requests the .SILENT form. Servers may still send FETCH flag
	// updates; those are delivered through UnilateralDataHandler.Fetch.
	Silent bool
	_      struct{}
}

// Store changes flags for a sequence-number set. A nil options replaces FLAGS
// without using .SILENT.
func (c *Client) Store(set imap.SeqSet, flags []imap.Flag, options *StoreOptions) *Command {
	return c.store("STORE", set.String(), flags, options)
}

// StoreUID changes flags for a UID set. A nil options replaces FLAGS without
// using .SILENT.
func (c *Client) StoreUID(set imap.UIDSet, flags []imap.Flag, options *StoreOptions) *Command {
	return c.store("UID STORE", set.String(), flags, options)
}

func (c *Client) store(name, set string, flags []imap.Flag, options *StoreOptions) *Command {
	if set == "" {
		return rejectedCommand(c, name, "STORE requires a non-empty set")
	}
	o := StoreOptions{Op: StoreFlagsSet}
	if options != nil {
		o = *options
		if o.Op == "" {
			o.Op = StoreFlagsSet
		}
	}
	if o.Op != StoreFlagsSet && o.Op != StoreFlagsAdd && o.Op != StoreFlagsRemove {
		return rejectedCommand(c, name, "invalid STORE flag operation")
	}
	return c.beginCommand(name, stateSelected, func(enc *imapwire.Encoder) {
		op := string(o.Op)
		if o.Silent {
			op += ".SILENT"
		}
		enc.SP()
		writeNumSet(enc, set)
		enc.SP().Atom(op).SP().List(len(flags), func(i int) { enc.Flag(string(flags[i])) })
	}, nil)
}
