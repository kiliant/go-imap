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
// descendants that meet the other selection options. It cannot be refused
// either. LIST-EXTENDED enables it, RFC 5258 section 3 answers BAD only to
// options the server does not recognise, and IMAP4rev2's LIST incorporates it.
// So the framework never hands it to a backend that has not said it evaluates
// it:
//
//   - A backend witnessing "LIST-EXTENDED" through [CapabilitySupport]
//     receives [ListOptions.SelectRecursiveMatch] and answers natively.
//   - For any other backend the framework evaluates the option itself, in
//     listRecursiveMatch, from List calls without it.
//
// The witness routes the evaluation; it does not gate the advertisement.
// LIST-EXTENDED stays framework-witnessed in capability.go, and a routing
// token never joins rev2Incorporated or a RequiresBackend witness.
const listRecursiveMatchWitness = "LIST-EXTENDED"

// listRecursiveMatchBaseOptions are the base selection options
// listRecursiveMatch evaluates. A base option registered later (RFC 5258
// section 9.3) is refused for an unwitnessed backend until it is added here,
// rather than passed to a backend that never agreed to evaluate it.
// TestListRecursiveMatchCoversBaseOptions keeps this list level with
// listSelectBaseOptions.
var listRecursiveMatchBaseOptions = []string{listSelectSubscribed}

func backendEvaluatesRecursiveMatch(ctx context.Context, c *conn) bool {
	var backend Backend
	if c.server != nil {
		backend = c.server.backend
	}
	return backendSupportsCapability(listRecursiveMatchWitness)(ctx, &c.state, backend)
}

// listRecursiveMatch answers a LIST with RECURSIVEMATCH for a backend that does
// not evaluate it. It starts from the backend's own answer to the command
// without the option, so it never returns less than that, and adds what
// RECURSIVEMATCH adds (RFC 5258 sections 3.1 and 3.5): each name matching the
// patterns that has a descendant meeting the base selection options, with
// ChildInfo naming them. The descendant need not match the patterns. A
// hierarchy level that is not a mailbox is added as \NonExistent. These are the
// semantics of imapserver/memory's native implementation.
//
// None of its List calls carries RECURSIVEMATCH:
//
//   - the command itself, for the names it selects;
//   - "*" with the base options, for the descendants that meet them.
//     Independent options do not interact with RECURSIVEMATCH (RFC 5258
//     section 6), so SPECIAL-USE is left out here and applied to the result
//     like any LIST result. REMOTE is kept, so remote descendants count.
//   - the patterns without selection options, for the attributes of the
//     parents to add, made only when there are some;
//   - for each such parent that call did not return, the name alone, to tell
//     a level that is not a mailbox from a mailbox the patterns do not select.
//
// Which parents match the patterns is decided here. The canonical LIST
// pattern, which RFC 5258 leaves to the server, is taken to be the reference
// followed by the pattern, unless the pattern starts with the hierarchy
// delimiter (RFC 3501 section 6.3.8). Only the added parents depend on that
// rule. A name the backend hides from "*" is still returned when the command
// selects it, but gets no added parents.
func listRecursiveMatch(ctx context.Context, session Session, writer *ListWriter, args *listArgs, options *ListOptions) error {
	for _, option := range args.selection {
		if slices.Contains(listSelectBaseOptions, option) && !slices.Contains(listRecursiveMatchBaseOptions, option) {
			return fmt.Errorf("imapserver: RECURSIVEMATCH with %s is not evaluated for this backend", option)
		}
	}
	if !options.Subscribed {
		return fmt.Errorf("imapserver: RECURSIVEMATCH needs a base selection option")
	}

	command := *options
	command.SelectRecursiveMatch = false
	results, err := collectList(ctx, session, args.reference, args.patterns, &command)
	if err != nil {
		return err
	}
	descendants, err := collectList(ctx, session, "", []string{"*"}, &ListOptions{Subscribed: true, SelectRemote: options.SelectRemote})
	if err != nil {
		return err
	}

	// below maps each name with a descendant that meets the base options to
	// the hierarchy delimiter it was found with.
	below := make(map[string]rune)
	for name, data := range descendants {
		for _, parent := range listParents(name, data.Delimiter) {
			if _, ok := below[parent]; !ok {
				below[parent] = data.Delimiter
			}
		}
	}
	var parents []string
	for name, delimiter := range below {
		if results[name] == nil && listPatternMatches(name, args.reference, args.patterns, delimiter) {
			parents = append(parents, name)
		}
	}
	if len(parents) > 0 {
		attrsOptions := command
		attrsOptions.Subscribed = false
		attrsOptions.SelectSpecialUse = false
		existing, err := collectList(ctx, session, args.reference, args.patterns, &attrsOptions)
		if err != nil {
			return err
		}
		for _, name := range parents {
			data := existing[name]
			if data == nil {
				exists, err := listNameExists(ctx, session, name, options.SelectRemote)
				if err != nil {
					return err
				}
				if exists {
					// A mailbox the patterns do not select, as the backend
					// reads them: not a parent to add.
					continue
				}
				data = &imap.ListData{Attrs: []imap.MailboxAttr{imap.MailboxAttrNonExistent, imap.MailboxAttrHasChildren}, Delimiter: below[name], Mailbox: name}
			}
			if descendants[name] != nil && !imap.ContainsAttr(data.Attrs, imap.MailboxAttrSubscribed) {
				data.Attrs = append(data.Attrs, imap.MailboxAttrSubscribed)
			}
			results[name] = data
		}
	}

	names := make([]string, 0, len(results))
	for name := range results {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		data := results[name]
		if _, ok := below[name]; ok {
			data.ChildInfo = []string{listSelectSubscribed}
		}
		if err := writer.WriteList(ctx, data); err != nil {
			return err
		}
	}
	return nil
}

// collectList returns what List yields, keyed by name and bounded by the LIST
// result limit. The results are copies, since a backend may reuse what it
// wrote.
func collectList(ctx context.Context, session Session, reference string, patterns []string, options *ListOptions) (map[string]*imap.ListData, error) {
	results := make(map[string]*imap.ListData)
	writer := newListWriter(func(_ context.Context, data *imap.ListData) error {
		if data == nil || data.Mailbox == "" {
			return fmt.Errorf("imapserver: backend LIST returned an invalid mailbox")
		}
		if _, ok := results[data.Mailbox]; ok {
			return nil
		}
		if len(results) >= maxCommandListResults {
			return commandLimitError("LIST result limit exceeded")
		}
		results[data.Mailbox] = &imap.ListData{Attrs: slices.Clone(data.Attrs), Delimiter: data.Delimiter, Mailbox: data.Mailbox}
		return nil
	})
	err := session.List(ctx, writer, reference, patterns, options)
	writer.core.close()
	return results, err
}

// listNameExists reports whether the backend lists name as a mailbox.
func listNameExists(ctx context.Context, session Session, name string, remote bool) (bool, error) {
	found, err := collectList(ctx, session, "", []string{name}, &ListOptions{SelectRemote: remote})
	if err != nil {
		return false, err
	}
	data := found[name]
	return data != nil && !imap.ContainsAttr(data.Attrs, imap.MailboxAttrNonExistent), nil
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
