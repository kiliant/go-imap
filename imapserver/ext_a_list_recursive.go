package imapserver

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/kiliant/go-imap"
)

// RECURSIVEMATCH (RFC 5258 section 3.1) is the LIST-EXTENDED selection option
// only a backend can evaluate natively: it depends on which names have
// subscribed descendants. It cannot be refused either. LIST-EXTENDED enables
// it, RFC 5258 section 3 answers BAD only to options the server does not
// recognise, and IMAP4rev2's LIST incorporates it. So the framework never
// hands it to a backend that has not said it evaluates it:
//
//   - A backend witnessing "LIST-EXTENDED" through [CapabilitySupport]
//     receives [ListOptions.SelectRecursiveMatch] and answers natively.
//   - For any other backend the framework evaluates the option itself, in
//     listRecursiveMatch, from List calls without it.
//
// The witness decides who evaluates the option, not whether it is advertised:
// LIST-EXTENDED stays framework-witnessed in capability.go.
const listRecursiveMatchWitness = "LIST-EXTENDED"

func backendEvaluatesRecursiveMatch(ctx context.Context, c *conn) bool {
	var backend Backend
	if c.server != nil {
		backend = c.server.backend
	}
	return backendSupportsCapability(listRecursiveMatchWitness)(ctx, &c.state, backend)
}

// listRecursiveMatch answers LIST (SUBSCRIBED RECURSIVEMATCH) for a backend
// that does not evaluate RECURSIVEMATCH, from two List calls over the whole
// account without it: one with Subscribed for every subscribed name, and one
// without for the attributes of unsubscribed parents. The result is every name
// matching the patterns that is subscribed or has a subscribed descendant, the
// second kind with ChildInfo ["SUBSCRIBED"]; the descendant need not match. A
// hierarchy level that is not a mailbox is returned as \NonExistent. RFC 5258
// section 3.5, and examples 9 and 10 in section 5. These are the semantics of
// imapserver/memory's native implementation.
//
// The canonical LIST pattern, which RFC 5258 leaves to the server, is the
// reference followed by the pattern, unless the pattern starts with the
// hierarchy delimiter (RFC 3501 section 6.3.8). A backend that interprets the
// reference differently should evaluate RECURSIVEMATCH itself.
func listRecursiveMatch(ctx context.Context, session Session, writer *ListWriter, reference string, patterns []string, options *ListOptions) error {
	subscribedOptions := *options
	subscribedOptions.SelectRecursiveMatch = false
	subscribed, err := collectAccountList(ctx, session, &subscribedOptions)
	if err != nil {
		return err
	}
	allOptions := subscribedOptions
	allOptions.Subscribed = false
	existing, err := collectAccountList(ctx, session, &allOptions)
	if err != nil {
		return err
	}

	type candidate struct {
		data            *imap.ListData
		delimiter       rune
		subscribedBelow bool
	}
	candidates := make(map[string]*candidate)
	get := func(name string, delimiter rune) *candidate {
		found := candidates[name]
		if found == nil {
			found = &candidate{delimiter: delimiter}
			candidates[name] = found
		}
		return found
	}
	for name, data := range subscribed {
		get(name, data.Delimiter).data = data
		for _, parent := range listParents(name, data.Delimiter) {
			get(parent, data.Delimiter).subscribedBelow = true
		}
	}
	names := make([]string, 0, len(candidates))
	for name := range candidates {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		found := candidates[name]
		if !listPatternMatches(name, reference, patterns, found.delimiter) {
			continue
		}
		result := &imap.ListData{Delimiter: found.delimiter, Mailbox: name}
		switch data := found.data; {
		case data != nil:
			result.Attrs = data.Attrs
		case existing[name] != nil:
			result.Attrs = existing[name].Attrs
		default:
			result.Attrs = []imap.MailboxAttr{imap.MailboxAttrNonExistent, imap.MailboxAttrHasChildren}
		}
		if found.subscribedBelow {
			result.ChildInfo = []string{listSelectSubscribed}
		}
		if err := writer.WriteList(ctx, result); err != nil {
			return err
		}
	}
	return nil
}

// collectAccountList returns what List yields for "*" under options, keyed by
// name and bounded by the LIST result limit. Attributes are copied, since a
// backend may reuse what it wrote.
func collectAccountList(ctx context.Context, session Session, options *ListOptions) (map[string]*imap.ListData, error) {
	results := make(map[string]*imap.ListData)
	writer := newListWriter(func(_ context.Context, data *imap.ListData) error {
		if data == nil || data.Mailbox == "" {
			return fmt.Errorf("imapserver: backend LIST returned an invalid mailbox")
		}
		if len(results) >= maxCommandListResults {
			return commandLimitError("LIST result limit exceeded")
		}
		if _, ok := results[data.Mailbox]; !ok {
			results[data.Mailbox] = &imap.ListData{Attrs: slices.Clone(data.Attrs), Delimiter: data.Delimiter, Mailbox: data.Mailbox}
		}
		return nil
	})
	err := session.List(ctx, writer, "", []string{"*"}, options)
	writer.core.close()
	return results, err
}

// listParents returns the hierarchy levels above name, outermost first.
func listParents(name string, delimiter rune) []string {
	if delimiter == 0 {
		return nil
	}
	var parents []string
	for i, r := range name {
		if r == delimiter && i > 0 {
			parents = append(parents, name[:i])
		}
	}
	return parents
}

// listPatternMatches reports whether name matches any canonical LIST pattern
// built from reference and patterns; see listRecursiveMatch.
func listPatternMatches(name, reference string, patterns []string, delimiter rune) bool {
	for _, pattern := range patterns {
		canonical := pattern
		if reference != "" && (delimiter == 0 || !strings.HasPrefix(pattern, string(delimiter))) {
			canonical = reference + pattern
		}
		if listWildcardMatch([]rune(name), []rune(canonical), delimiter) {
			return true
		}
	}
	return false
}

// listWildcardMatch matches a LIST pattern: "*" matches any sequence, "%" any
// sequence without the hierarchy delimiter (RFC 3501 section 6.3.8). With no
// delimiter, "%" matches like "*".
func listWildcardMatch(name, pattern []rune, delimiter rune) bool {
	// next[j] reports whether name[i+1:] matches pattern[j:] while row i is
	// filled; it starts as the row for the empty remainder of name.
	next := make([]bool, len(pattern)+1)
	next[len(pattern)] = true
	for j := len(pattern) - 1; j >= 0; j-- {
		next[j] = (pattern[j] == '*' || pattern[j] == '%') && next[j+1]
	}
	for i := len(name) - 1; i >= 0; i-- {
		current := make([]bool, len(pattern)+1)
		for j := len(pattern) - 1; j >= 0; j-- {
			switch pattern[j] {
			case '*':
				current[j] = current[j+1] || next[j]
			case '%':
				current[j] = current[j+1] || name[i] != delimiter && next[j]
			default:
				current[j] = name[i] == pattern[j] && next[j+1]
			}
		}
		next = current
	}
	return next[0]
}
