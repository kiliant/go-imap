package imapclient

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// SortKey is one sort key of a SORT command. It is an alias for [imap.SortKey],
// which both protocol directions share.
type SortKey = imap.SortKey

// Sort keys. RFC 5256 section 3 and RFC 5957.
const (
	SortKeyArrival     = imap.SortKeyArrival
	SortKeyCc          = imap.SortKeyCc
	SortKeyDate        = imap.SortKeyDate
	SortKeyFrom        = imap.SortKeyFrom
	SortKeySize        = imap.SortKeySize
	SortKeySubject     = imap.SortKeySubject
	SortKeyTo          = imap.SortKeyTo
	SortKeyDisplayFrom = imap.SortKeyDisplayFrom
	SortKeyDisplayTo   = imap.SortKeyDisplayTo
	SortKeyRelevancy   = imap.SortKeyRelevancy
)

// SortOptions configures SORT / UID SORT. A nil pointer selects UTF-8 charset
// and refuses the client-side fallback.
//
// Construct with keyed fields only; fields may be added in a future release.
type SortOptions struct {
	// Charset is sent as the SORT charset argument. Empty defaults to "UTF-8".
	Charset string

	// AllowClientFallback permits sorting in this process when the server does
	// not advertise SORT. See [Client.Sort] for the cost.
	AllowClientFallback bool

	_ struct{}
}

func (o *SortOptions) charset() string {
	if o == nil || o.Charset == "" {
		return "UTF-8"
	}
	return o.Charset
}

func (o *SortOptions) allowFallback() bool { return o != nil && o.AllowClientFallback }

// SortKeySpec is one entry of the SORT key list, optionally reversed. It is an
// alias for [imap.SortKeySpec], which both protocol directions share.
type SortKeySpec = imap.SortKeySpec

// SortData is the result of SORT or UID SORT. It is an alias for
// [imap.SortData], which both protocol directions share.
type SortData = imap.SortData

// Sort issues SORT and returns sequence numbers in sort order.
// SORT, RFC 5256.
//
// Sort blocks until completion. That is deliberate: the client-side fallback
// below is SEARCH + FETCH + in-process sort, so a command handle would lie
// about when the work happens.
//
// # Client-side fallback
//
// When the server does not advertise SORT and
// [SortOptions.AllowClientFallback] is set, this runs SEARCH, FETCHes every
// matching message's INTERNALDATE / RFC822.SIZE / ENVELOPE as needed for the
// requested keys, and sorts in-process. **That transfers the whole working set
// over the wire.** On a 50 000-message mailbox the difference between server
// SORT and this fallback is the difference between a short integer list and
// tens of megabytes of envelopes. DISPLAYFROM / DISPLAYTO are approximated
// from the envelope name/mailbox fields, which is not identical to a
// SORT=DISPLAY server's RFC 2047 decoding. Prefer a server that advertises
// SORT.
func (c *Client) Sort(ctx context.Context, keys []SortKeySpec, criteria imap.SearchCriteria, options *SortOptions) (*SortData, error) {
	return c.sort(ctx, false, keys, criteria, options)
}

// SortUID issues UID SORT. See [Client.Sort].
func (c *Client) SortUID(ctx context.Context, keys []SortKeySpec, criteria imap.SearchCriteria, options *SortOptions) (*SortData, error) {
	return c.sort(ctx, true, keys, criteria, options)
}

func (c *Client) sort(ctx context.Context, uid bool, keys []SortKeySpec, criteria imap.SearchCriteria, options *SortOptions) (*SortData, error) {
	name := "SORT"
	if uid {
		name = "UID SORT"
	}
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: name + " requires a non-nil context"}
	}
	if len(keys) == 0 {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "SORT requires at least one sort key"}
	}
	if criteria == nil {
		criteria = imap.SearchAll
	}
	if sortKeysWantRelevancy(keys) {
		if _, ok := criteria.(imap.SearchFuzzy); !ok {
			// RFC 6203: RELEVANCY requires a FUZZY search key in the same command.
			criteria = imap.SearchFuzzy{Criteria: criteria}
		}
	}
	if err := validateSearchCriteria(criteria); err != nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: err.Error()}
	}
	if err := validateSortKeys(keys, c, options.allowFallback()); err != nil {
		return nil, err
	}
	if !c.Supports("SORT") {
		if !options.allowFallback() {
			return nil, capabilityError(name, "SORT")
		}
		if sortKeysWantRelevancy(keys) {
			// RELEVANCY scores exist only on the server (RFC 6203). Emulating
			// them would invent rankings; refuse rather than silently reorder.
			return nil, &imap.Error{
				Type: imap.ErrorTypeProtocol,
				Text: "SORT RELEVANCY cannot be emulated client-side; the server must advertise SORT and SEARCH=FUZZY",
				Err:  ErrCapabilityNotAdvertised,
			}
		}
		return c.sortEmulated(ctx, uid, keys, criteria, options)
	}

	charset := options.charset()
	var numbers []uint32
	untagged := 0
	cmd := c.beginCommand(name, stateSelected, func(enc *imapwire.Encoder) {
		enc.SP().Special('(')
		for i, key := range keys {
			if i > 0 {
				enc.SP()
			}
			if key.Reverse {
				enc.Atom("REVERSE").SP()
			}
			enc.Atom(string(key.Key))
		}
		enc.Special(')').SP().Astring(charset).SP()
		writeSearchCriteria(enc, criteria)
	}, func(resp *untaggedResponse) (bool, error) {
		if resp.name != "SORT" || resp.hasNum {
			return false, nil
		}
		if err := countUntaggedResponse(&untagged, c.maxUntaggedResponses(), name); err != nil {
			return true, err
		}
		for resp.dec.SP() {
			var n uint32
			if !resp.dec.ExpectNumber(&n) {
				return true, resp.dec.Err()
			}
			numbers = append(numbers, n)
		}
		if !resp.dec.ExpectCRLF() {
			return true, resp.dec.Err()
		}
		return true, nil
	})
	if err := cmd.Wait(ctx); err != nil {
		return nil, err
	}
	return sortDataFromNumbers(uid, numbers, false), nil
}

