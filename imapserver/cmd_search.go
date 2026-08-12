package imapserver

import (
	"context"
	"fmt"
	"slices"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
	"github.com/kiliant/go-imap/internal/imapwire"
)

type searchArgs struct {
	charset  string
	criteria imap.SearchCriteria
}

func parseSearch(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &searchArgs{}
	if searchStartsWithCharset(decoder) {
		var keyword string
		if !decoder.ExpectAtom(&keyword) || !decoder.ExpectSP() || !decoder.ExpectAstring(&args.charset) || !decoder.ExpectSP() {
			return nil, 0, decoder.Err()
		}
	}
	criteria, err := imapcodec.ReadSearchCriteria(decoder)
	if err != nil {
		return nil, 0, err
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	args.criteria = criteria
	return args, int64(len(args.charset) + 64), nil
}

func searchStartsWithCharset(decoder *imapwire.Decoder) bool {
	return decoder.PeekAtomEqual("CHARSET")
}

func handleSearch(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*searchArgs)
	if args == nil || args.criteria == nil {
		return c.writeBad(command.tag, "invalid SEARCH arguments")
	}
	query := newSearchQuery(args.criteria, c.state.selected.uids)
	result, err := c.state.selected.mailbox.Search(ctx, query, &SearchOptions{Charset: args.charset})
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if result == nil {
		return writeBackendError(c, command.tag, command.name, fmt.Errorf("imapserver: backend SEARCH returned nil"))
	}
	if len(result.UIDs) > maxCommandSearchResults {
		return writeTaggedCondition(c, command.tag, "NO", imap.CodeLimit, "", "SEARCH result limit exceeded")
	}
	// Apply any mailbox changes published while the backend evaluated the
	// query before translating stable UIDs into sequence numbers.
	if err := c.drainUpdates(updateAccounting{}); err != nil {
		return err
	}
	uids := slices.Clone(result.UIDs)
	slices.Sort(uids)
	uids = slices.Compact(uids)
	numbers := make([]uint32, 0, len(uids))
	for _, uid := range uids {
		if uid == 0 {
			return writeBackendError(c, command.tag, command.name, fmt.Errorf("imapserver: backend SEARCH returned UID zero"))
		}
		seqNum, ok := c.state.selected.sequence(uid)
		if !ok {
			continue
		}
		if commandUsesUIDs(command) {
			numbers = append(numbers, uint32(uid))
		} else {
			numbers = append(numbers, uint32(seqNum))
		}
	}
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("SEARCH")
	for _, number := range numbers {
		c.encoder.SP().Number(number)
	}
	c.encoder.CRLF()
	if err := c.encoder.Flush(); err != nil {
		return err
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}
