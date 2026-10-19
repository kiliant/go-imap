package imapserver

import "github.com/kiliant/go-imap"

func wireFlags(c *conn, flags []imap.Flag) []imap.Flag {
	if c.state.revision != revisionIMAP4rev2 {
		return flags
	}
	out := make([]imap.Flag, 0, len(flags))
	for _, flag := range flags {
		if !flag.Equal(imap.FlagRecent) {
			out = append(out, flag)
		}
	}
	return out
}