func sortDataFromNumbers(uid bool, numbers []uint32, emulated bool) *SortData {
	data := &SortData{Emulated: emulated}
	if uid {
		data.UIDs = make([]imap.UID, len(numbers))
		for i, n := range numbers {
			data.UIDs[i] = imap.UID(n)
		}
		return data
	}
	data.SeqNums = make([]imap.SeqNum, len(numbers))
	for i, n := range numbers {
		data.SeqNums[i] = imap.SeqNum(n)
	}
	return data
}

func validateSortKeys(keys []SortKeySpec, c *Client, allowFallback bool) error {
	for _, key := range keys {
		if key.Key == "" || !isListKeyword(string(key.Key)) {
			return &imap.Error{Type: imap.ErrorTypeProtocol, Text: fmt.Sprintf("invalid SORT key %q", key.Key)}
		}
		upper := strings.ToUpper(string(key.Key))
		if upper == string(SortKeyDisplayFrom) || upper == string(SortKeyDisplayTo) {
			// Client-side fallback approximates DISPLAYFROM/DISPLAYTO from the
			// envelope; native SORT=DISPLAY is preferred but not required when
			// AllowClientFallback is set and SORT itself is absent.
			if !c.Supports("SORT=DISPLAY") && !(allowFallback && !c.Supports("SORT")) {
				return capabilityError("SORT DISPLAYFROM/DISPLAYTO", "SORT=DISPLAY")
			}
		}
		if upper == string(SortKeyRelevancy) && !c.Supports("SEARCH=FUZZY") {
			return capabilityError("SORT RELEVANCY", "SEARCH=FUZZY")
		}
	}
	return nil
}

func (c *Client) sortEmulated(ctx context.Context, uid bool, keys []SortKeySpec, criteria imap.SearchCriteria, options *SortOptions) (*SortData, error) {
	var numbers []uint32
	searchOpts := &SearchOptions{Charset: options.charset()}
	if uid {
		list, err := c.SearchUID(criteria, searchOpts).AllUID(ctx)
		if err != nil {
			return nil, err
		}
		numbers = make([]uint32, len(list))
		for i, u := range list {
			numbers[i] = uint32(u)
		}
	} else {
		list, err := c.Search(criteria, searchOpts).All(ctx)
		if err != nil {
			return nil, err
		}
		numbers = make([]uint32, len(list))
		for i, n := range list {
			numbers[i] = uint32(n)
		}
	}
	if len(numbers) == 0 {
		return sortDataFromNumbers(uid, nil, true), nil
	}
	sorted, err := c.sortClientSide(ctx, uid, numbers, keys)
	if err != nil {
		return nil, err
	}
	return sortDataFromNumbers(uid, sorted, true), nil
}

type sortRow struct {
	id       uint32
	arrival  time.Time
	date     time.Time
	size     int64
	from     string
	to       string
	cc       string
	subject  string
	dispFrom string
	dispTo   string
}

