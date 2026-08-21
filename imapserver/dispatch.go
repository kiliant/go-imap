package imapserver

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

type commandDescriptor struct {
	name    string
	states  stateMask
	barrier bool
	parse   func(*imapwire.Decoder) (any, int64, error)
	handle  func(context.Context, *conn, *queuedCommand) error
}

type queuedCommand struct {
	tag        string
	name       string
	descriptor *commandDescriptor
	args       any
	bytes      int64
	parseErr   error
	done       chan struct{}
}

var commandDescriptors = map[string]*commandDescriptor{
	"CAPABILITY": {name: "CAPABILITY", states: stateMaskAny, parse: parseNoArgs, handle: handleCapability},
	"NOOP":       {name: "NOOP", states: stateMaskAny, parse: parseNoArgs, handle: handleNoop},
	"LOGOUT":     {name: "LOGOUT", states: stateMaskAny, barrier: true, parse: parseNoArgs, handle: handleLogout},
	"STARTTLS":   {name: "STARTTLS", states: stateMaskNotAuthenticated, barrier: true, parse: parseNoArgs, handle: handleStartTLS},
	"COMPRESS":   {name: "COMPRESS", states: stateMaskAuthenticated | stateMaskSelected, barrier: true, parse: parseCompress, handle: handleCompress},
	"ENABLE":     {name: "ENABLE", states: stateMaskAuthenticated, barrier: true, parse: parseEnable, handle: handleEnable},
	"ID":         {name: "ID", states: stateMaskAny, parse: parseID, handle: handleID},
	"CHECK":      {name: "CHECK", states: stateMaskSelected, parse: parseNoArgs, handle: handleCheck},
	"UNSELECT":   {name: "UNSELECT", states: stateMaskSelected, barrier: true, parse: parseNoArgs, handle: handleUnselect},
}

func parseCompress(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	var algorithm string
	if !decoder.ExpectAtom(&algorithm) || !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	algorithm = strings.ToUpper(algorithm)
	return algorithm, int64(len(algorithm)), nil
}

func parseNoArgs(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return nil, 0, nil
}

