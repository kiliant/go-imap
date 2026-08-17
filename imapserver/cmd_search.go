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
	charset string
	// extended records that a RETURN clause was present, which selects the
	// ESEARCH response shape. See ext_a_esearch.go.
	extended      bool
	returnOptions []string
	// partial is PARTIAL's requested window. See ext_c_partial.go.
	partial  *searchPartialRange
	criteria imap.SearchCriteria
}

func parseSearch(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &searchArgs{}
	if err := parseSearchReturnOptions(decoder, args); err != nil {
		return nil, 0, err
	}
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
	if err := validateSearchReturnOptions(c, args); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	// FILTERS substitutes stored criteria before the backend sees the tree.
	// See ext_e_comparator.go.
	criteria, err := applySearchFilters(ctx, c, args.criteria)
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	// After substitution, not before: a FILTERS expansion can itself contain an
	// extension key, and gating the unexpanded tree would miss it.
	if err := requireCriteriaCapabilities(c, criteria); err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	query := newSearchQuery(criteria, c.state.selected.uids)
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
	uids := slices.Clone(result.UIDs)
	slices.Sort(uids)
	uids = slices.Compact(uids)
	numbers := make([]uint32, 0, len(uids))
	present := make([]imap.UID, 0, len(uids))
	for _, uid := range uids {
		if uid == 0 {
			return writeBackendError(c, command.tag, command.name, fmt.Errorf("imapserver: backend SEARCH returned UID zero"))
		}
		seqNum, ok := c.state.selected.sequence(uid)
		if !ok {
			continue
		}
		present = append(present, uid)
		if commandUsesUIDs(command) {
			numbers = append(numbers, uint32(uid))
		} else {
			numbers = append(numbers, uint32(seqNum))
		}
	}
	if err := writeSearchResponse(c, command, args, present, numbers); err != nil {
		return err
	}
	if err := c.writeTagged(command.tag, "OK", command.name+" completed"); err != nil {
		return err
	}
	return c.drainUpdates(updateAccounting{})
}