func (c *Client) sortClientSide(ctx context.Context, uid bool, numbers []uint32, keys []SortKeySpec) ([]uint32, error) {
	items := sortFetchItems(keys)
	var cmd *FetchCommand
	if uid {
		var set imap.UIDSet
		for _, n := range numbers {
			set.AddNum(imap.UID(n))
		}
		cmd = c.FetchUID(set, nil, items...)
	} else {
		var set imap.SeqSet
		for _, n := range numbers {
			set.AddNum(imap.SeqNum(n))
		}
		cmd = c.Fetch(set, nil, items...)
	}
	rows := make(map[uint32]*sortRow, len(numbers))
	seen := make(map[uint32]bool, len(numbers))
	for {
		msg, err := cmd.Next(ctx)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		id := uint32(msg.SeqNum)
		if uid {
			for _, values := range msg.Items {
				for _, value := range values {
					if u, ok := value.(imap.FetchDataUID); ok {
						id = uint32(u)
					}
				}
			}
		}
		row := rows[id]
		if row == nil {
			row = &sortRow{id: id}
			rows[id] = row
		}
		fillSortRow(row, msg)
		seen[id] = true
	}
	if err := cmd.Wait(ctx); err != nil {
		return nil, err
	}
	list := make([]*sortRow, 0, len(numbers))
	for _, n := range numbers {
		if !seen[n] {
			// Expunged (or otherwise missing) between SEARCH and FETCH — omit
			// rather than sorting a zero-valued row into a wrong position.
			continue
		}
		list = append(list, rows[n])
	}
	sort.SliceStable(list, func(i, j int) bool {
		return compareSortRows(list[i], list[j], keys) < 0
	})
	out := make([]uint32, len(list))
	for i, row := range list {
		out[i] = row.id
	}
	return out, nil
}

func sortFetchItems(keys []SortKeySpec) []imap.FetchItem {
	needEnv, needSize, needArrival, needUID := false, false, false, true
	for _, key := range keys {
		switch strings.ToUpper(string(key.Key)) {
		case string(SortKeyArrival):
			needArrival = true
		case string(SortKeySize):
			needSize = true
		case string(SortKeyDate), string(SortKeyFrom), string(SortKeyTo), string(SortKeyCc),
			string(SortKeySubject), string(SortKeyDisplayFrom), string(SortKeyDisplayTo):
			needEnv = true
		}
	}
	items := make([]imap.FetchItem, 0, 4)
	if needUID {
		items = append(items, imap.FetchItemUID)
	}
	if needArrival {
		items = append(items, imap.FetchItemInternalDate)
	}
	if needSize {
		items = append(items, imap.FetchItemRFC822Size)
	}
	if needEnv {
		items = append(items, imap.FetchItemEnvelope)
	}
	return items
}

func fillSortRow(row *sortRow, msg *imap.FetchMessageData) {
	for _, values := range msg.Items {
		for _, value := range values {
			switch v := value.(type) {
			case *imap.FetchDataInternalDate:
				if v != nil {
					row.arrival = v.Time
				}
			case imap.FetchDataRFC822Size:
				row.size = int64(v)
			case *imap.FetchDataEnvelope:
				if v == nil || v.Envelope == nil {
					continue
				}
				env := v.Envelope
				row.date = env.Date
				row.subject = env.Subject
				row.from = formatAddresses(env.From)
				row.to = formatAddresses(env.To)
				row.cc = formatAddresses(env.Cc)
				row.dispFrom = displayAddresses(env.From)
				row.dispTo = displayAddresses(env.To)
			}
		}
	}
}

func formatAddresses(addrs []imap.Address) string {
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if a.Mailbox != "" {
			if a.Host != "" {
				parts = append(parts, a.Mailbox+"@"+a.Host)
			} else {
				parts = append(parts, a.Mailbox)
			}
		}
	}
	return strings.ToLower(strings.Join(parts, ","))
}

func displayAddresses(addrs []imap.Address) string {
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if a.Name != "" {
			parts = append(parts, a.Name)
		} else if a.Mailbox != "" {
			parts = append(parts, a.Mailbox)
		}
	}
	return strings.ToLower(strings.Join(parts, ","))
}

func compareSortRows(a, b *sortRow, keys []SortKeySpec) int {
	for _, key := range keys {
		cmp := 0
		switch strings.ToUpper(string(key.Key)) {
		case string(SortKeyArrival):
			cmp = compareTime(a.arrival, b.arrival)
		case string(SortKeyDate):
			cmp = compareTime(a.date, b.date)
		case string(SortKeySize):
			cmp = compareInt64(a.size, b.size)
		case string(SortKeyFrom):
			cmp = strings.Compare(a.from, b.from)
		case string(SortKeyTo):
			cmp = strings.Compare(a.to, b.to)
		case string(SortKeyCc):
			cmp = strings.Compare(a.cc, b.cc)
		case string(SortKeySubject):
			cmp = strings.Compare(strings.ToLower(a.subject), strings.ToLower(b.subject))
		case string(SortKeyDisplayFrom):
			cmp = strings.Compare(a.dispFrom, b.dispFrom)
		case string(SortKeyDisplayTo):
			cmp = strings.Compare(a.dispTo, b.dispTo)
		}
		if key.Reverse {
			cmp = -cmp
		}
		if cmp != 0 {
			return cmp
		}
	}
	return compareUint32(a.id, b.id)
}

func compareTime(a, b time.Time) int {
	switch {
	case a.Equal(b):
		return 0
	case a.Before(b):
		return -1
	default:
		return 1
	}
}

func compareInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func compareUint32(a, b uint32) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