func parseEnable(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	var capabilities []string
	var bytes int64
	for {
		var capability string
		if !decoder.ExpectAtom(&capability) {
			return nil, 0, decoder.Err()
		}
		capability = strings.ToUpper(capability)
		capabilities = append(capabilities, capability)
		bytes += int64(len(capability))
		if !decoder.SP() {
			break
		}
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return capabilities, bytes, nil
}

func parseID(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	if !decoder.PeekSpecial('(') {
		var value string
		var isNil bool
		if !decoder.ExpectNString(&value, &isNil) || !isNil || !decoder.ExpectCRLF() {
			if decoder.Err() != nil {
				return nil, 0, decoder.Err()
			}
			return nil, 0, fmt.Errorf("ID expects NIL or a field list")
		}
		return []imap.IDField(nil), 0, nil
	}
	var fields []imap.IDField
	var bytes int64
	seen := make(map[string]bool)
	err := decoder.ExpectList(func() error {
		if len(fields) >= 30 {
			return fmt.Errorf("ID allows at most 30 fields")
		}
		var name string
		if !decoder.ExpectString(&name) {
			return decoder.Err()
		}
		if name == "" || len(name) > 30 || seen[strings.ToLower(name)] {
			return fmt.Errorf("invalid or duplicate ID field %q", name)
		}
		seen[strings.ToLower(name)] = true
		if !decoder.ExpectSP() {
			return decoder.Err()
		}
		var value string
		var isNil bool
		if !decoder.ExpectNString(&value, &isNil) {
			return decoder.Err()
		}
		if len(value) > 1024 {
			return fmt.Errorf("ID field value exceeds 1024 octets")
		}
		field := imap.IDField{Name: name}
		if !isNil {
			field.Value = &value
		}
		fields = append(fields, field)
		bytes += int64(len(name) + len(value))
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return fields, bytes, nil
}

func handleCapability(ctx context.Context, c *conn, command *queuedCommand) error {
	capabilities := deriveCapabilitiesContext(ctx, &c.state, c.server)
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("CAPABILITY")
	for _, capability := range capabilities {
		c.encoder.SP().Atom(capability)
	}
	c.encoder.CRLF()
	return c.writeTagged(command.tag, "OK", "CAPABILITY completed")
}

func handleNoop(_ context.Context, c *conn, command *queuedCommand) error {
	if err := c.drainUpdates(updateAccounting{}); err != nil {
		return err
	}
	return c.writeTagged(command.tag, "OK", "NOOP completed")
}

func handleLogout(_ context.Context, c *conn, command *queuedCommand) error {
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").RespCond(imapwire.RespCond{Status: "BYE", Text: imapwire.RespText{Text: "logging out"}}).CRLF()
	if err := c.writeTagged(command.tag, "OK", "LOGOUT completed"); err != nil {
		return err
	}
	c.logout = true
	return nil
}

func handleStartTLS(ctx context.Context, c *conn, command *queuedCommand) error {
	if c.state.tls || c.server.options.TLSConfig == nil {
		return c.writeTagged(command.tag, "BAD", "STARTTLS is not available")
	}
	if err := c.writeTagged(command.tag, "OK", "begin TLS negotiation now"); err != nil {
		return err
	}
	return c.upgradeTLS(ctx)
}

func handleCompress(_ context.Context, c *conn, command *queuedCommand) error {
	algorithm, _ := command.args.(string)
	if c.state.compressed || algorithm != "DEFLATE" {
		return c.writeTagged(command.tag, "BAD", "compression algorithm is not available")
	}
	if err := c.writeTagged(command.tag, "OK", "compression active"); err != nil {
		return err
	}
	return c.enableCompression()
}

func handleEnable(ctx context.Context, c *conn, command *queuedCommand) error {
	requested, _ := command.args.([]string)
	enabled := enableCapabilities(ctx, &c.state, c.server, requested)
	if len(enabled) > 0 {
		c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("ENABLED")
		for _, capability := range enabled {
			c.encoder.SP().Atom(capability)
		}
		c.encoder.CRLF()
	}
	if c.state.revision == revisionIMAP4rev2 || c.state.enabledCapability("UTF8=ACCEPT") {
		c.utf8Accept.Store(true)
	}
	return c.writeTagged(command.tag, "OK", "ENABLE completed")
}

func handleID(_ context.Context, c *conn, command *queuedCommand) error {
	_ = command.args // ID values are informational and never affect behaviour.
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("ID").SP()
	if len(c.server.options.ServerID) == 0 {
		c.encoder.NIL()
	} else {
		keys := make([]string, 0, len(c.server.options.ServerID))
		for key := range c.server.options.ServerID {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		c.encoder.List(len(keys)*2, func(i int) {
			key := keys[i/2]
			if i%2 == 0 {
				c.encoder.String(key)
			} else {
				c.encoder.String(c.server.options.ServerID[key])
			}
		})
	}
	c.encoder.CRLF()
	return c.writeTagged(command.tag, "OK", "ID completed")
}

func handleCheck(_ context.Context, c *conn, command *queuedCommand) error {
	if err := c.drainUpdates(updateAccounting{}); err != nil {
		return err
	}
	return c.writeTagged(command.tag, "OK", "CHECK completed")
}

func handleUnselect(ctx context.Context, c *conn, command *queuedCommand) error {
	selected := c.state.unselect()
	if selected == nil {
		return c.writeTagged(command.tag, "BAD", "no mailbox is selected")
	}
	selected.close()
	err := selected.mailbox.Unselect(ctx, nil)
	if err != nil {
		if writeErr := c.writeTagged(command.tag, "NO", "UNSELECT failed"); writeErr != nil {
			return writeErr
		}
		return fmt.Errorf("imapserver: backend UNSELECT failed: %w", err)
	}
	return c.writeTagged(command.tag, "OK", "UNSELECT completed")
}
