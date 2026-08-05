package imapclient

import (
	"context"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// ThreadAlgorithm names a THREAD algorithm. It is an alias for
// [imap.ThreadAlgorithm], which both protocol directions share.
type ThreadAlgorithm = imap.ThreadAlgorithm

// Thread algorithms. RFC 5256 section 4.
const (
	ThreadOrderedSubject = imap.ThreadOrderedSubject
	ThreadReferences     = imap.ThreadReferences
)

// ThreadOptions configures THREAD / UID THREAD. A nil pointer selects UTF-8.
//
// Construct with keyed fields only; fields may be added in a future release.
type ThreadOptions struct {
	// Charset is sent as the THREAD charset argument. Empty defaults to "UTF-8".
	Charset string

	_ struct{}
}

func (o *ThreadOptions) charset() string {
	if o == nil || o.Charset == "" {
		return "UTF-8"
	}
	return o.Charset
}

// ThreadNode is one node of a THREAD response tree. It is an alias for
// [imap.ThreadNode], which both protocol directions share.
type ThreadNode = imap.ThreadNode

// ThreadData is the result of THREAD or UID THREAD. It is an alias for
// [imap.ThreadData], which both protocol directions share.
type ThreadData = imap.ThreadData

// Thread issues THREAD and returns a forest of sequence-number trees.
// THREAD, RFC 5256.
//
// There is no client-side fallback: reconstructing REFERENCES threading
// requires the full set of Message-IDs, In-Reply-To and References headers for
// every matching message, which is both expensive and algorithmically subtle.
// Callers facing a server without THREAD=… must FETCH headers themselves.
func (c *Client) Thread(ctx context.Context, algorithm ThreadAlgorithm, criteria imap.SearchCriteria, options *ThreadOptions) (*ThreadData, error) {
	return c.thread(ctx, false, algorithm, criteria, options)
}

// ThreadUID issues UID THREAD. See [Client.Thread].
func (c *Client) ThreadUID(ctx context.Context, algorithm ThreadAlgorithm, criteria imap.SearchCriteria, options *ThreadOptions) (*ThreadData, error) {
	return c.thread(ctx, true, algorithm, criteria, options)
}

func (c *Client) thread(ctx context.Context, uid bool, algorithm ThreadAlgorithm, criteria imap.SearchCriteria, options *ThreadOptions) (*ThreadData, error) {
	name := "THREAD"
	if uid {
		name = "UID THREAD"
	}
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: name + " requires a non-nil context"}
	}
	alg := strings.ToUpper(string(algorithm))
	if alg == "" || !isListKeyword(alg) {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: fmt.Sprintf("invalid THREAD algorithm %q", algorithm)}
	}
	capName := "THREAD=" + alg
	if !c.Supports(capName) {
		return nil, capabilityError(name, capName)
	}
	if criteria == nil {
		criteria = imap.SearchAll
	}
	if err := validateSearchCriteria(criteria); err != nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: err.Error()}
	}

	charset := options.charset()
	var roots []ThreadNode
	untagged := 0
	cmd := c.beginCommand(name, stateSelected, func(enc *imapwire.Encoder) {
		enc.SP().Atom(alg).SP().Astring(charset).SP()
		writeSearchCriteria(enc, criteria)
	}, func(resp *untaggedResponse) (bool, error) {
		if resp.name != "THREAD" || resp.hasNum {
			return false, nil
		}
		if err := countUntaggedResponse(&untagged, c.maxUntaggedResponses(), name); err != nil {
			return true, err
		}
		parsed, err := readThreadForest(resp.dec)
		if err != nil {
			return true, err
		}
		roots = parsed
		return true, nil
	})
	if err := cmd.Wait(ctx); err != nil {
		return nil, err
	}
	return &ThreadData{Roots: roots, UID: uid}, nil
}

// readThreadForest parses the optional list of thread-lists after THREAD.
// An empty response (* THREAD\r\n) is a valid no-match result.
//
// Top-level thread-lists are concatenated with no intervening SP
// (RFC 5256: thread-data = "THREAD" [SP 1*thread-list]).
func readThreadForest(dec *imapwire.Decoder) ([]ThreadNode, error) {
	if !dec.SP() {
		if !dec.ExpectCRLF() {
			return nil, dec.Err()
		}
		return nil, nil
	}
	var roots []ThreadNode
	for dec.PeekSpecial('(') {
		node, err := readThreadList(dec)
		if err != nil {
			return nil, err
		}
		roots = append(roots, node)
	}
	if !dec.ExpectCRLF() {
		return nil, dec.Err()
	}
	return roots, nil
}

// readThreadList parses one thread-list (RFC 5256 section 5):
//
//	thread-list    = "(" (thread-members / thread-nested) ")"
//	thread-members = nz-number *(SP nz-number) [SP thread-nested]
//	thread-nested  = 2*thread-list
//
// Consecutive numbers form a parent→child chain. Nested thread-lists attach as
// children of the last number and are adjacent with no SP between them.
//
// Nesting is bounded by the decoder's MaxListDepth so a hostile THREAD
// response cannot exhaust the stack (the wire codec only counts depth inside
// ExpectList, which this parser does not use).
func readThreadList(dec *imapwire.Decoder) (ThreadNode, error) {
	return readThreadListDepth(dec, 1)
}

func readThreadListDepth(dec *imapwire.Decoder, depth int) (ThreadNode, error) {
	max := dec.Options().MaxListDepth
	if max <= 0 {
		max = imapwire.DefaultMaxListDepth
	}
	if depth > max {
		return ThreadNode{}, fmt.Errorf("THREAD nesting exceeds MaxListDepth %d", max)
	}
	if !dec.ExpectSpecial('(') {
		return ThreadNode{}, dec.Err()
	}
	if dec.PeekSpecial('(') {
		var children []ThreadNode
		for dec.PeekSpecial('(') {
			child, err := readThreadListDepth(dec, depth+1)
			if err != nil {
				return ThreadNode{}, err
			}
			children = append(children, child)
		}
		if !dec.ExpectSpecial(')') {
			return ThreadNode{}, dec.Err()
		}
		return ThreadNode{Children: children}, nil
	}

	var first uint32
	if !dec.ExpectNumber(&first) {
		return ThreadNode{}, dec.Err()
	}
	head := ThreadNode{Num: first}
	tail := &head
	for {
		if dec.Special(')') {
			return head, nil
		}
		if dec.PeekSpecial('(') {
			if err := readThreadNestedDepth(dec, tail, depth); err != nil {
				return ThreadNode{}, err
			}
			continue
		}
		if !dec.ExpectSP() {
			return ThreadNode{}, dec.Err()
		}
		if dec.PeekSpecial('(') {
			if err := readThreadNestedDepth(dec, tail, depth); err != nil {
				return ThreadNode{}, err
			}
			continue
		}
		var n uint32
		if !dec.ExpectNumber(&n) {
			return ThreadNode{}, dec.Err()
		}
		tail.Children = append(tail.Children, ThreadNode{Num: n})
		tail = &tail.Children[len(tail.Children)-1]
	}
}

func readThreadNestedDepth(dec *imapwire.Decoder, parent *ThreadNode, depth int) error {
	for dec.PeekSpecial('(') {
		child, err := readThreadListDepth(dec, depth+1)
		if err != nil {
			return err
		}
		parent.Children = append(parent.Children, child)
	}
	return nil
}
